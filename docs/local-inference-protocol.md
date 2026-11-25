# Cursor managed local-loop inference

Observed on Windows Cursor 3.31.15, 2026-11-25. This report distinguishes
installed-source observations, protocol fixture tests and real Cursor UI tests.
No credentials, prompt logs or account databases are included.

## Observed failure

The live Cursor Agent Host log reported `local_loop ON` and a managed harness
generation `5b1496a9b2a27ef90c3bf067f90bd1e0fe5738a656832b24860b05a5b9cbbfe3`.
The network log selected `http1_sse` under `server_force_all`; its WebSocket
cold pool did not start. The original attempt failed four times with
`http_404_unimplemented` at `RunInference`. The previous passing AgentService
WebSocket fixture exercised a different protocol and could not detect this gap.

The installed Agent Host service descriptor maps the bidi method `RunInference`
to HTTP `/aiserver.v1.InferenceService/RunInferenceSSE`. Its request is a
`BidiRequestId`. Client messages arrive through the shared
`/aiserver.v1.BidiService/BidiAppend`, whose outer envelope accepts both hex
`data = 1` and binary `data_binary = 4`.

After that route was implemented, the next live attempt reached `run_ready`
but failed before `invoke_model` with `metadata-unavailable`. A resolved model
alone satisfies the transport handshake; the downstream local prompt builder
also requires recognized prompt metadata and a row for the selected mode.

## Implemented contract

1. Cursor sends `RunInferenceClientMessage.run_request = 1` with conversation ID
   and the requested model. The server returns `run_ready = 2` with a nonempty
   `resolved_model = 1` and `prompt_model_metadata = 4`. The bridge selects the
   shipped generic `vendor = unknown`, `prompt_version = latest` profile rather
   than assigning an undocumented model family to a custom provider. Actual
   metadata field 46 carries Agent, Ask (`chat`), Plan, Debug and Multitask rows;
   field 48 names the native AskQuestion tool. The native generator applies each
   mode's editing restrictions. These interaction-tool selections are bridge
   policy; they are not claimed to reproduce remote per-account feature flags.
2. Cursor sends `invoke_model = 2` with an invocation ID and a complete
   `InferenceStreamRequest`. Native system prompts, messages and tool schemas
   are translated to the configured provider. Cursor retains responsibility
   for native tools, skills, mode behavior, approvals and conversation state.
3. Provider output becomes `invocation_response = 3`, wrapping text/thinking,
   tool calls, usage and the final `response_info`. The final response carries
   the full assistant message and tool calls; streaming deltas alone would
   leave the client's next invocation without the completed assistant history.
4. `invocation_end = 4` terminates one model call. Cursor executes requested
   tools and can send another invocation on the same run with native results.
   `finish_run = 4` on the client message closes the overall stream.
5. Invocation cancellation, stream disconnection and proxy stop cancel provider
   requests. Disconnection is detected on the next stream write/heartbeat.

Optional delta, speculative compression and resumption support are not
advertised. The bridge does not manufacture evaluated feature flags or backend
services. The schema is a wire-compatible subset; unknown protobuf fields
remain preserved by the generated runtime.

Custom tools have a distinct argument contract: their stream `args` is a JSON
encoded string, because the native stream converter calls `JSON.parse`.
The final tool-call message carries the original text as `raw_tool_call_args`
without an object-valued `args`, preserving the native string argument.

## Validation

`TestInferenceNativeLoopThroughTLSProxy` passes through a real temporary TLS
CONNECT proxy for both hex and binary BidiAppend formats. It verifies the
resolved-model handshake, a client-supplied MCP schema and skill/mode system
text, a complete tool-call stream plus final response, a second invocation
carrying the native tool result, and the final Connect end frame.

`TestInferenceProviderCancellationThroughTLSProxy` verifies explicit invocation
cancellation, closing the SSE response body and stopping the proxy. The stream
close case completed at the next 10-second heartbeat; the other cases completed
immediately. These tests use a local fake provider and temporary CA/client trust
pool; they do not modify OS proxy settings or installed Cursor state. Initial
combined run: 10.826 seconds, all five subcases passed.

Followup native reasoning tests cover two-turn OpenAI reasoning text and
Anthropic signed/redacted reasoning replay, preserving native assistant block
order. Consecutive native tool-result messages retain their required adjacency;
tool-result images are emitted after the full result group.

`TestInstalledInferencePromptGuards` additionally ran the actual installed
client's extracted `Xqr`, `fW`, `gen` and `hen` pure functions in a fresh Node VM.
It accepted the bridge profile and all five mode rows and rejected invalid
vendor/version values and missing mode rows. No application module was loaded;
the VM received no filesystem, network or process capability. This opt-in test
uses `CURSOR_BYOK_HARNESS_ARTIFACT`; the regular suite also verifies metadata
wire fields and its presence in the TLS handshake.

These results establish transport and translation behavior, not execution of a
real external MCP server or every Cursor UI tool. The parent Windows validation
report owns live Cursor/provider results and remaining feature limitations.

## Evidence and source pointers

- Managed artifact: `%APPDATA%/Cursor/agent-harness/versions/5b1496a9b2a27ef90c3bf067f90bd1e0fe5738a656832b24860b05a5b9cbbfe3/agent-harness.cjs`.
  SHA-256: `3479e57c02a341467e60b36f34e339c891b68a1fc5f7c84ff1ccc9a2cc90223d`.
- The application artifact was read as inert source; the separate opt-in test
  executes only the extracted pure prompt guards. In this
  exact artifact, UTF-16 character offsets 6556065–6559000 contain the stream
  response/final-history conversion, 6536957–6544500 contain custom-tool argument
  conversion, and 6598000 contains the resolved-model handshake requirement.
- Installed `extensions/cursor-agent-host/dist/main.js`, module 4410, declares
  `aiserver.v1.InferenceService` and its RunInference/SSE/Poll methods; the
  RunInference-to-SSE mapping occurs near character offset 2371565.
- Schemas: `proto/from_extensions/inference_v1.proto`,
  `proto/from_extensions/aiserver_v1.proto` (binary BidiAppend field).
- Runtime: `internal/agent/local_inference.go`, `internal/mitm/inference.go`.
- Tests: `internal/mitm/inference_test.go` and
  `internal/agent/local_inference_test.go`.
