package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

const (
	openAIUsageBaseURL       = "https://chatgpt.com/backend-api"
	openAIChatGPTUsageURL    = openAIUsageBaseURL + "/wham/usage"
	openAIUsageDefaultSource = "openai_chatgpt_usage"
)

var openAIQuotaEndpoint = openAIChatGPTUsageURL

type openAIQuotaPayload struct {
	PlanType             string                          `json:"plan_type"`
	RateLimit            *openAIRateLimitBody            `json:"rate_limit"`
	Credits              *openAICreditsBody              `json:"credits"`
	AdditionalRateLimits []openAIAdditionalRateLimitBody `json:"additional_rate_limits"`
	RateLimitReachedType *struct {
		Type string `json:"type"`
	} `json:"rate_limit_reached_type"`
}

type openAIAdditionalRateLimitBody struct {
	LimitName      string               `json:"limit_name"`
	MeteredFeature string               `json:"metered_feature"`
	RateLimit      *openAIRateLimitBody `json:"rate_limit"`
}

type openAIRateLimitBody struct {
	PrimaryWindow   *openAIRateLimitWindow `json:"primary_window"`
	SecondaryWindow *openAIRateLimitWindow `json:"secondary_window"`
}

type openAIRateLimitWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

type openAICreditsBody struct {
	HasCredits bool                 `json:"has_credits"`
	Unlimited  bool                 `json:"unlimited"`
	Balance    *openAIFlexibleFloat `json:"balance"`
}

type openAIAPIKeySubscriptionResponse struct {
	HardLimitUSD     float64 `json:"hard_limit_usd"`
	SoftLimitUSD     float64 `json:"soft_limit_usd"`
	HasPaymentMethod bool    `json:"has_payment_method"`
}

type openAIAPIKeyUsageResponse struct {
	TotalUsage float64 `json:"total_usage"`
}

type sub2APIUsageResponse struct {
	Mode      string               `json:"mode"`
	IsValid   *bool                `json:"isValid"`
	Status    string               `json:"status"`
	PlanName  string               `json:"planName"`
	Remaining *openAIFlexibleFloat `json:"remaining"`
	Balance   *openAIFlexibleFloat `json:"balance"`
	Unit      string               `json:"unit"`
	Quota     *struct {
		Limit     *openAIFlexibleFloat `json:"limit"`
		Used      *openAIFlexibleFloat `json:"used"`
		Remaining *openAIFlexibleFloat `json:"remaining"`
		Unit      string               `json:"unit"`
	} `json:"quota"`
}

type gatewayQuotaResponse struct {
	RemainingNanoUSD int64 `json:"remaining_nano_usd"`
	BalanceNanoUSD   int64 `json:"balance_nano_usd"`
}

func (a *OpenAIAdapter) FetchQuota(ctx context.Context, account core.Account) (core.AccountQuotaSnapshot, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, account.EffectiveProxyURL), account)
	if !IsOpenAIOAuthTokenSource(account.Credential.Metadata["token_source"]) {
		if strings.TrimSpace(account.Credential.Mode) != "manual-token" {
			return core.AccountQuotaSnapshot{}, fmt.Errorf("openai quota refresh requires an oauth-backed, token, or api-key account")
		}
		switch openAIAccountLoginMethod(account) {
		case "api_key":
			return fetchAPIKeyQuota(ctx, account)
		case "token":
		default:
			return core.AccountQuotaSnapshot{}, fmt.Errorf("unsupported openai account login method %q", openAIAccountLoginMethod(account))
		}
	}

	return a.fetchChatGPTUsageQuota(ctx, account, "")
}

func (a *OpenAIAdapter) fetchChatGPTUsageQuota(ctx context.Context, account core.Account, planFallback string) (core.AccountQuotaSnapshot, error) {
	payload, err := a.fetchChatGPTUsagePayload(ctx, account)
	if err != nil {
		return core.AccountQuotaSnapshot{}, err
	}
	refreshedAt := time.Now().UTC()
	plan := strings.TrimSpace(payload.PlanType)
	if plan == "" {
		plan = strings.TrimSpace(planFallback)
	}
	snapshot := core.AccountQuotaSnapshot{
		LimitID:     "codex",
		Source:      openAIUsageDefaultSource,
		Plan:        plan,
		Primary:     openAIQuotaWindow("primary", payload.RateLimit, true),
		Secondary:   openAIQuotaWindow("secondary", payload.RateLimit, false),
		Credits:     openAICredits(payload.Credits),
		ReachedType: strings.TrimSpace(reachedType(payload.RateLimitReachedType)),
		Additional:  openAIAdditionalQuotaSnapshots(payload.AdditionalRateLimits, plan, refreshedAt),
		RefreshedAt: &refreshedAt,
	}
	return snapshot, nil
}

