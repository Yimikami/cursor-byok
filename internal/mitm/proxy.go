package mitm

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"cursor-byok/internal/agent"
	"cursor-byok/internal/certs"
	"cursor-byok/internal/relay"

	"github.com/elazarl/goproxy"
)

func gzipReader(b []byte) (io.ReadCloser, error) {
	return gzip.NewReader(bytes.NewReader(b))
}

// Only the API host carries the RPCs translated to the user's BYOK provider.
// Authentication, marketplace downloads and unrelated applications retain
// their original end-to-end TLS connections.
var cursorAPI2Hosts = map[string]struct{}{"api2.cursor.sh": {}}

// normalizeHost lowercases a host and strips any port. Cursor connects with a
// lowercase hostname on :443, but normalizing keeps the CONNECT-time MITM
// decision (shouldMITMHost) and the request-time routing decision (the
// DoFunc handler) consistent — otherwise a differently-cased or non-443
// Cursor host could be MITM'd at one layer yet fall through to the upstream
// at the other.
func normalizeHost(host string) string {
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	return strings.ToLower(hostname)
}

func isCursorAPI2Host(host string) bool {
	_, ok := cursorAPI2Hosts[normalizeHost(host)]
	return ok
}

// shouldMITMHost reports whether a CONNECT target (host or host:port) is one
// of Cursor's own hosts we must intercept. Every other host is tunnelled
// straight through untouched.
//
// This matters because enabling the system proxy points *all* of the
// machine's HTTPS at this listener, not just Cursor's. MITMing every host
// would present our self-signed CA for sites we have no reason to touch and
// break any app that pins certificates or doesn't trust our CA — e.g. Discord
// fails to connect while the proxy is running (issue #4). Restricting MITM to
// Cursor's hosts lets unrelated traffic reach its real upstream and validate
// the genuine server certificate as usual.
func shouldMITMHost(host string) bool {
	return isCursorAPI2Host(host)
}

type Server struct {
	addr   string
	srv    *http.Server
	ln     net.Listener
	gw     *relay.Gateway
	cancel context.CancelFunc
}

// agentResolver lets the agent package fetch the user's first BYOK adapter
// at request time. The bridge package wires this in so MITM doesn't need to
// know about UserConfig parsing — we just ask "give me a usable provider".
type AgentResolver = agent.AdapterResolver

func New(addr string, ca *certs.CA, gw *relay.Gateway, resolver AgentResolver, selectedModel func(string) string) (*Server, error) {
	return newWithAuthorizer(addr, ca, gw, resolver, selectedModel, authorizeLocalRequest)
}

