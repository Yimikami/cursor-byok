package mitm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"cursor-byok/internal/agent"
)

func handleNameTab(req *http.Request, resolver agent.AdapterResolver, selectedModel string) *http.Response {
	return handleNativeGeneration(req, resolver, selectedModel, agent.HandleNameTab)
}

func handleWriteGitBranchName(req *http.Request, resolver agent.AdapterResolver, selectedModel string) *http.Response {
	return handleNativeGeneration(req, resolver, selectedModel, agent.HandleWriteGitBranchName)
}

func handleNativeGeneration(req *http.Request, resolver agent.AdapterResolver, selectedModel string, handler func(context.Context, []byte, string, agent.AdapterResolver, string) agent.Result) *http.Response {
	body, err := readDecodedBody(req)
	if err != nil {
		body, _ := json.Marshal(map[string]string{"code": "invalid_argument", "message": "read body: " + err.Error()})
		return makeJSONResp(req, http.StatusBadRequest, string(body))
	}
	res := handler(req.Context(), body, req.Header.Get("Content-Type"), resolver, selectedModel)
	hdr := http.Header{}
	hdr.Set("Content-Type", res.ContentType)
	return &http.Response{Status: http.StatusText(res.Status), StatusCode: res.Status, Proto: req.Proto, ProtoMajor: req.ProtoMajor, ProtoMinor: req.ProtoMinor, Body: io.NopCloser(bytes.NewReader(res.Body)), ContentLength: int64(len(res.Body)), Header: hdr, Request: req}
}
