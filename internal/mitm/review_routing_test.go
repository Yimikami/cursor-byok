package mitm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"cursor-byok/internal/agent"
	"cursor-byok/internal/certs"
	a "cursor-byok/internal/protocodec/gen/aiserver/v1"
	"github.com/elazarl/goproxy"
	"google.golang.org/protobuf/proto"
)

func TestBYOKReviewUsesProviderAndNeverNativeFallback(t *testing.T) {
	for _, status := range []int{200, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(status)
				if status == 200 {
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"NO_ISSUES\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				}
			}))
			defer provider.Close()
			s, client := testProxy(t, provider.URL)
			p := s.srv.Handler.(*goproxy.ProxyHttpServer)
			p.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
				ctx.RoundTripper = goproxy.RoundTripperFunc(func(*http.Request, *goproxy.ProxyCtx) (*http.Response, error) {
					t.Error("BYOK review escaped to Cursor billing")
					return nil, fmt.Errorf("unexpected native request")
				})
				return req, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			id := newSSETestOwner("byok-review")
			start, _ := proto.Marshal(&a.StreamBugBotAgenticClientMessage{Message: &a.StreamBugBotAgenticClientMessage_Start{Start: &a.StreamBugBotRequest{UserInstructions: proto.String("Review the small fixture diff")}}})
			sseAppend(t, ctx, client, id, 0, start, true)
			resp := ssePost(t, ctx, client, "/aiserver.v1.AiService/StreamBugBotAgenticSSE", wsBytes(1, []byte(id)))
			defer resp.Body.Close()
			done, failed := false, false
			for {
				b, end := sseFrame(t, resp.Body)
				if end {
					failed = failed || bytes.Contains(b, []byte("error"))
					break
				}
				var msg a.StreamBugBotAgenticServerMessage
				if err := proto.Unmarshal(b, &msg); err != nil {
					t.Fatal(err)
				}
				if msg.GetBugbotResponse().GetStatus().GetStatus() == a.BugBotStatus_STATUS_ERROR {
					failed = true
				}
				if msg.GetBugbotResponse().GetStatus().GetStatus() == a.BugBotStatus_STATUS_DONE {
					done = true
				}
			}
			if calls.Load() != 1 || (status == 200 && (!done || failed)) || (status == 503 && !failed) {
				t.Fatalf("calls=%d done=%t failed=%t", calls.Load(), done, failed)
			}
		})
	}
}

func TestCursorReviewOptInPreservesNativeRoutes(t *testing.T) {
	for _, path := range []string{"/aiserver.v1.AiService/StreamBugBotAgentic", "/aiserver.v1.AiService/StreamBugBotAgenticSSE", "/aiserver.v1.AiService/StreamBugBotAgenticPoll"} {
		for _, status := range []int{200, 429} {
			t.Run(fmt.Sprintf("%s/%d", path, status), func(t *testing.T) {
				ca, err := certs.LoadOrCreate(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				s, err := newWithAuthorizer("127.0.0.1:0", ca, nil, func() []agent.AdapterTarget { t.Error("native review resolved BYOK provider"); return nil }, func(purpose string) string {
					if purpose == "review-route" {
						return "cursor"
					}
					return ""
				}, func(context.Context, http.Header) bool {
					t.Error("native review used local authorization")
					return false
				})
				if err != nil {
					t.Fatal(err)
				}
				defer s.Stop(context.Background())
				p := s.srv.Handler.(*goproxy.ProxyHttpServer)
				calls := 0
				p.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
					ctx.RoundTripper = goproxy.RoundTripperFunc(func(r *http.Request, _ *goproxy.ProxyCtx) (*http.Response, error) {
						calls++
						if r.Header.Get("Authorization") != "Bearer native-owner" {
							t.Error("native credentials changed")
						}
						return &http.Response{StatusCode: status, Header: http.Header{"Access-Control-Allow-Origin": {"native-origin"}}, Body: io.NopCloser(bytes.NewBufferString("native-result")), Request: r}, nil
					})
					return req, nil
				})
				req := routeStreamRequest("review-opt-in", path, path == "/aiserver.v1.AiService/StreamBugBotAgenticPoll")
				req.Header.Set("Authorization", "Bearer native-owner")
				rec := httptest.NewRecorder()
				p.ServeHTTP(rec, req)
				if calls != 1 || rec.Code != status || rec.Body.String() != "native-result" || rec.Header().Get("Access-Control-Allow-Origin") != "native-origin" {
					t.Fatalf("calls=%d status=%d body=%q", calls, rec.Code, rec.Body.String())
				}
			})
		}
	}
}
