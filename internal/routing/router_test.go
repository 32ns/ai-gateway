package routing

import (
	"testing"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestBuildPlanUsesModelPrefixRule(t *testing.T) {
	router := NewRouter()
	policy := core.RoutePolicy{
		DefaultProvider:   core.ProviderOpenAI,
		FallbackProviders: []core.ProviderKind{core.ProviderClaude},
		Rules: []core.RouteRule{
			{ModelPrefix: "claude-", PreferredProviders: []core.ProviderKind{core.ProviderClaude, core.ProviderOpenAI}},
		},
	}

	plan := router.BuildPlan(&core.GatewayRequest{Model: "claude-sonnet-4-0"}, policy)

	if len(plan.Providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(plan.Providers))
	}
	if plan.Providers[0] != core.ProviderClaude {
		t.Fatalf("expected claude first, got %s", plan.Providers[0])
	}
	if plan.Providers[1] != core.ProviderOpenAI {
		t.Fatalf("expected openai second, got %s", plan.Providers[1])
	}
}

func TestBuildPlanFallsBackToDefaultPolicy(t *testing.T) {
	router := NewRouter()
	policy := core.RoutePolicy{
		DefaultProvider:   core.ProviderOpenAI,
		FallbackProviders: []core.ProviderKind{core.ProviderClaude},
	}

	plan := router.BuildPlan(&core.GatewayRequest{Model: "custom-model"}, policy)

	if len(plan.Providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(plan.Providers))
	}
	if plan.Providers[0] != core.ProviderOpenAI {
		t.Fatalf("expected openai first, got %s", plan.Providers[0])
	}
	if plan.Providers[1] != core.ProviderClaude {
		t.Fatalf("expected claude second, got %s", plan.Providers[1])
	}
}
