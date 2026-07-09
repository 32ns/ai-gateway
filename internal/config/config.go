package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Host                        string   `json:"host"`
	Port                        string   `json:"port"`
	Address                     string   `json:"-"`
	APIKey                      string   `json:"api_key"`
	AuditLimit                  int      `json:"audit_limit"`
	GatewayAudit                bool     `json:"gateway_audit"`
	GatewayAuditErrors          bool     `json:"gateway_audit_errors"`
	GatewayAuditRetentionDays   int      `json:"gateway_audit_retention_days"`
	MaxInFlight                 int      `json:"max_in_flight"`
	UpstreamMaxIdleConns        int      `json:"upstream_max_idle_conns"`
	UpstreamMaxIdleConnsPerHost int      `json:"upstream_max_idle_conns_per_host"`
	UpstreamMaxConnsPerHost     int      `json:"upstream_max_conns_per_host"`
	ProtocolRequestBodyLimit    int      `json:"protocol_request_body_limit"`
	DatabaseBackend             string   `json:"database_backend"`
	PostgresDSN                 string   `json:"postgres_dsn,omitempty"`
	StatePath                   string   `json:"state_path"`
	MasterKey                   string   `json:"master_key"`
	PublicBaseURL               string   `json:"public_base_url"`
	TrustedProxyCIDRs           []string `json:"trusted_proxy_cidrs,omitempty"`
}

const (
	DefaultPath                        = "config.json"
	defaultAuditLimit                  = 512
	defaultGatewayAuditRetentionDays   = 1
	defaultMaxInFlight                 = 1024
	defaultUpstreamMaxIdleConns        = 512
	defaultUpstreamMaxIdleConnsPerHost = 128
	defaultProtocolRequestBodyLimit    = 64 << 20
	defaultHost                        = "127.0.0.1"
	defaultPort                        = "8088"
)

var defaultTrustedProxyCIDRs = []string{"127.0.0.1/32", "::1/128"}

func Load() (Config, error) {
	return LoadWithOptions(Options{})
}

type Options struct {
	Path string
	Host string
	Port string
}

func LoadWithOptions(options Options) (Config, error) {
	path := strings.TrimSpace(options.Path)
	if path == "" {
		path = DefaultPath
	}

	cfg := Default()
	if raw, err := os.ReadFile(path); err == nil {
		if len(strings.TrimSpace(string(raw))) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return Config{}, fmt.Errorf("parse config %s: %w", path, err)
			}
		}
	} else if os.IsNotExist(err) {
		if err := writeDefaultConfig(path); err != nil {
			return Config{}, err
		}
	} else {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}

	if strings.TrimSpace(options.Host) != "" {
		cfg.Host = options.Host
	}
	if strings.TrimSpace(options.Port) != "" {
		cfg.Port = options.Port
	}
	if err := cfg.Normalize(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Default() Config {
	return Config{
		Host:                        defaultHost,
		Port:                        defaultPort,
		AuditLimit:                  defaultAuditLimit,
		GatewayAuditRetentionDays:   defaultGatewayAuditRetentionDays,
		MaxInFlight:                 defaultMaxInFlight,
		UpstreamMaxIdleConns:        defaultUpstreamMaxIdleConns,
		UpstreamMaxIdleConnsPerHost: defaultUpstreamMaxIdleConnsPerHost,
		UpstreamMaxConnsPerHost:     0,
		ProtocolRequestBodyLimit:    defaultProtocolRequestBodyLimit,
		DatabaseBackend:             "sqlite",
		StatePath:                   "data/state.db",
		TrustedProxyCIDRs:           append([]string(nil), defaultTrustedProxyCIDRs...),
	}
}

func writeDefaultConfig(path string) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create config directory %s: %w", dir, err)
		}
	}
	raw, err := json.MarshalIndent(Default(), "", "  ")
	if err != nil {
		return fmt.Errorf("marshal default config: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("write default config %s: %w", path, err)
	}
	return nil
}

