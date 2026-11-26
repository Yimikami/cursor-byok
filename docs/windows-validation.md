# Windows and native Cursor validation

For the later genuine-account Cursor 3.32.4 desktop checks and the historical
Plan failure, see [the follow-up report](windows-native-desktop-followup.md).
The shared CreatePlan wrapper fix and native picker subsequently passed Windows
Cursor 3.32.4 acceptance at `a2a8c5b`; see [the later Windows results](windows-native-20261126.md).
The historical failures below are retained to explain the fixes, not as current status.

Updated: 2026-11-26. Live UI checks used Windows Cursor 3.31.15; the installation
later updated to 3.32.4 before the final pure-consumer checks. Initial repository baseline
`519591e23ae2c75eef4312ae574e67a987eecb87`.

## Verified on the Windows host

- Frontend production build, Go tests, vet and Windows production compilation.
- 35 checks in an isolated Cursor profile using the real CurrentUser Root
  certificate store and WinINET proxy configuration: reject untrusted CA,
  missing adapter and occupied port without changing proxy state; start/stop;
  restore exact registry values and original Cursor settings/auth; recover a
  deliberately interrupted session and consume its recovery files.
- Public HTTPS during the proxy session retained genuine upstream certificates
  and valid TLS. Cursor's synthetic model endpoint used the expected local CA.
- Temporary certificate from the isolated-profile lifecycle test was removed.
- The actual Cursor model picker displayed the configured Letum adapter.
- Real Cursor traffic reproduced a fatal nil-URL panic in the CONNECT request
  hook. HTTP/2 `PRI *` and `OPTIONS *` can reach that hook with no URL after
  goproxy's URL reconstruction fails. The new wire-level regression reproduces
  both and verifies a subsequent HTTPS request still succeeds. With the fix,
  real Cursor traffic no longer killed the listener in the observed session.

## Live provider checks

The explicitly configured Cortex Letum provider passed
`TestLiveProviderNativeProtocol` on this host, repeated on the combined source
on 2026-11-26 (38.73 seconds total):

| Check | Result | Evidence boundary |
| --- | --- | --- |
| Streaming inference probe | Passed, 5.64 s | Real provider; same streaming transport as the agent |
| Commit generation | Passed, 12.99 s | Real provider through TLS proxy and native protobuf RPC; SCM button not covered |
| WebSocket Read roundtrip | Passed, 20.00 s | Real provider, TLS CONNECT, WebSocket v1, native Read messages and returned file token; executor was a bounded test fixture |

A separate run using the dashboard's current basic probe options (512 output
tokens, no forced reasoning option) also passed in 18.06 seconds. Full Go tests,
vet, frontend production build and Windows production compilation passed on the
combined source. Tests using an explicit fixture authorizer do not establish
desktop acceptance of the production account guard.

These tests are opt-in with `CURSOR_BYOK_LIVE_CONFIG` and spend provider tokens.
They read credentials from the supplied local config, use temporary client-side
CA trust, and do not change the OS proxy or the installed Cursor account.
Never commit a provider config, API key, auth database, or unredacted capture.

## Real UI finding and current work

The Cursor Agents UI initially failed a harmless Read request four times with a
server error. Its network and agent-host logs identify an automatically updated
managed harness (`5b1496a9b2a2`) running the local agent loop. It calls
`/aiserver.v1.InferenceService/RunInferenceSSE`; the older implementation returned
404. Passing the legacy AgentService WebSocket test therefore did not establish
current native UI compatibility.

The local-loop inference protocol is implemented from installed wire
declarations, including the required prompt/model/mode metadata. In that path
Cursor owns tool execution, permissions, skill loading, mode handling and local
task scheduling; BYOK supplies model inference. Tests execute the installed
client's extracted pure metadata guards in an isolated VM and cover TLS framing,
binary/hex Bidi input, tools, history, cancellation and signed reasoning replay.

In a separate Cursor user-data directory, the real UI completed a file search
and Read of a temporary marker file and returned the expected contents. The
configured local skill's response marker was also observed. Native MCP discovery
then failed on an upstream metadata 404. A tool call also outlived the old
30-second bridge timeout while still waiting for native approval. Both causes
have code fixes and regression coverage, but Windows desktop acceptance remains
pending. This is not an all-features certificate.

