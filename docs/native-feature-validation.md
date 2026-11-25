# Native Cursor validation

Tested on macOS with the official Cursor 3.32.4 ARM64 package on 2026-11-25,
using a disposable Git workspace and the user-configured Provider A `model-a` model.
No provider keys, account databases or captured private prompts belong in this
repository. Mock integration tests use temporary certificates and loopback
servers; they do not modify OS trust, system proxies or Cursor preferences.

## Native tools

Real Cursor turns completed text generation, `Read` and `Write` (a verified
marker file), cancellation and a subsequent successful message. The current
local-loop client advertised these 20 tools:

`Shell`, `Glob`, `SearchConversations`, `Grep`, `Await`, `Read`, `Delete`,
`StrReplace`, `Write`, `EditNotebook`, `TodoWrite`, `WebSearch`, `WebFetch`,
`GenerateImage`, `XSearch`, `AskQuestion`, `Task`, `GetMcpTools`,
`FetchMcpResource`, `CallMcpTool`.

Advertisement is not proof of execution. Only the exercised tools listed here
are desktop-verified. The provider bridge preserves arbitrary function schemas,
opaque call IDs, arguments and tool results instead of restricting current
clients to the older server-loop tool list. Regression tests cover nested MCP
schemas, parallel results with images, invalid batches, truncated streams and
owner-scoped legacy tool/approval delivery.

## MCP

A project-scoped, local stdio fixture exposed one harmless echo tool and one
resource. After enabling that fixture through Cursor's native MCP settings and
approving its single tool invocation, a real Cursor turn completed:

- `GetMcpTools` discovered the server.
- `CallMcpTool` returned `MCP_TOOL_OK CURSOR_NATIVE_MCP`.
- `FetchMcpResource` returned `MCP_RESOURCE_OK`.

An initially disabled server correctly appeared unavailable. Existing unrelated
MCP integrations were not invoked or granted additional permissions.

## Skills and rules

Cursor discovered `.cursor/skills/byok-smoke-test/SKILL.md`. Invoking its native
slash command produced the exact `SKILL_SMOKE_OK` response and a successful
turn in Cursor's transcript. A separate project-rule check was combined with
Ask mode; that turn stopped before inference on the missing mode metadata
described below, so rule application is not yet desktop-verified.

## Ask, Plan, Debug and Multitask

The real Ask attempt exposed a compatibility error: current Cursor requires
per-mode tool decisions in `RunReady`. The bridge now provides the inspected
wire IDs (`agent`, `chat` for Ask, `plan`, `debug`, `triage`, `multitask`), native
question configuration and switch-mode targets. A regression test decodes the
actual response and verifies the required rows and wire identifiers. Cursor's
own mode restrictions and execution approval logic remain responsible for
read-only behavior and tool execution.

The Mac locked before the corrected Ask turn, Plan, Debug and Multitask could
be exercised in the desktop. These are **implemented but awaiting desktop
retest**, not claimed as passing. Two separate chat sessions were started while
another turn was in progress, but this does not establish subagent or
Multitask-mode completion.

## Commit generation, branch names and chat titles

Native protobuf requests were sent through the running TLS proxy to the real
configured provider, using only a scratch repository diff:

| Native RPC | Real result |
| --- | --- |
| `WriteGitCommitMessage` | HTTP 200; valid commit message, 33.1 seconds |
| `WriteGitBranchName` | HTTP 200; valid branch name, 11.9 seconds |
| `NameTab` | Initial response was too long; after fixing the prompt and validation, HTTP 200 with 7 words / 62 characters, 18.2 seconds |

These are real provider/RPC checks. Commit and branch creation were not
performed. The corresponding desktop buttons still need independent checks.

## Native account, networking and transport

- The real account loaded in Cursor; access/refresh/email/sign-in values were
  unchanged. New sessions no longer fabricate identity or subscription status.
