package mitm

import (
	"bytes"
	"context"
	"cursor-byok/internal/agent"
	"io"
	"net/http"
)

func handleCppConfig(req *http.Request, resolver agent.AdapterResolver) *http.Response {
	return handleTabUnary(req, resolver, agent.HandleCppConfig)
}

func handleAvailableCppModels(req *http.Request, resolver agent.AdapterResolver) *http.Response {
	return handleTabUnary(req, resolver, agent.HandleAvailableCppModels)
}

func handleTabUnary(req *http.Request, resolver agent.AdapterResolver, handler func([]byte, string, agent.AdapterResolver) agent.Result) *http.Response {
	body, err := readDecodedBody(req)
	if err != nil {
		return makeJSONResp(req, http.StatusBadRequest, "{\"code\":\"invalid_argument\",\"message\":\"invalid CppConfig body\"}")
	}
	result := handler(body, req.Header.Get("Content-Type"), resolver)
	return &http.Response{
		Status: http.StatusText(result.Status), StatusCode: result.Status,
		Proto: req.Proto, ProtoMajor: req.ProtoMajor, ProtoMinor: req.ProtoMinor, Request: req,
		Body: io.NopCloser(bytes.NewReader(result.Body)), ContentLength: int64(len(result.Body)),
		Header: http.Header{"Content-Type": {result.ContentType}},
	}
}

func handleStreamCpp(req *http.Request, serverCtx context.Context, resolver agent.AdapterResolver, selectedModel string) *http.Response {
	headers := http.Header{
		"Content-Type": {"application/connect+proto"}, "Cache-Control": {"no-cache"},
		"Connect-Content-Encoding": {"gzip"}, "Connect-Accept-Encoding": {"gzip"},
	}
	return streamAgentResponse(req, serverCtx, headers, func(ctx context.Context, body []byte, writer io.Writer) {
		agent.HandleStreamCpp(ctx, body, req.Header.Get("Content-Type"), writer, resolver, selectedModel)
	})
}
