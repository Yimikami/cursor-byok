package mitm

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elazarl/goproxy"
)

func TestConnectIgnoresInheritedProxyEnvironment(t *testing.T) {
	var inherited atomic.Int32
	proxyEnv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { inherited.Add(1); w.WriteHeader(502) }))
	defer proxyEnv.Close()
	for _, key := range []string{"HTTPS_PROXY", "https_proxy"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("HTTPS_PROXY", "")
			t.Setenv("https_proxy", "")
			t.Setenv(key, proxyEnv.URL)
			server, _ := testProxy(t, "")
			var direct atomic.Int32
			server.srv.Handler.(*goproxy.ProxyHttpServer).Tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				direct.Add(1)
				a, b := net.Pipe()
				go func() { defer b.Close(); _, _ = io.Copy(b, b) }()
				return a, nil
			}
			conn, err := net.Dial("tcp", server.ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			_, _ = fmt.Fprint(conn, "CONNECT auth.cursor.sh:443 HTTP/1.1\r\nHost: auth.cursor.sh:443\r\n\r\n")
			reader := bufio.NewReader(conn)
			response, err := http.ReadResponse(reader, &http.Request{Method: "CONNECT"})
			if err != nil || response.StatusCode != 200 {
				t.Fatalf("CONNECT: %v, %v", response, err)
			}
			_, _ = conn.Write([]byte("PING"))
			got := make([]byte, 4)
			if _, err = io.ReadFull(reader, got); err != nil || string(got) != "PING" {
				t.Fatalf("direct tunnel: %q %v", got, err)
			}
			if direct.Load() != 1 || inherited.Load() != 0 {
				t.Fatalf("direct=%d inherited=%d", direct.Load(), inherited.Load())
			}
		})
	}
}
