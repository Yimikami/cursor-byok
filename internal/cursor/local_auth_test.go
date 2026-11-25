package cursor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalAccountAuthorizationTracksRefreshAndSignOut(t *testing.T) {
	db, path, _ := testCursorDB(t, map[string]string{"cursorAuth/accessToken": "owner-session"})
	ctx := context.Background()
	for _, token := range []string{"", "someone-else", fakeProSessionJWT} {
		if matchesAccessTokenAt(ctx, path, token) {
			t.Fatal("invalid identity authorized")
		}
	}
	if !matchesAccessTokenAt(ctx, path, "owner-session") {
		t.Fatal("owner not authorized")
	}
	if _, err := db.Exec("UPDATE ItemTable SET value = 'refreshed-session' WHERE key = 'cursorAuth/accessToken'"); err != nil {
		t.Fatal(err)
	}
	if matchesAccessTokenAt(ctx, path, "owner-session") || !matchesAccessTokenAt(ctx, path, "refreshed-session") {
		t.Fatal("refresh not respected")
	}
	if _, err := db.Exec("DELETE FROM ItemTable WHERE key = 'cursorAuth/accessToken'"); err != nil {
		t.Fatal(err)
	}
	if matchesAccessTokenAt(ctx, path, "refreshed-session") {
		t.Fatal("signed out identity authorized")
	}
	missing := filepath.Join(t.TempDir(), "missing.vscdb")
	if matchesAccessTokenAt(ctx, missing, "owner-session") {
		t.Fatal("missing database authorized")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("authorization created a missing database")
	}
}
