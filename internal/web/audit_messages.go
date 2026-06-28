package web

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

type auditFieldChange struct {
	Key  string
	From string
	To   string
}

func auditChangeMessage(changes ...auditFieldChange) string {
	fields := make([]auditMessageField, 0, len(changes)*2)
	for _, change := range changes {
		key := strings.TrimSpace(change.Key)
		if key == "" || strings.TrimSpace(change.From) == strings.TrimSpace(change.To) {
			continue
		}
		fields = append(fields,
			auditMessageField{Key: key + "_from", Value: change.From},
			auditMessageField{Key: key + "_to", Value: change.To},
		)
	}
	if len(fields) == 0 {
		return auditFieldsMessage(auditMessageField{Key: "no_changes", Value: "true"})
	}
	return auditFieldsMessage(fields...)
}

func auditFieldsMessage(fields ...auditMessageField) string {
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		key := strings.TrimSpace(field.Key)
		if key == "" {
			continue
		}
		parts = append(parts, key+"="+auditEncodeFieldValue(field.Value))
	}
	return strings.Join(parts, " ")
}

func auditTextChange(key, from, to string) auditFieldChange {
	return auditFieldChange{Key: key, From: compactAuditFieldValue(from), To: compactAuditFieldValue(to)}
}

func auditBoolChange(key string, from, to bool) auditFieldChange {
	return auditFieldChange{Key: key, From: strconv.FormatBool(from), To: strconv.FormatBool(to)}
}

func auditIntChange(key string, from, to int) auditFieldChange {
	return auditFieldChange{Key: key, From: strconv.Itoa(from), To: strconv.Itoa(to)}
}

func auditFloatChange(key string, from, to float64) auditFieldChange {
	return auditFieldChange{Key: key, From: strconv.FormatFloat(from, 'g', -1, 64), To: strconv.FormatFloat(to, 'g', -1, 64)}
}

func auditOptionalIntChange(key string, from, to *int) auditFieldChange {
	return auditFieldChange{Key: key, From: auditOptionalIntValue(from), To: auditOptionalIntValue(to)}
}

func auditOptionalIntValue(value *int) string {
	if value == nil {
		return "inherit"
	}
	return strconv.Itoa(*value)
}

func auditAmountChange(key string, from, to int64) auditFieldChange {
	return auditFieldChange{Key: key, From: core.FormatNanoUSD(from), To: core.FormatNanoUSD(to)}
}

func auditMultiplierChange(key string, from, to int64) auditFieldChange {
	return auditFieldChange{Key: key, From: core.FormatMultiplier(from), To: core.FormatMultiplier(to)}
}

func auditStringSliceChange(key string, from, to []string) auditFieldChange {
	return auditTextChange(key, auditStringListValue(from), auditStringListValue(to))
}

func auditEncodeFieldValue(value string) string {
	return url.PathEscape(compactAuditFieldValue(value))
}

func auditDecodeFieldValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return value
	}
	return decoded
}

func compactAuditFieldValue(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\t", " ")
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	return truncateRunes(value, 180)
}

func auditStringListValue(values []string) string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		out = append(out, value)
	}
	return strings.Join(out, ",")
}

func auditModelPricingChangeMessage(before, after core.ModelConfig, hasBefore bool) string {
	if !hasBefore {
		return auditFieldsMessage(
			auditMessageField{Key: "mode", Value: after.BillingMode},
			auditMessageField{Key: "fixed", Value: strconv.FormatBool(after.BillingFixed)},
			auditMessageField{Key: "input", Value: core.FormatNanoUSD(after.InputPriceNanoUSDPer1M)},
			auditMessageField{Key: "cached_input", Value: core.FormatNanoUSD(after.CachedInputPriceNanoUSDPer1M)},
			auditMessageField{Key: "output", Value: core.FormatNanoUSD(after.OutputPriceNanoUSDPer1M)},
			auditMessageField{Key: "request", Value: core.FormatNanoUSD(after.RequestPriceNanoUSD)},
			auditMessageField{Key: "tiers", Value: strconv.Itoa(len(after.PricingTiers))},
		)
	}
	return auditChangeMessage(
		auditTextChange("mode", before.BillingMode, after.BillingMode),
		auditBoolChange("fixed", before.BillingFixed, after.BillingFixed),
		auditAmountChange("input", before.InputPriceNanoUSDPer1M, after.InputPriceNanoUSDPer1M),
		auditAmountChange("cached_input", before.CachedInputPriceNanoUSDPer1M, after.CachedInputPriceNanoUSDPer1M),
		auditAmountChange("output", before.OutputPriceNanoUSDPer1M, after.OutputPriceNanoUSDPer1M),
		auditAmountChange("request", before.RequestPriceNanoUSD, after.RequestPriceNanoUSD),
		auditIntChange("tiers", len(before.PricingTiers), len(after.PricingTiers)),
	)
}

