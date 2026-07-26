# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- Restrict HTTPS interception to Cursor's own hosts and tunnel all other traffic straight through. With the system proxy enabled, the whole machine's HTTPS was previously routed through the MITM listener and intercepted with the local CA, breaking apps that pin certificates or don't trust the CA (e.g. Discord failing to log in) while the proxy was running (#4).

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
