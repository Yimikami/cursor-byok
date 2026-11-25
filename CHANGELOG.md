# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- Restore the native model selector for configured BYOK adapters on macOS and Windows using Cursor's own-key mode and named-model view, with exact preference restoration and protection for disabled saved native keys.
- Respect native schema wrappers, tuple/pattern constraints and repaired Anthropic tool history when normalizing provider arguments; fix real CreatePlan todo-array failures.
- Require the exact macOS CA SSL trust record before reporting readiness, and remove only trust domains containing that certificate.
- Keep OpenAI alternative choices separate so text, reasoning, tool calls and finish markers from another choice cannot corrupt the selected response.

- Decode macOS Cursor safeStorage sessions for local account ownership checks; validate account access before changing proxy settings and keep Keychain permission requests confined to an explicit service start.
- Preserve Anthropic signed/redacted thinking and OpenAI reasoning through native tool replay; reconcile output caps and report truncated replies as errors.
- Publish preference backups atomically, protect restored settings from stale recovery replay, and resolve macOS network services by stable identity.
- Require the current local Cursor account for paid BYOK requests and reject unrelated browser origins. Bound request bodies and gzip expansion.
- Preserve WebSocket upgrade read-ahead bytes, close upgraded tunnels, drain half-closed inference runs, and keep legacy SSE retries attached to their own conversation.
- Ignore inherited CONNECT proxy settings to prevent self-proxy loops; preserve Connect framing and trailers in rewritten model responses.
- Keep native tool approvals pending until Cursor responds or the run is canceled, instead of manufacturing incomplete results after 30 seconds.
- Recognize Windows safeStorage-wrapped legacy temporary sessions in memory and retain recovery data when ownership cannot be established.
- Support native Tab single-line predictions with bounded latency and disable incompatible thinking budgets for short completions.
- Support Cursor 3.32.4 local-loop inference over WebSocket and SSE, including native tool schemas, streamed tool calls, cancellation and provider errors.
- Preserve real Cursor sign-in and forward native account, update and extension requests with verified TLS; remove synthetic account creation and blanket auxiliary 404 responses.
- Preserve JSONC comments and unrelated edits, scope legacy tool results to their owning run, and stop allocating buffers for late shell events.
- Generate native chat titles and Git branch names through the selected BYOK provider; reject empty or truncated commit/review output.
- Keep the selected BYOK model when Cursor refreshes defaults, and supply the per-mode tool metadata required by current Plan/Ask/Debug/Multitask clients.
- Restore Cursor's previous model preferences on Stop, failed startup, and crash recovery. Validate account backups and restore account keys transactionally.
- Prevent an HTTP/2 connection preface from crashing the proxy during current Cursor startup; retain HTTP/1.1 WebSocket compatibility and route HTTP/2-only native streams through the same handlers, preserving gRPC trailers.
- Support Cursor's `/agent/v1/run` WebSocket v1 transport alongside the legacy SSE transport; handle the handshake, run lifecycle, ordered agent messages, cancellation, and terminal errors locally (#12).
- Verify the actual local CA certificate instead of accepting any certificate with the same name. Preserve damaged/incomplete CA files for recovery and install macOS trust in the current user's trust domain.
- Check service prerequisites before changing the system proxy, roll back failed setup, retain a reachable listener when restoration fails, and recover saved settings after an interrupted session.
- Preserve Windows proxy values and their absence, report backup failures, restore partial changes, and leave unrelated proxy settings untouched when no recovery record exists.
- Accept comments and trailing commas in Cursor's JSONC settings.

### Added

- Regression coverage for certificate identity, proxy failure recovery, JSONC settings, and TLS WebSocket agent streaming. CI now runs the Go test suite on Windows, macOS, and Linux.

## [1.0.2] - 2026-07-26

### Fixed

- Restrict HTTPS interception to Cursor's own hosts and tunnel all other traffic straight through. With the system proxy enabled, the whole machine's HTTPS was previously routed through the MITM listener and intercepted with the local CA, breaking apps that pin certificates or don't trust the CA (e.g. Discord failing to log in) while the proxy was running (#4).
- Release workflow now checks out the exact tag on manual (`workflow_dispatch`) re-runs, so re-published artifacts match the tag instead of the branch the workflow was launched from.

### Added

- Bug-report and feature-request issue templates under `.github/ISSUE_TEMPLATE/`.

## [1.0.1] - 2026-04-22

### Fixed

- Decoupled the default active model from adapter order so Cursor no longer keeps routing new chats to whichever adapter happens to be listed first.
- Added an explicit default active model setting in the dashboard and use it for Cursor model pinning, synthetic model defaults, and fallback request routing.
- Clear specialized/default model selections when their adapter is deleted so config state stays valid.

## [1.0.0] - 2026-04-22

Initial public release of `cursor-byok`.

### Added

- Local desktop app that turns Cursor IDE into a Bring-Your-Own-Key client.
- On-device HTTPS MITM proxy for the Cursor RPC paths needed by chat, agent, model listing, and default-model hints.
- Synthetic local Pro session injection so Cursor's model picker and agent UI can run without a Cursor account.
- OpenAI-compatible and Anthropic provider support with configurable base URL, API key, and model selection.
- Full agent loop support including tool calls, shell/file tools, MCP tools, plan mode, and streamed responses.
- Wails 3 desktop dashboard with Overview, Models, Stats, and Editor views plus system tray controls.
- Cross-platform Cursor/proxy integration for Windows, macOS, and Linux, including local CA installation flows.
- Per-conversation history and usage stats persisted locally under the app config directory.