func (a *OpenAIAdapter) fetchChatGPTUsagePayload(ctx context.Context, account core.Account) (openAIQuotaPayload, error) {
	endpoint := openAIQuotaEndpointURL()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return openAIQuotaPayload{}, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(account.Credential.AccessToken))
	req.Header.Set("User-Agent", openAIOAuthUserAgent)
	if accountID := strings.TrimSpace(account.Credential.Metadata["oauth_account_id"]); accountID != "" {
		switch openAIIdentitySource(account.Credential) {
		case "", "chatgpt_account_id":
			req.Header.Set("ChatGPT-Account-Id", accountID)
		}
	}

	resp, body, err := doOAuthRaw(req)
	if err != nil {
		return openAIQuotaPayload{}, mapOAuthHTTPError(resp, body, err)
	}
	if resp.StatusCode >= 400 {
		return openAIQuotaPayload{}, mapOAuthHTTPError(resp, body, fmt.Errorf("openai usage returned status %d", resp.StatusCode))
	}
	var payload openAIQuotaPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		if looksLikeHTMLPayload(body) {
			return openAIQuotaPayload{}, mapOAuthHTTPError(&http.Response{StatusCode: http.StatusForbidden}, body, err)
		}
		return openAIQuotaPayload{}, err
	}
	return payload, nil
}

func openAIQuotaEndpointURL() string {
	if endpoint := strings.TrimSpace(openAIQuotaEndpoint); endpoint != "" {
		return endpoint
	}
	return openAIChatGPTUsageURL
}

func isOpenAIChatGPTFreePlan(plan string) bool {
	return strings.EqualFold(strings.TrimSpace(plan), "free")
}

func OpenAIManualTokenUsesChatGPTBackend(account core.Account) bool {
	return account.Provider == core.ProviderOpenAI &&
		strings.TrimSpace(account.Credential.Mode) == "manual-token" &&
		openAIAccountLoginMethod(account) == "token"
}

func OpenAIAPIKeyQuotaRefreshConfigured(account core.Account) bool {
	if account.Provider != core.ProviderOpenAI {
		return false
	}
	return APIKeyQuotaRefreshConfigured(account)
}

func APIKeyQuotaRefreshConfigured(account core.Account) bool {
	if strings.TrimSpace(account.Credential.Mode) != "manual-token" ||
		apiKeyQuotaAccountLoginMethod(account) != "api_key" {
		return false
	}
	provider := strings.TrimSpace(account.Credential.Metadata["api_key_quota_provider"])
	baseURL := strings.TrimSpace(account.Credential.Metadata["base_url"])
	switch provider {
	case "new-api":
		return account.Provider == core.ProviderOpenAI && baseURL != "" && !isOfficialOpenAIAPIBaseURL(baseURL)
	case "sub2api", "gateway":
		return baseURL != ""
	default:
		return false
	}
}

func apiKeyQuotaAccountLoginMethod(account core.Account) string {
	if account.Provider == core.ProviderOpenAI {
		return openAIAccountLoginMethod(account)
	}
	method := strings.TrimSpace(account.Credential.Metadata["account_login_method"])
	if method == "" {
		return "api_key"
	}
	return method
}

func fetchAPIKeyQuota(ctx context.Context, account core.Account) (core.AccountQuotaSnapshot, error) {
	switch strings.TrimSpace(account.Credential.Metadata["api_key_quota_provider"]) {
	case "new-api":
		if account.Provider != core.ProviderOpenAI {
			return core.AccountQuotaSnapshot{}, fmt.Errorf("new-api quota refresh is only supported for openai api-key accounts")
		}
		return fetchNewAPIKeyQuota(ctx, account)
	case "sub2api":
		return fetchSub2APIKeyQuota(ctx, account)
	case "gateway":
		return fetchGatewayAPIKeyQuota(ctx, account)
	case "":
		return core.AccountQuotaSnapshot{}, fmt.Errorf("api key quota refresh is not configured")
	default:
		return core.AccountQuotaSnapshot{}, fmt.Errorf("unsupported api key quota provider %q", account.Credential.Metadata["api_key_quota_provider"])
	}
}

