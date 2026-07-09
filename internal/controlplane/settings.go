package controlplane

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func (s *Service) GetSystemSettings() (core.SystemSettings, error) {
	if settings, ok := s.cachedSystemSettings(); ok {
		return settings, nil
	}
	return s.loadSystemSettings()
}

func (s *Service) loadSystemSettings() (core.SystemSettings, error) {
	settings, err := s.repo.GetSystemSettings()
	if err != nil {
		return core.SystemSettings{}, err
	}
	settings = core.NormalizeSystemSettings(settings)
	s.systemSettings.Store(settings)
	return settings, nil
}

func (s *Service) GetStartupSystemSettings() (core.SystemSettings, error) {
	settings, err := storage.LoadStartupSystemSettings(s.repo)
	if err != nil {
		return core.SystemSettings{}, err
	}
	settings = core.StartupSystemSettingsFrom(settings)
	return settings, nil
}

func (s *Service) cachedSystemSettings() (core.SystemSettings, bool) {
	value := s.systemSettings.Load()
	if value == nil {
		return core.SystemSettings{}, false
	}
	settings, ok := value.(core.SystemSettings)
	if !ok {
		return core.SystemSettings{}, false
	}
	return core.NormalizeSystemSettings(settings), true
}

func (s *Service) currentSystemSettings() core.SystemSettings {
	settings, err := s.GetSystemSettings()
	if err != nil {
		return core.DefaultSystemSettings()
	}
	return settings
}

func (s *Service) UpdateSystemSettings(settings core.SystemSettings) (core.SystemSettings, error) {
	settings = core.NormalizeSystemSettings(settings)
	if err := validateSystemSettings(settings); err != nil {
		return core.SystemSettings{}, err
	}
	if err := s.repo.UpsertSystemSettings(settings); err != nil {
		return core.SystemSettings{}, err
	}
	if err := s.applyRetentionSettings(settings); err != nil {
		return core.SystemSettings{}, err
	}
	updated, err := s.loadSystemSettings()
	if err != nil {
		return core.SystemSettings{}, err
	}
	s.notifySystemSettingsHook(updated)
	return updated, nil
}

type SystemSettingsFallbacks struct {
	PublicBaseURL             string
	GatewayAuditErrors        bool
	GatewayAuditRetentionDays int
}

func (s *Service) ApplySystemSettings(auditLimitFallback int, publicBaseURLFallback ...string) error {
	fallbacks := SystemSettingsFallbacks{}
	if len(publicBaseURLFallback) > 0 {
		fallbacks.PublicBaseURL = publicBaseURLFallback[0]
	}
	return s.ApplySystemSettingsWithFallbacks(auditLimitFallback, fallbacks)
}

func (s *Service) ApplySystemSettingsWithFallbacks(auditLimitFallback int, fallbacks SystemSettingsFallbacks) error {
	settings, err := s.GetStartupSystemSettings()
	if err != nil {
		return err
	}
	changed := false
	if settings.UpdatedAt.IsZero() && auditLimitFallback > 0 {
		settings.Retention.AuditLimit = auditLimitFallback
		changed = true
	}
	if settings.UpdatedAt.IsZero() {
		settings.Retention.GatewayAuditErrors = fallbacks.GatewayAuditErrors
		if fallbacks.GatewayAuditRetentionDays > 0 {
			settings.Retention.GatewayAuditRetentionDays = fallbacks.GatewayAuditRetentionDays
		}
		changed = true
	}
	if fallbacks.PublicBaseURL != "" {
		publicBaseURL := strings.TrimSpace(fallbacks.PublicBaseURL)
		if publicBaseURL != "" && settings.Runtime.PublicBaseURL != publicBaseURL {
			settings.Runtime.PublicBaseURL = publicBaseURL
			changed = true
		}
	}
	if changed {
		fullSettings, err := s.loadSystemSettings()
		if err != nil {
			return err
		}
		if fullSettings.UpdatedAt.IsZero() && auditLimitFallback > 0 {
			fullSettings.Retention.AuditLimit = auditLimitFallback
		}
		if fullSettings.UpdatedAt.IsZero() {
			fullSettings.Retention.GatewayAuditErrors = fallbacks.GatewayAuditErrors
			if fallbacks.GatewayAuditRetentionDays > 0 {
				fullSettings.Retention.GatewayAuditRetentionDays = fallbacks.GatewayAuditRetentionDays
			}
		}
		if fallbacks.PublicBaseURL != "" {
			publicBaseURL := strings.TrimSpace(fallbacks.PublicBaseURL)
			if publicBaseURL != "" {
				fullSettings.Runtime.PublicBaseURL = publicBaseURL
			}
		}
		fullSettings = core.NormalizeSystemSettings(fullSettings)
		if err := s.repo.UpsertSystemSettings(fullSettings); err != nil {
			return err
		}
		settings, err = s.loadSystemSettings()
		if err != nil {
			return err
		}
	}
	if err := s.applyRetentionSettings(settings); err != nil {
		return err
	}
	s.notifySystemSettingsHook(settings)
	return nil
}

