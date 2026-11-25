# Cursor agent WebSocket transport

Issue [#12](https://github.com/Yimikami/cursor-byok/issues/12) reports a mocked
404 for `GET https://api2.cursor.sh/agent/v1/run` on Cursor 3.26.16. This is a
WebSocket upgrade, not a GET alias for the old Connect RunSSE endpoint.

## Evidence and compatibility boundary

The implementation was checked against the official macOS ARM64 Cursor 3.32.4
package downloaded on 2026-11-25 through
`https://api2.cursor.sh/updates/download/golden/darwin-arm64/cursor/3.32`.
The files below were read statically; their original code was not executed for
protocol inspection. Hashes are SHA-256:

- `extensions/cursor-always-local/dist/main.js`:
  `837ef687e08253972e9f3526a2eed3a59b967cb83ed0e96e08a479f8d10bfb56`
- `extensions/cursor-always-local/dist/254.js`:
  `f3786a72aa35132321324e727ff404b92d9c0eeef3cd86004668aa93144a9d59`

`main.js` constructs the `wss://api2.cursor.sh/agent/v1/run` URL and loads
`createAgentWebSocketRunSession` from chunk 254. That chunk contains the
protobuf field descriptions, handshake, sequence validation, run lifecycle,
and socket reuse rules summarized below. The wire numbers for the existing
`AgentClientMessage`, `AgentServerMessage`, `AgentRunRequest` (state 1, action 2,
model 3, MCP 4, conversation ID 5, requested model 9), `ConversationAction`
(user message 1), and `UserMessage` (text 1, mode 4) still match this repository.
Additional fields in the newer client are ignored by protobuf decoding.

REA static analysis was attempted. Its macOS process ownership helper failed
to compile via `xcrun` within 120 seconds, before JavaScript AST analysis. The
retained partial evidence is
`ev_ec51161c9b7ee2b21798f1f92580f56f42a29ae895a1b8106cd898c4c0bc5f66`;
it records artifact identity and incomplete coverage, not protocol semantics.
The protocol findings above come from direct inspection of the shipped files.

Cursor 3.26.16 was not independently inspected or run. The latest package's
v1 handshake and a simulated client are tested; this alone does not establish
complete desktop feature compatibility across Cursor releases.

## Wire contract

All application messages are **binary WebSocket messages containing protobuf**,
with no Connect 5-byte envelope and no HTTP gzip layer. The supported upgrade
subprotocols are `cursor-agent-v1` and `cursor-agent-preconnect-v1`.
Current local-loop clients also use `cursor-run-inference-v1` and
`cursor-run-inference-preconnect-v1` on this same URL. Their outer envelopes
are identical, but the embedded messages belong to InferenceService (below),
not AgentService.

| Client frame oneof field | Payload |
| --- | --- |
| 1 hello | protocol_version: uint32 field 1, must equal 1 |
| 2 run_start | identity field 1; repeated headers field 2 |
| 3 client_message | run_id string 1; sequence uint64 2; AgentClientMessage bytes 3 |
| 4 half_close | run_id string 1 |
| 5 cancel_run | run_id string 1 |

| Server frame oneof field | Payload |
| --- | --- |
| 1 hello | same version hello |
| 2 run_accepted | identity field 1; optional headers field 2 |
| 3 server_message | run_id string 1; sequence uint64 2; AgentServerMessage bytes 3 |
| 4 run_end | identity field 1; optional trailers field 2; optional Connect error field 3 |
| 5 run_rejected | identity field 1; Connect error field 2 |

An identity contains `run_id` string field 1 and `request_id` string field 2.
A Connect error contains numeric code field 1 and raw message string field 2
(with optional metadata/details fields 3/4). Codes use the Connect/gRPC
numbering: canceled 1, invalid argument 3, internal 13. Each direction's message
sequence starts at zero and increments once per message.

The client sends hello first and expects a v1 hello response. RunStart must
receive RunAccepted before server messages. Client messages may already be
pipelined behind RunStart. HalfClose stops further client messages without
canceling the response. CancelRun cancels upstream work and returns RunEnd.
For InferenceService, HalfClose waits for every accepted invocation to emit
its response and InvocationEnd before the final RunEnd. A disconnected socket
or explicit CancelRun still cancels those invocations immediately.

The implementation intentionally serves one run per socket. The inspected
client only reuses a socket when RunEnd includes the trailer
`x-cursor-agent-websocket-reuse: sequential-v1`; we do not advertise that
capability. Cold and preconnected sockets share the same protocol. StopProxy
cancels hijacked WebSockets as well as the HTTP listener.

## Local inference protocol

An actual Cursor 3.32.4 chat selected InferenceService rather than the older
remote AgentService. Its separately preconnected socket uses the inference
subprotocol; rejecting that upgrade caused a fallback to RunInferenceSSE.
The following schemas were read from `cursor-always-local/dist/main.js`
(`RunInferenceClientMessage`, `RunInferenceServerMessage`, and
`InferenceStreamRequest/Response`). Consumer behavior was checked in
`cursor-agent-host/dist/121.js`.

| Inner client field | Payload |
| --- | --- |
| 1 run_request | conversation_id 1; requested_model 3 |
| 2 invoke_model | invocation_id 1; full InferenceStreamRequest 2 |
| 3 cancel_invocation | invocation_id 1 |
| 4 finish_run | aborted 1; optional turn_completed 2 |
| 5 client_heartbeat | optional timings 1; acknowledged sequence 2 |

| Inner server field | Payload |
| --- | --- |
| 1 heartbeat | empty heartbeat when resumption is not offered |
| 2 run_ready | resolved_model 1; prompt_model_metadata 4; accepts_client_heartbeat 7 |
| 3 invocation_response | invocation_id 1; InferenceStreamResponse 2 |
| 4 invocation_end | invocation_id 1; optional error 2 |

Server messages have a separate sequence in field 100. The local client owns
the agent loop and executes tools; each complete invocation carries messages,
tool definitions, and previous tool results. BYOK translates those to the
configured provider and returns text/thinking deltas, completed tool calls,
usage, and final assistant message metadata. The `text_part.is_final` frame is
a finish notification; text on that frame is ignored by the client, so the
implementation emits an empty final text part after the actual deltas.

RunReady supplies a resolved model and generic supported prompt metadata.
Current mode configuration rows cover `agent`, `chat` (Ask), `plan`, `debug`,
`triage`, and `multitask`. Cursor 3.32.4 rejects a non-Agent local-loop turn
without its row; this was observed in a real Ask test. The metadata enables
native question and mode-switch discovery, while Cursor retains mode-specific
read/write restrictions and execution approvals. The native local-loop consumer
and mode string mapping were checked in agent-host chunks `121.js` and `7764.js`.
It does not advertise compressed requests, delta invocations, speculative
invocations, resumable streams, or server-assisted subagent model resolution.
Unsupported provider-defined tools and file content are reported explicitly
rather than silently dropped. Invocation cancellation, socket closure and
service stop cancel provider work.

InferenceService also supports `RunInferenceSSE` with the same session logic.
The endpoint takes `BidiRequestId` and returns compressed Connect envelopes
containing `RunInferenceServerMessage`. BidiAppend is shared by these services
and does not identify the inner message type: an opaque pending inbox is bound
to the protocol by the stream endpoint before decoding. Both older hex data
(field 1) and current binary data (field 4) are accepted. The inbox orders
concurrent POSTs by `append_seqno`, deduplicates retries, bounds pending input,
and discards queued input when a stream closes. This preserves the separate
AgentService and BugBot SSE decoders. Poll transport remains unsupported.
Detached AgentService and BugBot streams can reattach with the same request
ID and protocol, preserving the consumed append sequence watermark and the
session store's original conversation. Concurrent attachments and protocol
changes remain rejected; inference resumption is not advertised or enabled.

The shared BidiAppend endpoint also carries native Cursor requests. A
per-proxy registry separates local streams from the native chat, speech,
health and best-of-N SSE/poll endpoints found in the inspected client.
Appends received before their stream are retained with their original HTTP
body, compression and headers, within bounded memory. Once a native endpoint
claims the matching request ID and account credentials, its pending appends
are replayed to that native endpoint's upstream Bidi service in sequence.
Local BYOK requests are never mirrored upstream. Subsequent native appends
pass through normally; credential mismatches and failed replay return errors.
Queued bodies remain retained until each upstream append is acknowledged, so
stream retries can safely resend an unacknowledged sequence after transport
errors or cancellation. Overlapping retries are serialized and newly arriving
appends wait behind replay. HTTP and Connect gzip expansion are both bounded.
Stopping the proxy clears queued requests and credentials. Native polling
passes through; local BYOK polling returns an explicit `unimplemented` error
and is never forwarded to Cursor's native service.

Legacy tool-result and interaction waiters are scoped to the owning request.
WebSocket runs use a server-generated owner distinct from client-chosen wire
identities. Tool ids, execution ids and numeric ids from another run cannot
consume a waiter or an approval. Shell events arriving after a wait has been
removed are discarded before allocating an output buffer.

## Validation

`go test ./internal/mitm ./internal/agent` exercises a real local CONNECT/TLS
MITM and a mock OpenAI SSE provider, including both subprotocols, sequence and
identity checking, output protobuf decoding, terminal errors, missing provider,
cancel, disconnect, and service stop. No system trust store, system proxy,
Cursor account database, or real provider key is used by these tests.
Inference tests additionally cover both inference subprotocols, multiple
invocations on one run, local tool-call translation and invocation errors.
SSE tests run through real local TLS and cover appends arriving before attach,
out-of-order POSTs and retries, hex/binary input, old Agent/BugBot streams,
invocation cancellation, disconnected clients, service stop and errors.
Native routing tests cover original binary/gzip request preservation, account
scope, synthetic credential rejection, replay failures and queue limits. An
additional verified TLS upstream test exercises the installed proxy routing
hooks, queued native append replay, subsequent appends and scoped preflight.
