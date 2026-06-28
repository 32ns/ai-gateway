package web

import (
	"fmt"
	"hash/fnv"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"
)

type planGroupView struct {
	ID    string
	Name  string
	Plans []core.BillingPlan
}

type planGroupOption struct {
	Value           string
	Label           string
	SaleDisabled    bool
	QuotaPriceRatio string
	SortOrder       int
	Used            bool
	CanDelete       bool
	PlanCount       int
}

type adminPlanTab string

const (
	adminPlanTabPlans         adminPlanTab = "plans"
	adminPlanTabSubscriptions adminPlanTab = "subscriptions"
)

func (s *Server) handlePlansPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/plans" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r, user := s.withOptionalConsoleUser(w, r)
	hasUser := strings.TrimSpace(user.ID) != ""
	locale := resolveLocale(w, r)
	active := core.UserPlanEntitlement{}
	hasActive := false
	activePlans := []core.UserPlanEntitlement(nil)
	entitlements := []core.UserPlanEntitlement(nil)
	if hasUser {
		active, hasActive = activePlanForTemplate(s.control, user.ID)
		entitlements = s.control.ListUserPlanEntitlements(user.ID)
		activePlans = activePlansForTemplate(entitlements)
	}
	plans := s.control.ListBillingPlans(false)
	groups := s.control.ListBillingPlanGroups()
	settings := s.controlCurrentSettings()
	data := withCSRFData(map[string]any{
		"TitleKey":         "page_title_plans",
		"ActiveNav":        "plans",
		"Locale":           locale,
		"Plans":            plans,
		"PlanGroups":       groupBillingPlans(locale, plans, groups),
		"PaymentCNYPerUSD": settings.Payment.CNYPerUSD,
		"ActivePlan":       active,
		"ActivePlans":      activePlans,
		"HasActivePlan":    hasActive,
		"Entitlements":     entitlements,
		"CanPurchase":      hasUser,
		"PlanLoginURL":     "/login?next=%2Fplans",
		"PurchaseNotice":   strings.TrimSpace(r.URL.Query().Get("notice")) == "plan_purchased",
		"PurchaseBlocked":  strings.TrimSpace(r.URL.Query().Get("notice")) == "plan_active",
		"InsufficientCash": strings.TrimSpace(r.URL.Query().Get("notice")) == "insufficient_balance",
	}, r)
	s.render(w, "plans.html", locale, data)
}

func (s *Server) handlePlanPurchase(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/plans/purchase" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, "/plans", err)
		return
	}
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	purchase, err := s.control.PurchaseBillingPlan(user, r.FormValue("plan_id"), planPurchaseModeFromForm(r), r.FormValue("target_entitlement_id"))
	if err != nil {
		switch err {
		case storage.ErrInsufficientBalance:
			http.Redirect(w, r, "/plans?notice=insufficient_balance", http.StatusSeeOther)
		case storage.ErrBillingRequestConflict:
			http.Redirect(w, r, "/plans?notice=plan_active", http.StatusSeeOther)
		default:
			s.redirectWithNoticeError(w, r, "/plans", err)
		}
		return
	}
	s.recordAdminAudit(r, "ok", "plan.purchase", "billing_plan", purchase.Plan.ID, purchase.Plan.Name, fmt.Sprintf("user=%s price=%s quota=%s", user.ID, core.FormatNanoUSD(purchase.Plan.PriceNanoUSD), core.FormatNanoUSD(purchase.Plan.PeriodQuotaNanoUSD)))
	s.publishBalanceUpdated(user.ID)
	s.publishFinanceChanged("plan_purchase", purchase.Plan.ID)
	http.Redirect(w, r, "/plans?notice=plan_purchased", http.StatusSeeOther)
}

func (s *Server) handlePlanEntitlementPriority(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/plans/entitlements/priority" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, "/plans", err)
		return
	}
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if err := s.control.MoveUserPlanEntitlementPriority(user, r.FormValue("entitlement_id"), r.FormValue("direction")); err != nil {
		s.redirectWithNoticeError(w, r, "/plans", err)
		return
	}
	http.Redirect(w, r, "/plans", http.StatusSeeOther)
}

