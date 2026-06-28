package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
)

func (s *Server) handleAccountsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/accounts" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	state := s.control.AccountDashboard(r.Context())
	activeAccountGroupKey, activeAccountGroup := selectActiveAccountGroup(state.AccountGroups, r.URL.Query().Get("group"))
	activeAccountFilter := normalizeAccountFilterValue(r.URL.Query().Get("filter"))
	openAIStart, openAIConnectAvailable := s.connectStartIfAvailable(core.ProviderOpenAI, activeAccountGroupKey)
	claudeStart, claudeConnectAvailable := s.connectStartIfAvailable(core.ProviderClaude, activeAccountGroupKey)
	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":               "page_title_accounts",
		"ActiveNav":              "accounts",
		"Locale":                 locale,
		"State":                  state,
		"ActiveAccountGroup":     activeAccountGroup,
		"ActiveAccountGroupKey":  activeAccountGroupKey,
		"ActiveAccountFilter":    activeAccountFilter,
		"AccountCreated":         r.URL.Query().Get("created") == "1",
		"AccountTestTone":        strings.TrimSpace(r.URL.Query().Get("test_tone")),
		"AccountTestMessage":     strings.TrimSpace(r.URL.Query().Get("test_message")),
		"AccountBatchTone":       strings.TrimSpace(r.URL.Query().Get("batch_tone")),
		"AccountBatchMessage":    strings.TrimSpace(r.URL.Query().Get("batch_message")),
		"OpenAIConnectStart":     openAIStart,
		"ClaudeConnectStart":     claudeStart,
		"OpenAIConnectAvailable": openAIConnectAvailable,
		"ClaudeConnectAvailable": claudeConnectAvailable,
	}, r)
	s.render(w, "accounts.html", locale, data)
}

func (s *Server) connectStartIfAvailable(provider core.ProviderKind, group string) (controlplane.ConnectStart, bool) {
	if s == nil || s.control == nil {
		return controlplane.ConnectStart{}, false
	}
	start, err := s.control.StartConnect(provider, "", group)
	return start, err == nil
}

func (s *Server) handleAccountGroupsCreate(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/account-groups" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	group, err := s.control.CreateAccountGroup(r.FormValue("name"), r.FormValue("type"), r.FormValue("remark"))
	if err != nil {
		s.recordAdminAudit(r, "error", "account_group.create", "account_group", "", strings.TrimSpace(r.FormValue("name")), err.Error())
		s.redirectWithNoticeError(w, r, "/admin/accounts", err)
		return
	}
	s.recordAdminAudit(r, "ok", "account_group.create", "account_group", group.ID, group.Name, "")
	s.publishAccountPoolChanged()
	http.Redirect(w, r, "/admin/accounts?group="+url.QueryEscape(accountGroupQueryKey(group.Name)), http.StatusSeeOther)
}

func (s *Server) handleAccountRuntimeReconcile(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/accounts/reconcile" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	report, err := s.control.ReconcileAccountRuntimeState(r.Context(), time.Now().UTC())
	if err != nil {
		s.recordAdminAudit(r, "error", "account.runtime.reconcile", "account", "", "", err.Error())
		s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
		return
	}
	message := fmt.Sprintf("scanned=%d updated=%d reactivated=%d quota_cleared=%d", report.Scanned, report.Updated, report.Reactivated, report.QuotaCooldownCleared)
	tone := "good"
	if report.Updated == 0 && report.QuotaCooldownCleared == 0 {
		message = "account runtime already up to date"
	}
	s.recordAdminAudit(r, "ok", "account.runtime.reconcile", "account", "", "", message)
	s.publishAccountPoolChanged()
	http.Redirect(w, r, accountBatchRedirect(accountGroupReturnKey(r), accountFilterReturnValue(r), tone, message), http.StatusSeeOther)
}

func (s *Server) handleProxyTest(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/proxy-test" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	result, err := s.control.TestProxy(r.Context(), r.FormValue("proxy_url"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":      false,
			"message": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"message":     "proxy test passed",
		"target_url":  result.TargetURL,
		"status_code": result.StatusCode,
		"duration_ms": result.Duration.Milliseconds(),
	})
}

