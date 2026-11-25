package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "cursor-byok/internal/protocodec/gen/agent/v1"
	aiserverv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
	"google.golang.org/protobuf/proto"
)

func execResult(seq uint32, execID string) *agentv1.AgentClientMessage {
	return &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ExecClientMessage{
		ExecClientMessage: &agentv1.ExecClientMessage{Id: seq, ExecId: execID},
	}}
}

func shellOutput(seq uint32, text string) *agentv1.AgentClientMessage {
	msg := execResult(seq, "")
	msg.GetExecClientMessage().Message = &agentv1.ExecClientMessage_ShellStream{ShellStream: &agentv1.ShellStream{
		Event: &agentv1.ShellStream_Stdout{Stdout: &agentv1.ShellStreamStdout{Data: text}},
	}}
	return msg
}

func shellExit(seq uint32) *agentv1.AgentClientMessage {
	msg := execResult(seq, "")
	msg.GetExecClientMessage().Message = &agentv1.ExecClientMessage_ShellStream{ShellStream: &agentv1.ShellStream{
		Event: &agentv1.ShellStream_Exit{Exit: &agentv1.ShellStreamExit{Code: 0, Cwd: "/test"}},
	}}
	return msg
}

func assertNoToolResult(t *testing.T, ch <-chan *toolResultEnvelope) {
	t.Helper()
	select {
	case got := <-ch:
		t.Fatalf("unexpected tool result: %+v", got)
	default:
	}
}

func assertScopeRetired(t *testing.T, scope toolWaitScope) {
	t.Helper()
	pendingMu.Lock()
	defer pendingMu.Unlock()
	for key := range pending {
		if key.scope == scope {
			t.Errorf("pending waiter retained: %+v", key)
		}
	}
	for key := range execIDAlias {
		if key.scope == scope {
			t.Errorf("exec alias retained: %+v", key)
		}
	}
	for key := range seqAlias {
		if key.scope == scope {
			t.Errorf("sequence alias retained: %+v", key)
		}
	}
	for key := range shellAccum {
		if key.scope == scope {
			t.Errorf("shell accumulator retained: %+v", key)
		}
	}
	for key := range pendingInteraction {
		if key.scope == scope {
			t.Errorf("interaction retained: %+v", key)
		}
	}
}

func TestToolResultsAreScopedToTransportAndRequest(t *testing.T) {
	// Real providers can reuse tool-call IDs; Cursor's sequence counter is
	// likewise not a proof that the reply belongs to a particular connection.
	scopes := []toolWaitScope{socketToolScope(t.Name()), socketToolScope(t.Name() + "-other"), bidiToolScope(t.Name()), bugBotToolScope(t.Name())}
	channels := make([]chan *toolResultEnvelope, len(scopes))
	for i, scope := range scopes {
		channels[i] = registerToolWait(scope, "shared-call")
		registerExecIDAlias(scope, "shared-exec", 7, "shared-call")
		t.Cleanup(func() { cancelToolWait(channels[i]) })
	}
	for i, scope := range scopes {
		routeExecClientResult(scope, execResult(7, "shared-exec"))
		if got := waitForToolResultContext(context.Background(), channels[i], time.Second); got.Error != "" || got.ExecClient.GetExecId() != "shared-exec" {
			t.Fatalf("scope %v result = %+v", scope, got)
		}
		for _, ch := range channels[i+1:] {
			assertNoToolResult(t, ch)
		}
		assertScopeRetired(t, scope)
	}
}

func TestBidiCannotConsumeSocketToolOrApproval(t *testing.T) {
	scope := socketToolScope(t.Name())
	tool := registerToolWait(scope, "victim-call")
	registerExecIDAlias(scope, "victim-exec", 3, "victim-call")
	approval := registerInteractionWait(scope, 3)
	defer cancelToolWait(tool)
	defer DropSession(t.Name())
	for _, msg := range []*agentv1.AgentClientMessage{
		execResult(3, "victim-exec"),
		{Message: &agentv1.AgentClientMessage_InteractionResponse{InteractionResponse: &agentv1.InteractionResponse{Id: 3}}},
	} {
		payload, err := proto.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		result := handleAgentBidiAppendDecoded(&aiserverv1.BidiAppendRequest{
			RequestId: &aiserverv1.BidiRequestId{RequestId: t.Name()}, Data: hex.EncodeToString(payload),
		})
		if result.Status != 200 {
			t.Fatalf("BidiAppend: %d", result.Status)
		}
	}
	assertNoToolResult(t, tool)
	select {
	case <-approval:
		t.Fatal("BidiAppend consumed socket approval")
	default:
	}
	routeExecClientResult(scope, execResult(3, "victim-exec"))
	deliverInteractionResponse(scope, &agentv1.InteractionResponse{Id: 3})
	if got := waitForToolResultContext(context.Background(), tool, time.Second); got.Error != "" {
		t.Fatal(got.Error)
	}
	if got := waitForInteractionResponse(context.Background(), approval, time.Second); got == nil {
		t.Fatal("owner lost approval waiter")
	}
	assertScopeRetired(t, scope)
}

