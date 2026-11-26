package agent

import (
	"context"
	"io"
	"strings"
	"testing"

	aiserverv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
)

func TestBugBotDistinguishesNoIssuesFromInvalidOutput(t *testing.T) {
	for _, answer := range []string{"NO_ISSUES", "[]", "```json\n[]\n```", `{"bugs":[],"summary":"No defects"}`} {
		bugs, summary, err := parseBugBotResponse(answer)
		if err != nil || len(bugs) != 0 || summary != "No issues found" {
			t.Errorf("valid empty review %q: bugs=%v summary=%s err=%v", answer, bugs, summary, err)
		}
	}
	for _, answer := range []string{"", "I could not review this code", "{", "null", "{}", `[{}]`, `{"bugs":null}`} {
		if _, _, err := parseBugBotResponse(answer); err == nil {
			t.Errorf("invalid review reported as success: %q", answer)
		}
	}
	bugs, _, err := parseBugBotResponse(`[{"title":"Wrong sum","description":"add subtracts b","file":"calc.py","start_line":2,"end_line":2,"severity":"high"}]`)
	if err != nil || len(bugs) != 1 || bugs[0].GetLocations()[0].GetFile() != "calc.py" {
		t.Fatalf("valid finding lost: bugs=%v err=%v", bugs, err)
	}
}

func TestBugBotContinuesAfterToolPreamble(t *testing.T) {
	rounds := 0
	stream := func(ctx context.Context, baseURL, apiKey, model string, messages []openAIMessage, tools []openAITool, opts AdapterOpts, delta openAIDelta) (*streamResult, error) {
		rounds++
		if rounds == 1 {
			_ = delta("I will inspect the change", "", false)
			return &streamResult{FinishReason: "tool_calls", ToolCalls: []PendingToolCall{{ID: "call-1", Name: "unknown_test_tool", Arguments: "{}"}}}, nil
		}
		if len(messages) != 4 || !strings.Contains(string(messages[2].Content), "inspect the change") || messages[3].Role != "tool" {
			t.Fatalf("tool history lost: %+v", messages)
		}
		_ = delta("[]", "", false)
		return &streamResult{FinishReason: "stop"}, nil
	}
	response, err := runBugBotReview(context.Background(), "review-test", &Session{RequestID: "review-test", BugBotRequest: &aiserverv1.StreamBugBotRequest{}, UserText: "Review +change"}, "", "", "model", AdapterOpts{}, stream, io.Discard)
	if err != nil || rounds != 2 || response.GetStatus().GetStatus() != aiserverv1.BugBotStatus_STATUS_DONE || len(response.GetBugReports().GetBugReports()) != 0 {
		t.Fatalf("review response=%v rounds=%d err=%v", response, rounds, err)
	}
}

func TestBugBotRejectsEmptyOrFailedProvider(t *testing.T) {
	for _, result := range []*streamResult{nil, {FinishReason: "stop"}, {SSERaw: []byte("data: {\"error\":{\"message\":\"backend unavailable\"}}\n\n")}} {
		stream := func(context.Context, string, string, string, []openAIMessage, []openAITool, AdapterOpts, openAIDelta) (*streamResult, error) {
			return result, nil
		}
		response, err := runBugBotReview(context.Background(), "review-test", &Session{BugBotRequest: &aiserverv1.StreamBugBotRequest{}}, "", "", "model", AdapterOpts{}, stream, io.Discard)
		if err == nil || response != nil {
			t.Fatalf("failed review marked done: %v err=%v", response, err)
		}
	}
}

func TestBugBotRejectsTokenLimitedReviewEvenWithValidJSON(t *testing.T) {
	for _, reason := range []string{"length", "max_tokens"} {
		for _, answer := range []string{"NO_ISSUES", "[]", `[{"title":"Partial finding","description":"More review was cut off","file":"calc.py","start_line":2,"end_line":2,"severity":"high"}]`} {
			t.Run(reason+"/"+answer, func(t *testing.T) {
				stream := func(ctx context.Context, baseURL, apiKey, model string, messages []openAIMessage, tools []openAITool, opts AdapterOpts, delta openAIDelta) (*streamResult, error) {
					_ = delta(answer, "", false)
					return &streamResult{FinishReason: reason}, nil
				}
				response, err := runBugBotReview(context.Background(), t.Name(), &Session{BugBotRequest: &aiserverv1.StreamBugBotRequest{}}, "", "", "model", AdapterOpts{}, stream, io.Discard)
				if err == nil || !strings.Contains(err.Error(), "output limit") || response != nil {
					t.Fatalf("truncated review marked complete: response=%v err=%v", response, err)
				}
			})
		}
	}
}

func TestBugBotKeepsFindingsWithQualitativeConfidence(t *testing.T) {
	for _, confidence := range []string{`"high"`, `"medium"`, `"low"`, `" HIGH "`, `null`} {
		answer := `[{"title":"Wrong subtotal","description":"subtotal subtracts quantity instead of multiplying","file":"pricing.py","start_line":11,"end_line":11,"severity":"critical","confidence":` + confidence + `}]`
		bugs, _, err := parseBugBotResponse(answer)
		if err != nil || len(bugs) != 1 || bugs[0].GetLocations()[0].GetStartLine() != 11 {
			t.Fatalf("valid finding discarded for confidence %s: %v %v", confidence, bugs, err)
		}
		if bugs[0].Confidence != nil {
			t.Fatalf("invented numeric confidence for %s: %v", confidence, bugs[0].GetConfidence())
		}
	}
	bugs, _, err := parseBugBotResponse(`[{"description":"Wrong subtotal","confidence":0.85}]`)
	if err != nil || len(bugs) != 1 || bugs[0].GetConfidence() != 0.85 {
		t.Fatalf("numeric confidence lost: %v %v", bugs, err)
	}
	for _, confidence := range []string{`true`, `2`, `-0.1`, `"unknown"`, `{}`} {
		if _, _, err := parseBugBotResponse(`[{"description":"Wrong subtotal","confidence":` + confidence + `}]`); err == nil {
			t.Fatalf("invalid confidence %s accepted", confidence)
		}
	}
}
