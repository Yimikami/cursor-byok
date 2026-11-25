package agent

// Recent Cursor harnesses execute their agent loop locally. This transport
// supplies inference only: Cursor retains its own tools, modes, MCP, skills,
// approvals and conversation state instead of delegating that loop to us.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	inferencev1 "cursor-byok/internal/protocodec/gen/inference/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

type inferenceResponseSink interface {
	response(string, *inferencev1.InferenceStreamResponse) error
}

type inferenceRunWriter struct {
	mu     sync.Mutex
	ctx    context.Context
	writer io.Writer
	seq    uint64
}

func (w *inferenceRunWriter) emit(msg *inferencev1.RunInferenceServerMessage) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ctx.Err(); err != nil {
		return err
	}
	w.seq++
	msg.Seq = w.seq
	body, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	if err := writeFrame(w.writer, body, false); err != nil {
		return err
	}
	flushIfPossible(w.writer)
	return nil
}
func (w *inferenceRunWriter) response(id string, response *inferencev1.InferenceStreamResponse) error {
	return w.emit(&inferencev1.RunInferenceServerMessage{Message: &inferencev1.RunInferenceServerMessage_InvocationResponse{InvocationResponse: &inferencev1.RunInferenceInvocationResponse{InvocationId: id, Response: response}}})
}
func (w *inferenceRunWriter) invocationEnd(id string, err error) error {
	end := &inferencev1.RunInferenceInvocationEnd{InvocationId: id}
	if err != nil {
		code := int32(13)
		if errors.Is(err, context.Canceled) {
			code = 1
		}
		end.Error = &inferencev1.RunInferenceInvocationError{Code: code, Message: err.Error()}
	}
	return w.emit(&inferencev1.RunInferenceServerMessage{Message: &inferencev1.RunInferenceServerMessage_InvocationEnd{InvocationEnd: end}})
}

// Both native transports share the ordered inbox and session engine.
func HandleInferenceRunSSE(ctx context.Context, body []byte, contentType string, w io.Writer, resolve AdapterResolver) {
	HandleInferenceSSE(ctx, body, contentType, w, resolve)
}
func HandleInferenceBidiAppend(body []byte, contentType string) Result {
	return HandleBidiAppend(body, contentType)
}

func resolvedInferenceModel(target AdapterTarget, requested *inferencev1.InferenceRequestedModel) *inferencev1.InferenceRequestedModel {
	id := target.StableID
	if id == "" {
		id = target.Model
	}
	return &inferencev1.InferenceRequestedModel{ModelId: id, MaxMode: requested.GetMaxMode(), Parameters: requested.GetParameters()}
}

