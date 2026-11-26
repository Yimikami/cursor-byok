# Native helper routing and usage — 2026-11-26

Cursor 3.32.4 on macOS arm64. The Windows desktop report at `a2a8c5b` is a separate, earlier measurement.

## Current routing

| Feature | Destination | Failure behavior |
| --- | --- | --- |
| Chat, local Agent/Ask/Plan/Debug, Fix All Issues | Selected BYOK adapter | Provider errors remain visible; no paid native fallback |
| Agent Review | BYOK by default, separately selectable model | Invalid/truncated output is an error, never “No issues” |
| Agent Review with explicit Cursor Bugbot choice | Cursor native service | Native account usage, limits and errors apply |
| Conversation title, branch name, Tab, commit message | Cursor native service | No BYOK retry/fallback |
| Marketplace, voice and other hosted services | Native routing | Native service/account limits apply |

The dashboard warns about Cursor usage when selecting Bugbot and the provider's
token costs for BYOK review. Changing the setting does not reassign an existing
Bidi session to the other billing destination; active native polls keep working
after switching the default to BYOK. Review model choices use persistent adapter
identities, so identical model names at different providers stay distinct through
renaming, reordering and key rotation. A missing explicit selection fails rather
than billing a different adapter. BYOK Review supports the client's
SSE transport; unsupported direct/poll transports return an explicit error rather
than silently sending the review to Cursor. Native voice/review Bidi input does
not depend on local account decryption; local inference and late local input
still require the current Cursor account.

## Controlled desktop measurement

All test times below are UTC, using a disposable local repository with no remote.
Normal chat used **Claude Sonnet 4.6 (claude.gg)** through BYOK. We observed the
logged-in Cursor dashboard through its normal UI, without extracting cookies.
A temporary observer recorded only RPC path, timestamp and HTTP status; it did
not change requests, headers, response bytes or routing.

| Time | Action and observed result |
| --- | --- |
| 01:45:21–22 | BYOK Claude chat replied OK; native `NameTab` returned 200 and the conversation became “Zero-quantity subtotal test”. |
| 01:47:00–06 | Cursor's Create New Branch flow called native `WriteGitBranchName` (200), generated `subtotal-calc`, and made a local fixture commit. The commit message was manually supplied to isolate branch naming. Nothing was published. |
| 01:47:48–50 | Native `AvailableModels`, `CppConfig` and `StreamCpp` returned 200; an actual ghost-text completion was accepted with Tab and saved. |
| After those actions, and again approximately 01:54 | Dashboard remained at **296K total/included tokens, zero on-demand tokens, 8 events**. No new helper-feature usage event appeared. |

A short native Grok chat at 01:34 occurred before the corrected BYOK-only test
window and added a 15.9K-token / $0.02 included row. It is explicitly excluded
from helper-feature attribution. No Grok chat was used in the controlled window.
Earlier native Agent Review did produce five visible `agent_review` events,
approximately 179.6K tokens total. These were included usage, not on-demand spend.
This is why native Bugbot is an explicit opt-in rather than the default.

Tab, conversation titles and branch naming remain native. These actions did not
add a Cursor token-usage event in the recorded account. Voice retains its native
daily quota; its numeric allowance was not exposed by the inspected client.

## BYOK review follow-up

The real Claude review correctly identified the deliberately introduced
`unit_price - quantity` bug, but returned `confidence: "high"`. The old strict
numeric decoder rejected the whole valid finding. The decoder now accepts known
qualitative labels without inventing a numeric confidence score; numeric scores
must be in [0, 1]. The prompt explicitly requests numeric confidence and integer
line numbers. Invalid, missing or truncated reports still fail visibly.

The fixed desktop rerun succeeded: Cursor displayed **Found 1 Potential Issue**
at `pricing.py:11-11` with the correct subtraction-versus-multiplication finding.
The test observer identified `claude-sonnet-4-6`, one provider round, and a valid
JSON answer. Fix All Issues prepared the matching normal chat prompt with Claude
Sonnet 4.6 selected. The coding agent changed subtraction back to multiplication;
independent execution of the unchanged fixture tests printed `REVIEW_FIXTURE_OK`.

## Request accounting

- One modern native inference invocation makes one provider HTTP request. The
  same invocation ID in an active run is deduplicated. There is no hidden second
  critique/summary request and no automatic internal provider retry/fallback.
- Native prompts, tools and history are translated. JSON/tool wrappers can add
  small formatting overhead; this is not a claim of byte-identical payloads.
- Tool continuation, Task/multitask, a new client retry or an explicit new run can
  legitimately cause additional requests. Legacy review can use multiple tool
  rounds (maximum 20); the first captured Claude review used one round.
- Explicit adapter Test / Save and Test / Test All spends a bounded provider
  probe (up to 512 output tokens). Startup/status refresh is not a model probe.
- Titles/branch/Tab/commit now make zero BYOK calls through these proxy routes.
  Native requests still exist and remain subject to Cursor's accounting.

Cache discounts are **not certified**: native `prompt_cache_key` is not forwarded,
there is no explicit Anthropic `cache_control`, and the usage UI does not fully
account for provider cache-read/cache-write details. This does not establish that
OpenAI automatic caching is disabled. Third-party provider billing/cache behavior
has not been measured. Provider invoices remain the authority for actual cost.

## Automated checks

Real two-hop TLS fixtures exercise five native helper routes with 200/403/429/503,
preserving native credentials, bodies, responses and errors with zero BYOK calls.
Review tests cover BYOK success/provider failure, native opt-in direct/SSE/poll,
queued and late BidiAppend inputs, and no cross-provider billing fallback.
Provider accounting tests cover success, HTTP failure, truncated stream and
invocation deduplication. Separate parser tests cover qualitative confidence,
valid numeric confidence, malformed reports and output limits.
