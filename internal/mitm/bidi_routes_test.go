package mitm

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cursor-byok/internal/agent"
	"github.com/elazarl/goproxy"
	"google.golang.org/protobuf/encoding/protowire"
)

func routeAppendRequest(owner string, seq uint64, payload []byte, gz bool) *http.Request {
	body := append(wsBytes(2, wsBytes(1, []byte(owner))), wsBytes(4, payload)...)
	body = protowire.AppendVarint(protowire.AppendTag(body, 3, protowire.VarintType), seq)
	if gz {
		var compressed bytes.Buffer
		w := gzip.NewWriter(&compressed)
		w.Write(body)
		w.Close()
		body = compressed.Bytes()
	}
	req := httptest.NewRequest(http.MethodPost, "https://api2.cursor.sh/aiserver.v1.BidiService/BidiAppend", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Authorization", "Bearer native-test-token")
	req.Header.Set("Cookie", "owner=native")
	req.Header.Set("Proxy-Authorization", "Basic local-only")
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	return req
}

func routeStreamRequest(owner, path string, poll bool) *http.Request {
	body := wsBytes(1, []byte(owner))
	if poll {
		body = wsBytes(1, body)
	}
	req := httptest.NewRequest(http.MethodPost, "https://api2.cursor.sh"+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Authorization", "Bearer native-test-token")
	req.Header.Set("Cookie", "owner=native")
	return req
}

func closeRouteResponse(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
}

func TestNativeBidiReplayPreservesOriginalRequestsAndScope(t *testing.T) {
	for path, poll := range nativeBidiStreams {
		t.Run(path, func(t *testing.T) {
			routes := newBidiRouteRegistry()
			defer routes.close()
			owner := "native-owner"
			var expected [][]byte
			for seq := uint64(0); seq < 2; seq++ {
				req := routeAppendRequest(owner, seq, []byte{10, 1, byte('a' + seq)}, seq == 1)
				body, _ := io.ReadAll(req.Body)
				expected = append(expected, body)
				req.Body = io.NopCloser(bytes.NewReader(body))
				_, resp := routes.append(req)
				if resp == nil || resp.StatusCode != 200 {
					t.Fatalf("queued append %v", resp)
				}
				closeRouteResponse(resp)
			}
			// Another request's local input must never be mirrored to this native
			// stream even when it arrives before either stream has attached.
			_, resp := routes.append(routeAppendRequest("different-local-owner", 0, inferenceSSEStart(), false))
			closeRouteResponse(resp)
			calls := 0
			roundTrip := func(req *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(req.Body)
				if calls >= len(expected) || !bytes.Equal(body, expected[calls]) {
					t.Errorf("replay body changed or unrelated request replayed")
				}
				if req.URL.Path != "/aiserver.v1.BidiService/BidiAppend" || req.RequestURI != "" || req.Header.Get("Authorization") != "Bearer native-test-token" || req.Header.Get("Cookie") != "owner=native" || req.Header.Get("Content-Type") != "application/proto" || req.Header.Get("Proxy-Authorization") != "" {
					t.Error("replay scope/headers changed")
				}
				if (req.Header.Get("Content-Encoding") == "gzip") != (calls == 1) {
					t.Error("HTTP encoding changed")
				}
				calls++
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
			_, blocked, lease := routes.claim(routeStreamRequest(owner, path, poll), "native", poll, context.Background(), roundTrip)
			if blocked != nil {
				t.Fatalf("native claim: %d", blocked.StatusCode)
			}
			defer lease.close()
			if calls != 2 {
				t.Fatalf("replayed %d requests", calls)
			}
			forward, blocked := routes.append(routeAppendRequest(owner, 2, []byte{10, 1, 'c'}, true))
			if blocked != nil || forward.Header.Get("Proxy-Authorization") != "" {
				t.Fatal("established native append did not pass through")
			}
			if routes.queuedBytes == 0 {
				t.Fatal("unrelated pending request unexpectedly consumed")
			}
		})
	}
}

func TestLocalBidiInputsNeverReachNativeUpstream(t *testing.T) {
	routes := newBidiRouteRegistry()
	defer routes.close()
	owner := newSSETestOwner("registry-local")
	_, resp := routes.append(routeAppendRequest(owner, 0, inferenceSSEStart(), true))
	closeRouteResponse(resp)
	_, blocked, lease := routes.claim(routeStreamRequest(owner, inferenceSSEPath, false), "local", false, context.Background(), func(*http.Request) (*http.Response, error) { t.Fatal("BYOK input escaped upstream"); return nil, nil })
	if blocked != nil {
		t.Fatalf("local claim: %d", blocked.StatusCode)
	}
	defer lease.close()
	// Prove the replay entered the genuine local transport, where its ready
	// handling reports the missing provider through Connect framing.
	var out bytes.Buffer
	agent.HandleInferenceSSE(context.Background(), wsBytes(1, []byte(owner)), "application/proto", &out, nil)
	body, ended := sseFrame(t, &out)
	if !ended || !bytes.Contains(body, []byte("no provider")) {
		t.Fatalf("local replay not delivered: %s", body)
	}
	if routes.queuedBytes != 0 {
		t.Fatal("local queue retained input")
	}
}

func TestBidiNativeCredentialMismatchAndReplayFailures(t *testing.T) {
	for _, kind := range []string{"different-token", "synthetic-token", "upstream-error", "upstream-status"} {
		t.Run(kind, func(t *testing.T) {
			routes := newBidiRouteRegistry()
			defer routes.close()
			req := routeAppendRequest("guarded", 0, []byte{10, 1, 'a'}, false)
			if kind == "synthetic-token" {
				req.Header.Set("Authorization", "Bearer "+legacyLocalTokenFixture)
			}
			_, resp := routes.append(req)
			closeRouteResponse(resp)
			stream := routeStreamRequest("guarded", "/aiserver.v1.AiService/StreamSttSSE", false)
			if kind == "different-token" {
				stream.Header.Set("Authorization", "Bearer another-account")
			}
			if kind == "synthetic-token" {
				stream.Header.Set("Authorization", "Bearer "+legacyLocalTokenFixture)
			}
			calls := 0
			_, blocked, lease := routes.claim(stream, "native", false, context.Background(), func(*http.Request) (*http.Response, error) {
				calls++
				if kind == "upstream-error" {
					return nil, fmt.Errorf("provider transport failed")
				}
				return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("upstream private body"))}, nil
			})
			if lease != nil {
				lease.close()
			}
			if blocked == nil {
				t.Fatal("failed replay/credential check succeeded")
			}
			defer closeRouteResponse(blocked)
			if (kind == "different-token" || kind == "synthetic-token") && calls != 0 {
				t.Fatal("mismatched/synthetic credentials reached native upstream")
			}
			body, _ := io.ReadAll(blocked.Body)
			if bytes.Contains(body, []byte("upstream private body")) {
				t.Fatal("upstream error body disclosed")
			}
		})
	}
}

func TestBidiRegistryStopsAndBoundsPendingRequests(t *testing.T) {
	routes := newBidiRouteRegistry()
	routes.queuedBytes = bidiRouteLimit
	_, resp := routes.append(routeAppendRequest("full", 0, []byte{10, 1, 'x'}, false))
	if resp == nil || resp.StatusCode != 429 {
		t.Fatal("queue limit not enforced")
	}
	closeRouteResponse(resp)
	routes.queuedBytes = 0
	_, resp = routes.append(routeAppendRequest("queued", 0, []byte{10, 1, 'x'}, false))
	closeRouteResponse(resp)
	routes.close()
	if routes.queuedBytes != 0 || len(routes.entries) != 0 {
		t.Fatal("stop retained original bodies/credentials")
	}
	_, resp = routes.append(routeAppendRequest("after-stop", 0, []byte{10, 1, 'x'}, false))
	if resp == nil || resp.StatusCode == 200 {
		t.Fatal("stopped registry accepted append")
	}
	closeRouteResponse(resp)
}

// Exercise the installed request/response hooks and the real verified upstream
// transport, including the append-before-stream case that native Cursor uses.
func TestNativeBidiReplayThroughProxy(t *testing.T) {
	p := nativeTestProxy(t)
	var mu sync.Mutex
	var paths []string
	var bodies [][]byte
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if r.Header.Get("Authorization") != "Bearer native-test-token" || r.Header.Get("Cookie") != "owner=native" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("native credentials or proxy header scope changed")
		}
		mu.Lock()
		paths = append(paths, r.URL.Path)
		bodies = append(bodies, body)
		mu.Unlock()
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/proto")
		_, _ = w.Write([]byte("native-response"))
	}))
	defer upstream.Close()
	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	p.Tr.TLSClientConfig.RootCAs = pool
	p.Tr.TLSClientConfig.ServerName = "127.0.0.1"
	p.Tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	owner := "native-proxy-owner"
	queued := routeAppendRequest(owner, 0, []byte{10, 1, 'a'}, true)
	expected, _ := io.ReadAll(queued.Body)
	queued.Body = io.NopCloser(bytes.NewReader(expected))
	appendRecord := httptest.NewRecorder()
	p.ServeHTTP(appendRecord, queued)
	mu.Lock()
	beforeAttach := len(paths)
	mu.Unlock()
	if appendRecord.Code != 200 || beforeAttach != 0 {
		t.Fatalf("unknown append reached upstream: status=%d calls=%d", appendRecord.Code, beforeAttach)
	}
	streamPath := "/aiserver.v1.AiService/StreamSttSSE"
	streamRecord := httptest.NewRecorder()
	p.ServeHTTP(streamRecord, routeStreamRequest(owner, streamPath, false))
	if streamRecord.Code != 200 || streamRecord.Body.String() != "native-response" {
		t.Fatalf("native stream intercepted: status=%d body=%q", streamRecord.Code, streamRecord.Body.String())
	}
	mu.Lock()
	if len(paths) != 2 || paths[0] != "/aiserver.v1.BidiService/BidiAppend" || paths[1] != streamPath || !bytes.Equal(bodies[0], expected) {
		t.Errorf("queued original append/stream were not replayed in order: %v", paths)
	}
	mu.Unlock()
	lateRecord := httptest.NewRecorder()
	p.ServeHTTP(lateRecord, routeAppendRequest(owner, 1, []byte{10, 1, 'b'}, false))
	if lateRecord.Code != 200 || lateRecord.Body.String() != "native-response" {
		t.Fatal("established native append did not reach the upstream transport")
	}
	// Native preflight belongs to the upstream, while explicit local BYOK
	// endpoints still receive the proxy's own CORS response.
	for _, tc := range []struct {
		path string
		want int
	}{{"/native/future-feature", http.StatusAccepted}, {inferenceSSEPath, http.StatusNoContent}} {
		req := routeStreamRequest(owner, tc.path, false)
		req.Method = http.MethodOptions
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("OPTIONS %s: got %d, want %d", tc.path, rec.Code, tc.want)
		}
	}
}

