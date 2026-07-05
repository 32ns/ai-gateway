package web

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	personalpay "personalpay/sdk-go"
)

func (s *Server) render(w http.ResponseWriter, page, locale string, data any) {
	templates, templateErr := s.currentTemplates()
	if templateErr != nil {
		http.Error(w, templateErr.Error(), http.StatusInternalServerError)
		return
	}
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	base := templates[page]
	if base == nil {
		http.Error(w, "template not found: "+page, http.StatusInternalServerError)
		return
	}
	tmpl, err := base.Clone()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data = s.withGlobalTemplateData(data)
	tmpl.Funcs(renderFuncMap(s, locale, data))
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) renderFragment(w http.ResponseWriter, page, templateName, locale string, data any) {
	templates, templateErr := s.currentTemplates()
	if templateErr != nil {
		http.Error(w, templateErr.Error(), http.StatusInternalServerError)
		return
	}
	base := templates[page]
	if base == nil {
		http.Error(w, "template not found: "+page, http.StatusInternalServerError)
		return
	}
	tmpl, err := base.Clone()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data = s.withGlobalTemplateData(data)
	tmpl.Funcs(renderFuncMap(s, locale, data))
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, templateName, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func (s *Server) withGlobalTemplateData(data any) any {
	values, ok := data.(map[string]any)
	if !ok {
		return data
	}
	if _, exists := values["Home"]; !exists {
		values["Home"] = s.currentHomeSettings()
	}
	if _, exists := values["ShowImageLabNav"]; !exists {
		if user, ok := values["CurrentUser"].(core.User); ok {
			values["ShowImageLabNav"] = s.imageLabVisibleForUser(user)
		}
	}
	if _, exists := values["ShowImageLabPublic"]; !exists {
		values["ShowImageLabPublic"] = s.imageLabPublicVisible()
	}
	if _, exists := values["PublicPopupSiteMessages"]; !exists && s != nil && s.control != nil {
		values["PublicPopupSiteMessages"] = s.control.ListPublicPopupSiteMessages(5)
	}
	return values
}

func (s *Server) currentHomeSettings() core.SystemHomeSettings {
	settings := core.DefaultSystemSettings()
	if s != nil && s.control != nil {
		if loaded, err := s.control.GetSystemSettings(); err == nil {
			settings = loaded
		}
	}
	return core.NormalizeSystemHomeSettings(settings.Home)
}

func (s *Server) imageLabVisibleForUser(user core.User) bool {
	if strings.TrimSpace(user.ID) == "" {
		return false
	}
	if user.IsAdmin() {
		return true
	}
	settings := core.DefaultSystemSettings()
	if s != nil && s.control != nil {
		if loaded, err := s.control.GetSystemSettings(); err == nil {
			settings = loaded
		}
	}
	return core.ImageUserConsoleEnabled(settings.Image)
}

func (s *Server) imageLabPublicVisible() bool {
	settings := core.DefaultSystemSettings()
	if s != nil && s.control != nil {
		if loaded, err := s.control.GetSystemSettings(); err == nil {
			settings = loaded
		}
	}
	return core.ImageUserConsoleEnabled(settings.Image)
}

type userIdentityView struct {
	ID       string
	Username string
	Known    bool
}

func timedMultiplierWeekdayLabels(locale string) []string {
	return []string{
		translate(locale, "weekday_mon"),
		translate(locale, "weekday_tue"),
		translate(locale, "weekday_wed"),
		translate(locale, "weekday_thu"),
		translate(locale, "weekday_fri"),
		translate(locale, "weekday_sat"),
		translate(locale, "weekday_sun"),
	}
}

func (s *Server) currentTemplates() (map[string]*template.Template, error) {
	return s.templates, s.templateErr
}

func compileTemplates(assetFS fs.FS) (map[string]*template.Template, error) {
	pages, err := fs.Glob(assetFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	partials, err := fs.Glob(assetFS, "templates/*_panel.html")
	if err != nil {
		return nil, err
	}
	out := make(map[string]*template.Template, len(pages))
	funcs := renderFuncMap(nil, "", nil)
	for _, pagePath := range pages {
		page := strings.TrimPrefix(pagePath, "templates/")
		if page == "layout.html" || strings.HasSuffix(page, "_panel.html") {
			continue
		}
		files := append([]string{"templates/layout.html"}, partials...)
		files = append(files, pagePath)
		tmpl, err := template.New("").Funcs(funcs).ParseFS(assetFS, files...)
		if err != nil {
			return nil, err
		}
		out[page] = tmpl
	}
	return out, nil
}

func renderFuncMap(s *Server, locale string, data any) template.FuncMap {
	userIdentityCache := userIdentityMapFromData(data)
	userIdentity := func(userID string) userIdentityView {
		userID = strings.TrimSpace(userID)
		if userID == "" {
			return userIdentityView{}
		}
		if userIdentityCache == nil {
			userIdentityCache = make(map[string]core.User)
		}
		if user, ok := userIdentityCache[userID]; ok {
			return userIdentityView{ID: user.ID, Username: user.Username, Known: true}
		}
		if s != nil && s.control != nil {
			if user, err := s.control.GetUser(userID); err == nil {
				userIdentityCache[user.ID] = user
				return userIdentityView{ID: user.ID, Username: user.Username, Known: true}
			}
		}
		return userIdentityView{ID: userID}
	}
	var accountGroupCache []core.AccountGroup
	accountGroups := func() []core.AccountGroup {
		if accountGroupCache != nil {
			return accountGroupCache
		}
		if s != nil && s.control != nil {
			accountGroupCache = s.control.ListAccountGroups()
		}
		if len(accountGroupCache) == 0 {
			accountGroupCache = []core.AccountGroup{{
				ID:                       core.DefaultAccountGroupID,
				Name:                     core.DefaultAccountGroupName,
				BillingMultiplierBps:     core.AccountGroupDefaultMultiplierBps,
				PlanBillingMultiplierBps: core.AccountGroupDefaultMultiplierBps,
			}}
		}
		return accountGroupCache
	}
	accountGroupByName := func(name string) core.AccountGroup {
		name = core.NormalizeAccountGroupName(name)
		for _, group := range accountGroups() {
			if strings.EqualFold(core.NormalizeAccountGroupName(group.Name), name) {
				return core.NormalizeAccountGroupBilling(group)
			}
		}
		return core.NormalizeAccountGroupBilling(core.AccountGroup{
			Name: name,
		})
	}
	accountGroupMultiplierOptionText := func(group core.AccountGroup) string {
		now := time.Now()
		multiplier := core.EffectiveAccountGroupMultiplier(group, now)
		if rule, ok := core.ActiveAccountGroupTimedMultiplier(group, now); ok {
			name := strings.TrimSpace(rule.Name)
			if name == "" {
				name = translate(locale, "timed_multiplier_unnamed")
			}
			return "[" + name + "] " + core.FormatMultiplier(multiplier) + "x"
		}
		return core.FormatMultiplier(multiplier) + "x"
	}
	accountGroupPlanMultiplierOptionText := func(group core.AccountGroup) string {
		return core.FormatMultiplier(core.EffectiveAccountGroupPlanMultiplier(group, time.Now())) + "x"
	}
	accountGroupBillingOptionText := func(group core.AccountGroup, source string) string {
		source = core.NormalizeClientBillingSource(source)
		billingText := translate(locale, "balance_billing")
		multiplierText := accountGroupMultiplierOptionText(group)
		if source == core.ClientBillingSourcePlan {
			billingText = translate(locale, "plan_billing")
			multiplierText = accountGroupPlanMultiplierOptionText(group)
		}
		return fmt.Sprintf("%s (%s) [%s]", accountGroupLabelText(locale, group.Name), multiplierText, billingText)
	}
	return template.FuncMap{
		"maskToken":     maskToken,
		"joinProviders": joinProviders,
		"joinLines":     joinLines,
		"formatTime":    formatTime,
		"add": func(a, b int) int {
			return a + b
		},
		"dict": templateDict,
		"userDashboardCustomPanelHTML": func(settings core.SystemUserDashboardSettings) template.HTML {
			return template.HTML(settings.CustomPanelHTML)
		},
		"pathEscape":       url.PathEscape,
		"modalReturnPath":  modalReturnPath,
		"formatNanoUSD":    formatNanoUSDTemplate,
		"formatMultiplier": core.FormatMultiplier,
		"formatPercentBps": formatPercentBps,
		"planPeriodHours": func(plan core.BillingPlan) int64 {
			if plan.PeriodDurationSec <= 0 {
				return 24
			}
			return plan.PeriodDurationSec / 3600
		},
		"planActionURL":                 planActionURL,
		"planGroupActionURL":            planGroupActionURL,
		"adminPlansGroupHref":           adminPlansGroupHref,
		"adminPlanTabURL":               adminPlanTabURL,
		"planSubscriptionPageURL":       planSubscriptionPageURL,
		"planGroupDisplayName":          planGroupDisplayName,
		"planDialogID":                  planDialogID,
		"planSubscriptionUsageDialogID": planSubscriptionUsageDialogID,
		"planQuotaUsed":                 planQuotaUsed,
		"planQuotaRemaining":            planQuotaRemaining,
		"planQuotaUsedPercent":          planQuotaUsedPercent,
		"planQuotaRemainingPercent":     planQuotaRemainingPercent,
		"planCurrentQuotaLimit":         planCurrentQuotaLimit,
		"activePlansHasPlanID":          activePlansHasPlanID,
		"activePlansForPlanID":          activePlansForPlanID,
		"planEntitlementStatusText": func(status core.UserPlanEntitlementStatus) string {
			return planEntitlementStatusText(locale, status)
		},
		"planEntitlementStatusClass": planEntitlementStatusClass,
		"imageUserConsoleEnabled": func(settings core.SystemImageSettings) bool {
			return core.ImageUserConsoleEnabled(settings)
		},
		"responsesWebSocketUpstreamEnabled": func(settings core.SystemRuntimeSettings) bool {
			return core.ResponsesWebSocketUpstreamEnabled(settings)
		},
		"mcpTokenHasScope": func(token core.MCPToken, scope string) bool {
			return token.HasScope(scope)
		},
		"accountBatchSummaryText": func(selected, visible int) string {
			summary := translate(locale, "account_batch_summary_template")
			summary = strings.ReplaceAll(summary, "%selected%", fmt.Sprint(selected))
			summary = strings.ReplaceAll(summary, "%visible%", fmt.Sprint(visible))
			return summary
		},
		"timedMultiplierWeekdays": func() []struct {
			Value int
			Label string
		} {
			return []struct {
				Value int
				Label string
			}{
				{Value: 1, Label: translate(locale, "weekday_mon")},
				{Value: 2, Label: translate(locale, "weekday_tue")},
				{Value: 3, Label: translate(locale, "weekday_wed")},
				{Value: 4, Label: translate(locale, "weekday_thu")},
				{Value: 5, Label: translate(locale, "weekday_fri")},
				{Value: 6, Label: translate(locale, "weekday_sat")},
				{Value: 7, Label: translate(locale, "weekday_sun")},
			}
		},
		"timedMultiplierWeekdaySelected": func(weekdays []int, weekday int) bool {
			return slices.Contains(weekdays, weekday)
		},
		"timedMultiplierWeekdaySummary": func(weekdays []int) string {
			if len(weekdays) == 0 {
				return translate(locale, "timed_multiplier_everyday")
			}
			labels := timedMultiplierWeekdayLabels(locale)
			parts := make([]string, 0, len(weekdays))
			for _, weekday := range weekdays {
				if weekday >= 1 && weekday <= len(labels) {
					parts = append(parts, labels[weekday-1])
				}
			}
			if len(parts) == 0 {
				return translate(locale, "timed_multiplier_everyday")
			}
			return strings.Join(parts, " ")
		},
		"timedMultiplierDateSummary": func(rule core.AccountGroupTimedMultiplier) string {
			if rule.StartDate == "" && rule.EndDate == "" {
				return translate(locale, "timed_multiplier_any_date")
			}
			start := rule.StartDate
			if start == "" {
				start = translate(locale, "timed_multiplier_unbounded_start")
			}
			end := rule.EndDate
			if end == "" {
				end = translate(locale, "timed_multiplier_unbounded_end")
			}
			return start + " - " + end
		},
		"timedMultiplierTimeSummary": func(rule core.AccountGroupTimedMultiplier) string {
			if rule.StartTime == "" && rule.EndTime == "" {
				return translate(locale, "timed_multiplier_all_day")
			}
			return rule.StartTime + " - " + rule.EndTime
		},
		"timedMultiplierExpired": func(rule core.AccountGroupTimedMultiplier) bool {
			return core.TimedMultiplierExpired(rule, time.Now())
		},
		"timedMultiplierStatusText": func(rule core.AccountGroupTimedMultiplier) string {
			if core.TimedMultiplierExpired(rule, time.Now()) {
				return translate(locale, "timed_multiplier_status_expired")
			}
			if rule.Enabled {
				return translate(locale, "timed_multiplier_status_enabled")
			}
			return translate(locale, "timed_multiplier_status_disabled")
		},
		"timedMultiplierStatusClass": func(rule core.AccountGroupTimedMultiplier) string {
			if core.TimedMultiplierExpired(rule, time.Now()) {
				return "tone-bad"
			}
			if rule.Enabled {
				return "tone-good"
			}
			return "tone-muted"
		},
		"effectiveAccountGroupMultiplier": func(group core.AccountGroup) int64 {
			return core.EffectiveAccountGroupMultiplier(group, time.Now())
		},
		"accountGroupPlanBillingEnabled": func(group controlplane.AccountGroupSection) bool {
			return core.AccountGroupPlanBillingEnabled(core.AccountGroup{PlanBillingEnabled: group.PlanBillingEnabled})
		},
		"accountGroupMultiplierOptionText":     accountGroupMultiplierOptionText,
		"accountGroupPlanMultiplierOptionText": accountGroupPlanMultiplierOptionText,
		"accountGroupBillingOptionText":        accountGroupBillingOptionText,
		"accountGroupBillingOptionValue":       accountGroupBillingOptionValue,
		"clientBillingSourceSelected": func(editor controlplane.ClientEditor, group core.AccountGroup, source string) bool {
			return strings.EqualFold(accountGroupQueryKey(editor.SelectedAccountGroup), accountGroupQueryKey(group.Name)) &&
				core.NormalizeClientBillingSource(editor.SelectedBillingSource) == core.NormalizeClientBillingSource(source)
		},
		"accountGroupAllowsPlanBilling": func(group core.AccountGroup) bool {
			return core.AccountGroupPlanBillingEnabled(group)
		},
		"clientBillingSourceCash": func() string {
			return core.ClientBillingSourceCash
		},
		"clientBillingSourcePlan": func() string {
			return core.ClientBillingSourcePlan
		},
		"clientBillingSourceText": func(client core.APIClient) string {
			if core.NormalizeClientBillingSource(client.BillingSource) == core.ClientBillingSourcePlan {
				return translate(locale, "plan_billing")
			}
			return translate(locale, "balance_billing")
		},
		"clientBillingOptionText": func(client core.APIClient) string {
			return accountGroupBillingOptionText(accountGroupByName(client.AccountGroup), client.BillingSource)
		},
		"billingModeText":       billingModeText,
		"pricingExpression":     pricingExpression,
		"modelPricingBreakdown": modelPricingBreakdown,
		"modelGroupSelected":    modelGroupSelected,
		"modelGroupsText": func(model core.ModelConfig) string {
			return modelGroupListTextForLocale(locale, model.VisibleGroups, translate(locale, "no_visible_groups"))
		},
		"siteMessageTargetMode":          siteMessageTargetMode,
		"siteMessageTargetUserSelected":  siteMessageTargetUserSelected,
		"siteMessageTargetGroupSelected": siteMessageTargetGroupSelected,
		"siteMessageTargetText": func(message core.SiteMessage, users []core.User) string {
			return siteMessageTargetText(locale, message, users)
		},
		"siteMessageBrowserReadKey": siteMessageBrowserReadKey,
		"renderSiteMessageMarkdown": renderSiteMessageMarkdown,
		"supportStatusText": func(status core.SupportTicketStatus) string {
			return supportStatusText(locale, status)
		},
		"avatarInitial": avatarInitial,
		"documentStatusText": func(status core.DocumentStatus) string {
			return documentStatusText(locale, status)
		},
		"documentStatusClass": documentStatusClass,
		"documentIndexingText": func(document core.Document) string {
			return documentIndexingText(locale, document)
		},
		"documentIndexingClass": documentIndexingClass,
		"documentSEOIndexable":  controlplane.DocumentSEOIndexable,
		"documentRobotsNoIndex": controlplane.DocumentRobotsNoIndex,
		"formatUSD": func(nanoUSD int64) string {
			return formatUSDDisplay(nanoUSD)
		},
		"formatCNYCents": formatCNYCentsDisplay,
		"cooldownText":   cooldownText,
		"statusTone":     statusTone,
		"containsTag":    containsTag,
		"containsString": func(values []string, target string) bool {
			target = strings.ToLower(strings.TrimSpace(target))
			for _, value := range values {
				if strings.ToLower(strings.TrimSpace(value)) == target {
					return true
				}
			}
			return false
		},
		"t": func(key string) string {
			return translate(locale, key)
		},
		"tf": func(key string, args ...any) string {
			return translatef(locale, key, args...)
		},
		"providerListText": func(providers []core.ProviderKind) string {
			return providerListText(locale, providers)
		},
		"accountRoleText": func(account core.Account) string {
			return accountRoleText(locale, account)
		},
		"accountGroupLabel": func(group string) string {
			return strings.TrimSpace(group)
		},
		"accountGroupRemark": func(group string) string {
			return strings.TrimSpace(accountGroupByName(group).Remark)
		},
		"accountLabelDisplay": displayAccountLabel,
		"accountGroupTypeOptions": func() []struct {
			Value string
			Label string
		} {
			return []struct {
				Value string
				Label string
			}{
				{Value: core.AccountGroupTypeMixed, Label: translate(locale, "account_group_type_mixed")},
				{Value: core.AccountGroupTypeOpenAI, Label: translate(locale, "account_group_type_openai")},
				{Value: core.AccountGroupTypeClaude, Label: translate(locale, "account_group_type_claude")},
			}
		},
		"accountGroupTypeSelected": func(current, value string) bool {
			return core.NormalizeAccountGroupType(current) == core.NormalizeAccountGroupType(value)
		},
		"accountGroupTypeText": func(groupType string) string {
			switch core.NormalizeAccountGroupType(groupType) {
			case core.AccountGroupTypeOpenAI:
				return translate(locale, "account_group_type_openai")
			case core.AccountGroupTypeClaude:
				return translate(locale, "account_group_type_claude")
			default:
				return translate(locale, "account_group_type_mixed")
			}
		},
		"accountGroupShowInClientEditor": func(group controlplane.AccountGroupSection) bool {
			if strings.EqualFold(strings.TrimSpace(group.ID), "default") {
				return true
			}
			return core.AccountGroupVisibleInClientEditor(core.AccountGroup{ShowInClientEditor: group.ShowInClientEditor})
		},
		"accountGroupVisibleUserSelected": func(group controlplane.AccountGroupSection, userID string) bool {
			userID = strings.TrimSpace(userID)
			if userID == "" {
				return false
			}
			for _, visibleUserID := range group.VisibleUserIDs {
				if strings.EqualFold(strings.TrimSpace(visibleUserID), userID) {
					return true
				}
			}
			return false
		},
		"accountGroupQueryKey": accountGroupQueryKey,
		"accountGroupTabHref": func(key string) string {
			return accountsGroupHref(accountGroupQueryKey(key))
		},
		"accountsGroupHref": accountsGroupHref,
		"accountsGroupFilterHref": func(groupKey, filterKey string) string {
			return accountsGroupFilterHref(groupKey, filterKey)
		},
		"accountEditHref": func(accountID, groupKey, filterKey string) string {
			href := "/admin/accounts/" + url.PathEscape(accountID) + "/edit"
			values := url.Values{}
			if groupKey = strings.TrimSpace(groupKey); groupKey != "" {
				values.Set("group", groupKey)
			}
			if filterKey = normalizeAccountFilterValue(filterKey); filterKey != "all" {
				values.Set("filter", filterKey)
			}
			if len(values) == 0 {
				return href
			}
			return href + "?" + values.Encode()
		},
		"accountStatusText": func(status core.AccountStatus) string {
			return accountStatusText(locale, status)
		},
		"accountStatusClass": accountStatusClass,
		"healthStatusText": func(status string) string {
			return healthStatusText(locale, status)
		},
		"healthReasonText": func(reason string) string {
			return healthReasonText(locale, reason)
		},
		"personalPayAccountStatusText": func(status personalpay.AccountStatus) string {
			return personalPayAccountStatusText(locale, status)
		},
		"personalPayChannelText": func(channel personalpay.PaymentChannel) string {
			return personalPayChannelText(locale, channel)
		},
		"isAdminUser": func(user core.User) bool {
			return user.IsAdmin()
		},
		"oauthProvidersForUser": func(user core.User) []passwordOAuthProviderView {
			settings := core.DefaultSystemSettings()
			if s != nil && s.control != nil {
				if loaded, err := s.control.GetSystemSettings(); err == nil {
					settings = loaded
				}
			}
			return passwordOAuthProviders(user, settings)
		},
		"userRoleText": func(role core.UserRole) string {
			return userRoleText(locale, role)
		},
		"userRoleOptions":                 userRoleOptions,
		"userConcurrentRequestLimitValue": userConcurrentRequestLimitValue,
		"userConcurrentRequestLimitText": func(user core.User) string {
			return userConcurrentRequestLimitText(locale, user)
		},
		"userIPConcurrentRequestLimitValue": userIPConcurrentRequestLimitValue,
		"userIPConcurrentRequestLimitText": func(user core.User) string {
			return userIPConcurrentRequestLimitText(locale, user)
		},
		"userRequestRateLimitValue": userRequestRateLimitValue,
		"userRequestRateLimitText": func(user core.User) string {
			return userRequestRateLimitText(locale, user)
		},
		"userIdentity":      userIdentity,
		"userPageURL":       userPageURL,
		"userSearchPageURL": userSearchPageURL,
		"clientOwnerText": func(client core.APIClient, users []core.User) string {
			return clientOwnerText(locale, client, users)
		},
		"clientSpend": func(client core.APIClient) core.ClientSpend {
			if s == nil || s.control == nil {
				return core.ClientSpend{ClientID: client.ID, SpendLimitNanoUSD: client.SpendLimitNanoUSD}
			}
			spend, err := s.control.GetClientActualSpend(client.ID)
			if err != nil {
				return core.ClientSpend{ClientID: client.ID, SpendLimitNanoUSD: client.SpendLimitNanoUSD}
			}
			return spend
		},
		"clientsActualSpendTotal": func(clients []core.APIClient) int64 {
			var total int64
			if s == nil || s.control == nil {
				return total
			}
			for _, client := range clients {
				spend, err := s.control.GetClientActualSpend(client.ID)
				if err == nil {
					total = addDisplayNanoUSDSaturating(total, spend.SpendUsedNanoUSD)
				}
			}
			return total
		},
		"dashboardHistoricalSpend": func(user core.User, clients []core.APIClient) int64 {
			if s == nil || s.control == nil {
				return 0
			}
			userID := strings.TrimSpace(user.ID)
			if userID != "" && !user.IsAdmin() {
				return s.control.UserActualSpendTotal(user)
			}
			if user.IsAdmin() {
				return s.control.FinanceTotalSpendNanoUSD()
			}
			var total int64
			for _, client := range clients {
				spend, err := s.control.GetClientActualSpend(client.ID)
				if err == nil {
					total = addDisplayNanoUSDSaturating(total, spend.SpendUsedNanoUSD)
				}
			}
			return total
		},
		"userActualSpendTotal": func(user core.User, clients []core.APIClient) int64 {
			if s == nil || s.control == nil || strings.TrimSpace(user.ID) == "" {
				return 0
			}
			return s.control.UserActualSpendTotal(user)
		},
		"activeClientCount": func(clients []core.APIClient) int {
			count := 0
			for _, client := range clients {
				if client.Enabled {
					count++
				}
			}
			return count
		},
		"userUsageCostChart": func(user core.User) controlplane.UsageCostChart {
			if s == nil || s.control == nil {
				return controlplane.UsageCostChart{}
			}
			return s.control.UsageCostChartForUser(context.Background(), user, time.Now())
		},
		"userBillingLedger": func(user core.User) []core.BillingLedgerEntry {
			if s == nil || s.control == nil {
				return nil
			}
			return s.control.ListBillingLedger(user.ID, 8)
		},
		"billingLedgerKindText": func(kind string) string {
			return billingLedgerKindText(locale, kind)
		},
		"signedUSD": signedUSDDisplay,
		"accountQuota": func(account core.Account) *core.AccountQuotaSnapshot {
			return controlplane.ReadAccountQuota(account)
		},
		"accountQuotaError": func(account core.Account) string {
			message, _ := controlplane.ReadAccountQuotaError(account)
			return message
		},
		"accountQuotaErrorAt": func(account core.Account) *time.Time {
			_, at := controlplane.ReadAccountQuotaError(account)
			return at
		},
		"accountSupportsQuotaRefresh": func(account core.Account) bool {
			return s != nil && s.control != nil && s.control.SupportsQuotaRefresh(account)
		},
		"accountUsesManualKey": func(account core.Account) bool {
			method := strings.TrimSpace(account.Credential.Metadata["account_login_method"])
			if method == "" && strings.TrimSpace(account.Credential.Metadata["account_type"]) != "official" {
				method = "api_key"
			}
			return strings.TrimSpace(account.Credential.Mode) == "manual-token" && method == "api_key"
		},
		"quotaWindowSummary": func(window *core.AccountQuotaWindow) string {
			return quotaWindowSummary(window)
		},
		"quotaWindowPercent": func(window *core.AccountQuotaWindow) string {
			return quotaWindowPercent(window)
		},
		"accountFilterStatus": func(account core.Account) string {
			return controlplane.AccountFilterStatus(account)
		},
		"accountRuntimeStatus": func(account core.Account) string {
			return controlplane.AccountRuntimeStatus(account)
		},
		"accountRuntimeStatusText": func(account core.Account) string {
			return accountRuntimeStatusText(locale, controlplane.AccountRuntimeStatus(account))
		},
		"accountRuntimeStatusTone": func(account core.Account) string {
			return accountRuntimeStatusTone(controlplane.AccountRuntimeStatus(account))
		},
		"accountControlDisabled": func(account core.Account) bool {
			return account.ControlDisabled
		},
		"accountControlStatusTone": func(account core.Account) string {
			if account.ControlDisabled {
				return "tone-muted"
			}
			return "tone-good"
		},
		"accountRecoverable": func(account core.Account) bool {
			return controlplane.AccountRecoverable(account)
		},
		"quotaCreditsSummary": func(credits *core.AccountQuotaCredits) string {
			return quotaCreditsSummary(locale, credits)
		},
		"showQuotaImage": func(quota *core.AccountQuotaSnapshot) bool {
			return quota != nil && strings.TrimSpace(quota.Source) != "openai_chatgpt_usage" && quota.Image != nil
		},
		"quotaImageSummary": func(quota *core.AccountImageQuota) string {
			return quotaImageSummary(locale, quota)
		},
		"baseURLForAccount": func(account core.Account) string {
			return strings.TrimSpace(account.Credential.Metadata["base_url"])
		},
		"proxyURLForAccount": func(account core.Account) string {
			return strings.TrimSpace(account.ProxyURL)
		},
		"effectiveProxyURLForAccount": func(account core.Account) string {
			if proxyURL := strings.TrimSpace(account.ProxyURL); proxyURL != "" {
				return proxyURL
			}
			return strings.TrimSpace(account.EffectiveProxyURL)
		},
		"maskProxyURL": maskProxyURL,
		"maskClientKey": func(client core.APIClient) string {
			return maskSecret(client.APIKey)
		},
		"boolStateText": func(enabled bool) string {
			return boolStateText(locale, enabled)
		},
		"boolStateClass": boolStateClass,
		"auditStatusText": func(status string) string {
			return auditStatusText(locale, status)
		},
		"auditStatusClass": auditStatusClass,
		"auditActionText": func(action string) string {
			return auditActionText(locale, action)
		},
		"auditKindText": func(kind core.AuditKind) string {
			return auditKindText(locale, kind)
		},
		"auditSubjectText": func(event core.AuditEvent) string {
			return auditSubjectText(locale, event)
		},
		"auditOperationText": func(event core.AuditEvent) string {
			return auditOperationText(locale, event)
		},
		"auditResourceText": func(event core.AuditEvent) string {
			return auditResourceText(event)
		},
		"auditDetailLines": func(event core.AuditEvent) []auditDetailLine {
			return auditDetailLines(locale, event)
		},
		"auditMessagePreview": auditMessagePreview,
		"auditAttemptPreview": auditAttemptPreview,
		"auditPageURL": func(filter controlplane.AuditFilter, page int) string {
			return auditPageURL(filter, page)
		},
		"usageStatusText": func(status core.BillingRequestStatus) string {
			return usageStatusText(locale, status)
		},
		"monitorStatusText": func(status core.MonitorStatus) string {
			return monitorStatusText(locale, status)
		},
		"monitorStatusClass":       monitorStatusClass,
		"monitorViewStatus":        monitorViewStatus,
		"monitorHistoryBars":       monitorHistoryBars,
		"monitorAdminHistoryTitle": func(result core.MonitorResult) string { return monitorAdminHistoryTitle(locale, result) },
		"monitorAvailabilityClass": monitorAvailabilityClass,
		"monitorNextCheckText":     monitorNextCheckText,
		"monitorNextCheckUnix":     monitorNextCheckUnix,
		"monitorNoticeText": func(notice string) string {
			return monitorNoticeText(locale, notice)
		},
		"usageStatusClass":    usageStatusClass,
		"usageLogModelText":   usageLogModelText,
		"usageFirstTokenText": usageFirstTokenText,
		"usageAccountGroupLabel": func(group string, request core.BillingReservation) string {
			return usageAccountGroupLabel(locale, group, request)
		},
		"usageFailedAccountTitle": func(labels []string) string {
			return usageFailedAccountTitle(locale, labels)
		},
		"usageLogPageURL": func(filter controlplane.UsageLogFilter, page int) string {
			return usageLogPageURL(filter, page)
		},
		"financeUserPageURL": financeUserPageURL,
		"paymentOrderPageURL": func(filter controlplane.PaymentOrderFilter, page int) string {
			return paymentOrderPageURL(filter, page)
		},
		"financeUsageLogPageURL": func(filter controlplane.UsageLogFilter, page int) string {
			return financeUsageLogPageURL(filter, page)
		},
		"formatInteger": formatInteger,
		"accountTodayCalls": func(counts map[string]int, accountID string) int {
			if len(counts) == 0 {
				return 0
			}
			return counts[strings.TrimSpace(accountID)]
		},
		"financePaymentOrderPageURL": func(filter controlplane.PaymentOrderFilter, page int) string {
			return financePaymentOrderPageURL(filter, page)
		},
		"financeReconcileIssueKindText": func(kind string) string {
			return financeReconcileIssueKindText(locale, kind)
		},
		"financeReconcileSeverityText": func(severity string) string {
			return financeReconcileSeverityText(locale, severity)
		},
		"financeReconcileSeverityClass": financeReconcileSeverityClass,
		"paymentProviderText": func(provider core.PaymentProvider) string {
			return paymentProviderText(locale, provider)
		},
		"paymentOrderMethodText": func(order core.PaymentOrder) string {
			return paymentOrderMethodText(locale, order)
		},
		"paymentExchangeRateText": paymentExchangeRateText,
		"paymentOrderTypeText": func(order core.PaymentOrder) string {
			return paymentOrderTypeText(locale, order)
		},
		"paymentStatusText": func(status core.PaymentOrderStatus) string {
			return paymentStatusText(locale, status)
		},
		"paymentStatusClass": paymentStatusClass,
		"paymentRefundStatusText": func(status core.PaymentRefundStatus) string {
			return paymentRefundStatusText(locale, status)
		},
		"paymentRefundStatusClass": paymentRefundStatusClass,
		"usageBillingAmountText": func(request core.BillingReservation) string {
			return usageBillingAmountText(locale, request)
		},
		"datetimeInputValue": datetimeInputValue,
	}
}

func userIdentityMapFromData(data any) map[string]core.User {
	values, ok := data.(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := values["UserIdentityByID"]
	if !ok {
		return nil
	}
	users, ok := raw.(map[string]core.User)
	if !ok {
		return nil
	}
	out := make(map[string]core.User, len(users))
	for id, user := range users {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		out[id] = user
	}
	return out
}

func userConcurrentRequestLimitValue(user core.User) string {
	if user.ConcurrentRequestLimitOverride == nil {
		return ""
	}
	return fmt.Sprint(*user.ConcurrentRequestLimitOverride)
}

func userConcurrentRequestLimitText(locale string, user core.User) string {
	if user.ConcurrentRequestLimitOverride == nil {
		return translate(locale, "inherit_system_default")
	}
	if *user.ConcurrentRequestLimitOverride == 0 {
		return translate(locale, "unlimited")
	}
	return fmt.Sprint(*user.ConcurrentRequestLimitOverride)
}

func userIPConcurrentRequestLimitValue(user core.User) string {
	if user.IPConcurrentRequestLimitOverride == nil {
		return ""
	}
	return fmt.Sprint(*user.IPConcurrentRequestLimitOverride)
}

func userIPConcurrentRequestLimitText(locale string, user core.User) string {
	if user.IPConcurrentRequestLimitOverride == nil {
		return translate(locale, "unlimited")
	}
	if *user.IPConcurrentRequestLimitOverride == 0 {
		return translate(locale, "unlimited")
	}
	return fmt.Sprint(*user.IPConcurrentRequestLimitOverride)
}

func userRequestRateLimitValue(user core.User) string {
	if user.RequestRateLimitPerMinuteOverride == nil {
		return ""
	}
	return fmt.Sprint(*user.RequestRateLimitPerMinuteOverride)
}

func userRequestRateLimitText(locale string, user core.User) string {
	if user.RequestRateLimitPerMinuteOverride == nil {
		return translate(locale, "inherit_system_default")
	}
	if *user.RequestRateLimitPerMinuteOverride == 0 {
		return translate(locale, "unlimited")
	}
	return fmt.Sprint(*user.RequestRateLimitPerMinuteOverride)
}
