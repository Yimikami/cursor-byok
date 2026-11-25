package mitm

import (
	"net/http"
	"strings"
)

// Cursor's desktop renderer uses this registered, privileged file origin.
// The Node agent host sends no Origin. Neither form authenticates other local
// processes: this boundary prevents unrelated browser pages spending BYOK
// credentials through the user's system proxy.
const cursorRendererOrigin = "vscode-file://vscode-app"

func localOriginProtectedPath(path string) bool {
	return localProxyPath(path) || path == "/aiserver.v1.BidiService/BidiAppend"
}

func allowedNativeOrigin(req *http.Request) bool {
	origins := req.Header.Values("Origin")
	if len(origins) == 0 {
		// Browsers cannot set Sec-Fetch-Site themselves. Reject their
		// cross-site requests even if an opaque request omits Origin.
		return !strings.EqualFold(req.Header.Get("Sec-Fetch-Site"), "cross-site")
	}
	return len(origins) == 1 && origins[0] == cursorRendererOrigin
}

func applyNativeCORS(req *http.Request, response *http.Response) {
	if response == nil {
		return
	}
	if response.Header == nil {
		response.Header = make(http.Header)
	}
	response.Header.Del("Access-Control-Allow-Origin")
	response.Header.Del("Access-Control-Allow-Credentials")
	if allowedNativeOrigin(req) && req.Header.Get("Origin") == cursorRendererOrigin {
		response.Header.Set("Access-Control-Allow-Origin", cursorRendererOrigin)
		response.Header.Add("Vary", "Origin")
	}
}
