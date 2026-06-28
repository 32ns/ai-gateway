package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

const (
	claudeUsageURL           = "https://api.anthropic.com/api/oauth/usage"
	claudeTierFiveHour       = "five_hour"
	claudeTierSevenDay       = "seven_day"
	claudeTierSevenDayOpus   = "seven_day_opus"
	claudeTierSevenDaySonnet = "seven_day_sonnet"
)

var claudeQuotaEndpoint = claudeUsageURL

type claudeUsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

type claudeExtraUsage struct {
	IsEnabled bool     `json:"is_enabled"`
	Monthly   *float64 `json:"monthly_limit"`
	Used      *float64 `json:"used_credits"`
}

func (a *ClaudeAdapter) FetchQuota(ctx context.Context, account core.Account) (core.AccountQuotaSnapshot, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, account.EffectiveProxyURL), account)
	if strings.TrimSpace(account.Credential.Metadata["token_source"]) != ClaudeOAuthTokenSourceValue() {
		if APIKeyQuotaRefreshConfigured(account) {
			return fetchAPIKeyQuota(ctx, account)
		}
		return core.AccountQuotaSnapshot{}, fmt.Errorf("claude quota refresh requires an oauth-backed account or configured api-key quota provider")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeQuotaEndpoint, nil)
	if err != nil {
		return core.AccountQuotaSnapshot{}, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(account.Credential.AccessToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", claudeOAuthBetaHeader)

	resp, body, err := doOAuthRaw(req)
	if err != nil {
		return core.AccountQuotaSnapshot{}, mapOAuthHTTPError(resp, body, err)
	}
	if resp.StatusCode >= 400 {
		return core.AccountQuotaSnapshot{}, mapOAuthHTTPError(resp, body, fmt.Errorf("claude usage returned status %d", resp.StatusCode))
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return core.AccountQuotaSnapshot{}, err
	}

	refreshedAt := time.Now().UTC()
	snapshot := core.AccountQuotaSnapshot{
		Source:      "claude_oauth_usage",
		Plan:        firstJSONText(payload, "plan_type", "plan", "tier"),
		Primary:     claudeQuotaWindow(payload, claudeTierFiveHour, 5*60),
		Secondary:   firstClaudeWeeklyWindow(payload),
		RefreshedAt: &refreshedAt,
	}
	if raw := payload["extra_usage"]; len(raw) > 0 {
		var extra claudeExtraUsage
		if json.Unmarshal(raw, &extra) == nil {
			snapshot.Credits = &core.AccountQuotaCredits{
				HasCredits: extra.IsEnabled,
				Unlimited:  false,
				Balance:    claudeRemainingBalance(extra.Monthly, extra.Used),
			}
		}
	}
	return snapshot, nil
}

func firstClaudeWeeklyWindow(payload map[string]json.RawMessage) *core.AccountQuotaWindow {
	for _, candidate := range []struct {
		name    string
		minutes int64
	}{
		{claudeTierSevenDay, 7 * 24 * 60},
		{claudeTierSevenDayOpus, 7 * 24 * 60},
		{claudeTierSevenDaySonnet, 7 * 24 * 60},
	} {
		if window := claudeQuotaWindow(payload, candidate.name, candidate.minutes); window != nil {
			return window
		}
	}
	return nil
}

func claudeQuotaWindow(payload map[string]json.RawMessage, key string, minutes int64) *core.AccountQuotaWindow {
	raw, ok := payload[key]
	if !ok || len(raw) == 0 {
		return nil
	}
	var window claudeUsageWindow
	if err := json.Unmarshal(raw, &window); err != nil || window.Utilization == nil {
		return nil
	}
	out := &core.AccountQuotaWindow{
		Name:          key,
		UsedPercent:   *window.Utilization * 100,
		WindowMinutes: minutes,
	}
	if reset := parseResetTime(window.ResetsAt); reset != nil {
		out.ResetsAt = reset
	}
	return out
}

func parseResetTime(value string) *time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		utc := parsed.UTC()
		return &utc
	}
	return nil
}

func claudeRemainingBalance(limit, used *float64) *float64 {
	if limit == nil || used == nil {
		return nil
	}
	remaining := *limit - *used
	return &remaining
}

func firstJSONText(payload map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		raw, ok := payload[key]
		if !ok || len(raw) == 0 {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err == nil {
			value = strings.TrimSpace(value)
			if value != "" {
				return value
			}
		}
	}
	return ""
}
