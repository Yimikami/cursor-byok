package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// Cursor 3.32.4's InferenceService sends complete provider messages and tools;
// its local agent executes the resulting tool calls. These field numbers come
// from the shipped cursor-always-local InferenceStreamRequest/Response schema.
// send receives an inner InferenceStreamResponse, not an outer run envelope.
func runInferenceProvider(ctx context.Context, request []byte, target AdapterTarget, send func([]byte) error) error {
	messages, tools, opts, invocationID, err := decodeInferenceProviderRequest(request, target.Opts)
	if err != nil {
		return err
	}
	emit := func(body []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return send(body)
	}
	if invocationID != "" {
		if err := emit(providerProtoBytes(7, providerProtoString(1, invocationID))); err != nil {
			return err
		}
	}
	var text, reasoning strings.Builder
	stream := providerStreamer(streamOpenAI)
	if strings.EqualFold(target.ProviderType, "anthropic") {
		stream = streamAnthropic
	}
	result, err := stream(ctx, target.BaseURL, target.APIKey, target.Model, messages, tools, opts, func(content, thinking string, done bool) error {
		if content != "" {
			text.WriteString(content)
			if err := emit(providerProtoBytes(1, providerProtoString(1, content))); err != nil {
				return err
			}
		}
		if thinking != "" {
			reasoning.WriteString(thinking)
			if err := emit(providerProtoBytes(9, providerProtoString(1, thinking))); err != nil {
				return err
			}
		}
		return ctx.Err()
	})
	if err != nil {
		return err
	}
	if result == nil {
		return errors.New("provider returned no inference result")
	}
	if err := inferenceProviderStreamError(result); err != nil {
		return err
	}
	if text.Len() == 0 && len(result.ToolCalls) == 0 {
		return errors.New("provider stream ended without text or tool calls")
	}
	// Validate the entire batch before the first executable tool frame leaves
	// the proxy. A later malformed call must not cause a partially dispatched
	// batch that Cursor might repeat when retrying the failed invocation.
	if err := validateProviderToolCalls(result.ToolCalls); err != nil {
		return err
	}
	responseID := "byok-" + uuid.NewString()
	message := providerProtoString(1, responseID+"-assistant")
	message = append(message, providerProtoVarint(2, 2)...)
	message = append(message, providerProtoString(3, text.String())...)
	if len(result.AnthropicContent) > 0 {
		message, err = encodeInferenceAnthropicMessage(message, result.AnthropicContent, target.Model)
		if err != nil {
			return err
		}
	}
	for index, call := range result.ToolCalls {
		args := call.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		var object structpb.Struct
		if err := protojson.Unmarshal([]byte(args), &object); err != nil {
			return fmt.Errorf("provider tool %s returned invalid object arguments: %w", call.Name, err)
		}
		encodedArgs, err := proto.Marshal(&object)
		if err != nil {
			return err
		}
		tool := providerProtoString(1, call.ID)
		tool = append(tool, providerProtoString(2, call.Name)...)
		tool = append(tool, providerProtoBytes(3, encodedArgs)...)
		tool = append(tool, providerProtoString(4, args)...)
		message = append(message, providerProtoBytes(4, tool)...)
		part := providerProtoString(1, call.ID)
		part = append(part, providerProtoString(2, call.Name)...)
		part = append(part, providerProtoString(3, args)...)
		part = append(part, providerProtoVarint(4, 1)...)
		part = append(part, providerProtoVarint(5, uint64(index))...)
		if err := emit(providerProtoBytes(2, part)); err != nil {
			return err
		}
	}
	if reasoning.Len() > 0 && len(result.AnthropicContent) == 0 {
		part := providerProtoString(2, reasoning.String())
		part = append(part, providerProtoString(5, target.Model)...)
		message = append(message, providerProtoBytes(6, part)...)
	}
	usage := result.Usage
	if usage == nil {
		usage = &openAIUsage{}
	}
	total := usage.TotalTokens
	if total == 0 {
		total = usage.PromptTokens + usage.CompletionTokens
	}
	usageBody := providerProtoVarint(1, uint64(max(0, usage.PromptTokens)))
	usageBody = append(usageBody, providerProtoVarint(2, uint64(max(0, usage.CompletionTokens)))...)
	usageBody = append(usageBody, providerProtoVarint(3, uint64(max(0, total)))...)
	if err := emit(providerProtoBytes(3, usageBody)); err != nil {
		return err
	}
	info := providerProtoString(1, responseID)
	info = append(info, providerProtoString(2, target.Model)...)
	// The shipped client calls new Date(Number(createdAt)), so this is ms.
	info = append(info, providerProtoVarint(3, uint64(time.Now().UnixMilli()))...)
	info = append(info, providerProtoBytes(4, message)...)
	if err := emit(providerProtoBytes(4, info)); err != nil {
		return err
	}
	// is_final is a finish event, not a final text delta; the client ignores
	// text on this frame. Emit it only after provider success and metadata.
	return emit(providerProtoBytes(1, providerProtoVarint(2, 1)))
}

