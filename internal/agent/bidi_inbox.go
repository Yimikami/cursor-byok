package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	aiserv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
)

// BidiAppend contains no service name. Keep its payload opaque until the
// matching SSE endpoint selects AgentService, BugBot or InferenceService.
// Current Cursor sends up to 32 appends concurrently and retries by seqno,
// so arrival order must not determine invocation order.
const (
	bidiMaxPayload     = 32 << 20
	bidiMaxQueuedBytes = 64 << 20
	bidiMaxInboxes     = 256
	bidiMaxPending     = 128
	bidiInboxTTL       = 2 * time.Minute
)

type bidiInbox struct {
	owner    string
	protocol string
	queue    map[int64][]byte
	nextSeq  int64
	notify   chan struct{}
	closed   bool
	touched  time.Time
	store    *bidiInboxStore
}

type bidiInboxStore struct {
	mu          sync.Mutex
	entries     map[string]*bidiInbox
	queuedBytes int
}

var bidiInboxes = newBidiInboxStore()

// InspectBidiAppend validates only the shared transport envelope. It does not
// assign an inner service or queue the message.
func InspectBidiAppend(body []byte, contentType string) (string, int64, error) {
	bidi := &aiserv1.BidiAppendRequest{}
	if err := decodeUnary(body, contentType, bidi); err != nil {
		return "", 0, err
	}
	if _, err := decodeBidiPayload(bidi); err != nil {
		return "", 0, err
	}
	id := bidi.GetRequestId().GetRequestId()
	if id == "" || len(id) > 1024 || bidi.GetAppendSeqno() < 0 {
		return "", 0, errors.New("invalid bidi request identity")
	}
	return id, bidi.GetAppendSeqno(), nil
}

func InspectBidiStream(body []byte, contentType string, poll bool) (string, error) {
	var id string
	if poll {
		request := &aiserv1.BidiPollRequest{}
		if err := decodeUnary(body, contentType, request); err != nil {
			return "", err
		}
		id = request.GetRequestId().GetRequestId()
	} else {
		request := &aiserv1.BidiRequestId{}
		if err := decodeUnary(body, contentType, request); err != nil {
			return "", err
		}
		id = request.GetRequestId()
	}
	if id == "" || len(id) > 1024 {
		return "", errors.New("invalid bidi request_id")
	}
	return id, nil
}

func newBidiInboxStore() *bidiInboxStore {
	return &bidiInboxStore{entries: make(map[string]*bidiInbox)}
}

func decodeBidiPayload(bidi *aiserv1.BidiAppendRequest) ([]byte, error) {
	// data_binary is field 4 in Cursor 3.32.4. It is newer than the checked-in
	// generated proto, so read its unknown field without regenerating the
	// unrelated, very large aiserver schema.
	binary, err := inferenceBytes(bidi.ProtoReflect().GetUnknown(), 4)
	if err != nil {
		return nil, err
	}
	if binary != nil && bidi.GetData() != "" {
		return nil, errors.New("BidiAppend has both binary and hex data")
	}
	if len(binary) > bidiMaxPayload || len(bidi.GetData()) > 2*bidiMaxPayload {
		return nil, errors.New("BidiAppend payload too large")
	}
	if binary != nil {
		return binary, nil
	}
	data, err := hex.DecodeString(strings.TrimSpace(bidi.GetData()))
	if err != nil {
		return nil, fmt.Errorf("bidi data not hex: %w", err)
	}
	return data, nil
}

func (s *bidiInboxStore) expireLocked(now time.Time) {
	for id, in := range s.entries {
		if in.protocol != "" && !in.closed || now.Sub(in.touched) <= bidiInboxTTL {
			continue
		}
		for _, data := range in.queue {
			s.queuedBytes -= len(data)
		}
		delete(s.entries, id)
	}
}

