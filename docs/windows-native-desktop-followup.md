# Windows native desktop follow-up — 2026-11-26

These checks used Windows Cursor **3.32.4**, the user's genuine Cursor sign-in,
the configured Cortex Letum provider, and a disposable local project. They
extend the earlier protocol and Windows checks; they do not establish complete
parity with every Cursor-hosted service.

## Model selection

The native Auto picker displayed **“More models are only available on paid
plans.”** The companion extension provides a separate **Cursor BYOK: Select
Model** command and a clickable status-bar picker. It does not alter account
entitlements or unlock Cursor's native picker.

- The extension was packaged, installed, and selected **Letum** through the
  actual command palette. The status bar changed to **BYOK: Letum**.
- The extension reads only `models-public.json` and writes the separate
  `cursor-model-selection.json`; neither contains API keys, provider endpoints,
  Cursor credentials, or workspace content.
- Selection applies globally to local Agent inference and its subagents from
  the next model invocation. Existing native model IDs cannot silently override
  that explicit selection. Commit/review-specific routing stays independent.
- Two-model switching, clearing the override, stale/invalid selections,
  persistence, and preservation of private configuration passed deterministic
  tests. Only Letum was configured for the live UI test; live switching between
  two different providers and editor-restart acceptance remain unverified.

Install/build instructions are in
[the extension README](../editor-extension/README.md). The companion requires
the updated proxy build from this PR.

## Observed desktop results

| Feature | Result and boundary |
| --- | --- |
| Skill | `/byok-smoke` discovered; native Read completed; exact `BYOK_SKILL_OK CURSOR_BYOK_TOOL_OK_20261125` response. |
| MCP tool | Local stdio fixture connected; native one-time approval; `echo_marker` returned `BYOK_MCP_OK:CURSOR_NATIVE_20261126`. |
| MCP resource | `fixture://marker` returned `BYOK_RESOURCE_OK`. |
| Ask | Found `return a - b` in the fixture and returned `ASK_OK`; no edits or shell commands requested. |
| Plan / Build | Native plan editor and Build button opened. Build created greeting files, updated todos, and ran two passing unittest tests. **CreatePlan's todo-array failure remains open**, as detailed below. |
| Debug | Native hypotheses, instrumentation, log panel, reproduction, fix, and cleanup completed. `add(2, 3)` changed from -1 to 5; the explicit regression function passed after instrumentation removal. |
| Multitask | Two actual native Explorer Task subagents completed; the final response contained the probe marker and `Hello, Alice!`. |
| Commit generation | Native SCM action populated the message field with a relevant message; no commit or push of the scratch project was performed. |
| AskQuestion | Native question card displayed two options; selecting NOTEBOOK_ALPHA resumed the turn. |
| Grep / ReadLints / notebook | Found the greeting definition, returned no lint errors, created a notebook and edited its markdown cell using EditNotebook. |
| Other local tools | Native execution logs recorded successful Read, Glob, Write, StrReplace, Delete, Shell, Await, TodoWrite and Task calls during the bounded fixtures. |
| Ctrl+K | Cursor itself reported that inline edit and terminal Ctrl+K are retired; the shortcut now adds the selection to Agent. |
| Tab completion | No accepted native Tab suggestion was obtained. Source/protocol tests remain separate from this unfinished desktop acceptance check. |
| Hosted web/image/search | Bounded calls were started and approval cards appeared. The run was stopped when work was redirected to PR preparation; no complete pass is claimed for WebFetch, WebSearch, XSearch, or GenerateImage. |
| Cloud agents / indexing / browser | No new end-to-end acceptance established in this follow-up. |

## Open CreatePlan issue

The live client rejected `todos` with **“expected array, received string”**.
It created partial plan artifacts before the error, and repeated attempts
produced additional artifacts. Build could execute a plan afterward, but this
is not a passing CreatePlan test.

This PR contains a narrow collection-argument normalizer: it decodes JSON text
only when the advertised schema explicitly requires an array or object. It
preserves string fields, ambiguous schemas, unknown properties and invalid
JSON. Regression tests verify matching stream/final arguments and preservation
of numeric values. **The live CreatePlan retry still failed after this change.**
The actual request schema/custom-tool representation must be inspected before
claiming the Plan issue fixed. A temporary diagnostic build was prepared but
not run; its overlay and local artifacts are not part of the PR.

## Validation and cleanup

- `go test ./... -count=1 -timeout 6m` and `go vet ./...` passed on Windows.
- `node --test editor-extension/storage.test.js` passed (2 tests).
- Frontend typecheck and production build passed.
- VSIX packaging and actual installation succeeded.
- Both test proxy sessions shut down cleanly. The original system proxy state
  and Cursor settings were restored. Hash-only comparisons verified the new
  genuine account values were unchanged; SQLite `quick_check` returned `ok`.
- The model-picker extension and its intended persistent Letum selection remain
  installed. The earlier test CA remains trusted; no certificate removal is
  claimed. No credentials or raw conversations are included in this PR.

Next: review the model-picker implementation, inspect CreatePlan's actual
schema, then finish Tab and hosted-service acceptance checks. Merge is reserved
for the macOS review agent/user; this Windows task did not merge this PR.
