# Windows connection validation

The connection fixes have automated tests with temporary certificates, simulated
registry failures, and a loopback TLS/WebSocket provider. A Windows cross-build
does not verify the actual Windows trust store, OS proxy notifications, or
Cursor UI. Run these checks on the Windows device before treating that path as
fully verified.

## Record the environment

- Windows version and architecture, Cursor version, cursor-byok commit/build.
- Whether a VPN or another proxy is normally enabled.
- Whether ordinary HTTPS browsing works before starting cursor-byok.

Keep API keys, auth tokens and conversation contents out of shared logs.

## First connection

1. Close Cursor completely and run the updated cursor-byok under the same
   Windows account. Keep its existing configuration and backup files.
2. Add/select the intended adapter and run its provider test. A successful
   provider test verifies only the provider connection.
3. Select **Install CA**. The status must reflect the active certificate's
   fingerprint, not merely another root named `cursor-byok Local CA`.
4. Select **Start service**. It must either report Running or display an
   actionable error; check ordinary HTTPS browsing in either case.
5. Open Cursor and select the configured model. Send a short Ask message and
   confirm the answer finishes. Then use a scratch folder containing a harmless
   text file to check an Agent read-file tool call.
6. Cancel a response, then send another message. Both should complete without
   a repeating `/agent/v1/run` 404 or a permanently busy chat.
7. Select **Stop**. Verify ordinary browsing and restart Cursor to confirm the
   original account is available again.

## Failure recovery

- Without the active CA trusted, Start must fail before changing the system
  proxy. Install CA and retry.
- If port 18080 is already occupied, Start must report that problem and leave
  the OS proxy unchanged.
- If restoration fails, the application must remain available and show the
  error. Resolve the reported problem and use **Stop and restore settings**;
  the same recovery can be retried after reopening the application.
- Check tray Quit and native Quit after a normal Start: original proxy settings
  should be restored. Forced termination is different: recovery happens on the
  next launch.

If an older build has already left a dead manual proxy, inspect Windows proxy
settings. Only disable the stale `127.0.0.1:18080` entry when cursor-byok is no
longer running; preserve unrelated VPN/corporate settings and recovery files.

Model-picker preferences are still pinned separately from the auth backup.
Cursor 3.26.16, originally reported in issue #12, needs its own desktop check;
the implemented WebSocket contract was inspected in Cursor 3.32.4.
