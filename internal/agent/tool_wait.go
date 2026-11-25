package agent

import (
	"context"
	"sync"
	"time"

	agentv1 "cursor-byok/internal/protocodec/gen/agent/v1"
)

// A transport and its request own all tool executions and approval prompts.
// Provider tool IDs and Cursor sequence IDs are only unique within that owner.
// In particular, a client-chosen Bidi request ID must not address a WebSocket's
// waiters, even if it guesses the socket's server-generated request ID.
type toolWaitScope struct {
	transport string
	requestID string
}

func bidiToolScope(requestID string) toolWaitScope {
	return toolWaitScope{transport: "bidi", requestID: requestID}
}

func socketToolScope(requestID string) toolWaitScope {
	return toolWaitScope{transport: "websocket", requestID: requestID}
}

func bugBotToolScope(requestID string) toolWaitScope {
	return toolWaitScope{transport: "bugbot", requestID: requestID}
}

type toolWaitKey struct {
	scope toolWaitScope
	id    string
}

type sequenceWaitKey struct {
	scope toolWaitScope
	id    uint32
}

type pendingResult struct {
	ch                   chan *toolResultEnvelope
	backgroundComposerID string
}

var (
	pendingMu          sync.Mutex
	pending            = map[toolWaitKey]*pendingResult{}
	execIDAlias        = map[toolWaitKey]toolWaitKey{}
	seqAlias           = map[sequenceWaitKey]toolWaitKey{}
	shellAccum         = map[sequenceWaitKey]*shellAccumState{}
	pendingInteraction = map[sequenceWaitKey]chan *agentv1.InteractionResponse{}
)

func registerInteractionWait(scope toolWaitScope, id uint32) chan *agentv1.InteractionResponse {
	ch := make(chan *agentv1.InteractionResponse, 1)
	pendingMu.Lock()
	pendingInteraction[sequenceWaitKey{scope, id}] = ch
	pendingMu.Unlock()
	return ch
}

func deliverInteractionResponse(scope toolWaitScope, resp *agentv1.InteractionResponse) {
	if resp == nil {
		return
	}
	key := sequenceWaitKey{scope, resp.GetId()}
	pendingMu.Lock()
	defer pendingMu.Unlock()
	if ch := pendingInteraction[key]; ch != nil {
		delete(pendingInteraction, key)
		select {
		case ch <- resp:
		default:
		}
	}
}

func waitForInteractionResponse(ctx context.Context, ch <-chan *agentv1.InteractionResponse, timeout time.Duration) *agentv1.InteractionResponse {
	defer func() {
		pendingMu.Lock()
		defer pendingMu.Unlock()
		for key, waiting := range pendingInteraction {
			if waiting == ch {
				delete(pendingInteraction, key)
			}
		}
	}()
	var expired <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case <-ctx.Done():
		return nil
	case r := <-ch:
		return r
	case <-expired:
		return nil
	}
}

func registerToolWait(scope toolWaitScope, toolCallID string) chan *toolResultEnvelope {
	return registerBackgroundToolWait(scope, toolCallID, "")
}

// Only a background-composer executor assigns this publication destination.
// A foreground/socket waiter must not publish into a background stream even
// when its request ID happens to equal a known background-composer ID.
func registerBackgroundToolWait(scope toolWaitScope, toolCallID, backgroundComposerID string) chan *toolResultEnvelope {
	ch := make(chan *toolResultEnvelope, 1)
	key := toolWaitKey{scope, toolCallID}
	pendingMu.Lock()
	// Retire any old aliases before reusing a provider's tool-call ID.
	removePendingLocked(key)
	pending[key] = &pendingResult{ch: ch, backgroundComposerID: backgroundComposerID}
	pendingMu.Unlock()
	return ch
}

func registerExecIDAlias(scope toolWaitScope, execID string, seq uint32, toolCallID string) {
	key := toolWaitKey{scope, toolCallID}
	pendingMu.Lock()
	defer pendingMu.Unlock()
	if pending[key] != nil {
		execIDAlias[toolWaitKey{scope, execID}] = key
		seqAlias[sequenceWaitKey{scope, seq}] = key
	}
}

// removePendingLocked retires the waiter and every alias/accumulator together.
// Success, timeout, cancellation, and failed writes all use this same cleanup.
func removePendingLocked(key toolWaitKey) {
	delete(pending, key)
	for alias, target := range execIDAlias {
		if target == key {
			delete(execIDAlias, alias)
		}
	}
	for seq, target := range seqAlias {
		if target == key {
			delete(seqAlias, seq)
			delete(shellAccum, seq)
		}
	}
}

func completePendingLocked(key toolWaitKey, env *toolResultEnvelope) {
	pr := pending[key]
	removePendingLocked(key)
	if pr != nil {
		select {
		case pr.ch <- env:
		default:
		}
	}
}

func cancelToolWait(ch <-chan *toolResultEnvelope) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	for key, waiting := range pending {
		if waiting.ch == ch {
			removePendingLocked(key)
		}
	}
}

// pendingKeyLocked requires a known execution identity in this exact scope.
// Never deliver an unknown or late result to an unrelated "only waiter".
func pendingKeyLocked(scope toolWaitScope, execID string, seq uint32) (toolWaitKey, bool) {
	seqKey, hasSeq := seqAlias[sequenceWaitKey{scope, seq}]
	if execID != "" {
		key := toolWaitKey{scope, execID}
		if pending[key] == nil {
			key = execIDAlias[key]
		}
		if pending[key] == nil || (hasSeq && key != seqKey) {
			return toolWaitKey{}, false
		}
		return key, true
	}
	return seqKey, hasSeq && pending[seqKey] != nil
}

func waitForToolResultContext(ctx context.Context, ch chan *toolResultEnvelope, timeout time.Duration) *toolResultEnvelope {
	defer cancelToolWait(ch)
	var expired <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case <-ctx.Done():
		return &toolResultEnvelope{Error: ctx.Err().Error()}
	case env := <-ch:
		return env
	case <-expired:
		return &toolResultEnvelope{Error: "tool execution timed out after " + timeout.String()}
	}
}

// Native executors include their own timeout and approval policy. Their result
// wait must remain open while a user decides and must end on stream cancellation.
func waitForNativeToolResult(ctx context.Context, ch chan *toolResultEnvelope) *toolResultEnvelope {
	return waitForToolResultContext(ctx, ch, 0)
}