func newWithAuthorizer(addr string, ca *certs.CA, gw *relay.Gateway, resolver AgentResolver, selectedModel func(string) string, authorize localAuthorizer) (*Server, error) {
	tlsCert, err := tls.X509KeyPair(ca.CertPEM(), ca.KeyPEM())
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		return nil, err
	}
	tlsCert.Leaf = leaf

	signTLS := goproxy.TLSConfigFromCA(&tlsCert)
	tlsCfg := func(host string, ctx *goproxy.ProxyCtx) (*tls.Config, error) {
		cfg, err := signTLS(host, ctx)
		if err != nil {
			return nil, err
		}
		return cfg, nil
	}

	serverCtx, cancel := context.WithCancel(context.Background())
	bidiRoutes := newBidiRouteRegistry()
	context.AfterFunc(serverCtx, bidiRoutes.close)
	p := goproxy.NewProxyHttpServer()
	// CONNECT has a separate environment-derived dialer from Transport.Proxy.
	// A system proxy pointing back here must never recursively tunnel to itself.
	p.ConnectDial = nil
	// goproxy defaults to skipping upstream certificate verification. Native
	// requests now carry the user's real account credentials, so validate the
	// upstream certificate using the system roots and requested hostname.
	p.Tr = &http.Transport{
		// The OS already routes requests through this server. Consulting proxy
		// environment variables here can send upstream requests back to us.
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	debug := os.Getenv("DEBUG") == "1"
	// Keep goproxy's full-URL logging off: native authentication requests may
	// carry sensitive query parameters. Our diagnostics log host/path only.
	p.Verbose = false
	p.AllowHTTP2 = false
	mitmAction := connectTLSAction(serverCtx, p, tlsCfg)
	tunnelAction := &goproxy.ConnectAction{Action: goproxy.ConnectAccept}
	// Only decrypt Cursor's own hosts; tunnel everything else straight to its
	// real upstream. The system proxy routes the whole machine's HTTPS here,
	// so intercepting every host would break unrelated apps that pin certs or
	// don't trust our CA (issue #4). See shouldMITMHost.
	p.OnRequest().HandleConnectFunc(func(host string, _ *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
		if shouldMITMHost(host) {
			return mitmAction, host
		}
		return tunnelAction, host
	})

	// Intercept only the explicitly implemented BYOK routes. All other native
	// requests retain their real account, response and entitlement semantics.
	// Synthetic raw-protobuf responses cloned from the working app captures.
	// Each entry is a hand-rolled body the gateway can produce on demand
	// (so it can be parameterised by the user's BYOK adapter list). We send
	// these with Content-Type: application/proto, no Connect envelope —
	// mirrors the captured working-app response shape exactly.
	syntheticAPI2Paths := map[string]struct{}{
		"/aiserver.v1.AiService/GetDefaultModel":          {},
		"/aiserver.v1.AiService/AvailableModels":          {},
		"/aiserver.v1.AiService/GetDefaultModelNudgeData": {},
	}
	mockProto := func(req *http.Request, body []byte) *http.Response {
		contentType := "application/proto"
		if strings.HasPrefix(req.Header.Get("Content-Type"), "application/connect+proto") {
			contentType = "application/connect+proto"
			framed := binary.BigEndian.AppendUint32([]byte{0}, uint32(len(body)))
			framed = append(framed, body...)
			body = append(framed, 2, 0, 0, 0, 2, '{', '}')
		}
		if debug {
			log.Printf("[MOCK PROTO] 200 %s (%d bytes)", req.URL.Path, len(body))
		}
		return &http.Response{
			Status:        "200 OK",
			StatusCode:    http.StatusOK,
			Proto:         req.Proto,
			ProtoMajor:    req.ProtoMajor,
			ProtoMinor:    req.ProtoMinor,
			Body:          io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)),
			Header: http.Header{
				"Content-Type":                 {contentType},
				"Access-Control-Allow-Origin":  {"*"},
				"Access-Control-Allow-Methods": {"*"},
				"Access-Control-Allow-Headers": {"*"},
			},
			Request: req,
		}
	}
	p.OnRequest().DoFunc(func(req *http.Request, proxyCtx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		// goproxy invokes filters before rejecting an HTTP/2 preface or a
		// failed URL reconstruction. PRI * can therefore arrive with URL=nil.
		// Let its HTTP/2 guard close that connection, before logging/routing
		// dereferences URL. This also handles clients that ignore ALPN.
		if req.Method == "PRI" {
			return req, nil
		}
		if req.URL == nil {
			return req, makeJSONResp(req, http.StatusBadRequest, `{"code":"invalid_argument","message":"invalid request URL"}`)
		}
		if debug {
			log.Printf("[REQ] %s %s%s (CT: %s)", req.Method, req.URL.Host, req.URL.Path, req.Header.Get("Content-Type"))
		}
		if req.Method == http.MethodOptions && req.Header.Get("Access-Control-Request-Method") != "" && isCursorAPI2Host(req.URL.Host) && localProxyPath(req.URL.Path) {
			return req, &http.Response{
				StatusCode: http.StatusNoContent,
				Status:     "204 No Content",
				Proto:      req.Proto,
				ProtoMajor: req.ProtoMajor,
				ProtoMinor: req.ProtoMinor,
				Header: http.Header{
					"Access-Control-Allow-Origin":  {"*"},
					"Access-Control-Allow-Methods": {"GET,POST,PUT,PATCH,DELETE,OPTIONS"},
					"Access-Control-Allow-Headers": {"*"},
					"Access-Control-Max-Age":       {"86400"},
				},
				Body:    io.NopCloser(strings.NewReader("")),
				Request: req,
			}
		}
		path := req.URL.Path
		isAPI2Host := isCursorAPI2Host(req.URL.Host)
		if isAPI2Host {
			if req.Method == http.MethodOptions {
				return nativePassthroughRequest(req)
			}
			if paidLocalPath(path) && (authorize == nil || !authorize(req.Context(), req.Header)) {
				return req, makeJSONResp(req, http.StatusUnauthorized, `{"code":"unauthenticated","message":"Local inference requires the current Cursor account."}`)
			}
			if unsupportedLocalBidiPoll(path) {
				return req, makeJSONResp(req, http.StatusNotImplemented, `{"code":"unimplemented","message":"Local BYOK polling is not supported; use WebSocket or SSE"}`)
			}
			if path == "/aiserver.v1.BidiService/BidiAppend" {
				return bidiRoutes.append(req)
			}
			poll, native := nativeBidiStreams[path]
			if localBidiStream(path) || native {
				mode := "local"
				if native {
					mode = "native"
				}
				forward, response, lease := bidiRoutes.claim(req, mode, poll, serverCtx, proxyCtx.RoundTrip)
				if response != nil {
					return forward, response
				}
				req = forward
				proxyCtx.UserData = lease
				if native {
					previous := proxyCtx.RoundTripper
					proxyCtx.RoundTripper = goproxy.RoundTripperFunc(func(request *http.Request, ctx *goproxy.ProxyCtx) (*http.Response, error) {
						var response *http.Response
						var err error
						if previous != nil {
							response, err = previous.RoundTrip(request, ctx)
						} else {
							response, err = p.Tr.RoundTrip(request)
						}
						if err != nil {
							lease.close()
						}
						return response, err
					})
					return req, nil
				}
			}
			if path == "/agent/v1/run" {
				return req, handleAgentWebSocket(req, serverCtx, resolver, authorize)
			}
			if _, ok := syntheticAPI2Paths[path]; ok && gw != nil {
				if body := gw.SyntheticPath(path); body != nil {
					return req, mockProto(req, body)
				}
			}
			if path == "/agent.v1.AgentService/RunSSE" {
				return req, handleRunSSE(req, serverCtx, resolver)
			}
			if path == "/aiserver.v1.InferenceService/RunInferenceSSE" {
				return req, handleInferenceSSE(req, serverCtx, resolver)
			}
			if path == "/aiserver.v1.AiService/NameTab" {
				return req, handleNativeText(req, resolver, "")
			}
			if path == "/aiserver.v1.AiService/WriteGitBranchName" {
				return req, handleNativeText(req, resolver, selectedModel("commit"))
			}
			if path == "/aiserver.v1.AiService/WriteGitCommitMessage" {
				return req, handleWriteGitCommitMessage(req, resolver, selectedModel("commit"))
			}
			if path == "/aiserver.v1.AiService/StreamBugBotAgenticSSE" {
				return req, handleBugBotRunSSE(req, serverCtx, resolver, selectedModel("review"))
			}
			return nativePassthroughRequest(req)
		}
		return req, nil
	})

	p.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		var req *http.Request
		if ctx != nil {
			req = ctx.Req
		}
		if ctx != nil {
			if lease, ok := ctx.UserData.(*bidiRouteLease); ok && lease != nil {
				if resp == nil || resp.Body == nil {
					lease.close()
				} else {
					resp.Body = &bidiLeaseBody{ReadCloser: resp.Body, lease: lease}
				}
			}
		}
		if gw != nil {
			gw.MaybeRewriteResponse(req, resp)
		}
		return resp
	})

	return &Server{
		addr:   addr,
		cancel: cancel,
		gw:     gw,
		srv:    &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second},
	}, nil
}

