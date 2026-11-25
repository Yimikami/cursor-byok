package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "cursor-byok/internal/protocodec/gen/agent/v1"
	aiserverv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func decodeContractFrame(t *testing.T, wire []byte) *agentv1.AgentServerMessage {
	t.Helper()
	if len(wire) < 5 || int(binary.BigEndian.Uint32(wire[1:5])) != len(wire)-5 {
		t.Fatalf("invalid Connect frame: %x", wire)
	}
	body := wire[5:]
	if wire[0]&flagCompressed != 0 {
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		body, err = io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	msg := &agentv1.AgentServerMessage{}
	if err := proto.Unmarshal(body, msg); err != nil {
		t.Fatal(err)
	}
	return msg
}

// Exercise the real binary ExecServer -> BidiAppend -> model/pill path. The
// fixtures represent client replies, not filesystem execution or UI acceptance.
func TestNativeToolWireContracts(t *testing.T) {
	replacePath := filepath.Join(t.TempDir(), "replace.txt")
	if err := os.WriteFile(replacePath, []byte("first\r\nold\r\nlast\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	replaceArgs, _ := json.Marshal(map[string]any{"path": replacePath, "old_string": "first\nold", "new_string": "first\nnew"})
	sess := &Session{McpMap: map[string]*McpRef{"mcp_0__lookup": {ServerName: "fixture", ServerID: "plugin-fixture-tools", ToolName: "lookup"}}}
	tests := []struct {
		name, args, reply, resultMarker string
		checkExec                       func(*testing.T, *agentv1.ExecServerMessage)
		checkResult                     func(*testing.T, *agentv1.ToolCall, *toolResultEnvelope)
	}{
		{"Shell", `{"command":"printf native-marker","working_directory":"/workspace","block_until_ms":0}`, `{"shellStream":{"backgrounded":{"shellId":19,"command":"printf native-marker","workingDirectory":"/workspace","pid":1234}}}`, "isBackground",
			func(t *testing.T, e *agentv1.ExecServerMessage) {
				a := e.GetShellStreamArgs()
				if a == nil || a.Command != "printf native-marker" || a.Timeout != 0 || a.SkipApproval || a.ToolCallId != "call-Shell" {
					t.Fatalf("shell args: %v", a)
				}
			},
			func(t *testing.T, tc *agentv1.ToolCall, env *toolResultEnvelope) {
				if !tc.GetShellToolCall().GetResult().GetIsBackground() || !strings.Contains(env.ResultJSON, "1234") {
					t.Fatal("background acknowledgment lost")
				}
			}},
		{"Read", `{"path":"/workspace/read.txt","offset":2,"limit":1}`, `{"readResult":{"success":{"path":"/workspace/read.txt","totalLines":3,"fileSize":13,"content":"first\nsecond\nthird\n"}}}`, "2|second",
			func(t *testing.T, e *agentv1.ExecServerMessage) {
				if e.GetReadArgs().GetPath() != "/workspace/read.txt" || e.GetReadArgs().GetToolCallId() != "call-Read" {
					t.Fatal(e)
				}
			},
			func(t *testing.T, tc *agentv1.ToolCall, env *toolResultEnvelope) {
				r := tc.GetReadToolCall().GetResult().GetSuccess()
				if r.GetContent() != "2|second" || r.GetTotalLines() != 3 || !strings.Contains(env.ResultJSON, "2|second") || strings.Contains(env.ResultJSON, "first") {
					t.Fatalf("read range lost: %s / %v", env.ResultJSON, r)
				}
			}},
		{"Write", `{"path":"/workspace/new.txt","contents":"new content\n"}`, `{"writeResult":{"success":{"path":"/workspace/new.txt","linesCreated":1,"fileContentAfterWrite":"new content\n"}}}`, "new content",
			func(t *testing.T, e *agentv1.ExecServerMessage) {
				a := e.GetWriteArgs()
				if a.GetPath() != "/workspace/new.txt" || a.GetFileText() != "new content\n" || !a.GetReturnFileContentAfterWrite() {
					t.Fatal(a)
				}
			}, nil},
		{"Grep", `{"pattern":"needle","path":"/workspace","glob":"*.go","-i":true,"-C":2,"head_limit":7,"output_mode":"files_with_matches"}`, `{"grepResult":{"success":{"pattern":"needle","path":"/workspace","outputMode":"files_with_matches"}}}`, "needle",
			func(t *testing.T, e *agentv1.ExecServerMessage) {
				a := e.GetGrepArgs()
				if a.GetPattern() != "needle" || !a.GetCaseInsensitive() || a.GetContext() != 2 || a.GetHeadLimit() != 7 || a.GetGlob() != "*.go" {
					t.Fatal(a)
				}
			}, nil},
		{"Glob", `{"glob_pattern":"*.go","target_directory":"C:\\workspace"}`, `{"lsResult":{"success":{"directoryTreeRoot":{"absPath":"C:\\workspace","childrenFiles":[{"name":"root.go"},{"name":"readme.txt"}],"childrenDirs":[{"absPath":"C:\\workspace\\src","childrenFiles":[{"name":"nested.go"},{"name":"ignored.txt"}]}]}}}}`, "nested.go",
			func(t *testing.T, e *agentv1.ExecServerMessage) {
				if e.GetLsArgs().GetPath() != `C:\workspace` {
					t.Fatal(e)
				}
			},
			func(t *testing.T, tc *agentv1.ToolCall, env *toolResultEnvelope) {
				files := tc.GetGlobToolCall().GetResult().GetSuccess().GetFiles()
				if len(files) != 2 || strings.Contains(env.ResultJSON, ".txt") || !strings.Contains(env.ResultJSON, "root.go") {
					t.Fatalf("glob filtering lost: %s / %v", env.ResultJSON, files)
				}
			}},
		{"Delete", `{"path":"/workspace/old.txt"}`, `{"deleteResult":{"success":{"path":"/workspace/old.txt","deletedFile":"old.txt","fileSize":7,"prevContent":"removed"}}}`, "removed",
			func(t *testing.T, e *agentv1.ExecServerMessage) {
				if e.GetDeleteArgs().GetPath() != "/workspace/old.txt" || e.GetDeleteArgs().GetToolCallId() != "call-Delete" {
					t.Fatal(e)
				}
			}, nil},
		{"StrReplace", string(replaceArgs), `{"writeResult":{"success":{"path":"/workspace/replace.txt","fileContentAfterWrite":"first\r\nnew\r\nlast\r\n"}}}`, "new",
			func(t *testing.T, e *agentv1.ExecServerMessage) {
				if e.GetWriteArgs().GetFileText() != "first\r\nnew\r\nlast\r\n" {
					t.Fatal(e)
				}
				data, _ := os.ReadFile(replacePath)
				if string(data) != "first\r\nold\r\nlast\r\n" {
					t.Fatal("StrReplace wrote directly, bypassing native execution")
				}
			}, nil},
		{"CallMcpTool", `{"server":"fixture","toolName":"lookup","arguments":{"query":"native-marker","limit":2}}`, `{"mcpResult":{"success":{"content":[{"text":{"text":"mcp-result-marker"}}]}}}`, "mcp-result-marker",
			func(t *testing.T, e *agentv1.ExecServerMessage) {
				a := e.GetMcpArgs()
				if a.GetName() != "plugin-fixture-tools-lookup" || a.GetProviderIdentifier() != "plugin-fixture-tools" || a.GetToolName() != "lookup" || a.GetArgs()["query"].GetStringValue() != "native-marker" || a.GetToolCallId() != "call-CallMcpTool" {
					t.Fatal(a)
				}
			}, nil},
		{"FetchMcpResource", `{"server":"plugin-fixture-tools","uri":"fixture://doc","downloadPath":"/workspace/doc.txt"}`, `{"readMcpResourceExecResult":{"success":{"uri":"fixture://doc","text":"resource-marker","mimeType":"text/plain"}}}`, "resource-marker",
			func(t *testing.T, e *agentv1.ExecServerMessage) {
				a := e.GetReadMcpResourceExecArgs()
				if a.GetServer() != "plugin-fixture-tools" || a.GetUri() != "fixture://doc" || a.GetDownloadPath() != "/workspace/doc.txt" {
					t.Fatal(a)
				}
			}, nil},
		{"ListMcpResources", `{"server":"plugin-fixture-tools"}`, `{"listMcpResourcesExecResult":{"success":{"resources":[{"uri":"fixture://doc","name":"Document","server":"plugin-fixture-tools","mimeType":"text/plain"}]}}}`, "fixture://doc",
			func(t *testing.T, e *agentv1.ExecServerMessage) {
				if e.GetListMcpResourcesExecArgs().GetServer() != "plugin-fixture-tools" {
					t.Fatal(e)
				}
			}, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scope := bidiToolScope(t.Name())
			defer DropSession(t.Name())
			pc := PendingToolCall{ID: "call-" + test.name, Name: test.name, Arguments: test.args}
			tc, buildErr := buildToolCallProto(sess, pc)
			if buildErr != "" || tc == nil {
				t.Fatalf("build: %s", buildErr)
			}
			var outbound bytes.Buffer
			if err := writeExecRequest(&outbound, "exec-native", 11, pc, tc); err != nil {
				t.Fatal(err)
			}
			exec := decodeContractFrame(t, outbound.Bytes()).GetExecServerMessage()
			if exec.GetId() != 11 || exec.GetExecId() != "exec-native" {
				t.Fatal("execution correlation lost")
			}
			test.checkExec(t, exec)
			client := &agentv1.ExecClientMessage{}
			if err := protojson.Unmarshal([]byte(test.reply), client); err != nil {
				t.Fatal(err)
			}
			client.Id = 11
			client.ExecId = "exec-native"
			payload, err := proto.Marshal(&agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: client}})
			if err != nil {
				t.Fatal(err)
			}
			waiter := registerToolWait(scope, pc.ID)
			defer cancelToolWait(waiter)
			registerExecIDAlias(scope, "exec-native", 11, pc.ID)
			res := handleAgentBidiAppendDecoded(&aiserverv1.BidiAppendRequest{RequestId: &aiserverv1.BidiRequestId{RequestId: t.Name()}, Data: hex.EncodeToString(payload)})
			if res.Status != 200 {
				t.Fatalf("bidi reply: %d", res.Status)
			}
			env := waitForToolResultContext(context.Background(), waiter, time.Second)
			if env.Error != "" {
				t.Fatal(env.Error)
			}
			attachToolResultToProto(tc, pc.Name, env)
			if test.checkResult != nil {
				test.checkResult(t, tc, env)
			}
			outbound.Reset()
			if err := writeToolCallCompleted(&outbound, pc.ID, tc); err != nil {
				t.Fatal(err)
			}
			complete := decodeContractFrame(t, outbound.Bytes()).GetInteractionUpdate().GetToolCallCompleted()
			if complete.GetCallId() != pc.ID {
				t.Fatal("completion call ID lost")
			}
			body, err := protojson.Marshal(complete.GetToolCall())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), test.resultMarker) {
				t.Fatalf("result missing from completion: %s", body)
			}
			assertScopeRetired(t, scope)
		})
	}
}