The live run also exposed Cursor's new safeStorage token migration: comparing
only plaintext would discard an auth backup after Cursor encrypted the injected
token. Windows ownership checks now decrypt the envelope in memory using the
existing profile key and DPAPI with UI forbidden. Original backup values retain
their original representation. Unreadable/unsupported envelopes keep the backup
and return an error. The real installed token format was verified read-only;
fixture tests cover encrypted restoration, a later legitimate login and bad tags.
Non-Windows encrypted ownership currently fails closed and retains recovery data.

The first Windows live test did lose its original auth recovery record before
that guard existed. The temporary test identity remained; the saved baseline
contains hashes only and does not restore credentials. Original sign-in recovery
is still pending. The identified temporary fields were subsequently removed by
the new ownership-checked migration. The real login page opened, but the native
browser automation connection failed twice after a runtime reset. No successful
sign-in was observed. Settings match the pre-test baseline, SQLite quick-check is
OK, and the original disabled system-proxy state is restored.
Removing the separate live-test CA trust required an interactive Windows root
store security confirmation. The command was canceled without changing trust;
that certificate remains installed. The isolated lifecycle fixture's certificate
was removed as noted above. No proxy listener or live-test helper remains active.
This incident must not be described as a successful real-account
restore. The 35 isolated lifecycle checks above used disposable fixture data.

The combined macOS/Windows implementation preserves genuine account identity
instead of injecting a temporary account into new sessions. Model-picker
preferences have a separate profile-scoped recovery record; unrelated and
cloud-agent preferences must survive restoration. See
[native-feature-validation.md](native-feature-validation.md) for independent
macOS desktop evidence. The older Cursor 3.26.16 reported in issue #12 still
requires a version-specific desktop check.

An isolated Git workspace contains a read-only JSON-RPC MCP fixture, one skill,
a marker file and a small debugging fixture. These are local test inputs, not
production user projects. No external MCP account or service was changed.

## Regression coverage added

- All ten legacy native execution tools: Shell, Read, Write, Delete, StrReplace,
  Glob, Grep, CallMcpTool, FetchMcpResource and ListMcpResources, including their
  request wire fields and completion/result delivery. Dynamic MCP preserves
  schemas and server routing.
- Four modes at the provider boundary; native StartPlan/ExecutePlan actions;
  skills/debug metadata; mode switch approval/rejection/timeout behavior.
- Cross-conversation/transport result isolation, late shell chunks, cancellation
  and concurrent retirement. Linux CI additionally runs the race detector;
  Windows race execution requires an unavailable C compiler on this host.
- JSONC comments/BOM/line endings and unrelated edits; atomic auth changes;
  stale backup safety; startup rollback ordering.
- OpenAI reasoning and Anthropic signed/redacted reasoning across native tool
  turns; consecutive tool results precede their image attachments.
- Native NameTab and WriteGitBranchName generation through TLS and a streaming
  provider fixture, including rejection of invalid/empty branch output.
- Shutdown closes speculative TCP and hijacked CONNECT sockets, cancels active
  provider requests and releases the listener even after a graceful deadline.
- Native Tab model list/config and single-line prediction protocol; see
  [native-tab-protocol.md](native-tab-protocol.md) for the latency boundary.
- macOS stable service identity resolution for rename/deletion, plus a read-only
  framework inventory test for macOS CI. Actual Mac proxy mutation is not
  established by Windows cross-compilation.

See [native-feature-inventory.md](native-feature-inventory.md) for the bounded
feature inventory, installed-artifact evidence and remaining unsupported RPCs.

## Historical acceptance checklist

The seven requested Windows controls, including native picker and CreatePlan/Build,
subsequently passed at `a2a8c5b`: [acceptance report](windows-native-20261126.md).
The checklist below describes what was outstanding when this earlier report was written.

After normal Cursor sign-in and a working desktop automation connection, repeat
the local MCP tool/resource and explicit skill command, then Ask, Plan/Create and
Build, Debug/reproduction, simultaneous conversations and Multitask. Exercise the
SCM generation button, Tab, inline edit, the remaining native tools, and hosted
services separately. Mock transport coverage and the independent macOS MCP/skill
results do not substitute for these Windows desktop checks. The latest combined
account-authentication and transport guards also need a real desktop retest.