func inferenceProviderStreamError(result *streamResult) error {
	completed := false
	for _, line := range strings.Split(string(result.SSERaw), "\n") {
		payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if payload == "[DONE]" {
			completed = true
			continue
		}
		var event struct {
			Type    string          `json:"type"`
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(payload), &event) != nil {
			continue
		}
		completed = completed || event.Type == "message_stop"
		completed = completed || event.Delta.StopReason != ""
		for _, choice := range event.Choices {
			completed = completed || choice.FinishReason != ""
		}
		if len(event.Error) == 0 || string(event.Error) == "null" {
			continue
		}
		var detail struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Type    string `json:"type"`
		}
		_ = json.Unmarshal(event.Error, &detail)
		message := detail.Message
		if message == "" {
			_ = json.Unmarshal(event.Error, &message)
		}
		if message == "" {
			message = "provider reported a streaming error"
		}
		if len(message) > 512 {
			message = message[:512]
		}
		code := detail.Code
		if code == "" {
			code = detail.Type
		}
		if code != "" {
			return fmt.Errorf("upstream inference error (%s): %s", code, message)
		}
		return fmt.Errorf("upstream inference error: %s", message)
	}
	// The Anthropic adapter synthesizes FinishReason on EOF for the legacy
	// loop. Verify actual wire completion when raw events are available.
	if !completed && (len(result.SSERaw) != 0 || result.FinishReason == "") {
		return errors.New("provider stream ended before a completion marker")
	}
	return nil
}