// handleBidiAppend reads the full request body (Cursor sends BidiAppend as a
// unary POST), hands it to the agent package, and packages the result as a
// goproxy-compatible http.Response.
func handleBidiAppend(req *http.Request) *http.Response {
	body, err := readDecodedBody(req)
	if err != nil {
		return makeJSONResp(req, http.StatusBadRequest, `{"code":"invalid_argument","message":"read body: `+err.Error()+`"}`)
	}
	res := agent.HandleBidiAppend(body, req.Header.Get("Content-Type"))
	hdr := http.Header{}
	if res.ContentType != "" {
		hdr.Set("Content-Type", res.ContentType)
	}
	hdr.Set("Access-Control-Allow-Origin", "*")
	hdr.Set("Access-Control-Allow-Methods", "*")
	hdr.Set("Access-Control-Allow-Headers", "*")
	return &http.Response{
		Status:        http.StatusText(res.Status),
		StatusCode:    res.Status,
		Proto:         req.Proto,
		ProtoMajor:    req.ProtoMajor,
		ProtoMinor:    req.ProtoMinor,
		Body:          io.NopCloser(bytes.NewReader(res.Body)),
		ContentLength: int64(len(res.Body)),
		Header:        hdr,
		Request:       req,
	}
}

// handleRunSSE returns a streaming http.Response whose Body is an io.Pipe;
// a goroutine reads the request body once, calls into the agent package,
// and writes Connect SSE frames to the pipe writer. goproxy copies the
// pipe reader to the client connection so each frame reaches Cursor as
// soon as it's written (no buffering at our layer).
func handleRunSSE(req *http.Request, serverCtx context.Context, resolver agent.AdapterResolver) *http.Response {
	return streamAgentResponse(req, serverCtx, agent.RunSSEHeaders, func(ctx context.Context, body []byte, w io.Writer) {
		agent.HandleRunSSE(ctx, body, req.Header.Get("Content-Type"), w, resolver)
	})
}

