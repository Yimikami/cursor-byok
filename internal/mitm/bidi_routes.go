package mitm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"cursor-byok/internal/agent"
)

const bidiRouteLimit = 64 << 20
const bidiStagingLimit = 32 << 20
const bidiStagingEntries = 64

var errBidiCapacity = errors.New("bidi request capacity exceeded")

// These shared-Bidi mappings come from Cursor 3.32.4's BidiTransport factory.
// All other native RPCs retain their ordinary passthrough behavior.
var nativeBidiStreams = map[string]bool{
	"/aiserver.v1.ChatService/StreamUnifiedChatWithToolsSSE":            false,
	"/aiserver.v1.ChatService/StreamUnifiedChatWithToolsPoll":           true,
	"/aiserver.v1.ChatService/StreamUnifiedChatWithToolsIdempotentSSE":  false,
	"/aiserver.v1.ChatService/StreamUnifiedChatWithToolsIdempotentPoll": true,
	"/aiserver.v1.HealthService/StreamBidiSSE":                          false,
	"/aiserver.v1.HealthService/StreamBidiPoll":                         true,
	"/aiserver.v1.AiService/StreamSttSSE":                               false,
	"/aiserver.v1.AiService/StreamSttPoll":                              true,
	"/aiserver.v1.AiService/StreamUiBestOfNJudgeSSE":                    false,
	"/aiserver.v1.AiService/StreamUiBestOfNJudgePoll":                   true,
}

func reviewPath(path string) bool {
	switch path {
	case "/aiserver.v1.AiService/StreamBugBotAgentic", "/aiserver.v1.AiService/StreamBugBotAgenticSSE", "/aiserver.v1.AiService/StreamBugBotAgenticPoll":
		return true
	}
	return false
}

func localBidiStream(path string) bool {
	return path == "/agent.v1.AgentService/RunSSE" || path == "/aiserver.v1.InferenceService/RunInferenceSSE" || path == "/aiserver.v1.AiService/StreamBugBotAgenticSSE"
}

func unsupportedLocalBidiPoll(path string) bool {
	return path == "/agent.v1.AgentService/RunPoll" || path == "/aiserver.v1.InferenceService/RunInferencePoll" || path == "/aiserver.v1.AiService/StreamBugBotAgenticPoll"
}

func localProxyPath(path string) bool {
	if localBidiStream(path) || unsupportedLocalBidiPoll(path) {
		return true
	}
	switch path {
	case "/aiserver.v1.AiService/StreamBugBotAgentic", "/aiserver.v1.AiService/GetDefaultModel", "/agent/v1/run", "/aiserver.v1.AiService/AvailableModels", "/aiserver.v1.AiService/GetDefaultModelNudgeData":
		return true
	}
	return false
}

type queuedBidiRequest struct {
	request *http.Request
	body    []byte
	seq     int64
}
type bidiRoute struct {
	mode       string
	credential [32]byte
	pending    map[int64]queuedBidiRequest
	replayGate chan struct{}
	ready      bool
	active     int
	touched    time.Time
}
type bidiRouteRegistry struct {
	mu          sync.Mutex
	entries     map[string]*bidiRoute
	queuedBytes int
	closed      bool
}

func newBidiRouteRegistry() *bidiRouteRegistry {
	return &bidiRouteRegistry{entries: make(map[string]*bidiRoute)}
}
func bidiRouteKey(req *http.Request, id string) string { return normalizeHost(req.URL.Host) + "|" + id }
func bidiCredential(req *http.Request) [32]byte {
	return sha256.Sum256([]byte(strings.Join(req.Header.Values("Authorization"), "\x00") + "\x01" + strings.Join(req.Header.Values("Cookie"), "\x00")))
}

func (r *bidiRouteRegistry) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.entries = make(map[string]*bidiRoute)
	r.queuedBytes = 0
}

func (r *bidiRouteRegistry) dropEntryLocked(key string) {
	for _, p := range r.entries[key].pending {
		r.queuedBytes -= len(p.body)
	}
	delete(r.entries, key)
}

