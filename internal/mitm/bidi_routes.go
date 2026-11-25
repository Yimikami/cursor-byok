package mitm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"cursor-byok/internal/agent"
)

const bidiRouteLimit = 64 << 20

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
	case "/aiserver.v1.AiService/CppConfig", "/aiserver.v1.AiService/StreamCpp", "/aiserver.v1.CppService/AvailableModels":
		return true
	case "/aiserver.v1.AiService/GetDefaultModel", "/agent/v1/run", "/aiserver.v1.AiService/AvailableModels", "/aiserver.v1.AiService/GetDefaultModelNudgeData", "/aiserver.v1.AiService/NameTab", "/aiserver.v1.AiService/WriteGitBranchName", "/aiserver.v1.AiService/WriteGitCommitMessage":
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

func (r *bidiRouteRegistry) entryLocked(req *http.Request, id string) (*bidiRoute, error) {
	if r.closed {
		return nil, fmt.Errorf("proxy is stopping")
	}
	now := time.Now()
	for key, entry := range r.entries {
		if entry.active == 0 && now.Sub(entry.touched) > 2*time.Minute {
			for _, p := range entry.pending {
				r.queuedBytes -= len(p.body)
			}
			delete(r.entries, key)
		}
	}
	key := bidiRouteKey(req, id)
	entry := r.entries[key]
	if entry == nil {
		if len(r.entries) >= 256 {
			return nil, fmt.Errorf("too many bidi requests")
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
func (r *bidiRouteRegistry) append(req *http.Request) (*http.Request, *http.Response) {
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
	entry, err := r.entryLocked(req, id)
	if err != nil {
		r.mu.Unlock()
		return req, bidiRouteError(req, 400, err.Error())
	}
	mode := entry.mode
	if !entry.ready {
		mode = "" // Keep new appends behind any pending/retrying replay.
		if _, exists := entry.pending[seq]; !exists {
			if len(entry.pending) >= 128 || r.queuedBytes+len(raw) > bidiRouteLimit {
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
	entry, err := r.entryLocked(req, id)
	if err != nil {
		r.mu.Unlock()
		return req, bidiRouteError(req, 400, err.Error()), nil
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
