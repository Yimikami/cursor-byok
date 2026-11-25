package agent

import "strings"

// Signed and redacted reasoning is provider state. Keep it distinct from
// ordinary text so a tool continuation can replay the exact original blocks.
type providerReasoningPart struct {
	Text         string
	Signature    string
	RedactedData string
	IsRedacted   bool
	ModelName    string
	LineageTag   string
}

func preserveProviderReasoning(message *openAIMessage, result *streamResult) {
	if result == nil {
		return
	}
	message.ReasoningParts = result.ReasoningParts
	message.AnthropicNativeContent = result.AnthropicNativeContent
	var text strings.Builder
	for _, part := range result.ReasoningParts {
		if !part.IsRedacted {
			text.WriteString(part.Text)
		}
	}
	message.ReasoningContent = text.String()
}
