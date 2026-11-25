package relay

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"cursor-byok/internal/protocodec"
)

func rewriteTestFrame(flags byte, body []byte) []byte {
	return append(binary.BigEndian.AppendUint32([]byte{flags}, uint32(len(body))), body...)
}

func TestModelRewritersPreserveFramingAndCompression(t *testing.T) {
	adapters := []AdapterInfo{{ModelID: "model", DisplayName: "Model"}}
	gateway := NewGateway()
	gateway.SetAdapterProvider(func() []AdapterInfo { return adapters })
	zip := func(body []byte) []byte {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		_, _ = w.Write(body)
		_ = w.Close()
		return b.Bytes()
	}
	for path, rewrite := range rewrites {
		for _, shape := range []string{"raw", "framed", "framed-trailer", "frame-gzip", "http-gzip"} {
			t.Run(path+"/"+shape, func(t *testing.T) {
				body := []byte{10, 3, 'o', 'l', 'd'}
				ct := "application/proto"
				if shape != "raw" {
					ct = "application/connect+proto"
					flags := byte(0)
					if shape == "frame-gzip" {
						body = zip(body)
						flags = 1
					}
					body = rewriteTestFrame(flags, body)
				}
				if shape == "framed-trailer" {
					body = append(body, rewriteTestFrame(2, []byte(`{"metadata":{}}`))...)
				}
				if shape == "http-gzip" {
					body = zip(body)
				}
				response := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}
				response.Header.Set("Content-Type", ct)
				if shape == "http-gzip" {
					response.Header.Set("Content-Encoding", "gzip")
				}
				req := httptest.NewRequest("POST", "https://api2.cursor.sh"+path, nil)
				if !gateway.MaybeRewriteResponse(req, response) {
					t.Fatal("rewrite failed")
				}
				got, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				want, err := rewrite(nil, adapters)
				if err != nil {
					t.Fatal(err)
				}
				if response.Header.Get("Content-Type") != ct || response.Header.Get("Content-Encoding") != "" {
					t.Fatal("response headers don't match wire body")
				}
				if shape == "raw" {
					if !bytes.Equal(got, want) {
						t.Fatal("raw body changed framing")
					}
					return
				}
				reader := bytes.NewReader(got)
				frame, err := protocodec.ReadFrame(reader)
				if err != nil || frame.Flags != 0 || !bytes.Equal(frame.Data, want) {
					t.Fatalf("framed rewrite: %+v %v", frame, err)
				}
				if shape == "framed-trailer" {
					trailer, err := protocodec.ReadFrame(reader)
					if err != nil || trailer.Flags != 2 || string(trailer.Data) != `{"metadata":{}}` {
						t.Fatal("trailer changed")
					}
				}
				if reader.Len() != 0 {
					t.Fatal("unexpected bytes after response")
				}
			})
		}
	}
}

func TestMalformedModelFramesRemainUnchanged(t *testing.T) {
	for _, input := range [][]byte{{0, 0, 0, 0, 8, 10}, rewriteTestFrame(1, []byte("bad gzip")), append(rewriteTestFrame(2, []byte(`{}`)), rewriteTestFrame(0, []byte{10, 0})...)} {
		if _, _, err := tryRewrite(input, []AdapterInfo{{ModelID: "model"}}, rewriteGetDefaultModel); err == nil {
			t.Fatal("malformed envelope rewritten as success")
		}
	}
}