func planPurchaseModeFromForm(r *http.Request) core.BillingPlanPurchaseMode {
	if r == nil {
		return core.BillingPlanPurchaseSeparate
	}
	switch strings.TrimSpace(r.FormValue("purchase_mode")) {
	case string(core.BillingPlanPurchaseMergeQuota):
		return core.BillingPlanPurchaseMergeQuota
	case string(core.BillingPlanPurchaseExtendPeriod):
		return core.BillingPlanPurchaseExtendPeriod
	default:
		return core.BillingPlanPurchaseSeparate
	}
}

func planGrantTargetRefs(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ',', ';', '\n', '\r', '\t', ' ':
			return true
		default:
			return false
		}
	})
	out := make([]string, 0, len(fields))
	seen := map[string]struct{}{}
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		key := strings.ToLower(field)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, field)
	}
	return out
}

func (s *Server) handleAdminPlansPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/plans" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		locale := resolveLocale(w, r)
		tab := normalizeAdminPlanTab(r.URL.Query().Get("tab"))
		plans := s.control.ListBillingPlans(true)
		groups := s.control.ListBillingPlanGroups()
		planGroupOptions := planGroupOptions(locale, groups, plans)
		activeGroupKey := activeAdminPlanGroupKey(r, planGroupOptions)
		data := withCSRFData(map[string]any{
			"TitleKey":             "page_title_admin_plans",
			"ActiveNav":            "plans",
			"Locale":               locale,
			"AdminPlanTab":         string(tab),
			"Plans":                plans,
			"PlanGroups":           groupBillingPlans(locale, plans, groups),
			"BillingPlanGroups":    planGroupOptions,
			"ActivePlanGroupKey":   activeGroupKey,
			"ActivePlanGroup":      activePlanGroupOption(activeGroupKey, planGroupOptions),
			"ActivePlanGroupPlans": plansForAdminPlanGroup(plans, activeGroupKey),
			"PlanSubscriptions":    s.adminPlanSubscriptionsPage(r),
			"PlanGrantNotice":      strings.TrimSpace(r.URL.Query().Get("notice")) == "plan_granted",
			"PlanCancelNotice":     strings.TrimSpace(r.URL.Query().Get("notice")) == "plan_cancelled",
		}, r)
		s.render(w, "admin_plans.html", locale, data)
	case http.MethodPost:
		s.handleAdminPlanUpsert(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) adminPlanSubscriptionsPage(r *http.Request) controlplane.PlanSubscriptionsPage {
	query := r.URL.Query()
	status := core.UserPlanEntitlementActive
	if query.Has("subscription_status") {
		status = core.UserPlanEntitlementStatus(strings.TrimSpace(query.Get("subscription_status")))
	}
	return s.control.PlanSubscriptionsPage(controlplane.PlanSubscriptionFilter{
		UserID:   strings.TrimSpace(query.Get("subscription_user_id")),
		PlanID:   strings.TrimSpace(query.Get("subscription_plan_id")),
		Status:   status,
		Page:     parsePositiveInt(query.Get("subscription_page"), 1),
		PageSize: 25,
	})
}

func (s *Server) handleAdminPlanGrant(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/plans/grant" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, adminPlanTabURL(string(adminPlanTabSubscriptions)), err)
		return
	}
	actor, _ := currentUserFromContext(r.Context())
	result, err := s.control.GrantBillingPlan(controlplane.BillingPlanGrantInput{
		PlanID:          r.FormValue("plan_id"),
		TargetAll:       planGrantTargetAllFromForm(r),
		TargetPaidUsers: planGrantTargetPaidUsersFromForm(r),
		TargetUserRefs:  planGrantTargetRefsFromForm(r),
		Note:            r.FormValue("note"),
		ActorID:         actor.ID,
	})
	if err != nil {
		s.recordAdminAudit(r, "error", "plan.grant", "billing_plan", strings.TrimSpace(r.FormValue("plan_id")), "", err.Error())
		s.redirectWithNoticeError(w, r, adminPlanTabURL(string(adminPlanTabSubscriptions)), err)
		return
	}
	message := fmt.Sprintf("targets=%d granted=%d skipped=%d failed=%d notify_failed=%d", result.TotalTargets, len(result.Granted), len(result.SkippedRefs), len(result.FailedRefs), len(result.NotifyFailed))
	s.recordAdminAudit(r, "ok", "plan.grant", "billing_plan", result.Plan.ID, result.Plan.Name, message)
	s.publishFinanceChanged("plan_grant", result.Plan.ID)
	http.Redirect(w, r, "/admin/plans?tab=subscriptions&notice=plan_granted", http.StatusSeeOther)
}

