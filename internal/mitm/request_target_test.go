package mitm

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// Exercise the actual CONNECT parser; constructing an ordinary http.Request
// bypasses goproxy's failed URL reconstruction and misses the Windows crash.
func TestAsteriskRequestDoesNotCrashMITM(t *testing.T) {
	for _, tc := range []struct {
		request string
		status  int
	}{
		{"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n", 0},
		{"OPTIONS * HTTP/1.1\r\nHost: api2.cursor.sh\r\n\r\n", http.StatusBadRequest},
	} {
		t.Run(tc.request[:3], func(t *testing.T) {
			s, client := testProxy(t, "")
			conn, err := net.DialTimeout("tcp", s.ln.Addr().String(), 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			_, _ = fmt.Fprint(conn, "CONNECT api2.cursor.sh:443 HTTP/1.1\r\nHost: api2.cursor.sh:443\r\n\r\n")
			resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("CONNECT: %v %v", resp, err)
			}
			cfg := client.Transport.(*http.Transport).TLSClientConfig.Clone()
			cfg.ServerName = "api2.cursor.sh"
			tlsConn := tls.Client(conn, cfg)
			if err := tlsConn.Handshake(); err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(tlsConn, tc.request)
			resp, err = http.ReadResponse(bufio.NewReader(tlsConn), nil)
			if tc.status == 0 {
				if err == nil {
					_ = resp.Body.Close()
					t.Fatal("HTTP/2 preface without negotiated h2 was accepted")
				}
				if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
					t.Fatal("HTTP/2 preface rejection hung")
				}
			} else if err != nil {
				t.Fatal(err)
			} else {
				_ = resp.Body.Close()
				if resp.StatusCode != tc.status {
					t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
				}
			}
			client.Timeout = 5 * time.Second
			resp, err = client.Get("https://api2.cursor.sh/agent/v1/run")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("subsequent request = %d", resp.StatusCode)
			}
		})
	}
}
