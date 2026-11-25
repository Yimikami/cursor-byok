package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	aiserverv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
	"cursor-byok/internal/relay"

	"google.golang.org/protobuf/proto"
)

// execSeqCounter hands out monotonically increasing ExecServerMessage.Id
// values. Previously we derived seq from (round*10 + len(result.ToolCalls) + 1)
// — but len(result.ToolCalls) is constant across the inner loop, so every
// tool call in a single round received the SAME seq. That made seqAlias
// and shellAccum collide: a second tool call's shell Start event could
// land on the first call's accumulator, and backgrounded shell events
// arriving late could wake the wrong pending waiter.
//
// Starting at 1 so a zero-valued seq can be distinguished from "real" ids
// during debugging.
var execSeqCounter atomic.Uint32

func nextExecSeq() uint32 {
	return execSeqCounter.Add(1)
}

// lockedWriter serialises all frame writes against a single mutex so the
// background keepalive goroutine and the main response loop can share one
// pipe without interleaving bytes between a frame's header and its body.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (l *lockedWriter) WriteFrame(body []byte, endStream bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return writeFrame(l.w, body, endStream)
}

func (l *lockedWriter) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if f, ok := l.w.(interface{ Flush() }); ok {
		f.Flush()
	}
}

type AdapterTarget struct {
	// Set only by the user's local editor companion, never by account metadata.
	NativeSelectionOverride bool
	ProviderType            string
	BaseURL                 string
	APIKey                  string
	Model                   string
	StableID                string
	DisplayName             string
	Opts                    AdapterOpts
}

// AdapterResolver returns all configured adapters so handlers can resolve the
// request-selected model instead of always using the first configured adapter.
type AdapterResolver func() []AdapterTarget

type AdapterOpts struct {
	ReasoningEffort string
	ServiceTier     string
	MaxOutputTokens int
	ThinkingBudget  int
	Temperature     *float32
	TopP            *float32
	StopSequences   []string
}

func AdapterTargetFromRelay(a relay.AdapterInfo) AdapterTarget {
	return AdapterTarget{
		ProviderType: a.Type,
		BaseURL:      a.BaseURL,
		APIKey:       a.APIKey,
		Model:        a.ModelID,
		StableID:     a.StableID(),
		DisplayName:  a.DisplayName,
		Opts: AdapterOpts{
			ServiceTier:     a.ServiceTier,
			MaxOutputTokens: a.MaxOutputTokens,
			ThinkingBudget:  a.ThinkingBudget,
		},
	}
}

func ResolveAdapterForModel(adapters []AdapterTarget, requested string) (AdapterTarget, bool) {
	if len(adapters) == 0 {
		return AdapterTarget{}, false
	}
	requested = strings.TrimSpace(requested)

	if requested != "" {
		for _, a := range adapters {
			if strings.EqualFold(requested, a.StableID) || strings.EqualFold(requested, a.Model) || (a.DisplayName != "" && strings.EqualFold(requested, a.DisplayName)) {
				return a, true
			}
		}
	}
	// The adapter slice arrives priority-sorted by the bridge, with the
	// configured active model first.
	return adapters[0], true
}

// Explicit companion selection applies to local agent inference. Separate
// commit/review model routing and Cursor-hosted services retain their semantics.
func resolveNativeAdapterForModel(adapters []AdapterTarget, requested string) (AdapterTarget, bool) {
	for _, adapter := range adapters {
		if adapter.NativeSelectionOverride {
			return adapter, true
		}
	}
	return ResolveAdapterForModel(adapters, requested)
}

func ResolveAdapterForAgentSession(adapters []AdapterTarget, sess *Session) (AdapterTarget, bool) {
	requested := ""
	if sess != nil && sess.ModelDetails != nil {
		requested = sess.ModelDetails.GetModelId()
		if requested == "" {
			requested = sess.ModelDetails.GetDisplayModelId()
		}
	}
	target, ok := resolveNativeAdapterForModel(adapters, requested)
	if ok && sess != nil && sess.RequestedModel != nil {
		for _, param := range sess.RequestedModel.GetParameters() {
			if param.GetId() == "reasoning" && param.GetValue() != "none" {
				target.Opts.ReasoningEffort = param.GetValue()
			}
		}
	}
	return target, ok
}

