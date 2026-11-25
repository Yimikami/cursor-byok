package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	inferencev1 "cursor-byok/internal/protocodec/gen/inference/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestNativeArgumentsDecodeOnlyExplicitCollections(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"todos":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"meta":{"type":"object","properties":{"steps":{"type":"array","items":{"type":"integer"}}}}}}},"plan":{"type":"string"},"union":{"anyOf":[{"type":"string"},{"type":"array"}]},"nullable":{"type":["array","null"]},"constrained":{"type":"array","const":[]},"flag":{"type":"boolean"}}}`), &schema); err != nil {
		t.Fatal(err)
	}
	args := `{"todos":"[{\"id\":\"[literal]\",\"meta\":\"{\\\"steps\\\":\\\"[9007199254740993]\\\"}\"}]","plan":"[1,2]","union":"[]","nullable":"[]","constrained":"[]","flag":"true","unknown":"{}"}`
	want := `{"todos":[{"id":"[literal]","meta":{"steps":[9007199254740993]}}],"plan":"[1,2]","union":"[]","nullable":"[]","constrained":"[]","flag":"true","unknown":"{}"}`
	got := normalizeNativeArguments(args, schema)
	compact := func(raw string) any {
		var value any
		d := json.NewDecoder(bytes.NewBufferString(raw))
		d.UseNumber()
		if err := d.Decode(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if !reflect.DeepEqual(compact(got), compact(want)) {
		t.Fatalf("normalization changed argument meaning: %s", got)
	}
	for _, invalid := range []string{`{"todos":"not JSON"}`, `{"todos":"{}"}`, `{"todos":"null"}`, `{"todos":"[1] trailing"}`, `{"todos":[]}`, `{"plan":"[]"}`, `{"todos":`, `{} {}`} {
		if got := normalizeNativeArguments(invalid, schema); got != invalid {
			t.Errorf("unexpected normalization of %s: %s", invalid, got)
		}
	}
}

// Regression for the real Cursor 3.32.4/Letum CreatePlan failure: todos arrived
// as a JSON string. Both the complete stream part and final response must agree
// before Cursor executes the call or records it in conversation history.
func TestLocalInferenceNormalizesPlanCollectionsOnBothResponsePaths(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		args := `{"plan":"# A plan","todos":"[{\"id\":\"one\",\"content\":\"Write greeting\"}]"}`
		chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "plan-call", "type": "function", "function": map[string]any{"name": "CreatePlan", "arguments": args}}}}, "finish_reason": "tool_calls"}}})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + string(chunk) + "\n\ndata: [DONE]\n\n"))
	}))
	defer provider.Close()
	request := &inferencev1.InferenceStreamRequest{}
	if err := protojson.Unmarshal([]byte(`{"messages":[{"role":"INFERENCE_MESSAGE_ROLE_USER","text":"Plan a greeting"}],"tools":[{"name":"CreatePlan","parameters":{"type":"object","properties":{"plan":{"type":"string"},"todos":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"content":{"type":"string"}}}}}}}]}`), request); err != nil {
		t.Fatal(err)
	}
	target := AdapterTarget{BaseURL: provider.URL, APIKey: "fixture", Model: "fixture"}
	var wire bytes.Buffer
	writer := &inferenceRunWriter{ctx: context.Background(), writer: &wire}
	if err := runLocalInferenceInvocation(context.Background(), &inferencev1.RunInferenceInvokeModel{InvocationId: "plan", Request: request}, target, nil, nil, writer); err != nil {
		t.Fatal(err)
	}
	var streamed, final map[string]any
	for _, frame := range inferenceFrames(t, wire.Bytes()) {
		response := frame.GetInvocationResponse().GetResponse()
		if part := response.GetToolCallPart(); part != nil {
			if err := json.Unmarshal([]byte(part.GetArgs()), &streamed); err != nil {
				t.Fatal(err)
			}
		}
		if info := response.GetResponseInfo(); info != nil {
			final = info.GetMessages()[0].GetToolCalls()[0].GetArgs().AsMap()
		}
	}
	if !reflect.DeepEqual(streamed, final) || streamed == nil {
		t.Fatalf("stream/final mismatch: %v / %v", streamed, final)
	}
	todos, ok := final["todos"].([]any)
	if !ok || len(todos) != 1 || todos[0].(map[string]any)["id"] != "one" || final["plan"] != "# A plan" {
		t.Fatalf("native plan arguments remain invalid: %v", final)
	}
}
