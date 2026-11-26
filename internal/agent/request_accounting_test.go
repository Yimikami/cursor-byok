package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	inferencev1 "cursor-byok/internal/protocodec/gen/inference/v1"
)

// Count actual HTTP requests rather than inferring cost from emitted chunks.
// No production credentials or billable providers are used.
func TestInferenceRequestAccounting(t *testing.T) {
	for _, mode := range []string{"success", "http-error", "interrupted-stream"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var got openAIRequest
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
					return
				}
				if len(got.Messages) != 2 || got.Messages[0].Role != "system" || got.Messages[1].Role != "user" {
					t.Errorf("unexpected extra messages: %+v", got.Messages)
				} else if string(got.Messages[0].Content) != `"native-system-sentinel"` || string(got.Messages[1].Content) != `"native-user-sentinel"` {
					t.Error("native prompt was expanded or duplicated")
				}
				if mode == "http-error" {
					w.WriteHeader(503)
					fmt.Fprint(w, "unavailable")
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n")
				if mode == "success" {
					fmt.Fprint(w, "data: [DONE]\n\n")
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			target := AdapterTarget{BaseURL: server.URL, APIKey: "count-only-test-key", Model: "test-model"}
			invoke := &inferencev1.RunInferenceInvokeModel{InvocationId: "count", Request: &inferencev1.InferenceStreamRequest{Messages: []*inferencev1.InferenceCoreMessage{
				{Role: inferencev1.InferenceMessageRole_INFERENCE_MESSAGE_ROLE_SYSTEM, Content: &inferencev1.InferenceCoreMessage_Text{Text: "native-system-sentinel"}},
				{Role: inferencev1.InferenceMessageRole_INFERENCE_MESSAGE_ROLE_USER, Content: &inferencev1.InferenceCoreMessage_Text{Text: "native-user-sentinel"}},
			}}}
			var output bytes.Buffer
			err := runLocalInferenceInvocation(ctx, invoke, target, nil, nil, &inferenceRunWriter{ctx: ctx, writer: &output})
			if (err == nil) != (mode == "success") {
				t.Fatalf("unexpected outcome: %v", err)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("one native invocation made %d provider requests", got)
			}
		})
	}
}

func TestDuplicateInferenceDoesNotSpendAnotherProviderRequest(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		started <- struct{}{}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	messages := make(chan []byte)
	done := make(chan error, 1)
	go func() {
		done <- runInferenceSession(ctx, messages, io.Discard, func() []AdapterTarget {
			return []AdapterTarget{{BaseURL: server.URL, APIKey: "count-only-test-key", Model: "test-model"}}
		})
	}()
	send := func(b []byte) {
		t.Helper()
		select {
		case messages <- b:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	send(wireBytes(nil, 1, nil))
	request := providerTestRequest()
	invocation := wireBytes(nil, 2, wireBytes(wireBytes(nil, 1, []byte("same-id")), 2, request))
	send(invocation)
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	send(invocation)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "duplicate inference invocation id") {
			t.Fatalf("unexpected duplicate outcome: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("duplicate invocation made %d provider requests", got)
	}
}
