package mitm

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"

	"cursor-byok/internal/agent"
	"github.com/coder/websocket"
)

// upgradeResponse bridges websocket.Accept's standard HTTP interface to
// goproxy's synthetic-response interface. goproxy copies the duplex Body after
// forwarding the 101 headers, so no upstream Cursor connection is opened.
type upgradeResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
	conn   net.Conn
}

func (w *upgradeResponse) Header() http.Header    { return w.header }
func (w *upgradeResponse) WriteHeader(status int) { w.status = status }
func (w *upgradeResponse) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.body.Write(b)
}
func (w *upgradeResponse) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

func handleAgentWebSocket(req *http.Request, serverCtx context.Context, resolver agent.AdapterResolver, authorize localAuthorizer) *http.Response {
	acceptedProtocol := false
	for _, p := range strings.Split(req.Header.Get("Sec-WebSocket-Protocol"), ",") {
		p = strings.TrimSpace(p)
		if p == "cursor-agent-v1" || p == "cursor-agent-preconnect-v1" || p == "cursor-run-inference-v1" || p == "cursor-run-inference-preconnect-v1" {
			acceptedProtocol = true
			break
		}
	}
	if !acceptedProtocol {
		return makeJSONResp(req, http.StatusBadRequest, `{"code":"invalid_argument","message":"expected cursor-agent-v1 WebSocket subprotocol"}`)
	}
	proxyEnd, agentEnd := net.Pipe()
	w := &upgradeResponse{header: make(http.Header), conn: agentEnd}
	conn, err := websocket.Accept(w, req, &websocket.AcceptOptions{Subprotocols: []string{"cursor-agent-v1", "cursor-agent-preconnect-v1", "cursor-run-inference-v1", "cursor-run-inference-preconnect-v1"}})
	if err != nil {
		_ = proxyEnd.Close()
		_ = agentEnd.Close()
		return &http.Response{StatusCode: w.status, Status: http.StatusText(w.status), Proto: req.Proto, ProtoMajor: req.ProtoMajor, ProtoMinor: req.ProtoMinor, Header: w.header, Body: io.NopCloser(bytes.NewReader(w.body.Bytes())), ContentLength: int64(w.body.Len()), Request: req}
	}
	ctx, cancel := context.WithCancel(req.Context())
	stop := context.AfterFunc(serverCtx, cancel)
	go func() {
		defer cancel()
		defer stop()
		agent.HandleAgentWebSocket(ctx, conn, resolver, func(headers http.Header) bool {
			if authorize == nil {
				return false
			}
			if len(headers.Values("Authorization")) == 0 {
				headers = req.Header
			}
			return authorize(ctx, headers)
		})
	}()
	// goproxy treats Content-Length: 0 as intentional and does not add chunked
	// transfer encoding to this upgrade response.
	w.header.Set("Content-Length", "0")
	return &http.Response{StatusCode: http.StatusSwitchingProtocols, Status: "101 Switching Protocols", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: w.header, Body: proxyEnd, Request: req}
}
