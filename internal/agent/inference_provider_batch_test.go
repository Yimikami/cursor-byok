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

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestInferenceProviderValidatesWholeToolBatchBeforeDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, secondID, secondName, secondArgs, want string
	}{
		{"duplicate", "call-first", "Read", "{}", "duplicate tool call"},
		{"missing-id", "", "Read", "{}", "missing its ID"},
		{"missing-name", "call-second", "", "{}", "missing its ID or name"},
		{"malformed-args", "call-second", "Read", "{\"unfinished\":", "invalid object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := []any{
				map[string]any{"index": 0, "id": "call-first", "function": map[string]any{"name": "Write", "arguments": "{\"path\":\"safe.txt\",\"contents\":\"x\"}"}},
				map[string]any{"index": 1, "id": tc.secondID, "function": map[string]any{"name": tc.secondName, "arguments": tc.secondArgs}},
			}
			event, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": tools}, "finish_reason": "tool_calls"}}})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", event)
			}))
			defer server.Close()
			toolFrames := 0
			err := runInferenceProvider(context.Background(), providerTestRequest(), AdapterTarget{BaseURL: server.URL, APIKey: "test-key", Model: "test"}, func(raw []byte) error {
				if providerTestFields(t, raw).lastBytes(2) != nil {
					toolFrames++
				}
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) || toolFrames != 0 {
				t.Fatalf("error=%v toolFrames=%d", err, toolFrames)
			}
		})
	}
}

func TestInferenceProviderKeepsToolIndexOrderAndOpaqueIDs(t *testing.T) {
	tools := []any{
		map[string]any{"index": 1, "id": "toolu_second", "function": map[string]any{"name": "Read", "arguments": "{\"path\":\"b.txt\"}"}},
		map[string]any{"index": 0, "id": "call_first", "function": map[string]any{"name": "Read", "arguments": "{\"path\":\"a.txt\"}"}},
	}
	event, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": tools}, "finish_reason": "tool_calls"}}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", event)
	}))
	defer server.Close()
	var got, replay []string
	err := runInferenceProvider(context.Background(), providerTestRequest(), AdapterTarget{BaseURL: server.URL, APIKey: "test-key", Model: "test"}, func(raw []byte) error {
		fields := providerTestFields(t, raw)
		if part := fields.lastBytes(2); part != nil {
			got = append(got, string(providerTestFields(t, part).lastBytes(1)))
		}
		if info := fields.lastBytes(4); info != nil {
			message := providerTestFields(t, providerTestFields(t, info).lastBytes(4))
			for _, call := range message.bytes(4) {
				replay = append(replay, string(providerTestFields(t, call).lastBytes(1)))
			}
		}
		return nil
	})
	wanted := []string{"call_first", "toolu_second"}
	if err != nil || !reflect.DeepEqual(got, wanted) || !reflect.DeepEqual(replay, wanted) {
		t.Fatalf("delta=%v replay=%v err=%v", got, replay, err)
	}
}

func TestInferenceProviderKeepsParallelToolResultsBeforeSupplementaryContent(t *testing.T) {
	assistant := providerProtoVarint(1, 2)
	request := []byte{}
	for _, id := range []string{"call-1", "call-2"} {
		call := append(providerProtoString(1, id), providerProtoString(2, "Read")...)
		call = append(call, providerProtoString(4, "{}")...)
		assistant = append(assistant, providerProtoBytes(4, call)...)
	}
	request = append(request, providerProtoBytes(1, assistant)...)
	value, _ := proto.Marshal(structpb.NewStringValue("file read result"))
	for _, id := range []string{"call-1", "call-2"} {
		result := append(providerProtoString(1, id), providerProtoString(2, "Read")...)
		result = append(result, providerProtoBytes(3, value)...)
		image := append(providerProtoString(1, "aGVsbG8="), providerProtoString(2, "image/png")...)
		result = append(result, providerProtoBytes(5, providerProtoBytes(2, image))...)
		message := append(providerProtoVarint(1, 3), providerProtoBytes(6, providerProtoBytes(1, result))...)
		request = append(request, providerProtoBytes(1, message)...)
	}
	messages, _, _, _, err := decodeInferenceProviderRequest(request, AdapterOpts{})
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, message := range messages {
		roles = append(roles, message.Role)
	}
	if !reflect.DeepEqual(roles, []string{"assistant", "tool", "tool", "user", "user"}) ||
		messages[1].ToolCallID != "call-1" || messages[2].ToolCallID != "call-2" {
		t.Fatalf("parallel result order broken: %+v", messages)
	}
	if !strings.Contains(string(messages[3].Content), "data:image/png;base64,aGVsbG8=") {
		t.Fatal("supplementary image was lost")
	}
}

func TestInferenceProviderRetainsNestedToolSchema(t *testing.T) {
	schema := "{\"type\":\"object\",\"properties\":{\"items\":{\"type\":\"array\",\"items\":{\"anyOf\":[{\"type\":\"string\",\"enum\":[\"read\",\"write\"]},{\"type\":\"integer\",\"minimum\":0}]}}},\"required\":[\"items\"],\"additionalProperties\":false}"
	tool := providerProtoString(1, "McpTool")
	tool = append(tool, providerProtoBytes(3, providerTestStruct(t, schema))...)
	request := append(providerTestRequest(), providerProtoBytes(2, tool)...)
	_, tools, _, _, err := decodeInferenceProviderRequest(request, AdapterOpts{})
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools=%v err=%v", tools, err)
	}
	var got map[string]any
	var want any
	_ = json.Unmarshal(tools[0].Function, &got)
	_ = json.Unmarshal([]byte(schema), &want)
	if !reflect.DeepEqual(got["parameters"], want) {
		t.Fatalf("tool schema changed: %#v", got["parameters"])
	}
}

func TestInferenceProviderRejectsTruncatedAnthropicStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n")
	}))
	defer server.Close()
	finished := false
	err := runInferenceProvider(context.Background(), providerTestRequest(), AdapterTarget{ProviderType: "anthropic", BaseURL: server.URL, APIKey: "test-key", Model: "test"}, func(raw []byte) error {
		if part := providerTestFields(t, raw).lastBytes(1); part != nil {
			finished = finished || providerTestFields(t, part).varint(2) == 1
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "before a completion marker") || finished {
		t.Fatalf("truncated stream succeeded: err=%v finished=%v", err, finished)
	}
}