func runLocalInferenceInvocation(ctx context.Context, invoke *inferencev1.RunInferenceInvokeModel, target AdapterTarget, runModel *inferencev1.InferenceRequestedModel, resolve AdapterResolver, w inferenceResponseSink) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	request := invoke.GetRequest()
	if request == nil || invoke.GetDeltaBase() != nil || invoke.GetCompressedRequest() != nil {
		return errors.New("full uncompressed inference request required; delta/compression was not negotiated")
	}
	requested := request.GetRequestedModel()
	modelID := request.GetModelId()
	if requested.GetModelId() != "" {
		modelID = requested.GetModelId()
	}
	if modelID == "" {
		modelID = runModel.GetModelId()
	}
	if requested == nil {
		requested = runModel
	}
	if modelID != "" {
		var ok bool
		var adapters []AdapterTarget
		if resolve != nil {
			adapters = resolve()
		}
		target, ok = ResolveAdapterForModel(adapters, modelID)
		if !ok {
			return errors.New("no BYOK adapter configured")
		}
	}
	for _, parameter := range requested.GetParameters() {
		if parameter.GetId() == "reasoning" || parameter.GetId() == "reasoning_effort" {
			target.Opts.ReasoningEffort = parameter.GetValue()
		}
	}
	opts, err := localInferenceOptions(request, target.Opts)
	if err != nil {
		return err
	}
	target.Opts = opts
	messages, tools, custom, err := translateLocalInferenceRequest(request)
	if err != nil {
		return err
	}
	invocationID := invoke.GetInvocationId()
	if request.GetInvocationId() != "" {
		if err := w.response(invocationID, &inferencev1.InferenceStreamResponse{Response: &inferencev1.InferenceStreamResponse_InvocationId{InvocationId: &inferencev1.InferenceInvocationIdInfo{InvocationId: request.GetInvocationId()}}}); err != nil {
			return err
		}
	}
	var text, thinking strings.Builder
	result, err := pickProviderStreamer(target.ProviderType)(ctx, target.BaseURL, target.APIKey, target.Model, messages, tools, target.Opts, func(content, reasoning string, done bool) error {
		if content != "" {
			text.WriteString(content)
			if err := w.response(invocationID, &inferencev1.InferenceStreamResponse{Response: &inferencev1.InferenceStreamResponse_TextPart{TextPart: &inferencev1.InferenceTextStreamPart{Text: content}}}); err != nil {
				return err
			}
		}
		if reasoning != "" {
			thinking.WriteString(reasoning)
			if err := w.response(invocationID, &inferencev1.InferenceStreamResponse{Response: &inferencev1.InferenceStreamResponse_ThinkingPart{ThinkingPart: &inferencev1.InferenceThinkingStreamPart{Text: reasoning}}}); err != nil {
				return err
			}
		}
		return nil
	})
	if result != nil {
		if result.FinishReason == "length" || result.FinishReason == "max_tokens" {
			if err := w.response(invocationID, &inferencev1.InferenceStreamResponse{Response: &inferencev1.InferenceStreamResponse_Error{Error: &inferencev1.InferenceStreamError{Message: "provider reached the output token limit before completing the response", Code: "output_token_limit", IsOutputTokenLimitError: true}}}); err != nil {
				return err
			}
			return errors.New("provider reached the output token limit before completing the response")
		}
	}
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
	if err := validateProviderToolCalls(result.ToolCalls); err != nil {
		return err
	}
	for i, call := range result.ToolCalls {
		if strings.TrimSpace(call.Arguments) == "" {
			result.ToolCalls[i].Arguments = "{}"
		}
		if custom[call.Name] {
			var wrapped struct {
				Input *string `json:"input"`
			}
			if json.Unmarshal([]byte(call.Arguments), &wrapped) != nil || wrapped.Input == nil {
				return fmt.Errorf("custom tool %s requires an input string", call.Name)
			}
		}
	}
	if thinking.Len() > 0 {
		if err := w.response(invocationID, &inferencev1.InferenceStreamResponse{Response: &inferencev1.InferenceStreamResponse_ThinkingPart{ThinkingPart: &inferencev1.InferenceThinkingStreamPart{IsFinal: true}}}); err != nil {
			return err
		}
	}
	responseMessage := &inferencev1.InferenceResponseMessage{Id: invocationID, Role: inferencev1.InferenceMessageRole_INFERENCE_MESSAGE_ROLE_ASSISTANT}
	for _, part := range result.ReasoningParts {
		item := &inferencev1.InferenceReasoningPart{Text: part.Text, IsRedacted: part.IsRedacted}
		if part.Signature != "" {
			item.Signature = proto.String(part.Signature)
		}
		if part.RedactedData != "" {
			item.RedactedData = proto.String(part.RedactedData)
		}
		if part.ModelName != "" {
			item.ModelName = proto.String(part.ModelName)
		}
		if part.LineageTag != "" {
			item.LineageTag = proto.String(part.LineageTag)
		}
		responseMessage.ReasoningParts = append(responseMessage.ReasoningParts, item)
	}
	if len(result.AnthropicNativeContent) > 0 {
		responseMessage.AnthropicNativeContent = proto.String(string(result.AnthropicNativeContent))
		if err := preserveInferenceAnthropicOrder(responseMessage, result.AnthropicNativeContent); err != nil {
			return err
		}
	}
	if text.Len() > 0 {
		value := text.String()
		responseMessage.Content = &value
	}
	for index, call := range result.ToolCalls {
		arguments := call.Arguments
		if custom[call.Name] {
			var wrapped struct {
				Input *string `json:"input"`
			}
			if json.Unmarshal([]byte(arguments), &wrapped) != nil || wrapped.Input == nil {
				return fmt.Errorf("custom tool %s requires an input string", call.Name)
			}
			arguments = *wrapped.Input
		}
		item := &inferencev1.InferenceToolCall{ToolCallId: call.ID, ToolName: call.Name}
		if custom[call.Name] {
			item.RawToolCallArgs = &arguments
		} else {
			value := map[string]any{}
			if json.Unmarshal([]byte(arguments), &value) != nil {
				return fmt.Errorf("provider returned invalid arguments for %s", call.Name)
			}
			item.Args, err = structpb.NewStruct(value)
			if err != nil {
				return err
			}
		}
		responseMessage.ToolCalls = append(responseMessage.ToolCalls, item)
		toolIndex := int32(index)
		streamArgs := arguments
		if custom[call.Name] {
			// Cursor JSON.parse's completed stream args, including custom
			// tools. Its final response instead carries raw_tool_call_args.
			encoded, _ := json.Marshal(arguments)
			streamArgs = string(encoded)
		}
		if err := w.response(invocationID, &inferencev1.InferenceStreamResponse{Response: &inferencev1.InferenceStreamResponse_ToolCallPart{ToolCallPart: &inferencev1.InferenceToolCallStreamPart{ToolCallId: call.ID, ToolName: call.Name, Args: streamArgs, IsComplete: true, ToolIndex: &toolIndex}}}); err != nil {
			return err
		}
	}
	if result.Usage != nil {
		total := int32(result.Usage.TotalTokens)
		if err := w.response(invocationID, &inferencev1.InferenceStreamResponse{Response: &inferencev1.InferenceStreamResponse_Usage{Usage: &inferencev1.InferenceUsageInfo{PromptTokens: int32(result.Usage.PromptTokens), CompletionTokens: int32(result.Usage.CompletionTokens), TotalTokens: &total}}}); err != nil {
			return err
		}
	}
	if err := w.response(invocationID, &inferencev1.InferenceStreamResponse{Response: &inferencev1.InferenceStreamResponse_ResponseInfo{ResponseInfo: &inferencev1.InferenceResponseInfo{Id: invocationID, Model: target.Model, CreatedAt: time.Now().UnixMilli(), Messages: []*inferencev1.InferenceResponseMessage{responseMessage}}}}); err != nil {
		return err
	}
	return w.response(invocationID, &inferencev1.InferenceStreamResponse{Response: &inferencev1.InferenceStreamResponse_TextPart{TextPart: &inferencev1.InferenceTextStreamPart{IsFinal: true}}})
}

