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

The implementation intentionally serves one run per socket. The inspected
client only reuses a socket when RunEnd includes the trailer
`x-cursor-agent-websocket-reuse: sequential-v1`; we do not advertise that
capability. Cold and preconnected sockets share the same protocol. StopProxy
cancels hijacked WebSockets as well as the HTTP listener.

## Validation

`go test ./internal/mitm ./internal/agent` exercises a real local CONNECT/TLS
MITM and a mock OpenAI SSE provider, including both subprotocols, sequence and
identity checking, output protobuf decoding, terminal errors, missing provider,
cancel, disconnect, and service stop. No system trust store, system proxy,
Cursor account database, or real provider key is used by these tests.
