package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	aiserverv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
	inferencev1 "cursor-byok/internal/protocodec/gen/inference/v1"
	"google.golang.org/protobuf/proto"
)

func TestNativePermissionWaitSurvivesLegacyDeadlineAndCancels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scope := bidiToolScope(t.Name())
		wait := registerToolWait(scope, "mcp-permission")
		registerExecIDAlias(scope, "mcp-exec", 7, "mcp-permission")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan *toolResultEnvelope, 1)
		go func() { result <- waitForNativeToolResult(ctx, wait) }()
		synctest.Wait()
		time.Sleep(35 * time.Second) // virtual time; no wall-clock wait
		select {
		case <-result:
			t.Fatal("pending permission became a fake tool completion")
		default:
		}
		routeExecClientResult(scope, execResult(7, "mcp-exec"))
		synctest.Wait()
		if got := <-result; got.Error != "" || got.ExecClient.GetExecId() != "mcp-exec" {
			t.Fatal("real tool result was lost")
		}
		assertScopeRetired(t, scope)
		wait = registerToolWait(scope, "cancelled-permission")
		go func() { result <- waitForNativeToolResult(ctx, wait) }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		if got := <-result; got.Error != context.Canceled.Error() {
			t.Fatalf("cancellation = %+v", got)
		}
		assertScopeRetired(t, scope)
	})
}

func TestInferenceHalfCloseDrainsActiveInvocation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"completed after half-close\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	messages := make(chan []byte, 2)
	for _, message := range []*inferencev1.RunInferenceClientMessage{
		{Message: &inferencev1.RunInferenceClientMessage_RunRequest{RunRequest: &inferencev1.RunInferenceRunRequest{RequestedModel: &inferencev1.InferenceRequestedModel{ModelId: "fixture"}}}},
		{Message: &inferencev1.RunInferenceClientMessage_InvokeModel{InvokeModel: &inferencev1.RunInferenceInvokeModel{InvocationId: "active", Request: nativeReasoningRequest(t, `{"messages":[{"role":"INFERENCE_MESSAGE_ROLE_USER","text":"hello"}]}`)}}},
	} {
		raw, _ := proto.Marshal(message)
		messages <- raw
	}
	var wire bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runInferenceSession(ctx, messages, &wire, func() []AdapterTarget {
			return []AdapterTarget{{Model: "fixture", BaseURL: provider.URL, APIKey: "test-key"}}
		})
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("provider not started")
	}
	close(messages)
	select {
	case err := <-done:
		close(release)
		t.Fatalf("half-close cancelled active work: %v", err)
	case <-time.After(15 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var complete bool
	for _, frame := range inferenceFrames(t, wire.Bytes()) {
		if info := frame.GetInvocationResponse().GetResponse().GetResponseInfo(); info != nil {
			complete = info.GetMessages()[0].GetContent() == "completed after half-close"
		}
	}
	if !complete {
		t.Fatal("half-closed stream lost its final response")
	}
}

func TestBidiInboxDetachedReclaimPreservesWatermark(t *testing.T) {
	store := newBidiInboxStore()
	first, err := store.claim("reconnect", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if store.append("reconnect", 0, []byte("first")).Status != 200 {
		t.Fatal("append failed")
	}
	if _, err = first.next(context.Background()); err != nil {
		t.Fatal(err)
	}
	first.release()
	if _, err = store.claim("reconnect", "inference"); err == nil {
		t.Fatal("detached stream switched protocols")
	}
	second, err := store.claim("reconnect", "agent")
	if err != nil {
		t.Fatal(err)
	}
	defer second.release()
	first.release() // delayed old cleanup must not invalidate the new lease
	if _, err = store.claim("reconnect", "agent"); err == nil {
		t.Fatal("concurrent duplicate claim accepted")
	}
	if store.append("reconnect", 0, []byte("retry")).Status != 200 || store.append("reconnect", 1, []byte("second")).Status != 200 {
		t.Fatal("resume append failed")
	}
	if body, err := second.next(context.Background()); err != nil || string(body) != "second" {
		t.Fatalf("watermark lost: %q %v", body, err)
	}
}

func TestNativeOutputLimitDoesNotCompleteOrDispatchTools(t *testing.T) {
	for _, delta := range []string{`{"content":"partial answer"}`, `{"tool_calls":[{"index":0,"id":"partial-call","function":{"name":"Write","arguments":"{\"path\":"}}]}`} {
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":`+delta+`,"finish_reason":"length"}]}`+"\n\ndata: [DONE]\n\n")
		}))
		outputError, completed, dispatched := false, false, false
		err := runInferenceProvider(context.Background(), providerTestRequest(), AdapterTarget{BaseURL: provider.URL, APIKey: "test-key", Model: "fixture"}, func(raw []byte) error {
			response := &inferencev1.InferenceStreamResponse{}
			if err := proto.Unmarshal(raw, response); err != nil {
				return err
			}
			outputError = outputError || response.GetError().GetIsOutputTokenLimitError()
			completed = completed || response.GetResponseInfo() != nil || response.GetTextPart().GetIsFinal()
			dispatched = dispatched || response.GetToolCallPart() != nil
			return nil
		})
		provider.Close()
		if err == nil || !strings.Contains(err.Error(), "output token limit") || !outputError || completed || dispatched {
			t.Fatalf("truncation error=%v limit=%v complete=%v dispatched=%v", err, outputError, completed, dispatched)
		}
	}
}

