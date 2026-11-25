package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "cursor-byok/internal/protocodec/gen/agent/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func nativeString(s string) *string { return &s }

func nativeUserAction(mode agentv1.AgentMode, rc *agentv1.RequestContext) *agentv1.ConversationAction {
	return &agentv1.ConversationAction{Action: &agentv1.ConversationAction_UserMessageAction{
		UserMessageAction: &agentv1.UserMessageAction{UserMessage: &agentv1.UserMessage{Text: "test", Mode: mode}, RequestContext: rc},
	}}
}

func TestNativePlanActionsPreservePromptAndContext(t *testing.T) {
	rc := &agentv1.RequestContext{Rules: []*agentv1.CursorRule{{Content: "PLAN_RULE_MARKER"}}}
	for _, tc := range []struct {
		name   string
		action *agentv1.ConversationAction
		mode   agentv1.AgentMode
		want   string
	}{
		{"start", &agentv1.ConversationAction{Action: &agentv1.ConversationAction_StartPlanAction{StartPlanAction: &agentv1.StartPlanAction{
			UserMessage: &agentv1.UserMessage{Text: "Plan a cache", SelectedContext: &agentv1.SelectedContext{Files: []*agentv1.SelectedFile{{Content: "SELECTED_FILE_MARKER"}}}}, RequestContext: rc,
		}}}, agentv1.AgentMode_AGENT_MODE_PLAN, "Plan a cache"},
		{"start without text", &agentv1.ConversationAction{Action: &agentv1.ConversationAction_StartPlanAction{StartPlanAction: &agentv1.StartPlanAction{RequestContext: rc}}}, agentv1.AgentMode_AGENT_MODE_PLAN, "Create an implementation plan"},
		{"execute file", &agentv1.ConversationAction{Action: &agentv1.ConversationAction_ExecutePlanAction{ExecutePlanAction: &agentv1.ExecutePlanAction{
			RequestContext: rc, PlanFileContent: nativeString("Implement cache TTL"), Plan: &agentv1.ConversationPlan{Plan: "older plan"},
		}}}, agentv1.AgentMode_AGENT_MODE_AGENT, "Implement cache TTL"},
		{"execute plan", &agentv1.ConversationAction{Action: &agentv1.ConversationAction_ExecutePlanAction{ExecutePlanAction: &agentv1.ExecutePlanAction{
			RequestContext: rc, Plan: &agentv1.ConversationPlan{Plan: "Investigate cache bug"}, ExecutionMode: agentv1.AgentMode_AGENT_MODE_DEBUG,
		}}}, agentv1.AgentMode_AGENT_MODE_DEBUG, "Investigate cache bug"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := proto.Marshal(&agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_RunRequest{RunRequest: &agentv1.AgentRunRequest{Action: tc.action}}})
			if err != nil {
				t.Fatal(err)
			}
			var decoded agentv1.AgentClientMessage
			if err := proto.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			sess := &Session{}
			extractIntoSession(sess, &decoded)
			if sess.Mode != tc.mode || !strings.Contains(sess.UserText, tc.want) {
				t.Fatalf("mode/prompt = %v %q", sess.Mode, sess.UserText)
			}
			context := buildWorkspaceContext(sess)
			if !strings.Contains(context, "PLAN_RULE_MARKER") {
				t.Fatal("plan action lost request context when env is absent")
			}
			if tc.name == "start" && !strings.Contains(context, "SELECTED_FILE_MARKER") {
				t.Fatal("start plan lost selected file context")
			}
		})
	}
}

