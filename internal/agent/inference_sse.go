package agent

import (
	"context"
	"errors"
	"io"

	aiserv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
)

// HandleInferenceSSE uses the same InferenceService session as WebSocket. Only
// the transport differs: BidiAppend supplies ordered input, and output uses
// Connect's compressed five-byte envelopes, as the shipped RunInferenceSSE
// method's BidiRequestId/RunInferenceServerMessage descriptors specify.
func HandleInferenceSSE(ctx context.Context, reqBody []byte, contentType string, rawWriter io.Writer, resolve AdapterResolver) {
	w := &lockedWriter{w: rawWriter}
	request := &aiserv1.BidiRequestId{}
	if err := decodeUnary(reqBody, contentType, request); err != nil {
		writeEndStreamError(w, err.Error())
		return
	}
	in, err := bidiInboxes.claim(request.GetRequestId(), "inference")
	if err != nil {
		writeEndStreamError(w, err.Error())
		return
	}
	defer in.release()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	messages := make(chan []byte)
	go func() {
		defer close(messages)
		for {
			data, err := in.next(ctx)
			if err != nil {
				return
			}
			if len(data) == 0 {
				continue
			} // transport-only heartbeat
			select {
			case messages <- data:
			case <-ctx.Done():
				return
			}
		}
	}()
	err = runInferenceSession(ctx, messages, w, resolve)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			writeEndStreamError(w, err.Error())
		}
		return
	}
	_ = writeEndStream(w)
}