func auditModelGroupsChangeMessage(before, after core.ModelConfig, hasBefore bool) string {
	if !hasBefore {
		return auditFieldsMessage(auditMessageField{Key: "groups", Value: auditStringListValue(after.VisibleGroups)})
	}
	return auditChangeMessage(auditStringSliceChange("groups", before.VisibleGroups, after.VisibleGroups))
}

func auditClientUpdateChangeMessage(before, after core.APIClient, hasBefore bool) string {
	if !hasBefore {
		return auditFieldsMessage(
			auditMessageField{Key: "enabled", Value: strconv.FormatBool(after.Enabled)},
			auditMessageField{Key: "spend_limit", Value: core.FormatNanoUSD(after.SpendLimitNanoUSD)},
			auditMessageField{Key: "route", Value: auditRouteText(after.RoutePolicy)},
			auditMessageField{Key: "scope", Value: auditClientScopeText(after)},
			auditMessageField{Key: "billing_source", Value: core.NormalizeClientBillingSource(after.BillingSource)},
		)
	}
	return auditChangeMessage(
		auditTextChange("name", before.Name, after.Name),
		auditBoolChange("enabled", before.Enabled, after.Enabled),
		auditAmountChange("spend_limit", before.SpendLimitNanoUSD, after.SpendLimitNanoUSD),
		auditTextChange("route", auditRouteText(before.RoutePolicy), auditRouteText(after.RoutePolicy)),
		auditTextChange("scope", auditClientScopeText(before), auditClientScopeText(after)),
		auditTextChange("billing_source", core.NormalizeClientBillingSource(before.BillingSource), core.NormalizeClientBillingSource(after.BillingSource)),
	)
}

func auditAccountUpdateChangeMessage(before, after core.Account, hasBefore bool, accessTokenChanged, refreshTokenChanged, sessionTokenChanged bool) string {
	if !hasBefore {
		return auditFieldsMessage(
			auditMessageField{Key: "status", Value: string(after.Status)},
			auditMessageField{Key: "remark", Value: after.Remark},
			auditMessageField{Key: "control_disabled", Value: strconv.FormatBool(after.ControlDisabled)},
			auditMessageField{Key: "backup", Value: strconv.FormatBool(after.Backup)},
			auditMessageField{Key: "priority", Value: strconv.Itoa(after.Priority)},
			auditMessageField{Key: "weight", Value: strconv.Itoa(after.Weight)},
		)
	}
	changes := []auditFieldChange{
		auditTextChange("label", before.Label, after.Label),
		auditTextChange("remark", before.Remark, after.Remark),
		auditTextChange("account_group", before.Group, after.Group),
		auditTextChange("proxy_url", maskProxyURL(before.ProxyURL), maskProxyURL(after.ProxyURL)),
		auditTextChange("base_url", before.Credential.Metadata["base_url"], after.Credential.Metadata["base_url"]),
		auditTextChange("expires_at", auditTimePtrText(before.Credential.ExpiresAt), auditTimePtrText(after.Credential.ExpiresAt)),
		auditIntChange("priority", before.Priority, after.Priority),
		auditIntChange("weight", before.Weight, after.Weight),
		auditTextChange("status", string(before.Status), string(after.Status)),
		auditBoolChange("control_disabled", before.ControlDisabled, after.ControlDisabled),
		auditBoolChange("backup", before.Backup, after.Backup),
	}
	if accessTokenChanged {
		changes = append(changes, auditBoolChange("access_token_changed", false, true))
	}
	if refreshTokenChanged {
		changes = append(changes, auditBoolChange("refresh_token_changed", false, true))
	}
	if sessionTokenChanged {
		changes = append(changes, auditBoolChange("session_token_changed", false, true))
	}
	return auditChangeMessage(changes...)
}

