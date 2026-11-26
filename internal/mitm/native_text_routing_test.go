package mitm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cursor-byok/internal/agent"
	"cursor-byok/internal/certs"
	aiserverv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
	"google.golang.org/protobuf/proto"
)

func TestNameTabUsesActiveProviderAndBranchUsesCommitOverride(t *testing.T) {
	type providerRequest struct{ provider, body string }
	requests := make(chan providerRequest, 3)
	newProvider := func(name, text string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			requests <- providerRequest{name, string(body)}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", text)
		}))
	}
	active := newProvider("active", "Chat title")
	defer active.Close()
	commit := newProvider("commit", "fix/git-changes")
	defer commit.Close()
	ca, err := certs.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server, err := newWithAuthorizer("127.0.0.1:0", ca, nil, func() []agent.AdapterTarget {
		return []agent.AdapterTarget{
			{BaseURL: active.URL, APIKey: "test-active", Model: "active-model", StableID: "active-id"},
			{BaseURL: commit.URL, APIKey: "test-commit", Model: "commit-model", StableID: "commit-id"},
		}
	}, func(purpose string) string {
		if purpose != "commit" {
			t.Errorf("unexpected model override purpose %q", purpose)
		}
		return "commit-id"
	}, func(context.Context, http.Header) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop(context.Background())
	for _, tc := range []struct {
		method, wantProvider, wantPrompt string
		request                          proto.Message
	}{
		{"NameTab", "active", "private-chat-topic", &aiserverv1.NameTabRequest{Messages: []*aiserverv1.ConversationMessage{{Text: "private-chat-topic"}}}},
		{"WriteGitBranchName", "commit", "git-change", &aiserverv1.WriteGitBranchNameRequest{Diffs: "+git-change"}},
	} {
		t.Run(tc.method, func(t *testing.T) {
			body, err := proto.Marshal(tc.request)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "https://api2.cursor.sh/aiserver.v1.AiService/"+tc.method, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/proto")
			response := httptest.NewRecorder()
			server.srv.Handler.ServeHTTP(response, req)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body)
			}
			got := <-requests
			if got.provider != tc.wantProvider || !strings.Contains(got.body, tc.wantPrompt) {
				t.Fatalf("wrong provider or missing prompt: provider=%s want=%s", got.provider, tc.wantProvider)
			}
			if got.provider == "commit" && strings.Contains(got.body, "private-chat-topic") {
				t.Fatal("conversation content leaked to the commit provider")
			}
		})
	}
}
