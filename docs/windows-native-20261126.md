# Windows native acceptance — 2026-11-26

Status: COMPLETE — all seven requested Windows acceptance checks passed, with the instrumentation and observation limits below. No merge, issue closure, release, or test-change commit/push performed.

Source: PR #17, `fix/macos-native-completion`, full commit `a2a8c5bced6c3b8644b4a09fbdc532067f5f1fbb`. Local branch `codex/windows-acceptance-pr17`; tracked source unchanged. Cursor Windows 3.32.4 (product and file version).

Production binary SHA-256: `BA6D9C69DA67293106C10F506051B335461C6FFE971C87EB9602766E90A7BD3F`.
Temporary observer binary SHA-256: `3E5EC700CAC1EE59350AB63D5F9F1D53AB0D7603B7158778181F6CEBBEE11E7F`.
The unmodified production build passed an initial real UI Start/Stop. Subsequent native checks use the same commit plus a test-only Go overlay that discards ordinary logging, disables BYOK message-body persistence, and observes HTTP transport metadata (UTC, provider hostname, configured model, status, transport-error boolean). It does not change routing, model selection, account state, schemas, provider payloads or response bytes. Neither observer nor overlay is a product change.

## Baseline

- Cursor fully closed before initial Start. Companion `cursor-byok-local.cursor-byok-model-picker` disabled with official launch flag, independently verified on the running process; no permanent extension state change.
- Original WinINET: ProxyEnable=0; ProxyServer=`dc.decodo.com:10001`; ProxyOverride=`localhost;127.*;10.*;172.16.*;<local>`; AutoConfigURL and AutoDetect absent.
- Native useOpenAIKey=false, no saved native OpenAI key in either legacy or secret storage. The disabled-key guard was not bypassed.
- Real account fields/native credential entries and every owned model preference captured only as presence/hash, with settings.json hash and SQLite quick_check=ok.
- Existing CA thumbprint `982983E2F0140415274060308723E0F2D7064091` already trusted in the real CurrentUser Root store. Sandbox-only certificate enumeration initially hid it; real-user certutil and store enumeration resolved this discrepancy. No CA trust added during this run; preexisting trust must remain.
- Original provider config protected using CurrentUser DPAPI; decryptability and exact original hash verified, never printed. Both requested adapters use the preauthorized existing test key.

## Acceptance matrix

| Check | Status | Evidence |
| --- | --- | --- |
| Start/native menu | geçti | Start showed Running 127.0.0.1:18080; native picker directly listed Letum and Claude Sonnet 4.6 (Claude.gg), without searching or companion. Genuine account remained unchanged at initial Stop; Upgrade UI remained. |
| Same-chat A→B→A | geçti | Same native conversation preserved marker and two actual Read results across Letum → Claude → Letum. See transport metadata below. |
| Plan/CreatePlan + Build | geçti | Native Created Plan / Windows A2 Addition; file windows_a2_addition_08c9636a.plan.md contained exactly two object todos (fix-add, run-tests), initially pending. Native Build changed add from subtraction to addition, ran unchanged test, returned CALC_ACCEPTANCE_OK/exit 0, and marked both todos completed. No expected-array/received-string error. Native Keep accepted change. Independent test rerun passed; test file has no diff. |
| Tab | geçti | Actual grey `a * b` suggestion appeared after `return `; Tab accepted it, Ctrl+S saved src/tab.py. Native Tab stats became 2/2; disk contained return a * b and independent multiply(6,7)==42 passed. |
| MCP/skill/modes/commit | geçti | Actual native MCP tool and resource, slash-menu skill, Ask, Debug, Multitask/Task/Await, and SCM message generation all completed. Individual evidence below. |
| Dashboard save race | geçti | Real UI slow loopback probe remained pending while row changed from slow-a/v1 to slow-b/v2; late success left new values and untested metadata intact. Second pending probe: deleted row, added replacement-c/v3 at same index; late result did not resurrect old row or stamp replacement. Removed disposable replacement afterward, leaving 2 real adapters. Native Plan provider continuation succeeded during these saves. Frontend 3 race regression tests and backend concurrent routing/save regressions also passed. |
| Final Stop/restoration | geçti | Disabled the temporary MCP source, exited Cursor normally, independently verified zero Cursor processes, then clicked Stop and observed Stopped before quitting BYOK. Final proxy presence/types/values, account/native-key hashes, all owned model preferences, settings bytes, extension-disabled state, and original BYOK config bytes match baseline. SQLite quick_check=ok, no proxy listener, no test app/helper processes, normal GitHub HTTPS=200 with certificate validation enabled. Preexisting CA trust preserved; no new CA trust was installed. Encrypted config backup deleted after verified restore. |

## A→B→A provider evidence (UTC)

| Time | Actual outgoing host/model | Native result |
| --- | --- | --- |
| 00:14:55, 00:15:01 | cyber.cortexai.io / letum, HTTP 200 | Actual Read src/facts_a.txt, returned ALPHA_FILE_VALUE=cedar-4821 and original unique marker. |
| 00:15:35, 00:15:39 | app.claude.gg / claude-sonnet-4-6, HTTP 200 | Recalled original marker without repetition in new prompt and previous Read result; actual Read facts_b.txt returned BETA_FILE_VALUE=marble-7392, SWITCH_B_A2_OK. |
| 00:16:37 | cyber.cortexai.io / letum, HTTP 200 | Recalled original marker and both correct file values, SWITCH_RETURN_A2_OK. |

The initial Letum answer guessed a wrong file value without Read; it was rejected and is not counted as success. Explicit retry invoked actual Read and returned the independently known fixture content. Provider self-identification was not used as evidence.