func (s *Server) handleAdminPlanEntitlementCancel(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/plans/entitlements/cancel" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, adminPlanTabURL(string(adminPlanTabSubscriptions)), err)
		return
	}
	userID := strings.TrimSpace(r.FormValue("user_id"))
	entitlementID := strings.TrimSpace(r.FormValue("entitlement_id"))
	entitlement, err := s.control.CancelUserPlanEntitlement(userID, entitlementID)
	if err != nil {
		s.recordAdminAudit(r, "error", "plan.entitlement.cancel", "billing_plan_entitlement", entitlementID, "", err.Error())
		s.redirectWithNoticeError(w, r, adminPlanTabURL(string(adminPlanTabSubscriptions)), err)
		return
	}
	s.recordAdminAudit(r, "ok", "plan.entitlement.cancel", "billing_plan_entitlement", entitlement.ID, entitlement.PlanName, "user="+entitlement.UserID)
	s.publishFinanceChanged("plan_cancel", entitlement.ID)
	http.Redirect(w, r, "/admin/plans?tab=subscriptions&notice=plan_cancelled", http.StatusSeeOther)
}

func planGrantTargetAllFromForm(r *http.Request) bool {
	if r == nil {
		return false
	}
	return strings.TrimSpace(r.FormValue("target_mode")) == "all" || strings.TrimSpace(r.FormValue("target_scope")) == "all"
}

func planGrantTargetPaidUsersFromForm(r *http.Request) bool {
	if r == nil {
		return false
	}
	return strings.TrimSpace(r.FormValue("target_mode")) == "paid"
}

func planGrantTargetRefsFromForm(r *http.Request) []string {
	if r == nil {
		return nil
	}
	if refs := r.Form["target_user_id"]; len(refs) > 0 {
		return refs
	}
	return planGrantTargetRefs(r.FormValue("target_users"))
}

func (s *Server) handleAdminPlanActions(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/admin/plans/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "delete" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	plan, err := s.control.DeleteBillingPlan(parts[0])
	if err != nil {
		s.recordAdminAudit(r, "error", "plan.delete", "billing_plan", parts[0], "", err.Error())
		s.redirectWithNoticeError(w, r, adminPlansGroupHref(planGroupReturnKey(r)), err)
		return
	}
	s.recordAdminAudit(r, "ok", "plan.delete", "billing_plan", plan.ID, plan.Name, "")
	http.Redirect(w, r, adminPlansGroupHref(plan.Group), http.StatusSeeOther)
}

