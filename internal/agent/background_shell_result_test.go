package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	agentv1 "cursor-byok/internal/protocodec/gen/agent/v1"
	aiserverv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
	"google.golang.org/protobuf/proto"
)

func backgroundShellFinal(t *testing.T, env *toolResultEnvelope) *aiserverv1.ClientSideToolV2Result {
	t.Helper()
	result := buildBackgroundComposerFinalResult(aiserverv1.ClientSideToolV2_CLIENT_SIDE_TOOL_V2_RUN_TERMINAL_COMMAND_V2, PendingToolCall{ID: "shell-call", Name: "Shell"}, env)
	// Check the wire-visible result rather than only the in-memory mapper.
	data, err := proto.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	decoded := &aiserverv1.ClientSideToolV2Result{}
	if err := proto.Unmarshal(data, decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestBackgroundShellTerminalAcknowledgmentsDoNotClaimSuccess(t *testing.T) {
	pid := uint32(12345)
	for _, tt := range []struct {
		name   string
		stream *agentv1.ShellStream
	}{
		{"rejected", &agentv1.ShellStream{Event: &agentv1.ShellStream_Rejected{Rejected: &agentv1.ShellRejected{Reason: "user said no"}}}},
		{"permission denied", &agentv1.ShellStream{Event: &agentv1.ShellStream_PermissionDenied{PermissionDenied: &agentv1.ShellPermissionDenied{Error: "sandbox denied"}}}},
		{"backgrounded", &agentv1.ShellStream{Event: &agentv1.ShellStream_Backgrounded{Backgrounded: &agentv1.ShellStreamBackgrounded{ShellId: 71, Pid: &pid, WorkingDirectory: "/workspace"}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scope := bidiToolScope(t.Name())
			ch := registerToolWait(scope, "shell-call")
			defer cancelToolWait(ch)
			registerExecIDAlias(scope, "shell-exec", 77, "shell-call")
			msg := &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: &agentv1.ExecClientMessage{Id: 77, ExecId: "shell-exec", Message: &agentv1.ExecClientMessage_ShellStream{ShellStream: tt.stream}}}}
			routeExecClientResult(scope, msg)
			env := waitForToolResultContext(context.Background(), ch, time.Second)
			if env.Error != "" {
				t.Fatal(env.Error)
			}
			final := backgroundShellFinal(t, env)
			terminal := final.GetRunTerminalCommandV2Result()
			if terminal == nil || terminal.ExitCodeV2 != nil || terminal.GetEndedReason() == aiserverv1.RunTerminalCommandEndedReason_RUN_TERMINAL_COMMAND_ENDED_REASON_EXECUTION_COMPLETED {
				t.Fatalf("acknowledgment claimed an exit: %v", final)
			}
			if tt.name == "backgrounded" {
				if final.Error != nil || !terminal.GetIsRunningInBackground() || !terminal.GetPoppedOutIntoBackground() || terminal.GetBackgroundShellId() != 71 || terminal.GetResultingWorkingDirectory() != "/workspace" || !strings.Contains(terminal.GetOutputRaw(), "12345") {
					t.Fatalf("lost background state: %v", final)
				}
			} else if final.Error == nil || terminal.GetNotInterrupted() {
				t.Fatalf("refusal was not reported: %v", final)
			}
			if tt.name == "rejected" && !terminal.GetRejected() {
				t.Fatal("user rejection flag missing")
			}
		})
	}
}

func TestBackgroundShellDirectResultsPreserveOutcome(t *testing.T) {
	abort := agentv1.ShellAbortReason_SHELL_ABORT_REASON_USER_ABORT
	background := true
	shellID := uint32(25)
	for _, tt := range []struct {
		name      string
		shell     *agentv1.ShellResult
		wantCode  *int32
		wantError bool
		reason    aiserverv1.RunTerminalCommandEndedReason
	}{
		{"success", &agentv1.ShellResult{Result: &agentv1.ShellResult_Success{Success: &agentv1.ShellSuccess{Stdout: "done", WorkingDirectory: "/work"}}}, int32Ptr(0), false, aiserverv1.RunTerminalCommandEndedReason_RUN_TERMINAL_COMMAND_ENDED_REASON_EXECUTION_COMPLETED},
		{"failure", &agentv1.ShellResult{Result: &agentv1.ShellResult_Failure{Failure: &agentv1.ShellFailure{ExitCode: 7, Stderr: "failed"}}}, int32Ptr(7), false, aiserverv1.RunTerminalCommandEndedReason_RUN_TERMINAL_COMMAND_ENDED_REASON_EXECUTION_FAILED},
		{"aborted", &agentv1.ShellResult{Result: &agentv1.ShellResult_Failure{Failure: &agentv1.ShellFailure{ExitCode: 130, Aborted: true, AbortReason: &abort}}}, int32Ptr(130), false, aiserverv1.RunTerminalCommandEndedReason_RUN_TERMINAL_COMMAND_ENDED_REASON_EXECUTION_ABORTED},
		{"rejected", &agentv1.ShellResult{Result: &agentv1.ShellResult_Rejected{Rejected: &agentv1.ShellRejected{Reason: "no"}}}, nil, true, aiserverv1.RunTerminalCommandEndedReason_RUN_TERMINAL_COMMAND_ENDED_REASON_EXECUTION_ABORTED},
		{"denied", &agentv1.ShellResult{Result: &agentv1.ShellResult_PermissionDenied{PermissionDenied: &agentv1.ShellPermissionDenied{Error: "denied"}}}, nil, true, aiserverv1.RunTerminalCommandEndedReason_RUN_TERMINAL_COMMAND_ENDED_REASON_EXECUTION_FAILED},
		{"spawn error", &agentv1.ShellResult{Result: &agentv1.ShellResult_SpawnError{SpawnError: &agentv1.ShellSpawnError{Error: "not found"}}}, nil, true, aiserverv1.RunTerminalCommandEndedReason_RUN_TERMINAL_COMMAND_ENDED_REASON_EXECUTION_FAILED},
		{"timeout", &agentv1.ShellResult{Result: &agentv1.ShellResult_Timeout{Timeout: &agentv1.ShellTimeout{TimeoutMs: 1000}}}, nil, true, aiserverv1.RunTerminalCommandEndedReason_RUN_TERMINAL_COMMAND_ENDED_REASON_IDLE_TIMEOUT},
		{"background", &agentv1.ShellResult{IsBackground: &background, Result: &agentv1.ShellResult_Success{Success: &agentv1.ShellSuccess{ShellId: &shellID}}}, nil, false, aiserverv1.RunTerminalCommandEndedReason_RUN_TERMINAL_COMMAND_ENDED_REASON_UNSPECIFIED},
		{"missing outcome", &agentv1.ShellResult{}, nil, true, aiserverv1.RunTerminalCommandEndedReason_RUN_TERMINAL_COMMAND_ENDED_REASON_UNSPECIFIED},
	} {
		t.Run(tt.name, func(t *testing.T) {
			exec := &agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_ShellResult{ShellResult: tt.shell}}
			raw, _ := json.Marshal(exec)
			final := backgroundShellFinal(t, &toolResultEnvelope{ExecClient: exec, ResultJSON: string(raw)})
			terminal := final.GetRunTerminalCommandV2Result()
			if (final.Error != nil) != tt.wantError || terminal.GetEndedReason() != tt.reason || (terminal.ExitCodeV2 == nil) != (tt.wantCode == nil) {
				t.Fatalf("unexpected native outcome: %v", final)
			}
			if tt.wantCode != nil && terminal.GetExitCodeV2() != *tt.wantCode {
				t.Fatalf("exit code=%d want=%d", terminal.GetExitCodeV2(), *tt.wantCode)
			}
			if tt.name == "aborted" && terminal.GetNotInterrupted() {
				t.Fatal("aborted command reported as uninterrupted")
			}
			if tt.name == "background" && (!terminal.GetIsRunningInBackground() || terminal.GetBackgroundShellId() != shellID) {
				t.Fatal("direct background result lost state")
			}
		})
	}
}

