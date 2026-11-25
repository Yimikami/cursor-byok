package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	agentv1 "cursor-byok/internal/protocodec/gen/agent/v1"
)

// Cursor sends native Plan buttons as actions, not ordinary chat messages.
// Preserve their meaning instead of rejecting them as an empty prompt.
func extractConversationAction(sess *Session, action *agentv1.ConversationAction) {
	if sess == nil || action == nil {
		return
	}
	if msg := userMessageForAction(action); msg != nil {
		sess.UserText = msg.GetText()
		if mode := msg.GetMode(); mode != agentv1.AgentMode_AGENT_MODE_UNSPECIFIED {
			sess.Mode = mode
		}
	}
	if action.GetStartPlanAction() != nil {
		sess.Mode = agentv1.AgentMode_AGENT_MODE_PLAN
		if strings.TrimSpace(sess.UserText) == "" {
			sess.UserText = "Create an implementation plan for the current conversation."
		}
	}
	if execute := action.GetExecutePlanAction(); execute != nil {
		sess.Mode = execute.GetExecutionMode()
		if sess.Mode == agentv1.AgentMode_AGENT_MODE_UNSPECIFIED {
			sess.Mode = agentv1.AgentMode_AGENT_MODE_AGENT
		}
		plan := execute.GetPlanFileContent()
		if plan == "" && execute.GetPlan() != nil {
			plan = execute.GetPlan().GetPlan()
		}
		sess.UserText = "Execute the plan selected in Cursor."
		if plan != "" {
			sess.UserText += "\n\n<selected_plan>\n" + plan + "\n</selected_plan>"
		} else if uri := execute.GetPlanFileUri(); uri != "" {
			sess.UserText += "\nPlan file: " + uri + "\nRead the plan before making changes."
		}
	}
}

func userMessageForAction(action *agentv1.ConversationAction) *agentv1.UserMessage {
	if action == nil {
		return nil
	}
	if user := action.GetUserMessageAction(); user != nil {
		return user.GetUserMessage()
	}
	if plan := action.GetStartPlanAction(); plan != nil {
		return plan.GetUserMessage()
	}
	return nil
}

func appendNativeContext(b *strings.Builder, sess *Session) {
	if sess == nil {
		return
	}
	rc := requestContextFor(sess)
	var sources []*agentv1.SkillOptions
	// Request context describes this action; run-level options are a fallback.
	if rc != nil {
		sources = append(sources, rc.GetSkillOptions())
	}
	if sess.Run != nil {
		sources = append(sources, sess.Run.GetSkillOptions())
	}
	seen := map[string]bool{}
	var skills []string
	for _, options := range sources {
		for _, skill := range options.GetSkillDescriptors() {
			if skill == nil {
				continue
			}
			path := skill.GetReadmeFilePath()
			if path == "" && skill.GetFolderPath() != "" {
				path = filepath.Join(skill.GetFolderPath(), "SKILL.md")
			}
			key := skill.GetName() + "\x00" + path
			if seen[key] {
				continue
			}
			seen[key] = true
			if !skill.GetEnabled() || skill.GetParseError() != "" || path == "" {
				continue
			}
			skills = append(skills, fmt.Sprintf("- name=%q path=%q\n  %s\n", skill.GetName(), path, trimForContext(skill.GetDescription(), 4000)))
		}
	}
	if len(skills) > 0 {
		b.WriteString("\n<available_skills>\nThese enabled skills were supplied by Cursor for this request. When a skill is requested or relevant, read its listed instruction file using Read before applying it. Follow its scope and referenced resources; do not invent its contents.\n")
		for _, skill := range skills {
			b.WriteString(skill)
		}
		b.WriteString("</available_skills>\n")
	}
	if sess.Mode == agentv1.AgentMode_AGENT_MODE_DEBUG && rc != nil {
		if debug := rc.GetDebugModeConfig(); debug != nil {
			b.WriteString("\n<debug_session>\nUse this Cursor-provided configuration for runtime evidence. Form hypotheses, add focused instrumentation when needed, ask for reproduction, and inspect the log before claiming a cause. Do not invent a missing endpoint or log path.\n")
			if path := debug.GetLogPath(); path != "" {
				fmt.Fprintf(b, "log_path=%q\n", path)
			}
			if endpoint := debug.GetServerEndpoint(); endpoint != "" {
				fmt.Fprintf(b, "server_endpoint=%q\n", endpoint)
			}
			if id := debug.GetSessionId(); id != "" {
				fmt.Fprintf(b, "session_id=%q\n", id)
			}
			b.WriteString("</debug_session>\n")
		}
	}
}