func (s *Server) handleAdminPlanUpsert(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, adminPlansGroupHref(planGroupReturnKey(r)), err)
		return
	}
	quota, err := parseNanoUSDFormValue(r, "period_quota_usd")
	if err != nil {
		s.redirectWithNoticeError(w, r, adminPlansGroupHref(planGroupReturnKey(r)), err)
		return
	}
	price, err := parseNanoUSDFormValue(r, "price_usd")
	if err != nil {
		s.redirectWithNoticeError(w, r, adminPlansGroupHref(planGroupReturnKey(r)), err)
		return
	}
	durationHours := parsePositiveInt(r.FormValue("period_duration_hours"), 24)
	periodCount := parsePositiveInt(r.FormValue("period_count"), 1)
	sortOrder, err := strconv.Atoi(strings.TrimSpace(r.FormValue("sort_order")))
	if err != nil {
		sortOrder = 0
	}
	planID := strings.TrimSpace(r.FormValue("id"))
	if planID != "" {
		if _, err := s.control.GetBillingPlan(planID); err != nil {
			s.recordAdminAudit(r, "error", "plan.upsert", "billing_plan", planID, strings.TrimSpace(r.FormValue("name")), err.Error())
			s.redirectWithNoticeError(w, r, adminPlansGroupHref(planGroupReturnKey(r)), err)
			return
		}
	}
	input := controlplane.BillingPlanInput{
		ID:                 planID,
		Name:               r.FormValue("name"),
		Description:        r.FormValue("description"),
		Group:              r.FormValue("group"),
		Enabled:            r.FormValue("enabled") == "on",
		PriceNanoUSD:       price,
		PeriodQuotaNanoUSD: quota,
		PeriodDurationSec:  int64(durationHours) * 60 * 60,
		PeriodCount:        periodCount,
		SortOrder:          sortOrder,
	}
	plan, err := s.control.UpsertBillingPlan(input)
	if err != nil {
		s.recordAdminAudit(r, "error", "plan.upsert", "billing_plan", strings.TrimSpace(input.ID), strings.TrimSpace(input.Name), err.Error())
		s.redirectWithNoticeError(w, r, adminPlansGroupHref(planGroupReturnKey(r)), err)
		return
	}
	s.recordAdminAudit(r, "ok", "plan.upsert", "billing_plan", plan.ID, plan.Name, fmt.Sprintf("enabled=%t price=%s quota=%s periods=%d", plan.Enabled, core.FormatNanoUSD(plan.PriceNanoUSD), core.FormatNanoUSD(plan.PeriodQuotaNanoUSD), plan.PeriodCount))
	http.Redirect(w, r, adminPlansGroupHref(plan.Group), http.StatusSeeOther)
}

func (s *Server) handleAdminPlanGroupsCreate(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/plan-groups" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, adminPlansGroupHref(planGroupReturnKey(r)), err)
		return
	}
	sortOrder, err := strconv.Atoi(strings.TrimSpace(r.FormValue("sort_order")))
	if err != nil {
		sortOrder = 0
	}
	group, err := s.control.CreateBillingPlanGroup(controlplane.BillingPlanGroupInput{
		Name:            r.FormValue("name"),
		QuotaPriceRatio: r.FormValue("quota_price_ratio"),
		SortOrder:       sortOrder,
	})
	if err != nil {
		s.recordAdminAudit(r, "error", "plan_group.create", "billing_plan_group", "", strings.TrimSpace(r.FormValue("name")), err.Error())
		s.redirectWithNoticeError(w, r, adminPlansGroupHref(planGroupReturnKey(r)), err)
		return
	}
	s.recordAdminAudit(r, "ok", "plan_group.create", "billing_plan_group", group.ID, group.Name, fmt.Sprintf("sort_order=%d ratio=%s", group.SortOrder, group.QuotaPriceRatio))
	http.Redirect(w, r, adminPlansGroupHref(group.ID), http.StatusSeeOther)
}