func (s *Service) applyRetentionSettings(settings core.SystemSettings) error {
	settings = core.NormalizeSystemSettings(settings)
	if err := s.repo.ConfigureAuditLimit(settings.Retention.AuditLimit); err != nil {
		return err
	}
	if err := s.repo.ConfigureUsageLogRetention(settings.Retention.UsageLogMaxAgeDays); err != nil {
		return err
	}
	if err := s.repo.ConfigureBillingLedgerRetention(settings.Retention.BillingLedgerRetentionDays); err != nil {
		return err
	}
	if s.gatewayAuditRetention.Load() {
		retentionDays := 0
		if settings.Retention.GatewayAuditErrors {
			retentionDays = settings.Retention.GatewayAuditRetentionDays
		}
		if err := s.repo.ConfigureGatewayAuditRetention(retentionDays); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) SystemProxyURL() string {
	return s.currentSystemSettings().Network.SystemProxyURL
}

func (s *Service) OAuthEnabled(provider core.ProviderKind) bool {
	settings := s.currentSystemSettings()
	switch provider {
	case core.ProviderOpenAI:
		return settings.OAuth.OpenAIEnabled
	case core.ProviderClaude:
		return settings.OAuth.ClaudeEnabled
	default:
		return false
	}
}

func (s *Service) WithSystemProxy(ctx context.Context) context.Context {
	return providers.WithProxyURL(ctx, s.SystemProxyURL())
}

func (s *Service) PublicBaseURL() string {
	return s.currentSystemSettings().Runtime.PublicBaseURL
}

func (s *Service) RegistrationEmailRequired() bool {
	settings := s.currentSystemSettings()
	return settings.Email.RegistrationVerificationEnabled || settings.Runtime.RegistrationEmailAllowlistEnabled
}

func (s *Service) ValidateRegistrationEmail(email string) error {
	settings := s.currentSystemSettings()
	if !settings.Runtime.RegistrationEmailAllowlistEnabled {
		return nil
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return fmt.Errorf("email is required")
	}
	for _, allowedDomain := range settings.Runtime.RegistrationEmailAllowlist {
		if strings.HasSuffix(email, allowedDomain) && len(email) > len(allowedDomain) {
			return nil
		}
	}
	return fmt.Errorf("email is not allowed to register")
}

func validateSystemSettings(settings core.SystemSettings) error {
	if settings.Runtime.PublicBaseURL != "" {
		parsed, err := url.Parse(settings.Runtime.PublicBaseURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("public base url must be a valid URL")
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https":
		default:
			return fmt.Errorf("public base url scheme must be http or https")
		}
	}
	if err := validateProxyURL(settings.Network.SystemProxyURL); err != nil {
		return fmt.Errorf("system proxy url: %w", err)
	}
	if err := validateImageSettings(settings.Image); err != nil {
		return fmt.Errorf("image generation: %w", err)
	}
	if settings.Runtime.RegistrationEmailAllowlistEnabled {
		if len(settings.Runtime.RegistrationEmailAllowlist) == 0 {
			return fmt.Errorf("registration email allowlist is required")
		}
		for _, domain := range settings.Runtime.RegistrationEmailAllowlist {
			if !strings.HasPrefix(domain, "@") || strings.Count(domain, "@") != 1 || !strings.Contains(domain, ".") || strings.ContainsAny(domain, " \t\r\n") {
				return fmt.Errorf("registration email allowlist contains invalid domain: %s", domain)
			}
		}
	}
	if settings.Email.RegistrationVerificationEnabled {
		if err := validateEmailVerificationSettings(settings.Email); err != nil {
			return fmt.Errorf("email verification: %w", err)
		}
	}
	if settings.Registration.RequireInvitationCode && !settings.Invitation.Enabled {
		return fmt.Errorf("invitation registration must be enabled when registration requires an invitation code")
	}
	if settings.Registration.TurnstileEnabled {
		if strings.TrimSpace(settings.Registration.TurnstileSiteKey) == "" {
			return fmt.Errorf("turnstile site key is required when Turnstile is enabled")
		}
		if strings.TrimSpace(settings.Registration.TurnstileSecretKey) == "" {
			return fmt.Errorf("turnstile secret key is required when Turnstile is enabled")
		}
	}
	if err := validatePaymentSettings(settings.Payment); err != nil {
		return fmt.Errorf("payment settings: %w", err)
	}
	if settings.Backup.AndroidAutoEnabled {
		if !settings.Payment.PersonalPay.Enabled {
			return fmt.Errorf("android backup requires PersonalPay to be enabled")
		}
		if strings.TrimSpace(settings.Payment.PersonalPay.AndroidToken) == "" {
			return fmt.Errorf("android backup requires a PersonalPay android token")
		}
	}
	return nil
}

func validateImageSettings(settings core.SystemImageSettings) error {
	switch settings.Backend {
	case core.ImageBackendAuto, core.ImageBackendOfficial:
		return nil
	default:
		return fmt.Errorf("unsupported image backend %q", settings.Backend)
	}
}

func validatePaymentSettings(settings core.SystemPaymentSettings) error {
	if _, err := cnyCentsForCreditAmount(core.NanoUSDPerUSD, settings.CNYPerUSD); err != nil {
		return err
	}
	if settings.MaxRechargeNanoUSD > 0 && settings.MinRechargeNanoUSD > settings.MaxRechargeNanoUSD {
		return fmt.Errorf("maximum recharge must be greater than or equal to minimum recharge")
	}
	if settings.PersonalPay.Enabled && strings.TrimSpace(settings.PersonalPay.AndroidToken) == "" {
		return fmt.Errorf("personalpay android token is required when PersonalPay is enabled")
	}
	if settings.WeChatPay.Enabled {
		for _, item := range []struct {
			name  string
			value string
		}{
			{name: "wechat pay app id", value: settings.WeChatPay.AppID},
			{name: "wechat pay merchant id", value: settings.WeChatPay.MchID},
			{name: "wechat pay api v3 key", value: settings.WeChatPay.APIV3Key},
			{name: "wechat pay merchant serial no", value: settings.WeChatPay.MerchantSerialNo},
			{name: "wechat pay merchant private key", value: settings.WeChatPay.MerchantPrivateKeyPEM},
			{name: "wechat pay public key", value: settings.WeChatPay.WeChatPayPublicKeyPEM},
		} {
			if strings.TrimSpace(item.value) == "" {
				return fmt.Errorf("%s is required when WeChat Pay is enabled", item.name)
			}
		}
	}
	if settings.Alipay.Enabled {
		for _, item := range []struct {
			name  string
			value string
		}{
			{name: "alipay app id", value: settings.Alipay.AppID},
			{name: "alipay private key", value: settings.Alipay.PrivateKeyPEM},
			{name: "alipay public key", value: settings.Alipay.AlipayPublicKeyPEM},
		} {
			if strings.TrimSpace(item.value) == "" {
				return fmt.Errorf("%s is required when Alipay is enabled", item.name)
			}
		}
	}
	return nil
}
