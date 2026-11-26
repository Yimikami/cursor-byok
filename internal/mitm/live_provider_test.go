package mitm

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cursor-byok/internal/agent"
	"cursor-byok/internal/certs"
	agentv1 "cursor-byok/internal/protocodec/gen/agent/v1"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
)

// Opt-in only: this spends provider tokens. It uses a temporary CA/client
// trust pool and scratch history, never the OS proxy or installed Cursor data.
// The Read executor below is a protocol fixture, not a Cursor UI acceptance test.
func TestLiveProviderNativeProtocol(t *testing.T) {
	configPath := os.Getenv("CURSOR_BYOK_LIVE_CONFIG")
	if configPath == "" {
		t.Skip("set CURSOR_BYOK_LIVE_CONFIG to explicitly enable paid provider tests")
	}
	b, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		ModelAdapters []struct {
			Type, BaseURL, APIKey, ModelID string
			SupportedThinkingLevels        []string
		}
	}
	if err := json.Unmarshal(b, &cfg); err != nil || len(cfg.ModelAdapters) == 0 {
		t.Fatal("missing live adapter")
	}
	a := cfg.ModelAdapters[0]
	target := agent.AdapterTarget{ProviderType: a.Type, BaseURL: a.BaseURL, APIKey: a.APIKey, Model: a.ModelID, Opts: agent.AdapterOpts{MaxOutputTokens: 1024}}
	for _, level := range a.SupportedThinkingLevels {
		if level == "none" {
			target.Opts.ReasoningEffort = "none"
			break
		}
	}
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ca, err := certs.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// This opt-in test validates provider/protocol behavior without reading
	// the real Cursor account; production New always uses account matching.
	s, err := newWithAuthorizer("127.0.0.1:0", ca, nil, func() []agent.AdapterTarget { return []agent.AdapterTarget{target} }, func(string) string { return target.Model }, func(context.Context, http.Header) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	proxyURL, _ := url.Parse("http://" + s.ln.Addr().String())
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.CertPEM())
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 120 * time.Second}
	t.Run("streaming-probe", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		answer, err := agent.ProbeAdapter(ctx, target)
		if err != nil || strings.TrimSpace(answer) == "" {
			t.Fatalf("probe: %v", err)
		}
	})
	t.Run("websocket-read-roundtrip", func(t *testing.T) {
		fixture := filepath.Join(t.TempDir(), "probe.txt")
		const token = "NATIVE_TOOL_ROUNDTRIP_20261125"
		if err := os.WriteFile(fixture, []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		c := wsConnect(t, ctx, client, "cursor-agent-v1")
		wsStart(t, ctx, c)
		prompt := "Use the Read tool to read exactly " + fixture + ". Do not use any other tool. After reading, reply only with the exact content of that file."
		if err := c.Write(ctx, websocket.MessageBinary, wsClientMessage("run-1", 0, wsRunMessage(t, prompt))); err != nil {
			t.Fatal(err)
		}
		var answer strings.Builder
		readCount := 0
		clientSeq := uint64(1)
		for {
			_, frame, err := c.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if end := wsField(t, frame, 4); end != nil {
				if e := wsField(t, end, 3); e != nil {
					t.Fatal("provider run ended with error")
				}
				break
			}
			message := wsField(t, frame, 3)
			if message == nil {
				t.Fatal("missing server message")
			}
			var msg agentv1.AgentServerMessage
			if err := proto.Unmarshal(wsField(t, message, 3), &msg); err != nil {
				t.Fatal(err)
			}
			if u := msg.GetInteractionUpdate(); u != nil {
				if d := u.GetTextDelta(); d != nil {
					answer.WriteString(d.Text)
				}
			}
			if exec := msg.GetExecServerMessage(); exec != nil {
				args := exec.GetReadArgs()
				if args == nil || filepath.Clean(args.Path) != filepath.Clean(fixture) {
					t.Fatal("provider requested a tool outside the read fixture")
				}
				content, err := os.ReadFile(fixture)
				if err != nil {
					t.Fatal(err)
				}
				readCount++
				result := &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: &agentv1.ExecClientMessage{Id: exec.Id, ExecId: exec.ExecId, Message: &agentv1.ExecClientMessage_ReadResult{ReadResult: &agentv1.ReadResult{Result: &agentv1.ReadResult_Success{Success: &agentv1.ReadSuccess{Path: fixture, TotalLines: 1, FileSize: int64(len(content)), Output: &agentv1.ReadSuccess_Content{Content: string(content)}}}}}}}}
				body, _ := proto.Marshal(result)
				if err := c.Write(ctx, websocket.MessageBinary, wsClientMessage("run-1", clientSeq, body)); err != nil {
					t.Fatal(err)
				}
				clientSeq++
			}
		}
		if readCount == 0 || !strings.Contains(answer.String(), token) {
			t.Fatalf("missing tool-derived answer (reads=%d)", readCount)
		}
	})
}
