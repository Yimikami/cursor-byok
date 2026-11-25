package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"cursor-byok/internal/protocodec"
	aiserverv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
	"google.golang.org/protobuf/proto"
)

func tabFrames(t *testing.T, wire []byte) ([]*aiserverv1.StreamCppResponse, map[string]any) {
	t.Helper()
	var out []*aiserverv1.StreamCppResponse
	reader := bytes.NewReader(wire)
	for {
		frame, err := protocodec.ReadFrame(reader)
		if err != nil {
			t.Fatal(err)
		}
		body := frame.Data
		if frame.Flags&1 != 0 {
			zr, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			body, err = io.ReadAll(zr)
			zr.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		if frame.IsEnd() {
			var end map[string]any
			if err := json.Unmarshal(body, &end); err != nil {
				t.Fatal(err)
			}
			return out, end
		}
		response := &aiserverv1.StreamCppResponse{}
		if err := proto.Unmarshal(body, response); err != nil {
			t.Fatal(err)
		}
		out = append(out, response)
	}
}

func TestTabContextCoordinates(t *testing.T) {
	for _, tc := range []struct {
		name, contents      string
		line, column, start int32
		valid               bool
		wantLine            int32
	}{
		{"CRLF", "before\r\n  value = \r\nafter", 1, 10, 0, true, 2},
		{"unicode", "x😀", 0, 3, 0, true, 1},
		{"surrogate split", "x😀", 0, 2, 0, false, 0},
		{"outside line", "one", 1, 0, 0, false, 0},
		{"outside column", "one", 0, 4, 0, false, 0},
		{"empty file", "", 0, 0, 0, true, 1},
		{"partial context", "a\nb", 1, 1, 20, true, 21},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildTabContext(&aiserverv1.StreamCppRequest{CurrentFile: &aiserverv1.CurrentFileInfo{Contents: tc.contents, ContentsStartAtLine: tc.start, CursorPosition: &aiserverv1.CursorPosition{Line: tc.line, Column: tc.column}}})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
			if err == nil && got.LineOneIndexed != tc.wantLine {
				t.Fatalf("line=%d", got.LineOneIndexed)
			}
		})
	}
}

