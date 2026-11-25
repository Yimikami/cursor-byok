package cursor

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestExplicitCursorProfileKeepsRecoverySeparate(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	t.Setenv("CURSOR_BYOK_CURSOR_DATA_DIR", a)
	if got := settingsPath(); got != filepath.Join(a, "User", "settings.json") {
		t.Fatalf("settings outside chosen profile: %q", got)
	}
	if got := stateDBPath(); got != filepath.Join(a, "User", "globalStorage", "state.vscdb") {
		t.Fatalf("auth outside chosen profile: %q", got)
	}
	authA := ProfileBackupName("cursor-auth-backup.json")
	settingsA := backupPath()
	t.Setenv("CURSOR_BYOK_CURSOR_DATA_DIR", b)
	if authA == ProfileBackupName("cursor-auth-backup.json") || settingsA == backupPath() {
		t.Fatal("different profiles share recovery files")
	}
	t.Setenv("CURSOR_BYOK_CURSOR_DATA_DIR", "relative-profile")
	if settingsPath() != "" || stateDBPath() != "" {
		t.Fatal("invalid override silently selected a real profile")
	}
}

func TestExplicitDefaultCursorProfileUsesLegacyRecovery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Setenv("APPDATA", t.TempDir())
	} else {
		t.Setenv("HOME", t.TempDir())
	}
	t.Setenv("CURSOR_BYOK_CURSOR_DATA_DIR", "")
	if err := os.Unsetenv("CURSOR_BYOK_CURSOR_DATA_DIR"); err != nil {
		t.Fatal(err)
	}
	defaultSettings, defaultDB, defaultBackup := settingsPath(), stateDBPath(), backupPath()
	root := filepath.Dir(filepath.Dir(defaultSettings))
	if root != defaultCursorProfileRoot() || defaultDB != filepath.Join(root, "User", "globalStorage", "state.vscdb") {
		t.Fatal("backup identity default diverged from profile data paths")
	}
	for _, existing := range []bool{false, true} {
		if existing {
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("CURSOR_BYOK_CURSOR_DATA_DIR", root)
		if ProfileBackupName("cursor-auth-backup.json") != "cursor-auth-backup.json" || backupPath() != defaultBackup {
			t.Fatalf("explicit default changed recovery identity (existing=%v)", existing)
		}
	}
}

func TestWindowsCursorProfileCaseUsesSameRecovery(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path identity")
	}
	root := t.TempDir()
	for _, path := range []string{root, filepath.Join(root, "NotCreatedYet")} {
		t.Setenv("CURSOR_BYOK_CURSOR_DATA_DIR", strings.ToLower(path))
		auth := ProfileBackupName("cursor-auth-backup.json")
		settings := backupPath()
		t.Setenv("CURSOR_BYOK_CURSOR_DATA_DIR", strings.ToUpper(path))
		if auth != ProfileBackupName("cursor-auth-backup.json") || settings != backupPath() {
			t.Fatal("Windows casing changed profile recovery identity")
		}
	}
	t.Setenv("APPDATA", root)
	t.Setenv("CURSOR_BYOK_CURSOR_DATA_DIR", strings.ToUpper(filepath.Join(root, "Cursor")))
	if ProfileBackupName("cursor-auth-backup.json") != "cursor-auth-backup.json" {
		t.Fatal("case variant of explicit default lost legacy backup")
	}
}

func TestCursorProfileSymlinkUsesTargetRecovery(t *testing.T) {
	parent := t.TempDir()
	target, alias := filepath.Join(parent, "target"), filepath.Join(parent, "alias")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		if runtime.GOOS == "windows" && (os.IsPermission(err) || errors.Is(err, syscall.Errno(1314))) {
			t.Skipf("Windows symlink creation unavailable: %v", err)
		}
		t.Fatal(err)
	}
	t.Setenv("CURSOR_BYOK_CURSOR_DATA_DIR", target)
	auth, settings := ProfileBackupName("cursor-auth-backup.json"), backupPath()
	t.Setenv("CURSOR_BYOK_CURSOR_DATA_DIR", alias)
	if auth != ProfileBackupName("cursor-auth-backup.json") || settings != backupPath() {
		t.Fatal("existing symlink alias changed profile recovery identity")
	}
}
