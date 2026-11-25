package mitm

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cursor-byok/internal/protocodec"
	aiserverv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
	inferencev1 "cursor-byok/internal/protocodec/gen/inference/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// These fixtures follow the installed 3.31.15 managed harness. Unlike the old
// AgentService loop, Cursor owns native tools and supplies their full schemas
// to InferenceService. Tests use a temporary CA and local provider only.
func inferenceAppend(t *testing.T, ctx context.Context, client *http.Client, id string, seq int64, binary bool, payload []byte) {
	t.Helper()
	appendRequest := &aiserverv1.BidiAppendRequest{RequestId: &aiserverv1.BidiRequestId{RequestId: id}, AppendSeqno: seq}
	if binary {
		appendRequest.DataBinary = payload
	} else {
		appendRequest.Data = hex.EncodeToString(payload)
	}
	body, err := proto.Marshal(appendRequest)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api2.cursor.sh/aiserver.v1.BidiService/BidiAppend", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/proto")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	result, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("BidiAppend status=%d body=%s", resp.StatusCode, result)
	}
}

func inferenceAppendMessage(t *testing.T, ctx context.Context, client *http.Client, id string, seq int64, binary bool, message *inferencev1.RunInferenceClientMessage) {
	t.Helper()
	body, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	inferenceAppend(t, ctx, client, id, seq, binary, body)
}

func inferenceRead(t *testing.T, body io.Reader) (*inferencev1.RunInferenceServerMessage, bool) {
	t.Helper()
	frame, err := protocodec.ReadFrame(body)
	if err != nil {
		t.Fatal(err)
	}
	payload := frame.Data
	if frame.Flags&1 != 0 {
		reader, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		payload, err = io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if frame.IsEnd() {
		if string(payload) != "{}" {
			t.Fatalf("inference stream error: %s", payload)
		}
		return nil, true
	}
	var message inferencev1.RunInferenceServerMessage
	if err := proto.Unmarshal(payload, &message); err != nil {
		t.Fatal(err)
	}
	return &message, false
}

func inferenceStart(t *testing.T, ctx context.Context, client *http.Client, id string, binary bool) *http.Response {
	t.Helper()
	// Independent wire fixture: client.run_request{conversation_id, requested_model{model_id}}.
	run := append(wsBytes(1, []byte(id)), wsBytes(3, wsBytes(1, []byte("test-model")))...)
	inferenceAppend(t, ctx, client, id, 0, binary, wsBytes(1, run))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api2.cursor.sh/aiserver.v1.InferenceService/RunInferenceSSE", bytes.NewReader(wsBytes(1, []byte(id))))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/proto")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("RunInferenceSSE status=%d", resp.StatusCode)
	}
	for {
		message, end := inferenceRead(t, resp.Body)
		if end {
			t.Fatal("stream ended before run_ready")
		}
		if ready := message.GetRunReady(); ready != nil {
			if ready.GetResolvedModel().GetModelId() != "test-model" {
				t.Fatalf("missing resolved model: %v", ready)
			}
			if ready.AcceptsDeltaInvocations || ready.AcceptsDeltaBaseSeeds {
				t.Fatal("server advertised unsupported delta protocol")
			}
			metadata := ready.GetPromptModelMetadata()
			if metadata.GetVendor() != "unknown" || metadata.GetPromptVersion() != "latest" || len(metadata.GetAgentModeConfigs()) != 6 {
				t.Fatal("native prompt construction metadata missing from RunReady")
			}
			return resp
		}
	}
}

func inferenceInvoke(id string, messages ...*inferencev1.InferenceCoreMessage) *inferencev1.RunInferenceClientMessage {
	return &inferencev1.RunInferenceClientMessage{Message: &inferencev1.RunInferenceClientMessage_InvokeModel{InvokeModel: &inferencev1.RunInferenceInvokeModel{InvocationId: id, Request: &inferencev1.InferenceStreamRequest{Messages: messages}}}}
}

func inferenceUser(text string) *inferencev1.InferenceCoreMessage {
	return &inferencev1.InferenceCoreMessage{Role: inferencev1.InferenceMessageRole_INFERENCE_MESSAGE_ROLE_USER, Content: &inferencev1.InferenceCoreMessage_Text{Text: text}}
}

func inferenceInvocation(t *testing.T, body io.Reader, id string) ([]*inferencev1.InferenceStreamResponse, *inferencev1.InferenceResponseInfo) {
	t.Helper()
	var responses []*inferencev1.InferenceStreamResponse
	var final *inferencev1.InferenceResponseInfo
	for {
		message, end := inferenceRead(t, body)
		if end {
			t.Fatal("stream ended during model invocation")
		}
		if response := message.GetInvocationResponse(); response != nil {
			if response.InvocationId != id {
				t.Fatalf("response routed to wrong invocation: %q", response.InvocationId)
			}
			responses = append(responses, response.Response)
			if info := response.Response.GetResponseInfo(); info != nil {
				final = info
			}
		}
		if end := message.GetInvocationEnd(); end != nil {
			if end.InvocationId != id || end.Error != nil || final == nil {
				t.Fatalf("invalid invocation end=%v final=%v", end, final)
			}
			return responses, final
		}
	}
}