## Native MCP, skill, modes and SCM evidence

| Native feature | Status | Observed result |
| --- | --- | --- |
| MCP tool | geçti | Customize listed disposable byok-acceptance with 1 tool and 1 resource. Actual GetMcpTools/CallMcpTool execution of echo_marker returned `MCP_TOOL_A2_OK WINDOWS_MCP_482`; native execution group showed the call. One-time Run approval only; no Always Run grant. |
| MCP resource | geçti | Actual native Read resource/FetchMcpResource for fixture://marker returned `MCP_RESOURCE_A2_OK`. |
| Skill | geçti | Selected /byok-smoke from the native slash menu. Actual Read of facts_a.txt returned ALPHA_FILE_VALUE=cedar-4821 and `SKILL_A2_OK`. |
| Ask | geçti | Native Ask read debug_math.py, identified square(3)=6 instead of 9, returned ASK_A2_OK and made no edits. |
| Debug | geçti | Native Debug session 4422ee instrumented only the disposable fixture, recorded current 6 vs expected 9, corrected addition to multiplication, verified square(3)=9 and square(-2)=4, removed instrumentation, and returned DEBUG_A2_OK. Independent clean-file checks passed; native Keep accepted the change, Agent Stats 4/4. |
| Multitask / Task | geçti | Native Multitask launched the byok-reader task; its completed task card and notification returned the actual second-file value marble-7392 and TASK_A2_OK. |
| Await | geçti | A two-second background shell task produced the native Waited for shell result; terminal Read then showed AWAIT_A2_OK and exit 0. |
| Commit generation | geçti | Native SCM sparkle generated `fix: correct calculator arithmetic operations` in the commit input. The disposable repository still has only its initialization commit ff8e6f8 and no remotes; no test-change commit/push occurred. |

## Final restoration and cleanup

- `after-final.json` compared equal to `before.json` for every recorded category: proxy, account/native-key entries, owned model preferences, settings, config, native-key enabled/presence flags, extension-disabled state, and SQLite health.
- ProxyEnable returned to 0; original ProxyServer and ProxyOverride remained; AutoConfigURL and AutoDetect remained absent.
- `ca-after.json` matches the preexisting CurrentUser CA identity in `ca-before.json`. No CA was added by this run, so no preexisting trust was removed.
- The project MCP source was disabled again before Cursor exited. Its process and the disposable slow HTTP adapter process are gone. Cursor and BYOK are closed; port 18080 has no listener.
- Original BYOK config was restored byte-for-byte from its CurrentUser DPAPI backup; only after hash verification was that encrypted backup removed. No new plaintext credential file was created or retained in the test workspace/report.
- Ordinary HTTPS to github.com returned 200 without certificate-validation bypass. Independent example.com and www.microsoft.com probes timed out; these are recorded as probe limitations, not successful checks.
- Removed nine generated cache/build/test artifacts totaling 2,153,161,686 bytes (about 2.15 GB). Also removed only the test-created global Cursor plan after copying it to `native-plan.plan.md` as evidence. Preserved the production executable, small disposable fixture, report, existing user data and earlier deliverables. The observer executable was deleted; its small source overlay is retained as `observer-source.zip` for reproducibility.

## Checks and environment repairs

- `npm ci`, `npm test` (3/3), `npm run build`, native Windows production `go build`: passed.
- `go test -p 2 ./internal/...` and `go vet ./...`: passed with real-user localhost permissions. Initial sandbox TCP denials were environment failures and were rerun; not product regressions. Race detector not run on Windows in this acceptance pass.
- Native SCM initially failed because Cursor inherited GIT_CONFIG_COUNT without GIT_CONFIG_VALUE_0 from the tool launch environment. Quit Cursor normally, cleared only process-local GIT_CONFIG_COUNT/KEY_n/VALUE_n, relaunched with companion disabled. Git discovery and native commit generation then passed; no global Git or user settings changed.
- New local MCP fixture was disabled by default in Customize. Enabled only this disposable local fixture (1 tool, 1 resource), without changing unrelated servers or persistent Always Run approval; disabled it again during cleanup.
- No production source bug established in this acceptance pass; tracked source remains clean at the exact requested commit. No PR #16 companion code or global selection override reintroduced.

## Observation limits

- The native functional and outgoing-host/model checks used the explicitly described observer build. The unmodified production binary separately passed actual Start/Stop; this report does not present the observer as an unmodified binary.
- Metadata contains 55 successful Letum requests, 2 successful Claude requests, and one Letum transport interruption (status 0 at 00:19:20 UTC). The observer deliberately stores no raw error/body, so the interruption's cause is unclassified; required acceptance outcomes subsequently completed. Do not treat this as a zero-error network run.
- Letum occasionally repeated short output fragments, including the final Multitask marker suffix. The required native tool outcomes were correct, but the repetition's origin was not established and no speculative deduplication patch was applied.
- These results cover the seven requested acceptance controls and listed native smokes, not every conditional/cloud-only Cursor feature. macOS results are not counted as Windows evidence. Windows race-detector execution was not performed.

Evidence retained in the original Windows acceptance workspace (not included in this repository): `provider-metadata.jsonl`, `slow-probe-events.jsonl`, `go-test.txt`, `before.json`, `after-initial-stop.json`, `after-final.json`, `ca-before.json`, `ca-after.json`, `stop-checks.json`, `https-checks.json`, `cleanup.json`, and `native-plan.plan.md` in that workspace. No credentials or arbitrary provider message bodies in these evidence files.
