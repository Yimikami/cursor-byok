package mitm

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "cursor-byok/internal/protocodec/gen/agent/v1"
	aiserv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const inferenceSSEPath = "/aiserver.v1.InferenceService/RunInferenceSSE"

var sseTestOwner atomic.Uint64

func newSSETestOwner(prefix string) string { return fmt.Sprintf("%s-%d", prefix, sseTestOwner.Add(1)) }

func ssePost(t *testing.T, ctx context.Context, client *http.Client, path string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api2.cursor.sh"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/proto")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("%s: status %d %s", path, resp.StatusCode, b)
	}
	return resp
}

func sseAppend(t *testing.T, ctx context.Context, client *http.Client, owner string, seq uint64, payload []byte, binaryPayload bool) {
	t.Helper()
	body := wsBytes(2, wsBytes(1, []byte(owner)))
	body = protowire.AppendVarint(protowire.AppendTag(body, 3, protowire.VarintType), seq)
	if binaryPayload {
		body = append(body, wsBytes(4, payload)...)
	} else {
		body = append(body, wsBytes(1, []byte(hex.EncodeToString(payload)))...)
	}
	resp := ssePost(t, ctx, client, "/aiserver.v1.BidiService/BidiAppend", body)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func sseFrame(t *testing.T, r io.Reader) ([]byte, bool) {
	t.Helper()
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		t.Fatal(err)
	}
	n := binary.BigEndian.Uint32(header[1:])
	if n > 32<<20 {
		t.Fatalf("oversized frame %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		t.Fatal(err)
	}
	if header[0]&1 != 0 {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		body, err = io.ReadAll(zr)
		zr.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return body, header[0]&2 != 0
}

func inferenceSSEStart() []byte {
	return wsBytes(1, append(wsBytes(1, []byte("sse-conversation")), wsBytes(3, wsBytes(1, []byte("test-model")))...))
}

func TestInferenceSSEBuffersAndOrdersAppendBeforeAttach(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte("SSE prompt")) {
			t.Errorf("prompt missing: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"SSE reply\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	_, client := testProxy(t, provider.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	owner := newSSETestOwner("inference-sse-order")
	// Current Cursor pipelines independent POSTs and may retry a seqno.
	sseAppend(t, ctx, client, owner, 1, inferenceWebSocketInvoke("invoke-sse", "SSE prompt"), true)
	sseAppend(t, ctx, client, owner, 1, inferenceWebSocketInvoke("invoke-sse", "SSE prompt"), true)
	sseAppend(t, ctx, client, owner, 0, inferenceSSEStart(), false)
	resp := ssePost(t, ctx, client, inferenceSSEPath, wsBytes(1, []byte(owner)))
	defer resp.Body.Close()
	ready, ended := sseFrame(t, resp.Body)
	if ended || wsField(t, ready, 2) == nil {
		t.Fatalf("missing RunReady: %x", ready)
	}
	var text strings.Builder
	for {
		b, end := sseFrame(t, resp.Body)
		if end {
			t.Fatalf("early stream end: %s", b)
		}
		if done := wsField(t, b, 4); done != nil {
			if wsField(t, done, 2) != nil {
				t.Fatalf("invocation error %x", done)
			}
			break
		}
		response := wsField(t, b, 3)
		if response != nil {
			text.Write(wsField(t, wsField(t, wsField(t, response, 2), 1), 1))
		}
	}
	if text.String() != "SSE reply" || calls.Load() != 1 {
		t.Fatalf("text=%q provider calls=%d", text.String(), calls.Load())
	}
	sseAppend(t, ctx, client, owner, 2, wsBytes(4, nil), true)
	for {
		b, end := sseFrame(t, resp.Body)
		if end {
			if bytes.Contains(b, []byte("error")) {
				t.Fatalf("finish: %s", b)
			}
			break
		}
	}
}

func TestLegacySSESelectsItsOwnBidiProtocol(t *testing.T) {
	for _, service := range []string{"agent", "bugbot"} {
		t.Run(service, func(t *testing.T) {
			answer := "legacy reply"
			if service == "bugbot" {
				answer = "[]"
			}
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\ndata: [DONE]\n\n", answer)
			}))
			defer provider.Close()
			_, client := testProxy(t, provider.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			owner := newSSETestOwner("legacy-sse-" + service)
			payload, path := wsRunMessage(t, "legacy prompt"), "/agent.v1.AgentService/RunSSE"
			if service == "bugbot" {
				var err error
				payload, err = proto.Marshal(&aiserv1.StreamBugBotAgenticClientMessage{Message: &aiserv1.StreamBugBotAgenticClientMessage_Start{Start: &aiserv1.StreamBugBotRequest{UserInstructions: proto.String("review test")}}})
				if err != nil {
					t.Fatal(err)
				}
				path = "/aiserver.v1.AiService/StreamBugBotAgenticSSE"
			}
			sseAppend(t, ctx, client, owner, 0, payload, true)
			resp := ssePost(t, ctx, client, path, wsBytes(1, []byte(owner)))
			defer resp.Body.Close()
			found := false
			for {
				b, end := sseFrame(t, resp.Body)
				if end {
					if bytes.Contains(b, []byte("error")) {
						t.Fatalf("stream error: %s", b)
					}
					break
				}
				if service == "agent" {
					var m agentv1.AgentServerMessage
					if err := proto.Unmarshal(b, &m); err != nil {
						t.Fatal(err)
					}
					if m.GetInteractionUpdate().GetTextDelta().GetText() == answer {
						found = true
					}
				} else {
					var m aiserv1.StreamBugBotAgenticServerMessage
					if err := proto.Unmarshal(b, &m); err != nil {
						t.Fatal(err)
					}
					if m.GetBugbotResponse().GetSummary() == "No issues found" {
						found = true
					}
				}
			}
			if !found {
				t.Fatal("legacy payload decoded as wrong protocol")
			}
		})
	}
}

func TestInferenceSSECancellation(t *testing.T) {
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
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			owner := newSSETestOwner("sse-cancel-" + action)
			sseAppend(t, ctx, client, owner, 0, inferenceSSEStart(), false)
			resp := ssePost(t, ctx, client, inferenceSSEPath, wsBytes(1, []byte(owner)))
			defer resp.Body.Close()
			if b, end := sseFrame(t, resp.Body); end || wsField(t, b, 2) == nil {
				t.Fatal("missing ready")
			}
			sseAppend(t, ctx, client, owner, 1, inferenceWebSocketInvoke("cancel-sse", "cancel"), true)
			select {
			case <-requested:
			case <-ctx.Done():
				t.Fatal("provider not called")
			}
			switch action {
			case "cancel-invocation":
				sseAppend(t, ctx, client, owner, 2, wsBytes(3, wsBytes(1, []byte("cancel-sse"))), false)
				for {
					b, end := sseFrame(t, resp.Body)
					if end {
						t.Fatal("unexpected run end")
					}
					if done := wsField(t, b, 4); done != nil {
						if wsField(t, done, 2) == nil {
							t.Fatal("no cancellation error")
						}
						break
					}
				}
				sseAppend(t, ctx, client, owner, 3, wsBytes(4, nil), false)
			case "disconnect":
				resp.Body.Close()
			case "stop-proxy":
				if err := s.Stop(ctx); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-canceled:
			case <-ctx.Done():
				t.Fatal("provider request leaked after SSE cancellation")
			}
		})
	}
}

