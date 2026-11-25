package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func providerTestRequest() []byte {
	message := append(providerProtoVarint(1, 1), providerProtoString(2, "Say hello")...)
	request := providerProtoBytes(1, message)
	return append(request, providerProtoString(6, "invocation-test")...)
}

func providerTestFields(t *testing.T, raw []byte) providerWireFields {
	t.Helper()
	fields, err := providerProtoFields(raw)
	if err != nil {
		t.Fatal(err)
	}
	return fields
}

func providerTestStruct(t *testing.T, value string) []byte {
	t.Helper()
	var object structpb.Struct
	if err := protojson.Unmarshal([]byte(value), &object); err != nil {
		t.Fatal(err)
	}
	raw, err := proto.Marshal(&object)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestInferenceProviderTranslatesHistoryToolsAndOptions(t *testing.T) {
	system := append(providerProtoVarint(1, 4), providerProtoString(2, "Use the available tools")...)
	assistant := append(providerProtoVarint(1, 2), providerProtoString(2, "Reading")...)
	call := append(providerProtoString(1, "call-old"), providerProtoString(2, "Read")...)
	call = append(call, providerProtoBytes(3, providerTestStruct(t, `{"path":"a.txt"}`))...)
	assistant = append(assistant, providerProtoBytes(4, call)...)
	value, err := structpb.NewValue(map[string]any{"content": "file contents"})
	if err != nil {
		t.Fatal(err)
	}
	valueRaw, err := proto.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	result := append(providerProtoString(1, "call-old"), providerProtoString(2, "Read")...)
	result = append(result, providerProtoBytes(3, valueRaw)...)
	toolMessage := append(providerProtoVarint(1, 3), providerProtoBytes(6, providerProtoBytes(1, result))...)
	tool := append(providerProtoString(1, "Read"), providerProtoString(2, "Read a file")...)
	tool = append(tool, providerProtoBytes(3, providerTestStruct(t, `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`))...)
	request := providerProtoBytes(1, system)
	request = append(request, providerTestRequest()...)
	request = append(request, providerProtoBytes(1, assistant)...)
	request = append(request, providerProtoBytes(1, toolMessage)...)
	request = append(request, providerProtoBytes(2, tool)...)
	request = append(request, providerProtoBytes(4, providerProtoVarint(1, 256))...)
	parameter := append(providerProtoString(1, "reasoning"), providerProtoString(2, "high")...)
	request = append(request, providerProtoBytes(7, providerProtoBytes(3, parameter))...)

	var got openAIRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"Plan\",\"content\":\"Hello\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-new\",\"function\":{\"name\":\"Read\",\"arguments\":\"{\\\"path\\\":\\\"b.txt\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14}}\n\n")
		fmt.Fprint(w, "data:[DONE]\n\n")
	}))
	defer server.Close()
	var frames [][]byte
	err = runInferenceProvider(context.Background(), request, AdapterTarget{BaseURL: server.URL, APIKey: "test-key", Model: "provider-model", Opts: AdapterOpts{MaxOutputTokens: 2048}}, func(b []byte) error { frames = append(frames, b); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "provider-model" || got.MaxTokens != 256 || got.ReasoningEffort != "high" || len(got.Messages) != 4 || len(got.Tools) != 1 {
		t.Fatalf("incorrect request model/options/history: %+v", got)
	}
	if got.Messages[0].Role != "system" || got.Messages[2].ToolCalls[0].Function.Arguments != `{"path":"a.txt"}` || got.Messages[3].Role != "tool" || got.Messages[3].ToolCallID != "call-old" {
		t.Fatalf("incorrect history: %+v", got.Messages)
	}
	var text string
	var finalCount, calls int
	var info providerWireFields
	for _, raw := range frames {
		f := providerTestFields(t, raw)
		if p := f.lastBytes(1); p != nil {
			part := providerTestFields(t, p)
			if part.varint(2) == 1 {
				finalCount++
			} else {
				text += string(part.lastBytes(1))
			}
		}
		if p := f.lastBytes(2); p != nil {
			call := providerTestFields(t, p)
			if call.varint(4) != 1 || string(call.lastBytes(1)) != "call-new" || string(call.lastBytes(3)) != `{"path":"b.txt"}` {
				t.Fatalf("incorrect complete tool call: %#v", call)
			}
			calls++
		}
		if p := f.lastBytes(4); p != nil {
			info = providerTestFields(t, p)
		}
	}
	if text != "Hello" || finalCount != 1 || calls != 1 || string(info.lastBytes(2)) != "provider-model" || info.varint(3) < 1e12 {
		t.Fatalf("incomplete client events text=%q finals=%d calls=%d info=%#v", text, finalCount, calls, info)
	}
	message := providerTestFields(t, info.lastBytes(4))
	if message.varint(2) != 2 || string(message.lastBytes(3)) != "Hello" || len(message.bytes(4)) != 1 || len(message.bytes(6)) != 1 {
		t.Fatal("final assistant history omitted text, calls or reasoning")
	}
}

func TestInferenceProviderUsesAnthropicTransport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("wrong Anthropic route/auth")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()
	var text string
	err := runInferenceProvider(context.Background(), providerTestRequest(), AdapterTarget{ProviderType: "anthropic", BaseURL: server.URL, APIKey: "test-key", Model: "claude-test"}, func(raw []byte) error {
		fields := providerTestFields(t, raw)
		if part := fields.lastBytes(1); part != nil {
			text += string(providerTestFields(t, part).lastBytes(1))
		}
		return nil
	})
	if err != nil || text != "OK" {
		t.Fatalf("text=%q err=%v", text, err)
	}
}

