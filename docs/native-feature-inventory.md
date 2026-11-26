# Native feature compatibility inventory

Inspected on 2026-11-25 against Windows Cursor 3.31.15 and repository base
`519591e23ae2c75eef4312ae574e67a987eecb87`, with the fixes in this branch.
This is a bounded protocol/source inventory, not certification of every Cursor
feature. No Cursor UI execution or external MCP server was performed by this
inventory task. See `windows-validation.md` for the separate Windows checks.

The tables below describe the older AgentService/server-loop path. The live
3.31.15 client was subsequently observed using the managed local agent loop and
`InferenceService/RunInferenceSSE`. That path carries the client's actual native
prompt and tool definitions, so it is not limited to the 14-tool list below.
See [local-inference-protocol.md](local-inference-protocol.md) for the observed
failure, wire contract, implementation and distinct transport-test evidence.

## Covered by source and deterministic tests

| Feature | Bridge behavior and evidence | Remaining live check |
| --- | --- | --- |
| Agent / Ask / Plan / Debug | Native mode enum becomes the provider system instruction. `TestNativeModesReachProviderWithContextAndMatchingTools` sends all four through a local HTTP streaming provider and receives native output frames. | Select each mode in Cursor and complete a real task. |
| Native plan buttons | `StartPlanAction` and `ExecutePlanAction` now produce a prompt and preserve their request context. Plan file content takes precedence over the embedded plan. Wire round-trip regression tests cover start, empty-text start and both execute forms. | Click Create/Build in Cursor and observe its plan panel. |
| Ask and Plan editing boundaries | File mutation tools are omitted from the advertised set and rejected before execution. Plan still has plan/todo controls; MCP calls stay delegated to Cursor's executor and permissions. | Confirm actual client interaction and permission behavior. |
| Mode switching | Mode changes only after a matching approved native interaction response. Rejection, timeout, empty and mismatched responses retain the existing mode; approved switches refresh provider tools and system instruction. | Native picker and approval UI. |
| Skills | Enabled, valid skill descriptors from request context and run-level options are listed with exact instruction-file paths. The model is told to read the file through the native Read tool before applying it. Disabled/invalid descriptors are omitted and duplicates removed. | Real skill discovery, model reading the skill, and applying its instructions. |
| Debug context | `log_path`, `server_endpoint` and the installed client's `session_id` field 3 reach the provider in Debug mode. Wire-field regression test verifies session ID is retained. | Instrument a sample program, reproduce, consume Cursor's actual debug log. |
| MCP definitions | Rich definitions, request-context tools and filesystem descriptor definitions become distinct provider functions. Native Plan actions and run-level filesystem fallback are covered, including real schema properties and stale-map removal. | Invoke a connected MCP server, handle permissions/errors, verify its output in Cursor. |
| Workspace context | Rules, selected files/selections, terminals, project tree and image data have translation paths. Missing `env` no longer discards separately supplied context. StartPlan selected files are regression-tested. | Native attachments and provider-specific multimodal support. |
| Commit generation | `WriteGitCommitMessage` has a provider-backed handler in `bugbot.go`; this inventory did not execute that RPC. | Separate handler/live test and SCM input rendering. |
| Concurrent conversations | Separate session-isolation work adds scoped wait/result ownership. This inventory does not claim subagent or cloud-worker support from that. | Two simultaneous native chats and cancellation. |

`native_context_test.go` supplies the tests named above. Existing TLS WebSocket
tests exercise transport framing, provider errors, cancellation and proxy stop.
Neither a declared protobuf type nor a passing local fake-provider test proves
the corresponding Cursor UI feature works end to end.

## Exposed tool surface

The legacy server-loop schema exposes these 14 functions, plus one synthesized
function per discovered MCP tool. Current local-loop requests instead carry
Cursor's own definitions and results through the generic inference bridge:

| Group | Functions | Execution path |
| --- | --- | --- |
| Files and shell | Shell, Read, Write, Delete, StrReplace, Glob, Grep | Native ExecServer messages and matching result wrappers. StrReplace prepares replacement content locally before native Write. |
| Plan state | CreatePlan, AddTodo, UpdateTodo | Conversation-local plan state plus native plan/todo UI messages. |
| Mode | SwitchMode | Native interaction query/verdict. |
| MCP | CallMcpTool, ListMcpResources, FetchMcpResource | Native MCP execution/resource messages. |