func TestLocalBidiPollFailsWithoutNativeForwarding(t *testing.T) {
	p := nativeTestProxy(t)
	p.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		ctx.RoundTripper = goproxy.RoundTripperFunc(func(*http.Request, *goproxy.ProxyCtx) (*http.Response, error) {
			t.Error("local BYOK poll reached native upstream")
			return nil, fmt.Errorf("unexpected native forwarding")
		})
		return req, nil
	})
	for _, path := range []string{"/agent.v1.AgentService/RunPoll", "/aiserver.v1.InferenceService/RunInferencePoll", "/aiserver.v1.AiService/StreamBugBotAgenticPoll"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, routeStreamRequest("unsupported-poll", path, true))
		if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "unimplemented") {
			t.Errorf("local poll silently forwarded: %s status=%d", path, rec.Code)
		}
	}
}

func TestNativeBidiReplayRetainsUnacknowledgedInputForRetry(t *testing.T) {
	for _, failure := range []string{"status-503", "transport", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			routes := newBidiRouteRegistry()
			defer routes.close()
			owner := "replay-retry-" + failure
			var original [][]byte
			for seq := uint64(0); seq < 2; seq++ {
				req := routeAppendRequest(owner, seq, []byte{10, 1, byte('a' + seq)}, seq == 1)
				body, _ := io.ReadAll(req.Body)
				original = append(original, body)
				req.Body = io.NopCloser(bytes.NewReader(body))
				_, resp := routes.append(req)
				closeRouteResponse(resp)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			attempts := map[int64]int{}
			effective := map[int64]bool{}
			readAppend := func(req *http.Request) int64 {
				raw, decoded, err := readBidiBody(req)
				if err != nil {
					t.Fatal(err)
				}
				_, seq, err := agent.InspectBidiAppend(decoded, req.Header.Get("Content-Type"))
				if err != nil {
					t.Fatal(err)
				}
				if seq < 2 && !bytes.Equal(raw, original[seq]) {
					t.Fatal("retry changed original append")
				}
				attempts[seq]++
				// Like native Bidi, a repeated sequence is applied once even
				// when a previous response was lost after accepting the input.
				effective[seq] = true
				return seq
			}
			stream := routeStreamRequest(owner, "/aiserver.v1.AiService/StreamSttSSE", false).WithContext(ctx)
			_, blocked, lease := routes.claim(stream, "native", false, context.Background(), func(req *http.Request) (*http.Response, error) {
				if readAppend(req) == 1 {
					switch failure {
					case "transport":
						return nil, fmt.Errorf("connection lost after append")
					case "canceled":
						cancel()
						return nil, req.Context().Err()
					default:
						return &http.Response{StatusCode: 503, Body: http.NoBody}, nil
					}
				}
				return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
			})
			if blocked == nil || blocked.StatusCode != 502 || lease != nil {
				t.Fatal("failed replay was not surfaced")
			}
			closeRouteResponse(blocked)
			if routes.queuedBytes != len(original[1]) {
				t.Fatalf("unacknowledged input lost or acknowledged input retained: bytes=%d", routes.queuedBytes)
			}
			_, queued := routes.append(routeAppendRequest(owner, 2, []byte{10, 1, 'c'}, false))
			if queued == nil || queued.StatusCode != 200 {
				t.Fatal("new input overtook a failed replay")
			}
			closeRouteResponse(queued)
			_, blocked, lease = routes.claim(routeStreamRequest(owner, "/aiserver.v1.AiService/StreamSttSSE", false), "native", false, context.Background(), func(req *http.Request) (*http.Response, error) {
				readAppend(req)
				return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
			})
			if blocked != nil || lease == nil {
				t.Fatal("replay retry failed")
			}
			lease.close()
			if attempts[0] != 1 || attempts[1] != 2 || attempts[2] != 1 || len(effective) != 3 || routes.queuedBytes != 0 {
				t.Fatalf("retry lost/duplicated delivery or bytes: attempts=%v effective=%v bytes=%d", attempts, effective, routes.queuedBytes)
			}
		})
	}
}