func TestInferenceProviderAnthropicThinkingFitsInvocationCap(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		outputCap, budget     int
		wantMax, wantThinking int
	}{
		{"small-cap-disables", 256, 1024, 256, 0},
		{"minimum-output-disables", 1024, 2048, 1024, 0},
		{"minimum-valid-budget", 1025, 2048, 1025, 1024},
		{"clamps-budget", 2048, 3072, 2048, 2047},
		{"preserves-budget", 4096, 1024, 4096, 1024},
		{"invalid-small-budget-disables", 4096, 512, 4096, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan anthropicRequest, 1)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request anthropicRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				requests <- request
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\n")
				fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
				fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer provider.Close()
			request := append(providerTestRequest(), providerProtoBytes(4, providerProtoVarint(1, uint64(tc.outputCap)))...)
			err := runInferenceProvider(context.Background(), request, AdapterTarget{
				ProviderType: "anthropic", BaseURL: provider.URL, APIKey: "test-key", Model: "claude-test",
				Opts: AdapterOpts{MaxOutputTokens: 4096, ThinkingBudget: tc.budget},
			}, func([]byte) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			got := <-requests
			if got.MaxTokens != tc.wantMax {
				t.Fatalf("output cap changed: got=%d want=%d", got.MaxTokens, tc.wantMax)
			}
			if tc.wantThinking == 0 {
				if got.Thinking != nil {
					t.Fatalf("thinking must be omitted when no valid budget fits: %+v", got.Thinking)
				}
			} else if got.Thinking == nil || got.Thinking.Type != "enabled" || got.Thinking.BudgetTokens != tc.wantThinking {
				t.Fatalf("thinking=%+v want budget=%d", got.Thinking, tc.wantThinking)
			}
		})
	}
}

func TestInferenceProviderRejectsStreamingErrorsAndInterruptedResponse(t *testing.T) {
	for _, test := range []struct{ name, body, want string }{
		{"capacity", "data: {\"error\":{\"code\":\"capacity_unavailable\",\"message\":\"retry later\"}}\n\ndata: [DONE]\n\n", "capacity_unavailable"},
		{"truncated", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n", "before a completion marker"},
		{"empty", "data: [DONE]\n\n", "without text or tool calls"},
		{"reasoning-only", "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"unfinished analysis\"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n", "without text or tool calls"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			finished := false
			err := runInferenceProvider(context.Background(), providerTestRequest(), AdapterTarget{BaseURL: server.URL, APIKey: "test-key", Model: "test"}, func(raw []byte) error {
				f := providerTestFields(t, raw)
				if part := f.lastBytes(1); part != nil && providerTestFields(t, part).varint(2) == 1 {
					finished = true
				}
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), test.want) || finished {
				t.Fatalf("err=%v finished=%v", err, finished)
			}
		})
	}
}

func TestInferenceProviderPropagatesCancellationAndSendFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runInferenceProvider(ctx, providerTestRequest(), AdapterTarget{}, func([]byte) error { t.Fatal("sent after cancellation"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	sendErr := errors.New("client disconnected")
	if err := runInferenceProvider(context.Background(), providerTestRequest(), AdapterTarget{}, func([]byte) error { return sendErr }); !errors.Is(err, sendErr) {
		t.Fatalf("err=%v", err)
	}

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := runInferenceProvider(ctx, providerTestRequest(), AdapterTarget{BaseURL: server.URL, APIKey: "test-key", Model: "test"}, func([]byte) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
}
