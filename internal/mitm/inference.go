package mitm

import (
	"context"
	"cursor-byok/internal/agent"
	"net/http"
)

// Keep the Windows integration name while sharing the native stream lifecycle.
func handleInferenceRunSSE(req *http.Request, serverCtx context.Context, resolver agent.AdapterResolver) *http.Response {
	return handleInferenceSSE(req, serverCtx, resolver)
}
