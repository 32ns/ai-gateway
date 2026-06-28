package gateway

import (
	"testing"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestBillingCacheDiagnosticsForResponsesRequestRecordsHashedAffinity(t *testing.T) {
	req := &core.ResponsesRequest{
		Transport: core.ResponsesTransportSSE,
		Metadata: map[string]string{
			"route_affinity_key": "session-id:sess_123",
		},
	}
	diag := billingCacheDiagnosticsForResponsesRequest(req)

	if diag.RequestShape != "responses_sse" {
		t.Fatalf("RequestShape = %q, want responses_sse", diag.RequestShape)
	}
	if diag.PromptCacheKeySource != "metadata.route_affinity_key" || !diag.PromptCacheKeyPresent {
		t.Fatalf("prompt key source/present = %q/%v", diag.PromptCacheKeySource, diag.PromptCacheKeyPresent)
	}
	if diag.PromptCacheKeyHash == "" || diag.SessionAffinityHash == "" || diag.RouteAffinityHash == "" {
		t.Fatalf("diagnostic hashes missing: %#v", diag)
	}
	if diag.PromptCacheKeyHash == "sess_123" || diag.SessionAffinityHash == "sess_123" || diag.RouteAffinityHash == "session-id:sess_123" {
		t.Fatalf("diagnostics must not contain raw affinity values: %#v", diag)
	}
}

func TestBillingCacheDiagnosticsPrefersExplicitPromptCacheKey(t *testing.T) {
	req := &core.ResponsesRequest{
		Transport:      core.ResponsesTransportWebSocket,
		PromptCacheKey: "explicit-key",
		Metadata: map[string]string{
			"route_affinity_key": "session-id:sess_123",
		},
	}
	diag := billingCacheDiagnosticsForResponsesRequest(req)

	if diag.RequestShape != "responses_websocket" {
		t.Fatalf("RequestShape = %q, want responses_websocket", diag.RequestShape)
	}
	if diag.PromptCacheKeySource != "request.prompt_cache_key" {
		t.Fatalf("PromptCacheKeySource = %q", diag.PromptCacheKeySource)
	}
}
