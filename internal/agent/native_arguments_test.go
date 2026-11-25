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

func TestNativeArgumentsRespectPatternAndTupleSchemas(t *testing.T) {
	for _, tc := range []struct {
		name, schema, arguments, want string
	}{
		{
			"pattern-string-is-not-an-additional-array",
			`{"type":"object","patternProperties":{"^literal_":{"type":"string"}},"additionalProperties":{"type":"array"}}`,
			`{"literal_value":"[1,2]","other":[3]}`,
			`{"literal_value":"[1,2]","other":[3]}`,
		},
		{
			"prefix-schema-before-tail-items",
			`{"type":"array","prefixItems":[{"type":"string"},{"type":"object","properties":{"literal":{"type":"string"}}}],"items":{"type":"array"}}`,
			`["[1]","{\"literal\":\"[2]\"}","[3]"]`,
			`["[1]",{"literal":"[2]"},[3]]`,
		},
		{
			"boolean-prefix-does-not-inherit-tail-schema",
			`{"type":"array","prefixItems":[true],"items":{"type":"array"}}`,
			`["[1]","[2]"]`,
			`["[1]",[2]]`,
		},
		{
			"unknown-prefix-shape-fails-closed",
			`{"type":"array","prefixItems":{"type":"string"},"items":{"type":"array"}}`,
			`["[1]"]`,
			`["[1]"]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var schema map[string]any
			if err := json.Unmarshal([]byte(tc.schema), &schema); err != nil {
				t.Fatal(err)
			}
			got := normalizeNativeArguments(tc.arguments, schema)
			var actual, expected any
			if err := json.Unmarshal([]byte(got), &actual); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.want), &expected); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual, expected) {
				t.Fatalf("wrong schema applied to member: got=%s want=%s", got, tc.want)
			}
		})
	}
}

// Regression for the real Cursor 3.32.4/model-a CreatePlan failure: todos arrived
// as a JSON string. Both the complete stream part and final response must agree
// before Cursor executes the call or records it in conversation history.
func TestLocalInferenceNormalizesPlanCollectionsOnBothResponsePaths(t *testing.T) {
	// The wrapped form is the actual Cursor 3.32.4 CreatePlan capture. Keep
	// the plain form too: both request schemas must reach the provider intact.
	const schema = `{"properties":{"name":{"type":"string"},"overview":{"type":"string"},"plan":{"type":"string"},"todos":{"items":{"properties":{"content":{"type":"string"},"id":{"type":"string"}},"required":["id","content"],"type":"object"},"type":"array"}},"required":["plan"],"type":"object"}`
	for _, tc := range []struct{ name, parameters string }{
		{"plain", schema},
		{"native-wrapper", `{"jsonSchema":` + schema + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testLocalInferencePlanCollections(t, schema, tc.parameters)
		})
	}
}

func testLocalInferencePlanCollections(t *testing.T, schema, parameters string) {
	t.Helper()
	var expectedSchema map[string]any
	if err := json.Unmarshal([]byte(schema), &expectedSchema); err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []struct {
				Function struct {
					Name       string         `json:"name"`
					Parameters map[string]any `json:"parameters"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Tools) != 1 || request.Tools[0].Function.Name != "CreatePlan" || !reflect.DeepEqual(request.Tools[0].Function.Parameters, expectedSchema) {
			t.Errorf("provider did not receive the native tool's actual JSON schema: %+v (decode=%v)", request.Tools, err)
			http.Error(w, "wrong schema", http.StatusBadRequest)
			return
		}
		args := `{"plan":"# A plan","todos":"[{\"id\":\"one\",\"content\":\"Write greeting\"}]"}`
		chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "plan-call", "type": "function", "function": map[string]any{"name": "CreatePlan", "arguments": args}}}}, "finish_reason": "tool_calls"}}})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + string(chunk) + "\n\ndata: [DONE]\n\n"))
	}))
	defer provider.Close()
	request := &inferencev1.InferenceStreamRequest{}
	if err := protojson.Unmarshal([]byte(`{"messages":[{"role":"INFERENCE_MESSAGE_ROLE_USER","text":"Plan a greeting"}],"tools":[{"name":"CreatePlan","parameters":`+parameters+`}]}`), request); err != nil {
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

func TestNativeSchemaWrapperDoesNotChangePlainOrCustomToolContracts(t *testing.T) {
	for _, schema := range []string{
		`{"type":"object","properties":{"jsonSchema":{"type":"string"}}}`,
		`{"type":"object","jsonSchema":{"type":"array"},"properties":{"value":{"type":"string"}}}`,
		`{"jsonSchema":"an annotation"}`,
	} {
		request := &inferencev1.InferenceStreamRequest{}
		if err := protojson.Unmarshal([]byte(`{"messages":[{"role":"INFERENCE_MESSAGE_ROLE_USER","text":"fixture"}],"tools":[{"name":"Fixture","parameters":`+schema+`}]}`), request); err != nil {
			t.Fatal(err)
		}
		_, tools, _, err := translateLocalInferenceRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		var got struct{ Parameters map[string]any }
		if err := json.Unmarshal(tools[0].Function, &got); err != nil || !reflect.DeepEqual(got.Parameters, request.Tools[0].Parameters.AsMap()) {
			t.Fatalf("plain schema changed: %s (decode=%v)", tools[0].Function, err)
		}
	}
	request := &inferencev1.InferenceStreamRequest{}
	if err := protojson.Unmarshal([]byte(`{"messages":[{"role":"INFERENCE_MESSAGE_ROLE_USER","text":"fixture"}],"tools":[{"name":"Raw","parameters":{"jsonSchema":{"type":"array"}},"customToolFormat":{"type":"grammar","syntax":"lark","definition":"start: patch"}}]}`), request); err != nil {
		t.Fatal(err)
	}
	_, tools, custom, err := translateLocalInferenceRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Parameters struct {
			Type       string
			Properties map[string]struct{ Type string }
			Required   []string
		}
	}
	if err := json.Unmarshal(tools[0].Function, &got); err != nil || !custom["Raw"] || got.Parameters.Type != "object" || len(got.Parameters.Properties) != 1 || got.Parameters.Properties["input"].Type != "string" || !reflect.DeepEqual(got.Parameters.Required, []string{"input"}) {
		t.Fatalf("custom raw-input contract changed: %s (decode=%v)", tools[0].Function, err)
	}
}
