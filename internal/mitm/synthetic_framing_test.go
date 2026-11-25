package mitm

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"cursor-byok/internal/certs"
	"cursor-byok/internal/protocodec"
	"cursor-byok/internal/relay"
)

func TestSyntheticModelResponseMatchesConnectRequest(t *testing.T) {
	ca, err := certs.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gateway := relay.NewGateway()
	gateway.SetAdapterProvider(func() []relay.AdapterInfo { return []relay.AdapterInfo{{ModelID: "model"}} })
	server, err := New("127.0.0.1:0", ca, gateway, nil, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop(context.Background())
	for _, path := range []string{"GetDefaultModel", "AvailableModels", "GetDefaultModelNudgeData"} {
		req := httptest.NewRequest("POST", "https://api2.cursor.sh/aiserver.v1.AiService/"+path, bytes.NewReader([]byte{0, 0, 0, 0, 0}))
		req.Header.Set("Content-Type", "application/connect+proto")
		response := httptest.NewRecorder()
		server.srv.Handler.ServeHTTP(response, req)
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/connect+proto" {
			t.Fatalf("%s: status/header mismatch", path)
		}
		reader := bytes.NewReader(response.Body.Bytes())
		data, err := protocodec.ReadFrame(reader)
		if err != nil || data.IsEnd() || !bytes.Equal(data.Data, gateway.SyntheticPath(req.URL.Path)) {
			t.Fatalf("%s: invalid data frame", path)
		}
		end, err := protocodec.ReadFrame(reader)
		if err != nil || !end.IsEnd() || string(end.Data) != "{}" {
			t.Fatalf("%s: missing end frame", path)
		}
		if _, err := reader.ReadByte(); err != io.EOF {
			t.Fatal("extra frame data")
		}
	}
}
