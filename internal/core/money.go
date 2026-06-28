package core

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

const DefaultBillingPlanGroupQuotaPriceRatio = "1:1"

func ParseNanoUSDDecimal(raw string) (int64, error) {
	value := strings.TrimSpace(raw)
	value = strings.TrimPrefix(value, "$")
	value = strings.ReplaceAll(value, ",", "")
	if value == "" {
		return 0, nil
	}
	sign := int64(1)
	if strings.HasPrefix(value, "-") {
		sign = -1
		value = strings.TrimPrefix(value, "-")
	} else if strings.HasPrefix(value, "+") {
		value = strings.TrimPrefix(value, "+")
	}
	if value == "" {
		return 0, fmt.Errorf("amount is required")
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 {
		return 0, fmt.Errorf("amount must be a decimal")
	}
	wholeText := parts[0]
	if wholeText == "" {
		wholeText = "0"
	}
	if !decimalDigitsOnly(wholeText) {
		return 0, fmt.Errorf("amount must be a decimal")
	}
	whole, err := strconv.ParseInt(wholeText, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount is too large")
	}
	fractionText := ""
	if len(parts) == 2 {
		fractionText = parts[1]
	}
	if len(fractionText) > 9 {
		return 0, fmt.Errorf("amount supports up to 9 decimal places")
	}
	if !decimalDigitsOnly(fractionText) {
		return 0, fmt.Errorf("amount must be a decimal")
	}
	for len(fractionText) < 9 {
		fractionText += "0"
	}
	var fraction int64
	if fractionText != "" {
		fraction, err = strconv.ParseInt(fractionText, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("amount is too large")
		}
	}
	if whole > (1<<63-1-fraction)/NanoUSDPerUSD {
		return 0, fmt.Errorf("amount is too large")
	}
	return sign * (whole*NanoUSDPerUSD + fraction), nil
}

func FormatNanoUSD(nanoUSD int64) string {
	sign := ""
	if nanoUSD < 0 {
		sign = "-"
		nanoUSD = -nanoUSD
	}
	whole := nanoUSD / NanoUSDPerUSD
	fraction := nanoUSD % NanoUSDPerUSD
	if fraction == 0 {
		return sign + strconv.FormatInt(whole, 10)
	}
	fractionText := strings.TrimRight(fmt.Sprintf("%09d", fraction), "0")
	return fmt.Sprintf("%s%d.%s", sign, whole, fractionText)
}

func NormalizeBillingPlanGroupQuotaPriceRatio(raw string) (string, error) {
	quota, price, err := parseBillingPlanGroupQuotaPriceRatio(raw)
	if err != nil {
		return "", err
	}
	return FormatNanoUSD(quota) + ":" + FormatNanoUSD(price), nil
}

func BillingPlanGroupPriceForQuotaNanoUSD(ratio string, quotaNanoUSD int64) (int64, error) {
	if quotaNanoUSD <= 0 {
		return 0, nil
	}
	quotaUnit, priceUnit, err := parseBillingPlanGroupQuotaPriceRatio(ratio)
	if err != nil {
		return 0, err
	}
	numerator := new(big.Int).Mul(big.NewInt(quotaNanoUSD), big.NewInt(priceUnit))
	denominator := big.NewInt(quotaUnit)
	amount, remainder := new(big.Int).QuoRem(numerator, denominator, new(big.Int))
	if remainder.Sign() != 0 {
		twiceRemainder := new(big.Int).Mul(remainder, big.NewInt(2))
		if twiceRemainder.Cmp(denominator) >= 0 {
			amount.Add(amount, big.NewInt(1))
		}
	}
	if !amount.IsInt64() {
		return 0, fmt.Errorf("calculated amount is too large")
	}
	return amount.Int64(), nil
}

func parseBillingPlanGroupQuotaPriceRatio(raw string) (int64, int64, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		value = DefaultBillingPlanGroupQuotaPriceRatio
	}
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("ratio must use quota:balance format")
	}
	quota, err := ParseNanoUSDDecimal(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("ratio quota: %w", err)
	}
	price, err := ParseNanoUSDDecimal(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("ratio balance: %w", err)
	}
	if quota <= 0 || price <= 0 {
		return 0, 0, fmt.Errorf("ratio values must be positive")
	}
	return quota, price, nil
}

func ParseMultiplierDecimal(raw string) (int64, error) {
	value := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "x"))
	if value == "" {
		return 10000, nil
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 {
		return 0, fmt.Errorf("multiplier must be a non-negative decimal")
	}
	wholeText := parts[0]
	if wholeText == "" {
		wholeText = "0"
	}
	if !decimalDigitsOnly(wholeText) {
		return 0, fmt.Errorf("multiplier must be a non-negative decimal")
	}
	whole, err := strconv.ParseInt(wholeText, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("multiplier is too large")
	}
	fractionText := ""
	if len(parts) == 2 {
		fractionText = parts[1]
	}
	if len(fractionText) > 4 {
		return 0, fmt.Errorf("multiplier supports up to 4 decimal places")
	}
	if !decimalDigitsOnly(fractionText) {
		return 0, fmt.Errorf("multiplier must be a non-negative decimal")
	}
	for len(fractionText) < 4 {
		fractionText += "0"
	}
	var fraction int64
	if fractionText != "" {
		fraction, err = strconv.ParseInt(fractionText, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("multiplier is too large")
		}
	}
	if whole > (1<<63-1-fraction)/10000 {
		return 0, fmt.Errorf("multiplier is too large")
	}
	return whole*10000 + fraction, nil
}

func FormatMultiplier(bps int64) string {
	whole := bps / 10000
	fraction := bps % 10000
	if fraction == 0 {
		return strconv.FormatInt(whole, 10)
	}
	return fmt.Sprintf("%d.%s", whole, strings.TrimRight(fmt.Sprintf("%04d", fraction), "0"))
}

func decimalDigitsOnly(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
