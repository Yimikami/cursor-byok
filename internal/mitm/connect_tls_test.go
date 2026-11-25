package mitm

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elazarl/goproxy"
	"golang.org/x/net/http2"
)

func TestTunnelRejectsDifferentAuthorities(t *testing.T) {
	s, _ := testProxy(t, "")
	p := s.srv.Handler.(*goproxy.ProxyHttpServer)
	p.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		t.Error("cross-authority request reached proxy routing")
		return req, makeJSONResp(req, http.StatusInternalServerError, `{}`)
	})
	client := cursorH2Client(t, s)
	for _, target := range []string{"https://other.example/rpc", "https://api2.cursor.sh:444/rpc"} {
		req, _ := http.NewRequest("GET", target, nil)
		resp, err := client.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("h2 cross-authority request accepted: %s status=%d", target, resp.StatusCode)
		}
	}
	for _, target := range []string{"https://other.example/rpc", "https://api2.cursor.sh:444/rpc", "http://api2.cursor.sh/rpc", "https://user@api2.cursor.sh/rpc"} {
		conn := connectCursorTLS(t, s, []string{"http/1.1"})
		if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: api2.cursor.sh\r\n\r\n", target); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "GET"})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("h1 absolute-URI request accepted: %s status=%d", target, resp.StatusCode)
		}
		conn.Close()
	}
}

func TestProxyConstructionPreservesGoproxyGlobals(t *testing.T) {
	ca := string(goproxy.GoproxyCa.Certificate[0])
	accept, mitm, httpMITM, reject := goproxy.OkConnect, goproxy.MitmConnect, goproxy.HTTPMitmConnect, goproxy.RejectConnect
	_ = nativeTestProxy(t)
	if string(goproxy.GoproxyCa.Certificate[0]) != ca || goproxy.OkConnect != accept || goproxy.MitmConnect != mitm || goproxy.HTTPMitmConnect != httpMITM || goproxy.RejectConnect != reject {
		t.Fatal("constructing a proxy changed other servers' shared TLS configuration")
	}
}

func cursorH2Client(t *testing.T, s *Server) *http2.ClientConn {
	t.Helper()
	conn := connectCursorTLS(t, s, []string{"h2"})
	if conn.ConnectionState().NegotiatedProtocol != "h2" {
		t.Fatal("h2-only client did not negotiate HTTP/2")
	}
	client, err := (&http2.Transport{}).NewClientConn(conn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestHTTP2MultiplexesNativeAndLocalRequestsWithTrailers(t *testing.T) {
	s, _ := testProxy(t, "")
	p := s.srv.Handler.(*goproxy.ProxyHttpServer)
	var calls atomic.Int32
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	p.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		ctx.RoundTripper = goproxy.RoundTripperFunc(func(got *http.Request, _ *goproxy.ProxyCtx) (*http.Response, error) {
			calls.Add(1)
			body, err := io.ReadAll(got.Body)
			if err != nil || string(body) != "native-protobuf" || got.Header.Get("Authorization") != "Bearer test-native-token" || got.URL.String() != "https://api2.cursor.sh:443/native/rpc" {
				t.Errorf("native h2 request changed: url=%s body=%q err=%v", got.URL, body, err)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/grpc"}}, Trailer: http.Header{"Grpc-Status": {"0"}, "Grpc-Message": {"complete"}}, Body: reader, Request: got}, nil
		})
		return req, nil
	})
	client := cursorH2Client(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api2.cursor.sh/native/rpc", strings.NewReader("native-protobuf"))
	req.Header.Set("Authorization", "Bearer test-native-token")
	req.Header.Set("Content-Type", "application/grpc")
	go func() { _, _ = io.WriteString(writer, "first-frame") }()
	resp, err := client.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := make([]byte, len("first-frame"))
	if _, err := io.ReadFull(resp.Body, first); err != nil || string(first) != "first-frame" {
		t.Fatalf("native stream was buffered: %q %v", first, err)
	}
	// The first stream stays open while another stream hits the local handler.
	local, _ := http.NewRequestWithContext(ctx, "GET", "https://api2.cursor.sh/agent/v1/run", nil)
	localResp, err := client.RoundTrip(local)
	if err != nil {
		t.Fatal(err)
	}
	localResp.Body.Close()
	if localResp.StatusCode != http.StatusBadRequest || calls.Load() != 1 {
		t.Fatalf("local h2 stream bypassed routing: status=%d upstream=%d", localResp.StatusCode, calls.Load())
	}
	_ = writer.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	if resp.Trailer.Get("Grpc-Status") != "0" || resp.Trailer.Get("Grpc-Message") != "complete" {
		t.Fatalf("native gRPC trailers lost: %v", resp.Trailer)
	}
	blocked, _ := http.NewRequestWithContext(ctx, "GET", "https://api2.cursor.sh/auth/full_stripe_profile", nil)
	blocked.Header.Set("Authorization", "Bearer "+legacyLocalTokenFixture)
	blockedResp, err := client.RoundTrip(blocked)
	if err != nil {
		t.Fatal(err)
	}
	blockedResp.Body.Close()
	if blockedResp.StatusCode != http.StatusUnauthorized || calls.Load() != 1 {
		t.Fatal("h2 stream bypassed local credential guard")
	}
}

func TestHTTP2ShutdownClosesNativeStreams(t *testing.T) {
	s, _ := testProxy(t, "")
	p := s.srv.Handler.(*goproxy.ProxyHttpServer)
	requestCancelled := make(chan struct{})
	p.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		ctx.RoundTripper = goproxy.RoundTripperFunc(func(got *http.Request, _ *goproxy.ProxyCtx) (*http.Response, error) {
			reader, writer := io.Pipe()
			go func() {
				defer writer.Close()
				_, _ = io.WriteString(writer, "ready")
				<-got.Context().Done()
				close(requestCancelled)
			}()
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/grpc"}}, Body: reader, Request: got}, nil
		})
		return req, nil
	})
	client := cursorH2Client(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api2.cursor.sh/native/rpc", nil)
	resp, err := client.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestCancelled:
	case <-ctx.Done():
		t.Fatal("native h2 request survived proxy shutdown")
	}
}
