package mitm

import (
	"context"
	"net/http"
	"strings"
	"time"

	"cursor-byok/internal/cursor"
)

type localAuthorizer func(context.Context, http.Header) bool

func authorizeLocalRequest(ctx context.Context, header http.Header) bool {
	values := header.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return cursor.MatchesCurrentAccessToken(ctx, parts[1])
}

func paidLocalPath(path string) bool {
	if localBidiStream(path) {
		return true
	}
	switch path {
	case "/aiserver.v1.AiService/NameTab", "/aiserver.v1.AiService/WriteGitBranchName", "/aiserver.v1.AiService/WriteGitCommitMessage", "/aiserver.v1.AiService/StreamCpp", "/aiserver.v1.BidiService/BidiAppend":
		return true
	}
	return false
}
