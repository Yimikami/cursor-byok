package agent

import (
	"encoding/json"
	"errors"
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
)

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

func encodeInferenceAnthropicMessage(message []byte, raw json.RawMessage, model string) ([]byte, error) {
	if err := validateAnthropicNativeContent(raw); err != nil {
		return nil, err
	}
	message = append(message, providerProtoBytes(12, raw)...)
	var blocks []anthropicContentBlock
	_ = json.Unmarshal(raw, &blocks)
	for _, block := range blocks {
		kind := uint64(0)
		switch block.Type {
		case "thinking", "redacted_thinking":
			kind = 1
			part := providerProtoString(5, model)
			if block.Type == "thinking" {
				part = append(part, providerProtoString(2, block.Thinking)...)
				part = append(part, providerProtoString(3, block.Signature)...)
			} else {
				part = append(part, providerProtoVarint(1, 1)...)
				part = append(part, providerProtoString(4, block.Data)...)
			}
			message = append(message, providerProtoBytes(6, part)...)
		case "text":
			kind = 2
			message = append(message, providerProtoString(11, block.Text)...)
		case "tool_use":
			kind = 3
		}
		message = append(message, providerProtoVarint(10, kind)...)
	}
	return message, nil
}

func decodeInferenceAnthropicContent(fields providerWireFields, message openAIMessage) (json.RawMessage, error) {
	if raw := fields.lastBytes(18); len(raw) > 0 {
		return append(json.RawMessage(nil), raw...), nil
	}
	if len(fields.bytes(7)) == 0 {
		return nil, nil
	}
	var reasoning, text, tools []anthropicContentBlock
	for _, raw := range fields.bytes(7) {
		part, err := providerProtoFields(raw)
		if err != nil {
			return nil, err
		}
		block := anthropicContentBlock{Type: "thinking", Thinking: string(part.lastBytes(2)), Signature: string(part.lastBytes(3))}
		if part.varint(1) != 0 {
			block = anthropicContentBlock{Type: "redacted_thinking", Data: string(part.lastBytes(4))}
		}
		reasoning = append(reasoning, block)
	}
	if parts := fields.bytes(17); len(parts) > 0 {
		for _, part := range parts {
			text = append(text, anthropicContentBlock{Type: "text", Text: string(part)})
		}
	} else if value, ok := decodeAsString(message.Content); ok && value != "" {
		text = append(text, anthropicContentBlock{Type: "text", Text: value})
	}
	for _, call := range message.ToolCalls {
		tools = append(tools, anthropicContentBlock{Type: "tool_use", ID: call.ID, Name: call.Function.Name, Input: json.RawMessage(call.Function.Arguments)})
	}
	var order []uint64
	for _, field := range fields {
		if field.number != 16 {
			continue
		}
		if field.kind == protowire.VarintType {
			order = append(order, field.value)
		}
		if field.kind == protowire.BytesType {
			for raw := field.data; len(raw) > 0; {
				value, n := protowire.ConsumeVarint(raw)
				if n < 0 {
					return nil, protowire.ParseError(n)
				}
				order = append(order, value)
				raw = raw[n:]
			}
		}
	}
	var blocks []anthropicContentBlock
	if len(order) == 0 {
		for _, block := range reasoning {
			if (block.Signature != "" || block.Type == "redacted_thinking") && (len(text) > 0 || len(tools) > 0) {
				return nil, errors.New("signed Anthropic history is missing assistant part order")
			}
		}
		blocks = append(append(reasoning, text...), tools...)
	} else {
		parts := map[uint64][]anthropicContentBlock{1: reasoning, 2: text, 3: tools}
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
