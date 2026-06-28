package web

import (
	"net/http"

	"github.com/32ns/ai-gateway/internal/core"
)

func (s *Server) handleGatewayClientQuota(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	client := protocolClientPointerFromContext(r.Context())
	if client == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]string{
				"type":    "unauthorized",
				"message": "API key is required",
			},
		})
		return
	}
	quota, err := s.control.GetClientQuota(*client)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{
				"type":    "quota_unavailable",
				"message": "Quota information is unavailable.",
			},
		})
		return
	}
	writeJSON(w, http.StatusOK, quota)
}

func (s *Server) handleGatewayBillingSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	client := protocolClientPointerFromContext(r.Context())
	if client == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]string{
				"type":    "unauthorized",
				"message": "API key is required",
			},
		})
		return
	}
	quota, err := s.control.GetClientQuota(*client)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{
				"type":    "quota_unavailable",
				"message": "Quota information is unavailable.",
			},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":                "billing_subscription",
		"billing_hard_limit":    nanoUSDToUSD(quota.SpendLimitNanoUSD),
		"hard_limit_usd":        nanoUSDToUSD(quota.SpendLimitNanoUSD),
		"soft_limit_usd":        nanoUSDToUSD(quota.SpendLimitNanoUSD),
		"has_payment_method":    true,
		"system_hard_limit_usd": nanoUSDToUSD(quota.BalanceNanoUSD),
	})
}

func (s *Server) handleGatewayBillingUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	client := protocolClientPointerFromContext(r.Context())
	if client == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]string{
				"type":    "unauthorized",
				"message": "API key is required",
			},
		})
		return
	}
	quota, err := s.control.GetClientQuota(*client)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{
				"type":    "quota_unavailable",
				"message": "Quota information is unavailable.",
			},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":          "billing_usage",
		"total_usage":     nanoUSDToCents(quota.SpendUsedNanoUSD),
		"total_usage_usd": nanoUSDToUSD(quota.SpendUsedNanoUSD),
	})
}

func nanoUSDToUSD(value int64) float64 {
	return float64(value) / float64(core.NanoUSDPerUSD)
}

func nanoUSDToCents(value int64) float64 {
	return nanoUSDToUSD(value) * 100
}