func (s *Server) handleAccountGroupActions(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/admin/account-groups/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	switch parts[1] {
	case "delete":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		group, err := s.control.DeleteAccountGroup(parts[0])
		if err != nil {
			s.recordAdminAudit(r, "error", "account_group.delete", "account_group", parts[0], "", err.Error())
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		s.recordAdminAudit(r, "ok", "account_group.delete", "account_group", group.ID, group.Name, "")
		s.publishAccountPoolChanged()
		http.Redirect(w, r, accountsGroupFilterHref("", accountFilterReturnValue(r)), http.StatusSeeOther)
	case "proxy":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		before, hasBefore := s.auditAccountGroupConfig(parts[0])
		group, err := s.control.UpdateAccountGroupProxy(parts[0], r.FormValue("proxy_url"))
		if err != nil {
			s.recordAdminAudit(r, "error", "account_group.update_proxy", "account_group", parts[0], "", err.Error())
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		message := auditFieldsMessage(auditMessageField{Key: "proxy_url", Value: maskProxyURL(group.ProxyURL)})
		if hasBefore {
			message = auditChangeMessage(auditTextChange("proxy_url", maskProxyURL(before.ProxyURL), maskProxyURL(group.ProxyURL)))
		}
		s.recordAdminAudit(r, "ok", "account_group.update_proxy", "account_group", group.ID, group.Name, message)
		s.publishAccountPoolChanged()
		http.Redirect(w, r, accountsGroupFilterHref(accountGroupQueryKey(group.Name), accountFilterReturnValue(r)), http.StatusSeeOther)
	case "name":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		before, hasBefore := s.auditAccountGroupConfig(parts[0])
		group, err := s.control.UpdateAccountGroupName(parts[0], r.FormValue("name"))
		if err != nil {
			s.recordAdminAudit(r, "error", "account_group.update_name", "account_group", parts[0], "", err.Error())
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		message := auditFieldsMessage(auditMessageField{Key: "name", Value: group.Name})
		if hasBefore {
			message = auditChangeMessage(auditTextChange("name", before.Name, group.Name))
		}
		s.recordAdminAudit(r, "ok", "account_group.update_name", "account_group", group.ID, group.Name, message)
		s.publishAccountPoolChanged()
		http.Redirect(w, r, accountsGroupFilterHref(accountGroupQueryKey(group.Name), accountFilterReturnValue(r)), http.StatusSeeOther)
	case "profile":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		before, hasBefore := s.auditAccountGroupConfig(parts[0])
		group, err := s.control.UpdateAccountGroupProfile(parts[0], r.FormValue("name"), r.FormValue("type"), r.FormValue("remark"))
		if err != nil {
			s.recordAdminAudit(r, "error", "account_group.update_profile", "account_group", parts[0], "", err.Error())
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		message := auditFieldsMessage(
			auditMessageField{Key: "name", Value: group.Name},
			auditMessageField{Key: "type", Value: group.Type},
			auditMessageField{Key: "remark", Value: group.Remark},
		)
		if hasBefore {
			message = auditChangeMessage(
				auditTextChange("name", before.Name, group.Name),
				auditTextChange("type", before.Type, group.Type),
				auditTextChange("remark", before.Remark, group.Remark),
			)
		}
		s.recordAdminAudit(r, "ok", "account_group.update_profile", "account_group", group.ID, group.Name, message)
		s.publishAccountPoolChanged()
		http.Redirect(w, r, accountsGroupFilterHref(accountGroupQueryKey(group.Name), accountFilterReturnValue(r)), http.StatusSeeOther)
	case "type":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		before, hasBefore := s.auditAccountGroupConfig(parts[0])
		group, err := s.control.UpdateAccountGroupType(parts[0], r.FormValue("type"))
		if err != nil {
			s.recordAdminAudit(r, "error", "account_group.update_type", "account_group", parts[0], "", err.Error())
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		message := auditFieldsMessage(auditMessageField{Key: "type", Value: group.Type})
		if hasBefore {
			message = auditChangeMessage(auditTextChange("type", before.Type, group.Type))
		}
		s.recordAdminAudit(r, "ok", "account_group.update_type", "account_group", group.ID, group.Name, message)
		s.publishAccountPoolChanged()
		http.Redirect(w, r, accountsGroupFilterHref(accountGroupQueryKey(group.Name), accountFilterReturnValue(r)), http.StatusSeeOther)
	case "remark":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		before, hasBefore := s.auditAccountGroupConfig(parts[0])
		group, err := s.control.UpdateAccountGroupRemark(parts[0], r.FormValue("remark"))
		if err != nil {
			s.recordAdminAudit(r, "error", "account_group.update_remark", "account_group", parts[0], "", err.Error())
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		message := auditFieldsMessage(auditMessageField{Key: "remark", Value: group.Remark})
		if hasBefore {
			message = auditChangeMessage(auditTextChange("remark", before.Remark, group.Remark))
		}
		s.recordAdminAudit(r, "ok", "account_group.update_remark", "account_group", group.ID, group.Name, message)
		s.publishAccountPoolChanged()
		http.Redirect(w, r, accountsGroupFilterHref(accountGroupQueryKey(group.Name), accountFilterReturnValue(r)), http.StatusSeeOther)
	case "billing":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		multiplierBps, err := parseMultiplierFormValue(r, "billing_multiplier")
		if err != nil {
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		planMultiplierBps, err := parseMultiplierFormValue(r, "plan_billing_multiplier")
		if err != nil {
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		inputPrice, err := parseNanoUSDFormValue(r, "input_price_usd_per_1m")
		if err != nil {
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		cachedInputPrice, err := parseNanoUSDFormValue(r, "cached_input_price_usd_per_1m")
		if err != nil {
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		cacheWritePrice, err := parseNanoUSDFormValue(r, "cache_write_price_usd_per_1m")
		if err != nil {
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		cacheWrite5mPrice, err := parseNanoUSDFormValue(r, "cache_write_5m_price_usd_per_1m")
		if err != nil {
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		cacheWrite1hPrice, err := parseNanoUSDFormValue(r, "cache_write_1h_price_usd_per_1m")
		if err != nil {
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		outputPrice, err := parseNanoUSDFormValue(r, "output_price_usd_per_1m")
		if err != nil {
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		imageOutputPrice, err := parseNanoUSDFormValue(r, "image_output_price_usd_per_1m")
		if err != nil {
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		planBillingEnabled := parseBoolFormValue(r.FormValue("plan_billing_enabled"))
		timedMultipliers, err := parseTimedMultiplierFormValues(r)
		if err != nil {
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		before, hasBefore := s.auditAccountGroupConfig(parts[0])
		group, err := s.control.UpdateAccountGroupBillingFull(parts[0], multiplierBps, planMultiplierBps, inputPrice, cachedInputPrice, cacheWritePrice, cacheWrite5mPrice, cacheWrite1hPrice, outputPrice, imageOutputPrice, timedMultipliers, planBillingEnabled)
		if err != nil {
			s.recordAdminAudit(r, "error", "account_group.update_billing", "account_group", parts[0], "", err.Error())
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		s.recordAdminAudit(r, "ok", "account_group.update_billing", "account_group", group.ID, group.Name, auditAccountGroupBillingChangeMessage(before, group, hasBefore))
		s.publishAccountPoolChanged()
		http.Redirect(w, r, accountsGroupFilterHref(accountGroupQueryKey(group.Name), accountFilterReturnValue(r)), http.StatusSeeOther)
	case "visibility":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		show := parseBoolFormValue(r.FormValue("show_in_client_editor"))
		visibleUserIDs := r.Form["visible_user_id"]
		before, hasBefore := s.auditAccountGroupConfig(parts[0])
		group, err := s.control.UpdateAccountGroupVisibilitySettings(parts[0], show, visibleUserIDs)
		if err != nil {
			s.recordAdminAudit(r, "error", "account_group.update_visibility", "account_group", parts[0], "", err.Error())
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		message := auditFieldsMessage(auditMessageField{Key: "show_in_client_editor", Value: fmt.Sprintf("%t", show)})
		if hasBefore {
			message = auditChangeMessage(
				auditBoolChange("show_in_client_editor", auditAccountGroupShowValue(before), auditAccountGroupShowValue(group)),
				auditStringSliceChange("visible_users", before.VisibleUserIDs, group.VisibleUserIDs),
			)
		}
		s.recordAdminAudit(r, "ok", "account_group.update_visibility", "account_group", group.ID, group.Name, message)
		s.publishAccountPoolChanged()
		http.Redirect(w, r, accountsGroupFilterHref(accountGroupQueryKey(group.Name), accountFilterReturnValue(r)), http.StatusSeeOther)
	default:
		http.NotFound(w, r)
	}
}

func parseBoolFormValue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "on", "yes", "y":
		return true
	default:
		return false
	}
}

func parseTimedMultiplierFormValues(r *http.Request) ([]core.AccountGroupTimedMultiplier, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	enabled := formValueSet(r.Form["timed_multiplier_enabled"])
	deleted := formValueSet(r.Form["timed_multiplier_delete"])
	rules := make([]core.AccountGroupTimedMultiplier, 0, len(r.Form["timed_multiplier_id"])+1)
	for _, id := range r.Form["timed_multiplier_id"] {
		id = strings.TrimSpace(id)
		if id == "" || deleted[id] {
			continue
		}
		rule, err := parseTimedMultiplierRule(r, "timed_multiplier", id)
		if err != nil {
			return nil, err
		}
		rule.ID = id
		rule.Enabled = enabled[id]
		rules = append(rules, rule)
	}
	if hasNewTimedMultiplierFormValue(r) {
		rule, err := parseTimedMultiplierRule(r, "new_timed_multiplier", "")
		if err != nil {
			return nil, err
		}
		rule.ID = fmt.Sprintf("tm_%d", time.Now().UTC().UnixNano())
		rule.Enabled = r.FormValue("new_timed_multiplier_enabled") != ""
		rules = append(rules, rule)
	}
	return rules, nil
}

func parseTimedMultiplierRule(r *http.Request, prefix, id string) (core.AccountGroupTimedMultiplier, error) {
	suffix := ""
	if id != "" {
		suffix = "_" + id
	}
	multiplier, err := core.ParseMultiplierDecimal(r.FormValue(prefix + "_value" + suffix))
	if err != nil {
		return core.AccountGroupTimedMultiplier{}, err
	}
	priority := 0
	if raw := strings.TrimSpace(r.FormValue(prefix + "_priority" + suffix)); raw != "" {
		priority, err = strconv.Atoi(raw)
		if err != nil {
			return core.AccountGroupTimedMultiplier{}, fmt.Errorf("timed multiplier priority must be a number")
		}
	}
	weekdays, err := parseWeekdayValues(r.Form[prefix+"_weekday"+suffix])
	if err != nil {
		return core.AccountGroupTimedMultiplier{}, err
	}
	return core.AccountGroupTimedMultiplier{
		Name:          strings.TrimSpace(r.FormValue(prefix + "_name" + suffix)),
		MultiplierBps: multiplier,
		Weekdays:      weekdays,
		StartDate:     strings.TrimSpace(r.FormValue(prefix + "_start_date" + suffix)),
		EndDate:       strings.TrimSpace(r.FormValue(prefix + "_end_date" + suffix)),
		StartTime:     strings.TrimSpace(r.FormValue(prefix + "_start_time" + suffix)),
		EndTime:       strings.TrimSpace(r.FormValue(prefix + "_end_time" + suffix)),
		Priority:      priority,
	}, nil
}

func hasNewTimedMultiplierFormValue(r *http.Request) bool {
	fields := []string{
		"new_timed_multiplier_name",
		"new_timed_multiplier_value",
		"new_timed_multiplier_start_date",
		"new_timed_multiplier_end_date",
		"new_timed_multiplier_start_time",
		"new_timed_multiplier_end_time",
		"new_timed_multiplier_priority",
	}
	for _, field := range fields {
		if strings.TrimSpace(r.FormValue(field)) != "" {
			return true
		}
	}
	return len(r.Form["new_timed_multiplier_weekday"]) > 0
}

func parseWeekdayValues(values []string) ([]int, error) {
	weekdays := make([]int, 0, len(values))
	for _, raw := range values {
		value, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("timed multiplier weekday must be a number")
		}
		weekdays = append(weekdays, value)
	}
	return weekdays, nil
}

func formValueSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[strings.TrimSpace(value)] = true
	}
	return out
}

func (s *Server) handleConnectStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	provider := core.ProviderKind(strings.TrimPrefix(r.URL.Path, "/admin/connect/"))
	start, err := s.control.StartConnect(provider, r.URL.Query().Get("oauth_import"))
	if err != nil {
		s.redirectWithNoticeError(w, r, "/admin/accounts", err)
		return
	}
	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":  "page_title_connect",
		"ActiveNav": "accounts",
		"Locale":    locale,
		"Start":     start,
		"Error":     strings.TrimSpace(r.URL.Query().Get("error")),
	}, r)
	s.render(w, "connect.html", locale, data)
}

func (s *Server) handleOpenAIOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/connect/openai/oauth" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	accountID := strings.TrimSpace(r.URL.Query().Get("account_id"))
	var start controlplane.OpenAIOAuthStart
	var err error
	if accountID != "" {
		start, err = s.control.StartOpenAIOAuthForAccount(r.Context(), accountID)
	} else {
		start, err = s.control.StartOpenAIOAuth(r.Context())
	}
	if err != nil {
		s.redirectWithNoticeError(w, r, oauthStartErrorTarget(core.ProviderOpenAI, accountID), err)
		return
	}
	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":       "page_title_openai_oauth",
		"ActiveNav":      "accounts",
		"Locale":         locale,
		"Start":          start,
		"OAuthAccountID": accountID,
		"OAuthReturnURL": oauthReturnURL(core.ProviderOpenAI, accountID),
	}, r)
	s.render(w, "connect_oauth.html", locale, data)
}

func (s *Server) handleOpenAICodexImportUpload(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/connect/openai/codex-import-upload" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	group, files, err := readCodexUploadRequest(w, r)
	if err != nil {
		s.recordAdminAudit(r, "error", "account.import_codex_upload", "account", "", "", err.Error())
		http.Redirect(w, r, "/admin/connect/openai?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	result, err := s.control.ImportCodexOpenAIAuthUploads(files, group)
	if err != nil {
		s.recordAdminAudit(r, "error", "account.import_codex_upload", "account", "", "", err.Error())
		http.Redirect(w, r, "/admin/connect/openai?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	message := fmt.Sprintf("uploaded=%d imported=%d skipped=%d failed=%d", len(files), result.Imported, result.Skipped, result.Failed)
	s.recordAdminAudit(r, "ok", "account.import_codex_upload", "account", "", "", message)
	if result.Imported > 0 {
		s.publishAccountPoolChanged()
	}
	if result.Imported == 0 && result.Failed > 0 {
		http.Redirect(w, r, "/admin/connect/openai?error="+url.QueryEscape(message), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/accounts?created=1&imported="+url.QueryEscape(fmt.Sprintf("%d", result.Imported)), http.StatusSeeOther)
}

const (
	codexUploadBodyLimit = 64 << 20
	codexUploadFileLimit = 4 << 20
)

func readCodexUploadRequest(w http.ResponseWriter, r *http.Request) (string, []controlplane.CodexOpenAIAuthUpload, error) {
	if r.MultipartForm != nil {
		return readCodexUploadForm(r.MultipartForm)
	}
	r.Body = http.MaxBytesReader(w, r.Body, codexUploadBodyLimit)
	reader, err := r.MultipartReader()
	if err != nil {
		return "", nil, err
	}
	return readCodexUploadParts(reader)
}

func readCodexUploadForm(form *multipart.Form) (string, []controlplane.CodexOpenAIAuthUpload, error) {
	if form == nil {
		return "", nil, nil
	}
	group := strings.TrimSpace(firstMultipartValue(form.Value["group"]))
	var files []controlplane.CodexOpenAIAuthUpload
	var totalSize int64
	for _, header := range form.File["accounts"] {
		filename := strings.TrimSpace(header.Filename)
		if filename == "" {
			continue
		}
		if header.Size > 0 {
			totalSize += header.Size
			if totalSize > codexUploadBodyLimit {
				return "", nil, fmt.Errorf("uploaded account files are too large")
			}
		}
		file, err := header.Open()
		if err != nil {
			files = append(files, controlplane.CodexOpenAIAuthUpload{Name: filename, Error: err.Error()})
			continue
		}
		payload, readErr := readUploadPart(file, codexUploadFileLimit)
		if closeErr := file.Close(); readErr == nil && closeErr != nil {
			readErr = closeErr
		}
		if readErr != nil {
			files = append(files, controlplane.CodexOpenAIAuthUpload{Name: filename, Error: readErr.Error()})
			continue
		}
		files = append(files, controlplane.CodexOpenAIAuthUpload{Name: filename, Payload: payload})
	}
	return group, files, nil
}

func firstMultipartValue(values []string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func readCodexUploadParts(reader *multipart.Reader) (string, []controlplane.CodexOpenAIAuthUpload, error) {
	var group string
	var files []controlplane.CodexOpenAIAuthUpload
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, err
		}
		func() {
			defer part.Close()
			switch part.FormName() {
			case "group":
				raw, readErr := io.ReadAll(io.LimitReader(part, 4096))
				if readErr == nil {
					group = strings.TrimSpace(string(raw))
				}
			case "accounts":
				filename := strings.TrimSpace(part.FileName())
				if filename == "" {
					return
				}
				payload, readErr := readUploadPart(part, codexUploadFileLimit)
				if readErr != nil {
					files = append(files, controlplane.CodexOpenAIAuthUpload{Name: filename, Error: readErr.Error()})
					return
				}
				files = append(files, controlplane.CodexOpenAIAuthUpload{Name: filename, Payload: payload})
			}
		}()
	}
	return group, files, nil
}

func readUploadPart(reader io.Reader, maxBytes int64) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > maxBytes {
		return nil, fmt.Errorf("uploaded account file is too large")
	}
	return payload, nil
}

func (s *Server) handleOpenAIOAuthPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, consoleURLEncodedFormBodyLimit)
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": err.Error()})
		return
	}
	result, err := s.control.CompleteOpenAIOAuth(r.Context(), r.FormValue("device_code"))
	if errors.Is(err, controlplane.ErrOAuthPending) {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
		return
	}
	if err != nil {
		s.recordAdminAudit(r, "error", "account.connect", "account", "", strings.TrimSpace(r.FormValue("label")), err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": err.Error()})
		return
	}
	if strings.TrimSpace(result.TargetAccountID) != "" {
		if account, err := s.control.GetAccount(result.TargetAccountID); err == nil {
			s.recordAdminAudit(r, "ok", "account.oauth_token_update", "account", account.ID, account.Label, "provider=openai source=oauth")
		}
		s.publishAccountPoolChanged()
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "redirect": accountTokenUpdatedURL(result.TargetAccountID)})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "redirect": "/admin/connect/openai?oauth_import=" + url.QueryEscape(result.ImportID)})
}

func (s *Server) handleClaudeOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/connect/claude/oauth" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	accountID := strings.TrimSpace(r.URL.Query().Get("account_id"))
	var start controlplane.ClaudeOAuthStart
	var err error
	if accountID != "" {
		start, err = s.control.StartClaudeOAuthForAccount(r.Context(), s.claudeOAuthRedirectURI(r), accountID)
	} else {
		start, err = s.control.StartClaudeOAuth(r.Context(), s.claudeOAuthRedirectURI(r))
	}
	if err != nil {
		s.redirectWithNoticeError(w, r, oauthStartErrorTarget(core.ProviderClaude, accountID), err)
		return
	}
	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":       "page_title_claude_oauth",
		"ActiveNav":      "accounts",
		"Locale":         locale,
		"Start":          start,
		"Error":          strings.TrimSpace(r.URL.Query().Get("error")),
		"OAuthAccountID": accountID,
		"OAuthReturnURL": oauthReturnURL(core.ProviderClaude, accountID),
	}, r)
	s.render(w, "connect_claude_oauth.html", locale, data)
}

func (s *Server) handleClaudeOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/connect/claude/oauth/callback" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if errCode := strings.TrimSpace(r.URL.Query().Get("error")); errCode != "" {
		message := strings.TrimSpace(r.URL.Query().Get("error_description"))
		if message == "" {
			message = errCode
		}
		s.redirectClaudeOAuthError(w, r, message)
		return
	}

	result, err := s.control.CompleteClaudeOAuth(r.Context(), r.URL.Query().Get("state"), r.URL.Query().Get("code"))
	if err != nil {
		s.redirectClaudeOAuthError(w, r, err.Error())
		return
	}
	if strings.TrimSpace(result.TargetAccountID) != "" {
		if account, err := s.control.GetAccount(result.TargetAccountID); err == nil {
			s.recordAdminAudit(r, "ok", "account.oauth_token_update", "account", account.ID, account.Label, "provider=claude source=oauth")
		}
		s.publishAccountPoolChanged()
		http.Redirect(w, r, accountTokenUpdatedURL(result.TargetAccountID), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/connect/claude?oauth_import="+url.QueryEscape(result.ImportID), http.StatusSeeOther)
}

func (s *Server) redirectClaudeOAuthError(w http.ResponseWriter, r *http.Request, message string) {
	target := "/admin/connect/claude/oauth"
	if trimmed := strings.TrimSpace(message); trimmed != "" {
		target += "?error=" + url.QueryEscape(trimmed)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) handleConnectComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, connectErrorTarget(core.ProviderKind(r.FormValue("provider"))), err)
		return
	}

	provider := core.ProviderKind(r.FormValue("provider"))
	priority, _ := strconv.Atoi(r.FormValue("priority"))
	weight, _ := strconv.Atoi(r.FormValue("weight"))
	if priority == 0 {
		priority = 100
	}
	if weight == 0 {
		weight = 100
	}

	expiresAt, err := parseOptionalTimestamp(r.FormValue("expires_at"))
	if err != nil {
		s.renderConnectFormError(w, r, err)
		return
	}

	err = s.control.CompleteManualConnect(controlplane.ManualConnectInput{
		Provider:            provider,
		Label:               r.FormValue("label"),
		Group:               strings.TrimSpace(r.FormValue("group")),
		ProxyURL:            r.FormValue("proxy_url"),
		AccessToken:         r.FormValue("access_token"),
		RefreshToken:        r.FormValue("refresh_token"),
		SessionToken:        r.FormValue("session_token"),
		BaseURL:             r.FormValue("base_url"),
		ExpiresAt:           expiresAt,
		Backup:              parseBoolFormValue(r.FormValue("backup")),
		Priority:            priority,
		Weight:              weight,
		AccountLoginMethod:  r.FormValue("account_login_method"),
		APIKeyQuotaProvider: r.FormValue("api_key_quota_provider"),
		CredentialMode:      r.FormValue("credential_mode"),
		TokenSource:         r.FormValue("token_source"),
		OAuthAccountID:      r.FormValue("oauth_account_id"),
		OAuthEmail:          r.FormValue("oauth_email"),
		CodexAuthPath:       r.FormValue("codex_auth_path"),
	})
	if err != nil {
		s.recordAdminAudit(r, "error", "account.connect", "account", "", strings.TrimSpace(r.FormValue("label")), err.Error())
		s.renderConnectFormError(w, r, err)
		return
	}
	message := fmt.Sprintf("provider=%s priority=%d weight=%d backup=%t", provider, priority, weight, parseBoolFormValue(r.FormValue("backup")))
	if providers.IsOpenAIOAuthTokenSource(r.FormValue("token_source")) {
		message += " source=oauth"
	}
	s.recordAdminAudit(r, "ok", "account.connect", "account", "", strings.TrimSpace(r.FormValue("label")), message)
	s.publishAccountPoolChanged()
	http.Redirect(w, r, accountCreatedRedirect(r.FormValue("group")), http.StatusSeeOther)
}

func (s *Server) renderConnectFormError(w http.ResponseWriter, r *http.Request, err error) {
	s.redirectWithNoticeError(w, r, connectErrorTarget(core.ProviderKind(r.FormValue("provider"))), err)
}

func connectErrorTarget(provider core.ProviderKind) string {
	switch provider {
	case core.ProviderClaude:
		return "/admin/connect/claude"
	case core.ProviderOpenAI:
		return "/admin/connect/openai"
	default:
		return "/admin/accounts"
	}
}

func oauthStartErrorTarget(provider core.ProviderKind, accountID string) string {
	if strings.TrimSpace(accountID) != "" {
		return accountEditURL(accountID)
	}
	return connectErrorTarget(provider)
}

func (s *Server) handleAccountActions(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/admin/accounts/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}

	switch parts[1] {
	case "toggle":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		before, beforeErr := s.control.GetAccount(parts[0])
		if err := s.control.ToggleAccount(parts[0]); err != nil {
			s.recordAdminAudit(r, "error", "account.toggle", "account", parts[0], "", err.Error())
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		if account, err := s.control.GetAccount(parts[0]); err == nil {
			message := auditFieldsMessage(
				auditMessageField{Key: "control_disabled", Value: fmt.Sprintf("%t", account.ControlDisabled)},
				auditMessageField{Key: "status", Value: string(account.Status)},
			)
			if beforeErr == nil {
				message = auditFieldsMessage(
					auditMessageField{Key: "control_disabled_from", Value: fmt.Sprintf("%t", before.ControlDisabled)},
					auditMessageField{Key: "control_disabled_to", Value: fmt.Sprintf("%t", account.ControlDisabled)},
					auditMessageField{Key: "status", Value: string(account.Status)},
				)
			}
			s.recordAdminAudit(r, "ok", "account.toggle", "account", account.ID, account.Label, message)
		}
		s.publishAccountPoolChanged()
		http.Redirect(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), http.StatusSeeOther)
	case "refresh-quota":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		account, snapshot, err := s.control.RefreshAccountQuota(r.Context(), parts[0])
		if err != nil {
			s.recordAdminAudit(r, "error", "account.refresh_quota", "account", parts[0], "", err.Error())
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		message := fmt.Sprintf("source=%s", snapshot.Source)
		if strings.TrimSpace(snapshot.Plan) != "" {
			message += " plan=" + strings.TrimSpace(snapshot.Plan)
		}
		s.recordAdminAudit(r, "ok", "account.refresh_quota", "account", account.ID, account.Label, message)
		s.publishAccountPoolChanged()
		http.Redirect(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), http.StatusSeeOther)
	case "delete":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		account, err := s.control.DeleteAccount(parts[0])
		if err != nil {
			s.recordAdminAudit(r, "error", "account.delete", "account", parts[0], "", err.Error())
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		s.recordAdminAudit(r, "ok", "account.delete", "account", account.ID, account.Label, fmt.Sprintf("provider=%s", account.Provider))
		s.publishAccountPoolChanged()
		http.Redirect(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), http.StatusSeeOther)
	case "test":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		account, err := s.control.DetectAccountStatus(ctx, parts[0])
		locale := resolveLocale(w, r)
		if err != nil {
			s.recordAdminAudit(r, "error", "account.detect", "account", parts[0], "", err.Error())
			http.Redirect(w, r, accountTestRedirect(accountGroupReturnKey(r), accountFilterReturnValue(r), "bad", accountDetectFailureMessage(locale, account, parts[0], err)), http.StatusSeeOther)
			return
		}
		s.recordAdminAudit(r, "ok", "account.detect", "account", account.ID, account.Label, fmt.Sprintf("status=%s", account.Status))
		s.publishAccountPoolChanged()
		http.Redirect(w, r, accountTestRedirect(accountGroupReturnKey(r), accountFilterReturnValue(r), "good", accountDetectSuccessMessage(locale, account)), http.StatusSeeOther)
	case "recover":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		account, err := s.control.RecoverAccount(parts[0])
		if err != nil {
			s.recordAdminAudit(r, "error", "account.recover", "account", parts[0], "", err.Error())
			s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
			return
		}
		s.recordAdminAudit(r, "ok", "account.recover", "account", account.ID, account.Label, fmt.Sprintf("status=%s total_fails=%d", account.Status, account.TotalFails))
		s.publishAccountPoolChanged()
		http.Redirect(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), http.StatusSeeOther)
	case "edit":
		switch r.Method {
		case http.MethodGet:
			s.handleAccountEditPage(w, r, parts[0])
		case http.MethodPost:
			s.handleAccountEditSubmit(w, r, parts[0])
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	default:
		http.NotFound(w, r)
	}
}

