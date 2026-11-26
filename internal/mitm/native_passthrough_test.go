package mitm

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cursor-byok/internal/certs"
	"github.com/elazarl/goproxy"
)

// This deliberately unsigned sentinel is the non-authenticating local state
// written by older versions, not a credential. Build the fixture from its
// public test claims instead of duplicating a token-shaped literal.
var legacyLocalTokenFixture = func() string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"fake-cursor-local-user","email":"cursor@ai.com","type":"session","iss":"cursor-client","scope":"openid profile email","exp":4070908800}`))
	return header + "." + claims + ".fake-local-state-token"
}()

func nativeTestProxy(t *testing.T, reviewRoute ...string) *goproxy.ProxyHttpServer {
	t.Helper()
	route := ""
	if len(reviewRoute) > 0 {
		route = reviewRoute[0]
	}
	return nativeTestProxyWithAuthorizer(t, route, func(context.Context, http.Header) bool { return true })
}

func nativeTestProxyWithAuthorizer(t *testing.T, reviewRoute string, authorize localAuthorizer) *goproxy.ProxyHttpServer {
	t.Helper()
	ca, err := certs.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := newWithAuthorizer("127.0.0.1:0", ca, nil, nil, func(purpose string) string {
		if purpose == "review-route" {
			return reviewRoute
		}
		return ""
	}, authorize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.cancel)
	p := s.srv.Handler.(*goproxy.ProxyHttpServer)
	t.Cleanup(p.Tr.CloseIdleConnections)
	return p
}

func TestNativeRequestsPreserveOfficialAccountAndPayload(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, contentType, body, response string
	}{
		{"native commit generation", "POST", "/aiserver.v1.AiService/WriteGitCommitMessage", "application/proto", "\x0a\x05+diff", "\x0a\x03fix"},
		{"plugin marketplace", "POST", "/aiserver.v1.DashboardService/ListMarketplacePlugins", "application/proto", "\x08\x01", "\x0a\x04list"},
		{"account profile", "POST", "/aiserver.v1.DashboardService/GetMe", "application/proto", "\x08\x01", "\x0a\x04real"},
		{"real membership", "GET", "/auth/full_stripe_profile", "", "", `{"membershipType":"free"}`},
		{"legacy membership", "GET", "/auth/stripe_profile", "", "", `null`},
		{"updates", "GET", "/updates/api/update/darwin-arm64/cursor/3.32.4/test-machine/stable?forceUpdate=1", "", "", `{"version":"next"}`},
		{"extension control", "GET", "/extensions-control", "", "", `{"malicious":["unsafe.extension"]}`},
		{"future JSON feature", "POST", "/native/settings/future-feature", "application/json", `{"enabled":true}`, `{"enabled":true}`},
		{"future protobuf feature", "POST", "/aiserver.v1.FutureService/SaveSetting", "application/proto", "\x0a\x03new", "\x08\x01"},
		{"cloud followup", "POST", "/aiserver.v1.BackgroundComposerService/AddAsyncFollowupBackgroundComposer", "application/proto", "\x0a\x0ccloud-native", "\x0a\x08accepted"},
		{"cloud status", "POST", "/aiserver.v1.BackgroundComposerService/GetBackgroundComposerStatus", "application/proto", "\x0a\x0ccloud-native", "\x08\x02"},
		{"cloud attach", "POST", "/aiserver.v1.BackgroundComposerService/AttachBackgroundComposer", "application/proto", "\x0a\x0ccloud-native", "\x0a\x06native"},
		{"cloud interactions", "POST", "/aiserver.v1.BackgroundComposerService/StreamInteractionUpdatesSSE", "application/proto", "\x0a\x0ccloud-native", "\x0a\x08approved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(tc.path, "/aiserver.v1.BackgroundComposerService/") && localProxyPath(tc.path) {
				t.Fatal("native cloud method classified as locally executed")
			}
			p := nativeTestProxy(t)
			calls := 0
			p.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
				ctx.RoundTripper = goproxy.RoundTripperFunc(func(got *http.Request, _ *goproxy.ProxyCtx) (*http.Response, error) {
					calls++
					body, err := io.ReadAll(got.Body)
					if err != nil {
						t.Fatal(err)
					}
					if got.Method != tc.method || got.URL.String() != "https://api2.cursor.sh"+tc.path || string(body) != tc.body {
						t.Errorf("native request changed: %s %s body=%q", got.Method, got.URL, body)
					}
					if got.Header.Get("Authorization") != "Bearer test-native-token" || got.Header.Get("Cookie") != "native=test-cookie" || got.Header.Get("Content-Type") != tc.contentType {
						t.Error("native credentials or content type changed")
					}
					if got.Header.Get("Proxy-Authorization") != "" || got.Header.Get("Proxy-Connection") != "" {
						t.Error("local proxy headers escaped upstream")
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {tc.contentType}}, Body: io.NopCloser(strings.NewReader(tc.response)), Request: got}, nil
				})
				return req, nil
			})
			req := httptest.NewRequest(tc.method, "https://api2.cursor.sh"+tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer test-native-token")
			req.Header.Set("Cookie", "native=test-cookie")
			req.Header.Set("Content-Type", tc.contentType)
			req.Header.Set("Proxy-Authorization", "Basic test-proxy-secret")
			req.Header.Set("Proxy-Connection", "keep-alive")
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			if calls != 1 || rec.Code != http.StatusOK || rec.Body.String() != tc.response {
				t.Fatalf("native result was intercepted: calls=%d status=%d body=%q", calls, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestNativePassthroughRejectsLegacyLocalCredentials(t *testing.T) {
	p := nativeTestProxy(t)
	p.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		ctx.RoundTripper = goproxy.RoundTripperFunc(func(*http.Request, *goproxy.ProxyCtx) (*http.Response, error) {
			t.Fatal("synthetic credentials reached upstream")
			return nil, nil
		})
		return req, nil
	})
	for _, auth := range [][]string{
		{"Bearer " + legacyLocalTokenFixture},
		{"bearer\t" + legacyLocalTokenFixture},
		{"Bearer test-native-token", "Bearer " + legacyLocalTokenFixture},
		{"Bearer test-native-token, Bearer " + legacyLocalTokenFixture},
	} {
		req := httptest.NewRequest("GET", "https://api2.cursor.sh/auth/full_stripe_profile", nil)
		req.Header["Authorization"] = auth
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), legacyLocalTokenFixture) {
			t.Fatalf("local credentials were not safely rejected: status=%d", rec.Code)
		}
	}
}

func TestNativePassthroughDoesNotInjectOrMutateCredentials(t *testing.T) {
	req := httptest.NewRequest("GET", "https://api2.cursor.sh/extensions-control", nil)
	req.Header.Set("Proxy-Authorization", "Basic local")
	forward, resp := nativePassthroughRequest(req)
	if resp != nil || forward.Header.Get("Authorization") != "" || forward.Header.Get("Proxy-Authorization") != "" {
		t.Fatal("public request acquired credentials")
	}
	if req.Header.Get("Proxy-Authorization") != "Basic local" {
		t.Fatal("caller request was mutated")
	}
}

func TestNativeUpstreamTLSVerifiesCertificates(t *testing.T) {
	p := nativeTestProxy(t)
	if p.Tr.TLSClientConfig == nil || p.Tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("native upstream TLS certificate verification is disabled")
	}
	if p.Tr.Proxy != nil {
		t.Fatal("upstream transport can route back into the system proxy")
	}
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "verified")
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()
	req, err := http.NewRequest("GET", upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := p.Tr.RoundTrip(req); err == nil {
		resp.Body.Close()
		t.Fatal("untrusted upstream certificate was accepted")
	}
	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	p.Tr.TLSClientConfig.RootCAs = pool
	resp, err := p.Tr.RoundTrip(req.Clone(req.Context()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatal("native upstream cannot negotiate HTTP/2")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(body, []byte("verified")) {
		t.Fatalf("trusted upstream failed: %q %v", body, err)
	}
}
