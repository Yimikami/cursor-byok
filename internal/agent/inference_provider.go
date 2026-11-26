package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	inferencev1 "cursor-byok/internal/protocodec/gen/inference/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

type providerInferenceSink struct{ send func([]byte) error }

func (sink providerInferenceSink) response(_ string, response *inferencev1.InferenceStreamResponse) error {
	body, err := proto.Marshal(response)
	if err != nil {
		return err
	}
	return sink.send(body)
}
func runInferenceProvider(ctx context.Context, raw []byte, target AdapterTarget, send func([]byte) error) error {
	request := &inferencev1.InferenceStreamRequest{}
	if err := proto.Unmarshal(raw, request); err != nil {
		return err
	}
	id := request.GetInvocationId()
	if id == "" {
		id = "byok-" + uuid.NewString()
	}
	return runLocalInferenceInvocation(ctx, &inferencev1.RunInferenceInvokeModel{InvocationId: id, Request: request}, target, nil, func() []AdapterTarget { return []AdapterTarget{target} }, providerInferenceSink{send: send})
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
				Index        int    `json:"index"`
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
			completed = completed || choice.Index == 0 && choice.FinishReason != ""
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
	request := &inferencev1.InferenceStreamRequest{}
	if err := proto.Unmarshal(raw, request); err != nil {
		return nil, nil, opts, "", err
	}
	messages, tools, _, err := translateLocalInferenceRequest(request)
	if err != nil {
		return nil, nil, opts, "", err
	}
	opts, err = localInferenceOptions(request, opts)
	return messages, tools, opts, request.GetInvocationId(), err
}
func decodeInferenceMessages(raw []byte) ([]openAIMessage, error) {
	message := &inferencev1.InferenceCoreMessage{}
	if err := proto.Unmarshal(raw, message); err != nil {
		return nil, err
	}
	messages, _, _, err := translateLocalInferenceRequest(&inferencev1.InferenceStreamRequest{Messages: []*inferencev1.InferenceCoreMessage{message}})
	return messages, err
}
func decodeInferenceContentParts(rawParts [][]byte) ([]openAIContentPart, error) {
	var parts []*inferencev1.InferenceContentPart
	for _, raw := range rawParts {
		part := &inferencev1.InferenceContentPart{}
		if err := proto.Unmarshal(raw, part); err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return localInferenceParts(parts)
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
