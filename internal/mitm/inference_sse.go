package mitm

import (
	"context"
	"io"
	"net/http"

	"cursor-byok/internal/agent"
)

type canceledStreamBody struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (b *canceledStreamBody) Close() error { b.cancel(); return b.PipeReader.Close() }

// Closing a hijacked goproxy response does not cancel the original request's
// context. Tie pipe closure and server stop to the provider/session context.
func streamAgentResponse(req *http.Request, serverCtx context.Context, headers http.Header, run func(context.Context, []byte, io.Writer)) *http.Response {
	body, err := readDecodedBody(req)
	if err != nil {
		return makeJSONResp(req, http.StatusBadRequest, `{"code":"invalid_argument","message":"invalid stream body"}`)
	}
	ctx, cancel := context.WithCancel(req.Context())
	stopServer := context.AfterFunc(serverCtx, cancel)
	pr, pw := io.Pipe()
	stopPipe := context.AfterFunc(ctx, func() { _ = pw.CloseWithError(ctx.Err()); _ = pr.CloseWithError(ctx.Err()) })
	go func() {
		defer func() { stopPipe(); stopServer(); _ = pw.Close(); cancel() }()
		run(ctx, body, pw)
	}()
	hdr := headers.Clone()
	hdr.Set("Access-Control-Allow-Origin", "*")
	hdr.Set("Access-Control-Allow-Methods", "*")
	hdr.Set("Access-Control-Allow-Headers", "*")
	return &http.Response{Status: "200 OK", StatusCode: http.StatusOK, Proto: req.Proto, ProtoMajor: req.ProtoMajor, ProtoMinor: req.ProtoMinor, Body: &canceledStreamBody{pr, cancel}, ContentLength: -1, Header: hdr, Request: req}
}

func handleInferenceSSE(req *http.Request, serverCtx context.Context, resolve agent.AdapterResolver) *http.Response {
	return streamAgentResponse(req, serverCtx, agent.RunSSEHeaders, func(ctx context.Context, body []byte, w io.Writer) {
		agent.HandleInferenceSSE(ctx, body, req.Header.Get("Content-Type"), w, resolve)
	})
}

func handleBugBotRunSSE(req *http.Request, serverCtx context.Context, resolver agent.AdapterResolver, selectedModel string) *http.Response {
	return streamAgentResponse(req, serverCtx, agent.BugBotSSEHeaders, func(ctx context.Context, body []byte, w io.Writer) {
		agent.HandleBugBotRunSSE(ctx, body, req.Header.Get("Content-Type"), w, resolver, selectedModel)
	})
}
