package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	aiserverv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
)

func TestLegacyAnthropicToolLoopsPreserveSignedContent(t *testing.T) {
	for _, service := range []string{"agent", "bugbot"} {
		t.Run(service, func(t *testing.T) {
			expected := []map[string]any{
				{"type": "thinking", "thinking": "inspect first", "signature": "signed-thought"},
				{"type": "redacted_thinking", "data": "opaque-data"},
				{"type": "text", "text": "checking"},
				{"type": "tool_use", "id": "tool_signed", "name": "unknown_test_tool", "input": map[string]any{}},
			}
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Messages []struct {
						Role    string           `json:"role"`
						Content []map[string]any `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if calls.Add(1) == 1 {
					for index, block := range expected {
						event, _ := json.Marshal(map[string]any{"type": "content_block_start", "index": index, "content_block": block})
						fmt.Fprintf(w, "data: %s\n\n", event)
					}
					fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
					return
				}
				if len(request.Messages) < 3 {
					t.Errorf("tool-round history missing: %+v", request.Messages)
					return
				}
				assistant := request.Messages[len(request.Messages)-2]
				result := request.Messages[len(request.Messages)-1]
				if assistant.Role != "assistant" || !reflect.DeepEqual(assistant.Content, expected) {
					t.Errorf("signed assistant content changed: %#v", assistant)
				}
				if result.Role != "user" || len(result.Content) != 1 || result.Content[0]["type"] != "tool_result" || result.Content[0]["tool_use_id"] != "tool_signed" {
					t.Errorf("tool result missing or mismatched: %#v", result)
				}
				fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"NO_ISSUES\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer provider.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			session := &Session{RequestID: t.Name(), UserText: "Review this change", BugBotRequest: &aiserverv1.StreamBugBotRequest{}}
			opts := AdapterOpts{ThinkingBudget: 2048}
			if service == "agent" {
				runAgentSession(ctx, session.RequestID, session, io.Discard, func() []AdapterTarget {
					return []AdapterTarget{{ProviderType: "anthropic", BaseURL: provider.URL, APIKey: "test-key", Model: "claude-test", Opts: opts}}
				})
			} else {
				response, err := runBugBotReview(ctx, session, provider.URL, "test-key", "claude-test", opts, streamAnthropic, io.Discard)
				if err != nil || response.GetStatus().GetStatus() != aiserverv1.BugBotStatus_STATUS_DONE {
					t.Fatalf("review did not finish: response=%v err=%v", response, err)
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("expected two provider rounds, got %d", calls.Load())
			}
		})
	}
}

func TestLegacyAnthropicHistoryReplaysStoredSignedContent(t *testing.T) {
	native := json.RawMessage(`[{"type":"thinking","thinking":"","signature":"history-signature"},{"type":"redacted_thinking","data":"history-opaque"},{"type":"text","text":"saved"},{"type":"tool_use","id":"history-tool","name":"Read","input":{"path":"a.txt"}}]`)
	original := textMessage("assistant", "saved")
	original.AnthropicContent = native
	original.ToolCalls = []openAIToolCallMsg{{ID: "history-tool", Type: "function", Function: openAIToolCallFn{Name: "Read", Arguments: `{"path":"a.txt"}`}}}
	saved, err := json.Marshal(openAIToStored(original))
	if err != nil {
		t.Fatal(err)
	}
	var stored StoredMessage
	if err := json.Unmarshal(saved, &stored); err != nil {
		t.Fatal(err)
	}
	restored := storedToOpenAI(stored)
	_, messages, err := convertMessagesToAnthropic([]openAIMessage{restored, toolResultMessage("history-tool", "Read", "file content")})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	var request []struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(encoded, &request); err != nil {
		t.Fatal(err)
	}
	if len(request) != 2 || string(request[0].Content) != string(native) {
		t.Fatalf("stored signed content was changed: %s", encoded)
	}
	var old StoredMessage
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":"old history"}`), &old); err != nil {
		t.Fatal(err)
	}
	if got := storedToOpenAI(old); len(got.AnthropicContent) != 0 || string(got.Content) != `"old history"` {
		t.Fatalf("legacy history no longer loads: %+v", got)
	}
}