func ResolveAdapterForBugBotRequest(adapters []AdapterTarget, req *aiserverv1.StreamBugBotRequest) (AdapterTarget, bool) {
	requested := ""
	if req != nil && req.GetModelDetails() != nil {
		requested = req.GetModelDetails().GetModelName()
	}
	return ResolveAdapterForModel(adapters, requested)
}

// RunSSEHeaders is the response header set the bridge writes BEFORE handing
// the body pipe to RunSSE. They match the working app's captured
// agent.v1.AgentService/RunSSE response: text/event-stream content type and
// Connect's gzip negotiation hints.
var RunSSEHeaders = http.Header{
	"Content-Type":             {"text/event-stream"},
	"Cache-Control":            {"no-cache"},
	"Connect-Content-Encoding": {"gzip"},
	"Connect-Accept-Encoding":  {"gzip"},
}

// HandleRunSSE streams the BYOK chat completion to w as Connect SSE frames.
// The MITM bridge already wrote response headers and a 200 status; this
// function only writes the body. Returns when the stream finishes (success
// or error), at which point the bridge can close the underlying pipe.
func HandleRunSSE(
	ctx context.Context,
	reqBody []byte,
	contentType string,
	rawWriter io.Writer,
	resolve AdapterResolver,
) {
	// Wrap the pipe writer in a mutex so the keepalive goroutine below
	// and the main stream loop never interleave bytes between a frame's
	// header and its body. writeFrame already serializes into a single
	// Write call, so a per-Write lock is enough.
	w := &lockedWriter{w: rawWriter}
	bid := &aiserverv1.BidiRequestId{}
	if err := decodeUnary(reqBody, contentType, bid); err != nil {
		writeEndStreamError(w, "decode bidi request id: "+err.Error())
		return
	}
	requestID := bid.GetRequestId()
	ctx, detach, inputErrors, err := attachLegacyBidi(ctx, requestID, "agent", applyAgentBidiPayload)
	if err != nil {
		writeEndStreamError(w, err.Error())
		return
	}
	defer detach()
	sess := WaitForSession(ctx, requestID)
	if sess == nil || sess.UserText == "" {
		select {
		case err := <-inputErrors:
			writeEndStreamError(w, err.Error())
			return
		default:
		}
		writeEndStreamError(w, "no session/user text for request_id="+requestID)
		return
	}
	runAgentSession(ctx, requestID, sess, rawWriter, resolve)
}

// runAgentSession is shared by the legacy HTTP transport and WebSocket v1.
func runAgentSession(ctx context.Context, requestID string, sess *Session, rawWriter io.Writer, resolve AdapterResolver) {
	runAgentSessionWithScope(ctx, requestID, sess, rawWriter, resolve, bidiToolScope(requestID))
}

