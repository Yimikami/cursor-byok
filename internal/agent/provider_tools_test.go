package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderCollectorsRejectIncompleteToolBatchForAllCallers(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if provider == "openai" {
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-first\",\"function\":{\"name\":\"Write\",\"arguments\":\"{}\"}},{\"index\":1,\"id\":\"call-second\",\"function\":{\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
					return
				}
				fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-first\",\"name\":\"Write\",\"input\":{}}}\n\n")
				fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n")
				fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-second\",\"input\":{}}}\n\n")
				fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n")
				fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer server.Close()
			// The legacy loop also receives an error, instead of a shortened
			// success batch whose valid first call could execute on its own.
			stream := pickProviderStreamer(provider)
			finished := false
			result, err := stream(context.Background(), server.URL, "test-key", "test-model", []openAIMessage{textMessage("user", "test")}, nil, AdapterOpts{}, func(_, _ string, done bool) error {
				finished = finished || done
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "missing its ID or name") || finished ||
				result == nil || len(result.ToolCalls) != 2 || result.ToolCalls[1].Name != "" {
				t.Fatalf("collector hid malformed call: result=%+v err=%v finished=%v", result, err, finished)
			}
			toolFrames := 0
			err = runInferenceProvider(context.Background(), providerTestRequest(), AdapterTarget{ProviderType: provider, BaseURL: server.URL, APIKey: "test-key", Model: "test-model"}, func(raw []byte) error {
				if providerTestFields(t, raw).lastBytes(2) != nil {
					toolFrames++
				}
				return nil
			})
			if err == nil || toolFrames != 0 {
				t.Fatalf("incomplete tool batch dispatched: frames=%d err=%v", toolFrames, err)
			}
		})
	}
}