func (s *bidiInboxStore) getLocked(owner string) (*bidiInbox, error) {
	if owner == "" || len(owner) > 1024 {
		return nil, errors.New("invalid bidi request_id")
	}
	s.expireLocked(time.Now())
	in := s.entries[owner]
	if in == nil {
		if len(s.entries) >= bidiMaxInboxes {
			return nil, errors.New("too many bidi streams")
		}
		in = &bidiInbox{owner: owner, queue: make(map[int64][]byte), notify: make(chan struct{}, 1), touched: time.Now(), store: s}
		s.entries[owner] = in
	}
	return in, nil
}

func (s *bidiInboxStore) append(owner string, seq int64, payload []byte) Result {
	if seq < 0 {
		return errResult(http.StatusBadRequest, "negative bidi append sequence")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	in, err := s.getLocked(owner)
	if err != nil {
		return errResult(http.StatusBadRequest, err.Error())
	}
	// A retried already-consumed append is idempotent, including after the
	// stream has finished. Future appends cannot revive a closed stream.
	if seq < in.nextSeq {
		return Result{Status: 200, ContentType: "application/proto"}
	}
	if in.closed {
		return errResult(http.StatusGone, "bidi stream is closed")
	}
	if _, exists := in.queue[seq]; exists {
		return Result{Status: 200, ContentType: "application/proto"}
	}
	if len(in.queue) >= bidiMaxPending || seq-in.nextSeq >= bidiMaxPending || len(payload) > bidiMaxPayload || s.queuedBytes+len(payload) > bidiMaxQueuedBytes {
		return errResult(http.StatusTooManyRequests, "bidi input queue is full")
	}
	in.queue[seq] = append([]byte{}, payload...)
	s.queuedBytes += len(payload)
	in.touched = time.Now()
	select {
	case in.notify <- struct{}{}:
	default:
	}
	return Result{Status: 200, ContentType: "application/proto"}
}

func (s *bidiInboxStore) claim(owner, protocol string) (*bidiInbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, err := s.getLocked(owner)
	if err != nil {
		return nil, err
	}
	if in.protocol != "" || in.closed {
		if !in.closed || in.protocol != protocol || (protocol != "agent" && protocol != "bugbot") {
			return nil, errors.New("bidi request_id already belongs to a stream")
		}
		// Legacy SSE retries recover the conversation from the session store.
		// Use a new attachment so an old pump/release can never touch it, while
		// preserving the watermark that makes retransmitted appends idempotent.
		in = &bidiInbox{owner: owner, protocol: protocol, nextSeq: in.nextSeq, queue: make(map[int64][]byte), notify: make(chan struct{}, 1), store: s}
		s.entries[owner] = in
	}
	in.protocol = protocol
	in.touched = time.Now()
	return in, nil
}

func (in *bidiInbox) next(ctx context.Context) ([]byte, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		in.store.mu.Lock()
		if data, ok := in.queue[in.nextSeq]; ok {
			delete(in.queue, in.nextSeq)
			in.nextSeq++
			in.store.queuedBytes -= len(data)
			in.touched = time.Now()
			in.store.mu.Unlock()
			return data, nil
		}
		closed := in.closed
		in.store.mu.Unlock()
		if closed {
			return nil, io.EOF
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-in.notify:
		}
	}
}

func (in *bidiInbox) release() {
	in.store.mu.Lock()
	defer in.store.mu.Unlock()
	for seq, data := range in.queue {
		in.store.queuedBytes -= len(data)
		delete(in.queue, seq)
	}
	in.closed = true
	in.touched = time.Now()
	select {
	case in.notify <- struct{}{}:
	default:
	}
}

func attachLegacyBidi(ctx context.Context, owner, protocol string, apply func(string, []byte) Result) (context.Context, func(), <-chan error, error) {
	in, err := bidiInboxes.claim(owner, protocol)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	failures := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			data, err := in.next(ctx)
			if err != nil {
				return
			}
			result := apply(owner, data)
			if result.Status != http.StatusOK {
				failures <- fmt.Errorf("invalid %s input: %s", protocol, result.Body)
				cancel()
				return
			}
		}
	}()
	return ctx, func() { cancel(); <-done; in.release() }, failures, nil
}
