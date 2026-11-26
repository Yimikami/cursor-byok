package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	inferencev1 "cursor-byok/internal/protocodec/gen/inference/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestAnthropicToolRepairPreservesOtherRawBlocksAndMatchesIdentity(t *testing.T) {
	raw := json.RawMessage(`[
 { "type":"thinking", "thinking":"signed\ntext", "signature":"original", "future":9007199254740993 },
 {"type":"tool_use","id":"call-a","name":"A","input":{"items":"[]"},"extra":{"v":9007199254740993}},
 { "type":"redacted_thinking", "data":"opaque\/unchanged" },
 {"type":"tool_use","id":"call-b","name":"B","input":{"items":"{}"}},
 { "type":"tool_use", "id":"unchanged", "name":"Read", "input":{"path":"[]"} }
]`)
	repaired := []PendingToolCall{{ID: "call-b", Name: "B", Arguments: `{"items":{}}`}, {ID: "call-a", Name: "A", Arguments: `{"items":[]}`}}
	got, err := repairInferenceAnthropicToolInputs(raw, repaired)
	if err != nil {
		t.Fatal(err)
	}
	var before, after []json.RawMessage
	_ = json.Unmarshal(raw, &before)
	_ = json.Unmarshal(got, &after)
	if len(before) != len(after) {
		t.Fatal("block count changed")
	}
	for _, index := range []int{0, 2, 4} {
		if !bytes.Equal(before[index], after[index]) {
			t.Fatalf("unrepaired block %d bytes changed", index)
		}
	}
	for _, index := range []int{1, 3} {
		var oldFields, newFields map[string]json.RawMessage
		_ = json.Unmarshal(before[index], &oldFields)
		_ = json.Unmarshal(after[index], &newFields)
		for key, value := range oldFields {
			if key != "input" && !bytes.Equal(value, newFields[key]) {
				t.Fatalf("tool metadata %s changed", key)
			}
		}
		want := repaired[1].Arguments
		if index == 3 {
			want = repaired[0].Arguments
		}
		if string(newFields["input"]) != want {
			t.Fatalf("repair matched the wrong call at %d", index)
		}
	}
	unchanged, err := repairInferenceAnthropicToolInputs(raw, nil)
	if err != nil || !bytes.Equal(unchanged, raw) {
		t.Fatal("no-repair history changed")
	}
	for _, call := range []PendingToolCall{{ID: "missing", Name: "A", Arguments: `{}`}, {ID: "call-a", Name: "WrongName", Arguments: `{}`}} {
		if _, err := repairInferenceAnthropicToolInputs(raw, []PendingToolCall{call}); err == nil {
			t.Fatal("unmatched repair was silently accepted")
		}
	}
}