func runAgentSessionWithScope(ctx context.Context, requestID string, sess *Session, rawWriter io.Writer, resolve AdapterResolver, scope toolWaitScope) {
	w := &lockedWriter{w: rawWriter}
	// Periodic keepalive — Cursor closes the SSE pipe after ~60-90s of
	// write-side silence, and long OpenAI generations (especially with
	// reasoning models that buffer before emitting) can easily cross that
	// threshold mid-stream, forcing a reconnect that throws away chat
	// history on the UI side. A zero-delta TokenDelta frame every 15s
	// looks like no-op to Cursor's renderer but keeps the connection
	// active all the way through.
	keepaliveCtx, stopKeepalive := context.WithCancel(ctx)
	defer stopKeepalive()
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-keepaliveCtx.Done():
				return
			case <-t.C:
				if err := writeKeepaliveFrame(w); err != nil {
					return
				}
			}
		}
	}()
	target, ok := ResolveAdapterForAgentSession(resolve(), sess)
	if !ok {
		writeEndStreamError(w, "no BYOK adapter configured")
		return
	}
	providerType, baseURL, apiKey, model, adapterOpts := target.ProviderType, target.BaseURL, target.APIKey, target.Model, target.Opts
	// Pick the transport by adapter type. OpenAI is the default because
	// "openai-compatible" covers the long tail of providers (Groq, OpenRouter,
	// Together, vLLM, llama.cpp, Azure OpenAI, etc.) — anything that isn't
	// explicitly "anthropic" goes through the chat-completions path.
	var stream providerStreamer = streamOpenAI
	if strings.EqualFold(providerType, "anthropic") {
		stream = streamAnthropic
	}

	// Persist the user message immediately so history exists before streaming.
	// The assistant reply will be appended later when the stream finishes.
	earlyPersistUserTurn(sess.ConversationID, requestID, sess.UserText)

	// Populate sess.McpMap first — buildMessageHistory reads it to emit the
	// <mcp_tools> block that tells the model which synthesised function
	// maps to which real server/tool.
	tools := openAIToolsForRequest(sess)
	messages := buildMessageHistory(sess)
	// Snapshot the prompt length BEFORE the loop starts: everything the loop
	// appends (assistant-with-tool_calls + tool-role results) is new state
	// we want to persist as the turn's replay. Anything <= initialLen is
	// the static prompt (system + history + current user message) and must
	// NOT be saved a second time.
	initialLen := len(messages)
	startedAt := time.Now()

	var assistantBuf strings.Builder
	var lastResult *streamResult
	var promptTokens, completionTokens int64
	// Round cap was 6 but heavy tool users (Plan mode scanning a large repo,
	// Debug mode tailing many files) routinely hit that and the loop exited
	// mid-tool_call. With rich history persistence we can afford 20 rounds;
	// the wallclock cap below protects against runaway loops.
	const maxLoopRounds = 20
	const maxTurnDuration = 5 * time.Minute
	turnDeadline := startedAt.Add(maxTurnDuration)

	for round := 0; round < maxLoopRounds; round++ {
		if time.Now().After(turnDeadline) {
			break
		}
		roundBuf := strings.Builder{}
		thinkingStarted := false
		thinkingStart := time.Now()
		result, streamErr := stream(ctx, baseURL, apiKey, model, messages, tools, adapterOpts,
			func(chunk string, reasoning string, done bool) error {
				if reasoning != "" {
					if !thinkingStarted {
						thinkingStarted = true
						thinkingStart = time.Now()
					}
					if err := writeThinkingDeltaFrame(w, reasoning); err != nil {
						return err
					}
				}
				if chunk != "" {
					if thinkingStarted {
						dur := int32(time.Since(thinkingStart).Milliseconds())
						_ = writeThinkingCompletedFrame(w, dur)
						thinkingStarted = false
					}
					roundBuf.WriteString(chunk)
					assistantBuf.WriteString(chunk)
					if err := writeTextDeltaFrame(w, chunk); err != nil {
						return err
					}
				}
				if done && thinkingStarted {
					dur := int32(time.Since(thinkingStart).Milliseconds())
					_ = writeThinkingCompletedFrame(w, dur)
					thinkingStarted = false
				}
				return nil
			})
		if result != nil {
			// Track prompt / completion separately so the summary reports
			// real billable usage instead of the double-counted sum of every
			// round's usage.total_tokens (which includes a repeat of the
			// full prompt each round).
			var roundPrompt, roundCompletion int64
			if result.Usage != nil {
				roundPrompt = int64(result.Usage.PromptTokens)
				roundCompletion = int64(result.Usage.CompletionTokens)
			}
			if roundPrompt == 0 && roundCompletion == 0 {
				for _, m := range messages {
					roundPrompt += int64(len(m.Content) / 4)
				}
				roundCompletion = int64(roundBuf.Len() / 4)
			}
			promptTokens += roundPrompt
			completionTokens += roundCompletion
			if delta := roundPrompt + roundCompletion; delta > 0 {
				// TokenDeltaUpdate.Tokens is int32 — clamp to stay in range
				// on very large contexts. Overflow would flip to a negative
				// count and confuse Cursor's UI counter.
				d := delta
				if d > int64(^uint32(0)>>1) {
					d = int64(^uint32(0) >> 1)
				}
				_ = writeTokenDeltaFrame(w, int32(d))
			}
		}
		lastResult = result
		if streamErr != nil {
			writeEndStreamError(w, streamErr.Error())
			persistFailure(sess, requestID, startedAt, messages, baseURL, model, result, streamErr)
			DropSession(requestID)
			return
		}
		// Record this round's assistant message (including any tool_calls)
		// in the running message list so the next round sees the context.
		assistantMsg := textMessage("assistant", roundBuf.String())
		preserveProviderReasoning(&assistantMsg, result)
		if len(result.ToolCalls) > 0 {
			for _, tc := range result.ToolCalls {
				assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, openAIToolCallMsg{
					ID:       tc.ID,
					Type:     "function",
					Function: openAIToolCallFn{Name: tc.Name, Arguments: tc.Arguments},
				})
			}
		}
		messages = append(messages, assistantMsg)

		// No tool calls: the model answered with final text. Finish up.
		if len(result.ToolCalls) == 0 {
			if err := writeTurnEndedFrame(w); err != nil {
				writeEndStreamError(w, err.Error())
				DropSession(requestID)
				return
			}
			break
		}

		// Build a checkpoint replay block once per round so Cursor can
		// sync state between actions — working app captures show this
		// re-broadcast (~10KB conversation_checkpoint_update) bracketing
		// every user-visible action and tool exec.
		var replay []byte
		if ctx := buildWorkspaceContext(sess); ctx != "" {
			rep := map[string]string{"content": ctx, "role": "user"}
			if b, err := json.Marshal(rep); err == nil {
				replay = b
			}
		}
		_ = writeConversationCheckpoint(w, replay)

		// Execute every tool the model asked for, in order. Sequential for
		// the MVP — Cursor's UI renders parallel ones fine but correlating
		// responses serially is simpler to reason about first.
		for _, pc := range result.ToolCalls {
			// Local-only tools (SwitchMode, CreatePlan, AddTodo, UpdateTodo)
			// don't need to hit Cursor's IDE. Resolve them in-process, emit
			// the UI pill frames (when a proto wrapper is available so Cursor
			// can render the call), and feed the result back to the model.
			if localResult, pill, interaction, handled := handleLocalTool(sess, pc); handled {
				if pill != nil {
					_ = writeToolCallStarted(w, pc.ID, pill)
				}
				// For interactions that REQUIRE a user verdict (SwitchMode
				// shows an Approve/Reject dialog), register a waiter BEFORE
				// emitting the frame so we don't miss a fast response, then
				// block until Cursor's BidiAppend delivers the verdict.
				// Without this the model says "switched to X mode" before
				// the user has even clicked Approve.
				if interaction != nil && pc.Name == "SwitchMode" {
					waitCh := registerInteractionWait(scope, interaction.GetId())
					_ = writeInteractionQuery(w, interaction)
					verdict := waitForInteractionResponse(ctx, waitCh, 0)
					localResult = applyModeSwitchVerdict(sess, pill, interaction, verdict)
					tools = openAIToolsForRequest(sess)
					messages[0] = textMessage("system", systemPromptFor(sess.Mode))
				} else if interaction != nil {
					// Fire-and-forget for interactions that don't need a
					// blocking verdict (e.g. first CreatePlan, which just
					// opens the Plan panel).
					_ = writeInteractionQuery(w, interaction)
				}
				if pill != nil {
					_ = writeToolCallCompleted(w, pc.ID, pill)
				}
				messages = append(messages, toolResultMessage(pc.ID, pc.Name, localResult))
				continue
			}
			tc, buildErr := buildToolCallProto(sess, pc)
			if buildErr != "" {
				// Tool args failed local pre-processing (e.g. StrReplace's
				// read-modify-write hit file-not-found or a non-unique
				// match). Feed the error back to the model so it can retry
				// with better args instead of hanging the loop.
				messages = append(messages, toolResultMessage(pc.ID, pc.Name, `{"error":`+jsonStringEscape(buildErr)+`}`))
				continue
			}
			if tc == nil {
				messages = append(messages, toolResultMessage(pc.ID, pc.Name, `{"error": "tool `+pc.Name+` not implemented in cursor-byok yet"}`))
				continue
			}
			// Frame 1: let Cursor's UI draw the "tool is running" pill.
			if err := writeToolCallStarted(w, pc.ID, tc); err != nil {
				writeEndStreamError(w, err.Error())
				DropSession(requestID)
				return
			}
			// Frame 2: ExecServerMessage with the proper args variant so
			// Cursor's IDE actually executes the tool. Shell uses the
			// streaming variant; everything else has its own matching args
			// field (WriteArgs / ReadArgs / LsArgs / GrepArgs / DeleteArgs).
			// Cursor never ACKs back unless this frame lands.
			execID := newExecID(execToolName(pc.Name))
			// Use a process-wide monotonic counter so every tool_call —
			// regardless of round, parallel tools in the same round, or
			// late shell events backing up in the queue — keys its own
			// seqAlias/shellAccum entry. See nextExecSeq doc for the bug
			// this replaces.
			seq := nextExecSeq()
			waitCh := registerToolWait(scope, pc.ID)
			registerExecIDAlias(scope, execID, seq, pc.ID)
			if err := writeExecRequest(w, execID, seq, pc, tc); err != nil {
				cancelToolWait(waitCh)
				writeEndStreamError(w, err.Error())
				DropSession(requestID)
				return
			}
			// Cursor owns execution timeouts and approvals. A pending permission
			// dialog is not a failed tool; wait for its terminal event or cancel.
			env := waitForNativeToolResult(ctx, waitCh)
			if ctx.Err() != nil {
				DropSession(requestID)
				return
			}

			// Attach the tool result back onto the ToolCall proto so
			// Cursor's pill renders stdout/stderr directly under the
			// command. Without this the model sees the output via the
			// tool-role message but the user-facing UI shows only the
			// command header, which feels like the tool "did nothing".
			attachToolResultToProto(tc, pc.Name, env)

			// Frame 3: mark the pill as finished.
			if err := writeToolCallCompleted(w, pc.ID, tc); err != nil {
				writeEndStreamError(w, err.Error())
				DropSession(requestID)
				return
			}
			content := env.ResultJSON
			if env.Error != "" {
				content = `{"error":` + jsonStringEscape(env.Error) + `}`
			}

			if content == "" {
				content = `{"result":"ok"}`
			}
			// Cap tool-result size fed back to the model. Big Write/Read/
			// Shell outputs otherwise get re-sent on every subsequent
			// round, multiplying the prompt bill. 24KB is plenty for the
			// model to reason about a result; anything bigger gets a tail
			// truncation marker so it can call Read again if it needs more.
			const maxToolResultBytes = 24 * 1024
			if len(content) > maxToolResultBytes {
				content = content[:maxToolResultBytes] + "\n…[tool output truncated — call Read on the file or re-run with a narrower scope to see the rest]"
			}
			messages = append(messages, toolResultMessage(pc.ID, pc.Name, content))
			// Replay the carry-forward block again after each tool result
			// to match working app's frame ordering.
			_ = writeConversationCheckpoint(w, replay)
		}
	}

	// If the loop exited while the model still had pending tool_calls (round
	// cap or wallclock deadline hit), surface a friendly warning and emit a
	// clean TurnEnded frame instead of END-STREAM+error. writeEndStreamError
	// makes Cursor drop the UI history; writeTurnEndedFrame leaves it intact
	// so the user can send a follow-up to continue the task.
	capWarning := ""
	if lastResult != nil && len(lastResult.ToolCalls) > 0 {
		capWarning = "\n\n_[tool loop capped at " + strconv.Itoa(maxLoopRounds) +
			" rounds / 5 minutes — the task may be incomplete. Send a follow-up message to continue.]_"
		_ = writeTextDeltaFrame(w, capWarning)
		assistantBuf.WriteString(capWarning)
		_ = writeTurnEndedFrame(w)
	}

	// Persist + close stream.
	var art *turnArtifacts
	if lastResult != nil {
		var tcNames []string
		for _, tc := range lastResult.ToolCalls {
			tcNames = append(tcNames, tc.Name+"#"+tc.ID)
		}
		billable := promptTokens + completionTokens
		summary := map[string]any{
			"request_id":        requestID,
			"started_at":        startedAt.UTC().Format(time.RFC3339Nano),
			"finished_at":       time.Now().UTC().Format(time.RFC3339Nano),
			"duration_ms":       time.Since(startedAt).Milliseconds(),
			"model":             model,
			"requested_model":   requestedModelForSession(sess),
			"provider":          providerFromURL(baseURL),
			"output_len":        assistantBuf.Len(),
			"message_count":     len(messages),
			"finish_reason":     lastResult.FinishReason,
			"tool_calls":        tcNames,
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      billable,
		}
		summaryBytes, _ := json.MarshalIndent(summary, "", "  ")
		art = &turnArtifacts{
			RequestBody: lastResult.RequestBody,
			SSEJSONL:    lastResult.SSERaw,
			SummaryJSON: summaryBytes,
			TotalTokens: int(billable),
		}
	}
	// Always persist the turn — even when the assistant produced only tool
	// calls and zero user-visible text. Skipping that case used to hide
	// every tool-heavy turn from the history viewer and make debugging
	// "my tool call didn't fire" indistinguishable from "the proxy was off".
	assistantText := assistantBuf.String()
	if assistantText == "" && lastResult != nil && len(lastResult.ToolCalls) > 0 {
		var names []string
		for _, tc := range lastResult.ToolCalls {
			names = append(names, tc.Name)
		}
		assistantText = "[tool-only turn: " + strings.Join(names, ", ") + "]"
	}
	// Collect just the messages this RunSSE appended (tool_calls + tool
	// results) so the next turn replays them verbatim. Anything <= initialLen
	// was already on disk from earlier turns.
	var storedMessages []StoredMessage
	if len(messages) > initialLen {
		storedMessages = make([]StoredMessage, 0, len(messages)-initialLen)
		for _, m := range messages[initialLen:] {
			storedMessages = append(storedMessages, openAIToStored(m))
		}
	}
	RecordTurn(sess.ConversationID, requestID, sess.UserText, assistantText, modeString(sess.Mode), art, storedMessages)
	_ = writeEndStream(w)
	DropSession(requestID)
}

