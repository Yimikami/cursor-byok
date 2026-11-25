//go:build darwin

package cursor

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinSafeStorageChromiumKnownAnswer(t *testing.T) {
	// Chromium OSCryptMacTest.KnownAnswers, also carried in Electron43.8.9
	// patches/chromium/revert_oscrypt_remove_sync_backend.patch. This fixture
	// is independent of this implementation's test encryption helper.
	ciphertext := []byte{
		0x76, 0x31, 0x30, 0xbf, 0x08, 0x6d, 0x20, 0x56, 0x86, 0x1a, 0x80,
		0xde, 0x82, 0x5f, 0xc9, 0x35, 0x86, 0x86, 0x30, 0x64, 0x4f, 0x2c,
		0xa1, 0x87, 0x45, 0x02, 0x13, 0xae, 0x66, 0x81, 0xb4, 0xd6, 0x43,
		0xd1, 0x9b, 0x25, 0x81, 0xc8, 0x5c, 0x88, 0x78, 0xc1, 0xbc, 0x97,
		0xe7, 0x26, 0xa1, 0x0e, 0x51, 0xea, 0x77,
	}
	password := []byte("mock_password")
	got, err := decryptCursorSafeStorageDarwin(ciphertext, func() ([]byte, error) { return password, nil })
	want := make([]byte, 32)
	for i := range want {
		want[i] = byte(i)
	}
	if err != nil || !bytes.Equal([]byte(got), want) {
		t.Fatalf("Chromium known answer failed: %v", err)
	}
	if !bytes.Equal(password, make([]byte, len(password))) {
		t.Fatal("password buffer was not cleared")
	}
}

func darwinAuthFixture(t *testing.T, plaintext []byte, pad bool) []byte {
	t.Helper()
	key, err := pbkdf2.Key(sha1.New, "fixture-password", []byte("saltysalt"), 1003, 16)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	body := append([]byte(nil), plaintext...)
	if pad {
		n := aes.BlockSize - len(body)%aes.BlockSize
		body = append(body, bytes.Repeat([]byte{byte(n)}, n)...)
	}
	cipher.NewCBCEncrypter(block, bytes.Repeat([]byte{' '}, aes.BlockSize)).CryptBlocks(body, body)
	return append([]byte("v10"), body...)
}

func TestDarwinSafeStorageEnvelopeAndErrors(t *testing.T) {
	read := func() ([]byte, error) { return []byte("fixture-password"), nil }
	decrypt := func(_ string, blob []byte) (string, error) { return decryptCursorSafeStorageDarwin(blob, read) }
	for _, token := range []string{"fixture-owner", "refreshed-owner", "token-✓", strings.Repeat("x", 16)} {
		value := cursorSafeStoragePrefix + base64.StdEncoding.EncodeToString(darwinAuthFixture(t, []byte(token), true))
		got, err := decodeCursorAuthTokenWith("unused", value, decrypt)
		if err != nil || got != token {
			t.Fatalf("fixture decode failed: %v", err)
		}
	}
	for _, envelope := range []string{"cursor-safe-storage:v2:AAAA", cursorSafeStoragePrefix + "not_base64!", cursorSafeStoragePrefix} {
		if _, err := decodeCursorAuthTokenWith("unused", envelope, decrypt); err == nil {
			t.Fatal("malformed envelope accepted")
		}
	}
	badPadding := bytes.Repeat([]byte{2}, aes.BlockSize)
	badPadding[aes.BlockSize-2] = 3
	for _, blob := range [][]byte{
		[]byte("v11unsupported"), []byte("v10"), []byte("v10partial"),
		darwinAuthFixture(t, bytes.Repeat([]byte{0}, 16), false),
		darwinAuthFixture(t, bytes.Repeat([]byte{17}, 16), false),
		darwinAuthFixture(t, badPadding, false),
		darwinAuthFixture(t, []byte{0xff, 0xfe}, true),
	} {
		if _, err := decryptCursorSafeStorageDarwin(blob, read); err == nil {
			t.Fatal("invalid cipher, padding or encoding accepted")
		}
	}
	denied := errors.New("fixture Keychain denied")
	blob := darwinAuthFixture(t, []byte("owner"), true)
	if _, err := decryptCursorSafeStorageDarwin(blob, func() ([]byte, error) { return nil, denied }); !errors.Is(err, denied) {
		t.Fatal("Keychain failure not propagated")
	}
	if _, err := decryptCursorSafeStorageDarwin(blob, func() ([]byte, error) { return nil, nil }); err == nil {
		t.Fatal("empty Keychain password accepted")
	}
	called := false
	if _, err := decryptCursorSafeStorageDarwin([]byte("v10partial"), func() ([]byte, error) { called = true; return nil, nil }); err == nil || called {
		t.Fatal("malformed ciphertext reached Keychain")
	}
}

// Explicit opt-in only. Run against a read-only SQLite backup, never an active
// user's database in the normal test suite. No credentials are printed.
func TestLiveDarwinCursorAuthSafeStorageOwnership(t *testing.T) {
	path := os.Getenv("CURSOR_BYOK_AUTH_READONLY_DB")
	if path == "" {
		t.Skip("set CURSOR_BYOK_AUTH_READONLY_DB to an explicit read-only backup")
	}
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String() + "?mode=ro&immutable=1"
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var encrypted string
	if err := db.QueryRow("SELECT value FROM ItemTable WHERE key = ?", "cursorAuth/accessToken").Scan(&encrypted); err != nil {
		t.Fatal("read backup token failed")
	}
	if !strings.HasPrefix(encrypted, cursorSafeStoragePrefix) {
		t.Fatal("backup does not contain a safeStorage envelope")
	}
	token, err := decodeCursorAuthToken(path, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(token) == "" || IsLocalSessionToken(token) {
		t.Fatal("backup does not contain a genuine nonempty session")
	}
	if !matchesAccessTokenAt(context.Background(), path, token) || matchesAccessTokenAt(context.Background(), path, "wrong-fixture-session") {
		t.Fatal("owner authorization did not match decrypted session")
	}
	t.Log("genuine encrypted owner session verified read-only; no account values exposed or changed")
}

func TestDarwinExplicitKeychainAccessClearsReturnedSecret(t *testing.T) {
	for _, failure := range []error{nil, &CursorKeychainAccessError{Status: -25293}} {
		password := []byte("fixture-only-secret")
		called := 0
		err := requestCursorKeychainAccessWith(func(allowInteraction bool) ([]byte, error) {
			called++
			if !allowInteraction {
				t.Fatal("explicit action did not allow the system permission dialog")
			}
			return password, failure
		})
		if called != 1 || !errors.Is(err, failure) {
			t.Fatal("explicit permission lookup did not preserve its result")
		}
		if !bytes.Equal(password, make([]byte, len(password))) {
			t.Fatal("explicit permission lookup retained secret bytes")
		}
	}
}
