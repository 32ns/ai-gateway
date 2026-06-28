package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestOriginIgnoresForwardedHeadersFromUntrustedRemote(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://internal.local/dashboard", nil)
	req.Host = "internal.local"
	req.RemoteAddr = "198.51.100.10:54321"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "public.example.com")

	if got := requestOrigin(req); got != "http://internal.local" {
		t.Fatalf("requestOrigin = %q, want http://internal.local", got)
	}
	if got := requestDomain(req); got != "internal.local" {
		t.Fatalf("requestDomain = %q, want internal.local", got)
	}
	if requestIsHTTPS(req) {
		t.Fatal("requestIsHTTPS = true, want false")
	}
}

func TestRequestOriginTrustsForwardedHeadersFromTrustedProxy(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://internal.local/dashboard", nil)
	req.Host = "internal.local"
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("X-Forwarded-Proto", "https, http")
	req.Header.Set("X-Forwarded-Host", "public.example.com, proxy.local")

	if got := requestOrigin(req); got != "https://public.example.com" {
		t.Fatalf("requestOrigin = %q, want https://public.example.com", got)
	}
	if got := requestDomain(req); got != "public.example.com" {
		t.Fatalf("requestDomain = %q, want public.example.com", got)
	}
	if !requestIsHTTPS(req) {
		t.Fatal("requestIsHTTPS = false, want true")
	}
}

func TestRequestOriginRequiresConfiguredPrivateProxyCIDR(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://internal.local/dashboard", nil)
	req.Host = "internal.local"
	req.RemoteAddr = "10.1.2.3:54321"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "public.example.com")

	if got := requestOrigin(req); got != "http://internal.local" {
		t.Fatalf("requestOrigin without configured proxy = %q, want http://internal.local", got)
	}

	req = req.WithContext(withTrustedProxies(req.Context(), newTrustedProxySet([]string{"10.0.0.0/8"})))
	if got := requestOrigin(req); got != "https://public.example.com" {
		t.Fatalf("requestOrigin with configured proxy = %q, want https://public.example.com", got)
	}
}

func TestClientIPRequiresConfiguredPrivateProxyCIDR(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://internal.local/register", nil)
	req.RemoteAddr = "10.1.2.3:54321"
	req.Header.Set("X-Forwarded-For", "203.0.113.50")

	if got := clientIP(req); got != "10.1.2.3" {
		t.Fatalf("clientIP without configured proxy = %q, want 10.1.2.3", got)
	}

	req = req.WithContext(withTrustedProxies(req.Context(), newTrustedProxySet([]string{"10.0.0.0/8"})))
	if got := clientIP(req); got != "203.0.113.50" {
		t.Fatalf("clientIP with configured proxy = %q, want 203.0.113.50", got)
	}
}
