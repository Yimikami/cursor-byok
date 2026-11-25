//go:build windows

package cursor

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func encryptedAuthTestDB(t *testing.T) (string, string, *sql.DB, func(string) string) {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "User", "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec("CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT)"); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	input := windows.DataBlob{Size: uint32(len(key)), Data: &key[0]}
	var output windows.DataBlob
	if err := windows.CryptProtectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		t.Fatalf("protect fixture key: %v", err)
	}
	wrapped := append([]byte("DPAPI"), unsafe.Slice(output.Data, output.Size)...)
	windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	metadata, _ := json.Marshal(map[string]any{"os_crypt": map[string]string{"encrypted_key": base64.StdEncoding.EncodeToString(wrapped)}})
	if err := os.WriteFile(filepath.Join(root, "Local State"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	encrypt := func(value string) string {
		t.Helper()
		block, _ := aes.NewCipher(key)
		gcm, _ := cipher.NewGCM(block)
		nonce := make([]byte, gcm.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			t.Fatal(err)
		}
		blob := append([]byte("v10"), nonce...)
		blob = gcm.Seal(blob, nonce, []byte(value), nil)
		return cursorSafeStoragePrefix + base64.StdEncoding.EncodeToString(blob)
	}
	return dbPath, filepath.Join(root, "auth-backup.json"), db, encrypt
}

func TestAuthRestoreRecognizesCursorSafeStorageMigration(t *testing.T) {
	dbPath, backupPath, db, encrypt := encryptedAuthTestDB(t)
	original := encrypt("original-fixture-session")
	setAuthValue(t, db, fakeAuthKeys[0], original)
	setAuthValue(t, db, fakeAuthKeys[2], "original-fixture-email")
	if err := injectFakeProUserAt(dbPath, backupPath); err != nil {
		t.Fatal(err)
	}
	setAuthValue(t, db, fakeAuthKeys[0], encrypt(fakeProSessionJWT))
	setAuthValue(t, db, fakeAuthKeys[1], encrypt(fakeProSessionJWT))
	if err := restoreFakeProUser(dbPath, backupPath, os.Remove); err != nil {
		t.Fatal(err)
	}
	if authValue(t, db, fakeAuthKeys[0]) != original || authValue(t, db, fakeAuthKeys[2]) != "original-fixture-email" {
		t.Fatal("encrypted fake session was mistaken for a new real login")
	}
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Fatal("successful restore left backup")
	}
}

func TestAuthRestoreKeepsNewEncryptedLoginAfterCleanupFailure(t *testing.T) {
	dbPath, backupPath, db, encrypt := encryptedAuthTestDB(t)
	setAuthValue(t, db, fakeAuthKeys[0], encrypt("original-fixture-session"))
	if err := injectFakeProUserAt(dbPath, backupPath); err != nil {
		t.Fatal(err)
	}
	setAuthValue(t, db, fakeAuthKeys[0], encrypt(fakeProSessionJWT))
	if err := restoreFakeProUser(dbPath, backupPath, func(string) error { return os.ErrPermission }); err == nil {
		t.Fatal("expected cleanup failure")
	}
	newLogin := encrypt("new-fixture-session")
	setAuthValue(t, db, fakeAuthKeys[0], newLogin)
	setAuthValue(t, db, fakeAuthKeys[2], "new-fixture-email")
	if err := restoreFakeProUser(dbPath, backupPath, os.Remove); err != nil {
		t.Fatal(err)
	}
	if authValue(t, db, fakeAuthKeys[0]) != newLogin || authValue(t, db, fakeAuthKeys[2]) != "new-fixture-email" {
		t.Fatal("retry overwrote a newer encrypted login")
	}
}

func TestPrepareCursorAuthPreservesEncryptedRealAccountAndClearsOwnOrphan(t *testing.T) {
	for _, temporary := range []bool{false, true} {
		t.Run(map[bool]string{false: "real-account", true: "legacy-temporary"}[temporary], func(t *testing.T) {
			dbPath, backupPath, db, encrypt := encryptedAuthTestDB(t)
			token, email := "real-fixture-session", "real-fixture-email"
			if temporary {
				token, email = fakeProSessionJWT, fakeProEmail
			}
			setAuthValue(t, db, fakeAuthKeys[0], encrypt(token))
			setAuthValue(t, db, fakeAuthKeys[2], email)
			setAuthValue(t, db, "unrelated", "keep")
			before := readTestItems(t, db)
			err := prepareCursorAuthAt(dbPath, backupPath)
			if temporary {
				if err == nil {
					t.Fatal("temporary session was accepted as a genuine account")
				}
				if after := readTestItems(t, db); len(after) != 1 || after["unrelated"] != "keep" {
					t.Fatal("orphan temporary fields were not narrowly cleared")
				}
			} else if err != nil || !reflect.DeepEqual(before, readTestItems(t, db)) {
				t.Fatalf("genuine encrypted account was changed: %v", err)
			}
			if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
				t.Fatal("new auth preparation created a fabricated-session backup")
			}
		})
	}
}

func TestAuthRestoreRetainsBackupOnSafeStorageAuthenticationFailure(t *testing.T) {
	dbPath, backupPath, db, encrypt := encryptedAuthTestDB(t)
	setAuthValue(t, db, fakeAuthKeys[0], "original-fixture-session")
	if err := injectFakeProUserAt(dbPath, backupPath); err != nil {
		t.Fatal(err)
	}
	token := encrypt(fakeProSessionJWT)
	blob, _ := base64.StdEncoding.DecodeString(token[len(cursorSafeStoragePrefix):])
	blob[len(blob)-1] ^= 1
	corrupt := cursorSafeStoragePrefix + base64.StdEncoding.EncodeToString(blob)
	setAuthValue(t, db, fakeAuthKeys[0], corrupt)
	if err := restoreFakeProUser(dbPath, backupPath, os.Remove); err == nil {
		t.Fatal("accepted unauthenticated ciphertext")
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatal("removed backup after decryption failure")
	}
	if authValue(t, db, fakeAuthKeys[0]) != corrupt {
		t.Fatal("changed current state after decryption failure")
	}
}

// Explicitly opt-in and read-only: confirms the installed client's envelope
// without exposing account values or using the user's DB in normal tests.
func TestLiveCursorAuthSafeStorageOwnership(t *testing.T) {
	dbPath := os.Getenv("CURSOR_BYOK_AUTH_READONLY_DB")
	if dbPath == "" {
		t.Skip("set CURSOR_BYOK_AUTH_READONLY_DB for read-only installed-format verification")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var token string
	if err := db.QueryRow("SELECT value FROM ItemTable WHERE key = ?", fakeAuthKeys[0]).Scan(&token); err != nil {
		t.Fatal(err)
	}
	value, err := decodeCursorAuthToken(dbPath, token)
	if err != nil {
		t.Fatal(err)
	}
	if value != fakeProSessionJWT {
		t.Fatal("current session is not the injected temporary session")
	}
	t.Log("installed safeStorage token verified as temporary session; no values exposed or changed")
}
