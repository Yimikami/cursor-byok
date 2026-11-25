package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	inferencev1 "cursor-byok/internal/protocodec/gen/inference/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func inferenceFrames(t *testing.T, wire []byte) []*inferencev1.RunInferenceServerMessage {
	t.Helper()
	var messages []*inferencev1.RunInferenceServerMessage
	for len(wire) > 0 {
		if len(wire) < 5 {
			t.Fatal("truncated frame header")
		}
		size := int(binary.BigEndian.Uint32(wire[1:5]))
		if size > len(wire)-5 {
			t.Fatal("truncated frame")
		}
		flags, body := wire[0], wire[5:5+size]
		wire = wire[5+size:]
		if flags&flagCompressed != 0 {
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
		if flags&flagEndStream != 0 {
			continue
		}
		msg := &inferencev1.RunInferenceServerMessage{}
		if err := proto.Unmarshal(body, msg); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, msg)
	}
	return messages
}

func TestLocalInferenceCustomToolRoundTripAndModelControls(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: native.txt\n+hello\n*** End Patch"
	var providerBody openAIRequest
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected provider path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&providerBody); err != nil {
			t.Error(err)
		}
		arguments, _ := json.Marshal(map[string]string{"input": patch})
		chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "patch-call", "type": "function", "function": map[string]any{"name": "apply_patch", "arguments": string(arguments)}}}}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 20, "completion_tokens": 10, "total_tokens": 30}})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + string(chunk) + "\n\ndata: [DONE]\n\n"))
	}))
	defer provider.Close()
	request := &inferencev1.InferenceStreamRequest{}
	fixture := `{"messages":[{"role":"INFERENCE_MESSAGE_ROLE_SYSTEM","text":"Use native tools."},{"role":"INFERENCE_MESSAGE_ROLE_USER","text":"Create the file"},{"role":"INFERENCE_MESSAGE_ROLE_ASSISTANT","toolCalls":[{"toolCallId":"old-call","toolName":"apply_patch","rawToolCallArgs":"old patch"}]},{"role":"INFERENCE_MESSAGE_ROLE_TOOL","toolContent":{"parts":[{"toolCallId":"old-call","toolName":"apply_patch","result":"old patch failed","isError":true}]}},{"role":"INFERENCE_MESSAGE_ROLE_USER","text":"Retry"}],"tools":[{"name":"apply_patch","description":"Apply a patch","customToolFormat":{"type":"grammar","syntax":"lark","definition":"start: patch"}}],"modelConfig":{"maxTokens":5000,"temperature":0.25,"topP":0.8,"stopSequences":["STOP"]},"requestedModel":{"modelId":"fixture-model","parameters":[{"id":"reasoning","value":"none"}]}}`
	if err := protojson.Unmarshal([]byte(fixture), request); err != nil {
		t.Fatal(err)
	}
	target := AdapterTarget{BaseURL: provider.URL, APIKey: "fixture-key", Model: "fixture-model", Opts: AdapterOpts{MaxOutputTokens: 2048}}
	var wire bytes.Buffer
	writer := &inferenceRunWriter{ctx: context.Background(), writer: &wire}
	invoke := &inferencev1.RunInferenceInvokeModel{InvocationId: "invoke-custom", Request: request}
	if err := runLocalInferenceInvocation(context.Background(), invoke, target, nil, func() []AdapterTarget { return []AdapterTarget{target} }, writer); err != nil {
		t.Fatal(err)
	}
	if providerBody.MaxTokens != 2048 || providerBody.Temperature == nil || *providerBody.Temperature != 0.25 || providerBody.TopP == nil || *providerBody.TopP != 0.8 || len(providerBody.Stop) != 1 || providerBody.ReasoningEffort != "none" {
		t.Fatalf("native/configured model controls lost: %+v", providerBody)
	}
	if len(providerBody.Tools) != 1 || !strings.Contains(string(providerBody.Tools[0].Function), `"input"`) {
		t.Fatal("custom tool input wrapper missing")
	}
	if !strings.Contains(providerBody.Messages[2].ToolCalls[0].Function.Arguments, `"input":"old patch"`) || !strings.Contains(string(providerBody.Messages[3].Content), "Tool execution error") {
		t.Fatal("custom tool history/result lost")
	}
	var gotStream, gotFinal bool
	for i, frame := range inferenceFrames(t, wire.Bytes()) {
		if frame.GetSeq() != uint64(i+1) {
			t.Fatal("nonmonotonic server sequence")
		}
		response := frame.GetInvocationResponse()
		if response.GetInvocationId() != "invoke-custom" {
			t.Fatal("invocation identity lost")
		}
		if part := response.GetResponse().GetToolCallPart(); part != nil {
			var raw string
			if err := json.Unmarshal([]byte(part.GetArgs()), &raw); err != nil || raw != patch || !part.GetIsComplete() {
				t.Fatalf("native custom stream args invalid: %s", part.GetArgs())
			}
			gotStream = true
		}
		if info := response.GetResponse().GetResponseInfo(); info != nil {
			call := info.GetMessages()[0].GetToolCalls()[0]
			if call.GetRawToolCallArgs() != patch || call.Args != nil {
				t.Fatal("final raw custom tool history lost")
			}
			gotFinal = true
		}
	}
	if !gotStream || !gotFinal {
		t.Fatal("missing native tool stream/final response")
	}
}

func TestLocalInferencePreservesNativeToolSchemasAndImages(t *testing.T) {
	request := &inferencev1.InferenceStreamRequest{}
	if err := protojson.Unmarshal([]byte(`{"messages":[{"role":"INFERENCE_MESSAGE_ROLE_USER","parts":{"parts":[{"text":{"text":"inspect this"}},{"image":{"data":"aW1hZ2U=","mimeType":"image/png"}}]}},{"role":"INFERENCE_MESSAGE_ROLE_ASSISTANT","assistantTextParts":["part one"," part two"],"toolCalls":[{"toolCallId":"mcp-call","toolName":"mcp_lookup","args":{"query":"fixture"}}]},{"role":"INFERENCE_MESSAGE_ROLE_TOOL","toolContent":{"parts":[{"toolCallId":"mcp-call","toolName":"mcp_lookup","result":{"found":true}}]}}],"tools":[{"name":"mcp_lookup","description":"Native MCP schema","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}]}`), request); err != nil {
		t.Fatal(err)
	}
	messages, tools, _, err := translateLocalInferenceRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || len(tools) != 1 || !strings.Contains(string(messages[0].Content), "data:image/png;base64,aW1hZ2U=") || !strings.Contains(string(messages[1].Content), "part one part two") || messages[2].ToolCallID != "mcp-call" {
		t.Fatalf("native content lost: %+v", messages)
	}
	if !strings.Contains(string(tools[0].Function), `"required":["query"]`) {
		t.Fatal("MCP schema changed")
	}
}

func TestLocalInferenceDoesNotSilentlyDropUnsupportedFeatures(t *testing.T) {
	for _, fixture := range []string{
		`{"providerDefinedTools":[{"name":"search","type":"web_search"}],"messages":[{"role":"INFERENCE_MESSAGE_ROLE_USER","text":"search"}]}`,
		`{"messages":[{"role":"INFERENCE_MESSAGE_ROLE_USER","parts":{"parts":[{"file":{"data":"abc","mediaType":"application/pdf"}}]}}]}`,
	} {
		request := &inferencev1.InferenceStreamRequest{}
		if err := protojson.Unmarshal([]byte(fixture), request); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := translateLocalInferenceRequest(request); err == nil {
			t.Fatal("unsupported native feature silently ignored")
		}
	}
}