func TestDynamicMcpPreservesSchemaAndRouting(t *testing.T) {
	run := &agentv1.AgentRunRequest{}
	if err := protojson.Unmarshal([]byte(`{"mcpTools":{"mcpTools":[{"providerIdentifier":"plugin-fixture","toolName":"lookup.items","description":"Lookup items","inputSchema":{"type":"object","properties":{"ids":{"type":"array","items":{"type":"integer"}}},"required":["ids"]}}]}}`), run); err != nil {
		t.Fatal(err)
	}
	sess := &Session{Run: run}
	tools := openAIToolsForRequest(sess)
	var name string
	for _, tool := range tools {
		var f struct {
			Name       string         `json:"name"`
			Parameters map[string]any `json:"parameters"`
		}
		if err := json.Unmarshal(tool.Function, &f); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(f.Name, "mcp_") {
			name = f.Name
			p, _ := json.Marshal(f.Parameters)
			if !strings.Contains(string(p), `"required":["ids"]`) || !strings.Contains(string(p), `"integer"`) {
				t.Fatalf("MCP schema lost: %s", p)
			}
		}
	}
	if name == "" {
		t.Fatal("MCP tool not advertised")
	}
	pc := PendingToolCall{ID: "dynamic-call", Name: name, Arguments: `{"ids":[1,2]}`}
	tc, err := buildToolCallProto(sess, pc)
	if err != "" {
		t.Fatal(err)
	}
	a := tc.GetMcpToolCall().GetArgs()
	if a.GetName() != "plugin-fixture-lookup.items" || a.GetProviderIdentifier() != "plugin-fixture" || a.GetToolName() != "lookup.items" || len(a.GetArgs()["ids"].GetListValue().GetValues()) != 2 {
		t.Fatalf("MCP route lost: %v", a)
	}
	if _, err := buildToolCallProto(sess, PendingToolCall{ID: "bad", Name: "mcp_99__unknown", Arguments: `{}`}); err == "" {
		t.Fatal("unadvertised dynamic MCP tool accepted")
	}
}

