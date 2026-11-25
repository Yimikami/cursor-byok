package mitm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/encoding/protowire"
)

func inferenceStart(t *testing.T, ctx context.Context, c *websocket.Conn) {
	t.Helper()
	wsStart(t, ctx, c)
	// RunInferenceClientMessage.run_request.requested_model.model_id.
	run := wsBytes(1, []byte("conversation-inference"))
	run = append(run, wsBytes(3, wsBytes(1, []byte("test-model")))...)
	if err := c.Write(ctx, websocket.MessageBinary, wsClientMessage("run-1", 0, wsBytes(1, run))); err != nil {
		t.Fatal(err)
	}
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ready := wsField(t, wsField(t, wsField(t, b, 3), 3), 2)
	if string(wsField(t, wsField(t, ready, 1), 1)) != "test-model" {
		t.Fatalf("run_ready = %x", ready)
	}
}

func inferenceInvoke(id, text string) []byte {
	message := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 1) // user
	message = append(message, wsBytes(2, []byte(text))...)
	request := wsBytes(1, message)
	return wsBytes(2, append(wsBytes(1, []byte(id)), wsBytes(2, request)...))
}

func TestInferenceWebSocketThroughTLSProxy(t *testing.T) {
	for _, protocol := range []string{"cursor-run-inference-v1", "cursor-run-inference-preconnect-v1"} {
		t.Run(protocol, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Model    string `json:"model"`
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if req.Model != "test-model" || len(req.Messages) != 1 || req.Messages[0].Content != "local agent hello" {
					t.Errorf("provider request: %+v", req)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"inference reply\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n")
			}))
			defer provider.Close()
			_, client := testProxy(t, provider.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			c := wsConnect(t, ctx, client, protocol)
			inferenceStart(t, ctx, c)
			// The local agent can invoke the provider more than once on one run
			// (for example after executing a tool locally).
			for i := uint64(1); i <= 2; i++ {
				id := fmt.Sprintf("invocation-%d", i)
				if err := c.Write(ctx, websocket.MessageBinary, wsClientMessage("run-1", i, inferenceInvoke(id, "local agent hello"))); err != nil {
					t.Fatal(err)
				}
				var text strings.Builder
				for {
					_, b, err := c.Read(ctx)
					if err != nil {
						t.Fatal(err)
					}
					inner := wsField(t, wsField(t, b, 3), 3)
					if end := wsField(t, inner, 4); end != nil {
						if string(wsField(t, end, 1)) != id || wsField(t, end, 2) != nil {
							t.Fatalf("invocation end %x", end)
						}
						break
					}
					response := wsField(t, inner, 3)
					if response != nil {
						if string(wsField(t, response, 1)) != id {
							t.Fatalf("wrong invocation: %x", response)
						}
						text.Write(wsField(t, wsField(t, wsField(t, response, 2), 1), 1))
					}
				}
				if text.String() != "inference reply" {
					t.Fatalf("text = %q", text.String())
				}
			}
			if err := c.Write(ctx, websocket.MessageBinary, wsClientMessage("run-1", 3, wsBytes(4, nil))); err != nil {
				t.Fatal(err)
			}
			if e := wsField(t, wsReadEnd(t, ctx, c), 3); e != nil {
				t.Fatalf("run error %x", e)
			}
		})
	}
}

func TestInferenceWebSocketCancellation(t *testing.T) {
	for _, action := range []string{"cancel-invocation", "disconnect", "stop-proxy"} {
		t.Run(action, func(t *testing.T) {
			requested, canceled := make(chan struct{}), make(chan struct{})
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				close(requested)
				<-r.Context().Done()
				close(canceled)
			}))
			defer provider.Close()
			s, client := testProxy(t, provider.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			c := wsConnect(t, ctx, client, "cursor-run-inference-v1")
			inferenceStart(t, ctx, c)
			if err := c.Write(ctx, websocket.MessageBinary, wsClientMessage("run-1", 1, inferenceInvoke("cancel-id", "cancel"))); err != nil {
				t.Fatal(err)
			}
			select {
			case <-requested:
			case <-ctx.Done():
				t.Fatal("provider not called")
			}
			switch action {
			case "cancel-invocation":
				if err := c.Write(ctx, websocket.MessageBinary, wsClientMessage("run-1", 2, wsBytes(3, wsBytes(1, []byte("cancel-id"))))); err != nil {
					t.Fatal(err)
				}
				_, b, err := c.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}
				end := wsField(t, wsField(t, wsField(t, b, 3), 3), 4)
				if wsField(t, end, 2) == nil {
					t.Fatalf("missing cancellation error: %x", b)
				}
			case "disconnect":
				c.CloseNow()
			case "stop-proxy":
				if err := s.Stop(ctx); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-canceled:
			case <-ctx.Done():
				t.Fatal("inference request leaked")
			}
		})
	}
}

func TestInferenceWebSocketWithoutProvider(t *testing.T) {
	_, client := testProxy(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := wsConnect(t, ctx, client, "cursor-run-inference-v1")
	wsStart(t, ctx, c)
	if err := c.Write(ctx, websocket.MessageBinary, wsClientMessage("run-1", 0, wsBytes(1, wsBytes(3, wsBytes(1, []byte("missing")))))); err != nil {
		t.Fatal(err)
	}
	if wsField(t, wsReadEnd(t, ctx, c), 3) == nil {
		t.Fatal("no-provider run succeeded")
	}
}
