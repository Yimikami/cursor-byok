package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	aiserv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
)

func TestBidiConnectGzipBoundsExpandedPayload(t *testing.T) {
	input := bytes.Repeat([]byte("a"), 257)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write(input)
	_ = writer.Close()
	if _, err := gunzipLimit(compressed.Bytes(), 256); err == nil {
		t.Fatal("compressed payload exceeded expanded size limit")
	}
	decoded, err := gunzipLimit(compressed.Bytes(), 257)
	if err != nil || !bytes.Equal(decoded, input) {
		t.Fatalf("payload exactly at limit rejected: %v", err)
	}
	if _, err := gunzipLimit(compressed.Bytes()[:compressed.Len()-4], 257); err == nil {
		t.Fatal("truncated gzip accepted")
	}
}

func TestBidiInboxOrdersRetriesAndBindsOneProtocol(t *testing.T) {
	s := newBidiInboxStore()
	for seq, payload := range map[int64]string{2: "third", 0: "first"} {
		if r := s.append("run", seq, []byte(payload)); r.Status != 200 {
			t.Fatal(r)
		}
	}
	in, err := s.claim("run", "inference")
	if err != nil {
		t.Fatal(err)
	}
	defer in.release()
	if _, err := s.claim("run", "agent"); err == nil {
		t.Fatal("one id accepted two inner protocols")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	b, err := in.next(ctx)
	if err != nil || string(b) != "first" {
		t.Fatalf("first=%q %v", b, err)
	}
	gap, cancelGap := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancelGap()
	if _, err := in.next(gap); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("out-of-order append delivered across sequence gap")
	}
	if r := s.append("run", 0, []byte("retried first")); r.Status != 200 {
		t.Fatal(r)
	}
	if r := s.append("run", 1, []byte("second")); r.Status != 200 {
		t.Fatal(r)
	}
	for _, want := range []string{"second", "third"} {
		b, err := in.next(ctx)
		if err != nil || string(b) != want {
			t.Fatalf("got=%q want=%q err=%v", b, want, err)
		}
	}
	if s.queuedBytes != 0 {
		t.Fatalf("consumed queue retained %d bytes", s.queuedBytes)
	}
	if r := s.append("run", 3, []byte("queued when closing")); r.Status != 200 {
		t.Fatal(r)
	}
	in.release()
	if s.queuedBytes != 0 {
		t.Fatal("close retained queued bytes")
	}
	if r := s.append("run", 4, []byte("late")); r.Status != http.StatusGone {
		t.Fatalf("late append=%d", r.Status)
	}
}

func TestBidiInboxBoundsAndExpiresUnclaimedInputs(t *testing.T) {
	s := newBidiInboxStore()
	if r := s.append("stale", 0, []byte("discard me")); r.Status != 200 {
		t.Fatal(r)
	}
	s.entries["stale"].touched = time.Now().Add(-bidiInboxTTL - time.Second)
	if r := s.append("new", 0, []byte("live")); r.Status != 200 {
		t.Fatal(r)
	}
	if s.entries["stale"] != nil || s.queuedBytes != 4 {
		t.Fatal("expired pending payload retained")
	}
	if r := s.append("new", bidiMaxPending, []byte("too far")); r.Status != http.StatusTooManyRequests {
		t.Fatalf("far sequence=%d", r.Status)
	}
	if r := s.append("new", -1, nil); r.Status != http.StatusBadRequest {
		t.Fatalf("negative sequence=%d", r.Status)
	}
	s.queuedBytes = bidiMaxQueuedBytes
	if r := s.append("new", 1, []byte("full")); r.Status != http.StatusTooManyRequests {
		t.Fatalf("full queue=%d", r.Status)
	}
}

func TestBidiPayloadBinaryAndHexAreUnambiguous(t *testing.T) {
	bidi := &aiserv1.BidiAppendRequest{Data: "0102"}
	b, err := decodeBidiPayload(bidi)
	if err != nil || string(b) != "\x01\x02" {
		t.Fatalf("hex %x %v", b, err)
	}
	bidi.Data = ""
	bidi.DataBinary = []byte{3, 4}
	b, err = decodeBidiPayload(bidi)
	if err != nil || string(b) != "\x03\x04" {
		t.Fatalf("binary %x %v", b, err)
	}
	bidi.Data = "0102"
	if _, err := decodeBidiPayload(bidi); err == nil {
		t.Fatal("accepted conflicting binary and hex payload")
	}
}

func TestLegacyBidiReconnectPreservesSequenceAndExclusiveProtocol(t *testing.T) {
	for _, protocol := range []string{"agent", "bugbot"} {
		t.Run(protocol, func(t *testing.T) {
			s := newBidiInboxStore()
			if r := s.append("retry", 0, []byte("initial")); r.Status != 200 {
				t.Fatal(r)
			}
			old, err := s.claim("retry", protocol)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.claim("retry", protocol); err == nil {
				t.Fatal("concurrent legacy stream accepted")
			}
			if _, err := old.next(context.Background()); err != nil {
				t.Fatal(err)
			}
			old.release()
			if _, err := s.claim("retry", "inference"); err == nil {
				t.Fatal("detached stream changed protocol")
			}
			var workers sync.WaitGroup
			claimed := make(chan *bidiInbox, 8)
			for i := 0; i < 8; i++ {
				workers.Add(1)
				go func() {
					defer workers.Done()
					if in, err := s.claim("retry", protocol); err == nil {
						claimed <- in
					}
				}()
			}
			workers.Wait()
			close(claimed)
			if len(claimed) != 1 {
				t.Fatalf("concurrent retry owners=%d", len(claimed))
			}
			in := <-claimed
			defer in.release()
			old.release() // A stale release must not close the new attachment.
			if r := s.append("retry", 0, []byte("must not replay")); r.Status != 200 {
				t.Fatal(r)
			}
			if r := s.append("retry", 1, []byte("next")); r.Status != 200 {
				t.Fatal(r)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			data, err := in.next(ctx)
			if err != nil || string(data) != "next" || in.nextSeq != 2 || s.queuedBytes != 0 {
				t.Fatalf("retry replayed consumed input: data=%q next=%d bytes=%d err=%v", data, in.nextSeq, s.queuedBytes, err)
			}
		})
	}
	s := newBidiInboxStore()
	in, _ := s.claim("inference", "inference")
	in.release()
	if _, err := s.claim("inference", "inference"); err == nil {
		t.Fatal("inference resumption was enabled without state replay")
	}
}
