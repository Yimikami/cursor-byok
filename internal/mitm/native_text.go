package mitm

import (
	"bytes"
	"io"
	"net/http"

	"cursor-byok/internal/agent"
)

func handleNativeText(req *http.Request, resolver agent.AdapterResolver, selectedModel string) *http.Response {
	body, err := readDecodedBody(req)
	if err != nil {
		return makeJSONResp(req, http.StatusBadRequest, `{"code":"invalid_argument","message":"could not read native text request"}`)
	}
	result := agent.HandleNativeText(req.Context(), req.URL.Path, body, req.Header.Get("Content-Type"), resolver, selectedModel)
	return &http.Response{
		Status: http.StatusText(result.Status), StatusCode: result.Status,
		Proto: req.Proto, ProtoMajor: req.ProtoMajor, ProtoMinor: req.ProtoMinor,
		Body: io.NopCloser(bytes.NewReader(result.Body)), ContentLength: int64(len(result.Body)),
		Header: http.Header{"Content-Type": {result.ContentType}}, Request: req,
	}
}