func translateLocalInferenceRequest(request *inferencev1.InferenceStreamRequest) ([]openAIMessage, []openAITool, map[string]bool, error) {
	if len(request.GetProviderDefinedTools()) > 0 {
		return nil, nil, nil, errors.New("provider-defined native tools are not supported by this BYOK adapter; use Cursor/MCP tools with explicit schemas")
	}
	custom := map[string]bool{}
	var tools []openAITool
	for _, tool := range request.GetTools() {
		if tool.GetName() == "" {
			return nil, nil, nil, errors.New("native tool has no name")
		}
		parameters := map[string]any{"type": "object", "properties": map[string]any{}}
		if tool.GetParameters() != nil {
			parameters = tool.GetParameters().AsMap()
		}
		description := tool.GetDescription()
		if format := tool.GetCustomToolFormat(); format != nil {
			custom[tool.GetName()] = true
			parameters = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string", "description": "The exact raw input for this custom tool."}}, "required": []string{"input"}, "additionalProperties": false}
			description += "\nReturn the raw tool input in the input string. Format: " + format.GetType() + "; syntax: " + format.GetSyntax() + "\n" + format.GetDefinition()
		}
		body, err := json.Marshal(map[string]any{"name": tool.GetName(), "description": description, "parameters": parameters})
		if err != nil {
			return nil, nil, nil, err
		}
		tools = append(tools, openAITool{Type: "function", Function: body})
	}
	var messages []openAIMessage
	var toolVisuals []openAIMessage
	flushToolVisuals := func() {
		messages = append(messages, toolVisuals...)
		toolVisuals = nil
	}
	for _, message := range request.GetMessages() {
		role := ""
		switch message.GetRole() {
		case inferencev1.InferenceMessageRole_INFERENCE_MESSAGE_ROLE_USER:
			role = "user"
		case inferencev1.InferenceMessageRole_INFERENCE_MESSAGE_ROLE_ASSISTANT:
			role = "assistant"
		case inferencev1.InferenceMessageRole_INFERENCE_MESSAGE_ROLE_TOOL:
			role = "tool"
		case inferencev1.InferenceMessageRole_INFERENCE_MESSAGE_ROLE_SYSTEM:
			role = "system"
		default:
			return nil, nil, nil, errors.New("native inference message has an unknown role")
		}
		if role == "tool" {
			if len(message.GetToolContent().GetParts()) == 0 {
				return nil, nil, nil, errors.New("native tool message has no results")
			}
			for _, part := range message.GetToolContent().GetParts() {
				if part.GetToolCallId() == "" {
					return nil, nil, nil, errors.New("native tool result has no tool_call_id")
				}
				content := "null"
				if part.GetResult() != nil {
					value := part.GetResult().AsInterface()
					if text, ok := value.(string); ok {
						content = text
					} else {
						body, _ := json.Marshal(value)
						content = string(body)
					}
				}
				if part.GetIsError() || part.GetCursorToolCallIsError() {
					content = "Tool execution error: " + content
				}
				messages = append(messages, toolResultMessage(part.GetToolCallId(), part.GetToolName(), content))
				if len(part.GetExperimentalContent()) > 0 {
					parts, err := localInferenceParts(part.GetExperimentalContent())
					if err != nil {
						return nil, nil, nil, err
					}
					toolVisuals = append(toolVisuals, multipartMessage("user", parts))
				}
			}
			continue
		}
		// Parallel results may be split across several native CoreMessages.
		// OpenAI requires the whole tool-result batch before any user image.
		flushToolVisuals()
		out := textMessage(role, message.GetText())
		if message.GetText() == "" && len(message.GetAssistantTextParts()) > 0 {
			out = textMessage(role, strings.Join(message.GetAssistantTextParts(), ""))
		}
		if parts := message.GetParts(); parts != nil {
			converted, err := localInferenceParts(parts.GetParts())
			if err != nil {
				return nil, nil, nil, err
			}
			if role == "system" {
				var texts []string
				for _, part := range converted {
					if part.Type != "text" {
						return nil, nil, nil, errors.New("system image content is unsupported")
					}
					texts = append(texts, part.Text)
				}
				out = textMessage(role, strings.Join(texts, "\n"))
			} else {
				out = multipartMessage(role, converted)
			}
		}
		for _, call := range message.GetToolCalls() {
			args := "{}"
			if call.GetArgs() != nil {
				body, _ := call.GetArgs().MarshalJSON()
				args = string(body)
			}
			if custom[call.GetToolName()] {
				raw := call.GetRawToolCallArgs()
				if call.RawToolCallArgs == nil {
					raw = args
				}
				body, _ := json.Marshal(map[string]string{"input": raw})
				args = string(body)
			} else if call.RawToolCallArgs != nil {
				args = call.GetRawToolCallArgs()
			}
			out.ToolCalls = append(out.ToolCalls, openAIToolCallMsg{ID: call.GetToolCallId(), Type: "function", Function: openAIToolCallFn{Name: call.GetToolName(), Arguments: args}})
		}
		if role == "assistant" {
			nativeContent, err := decodeInferenceAnthropicContent(message, out)
			if err != nil {
				return nil, nil, nil, err
			}
			state := &streamResult{AnthropicNativeContent: nativeContent}
			for _, part := range message.GetReasoningParts() {
				state.ReasoningParts = append(state.ReasoningParts, providerReasoningPart{
					Text: part.GetText(), Signature: part.GetSignature(), IsRedacted: part.GetIsRedacted(),
					RedactedData: part.GetRedactedData(), ModelName: part.GetModelName(), LineageTag: part.GetLineageTag(),
				})
			}
			preserveProviderReasoning(&out, state)
		}
		messages = append(messages, out)
	}
	flushToolVisuals()
	if len(messages) == 0 {
		return nil, nil, nil, errors.New("native inference request has no messages")
	}
	return messages, tools, custom, nil
}