func accountTestRedirect(groupKey, filterKey, tone, message string) string {
	message = truncateNoticeMessage(message)
	values := url.Values{}
	if groupKey = strings.TrimSpace(groupKey); groupKey != "" {
		values.Set("group", groupKey)
	}
	if filterKey = normalizeAccountFilterValue(filterKey); filterKey != "all" {
		values.Set("filter", filterKey)
	}
	values.Set("test_tone", tone)
	values.Set("test_message", message)
	return "/admin/accounts?" + values.Encode()
}

func accountCreatedRedirect(groupName string) string {
	values := url.Values{"created": []string{"1"}}
	groupName = accountGroupQueryKey(groupName)
	if groupName != "" && !strings.EqualFold(groupName, core.DefaultAccountGroupName) {
		values.Set("group", groupName)
	}
	return "/admin/accounts?" + values.Encode()
}

func accountDetectSuccessMessage(locale string, account core.Account) string {
	return translatef(
		locale,
		"account_detect_success",
		accountNoticeLabel(account, ""),
		accountRuntimeStatusText(locale, controlplane.AccountRuntimeStatus(account)),
	)
}

func accountDetectFailureMessage(locale string, account core.Account, fallbackID string, err error) string {
	status := accountRuntimeStatusText(locale, controlplane.AccountRuntimeStatus(account))
	if strings.TrimSpace(string(account.Status)) == "" {
		status = translate(locale, "unknown")
	}
	return translatef(
		locale,
		"account_detect_failure",
		accountNoticeLabel(account, fallbackID),
		status,
		accountCheckReasonText(locale, errorString(err)),
	)
}

