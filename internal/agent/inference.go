package agent

// Cursor 3.32 runs its agent loop locally and uses InferenceService for each
// model invocation. These envelopes were read from the shipped client; they
// are intentionally separate from the older AgentClientMessage protocol.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	inferencev1 "cursor-byok/internal/protocodec/gen/inference/v1"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func inferenceBytes(b []byte, field protowire.Number) ([]byte, error) {
	var found []byte
	err := wireFields(b, func(n protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
		if n == field {
			if typ != protowire.BytesType {
				return fmt.Errorf("invalid inference field %d", n)
			}
			found = value
		}
		return nil
	})
	return found, err
}

func inferenceVariant(b []byte) (protowire.Number, []byte, error) {
	var kind protowire.Number
	var payload []byte
	err := wireFields(b, func(n protowire.Number, typ protowire.Type, value []byte, _ uint64) error {
		if n >= 1 && n <= 12 {
			if typ != protowire.BytesType || kind != 0 {
				return errors.New("invalid inference client message")
			}
			kind, payload = n, value
		}
		return nil
	})
	if err == nil && kind == 0 {
		err = errors.New("missing inference client message")
	}
	return kind, payload, err
}

type inferenceWriter struct {
	w   io.Writer
	mu  sync.Mutex
	seq uint64
}

func (w *inferenceWriter) send(kind protowire.Number, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	b := wireUint(wireBytes(nil, kind, payload), 100, w.seq)
	if err := writeFrame(w.w, b, false); err != nil {
		return err
	}
	flushIfPossible(w.w)
	return nil
}

func inferenceRunReady(requested []byte, target AdapterTarget) []byte {
	model := &inferencev1.InferenceRequestedModel{}
	_ = proto.Unmarshal(requested, model)
	ready := &inferencev1.RunInferenceRunReady{ResolvedModel: resolvedInferenceModel(target, model), PromptModelMetadata: localInferencePromptMetadata(), AcceptsClientHeartbeat: true, AcceptsAbortedFinish: true, AcceptsSubagentModelResolution: true}
	body, _ := proto.Marshal(ready)
	return body
}

type inferenceSessionSink struct{ writer *inferenceWriter }

func (sink inferenceSessionSink) response(id string, response *inferencev1.InferenceStreamResponse) error {
	body, err := proto.Marshal(&inferencev1.RunInferenceInvocationResponse{InvocationId: id, Response: response})
	if err != nil {
		return err
	}
	return sink.writer.send(3, body)
}

