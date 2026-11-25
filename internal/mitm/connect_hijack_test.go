package mitm

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

type bufferedTestHijacker struct {
	*httptest.ResponseRecorder
	conn net.Conn
	rw   *bufio.ReadWriter
}

func (w bufferedTestHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, w.rw, nil
}

func TestHTTP1HijackPreservesReadAheadWebSocketByte(t *testing.T) {
	// net/http's background read may consume the first 0x82 before Hijack.
	// The next byte is 0x84 (masked four-byte hello); losing 0x82 makes the
	// WebSocket server reject opcode 4, as observed in Linux CI.
	for _, buffered := range []int{1, 10} {
		t.Run(strconv.Itoa(buffered), func(t *testing.T) {
			hello := []byte{0x82, 0x84, 1, 2, 3, 4, 0x0a ^ 1, 0x02 ^ 2, 0x08 ^ 3, 0x01 ^ 4}
			server, client := net.Pipe()
			defer server.Close()
			defer client.Close()
			_ = server.SetReadDeadline(time.Now().Add(time.Second))
			reader := bufio.NewReader(io.MultiReader(bytes.NewReader(hello[:buffered]), server))
			if _, err := reader.Peek(buffered); err != nil {
				t.Fatal(err)
			}
			rw := bufio.NewReadWriter(reader, bufio.NewWriter(server))
			writer := &http1TunnelWriter{ResponseWriter: bufferedTestHijacker{httptest.NewRecorder(), server, rw}}
			conn, _, err := writer.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			// The hijacked connection must own a copy, not net/http's old buffer.
			reader.Reset(bytes.NewReader([]byte("reused HTTP buffer")))
			// goproxy ignores the returned ReadWriter. It reads only net.Conn.
			go func() { _, _ = client.Write(hello[buffered:]) }()
			got := make([]byte, len(hello))
			if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, hello) {
				t.Fatalf("buffered WebSocket frame corrupted: got=%x want=%x error=%v", got, hello, err)
			}
			writer.closeHijacked()
		})
	}
}

func TestHTTP1HijackCleanupReleasesTunnelListener(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	tracked := &tunnelConn{Conn: server, closed: make(chan struct{})}
	listener := &tunnelListener{conn: tracked}
	if _, err := listener.Accept(); err != nil {
		t.Fatal(err)
	}
	writer := &http1TunnelWriter{ResponseWriter: bufferedTestHijacker{httptest.NewRecorder(), tracked, bufio.NewReadWriter(bufio.NewReader(tracked), bufio.NewWriter(tracked))}}
	if _, _, err := writer.Hijack(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _, _ = listener.Accept(); close(done) }()
	writer.closeHijacked()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("hijacked tunnel listener survived handler completion")
	}
}

var _ http.Hijacker = (*http1TunnelWriter)(nil)
var _ http.Flusher = (*http1TunnelWriter)(nil)