func (r *bidiRouteRegistry) stagingUsageLocked() (count, size int) {
	for _, entry := range r.entries {
		if entry.mode == "" {
			count++
			for _, p := range entry.pending {
				size += len(p.body)
			}
		}
	}
	return
}

// Unclaimed input must never reserve the capacity needed to claim a stream.
// Evict only staging entries; established routes and their replay are retained.
func (r *bidiRouteRegistry) evictStagingLocked() bool {
	oldestKey := ""
	var oldest time.Time
	for key, entry := range r.entries {
		if entry.mode == "" && entry.active == 0 && (oldestKey == "" || entry.touched.Before(oldest)) {
			oldestKey, oldest = key, entry.touched
		}
	}
	if oldestKey == "" {
		return false
	}
	r.dropEntryLocked(oldestKey)
	return true
}

func (r *bidiRouteRegistry) entryLocked(req *http.Request, id string, claiming bool) (*bidiRoute, error) {
	if r.closed {
		return nil, fmt.Errorf("proxy is stopping")
	}
	now := time.Now()
	for key, entry := range r.entries {
		if entry.active == 0 && now.Sub(entry.touched) > 2*time.Minute {
			r.dropEntryLocked(key)
		}
	}
	key := bidiRouteKey(req, id)
	entry := r.entries[key]
	if entry == nil {
		if !claiming {
			count, _ := r.stagingUsageLocked()
			if count >= bidiStagingEntries {
				return nil, errBidiCapacity
			}
		} else if len(r.entries) >= 256 {
			r.evictStagingLocked()
		}
		if len(r.entries) >= 256 {
			return nil, errBidiCapacity
		}
		entry = &bidiRoute{credential: bidiCredential(req), pending: make(map[int64]queuedBidiRequest), replayGate: make(chan struct{}, 1)}
		r.entries[key] = entry
	}
	if entry.credential != bidiCredential(req) {
		return nil, fmt.Errorf("bidi stream credentials do not match")
	}
	entry.touched = now
	return entry, nil
}

func readBidiBody(req *http.Request) (raw, decoded []byte, err error) {
	if req.Body == nil {
		return nil, nil, fmt.Errorf("missing request body")
	}
	raw, err = io.ReadAll(io.LimitReader(req.Body, bidiRouteLimit+1))
	_ = req.Body.Close()
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > bidiRouteLimit {
		return nil, nil, fmt.Errorf("bidi request too large")
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	encoding := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Encoding")))
	if encoding != "gzip" && encoding != "x-gzip" {
		return raw, raw, nil
	}
	reader, err := gzipReader(raw)
	if err != nil {
		return nil, nil, err
	}
	defer reader.Close()
	decoded, err = io.ReadAll(io.LimitReader(reader, bidiRouteLimit+1))
	if len(decoded) > bidiRouteLimit {
		return nil, nil, fmt.Errorf("decoded bidi request too large")
	}
	return raw, decoded, err
}

// existingMode preserves the billing destination of an already claimed stream
// across setting changes. Reading the envelope restores the original wire body.
func (r *bidiRouteRegistry) existingMode(req *http.Request, poll bool) (string, error) {
	// New/unrecognized accounts must reach the normal authorization guard
	// without consuming their request body. Only a previously claimed owner
	// can ask to retain a route across setting changes.
	credential := bidiCredential(req)
	r.mu.Lock()
	knownOwner := false
	for _, entry := range r.entries {
		if entry.mode != "" && entry.credential == credential && (entry.active > 0 || time.Since(entry.touched) <= 2*time.Minute) {
			knownOwner = true
			break
		}
	}
	r.mu.Unlock()
	if !knownOwner {
		return "", nil
	}
	_, decoded, err := readBidiBody(req)
	if err != nil {
		return "", err
	}
	id, err := agent.InspectBidiStream(decoded, req.Header.Get("Content-Type"), poll)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return "", fmt.Errorf("proxy is stopping")
	}
	entry := r.entries[bidiRouteKey(req, id)]
	if entry == nil || (entry.active == 0 && time.Since(entry.touched) > 2*time.Minute) {
		return "", nil
	}
	if entry.credential != bidiCredential(req) {
		return "", fmt.Errorf("bidi stream credentials do not match")
	}
	return entry.mode, nil
}

