package cursor

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func authTestDB(t *testing.T) (string, string, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.vscdb")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec("CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT)"); err != nil {
		t.Fatal(err)
	}
	return dbPath, filepath.Join(dir, "auth-backup.json"), db
}

func setAuthValue(t *testing.T, db *sql.DB, key, value string) {
	t.Helper()
	if _, err := db.Exec("INSERT INTO ItemTable(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value); err != nil {
		t.Fatal(err)
	}
}

func authValue(t *testing.T, db *sql.DB, key string) string {
	t.Helper()
	var value string
	if err := db.QueryRow("SELECT value FROM ItemTable WHERE key = ?", key).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestAuthRestoreRemovalFailureDoesNotReplayOverNewLogin(t *testing.T) {
	dbPath, backupPath, db := authTestDB(t)
	setAuthValue(t, db, fakeAuthKeys[0], "original-session")
	setAuthValue(t, db, "unrelated", "keep")
	if err := injectFakeProUserAt(dbPath, backupPath); err != nil {
		t.Fatal(err)
	}
	removeFailure := errors.New("sidecar is locked")
	err := restoreFakeProUser(dbPath, backupPath, func(string) error { return removeFailure })
	if !errors.Is(err, removeFailure) || !strings.Contains(err.Error(), "remove restored auth backup") {
		t.Fatalf("deletion failure hidden: %v", err)
	}
	if got := authValue(t, db, fakeAuthKeys[0]); got != "original-session" {
		t.Fatalf("initial restore failed: %q", got)
	}
	setAuthValue(t, db, fakeAuthKeys[0], "new-login")
	setAuthValue(t, db, fakeAuthKeys[2], "new-email")
	if err := restoreFakeProUser(dbPath, backupPath, os.Remove); err != nil {
		t.Fatal(err)
	}
	if authValue(t, db, fakeAuthKeys[0]) != "new-login" || authValue(t, db, fakeAuthKeys[2]) != "new-email" || authValue(t, db, "unrelated") != "keep" {
		t.Fatal("retry overwrote newer user state")
	}
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Fatalf("stale backup remains: %v", err)
	}
}

func TestAuthRestorePreservesLogoutAfterRemovalFailure(t *testing.T) {
	dbPath, backupPath, db := authTestDB(t)
	setAuthValue(t, db, fakeAuthKeys[0], "original-session")
	if err := injectFakeProUserAt(dbPath, backupPath); err != nil {
		t.Fatal(err)
	}
	if err := restoreFakeProUser(dbPath, backupPath, func(string) error { return os.ErrPermission }); err == nil {
		t.Fatal("expected cleanup error")
	}
	if _, err := db.Exec("DELETE FROM ItemTable WHERE key LIKE 'cursorAuth/%'"); err != nil {
		t.Fatal(err)
	}
	if err := restoreFakeProUser(dbPath, backupPath, os.Remove); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM ItemTable").Scan(&count); err != nil || count != 0 {
		t.Fatalf("logout was reverted: count=%d err=%v", count, err)
	}
}

func TestAuthRestoreRetainsBackupWhenSessionOwnershipIsAmbiguous(t *testing.T) {
	for _, token := range []string{"unrecognized-transformed-token", cursorSafeStoragePrefix + "%%%", cursorSafeStoragePrefix + "djIw"} {
		t.Run(token, func(t *testing.T) {
			dbPath, backupPath, db := authTestDB(t)
			setAuthValue(t, db, fakeAuthKeys[0], "original-session")
			if err := injectFakeProUserAt(dbPath, backupPath); err != nil {
				t.Fatal(err)
			}
			originalBackup, err := os.ReadFile(backupPath)
			if err != nil {
				t.Fatal(err)
			}
			setAuthValue(t, db, fakeAuthKeys[0], token)
			if err := restoreFakeProUser(dbPath, backupPath, os.Remove); err == nil || !strings.Contains(err.Error(), "backup retained") {
				t.Fatalf("ambiguous ownership did not preserve recovery: %v", err)
			}
			if authValue(t, db, fakeAuthKeys[0]) != token || authValue(t, db, fakeAuthKeys[2]) != fakeProEmail {
				t.Fatal("ambiguous state was overwritten")
			}
			backup, err := os.ReadFile(backupPath)
			if err != nil || string(backup) != string(originalBackup) {
				t.Fatal("original recovery backup was changed or removed")
			}
		})
	}
}
