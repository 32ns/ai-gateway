package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDefaultsWhenDefaultConfigIsMissing(t *testing.T) {
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tempDir := t.TempDir()
	t.Cleanup(func() {
		if err := os.Chdir(originalDir); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.Chdir(tempDir); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Address != "127.0.0.1:8088" {
		t.Fatalf("default address = %q, want 127.0.0.1:8088", cfg.Address)
	}
	if cfg.ProtocolRequestBodyLimit != defaultProtocolRequestBodyLimit {
		t.Fatalf("protocol request body limit = %d, want %d", cfg.ProtocolRequestBodyLimit, defaultProtocolRequestBodyLimit)
	}
	if cfg.GatewayAuditRetentionDays != defaultGatewayAuditRetentionDays {
		t.Fatalf("gateway audit retention days = %d, want %d", cfg.GatewayAuditRetentionDays, defaultGatewayAuditRetentionDays)
	}
	if cfg.DatabaseBackend != "sqlite" {
		t.Fatalf("database backend = %q, want sqlite", cfg.DatabaseBackend)
	}
	if !filepath.IsAbs(cfg.StatePath) {
		t.Fatalf("state path is not absolute: %q", cfg.StatePath)
	}
	if _, err := os.Stat(DefaultPath); err != nil {
		t.Fatalf("default config was not created: %v", err)
	}
}

func TestLoadConfigFileOverridesDefaults(t *testing.T) {
	configPath := writeConfig(t, `{
		"host": "127.0.0.1",
		"port": "9090",
		"api_key": "seed-key",
		"audit_limit": 128,
		"gateway_audit": true,
		"gateway_audit_errors": true,
		"gateway_audit_retention_days": 3,
		"max_in_flight": 123,
		"upstream_max_idle_conns": 456,
		"upstream_max_idle_conns_per_host": 789,
		"upstream_max_conns_per_host": 321,
		"protocol_request_body_limit": 1024,
		"database_backend": "sqlite",
		"state_path": "custom/state.db",
		"master_key": "secret",
		"trusted_proxy_cidrs": ["10.0.0.0/8", "127.0.0.1"]
	}`)

	cfg, err := LoadWithOptions(Options{Path: configPath})
	if err != nil {
		t.Fatalf("LoadWithOptions returned error: %v", err)
	}
	if cfg.Address != "127.0.0.1:9090" {
		t.Fatalf("address = %q, want 127.0.0.1:9090", cfg.Address)
	}
	if cfg.APIKey != "seed-key" {
		t.Fatalf("api key = %q", cfg.APIKey)
	}
	if cfg.AuditLimit != 128 || !cfg.GatewayAudit || cfg.MaxInFlight != 123 {
		t.Fatalf("loaded limits = audit:%d gateway:%t max:%d", cfg.AuditLimit, cfg.GatewayAudit, cfg.MaxInFlight)
	}
	if !cfg.GatewayAuditErrors || cfg.GatewayAuditRetentionDays != 3 {
		t.Fatalf("gateway audit errors = %t retention = %d", cfg.GatewayAuditErrors, cfg.GatewayAuditRetentionDays)
	}
	if cfg.UpstreamMaxIdleConns != 456 || cfg.UpstreamMaxIdleConnsPerHost != 789 || cfg.UpstreamMaxConnsPerHost != 321 {
		t.Fatalf("upstream limits = %d/%d/%d", cfg.UpstreamMaxIdleConns, cfg.UpstreamMaxIdleConnsPerHost, cfg.UpstreamMaxConnsPerHost)
	}
	if cfg.ProtocolRequestBodyLimit != 1024 {
		t.Fatalf("protocol request body limit = %d, want 1024", cfg.ProtocolRequestBodyLimit)
	}
	if cfg.DatabaseBackend != "sqlite" {
		t.Fatalf("database backend = %q", cfg.DatabaseBackend)
	}
	if !strings.HasSuffix(filepath.ToSlash(cfg.StatePath), "custom/state.db") {
		t.Fatalf("state path = %q", cfg.StatePath)
	}
	if cfg.MasterKey != "secret" {
		t.Fatalf("master config not loaded: %#v", cfg)
	}
	if got := strings.Join(cfg.TrustedProxyCIDRs, ","); got != "10.0.0.0/8,127.0.0.1/32" {
		t.Fatalf("trusted proxy cidrs = %q", got)
	}
}

func TestExplicitMissingConfigCreatesDefaultFile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "nested", "missing.json")
	cfg, err := LoadWithOptions(Options{Path: configPath})
	if err != nil {
		t.Fatalf("LoadWithOptions returned error: %v", err)
	}
	if cfg.Address != "127.0.0.1:8088" {
		t.Fatalf("address = %q, want 127.0.0.1:8088", cfg.Address)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("default config was not created: %v", err)
	}
	if !strings.Contains(string(raw), `"port": "8088"`) {
		t.Fatalf("created config missing default port: %s", string(raw))
	}
}