func (s *Server) handleAdminPlanGroupActions(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/admin/plan-groups/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) != 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, adminPlansGroupHref(planGroupReturnKey(r)), err)
		return
	}
	switch parts[1] {
	case "update":
		sortOrder, err := strconv.Atoi(strings.TrimSpace(r.FormValue("sort_order")))
		if err != nil {
			sortOrder = 0
		}
		group, err := s.control.UpdateBillingPlanGroup(parts[0], controlplane.BillingPlanGroupInput{
			Name:            r.FormValue("name"),
			SaleDisabled:    r.FormValue("sale_enabled") != "on",
			QuotaPriceRatio: r.FormValue("quota_price_ratio"),
			SortOrder:       sortOrder,
		})
		if err != nil {
			s.recordAdminAudit(r, "error", "plan_group.update", "billing_plan_group", parts[0], strings.TrimSpace(r.FormValue("name")), err.Error())
			s.redirectWithNoticeError(w, r, adminPlansGroupHref(planGroupReturnKey(r)), err)
			return
		}
		s.recordAdminAudit(r, "ok", "plan_group.update", "billing_plan_group", group.ID, group.Name, fmt.Sprintf("sale_enabled=%t sort_order=%d ratio=%s", !group.SaleDisabled, group.SortOrder, group.QuotaPriceRatio))
		http.Redirect(w, r, adminPlansGroupHref(group.ID), http.StatusSeeOther)
	case "delete":
		group, err := s.control.DeleteBillingPlanGroup(parts[0])
		if err != nil {
			s.recordAdminAudit(r, "error", "plan_group.delete", "billing_plan_group", parts[0], "", err.Error())
			s.redirectWithNoticeError(w, r, adminPlansGroupHref(planGroupReturnKey(r)), err)
			return
		}
		s.recordAdminAudit(r, "ok", "plan_group.delete", "billing_plan_group", group.ID, group.Name, "")
		http.Redirect(w, r, "/admin/plans", http.StatusSeeOther)
	default:
		http.NotFound(w, r)
		return
	}
}

func activePlanForTemplate(service *controlplane.Service, userID string) (core.UserPlanEntitlement, bool) {
	if service == nil {
		return core.UserPlanEntitlement{}, false
	}
	entitlement, err := service.GetActiveUserPlanEntitlement(userID)
	return entitlement, err == nil
}

func activePlansForTemplate(entitlements []core.UserPlanEntitlement) []core.UserPlanEntitlement {
	out := make([]core.UserPlanEntitlement, 0)
	for _, entitlement := range entitlements {
		if entitlement.Status == core.UserPlanEntitlementActive {
			out = append(out, entitlement)
		}
	}
	return out
}

func activePlansHasPlanID(entitlements []core.UserPlanEntitlement, planID string) bool {
	planID = strings.TrimSpace(planID)
	if planID == "" {
		return false
	}
	for _, entitlement := range entitlements {
		if entitlement.Status == core.UserPlanEntitlementActive && strings.TrimSpace(entitlement.PlanID) == planID {
			return true
		}
	}
	return false
}

func activePlansForPlanID(entitlements []core.UserPlanEntitlement, planID string) []core.UserPlanEntitlement {
	planID = strings.TrimSpace(planID)
	if planID == "" {
		return nil
	}
	out := make([]core.UserPlanEntitlement, 0)
	for _, entitlement := range entitlements {
		if entitlement.Status == core.UserPlanEntitlementActive && strings.TrimSpace(entitlement.PlanID) == planID {
			out = append(out, entitlement)
		}
	}
	return out
}

func planActionURL(id string, action string) string {
	return "/admin/plans/" + url.PathEscape(strings.TrimSpace(id)) + "/" + strings.Trim(strings.TrimSpace(action), "/")
}

func planDialogID(id string) string {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(strings.TrimSpace(id)))
	return "plan-edit-" + strconv.FormatUint(hash.Sum64(), 16)
}

func planSubscriptionUsageDialogID(id string) string {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(strings.TrimSpace(id)))
	return "plan-usage-" + strconv.FormatUint(hash.Sum64(), 16)
}

func planGroupActionURL(id string, action string) string {
	return "/admin/plan-groups/" + url.PathEscape(strings.TrimSpace(id)) + "/" + strings.Trim(strings.TrimSpace(action), "/")
}

func adminPlansGroupHref(groupKey string) string {
	groupKey = core.NormalizeBillingPlanGroup(groupKey)
	values := url.Values{}
	values.Set("tab", string(adminPlanTabPlans))
	if groupKey == "" {
		return "/admin/plans?" + values.Encode()
	}
	values.Set("group", groupKey)
	return "/admin/plans?" + values.Encode()
}

func adminPlanTabURL(tab string) string {
	tab = string(normalizeAdminPlanTab(tab))
	if tab == string(adminPlanTabSubscriptions) {
		return "/admin/plans"
	}
	values := url.Values{}
	values.Set("tab", tab)
	return "/admin/plans?" + values.Encode()
}

