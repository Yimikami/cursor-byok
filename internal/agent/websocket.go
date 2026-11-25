package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	agentv1 "cursor-byok/internal/protocodec/gen/agent/v1"
	"github.com/coder/websocket"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// WebSocket v1 envelope fields were verified against Cursor 3.32.4's
// cursor-always-local/dist/254.js. See docs/agent-websocket.md. These small
// transport envelopes contain the existing AgentClient/ServerMessage protos.
// Header lists and unknown fields are skipped for forward compatibility.
type socketFrame struct {
	kind             protowire.Number
	runID, requestID string
	sequence         uint64
	payload          []byte
	version          uint64
}

func wireFields(b []byte, visit func(protowire.Number, protowire.Type, []byte, uint64) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		var data []byte
		var value uint64
		switch typ {
		case protowire.BytesType:
			data, n = protowire.ConsumeBytes(b)
		case protowire.VarintType:
			value, n = protowire.ConsumeVarint(b)
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 {
			return protowire.ParseError(n)
		}
		if err := visit(num, typ, data, value); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

func decodeSocketFrame(b []byte) (socketFrame, error) {
	var f socketFrame
	err := wireFields(b, func(num protowire.Number, typ protowire.Type, data []byte, _ uint64) error {
		if num < 1 || num > 5 {
			return nil
		}
		if typ != protowire.BytesType || f.kind != 0 {
			return errors.New("invalid WebSocket frame variant")
		}
		f.kind = num
		return wireFields(data, func(n protowire.Number, t protowire.Type, d []byte, v uint64) error {
			switch {
			case num == 1 && n == 1:
				if t != protowire.VarintType {
					return errors.New("invalid protocol version")
				}
				f.version = v
			case num == 2 && n == 1:
				if t != protowire.BytesType {
					return errors.New("invalid run identity")
				}
				return wireFields(d, func(i protowire.Number, it protowire.Type, id []byte, _ uint64) error {
					if i == 1 || i == 2 {
						if it != protowire.BytesType {
							return errors.New("invalid run identity field")
						}
						if i == 1 {
							f.runID = string(id)
						} else {
							f.requestID = string(id)
						}
					}
					return nil
				})
			case num >= 3 && n == 1:
				if t != protowire.BytesType {
					return errors.New("invalid run id")
				}
				f.runID = string(d)
			case num == 3 && n == 2:
				if t != protowire.VarintType {
					return errors.New("invalid sequence")
				}
				f.sequence = v
			case num == 3 && n == 3:
				if t != protowire.BytesType {
					return errors.New("invalid agent message")
				}
				f.payload = append([]byte(nil), d...)
			}
			return nil
		})
	})
	if err != nil {
		return f, err
	}
	if f.kind == 0 {
		return f, errors.New("missing WebSocket frame variant")
	}
	if f.kind == 1 && f.version != 1 {
		return f, errors.New("unsupported WebSocket protocol version")
	}
	if f.kind >= 2 && f.runID == "" {
		return f, errors.New("missing run id")
	}
	if f.kind == 2 && f.requestID == "" {
		return f, errors.New("missing request id")
	}
	if f.kind == 3 && len(f.payload) == 0 {
		return f, errors.New("missing agent message")
	}
	return f, nil
}

func wireBytes(dst []byte, n protowire.Number, data []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(dst, n, protowire.BytesType), data)
}
func wireUint(dst []byte, n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(dst, n, protowire.VarintType), v)
}
func socketIdentity(runID, requestID string) []byte {
	return wireBytes(wireBytes(nil, 1, []byte(runID)), 2, []byte(requestID))
}

// socketWriter serializes terminal and data frames with the keepalive. The
// connection context stays live when a single run is cancelled, so RunEnd can
// still be sent. A terminal frame is emitted at most once.
type socketWriter struct {
	ctx      context.Context
	conn     *websocket.Conn
	identity []byte
	runID    string
	mu       sync.Mutex
	sequence uint64
	ended    bool
}