func fetchNewAPIKeyQuota(ctx context.Context, account core.Account) (core.AccountQuotaSnapshot, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(account.Credential.Metadata["base_url"]), "/")
	if baseURL == "" {
		return core.AccountQuotaSnapshot{}, fmt.Errorf("new-api quota refresh requires a base url")
	}
	if isOfficialOpenAIAPIBaseURL(baseURL) {
		return core.AccountQuotaSnapshot{}, fmt.Errorf("official OpenAI API keys do not expose dashboard billing quota")
	}

	subscriptionURL := appendEndpointSuffix(baseURL, "/v1/dashboard/billing/subscription")
	body, err := fetchOpenAIAPIKeyBilling(ctx, subscriptionURL, account.Credential.AccessToken)
	if err != nil {
		return core.AccountQuotaSnapshot{}, err
	}
	if err := openAIAPIKeyBillingBodyError(body); err != nil {
		return core.AccountQuotaSnapshot{}, err
	}
	var subscription openAIAPIKeySubscriptionResponse
	if err := json.Unmarshal(body, &subscription); err != nil {
		return core.AccountQuotaSnapshot{}, err
	}

	now := time.Now().UTC()
	startDate := fmt.Sprintf("%s-01", now.Format("2006-01"))
	if !subscription.HasPaymentMethod {
		startDate = now.AddDate(0, 0, -100).Format("2006-01-02")
	}
	usageEndpoint := appendEndpointSuffix(baseURL, "/v1/dashboard/billing/usage")
	separator := "?"
	if strings.Contains(usageEndpoint, "?") {
		separator = "&"
	}
	usageURL := fmt.Sprintf("%s%sstart_date=%s&end_date=%s", usageEndpoint, separator, startDate, now.Format("2006-01-02"))
	body, err = fetchOpenAIAPIKeyBilling(ctx, usageURL, account.Credential.AccessToken)
	if err != nil {
		return core.AccountQuotaSnapshot{}, err
	}
	if err := openAIAPIKeyBillingBodyError(body); err != nil {
		return core.AccountQuotaSnapshot{}, err
	}
	var usage openAIAPIKeyUsageResponse
	if err := json.Unmarshal(body, &usage); err != nil {
		return core.AccountQuotaSnapshot{}, err
	}

	usedAmount := usage.TotalUsage / 100
	balance := subscription.HardLimitUSD - usedAmount
	refreshedAt := now
	if isNewAPIUnlimitedBillingSentinel(subscription.HardLimitUSD) {
		return core.AccountQuotaSnapshot{
			Source: "new_api_key_billing",
			Plan:   "api_key",
			Credits: &core.AccountQuotaCredits{
				HasCredits: true,
				Unlimited:  true,
			},
			RefreshedAt: &refreshedAt,
		}, nil
	}
	return core.AccountQuotaSnapshot{
		Source: "new_api_key_billing",
		Plan:   "api_key",
		Credits: &core.AccountQuotaCredits{
			HasCredits: true,
			Balance:    &balance,
		},
		Primary: &core.AccountQuotaWindow{
			Name:        "billing",
			UsedPercent: percentOfLimit(usedAmount, usedAmount+balance),
		},
		RefreshedAt: &refreshedAt,
	}, nil
}

func isNewAPIUnlimitedBillingSentinel(value float64) bool {
	return value >= 99999999
}

func openAIAPIKeyBillingBodyError(body []byte) error {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	if _, exists := payload["error"]; !exists {
		return nil
	}
	message := strings.TrimSpace(extractErrorMessage(body))
	if message == "" {
		message = "new-api billing endpoint returned an error"
	}
	return &InvokeError{
		Code:      ErrorCodeUpstreamRejected,
		Temporary: false,
		Cooldown:  2 * time.Minute,
		Err:       fmt.Errorf("%s", message),
	}
}

