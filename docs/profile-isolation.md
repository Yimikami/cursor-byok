# Isolated Cursor profiles

Set `CURSOR_BYOK_CURSOR_DATA_DIR` to the absolute directory passed to Cursor's
`--user-data-dir`. Start both processes with that same directory.

For a new directory, first launch Cursor once with that `--user-data-dir` to
initialize the profile, then close that Cursor instance before starting BYOK.

For example, in PowerShell:

```powershell
$profileRoot = 'C:\CursorProfiles\byok-test'
$env:CURSOR_BYOK_CURSOR_DATA_DIR = $profileRoot
& 'C:\path\to\cursor-byok.exe'
# In another terminal:
& 'C:\path\to\Cursor.exe' --user-data-dir 'C:\CursorProfiles\byok-test'
```

The override selects `<profile>\User\settings.json` and
`<profile>\User\globalStorage\state.vscdb`. It does not copy an existing Cursor
profile, change the BYOK configuration directory, move API keys, or create a
separate CA certificate. An empty or relative override fails instead of falling
back to the ordinary Cursor profile. Leave the variable unset for normal use.

Auth and Cursor settings recovery files remain in the normal BYOK configuration
directory, with a profile identity suffix. Windows path casing and existing
symlink/junction aliases share the same identity. Explicitly selecting the
ordinary default profile uses its existing, unsuffixed recovery filenames.
Create symlink aliases before starting BYOK and keep the profile path unchanged
until Stop has restored its settings and auth.

The system proxy and trusted CA remain shared at the operating system's
configured user/machine scope. Profile isolation does **not** isolate those
resources: run only one BYOK proxy instance at a time, and stop it before
switching profiles. Stop the running instance before rebuilding/replacing it so
its own recovery files are consumed by the version that created them.