func handleWriteGitCommitMessage(req *http.Request, resolver agent.AdapterResolver, selectedModel string) *http.Response {
	body, err := readDecodedBody(req)
	if err != nil {
		return makeJSONResp(req, http.StatusBadRequest, `{"code":"invalid_argument","message":"read body: `+err.Error()+`"}`)
	}
	res := agent.HandleWriteGitCommitMessage(req.Context(), body, req.Header.Get("Content-Type"), resolver, selectedModel)
	hdr := http.Header{}
	if res.ContentType != "" {
		hdr.Set("Content-Type", res.ContentType)
	}
	return &http.Response{
		Status:        http.StatusText(res.Status),
		StatusCode:    res.Status,
		Proto:         req.Proto,
		ProtoMajor:    req.ProtoMajor,
		ProtoMinor:    req.ProtoMinor,
		Body:          io.NopCloser(bytes.NewReader(res.Body)),
		ContentLength: int64(len(res.Body)),
		Header:        hdr,
		Request:       req,
	}
}

func handleBugBotRunSSE(req *http.Request, serverCtx context.Context, resolver agent.AdapterResolver, selectedModel string) *http.Response {
	return streamAgentResponse(req, serverCtx, agent.BugBotSSEHeaders, func(ctx context.Context, body []byte, w io.Writer) {
		agent.HandleBugBotRunSSE(ctx, body, req.Header.Get("Content-Type"), w, resolver, selectedModel)
	})
}

func handleBackgroundComposerAttach(req *http.Request, resolver agent.AdapterResolver) *http.Response {
	body, err := readDecodedBody(req)
	if err != nil {
		return makeJSONResp(req, http.StatusBadRequest, `{"code":"invalid_argument","message":"read body: `+err.Error()+`"}`)
	}
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		agent.HandleBackgroundComposerAttach(req.Context(), body, req.Header.Get("Content-Type"), pw, resolver)
	}()
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Proto:      req.Proto,
		ProtoMajor: req.ProtoMajor,
		ProtoMinor: req.ProtoMinor,
		Body:       pr,
		Header: http.Header{
			"Content-Type":             {"text/event-stream"},
			"Cache-Control":            {"no-cache"},
			"Connect-Content-Encoding": {"gzip"},
			"Connect-Accept-Encoding":  {"gzip"},
		},
		Request: req,
	}
}

