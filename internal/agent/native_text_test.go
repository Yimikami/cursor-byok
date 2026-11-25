package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	aiserverv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestNativeTextUnaryContracts(t *testing.T) {
	cases := []struct {
		name, path, contentType, output string
		request                         proto.Message
		response                        proto.Message
		prompt                          []string
	}{
		{"commit-proto", "/aiserver.v1.AiService/WriteGitCommitMessage", "application/proto", "fix: prevent empty replies",
			&aiserverv1.WriteGitCommitMessageRequest{Diffs: []string{"+return error"}, PreviousCommitMessages: []string{"fix: preserve settings"}, ExplicitContext: &aiserverv1.ExplicitContext{Context: "Fail closed"}},
			&aiserverv1.WriteGitCommitMessageResponse{CommitMessage: "fix: prevent empty replies"}, []string{"+return error", "fix: preserve settings", "Fail closed"}},
		{"commit-json", "/aiserver.v1.AiService/WriteGitCommitMessage", "application/json", "fix: preserve settings",
			&aiserverv1.WriteGitCommitMessageRequest{Diffs: []string{"+restore backup"}}, &aiserverv1.WriteGitCommitMessageResponse{CommitMessage: "fix: preserve settings"}, []string{"+restore backup"}},
		{"branch", "/aiserver.v1.AiService/WriteGitBranchName", "application/proto", "fix/restore-settings",
			&aiserverv1.WriteGitBranchNameRequest{Diffs: "+restore backup", Context: proto.String("Safe shutdown")}, &aiserverv1.WriteGitBranchNameResponse{BranchName: "fix/restore-settings"}, []string{"+restore backup", "Safe shutdown"}},
		{"title", "/aiserver.v1.AiService/NameTab", "application/json", "Repair Cursor connection",
			&aiserverv1.NameTabRequest{Messages: []*aiserverv1.ConversationMessage{{Text: "Cursor does not connect"}, {Text: "Check the certificate"}}}, &aiserverv1.NameTabResponse{Name: "Repair Cursor connection"}, []string{"Cursor does not connect", "Check the certificate"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer configured-key" {
					t.Errorf("configured credentials were not used")
				}
				var request openAIRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if request.Model != "selected-model" {
					t.Errorf("model = %q", request.Model)
				}
				if len(request.Messages) != 2 {
					t.Fatalf("messages = %d", len(request.Messages))
				}
				for _, wanted := range tc.prompt {
					if !strings.Contains(string(request.Messages[1].Content), wanted) {
						t.Errorf("missing prompt context %q", wanted)
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				event, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": tc.output, "reasoning_content": "private analysis"}, "finish_reason": "stop"}}})
				fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", event)
			}))
			defer server.Close()
			resolve := func() []AdapterTarget {
				return []AdapterTarget{{Model: "wrong-default"}, {Model: "selected-model", StableID: "selected-id", BaseURL: server.URL, APIKey: "configured-key"}}
			}
			var raw []byte
			var err error
			if tc.contentType == "application/json" {
				raw, err = protojson.Marshal(tc.request)
			} else {
				raw, err = proto.Marshal(tc.request)
				// Cursor 3.32.4 adds credential fields after the retained wire
				// fields. Unknown values must never override configured secrets.
				raw = append(raw, providerProtoBytes(4, providerProtoString(1, "untrusted-client-key"))...)
			}
			if err != nil {
				t.Fatal(err)
			}
			var result Result
			if strings.HasSuffix(tc.path, "WriteGitCommitMessage") {
				result = HandleWriteGitCommitMessage(context.Background(), raw, tc.contentType, resolve, "selected-id")
			} else {
				result = HandleNativeText(context.Background(), tc.path, raw, tc.contentType, resolve, "selected-id")
			}
			if result.Status != http.StatusOK || result.ContentType != tc.contentType || calls != 1 {
				t.Fatalf("result=%+v calls=%d", result, calls)
			}
			got := tc.response.ProtoReflect().New().Interface()
			if result.ContentType == "application/json" {
				err = protojson.Unmarshal(result.Body, got)
			} else {
				err = proto.Unmarshal(result.Body, got)
			}
			if err != nil || !proto.Equal(got, tc.response) {
				t.Fatalf("response=%v err=%v wanted=%v", got, err, tc.response)
			}
		})
	}
}

