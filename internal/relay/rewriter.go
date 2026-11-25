package relay

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	_ "cursor-byok/internal/protocodec/gen/aiserver/v1"
)

// AdapterInfo carries the bits of a configured BYOK adapter that the rewriter
// needs to forge synthetic responses.
type AdapterInfo struct {
	DisplayName             string
	Type                    string
	ModelID                 string
	BaseURL                 string
	APIKey                  string
	SupportedThinkingLevels []string
	ServiceTier             string
	MaxOutputTokens         int
	ThinkingBudget          int
}

// StableID returns a 16-char hex identifier derived from the provider model
// ID alone. Cursor caches this id across many SQLite keys
// (aiSettings.modelConfig.<feature>.modelName, featureModelConfigs.<feature>
// .defaultModel, etc.); deriving from ModelID — not display name — keeps the
// id stable when the user only renames the adapter, so Cursor's cached
// references remain valid.
func (a AdapterInfo) StableID() string {
	h := sha256.Sum256([]byte("byok|" + a.ModelID))
	return hex.EncodeToString(h[:8])
}

// rewriter is a per-path response transformer. It receives the upstream body
// (already gunzipped by the caller if needed) plus the user's adapter list
// and returns a new body to send back to Cursor.
type rewriter func(body []byte, adapters []AdapterInfo) ([]byte, error)

var rewrites = map[string]rewriter{
	"/aiserver.v1.AiService/AvailableModels": rewriteAvailableModels,
	"/aiserver.v1.AiService/GetDefaultModel": rewriteGetDefaultModel,
}

// SyntheticPath returns the hand-rolled response bytes (raw protobuf, no
// Connect envelope) for paths the working app implements via local mocks
// rather than upstream forwarding. Returns nil if the path has no synthetic
// response. Callers must wrap in a Connect envelope themselves if the
// request used Connect framing.
func (g *Gateway) SyntheticPath(path string) []byte {
	if g.adapterProvider == nil {
		return nil
	}
	adapters := g.adapterProvider()
	if len(adapters) == 0 {
		return nil
	}
	switch path {
	case "/aiserver.v1.AiService/GetDefaultModel":
		return buildGetDefaultModelResponse(adapters)
	case "/aiserver.v1.AiService/GetDefaultModelNudgeData":
		return buildGetDefaultModelNudgeDataResponse(adapters)
	case "/aiserver.v1.AiService/AvailableModels":
		return buildAvailableModelsResponse(adapters)
	}
	return nil
}

// MaybeRewriteResponse swaps the body of any path we know how to forge. It
// transparently handles 5-byte Connect framing and HTTP-level gzip.
func (g *Gateway) MaybeRewriteResponse(req *http.Request, resp *http.Response) bool {
	if req == nil || req.URL == nil || resp == nil || resp.Body == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	fn, ok := rewrites[req.URL.Path]
	if !ok || g.adapterProvider == nil {
		return false
	}
	adapters := g.adapterProvider()
	if len(adapters) == 0 {
		return false
	}

	originalBody := resp.Body
	body, err := io.ReadAll(io.LimitReader(originalBody, (32<<20)+1))
	if err != nil || len(body) > 32<<20 {
		// Rewriting is optional; retain the original stream when buffering fails.
		resp.Body = &prefixResponseBody{Reader: io.MultiReader(bytes.NewReader(body), originalBody), Closer: originalBody}
		return false
	}
	_ = originalBody.Close()

	decoded := body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		decoded, err = gunzipBytes(body)
		if err != nil {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			return false
		}
	}
	newBody, framed, err := tryRewrite(decoded, adapters, fn)
	if err != nil {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return false
	}

	resp.Body = io.NopCloser(bytes.NewReader(newBody))
	resp.ContentLength = int64(len(newBody))
	resp.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
	if framed {
		if !strings.Contains(resp.Header.Get("Content-Type"), "grpc") {
			resp.Header.Set("Content-Type", "application/connect+proto")
		}
	} else {
		resp.Header.Set("Content-Type", "application/proto")
	}
	resp.Header.Del("Content-Encoding")
	return true
}

type prefixResponseBody struct {
	io.Reader
	io.Closer
}

// tryRewrite detects framing before calling a body-independent synthetic
// rewriter. Low flag bytes cannot begin a valid protobuf field tag. Preserve
// every data envelope and the original end/trailer envelope in sequence.
func tryRewrite(body []byte, adapters []AdapterInfo, fn rewriter) (newBody []byte, framed bool, err error) {
	if len(body) == 0 || body[0] > 3 {
		out, err := fn(body, adapters)
		return out, false, err
	}
	var result []byte
	for len(body) > 0 {
		if len(body) < 5 || body[0] > 3 {
			return nil, true, errors.New("invalid Connect envelope")
		}
		flags := body[0]
		length := uint64(binary.BigEndian.Uint32(body[1:5]))
		if length > uint64(len(body)-5) {
			return nil, true, errors.New("truncated Connect envelope")
		}
		frame := body[:5+int(length)]
		body = body[len(frame):]
		if flags&2 != 0 {
			if len(body) != 0 {
				return nil, true, errors.New("data after Connect end envelope")
			}
			result = append(result, frame...)
			continue
		}
		payload := frame[5:]
		if flags&1 != 0 {
			payload, err = gunzipBytes(payload)
			if err != nil {
				return nil, true, err
			}
		}
		out, err := fn(payload, adapters)
		if err != nil {
			return nil, true, err
		}
		result = append(result, 0)
		result = binary.BigEndian.AppendUint32(result, uint32(len(out)))
		result = append(result, out...)
	}
	return result, true, nil
}

// ---------------- Per-endpoint rewriters ----------------

func rewriteAvailableModels(body []byte, adapters []AdapterInfo) ([]byte, error) {
	// Build the response from scratch using the byte template that matches
	// the working app's wire format exactly. The proto types we ship are
	// missing several fields the picker relies on, so going through
	// proto.Marshal would lose them.
	_ = body
	return buildAvailableModelsResponse(adapters), nil
}

func rewriteGetDefaultModel(body []byte, adapters []AdapterInfo) ([]byte, error) {
	if len(adapters) == 0 {
		return body, nil
	}
	return buildGetDefaultModelResponse(adapters), nil
}

func rewriteGetDefaultModelNudge(body []byte, adapters []AdapterInfo) ([]byte, error) {
	// Append-mode: leave the nudge data as Cursor sent it.
	return body, nil
}

func rewriteCppAvailableModels(body []byte, adapters []AdapterInfo) ([]byte, error) {
	// Append-mode: leave tab/cmd-K models alone.
	return body, nil
}

// (injectBYOKModels was deleted — buildAvailableModelsResponse in
// models_template.go now produces the response directly with byte-exact
// fidelity to the working app's wire format.)

func gunzipBytes(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out, err := io.ReadAll(io.LimitReader(r, (32<<20)+1))
	if len(out) > 32<<20 {
		return nil, errors.New("expanded response exceeds limit")
	}
	return out, err
}

// AvailableModelsPath kept for backwards compatibility with anything that
// imported the constant. Use the rewrites map for new code.
const AvailableModelsPath = "/aiserver.v1.AiService/AvailableModels"

var _ = fmt.Sprintf
