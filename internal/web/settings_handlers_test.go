package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/payments"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
	personalpay "personalpay/sdk-go"
)

func TestSystemSettingsFromFormParsesUserConcurrentRequestLimit(t *testing.T) {
	form := url.Values{}
	form.Set("user_concurrent_request_limit", "3")
	form.Set("plan_concurrent_request_limit", "2")
	form.Set("user_request_rate_limit_per_minute", "60")
	form.Set("responses_websocket_upstream_present", "1")
	form.Set("responses_websocket_upstream_enabled", "on")
	form.Set("registration_username_min_length", "5")
	form.Set("usage_log_max_age_days", "5")
	form.Set("billing_ledger_retention_days", "14")
	form.Set("image_user_console_enabled", "on")
	form.Set("email_template_subject", "注册验证码")
	form.Set("email_template_text", "验证码：{{code}}")
	form.Set("email_template_html", "<p>验证码：<b>{{code}}</b></p>")

	req := httptest.NewRequest(http.MethodPost, "/admin/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	settings := systemSettingsFromForm(req, core.DefaultSystemSettings())
	if settings.Runtime.UserConcurrentRequestLimit != 3 {
		t.Fatalf("UserConcurrentRequestLimit = %d, want 3", settings.Runtime.UserConcurrentRequestLimit)
	}
	if settings.Runtime.PlanConcurrentRequestLimit != 2 {
		t.Fatalf("PlanConcurrentRequestLimit = %d, want 2", settings.Runtime.PlanConcurrentRequestLimit)
	}
	if settings.Runtime.UserRequestRateLimitPerMinute != 60 {
		t.Fatalf("UserRequestRateLimitPerMinute = %d, want 60", settings.Runtime.UserRequestRateLimitPerMinute)
	}
	if !core.ResponsesWebSocketUpstreamEnabled(settings.Runtime) {
		t.Fatal("ResponsesWebSocketUpstreamEnabled = false, want true")
	}
	if settings.Registration.UsernameMinLength != 5 {
		t.Fatalf("UsernameMinLength = %d, want 5", settings.Registration.UsernameMinLength)
	}
	if settings.Retention.UsageLogMaxAgeDays != 5 {
		t.Fatalf("usage log retention = %#v", settings.Retention)
	}
	if settings.Retention.BillingLedgerRetentionDays != 14 {
		t.Fatalf("BillingLedgerRetentionDays = %d, want 14", settings.Retention.BillingLedgerRetentionDays)
	}
	if !core.ImageUserConsoleEnabled(settings.Image) {
		t.Fatalf("ImageUserConsoleEnabled = false, want true")
	}
	if settings.Email.VerificationSubjectTemplate != "注册验证码" || settings.Email.VerificationTextTemplate != "验证码：{{code}}" || settings.Email.VerificationHTMLTemplate != "<p>验证码：<b>{{code}}</b></p>" {
		t.Fatalf("email templates = %#v", settings.Email)
	}
}

func TestSystemSettingsFromFormParsesInvitationRechargeRewardPercent(t *testing.T) {
	form := url.Values{}
	form.Set("inviter_recharge_reward_percent", "5.25")

	req := httptest.NewRequest(http.MethodPost, "/admin/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	settings := systemSettingsFromForm(req, core.DefaultSystemSettings())
	if settings.Invitation.InviterRechargeRewardBps != 525 {
		t.Fatalf("InviterRechargeRewardBps = %d, want 525", settings.Invitation.InviterRechargeRewardBps)
	}

	form.Set("inviter_recharge_reward_percent", "100.01")
	req = httptest.NewRequest(http.MethodPost, "/admin/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	settings = systemSettingsFromForm(req, core.DefaultSystemSettings())
	if settings.Invitation.InviterRechargeRewardBps != 10000 {
		t.Fatalf("clamped InviterRechargeRewardBps = %d, want 10000", settings.Invitation.InviterRechargeRewardBps)
	}
}

func TestSystemSettingsFromFormParsesResponsesWebSocketUpstreamToggle(t *testing.T) {
	existing := core.DefaultSystemSettings()
	form := url.Values{}
	form.Set("responses_websocket_upstream_present", "1")

	req := httptest.NewRequest(http.MethodPost, "/admin/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	settings := systemSettingsFromForm(req, existing)
	if core.ResponsesWebSocketUpstreamEnabled(settings.Runtime) {
		t.Fatal("ResponsesWebSocketUpstreamEnabled = true, want false")
	}
	if settings.Runtime.ResponsesWebSocketUpstreamEnabled == nil || *settings.Runtime.ResponsesWebSocketUpstreamEnabled {
		t.Fatalf("raw ResponsesWebSocketUpstreamEnabled = %#v, want false pointer", settings.Runtime.ResponsesWebSocketUpstreamEnabled)
	}

	disabled := false
	existing.Runtime.ResponsesWebSocketUpstreamEnabled = &disabled
	req = httptest.NewRequest(http.MethodPost, "/admin/settings", strings.NewReader(url.Values{}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	settings = systemSettingsFromForm(req, existing)
	if core.ResponsesWebSocketUpstreamEnabled(settings.Runtime) {
		t.Fatal("partial form should preserve disabled websocket upstream setting")
	}
}

func TestSystemSettingsFromFormAllowsDisablingUsageRetention(t *testing.T) {
	form := url.Values{}
	form.Set("audit_limit", "512")
	form.Set("usage_log_max_age_days", "0")
	form.Set("billing_ledger_retention_days", "1")

	req := httptest.NewRequest(http.MethodPost, "/admin/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	settings := systemSettingsFromForm(req, core.DefaultSystemSettings())
	if settings.Retention.UsageLogMaxAgeDays != 0 {
		t.Fatalf("usage log retention = %#v, want disabled", settings.Retention)
	}
	if settings.Retention.BillingLedgerRetentionDays != core.MinimumBillingLedgerRetentionDays {
		t.Fatalf("BillingLedgerRetentionDays = %d, want %d", settings.Retention.BillingLedgerRetentionDays, core.MinimumBillingLedgerRetentionDays)
	}
}

func TestSystemSettingsFromFormParsesPaymentRechargeInputMode(t *testing.T) {
	form := url.Values{}
	form.Set("payment_recharge_input_mode", "payment_cny")
	form.Set("payment_cny_per_usd", "0.1")
	form.Set("payment_min_recharge_usd", "2.50")
	form.Set("payment_max_recharge_usd", "200")

	req := httptest.NewRequest(http.MethodPost, "/admin/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	settings := systemSettingsFromForm(req, core.DefaultSystemSettings())
	if settings.Payment.RechargeInputMode != core.RechargeInputModePaymentCNY {
		t.Fatalf("RechargeInputMode = %q, want %q", settings.Payment.RechargeInputMode, core.RechargeInputModePaymentCNY)
	}
	if settings.Payment.CNYPerUSD != "0.1" {
		t.Fatalf("CNYPerUSD = %q, want 0.1", settings.Payment.CNYPerUSD)
	}
	if settings.Payment.MinRechargeNanoUSD != 2500000000 || settings.Payment.MaxRechargeNanoUSD != 200*core.NanoUSDPerUSD {
		t.Fatalf("payment recharge limits = %#v", settings.Payment)
	}
}

func TestValidatePaymentRechargeLimitFormRejectsInvalidValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		form url.Values
		want string
	}{
		{
			name: "invalid minimum",
			form: url.Values{"payment_min_recharge_usd": {"abc"}},
			want: "minimum recharge",
		},
		{
			name: "negative maximum",
			form: url.Values{"payment_max_recharge_usd": {"-1"}},
			want: "maximum recharge",
		},
		{
			name: "maximum below minimum",
			form: url.Values{"payment_min_recharge_usd": {"10"}, "payment_max_recharge_usd": {"5"}},
			want: "maximum recharge must be greater than or equal to minimum recharge",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/admin/settings", strings.NewReader(tc.form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if err := req.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if err := validatePaymentRechargeLimitForm(req); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validatePaymentRechargeLimitForm err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSystemSettingsDefaultPaymentRechargeInputModeIsCNY(t *testing.T) {
	settings := core.NormalizeSystemSettings(core.DefaultSystemSettings())
	if settings.Payment.RechargeInputMode != core.RechargeInputModePaymentCNY {
		t.Fatalf("RechargeInputMode = %q, want %q", settings.Payment.RechargeInputMode, core.RechargeInputModePaymentCNY)
	}

	settings.Payment.RechargeInputMode = ""
	settings = core.NormalizeSystemSettings(settings)
	if settings.Payment.RechargeInputMode != core.RechargeInputModePaymentCNY {
		t.Fatalf("empty RechargeInputMode = %q, want %q", settings.Payment.RechargeInputMode, core.RechargeInputModePaymentCNY)
	}
}

func TestSystemSettingsFromFormParsesAndroidBackupSettings(t *testing.T) {
	form := url.Values{}
	form.Set("personalpay_enabled", "on")
	form.Set("backup_android_auto_enabled", "on")
	form.Set("backup_android_time", "02:30")
	form.Add("backup_android_data", "billing")
	form.Add("backup_android_data", "clients")
	form.Add("backup_android_data", "unknown")

	req := httptest.NewRequest(http.MethodPost, "/admin/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	settings := systemSettingsFromForm(req, core.DefaultSystemSettings())
	if !settings.Backup.AndroidAutoEnabled {
		t.Fatal("AndroidAutoEnabled = false, want true")
	}
	if settings.Backup.AndroidTimeOfDay != "02:30" {
		t.Fatalf("AndroidTimeOfDay = %q, want 02:30", settings.Backup.AndroidTimeOfDay)
	}
	if len(settings.Backup.AndroidDataSets) != 2 || settings.Backup.AndroidDataSets[0] != "clients" || settings.Backup.AndroidDataSets[1] != "billing" {
		t.Fatalf("AndroidDataSets = %#v, want clients,billing in backup option order", settings.Backup.AndroidDataSets)
	}
}

func TestSystemSettingsFromFormIgnoresAndroidBackupWithoutPersonalPay(t *testing.T) {
	form := url.Values{}
	form.Set("backup_android_auto_enabled", "on")
	form.Set("backup_android_time", "02:30")
	form.Add("backup_android_data", "billing")

	req := httptest.NewRequest(http.MethodPost, "/admin/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	settings := systemSettingsFromForm(req, core.DefaultSystemSettings())
	if settings.Backup.AndroidAutoEnabled {
		t.Fatal("AndroidAutoEnabled = true without PersonalPay, want false")
	}
}

func TestSettingsPageRendersUserConcurrentRequestLimitField(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/settings", nil)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")

	server.renderSettingsPage(rec, req, core.DefaultSystemSettings(), false, "", http.StatusOK)

	body := rec.Body.String()
	for _, want := range []string{`name="user_concurrent_request_limit"`, `name="plan_concurrent_request_limit"`, `name="user_request_rate_limit_per_minute"`, `name="registration_username_min_length"`, `name="usage_log_max_age_days"`, `name="billing_ledger_retention_days"`, `min="3" max="365" name="billing_ledger_retention_days"`, `data-group-settings-open="email-template-editor"`, `name="email_template_subject"`, `name="payment_recharge_input_mode"`, "验证邮件模版", "用户名最小长度", "用户并发请求上限", "套餐并发请求上限", "用户请求速率上限（次/分钟）", "使用日志保留天数", "财务流水保留天数", "充值输入口径", "按人民币支付金额输入", `href="#settings-image"`, "图片设置", "image_user_console_enabled", "向普通用户显示生图工作台", "image_backend", "保存后立即生效的本地上限。用户或套餐并发请求设为 0 表示不限制；请求速率设为 0 表示不做排队延迟。财务流水最少保留 3 天。"} {
		if !strings.Contains(body, want) {
			t.Fatalf("settings page missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, `name="usage_log_max_rows"`) || strings.Contains(body, "使用日志保留数量") {
		t.Fatalf("settings page should not render usage log row limit: %s", body)
	}
	for _, hidden := range []string{`name="backup_android_auto_enabled"`, `name="backup_android_time"`, `name="backup_android_data" value="billing"`, "自动备份到 Android"} {
		if strings.Contains(body, hidden) {
			t.Fatalf("settings page should hide PersonalPay Android backup without PersonalPay integration, found %q: %s", hidden, body)
		}
	}
	usersIndex := strings.Index(body, `href="#settings-users"`)
	imageIndex := strings.Index(body, `href="#settings-image"`)
	if usersIndex == -1 || imageIndex == -1 || imageIndex < usersIndex {
		t.Fatalf("image settings tab should appear after user settings tab: users=%d image=%d body=%s", usersIndex, imageIndex, body)
	}
	paymentCNYIndex := strings.Index(body, `value="payment_cny" selected`)
	balanceUSDIndex := strings.Index(body, `value="balance_usd"`)
	if paymentCNYIndex == -1 || balanceUSDIndex == -1 || paymentCNYIndex > balanceUSDIndex {
		t.Fatalf("payment CNY option should be selected and appear before $ balance option: cny=%d balance=%d body=%s", paymentCNYIndex, balanceUSDIndex, body)
	}
}

func TestSettingsPageRendersAndroidBackupWhenPersonalPayIntegrated(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")

	settings := core.DefaultSystemSettings()
	settings.Payment.PersonalPay.Enabled = true
	settings.Payment.PersonalPay.AndroidToken = "android-token"
	settings.Backup.AndroidAutoEnabled = true
	settings.Backup.AndroidTimeOfDay = "02:30"
	settings.Backup.AndroidDataSets = []string{"clients", "billing"}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/settings", nil)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")

	server.renderSettingsPage(rec, req, settings, false, "", http.StatusOK)

	body := rec.Body.String()
	for _, want := range []string{`name="backup_android_auto_enabled" checked`, `name="backup_android_time" value="02:30"`, `name="backup_android_data" value="billing" checked`, "自动备份到 Android"} {
		if !strings.Contains(body, want) {
			t.Fatalf("settings page missing PersonalPay backup field %q: %s", want, body)
		}
	}
}

func TestSettingsPageRendersPersonalPayRuntime(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://gateway.example.com/admin/settings", nil)
	req.Host = "gateway.example.com"
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")

	server.renderSettingsPage(rec, req, core.DefaultSystemSettings(), false, "", http.StatusOK)

	body := rec.Body.String()
	for _, want := range []string{
		"Android 设备",
		"在线账号",
		"还没有 Android 支付设备连接",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("settings page missing %q: %s", want, body)
		}
	}
}

func TestSettingsPageRendersPersonalPayReleaseOrderAction(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	control.SetPaymentClient(&fakeSettingsPaymentClient{
		runtime: payments.PersonalPayRuntime{
			Devices: []personalpay.Device{{
				ID:       "device-1",
				Label:    "Phone",
				Online:   true,
				LastSeen: time.Now(),
				Accounts: []personalpay.Account{{
					ID:          "device-1:wechat:account-1",
					Channel:     personalpay.ChannelWeChat,
					DisplayName: "微信账号",
					Status:      personalpay.AccountOccupied,
					OccupiedBy:  "pay_occupied",
				}},
			}},
		},
	})
	server := NewServer(control, nil, "data/state.db")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://gateway.example.com/admin/settings", nil)
	req.Host = "gateway.example.com"
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")

	server.renderSettingsPage(rec, req, core.DefaultSystemSettings(), false, "", http.StatusOK)

	body := rec.Body.String()
	for _, want := range []string{
		`/admin/personalpay/accounts/device-1:wechat:account-1/release`,
		"释放订单",
		"pay_occupied",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("settings page missing %q: %s", want, body)
		}
	}
}

type fakeSettingsPaymentClient struct {
	runtime payments.PersonalPayRuntime
}

func (f *fakeSettingsPaymentClient) CreateOrder(context.Context, payments.CreateOrderInput) (payments.CreateOrderResult, error) {
	return payments.CreateOrderResult{}, nil
}

func (f *fakeSettingsPaymentClient) QueryOrder(context.Context, payments.QueryOrderInput) (payments.QueryOrderResult, error) {
	return payments.QueryOrderResult{}, nil
}

func (f *fakeSettingsPaymentClient) CancelOrder(context.Context, payments.CancelOrderInput) (payments.CancelOrderResult, error) {
	return payments.CancelOrderResult{}, nil
}

func (f *fakeSettingsPaymentClient) PersonalPayRuntime(context.Context) payments.PersonalPayRuntime {
	return f.runtime
}

func (f *fakeSettingsPaymentClient) DeletePersonalPayDevice(context.Context, string) error {
	return nil
}

func TestUserInputFromFormParsesConcurrentRequestLimitOverride(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    *int
		wantErr bool
	}{
		{name: "inherit", value: ""},
		{name: "unlimited", value: "0", want: intPtr(0)},
		{name: "positive", value: "12", want: intPtr(12)},
		{name: "negative", value: "-1", wantErr: true},
		{name: "invalid", value: "abc", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form := url.Values{}
			form.Set("username", "alice")
			form.Set("role", string(core.UserRoleUser))
			form.Set("enabled", "on")
			form.Set("concurrent_request_limit", tt.value)
			req := httptest.NewRequest(http.MethodPost, "/admin/users/user_1/edit", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			input, err := userInputFromForm(req)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("userInputFromForm returned error: %v", err)
			}
			switch {
			case tt.want == nil && input.ConcurrentRequestLimitOverride != nil:
				t.Fatalf("ConcurrentRequestLimitOverride = %#v, want nil", input.ConcurrentRequestLimitOverride)
			case tt.want != nil && input.ConcurrentRequestLimitOverride == nil:
				t.Fatalf("ConcurrentRequestLimitOverride = nil, want %d", *tt.want)
			case tt.want != nil && *input.ConcurrentRequestLimitOverride != *tt.want:
				t.Fatalf("ConcurrentRequestLimitOverride = %d, want %d", *input.ConcurrentRequestLimitOverride, *tt.want)
			}
		})
	}
}

func TestUserInputFromFormParsesRequestRateLimitOverride(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    *int
		wantErr bool
	}{
		{name: "inherit", value: ""},
		{name: "unlimited", value: "0", want: intPtr(0)},
		{name: "positive", value: "60", want: intPtr(60)},
		{name: "negative", value: "-1", wantErr: true},
		{name: "invalid", value: "abc", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form := url.Values{}
			form.Set("username", "alice")
			form.Set("role", string(core.UserRoleUser))
			form.Set("enabled", "on")
			form.Set("request_rate_limit_per_minute", tt.value)
			req := httptest.NewRequest(http.MethodPost, "/admin/users/user_1/edit", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			input, err := userInputFromForm(req)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("userInputFromForm returned error: %v", err)
			}
			switch {
			case tt.want == nil && input.RequestRateLimitPerMinuteOverride != nil:
				t.Fatalf("RequestRateLimitPerMinuteOverride = %#v, want nil", input.RequestRateLimitPerMinuteOverride)
			case tt.want != nil && input.RequestRateLimitPerMinuteOverride == nil:
				t.Fatalf("RequestRateLimitPerMinuteOverride = nil, want %d", *tt.want)
			case tt.want != nil && *input.RequestRateLimitPerMinuteOverride != *tt.want:
				t.Fatalf("RequestRateLimitPerMinuteOverride = %d, want %d", *input.RequestRateLimitPerMinuteOverride, *tt.want)
			}
		})
	}
}

func TestUsersPageRendersConcurrentRequestLimitOverrideControls(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	unlimited := 0
	user, err := control.CreateUser(controlplane.UserInput{
		Username:                          "priority",
		Password:                          "priority-secret",
		Role:                              core.UserRoleUser,
		Enabled:                           true,
		ConcurrentRequestLimitOverride:    &unlimited,
		RequestRateLimitPerMinuteOverride: &unlimited,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/users?partial=users-panel", nil)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`name="concurrent_request_limit"`, `name="request_rate_limit_per_minute"`, `value="0"`, "单用户并发请求上限", "单用户请求速率上限（次/分钟）", "留空继承系统默认"} {
		if !strings.Contains(body, want) {
			t.Fatalf("users page missing %q: %s", want, body)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/users/"+user.ID+"/details", nil)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("details status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, ">不限<") {
		t.Fatalf("user details missing unlimited override text: %s", body)
	}
}

func intPtr(value int) *int {
	return &value
}