func handleBackgroundComposerInteractionUpdates(req *http.Request) *http.Response {
	body, err := readDecodedBody(req)
	if err != nil {
		return makeJSONResp(req, http.StatusBadRequest, `{"code":"invalid_argument","message":"read body: `+err.Error()+`"}`)
	}
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		agent.HandleBackgroundComposerInteractionUpdates(req.Context(), body, req.Header.Get("Content-Type"), pw)
	}()
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Proto:      req.Proto,
		ProtoMajor: req.ProtoMajor,
		ProtoMinor: req.ProtoMinor,
		Body:       pr,
		Header: http.Header{
			"Content-Type":             {"text/event-stream"},
			"Cache-Control":            {"no-cache"},
			"Connect-Content-Encoding": {"gzip"},
			"Connect-Accept-Encoding":  {"gzip"},
		},
		Request: req,
	}
}

func handleBackgroundComposerAddFollowup(req *http.Request) *http.Response {
	body, err := readDecodedBody(req)
	if err != nil {
		return makeJSONResp(req, http.StatusBadRequest, `{"code":"invalid_argument","message":"read body: `+err.Error()+`"}`)
	}
	res := agent.HandleAddAsyncBackgroundComposer(body, req.Header.Get("Content-Type"))
	hdr := http.Header{}
	if res.ContentType != "" {
		hdr.Set("Content-Type", res.ContentType)
	}
	return &http.Response{Status: http.StatusText(res.Status), StatusCode: res.Status, Proto: req.Proto, ProtoMajor: req.ProtoMajor, ProtoMinor: req.ProtoMinor, Body: io.NopCloser(bytes.NewReader(res.Body)), ContentLength: int64(len(res.Body)), Header: hdr, Request: req}
}

func handleBackgroundComposerStatus(req *http.Request) *http.Response {
	body, err := readDecodedBody(req)
	if err != nil {
		return makeJSONResp(req, http.StatusBadRequest, `{"code":"invalid_argument","message":"read body: `+err.Error()+`"}`)
	}
	res := agent.HandleGetBackgroundComposerStatus(body, req.Header.Get("Content-Type"))
	hdr := http.Header{}
	if res.ContentType != "" {
		hdr.Set("Content-Type", res.ContentType)
	}
	return &http.Response{Status: http.StatusText(res.Status), StatusCode: res.Status, Proto: req.Proto, ProtoMajor: req.ProtoMajor, ProtoMinor: req.ProtoMinor, Body: io.NopCloser(bytes.NewReader(res.Body)), ContentLength: int64(len(res.Body)), Header: hdr, Request: req}
}

// readDecodedBody reads req.Body and transparently un-gzips when Cursor
// shipped the payload with HTTP-level gzip (Content-Encoding: gzip). The
// raw transport here is decoupled from any Connect envelope/compression
// the inner protobuf might also carry — that's still done by the agent
// package via decodeUnary.
const maxLocalRequestBody = 32 << 20

func readDecodedBody(req *http.Request) ([]byte, error) {
	return readDecodedBodyLimit(req, maxLocalRequestBody)
}

func readDecodedBodyLimit(req *http.Request, limit int64) ([]byte, error) {
	if req.Body == nil {
		return nil, errors.New("missing request body")
	}
	defer req.Body.Close()
	if req.ContentLength > limit {
		return nil, errors.New("request body too large")
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, limit+1))
	if int64(len(raw)) > limit {
		return nil, errors.New("request body too large")
	}
	if err != nil {
		return nil, err
	}
	enc := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Encoding")))
	if enc == "gzip" || enc == "x-gzip" {
		zr, gerr := gzipReader(raw)
		if gerr != nil {
			return nil, gerr
		}
		defer zr.Close()
		out, rerr := io.ReadAll(io.LimitReader(zr, limit+1))
		if int64(len(out)) > limit {
			return nil, errors.New("decoded request body too large")
		}
		if rerr != nil {
			return nil, rerr
		}
		return out, nil
	}
	return raw, nil
}

func makeJSONResp(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		Status:        http.StatusText(status),
		StatusCode:    status,
		Proto:         req.Proto,
		ProtoMajor:    req.ProtoMajor,
		ProtoMinor:    req.ProtoMinor,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Header:        http.Header{"Content-Type": {"application/json"}},
		Request:       req,
	}
}

func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.ln = ln
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	if s.cancel != nil {
		s.cancel()
	}
	if s.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}
