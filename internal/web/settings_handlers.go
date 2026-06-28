package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/32ns/ai-gateway/internal/core"
)

func (s *Server) handleGatewayPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/gateway" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":    "page_title_gateway",
		"ActiveNav":   "gateway",
		"Locale":      locale,
		"State":       s.control.GatewayDashboard(r.Context()),
		"APIGroups":   gatewayAPICatalog(locale),
		"ErrorGroups": gatewayErrorCatalog(locale),
	}, r)
	s.render(w, "gateway.html", locale, data)
}

func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/settings" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		settings, err := s.control.GetSystemSettings()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.renderSettingsPage(w, r, settings, r.URL.Query().Get("saved") == "1", "", http.StatusOK)
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			s.redirectWithNoticeError(w, r, "/admin/settings", err)
			return
		}
		if err := validatePaymentRechargeLimitForm(r); err != nil {
			s.redirectWithNoticeError(w, r, "/admin/settings#settings-payment", err)
			return
		}
		existing, loadErr := s.control.GetSystemSettings()
		hasExisting := loadErr == nil
		input := systemSettingsFromForm(r, existing)
		if hasExisting {
			preserveBlankSystemSecrets(&input, existing)
		}
		settings, err := s.control.UpdateSystemSettings(input)
		if err != nil {
			s.recordAdminAudit(r, "error", "system_settings.update", "system_settings", "global", "", err.Error())
			s.redirectWithNoticeError(w, r, "/admin/settings", err)
			return
		}
		s.recordAdminAudit(
			r,
			"ok",
			"system_settings.update",
			"system_settings",
			"global",
			"",
			auditSystemSettingsChangeMessage(existing, settings, hasExisting),
		)
		s.publishSettingsUpdated("system")
		http.Redirect(w, r, "/admin/settings?saved=1", http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func validatePaymentRechargeLimitForm(r *http.Request) error {
	minRecharge, err := parseOptionalNanoUSDFormValue(r, "payment_min_recharge_usd")
	if err != nil {
		return fmt.Errorf("minimum recharge: %w", err)
	}
	maxRecharge, err := parseOptionalNanoUSDFormValue(r, "payment_max_recharge_usd")
	if err != nil {
		return fmt.Errorf("maximum recharge: %w", err)
	}
	if maxRecharge > 0 && minRecharge > maxRecharge {
		return fmt.Errorf("maximum recharge must be greater than or equal to minimum recharge")
	}
	return nil
}

func parseOptionalNanoUSDFormValue(r *http.Request, name string) (int64, error) {
	raw := strings.TrimSpace(r.FormValue(name))
	if raw == "" {
		return 0, nil
	}
	value, err := core.ParseNanoUSDDecimal(raw)
	if err != nil {
		return 0, err
	}
	if value < 0 {
		return 0, fmt.Errorf("amount must be zero or greater")
	}
	return value, nil
}

func (s *Server) handlePersonalPayDeviceActions(w http.ResponseWriter, r *http.Request) {
	const prefix = "/admin/personalpay/devices/"
	const deleteSuffix = "/delete"
	target := "/admin/settings#settings-payment-personalpay"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	actionPath := strings.TrimPrefix(r.URL.Path, prefix)
	if !strings.HasSuffix(actionPath, deleteSuffix) {
		http.NotFound(w, r)
		return
	}
	encodedID := strings.TrimSuffix(actionPath, deleteSuffix)
	deviceID, err := url.PathUnescape(strings.Trim(encodedID, "/"))
	if err != nil || strings.TrimSpace(deviceID) == "" {
		s.redirectWithNoticeError(w, r, target, errors.New("personalpay device id is required"))
		return
	}
	deviceID = strings.TrimSpace(deviceID)
	if err := s.control.DeletePersonalPayDevice(r.Context(), deviceID); err != nil {
		s.recordAdminAudit(r, "error", "personalpay.device.delete", "personalpay_device", deviceID, deviceID, err.Error())
		s.redirectWithNoticeError(w, r, target, err)
		return
	}
	s.recordAdminAudit(r, "ok", "personalpay.device.delete", "personalpay_device", deviceID, deviceID, "")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) handlePersonalPayAccountActions(w http.ResponseWriter, r *http.Request) {
	const prefix = "/admin/personalpay/accounts/"
	const releaseSuffix = "/release"
	target := "/admin/settings#settings-payment-personalpay"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	actionPath := strings.TrimPrefix(r.URL.Path, prefix)
	if !strings.HasSuffix(actionPath, releaseSuffix) {
		http.NotFound(w, r)
		return
	}
	encodedID := strings.TrimSuffix(actionPath, releaseSuffix)
	accountID, err := url.PathUnescape(strings.Trim(encodedID, "/"))
	if err != nil || strings.TrimSpace(accountID) == "" {
		s.redirectWithNoticeError(w, r, target, errors.New("personalpay account id is required"))
		return
	}
	accountID = strings.TrimSpace(accountID)
	order, err := s.control.ReleasePersonalPayAccountOrder(r.Context(), accountID)
	if err != nil {
		s.recordAdminAudit(r, "error", "personalpay.account.release", "personalpay_account", accountID, accountID, err.Error())
		s.redirectWithNoticeError(w, r, target, err)
		return
	}
	s.recordAdminAudit(r, "ok", "personalpay.account.release", "personalpay_account", accountID, accountID, "order="+order.OutTradeNo)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) handleEmailTest(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/email-test" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := parseEmailTestForm(w, r); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": err.Error()})
		return
	}
	existing, loadErr := s.control.GetSystemSettings()
	input := systemSettingsFromForm(r, existing)
	if loadErr == nil {
		preserveBlankSystemSecrets(&input, existing)
	}
	toEmail := firstNonBlankFormValue(r, "email_test_to", "test_email_to", "email")
	if strings.TrimSpace(toEmail) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": translate(resolveLocale(w, r), "email_test_to_required")})
		return
	}
	if err := s.control.TestEmailVerificationSettings(r.Context(), input.Email, toEmail); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "message": translate(resolveLocale(w, r), "email_test_success")})
}