func TestInferenceNativeLoopThroughTLSProxy(t *testing.T) {
	for _, binary := range []bool{false, true} {
		t.Run(fmt.Sprintf("binary=%v", binary), func(t *testing.T) {
			requests := make(chan map[string]any, 2)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				requests <- request
				w.Header().Set("Content-Type", "text/event-stream")
				messages := request["messages"].([]any)
				if messages[len(messages)-1].(map[string]any)["role"] == "tool" {
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"native result received\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				} else {
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"native-call\",\"type\":\"function\",\"function\":{\"name\":\"mcp__contract__read\",\"arguments\":\"{\\\"key\\\":\\\"native\\\"}\"}}]},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
				}
			}))
			defer provider.Close()
			_, client := testProxy(t, provider.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			id := t.Name()
			stream := inferenceStart(t, ctx, client, id, binary)
			schema, _ := structpb.NewStruct(map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}}, "required": []any{"key"}})
			system := &inferencev1.InferenceCoreMessage{Role: inferencev1.InferenceMessageRole_INFERENCE_MESSAGE_ROLE_SYSTEM, Content: &inferencev1.InferenceCoreMessage_Text{Text: "Native skill and mode instructions"}}
			user := inferenceUser("Use the native contract tool")
			invoke := inferenceInvoke("first", system, user)
			invoke.GetInvokeModel().Request.Tools = []*inferencev1.InferenceAgentTool{{Name: "mcp__contract__read", Description: "Client supplied native MCP tool", Parameters: schema}}
			inferenceAppendMessage(t, ctx, client, id, 1, binary, invoke)
			responses, final := inferenceInvocation(t, stream.Body, "first")
			if len(final.Messages) != 1 || len(final.Messages[0].ToolCalls) != 1 || final.Messages[0].ToolCalls[0].ToolName != "mcp__contract__read" {
				t.Fatalf("native tool missing from final response: %v", final)
			}
			complete := false
			for _, response := range responses {
				if call := response.GetToolCallPart(); call != nil && call.IsComplete {
					complete = call.ToolCallId == "native-call" && json.Valid([]byte(call.Args))
				}
			}
			if !complete {
				t.Fatal("native client received no complete tool call")
			}
			request := <-requests
			encoded, _ := json.Marshal(request)
			for _, expected := range []string{"Native skill and mode instructions", "mcp__contract__read", "Client supplied native MCP tool", "required", "key"} {
				if !bytes.Contains(encoded, []byte(expected)) {
					t.Fatalf("client prompt/schema dropped: %q", expected)
				}
			}
			result, _ := structpb.NewValue(map[string]any{"value": "native fixture result"})
			assistant := &inferencev1.InferenceCoreMessage{Role: inferencev1.InferenceMessageRole_INFERENCE_MESSAGE_ROLE_ASSISTANT, ToolCalls: final.Messages[0].ToolCalls}
			toolResult := &inferencev1.InferenceCoreMessage{Role: inferencev1.InferenceMessageRole_INFERENCE_MESSAGE_ROLE_TOOL, Content: &inferencev1.InferenceCoreMessage_ToolContent{ToolContent: &inferencev1.InferenceToolResultContent{Parts: []*inferencev1.InferenceToolResultPart{{ToolCallId: "native-call", ToolName: "mcp__contract__read", Result: result}}}}}
			inferenceAppendMessage(t, ctx, client, id, 2, binary, inferenceInvoke("second", system, user, assistant, toolResult))
			_, final = inferenceInvocation(t, stream.Body, "second")
			if len(final.Messages) != 1 || final.Messages[0].GetContent() != "native result received" {
				t.Fatalf("final assistant message lost: %v", final)
			}
			request = <-requests
			encoded, _ = json.Marshal(request)
			if !bytes.Contains(encoded, []byte("native fixture result")) || !bytes.Contains(encoded, []byte("native-call")) {
				t.Fatal("native tool result/history lost in follow-up invocation")
			}
			inferenceAppendMessage(t, ctx, client, id, 3, binary, &inferencev1.RunInferenceClientMessage{Message: &inferencev1.RunInferenceClientMessage_FinishRun{FinishRun: &inferencev1.RunInferenceFinishRun{TurnCompleted: proto.Bool(true)}}})
			for {
				if _, end := inferenceRead(t, stream.Body); end {
					break
				}
			}
		})
	}
}

func TestInferenceProviderCancellationThroughTLSProxy(t *testing.T) {
	for _, action := range []string{"cancel-invocation", "close-stream", "stop-proxy"} {
		t.Run(action, func(t *testing.T) {
			requested, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				close(requested)
				select {
				case <-r.Context().Done():
					close(canceled)
				case <-release:
				}
			}))
			defer provider.Close()
			defer close(release)
			proxy, client := testProxy(t, provider.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			id := strings.ReplaceAll(t.Name(), "/", "-")
			stream := inferenceStart(t, ctx, client, id, true)
			inferenceAppendMessage(t, ctx, client, id, 1, true, inferenceInvoke("pending", inferenceUser("wait")))
			select {
			case <-requested:
			case <-ctx.Done():
				t.Fatal("provider was not invoked")
			}
			switch action {
			case "cancel-invocation":
				inferenceAppendMessage(t, ctx, client, id, 2, true, &inferencev1.RunInferenceClientMessage{Message: &inferencev1.RunInferenceClientMessage_CancelInvocation{CancelInvocation: &inferencev1.RunInferenceCancelInvocation{InvocationId: "pending"}}})
			case "close-stream":
				stream.Body.Close()
			case "stop-proxy":
				if err := proxy.Stop(ctx); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-canceled:
			case <-ctx.Done():
				t.Fatal("provider request leaked after " + action)
			}
		})
	}
}