func (cfg *Config) Normalize() error {
	cfg.Host = normalizeHost(cfg.Host, defaultHost)
	cfg.Port = normalizePort(cfg.Port, defaultPort)
	cfg.Address = net.JoinHostPort(cfg.Host, cfg.Port)

	if cfg.AuditLimit <= 0 {
		cfg.AuditLimit = defaultAuditLimit
	}
	if cfg.GatewayAuditRetentionDays <= 0 {
		cfg.GatewayAuditRetentionDays = defaultGatewayAuditRetentionDays
	}
	if cfg.MaxInFlight < 0 {
		cfg.MaxInFlight = defaultMaxInFlight
	}
	if cfg.UpstreamMaxIdleConns < 0 {
		cfg.UpstreamMaxIdleConns = defaultUpstreamMaxIdleConns
	}
	if cfg.UpstreamMaxIdleConnsPerHost < 0 {
		cfg.UpstreamMaxIdleConnsPerHost = defaultUpstreamMaxIdleConnsPerHost
	}
	if cfg.UpstreamMaxConnsPerHost < 0 {
		cfg.UpstreamMaxConnsPerHost = 0
	}
	if cfg.ProtocolRequestBodyLimit <= 0 {
		cfg.ProtocolRequestBodyLimit = defaultProtocolRequestBodyLimit
	}
	cfg.DatabaseBackend = strings.ToLower(strings.TrimSpace(cfg.DatabaseBackend))
	if cfg.DatabaseBackend == "" {
		cfg.DatabaseBackend = "sqlite"
	}
	switch cfg.DatabaseBackend {
	case "sqlite":
	case "postgres":
		if strings.TrimSpace(cfg.PostgresDSN) == "" {
			return fmt.Errorf("postgres_dsn is required when database_backend is postgres")
		}
	default:
		return fmt.Errorf("invalid database_backend %q: expected sqlite or postgres", cfg.DatabaseBackend)
	}
	if strings.TrimSpace(cfg.StatePath) == "" {
		cfg.StatePath = "data/state.db"
	}

	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.PostgresDSN = strings.TrimSpace(cfg.PostgresDSN)
	cfg.MasterKey = strings.TrimSpace(cfg.MasterKey)
	cfg.PublicBaseURL = strings.TrimRight(strings.TrimSpace(cfg.PublicBaseURL), "/")
	trustedProxyCIDRs, err := normalizeTrustedProxyCIDRs(cfg.TrustedProxyCIDRs)
	if err != nil {
		return err
	}
	cfg.TrustedProxyCIDRs = trustedProxyCIDRs
	cfg.StatePath = absolutePath(cfg.StatePath)
	return nil
}

func ValidatePort(port string) error {
	trimmed := strings.TrimSpace(port)
	if trimmed == "" {
		return nil
	}
	normalized := strings.TrimPrefix(trimmed, ":")
	parsed := 0
	for _, r := range normalized {
		if r < '0' || r > '9' {
			return fmt.Errorf("invalid port %q: expected 1-65535", port)
		}
		parsed = parsed*10 + int(r-'0')
	}
	if parsed < 1 || parsed > 65535 {
		return fmt.Errorf("invalid port %q: expected 1-65535", port)
	}
	return nil
}

func ValidateHost(host string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return nil
	}
	return fmt.Errorf("invalid host %q: expected IP address or localhost", host)
}

func HostIsLoopback(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func normalizePort(raw, fallback string) string {
	port := strings.TrimPrefix(strings.TrimSpace(raw), ":")
	if err := ValidatePort(port); err != nil {
		return fallback
	}
	if port == "" {
		return fallback
	}
	return port
}

func normalizeHost(raw, fallback string) string {
	host := strings.TrimSpace(raw)
	if err := ValidateHost(host); err != nil {
		return fallback
	}
	if host == "" {
		return fallback
	}
	return host
}

func normalizeTrustedProxyCIDRs(values []string) ([]string, error) {
	if len(values) == 0 {
		return append([]string(nil), defaultTrustedProxyCIDRs...), nil
	}
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		prefix, err := parseTrustedProxyPrefix(value)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted_proxy_cidrs entry %q: %w", value, err)
		}
		key := prefix.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, key)
	}
	if len(normalized) == 0 {
		return append([]string(nil), defaultTrustedProxyCIDRs...), nil
	}
	return normalized, nil
}

func parseTrustedProxyPrefix(value string) (netip.Prefix, error) {
	if strings.Contains(value, "/") {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return netip.Prefix{}, err
		}
		return prefix.Masked(), nil
	}
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

func absolutePath(path string) string {
	if absPath, err := filepath.Abs(path); err == nil {
		return absPath
	}
	return path
}