func firstNonBlankFormValue(r *http.Request, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(r.FormValue(name)); value != "" {
			return value
		}
	}
	return ""
}

func parseEmailTestForm(w http.ResponseWriter, r *http.Request) error {
	contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if strings.HasPrefix(contentType, "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, consoleURLEncodedFormBodyLimit)
		return r.ParseMultipartForm(1 << 20)
	}
	r.Body = http.MaxBytesReader(w, r.Body, consoleURLEncodedFormBodyLimit)
	return r.ParseForm()
}

func preserveBlankLoginOAuthSecrets(input *core.SystemSettings, existing core.SystemSettings) {
	preserveBlankSystemSecrets(input, existing)
}

func preserveBlankSystemSecrets(input *core.SystemSettings, existing core.SystemSettings) {
	if input == nil {
		return
	}
	if strings.TrimSpace(input.OAuth.GitHubLoginSecret) == "" {
		input.OAuth.GitHubLoginSecret = existing.OAuth.GitHubLoginSecret
	}
	if strings.TrimSpace(input.OAuth.GoogleLoginSecret) == "" {
		input.OAuth.GoogleLoginSecret = existing.OAuth.GoogleLoginSecret
	}
	if strings.TrimSpace(input.Email.SMTPPassword) == "" {
		input.Email.SMTPPassword = existing.Email.SMTPPassword
	}
	if strings.TrimSpace(input.Email.CloudMailPassword) == "" {
		input.Email.CloudMailPassword = existing.Email.CloudMailPassword
	}
	if strings.TrimSpace(input.Registration.TurnstileSecretKey) == "" {
		input.Registration.TurnstileSecretKey = existing.Registration.TurnstileSecretKey
	}
	if strings.TrimSpace(input.Payment.WeChatPay.APIV3Key) == "" {
		input.Payment.WeChatPay.APIV3Key = existing.Payment.WeChatPay.APIV3Key
	}
	if strings.TrimSpace(input.Payment.WeChatPay.MerchantPrivateKeyPEM) == "" {
		input.Payment.WeChatPay.MerchantPrivateKeyPEM = existing.Payment.WeChatPay.MerchantPrivateKeyPEM
	}
	if strings.TrimSpace(input.Payment.Alipay.PrivateKeyPEM) == "" {
		input.Payment.Alipay.PrivateKeyPEM = existing.Payment.Alipay.PrivateKeyPEM
	}
	if strings.TrimSpace(input.Payment.PersonalPay.AndroidToken) == "" {
		input.Payment.PersonalPay.AndroidToken = existing.Payment.PersonalPay.AndroidToken
	}
	switch input.Email.Provider {
	case core.EmailProviderCloudMail:
		input.Email.SMTPHost = existing.Email.SMTPHost
		input.Email.SMTPPort = existing.Email.SMTPPort
		input.Email.SMTPUsername = existing.Email.SMTPUsername
		input.Email.SMTPPassword = existing.Email.SMTPPassword
		input.Email.FromEmail = existing.Email.FromEmail
	case core.EmailProviderSMTP, "":
		input.Email.CloudMailBaseURL = existing.Email.CloudMailBaseURL
		input.Email.CloudMailEmail = existing.Email.CloudMailEmail
		input.Email.CloudMailPassword = existing.Email.CloudMailPassword
		input.Email.CloudMailAccountID = existing.Email.CloudMailAccountID
	}
}

func (s *Server) renderSettingsPage(w http.ResponseWriter, r *http.Request, settings core.SystemSettings, saved bool, message string, status int) {
	locale := resolveLocale(w, r)
	normalized := core.NormalizeSystemSettings(settings)
	data := withCSRFData(map[string]any{
		"TitleKey":                     "page_title_settings",
		"ActiveNav":                    "settings",
		"Locale":                       locale,
		"Settings":                     normalized,
		"PersonalPay":                  s.control.PersonalPayRuntime(r.Context()),
		"Saved":                        saved,
		"Error":                        strings.TrimSpace(message),
		"BackupOptions":                backupDataSetOptions(),
		"ShowPersonalPayAndroidBackup": showPersonalPayAndroidBackup(normalized),
		"Restored":                     r.URL.Query().Get("restored") == "1",
		"Backup":                       strings.TrimSpace(r.URL.Query().Get("backup")),
	}, r)
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.render(w, "settings.html", locale, data)
}

func showPersonalPayAndroidBackup(settings core.SystemSettings) bool {
	settings = core.NormalizeSystemSettings(settings)
	return settings.Payment.PersonalPay.Enabled && strings.TrimSpace(settings.Payment.PersonalPay.AndroidToken) != ""
}