func localInferenceParts(parts []*inferencev1.InferenceContentPart) ([]openAIContentPart, error) {
	var out []openAIContentPart
	for _, part := range parts {
		if text := part.GetText(); text != nil {
			out = append(out, openAIContentPart{Type: "text", Text: text.GetText()})
			continue
		}
		if image := part.GetImage(); image != nil {
			data := image.GetData()
			if !strings.HasPrefix(data, "data:") && !strings.HasPrefix(data, "https://") && !strings.HasPrefix(data, "http://") {
				mime := image.GetMimeType()
				if mime == "" {
					mime = "image/png"
				}
				data = "data:" + mime + ";base64," + data
			}
			out = append(out, openAIContentPart{Type: "image_url", ImageURL: &openAIImageURL{URL: data}})
			continue
		}
		if part.GetFile() != nil {
			return nil, errors.New("native file attachment inference is not supported by this provider adapter; supply extracted text or images")
		}
		return nil, errors.New("unsupported native inference content part")
	}
	return out, nil
}

func localInferenceOptions(request *inferencev1.InferenceStreamRequest, opts AdapterOpts) (AdapterOpts, error) {
	for _, p := range request.GetRequestedModel().GetParameters() {
		if p.GetId() == "reasoning" || p.GetId() == "reasoning_effort" {
			opts.ReasoningEffort = p.GetValue()
		}
	}
	if config := request.GetModelConfig(); config != nil {
		if config.GetMaxTokens() > 0 && (opts.MaxOutputTokens <= 0 || int(config.GetMaxTokens()) < opts.MaxOutputTokens) {
			opts.MaxOutputTokens = int(config.GetMaxTokens())
		}
		opts.Temperature = config.Temperature
		opts.TopP = config.TopP
		opts.StopSequences = config.GetStopSequences()
		if config.GetTopK() != 0 || config.GetContinueFinalMessage() || len(config.GetChatTemplateKwargs().GetFields()) > 0 {
			return opts, errors.New("this provider adapter does not support native top_k, continuation, or chat-template overrides")
		}
	}
	return opts, nil
}