func TestInferenceSSEReportsProtocolAndProviderErrors(t *testing.T) {
	for _, kind := range []string{"missing-provider", "bad-input", "upstream"} {
		t.Run(kind, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				http.Error(w, "test provider rejected", http.StatusBadGateway)
			}))
			defer provider.Close()
			base := provider.URL
			if kind == "missing-provider" {
				base = ""
			}
			_, client := testProxy(t, base)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			owner := newSSETestOwner("sse-error-" + kind)
			payload := inferenceSSEStart()
			if kind == "bad-input" {
				payload = []byte{0xff}
			}
			sseAppend(t, ctx, client, owner, 0, payload, true)
			if kind == "upstream" {
				sseAppend(t, ctx, client, owner, 1, inferenceWebSocketInvoke("bad-provider", "prompt"), false)
			}
			resp := ssePost(t, ctx, client, inferenceSSEPath, wsBytes(1, []byte(owner)))
			defer resp.Body.Close()
			for {
				b, end := sseFrame(t, resp.Body)
				if end {
					if !bytes.Contains(b, []byte("error")) {
						t.Fatalf("missing terminal error: %s", b)
					}
					break
				}
				if done := wsField(t, b, 4); done != nil {
					if wsField(t, done, 2) == nil {
						t.Fatal("missing invocation error")
					}
					sseAppend(t, ctx, client, owner, 2, wsBytes(4, nil), true)
					break
				}
			}
		})
	}
}