func TestInvalidConfigReturnsError(t *testing.T) {
	configPath := writeConfig(t, `{`)
	_, err := LoadWithOptions(Options{Path: configPath})
	if err == nil {
		t.Fatal("expected invalid config error")
	}
}

func TestTrustedProxyCIDRValidation(t *testing.T) {
	cfg := Default()
	cfg.TrustedProxyCIDRs = []string{"10.0.0.0/8", "::1"}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("Normalize returned error: %v", err)
	}
	if got := strings.Join(cfg.TrustedProxyCIDRs, ","); got != "10.0.0.0/8,::1/128" {
		t.Fatalf("trusted proxy cidrs = %q", got)
	}

	cfg.TrustedProxyCIDRs = []string{"not-a-cidr"}
	if err := cfg.Normalize(); err == nil {
		t.Fatal("expected trusted proxy CIDR validation error")
	}
}

func TestDatabaseBackendValidation(t *testing.T) {
	cfg := Default()
	cfg.DatabaseBackend = "postgres"
	cfg.PostgresDSN = "postgres://user:pass@localhost:5432/ai_gateway?sslmode=disable"
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("Normalize returned error: %v", err)
	}
	if cfg.DatabaseBackend != "postgres" {
		t.Fatalf("database backend = %q, want postgres", cfg.DatabaseBackend)
	}

	cfg.DatabaseBackend = "postgres"
	cfg.PostgresDSN = ""
	if err := cfg.Normalize(); err == nil {
		t.Fatal("expected postgres DSN validation error")
	}
}

func TestValidatePort(t *testing.T) {
	valid := []string{"", "1", "8088", ":65535"}
	for _, port := range valid {
		if err := ValidatePort(port); err != nil {
			t.Fatalf("ValidatePort(%q) returned error: %v", port, err)
		}
	}

	invalid := []string{":", "0", "65536", "abc", "127.0.0.1:8088"}
	for _, port := range invalid {
		if err := ValidatePort(port); err == nil {
			t.Fatalf("ValidatePort(%q) returned nil error", port)
		}
	}
}

func TestValidateHost(t *testing.T) {
	valid := []string{"", "localhost", "127.0.0.1", "0.0.0.0", "::1"}
	for _, host := range valid {
		if err := ValidateHost(host); err != nil {
			t.Fatalf("ValidateHost(%q) returned error: %v", host, err)
		}
	}

	invalid := []string{"example.com", "127.0.0.1:8088", "bad host"}
	for _, host := range invalid {
		if err := ValidateHost(host); err == nil {
			t.Fatalf("ValidateHost(%q) returned nil error", host)
		}
	}
}

func TestHostIsLoopback(t *testing.T) {
	loopback := []string{"", "localhost", "127.0.0.1", "::1"}
	for _, host := range loopback {
		if !HostIsLoopback(host) {
			t.Fatalf("HostIsLoopback(%q) = false, want true", host)
		}
	}
	if HostIsLoopback("0.0.0.0") {
		t.Fatal("HostIsLoopback(0.0.0.0) = true, want false")
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
