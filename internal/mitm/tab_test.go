package mitm

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cursor-byok/internal/protocodec"
	aiserverv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
	"google.golang.org/protobuf/proto"
)

func tabProxyPost(t *testing.T, ctx context.Context, client *http.Client, method string, message proto.Message, framed bool) *http.Response {
	t.Helper()
	body, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	contentType := "application/proto"
	if framed {
		header := make([]byte, 5)
		binary.BigEndian.PutUint32(header[1:], uint32(len(body)))
		body = append(header, body...)
		contentType = "application/connect+proto"
	}
	service := "AiService"
	if method == "AvailableModels" {
		service = "CppService"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api2.cursor.sh/aiserver.v1."+service+"/"+method, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != 200 {
		t.Fatalf("%s status=%d", method, resp.StatusCode)
	}
	return resp
}

func tabProxyRead(t *testing.T, r io.Reader) (*aiserverv1.StreamCppResponse, map[string]any) {
	t.Helper()
	frame, err := protocodec.ReadFrame(r)
	if err != nil {
		t.Fatal(err)
	}
	body := frame.Data
	if frame.Flags&1 != 0 {
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		body, err = io.ReadAll(gz)
		gz.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if frame.IsEnd() {
		var end map[string]any
		if err := json.Unmarshal(body, &end); err != nil {
			t.Fatal(err)
		}
		return nil, end
	}
	var response aiserverv1.StreamCppResponse
	if err := proto.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	return &response, nil
}

func TestTabThroughTLSProxy(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("invalid provider JSON")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		answer, _ := json.Marshal(map[string]string{"replacement": "    return a + b"})
		chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": string(answer)}, "finish_reason": "stop"}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer provider.Close()
	_, client := testProxy(t, provider.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	modelResp := tabProxyPost(t, ctx, client, "AvailableModels", &aiserverv1.AvailableCppModelsRequest{}, false)
	modelBody, _ := io.ReadAll(modelResp.Body)
	var models aiserverv1.AvailableCppModelsResponse
	if proto.Unmarshal(modelBody, &models) != nil || len(models.Models) != 1 || models.GetDefaultModel() != "test-model" {
		t.Fatal("native Tab model-list prerequisite failed")
	}
	cfgResp := tabProxyPost(t, ctx, client, "CppConfig", &aiserverv1.CppConfigRequest{SupportsCpt: proto.Bool(true)}, false)
	body, _ := io.ReadAll(cfgResp.Body)
	var cfg aiserverv1.CppConfigResponse
	if proto.Unmarshal(body, &cfg) != nil || !cfg.GetIsOn() || cfg.GeoCppBackendUrl != "https://api2.cursor.sh" {
		t.Fatal("Tab config missing native routing")
	}
	request := &aiserverv1.StreamCppRequest{CurrentFile: &aiserverv1.CurrentFileInfo{RelyOnFilesync: true}, SupportsCpt: proto.Bool(true)}
	omitted := tabProxyPost(t, ctx, client, "StreamCpp", request, true)
	_, end := tabProxyRead(t, omitted.Body)
	detail := end["error"].(map[string]any)["details"].([]any)[0].(map[string]any)
	raw, err := base64.StdEncoding.DecodeString(detail["value"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var typed aiserverv1.ErrorDetails
	if proto.Unmarshal(raw, &typed) != nil || typed.Error != aiserverv1.ErrorDetails_ERROR_FILE_NOT_FOUND {
		t.Fatal("native full-content retry unavailable")
	}
	request.CurrentFile = &aiserverv1.CurrentFileInfo{RelativeWorkspacePath: "add.py", Contents: "def add(a, b):\n    return ", CursorPosition: &aiserverv1.CursorPosition{Line: 1, Column: 11}}
	stream := tabProxyPost(t, ctx, client, "StreamCpp", request, true)
	var frames []*aiserverv1.StreamCppResponse
	for {
		frame, end := tabProxyRead(t, stream.Body)
		if end != nil {
			if end["error"] != nil {
				t.Fatalf("Tab stream error: %v", end)
			}
			break
		}
		frames = append(frames, frame)
	}
	if len(frames) != 4 || frames[0].ModelInfo == nil || frames[1].GetRangeToReplace().GetStartLineNumber() != 2 || frames[1].GetRangeToReplace().GetEndLineNumberInclusive() != 2 || frames[2].Text != "    return a + b" || !frames[3].GetDoneStream() {
		t.Fatalf("invalid native edit: %v", frames)
	}
}

func TestTabProviderCanceledOnProxyStop(t *testing.T) {
	requested, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(requested)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	defer provider.Close()
	defer close(release)
	server, client := testProxy(t, provider.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := tabProxyPost(t, ctx, client, "StreamCpp", &aiserverv1.StreamCppRequest{CurrentFile: &aiserverv1.CurrentFileInfo{Contents: "x", CursorPosition: &aiserverv1.CursorPosition{Column: 1}}}, true)
	if first, _ := tabProxyRead(t, stream.Body); first.GetModelInfo() == nil {
		t.Fatal("missing initial native model info")
	}
	select {
	case <-requested:
	case <-ctx.Done():
		t.Fatal("provider not called")
	}
	if err := server.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("provider leaked after proxy stop")
	}
}
