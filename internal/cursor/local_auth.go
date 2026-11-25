package cursor

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"net/url"
	"path/filepath"
	"strings"
)

// MatchesCurrentAccessToken binds paid local inference to the Cursor account
// owned by the process user. Read at each run so account refresh/sign-out takes
// effect without a proxy restart. Never return or log the stored credential.
func MatchesCurrentAccessToken(ctx context.Context, token string) bool {
	return matchesAccessTokenAt(ctx, stateDBPath(), token)
}

func matchesAccessTokenAt(ctx context.Context, path, token string) bool {
	if path == "" || strings.TrimSpace(token) == "" || IsLocalSessionToken(token) {
		return false
	}
	uriPath := filepath.ToSlash(path)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	uri := (&url.URL{Scheme: "file", Path: uriPath}).String() + "?mode=ro&_pragma=busy_timeout=1000"
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return false
	}
	defer db.Close()
	var current string
	if err = db.QueryRowContext(ctx, "SELECT value FROM ItemTable WHERE key = ?", "cursorAuth/accessToken").Scan(&current); err != nil {
		return false
	}
	// Cursor stores Windows credentials in safeStorage but sends the decrypted
	// token on requests. Identify it in memory; ciphertext or unavailable crypto
	// must never authorize a local provider call, including a migrated fake.
	current, err = decodeCursorAuthToken(path, current)
	if err != nil || ctx.Err() != nil || IsLocalSessionToken(current) {
		return false
	}
	return strings.TrimSpace(current) != "" && subtle.ConstantTimeCompare([]byte(token), []byte(current)) == 1
}
