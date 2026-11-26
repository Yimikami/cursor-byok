# Native services with BYOK enabled — 2026-11-26

Cursor 3.32.4, macOS arm64. These checks follow the Windows acceptance at
`a2a8c5b` in [windows-native-20261126.md](windows-native-20261126.md).
The routing changes described here were made afterward; the Windows desktop
report must not be presented as a test of this later commit.

## Routing decision

Agent Review and SCM commit-message generation stay on Cursor's native service.
The review SSE/poll endpoints and their shared BidiAppend input are forwarded
with genuine native credentials. Fix All Issues prepares a normal chat prompt,
so its coding run still uses the selected BYOK model. Chat titles and branch
names retain their existing BYOK routing.

## Observed desktop results

| Check | Result and scope |
| --- | --- |
| Native Agent Review, proxy off | A disposable Git fixture deliberately changed multiplication to addition. Review found the correct issue at pricing.py:9–10 in 43.56 s. Structured logs identify api2.cursor.sh / StreamBugBotAgentic and successful stream completion. |
| Fix All Issues, proxy off | The button prepared a chat prompt; native Agent fixed the actual expression. Unchanged fixture tests printed REVIEW_FIXTURE_OK. This establishes the button's ordinary-chat behavior, not a separate fix API. |
| Native commit generation | The actual SCM button generated an appropriate message with proxy off and again with BYOK on. No source-control commit was performed by the button test. Standard logs did not expose the commit RPC; do not infer an observed endpoint from the UI alone. |
| Native Agent Review, BYOK on | Review of the corrected fixture returned No issues found (63.45 s). The native api2.cursor.sh / StreamBugBotAgentic stream completed successfully. |
| Task Models | Settings → Models → Task Models → Explore Subagent Model listed both configured BYOK models. Selecting model-b updated the field; reset to default afterward. No extra catalog patch was needed. This is selection acceptance; earlier platform reports cover actual Task/Await runs. |
| Marketplace | Customize loaded the native marketplace with 211 entries and install controls. Existing installed plugins were accessible. No new plugin was installed and this is not an end-to-end certification of every plugin. |
| Voice chat with BYOK | User confirmed two-way voice with model-b selected. A spoken request added an assertion to a real test file; independent python3 execution passed. A following voice request ran tests and reported no lint errors. |

The BYOK-on helper was built from the same production routing sources, using a
test-only overlay to discard ordinary logs and disable message-body history.
It does not alter routes, credentials, selection, provider payloads or response
bytes. No outgoing provider-body or voice-WebSocket capture was made in this
run. Earlier A→B→A evidence separately verifies provider/model switching.

## Voice architecture and limits

Read-only REA analysis of the installed bundle, with direct source checks,
shows native MintRealtimeVoiceSecret obtaining a session credential and settings.
The client permits voice WebSockets under wss://api.x.ai/, sends 24 kHz PCM audio,
and uses server voice activity detection. The selected coding model is separate:
voice send_task ultimately submits an ordinary message to the existing agent
with sentByVoiceCall set, honoring queue/steer behavior. Read-only voice helpers
can inspect diffs, command results and worker status; file operations and new
commands go through the coding agent.

The daily refusal is identified by server metadata voice_mint_limit=daily.
No numeric daily allowance was found in the inspected client path or mint
success fields. The server may control the value; plan differences and reset
time are unverified. idleSeconds is a separate idle timeout. The official
[voice documentation](https://cursor.com/docs/agent/voice-chat) confirms native model
Voice, preservation of the chosen coding model, the IDE's first-message
requirement, a daily cap without a published number, and an approximately
10-minute idle timeout. BYOK does not remove the native voice quota.

Installed workbench SHA-256:
`7790e8debe902d9edce3948607f486d870c43d9f565ad6d658c2783309666072`.
REA analyzed exact source slices without executing them: 5 connection-related
expressions (61,957 bytes), then 9 handoff/quota expressions (39,900 bytes), both
with zero parse errors and no node-ceiling truncation. Dynamic relationships
and external dependencies remain partial; this is not a complete runtime trace.
Evidence parents: `ev_8b11a0b0c14c14e210642319cf386b5dea1a9fb2ea8ef2397df8439400c3b428`
and `ev_c57ee55be53fbbf3478141243cfb27cc6d4507a760e861d523d4a8eaa8caa729`.
No Cursor executable patch or voice-provider substitution was needed.

## Automated validation

All backend package tests and go vet passed. Frontend regression tests (3/3),
Vue/TypeScript production build and git diff --check passed. Native transport
regressions preserve commit/review/marketplace requests and exercise review
SSE/poll plus queued and late BidiAppend replay through verified TLS.