func auditAccountGroupBillingChangeMessage(before, after core.AccountGroup, hasBefore bool) string {
	if !hasBefore {
		return auditFieldsMessage(
			auditMessageField{Key: "multiplier", Value: core.FormatMultiplier(after.BillingMultiplierBps)},
			auditMessageField{Key: "plan_multiplier", Value: core.FormatMultiplier(after.PlanBillingMultiplierBps)},
			auditMessageField{Key: "plan_billing_enabled", Value: strconv.FormatBool(core.AccountGroupPlanBillingEnabled(after))},
			auditMessageField{Key: "input", Value: core.FormatNanoUSD(after.InputPriceNanoUSDPer1M)},
			auditMessageField{Key: "cached_input", Value: core.FormatNanoUSD(after.CachedInputPriceNanoUSDPer1M)},
			auditMessageField{Key: "cache_write", Value: core.FormatNanoUSD(after.CacheWritePriceNanoUSDPer1M)},
			auditMessageField{Key: "cache_write_5m", Value: core.FormatNanoUSD(after.CacheWrite5mPriceNanoUSDPer1M)},
			auditMessageField{Key: "cache_write_1h", Value: core.FormatNanoUSD(after.CacheWrite1hPriceNanoUSDPer1M)},
			auditMessageField{Key: "output", Value: core.FormatNanoUSD(after.OutputPriceNanoUSDPer1M)},
			auditMessageField{Key: "image_output", Value: core.FormatNanoUSD(after.ImageOutputPriceNanoUSDPer1M)},
		)
	}
	return auditChangeMessage(
		auditMultiplierChange("multiplier", before.BillingMultiplierBps, after.BillingMultiplierBps),
		auditMultiplierChange("plan_multiplier", before.PlanBillingMultiplierBps, after.PlanBillingMultiplierBps),
		auditBoolChange("plan_billing_enabled", core.AccountGroupPlanBillingEnabled(before), core.AccountGroupPlanBillingEnabled(after)),
		auditAmountChange("input", before.InputPriceNanoUSDPer1M, after.InputPriceNanoUSDPer1M),
		auditAmountChange("cached_input", before.CachedInputPriceNanoUSDPer1M, after.CachedInputPriceNanoUSDPer1M),
		auditAmountChange("cache_write", before.CacheWritePriceNanoUSDPer1M, after.CacheWritePriceNanoUSDPer1M),
		auditAmountChange("cache_write_5m", before.CacheWrite5mPriceNanoUSDPer1M, after.CacheWrite5mPriceNanoUSDPer1M),
		auditAmountChange("cache_write_1h", before.CacheWrite1hPriceNanoUSDPer1M, after.CacheWrite1hPriceNanoUSDPer1M),
		auditAmountChange("output", before.OutputPriceNanoUSDPer1M, after.OutputPriceNanoUSDPer1M),
		auditAmountChange("image_output", before.ImageOutputPriceNanoUSDPer1M, after.ImageOutputPriceNanoUSDPer1M),
		auditIntChange("timed_rules", len(before.TimedMultipliers), len(after.TimedMultipliers)),
	)
}

func auditUserUpdateChangeMessage(before, after core.User, hasBefore bool, passwordChanged bool) string {
	if !hasBefore {
		fields := []auditMessageField{
			auditMessageField{Key: "role", Value: string(after.Role)},
			auditMessageField{Key: "enabled", Value: strconv.FormatBool(after.Enabled)},
		}
		if after.ConcurrentRequestLimitOverride != nil {
			fields = append(fields, auditMessageField{Key: "user_concurrent_request_limit_override", Value: auditOptionalIntValue(after.ConcurrentRequestLimitOverride)})
		}
		if after.RequestRateLimitPerMinuteOverride != nil {
			fields = append(fields, auditMessageField{Key: "user_request_rate_limit_override", Value: auditOptionalIntValue(after.RequestRateLimitPerMinuteOverride)})
		}
		return auditFieldsMessage(fields...)
	}
	changes := []auditFieldChange{
		auditTextChange("username", before.Username, after.Username),
		auditTextChange("role", string(before.Role), string(after.Role)),
		auditBoolChange("enabled", before.Enabled, after.Enabled),
		auditOptionalIntChange("user_concurrent_request_limit_override", before.ConcurrentRequestLimitOverride, after.ConcurrentRequestLimitOverride),
		auditOptionalIntChange("user_request_rate_limit_override", before.RequestRateLimitPerMinuteOverride, after.RequestRateLimitPerMinuteOverride),
	}
	if passwordChanged {
		changes = append(changes, auditBoolChange("password_changed", false, true))
	}
	return auditChangeMessage(changes...)
}

