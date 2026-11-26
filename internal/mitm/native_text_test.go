package mitm

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	aiserverv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
	"google.golang.org/protobuf/proto"
)

func TestNativeTextRoutesThroughTLSProxy(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"fix/repair-cursor\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	_, client := testProxy(t, provider.URL)
	for _, tc := range []struct {
		method string
		input  proto.Message
	}{
		{"NameTab", &aiserverv1.NameTabRequest{Messages: []*aiserverv1.ConversationMessage{{Text: "Fix Cursor"}}}},
		{"WriteGitBranchName", &aiserverv1.WriteGitBranchNameRequest{Diffs: "+change"}},
	} {
		t.Run(tc.method, func(t *testing.T) {
			raw, err := proto.Marshal(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			var compressed bytes.Buffer
			gz := gzip.NewWriter(&compressed)
			_, _ = gz.Write(raw)
			_ = gz.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api2.cursor.sh/aiserver.v1.AiService/"+tc.method, &compressed)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/proto")
			req.Header.Set("Content-Encoding", "gzip")
			response, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s err=%v", response.StatusCode, body, err)
			}
			// Both retained response schemas use field 1 for the text.
			if got := string(wsField(t, body, 1)); got != "fix/repair-cursor" {
				t.Fatalf("response field 1 = %q", got)
			}
		})
	}
}