func bidiEntryError(req *http.Request, err error) *http.Response {
	status := http.StatusBadRequest
	if errors.Is(err, errBidiCapacity) {
		status = http.StatusTooManyRequests
	}
	return bidiRouteError(req, status, err.Error())
}

func bidiRouteError(req *http.Request, status int, message string) *http.Response {
	code := "invalid_argument"
	if status == 429 {
		code = "resource_exhausted"
	}
	if status == 502 {
		code = "unavailable"
	}
	return makeJSONResp(req, status, fmt.Sprintf(`{"code":%q,"message":%q}`, code, message))
}

// append either delivers locally, passes an established native request through,
// or retains the exact original HTTP request until the stream selects a route.
func (r *bidiRouteRegistry) append(req *http.Request, authorizers ...localAuthorizer) (*http.Request, *http.Response) {
	// Before a stream claims the ID, an append is only bounded pending input:
	// it cannot invoke a provider. Native authorization belongs to Cursor;
	// local authorization is required when the local stream claims it and
	// again for late local appends. Never load local account state for native input.
	if len(authorizers) > 0 {
		parts := strings.Fields(req.Header.Get("Authorization"))
		if len(req.Header.Values("Authorization")) != 1 || len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return req, makeJSONResp(req, http.StatusUnauthorized, `{"code":"unauthenticated","message":"Bidi input requires native Cursor credentials."}`)
		}
		if _, blocked := nativePassthroughRequest(req); blocked != nil {
			return req, blocked
		}
	}
	raw, decoded, err := readBidiBody(req)
	if err != nil {
		return req, bidiRouteError(req, 400, err.Error())
	}
	id, seq, err := agent.InspectBidiAppend(decoded, req.Header.Get("Content-Type"))
	if err != nil {
		return req, bidiRouteError(req, 400, err.Error())
	}
	r.mu.Lock()
	previousEntry := r.entries[bidiRouteKey(req, id)]
	entry, err := r.entryLocked(req, id, false)
	if err != nil {
		r.mu.Unlock()
		return req, bidiEntryError(req, err)
	}
	mode := entry.mode
	if mode == "local" && len(authorizers) > 0 {
		// Do not hold the routing mutex while reading/decrypting account state.
		r.mu.Unlock()
		authorize := authorizers[0]
		if authorize == nil || !authorize(req.Context(), req.Header) {
			return req, makeJSONResp(req, http.StatusUnauthorized, `{"code":"unauthenticated","message":"Local inference requires the current Cursor account."}`)
		}
		r.mu.Lock()
		if r.closed || r.entries[bidiRouteKey(req, id)] != entry {
			r.mu.Unlock()
			return req, bidiRouteError(req, 409, "bidi stream is no longer available")
		}
	}
	if !entry.ready {
		mode = "" // Keep new appends behind any pending/retrying replay.
		if _, exists := entry.pending[seq]; !exists {
			stagingFull := false
			if entry.mode == "" {
				_, size := r.stagingUsageLocked()
				stagingFull = size+len(raw) > bidiStagingLimit
			} else if len(entry.pending) < 128 && len(raw) <= bidiRouteLimit {
				for r.queuedBytes+len(raw) > bidiRouteLimit && r.evictStagingLocked() {
				}
			}
			if len(entry.pending) >= 128 || stagingFull || r.queuedBytes+len(raw) > bidiRouteLimit {
				if entry != previousEntry {
					delete(r.entries, bidiRouteKey(req, id))
				}
				r.mu.Unlock()
				return req, bidiRouteError(req, 429, "bidi pending queue is full")
			}
			clone := req.Clone(context.Background())
			clone.Body = nil
			entry.pending[seq] = queuedBidiRequest{clone, raw, seq}
			r.queuedBytes += len(raw)
		}
	}
	r.mu.Unlock()
	switch mode {
	case "native":
		*req = *req.WithContext(context.WithValue(req.Context(), nativeReviewRequestKey{}, true))
		return nativePassthroughRequest(req)
	case "local":
		return req, handleBidiAppend(req)
	default:
		return req, &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{"Content-Type": {"application/proto"}}, Body: io.NopCloser(bytes.NewReader(nil)), ContentLength: 0, Request: req}
	}
}