func decodeInferenceProviderRequest(raw []byte, opts AdapterOpts) ([]openAIMessage, []openAITool, AdapterOpts, string, error) {
	fields, err := providerProtoFields(raw)
	if err != nil {
		return nil, nil, opts, "", err
	}
	var messages []openAIMessage
	var tools []openAITool
	var pendingToolExtras []openAIMessage
	for _, data := range fields.bytes(1) {
		decoded, err := decodeInferenceMessages(data)
		if err != nil {
			return nil, nil, opts, "", err
		}
		if len(decoded) > 0 && decoded[0].Role == "tool" {
			for _, message := range decoded {
				if message.Role == "tool" {
					messages = append(messages, message)
				} else {
					pendingToolExtras = append(pendingToolExtras, message)
				}
			}
		} else {
			messages = append(messages, pendingToolExtras...)
			pendingToolExtras = nil
			messages = append(messages, decoded...)
		}
	}
	messages = append(messages, pendingToolExtras...)
	for _, data := range fields.bytes(2) {
		tool, err := providerProtoFields(data)
		if err != nil {
			return nil, nil, opts, "", err
		}
		name := string(tool.lastBytes(1))
		if name == "" {
			return nil, nil, opts, "", errors.New("inference tool has no name")
		}
		var schema structpb.Struct
		if err := proto.Unmarshal(tool.lastBytes(3), &schema); err != nil {
			return nil, nil, opts, "", fmt.Errorf("tool schema: %w", err)
		}
		parameters, err := protojson.Marshal(&schema)
		if err != nil {
			return nil, nil, opts, "", err
		}
		if len(tool.lastBytes(3)) == 0 {
			parameters = []byte(`{"type":"object","properties":{}}`)
		}
		function, err := json.Marshal(map[string]any{"name": name, "description": string(tool.lastBytes(2)), "parameters": json.RawMessage(parameters)})
		if err != nil {
			return nil, nil, opts, "", err
		}
		tools = append(tools, openAITool{Type: "function", Function: function})
	}
	if len(fields.bytes(3)) > 0 {
		return nil, nil, opts, "", errors.New("provider-defined inference tools are not supported by this BYOK adapter")
	}
	if len(messages) == 0 {
		return nil, nil, opts, "", errors.New("inference request has no messages")
	}
	if config := fields.lastBytes(4); len(config) > 0 {
		cfg, err := providerProtoFields(config)
		if err != nil {
			return nil, nil, opts, "", err
		}
		if n := cfg.varint(1); n > 0 && n <= 1<<31-1 && (opts.MaxOutputTokens <= 0 || n < uint64(opts.MaxOutputTokens)) {
			opts.MaxOutputTokens = int(n)
		}
	}
	if model := fields.lastBytes(7); len(model) > 0 {
		requested, err := providerProtoFields(model)
		if err != nil {
			return nil, nil, opts, "", err
		}
		for _, p := range requested.bytes(3) {
			parameter, err := providerProtoFields(p)
			if err != nil {
				return nil, nil, opts, "", err
			}
			if id := string(parameter.lastBytes(1)); id == "reasoning" || id == "reasoning_effort" {
				opts.ReasoningEffort = string(parameter.lastBytes(2))
			}
		}
	}
	return messages, tools, opts, string(fields.lastBytes(6)), nil
}

func decodeInferenceMessages(raw []byte) ([]openAIMessage, error) {
	fields, err := providerProtoFields(raw)
	if err != nil {
		return nil, err
	}
	role := map[uint64]string{1: "user", 2: "assistant", 3: "tool", 4: "system"}[fields.varint(1)]
	if role == "" {
		return nil, errors.New("unknown inference message role")
	}
	if role == "tool" {
		content, err := providerProtoFields(fields.lastBytes(6))
		if err != nil {
			return nil, err
		}
		var messages []openAIMessage
		for _, rawPart := range content.bytes(1) {
			part, err := providerProtoFields(rawPart)
			if err != nil {
				return nil, err
			}
			var value structpb.Value
			if err := proto.Unmarshal(part.lastBytes(3), &value); err != nil {
				return nil, fmt.Errorf("tool result: %w", err)
			}
			text := value.GetStringValue()
			if _, ok := value.Kind.(*structpb.Value_StringValue); !ok {
				encoded, err := protojson.Marshal(&value)
				if err != nil {
					return nil, err
				}
				text = string(encoded)
			}
			id, name := string(part.lastBytes(1)), string(part.lastBytes(2))
			if id == "" {
				return nil, errors.New("inference tool result has no tool_call_id")
			}
			messages = append(messages, toolResultMessage(id, name, text))
			if extra := part.bytes(5); len(extra) > 0 {
				parts, err := decodeInferenceContentParts(extra)
				if err != nil {
					return nil, err
				}
				if len(parts) > 0 {
					messages = append(messages, multipartMessage("user", parts))
				}
			}
		}
		if len(messages) == 0 {
			return nil, errors.New("inference tool message has no results")
		}
		return messages, nil
	}
	message := textMessage(role, string(fields.lastBytes(2)))
	if content := fields.lastBytes(3); len(content) > 0 {
		parts, err := providerProtoFields(content)
		if err != nil {
			return nil, err
		}
		decoded, err := decodeInferenceContentParts(parts.bytes(1))
		if err != nil {
			return nil, err
		}
		message = multipartMessage(role, decoded)
	} else if len(fields.bytes(2)) == 0 && len(fields.bytes(17)) > 0 {
		var text strings.Builder
		for _, part := range fields.bytes(17) {
			text.Write(part)
		}
		message = textMessage(role, text.String())
	}
	for _, data := range fields.bytes(4) {
		call, err := providerProtoFields(data)
		if err != nil {
			return nil, err
		}
		args := string(call.lastBytes(4))
		if args == "" {
			var object structpb.Struct
			if err := proto.Unmarshal(call.lastBytes(3), &object); err != nil {
				return nil, fmt.Errorf("tool call arguments: %w", err)
			}
			encoded, err := protojson.Marshal(&object)
			if err != nil {
				return nil, err
			}
			args = string(encoded)
		}
		message.ToolCalls = append(message.ToolCalls, openAIToolCallMsg{ID: string(call.lastBytes(1)), Type: "function", Function: openAIToolCallFn{Name: string(call.lastBytes(2)), Arguments: args}})
	}
	if role == "assistant" {
		message.AnthropicContent, err = decodeInferenceAnthropicContent(fields, message)
		if err != nil {
			return nil, err
		}
	}
	return []openAIMessage{message}, nil
}

