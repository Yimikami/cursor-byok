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

func TestNativeReviewPollKeepsRouteAfterSettingChange(t *testing.T) {
	ca, err := certs.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var native atomic.Bool
	native.Store(true)
	s, err := newWithAuthorizer("127.0.0.1:0", ca, nil, func() []agent.AdapterTarget { t.Error("native poll resolved BYOK"); return nil }, func(purpose string) string {
		if purpose == "review-route" && native.Load() {
			return "cursor"
		}
		return ""
	}, func(_ context.Context, headers http.Header) bool {
		return headers.Get("Authorization") == "Bearer native-test-token"
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop(context.Background())
	p := s.srv.Handler.(*goproxy.ProxyHttpServer)
	calls, preflightCalls := 0, 0
	p.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		ctx.RoundTripper = goproxy.RoundTripperFunc(func(r *http.Request, _ *goproxy.ProxyCtx) (*http.Response, error) {
			if r.Method == http.MethodOptions {
				preflightCalls++
				return &http.Response{StatusCode: 204, Header: http.Header{"Access-Control-Allow-Origin": {cursorRendererOrigin}, "Access-Control-Allow-Methods": {"POST"}, "Access-Control-Allow-Headers": {"authorization,content-type"}}, Body: http.NoBody, Request: r}, nil
			}
			if r.Header.Get("Origin") != cursorRendererOrigin {
				t.Error("renderer Origin was not preserved")
			}
			calls++
			return &http.Response{StatusCode: 200, Header: http.Header{"Access-Control-Allow-Origin": {cursorRendererOrigin}}, Body: io.NopCloser(bytes.NewBufferString("native-poll")), Request: r}, nil
		})
		return req, nil
	})
	path := "/aiserver.v1.AiService/StreamBugBotAgenticPoll"
	for i := 0; i < 2; i++ {
		preflight := httptest.NewRequest(http.MethodOptions, "https://api2.cursor.sh"+path, nil)
		preflight.Header.Set("Origin", cursorRendererOrigin)
		preflight.Header.Set("Access-Control-Request-Method", "POST")
		preflight.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
		preflightResult := httptest.NewRecorder()
		p.ServeHTTP(preflightResult, preflight)
		if preflightResult.Code != 204 || preflightResult.Header().Get("Access-Control-Allow-Origin") != cursorRendererOrigin || preflightResult.Header().Get("Access-Control-Allow-Headers") != "authorization,content-type" || preflightCalls != 1 {
			t.Fatalf("preflight %d failed: %d", i, preflightResult.Code)
		}
		rec := httptest.NewRecorder()
		req := routeStreamRequest("active-review", path, true)
		req.Header.Set("Origin", cursorRendererOrigin)
		p.ServeHTTP(rec, req)
		if rec.Code != 200 || rec.Body.String() != "native-poll" || rec.Header().Get("Access-Control-Allow-Origin") != cursorRendererOrigin {
			t.Fatalf("poll %d: %d %s", i, rec.Code, rec.Body.String())
		}
		native.Store(false)
	}
	// A new review follows the new BYOK setting and cannot incur native charges.
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, routeStreamRequest("new-review", path, true))
	if rec.Code != 501 || calls != 2 {
		t.Fatalf("new poll=%d native calls=%d", rec.Code, calls)
	}
	// Knowing a request ID does not grant access to its previously native route.
	req := routeStreamRequest("active-review", path, true)
	req.Header.Set("Authorization", "Bearer another-account")
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 501 || calls != 2 {
		t.Fatalf("wrong owner poll=%d calls=%d", rec.Code, calls)
	}
}