type bidiRouteLease struct {
	once    sync.Once
	release func()
}

func (l *bidiRouteLease) close() { l.once.Do(l.release) }

type bidiLeaseBody struct {
	io.ReadCloser
	lease *bidiRouteLease
}

func (b *bidiLeaseBody) Close() error { defer b.lease.close(); return b.ReadCloser.Close() }

func (r *bidiRouteRegistry) claim(req *http.Request, mode string, poll bool, serverCtx context.Context, roundTrip func(*http.Request) (*http.Response, error)) (*http.Request, *http.Response, *bidiRouteLease) {
	if mode == "native" {
		forward, blocked := nativePassthroughRequest(req)
		if blocked != nil {
			return req, blocked, nil
		}
		req = forward
	}
	_, decoded, err := readBidiBody(req)
	if err != nil {
		return req, bidiRouteError(req, 400, err.Error()), nil
	}
	id, err := agent.InspectBidiStream(decoded, req.Header.Get("Content-Type"), poll)
	if err != nil {
		return req, bidiRouteError(req, 400, err.Error()), nil
	}
	r.mu.Lock()
	entry, err := r.entryLocked(req, id, true)
	if err != nil {
		r.mu.Unlock()
		return req, bidiEntryError(req, err), nil
	}
	if entry.mode != "" && entry.mode != mode {
		r.mu.Unlock()
		return req, bidiRouteError(req, 409, "bidi request already belongs to another protocol"), nil
	}
	entry.mode = mode
	entry.active++
	r.mu.Unlock()
	lease := &bidiRouteLease{release: func() { r.mu.Lock(); defer r.mu.Unlock(); entry.active--; entry.touched = time.Now() }}
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(serverCtx, cancel)
	defer stop()
	// A stream retry may overlap a previous replay. Serialize it per request
	// without holding the registry lock during network I/O; both waiting and
	// replaying remain cancelable. Keep original bodies accounted for until
	// the upstream actually acknowledges each append's idempotent sequence.
	select {
	case entry.replayGate <- struct{}{}:
		defer func() { <-entry.replayGate }()
	case <-ctx.Done():
		lease.close()
		return req, bidiRouteError(req, 502, "BidiAppend replay canceled"), nil
	}
	for {
		if ctx.Err() != nil {
			lease.close()
			return req, bidiRouteError(req, 502, "BidiAppend replay canceled"), nil
		}
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			lease.close()
			return req, bidiRouteError(req, 502, "proxy is stopping"), nil
		}
		var queued queuedBidiRequest
		found := false
		for _, candidate := range entry.pending {
			if !found || candidate.seq < queued.seq {
				queued, found = candidate, true
			}
		}
		if !found {
			entry.ready = true
			r.mu.Unlock()
			return req, nil, lease
		}
		entry.ready = false
		r.mu.Unlock()
		appendReq := queued.request.Clone(ctx)
		appendReq.Body = io.NopCloser(bytes.NewReader(queued.body))
		var response *http.Response
		if mode == "local" {
			response = handleBidiAppend(appendReq)
		} else {
			forward, blocked := nativePassthroughRequest(appendReq)
			if blocked != nil {
				lease.close()
				return req, blocked, nil
			}
			forward.RequestURI = ""
			response, err = roundTrip(forward)
			if err != nil {
				lease.close()
				return req, bidiRouteError(req, 502, "native BidiAppend replay failed"), nil
			}
		}
		if response == nil {
			lease.close()
			return req, bidiRouteError(req, 502, "missing BidiAppend response"), nil
		}
		if response.Body != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
			_ = response.Body.Close()
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			lease.close()
			return req, bidiRouteError(req, 502, fmt.Sprintf("BidiAppend rejected with status %d", response.StatusCode)), nil
		}
		r.mu.Lock()
		if !r.closed {
			delete(entry.pending, queued.seq)
			r.queuedBytes -= len(queued.body)
		}
		r.mu.Unlock()
	}
}
