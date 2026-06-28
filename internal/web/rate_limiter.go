package web

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

type trustedProxyContextKey struct{}

type trustedProxySet struct {
	prefixes []netip.Prefix
}

var defaultTrustedProxies = trustedProxySet{prefixes: mustTrustedProxyPrefixes("127.0.0.1/32", "::1/128")}

type ipRateLimiter struct {
	mu          sync.Mutex
	hits        map[string][]time.Time
	lastCleanup time.Time
}

func newIPRateLimiter() *ipRateLimiter {
	return &ipRateLimiter{hits: make(map[string][]time.Time)}
}

func (l *ipRateLimiter) allow(scope, ip string, limit int, window time.Duration) bool {
	if l == nil || limit <= 0 || window <= 0 {
		return true
	}
	now := time.Now().UTC()
	cutoff := now.Add(-window)
	key := rateLimiterKey(scope, ip)

	l.mu.Lock()
	defer l.mu.Unlock()

	l.cleanupLocked(cutoff, window, now)

	values := retainAfter(l.hits[key], cutoff)
	if len(values) >= limit {
		l.hits[key] = values
		return false
	}
	values = append(values, now)
	l.hits[key] = values
	return true
}

func (l *ipRateLimiter) blocked(scope, ip string, limit int, window time.Duration) bool {
	if l == nil || limit <= 0 || window <= 0 {
		return false
	}
	now := time.Now().UTC()
	cutoff := now.Add(-window)
	key := rateLimiterKey(scope, ip)

	l.mu.Lock()
	defer l.mu.Unlock()

	l.cleanupLocked(cutoff, window, now)
	values := retainAfter(l.hits[key], cutoff)
	if len(values) == 0 {
		delete(l.hits, key)
		return false
	}
	l.hits[key] = values
	return len(values) >= limit
}

func (l *ipRateLimiter) reset(scope, ip string) {
	if l == nil {
		return
	}
	key := rateLimiterKey(scope, ip)
	l.mu.Lock()
	delete(l.hits, key)
	l.mu.Unlock()
}

func (l *ipRateLimiter) cleanupLocked(cutoff time.Time, window time.Duration, now time.Time) {
	if l.lastCleanup.IsZero() || l.lastCleanup.Add(window).Before(now) {
		for existingKey, values := range l.hits {
			l.hits[existingKey] = retainAfter(values, cutoff)
			if len(l.hits[existingKey]) == 0 {
				delete(l.hits, existingKey)
			}
		}
		l.lastCleanup = now
	}
}

func rateLimiterKey(scope, ip string) string {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		ip = "unknown"
	}
	return strings.TrimSpace(scope) + "\x00" + ip
}

func retainAfter(values []time.Time, cutoff time.Time) []time.Time {
	if len(values) == 0 {
		return values
	}
	index := 0
	for index < len(values) && !values[index].After(cutoff) {
		index++
	}
	if index == 0 {
		return values
	}
	retained := values[index:]
	return append(values[:0], retained...)
}

func clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	remoteIP := normalizeClientIP(r.RemoteAddr)
	if trustedProxiesFromRequest(r).contains(remoteIP) {
		if value := normalizeClientIP(r.Header.Get("X-Real-IP")); value != "" {
			return value
		}
		if value := lastForwardedValue(r.Header.Get("X-Forwarded-For")); value != "" {
			return normalizeClientIP(value)
		}
	}
	return remoteIP
}

func normalizeClientIP(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(value, "[]")
}

func lastForwardedValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parts := strings.Split(value, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		if part := strings.TrimSpace(parts[i]); part != "" {
			return part
		}
	}
	return ""
}

func newTrustedProxySet(values []string) trustedProxySet {
	if len(values) == 0 {
		values = []string{"127.0.0.1/32", "::1/128"}
	}
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, ok := parseTrustedProxySetEntry(value)
		if !ok {
			continue
		}
		prefixes = append(prefixes, prefix)
	}
	if len(prefixes) == 0 {
		return defaultTrustedProxies
	}
	return trustedProxySet{prefixes: prefixes}
}

func parseTrustedProxySetEntry(value string) (netip.Prefix, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return netip.Prefix{}, false
	}
	if strings.Contains(value, "/") {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return netip.Prefix{}, false
		}
		return prefix.Masked(), true
	}
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(addr, addr.BitLen()), true
}

func mustTrustedProxyPrefixes(values ...string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, ok := parseTrustedProxySetEntry(value)
		if !ok {
			panic("invalid default trusted proxy prefix: " + value)
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}

func (s trustedProxySet) contains(remoteIP string) bool {
	addr, err := netip.ParseAddr(strings.TrimSpace(remoteIP))
	if err != nil {
		return false
	}
	for _, prefix := range s.prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func withTrustedProxies(ctx context.Context, proxies trustedProxySet) context.Context {
	if len(proxies.prefixes) == 0 {
		proxies = defaultTrustedProxies
	}
	return context.WithValue(ctx, trustedProxyContextKey{}, proxies)
}

func trustedProxiesFromRequest(r *http.Request) trustedProxySet {
	if r == nil {
		return defaultTrustedProxies
	}
	if proxies, ok := r.Context().Value(trustedProxyContextKey{}).(trustedProxySet); ok && len(proxies.prefixes) > 0 {
		return proxies
	}
	return defaultTrustedProxies
}