func persistFailure(sess *Session, requestID string, startedAt time.Time, messages []openAIMessage, baseURL, model string, result *streamResult, streamErr error) {
	if sess == nil || result == nil {
		return
	}
	summary := map[string]any{
		"request_id":      requestID,
		"started_at":      startedAt.UTC().Format(time.RFC3339Nano),
		"finished_at":     time.Now().UTC().Format(time.RFC3339Nano),
		"duration_ms":     time.Since(startedAt).Milliseconds(),
		"model":           model,
		"requested_model": requestedModelForSession(sess),
		"provider":        providerFromURL(baseURL),
		"message_count":   len(messages),
		"error":           streamErr.Error(),
	}
	sb, _ := json.MarshalIndent(summary, "", "  ")
	RecordTurn(sess.ConversationID, requestID, sess.UserText, "", modeString(sess.Mode), &turnArtifacts{
		RequestBody: result.RequestBody,
		SSEJSONL:    result.SSERaw,
		SummaryJSON: sb,
	}, nil)
}

func jsonStringEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// providerFromURL guesses a human-readable provider slug from the baseURL
// so the summary.json artifact can be filtered/grouped later.
func providerFromURL(url string) string {
	url = strings.ToLower(url)
	switch {
	case strings.Contains(url, "openai.com"):
		return "openai"
	case strings.Contains(url, "anthropic.com"):
		return "anthropic"
	case strings.Contains(url, "openrouter.ai"):
		return "openrouter"
	case strings.Contains(url, "groq.com"):
		return "groq"
	case strings.Contains(url, "together"):
		return "together"
	default:
		return "custom"
	}
}

