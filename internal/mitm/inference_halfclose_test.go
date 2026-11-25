package mitm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestInferenceHalfCloseDrainsOrCancelsExplicitly(t *testing.T) {
	for _, action := range []string{"drain", "cancel-run", "disconnect"} {
		t.Run(action, func(t *testing.T) {
			started, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(started)
				select {
				case <-r.Context().Done():
					close(canceled)
					return
				case <-release:
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"complete response\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer provider.Close()
			_, client := testProxy(t, provider.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c := wsConnect(t, ctx, client, "cursor-run-inference-v1")
			defer c.CloseNow()
			inferenceStart(t, ctx, c)
			if err := c.Write(ctx, websocket.MessageBinary, wsClientMessage("run-1", 1, inferenceInvoke("half-close", "finish this response"))); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("provider did not start")
			}
			if err := c.Write(ctx, websocket.MessageBinary, wsBytes(4, wsBytes(1, []byte("run-1")))); err != nil {
				t.Fatal(err)
			}
			// Give the half-close time to reach the waiting session. The old
			// implementation deterministically canceled the held provider here.
			select {
			case <-canceled:
				t.Fatal("HalfClose canceled an active provider")
			case <-time.After(40 * time.Millisecond):
			}
			if action == "drain" {
				close(release)
				var text strings.Builder
				invocationEnded := false
				for {
					_, frame, err := c.Read(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if end := wsField(t, frame, 4); end != nil {
						if !invocationEnded || wsField(t, end, 3) != nil || text.String() != "complete response" {
							t.Fatalf("run ended before complete invocation: text=%q ended=%v frame=%x", text.String(), invocationEnded, end)
						}
						break
					}
					inner := wsField(t, wsField(t, frame, 3), 3)
					if end := wsField(t, inner, 4); end != nil {
						if string(wsField(t, end, 1)) != "half-close" || wsField(t, end, 2) != nil {
							t.Fatalf("invocation failed after HalfClose: %x", end)
						}
						invocationEnded = true
					}
					if response := wsField(t, inner, 3); response != nil {
						text.Write(wsField(t, wsField(t, wsField(t, response, 2), 1), 1))
					}
				}
				return
			}
			if action == "cancel-run" {
				if err := c.Write(ctx, websocket.MessageBinary, wsBytes(5, wsBytes(1, []byte("run-1")))); err != nil {
					t.Fatal(err)
				}
				if wsField(t, wsReadEnd(t, ctx, c), 3) == nil {
					t.Fatal("CancelRun reported success")
				}
			} else {
				_ = c.CloseNow()
			}
			select {
			case <-canceled:
			case <-ctx.Done():
				t.Fatal("explicit cancellation did not stop half-closed provider")
			}
		})
	}
}
