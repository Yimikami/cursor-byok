# Native Tab completion contract

Windows Cursor 3.31.15, inspected 2026-11-25. This is source/fixture validation;
real Tab UI acceptance and provider latency remain separate checks.

## Active installed path

The workbench calls `CppService/AvailableModels` before `AiService/CppConfig`.
A 404 from the first call aborts configuration. The model-list response has
repeated string `models = 1` and optional string `default_model = 2`.
The bridge advertises actual configured provider models in priority order.

CppConfig directs `geoCppTransport` to `https://api2.cursor.sh`, the host
already intercepted by this bridge. It enables Tab when a model is configured,
uses 300/500 ms client/global debounce, and declares no fused cursor prediction
or multi-edit support. The unimplemented remote import classifier is disabled.

The current extension actively calls `AiService/StreamCpp`. Its current-file
cursor is zero-based, with UTF-16 columns. The response replacement range is
one-indexed and inclusive, at whole-line granularity.

The bridge asks the provider for one current-line replacement, with up to 24
surrounding lines on each side and bounded recent edit context. The replacement
may add lines; its indentation, trailing spaces and LF/CRLF are preserved.
It is returned as `model_info`, `range_to_replace`, `text`,
`done_stream`, then the Connect end frame. It does not emit `done_edit`
or `begin_edit`: those would make the non-fused native text reader throw.

When Cursor omits file contents because it expects remote filesync, the bridge
returns a Connect error with typed `aiserver.v1.ErrorDetails` and
`FILE_NOT_FOUND = 33`. The installed extension explicitly handles that
detail by retrying with full contents. No synthetic file-sync state is stored.

## Limits and verification

The installed `flushCpp` rejects streams after ten seconds. This bridge
sets a nine-second provider deadline and reports a meaningful timeout rather
than returning a fake completion. A slow reasoning model can work for chat
while being too slow for native Tab. JSON decoding validates the replacement;
malformed, truncated and tool responses are rejected.

- `TestTabContextCoordinates`: CRLF, zero-based line/UTF-16 column handling,
  supplementary characters, invalid positions, empty files, partial context.
- `TestTabConfigAndFilesyncRetry`: configured-model gating, model picker
  order/deduplication, exact typed retry error, no provider call on absent data.
- `TestTabProviderReplacementAndErrors`: local provider translation,
  native frame order, whitespace preservation, malformed/truncated responses.
- `TestInstalledTabTextConsumer`: opt-in execution of only the installed
  pure text-reader functions in a fresh Node VM, with positive text and
  negative unsupported-marker controls. The application module is never loaded.
- `TestTabThroughTLSProxy`: actual temporary TLS CONNECT proxy, framed
  StreamCpp request, CppConfig, filesync retry, final native replacement.
- `TestTabProviderCanceledOnProxyStop`: active provider cancellation.

No cursor-jump, multi-edit, hold-Tab, cloud import, or provider-speed parity is
claimed by these tests. They do not modify the installed Cursor profile.

## Reproducible source evidence

Artifact `out/vs/workbench/workbench.desktop.main.js` SHA-256:
`79a1c7cc4fc3b14df093b18829ab88c1ee955e60ea49bb7984dbaf488f033713`.
UTF-16 offsets: 39030177 model-list prerequisite; 39109222 cursor conversion;
39079251 range consumption; 39010576 non-fused text reader.

Artifact `extensions/cursor-always-local/dist/main.js` SHA-256:
`8b5b25384080f5c9c05ef4fff5f8584d29242742574b531bdbba3b15f0d84934`.
Offsets: 4469654 active RPC; 4475773 filesync retry; 4003581 geo transport.
Current field declarations were checked against the repository protobuf; no
new guessed messages or successful empty backend stubs were introduced.
