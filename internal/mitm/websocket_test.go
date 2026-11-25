package mitm

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"cursor-byok/internal/agent"
	"cursor-byok/internal/certs"
	agentv1 "cursor-byok/internal/protocodec/gen/agent/v1"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func wsBytes(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func wsID(run, request string) []byte {
	return append(wsBytes(1, []byte(run)), wsBytes(2, []byte(request))...)
}
func wsField(t *testing.T, b []byte, want protowire.Number) []byte {
	t.Helper()
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			t.Fatal("invalid tag")
		}
		b = b[k:]
		if typ == protowire.BytesType {
			v, k := protowire.ConsumeBytes(b)
			if k < 0 {
				t.Fatal("invalid bytes")
			}
			if n == want {
				return v
			}
			b = b[k:]
		} else {
			k := protowire.ConsumeFieldValue(n, typ, b)
			if k < 0 {
				t.Fatal("invalid field")
			}
			b = b[k:]
		}
	}
	return nil
}
func wsSequence(t *testing.T, b []byte) uint64 {
	t.Helper()
	for len(b) > 0 {
		n, typ, k := protowire.ConsumeTag(b)
		if k < 0 {
			t.Fatal("invalid tag")
		}
		b = b[k:]
		if n == 2 && typ == protowire.VarintType {
			v, k := protowire.ConsumeVarint(b)
			if k < 0 {
				t.Fatal("invalid seq")
			}
			return v
		}
		k = protowire.ConsumeFieldValue(n, typ, b)
		if k < 0 {
			t.Fatal("invalid field")
		}
		b = b[k:]
	}
	return 0
}
func wsRunMessage(t *testing.T, text string) []byte {
	t.Helper()
	b, err := proto.Marshal(&agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_RunRequest{RunRequest: &agentv1.AgentRunRequest{Action: &agentv1.ConversationAction{Action: &agentv1.ConversationAction_UserMessageAction{UserMessageAction: &agentv1.UserMessageAction{UserMessage: &agentv1.UserMessage{Text: text, Mode: agentv1.AgentMode_AGENT_MODE_ASK}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func wsClientMessage(run string, seq uint64, body []byte) []byte {
	fields := wsBytes(1, []byte(run))
	fields = protowire.AppendVarint(protowire.AppendTag(fields, 2, protowire.VarintType), seq)
	fields = append(fields, wsBytes(3, body)...)
	return wsBytes(3, fields)
}

var proxyTestCA struct {
	sync.Once
	ca  *certs.CA
	err error
}

func testProxy(t *testing.T, provider string) (*Server, *http.Client) {
	return testProxyWithAuthorizer(t, provider, func(context.Context, http.Header) bool { return true })
}

func testProxyWithAuthorizer(t *testing.T, provider string, authorize localAuthorizer) (*Server, *http.Client) {
	t.Helper()
	proxyTestCA.Do(func() { proxyTestCA.ca, proxyTestCA.err = certs.LoadOrCreate(t.TempDir()) })
	ca, err := proxyTestCA.ca, proxyTestCA.err
	if err != nil {
		t.Fatal(err)
	}
	s, err := newWithAuthorizer("127.0.0.1:0", ca, nil, func() []agent.AdapterTarget {
		if provider == "" {
			return nil
		}
		return []agent.AdapterTarget{{ProviderType: "openai", BaseURL: provider, Model: "test-model", APIKey: "test-only"}}
	}, func(string) string { return "" }, authorize)
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
	return s, &http.Client{Transport: transport}
}
func wsConnect(t *testing.T, ctx context.Context, client *http.Client, protocol string) *websocket.Conn {
	t.Helper()
	c, resp, err := websocket.Dial(ctx, "wss://api2.cursor.sh/agent/v1/run", &websocket.DialOptions{HTTPClient: client, Subprotocols: []string{protocol}})
	if err != nil {
		t.Fatalf("upgrade: %v (%v)", err, resp)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	if c.Subprotocol() != protocol {
		t.Fatalf("subprotocol %q", c.Subprotocol())
	}
	// Literal protobuf fixture: hello { protocol_version: V1 }.
	if err = c.Write(ctx, websocket.MessageBinary, []byte{0x0a, 0x02, 0x08, 0x01}); err != nil {
		t.Fatal(err)
	}
	typ, b, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageBinary || !bytes.Equal(b, []byte{0x0a, 0x02, 0x08, 0x01}) {
		t.Fatalf("hello = %x", b)
	}
	return c
}
func wsStart(t *testing.T, ctx context.Context, c *websocket.Conn) {
	t.Helper()
	identity := wsID("run-1", "request-1")
	if err := c.Write(ctx, websocket.MessageBinary, wsBytes(2, wsBytes(1, identity))); err != nil {
		t.Fatal(err)
	}
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := wsField(t, wsField(t, b, 2), 1); !bytes.Equal(got, identity) {
		t.Fatalf("accepted identity = %x", got)
	}
}

func wsReadEnd(t *testing.T, ctx context.Context, c *websocket.Conn) []byte {
	t.Helper()
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if end := wsField(t, b, 4); end != nil {
			return end
		}
		if wsField(t, b, 3) == nil {
			t.Fatalf("unexpected frame %x", b)
		}
	}
}

func TestAgentWebSocketThroughTLSProxy(t *testing.T) {
	for _, protocol := range []string{"cursor-agent-v1", "cursor-agent-preconnect-v1"} {
		t.Run(protocol, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/chat/completions" {
					t.Errorf("provider path = %s", r.URL.Path)
				}
				var req struct {
					Model    string `json:"model"`
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if req.Model != "test-model" {
					t.Errorf("model = %q", req.Model)
				}
				found := false
				for _, m := range req.Messages {
					if strings.Contains(m.Content, "hello websocket") {
						found = true
					}
				}
				if !found {
					t.Error("user prompt missing")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello back\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer provider.Close()
			_, client := testProxy(t, provider.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c := wsConnect(t, ctx, client, protocol)
			wsStart(t, ctx, c)
			if err := c.Write(ctx, websocket.MessageBinary, wsClientMessage("run-1", 0, wsRunMessage(t, "hello websocket"))); err != nil {
				t.Fatal(err)
			}
			if err := c.Write(ctx, websocket.MessageBinary, wsBytes(4, wsBytes(1, []byte("run-1")))); err != nil {
				t.Fatal(err)
			}
			var sequence uint64
			var text strings.Builder
			turnEnded := false
			for {
				_, b, err := c.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if end := wsField(t, b, 4); end != nil {
					if e := wsField(t, end, 3); e != nil {
						t.Fatalf("run error %x", e)
					}
					break
				}
				message := wsField(t, b, 3)
				if message == nil {
					t.Fatalf("unexpected frame %x", b)
				}
				if string(wsField(t, message, 1)) != "run-1" || wsSequence(t, message) != sequence {
					t.Fatal("wrong run or sequence")
				}
				sequence++
				var msg agentv1.AgentServerMessage
				if err = proto.Unmarshal(wsField(t, message, 3), &msg); err != nil {
					t.Fatal(err)
				}
				if u := msg.GetInteractionUpdate(); u != nil {
					if d := u.GetTextDelta(); d != nil {
						text.WriteString(d.Text)
					}
					turnEnded = turnEnded || u.GetTurnEnded() != nil
				}
			}
			if text.String() != "hello back" || !turnEnded {
				t.Fatalf("response = %q turnEnded=%v", text.String(), turnEnded)
			}
		})
	}
}

func TestAgentWebSocketCancellation(t *testing.T) {
	for _, action := range []string{"cancel-frame", "disconnect", "stop-proxy"} {
		t.Run(action, func(t *testing.T) {
			requested, canceled := make(chan struct{}), make(chan struct{})
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				close(requested)
				<-r.Context().Done()
				close(canceled)
			}))
			defer provider.Close()
			s, client := testProxy(t, provider.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c := wsConnect(t, ctx, client, "cursor-agent-v1")
			wsStart(t, ctx, c)
			if err := c.Write(ctx, websocket.MessageBinary, wsClientMessage("run-1", 0, wsRunMessage(t, "cancel me"))); err != nil {
				t.Fatal(err)
			}
			select {
			case <-requested:
			case <-ctx.Done():
				t.Fatal("provider was not called")
			}
			switch action {
			case "cancel-frame":
				if err := c.Write(ctx, websocket.MessageBinary, wsBytes(5, wsBytes(1, []byte("run-1")))); err != nil {
					t.Fatal(err)
				}
				if wsField(t, wsReadEnd(t, ctx, c), 3) == nil {
					t.Fatal("cancellation did not return RunEnd error")
				}
			case "disconnect":
				c.CloseNow()
			case "stop-proxy":
				if err := s.Stop(ctx); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-canceled:
			case <-ctx.Done():
				t.Fatal("provider request leaked after cancellation")
			}
		})
	}
}

func TestAgentWebSocketRejectsBadSequence(t *testing.T) {
	_, client := testProxy(t, "http://127.0.0.1:1")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c := wsConnect(t, ctx, client, "cursor-agent-v1")
	wsStart(t, ctx, c)
	if err := c.Write(ctx, websocket.MessageBinary, wsClientMessage("run-1", 2, wsRunMessage(t, "hello"))); err != nil {
		t.Fatal(err)
	}
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if wsField(t, wsField(t, b, 4), 3) == nil {
		t.Fatal("bad sequence accepted")
	}
}

func TestAgentWebSocketRequiresUpgrade(t *testing.T) {
	_, client := testProxy(t, "")
	resp, err := client.Get("https://api2.cursor.sh/agent/v1/run")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestAgentWebSocketProviderErrors(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			base := ""
			if configured {
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Error(w, "invalid test key", http.StatusUnauthorized)
				}))
				defer provider.Close()
				base = provider.URL
			}
			_, client := testProxy(t, base)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			c := wsConnect(t, ctx, client, "cursor-agent-v1")
			wsStart(t, ctx, c)
			if err := c.Write(ctx, websocket.MessageBinary, wsClientMessage("run-1", 0, wsRunMessage(t, "hello"))); err != nil {
				t.Fatal(err)
			}
			e := wsField(t, wsReadEnd(t, ctx, c), 3)
			if e == nil {
				t.Fatal("missing provider RunEnd error")
			}
			message := string(wsField(t, e, 2))
			if !configured && !strings.Contains(message, "no BYOK adapter configured") {
				t.Fatalf("error = %q", message)
			}
			if configured && !strings.Contains(message, "401") {
				t.Fatalf("error = %q", message)
			}
		})
	}
}
