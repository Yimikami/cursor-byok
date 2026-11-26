package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	inferencev1 "cursor-byok/internal/protocodec/gen/inference/v1"
)

// Cursor executes the normalized structured calls but replays the native block
// array on its next invocation. Keep only repaired tool inputs in sync; signed
// thinking and all other blocks retain their original bytes and order.
func repairInferenceAnthropicToolInputs(raw json.RawMessage, repaired []PendingToolCall) (json.RawMessage, error) {
	if len(repaired) == 0 {
		return raw, nil
	}
	if err := validateProviderToolCalls(repaired); err != nil {
		return nil, err
	}
	byID := make(map[string]PendingToolCall, len(repaired))
	for _, call := range repaired {
		byID[call.ID] = call
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, errors.New("invalid Anthropic content while repairing tool history")
	}
	matched := make(map[string]bool, len(repaired))
	for i, rawBlock := range blocks {
		var block struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(rawBlock, &block); err != nil {
			return nil, errors.New("invalid Anthropic block while repairing tool history")
		}
		call, ok := byID[block.ID]
		if block.Type != "tool_use" || !ok {
			continue
		}
		if matched[block.ID] || block.Name != call.Name {
			return nil, errors.New("ambiguous Anthropic tool identity while repairing history")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rawBlock, &fields); err != nil {
			return nil, err
		}
		fields["input"] = json.RawMessage(call.Arguments)
		updated, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		blocks[i] = updated
		matched[block.ID] = true
	}
	if len(matched) != len(repaired) {
		return nil, errors.New("repaired Anthropic tool call is missing from native history")
	}
	var encoded bytes.Buffer
	encoded.WriteByte('[')
	for i, block := range blocks {
		if i > 0 {
			encoded.WriteByte(',')
		}
		encoded.Write(block)
	}
	encoded.WriteByte(']')
	return encoded.Bytes(), nil
}

// Cursor 3.32.4 preserves ResponseMessage.anthropic_native_content (field12)
// as CoreMessage.anthropic_native_content (field18) on the next invocation.
// This carries the entire ordered assistant block array, including signed and
// redacted thinking, rather than trying to recreate it from visible text.
func validateAnthropicNativeContent(raw json.RawMessage) error {
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil || len(blocks) == 0 {
		return errors.New("invalid Anthropic assistant content")
	}
	for _, block := range blocks {
		switch block.Type {
		case "thinking":
			if block.Signature == "" {
				return errors.New("Anthropic thinking block is missing its signature")
			}
		case "redacted_thinking":
			if block.Data == "" {
				return errors.New("Anthropic redacted thinking block is missing its data")
			}
		case "text":
		case "tool_use":
			var input map[string]json.RawMessage
			if block.ID == "" || block.Name == "" || json.Unmarshal(block.Input, &input) != nil || input == nil {
				return errors.New("invalid Anthropic assistant tool block")
			}
		default:
			return fmt.Errorf("unsupported Anthropic assistant block %q", block.Type)
		}
	}
	return nil
}

// Keep the native assistant part order alongside the authoritative raw content.
// Cursor may replay either representation on the next invocation.
func preserveInferenceAnthropicOrder(message *inferencev1.InferenceResponseMessage, raw json.RawMessage) error {
	if err := validateAnthropicNativeContent(raw); err != nil {
		return err
	}
	var blocks []anthropicContentBlock
	_ = json.Unmarshal(raw, &blocks)
	for _, block := range blocks {
		switch block.Type {
		case "thinking", "redacted_thinking":
			message.AssistantPartOrder = append(message.AssistantPartOrder, 1)
		case "text":
			message.AssistantPartOrder = append(message.AssistantPartOrder, 2)
			message.AssistantTextParts = append(message.AssistantTextParts, block.Text)
		case "tool_use":
			message.AssistantPartOrder = append(message.AssistantPartOrder, 3)
		}
	}
	return nil
}

func decodeInferenceAnthropicContent(native *inferencev1.InferenceCoreMessage, message openAIMessage) (json.RawMessage, error) {
	if raw := native.GetAnthropicNativeContent(); raw != "" {
		return append(json.RawMessage(nil), []byte(raw)...), nil
	}
	if len(native.GetReasoningParts()) == 0 {
		return nil, nil
	}
	var reasoning, text, tools []anthropicContentBlock
	hasAnthropicReasoning := false
	for _, part := range native.GetReasoningParts() {
		if !part.GetIsRedacted() && part.GetSignature() == "" {
			// Generic reasoning (for example OpenAI reasoning_content) is not
			// an Anthropic thinking block. Keep its position so mixed histories
			// still consume assistant_part_order correctly, but never forge a
			// signed block when the user switches providers.
			reasoning = append(reasoning, anthropicContentBlock{})
			continue
		}
		hasAnthropicReasoning = true
		block := anthropicContentBlock{Type: "thinking", Thinking: part.GetText(), Signature: part.GetSignature()}
		if part.GetIsRedacted() {
			block = anthropicContentBlock{Type: "redacted_thinking", Data: part.GetRedactedData()}
		}
		reasoning = append(reasoning, block)
	}
	if !hasAnthropicReasoning {
		// Normal text and tool calls remain in message. Generic reasoning is
		// retained separately for providers that accept reasoning_content.
		return nil, nil
	}
	if parts := native.GetAssistantTextParts(); len(parts) > 0 {
		for _, part := range parts {
			text = append(text, anthropicContentBlock{Type: "text", Text: part})
		}
	} else if value, ok := decodeAsString(message.Content); ok && value != "" {
		text = append(text, anthropicContentBlock{Type: "text", Text: value})
	}
	for _, call := range message.ToolCalls {
		tools = append(tools, anthropicContentBlock{Type: "tool_use", ID: call.ID, Name: call.Function.Name, Input: json.RawMessage(call.Function.Arguments)})
	}
	order := native.GetAssistantPartOrder()
	var blocks []anthropicContentBlock
	if len(order) == 0 {
		for _, block := range reasoning {
			if (block.Signature != "" || block.Type == "redacted_thinking") && (len(text) > 0 || len(tools) > 0) {
				return nil, errors.New("signed Anthropic history is missing assistant part order")
			}
		}
		blocks = append(append(reasoning, text...), tools...)
	} else {
		parts := map[int32][]anthropicContentBlock{1: reasoning, 2: text, 3: tools}
		for _, kind := range order {
			if len(parts[kind]) == 0 {
				return nil, errors.New("invalid assistant part order")
			}
			blocks = append(blocks, parts[kind][0])
			parts[kind] = parts[kind][1:]
		}
		for _, remaining := range parts {
			if len(remaining) > 0 {
				return nil, errors.New("incomplete assistant part order")
			}
		}
	}
	// Preserve required empty strings too: omitempty on the shared content
	// struct is useful for other variants but must not erase signed payloads.
	var encoded []map[string]any
	for _, block := range blocks {
		var value map[string]any
		switch block.Type {
		case "": // unsigned generic reasoning placeholder
			continue
		case "thinking":
			value = map[string]any{"type": block.Type, "thinking": block.Thinking, "signature": block.Signature}
		case "redacted_thinking":
			value = map[string]any{"type": block.Type, "data": block.Data}
		case "text":
			value = map[string]any{"type": block.Type, "text": block.Text}
		case "tool_use":
			value = map[string]any{"type": block.Type, "id": block.ID, "name": block.Name, "input": block.Input}
		}
		encoded = append(encoded, value)
	}
	return json.Marshal(encoded)
}