func auditSiteMessageUpdateChangeMessage(before, after core.SiteMessage, hasBefore bool) string {
	if !hasBefore {
		return auditFieldsMessage(auditMessageField{Key: "enabled", Value: strconv.FormatBool(after.Enabled)})
	}
	return auditChangeMessage(
		auditTextChange("title", before.Title, after.Title),
		auditTextChange("body", before.Body, after.Body),
		auditBoolChange("enabled", before.Enabled, after.Enabled),
		auditStringSliceChange("target_users", before.TargetUserIDs, after.TargetUserIDs),
		auditStringSliceChange("target_groups", before.TargetAccountGroups, after.TargetAccountGroups),
	)
}

func auditSystemSettingsChangeMessage(before, after core.SystemSettings, hasBefore bool) string {
	after = core.NormalizeSystemSettings(after)
	if !hasBefore {
		defaultSettings := core.NormalizeSystemSettings(core.DefaultSystemSettings())
		defaultImage := defaultSettings.Image
		fields := []auditMessageField{
			{Key: "audit_limit", Value: strconv.Itoa(after.Retention.AuditLimit)},
			{Key: "usage_log_max_age_days", Value: strconv.Itoa(after.Retention.UsageLogMaxAgeDays)},
			{Key: "billing_ledger_retention_days", Value: strconv.Itoa(after.Retention.BillingLedgerRetentionDays)},
		}
		if after.Runtime.UserConcurrentRequestLimit > 0 {
			fields = append(fields, auditMessageField{Key: "user_concurrent_request_limit", Value: strconv.Itoa(after.Runtime.UserConcurrentRequestLimit)})
		}
		if after.Runtime.PlanConcurrentRequestLimit > 0 {
			fields = append(fields, auditMessageField{Key: "plan_concurrent_request_limit", Value: strconv.Itoa(after.Runtime.PlanConcurrentRequestLimit)})
		}
		if after.Runtime.UserRequestRateLimitPerMinute > 0 {
			fields = append(fields, auditMessageField{Key: "user_request_rate_limit_per_minute", Value: strconv.Itoa(after.Runtime.UserRequestRateLimitPerMinute)})
		}
		if core.ResponsesWebSocketUpstreamEnabled(after.Runtime) != core.ResponsesWebSocketUpstreamEnabled(defaultSettings.Runtime) {
			fields = append(fields, auditMessageField{Key: "responses_websocket_upstream_enabled", Value: strconv.FormatBool(core.ResponsesWebSocketUpstreamEnabled(after.Runtime))})
		}
		if core.ImageUserConsoleEnabled(after.Image) != core.ImageUserConsoleEnabled(defaultImage) {
			fields = append(fields, auditMessageField{Key: "image_user_console_enabled", Value: strconv.FormatBool(core.ImageUserConsoleEnabled(after.Image))})
		}
		return auditFieldsMessage(fields...)
	}
	before = core.NormalizeSystemSettings(before)
	return auditChangeMessage(
		auditTextChange("public_base_url", before.Runtime.PublicBaseURL, after.Runtime.PublicBaseURL),
		auditBoolChange("allow_public_registration", before.Runtime.AllowPublicRegistration, after.Runtime.AllowPublicRegistration),
		auditBoolChange("registration_email_allowlist_enabled", before.Runtime.RegistrationEmailAllowlistEnabled, after.Runtime.RegistrationEmailAllowlistEnabled),
		auditStringSliceChange("registration_email_allowlist", before.Runtime.RegistrationEmailAllowlist, after.Runtime.RegistrationEmailAllowlist),
		auditBoolChange("responses_websocket_upstream_enabled", core.ResponsesWebSocketUpstreamEnabled(before.Runtime), core.ResponsesWebSocketUpstreamEnabled(after.Runtime)),
		auditIntChange("user_request_rate_limit_per_minute", before.Runtime.UserRequestRateLimitPerMinute, after.Runtime.UserRequestRateLimitPerMinute),
		auditTextChange("system_proxy_url", maskProxyURL(before.Network.SystemProxyURL), maskProxyURL(after.Network.SystemProxyURL)),
		auditBoolChange("image_user_console_enabled", core.ImageUserConsoleEnabled(before.Image), core.ImageUserConsoleEnabled(after.Image)),
		auditTextChange("image_backend", before.Image.Backend, after.Image.Backend),
		auditBoolChange("openai_oauth_enabled", before.OAuth.OpenAIEnabled, after.OAuth.OpenAIEnabled),
		auditBoolChange("claude_oauth_enabled", before.OAuth.ClaudeEnabled, after.OAuth.ClaudeEnabled),
		auditBoolChange("github_login_enabled", before.OAuth.GitHubLoginEnabled, after.OAuth.GitHubLoginEnabled),
		auditTextChange("github_login_client_id", before.OAuth.GitHubLoginClientID, after.OAuth.GitHubLoginClientID),
		auditBoolChange("github_login_secret_changed", false, before.OAuth.GitHubLoginSecret != after.OAuth.GitHubLoginSecret),
		auditBoolChange("google_login_enabled", before.OAuth.GoogleLoginEnabled, after.OAuth.GoogleLoginEnabled),
		auditTextChange("google_login_client_id", before.OAuth.GoogleLoginClientID, after.OAuth.GoogleLoginClientID),
		auditBoolChange("google_login_secret_changed", false, before.OAuth.GoogleLoginSecret != after.OAuth.GoogleLoginSecret),
		auditBoolChange("login_auto_create_user", before.OAuth.LoginAutoCreateUser, after.OAuth.LoginAutoCreateUser),
		auditBoolChange("registration_verification_enabled", before.Email.RegistrationVerificationEnabled, after.Email.RegistrationVerificationEnabled),
		auditTextChange("email_provider", before.Email.Provider, after.Email.Provider),
		auditTextChange("smtp_host", before.Email.SMTPHost, after.Email.SMTPHost),
		auditIntChange("smtp_port", before.Email.SMTPPort, after.Email.SMTPPort),
		auditTextChange("smtp_username", before.Email.SMTPUsername, after.Email.SMTPUsername),
		auditBoolChange("smtp_password_changed", false, before.Email.SMTPPassword != after.Email.SMTPPassword),
		auditTextChange("cloudmail_base_url", before.Email.CloudMailBaseURL, after.Email.CloudMailBaseURL),
		auditTextChange("cloudmail_email", before.Email.CloudMailEmail, after.Email.CloudMailEmail),
		auditBoolChange("cloudmail_password_changed", false, before.Email.CloudMailPassword != after.Email.CloudMailPassword),
		auditIntChange("cloudmail_account_id", before.Email.CloudMailAccountID, after.Email.CloudMailAccountID),
		auditTextChange("from_email", before.Email.FromEmail, after.Email.FromEmail),
		auditTextChange("from_name", before.Email.FromName, after.Email.FromName),
		auditTextChange("email_template_subject", before.Email.VerificationSubjectTemplate, after.Email.VerificationSubjectTemplate),
		auditTextChange("email_template_text", before.Email.VerificationTextTemplate, after.Email.VerificationTextTemplate),
		auditTextChange("email_template_html", before.Email.VerificationHTMLTemplate, after.Email.VerificationHTMLTemplate),
		auditIntChange("email_code_ttl_seconds", before.Email.CodeTTLSeconds, after.Email.CodeTTLSeconds),
		auditIntChange("email_send_cooldown_seconds", before.Email.SendCooldownSeconds, after.Email.SendCooldownSeconds),
		auditIntChange("email_hourly_send_limit", before.Email.HourlySendLimit, after.Email.HourlySendLimit),
		auditIntChange("email_max_attempts", before.Email.MaxAttempts, after.Email.MaxAttempts),
		auditTextChange("brand_title", before.Home.BrandTitle, after.Home.BrandTitle),
		auditTextChange("brand_subtitle", before.Home.BrandSubtitle, after.Home.BrandSubtitle),
		auditTextChange("home_heading", before.Home.Heading, after.Home.Heading),
		auditTextChange("home_summary", before.Home.Summary, after.Home.Summary),
		auditTextChange("home_availability", before.Home.Availability, after.Home.Availability),
		auditTextChange("home_cost_multiplier", before.Home.CostMultiplier, after.Home.CostMultiplier),
		auditTextChange("home_latency", before.Home.Latency, after.Home.Latency),
		auditTextChange("home_capability", before.Home.Capability, after.Home.Capability),
		auditTextChange("payment_cny_per_usd", before.Payment.CNYPerUSD, after.Payment.CNYPerUSD),
		auditTextChange("payment_recharge_input_mode", before.Payment.RechargeInputMode, after.Payment.RechargeInputMode),
		auditAmountChange("payment_min_recharge", before.Payment.MinRechargeNanoUSD, after.Payment.MinRechargeNanoUSD),
		auditAmountChange("payment_max_recharge", before.Payment.MaxRechargeNanoUSD, after.Payment.MaxRechargeNanoUSD),
		auditBoolChange("personalpay_enabled", before.Payment.PersonalPay.Enabled, after.Payment.PersonalPay.Enabled),
		auditBoolChange("personalpay_android_token_changed", false, before.Payment.PersonalPay.AndroidToken != after.Payment.PersonalPay.AndroidToken),
		auditIntChange("personalpay_expire_after_sec", before.Payment.PersonalPay.ExpireAfterSec, after.Payment.PersonalPay.ExpireAfterSec),
		auditBoolChange("wechatpay_enabled", before.Payment.WeChatPay.Enabled, after.Payment.WeChatPay.Enabled),
		auditTextChange("wechatpay_app_id", before.Payment.WeChatPay.AppID, after.Payment.WeChatPay.AppID),
		auditTextChange("wechatpay_mch_id", before.Payment.WeChatPay.MchID, after.Payment.WeChatPay.MchID),
		auditBoolChange("wechatpay_api_v3_key_changed", false, before.Payment.WeChatPay.APIV3Key != after.Payment.WeChatPay.APIV3Key),
		auditTextChange("wechatpay_merchant_serial_no", before.Payment.WeChatPay.MerchantSerialNo, after.Payment.WeChatPay.MerchantSerialNo),
		auditBoolChange("wechatpay_private_key_changed", false, before.Payment.WeChatPay.MerchantPrivateKeyPEM != after.Payment.WeChatPay.MerchantPrivateKeyPEM),
		auditTextChange("wechatpay_public_key_id", before.Payment.WeChatPay.WeChatPayPublicKeyID, after.Payment.WeChatPay.WeChatPayPublicKeyID),
		auditBoolChange("wechatpay_public_key_changed", false, before.Payment.WeChatPay.WeChatPayPublicKeyPEM != after.Payment.WeChatPay.WeChatPayPublicKeyPEM),
		auditTextChange("wechatpay_notify_url", before.Payment.WeChatPay.NotifyURL, after.Payment.WeChatPay.NotifyURL),
		auditBoolChange("alipay_enabled", before.Payment.Alipay.Enabled, after.Payment.Alipay.Enabled),
		auditTextChange("alipay_app_id", before.Payment.Alipay.AppID, after.Payment.Alipay.AppID),
		auditBoolChange("alipay_private_key_changed", false, before.Payment.Alipay.PrivateKeyPEM != after.Payment.Alipay.PrivateKeyPEM),
		auditBoolChange("alipay_public_key_changed", false, before.Payment.Alipay.AlipayPublicKeyPEM != after.Payment.Alipay.AlipayPublicKeyPEM),
		auditTextChange("alipay_gateway_url", before.Payment.Alipay.GatewayURL, after.Payment.Alipay.GatewayURL),
		auditTextChange("alipay_notify_url", before.Payment.Alipay.NotifyURL, after.Payment.Alipay.NotifyURL),
		auditTextChange("alipay_return_url", before.Payment.Alipay.ReturnURL, after.Payment.Alipay.ReturnURL),
		auditTextChange("alipay_sign_type", before.Payment.Alipay.SignType, after.Payment.Alipay.SignType),
		auditBoolChange("backup_android_auto_enabled", before.Backup.AndroidAutoEnabled, after.Backup.AndroidAutoEnabled),
		auditTextChange("backup_android_time", before.Backup.AndroidTimeOfDay, after.Backup.AndroidTimeOfDay),
		auditStringSliceChange("backup_android_data", before.Backup.AndroidDataSets, after.Backup.AndroidDataSets),
		auditBoolChange("new_user_reward_enabled", before.Registration.NewUserRewardEnabled, after.Registration.NewUserRewardEnabled),
		auditAmountChange("new_user_reward", before.Registration.NewUserRewardNanoUSD, after.Registration.NewUserRewardNanoUSD),
		auditBoolChange("require_invitation_code", before.Registration.RequireInvitationCode, after.Registration.RequireInvitationCode),
		auditIntChange("registration_username_min_length", before.Registration.UsernameMinLength, after.Registration.UsernameMinLength),
		auditIntChange("register_ip_hourly_limit", before.Registration.RegisterIPHourlyLimit, after.Registration.RegisterIPHourlyLimit),
		auditIntChange("email_code_ip_hourly_limit", before.Registration.EmailCodeIPHourlyLimit, after.Registration.EmailCodeIPHourlyLimit),
		auditBoolChange("turnstile_enabled", before.Registration.TurnstileEnabled, after.Registration.TurnstileEnabled),
		auditTextChange("turnstile_site_key", before.Registration.TurnstileSiteKey, after.Registration.TurnstileSiteKey),
		auditBoolChange("turnstile_secret_changed", false, before.Registration.TurnstileSecretKey != after.Registration.TurnstileSecretKey),
		auditBoolChange("invitation_enabled", before.Invitation.Enabled, after.Invitation.Enabled),
		auditTextChange("inviter_recharge_reward_percent", formatPercentBps(before.Invitation.InviterRechargeRewardBps)+"%", formatPercentBps(after.Invitation.InviterRechargeRewardBps)+"%"),
		auditAmountChange("invitee_reward", before.Invitation.InviteeRewardNanoUSD, after.Invitation.InviteeRewardNanoUSD),
		auditBoolChange("user_dashboard_custom_panel_enabled", before.UserDashboard.CustomPanelEnabled, after.UserDashboard.CustomPanelEnabled),
		auditTextChange("user_dashboard_custom_panel_html", before.UserDashboard.CustomPanelHTML, after.UserDashboard.CustomPanelHTML),
		auditIntChange("user_concurrent_request_limit", before.Runtime.UserConcurrentRequestLimit, after.Runtime.UserConcurrentRequestLimit),
		auditIntChange("plan_concurrent_request_limit", before.Runtime.PlanConcurrentRequestLimit, after.Runtime.PlanConcurrentRequestLimit),
		auditIntChange("audit_limit", before.Retention.AuditLimit, after.Retention.AuditLimit),
		auditIntChange("usage_log_max_age_days", before.Retention.UsageLogMaxAgeDays, after.Retention.UsageLogMaxAgeDays),
		auditIntChange("billing_ledger_retention_days", before.Retention.BillingLedgerRetentionDays, after.Retention.BillingLedgerRetentionDays),
	)
}

