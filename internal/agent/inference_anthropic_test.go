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

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestInferenceAnthropicPreservesSignedToolTurn(t *testing.T) {
	for _, native := range []bool{true, false} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			expected := []any{
				map[string]any{"type": "thinking", "thinking": "first\nthought", "signature": "sig-part-one-two"},
				map[string]any{"type": "text", "text": "checking"},
				map[string]any{"type": "redacted_thinking", "data": "opaque-redacted-data"},
				map[string]any{"type": "tool_use", "id": "toolu_signed", "name": "Read", "input": map[string]any{"path": "a.txt"}},
			}
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				var request struct {
					Messages []struct {
						Role    string            `json:"role"`
						Content []json.RawMessage `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if call == 1 {
					events := []string{
						`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"first\n","signature":""}}`,
						`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"thought"}}`,
						`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-part-one"}}`,
						`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"-two"}}`,
						`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":"check"}}`,
						`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"ing"}}`,
						`{"type":"content_block_start","index":2,"content_block":{"type":"redacted_thinking","data":"opaque-redacted-data"}}`,
						`{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_signed","name":"Read","input":{}}}`,
						`{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.txt\"}"}}`,
						`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
						`{"type":"message_stop"}`,
					}
					for _, event := range events {
						fmt.Fprintf(w, "data: %s\n\n", event)
					}
					return
				}
				if len(request.Messages) != 3 || request.Messages[1].Role != "assistant" || request.Messages[2].Role != "user" {
					t.Errorf("wrong replay shape: %+v", request)
					return
				}
				var blocks []any
				for _, raw := range request.Messages[1].Content {
					var block any
					_ = json.Unmarshal(raw, &block)
					blocks = append(blocks, block)
				}
				if !reflect.DeepEqual(blocks, expected) {
					t.Errorf("signed block order/content changed: %#v", blocks)
				}
				var result map[string]any
				_ = json.Unmarshal(request.Messages[2].Content[0], &result)
				if result["tool_use_id"] != "toolu_signed" || result["type"] != "tool_result" {
					t.Errorf("tool result mismatched: %#v", result)
				}
				fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"finished\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer provider.Close()
			target := AdapterTarget{ProviderType: "anthropic", BaseURL: provider.URL, APIKey: "test-key", Model: "claude-test", Opts: AdapterOpts{ThinkingBudget: 2048}}
			var response []byte
			err := runInferenceProvider(context.Background(), providerTestRequest(), target, func(raw []byte) error {
				if info := providerTestFields(t, raw).lastBytes(4); info != nil {
					response = providerTestFields(t, info).lastBytes(4)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			fields := providerTestFields(t, response)
			assistant := providerProtoVarint(1, 2)
			assistant = append(assistant, providerProtoBytes(2, fields.lastBytes(3))...)
			for _, raw := range fields.bytes(4) {
				assistant = append(assistant, providerProtoBytes(4, raw)...)
			}
			for _, raw := range fields.bytes(6) {
				assistant = append(assistant, providerProtoBytes(7, raw)...)
			}
			for _, field := range fields {
				if field.number == 10 {
					assistant = append(assistant, providerProtoVarint(16, field.value)...)
				}
				if field.number == 11 {
					assistant = append(assistant, providerProtoBytes(17, field.data)...)
				}
			}
			if native {
				assistant = append(assistant, providerProtoBytes(18, fields.lastBytes(12))...)
			}
			value, _ := proto.Marshal(structpb.NewStringValue("file content"))
			part := append(providerProtoString(1, "toolu_signed"), providerProtoString(2, "Read")...)
			part = append(part, providerProtoBytes(3, value)...)
			tool := append(providerProtoVarint(1, 3), providerProtoBytes(6, providerProtoBytes(1, part))...)
			request := append(providerTestRequest(), providerProtoBytes(1, assistant)...)
			request = append(request, providerProtoBytes(1, tool)...)
			if err := runInferenceProvider(context.Background(), request, target, func([]byte) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 {
				t.Fatalf("provider calls=%d", calls.Load())
			}
		})
	}
}

func TestInferenceAnthropicRejectsUnsignedReplayBeforeProvider(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer provider.Close()
	for _, native := range []bool{true, false} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			assistant := providerProtoVarint(1, 2)
			if native {
				assistant = append(assistant, providerProtoString(18, `[{"type":"thinking","thinking":"unsigned"},{"type":"text","text":"hello"}]`)...)
			} else {
				assistant = append(assistant, providerProtoBytes(7, providerProtoString(2, "unsigned"))...)
				assistant = append(assistant, providerProtoString(17, "hello")...)
				assistant = append(assistant, providerProtoVarint(16, 1)...)
				assistant = append(assistant, providerProtoVarint(16, 2)...)
			}
			request := append(providerTestRequest(), providerProtoBytes(1, assistant)...)
			err := runInferenceProvider(context.Background(), request, AdapterTarget{ProviderType: "anthropic", BaseURL: provider.URL, APIKey: "key", Model: "test"}, func([]byte) error { return nil })
			if err == nil {
				t.Fatal("unsigned thinking was accepted for Anthropic replay")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("invalid signed history reached the provider")
	}
}

func TestInferenceAnthropicFallbackPreservesEmptyFieldsAndPackedOrder(t *testing.T) {
	assistant := providerProtoVarint(1, 2)
	thinking := append(providerProtoString(2, ""), providerProtoString(3, "signature")...)
	assistant = append(assistant, providerProtoBytes(7, thinking)...)
	assistant = append(assistant, providerProtoString(17, "")...)
	// Repeated enums are normally packed by the protobuf client.
	assistant = append(assistant, providerProtoBytes(16, []byte{1, 2})...)
	messages, err := decodeInferenceMessages(assistant)
	if err != nil {
		t.Fatal(err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(messages[0].AnthropicContent, &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 || blocks[0]["thinking"] != "" || blocks[0]["signature"] != "signature" || blocks[1]["text"] != "" {
		t.Fatalf("required empty fields lost: %#v", blocks)
	}
	withoutOrder := append(providerProtoVarint(1, 2), providerProtoBytes(7, thinking)...)
	withoutOrder = append(withoutOrder, providerProtoString(17, "text")...)
	if _, err := decodeInferenceMessages(withoutOrder); err == nil {
		t.Fatal("ambiguous signed history was silently reordered")
	}
	encoded, err := json.Marshal(messages[0])
	if err != nil {
		t.Fatal(err)
	}
	var openAI map[string]any
	_ = json.Unmarshal(encoded, &openAI)
	if _, exists := openAI["AnthropicContent"]; exists {
		t.Fatal("private Anthropic replay metadata leaked into OpenAI JSON")
	}
}