func TestAnthropicNativeCapAndTabThinkingBudget(t *testing.T) {
	for _, cap := range []int{512, 2048} {
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request anthropicRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request.MaxTokens != cap {
				t.Errorf("cap=%d", request.MaxTokens)
			}
			if cap == 512 && request.Thinking != nil {
				t.Error("small cap enabled incompatible thinking")
			}
			if cap == 2048 && (request.Thinking == nil || request.Thinking.BudgetTokens != 2047) {
				t.Error("native thinking cap was not reconciled")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			text := "done"
			if cap == 512 {
				text = `{"replacement":"x = 42"}`
			}
			event, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": text}})
			_, _ = io.WriteString(w, "data: "+string(event)+"\n\ndata: {\"type\":\"message_stop\"}\n\n")
		}))
		if cap == 512 {
			body, _ := proto.Marshal(&aiserverv1.StreamCppRequest{CurrentFile: &aiserverv1.CurrentFileInfo{Contents: "x = ", CursorPosition: &aiserverv1.CursorPosition{Line: 0, Column: 4}}})
			var wire bytes.Buffer
			HandleStreamCpp(context.Background(), body, "application/proto", &wire, func() []AdapterTarget {
				return []AdapterTarget{{ProviderType: "anthropic", BaseURL: provider.URL, APIKey: "test-key", Model: "fixture", Opts: AdapterOpts{MaxOutputTokens: 4096, ThinkingBudget: 8000}}}
			}, "")
			_, end := tabFrames(t, wire.Bytes())
			if end["error"] != nil {
				t.Fatalf("Anthropic Tab failed: %v", end)
			}
		} else {
			_, err := streamAnthropic(context.Background(), provider.URL, "test-key", "fixture", []openAIMessage{textMessage("user", "test")}, nil, AdapterOpts{MaxOutputTokens: cap, ThinkingBudget: 8000}, func(string, string, bool) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
		}
		provider.Close()
	}
}

func TestBugBotRejectsParsableTruncatedReview(t *testing.T) {
	stream := func(_ context.Context, _, _, _ string, _ []openAIMessage, _ []openAITool, _ AdapterOpts, delta openAIDelta) (*streamResult, error) {
		_ = delta(`[]`, "", true)
		return &streamResult{FinishReason: "length"}, nil
	}
	result, err := runBugBotReview(context.Background(), "truncated", &Session{BugBotRequest: &aiserverv1.StreamBugBotRequest{}}, "", "", "fixture", AdapterOpts{}, stream, io.Discard)
	if err == nil || errors.Is(err, context.Canceled) || result != nil || !strings.Contains(err.Error(), "output limit") {
		t.Fatalf("partial review marked done: %v %v", result, err)
	}
}