func requestedModelForSession(sess *Session) string {
	if sess == nil || sess.ModelDetails == nil {
		return ""
	}
	if id := sess.ModelDetails.GetModelId(); id != "" {
		return id
	}
	return sess.ModelDetails.GetDisplayModelId()
}

// writeEndStreamError emits the Connect END-STREAM frame with an error
// payload — Cursor parses {"error": {"code": ..., "message": ...}} as an
// abnormal terminator and shows it instead of looping in retry forever.
func writeEndStreamError(w io.Writer, msg string) {
	body := []byte(`{"error":{"code":"internal","message":` + jsonString(msg) + `}}`)
	_ = writeFrame(w, body, true)
	flushIfPossible(w)
}

func jsonString(s string) string {
	b, err := protoMarshalJSONString(s)
	if err != nil {
		return `""`
	}
	return b
}

func protoMarshalJSONString(s string) (string, error) {
	// Avoid a json.Marshal import here just for one string by hand-quoting.
	out := []byte{'"'}
	for _, r := range s {
		switch r {
		case '\\':
			out = append(out, '\\', '\\')
		case '"':
			out = append(out, '\\', '"')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			if r < 0x20 {
				out = append(out, '\\', 'u', '0', '0', hexNibble(byte(r>>4)), hexNibble(byte(r&0xf)))
				continue
			}
			out = append(out, []byte(string(r))...)
		}
	}
	out = append(out, '"')
	return string(out), nil
}

func hexNibble(b byte) byte {
	if b < 10 {
		return '0' + b
	}
	return 'a' + b - 10
}

// Compile-time guard so I notice if proto stops being used here in a future
// refactor (the package-level imports are otherwise valid even if proto.X
// drops out).
var _ = proto.Marshal
