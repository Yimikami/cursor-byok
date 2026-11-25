package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	inferencev1 "cursor-byok/internal/protocodec/gen/inference/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

func nativeReasoningRequest(t *testing.T, fixture string) *inferencev1.InferenceStreamRequest {
	t.Helper()
	request := &inferencev1.InferenceStreamRequest{}
	if err := protojson.Unmarshal([]byte(fixture), request); err != nil {
		t.Fatal(err)
	}
	return request
}

func TestNativeParallelToolImagesFollowAllResults(t *testing.T) {
	request := nativeReasoningRequest(t, `{"messages":[{"role":"INFERENCE_MESSAGE_ROLE_ASSISTANT","toolCalls":[{"toolCallId":"image","toolName":"capture","args":{}},{"toolCallId":"read","toolName":"read_file","args":{}}]},{"role":"INFERENCE_MESSAGE_ROLE_TOOL","toolContent":{"parts":[{"toolCallId":"image","toolName":"capture","result":"captured","experimentalContent":[{"image":{"data":"aW1hZ2U=","mimeType":"image/png"}}]}]}},{"role":"INFERENCE_MESSAGE_ROLE_TOOL","toolContent":{"parts":[{"toolCallId":"read","toolName":"read_file","result":"contents"}]}},{"role":"INFERENCE_MESSAGE_ROLE_USER","text":"Continue"}]}`)
	messages, _, _, err := translateLocalInferenceRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, message := range messages {
		roles = append(roles, message.Role)
	}
	if got := strings.Join(roles, ","); got != "assistant,tool,tool,user,user" {
		t.Fatalf("tool batch interrupted by image: %s", got)
	}
	if messages[1].ToolCallID != "image" || messages[2].ToolCallID != "read" || !bytes.Contains(messages[3].Content, []byte("data:image/png;base64,aW1hZ2U=")) {
		t.Fatal("tool result identity or image content lost")
	}
	_, anthropic, err := convertMessagesToAnthropic(messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(anthropic) != 2 || len(anthropic[1].Content) != 4 || anthropic[1].Content[0].Type != "tool_result" || anthropic[1].Content[1].Type != "tool_result" || anthropic[1].Content[2].Type != "image" {
		t.Fatal("Anthropic parallel results must also precede visual/user content")
	}
}

func TestNativeReasoningSurvivesProviderToolRoundTrip(t *testing.T) {
	for _, providerType := range []string{"openai", "anthropic"} {
		t.Run(providerType, func(t *testing.T) {
			var requests atomic.Int32
			secondBody := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				if requests.Add(1) == 1 {
					if providerType == "anthropic" {
						_, _ = io.WriteString(w, anthropicSignedToolFixture)
					} else {
						_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"Inspect the \"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"reasoning_content\":\"selected file.\",\"tool_calls\":[{\"index\":0,\"id\":\"read-1\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"main.go\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
					}
					return
				}
				secondBody <- body
				if providerType == "anthropic" {
					_, _ = io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Done\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
				} else {
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				}
			}))
			defer server.Close()
			target := AdapterTarget{ProviderType: providerType, BaseURL: server.URL, APIKey: "fixture-key", Model: "fixture-model", Opts: AdapterOpts{ThinkingBudget: 1024}}
			resolver := func() []AdapterTarget { return []AdapterTarget{target} }
			request := nativeReasoningRequest(t, `{"messages":[{"role":"INFERENCE_MESSAGE_ROLE_USER","text":"Inspect main.go"}],"tools":[{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]}`)
			invoke := func(id string, req *inferencev1.InferenceStreamRequest) *inferencev1.InferenceResponseMessage {
				var wire bytes.Buffer
				writer := &inferenceRunWriter{ctx: context.Background(), writer: &wire}
				if err := runLocalInferenceInvocation(context.Background(), &inferencev1.RunInferenceInvokeModel{InvocationId: id, Request: req}, target, nil, resolver, writer); err != nil {
					t.Fatal(err)
				}
				for _, frame := range inferenceFrames(t, wire.Bytes()) {
					if info := frame.GetInvocationResponse().GetResponse().GetResponseInfo(); info != nil && len(info.GetMessages()) == 1 {
						return info.Messages[0]
					}
				}
				t.Fatal("missing complete native response")
				return nil
			}
			first := invoke("first", request)
			if len(first.GetReasoningParts()) == 0 || first.GetReasoningParts()[0].GetText() != "Inspect the selected file." || len(first.GetToolCalls()) != 1 {
				t.Fatal("final native response lost reasoning or tool call")
			}
			continuation := nativeReasoningRequest(t, `{"messages":[{"role":"INFERENCE_MESSAGE_ROLE_TOOL","toolContent":{"parts":[{"toolCallId":"read-1","toolName":"read_file","result":"package main"}]}}]}`)
			request.Messages = append(request.Messages, &inferencev1.InferenceCoreMessage{
				Role: first.Role, Content: &inferencev1.InferenceCoreMessage_Text{Text: first.GetContent()}, ToolCalls: first.ToolCalls,
				ReasoningParts: first.ReasoningParts, AnthropicNativeContent: first.AnthropicNativeContent,
			}, continuation.Messages[0])
			if second := invoke("second", request); second.GetContent() != "Done" {
				t.Fatal("continuation did not complete")
			}
			if providerType == "openai" {
				var sent openAIRequest
				if err := json.Unmarshal(<-secondBody, &sent); err != nil {
					t.Fatal(err)
				}
				if sent.Messages[1].ReasoningContent != "Inspect the selected file." || sent.Messages[2].ToolCallID != "read-1" {
					t.Fatal("OpenAI continuation lost reasoning/tool result")
				}
			} else {
				parts := first.GetReasoningParts()
				if len(parts) != 2 || parts[0].GetSignature() != "signed-fixture" || !parts[1].GetIsRedacted() || parts[1].GetRedactedData() != "encrypted-fixture" {
					t.Fatal("signed/redacted reasoning was altered")
				}
				var sent struct {
					Messages []struct {
						Role    string
						Content json.RawMessage
					}
				}
				if err := json.Unmarshal(<-secondBody, &sent); err != nil {
					t.Fatal(err)
				}
				var actual, expected any
				_ = json.Unmarshal(sent.Messages[1].Content, &actual)
				_ = json.Unmarshal([]byte(first.GetAnthropicNativeContent()), &expected)
				if !reflect.DeepEqual(actual, expected) || len(actual.([]any)) != 4 {
					t.Fatal("Anthropic assistant blocks were not replayed exactly once in original order")
				}
			}
		})
	}
}

