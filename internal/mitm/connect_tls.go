package mitm

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/elazarl/goproxy"
	"golang.org/x/net/http2"
)

type tunnelResponseKey struct{}
type tunnelResponse struct{ response *http.Response }

// connectTLSAction terminates both HTTP versions before dispatching individual
// requests through the same proxy filters. goproxy's AllowHTTP2 frame relay
// bypasses those filters, including local BYOK routes and credential guards.
func connectTLSAction(serverCtx context.Context, proxy *goproxy.ProxyHttpServer, tlsConfig func(string, *goproxy.ProxyCtx) (*tls.Config, error)) *goproxy.ConnectAction {
	// goproxy copies response bodies but omits HTTP trailers. Capture the final
	// response so the inner HTTP server can emit native gRPC status trailers.
	proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		if ctx != nil && ctx.Req != nil {
			if capture, ok := ctx.Req.Context().Value(tunnelResponseKey{}).(*tunnelResponse); ok {
				capture.response = resp
			}
		}
		return resp
	})
	return &goproxy.ConnectAction{
		Action: goproxy.ConnectHijack,
		Hijack: func(req *http.Request, raw net.Conn, ctx *goproxy.ProxyCtx) {
			go serveCursorTLS(serverCtx, proxy, tlsConfig, req, raw, ctx)
		},
	}
}

func serveCursorTLS(serverCtx context.Context, proxy *goproxy.ProxyHttpServer, tlsConfig func(string, *goproxy.ProxyCtx) (*tls.Config, error), connect *http.Request, raw net.Conn, proxyCtx *goproxy.ProxyCtx) {
	defer raw.Close()
	stop := context.AfterFunc(serverCtx, func() { _ = raw.Close() })
	defer stop()
	config, err := tlsConfig(connect.Host, proxyCtx)
	if err != nil {
		_, _ = fmt.Fprint(raw, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	config = config.Clone()
	// Keep existing browser/WebSocket connections on HTTP/1.1, while accepting
	// native RPC clients that offer only h2.
	config.NextProtos = []string{"http/1.1", http2.NextProtoTLS}
	if _, err := fmt.Fprint(raw, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	conn := tls.Server(raw, config)
	handshakeCtx, cancel := context.WithTimeout(serverCtx, 30*time.Second)
	err = conn.HandshakeContext(handshakeCtx)
	cancel()
	if err != nil {
		return
	}
	state := conn.ConnectionState()
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Reject a raw h2 preface sent without h2 negotiation. In particular,
		// never send PRI/CONNECT into goproxy's HTTP/1-only hijack machinery.
		if req.Method == "PRI" {
			if hijacker, ok := w.(http.Hijacker); ok {
				if c, _, err := hijacker.Hijack(); err == nil {
					_ = c.Close()
					return
				}
			}
			http.Error(w, "invalid HTTP preface", http.StatusBadRequest)
			return
		}
		if req.Method == http.MethodConnect || req.URL == nil || !sameTunnelAuthority(req.Host, connect.Host) || req.URL.User != nil || (req.URL.Host != "" && (!sameTunnelAuthority(req.URL.Host, connect.Host) || req.URL.Scheme != "https")) {
			http.Error(w, "invalid tunnel request", http.StatusBadRequest)
			return
		}
		capture := &tunnelResponse{}
		forward := req.Clone(context.WithValue(req.Context(), tunnelResponseKey{}, capture))
		forward.URL.Scheme = "https"
		forward.URL.Host = connect.Host
		forward.TLS = &state
		var responseWriter http.ResponseWriter = w
		if req.ProtoMajor == 2 {
			// Native gRPC streams do not carry text/event-stream headers. Flush
			// each copied chunk instead of waiting for the stream to terminate.
			responseWriter = flushingTunnelWriter{ResponseWriter: w}
		} else {
			http1 := &http1TunnelWriter{ResponseWriter: w}
			defer http1.closeHijacked()
			responseWriter = http1
		}
		proxy.ServeHTTP(responseWriter, forward)
		if capture.response != nil {
			for key, values := range capture.response.Trailer {
				w.Header()[http.TrailerPrefix+key] = append([]string(nil), values...)
			}
		}
	})
	base := &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second, BaseContext: func(net.Listener) context.Context { return serverCtx }}
	if state.NegotiatedProtocol == http2.NextProtoTLS {
		(&http2.Server{}).ServeConn(conn, &http2.ServeConnOpts{Context: serverCtx, BaseConfig: base, Handler: handler})
		return
	}
	tracked := &tunnelConn{Conn: conn, closed: make(chan struct{})}
	_ = base.Serve(&tunnelListener{conn: tracked})
}

func sameTunnelAuthority(first, second string) bool {
	canonical := func(authority string) string {
		host, port, err := net.SplitHostPort(authority)
		if err != nil {
			if strings.Contains(authority, ":") || authority == "" {
				return ""
			}
			host, port = authority, "443"
		}
		return net.JoinHostPort(strings.ToLower(host), port)
	}
	first = canonical(first)
	return first != "" && first == canonical(second)
}

type flushingTunnelWriter struct{ http.ResponseWriter }

func (w flushingTunnelWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}

func (w flushingTunnelWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// A one-connection listener lets net/http preserve ordinary HTTP/1 keepalive,
// cancellation and WebSocket hijacking without implementing another parser.
type tunnelConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *tunnelConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}

type tunnelListener struct {
	conn     *tunnelConn
	accepted bool
}

func (l *tunnelListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return l.conn, nil
	}
	<-l.conn.closed
	return nil, net.ErrClosed
}
func (l *tunnelListener) Close() error   { return l.conn.Close() }
func (l *tunnelListener) Addr() net.Addr { return l.conn.LocalAddr() }
