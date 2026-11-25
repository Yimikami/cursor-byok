package agent

import (
	inferencev1 "cursor-byok/internal/protocodec/gen/inference/v1"
	"google.golang.org/protobuf/proto"
)

// localInferencePromptMetadata selects Cursor's shipped generic prompt path.
// "unknown" and "latest" are accepted by the 3.31.15 harness's Xqr guard. An
// OpenAI-compatible transport does not identify the underlying model family,
// so we deliberately set no GPT/Claude/Composer family flags. Cursor constructs
// its own native prompt and tools with its pinned feature-flag defaults.
func localInferencePromptMetadata() *inferencev1.RunInferencePromptModelMetadata {
	metadata := &inferencev1.RunInferencePromptModelMetadata{
		Vendor: "unknown", PromptVersion: "latest", SelfIdentityName: "Cursor", UseNewPlanModePrompts: proto.Bool(true),
		AskQuestionConfig: &inferencev1.RunInferenceAskQuestionConfig{ToolName: "AskQuestion"},
	}
	// fW/uB use "chat" for Ask. hen requires an explicit row for every
	// non-Agent mode. The native toolsGenerator applies each mode's editing
	// boundaries; this only enables its question/mode interaction tools.
	modes := []string{"agent", "chat", "plan", "debug", "triage", "multitask"}
	for _, mode := range modes {
		row := &inferencev1.RunInferenceAgentModeConfig{ModeId: mode, EnableAskQuestionTool: true}
		for _, target := range modes {
			if target != mode {
				if row.SwitchModeToolConfig == nil {
					row.SwitchModeToolConfig = &inferencev1.RunInferenceSwitchModeToolConfig{}
				}
				row.SwitchModeToolConfig.TargetModes = append(row.SwitchModeToolConfig.TargetModes, target)
			}
		}
		metadata.AgentModeConfigs = append(metadata.AgentModeConfigs, row)
	}
	return metadata
}