func accountNoticeLabel(account core.Account, fallbackID string) string {
	if label := strings.TrimSpace(account.Label); label != "" {
		return label
	}
	if id := strings.TrimSpace(account.ID); id != "" {
		return id
	}
	return strings.TrimSpace(fallbackID)
}

func accountCheckReasonText(locale string, message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return translate(locale, "account_detect_reason_unknown")
	}
	if strings.Contains(message, "context deadline exceeded") {
		return translate(locale, "account_detect_reason_timeout")
	}
	for _, prefix := range []string{
		"upstream_transport_error:",
		"upstream_read_error:",
		"upstream_server_error:",
		"upstream_temporarily_unavailable:",
		"upstream_rate_limited:",
		"upstream_auth_error:",
		"gateway_api_key_disabled:",
		"credential_expired:",
	} {
		if strings.HasPrefix(message, prefix) {
			message = strings.TrimSpace(strings.TrimPrefix(message, prefix))
			break
		}
	}
	return truncateRunes(message, 110)
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Server) handleAccountEditPage(w http.ResponseWriter, r *http.Request, accountID string) {
	editor, err := s.control.GetAccountEditor(accountID)
	if err != nil {
		s.redirectWithNoticeError(w, r, accountsGroupHref(accountGroupReturnKey(r)), err)
		return
	}

	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":               "page_title_account_edit",
		"ActiveNav":              "accounts",
		"Locale":                 locale,
		"Editor":                 editor,
		"ReturnAccountGroupKey":  accountGroupReturnKey(r),
		"ReturnAccountFilterKey": accountFilterReturnValue(r),
		"OAuthTokenUpdateURL":    accountOAuthTokenUpdateURL(editor.Account),
		"TokenUpdated":           r.URL.Query().Get("token_updated") == "1",
	}, r)
	s.render(w, "account_edit.html", locale, data)
}