func TestBackgroundShellStreamExitPreservesFailureAndAbort(t *testing.T) {
	for _, code := range []uint32{0, 9} {
		final := backgroundShellFinal(t, &toolResultEnvelope{ShellAccum: &shellAccumState{Exited: true, ExitCode: code, Stdout: []byte("output"), Cwd: "/work"}})
		terminal := final.GetRunTerminalCommandV2Result()
		if terminal.ExitCodeV2 == nil || terminal.GetExitCodeV2() != int32(code) || terminal.GetOutput() != "output" || terminal.GetResultingWorkingDirectory() != "/work" {
			t.Fatalf("lost streamed exit: %v", final)
		}
		if code != 0 && terminal.GetEndedReason() != aiserverv1.RunTerminalCommandEndedReason_RUN_TERMINAL_COMMAND_ENDED_REASON_EXECUTION_FAILED {
			t.Fatal("nonzero streamed exit marked successful")
		}
	}
	scope := bidiToolScope(t.Name())
	ch := registerToolWait(scope, "aborted-shell")
	defer cancelToolWait(ch)
	registerExecIDAlias(scope, "aborted-exec", 88, "aborted-shell")
	routeExecClientResult(scope, &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ExecClientMessage{
		ExecClientMessage: &agentv1.ExecClientMessage{Id: 88, ExecId: "aborted-exec", Message: &agentv1.ExecClientMessage_ShellStream{ShellStream: &agentv1.ShellStream{Event: &agentv1.ShellStream_Exit{Exit: &agentv1.ShellStreamExit{Aborted: true}}}}},
	}})
	final := backgroundShellFinal(t, waitForToolResultContext(context.Background(), ch, time.Second))
	if terminal := final.GetRunTerminalCommandV2Result(); terminal.GetNotInterrupted() || terminal.GetEndedReason() != aiserverv1.RunTerminalCommandEndedReason_RUN_TERMINAL_COMMAND_ENDED_REASON_EXECUTION_ABORTED {
		t.Fatalf("aborted zero exit marked successful: %v", final)
	}
}
