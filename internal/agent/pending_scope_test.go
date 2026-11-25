package agent

import (
	"context"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	agentv1 "cursor-byok/internal/protocodec/gen/agent/v1"
	aiserv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func appendForOwner(t *testing.T, owner string, msg *agentv1.AgentClientMessage) {
	t.Helper()
	in, err := bidiInboxes.claim(owner, "agent")
	if err != nil {
		bidiInboxes.mu.Lock()
		in = bidiInboxes.entries[owner]
		bidiInboxes.mu.Unlock()
		if in == nil || in.protocol != "agent" {
			t.Fatal(err)
		}
	} else {
		t.Cleanup(in.release)
	}
	bidiInboxes.mu.Lock()
	seq := in.nextSeq
	bidiInboxes.mu.Unlock()
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	bidi, err := proto.Marshal(&aiserv1.BidiAppendRequest{RequestId: &aiserv1.BidiRequestId{RequestId: owner}, Data: hex.EncodeToString(raw), AppendSeqno: seq})
	if err != nil {
		t.Fatal(err)
	}
	if result := HandleBidiAppend(bidi, "application/proto"); result.Status != 200 {
		t.Fatalf("append: %d %s", result.Status, result.Body)
	}
	data, err := in.next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result := applyAgentBidiPayload(owner, data); result.Status != 200 {
		t.Fatalf("apply: %d %s", result.Status, result.Body)
	}
}

func assertToolStillPending(t *testing.T, ch chan *toolResultEnvelope) {
	t.Helper()
	select {
	case result := <-ch:
		t.Fatalf("another owner delivered result %+v", result)
	default:
	}
}

func TestPendingResultsAreScopedToEachRun(t *testing.T) {
	a, b, attacker := "scope-run-a-"+uuid.NewString(), "scope-run-b-"+uuid.NewString(), "scope-run-other-"+uuid.NewString()
	for _, owner := range []string{a, b, attacker} {
		t.Cleanup(func() { DropSession(owner) })
	}
	// Providers can reuse tool-call ids in different conversations, and wire
	// ids are client supplied. Neither may overwrite another run's waiter.
	toolA, toolB := registerToolWait(bidiToolScope(a), "same-tool"), registerToolWait(bidiToolScope(b), "same-tool")
	t.Cleanup(func() { cancelToolWait(toolA); cancelToolWait(toolB) })
	registerExecIDAlias(bidiToolScope(a), "same-exec", 77, "same-tool")
	registerExecIDAlias(bidiToolScope(b), "same-exec", 77, "same-tool")
	interactionA, interactionB := registerInteractionWait(bidiToolScope(a), 88), registerInteractionWait(bidiToolScope(b), 88)
	t.Cleanup(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		waitForInteractionResponse(ctx, interactionA, time.Hour)
		waitForInteractionResponse(ctx, interactionB, time.Hour)
	})
	toolMsg := &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: &agentv1.ExecClientMessage{Id: 77, ExecId: "same-exec"}}}
	approval := &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_InteractionResponse{InteractionResponse: &agentv1.InteractionResponse{Id: 88}}}
	// Both guessed aliases and identifier-free legacy messages from an
	// unrelated Bidi request must leave the live runs untouched.
	appendForOwner(t, attacker, toolMsg)
	appendForOwner(t, attacker, &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: &agentv1.ExecClientMessage{}}})
	appendForOwner(t, attacker, approval)
	assertToolStillPending(t, toolA)
	assertToolStillPending(t, toolB)
	select {
	case <-interactionA:
		t.Fatal("foreign approval reached run A")
	default:
	}
	select {
	case <-interactionB:
		t.Fatal("foreign approval reached run B")
	default:
	}
	// Deliver independent runs concurrently through the actual Bidi parser.
	var wg sync.WaitGroup
	for _, owner := range []string{a, b} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			appendForOwner(t, owner, toolMsg)
			appendForOwner(t, owner, approval)
		}(owner)
	}
	wg.Wait()
	for _, ch := range []chan *toolResultEnvelope{toolA, toolB} {
		select {
		case env := <-ch:
			if env.ExecClient.GetExecId() != "same-exec" {
				t.Fatal("incorrect result")
			}
		default:
			t.Fatal("owner's result not delivered")
		}
	}
	for _, ch := range []chan *agentv1.InteractionResponse{interactionA, interactionB} {
		select {
		case resp := <-ch:
			if resp.Id != 88 {
				t.Fatal("incorrect approval")
			}
		default:
			t.Fatal("owner's approval not delivered")
		}
	}
	pendingMu.Lock()
	defer pendingMu.Unlock()
	for _, owner := range []string{a, b} {
		if pending[toolWaitKey{bidiToolScope(owner), "same-tool"}] != nil || execIDAlias[toolWaitKey{bidiToolScope(owner), "same-exec"}] != (toolWaitKey{}) || seqAlias[sequenceWaitKey{bidiToolScope(owner), 77}] != (toolWaitKey{}) {
			t.Fatal("completed waiter retained aliases")
		}
	}
}

func TestLateAndForeignShellEventsDoNotAllocateBuffers(t *testing.T) {
	const owner = "late-shell-owner"
	ch := registerToolWait(bidiToolScope(owner), "shell-tool")
	defer cancelToolWait(ch)
	registerExecIDAlias(bidiToolScope(owner), "shell-exec", 91, "shell-tool")
	msg := &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: &agentv1.ExecClientMessage{Id: 91, Message: &agentv1.ExecClientMessage_ShellStream{ShellStream: &agentv1.ShellStream{Event: &agentv1.ShellStream_Stdout{Stdout: &agentv1.ShellStreamStdout{Data: "output"}}}}}}}
	routeExecClientResult(bidiToolScope("foreign-owner"), msg)
	pendingMu.Lock()
	foreign := shellAccum[sequenceWaitKey{bidiToolScope("foreign-owner"), 91}]
	pendingMu.Unlock()
	if foreign != nil {
		t.Fatal("foreign event allocated shell buffer")
	}
	routeExecClientResult(bidiToolScope(owner), msg)
	pendingMu.Lock()
	state := shellAccum[sequenceWaitKey{bidiToolScope(owner), 91}]
	pendingMu.Unlock()
	if state == nil || string(state.Stdout) != "output" {
		t.Fatal("active shell output was lost")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = waitForToolResultContext(ctx, ch, time.Hour)
	// Commands may continue producing output after the foreground timeout.
	for i := 0; i < 10; i++ {
		routeExecClientResult(bidiToolScope(owner), msg)
	}
	pendingMu.Lock()
	defer pendingMu.Unlock()
	if shellAccum[sequenceWaitKey{bidiToolScope(owner), 91}] != nil {
		t.Fatal("late output recreated a discarded buffer")
	}
}
