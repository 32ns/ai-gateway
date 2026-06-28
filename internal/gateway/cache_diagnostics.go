package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/32ns/ai-gateway/internal/core"
)

func billingCacheDiagnosticsForGatewayRequest(req *core.GatewayRequest) core.BillingCacheDiagnostics {
	if req == nil {
		return core.BillingCacheDiagnostics{}
	}
	return billingCacheDiagnostics("chat", req.PromptCacheKey, req.Metadata)
}

func billingCacheDiagnosticsForResponsesRequest(req *core.ResponsesRequest) core.BillingCacheDiagnostics {
	if req == nil {
		return core.BillingCacheDiagnostics{}
	}
	shape := "responses"
	switch req.Transport {
	case core.ResponsesTransportHTTP:
		shape = "responses_http"
	case core.ResponsesTransportSSE:
		shape = "responses_sse"
	case core.ResponsesTransportWebSocket:
		shape = "responses_websocket"
	}
	return billingCacheDiagnostics(shape, req.PromptCacheKey, req.Metadata)
}

func billingCacheDiagnostics(shape, explicit string, metadata map[string]string) core.BillingCacheDiagnostics {
	shape = strings.TrimSpace(shape)
	diag := core.BillingCacheDiagnostics{RequestShape: shape}
	promptKey, source := billingPromptCacheKeySource(explicit, metadata)
	if promptKey != "" {
		diag.PromptCacheKeyPresent = true
		diag.PromptCacheKeySource = source
		diag.PromptCacheKeyHash = shortCacheDiagnosticHash("prompt_cache_key", promptKey)
	}
	if routeKey := strings.TrimSpace(metadata["route_affinity_key"]); routeKey != "" {
		diag.RouteAffinityPresent = true
		diag.RouteAffinityHash = shortCacheDiagnosticHash("route_affinity_key", routeKey)
	}
	if sessionKey := billingSessionAffinityKey(metadata); sessionKey != "" {
		diag.SessionAffinityPresent = true
		diag.SessionAffinityHash = shortCacheDiagnosticHash("session_affinity", sessionKey)
	}
	return diag
}

func billingPromptCacheKeySource(explicit string, metadata map[string]string) (string, string) {
	if value := strings.TrimSpace(explicit); value != "" {
		return value, "request.prompt_cache_key"
	}
	if metadata != nil {
		if value := strings.TrimSpace(metadata["prompt_cache_key"]); value != "" {
			return value, "metadata.prompt_cache_key"
		}
		if value := strings.TrimSpace(metadata["cache_affinity_key"]); value != "" {
			return value, "metadata.cache_affinity_key"
		}
		if value := strings.TrimSpace(metadata["route_affinity_key"]); value != "" {
			return routeAffinityDiagnosticValue(value), "metadata.route_affinity_key"
		}
	}
	return "", ""
}

func billingSessionAffinityKey(metadata map[string]string) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range []string{"route_affinity_key", "cache_affinity_key", "prompt_cache_key"} {
		if value := strings.TrimSpace(metadata[key]); value != "" {
			return routeAffinityDiagnosticValue(value)
		}
	}
	return ""
}

func routeAffinityDiagnosticValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if idx := strings.Index(value, ":"); idx >= 0 {
		return strings.TrimSpace(value[idx+1:])
	}
	return value
}

func shortCacheDiagnosticHash(scope, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("ai-gateway/cache-diagnostics/" + scope + "\x00" + value))
	return hex.EncodeToString(sum[:8])
}