func runInferenceSession(ctx context.Context, messages <-chan []byte, rawWriter io.Writer, resolve AdapterResolver) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := &inferenceWriter{w: rawWriter}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	type finished struct {
		id  string
		end []byte
	}
	done := make(chan finished, 16)
	active := make(map[string]context.CancelFunc)
	seen := make(map[string]bool)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	var target AdapterTarget
	var runModel *inferencev1.InferenceRequestedModel
	ready := false
	var clientSeqAcked uint64
	for {
		if messages == nil && len(active) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if ready {
				if err := w.send(1, wireUint(nil, 3, clientSeqAcked)); err != nil {
					return err
				}
			}
		case result := <-done:
			// Publish terminal frames in the admission loop: a slow reader must
			// backpressure new work instead of accumulating end-frame writers.
			delete(active, result.id)
			if err := w.send(4, result.end); err != nil {
				return err
			}
		case b, ok := <-messages:
			if !ok {
				if !ready {
					return errors.New("inference input ended before run request")
				}
				// Half-close ends client input, not accepted provider invocations.
				messages = nil
				continue
			}
			kind, payload, err := inferenceVariant(b)
			if err != nil {
				return err
			}
			native := &inferencev1.RunInferenceClientMessage{}
			if err := proto.Unmarshal(b, native); err != nil {
				return err
			}
			if native.GetAppendSeq() > clientSeqAcked {
				clientSeqAcked = native.GetAppendSeq()
			}
			switch kind {
			case 1: // RunInferenceRunRequest
				if ready {
					return errors.New("inference run already started")
				}
				requested, err := inferenceBytes(payload, 3)
				if err != nil {
					return err
				}
				id, err := inferenceBytes(requested, 1)
				if err != nil {
					return err
				}
				var adapters []AdapterTarget
				if resolve != nil {
					adapters = resolve()
				}
				var found bool
				target, found = resolveNativeAdapterForModel(adapters, string(id))
				if !found {
					return errors.New("no provider is configured")
				}
				runModel = &inferencev1.InferenceRequestedModel{}
				if err := proto.Unmarshal(requested, runModel); err != nil {
					return err
				}
				if err := w.send(2, inferenceRunReady(requested, target)); err != nil {
					return err
				}
				ready = true
			case 2: // RunInferenceInvokeModel
				if !ready {
					return errors.New("inference invocation before run request")
				}
				id, err := inferenceBytes(payload, 1)
				if err != nil {
					return err
				}
				request, err := inferenceBytes(payload, 2)
				if err != nil {
					return err
				}
				if len(id) == 0 || len(request) == 0 {
					return errors.New("inference invocation needs id and complete request")
				}
				invocationID := string(id)
				if seen[invocationID] {
					return errors.New("duplicate inference invocation id")
				}
				seen[invocationID] = true
				if len(active) >= 16 {
					failure := wireBytes(wireUint(nil, 1, 8), 2, []byte("too many concurrent inference invocations"))
					if err := w.send(4, wireBytes(wireBytes(nil, 1, id), 2, failure)); err != nil {
						return err
					}
					continue
				}
				invocationCtx, cancelInvocation := context.WithCancel(ctx)
				active[invocationID] = cancelInvocation
				workers.Add(1)
				go func(target AdapterTarget) {
					defer workers.Done()
					defer cancelInvocation()
					invocation := &inferencev1.RunInferenceInvokeModel{}
					err := proto.Unmarshal(payload, invocation)
					if err == nil {
						err = runLocalInferenceInvocation(invocationCtx, invocation, target, runModel, resolve, inferenceSessionSink{writer: w})
					}
					end := wireBytes(nil, 1, id)
					if err != nil {
						code := uint64(13)
						if errors.Is(err, context.Canceled) {
							code = 1
						}
						end = wireBytes(end, 2, wireBytes(wireUint(nil, 1, code), 2, []byte(err.Error())))
					}
					select {
					case done <- finished{invocationID, end}:
					case <-ctx.Done():
					}
				}(target)
			case 3: // cancel_invocation
				id, err := inferenceBytes(payload, 1)
				if err != nil {
					return err
				}
				if stop := active[string(id)]; stop != nil {
					stop()
				}
			case 4: // finish_run
				return nil
			case 9:
				request := &inferencev1.RunInferenceResolveSubagentModels{}
				if err := proto.Unmarshal(payload, request); err != nil {
					return err
				}
				response := &inferencev1.RunInferenceSubagentModelsResolved{ResolutionId: request.GetResolutionId()}
				for _, name := range request.GetRequestedModels() {
					var adapters []AdapterTarget
					if resolve != nil {
						adapters = resolve()
					}
					adapter, ok := resolveNativeAdapterForModel(adapters, name)
					entry := &inferencev1.RunInferenceResolvedSubagentModel{Requested: name, Policy: &inferencev1.RunInferenceSubagentModelPolicy{Valid: ok, Blocked: !ok}}
					if ok {
						slug := resolvedInferenceModel(adapter, nil).GetModelId()
						entry.ResolvedSlug = &slug
						entry.Policy.Slug = slug
						entry.Policy.Advertised = true
					}
					response.Models = append(response.Models, entry)
				}
				body, err := proto.Marshal(response)
				if err != nil {
					return err
				}
				if err = w.send(5, body); err != nil {
					return err
				}
			case 5, 6, 7, 10: // heartbeats and optional telemetry
				if !ready {
					return errors.New("inference message before run request")
				}
			default:
				return fmt.Errorf("unsupported inference client message %d", kind)
			}
		}
	}
}

func handleInferenceSocket(ctx context.Context, conn *websocket.Conn, start socketFrame, writer *socketWriter, resolve AdapterResolver, read func(context.Context) (socketFrame, error)) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	messages := make(chan []byte)
	done := make(chan error, 1)
	go func() { done <- runInferenceSession(ctx, messages, writer, resolve) }()
	type incoming struct {
		frame socketFrame
		err   error
	}
	in := make(chan incoming)
	go func() {
		for {
			f, err := read(ctx)
			select {
			case in <- incoming{f, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	var seq uint64
	closed := false
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-done:
			if err != nil {
				writer.end("internal", err.Error())
			} else {
				_ = writer.WriteFrame([]byte("{}"), true)
			}
			return
		case message := <-in:
			if message.err != nil {
				return
			}
			f := message.frame
			if f.runID != start.runID {
				writer.end("invalid_argument", "message belongs to a different run")
				return
			}
			switch f.kind {
			case 3:
				if closed || f.sequence != seq {
					writer.end("invalid_argument", "invalid client message sequence")
					return
				}
				seq++
				select {
				case messages <- f.payload:
				case err := <-done:
					if err != nil {
						writer.end("internal", err.Error())
					} else {
						_ = writer.WriteFrame([]byte("{}"), true)
					}
					return
				case <-ctx.Done():
					return
				}
			case 4:
				if !closed {
					close(messages)
					closed = true
				}
			case 5:
				writer.end("canceled", "request canceled")
				return
			default:
				writer.end("invalid_argument", "unexpected inference socket frame")
				return
			}
		}
	}
}

func isInferenceProtocol(protocol string) bool {
	return strings.HasPrefix(protocol, "cursor-run-inference-")
}