Ask/Plan omit Shell, Write, Delete and StrReplace. Ask omits plan mutation tools.
Resource reads are allowed; an explicit download path is rejected in those modes
because it writes a file. MCP server permissions remain Cursor's responsibility;
the bridge does not infer that every MCP tool is read-only or mutating.

## Known gaps requiring implementation or client evidence

- `AskQuestion`, semantic search, WebSearch/WebFetch, Task/subagent delegation,
  ReadLints/diagnostics, image generation, browser/computer control and stdin tools
  have declarations in the extracted protocol but are not exposed by the current
  legacy 14-function schema. Current local-loop clients can advertise their own
  tools through the generic adapter. Schema transport still does not establish
  that each client tool or its hosted service works; see the separate desktop
  results in [native-feature-validation.md](native-feature-validation.md).
- Tab's active `CppService/AvailableModels`, `CppConfig` and `StreamCpp`
  paths now have provider-backed single-edit support and TLS/client-consumer
  fixture tests. See [native-tab-protocol.md](native-tab-protocol.md) for its
  native latency limit and remaining live acceptance work.
- NameTab and WriteGitBranchName now have provider-backed unary handlers and
  real TLS transport tests, including invalid output handling. UI acceptance
  remains separate. Commit generation and Agent Review now stay native after
  successful proxy-off desktop tests. Fix All Issues stays in BYOK chat.
- Ancillary Cursor RPCs, including historical inline-edit endpoints, now pass
  through with genuine native credentials instead of a blanket local 404.
  Current client callsites and the inline-edit UI still need live verification;
  passthrough does not supply a BYOK implementation of a hosted feature.
- Native cloud background-agent requests pass through unchanged. Transport
  tests preserve credentials, bodies and responses, but do not establish native
  cloud VM, remote worktree or Task/subagent desktop completion.
- `AgentRunRequest.custom_system_prompt`, hook execution, custom subagents and
  other request-context metadata are still not fully translated. Native support
  should be implemented from their actual behavior, without treating mere field
  presence as proof that execution is supported.
- Provider quality and feature support remain separate: a provider accepting
  function schemas does not prove correct skill use, tool choice or debugging.

## Installed-artifact evidence

The installed package version is read from `resources/app/package.json`.
Inert inspection of `extensions/cursor-agent-worker/dist/main.js` found the
same SkillOptions and native plan-action fields as the extracted protobuf,
plus `DebugModeConfig.session_id = 3`, which is now in the generated Go schema.
The worker file SHA-256 is
`85bdabaa82b19bbd66bf7508cd1962d1579bcc1c112b5c7f998b3307b02e0885`.
The debug declaration occurs at character offset 518836 and skill descriptor
at 519026 in that exact file (UTF-16 indexing).

The installed `extensions/cursor-commits/dist/main.js` declares
`WriteGitCommitMessage`, `WriteGitBranchName`, and `StreamCpp` RPC names.
Its SHA-256 is
`a6aa38f7967e110e87d2daf022850d0a539cd89db0de46cfb16d75430e3ee618`.
These are static declarations, not observed network requests.

REA evidence `ev_f6a19cdfde5890ceb430f07d95d2730f35caf9ba84922da2d8a854ad98b97593`
retains the selected worker artifact analysis. It hit the 45-second per-source
deadline, so semantic/relationship coverage is explicitly partial. The followup
summary is `ev_936794442074fb7608e14e543d3ab8749475fda3df7633e26b2984e5d82154f7`.
Exact field observations above were also checked directly in the installed file;
no target JavaScript was executed. A local evidence bundle was exported outside
the repository; no user credentials or Cursor account database were inspected.

## Native-service desktop follow-up

See [native-services-20261126.md](native-services-20261126.md) for proxy-off and
BYOK-on Agent Review/commit generation, Task Models, marketplace and voice results.
See [windows-native-20261126.md](windows-native-20261126.md) for the final Windows
acceptance at `a2a8c5b`, including same-chat model switching and its limitations.
