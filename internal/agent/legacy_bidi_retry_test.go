package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "cursor-byok/internal/protocodec/gen/agent/v1"
	aiserv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func TestLegacySSEReattachesOriginalConversationAfterDisconnect(t *testing.T) {
	for _, service := range []string{"agent", "bugbot"} {
		t.Run(service, func(t *testing.T) {
			owner := "legacy-retry-" + uuid.NewString()
			conversation := "conversation-" + owner
			prompt := "original retry prompt " + owner
			var initial proto.Message = &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_RunRequest{RunRequest: &agentv1.AgentRunRequest{ConversationId: proto.String(conversation), Action: &agentv1.ConversationAction{Action: &agentv1.ConversationAction_UserMessageAction{UserMessageAction: &agentv1.UserMessageAction{UserMessage: &agentv1.UserMessage{Text: prompt, Mode: agentv1.AgentMode_AGENT_MODE_ASK}}}}}}}
			answer := "retry completed"
			if service == "bugbot" {
				initial = &aiserv1.StreamBugBotAgenticClientMessage{Message: &aiserv1.StreamBugBotAgenticClientMessage_Start{Start: &aiserv1.StreamBugBotRequest{UserInstructions: proto.String(prompt)}}}
				conversation = "bugbot:" + owner
				answer = "[]"
			}
			payload, err := proto.Marshal(initial)
			if err != nil {
				t.Fatal(err)
			}
			if result := bidiInboxes.append(owner, 0, payload); result.Status != 200 {
				t.Fatal(result)
			}
			started := make(chan struct{})
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if !bytes.Contains(body, []byte(prompt)) || bytes.Contains(body, []byte("sibling conversation")) {
					t.Error("SSE retry lost its original conversation")
				}
				if calls.Add(1) == 1 {
					close(started)
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", answer)
			}))
			defer provider.Close()
			resolver := func() []AdapterTarget {
				return []AdapterTarget{{BaseURL: provider.URL, APIKey: "test-only", Model: "test"}}
			}
			run := func(ctx context.Context, output io.Writer) {
				body := wireBytes(nil, 1, []byte(owner))
				if service == "agent" {
					HandleRunSSE(ctx, body, "application/proto", output, resolver)
				} else {
					HandleBugBotRunSSE(ctx, body, "application/proto", output, resolver, "")
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			firstCtx, disconnect := context.WithCancel(ctx)
			defer disconnect()
			finished := make(chan struct{})
			go func() { defer close(finished); run(firstCtx, io.Discard) }()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("first provider invocation did not start")
			}
			disconnect()
			select {
			case <-finished:
			case <-ctx.Done():
				t.Fatal("disconnected stream did not detach")
			}
			// Exercise droppedIDConv rather than whichever conversation is last.
			PutSession(&Session{RequestID: owner + "-sibling", ConversationID: conversation + "-sibling", UserText: "sibling conversation"})
			if result := bidiInboxes.append(owner, 0, []byte("already consumed retry")); result.Status != 200 {
				t.Fatal("consumed append was not acknowledged idempotently")
			}
			var output bytes.Buffer
			run(ctx, &output)
			ended := false
			for output.Len() > 0 {
				header := make([]byte, 5)
				if _, err := io.ReadFull(&output, header); err != nil {
					t.Fatal(err)
				}
				body := make([]byte, binary.BigEndian.Uint32(header[1:]))
				if _, err := io.ReadFull(&output, body); err != nil {
					t.Fatal(err)
				}
				if header[0]&flagCompressed != 0 {
					body, err = gunzip(body)
					if err != nil {
						t.Fatal(err)
					}
				}
				if header[0]&flagEndStream != 0 {
					ended = true
					if bytes.Contains(body, []byte("error")) {
						t.Fatalf("same-protocol retry rejected: %s", body)
					}
				}
			}
			if !ended || calls.Load() != 2 {
				t.Fatalf("retry did not complete: ended=%v calls=%d", ended, calls.Load())
			}
		})
	}
}