func fetchSub2APIKeyQuota(ctx context.Context, account core.Account) (core.AccountQuotaSnapshot, error) {
	baseURL := gatewayQuotaBaseURL(account)
	if baseURL == "" {
		return core.AccountQuotaSnapshot{}, fmt.Errorf("sub2api quota refresh requires a base url")
	}
	body, err := fetchOpenAIAPIKeyBilling(ctx, appendEndpointSuffix(baseURL, "/v1/usage"), account.Credential.AccessToken)
	if err != nil {
		return core.AccountQuotaSnapshot{}, err
	}
	var payload sub2APIUsageResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return core.AccountQuotaSnapshot{}, err
	}
	if payload.IsValid != nil && !*payload.IsValid {
		return core.AccountQuotaSnapshot{}, &InvokeError{
			Code:      ErrorCodeUpstreamAuthError,
			Temporary: false,
			Cooldown:  2 * time.Minute,
			Err:       fmt.Errorf("sub2api reported API key is invalid"),
		}
	}

	balance, unlimited, primary, err := sub2APIUsageQuotaSnapshot(payload)
	if err != nil {
		return core.AccountQuotaSnapshot{}, err
	}
	plan := strings.TrimSpace(payload.PlanName)
	if plan == "" {
		plan = "api_key"
	}
	refreshedAt := time.Now().UTC()
	return core.AccountQuotaSnapshot{
		Source:  "sub2api_usage",
		Plan:    plan,
		Primary: primary,
		Credits: &core.AccountQuotaCredits{
			HasCredits: true,
			Unlimited:  unlimited,
			Balance:    balance,
		},
		RefreshedAt: &refreshedAt,
	}, nil
}

func sub2APIUsageQuotaSnapshot(payload sub2APIUsageResponse) (*float64, bool, *core.AccountQuotaWindow, error) {
	if payload.Quota != nil {
		remaining := flexibleFloatValue(payload.Quota.Remaining)
		if remaining == nil {
			remaining = flexibleFloatValue(payload.Remaining)
		}
		limit := flexibleFloatValue(payload.Quota.Limit)
		used := flexibleFloatValue(payload.Quota.Used)
		var primary *core.AccountQuotaWindow
		if limit != nil && *limit > 0 {
			usedAmount := float64(0)
			if used != nil {
				usedAmount = *used
			} else if remaining != nil {
				usedAmount = *limit - *remaining
			}
			primary = &core.AccountQuotaWindow{
				Name:        "billing",
				UsedPercent: percentOfLimit(usedAmount, *limit),
			}
		}
		if remaining != nil {
			return remaining, false, primary, nil
		}
	}

	if balance := flexibleFloatValue(payload.Balance); balance != nil {
		return balance, false, nil, nil
	}
	if remaining := flexibleFloatValue(payload.Remaining); remaining != nil {
		if *remaining < 0 {
			return nil, true, nil, nil
		}
		return remaining, false, nil, nil
	}
	return nil, false, nil, fmt.Errorf("sub2api usage response did not include remaining balance")
}

func flexibleFloatValue(value *openAIFlexibleFloat) *float64 {
	if value == nil {
		return nil
	}
	return value.Value()
}

func fetchGatewayAPIKeyQuota(ctx context.Context, account core.Account) (core.AccountQuotaSnapshot, error) {
	baseURL := gatewayQuotaBaseURL(account)
	if baseURL == "" {
		return core.AccountQuotaSnapshot{}, fmt.Errorf("gateway quota refresh requires a base url")
	}
	body, err := fetchOpenAIAPIKeyBilling(ctx, appendEndpointSuffix(baseURL, "/ag/v1/account/quota"), account.Credential.AccessToken)
	if err != nil {
		return core.AccountQuotaSnapshot{}, err
	}
	var payload gatewayQuotaResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return core.AccountQuotaSnapshot{}, err
	}

	balance := float64(payload.RemainingNanoUSD) / float64(core.NanoUSDPerUSD)
	refreshedAt := time.Now().UTC()
	return core.AccountQuotaSnapshot{
		Source: "ai_gateway_quota",
		Plan:   "api_key",
		Credits: &core.AccountQuotaCredits{
			HasCredits: true,
			Balance:    &balance,
		},
		RefreshedAt: &refreshedAt,
	}, nil
}

func gatewayQuotaBaseURL(account core.Account) string {
	baseURL := strings.TrimRight(strings.TrimSpace(account.Credential.Metadata["base_url"]), "/")
	if strings.HasSuffix(baseURL, "/v1") {
		return strings.TrimSuffix(baseURL, "/v1")
	}
	return baseURL
}

func isOfficialOpenAIAPIBaseURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Hostname(), "api.openai.com")
}