func readOnlyMode(sess *Session) bool {
	return sess != nil && (sess.Mode == agentv1.AgentMode_AGENT_MODE_ASK || sess.Mode == agentv1.AgentMode_AGENT_MODE_PLAN)
}

// Keep file mutations out of Ask and Plan even if the provider ignores its
// advertised tool set. MCP permissions remain the native executor's decision.
func toolAllowedInMode(sess *Session, name string) bool {
	if !readOnlyMode(sess) {
		return true
	}
	switch name {
	case "Write", "Delete", "StrReplace", "Shell":
		return false
	case "CreatePlan", "AddTodo", "UpdateTodo":
		return sess.Mode == agentv1.AgentMode_AGENT_MODE_PLAN
	default:
		return true
	}
}

func toolModeError(sess *Session, pc PendingToolCall) string {
	if !toolAllowedInMode(sess, pc.Name) {
		return fmt.Sprintf("%s is unavailable in %s mode; switch to Agent or Debug with approval first", pc.Name, modeString(sess.Mode))
	}
	if readOnlyMode(sess) && pc.Name == "FetchMcpResource" {
		var args struct {
			DownloadPath string `json:"downloadPath"`
		}
		if err := json.Unmarshal([]byte(pc.Arguments), &args); err != nil {
			return "parse FetchMcpResource args: " + err.Error()
		}
		if args.DownloadPath != "" {
			return "MCP resource downloads write files; switch to Agent or Debug with approval first"
		}
	}
	return ""
}

func applyModeSwitchVerdict(sess *Session, pill *agentv1.ToolCall, query *agentv1.InteractionQuery, verdict *agentv1.InteractionResponse) string {
	if sess == nil || query == nil || query.GetSwitchModeRequestQuery() == nil {
		return `{"result":"error","note":"Invalid mode switch request; mode unchanged."}`
	}
	if verdict == nil {
		return `{"result":"timeout","note":"No mode switch approval received; stay in the current mode."}`
	}
	response := verdict.GetSwitchModeRequestResponse()
	if verdict.GetId() != query.GetId() || response == nil {
		return `{"result":"error","note":"Unexpected mode switch response; mode unchanged."}`
	}
	if rejected := response.GetRejected(); rejected != nil {
		return `{"result":"rejected","reason":` + jsonString(rejected.GetReason()) + `,"note":"Stay in the current mode."}`
	}
	if response.GetApproved() == nil {
		return `{"result":"error","note":"Missing mode switch verdict; mode unchanged."}`
	}
	args := query.GetSwitchModeRequestQuery().GetArgs()
	var mode agentv1.AgentMode
	switch args.GetTargetModeId() {
	case "agent":
		mode = agentv1.AgentMode_AGENT_MODE_AGENT
	case "chat", "ask":
		mode = agentv1.AgentMode_AGENT_MODE_ASK
	case "plan":
		mode = agentv1.AgentMode_AGENT_MODE_PLAN
	case "debug":
		mode = agentv1.AgentMode_AGENT_MODE_DEBUG
	default:
		return `{"result":"error","note":"Unknown target mode; mode unchanged."}`
	}
	previous := sess.Mode
	sess.Mode = mode
	if call := pill.GetSwitchModeToolCall(); call != nil {
		call.Result = &agentv1.SwitchModeResult{Result: &agentv1.SwitchModeResult_Success{Success: &agentv1.SwitchModeSuccess{
			FromModeId: cursorPickerID(previous), ToModeId: args.GetTargetModeId(),
		}}}
	}
	return `{"result":"approved","note":"Mode switch approved by Cursor; the new mode is active."}`
}