func TestShellTerminalEventsCompleteWithoutTimeout(t *testing.T) {
	for _, fixture := range []string{`{"shellStream":{"rejected":{"reason":"User declined"}}}`, `{"shellStream":{"permissionDenied":{"error":"Permission required"}}}`, `{"shellResult":{"failure":{"command":"false","exitCode":2,"stderr":"failed"}}}`} {
		t.Run(fixture, func(t *testing.T) {
			scope := socketToolScope(t.Name())
			ch := registerToolWait(scope, "call")
			registerExecIDAlias(scope, "exec", 1, "call")
			msg := &agentv1.ExecClientMessage{}
			if err := protojson.Unmarshal([]byte(fixture), msg); err != nil {
				t.Fatal(err)
			}
			msg.Id = 1
			routeExecClientResult(scope, &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: msg}})
			env := waitForToolResultContext(context.Background(), ch, time.Second)
			if env.Error != "" {
				t.Fatal(env.Error)
			}
			tc, _ := buildToolCallProto(nil, PendingToolCall{Name: "Shell", Arguments: `{"command":"false"}`})
			attachToolResultToProto(tc, "Shell", env)
			if tc.GetShellToolCall().GetResult().GetResult() == nil {
				t.Fatal("terminal result not visible")
			}
			assertScopeRetired(t, scope)
		})
	}
}

