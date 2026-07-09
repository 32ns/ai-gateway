package storage

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestMemoryRepositorySystemSettings(t *testing.T) {
	repo := NewMemoryRepository()

	defaults, err := repo.GetSystemSettings()
	if err != nil {
		t.Fatalf("GetSystemSettings returned error: %v", err)
	}
	if !defaults.OAuth.OpenAIEnabled || !defaults.OAuth.ClaudeEnabled {
		t.Fatalf("oauth defaults = %#v, want enabled", defaults.OAuth)
	}
	if defaults.Runtime.AllowPublicRegistration {
		t.Fatalf("AllowPublicRegistration default = true, want false")
	}
	if defaults.Registration.RegisterIPHourlyLimit != core.DefaultRegisterIPHourlyLimit || defaults.Registration.EmailCodeIPHourlyLimit != core.DefaultEmailCodeIPHourlyLimit {
		t.Fatalf("registration IP limits = %#v", defaults.Registration)
	}
	if defaults.Email.SMTPPort != core.DefaultSMTPPort {
		t.Fatalf("default SMTPPort = %d, want %d", defaults.Email.SMTPPort, core.DefaultSMTPPort)
	}
	if defaults.Retention.UsageLogMaxAgeDays != core.DefaultUsageLogMaxAgeDays {
		t.Fatalf("default usage log retention = %#v", defaults.Retention)
	}
	if defaults.Retention.BillingLedgerRetentionDays != core.DefaultBillingLedgerRetentionDays {
		t.Fatalf("default billing ledger retention = %#v", defaults.Retention)
	}
	if defaults.Retention.GatewayAuditRetentionDays != core.DefaultGatewayAuditRetentionDays {
		t.Fatalf("default gateway audit retention = %#v", defaults.Retention)
	}
	if defaults.Payment.PersonalPay.ExpireAfterSec != core.DefaultPersonalPayExpireAfterSec {
		t.Fatalf("default personalpay expiry = %d, want %d", defaults.Payment.PersonalPay.ExpireAfterSec, core.DefaultPersonalPayExpireAfterSec)
	}
	if defaults.Email.VerificationSubjectTemplate != core.DefaultEmailSubjectTemplate || defaults.Email.VerificationTextTemplate != core.DefaultEmailTextTemplate || defaults.Email.VerificationHTMLTemplate != core.DefaultEmailHTMLTemplate {
		t.Fatalf("default email templates = %#v", defaults.Email)
	}

	settings := core.DefaultSystemSettings()
	settings.OAuth.OpenAIEnabled = false
	settings.OAuth.GitHubLoginEnabled = true
	settings.OAuth.GitHubLoginClientID = "github-client"
	settings.OAuth.GitHubLoginSecret = "github-secret"
	settings.OAuth.LinuxDOLoginEnabled = true
	settings.OAuth.LinuxDOClientID = "linuxdo-client"
	settings.OAuth.LinuxDOSecret = "linuxdo-secret"
	settings.OAuth.LoginAutoCreateUser = true
	settings.Runtime.AllowPublicRegistration = true
	settings.Runtime.RegistrationEmailAllowlistEnabled = true
	settings.Runtime.RegistrationEmailAllowlist = []string{" @Example.COM ", "@qq.com", "@example.com"}
	settings.Runtime.UserConcurrentRequestLimit = 3
	settings.Runtime.PlanConcurrentRequestLimit = 2
	settings.Runtime.UserRequestRateLimitPerMinute = 60
	settings.Registration.NewUserRewardEnabled = true
	settings.Registration.NewUserRewardNanoUSD = 1750
	settings.Registration.RequireInvitationCode = true
	settings.Registration.RegisterIPHourlyLimit = 13
	settings.Registration.EmailCodeIPHourlyLimit = 7
	settings.Registration.TurnstileEnabled = true
	settings.Registration.TurnstileSiteKey = "turnstile-site"
	settings.Registration.TurnstileSecretKey = "turnstile-secret"
	settings.Invitation.Enabled = true
	settings.Invitation.InviterRechargeRewardBps = 250
	settings.Invitation.InviteeRewardNanoUSD = 1500
	settings.UserDashboard.CustomPanelEnabled = true
	settings.UserDashboard.CustomPanelHTML = "<strong>Console notice</strong>"
	settings.Network.SystemProxyURL = " http://127.0.0.1:7890 "
	settings.Email.Provider = core.EmailProviderCloudMail
	settings.Email.CloudMailBaseURL = " https://mail.example.com/ "
	settings.Email.CloudMailEmail = " Mail@Example.COM "
	settings.Email.CloudMailPassword = "cloudmail-secret"
	settings.Email.CloudMailAccountID = 2
	settings.Email.VerificationSubjectTemplate = " 注册验证码 "
	settings.Email.VerificationTextTemplate = "验证码：{{code}}\r\n有效期：{{minutes}} 分钟"
	settings.Email.VerificationHTMLTemplate = "<p>验证码：<b>{{code}}</b></p>"
	settings.Retention.AuditLimit = 7
	settings.Retention.UsageLogMaxAgeDays = 2
	settings.Retention.BillingLedgerRetentionDays = 5
	settings.Retention.GatewayAuditErrors = true
	settings.Retention.GatewayAuditRetentionDays = 3
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatalf("UpsertSystemSettings returned error: %v", err)
	}

	stored, err := repo.GetSystemSettings()
	if err != nil {
		t.Fatalf("GetSystemSettings returned error: %v", err)
	}
	if stored.OAuth.OpenAIEnabled {
		t.Fatalf("OpenAIEnabled = true, want false")
	}
	if !stored.OAuth.GitHubLoginEnabled || stored.OAuth.GitHubLoginClientID != "github-client" || stored.OAuth.GitHubLoginSecret != "github-secret" || !stored.OAuth.LoginAutoCreateUser {
		t.Fatalf("OAuth = %#v", stored.OAuth)
	}
	if !stored.OAuth.LinuxDOLoginEnabled || stored.OAuth.LinuxDOClientID != "linuxdo-client" || stored.OAuth.LinuxDOSecret != "linuxdo-secret" {
		t.Fatalf("OAuth = %#v", stored.OAuth)
	}
	if !stored.Runtime.AllowPublicRegistration {
		t.Fatalf("AllowPublicRegistration = false, want true")
	}
	if stored.Runtime.UserConcurrentRequestLimit != 3 {
		t.Fatalf("UserConcurrentRequestLimit = %d, want 3", stored.Runtime.UserConcurrentRequestLimit)
	}
	if stored.Runtime.PlanConcurrentRequestLimit != 2 {
		t.Fatalf("PlanConcurrentRequestLimit = %d, want 2", stored.Runtime.PlanConcurrentRequestLimit)
	}
	if stored.Runtime.UserRequestRateLimitPerMinute != 60 {
		t.Fatalf("UserRequestRateLimitPerMinute = %d, want 60", stored.Runtime.UserRequestRateLimitPerMinute)
	}
	if !stored.Runtime.RegistrationEmailAllowlistEnabled || len(stored.Runtime.RegistrationEmailAllowlist) != 2 || stored.Runtime.RegistrationEmailAllowlist[0] != "@example.com" || stored.Runtime.RegistrationEmailAllowlist[1] != "@qq.com" {
		t.Fatalf("RegistrationEmailAllowlist = %#v", stored.Runtime)
	}
	if !stored.Registration.NewUserRewardEnabled || stored.Registration.NewUserRewardNanoUSD != 1750 {
		t.Fatalf("Registration = %#v", stored.Registration)
	}
	if !stored.Registration.RequireInvitationCode || stored.Registration.RegisterIPHourlyLimit != 13 || stored.Registration.EmailCodeIPHourlyLimit != 7 || !stored.Registration.TurnstileEnabled || stored.Registration.TurnstileSiteKey != "turnstile-site" || stored.Registration.TurnstileSecretKey != "turnstile-secret" {
		t.Fatalf("Registration security = %#v", stored.Registration)
	}
	if !stored.Invitation.Enabled || stored.Invitation.InviterRechargeRewardBps != 250 || stored.Invitation.InviteeRewardNanoUSD != 1500 {
		t.Fatalf("Invitation = %#v", stored.Invitation)
	}
	if !stored.UserDashboard.CustomPanelEnabled || stored.UserDashboard.CustomPanelHTML != "<strong>Console notice</strong>" {
		t.Fatalf("UserDashboard = %#v", stored.UserDashboard)
	}
	if stored.Network.SystemProxyURL != "http://127.0.0.1:7890" {
		t.Fatalf("SystemProxyURL = %q", stored.Network.SystemProxyURL)
	}
	if stored.Email.Provider != core.EmailProviderCloudMail || stored.Email.CloudMailBaseURL != "https://mail.example.com" || stored.Email.CloudMailEmail != "mail@example.com" || stored.Email.CloudMailPassword != "cloudmail-secret" || stored.Email.CloudMailAccountID != 2 {
		t.Fatalf("Email = %#v", stored.Email)
	}
	if stored.Email.VerificationSubjectTemplate != "注册验证码" || stored.Email.VerificationTextTemplate != "验证码：{{code}}\n有效期：{{minutes}} 分钟" || stored.Email.VerificationHTMLTemplate != "<p>验证码：<b>{{code}}</b></p>" {
		t.Fatalf("Email templates = %#v", stored.Email)
	}
	if stored.Retention.AuditLimit != 7 {
		t.Fatalf("AuditLimit = %d, want 7", stored.Retention.AuditLimit)
	}
	if stored.Retention.UsageLogMaxAgeDays != 2 {
		t.Fatalf("Usage log retention = %#v", stored.Retention)
	}
	if stored.Retention.BillingLedgerRetentionDays != 5 {
		t.Fatalf("Billing ledger retention = %#v", stored.Retention)
	}
	if !stored.Retention.GatewayAuditErrors {
		t.Fatal("GatewayAuditErrors = false, want true")
	}
	if stored.Retention.GatewayAuditRetentionDays != 3 {
		t.Fatalf("GatewayAuditRetentionDays = %d, want 3", stored.Retention.GatewayAuditRetentionDays)
	}
	if stored.UpdatedAt.IsZero() {
		t.Fatalf("UpdatedAt was not set")
	}
}