const anthropicSignedToolFixture = `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Inspect the selected file."}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"signed-"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"fixture"}}

data: {"type":"content_block_stop","index":0}

data: {"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"encrypted-fixture"}}

data: {"type":"content_block_stop","index":1}

data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":"I will inspect it."}}

data: {"type":"content_block_stop","index":2}

data: {"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"read-1","name":"read_file","input":{}}}

data: {"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"main.go\"}"}}

data: {"type":"content_block_stop","index":3}

data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}

data: {"type":"message_stop"}

`

func TestNativeSignedReasoningFallbackAndOpaqueContent(t *testing.T) {
	request := nativeReasoningRequest(t, `{"messages":[{"role":"INFERENCE_MESSAGE_ROLE_ASSISTANT","text":"Checking","assistantPartOrder":[1,1,2,3],"reasoningParts":[{"text":"signed thought","signature":"signature"},{"isRedacted":true,"redactedData":"opaque"}],"toolCalls":[{"toolCallId":"read-1","toolName":"read_file","args":{"path":"main.go"}}]}]}`)
	messages, _, _, err := translateLocalInferenceRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	_, converted, err := convertMessagesToAnthropic(messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(converted[0].Content) != 4 || converted[0].Content[0].Signature != "signature" || converted[0].Content[1].Data != "opaque" {
		t.Fatal("signed fallback blocks lost")
	}
	// Unknown provider fields within an explicitly typed native array are opaque.
	raw := `[{"type":"thinking","thinking":"","signature":"signature","future":{"v":1}},{"type":"text","text":"checking"}]`
	messages[0].AnthropicNativeContent = json.RawMessage(raw)
	_, converted, err = convertMessagesToAnthropic(messages)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(converted[0].Content)
	if string(encoded) != raw {
		t.Fatal("opaque native fields or empty signed thinking changed")
	}
	messages[0].AnthropicNativeContent = json.RawMessage(`{"unknown":"wrapper"}`)
	if _, _, err = convertMessagesToAnthropic(messages); err == nil {
		t.Fatal("unknown native wrapper silently discarded")
	}
}

func TestNativeCustomToolEmptyRawInputIsPreserved(t *testing.T) {
	request := nativeReasoningRequest(t, `{"messages":[{"role":"INFERENCE_MESSAGE_ROLE_ASSISTANT","toolCalls":[{"toolCallId":"empty","toolName":"custom","rawToolCallArgs":""},{"toolCallId":"missing","toolName":"custom","args":{"key":"value"}}]}],"tools":[{"name":"custom","customToolFormat":{"type":"text"}}]}`)
	messages, _, _, err := translateLocalInferenceRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if got := messages[0].ToolCalls[0].Function.Arguments; got != `{"input":""}` {
		t.Fatalf("present empty custom input became %s", got)
	}
	var fallback struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal([]byte(messages[0].ToolCalls[1].Function.Arguments), &fallback); err != nil || fallback.Input != `{"key":"value"}` {
		t.Fatal("missing raw input lost structured fallback")
	}
}
