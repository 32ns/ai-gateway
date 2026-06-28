package routing

import (
	"strings"

	"github.com/32ns/ai-gateway/internal/core"
)

type Router struct{}

func NewRouter() *Router {
	return &Router{}
}

func (r *Router) BuildPlan(req *core.GatewayRequest, policy core.RoutePolicy) core.RoutePlan {
	return r.buildPlan(req.Model, policy)
}

func (r *Router) BuildResponsesPlan(req *core.ResponsesRequest, policy core.RoutePolicy) core.RoutePlan {
	return r.buildPlan(req.Model, policy)
}

func (r *Router) BuildEmbeddingPlan(req *core.EmbeddingRequest, policy core.RoutePolicy) core.RoutePlan {
	return r.buildPlan(req.Model, policy)
}

func (r *Router) BuildModerationPlan(req *core.ModerationRequest, policy core.RoutePolicy) core.RoutePlan {
	return r.buildPlan(req.Model, policy)
}

func (r *Router) BuildImageGenerationPlan(req *core.ImageGenerationRequest, policy core.RoutePolicy) core.RoutePlan {
	return r.buildPlan(req.Model, policy)
}

func (r *Router) BuildImageMultipartPlan(req *core.ImageMultipartRequest, policy core.RoutePolicy) core.RoutePlan {
	return r.buildPlan(req.Model, policy)
}

func (r *Router) BuildAudioSpeechPlan(req *core.AudioSpeechRequest, policy core.RoutePolicy) core.RoutePlan {
	return r.buildPlan(req.Model, policy)
}

func (r *Router) BuildAudioMultipartPlan(req *core.AudioMultipartRequest, policy core.RoutePolicy) core.RoutePlan {
	return r.buildPlan(req.Model, policy)
}

func (r *Router) BuildTokenCountPlan(req *core.TokenCountRequest, policy core.RoutePolicy) core.RoutePlan {
	return r.buildPlan(req.Model, policy)
}

func (r *Router) buildPlan(model string, policy core.RoutePolicy) core.RoutePlan {
	for _, rule := range policy.Rules {
		if strings.HasPrefix(model, rule.ModelPrefix) {
			return core.RoutePlan{
				Providers: uniqueRuleProviders(rule.PreferredProviders, policy.DefaultProvider, policy.FallbackProviders),
				Model:     model,
				Reason:    "matched model prefix rule",
			}
		}
	}

	return core.RoutePlan{
		Providers: uniqueRouteProviders(policy.DefaultProvider, policy.FallbackProviders),
		Model:     model,
		Reason:    "default route policy",
	}
}

func uniqueRouteProviders(defaultProvider core.ProviderKind, fallback []core.ProviderKind) []core.ProviderKind {
	out := make([]core.ProviderKind, 0, 1+len(fallback))
	out = appendUniqueProvider(out, defaultProvider)
	for _, provider := range fallback {
		out = appendUniqueProvider(out, provider)
	}
	return out
}

func uniqueRuleProviders(preferred []core.ProviderKind, defaultProvider core.ProviderKind, fallback []core.ProviderKind) []core.ProviderKind {
	capacity := len(preferred) + 1 + len(fallback)
	out := make([]core.ProviderKind, 0, capacity)

	for _, provider := range preferred {
		out = appendUniqueProvider(out, provider)
	}
	out = appendUniqueProvider(out, defaultProvider)
	for _, provider := range fallback {
		out = appendUniqueProvider(out, provider)
	}

	return out
}

func appendUniqueProvider(out []core.ProviderKind, provider core.ProviderKind) []core.ProviderKind {
	if provider == "" {
		return out
	}
	for _, existing := range out {
		if existing == provider {
			return out
		}
	}
	return append(out, provider)
}
