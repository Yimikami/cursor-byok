package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	inferencev1 "cursor-byok/internal/protocodec/gen/inference/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestNativeModelSelectionAppliesToNextInvocation(t *testing.T) {
	var models []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		models = append(models, request.Model)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	first := AdapterTarget{Model: "first", StableID: "first-id", BaseURL: provider.URL, APIKey: "fixture"}
	second := AdapterTarget{Model: "second", StableID: "second-id", BaseURL: provider.URL, APIKey: "fixture"}
	for index, selected := range []bool{false, true, false} {
		second.NativeSelectionOverride = selected
		resolver := func() []AdapterTarget { return []AdapterTarget{first, second} }
		var wire bytes.Buffer
		request := &inferencev1.InferenceStreamRequest{}
		if err := protojson.Unmarshal([]byte(`{"modelId":"first-id","messages":[{"role":"INFERENCE_MESSAGE_ROLE_USER","text":"hello"}]}`), request); err != nil {
			t.Fatal(err)
		}
		if err := runLocalInferenceInvocation(context.Background(), &inferencev1.RunInferenceInvokeModel{InvocationId: "same-run", Request: request}, first, nil, resolver, &inferenceRunWriter{ctx: context.Background(), writer: &wire}); err != nil {
			t.Fatal(err)
		}
		want := "first"
		if selected {
			want = "second"
		}
		if models[index] != want {
			t.Fatalf("request %d routed to %s, want %s", index, models[index], want)
		}
		// A configured commit/review request must not inherit the Agent override.
		if target, _ := ResolveAdapterForModel(resolver(), "first-id"); target.Model != "first" {
			t.Fatal("specialized model was overridden")
		}
	}
}