func TestInferenceAnthropicReplaysExecutedRepairedArguments(t *testing.T) {
	const planArguments = `{"plan":"# A plan","todos":"[{\"id\":\"one\",\"content\":\"Write greeting\"}]"}`
	const readArguments = `{"path":"[]"}`
	blocks := []map[string]any{
		{"type": "thinking", "thinking": "signed\nthought", "signature": "original-signature", "future": map[string]any{"v": float64(1)}},
		{"type": "text", "text": "Planning"},
		{"type": "redacted_thinking", "data": "original-opaque-data"},
		{"type": "tool_use", "id": "plan-call", "name": "CreatePlan", "input": map[string]any{}, "future_tool_field": "preserved"},
		{"type": "tool_use", "id": "read-call", "name": "Read", "input": map[string]any{}},
	}
	var executedPlan map[string]any
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		writeEvent := func(event any) { raw, _ := json.Marshal(event); fmt.Fprintf(w, "data: %s\n\n", raw) }
		if call == 1 {
			for i, block := range blocks {
				writeEvent(map[string]any{"type": "content_block_start", "index": i, "content_block": block})
				if i >= 3 {
					arguments := planArguments
					if i == 4 {
						arguments = readArguments
					}
					writeEvent(map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": arguments}})
				}
			}
			writeEvent(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}})
			writeEvent(map[string]any{"type": "message_stop"})
			return
		}
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
		if len(request.Messages) != 3 || len(request.Messages[1].Content) != len(blocks) {
			t.Errorf("wrong replay shape: %+v", request)
			return
		}
		replayed := request.Messages[1].Content
		for i := 0; i < 3; i++ {
			if !reflect.DeepEqual(replayed[i], blocks[i]) {
				t.Errorf("signed/redacted/text block %d changed: %#v", i, replayed[i])
			}
		}
		if !reflect.DeepEqual(replayed[3]["input"], executedPlan) || replayed[3]["id"] != "plan-call" || replayed[3]["future_tool_field"] != "preserved" {
			t.Errorf("continuation did not replay the executed plan arguments: input=%#v executed=%#v", replayed[3], executedPlan)
		}
		if !reflect.DeepEqual(replayed[4]["input"], map[string]any{"path": "[]"}) || replayed[4]["id"] != "read-call" {
			t.Errorf("unrepaired tool changed: %#v", replayed[4])
		}
		results := request.Messages[2].Content
		if len(results) != 2 || results[0]["tool_use_id"] != "plan-call" || results[0]["content"] != "plan created" || results[1]["tool_use_id"] != "read-call" {
			t.Errorf("tool results detached from executed calls: %#v", results)
		}
		writeEvent(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": "Finished"}})
		writeEvent(map[string]any{"type": "message_stop"})
	}))
	defer provider.Close()
	target := AdapterTarget{ProviderType: "anthropic", BaseURL: provider.URL, APIKey: "fixture", Model: "claude-test", Opts: AdapterOpts{ThinkingBudget: 2048}}
	request := &inferencev1.InferenceStreamRequest{}
	if err := protojson.Unmarshal([]byte(`{"messages":[{"role":"INFERENCE_MESSAGE_ROLE_USER","text":"Plan a greeting"}],"tools":[{"name":"CreatePlan","parameters":{"jsonSchema":{"type":"object","properties":{"plan":{"type":"string"},"todos":{"type":"array","items":{"type":"object"}}}}}}]}`), request); err != nil {
		t.Fatal(err)
	}
	raw, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var first *inferencev1.InferenceResponseMessage
	if err := runInferenceProvider(context.Background(), raw, target, func(raw []byte) error {
		var event inferencev1.InferenceStreamResponse
		if err := proto.Unmarshal(raw, &event); err != nil {
			return err
		}
		if part := event.GetToolCallPart(); part != nil && part.ToolCallId == "plan-call" {
			return json.Unmarshal([]byte(part.Args), &executedPlan)
		}
		if info := event.GetResponseInfo(); info != nil && len(info.Messages) > 0 {
			first = info.Messages[0]
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if first == nil || len(first.ToolCalls) != 2 || !reflect.DeepEqual(executedPlan, first.ToolCalls[0].Args.AsMap()) {
		t.Fatal("streamed and final executed arguments differ")
	}
	if _, ok := executedPlan["todos"].([]any); !ok {
		t.Fatalf("plan argument was not repaired: %#v", executedPlan)
	}
	assistant := &inferencev1.InferenceCoreMessage{Role: first.Role, Content: &inferencev1.InferenceCoreMessage_Text{Text: first.GetContent()}, ToolCalls: first.ToolCalls, ReasoningParts: first.ReasoningParts, AssistantPartOrder: first.AssistantPartOrder, AssistantTextParts: first.AssistantTextParts, AnthropicNativeContent: first.AnthropicNativeContent}
	results := &inferencev1.InferenceCoreMessage{Role: inferencev1.InferenceMessageRole_INFERENCE_MESSAGE_ROLE_TOOL, Content: &inferencev1.InferenceCoreMessage_ToolContent{ToolContent: &inferencev1.InferenceToolResultContent{Parts: []*inferencev1.InferenceToolResultPart{{ToolCallId: "plan-call", ToolName: "CreatePlan", Result: structpb.NewStringValue("plan created")}, {ToolCallId: "read-call", ToolName: "Read", Result: structpb.NewStringValue("file content")}}}}}
	request.Messages = append(request.Messages, assistant, results)
	raw, err = proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := runInferenceProvider(context.Background(), raw, target, func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("provider calls=%d", calls.Load())
	}
}
