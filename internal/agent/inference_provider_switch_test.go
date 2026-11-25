package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	inferencev1 "cursor-byok/internal/protocodec/gen/inference/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestInferenceSwitchFromOpenAIReasoningToAnthropic(t *testing.T) {
	for _, ordered := range []bool{false, true} {
		t.Run(fmt.Sprintf("ordered=%v", ordered), func(t *testing.T) {
			var openAICalls, anthropicCalls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if r.URL.Path == "/chat/completions" {
					openAICalls.Add(1)
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"Need to inspect the file.\",\"content\":\"Reading\"}}]}\n\n")
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-read\",\"function\":{\"name\":\"Read\",\"arguments\":\"{\\\"path\\\":\\\"a.txt\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
					return
				}
				if r.URL.Path != "/v1/messages" {
					t.Errorf("unexpected provider path %s", r.URL.Path)
					return
				}
				anthropicCalls.Add(1)
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
				if len(request.Messages) != 3 || request.Messages[1].Role != "assistant" || request.Messages[2].Role != "user" {
					t.Errorf("wrong continuation messages: %+v", request)
					return
				}
				expected := []map[string]any{
					{"type": "text", "text": "Reading"},
					{"type": "tool_use", "id": "call-read", "name": "Read", "input": map[string]any{"path": "a.txt"}},
				}
				if !reflect.DeepEqual(request.Messages[1].Content, expected) {
					t.Errorf("foreign reasoning or changed tool call in Anthropic history: %#v", request.Messages[1].Content)
				}
				results := request.Messages[2].Content
				if len(results) != 1 || results[0]["type"] != "tool_result" || results[0]["tool_use_id"] != "call-read" || results[0]["content"] != "file contents" {
					t.Errorf("tool result changed: %#v", results)
				}
				fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"Finished\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer provider.Close()
			var first *inferencev1.InferenceResponseMessage
			if err := runInferenceProvider(context.Background(), providerTestRequest(), AdapterTarget{ProviderType: "openai", BaseURL: provider.URL, APIKey: "test-key", Model: "reasoner"}, func(raw []byte) error {
				var event inferencev1.InferenceStreamResponse
				if err := proto.Unmarshal(raw, &event); err != nil {
					return err
				}
				if info := event.GetResponseInfo(); info != nil && len(info.Messages) > 0 {
					first = info.Messages[0]
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if first == nil || len(first.ReasoningParts) != 1 || first.ReasoningParts[0].GetSignature() != "" || first.ReasoningParts[0].GetText() != "Need to inspect the file." || first.GetAnthropicNativeContent() != "" {
				t.Fatalf("first invocation did not produce generic reasoning: %v", first)
			}
			assistant := &inferencev1.InferenceCoreMessage{Role: first.Role, Content: &inferencev1.InferenceCoreMessage_Text{Text: first.GetContent()}, ToolCalls: first.ToolCalls, ReasoningParts: first.ReasoningParts, AssistantPartOrder: first.AssistantPartOrder, AssistantTextParts: first.AssistantTextParts, AnthropicNativeContent: first.AnthropicNativeContent}
			if ordered {
				assistant.AssistantPartOrder = []int32{1, 2, 3}
			}
			request := &inferencev1.InferenceStreamRequest{}
			if err := proto.Unmarshal(providerTestRequest(), request); err != nil {
				t.Fatal(err)
			}
			request.Messages = append(request.Messages, assistant, &inferencev1.InferenceCoreMessage{Role: inferencev1.InferenceMessageRole_INFERENCE_MESSAGE_ROLE_TOOL, Content: &inferencev1.InferenceCoreMessage_ToolContent{ToolContent: &inferencev1.InferenceToolResultContent{Parts: []*inferencev1.InferenceToolResultPart{{ToolCallId: "call-read", ToolName: "Read", Result: structpb.NewStringValue("file contents")}}}}})
			encoded, err := proto.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			if err := runInferenceProvider(context.Background(), encoded, AdapterTarget{ProviderType: "anthropic", BaseURL: provider.URL, APIKey: "test-key", Model: "claude-test"}, func([]byte) error { return nil }); err != nil {
				t.Fatalf("second invocation after switching providers failed: %v", err)
			}
			if openAICalls.Load() != 1 || anthropicCalls.Load() != 1 {
				t.Fatalf("provider calls openai=%d anthropic=%d", openAICalls.Load(), anthropicCalls.Load())
			}
		})
	}
}

func TestInferenceMixedReasoningPreservesAnthropicBlockOrder(t *testing.T) {
	args, _ := structpb.NewStruct(map[string]any{"path": "a.txt"})
	message := &inferencev1.InferenceCoreMessage{
		Role: inferencev1.InferenceMessageRole_INFERENCE_MESSAGE_ROLE_ASSISTANT,
		ReasoningParts: []*inferencev1.InferenceReasoningPart{
			{Text: "foreign reasoning"},
			{Text: "signed reasoning", Signature: proto.String("original-signature")},
			{IsRedacted: true, RedactedData: proto.String("original-opaque-data")},
		},
		AssistantTextParts: []string{"between blocks"},
		ToolCalls:          []*inferencev1.InferenceToolCall{{ToolCallId: "call-read", ToolName: "Read", Args: args}},
		AssistantPartOrder: []int32{1, 1, 2, 1, 3},
	}
	request := &inferencev1.InferenceStreamRequest{Messages: []*inferencev1.InferenceCoreMessage{message}}
	messages, _, _, err := translateLocalInferenceRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].ReasoningContent != "foreign reasoningsigned reasoning" {
		t.Fatal("generic reasoning was not retained for compatible providers")
	}
	if err := validateAnthropicNativeContent(messages[0].AnthropicNativeContent); err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(messages[0].AnthropicNativeContent, &got); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"type": "thinking", "thinking": "signed reasoning", "signature": "original-signature"},
		{"type": "text", "text": "between blocks"},
		{"type": "redacted_thinking", "data": "original-opaque-data"},
		{"type": "tool_use", "id": "call-read", "name": "Read", "input": map[string]any{"path": "a.txt"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("signed/redacted history changed: %#v", got)
	}
}
