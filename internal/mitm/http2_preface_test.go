package mitm

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

func connectCursorTLS(t *testing.T, s *Server, protocols []string) *tls.Conn {
	t.Helper()
	tcp, err := net.DialTimeout("tcp", s.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tcp.Close() })
	_ = tcp.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err = fmt.Fprint(tcp, "CONNECT api2.cursor.sh:443 HTTP/1.1\r\nHost: api2.cursor.sh:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tcp), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status %d", resp.StatusCode)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(proxyTestCA.ca.CertPEM())
	conn := tls.Client(tcp, &tls.Config{ServerName: "api2.cursor.sh", RootCAs: roots, NextProtos: protocols})
	if err = conn.HandshakeContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestCursorTLSNegotiatesHTTP1(t *testing.T) {
	s, _ := testProxy(t, "")
	conn := connectCursorTLS(t, s, []string{"h2", "http/1.1"})
	if got := conn.ConnectionState().NegotiatedProtocol; got != "http/1.1" {
		t.Fatalf("ALPN = %q", got)
	}
}

func TestHTTP2PrefaceDoesNotCrashProxy(t *testing.T) {
	// Reproduce the actual Electron request with DEBUG on too: logging used to
	// dereference nil URL before the routing code got a chance to reject it.
	t.Setenv("DEBUG", "1")
	s, client := testProxy(t, "")
	conn := connectCursorTLS(t, s, nil)
	if _, err := fmt.Fprint(conn, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := conn.Read(b[:]); err == nil {
		t.Fatal("expected unsupported HTTP/2 connection to close")
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("HTTP/2 rejection hung")
	}
	// A panic in goproxy's hijacked goroutine would terminate this process.
	// Verify an independent request still reaches our local handler.
	resp, err := client.Get("https://api2.cursor.sh/agent/v1/run")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("proxy no longer serving: status %d", resp.StatusCode)
	}
}