func (s *Server) handleAccountEditSubmit(w http.ResponseWriter, r *http.Request, accountID string) {
	if err := r.ParseForm(); err != nil {
		s.renderAccountEditFormError(w, r, accountID, err)
		return
	}
	before, beforeErr := s.control.GetAccount(accountID)
	hasBefore := beforeErr == nil

	priority, _ := strconv.Atoi(r.FormValue("priority"))
	weight, _ := strconv.Atoi(r.FormValue("weight"))
	if priority == 0 {
		priority = 100
	}
	if weight == 0 {
		weight = 100
	}

	expiresAt, err := parseOptionalTimestamp(r.FormValue("expires_at"))
	if err != nil {
		s.renderAccountEditFormError(w, r, accountID, err)
		return
	}

	if err := s.control.UpdateAccount(
		accountID,
		r.FormValue("label"),
		r.FormValue("remark"),
		strings.TrimSpace(r.FormValue("group")),
		r.FormValue("proxy_url"),
		r.FormValue("access_token"),
		r.FormValue("refresh_token"),
		r.FormValue("session_token"),
		r.FormValue("base_url"),
		expiresAt,
		priority,
		weight,
		core.AccountStatus(r.FormValue("status")),
		r.FormValue("control_enabled") == "",
		parseBoolFormValue(r.FormValue("backup")),
	); err != nil {
		s.recordAdminAudit(r, "error", "account.update", "account", accountID, strings.TrimSpace(r.FormValue("label")), err.Error())
		s.renderAccountEditFormError(w, r, accountID, err)
		return
	}
	if account, err := s.control.GetAccount(accountID); err == nil {
		s.recordAdminAudit(r, "ok", "account.update", "account", account.ID, account.Label, auditAccountUpdateChangeMessage(
			before,
			account,
			hasBefore,
			strings.TrimSpace(r.FormValue("access_token")) != "",
			strings.TrimSpace(r.FormValue("refresh_token")) != "",
			strings.TrimSpace(r.FormValue("session_token")) != "",
		))
	}

	s.publishAccountPoolChanged()
	http.Redirect(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), http.StatusSeeOther)
}

