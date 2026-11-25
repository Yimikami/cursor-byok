package mitm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	aiserverv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
	"google.golang.org/protobuf/proto"
)

func waitTrackedState(t *testing.T, s *Server, state http.ConnState) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.connections.Lock()
		found := false
		for _, current := range s.connections.connections {
			if current == state {
				found = true
				break
			}
		}
		s.connections.Unlock()
		if found {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no connection reached state %v", state)
}

func assertProxySocketClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err := conn.Read(b[:]); err == nil {
		t.Fatal("proxy socket stayed readable")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("proxy socket stayed open")
	}
}

func TestStopClosesSpeculativeConnectionsAndReleasesPort(t *testing.T) {
	s, _ := testProxy(t, "")
	address := s.ln.Addr().String()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waitTrackedState(t, s, http.StateNew)
	started := time.Now()
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("Stop waited for an unused socket to send headers")
	}
	assertProxySocketClosed(t, conn)
	rebound, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("proxy port remained occupied: %v", err)
	}
	rebound.Close()
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop was not idempotent: %v", err)
	}
}

func TestStopClosesHijackedCONNECTIncludingPendingTLS(t *testing.T) {
	for _, intercepted := range []bool{false, true} {
		t.Run(fmt.Sprint(intercepted), func(t *testing.T) {
			s, _ := testProxy(t, "")
			target := "api2.cursor.sh:443"
			var upstream net.Listener
			var upstreamClosed chan struct{}
			if !intercepted {
				var err error
				upstream, err = net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer upstream.Close()
				target = upstream.Addr().String()
				upstreamClosed = make(chan struct{})
				go func() {
					conn, err := upstream.Accept()
					if err != nil {
						return
					}
					defer conn.Close()
					defer close(upstreamClosed)
					_, _ = io.Copy(io.Discard, conn)
				}()
			}
			conn, err := net.Dial("tcp", s.ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
			response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
			if err != nil || response.StatusCode != 200 {
				t.Fatalf("CONNECT handshake: %v / %v", response, err)
			}
			waitTrackedState(t, s, http.StateHijacked)
			if err := s.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertProxySocketClosed(t, conn)
			if upstreamClosed != nil {
				select {
				case <-upstreamClosed:
				case <-time.After(time.Second):
					t.Fatal("upstream CONNECT tunnel leaked")
				}
			}
		})
	}
}

func TestStopCancelsActiveHTTPUpstreamRequest(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(canceled) }))
	defer upstream.Close()
	s, client := testProxy(t, "")
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, _ := client.Get(upstream.URL)
		if response != nil {
			response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request did not start")
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream request context was not canceled")
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("client request remained blocked")
	}
}

func TestStopForceClosesHandlerAfterGraceDeadline(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	s := &Server{addr: "127.0.0.1:0", connections: &connectionTracker{connections: make(map[*trackedConnection]http.ConnState)}}
	s.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release }), ConnState: s.connections.stateChanged}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop(context.Background())
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, _ := http.Get("http://" + s.ln.Addr().String())
		if response != nil {
			response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("test handler did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("successful forced socket close reported failure: %v", err)
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("forced close left client connection open")
	}
	s.connections.Lock()
	remaining := len(s.connections.connections)
	s.connections.Unlock()
	if remaining != 0 {
		t.Fatalf("retained %d owned sockets", remaining)
	}
}

func TestStopCancelsLegacySSEBeforeProviderFirstByte(t *testing.T) {
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	defer provider.Close()
	defer close(release)
	s, client := testProxy(t, provider.URL)
	id := t.Name()
	body, _ := proto.Marshal(&aiserverv1.BidiAppendRequest{RequestId: &aiserverv1.BidiRequestId{RequestId: id}, Data: hex.EncodeToString(wsRunMessage(t, "wait for provider"))})
	response, err := client.Post("https://api2.cursor.sh/aiserver.v1.BidiService/BidiAppend", "application/proto", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("BidiAppend: %d", response.StatusCode)
	}
	body, _ = proto.Marshal(&aiserverv1.BidiRequestId{RequestId: id})
	done := make(chan struct{})
	go func() {
		defer close(done)
		response, _ := client.Post("https://api2.cursor.sh/agent.v1.AgentService/RunSSE", "application/proto", bytes.NewReader(body))
		if response != nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("legacy SSE did not invoke provider")
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("hijacked legacy request leaked provider inference")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("legacy client request leaked")
	}
}
