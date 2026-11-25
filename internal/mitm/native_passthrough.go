package mitm

import (
	"net/http"
	"strings"
	"unicode"

	"cursor-byok/internal/cursor"
)

// nativePassthroughRequest forwards requests made by Cursor itself, including
// account metadata, updates, extensions and native settings. It never loads or
// injects saved credentials, generates requests, or rewrites server entitlements.
// A legacy local session must be recovered before any native authenticated call.
func nativePassthroughRequest(req *http.Request) (*http.Request, *http.Response) {
	for _, value := range req.Header.Values("Authorization") {
		for _, token := range strings.FieldsFunc(value, func(r rune) bool { return unicode.IsSpace(r) || r == ',' }) {
			if cursor.IsLocalSessionToken(token) {
				return req, makeJSONResp(req, http.StatusUnauthorized, `{"code":"unauthenticated","message":"Sign in to Cursor with your own account before using native features."}`)
			}
		}
	}
	forward := req.Clone(req.Context())
	// These headers authenticate or configure the local proxy, not Cursor's
	// upstream. Preserve native Authorization, cookies, body and request URL.
	forward.Header.Del("Proxy-Authorization")
	forward.Header.Del("Proxy-Connection")
	return forward, nil
}
