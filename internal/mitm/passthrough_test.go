package mitm

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Non-Cursor applications must receive their upstream certificate, even
// while the OS routes their HTTPS requests through cursor-byok's listener.
func TestNonCursorHTTPSKeepsUpstreamCertificate(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ordinary-app" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, "upstream response")
	}))
	defer upstream.Close()
	_, client := testProxy(t, "")
	client.Timeout = 5 * time.Second
	transport := client.Transport.(*http.Transport)
	transport.TLSClientConfig.RootCAs.AddCert(upstream.Certificate())
	resp, err := client.Get(upstream.URL + "/ordinary-app")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 || !bytes.Equal(resp.TLS.PeerCertificates[0].Raw, upstream.Certificate().Raw) {
		t.Fatal("non-Cursor HTTPS certificate was replaced by the proxy")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || string(body) != "upstream response" {
		t.Fatalf("passthrough status=%d body=%q err=%v", resp.StatusCode, body, err)
	}
}
