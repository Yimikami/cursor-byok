package agent

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// These unary RPCs are still called by Cursor 3.32.4's workbench. Newer
// credential fields are intentionally ignored: the selected BYOK adapter is
// the credential authority, as it is for WriteGitCommitMessage.
func HandleNativeText(ctx context.Context, path string, body []byte, contentType string, resolve AdapterResolver, selectedModel string) Result {
	switch path {
	case "/aiserver.v1.AiService/NameTab":
		return HandleNameTab(ctx, body, contentType, resolve, selectedModel)
	case "/aiserver.v1.AiService/WriteGitBranchName":
		return HandleWriteGitBranchName(ctx, body, contentType, resolve, selectedModel)
	default:
		return errResult(http.StatusNotFound, "unsupported native text method")
	}
}

func validConversationTitle(text string) bool {
	return strings.TrimSpace(text) != "" && len(strings.Fields(text)) <= 8 && utf8.RuneCountInString(text) <= 80 &&
		!strings.ContainsFunc(text, unicode.IsControl)
}

func decodeNativeUnary(body []byte, contentType string, request proto.Message) error {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "application/json") {
		return (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, request)
	}
	return decodeUnary(body, contentType, request)
}

func nativeUnaryResult(response proto.Message, requestContentType string) Result {
	contentType := "application/proto"
	var body []byte
	var err error
	if strings.Contains(strings.ToLower(requestContentType), "json") {
		contentType = "application/json"
		body, err = protojson.Marshal(response)
	} else {
		body, err = proto.Marshal(response)
	}
	if err != nil {
		return errResult(http.StatusInternalServerError, "encode native text response: "+err.Error())
	}
	return Result{Status: http.StatusOK, ContentType: contentType, Body: body}
}

func validateBranchName(name string) error {
	if !validGeneratedBranchName(name) {
		return fmt.Errorf("provider returned an invalid Git branch name")
	}
	return nil
}
