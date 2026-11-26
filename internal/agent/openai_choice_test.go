package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	inferencev1 "cursor-byok/internal/protocodec/gen/inference/v1"
	"google.golang.org/protobuf/proto"
)

func openAIChoiceFixture(events []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func TestOpenAIStreamIsolatesChoiceZeroTextReasoningAndFinish(t *testing.T) {
	server := openAIChoiceFixture([]string{
		`{"choices":[{"index":1,"delta":{"content":"other answer ","reasoning_content":"other thought "}},{"index":0,"delta":{"content":"selected ","reasoning_content":"selected thought "}}]}`,
		// Some compatible providers omit index for the sole/default choice.
		`{"choices":[{"delta":{"content":"answer","reasoning_content":"continued"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		// Usage is response-wide, even when attached to an ignored choice.
		`{"choices":[{"index":1,"delta":{"content":"discarded suffix"},"finish_reason":"length"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
	})
	defer server.Close()
	var texts, thoughts []string
	finals := 0
	result, err := streamOpenAI(context.Background(), server.URL, "fixture-key", "fixture-model", []openAIMessage{textMessage("user", "fixture")}, nil, AdapterOpts{}, func(text, thought string, done bool) error {
		if text != "" {
			texts = append(texts, text)
		}
		if thought != "" {
			thoughts = append(thoughts, thought)
		}
		if done {
			finals++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(texts, []string{"selected ", "answer"}) {
		t.Errorf("alternative text leaked into selected deltas: %q", texts)
	}
	if !reflect.DeepEqual(thoughts, []string{"selected thought ", "continued"}) || len(result.ReasoningParts) != 1 || result.ReasoningParts[0].Text != "selected thought continued" {
		t.Errorf("alternative reasoning leaked: deltas=%q final=%+v", thoughts, result.ReasoningParts)
	}
	if result.FinishReason != "stop" || finals != 1 {
		t.Errorf("alternative finish reason affected selected choice: reason=%q finals=%d", result.FinishReason, finals)
	}
	if result.Usage == nil || *result.Usage != (openAIUsage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18}) {
		t.Errorf("response-wide usage lost: %+v", result.Usage)
	}
}

func TestOpenAIStreamIsolatesChoiceZeroToolArguments(t *testing.T) {
	server := openAIChoiceFixture([]string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"selected-call","type":"function","function":{"name":"Read","arguments":"{\"path\":"}}]}},{"index":1,"delta":{"tool_calls":[{"index":0,"id":"other-call","type":"function","function":{"name":"Write","arguments":"{\"contents\":"}}]}}]}`,
		`{"choices":[{"index":1,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"unwanted\"}"}},{"index":1,"id":"extra-call","function":{"name":"Delete","arguments":"{\"path\":\"unwanted\"}"}}]},"finish_reason":"tool_calls"},{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"selected.txt\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"index":1,"delta":{},"finish_reason":"stop"}]}`,
	})
	defer server.Close()
	result, err := streamOpenAI(context.Background(), server.URL, "fixture-key", "fixture-model", []openAIMessage{textMessage("user", "fixture")}, nil, AdapterOpts{}, func(string, string, bool) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	want := []PendingToolCall{{ID: "selected-call", Name: "Read", Arguments: `{"path":"selected.txt"}`}}
	if !reflect.DeepEqual(result.ToolCalls, want) || result.FinishReason != "tool_calls" {
		t.Fatalf("alternative tool arguments or finish reason leaked: calls=%+v finish=%q", result.ToolCalls, result.FinishReason)
	}
}

func TestLocalInferenceKeepsExactTextDeltasAndIntentionalRepetition(t *testing.T) {
	wantDeltas := []string{"H3 is rejected ", "because ", "the value is ", "very ", "very small."}
	var events []string
	for _, delta := range wantDeltas {
		event, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": delta}}}})
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, string(event))
	}
	events = append(events, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	server := openAIChoiceFixture(events)
	defer server.Close()
	var deltas []string
	var finalContent string
	var finalMarkers, responseInfos int
	err := runInferenceProvider(context.Background(), providerTestRequest(), AdapterTarget{BaseURL: server.URL, APIKey: "fixture-key", Model: "fixture-model"}, func(raw []byte) error {
		response := &inferencev1.InferenceStreamResponse{}
		if err := proto.Unmarshal(raw, response); err != nil {
			return err
		}
		if part := response.GetTextPart(); part != nil {
			if part.GetIsFinal() {
				finalMarkers++
				if part.GetText() != "" {
					t.Error("finish marker repeated response text")
				}
			} else {
				deltas = append(deltas, part.GetText())
			}
		}
		if info := response.GetResponseInfo(); info != nil {
			responseInfos++
			if len(info.GetMessages()) != 1 {
				t.Errorf("final response messages=%d, want 1", len(info.GetMessages()))
			} else {
				finalContent = info.GetMessages()[0].GetContent()
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deltas, wantDeltas) || finalContent != strings.Join(wantDeltas, "") || finalMarkers != 1 || responseInfos != 1 {
		t.Fatalf("native text changed: deltas=%q final=%q finishMarkers=%d responseInfos=%d", deltas, finalContent, finalMarkers, responseInfos)
	}
}

func TestInferenceProviderOnlyAcceptsSelectedChoiceCompletion(t *testing.T) {
	for _, tc := range []struct {
		name       string
		completion string
		wantError  bool
	}{
		{"other-choice", `{"index":1,"delta":{},"finish_reason":"stop"}`, true},
		{"negative-choice", `{"index":-1,"delta":{},"finish_reason":"stop"}`, true},
		{"selected-choice", `{"index":0,"delta":{},"finish_reason":"stop"}`, false},
		{"default-choice", `{"delta":{},"finish_reason":"stop"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial answer\"}}]}\n\n")
				fmt.Fprintf(w, "data: {\"choices\":[%s],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n", tc.completion)
				// EOF without [DONE]: only the selected choice's finish marker
				// can establish that the selected response completed.
			}))
			defer server.Close()
			var text strings.Builder
			var responseInfos, finalMarkers int
			var usage *inferencev1.InferenceUsageInfo
			err := runInferenceProvider(context.Background(), providerTestRequest(), AdapterTarget{BaseURL: server.URL, APIKey: "fixture-key", Model: "fixture-model"}, func(raw []byte) error {
				response := &inferencev1.InferenceStreamResponse{}
				if err := proto.Unmarshal(raw, response); err != nil {
					return err
				}
				if part := response.GetTextPart(); part != nil {
					text.WriteString(part.GetText())
					if part.GetIsFinal() {
						finalMarkers++
					}
				}
				if response.GetResponseInfo() != nil {
					responseInfos++
				}
				if response.GetUsage() != nil {
					usage = response.GetUsage()
				}
				return nil
			})
			if text.String() != "partial answer" {
				t.Errorf("selected text changed: %q", text.String())
			}
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "before a completion marker") {
					t.Errorf("other choice certified a partial response: %v", err)
				}
				if responseInfos != 0 || finalMarkers != 0 {
					t.Errorf("partial response published as complete: responses=%d finalMarkers=%d", responseInfos, finalMarkers)
				}
			} else {
				if err != nil || responseInfos != 1 || finalMarkers != 1 {
					t.Fatalf("selected completion rejected: error=%v responses=%d finalMarkers=%d", err, responseInfos, finalMarkers)
				}
				if usage.GetPromptTokens() != 3 || usage.GetCompletionTokens() != 2 || usage.GetTotalTokens() != 5 {
					t.Errorf("usage changed: %+v", usage)
				}
			}
		})
	}
}
