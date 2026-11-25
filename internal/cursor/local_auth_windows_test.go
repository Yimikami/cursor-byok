//go:build windows

package cursor

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLocalAccountAuthorizationDecodesWindowsSafeStorage(t *testing.T) {
	path, _, db, encrypt := encryptedAuthTestDB(t)
	profile := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	t.Setenv("CURSOR_BYOK_CURSOR_DATA_DIR", profile)
	stored := encrypt("owner-fixture-session")
	setAuthValue(t, db, fakeAuthKeys[0], stored)
	before := readTestItems(t, db)
	ctx := context.Background()
	if !MatchesCurrentAccessToken(ctx, "owner-fixture-session") {
		t.Fatal("profile owner's encrypted token was not recognized")
	}
	for _, token := range []string{stored, "wrong-session", "", fakeProSessionJWT} {
		if MatchesCurrentAccessToken(ctx, token) {
			t.Fatal("ciphertext or unowned identity authorized")
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if MatchesCurrentAccessToken(canceled, "owner-fixture-session") {
		t.Fatal("canceled authorization succeeded")
	}
	if !reflect.DeepEqual(before, readTestItems(t, db)) {
		t.Fatal("authorization modified account state")
	}
	setAuthValue(t, db, fakeAuthKeys[0], encrypt("refreshed-fixture-session"))
	if MatchesCurrentAccessToken(ctx, "owner-fixture-session") || !MatchesCurrentAccessToken(ctx, "refreshed-fixture-session") {
		t.Fatal("encrypted refresh not respected")
	}
	if _, err := db.Exec("DELETE FROM ItemTable WHERE key = ?", fakeAuthKeys[0]); err != nil {
		t.Fatal(err)
	}
	if MatchesCurrentAccessToken(ctx, "refreshed-fixture-session") {
		t.Fatal("signed-out encrypted identity authorized")
	}
}

func TestLocalAccountAuthorizationRejectsEncryptedFakeAndUnavailableCrypto(t *testing.T) {
	path, _, db, encrypt := encryptedAuthTestDB(t)
	ctx := context.Background()
	for _, plaintext := range []string{fakeProSessionJWT, " \n" + fakeProSessionJWT + "\t"} {
		stored := encrypt(plaintext)
		setAuthValue(t, db, fakeAuthKeys[0], stored)
		if matchesAccessTokenAt(ctx, path, plaintext) || matchesAccessTokenAt(ctx, path, stored) {
			t.Fatal("migrated local-only session authorized")
		}
	}
	stored := encrypt("owner-fixture-session")
	blob, _ := base64.StdEncoding.DecodeString(stored[len(cursorSafeStoragePrefix):])
	blob[len(blob)-1] ^= 1
	setAuthValue(t, db, fakeAuthKeys[0], cursorSafeStoragePrefix+base64.StdEncoding.EncodeToString(blob))
	if matchesAccessTokenAt(ctx, path, "owner-fixture-session") {
		t.Fatal("unauthenticated ciphertext authorized")
	}
	setAuthValue(t, db, fakeAuthKeys[0], stored)
	if err := os.Remove(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(path))), "Local State")); err != nil {
		t.Fatal(err)
	}
	if matchesAccessTokenAt(ctx, path, "owner-fixture-session") || matchesAccessTokenAt(ctx, path, stored) {
		t.Fatal("unavailable decryption key authorized")
	}
}