- The selected model initially reverted to Auto after a native default refresh.
  `GetDefaultModel` now returns the configured BYOK identity; this was decoded
  successfully through a real HTTP/2-only connection to the running proxy.
- Native metadata requests pass through with original credentials and verified
  upstream TLS. Authentication hosts and unrelated HTTPS are tunneled.
- Cloud background-agent followup, status, attach and interaction RPCs also
  pass through unchanged. Mock upstream integration tests verify their request
  credentials, payloads and HTTP 200 response bodies are preserved. They are
  not handled by a local cloud-agent simulation.
- Local model pinning and new preference backups leave cloud background-agent
  selections untouched, including changes made during a BYOK session. Legacy
  snapshots remain readable to restore earlier overrides. The local model
  catalog does not advertise a BYOK ID as a cloud-agent default.
- On one real HTTP/2-only session, `/extensions-control` returned HTTP 200 and
  matched a direct verified vendor response byte for byte. An unauthenticated
  profile request retained the genuine HTTP 401 response.
- Ordinary GitHub HTTPS through the active proxy returned HTTP 200 with
  certificate verification result 0. Cursor strict SSL validation stayed enabled.
- Paid local RPCs now require the process owner's current Cursor access token.
  WebSockets validate native run-start or upgrade authorization before accepting
  a run; account refresh and sign-out take effect without restarting the proxy.
  This post-desktop-test change has mock transport and read-only SQLite coverage;
  it still needs a real desktop retest when the Mac is unlocked.
- HTTP request bodies and their gzip expansion are bounded independently.
- Model response rewrites preserve Connect data/trailer framing and both frame
  and HTTP gzip handling. CONNECT tunnels ignore inherited proxy environment
  settings, preventing recursion back into the local listener.
- Preference snapshots publish atomically. Successful model restoration is
  recorded in the same SQLite transaction, so a failed sidecar deletion cannot
  replay stale choices over a later user edit. Pending non-routing recovery is
  tracked separately from whether BYOK routing is active.
- A Linux CI timing failure exposed lost read-ahead bytes during the WebSocket
  upgrade. A deterministic byte-level regression now covers that fix and
  upgraded-connection cleanup. Half-close drains active provider responses;
  legacy SSE retries retain their input sequence and conversation identity.
- Integration tests cover HTTP/1.1 WebSocket, HTTP/2 multiplexing and gRPC
  trailers, current inference SSE, legacy SSE, native shared-Bidi replay,
  cancellation, disconnect, shutdown and authority checks.

Shared Bidi inputs are not mirrored upstream. They are scoped to the matching
local or native stream before delivery. BYOK supports WebSocket and SSE;
unsupported local Poll endpoints return an explicit error. Native Poll endpoints
from the inspected client pass through.

## Remaining compatibility boundaries

Native Tab, inline edit/Cmd-K, cloud agents, indexing, browser/image/search
services and other account-hosted features are not all desktop-verified. Native
services retain their normal account, provider and subscription requirements;
passthrough does not turn a server-hosted feature into a local BYOK feature.

Anthropic signed and redacted thinking blocks now round-trip through Cursor's
native content fields with their original order and matching tool results; a
two-invocation mock-provider test covers both native-content and reasoning-part
history. This path has not been tested against a live Anthropic account.

Provider-defined tools, arbitrary file content parts, other providers' opaque
reasoning replay and every sampling option are not implemented in the adapter.
Current local-loop usage is returned to Cursor but is not yet persisted in the
application's legacy usage dashboard. These limits must not be described as
complete native parity.

## Repeatable checks

```sh
go test -p 2 ./...
go test -race -p 2 ./internal/agent ./internal/mitm ./internal/cursor ./internal/bridge
cd frontend && npm run build
```

All commands passed on the tested macOS tree. Windows/macOS/Linux CI supplies
separate build/test coverage; only tests actually run on Windows establish
Windows device behavior.
