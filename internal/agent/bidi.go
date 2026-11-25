package agent

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"strings"

	agentv1 "cursor-byok/internal/protocodec/gen/agent/v1"
	aiserverv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"

	"google.golang.org/protobuf/proto"
)

// Result is returned by the unary handlers (BidiAppend) so the MITM bridge
// can build a goproxy http.Response. Body is the marshalled response proto;
// Status is the HTTP status the bridge should set; ContentType is filled
// for non-error replies and left empty otherwise (bridge defaults to JSON).
type Result struct {
	Status      int
	ContentType string
	Body        []byte
}

// HandleBidiAppend validates the transport envelope and queues its opaque
// inner payload. The matching stream endpoint selects the inner protocol.
func HandleBidiAppend(reqBody []byte, contentType string) Result {
	bidi := &aiserverv1.BidiAppendRequest{}
	if err := decodeUnary(reqBody, contentType, bidi); err != nil {
		return errResult(http.StatusBadRequest, "decode bidi append: "+err.Error())
	}
	data, err := decodeBidiPayload(bidi)
	if err != nil {
		return errResult(http.StatusBadRequest, err.Error())
	}
	return bidiInboxes.append(bidi.GetRequestId().GetRequestId(), bidi.GetAppendSeqno(), data)
}

func handleAgentBidiAppendDecoded(bidi *aiserverv1.BidiAppendRequest) Result {
	if bidi == nil {
		return errResult(http.StatusBadRequest, "missing bidi append body")
	}

	requestID := ""
	if bidi.RequestId != nil {
		requestID = bidi.RequestId.RequestId
	}
	if requestID == "" {
		return errResult(http.StatusBadRequest, "missing request_id")
	}

	raw, err := decodeBidiPayload(bidi)
	if err != nil {
		return errResult(http.StatusBadRequest, err.Error())
	}
	return applyAgentBidiPayload(requestID, raw)
}

func applyAgentBidiPayload(requestID string, raw []byte) Result {
	sess := &Session{RequestID: requestID}
	if existing := GetSession(requestID); existing != nil {
		sess = existing
	}

	if len(raw) > 0 {
		acm := &agentv1.AgentClientMessage{}
		if err := proto.Unmarshal(raw, acm); err != nil {
			return errResult(http.StatusBadRequest, "parse agent client message: "+err.Error())
		}
		extractIntoSession(sess, acm)
		// Route tool results back to the RunSSE goroutine waiting on them.
		// Cursor posts ExecClientMessage with the matching tool_call_id
		// (exec_id) once the IDE finishes running a tool.
		routeExecClientResult(bidiToolScope(requestID), acm)
		// Route interaction verdicts (user approved/rejected a SwitchMode
		// prompt, CreatePlan prompt, WebSearch prompt, etc.) back to the
		// RunSSE goroutine that's blocking on the user's decision.
		if ir := acm.GetInteractionResponse(); ir != nil {
			deliverInteractionResponse(bidiToolScope(requestID), ir)
		}
	}

	PutSession(sess)
	body, err := proto.Marshal(&aiserverv1.BidiAppendResponse{})
	if err != nil {
		return errResult(http.StatusInternalServerError, "marshal response: "+err.Error())
	}
	return Result{Status: http.StatusOK, ContentType: "application/proto", Body: body}
}

// HandleBugBotBidiAppend mirrors HandleBidiAppend for BugBot's parallel bidi
// transport. Cursor sends the initial StreamBugBotRequest and later tool
// results through the same BidiAppend envelope keyed by request_id.
func HandleBugBotBidiAppend(reqBody []byte, contentType string) Result {
	return HandleBidiAppend(reqBody, contentType)
}

func handleBugBotBidiAppendDecoded(bidi *aiserverv1.BidiAppendRequest) Result {
	if bidi == nil {
		return errResult(http.StatusBadRequest, "missing bugbot bidi append body")
	}

	requestID := ""
	if bidi.RequestId != nil {
		requestID = bidi.RequestId.RequestId
	}
	if requestID == "" {
		return errResult(http.StatusBadRequest, "missing request_id")
	}

	raw, err := decodeBidiPayload(bidi)
	if err != nil {
		return errResult(http.StatusBadRequest, err.Error())
	}
	return applyBugBotBidiPayload(requestID, raw)
}

func applyBugBotBidiPayload(requestID string, raw []byte) Result {
	sess := &Session{RequestID: requestID}
	if existing := GetSession(requestID); existing != nil {
		sess = existing
	}

	if len(raw) > 0 {
		acm := &aiserverv1.StreamBugBotAgenticClientMessage{}
		if err := proto.Unmarshal(raw, acm); err != nil {
			return errResult(http.StatusBadRequest, "parse bugbot client message: "+err.Error())
		}
		if start := acm.GetStart(); start != nil {
			sess.BugBotRequest = start
			sess.UserText = buildBugBotPrompt(start)
			if sess.ConversationID == "" {
				sess.ConversationID = "bugbot:" + requestID
			}
		}
		if execMsg := acm.GetExecClientMessage(); execMsg != nil {
			routeBugBotExecClientResult(bugBotToolScope(requestID), execMsg)
		}
	}

	PutSession(sess)
	body, err := proto.Marshal(&aiserverv1.BidiAppendResponse{})
	if err != nil {
		return errResult(http.StatusInternalServerError, "marshal response: "+err.Error())
	}
	return Result{Status: http.StatusOK, ContentType: "application/proto", Body: body}
}

