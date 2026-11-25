package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAdapterProbeUsesAgentStreamingTransport(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["stream"] != true || body["model"] != "fixture" {
					t.Errorf("unexpected probe: %v", body)
				}
				if _, present := body["reasoning_effort"]; present {
					t.Error("probe sent an optional reasoning parameter for the default None setting")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if provider == "anthropic" {
					if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "fixture-key" {
						t.Error("wrong Anthropic request")
					}
					fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				} else {
					if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-key" {
						t.Error("wrong OpenAI request")
					}
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				}
			}))
			defer srv.Close()
			got, err := runAdapterInferenceTest(context.Background(), ModelAdapterConfig{Type: provider, BaseURL: srv.URL, APIKey: "fixture-key", ModelID: "fixture", SupportedThinkingLevels: []string{"none"}})
			if err != nil || got != "OK" {
				t.Fatalf("text=%q err=%v", got, err)
			}
		})
	}
}

func TestAdapterProbeRejectsEmptySuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "data: [DONE]\n\n") }))
	defer srv.Close()
	_, err := runAdapterInferenceTest(context.Background(), ModelAdapterConfig{BaseURL: srv.URL, APIKey: "fixture", ModelID: "fixture"})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty inference error, got %v", err)
	}
}

func TestAdapterProbeHonorsCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := runAdapterInferenceTest(ctx, ModelAdapterConfig{BaseURL: srv.URL, APIKey: "fixture", ModelID: "fixture"})
	if err == nil {
		t.Fatal("expected cancellation")
	}
}
