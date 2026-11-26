package mitm

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"cursor-byok/internal/certs"
	"github.com/coder/websocket"
)

func TestProductionLocalRoutesRejectMissingAccount(t *testing.T) {
	// Missing/malformed Authorization must fail before reading any account
	// database or request payload. Do not use the permissive fixture factory.
	ca, err := certs.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server, err := New("127.0.0.1:0", ca, nil, nil, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop(context.Background())
	for _, path := range []string{
		"/agent.v1.AgentService/RunSSE",
		"/aiserver.v1.InferenceService/RunInferenceSSE",
		"/aiserver.v1.AiService/StreamCpp",
		"/aiserver.v1.BidiService/BidiAppend",
		"/aiserver.v1.AiService/NameTab",
		"/aiserver.v1.AiService/WriteGitBranchName",
	} {
		for _, values := range [][]string{nil, {"Basic invalid"}, {"Bearer"}, {"Bearer one", "Bearer two"}} {
			req := httptest.NewRequest(http.MethodPost, "https://api2.cursor.sh"+path, nil)
			req.Body = io.NopCloser(failRead{t})
			for _, value := range values {
				req.Header.Add("Authorization", value)
			}
			rec := httptest.NewRecorder()
			server.srv.Handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s accepted missing/malformed account: status=%d", path, rec.Code)
			}
		}
	}
}

func testOwnerAuthorization(_ context.Context, h http.Header) bool {
	return len(h.Values("Authorization")) == 1 && h.Get("Authorization") == "Bearer fixture-owner"
}

func TestPaidRoutesRequireOwnerBeforeReadingOrProvider(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"fix/local-auth\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	_, client := testProxyWithAuthorizer(t, provider.URL, testOwnerAuthorization)
	for _, path := range []string{"/aiserver.v1.AiService/StreamCpp", "/aiserver.v1.BidiService/BidiAppend", "/agent.v1.AgentService/RunSSE", "/aiserver.v1.InferenceService/RunInferenceSSE", "/aiserver.v1.AiService/NameTab", "/aiserver.v1.AiService/WriteGitBranchName"} {
		for _, token := range []string{"", "Bearer other-account"} {
			req, _ := http.NewRequest("POST", "https://api2.cursor.sh"+path, bytes.NewBufferString("not protobuf"))
			if token != "" {
				req.Header.Set("Authorization", token)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s returned %d", path, resp.StatusCode)
			}
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unauthorized request invoked provider")
	}
	req, _ := http.NewRequest("POST", "https://api2.cursor.sh/aiserver.v1.AiService/WriteGitBranchName", bytes.NewReader(wsBytes(1, []byte("+local change"))))
	req.Header.Set("Authorization", "Bearer fixture-owner")
	req.Header.Set("Content-Type", "application/proto")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("owner status=%d calls=%d", resp.StatusCode, calls.Load())
	}
}

func TestWebSocketAuthorizesNativeRunStartHeaders(t *testing.T) {
	_, client := testProxyWithAuthorizer(t, "", testOwnerAuthorization)
	for _, protocol := range []string{"cursor-agent-v1", "cursor-agent-preconnect-v1", "cursor-run-inference-v1", "cursor-run-inference-preconnect-v1"} {
		for _, tc := range []struct {
			name, outer, inner string
			accepted           bool
		}{
			{"missing", "", "", false}, {"wrong", "", "Bearer other-account", false},
			{"native-run-header", "", "Bearer fixture-owner", true},
			{"node-upgrade-header", "Bearer fixture-owner", "", true},
			{"inner-cannot-borrow-outer", "Bearer fixture-owner", "Bearer other-account", false},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				h := make(http.Header)
				if tc.outer != "" {
					h.Set("Authorization", tc.outer)
				}
				conn, _, err := websocket.Dial(ctx, "wss://api2.cursor.sh/agent/v1/run", &websocket.DialOptions{HTTPClient: client, HTTPHeader: h, Subprotocols: []string{protocol}})
				if err != nil {
					t.Fatal(err)
				}
				defer conn.CloseNow()
				if err = conn.Write(ctx, websocket.MessageBinary, []byte{10, 2, 8, 1}); err != nil {
					t.Fatal(err)
				}
				if _, _, err = conn.Read(ctx); err != nil {
					t.Fatal(err)
				}
				start := wsBytes(1, wsID("auth-run", "auth-request"))
				if tc.inner != "" {
					header := append(wsBytes(1, []byte("authorization")), wsBytes(2, []byte(tc.inner))...)
					start = append(start, wsBytes(2, header)...)
				}
				if err = conn.Write(ctx, websocket.MessageBinary, wsBytes(2, start)); err != nil {
					t.Fatal(err)
				}
				_, reply, err := conn.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if tc.accepted {
					if wsField(t, reply, 2) == nil {
						t.Fatalf("owner not accepted: %x", reply)
					}
				} else {
					rejection := wsField(t, reply, 5)
					if rejection == nil || !bytes.HasPrefix(wsField(t, rejection, 2), []byte{8, 16}) {
						t.Fatalf("missing unauthenticated rejection: %x", reply)
					}
				}
			})
		}
	}
}
