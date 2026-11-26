package mitm

import (
	"bytes"
	"context"
	"crypto/x509"
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

// Both hops use real TLS. The local provider must remain unused on success,
// account/quota rejection and transient native failure alike.
func TestNativeAuxiliaryRoutesThroughTLSWithoutBYOKFallback(t *testing.T) {
	paths := []string{
		"/aiserver.v1.AiService/NameTab",
		"/aiserver.v1.AiService/WriteGitBranchName",
		"/aiserver.v1.CppService/AvailableModels",
		"/aiserver.v1.AiService/CppConfig",
		"/aiserver.v1.AiService/StreamCpp",
	}
	for _, path := range paths {
		for _, status := range []int{200, 403, 429, 503} {
			t.Run(fmt.Sprintf("%s/%d", path, status), func(t *testing.T) {
				if localProxyPath(path) || paidLocalPath(path) {
					t.Fatal("native auxiliary route still classified as local")
				}
				var providerCalls, nativeCalls atomic.Int32
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					providerCalls.Add(1)
					w.WriteHeader(http.StatusInternalServerError)
				}))
				defer provider.Close()
				payload := []byte{10, 5, 'i', 'n', 'p', 'u', 't'}
				response := []byte("native response or error, unchanged")
				upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					nativeCalls.Add(1)
					body, err := io.ReadAll(r.Body)
					if err != nil || !bytes.Equal(body, payload) || r.URL.Path != path || r.Host != "api2.cursor.sh" || r.Method != http.MethodPost {
						t.Error("native request payload or destination changed")
					}
					if r.Header.Get("Authorization") != "Bearer native-owner" || r.Header.Get("Cookie") != "native=session" || r.Header.Get("Content-Type") != "application/connect+proto" {
						t.Error("native credentials or framing changed")
					}
					w.Header().Set("Content-Type", "application/connect+proto")
					w.Header().Set("X-Native-Response", "preserved")
					w.WriteHeader(status)
					_, _ = w.Write(response)
				}))
				defer upstream.Close()
				server, client := testProxyWithAuthorizer(t, provider.URL, func(context.Context, http.Header) bool {
					t.Error("native route attempted local BYOK authorization")
					return false
				})
				p := server.srv.Handler.(*goproxy.ProxyHttpServer)
				pool := x509.NewCertPool()
				pool.AddCert(upstream.Certificate())
				p.Tr.TLSClientConfig.RootCAs = pool
				p.Tr.TLSClientConfig.ServerName = "127.0.0.1"
				p.Tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api2.cursor.sh"+path, bytes.NewReader(payload))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer native-owner")
				req.Header.Set("Cookie", "native=session")
				req.Header.Set("Content-Type", "application/connect+proto")
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil || resp.StatusCode != status || !bytes.Equal(body, response) || resp.Header.Get("X-Native-Response") != "preserved" {
					t.Fatalf("native response changed: status=%d err=%v", resp.StatusCode, err)
				}
				if nativeCalls.Load() != 1 || providerCalls.Load() != 0 {
					t.Fatalf("native calls=%d, BYOK calls=%d", nativeCalls.Load(), providerCalls.Load())
				}
			})
		}
	}
}