func TestTabConfigAndFilesyncRetry(t *testing.T) {
	list := HandleAvailableCppModels(nil, "application/proto", func() []AdapterTarget {
		return []AdapterTarget{{Model: "first"}, {Model: "second"}, {Model: "first"}, {}}
	})
	var models aiserverv1.AvailableCppModelsResponse
	if proto.Unmarshal(list.Body, &models) != nil || len(models.Models) != 2 || models.GetDefaultModel() != "first" {
		t.Fatal("Tab picker must advertise configured models in priority order")
	}
	for _, configured := range []bool{false, true} {
		result := HandleCppConfig(nil, "application/proto", func() []AdapterTarget {
			if configured {
				return []AdapterTarget{{Model: "fixture"}}
			}
			return nil
		})
		var cfg aiserverv1.CppConfigResponse
		if result.Status != 200 || proto.Unmarshal(result.Body, &cfg) != nil || cfg.GetIsOn() != configured || cfg.GeoCppBackendUrl != tabBackendURL || cfg.IsFusedCursorPredictionModel {
			t.Fatalf("invalid config: %v", &cfg)
		}
	}
	var wire bytes.Buffer
	body, _ := proto.Marshal(&aiserverv1.StreamCppRequest{CurrentFile: &aiserverv1.CurrentFileInfo{RelyOnFilesync: true}})
	HandleStreamCpp(context.Background(), body, "application/proto", &wire, func() []AdapterTarget { t.Fatal("filesync omission must not call provider"); return nil }, "")
	frames, end := tabFrames(t, wire.Bytes())
	if len(frames) != 0 {
		t.Fatal("filesync omission generated a suggestion")
	}
	connectError := end["error"].(map[string]any)
	detail := connectError["details"].([]any)[0].(map[string]any)
	raw, err := base64.StdEncoding.DecodeString(detail["value"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var decoded aiserverv1.ErrorDetails
	if proto.Unmarshal(raw, &decoded) != nil || decoded.Error != aiserverv1.ErrorDetails_ERROR_FILE_NOT_FOUND || detail["type"] != "aiserver.v1.ErrorDetails" {
		t.Fatal("client filesync fallback would not activate")
	}
}

func TestTabProviderReplacementAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name, answer, finish string
		success              bool
	}{
		{"replacement", "{\"replacement\":\"  value = 42\\n  next = value  \"}", "stop", true},
		{"empty replacement", "{\"replacement\":\"\"}", "stop", true},
		{"malformed", "some code", "stop", false},
		{"missing property", "{}", "stop", false},
		{"truncated", "{\"replacement\":\"unsafe partial\"}", "length", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request openAIRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if request.Model != "tab-model" || request.MaxTokens > 512 || len(request.Tools) != 0 {
					t.Error("Tab provider mapping lost")
				}
				joined, _ := json.Marshal(request.Messages)
				if !bytes.Contains(joined, []byte("current_line")) || !bytes.Contains(joined, []byte("before")) {
					t.Error("Tab context lost")
				}
				chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": tc.answer}, "finish_reason": tc.finish}}})
				w.Header().Set("Content-Type", "text/event-stream")
				w.Write([]byte("data: " + string(chunk) + "\n\ndata: [DONE]\n\n"))
			}))
			defer provider.Close()
			body, _ := proto.Marshal(&aiserverv1.StreamCppRequest{CurrentFile: &aiserverv1.CurrentFileInfo{Contents: "before\r\n  value = \r\nafter", CursorPosition: &aiserverv1.CursorPosition{Line: 1, Column: 10}}})
			var wire bytes.Buffer
			HandleStreamCpp(context.Background(), body, "application/proto", &wire, func() []AdapterTarget {
				return []AdapterTarget{{ProviderType: "openai", BaseURL: provider.URL, APIKey: "test-only", Model: "tab-model"}}
			}, "")
			frames, end := tabFrames(t, wire.Bytes())
			if !tc.success {
				if end["error"] == nil {
					t.Fatal("invalid output accepted")
				}
				return
			}
			if end["error"] != nil || len(frames) != 4 || frames[0].ModelInfo == nil || frames[1].GetRangeToReplace().GetStartLineNumber() != 2 || frames[1].GetRangeToReplace().GetEndLineNumberInclusive() != 2 || !frames[3].GetDoneStream() {
				t.Fatalf("native frame contract failed: %v %v", frames, end)
			}
			for _, frame := range frames {
				if frame.GetDoneEdit() || frame.GetBeginEdit() {
					t.Fatal("non-fused reader rejects edit marker")
				}
			}
			if tc.name == "replacement" && frames[2].Text != "  value = 42\r\n  next = value  " {
				t.Fatalf("whitespace lost: %q", frames[2].Text)
			}
		})
	}
}

func TestInstalledTabTextConsumer(t *testing.T) {
	artifact := os.Getenv("CURSOR_BYOK_WORKBENCH_ARTIFACT")
	if artifact == "" {
		t.Skip("set CURSOR_BYOK_WORKBENCH_ARTIFACT for installed text-consumer check")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const fs=require('node:fs'),vm=require('node:vm');
const src=fs.readFileSync(process.env.CURSOR_BYOK_WORKBENCH_ARTIFACT,'utf8');
const a=src.indexOf('function XMw('),b=src.indexOf('Qo(),Ky()',a);
if(a<0||b<0||b-a>12000)throw new Error('Tab reader moved');
const context=vm.createContext({});
vm.runInContext(src.slice(a,b),context,{timeout:1000});
const test="(async()=>{const stream={async *[Symbol.asyncIterator](){yield {case:'text',text:'  x = 42  '}}};const result=await ZMw(stream[Symbol.asyncIterator](),'  x = ',0);if(result[0]!=='  x = 42  ')throw new Error('replacement lost');let rejected=false;try{await ZMw((async function*(){yield {case:'doneEdit'}})(),'x',0)}catch{rejected=true}if(!rejected)throw new Error('negative edit marker accepted')})()";
vm.runInContext(test,context,{timeout:1000}).then(()=>process.stdout.write('installed Tab reader passed'),err=>{process.stderr.write(String(err));process.exitCode=1});
`
	command := exec.Command(node, "-e", script)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("client reader: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "passed") {
		t.Fatal("missing installed reader verdict")
	}
}