func (w *socketWriter) Write([]byte) (int, error) {
	return 0, errors.New("WebSocket requires message frames")
}
func (w *socketWriter) WriteFrame(body []byte, end bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended {
		return io.ErrClosedPipe
	}
	var frame []byte
	if end {
		var trailer struct {
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &trailer); err != nil {
			return err
		}
		frame = wireBytes(nil, 1, w.identity)
		if trailer.Error != nil {
			code := uint64(13)
			switch trailer.Error.Code {
			case "canceled":
				code = 1
			case "invalid_argument":
				code = 3
			}
			connectErr := wireBytes(wireUint(nil, 1, code), 2, []byte(trailer.Error.Message))
			frame = wireBytes(frame, 3, connectErr)
		}
		frame = wireBytes(nil, 4, frame)
		w.ended = true
	} else {
		frame = wireBytes(nil, 1, []byte(w.runID))
		frame = wireUint(frame, 2, w.sequence)
		frame = wireBytes(frame, 3, body)
		frame = wireBytes(nil, 3, frame)
		w.sequence++
	}
	return w.conn.Write(w.ctx, websocket.MessageBinary, frame)
}
func (w *socketWriter) end(code, message string) {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": message}})
	_ = w.WriteFrame(body, true)
}

// HandleAgentWebSocket serves one run per connection. The client only reuses
// sockets when RunEnd explicitly includes a sequential-v1 trailer; we omit it.
// Cold connections and cursor-agent-preconnect-v1 use the same hello handshake.
func HandleAgentWebSocket(ctx context.Context, conn *websocket.Conn, resolve AdapterResolver) {
	defer conn.CloseNow()
	conn.SetReadLimit(32 << 20)
	read := func(ctx context.Context) (socketFrame, error) {
		kind, b, err := conn.Read(ctx)
		if err != nil {
			return socketFrame{}, err
		}
		if kind != websocket.MessageBinary {
			return socketFrame{}, errors.New("expected binary protobuf frame")
		}
		return decodeSocketFrame(b)
	}
	helloCtx, cancelHello := context.WithTimeout(ctx, 10*time.Second)
	hello, err := read(helloCtx)
	cancelHello()
	if err != nil || hello.kind != 1 {
		_ = conn.Close(websocket.StatusProtocolError, "expected protocol v1 hello")
		return
	}
	if err = conn.Write(ctx, websocket.MessageBinary, wireBytes(nil, 1, wireUint(nil, 1, 1))); err != nil {
		return
	}
	start, err := read(ctx)
	if err != nil || start.kind != 2 {
		_ = conn.Close(websocket.StatusProtocolError, "expected run start")
		return
	}
	identity := socketIdentity(start.runID, start.requestID)
	if err = conn.Write(ctx, websocket.MessageBinary, wireBytes(nil, 2, wireBytes(nil, 1, identity))); err != nil {
		return
	}
	writer := &socketWriter{ctx: ctx, conn: conn, runID: start.runID, identity: identity}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	type incoming struct {
		frame socketFrame
		err   error
	}
	messages := make(chan incoming)
	readCtx, cancelRead := context.WithCancel(ctx)
	defer cancelRead()
	go func() {
		for {
			f, err := read(readCtx)
			select {
			case messages <- incoming{f, err}:
			case <-readCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	done := make(chan struct{})
	started, halfClosed := false, false
	var next uint64
	// A socket owns its session; client-chosen ids cannot collide with an SSE
	// request or another socket's shared session-store entry.
	sess := &Session{RequestID: "ws-" + uuid.NewString()}
	defer DropSession(sess.RequestID)
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case in := <-messages:
			if in.err != nil {
				return
			}
			f := in.frame
			if f.runID != start.runID {
				writer.end("invalid_argument", "message belongs to a different run")
				return
			}
			switch f.kind {
			case 3:
				if halfClosed || f.sequence != next {
					writer.end("invalid_argument", "invalid client message sequence")
					return
				}
				next++
				msg := &agentv1.AgentClientMessage{}
				if err := proto.Unmarshal(f.payload, msg); err != nil {
					writer.end("invalid_argument", "invalid agent message")
					return
				}
				if !started {
					extractIntoSession(sess, msg)
					if msg.GetRunRequest() != nil {
						if sess.UserText == "" {
							writer.end("invalid_argument", "missing user message")
							return
						}
						started = true
						PutSession(sess)
						go func() { defer close(done); runAgentSession(runCtx, sess.RequestID, sess, writer, resolve) }()
					}
				} else if msg.GetRunRequest() != nil {
					writer.end("invalid_argument", "run already started")
					return
				}
				routeExecClientResult(msg)
				if ir := msg.GetInteractionResponse(); ir != nil {
					deliverInteractionResponse(ir)
				}
			case 4:
				halfClosed = true
				if !started {
					writer.end("invalid_argument", "input ended before run request")
					return
				}
			case 5:
				writer.end("canceled", "request canceled")
				cancelRun()
				return
			default:
				writer.end("invalid_argument", fmt.Sprintf("unexpected frame %d", f.kind))
				return
			}
		}
	}
}
