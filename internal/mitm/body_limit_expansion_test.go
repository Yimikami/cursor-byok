package mitm

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodedBodyLimitsWireAndGzipExpansion(t *testing.T) {
	const limit = 1024
	for _, compressed := range []bool{false, true} {
		for _, size := range []int{limit, limit + 1, limit * 64} {
			raw := []byte(strings.Repeat("x", size))
			var payload bytes.Buffer
			if compressed {
				gz := gzip.NewWriter(&payload)
				_, _ = gz.Write(raw)
				_ = gz.Close()
			} else {
				payload.Write(raw)
			}
			req := httptest.NewRequest("POST", "/", &payload)
			req.ContentLength = -1 // exercise streaming/chunked reads too
			if compressed {
				req.Header.Set("Content-Encoding", "gzip")
			}
			got, err := readDecodedBodyLimit(req, limit)
			if size > limit {
				if err == nil || got != nil {
					t.Fatalf("oversized compressed=%v size=%d accepted", compressed, size)
				}
			} else if err != nil || !bytes.Equal(got, raw) {
				t.Fatalf("boundary compressed=%v failed: %v", compressed, err)
			}
		}
	}
	req := httptest.NewRequest("POST", "/", io.NopCloser(strings.NewReader("bad gzip")))
	req.Header.Set("Content-Encoding", "gzip")
	if _, err := readDecodedBodyLimit(req, limit); err == nil {
		t.Fatal("invalid compression accepted")
	}
	req.Body = nil
	if _, err := readDecodedBodyLimit(req, limit); err == nil {
		t.Fatal("nil body accepted")
	}
}