func decodeInferenceContentParts(rawParts [][]byte) ([]openAIContentPart, error) {
	var parts []openAIContentPart
	for _, raw := range rawParts {
		part, err := providerProtoFields(raw)
		if err != nil {
			return nil, err
		}
		if text := part.lastBytes(1); text != nil {
			value, err := providerProtoFields(text)
			if err != nil {
				return nil, err
			}
			parts = append(parts, openAIContentPart{Type: "text", Text: string(value.lastBytes(1))})
		} else if image := part.lastBytes(2); image != nil {
			value, err := providerProtoFields(image)
			if err != nil {
				return nil, err
			}
			data, mime := string(value.lastBytes(1)), string(value.lastBytes(2))
			if !strings.HasPrefix(data, "http://") && !strings.HasPrefix(data, "https://") && !strings.HasPrefix(data, "data:") {
				if mime == "" {
					mime = "image/png"
				}
				data = "data:" + mime + ";base64," + data
			}
			parts = append(parts, openAIContentPart{Type: "image_url", ImageURL: &openAIImageURL{URL: data}})
		} else if len(part.bytes(3)) > 0 {
			return nil, errors.New("file content parts are not supported by this BYOK adapter")
		} else {
			return nil, errors.New("unknown inference content part")
		}
	}
	return parts, nil
}

type providerWireField struct {
	number protowire.Number
	kind   protowire.Type
	data   []byte
	value  uint64
}
type providerWireFields []providerWireField

func providerProtoFields(raw []byte) (providerWireFields, error) {
	var out providerWireFields
	for len(raw) > 0 {
		number, kind, n := protowire.ConsumeTag(raw)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		raw = raw[n:]
		length := protowire.ConsumeFieldValue(number, kind, raw)
		if length < 0 {
			return nil, protowire.ParseError(length)
		}
		field := providerWireField{number: number, kind: kind}
		switch kind {
		case protowire.BytesType:
			field.data, _ = protowire.ConsumeBytes(raw)
		case protowire.VarintType:
			field.value, _ = protowire.ConsumeVarint(raw)
		}
		out = append(out, field)
		raw = raw[length:]
	}
	return out, nil
}
func (f providerWireFields) bytes(number protowire.Number) [][]byte {
	var out [][]byte
	for _, field := range f {
		if field.number == number && field.kind == protowire.BytesType {
			out = append(out, field.data)
		}
	}
	return out
}
func (f providerWireFields) lastBytes(number protowire.Number) []byte {
	for i := len(f) - 1; i >= 0; i-- {
		if f[i].number == number && f[i].kind == protowire.BytesType {
			return f[i].data
		}
	}
	return nil
}
func (f providerWireFields) varint(number protowire.Number) uint64 {
	for i := len(f) - 1; i >= 0; i-- {
		if f[i].number == number && f[i].kind == protowire.VarintType {
			return f[i].value
		}
	}
	return 0
}
func providerProtoBytes(number protowire.Number, value []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, number, protowire.BytesType), value)
}
func providerProtoString(number protowire.Number, value string) []byte {
	return providerProtoBytes(number, []byte(value))
}
func providerProtoVarint(number protowire.Number, value uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, number, protowire.VarintType), value)
}
