package mitm

import (
	"errors"
	"net"
	"net/http"
	"sync"
)

// net/http deliberately forgets hijacked CONNECT/WebSocket connections and
// gives new connections five seconds to send a first request during Shutdown.
// A system proxy owns those sockets too, including speculative browser dials.
type connectionTracker struct {
	sync.Mutex
	connections map[*trackedConnection]http.ConnState
	stopping    bool
}

type trackedConnection struct {
	net.Conn
	tracker *connectionTracker
	once    sync.Once
}

func (c *trackedConnection) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.tracker.Lock(); delete(c.tracker.connections, c); c.tracker.Unlock() })
	return err
}

// Preserve goproxy's half-close tunnel behavior while wrapping TCP sockets.
func (c *trackedConnection) CloseRead() error {
	if conn, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return conn.CloseRead()
	}
	return c.Close()
}
func (c *trackedConnection) CloseWrite() error {
	if conn, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return c.Close()
}

type trackedListener struct {
	net.Listener
	tracker *connectionTracker
}

func (l *trackedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tracked := &trackedConnection{Conn: conn, tracker: l.tracker}
	l.tracker.Lock()
	if l.tracker.stopping {
		l.tracker.Unlock()
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	l.tracker.connections[tracked] = http.StateNew
	l.tracker.Unlock()
	return tracked, nil
}

func (t *connectionTracker) stateChanged(conn net.Conn, state http.ConnState) {
	tracked, ok := conn.(*trackedConnection)
	if !ok {
		return
	}
	t.Lock()
	defer t.Unlock()
	if _, exists := t.connections[tracked]; exists {
		t.connections[tracked] = state
	}
}

func (t *connectionTracker) closeConnections(includeActive bool) error {
	if t == nil {
		return nil
	}
	t.Lock()
	t.stopping = true
	var selected []*trackedConnection
	for conn, state := range t.connections {
		if includeActive || state != http.StateActive {
			selected = append(selected, conn)
		}
	}
	t.Unlock()
	var failures []error
	for _, conn := range selected {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
