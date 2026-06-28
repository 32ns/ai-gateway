package web

import (
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestParsePricingTiersForm(t *testing.T) {
	form := url.Values{}
	form.Add("pricing_tier_name", "standard")
	form.Add("pricing_tier_max_input_tokens", "200000")
	form.Add("pricing_tier_input_price_usd_per_1m", "3")
	form.Add("pricing_tier_cached_input_price_usd_per_1m", "0.3")
	form.Add("pricing_tier_output_price_usd_per_1m", "15")

	req := httptest.NewRequest("POST", "/admin/models", nil)
	req.Form = form

	tiers, err := parsePricingTiersForm(req)
	if err != nil {
		t.Fatalf("parsePricingTiersForm returned error: %v", err)
	}
	if len(tiers) != 1 {
		t.Fatalf("tiers len = %d, want 1", len(tiers))
	}
	tier := tiers[0]
	if tier.Name != "standard" {
		t.Fatalf("name = %q, want standard", tier.Name)
	}
	if tier.MaxInputTokens != 200000 {
		t.Fatalf("max input tokens = %d, want 200000", tier.MaxInputTokens)
	}
	if tier.InputPriceNanoUSD != 3*core.NanoUSDPerUSD {
		t.Fatalf("input price = %d, want %d", tier.InputPriceNanoUSD, 3*core.NanoUSDPerUSD)
	}
}