func errResult(status int, msg string) Result {
	body := []byte(`{"code":"invalid_argument","message":` + jsonString(msg) + `}`)
	return Result{Status: status, ContentType: "application/json", Body: body}
}

// extractIntoSession pulls the bits of AgentClientMessage we care about into
// the session: the user's text, the chosen model, and the conversation
// state (so future iterations can reconstruct full chat history).
func extractIntoSession(sess *Session, acm *agentv1.AgentClientMessage) {
	if run := acm.GetRunRequest(); run != nil {
		sess.Run = run
		sess.ModelDetails = run.GetModelDetails()
		sess.RequestedModel = run.GetRequestedModel()
		sess.State = run.GetConversationState()
		if cid := run.GetConversationId(); cid != "" {
			sess.ConversationID = cid
		}
		if action := run.GetAction(); action != nil {
			sess.Action = action
			extractConversationAction(sess, action)
		}
	}
	if pre := acm.GetPrewarmRequest(); pre != nil && sess.ModelDetails == nil {
		sess.ModelDetails = pre.GetModelDetails()
	}
}

func buildBugBotPrompt(req *aiserverv1.StreamBugBotRequest) string {
	if req == nil {
		return "Review the provided git diff and find issues."
	}
	var b strings.Builder
	b.WriteString("Review the provided git diff and find concrete bugs, regressions, or correctness issues. ")
	b.WriteString("Focus on actionable findings with file and line references.\n\n")
	if instr := strings.TrimSpace(req.GetUserInstructions()); instr != "" {
		b.WriteString("User instructions:\n")
		b.WriteString(instr)
		b.WriteString("\n\n")
	}
	if guide := strings.TrimSpace(req.GetBugDetectionGuidelines()); guide != "" {
		b.WriteString("Bug detection guidelines:\n")
		b.WriteString(guide)
		b.WriteString("\n\n")
	}
	b.WriteString("Git diff:\n")
	b.WriteString(renderBugBotDiff(req.GetGitDiff()))
	if files := req.GetContextFiles(); len(files) > 0 {
		b.WriteString("\n\nContext files:\n")
		for _, f := range files {
			if f == nil {
				continue
			}
			if name := strings.TrimSpace(f.GetRelativeWorkspacePath()); name != "" {
				b.WriteString("\nFile: ")
				b.WriteString(name)
				b.WriteString("\n")
			}
			if contents := strings.TrimSpace(f.GetContents()); contents != "" {
				b.WriteString(contents)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

func renderBugBotDiff(diff *aiserverv1.GitDiff) string {
	if diff == nil || len(diff.GetDiffs()) == 0 {
		return "<empty diff>"
	}
	var b strings.Builder
	for i, fd := range diff.GetDiffs() {
		if fd == nil {
			continue
		}
		if i > 0 {
			b.WriteString("\n\n")
		}
		from := fd.GetFrom()
		to := fd.GetTo()
		if from == "" {
			from = "/dev/null"
		}
		if to == "" {
			to = "/dev/null"
		}
		b.WriteString("diff --git a/")
		b.WriteString(from)
		b.WriteString(" b/")
		b.WriteString(to)
		for _, ch := range fd.GetChunks() {
			if ch == nil {
				continue
			}
			b.WriteString("\n")
			b.WriteString(ch.GetContent())
		}
	}
	if b.Len() == 0 {
		return "<empty diff>"
	}
	return b.String()
}

// ---- Connect unary codec helpers ----

// decodeUnary unmarshals a unary Connect request body into msg, transparently
// stripping the optional 5-byte envelope and gzip layer that connect-go
// emits when Content-Type is "application/connect+proto".
func decodeUnary(body []byte, contentType string, msg proto.Message) error {
	payload := body
	if isConnectEnvelope(contentType) && len(payload) >= 5 {
		flags := payload[0]
		length := int(binary.BigEndian.Uint32(payload[1:5]))
		if length >= 0 && len(payload) >= 5+length {
			payload = payload[5 : 5+length]
			if flags&flagCompressed != 0 {
				if up, err := gunzip(payload); err == nil {
					payload = up
				} else {
					return err
				}
			}
		}
	}
	return proto.Unmarshal(payload, msg)
}

func isConnectEnvelope(contentType string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(contentType)), "connect")
}

func gunzip(b []byte) ([]byte, error) {
	return gunzipLimit(b, bidiMaxQueuedBytes)
}

func gunzipLimit(b []byte, limit int64) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if int64(len(out)) > limit {
		return nil, errors.New("decoded Connect request too large")
	}
	return out, nil
}