func TestNativeTextRejectsIncompleteProviderResponses(t *testing.T) {
	for _, payload := range []string{
		`data: {"error":{"code":"capacity_unavailable","message":"try later"}}` + "\n\n",
		`data: {"choices":[{"delta":{"content":"Partial answer"}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"content":"Incomplete answer"},"finish_reason":"length"}]}` + "\n\n",
		"data: [DONE]\n\n",
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, payload)
		}))
		resolve := func() []AdapterTarget {
			return []AdapterTarget{{Model: "model", BaseURL: server.URL, APIKey: "test-key"}}
		}
		result := HandleWriteGitCommitMessage(context.Background(), []byte(`{"diffs":["+change"]}`), "application/json", resolve, "")
		server.Close()
		if result.Status != http.StatusBadGateway {
			t.Errorf("incomplete response succeeded: %s", payload)
		}
	}
}

func TestNativeTextValidatesInputsAndBranchOutput(t *testing.T) {
	resolve := func() []AdapterTarget { t.Fatal("invalid request resolved an adapter"); return nil }
	for _, tc := range []struct{ method, body string }{
		{"NameTab", `{}`}, {"NameTab", `{"messages":[{"text":" "}]}`},
		{"WriteGitBranchName", `{}`}, {"WriteGitBranchName", `{`},
	} {
		result := HandleNativeText(context.Background(), "/aiserver.v1.AiService/"+tc.method, []byte(tc.body), "application/json", resolve, "")
		if result.Status != http.StatusBadRequest {
			t.Errorf("invalid %s request succeeded", tc.method)
		}
	}
	for _, bad := range []string{"", "--flag", "branch with spaces", "a..b", "refs/.hidden", "a.lock", "a//b", "a@{b", "a\nb", "a~b", "a/"} {
		if validateBranchName(bad) == nil {
			t.Errorf("invalid branch accepted: %q", bad)
		}
	}
	for _, good := range []string{"fix/restore-settings", "feature-1", "user/repo/update"} {
		if err := validateBranchName(good); err != nil {
			t.Errorf("valid branch rejected: %s", good)
		}
	}
}

func TestNativeTextAnthropicDisablesOversizedThinkingBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request anthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Thinking != nil || request.MaxTokens != 120 {
			t.Errorf("short text request has incompatible thinking config: %+v", request)
		}
		if r.Header.Get("x-api-key") != "configured-key" {
			t.Error("Anthropic credential missing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"fix/restore-settings\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":8}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()
	resolve := func() []AdapterTarget {
		return []AdapterTarget{{ProviderType: "anthropic", BaseURL: server.URL, APIKey: "configured-key", Model: "test-model", Opts: AdapterOpts{MaxOutputTokens: 4096, ThinkingBudget: 1024}}}
	}
	for _, method := range []string{"WriteGitCommitMessage", "WriteGitBranchName"} {
		var result Result
		if method == "WriteGitCommitMessage" {
			result = HandleWriteGitCommitMessage(context.Background(), []byte("{\"diffs\":[\"+restore settings\"]}"), "application/json", resolve, "")
		} else {
			result = HandleNativeText(context.Background(), "/aiserver.v1.AiService/"+method, []byte("{\"diffs\":\"+restore settings\"}"), "application/json", resolve, "")
		}
		if result.Status != http.StatusOK {
			t.Fatalf("%s failed: %s", method, result.Body)
		}
	}
}

func TestNativeTitleRejectsAnAnswerAndOmitsAssistantStatusClaims(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request openAIRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if len(request.Messages) != 2 || !strings.Contains(string(request.Messages[1].Content), "Test native MCP tools") ||
			strings.Contains(string(request.Messages[1].Content), "MCP tools unavailable") {
			t.Errorf("title prompt lost topic or inherited stale assistant claims: %+v", request.Messages)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"MCP tools unavailable so I could not complete the requested test\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	request := &aiserverv1.NameTabRequest{Messages: []*aiserverv1.ConversationMessage{
		{Type: aiserverv1.ConversationMessage_MESSAGE_TYPE_HUMAN, Text: "Test native MCP tools"},
		{Type: aiserverv1.ConversationMessage_MESSAGE_TYPE_AI, Text: "MCP tools unavailable"},
	}}
	body, _ := proto.Marshal(request)
	resolve := func() []AdapterTarget {
		return []AdapterTarget{{Model: "test-model", APIKey: "test-key", BaseURL: server.URL}}
	}
	result := HandleNativeText(context.Background(), "/aiserver.v1.AiService/NameTab", body, "application/proto", resolve, "")
	if result.Status != http.StatusBadGateway || !strings.Contains(string(result.Body), "invalid conversation title") {
		t.Fatalf("invalid title reported as success: %s", result.Body)
	}
	for _, invalid := range []string{"", "line one\nline two", strings.Repeat("a", 81), "one two three four five six seven eight nine"} {
		if validConversationTitle(invalid) {
			t.Errorf("invalid title accepted: %q", invalid)
		}
	}
	if !validConversationTitle("Yerel MCP araçlarının testi") {
		t.Fatal("valid Turkish title rejected")
	}
}