func TestReadRangesAndNativeErrorResults(t *testing.T) {
	for _, test := range []struct{ args, want string }{
		{`{"path":"/file","offset":-1}`, "3|third"},
		{`{"path":"/file","offset":-2,"limit":1,"include_line_numbers":false}`, "second"},
		{`{"path":"/file","offset":10}`, ""},
	} {
		tc, err := buildToolCallProto(nil, PendingToolCall{Name: "Read", Arguments: test.args})
		if err != "" {
			t.Fatal(err)
		}
		msg := &agentv1.ExecClientMessage{}
		if err := protojson.Unmarshal([]byte(`{"readResult":{"success":{"path":"/file","totalLines":3,"content":"first\nsecond\nthird\n"}}}`), msg); err != nil {
			t.Fatal(err)
		}
		attachToolResultToProto(tc, "Read", &toolResultEnvelope{ExecClient: msg})
		if got := tc.GetReadToolCall().GetResult().GetSuccess().GetContent(); got != test.want {
			t.Fatalf("args %s got %q want %q", test.args, got, test.want)
		}
	}
	for _, test := range []struct{ name, args, reply, marker string }{
		{"Read", `{"path":"/missing"}`, `{"readResult":{"fileNotFound":{"path":"/missing"}}}`, "File not found"},
		{"Read", `{"path":"/private"}`, `{"readResult":{"permissionDenied":{"path":"/private"}}}`, "Permission denied"},
		{"CallMcpTool", `{"server":"fixture","toolName":"lookup"}`, `{"mcpResult":{"rejected":{"reason":"User declined"}}}`, "User declined"},
		{"CallMcpTool", `{"server":"fixture","toolName":"lookup"}`, `{"mcpResult":{"permissionDenied":{"error":"Consent required"}}}`, "Consent required"},
		{"CallMcpTool", `{"server":"fixture","toolName":"lookup"}`, `{"mcpResult":{"toolNotFound":{"name":"fixture-lookup"}}}`, "MCP tool not found"},
		{"Write", `{"path":"/file","contents":"before format"}`, `{"writeResult":{"success":{"path":"/file","fileContentAfterWrite":"AFTER FORMAT"}}}`, "AFTER FORMAT"},
	} {
		tc, err := buildToolCallProto(nil, PendingToolCall{Name: test.name, Arguments: test.args})
		if err != "" {
			t.Fatal(err)
		}
		msg := &agentv1.ExecClientMessage{}
		if err := protojson.Unmarshal([]byte(test.reply), msg); err != nil {
			t.Fatal(err)
		}
		attachToolResultToProto(tc, test.name, &toolResultEnvelope{ExecClient: msg})
		body, _ := protojson.Marshal(tc)
		if !strings.Contains(string(body), test.marker) {
			t.Fatalf("native result lost: %s", body)
		}
	}
}

func TestMalformedNativeArgumentsAreNotExecuted(t *testing.T) {
	for _, name := range []string{"Shell", "Read", "Write", "Grep", "Glob", "Delete", "StrReplace", "CallMcpTool", "FetchMcpResource", "ListMcpResources"} {
		for _, args := range []string{`null`, `[]`, `{broken`} {
			if tc, err := buildToolCallProto(nil, PendingToolCall{Name: name, Arguments: args}); tc != nil || err == "" {
				t.Fatalf("%s accepted %s", name, args)
			}
		}
	}
	for _, args := range []string{`{"path":"/file"}`, `{"path":"/file","contents":null}`} {
		if tc, err := buildToolCallProto(nil, PendingToolCall{Name: "Write", Arguments: args}); tc != nil || err == "" {
			t.Fatalf("Write accepted missing contents: %s", args)
		}
	}
	if tc, err := buildToolCallProto(nil, PendingToolCall{Name: "Write", Arguments: `{"path":"/file","contents":""}`}); tc == nil || err != "" {
		t.Fatal("intentional empty file write rejected")
	}
	for _, args := range []string{`{"command":"echo test"}`, `{"command":"echo test","block_until_ms":45000}`} {
		tc, err := buildToolCallProto(nil, PendingToolCall{Name: "Shell", Arguments: args})
		if err != "" {
			t.Fatal(err)
		}
		want := int32(30000)
		if strings.Contains(args, "45000") {
			want = 45000
		}
		if tc.GetShellToolCall().GetArgs().GetTimeout() != want || tc.GetShellToolCall().GetArgs().GetSkipApproval() {
			t.Fatal("native shell timeout/approval policy lost")
		}
	}
}
