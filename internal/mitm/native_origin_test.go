package mitm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestLocalRoutesRejectBrowserOriginsBeforeDispatch(t *testing.T) {
	proxy := nativeTestProxy(t)
	paths := []string{
		"/agent/v1/run",
		"/agent.v1.AgentService/RunSSE",
		"/aiserver.v1.InferenceService/RunInferenceSSE",
		"/aiserver.v1.BidiService/BidiAppend",
		"/aiserver.v1.AiService/NameTab",
		"/aiserver.v1.AiService/WriteGitBranchName",
		"/aiserver.v1.AiService/CppConfig",
		"/aiserver.v1.AiService/StreamCpp",
	}
	origins := []string{"https://attacker.example", "null", "file://", "https://vscode-app", cursorRendererOrigin + ".attacker.example", ""}
	for _, path := range paths {
		for _, origin := range origins {
			for _, method := range []string{http.MethodPost, http.MethodOptions} {
				req := httptest.NewRequest(method, "https://api2.cursor.sh"+path, strings.NewReader("not parsed"))
				req.Header.Set("Origin", origin)
				req.Header.Set("Access-Control-Request-Method", http.MethodPost)
				rec := httptest.NewRecorder()
				proxy.ServeHTTP(rec, req)
				if rec.Code != http.StatusForbidden || rec.Header().Get("Access-Control-Allow-Origin") != "" {
					t.Fatalf("%s %s origin=%q: status=%d cors=%q", method, path, origin, rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
				}
			}
		}
	}
}

func TestNativeOriginScope(t *testing.T) {
	proxy := nativeTestProxy(t)
	for _, tc := range []struct {
		name      string
		origins   []string
		fetchSite string
		want      int
	}{
		{"native-node", nil, "", 204},
		{"native-renderer", []string{cursorRendererOrigin}, "cross-site", 204},
		{"duplicate-origin", []string{cursorRendererOrigin, "https://attacker.example"}, "", 403},
		{"opaque-cross-site", nil, "cross-site", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodOptions, "https://api2.cursor.sh/aiserver.v1.InferenceService/RunInferenceSSE", nil)
			for _, origin := range tc.origins {
				req.Header.Add("Origin", origin)
			}
			req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			req.Header.Set("Access-Control-Request-Method", http.MethodPost)
			rec := httptest.NewRecorder()
			proxy.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d", rec.Code)
			}
			wantOrigin := ""
			if tc.name == "native-renderer" {
				wantOrigin = cursorRendererOrigin
			}
			if rec.Header().Get("Access-Control-Allow-Origin") != wantOrigin {
				t.Fatalf("CORS origin=%q, want %q", rec.Header().Get("Access-Control-Allow-Origin"), wantOrigin)
			}
		})
	}
}

func TestNativeRendererWebSocketClosesTunnelWithoutStoppingProxy(t *testing.T) {
	server, client := testProxy(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "wss://api2.cursor.sh/agent/v1/run", &websocket.DialOptions{
		HTTPClient: client, Subprotocols: []string{"cursor-run-inference-v1"},
		HTTPHeader: http.Header{"Origin": {cursorRendererOrigin}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// An ordinary close, without proxy Stop, must release the hijacked TLS
	// connection and both bridge copies.
	if err := conn.CloseNow(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		server.connections.Lock()
		remaining := len(server.connections.connections)
		server.connections.Unlock()
		if remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d owned sockets leaked after WebSocket close", remaining)
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := client.Get("https://api2.cursor.sh/agent/v1/run")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("proxy no longer serving: %d", resp.StatusCode)
	}
}
