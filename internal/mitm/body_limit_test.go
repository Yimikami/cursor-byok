package mitm

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"cursor-byok/internal/agent"
)

func TestNativeTextBodyLimitBeforeProvider(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		body := bytes.Repeat([]byte("x"), (1<<20)+1)
		if compressed {
			var encoded bytes.Buffer
			writer := gzip.NewWriter(&encoded)
			_, _ = writer.Write(body)
			_ = writer.Close()
			body = encoded.Bytes()
		}
		req := httptest.NewRequest(http.MethodPost, "https://api2.cursor.sh/aiserver.v1.AiService/NameTab", bytes.NewReader(body))
		if compressed {
			req.Header.Set("Content-Encoding", "gzip")
		}
		req.ContentLength = -1 // Chunked requests must receive the same bound.
		response := handleNativeText(req, func() []agent.AdapterTarget {
			t.Fatal("oversized request reached provider resolution")
			return nil
		}, "")
		_ = response.Body.Close()
		if response.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("compressed=%t status=%d", compressed, response.StatusCode)
		}
	}
}

func TestDecodedBodyBoundsBothRepresentations(t *testing.T) {
	for _, size := range []int{32, 33} {
		for _, compressed := range []bool{false, true} {
			payload := bytes.Repeat([]byte("x"), size)
			wire := payload
			if compressed {
				var encoded bytes.Buffer
				writer := gzip.NewWriter(&encoded)
				_, _ = writer.Write(payload)
				_ = writer.Close()
				wire = encoded.Bytes()
			}
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(wire))
			req.ContentLength = -1
			if compressed {
				req.Header.Set("Content-Encoding", "gzip")
			}
			body, err := readDecodedBodyLimit(req, 32)
			if size == 33 && !errors.Is(err, errRequestBodyTooLarge) {
				t.Fatalf("unbounded decoded body compressed=%t: %v", compressed, err)
			}
			if size == 32 && (err != nil || !bytes.Equal(body, payload)) {
				t.Fatalf("valid bounded payload rejected: %v", err)
			}
		}
	}
	// An enormous declared length is rejected without reading an upload.
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Body = io.NopCloser(failRead{t})
	req.ContentLength = 1000
	if _, err := readDecodedBodyLimit(req, 32); !errors.Is(err, errRequestBodyTooLarge) {
		t.Fatal(err)
	}
}

type failRead struct{ t *testing.T }

func (r failRead) Read([]byte) (int, error) {
	r.t.Fatal("read beyond declared body bound")
	return 0, io.EOF
}