func TestSkillContextUsesEnabledDescriptorsFromBothSources(t *testing.T) {
	local := &agentv1.SkillDescriptor{Name: "local-skill", Description: "LOCAL_DESCRIPTION", FolderPath: "/workspace/skills/local", ReadmeFilePath: "/workspace/skills/local/SKILL.md", Enabled: true}
	disabled := &agentv1.SkillDescriptor{Name: "disabled-skill", ReadmeFilePath: "/skills/disabled.md", Enabled: false}
	broken := &agentv1.SkillDescriptor{Name: "broken-skill", ReadmeFilePath: "/skills/broken.md", Enabled: true, ParseError: nativeString("invalid frontmatter")}
	rc := &agentv1.RequestContext{SkillOptions: &agentv1.SkillOptions{SkillDescriptors: []*agentv1.SkillDescriptor{local, disabled, broken}}}
	sess := &Session{Action: nativeUserAction(agentv1.AgentMode_AGENT_MODE_AGENT, rc), Run: &agentv1.AgentRunRequest{SkillOptions: &agentv1.SkillOptions{SkillDescriptors: []*agentv1.SkillDescriptor{
		local, {Name: "run-skill", ReadmeFilePath: "/skills/run/SKILL.md", Enabled: true}, {Name: "disabled-skill", ReadmeFilePath: "/skills/disabled.md", Enabled: true},
	}}}}
	context := buildWorkspaceContext(sess)
	for _, want := range []string{"LOCAL_DESCRIPTION", "/workspace/skills/local/SKILL.md", "run-skill", "using Read before applying"} {
		if !strings.Contains(context, want) {
			t.Fatalf("missing skill context %q: %s", want, context)
		}
	}
	for _, absent := range []string{"disabled-skill", "broken-skill"} {
		if strings.Contains(context, absent) {
			t.Fatalf("unavailable skill leaked: %s", absent)
		}
	}
	if strings.Count(context, "LOCAL_DESCRIPTION") != 1 {
		t.Fatal("duplicate skill descriptor")
	}
}

func TestDebugSessionConfigPreservesInstalledWireField(t *testing.T) {
	raw := protowire.AppendTag(nil, 1, protowire.BytesType)
	raw = protowire.AppendString(raw, "/tmp/debug.log")
	raw = protowire.AppendTag(raw, 2, protowire.BytesType)
	raw = protowire.AppendString(raw, "http://127.0.0.1:7777/ingest")
	raw = protowire.AppendTag(raw, 3, protowire.BytesType)
	raw = protowire.AppendString(raw, "debug-session-42")
	debug := &agentv1.DebugModeConfig{}
	if err := proto.Unmarshal(raw, debug); err != nil {
		t.Fatal(err)
	}
	sess := &Session{Mode: agentv1.AgentMode_AGENT_MODE_DEBUG, Action: nativeUserAction(agentv1.AgentMode_AGENT_MODE_DEBUG, &agentv1.RequestContext{DebugModeConfig: debug})}
	context := buildWorkspaceContext(sess)
	for _, want := range []string{"/tmp/debug.log", "http://127.0.0.1:7777/ingest", "debug-session-42"} {
		if !strings.Contains(context, want) {
			t.Fatalf("missing debug configuration %q", want)
		}
	}
	sess.Mode = agentv1.AgentMode_AGENT_MODE_ASK
	if strings.Contains(buildWorkspaceContext(sess), "<debug_session>") {
		t.Fatal("stale debug config exposed outside Debug mode")
	}
}

