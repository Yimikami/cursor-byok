package mitm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	aiserverv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
	"google.golang.org/protobuf/proto"
)

func TestNativeGenerationThroughTLSProxy(t *testing.T) {
	for _, tc := range []struct {
		method, answer, context string
		request, response       proto.Message
	}{
		{"NameTab", "Dosya okuma testi", "Read the marker", &aiserverv1.NameTabRequest{Messages: []*aiserverv1.ConversationMessage{{Text: "Read the marker"}}}, &aiserverv1.NameTabResponse{}},
		{"WriteGitBranchName", "fix/native-read", "fix broken reading", &aiserverv1.WriteGitBranchNameRequest{Diffs: "+return read(file)", Context: proto.String("fix broken reading")}, &aiserverv1.WriteGitBranchNameResponse{}},
	} {
		t.Run(tc.method, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				wire, _ := json.Marshal(request)
				if request["stream"] != true || request["model"] != "test-model" || !bytes.Contains(wire, []byte(tc.context)) {
					t.Errorf("wrong generation request: %s", wire)
				}
				if limit, ok := request["max_tokens"].(float64); ok && limit > 512 {
					t.Error("short generation output limit lost")
				}
				answer, _ := json.Marshal(tc.answer)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", answer)
			}))
			defer provider.Close()
			_, client := testProxy(t, provider.URL)
			body, _ := proto.Marshal(tc.request)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "POST", "https://api2.cursor.sh/aiserver.v1.AiService/"+tc.method, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/proto")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			wire, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				t.Fatalf("generation status=%d body=%s", resp.StatusCode, wire)
			}
			if err := proto.Unmarshal(wire, tc.response); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(fmt.Sprint(tc.response), tc.answer) {
				t.Fatalf("native response missing answer: %v", tc.response)
			}
		})
	}
}

func TestNativeGenerationRejectsProviderFailure(t *testing.T) {
	for _, answer := range []string{"", "invalid branch name", "@{-1}", "fix/path.lock"} {
		t.Run(answer, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				encoded, _ := json.Marshal(answer)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", encoded)
			}))
			defer provider.Close()
			_, client := testProxy(t, provider.URL)
			body, _ := proto.Marshal(&aiserverv1.WriteGitBranchNameRequest{Context: proto.String("test change")})
			resp, err := client.Post("https://api2.cursor.sh/aiserver.v1.AiService/WriteGitBranchName", "application/proto", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("invalid model output got status %d", resp.StatusCode)
			}
		})
	}
}
