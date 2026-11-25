package mitm

import (
	"bytes"
	"errors"
	"io"
	"net/http"

	"cursor-byok/internal/agent"
)

func handleNativeText(req *http.Request, resolver agent.AdapterResolver, selectedModel string) *http.Response {
	body, err := readDecodedBodyLimit(req, 1<<20)
	if err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			return makeJSONResp(req, http.StatusRequestEntityTooLarge, `{"code":"resource_exhausted","message":"native text request exceeds 1 MiB"}`)
		}
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
