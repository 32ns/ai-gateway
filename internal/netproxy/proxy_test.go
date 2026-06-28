package netproxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestNewTransportIgnoresEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:8888")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:8888")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:8888")

	transport := NewTransport(time.Second)
	if transport.Proxy != nil {
		t.Fatal("expected transport to ignore environment proxy")
	}
}

func TestConfigureTransportAppliesConnectionLimits(t *testing.T) {
	ConfigureTransport(TransportConfig{
		MaxIdleConns:        123,
		MaxIdleConnsPerHost: 45,
		MaxConnsPerHost:     67,
	})
	t.Cleanup(func() { ConfigureTransport(TransportConfig{}) })

	transport := NewTransport(time.Second)
	if transport.MaxIdleConns != 123 {
		t.Fatalf("MaxIdleConns = %d, want 123", transport.MaxIdleConns)
	}
	if transport.MaxIdleConnsPerHost != 45 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want 45", transport.MaxIdleConnsPerHost)
	}
	if transport.MaxConnsPerHost != 67 {
		t.Fatalf("MaxConnsPerHost = %d, want 67", transport.MaxConnsPerHost)
	}
}

func TestNewHTTPClientWithoutProxyIsDirect(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:8888")

	client, err := NewHTTPClient("", time.Second, time.Second)
	if err != nil {
		t.Fatalf("NewHTTPClient returned error: %v", err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("expected direct client to ignore environment proxy")
	}
}

func TestNewTransportIgnoresDefaultTransportDialer(t *testing.T) {
	previous := http.DefaultTransport
	http.DefaultTransport = &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("custom default dialer should not be inherited")
		},
	}
	t.Cleanup(func() { http.DefaultTransport = previous })

	transport := NewTransport(time.Second)
	if transport.DialContext == nil {
		t.Fatal("expected direct dialer")
	}
	_, err := transport.DialContext(context.Background(), "tcp", "127.0.0.1:1")
	if err != nil && err.Error() == "custom default dialer should not be inherited" {
		t.Fatal("transport inherited http.DefaultTransport dialer")
	}
}

func TestConfigureTransportProxyUsesConfiguredProxyOnly(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:8888")

	transport := NewTransport(time.Second)
	if err := ConfigureTransportProxy(transport, "http://127.0.0.1:7890"); err != nil {
		t.Fatalf("ConfigureTransportProxy returned error: %v", err)
	}
	if transport.Proxy == nil {
		t.Fatal("expected configured proxy")
	}
}

func TestConfigureTransportProxyClearsProxyForSOCKS5(t *testing.T) {
	transport := NewTransport(time.Second)
	transport.Proxy = http.ProxyFromEnvironment

	if err := ConfigureTransportProxy(transport, "socks5://127.0.0.1:1080"); err != nil {
		t.Fatalf("ConfigureTransportProxy returned error: %v", err)
	}
	if transport.Proxy != nil {
		t.Fatal("expected socks5 transport to ignore HTTP environment proxy")
	}
	if transport.DialContext == nil {
		t.Fatal("expected socks5 dial context")
	}
}

func TestConfigureTransportProxyResetsPreviousDialerForHTTPProxy(t *testing.T) {
	transport := NewTransport(time.Second)
	transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("previous dialer should not be retained")
	}

	if err := ConfigureTransportProxy(transport, "http://127.0.0.1:7890"); err != nil {
		t.Fatalf("ConfigureTransportProxy returned error: %v", err)
	}
	_, err := transport.DialContext(context.Background(), "tcp", "127.0.0.1:1")
	if err != nil && err.Error() == "previous dialer should not be retained" {
		t.Fatal("http proxy transport retained previous dialer")
	}
}