func TestMcpToolsSurviveNativePlanActionsAndRunFallback(t *testing.T) {
	definition := filepath.Join(t.TempDir(), "read-tool.json")
	if err := os.WriteFile(definition, []byte(`{"description":"Read fixture","inputSchema":{"type":"object","properties":{"document":{"type":"string"}},"required":["document"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	schema, _ := structpb.NewValue(map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}})
	rc := &agentv1.RequestContext{Tools: []*agentv1.McpToolDefinition{{ProviderIdentifier: "docs", ToolName: "search", InputSchema: schema}}}
	sess := &Session{Mode: agentv1.AgentMode_AGENT_MODE_PLAN,
		Action: &agentv1.ConversationAction{Action: &agentv1.ConversationAction_ExecutePlanAction{ExecutePlanAction: &agentv1.ExecutePlanAction{RequestContext: rc}}},
		Run:    &agentv1.AgentRunRequest{McpFileSystemOptions: &agentv1.McpFileSystemOptions{McpDescriptors: []*agentv1.McpDescriptor{{ServerName: "docs", ServerIdentifier: "docs", Tools: []*agentv1.McpToolDescriptor{{ToolName: "read", DefinitionPath: &definition}}}}}},
	}
	tools := openAIToolsForRequest(sess)
	if len(sess.McpMap) != 2 {
		t.Fatalf("MCP tools = %#v", sess.McpMap)
	}
	for _, tool := range tools {
		var fn struct {
			Name       string `json:"name"`
			Parameters struct {
				Properties map[string]any `json:"properties"`
			} `json:"parameters"`
		}
		if err := json.Unmarshal(tool.Function, &fn); err != nil {
			t.Fatal(err)
		}
		if ref := sess.McpMap[fn.Name]; ref != nil {
			want := "document"
			if ref.ToolName == "search" {
				want = "query"
			}
			if _, ok := fn.Parameters.Properties[want]; !ok {
				t.Fatalf("missing actual MCP schema: %s", tool.Function)
			}
		}
	}
	sess.Run = nil
	_ = openAIToolsForRequest(sess)
	if len(sess.McpMap) != 1 {
		t.Fatal("tool refresh retained stale MCP routing entries")
	}
}

func TestReadOnlyModesRejectFileMutationsAtExposureAndExecution(t *testing.T) {
	for _, mode := range []agentv1.AgentMode{agentv1.AgentMode_AGENT_MODE_ASK, agentv1.AgentMode_AGENT_MODE_PLAN} {
		sess := &Session{Mode: mode}
		for _, name := range []string{"Shell", "Write", "Delete", "StrReplace"} {
			if toolAllowedInMode(sess, name) {
				t.Fatalf("exposed %s in %v", name, mode)
			}
			if _, err := buildToolCallProto(sess, PendingToolCall{Name: name, Arguments: `{}`}); err == "" {
				t.Fatalf("executed %s in %v", name, mode)
			}
		}
		for _, tool := range openAIToolsForRequest(sess) {
			var fn struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(tool.Function, &fn)
			if !toolAllowedInMode(sess, fn.Name) {
				t.Fatalf("advertised forbidden %s", fn.Name)
			}
		}
		if !toolAllowedInMode(sess, "CallMcpTool") || !toolAllowedInMode(sess, "mcp_0__query") {
			t.Fatal("native MCP delegation disabled")
		}
		if err := toolModeError(sess, PendingToolCall{Name: "FetchMcpResource", Arguments: `{"downloadPath":"/tmp/output"}`}); err == "" {
			t.Fatal("allowed resource download to write a file")
		}
	}
}

func TestModeSwitchRequiresExplicitMatchingApproval(t *testing.T) {
	for _, outcome := range []string{"timeout", "rejected", "empty", "wrong id", "approved"} {
		t.Run(outcome, func(t *testing.T) {
			sess := &Session{Mode: agentv1.AgentMode_AGENT_MODE_ASK}
			_, pill, query, handled := handleLocalTool(sess, PendingToolCall{ID: "mode-test", Name: "SwitchMode", Arguments: `{"target_mode_id":"agent","explanation":"Apply the requested fix"}`})
			if !handled || query == nil || sess.Mode != agentv1.AgentMode_AGENT_MODE_ASK {
				t.Fatal("mode changed before approval")
			}
			var verdict *agentv1.InteractionResponse
			if outcome != "timeout" {
				sm := &agentv1.SwitchModeRequestResponse{}
				if outcome == "rejected" {
					sm.Result = &agentv1.SwitchModeRequestResponse_Rejected_{Rejected: &agentv1.SwitchModeRequestResponse_Rejected{Reason: "Keep explaining"}}
				}
				if outcome == "approved" || outcome == "wrong id" {
					sm.Result = &agentv1.SwitchModeRequestResponse_Approved_{Approved: &agentv1.SwitchModeRequestResponse_Approved{}}
				}
				verdict = &agentv1.InteractionResponse{Id: query.GetId(), Result: &agentv1.InteractionResponse_SwitchModeRequestResponse{SwitchModeRequestResponse: sm}}
				if outcome == "wrong id" {
					verdict.Id++
				}
			}
			result := applyModeSwitchVerdict(sess, pill, query, verdict)
			if outcome == "approved" {
				if sess.Mode != agentv1.AgentMode_AGENT_MODE_AGENT || !toolAllowedInMode(sess, "Write") || pill.GetSwitchModeToolCall().GetResult().GetSuccess() == nil {
					t.Fatalf("approved switch not applied: %s", result)
				}
			} else if sess.Mode != agentv1.AgentMode_AGENT_MODE_ASK {
				t.Fatalf("unapproved switch escaped Ask: %s", result)
			}
		})
	}
}

func TestNativeModesReachProviderWithContextAndMatchingTools(t *testing.T) {
	for _, mode := range []agentv1.AgentMode{agentv1.AgentMode_AGENT_MODE_AGENT, agentv1.AgentMode_AGENT_MODE_ASK, agentv1.AgentMode_AGENT_MODE_PLAN, agentv1.AgentMode_AGENT_MODE_DEBUG} {
		t.Run(modeString(mode), func(t *testing.T) {
			requests := make(chan openAIRequest, 1)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request openAIRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				requests <- request
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"NATIVE_MODE_OK\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer provider.Close()
			rc := &agentv1.RequestContext{
				SkillOptions:    &agentv1.SkillOptions{SkillDescriptors: []*agentv1.SkillDescriptor{{Name: "contract-skill", ReadmeFilePath: "/skills/contract/SKILL.md", Enabled: true}}},
				DebugModeConfig: &agentv1.DebugModeConfig{LogPath: "/tmp/contract.log", ServerEndpoint: "http://127.0.0.1:7777/ingest", SessionId: "contract-debug"},
			}
			action := nativeUserAction(mode, rc)
			if mode == agentv1.AgentMode_AGENT_MODE_PLAN {
				action = &agentv1.ConversationAction{Action: &agentv1.ConversationAction_StartPlanAction{StartPlanAction: &agentv1.StartPlanAction{UserMessage: &agentv1.UserMessage{Text: "Plan a smoke test"}, RequestContext: rc}}}
			}
			sess := &Session{RequestID: t.Name()}
			extractIntoSession(sess, &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_RunRequest{RunRequest: &agentv1.AgentRunRequest{Action: action}}})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var nativeOutput bytes.Buffer
			runAgentSession(ctx, sess.RequestID, sess, &nativeOutput, func() []AdapterTarget {
				return []AdapterTarget{{ProviderType: "openai", BaseURL: provider.URL, APIKey: "local-test-key", Model: "contract-model"}}
			})
			select {
			case request := <-requests:
				if request.Model != "contract-model" || !request.Stream {
					t.Fatal("native mode did not use streaming provider")
				}
				joined, _ := json.Marshal(request.Messages)
				if !bytes.Contains(joined, []byte("Current Mode: "+strings.ToUpper(modeString(mode)[:1])+modeString(mode)[1:])) || !bytes.Contains(joined, []byte("contract-skill")) {
					t.Fatalf("lost mode/skill context: %s", joined)
				}
				if mode == agentv1.AgentMode_AGENT_MODE_DEBUG && !bytes.Contains(joined, []byte("contract-debug")) {
					t.Fatal("lost debug session at provider boundary")
				}
				for _, tool := range request.Tools {
					var fn struct {
						Name string `json:"name"`
					}
					_ = json.Unmarshal(tool.Function, &fn)
					if !toolAllowedInMode(sess, fn.Name) {
						t.Fatalf("forbidden tool sent to provider: %s", fn.Name)
					}
				}
			default:
				t.Fatal("native action never reached provider")
			}
			if nativeOutput.Len() == 0 {
				t.Fatal("provider response not returned as native frames")
			}
		})
	}
}