func planSubscriptionPageURL(filter controlplane.PlanSubscriptionFilter, page int) string {
	if page < 1 {
		page = 1
	}
	values := url.Values{}
	values.Set("tab", string(adminPlanTabSubscriptions))
	if userID := strings.TrimSpace(filter.UserID); userID != "" {
		values.Set("subscription_user_id", userID)
	}
	if planID := strings.TrimSpace(filter.PlanID); planID != "" {
		values.Set("subscription_plan_id", planID)
	}
	if status := strings.TrimSpace(string(filter.Status)); status != "" {
		values.Set("subscription_status", status)
	}
	if page > 1 {
		values.Set("subscription_page", strconv.Itoa(page))
	}
	return "/admin/plans?" + values.Encode()
}

func normalizeAdminPlanTab(raw string) adminPlanTab {
	switch strings.TrimSpace(raw) {
	case string(adminPlanTabPlans):
		return adminPlanTabPlans
	case string(adminPlanTabSubscriptions):
		return adminPlanTabSubscriptions
	default:
		return adminPlanTabSubscriptions
	}
}

func planGroupReturnKey(r *http.Request) string {
	if r == nil {
		return ""
	}
	if key := strings.TrimSpace(r.FormValue("current_group")); key != "" {
		return core.NormalizeBillingPlanGroup(key)
	}
	return core.NormalizeBillingPlanGroup(r.URL.Query().Get("group"))
}

func activeAdminPlanGroupKey(r *http.Request, groups []planGroupOption) string {
	requested := planGroupReturnKey(r)
	for _, group := range groups {
		if requested != "" && strings.EqualFold(group.Value, requested) {
			return group.Value
		}
	}
	if len(groups) > 0 {
		return groups[0].Value
	}
	return ""
}

func activePlanGroupOption(groupKey string, groups []planGroupOption) planGroupOption {
	for _, group := range groups {
		if strings.EqualFold(group.Value, groupKey) {
			return group
		}
	}
	return planGroupOption{}
}

func plansForAdminPlanGroup(plans []core.BillingPlan, groupKey string) []core.BillingPlan {
	groupKey = core.NormalizeBillingPlanGroup(groupKey)
	if groupKey == "" {
		return nil
	}
	out := make([]core.BillingPlan, 0)
	for _, plan := range plans {
		planGroup := core.NormalizeBillingPlanGroup(plan.Group)
		if planGroup == "" {
			continue
		}
		if strings.EqualFold(planGroup, groupKey) {
			out = append(out, plan)
		}
	}
	return out
}

func groupBillingPlans(locale string, plans []core.BillingPlan, groupDefs []core.BillingPlanGroup) []planGroupView {
	plansByGroup := map[string][]core.BillingPlan{}
	for _, plan := range plans {
		groupID := core.NormalizeBillingPlanGroup(plan.Group)
		if groupID == "" {
			continue
		}
		plansByGroup[groupID] = append(plansByGroup[groupID], plan)
	}
	out := make([]planGroupView, 0, len(plansByGroup))
	for _, group := range groupDefs {
		groupID := core.NormalizeBillingPlanGroup(group.ID)
		if groupID == "" {
			groupID = core.NormalizeBillingPlanGroup(group.Name)
		}
		if groupID == "" {
			continue
		}
		groupPlans := plansByGroup[groupID]
		if len(groupPlans) == 0 {
			continue
		}
		out = append(out, planGroupView{
			ID:    groupID,
			Name:  planGroupName(locale, group),
			Plans: groupPlans,
		})
	}
	return out
}