func (s *Server) auditModelConfig(ctx context.Context, id string) (core.ModelConfig, bool) {
	if s == nil || s.control == nil {
		return core.ModelConfig{}, false
	}
	id = strings.TrimSpace(id)
	for _, model := range s.control.ModelPage(ctx).Models {
		if model.ID == id {
			return model, true
		}
	}
	return core.ModelConfig{}, false
}

func (s *Server) auditAccountGroupConfig(id string) (core.AccountGroup, bool) {
	if s == nil || s.control == nil {
		return core.AccountGroup{}, false
	}
	id = strings.TrimSpace(id)
	for _, group := range s.control.ListAccountGroups() {
		if group.ID == id {
			return core.NormalizeAccountGroupBilling(group), true
		}
	}
	if strings.EqualFold(id, core.DefaultAccountGroupID) {
		planBillingEnabled := true
		return core.AccountGroup{ID: core.DefaultAccountGroupID, Name: core.DefaultAccountGroupName, BillingMultiplierBps: core.AccountGroupDefaultMultiplierBps, PlanBillingMultiplierBps: core.AccountGroupDefaultMultiplierBps, PlanBillingEnabled: &planBillingEnabled}, true
	}
	return core.AccountGroup{}, false
}

func auditAccountGroupShowValue(group core.AccountGroup) bool {
	return core.AccountGroupVisibleInClientEditor(group)
}

func auditTimePtrText(value *time.Time) string {
	if value == nil || value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}
