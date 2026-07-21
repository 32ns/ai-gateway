package providers

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestResponsesWebSocketDialerForcesHTTP1ALPN(t *testing.T) {
	originalTransport := http.DefaultTransport
	originalTLSConfig := &tls.Config{
		ServerName: "upstream.example",
		NextProtos: []string{"h2", "http/1.1"},
		MinVersion: tls.VersionTLS12,
	}
	http.DefaultTransport = &http.Transport{TLSClientConfig: originalTLSConfig}
	t.Cleanup(func() {
		http.DefaultTransport = originalTransport
	})

	dialer, err := responsesWebSocketDialer("")
	if err != nil {
		t.Fatalf("responsesWebSocketDialer returned error: %v", err)
	}
	if dialer.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig is nil")
	}
	if dialer.TLSClientConfig == originalTLSConfig {
		t.Fatal("TLSClientConfig reused the HTTP transport config")
	}
	if got := dialer.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" {
		t.Fatalf("NextProtos = %v, want [http/1.1]", got)
	}
	if dialer.TLSClientConfig.ServerName != originalTLSConfig.ServerName || dialer.TLSClientConfig.MinVersion != originalTLSConfig.MinVersion {
		t.Fatalf("TLS config fields were not preserved: %#v", dialer.TLSClientConfig)
	}
	if got := originalTLSConfig.NextProtos; len(got) != 2 || got[0] != "h2" || got[1] != "http/1.1" {
		t.Fatalf("original NextProtos mutated: %v", got)
	}
}

func TestResponsesWebSocketDialerConnectsToHTTP2CapableTLSServer(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		_ = conn.Close()
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	originalTransport := http.DefaultTransport
	http.DefaultTransport = &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, // Test server certificate.
		NextProtos:         []string{"h2", "http/1.1"},
	}}
	t.Cleanup(func() {
		http.DefaultTransport = originalTransport
	})

	dialer, err := responsesWebSocketDialer("")
	if err != nil {
		t.Fatalf("responsesWebSocketDialer returned error: %v", err)
	}
	wsURL := "wss" + strings.TrimPrefix(server.URL, "https")
	conn, response, err := dialer.Dial(wsURL, nil)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		t.Fatalf("websocket dial returned error: %v", err)
	}
	defer conn.Close()
}