func TestNativeBidiReplaySerializesNewInputAndCancelableClaims(t *testing.T) {
	routes := newBidiRouteRegistry()
	defer routes.close()
	owner := "serial-replay"
	_, response := routes.append(routeAppendRequest(owner, 0, []byte{10, 1, 'a'}, false))
	closeRouteResponse(response)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var order []int64
	go func() {
		defer close(finished)
		_, blocked, lease := routes.claim(routeStreamRequest(owner, "/aiserver.v1.AiService/StreamSttSSE", false), "native", false, context.Background(), func(req *http.Request) (*http.Response, error) {
			_, decoded, err := readBidiBody(req)
			if err != nil {
				return nil, err
			}
			_, seq, err := agent.InspectBidiAppend(decoded, req.Header.Get("Content-Type"))
			if err != nil {
				return nil, err
			}
			order = append(order, seq)
			if seq == 0 {
				close(entered)
				<-release
			}
			return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
		})
		if blocked != nil {
			t.Errorf("first replay failed: %d", blocked.StatusCode)
			closeRouteResponse(blocked)
		}
		if lease != nil {
			lease.close()
		}
	}()
	defer func() {
		close(release)
		<-finished
		if len(order) != 2 || order[0] != 0 || order[1] != 1 || routes.queuedBytes != 0 {
			t.Errorf("replay order or accounting: order=%v bytes=%d", order, routes.queuedBytes)
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("replay did not start")
	}
	_, response = routes.append(routeAppendRequest(owner, 1, []byte{10, 1, 'b'}, false))
	if response == nil || response.StatusCode != 200 {
		t.Fatal("append escaped while replay was in progress")
	}
	closeRouteResponse(response)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, blocked, lease := routes.claim(routeStreamRequest(owner, "/aiserver.v1.AiService/StreamSttSSE", false).WithContext(ctx), "native", false, context.Background(), func(*http.Request) (*http.Response, error) {
		t.Error("overlapping replay bypassed serialization")
		return nil, fmt.Errorf("unexpected replay")
	})
	if blocked == nil || blocked.StatusCode != 502 || lease != nil {
		t.Fatal("waiting replay did not honor cancellation")
	}
	closeRouteResponse(blocked)
}

func TestNativeBidiCanceledExtraClaimPreservesEstablishedRoute(t *testing.T) {
	routes := newBidiRouteRegistry()
	defer routes.close()
	path := "/aiserver.v1.AiService/StreamSttSSE"
	_, blocked, lease := routes.claim(routeStreamRequest("established", path, false), "native", false, context.Background(), nil)
	if blocked != nil || lease == nil {
		t.Fatal("cannot establish route")
	}
	defer lease.close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, blocked, canceledLease := routes.claim(routeStreamRequest("established", path, false).WithContext(ctx), "native", false, context.Background(), nil)
	if blocked == nil || canceledLease != nil {
		t.Fatal("canceled extra claim succeeded")
	}
	closeRouteResponse(blocked)
	_, response := routes.append(routeAppendRequest("established", 1, []byte{10, 1, 'a'}, false))
	if response != nil {
		closeRouteResponse(response)
		t.Fatal("canceling an extra claim stranded an established stream's append")
	}
}