func (s *Server) renderAccountEditFormError(w http.ResponseWriter, r *http.Request, accountID string, err error) {
	target := accountEditURL(accountID)
	values := url.Values{}
	if groupKey := accountGroupReturnKey(r); groupKey != "" {
		values.Set("group", groupKey)
	}
	if filterKey := accountFilterReturnValue(r); filterKey != "all" {
		values.Set("filter", filterKey)
	}
	if len(values) > 0 {
		target += "?" + values.Encode()
	}
	s.redirectWithNoticeError(w, r, target, err)
}

func accountOAuthTokenUpdateURL(account core.Account) string {
	if !providers.CredentialRefreshable(account) {
		return ""
	}
	switch account.Provider {
	case core.ProviderOpenAI:
		return "/admin/connect/openai/oauth?account_id=" + url.QueryEscape(account.ID)
	case core.ProviderClaude:
		return "/admin/connect/claude/oauth?account_id=" + url.QueryEscape(account.ID)
	default:
		return ""
	}
}

func oauthReturnURL(provider core.ProviderKind, accountID string) string {
	if strings.TrimSpace(accountID) != "" {
		return accountEditURL(accountID)
	}
	return connectErrorTarget(provider)
}

func accountEditURL(accountID string) string {
	return "/admin/accounts/" + url.PathEscape(strings.TrimSpace(accountID)) + "/edit"
}

func accountTokenUpdatedURL(accountID string) string {
	return accountEditURL(accountID) + "?token_updated=1"
}