func TestSQLiteRepositoryPersistsSystemSettings(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.db")

	repo, err := NewSQLiteRepository(statePath, "test-master-key")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	settings := core.DefaultSystemSettings()
	settings.Runtime.PublicBaseURL = "https://gateway.example.com/"
	settings.Runtime.AllowPublicRegistration = true
	settings.Runtime.RegistrationEmailAllowlistEnabled = true
	settings.Runtime.RegistrationEmailAllowlist = []string{"@example.com"}
	settings.Runtime.UserConcurrentRequestLimit = 4
	settings.Runtime.PlanConcurrentRequestLimit = 6
	settings.Runtime.UserRequestRateLimitPerMinute = 90
	settings.OAuth.GoogleLoginEnabled = true
	settings.OAuth.GoogleLoginClientID = "google-client"
	settings.OAuth.GoogleLoginSecret = "google-secret"
	settings.OAuth.LinuxDOLoginEnabled = true
	settings.OAuth.LinuxDOClientID = "linuxdo-client"
	settings.OAuth.LinuxDOSecret = "linuxdo-secret"
	settings.Registration.NewUserRewardEnabled = true
	settings.Registration.NewUserRewardNanoUSD = 2750
	settings.Registration.RequireInvitationCode = true
	settings.Registration.RegisterIPHourlyLimit = 11
	settings.Registration.EmailCodeIPHourlyLimit = 5
	settings.Registration.TurnstileEnabled = true
	settings.Registration.TurnstileSiteKey = "turnstile-site"
	settings.Registration.TurnstileSecretKey = "turnstile-secret"
	settings.Invitation.Enabled = true
	settings.Invitation.InviterRechargeRewardBps = 450
	settings.Invitation.InviteeRewardNanoUSD = 3500
	settings.UserDashboard.CustomPanelEnabled = true
	settings.UserDashboard.CustomPanelHTML = "<em>Stored console HTML</em>"
	settings.Network.SystemProxyURL = "socks5://127.0.0.1:1080"
	settings.Email.Provider = core.EmailProviderCloudMail
	settings.Email.CloudMailBaseURL = "https://mail.example.com"
	settings.Email.CloudMailEmail = "mail@example.com"
	settings.Email.CloudMailPassword = "cloudmail-secret"
	settings.Email.CloudMailAccountID = 2
	settings.Email.VerificationSubjectTemplate = "Code {{code}}"
	settings.Email.VerificationTextTemplate = "Use {{code}} for {{email}}."
	settings.Email.VerificationHTMLTemplate = "<p>Use <strong>{{code}}</strong> for {{email}}.</p>"
	settings.OAuth.ClaudeEnabled = false
	settings.Retention.UsageLogMaxAgeDays = 6
	settings.Retention.BillingLedgerRetentionDays = 8
	settings.Retention.GatewayAuditErrors = true
	settings.Retention.GatewayAuditRetentionDays = 4
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatalf("UpsertSystemSettings returned error: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	reloaded, err := NewSQLiteRepository(statePath, "test-master-key")
	if err != nil {
		t.Fatalf("reload NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = reloaded.Close() })

	stored, err := reloaded.GetSystemSettings()
	if err != nil {
		t.Fatalf("GetSystemSettings returned error: %v", err)
	}
	if stored.Runtime.PublicBaseURL != "https://gateway.example.com" {
		t.Fatalf("PublicBaseURL = %q", stored.Runtime.PublicBaseURL)
	}
	if !stored.Runtime.AllowPublicRegistration {
		t.Fatalf("AllowPublicRegistration = false, want true")
	}
	if stored.Runtime.UserConcurrentRequestLimit != 4 {
		t.Fatalf("UserConcurrentRequestLimit = %d, want 4", stored.Runtime.UserConcurrentRequestLimit)
	}
	if stored.Runtime.PlanConcurrentRequestLimit != 6 {
		t.Fatalf("PlanConcurrentRequestLimit = %d, want 6", stored.Runtime.PlanConcurrentRequestLimit)
	}
	if stored.Runtime.UserRequestRateLimitPerMinute != 90 {
		t.Fatalf("UserRequestRateLimitPerMinute = %d, want 90", stored.Runtime.UserRequestRateLimitPerMinute)
	}
	if !stored.Runtime.RegistrationEmailAllowlistEnabled || len(stored.Runtime.RegistrationEmailAllowlist) != 1 || stored.Runtime.RegistrationEmailAllowlist[0] != "@example.com" {
		t.Fatalf("RegistrationEmailAllowlist = %#v", stored.Runtime)
	}
	if !stored.Registration.NewUserRewardEnabled || stored.Registration.NewUserRewardNanoUSD != 2750 {
		t.Fatalf("Registration = %#v", stored.Registration)
	}
	if !stored.Registration.RequireInvitationCode || stored.Registration.RegisterIPHourlyLimit != 11 || stored.Registration.EmailCodeIPHourlyLimit != 5 || !stored.Registration.TurnstileEnabled || stored.Registration.TurnstileSiteKey != "turnstile-site" || stored.Registration.TurnstileSecretKey != "turnstile-secret" {
		t.Fatalf("Registration security = %#v", stored.Registration)
	}
	if !stored.Invitation.Enabled || stored.Invitation.InviterRechargeRewardBps != 450 || stored.Invitation.InviteeRewardNanoUSD != 3500 {
		t.Fatalf("Invitation = %#v", stored.Invitation)
	}
	if !stored.UserDashboard.CustomPanelEnabled || stored.UserDashboard.CustomPanelHTML != "<em>Stored console HTML</em>" {
		t.Fatalf("UserDashboard = %#v", stored.UserDashboard)
	}
	if stored.Network.SystemProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("SystemProxyURL = %q", stored.Network.SystemProxyURL)
	}
	if stored.Email.Provider != core.EmailProviderCloudMail || stored.Email.CloudMailBaseURL != "https://mail.example.com" || stored.Email.CloudMailEmail != "mail@example.com" || stored.Email.CloudMailPassword != "cloudmail-secret" || stored.Email.CloudMailAccountID != 2 {
		t.Fatalf("Email = %#v", stored.Email)
	}
	if stored.Email.VerificationSubjectTemplate != "Code {{code}}" || stored.Email.VerificationTextTemplate != "Use {{code}} for {{email}}." || stored.Email.VerificationHTMLTemplate != "<p>Use <strong>{{code}}</strong> for {{email}}.</p>" {
		t.Fatalf("Email templates = %#v", stored.Email)
	}
	if stored.OAuth.ClaudeEnabled {
		t.Fatalf("ClaudeEnabled = true, want false")
	}
	if stored.Retention.UsageLogMaxAgeDays != 6 {
		t.Fatalf("Usage log retention = %#v", stored.Retention)
	}
	if !stored.OAuth.GoogleLoginEnabled || stored.OAuth.GoogleLoginClientID != "google-client" || stored.OAuth.GoogleLoginSecret != "google-secret" {
		t.Fatalf("OAuth = %#v", stored.OAuth)
	}
	if !stored.OAuth.LinuxDOLoginEnabled || stored.OAuth.LinuxDOClientID != "linuxdo-client" || stored.OAuth.LinuxDOSecret != "linuxdo-secret" {
		t.Fatalf("OAuth = %#v", stored.OAuth)
	}
	if stored.Retention.BillingLedgerRetentionDays != 8 {
		t.Fatalf("Billing ledger retention = %#v", stored.Retention)
	}
	if !stored.Retention.GatewayAuditErrors {
		t.Fatal("GatewayAuditErrors = false, want true")
	}
	if stored.Retention.GatewayAuditRetentionDays != 4 {
		t.Fatalf("GatewayAuditRetentionDays = %d, want 4", stored.Retention.GatewayAuditRetentionDays)
	}
	if stored.UpdatedAt.IsZero() {
		t.Fatalf("UpdatedAt was not set")
	}
}

func TestSystemSettingsClampsBillingLedgerRetentionMinimum(t *testing.T) {
	repo := NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.Retention.BillingLedgerRetentionDays = 1
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatalf("UpsertSystemSettings returned error: %v", err)
	}
	stored, err := repo.GetSystemSettings()
	if err != nil {
		t.Fatalf("GetSystemSettings returned error: %v", err)
	}
	if stored.Retention.BillingLedgerRetentionDays != core.MinimumBillingLedgerRetentionDays {
		t.Fatalf("BillingLedgerRetentionDays = %d, want %d", stored.Retention.BillingLedgerRetentionDays, core.MinimumBillingLedgerRetentionDays)
	}
}

func TestSQLiteRepositoryEncryptsSensitiveSettingsAndProxyFields(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.db")
	masterKey := "test-master-key"

	repo, err := NewSQLiteRepository(statePath, masterKey)
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	settings := core.DefaultSystemSettings()
	settings.Network.SystemProxyURL = "http://user:system-proxy-secret@127.0.0.1:7890"
	settings.OAuth.GitHubLoginSecret = "github-login-secret"
	settings.OAuth.GoogleLoginSecret = "google-login-secret"
	settings.Email.SMTPPassword = "smtp-secret"
	settings.Email.CloudMailPassword = "cloudmail-secret"
	settings.Registration.TurnstileSecretKey = "turnstile-secret"
	settings.Payment.WeChatPay.APIV3Key = "wechat-v3-secret"
	settings.Payment.WeChatPay.MerchantPrivateKeyPEM = "wechat-private-key"
	settings.Payment.Alipay.PrivateKeyPEM = "alipay-private-key"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatalf("UpsertSystemSettings returned error: %v", err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_proxy",
		Provider: core.ProviderOpenAI,
		Label:    "Proxy Account",
		ProxyURL: "http://user:account-proxy-secret@proxy.example.com:8080",
		Status:   core.AccountStatusActive,
	}); err != nil {
		t.Fatalf("UpsertAccount returned error: %v", err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:       "group_proxy",
		Name:     "Proxy Group",
		ProxyURL: "socks5://user:group-proxy-secret@proxy.example.com:1080",
	}); err != nil {
		t.Fatalf("UpsertAccountGroup returned error: %v", err)
	}

	rawPayloads := map[string]string{}
	for name, query := range map[string]string{
		"system_settings":     `SELECT payload FROM system_settings WHERE key = 'global'`,
		"account_credentials": `SELECT payload FROM account_credentials WHERE account_id = 'acct_proxy'`,
		"account_groups":      `SELECT payload FROM account_groups WHERE id = 'group_proxy'`,
	} {
		var payload string
		if err := repo.db.QueryRow(query).Scan(&payload); err != nil {
			t.Fatalf("select %s payload: %v", name, err)
		}
		rawPayloads[name] = payload
		if !payloadContainsEncryptedValue(payload) {
			t.Fatalf("%s payload does not contain encrypted values: %s", name, payload)
		}
	}

	for _, secret := range []string{
		"system-proxy-secret",
		"github-login-secret",
		"google-login-secret",
		"smtp-secret",
		"cloudmail-secret",
		"turnstile-secret",
		"wechat-v3-secret",
		"wechat-private-key",
		"alipay-private-key",
		"account-proxy-secret",
		"group-proxy-secret",
	} {
		for name, payload := range rawPayloads {
			if strings.Contains(payload, secret) {
				t.Fatalf("%s payload contains plaintext secret %q: %s", name, secret, payload)
			}
		}
	}

	storedSettings, err := repo.GetSystemSettings()
	if err != nil {
		t.Fatalf("GetSystemSettings returned error: %v", err)
	}
	if storedSettings.Network.SystemProxyURL != "http://user:system-proxy-secret@127.0.0.1:7890" ||
		storedSettings.OAuth.GitHubLoginSecret != "github-login-secret" ||
		storedSettings.OAuth.GoogleLoginSecret != "google-login-secret" ||
		storedSettings.Email.SMTPPassword != "smtp-secret" ||
		storedSettings.Email.CloudMailPassword != "cloudmail-secret" ||
		storedSettings.Registration.TurnstileSecretKey != "turnstile-secret" ||
		storedSettings.Payment.WeChatPay.APIV3Key != "wechat-v3-secret" ||
		storedSettings.Payment.WeChatPay.MerchantPrivateKeyPEM != "wechat-private-key" ||
		storedSettings.Payment.Alipay.PrivateKeyPEM != "alipay-private-key" {
		t.Fatalf("decrypted settings = %#v", storedSettings)
	}

	account, err := repo.GetAccount("acct_proxy")
	if err != nil {
		t.Fatalf("GetAccount returned error: %v", err)
	}
	if account.ProxyURL != "http://user:account-proxy-secret@proxy.example.com:8080" {
		t.Fatalf("account proxy URL = %q", account.ProxyURL)
	}
	groups := repo.ListAccountGroups()
	if len(groups) != 1 || groups[0].ProxyURL != "socks5://user:group-proxy-secret@proxy.example.com:1080" {
		t.Fatalf("account groups = %#v", groups)
	}

	if err := repo.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if _, err := NewSQLiteRepository(statePath, ""); err == nil {
		t.Fatal("expected error when loading encrypted sensitive fields without master_key")
	} else if !strings.Contains(err.Error(), "master_key") {
		t.Fatalf("error = %q, want missing key hint", err.Error())
	}
}

func payloadContainsEncryptedValue(payload string) bool {
	return strings.Contains(payload, encryptedValuePrefixV3)
}
