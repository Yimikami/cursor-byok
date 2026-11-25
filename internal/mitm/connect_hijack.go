package mitm

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
)

type http1TunnelWriter struct {
	http.ResponseWriter
	hijacked net.Conn
}

func (w *http1TunnelWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	w.hijacked = conn
	if err := rw.Writer.Flush(); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	// net/http may read ahead into the first WebSocket frame while flushing
	// the 101 response. goproxy discards Hijack's ReadWriter, so preserve those
	// bytes on the returned connection itself. Copy them before the original
	// HTTP reader can be reset or reused after handler completion.
	buffered, err := rw.Reader.Peek(rw.Reader.Buffered())
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	prefix := append([]byte(nil), buffered...)
	wrapped := &bufferedTunnelConn{Conn: conn, reader: io.MultiReader(bytes.NewReader(prefix), conn)}
	w.hijacked = wrapped
	return wrapped, bufio.NewReadWriter(bufio.NewReader(wrapped), bufio.NewWriter(wrapped)), nil
}

type bufferedTunnelConn struct {
	net.Conn
	reader io.Reader
}

func (c *bufferedTunnelConn) Read(data []byte) (int, error) { return c.reader.Read(data) }

func (w *http1TunnelWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *http1TunnelWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	// Connect protobuf streams (including Tab) are not text/event-stream.
	// Send their initial frames before provider completion.
	w.Flush()
	return n, err
}

func (w *http1TunnelWriter) closeHijacked() {
	// goproxy stops its WebSocket bridge after either copy direction ends.
	// Closing the client here also unblocks the other direction and releases
	// the single-connection listener after net/http gives up ownership.
	if w.hijacked != nil {
		_ = w.hijacked.Close()
	}
}