func planGroupOptions(locale string, groups []core.BillingPlanGroup, plans []core.BillingPlan) []planGroupOption {
	usage := map[string]int{}
	for _, plan := range plans {
		groupID := core.NormalizeBillingPlanGroup(plan.Group)
		if groupID == "" {
			continue
		}
		usage[groupID]++
	}
	out := make([]planGroupOption, 0, len(groups))
	for _, group := range groups {
		groupID := core.NormalizeBillingPlanGroup(group.ID)
		if groupID == "" {
			groupID = core.NormalizeBillingPlanGroup(group.Name)
		}
		if groupID == "" {
			continue
		}
		count := usage[groupID]
		out = append(out, planGroupOption{
			Value:           groupID,
			Label:           planGroupName(locale, group),
			SaleDisabled:    group.SaleDisabled,
			QuotaPriceRatio: group.QuotaPriceRatio,
			SortOrder:       group.SortOrder,
			Used:            count > 0,
			CanDelete:       !group.CreatedAt.IsZero() && count == 0,
			PlanCount:       count,
		})
	}
	return out
}

func planGroupName(locale string, group core.BillingPlanGroup) string {
	groupID := core.NormalizeBillingPlanGroup(group.ID)
	if groupID == "" {
		groupID = core.NormalizeBillingPlanGroup(group.Name)
	}
	if strings.TrimSpace(group.Name) != "" {
		return strings.TrimSpace(group.Name)
	}
	return strings.TrimSpace(groupID)
}

func planGroupDisplayName(groupID string, groups []planGroupOption) string {
	normalized := core.NormalizeBillingPlanGroup(groupID)
	if normalized == "" {
		return ""
	}
	for _, group := range groups {
		if strings.EqualFold(group.Value, normalized) && strings.TrimSpace(group.Label) != "" {
			return strings.TrimSpace(group.Label)
		}
	}
	return strings.TrimSpace(groupID)
}

func planQuotaUsed(entitlement core.UserPlanEntitlement) int64 {
	limit := planCurrentQuotaLimit(entitlement)
	if limit <= 0 {
		return 0
	}
	return limit - planQuotaRemaining(entitlement)
}

func planQuotaUsedPercent(entitlement core.UserPlanEntitlement) int {
	limit := planCurrentQuotaLimit(entitlement)
	if limit <= 0 {
		return 0
	}
	used := planQuotaUsed(entitlement)
	percent := int((float64(used) / float64(limit)) * 100)
	if percent < 0 {
		return 0
	}
	if percent > 100 {
		return 100
	}
	return percent
}

func planQuotaRemainingPercent(entitlement core.UserPlanEntitlement) int {
	limit := planCurrentQuotaLimit(entitlement)
	if limit <= 0 {
		return 0
	}
	percent := int((float64(planQuotaRemaining(entitlement)) / float64(limit)) * 100)
	if percent < 0 {
		return 0
	}
	if percent > 100 {
		return 100
	}
	return percent
}

func planQuotaRemaining(entitlement core.UserPlanEntitlement) int64 {
	limit := planCurrentQuotaLimit(entitlement)
	remaining := entitlement.CurrentQuotaNanoUSD
	if remaining < 0 {
		return 0
	}
	if limit > 0 && remaining > limit {
		return limit
	}
	return remaining
}

func planCurrentQuotaLimit(entitlement core.UserPlanEntitlement) int64 {
	limit := entitlement.PeriodQuotaNanoUSD
	if entitlement.CurrentQuotaNanoUSD > limit {
		limit = entitlement.CurrentQuotaNanoUSD
	}
	if limit < 0 {
		return 0
	}
	return limit
}

func planEntitlementStatusText(locale string, status core.UserPlanEntitlementStatus) string {
	switch status {
	case core.UserPlanEntitlementActive:
		return translate(locale, "plan_status_active")
	case core.UserPlanEntitlementExpired:
		return translate(locale, "plan_status_expired")
	case core.UserPlanEntitlementCancelled:
		return translate(locale, "plan_status_cancelled")
	default:
		value := strings.TrimSpace(string(status))
		if value == "" {
			return translate(locale, "unknown")
		}
		return value
	}
}

func planEntitlementStatusClass(status core.UserPlanEntitlementStatus) string {
	switch status {
	case core.UserPlanEntitlementActive:
		return "tone-good"
	case core.UserPlanEntitlementExpired:
		return "tone-muted"
	case core.UserPlanEntitlementCancelled:
		return "tone-bad"
	default:
		return "tone-muted"
	}
}
