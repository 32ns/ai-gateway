package core

import "testing"

func TestParseMultiplierDecimal(t *testing.T) {
	tests := []struct {
		raw  string
		want int64
	}{
		{raw: "", want: 10000},
		{raw: "1", want: 10000},
		{raw: "0.8", want: 8000},
		{raw: "0.25x", want: 2500},
		{raw: ".125", want: 1250},
		{raw: "0", want: 0},
		{raw: "2", want: 20000},
		{raw: "5.5", want: 55000},
	}
	for _, tt := range tests {
		got, err := ParseMultiplierDecimal(tt.raw)
		if err != nil {
			t.Fatalf("ParseMultiplierDecimal(%q) returned error: %v", tt.raw, err)
		}
		if got != tt.want {
			t.Fatalf("ParseMultiplierDecimal(%q) = %d, want %d", tt.raw, got, tt.want)
		}
	}
}

func TestParseMultiplierDecimalBlankDefaultsToOne(t *testing.T) {
	got, err := ParseMultiplierDecimal("")
	if err != nil {
		t.Fatalf("ParseMultiplierDecimal returned error: %v", err)
	}
	if got != 10000 {
		t.Fatalf("blank multiplier = %d, want 10000", got)
	}
}

func TestParseMultiplierDecimalRejectsInvalidValues(t *testing.T) {
	for _, raw := range []string{"-0.1", "abc", "1.23456"} {
		if _, err := ParseMultiplierDecimal(raw); err == nil {
			t.Fatalf("ParseMultiplierDecimal(%q) expected error", raw)
		}
	}
}

func TestFormatMultiplier(t *testing.T) {
	tests := []struct {
		bps  int64
		want string
	}{
		{bps: 10000, want: "1"},
		{bps: 8000, want: "0.8"},
		{bps: 2500, want: "0.25"},
		{bps: 125, want: "0.0125"},
		{bps: 0, want: "0"},
		{bps: 20000, want: "2"},
		{bps: 55000, want: "5.5"},
	}
	for _, tt := range tests {
		if got := FormatMultiplier(tt.bps); got != tt.want {
			t.Fatalf("FormatMultiplier(%d) = %q, want %q", tt.bps, got, tt.want)
		}
	}
}

func TestBillingPlanGroupQuotaPriceRatio(t *testing.T) {
	ratio, err := NormalizeBillingPlanGroupQuotaPriceRatio("1:0.80")
	if err != nil {
		t.Fatalf("NormalizeBillingPlanGroupQuotaPriceRatio returned error: %v", err)
	}
	if ratio != "1:0.8" {
		t.Fatalf("ratio = %q, want %q", ratio, "1:0.8")
	}
	price, err := BillingPlanGroupPriceForQuotaNanoUSD(ratio, 2*NanoUSDPerUSD)
	if err != nil {
		t.Fatalf("BillingPlanGroupPriceForQuotaNanoUSD returned error: %v", err)
	}
	if price != 1600000000 {
		t.Fatalf("price = %d, want %d", price, int64(1600000000))
	}
}

func TestBillingPlanGroupQuotaPriceRatioRejectsInvalidValues(t *testing.T) {
	for _, raw := range []string{"1", "1:0", "0:1", "abc:1"} {
		if _, err := NormalizeBillingPlanGroupQuotaPriceRatio(raw); err == nil {
			t.Fatalf("NormalizeBillingPlanGroupQuotaPriceRatio(%q) returned nil error", raw)
		}
	}
}