func fetchOpenAIAPIKeyBilling(ctx context.Context, url string, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	req.Header.Set("User-Agent", "ai-gateway")

	resp, body, err := doOAuthRaw(req)
	if err != nil {
		if resp != nil {
			return nil, mapHTTPErrorForContext(ctx, resp.StatusCode, body)
		}
		return nil, &InvokeError{Code: ErrorCodeUpstreamTransportError, Temporary: true, Cooldown: 20 * time.Second, Err: err}
	}
	if resp.StatusCode >= 400 {
		return nil, mapHTTPErrorForContext(ctx, resp.StatusCode, body)
	}
	return body, nil
}

func openAIAccountLoginMethod(account core.Account) string {
	method := strings.TrimSpace(account.Credential.Metadata["account_login_method"])
	if method != "" {
		return method
	}
	if strings.TrimSpace(account.Credential.Metadata["account_type"]) == "official" {
		return "token"
	}
	return "api_key"
}

func percentOfLimit(used float64, limit float64) float64 {
	if limit <= 0 {
		return 0
	}
	percent := used / limit * 100
	if percent < 0 {
		return 0
	}
	return percent
}

func openAIQuotaWindow(name string, payload *openAIRateLimitBody, primary bool) *core.AccountQuotaWindow {
	if payload == nil {
		return nil
	}
	var window *openAIRateLimitWindow
	if primary {
		window = payload.PrimaryWindow
	} else {
		window = payload.SecondaryWindow
	}
	if window == nil {
		return nil
	}
	out := &core.AccountQuotaWindow{
		Name:          name,
		UsedPercent:   window.UsedPercent,
		WindowMinutes: secondsToMinutes(window.LimitWindowSeconds),
	}
	if window.ResetAt > 0 {
		reset := time.Unix(window.ResetAt, 0).UTC()
		out.ResetsAt = &reset
	}
	return out
}

func openAIAdditionalQuotaSnapshots(items []openAIAdditionalRateLimitBody, plan string, refreshedAt time.Time) map[string]core.AccountQuotaSnapshot {
	if len(items) == 0 {
		return nil
	}
	out := make(map[string]core.AccountQuotaSnapshot, len(items))
	for _, item := range items {
		limitID := strings.TrimSpace(item.MeteredFeature)
		if limitID == "" {
			limitID = strings.TrimSpace(item.LimitName)
		}
		if limitID == "" {
			continue
		}
		reset := refreshedAt
		out[limitID] = core.AccountQuotaSnapshot{
			LimitID:     limitID,
			LimitName:   strings.TrimSpace(item.LimitName),
			Source:      openAIUsageDefaultSource,
			Plan:        plan,
			Primary:     openAIQuotaWindow("primary", item.RateLimit, true),
			Secondary:   openAIQuotaWindow("secondary", item.RateLimit, false),
			RefreshedAt: &reset,
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func openAICredits(payload *openAICreditsBody) *core.AccountQuotaCredits {
	if payload == nil {
		return nil
	}
	var balance *float64
	if payload.Balance != nil {
		balance = payload.Balance.Value()
	}
	return &core.AccountQuotaCredits{
		HasCredits: payload.HasCredits,
		Unlimited:  payload.Unlimited,
		Balance:    balance,
	}
}

func secondsToMinutes(seconds int64) int64 {
	if seconds <= 0 {
		return 0
	}
	return (seconds + 59) / 60
}

func reachedType(payload *struct {
	Type string `json:"type"`
}) string {
	if payload == nil {
		return ""
	}
	return payload.Type
}

type openAIFlexibleFloat struct {
	value *float64
}

func (f *openAIFlexibleFloat) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		f.value = nil
		return nil
	}

	var number float64
	if err := json.Unmarshal(data, &number); err == nil {
		f.value = &number
		return nil
	}

	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		f.value = nil
		return nil
	}
	number, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return err
	}
	f.value = &number
	return nil
}

func (f *openAIFlexibleFloat) Value() *float64 {
	if f == nil || f.value == nil {
		return nil
	}
	value := *f.value
	return &value
}

type openAIFlexibleInt struct {
	value *int64
}

func (f *openAIFlexibleInt) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		f.value = nil
		return nil
	}

	var number float64
	if err := json.Unmarshal(data, &number); err == nil {
		value := int64(number)
		f.value = &value
		return nil
	}

	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		f.value = nil
		return nil
	}
	if parsed, err := strconv.ParseInt(text, 10, 64); err == nil {
		f.value = &parsed
		return nil
	}
	number, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return err
	}
	value := int64(number)
	f.value = &value
	return nil
}

func (f *openAIFlexibleInt) Value() *int64 {
	if f == nil || f.value == nil {
		return nil
	}
	value := *f.value
	return &value
}