func TestUnknownAndConflictingResultsCannotConsumeOnlyWaiter(t *testing.T) {
	scope := bidiToolScope(t.Name())
	first := registerToolWait(scope, "first")
	registerExecIDAlias(scope, "first-exec", 1, "first")
	defer cancelToolWait(first)
	// The old single-waiter fallback delivered this unrelated result.
	routeExecClientResult(scope, execResult(900, ""))
	assertNoToolResult(t, first)
	second := registerToolWait(scope, "second")
	registerExecIDAlias(scope, "second-exec", 2, "second")
	defer cancelToolWait(second)
	routeExecClientResult(scope, execResult(1, "second-exec"))
	assertNoToolResult(t, first)
	assertNoToolResult(t, second)
	routeExecClientResult(scope, execResult(1, "first-exec"))
	routeExecClientResult(scope, execResult(2, ""))
	if got := waitForToolResultContext(context.Background(), first, time.Second); got.Error != "" {
		t.Fatal(got.Error)
	}
	if got := waitForToolResultContext(context.Background(), second, time.Second); got.Error != "" {
		t.Fatal(got.Error)
	}
	assertScopeRetired(t, scope)
}

func TestSameApprovalIDIsIndependentAcrossSocketRuns(t *testing.T) {
	firstScope, secondScope := socketToolScope(t.Name()), socketToolScope(t.Name()+"-second")
	first, second := registerInteractionWait(firstScope, 12), registerInteractionWait(secondScope, 12)
	deliverInteractionResponse(secondScope, &agentv1.InteractionResponse{Id: 12})
	select {
	case <-first:
		t.Fatal("sibling socket consumed approval")
	default:
	}
	if waitForInteractionResponse(context.Background(), second, time.Second) == nil {
		t.Fatal("own approval not delivered")
	}
	deliverInteractionResponse(firstScope, &agentv1.InteractionResponse{Id: 12})
	if waitForInteractionResponse(context.Background(), first, time.Second) == nil {
		t.Fatal("original approval lost")
	}
	assertScopeRetired(t, firstScope)
	assertScopeRetired(t, secondScope)
}

func TestLateShellEventsDoNotRecreateStateOrReachReusedCallID(t *testing.T) {
	scope := socketToolScope(t.Name())
	old := registerToolWait(scope, "reused-call")
	registerExecIDAlias(scope, "old-exec", 100, "reused-call")
	routeExecClientResult(scope, shellOutput(100, "before timeout"))
	if got := waitForToolResultContext(context.Background(), old, time.Nanosecond); !strings.Contains(got.Error, "timed out") {
		t.Fatalf("result = %+v", got)
	}
	assertScopeRetired(t, scope)
	current := registerToolWait(scope, "reused-call")
	registerExecIDAlias(scope, "new-exec", 101, "reused-call")
	defer cancelToolWait(current)
	for i := 0; i < 10; i++ {
		routeExecClientResult(scope, shellOutput(100, "late output"))
		routeExecClientResult(scope, shellExit(100))
		routeExecClientResult(scope, execResult(100, "old-exec"))
	}
	// Even a valid exec alias must not create shell state for an unknown seq.
	bogus := shellOutput(999, "unknown sequence")
	bogus.GetExecClientMessage().ExecId = "new-exec"
	routeExecClientResult(scope, bogus)
	assertNoToolResult(t, current)
	routeExecClientResult(scope, shellOutput(101, "current output"))
	routeExecClientResult(scope, shellExit(101))
	got := waitForToolResultContext(context.Background(), current, time.Second)
	if got.Error != "" || got.ShellAccum == nil || string(got.ShellAccum.Stdout) != "current output" {
		t.Fatalf("result = %+v", got)
	}
	assertScopeRetired(t, scope)
}

func TestConcurrentShellChunksAndRetirement(t *testing.T) {
	scope := socketToolScope(t.Name())
	tool := registerToolWait(scope, "shell")
	registerExecIDAlias(scope, "shell-exec", 42, "shell")
	var workers sync.WaitGroup
	for i := 0; i < 64; i++ {
		workers.Add(1)
		go func() { defer workers.Done(); routeExecClientResult(scope, shellOutput(42, "x")) }()
	}
	workers.Wait()
	routeExecClientResult(scope, shellExit(42))
	got := waitForToolResultContext(context.Background(), tool, time.Second)
	if got.ShellAccum == nil || len(got.ShellAccum.Stdout) != 64 {
		t.Fatalf("lost concurrent stdout: %+v", got)
	}
	for i := 0; i < 64; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			routeExecClientResult(scope, shellOutput(42, "late"))
			cancelToolWait(tool)
		}()
	}
	workers.Wait()
	if string(got.ShellAccum.Stdout) != strings.Repeat("x", 64) {
		t.Fatal("delivered shell result changed after retirement")
	}
	assertScopeRetired(t, scope)
	// Exercise cleanup while producer goroutines are still inside routing.
	racing := registerToolWait(scope, "racing")
	registerExecIDAlias(scope, "racing-exec", 43, "racing")
	for i := 0; i < 64; i++ {
		workers.Add(1)
		go func() { defer workers.Done(); routeExecClientResult(scope, shellOutput(43, "x")) }()
	}
	cancelToolWait(racing)
	workers.Wait()
	routeExecClientResult(scope, shellExit(43))
	assertNoToolResult(t, racing)
	assertScopeRetired(t, scope)
}

type failedToolWriter struct{}

func (failedToolWriter) Write([]byte) (int, error) { return 0, errors.New("test stream closed") }

func TestFailedExecWriteRetiresWaiter(t *testing.T) {
	sess := &Session{RequestID: t.Name()}
	_, result := executeBackgroundComposerTool(context.Background(), failedToolWriter{}, sess, PendingToolCall{ID: "write-failed", Name: "Shell", Arguments: `{"command":"echo test"}`})
	if result.GetError() == nil {
		t.Fatal("failed writer was accepted")
	}
	assertScopeRetired(t, bidiToolScope(sess.RequestID))
}
