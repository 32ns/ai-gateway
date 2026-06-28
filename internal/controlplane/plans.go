package controlplane

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"
)

type BillingPlanInput struct {
	ID                 string
	Name               string
	Description        string
	Group              string
	Enabled            bool
	PriceNanoUSD       int64
	PeriodQuotaNanoUSD int64
	PeriodDurationSec  int64
	PeriodCount        int
	SortOrder          int
}

type BillingPlanGroupInput struct {
	ID              string
	Name            string
	SaleDisabled    bool
	QuotaPriceRatio string
	SortOrder       int
}

type PlanSubscriptionFilter struct {
	UserID   string
	PlanID   string
	Status   core.UserPlanEntitlementStatus
	Page     int
	PageSize int
}

type PlanSubscriptionPageInfo struct {
	Total    int
	Page     int
	PageSize int
	HasPrev  bool
	PrevPage int
	HasNext  bool
	NextPage int
}

type PlanSubscriptionSummary struct {
	Entitlement        core.UserPlanEntitlement
	Username           string
	PlanGroup          string
	UserBalanceNanoUSD int64
	UsageDetails       PlanSubscriptionUsageDetails
}

type PlanSubscriptionStats struct {
	TotalCount             int
	ActiveCount            int
	ExpiredCount           int
	CancelledCount         int
	RevenueNanoUSD         int64
	ActiveRemainingNanoUSD int64
	ActiveUsedNanoUSD      int64
}

type PlanSubscriptionPlanSummary struct {
	PlanID                 string
	PlanName               string
	PlanGroup              string
	PurchaseCount          int
	ActiveCount            int
	RevenueNanoUSD         int64
	ActiveRemainingNanoUSD int64
}

type PlanSubscriptionsPage struct {
	Filter        PlanSubscriptionFilter
	Stats         PlanSubscriptionStats
	PlanSummaries []PlanSubscriptionPlanSummary
	Rows          []PlanSubscriptionSummary
	Page          PlanSubscriptionPageInfo
}

type PlanQuotaUsageSummary struct {
	PeriodStartedAt time.Time
	PeriodEndsAt    time.Time
	UserID          string
	Username        string
	PlanID          string
	PlanName        string
	EntitlementID   string
	GrantedNanoUSD  int64
	UsedNanoUSD     int64
	ReturnedNanoUSD int64
	ExpiredNanoUSD  int64
	NetNanoUSD      int64
}

type PlanSubscriptionUsageDetails struct {
	UserID                        string
	Username                      string
	EntitlementID                 string
	PlanName                      string
	CurrentPeriodQuotaNanoUSD     int64
	CurrentPeriodRemainingNanoUSD int64
	CurrentPeriodUsedNanoUSD      int64
	CurrentPeriodStartedAt        time.Time
	CurrentPeriodEndsAt           time.Time
	Stats                         PlanQuotaUsageStats
	Rows                          []PlanQuotaUsageSummary
}

type PlanQuotaUsageStats struct {
	GrantedNanoUSD  int64
	UsedNanoUSD     int64
	ReturnedNanoUSD int64
	ExpiredNanoUSD  int64
	NetNanoUSD      int64
}

type BillingPlanGrantInput struct {
	PlanID          string
	TargetAll       bool
	TargetPaidUsers bool
	TargetUserRefs  []string
	Note            string
	ActorID         string
	IncludeDisabled bool
}

type BillingPlanGrantResult struct {
	Plan         core.BillingPlan
	Granted      []core.BillingPlanPurchase
	SkippedRefs  []string
	FailedRefs   []string
	NotifyFailed []string
	TargetedAll  bool
	TotalTargets int
}

func (s *Service) ListBillingPlans(admin bool) []core.BillingPlan {
	plans := s.repo.ListBillingPlans()
	if admin {
		return plans
	}
	saleableGroups := s.saleableBillingPlanGroups()
	out := make([]core.BillingPlan, 0, len(plans))
	for _, plan := range plans {
		if plan.Enabled && saleableGroups[core.NormalizeBillingPlanGroup(plan.Group)] {
			out = append(out, plan)
		}
	}
	return out
}

func (s *Service) GetBillingPlan(id string) (core.BillingPlan, error) {
	return s.repo.GetBillingPlan(strings.TrimSpace(id))
}

func (s *Service) ListBillingPlanGroups() []core.BillingPlanGroup {
	groupsByID := map[string]core.BillingPlanGroup{}
	for _, group := range s.repo.ListBillingPlanGroups() {
		group = normalizeControlBillingPlanGroup(group)
		if group.ID == "" || group.Name == "" {
			continue
		}
		groupsByID[group.ID] = group
	}
	groups := make([]core.BillingPlanGroup, 0, len(groupsByID))
	for _, group := range groupsByID {
		groups = append(groups, group)
	}
	return sortControlBillingPlanGroups(groups)
}

func (s *Service) GetBillingPlanGroup(id string) (core.BillingPlanGroup, error) {
	id = core.NormalizeBillingPlanGroup(id)
	if id == "" {
		return core.BillingPlanGroup{}, storage.ErrNotFound
	}
	for _, group := range s.ListBillingPlanGroups() {
		if group.ID == id {
			return group, nil
		}
	}
	return core.BillingPlanGroup{}, storage.ErrNotFound
}

func (s *Service) CreateBillingPlanGroup(input BillingPlanGroupInput) (core.BillingPlanGroup, error) {
	ratio, err := normalizeBillingPlanGroupInputRatio(input.QuotaPriceRatio)
	if err != nil {
		return core.BillingPlanGroup{}, err
	}
	group := core.BillingPlanGroup{
		ID:              strings.TrimSpace(input.ID),
		Name:            strings.TrimSpace(input.Name),
		SaleDisabled:    input.SaleDisabled,
		QuotaPriceRatio: ratio,
		SortOrder:       input.SortOrder,
	}
	if group.Name == "" {
		return core.BillingPlanGroup{}, fmt.Errorf("plan group name is required")
	}
	if group.ID == "" {
		id, err := s.generateBillingPlanGroupID(group.Name)
		if err != nil {
			return core.BillingPlanGroup{}, err
		}
		group.ID = id
	}
	group = normalizeControlBillingPlanGroup(group)
	if group.ID == "" || group.Name == "" {
		return core.BillingPlanGroup{}, fmt.Errorf("plan group name is required")
	}
	if strings.ContainsAny(group.ID, "/\\\x00") {
		return core.BillingPlanGroup{}, fmt.Errorf("plan group id contains invalid characters")
	}
	if _, err := s.repo.GetBillingPlanGroup(group.ID); err == nil {
		return core.BillingPlanGroup{}, fmt.Errorf("plan group already exists")
	} else if !errors.Is(err, storage.ErrNotFound) {
		return core.BillingPlanGroup{}, err
	}
	if err := s.repo.UpsertBillingPlanGroup(group); err != nil {
		return core.BillingPlanGroup{}, err
	}
	return s.repo.GetBillingPlanGroup(group.ID)
}

func (s *Service) UpdateBillingPlanGroup(id string, input BillingPlanGroupInput) (core.BillingPlanGroup, error) {
	group, err := s.GetBillingPlanGroup(id)
	if err != nil {
		return core.BillingPlanGroup{}, err
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return core.BillingPlanGroup{}, fmt.Errorf("plan group name is required")
	}
	group.Name = name
	group.SaleDisabled = input.SaleDisabled
	if strings.TrimSpace(input.QuotaPriceRatio) != "" {
		ratio, err := normalizeBillingPlanGroupInputRatio(input.QuotaPriceRatio)
		if err != nil {
			return core.BillingPlanGroup{}, err
		}
		group.QuotaPriceRatio = ratio
	}
	group.SortOrder = input.SortOrder
	group = normalizeControlBillingPlanGroup(group)
	if err := s.repo.UpsertBillingPlanGroup(group); err != nil {
		return core.BillingPlanGroup{}, err
	}
	return s.repo.GetBillingPlanGroup(group.ID)
}

func (s *Service) DeleteBillingPlanGroup(id string) (core.BillingPlanGroup, error) {
	group, err := s.GetBillingPlanGroup(id)
	if err != nil {
		return core.BillingPlanGroup{}, err
	}
	for _, plan := range s.repo.ListBillingPlans() {
		if core.NormalizeBillingPlanGroup(plan.Group) == group.ID {
			return core.BillingPlanGroup{}, fmt.Errorf("plan group is used by existing plans")
		}
	}
	if err := s.repo.DeleteBillingPlanGroup(group.ID); err != nil {
		return core.BillingPlanGroup{}, err
	}
	return group, nil
}

func (s *Service) UpsertBillingPlan(input BillingPlanInput) (core.BillingPlan, error) {
	plan := core.BillingPlan{
		ID:                 strings.TrimSpace(input.ID),
		Name:               strings.TrimSpace(input.Name),
		Description:        strings.TrimSpace(input.Description),
		Group:              strings.TrimSpace(input.Group),
		Enabled:            input.Enabled,
		PriceNanoUSD:       input.PriceNanoUSD,
		PeriodQuotaNanoUSD: input.PeriodQuotaNanoUSD,
		PeriodDurationSec:  input.PeriodDurationSec,
		PeriodCount:        input.PeriodCount,
		SortOrder:          input.SortOrder,
	}
	if plan.Name == "" {
		return core.BillingPlan{}, fmt.Errorf("plan name is required")
	}
	groupID := core.NormalizeBillingPlanGroup(plan.Group)
	if groupID == "" {
		return core.BillingPlan{}, fmt.Errorf("plan group is required")
	}
	group, err := s.GetBillingPlanGroup(groupID)
	if err != nil {
		return core.BillingPlan{}, fmt.Errorf("plan group does not exist")
	}
	plan.Group = group.ID
	if plan.ID == "" {
		id, err := s.generateBillingPlanID()
		if err != nil {
			return core.BillingPlan{}, err
		}
		plan.ID = id
	} else {
		if _, err := s.repo.GetBillingPlan(plan.ID); err != nil {
			return core.BillingPlan{}, err
		}
	}
	if strings.ContainsAny(plan.ID, "/\\\x00") {
		return core.BillingPlan{}, fmt.Errorf("plan id contains invalid characters")
	}
	if plan.PriceNanoUSD < 0 || plan.PeriodQuotaNanoUSD < 0 {
		return core.BillingPlan{}, fmt.Errorf("plan amounts must be non-negative")
	}
	if plan.PeriodDurationSec <= 0 {
		plan.PeriodDurationSec = 24 * 60 * 60
	}
	if plan.PeriodCount <= 0 {
		plan.PeriodCount = 1
	}
	if err := s.repo.UpsertBillingPlan(plan); err != nil {
		return core.BillingPlan{}, err
	}
	return s.repo.GetBillingPlan(plan.ID)
}

func (s *Service) generateBillingPlanGroupID(name string) (string, error) {
	base := billingPlanGroupIDBase(name)
	for index := 0; index < 8; index++ {
		id := base
		if index > 0 {
			suffix, err := randomHex(3)
			if err != nil {
				return "", err
			}
			id = base + "_" + suffix
		}
		if _, err := s.GetBillingPlanGroup(id); errors.Is(err, storage.ErrNotFound) {
			return id, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("failed to generate unique plan group id")
}

func billingPlanGroupIDBase(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var builder strings.Builder
	lastSep := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
			lastSep = false
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastSep = false
		default:
			if !lastSep && builder.Len() > 0 {
				builder.WriteByte('_')
				lastSep = true
			}
		}
	}
	value := strings.Trim(builder.String(), "_")
	if value == "" {
		value = "custom"
	}
	if len(value) > 40 {
		value = strings.Trim(value[:40], "_")
	}
	return "group_" + value
}

func (s *Service) generateBillingPlanID() (string, error) {
	for range 8 {
		suffix, err := randomHex(6)
		if err != nil {
			return "", err
		}
		id := "plan_" + suffix
		if _, err := s.repo.GetBillingPlan(id); errors.Is(err, storage.ErrNotFound) {
			return id, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("failed to generate unique plan id")
}

func (s *Service) DeleteBillingPlan(id string) (core.BillingPlan, error) {
	id = strings.TrimSpace(id)
	plan, err := s.repo.GetBillingPlan(id)
	if err != nil {
		return core.BillingPlan{}, err
	}
	if err := s.repo.DeleteBillingPlan(id); err != nil {
		return core.BillingPlan{}, err
	}
	return plan, nil
}

func (s *Service) PurchaseBillingPlan(user core.User, planID string, mode core.BillingPlanPurchaseMode, targetEntitlementID string) (core.BillingPlanPurchase, error) {
	if strings.TrimSpace(user.ID) == "" {
		return core.BillingPlanPurchase{}, storage.ErrNotFound
	}
	plan, err := s.repo.GetBillingPlan(strings.TrimSpace(planID))
	if err != nil {
		return core.BillingPlanPurchase{}, err
	}
	groupID := core.NormalizeBillingPlanGroup(plan.Group)
	if !plan.Enabled || (groupID != "" && !s.billingPlanGroupSaleEnabled(groupID)) {
		return core.BillingPlanPurchase{}, storage.ErrNotFound
	}
	return s.repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{
		UserID:              user.ID,
		PlanID:              strings.TrimSpace(planID),
		Mode:                mode,
		TargetEntitlementID: strings.TrimSpace(targetEntitlementID),
	})
}

func (s *Service) GrantBillingPlan(input BillingPlanGrantInput) (BillingPlanGrantResult, error) {
	planID := strings.TrimSpace(input.PlanID)
	if planID == "" {
		return BillingPlanGrantResult{}, fmt.Errorf("plan is required")
	}
	plan, err := s.GetBillingPlan(planID)
	if err != nil {
		return BillingPlanGrantResult{}, err
	}
	if !plan.Enabled {
		return BillingPlanGrantResult{}, fmt.Errorf("plan is disabled")
	}
	skipped := []string{}
	var targets []core.User
	if input.TargetAll {
		targets = s.grantBillingPlanAllUsers(input.IncludeDisabled)
	} else if input.TargetPaidUsers {
		targets = s.grantBillingPlanPaidUsers(input.IncludeDisabled)
	} else {
		targets, skipped = s.resolveBillingPlanGrantUsers(input.TargetUserRefs, input.IncludeDisabled)
	}
	if len(targets) == 0 {
		return BillingPlanGrantResult{Plan: plan, SkippedRefs: skipped, TargetedAll: input.TargetAll}, fmt.Errorf("no target users found")
	}
	granted := make([]core.BillingPlanPurchase, 0, len(targets))
	failed := make([]string, 0)
	notifyFailed := make([]string, 0)
	for _, user := range targets {
		purchase, err := s.repo.GrantBillingPlan(core.BillingPlanGrantInput{
			UserID: user.ID,
			PlanID: plan.ID,
			Note:   input.Note,
		})
		if err != nil {
			failed = append(failed, user.ID)
			continue
		}
		granted = append(granted, purchase)
		if err := s.createBillingPlanGrantMessage(input.ActorID, user, purchase); err != nil {
			notifyFailed = append(notifyFailed, user.ID)
		}
	}
	result := BillingPlanGrantResult{
		Plan:         plan,
		Granted:      granted,
		SkippedRefs:  skipped,
		FailedRefs:   failed,
		NotifyFailed: notifyFailed,
		TargetedAll:  input.TargetAll,
		TotalTargets: len(targets),
	}
	if len(granted) == 0 {
		return result, fmt.Errorf("failed to grant plan to target users")
	}
	return result, nil
}

func (s *Service) grantBillingPlanAllUsers(includeDisabled bool) []core.User {
	out := make([]core.User, 0)
	const pageSize = 200
	for offset := 0; ; offset += pageSize {
		items, _, total := s.repo.ListUsersPage(storage.UserListQuery{
			Offset: offset,
			Limit:  pageSize,
			Sort:   "created_at",
		})
		for _, item := range items {
			user := item.User
			if strings.TrimSpace(user.ID) == "" || (!includeDisabled && !user.Enabled) {
				continue
			}
			out = append(out, user)
		}
		if offset+len(items) >= total || len(items) == 0 {
			break
		}
	}
	return out
}

func (s *Service) saleableBillingPlanGroups() map[string]bool {
	groups := s.ListBillingPlanGroups()
	out := make(map[string]bool, len(groups))
	for _, group := range groups {
		groupID := core.NormalizeBillingPlanGroup(group.ID)
		if groupID != "" && !group.SaleDisabled {
			out[groupID] = true
		}
	}
	return out
}

func (s *Service) billingPlanGroupSaleEnabled(groupID string) bool {
	groupID = core.NormalizeBillingPlanGroup(groupID)
	if groupID == "" {
		return false
	}
	group, err := s.GetBillingPlanGroup(groupID)
	return err == nil && !group.SaleDisabled
}

func (s *Service) createBillingPlanGrantMessage(actorID string, user core.User, purchase core.BillingPlanPurchase) error {
	userID := strings.TrimSpace(user.ID)
	if userID == "" {
		return storage.ErrNotFound
	}
	actorID = strings.TrimSpace(actorID)
	if actorID == "" {
		actorID = "system"
	}
	planName := strings.TrimSpace(purchase.Plan.Name)
	if planName == "" {
		planName = purchase.Plan.ID
	}
	body := fmt.Sprintf("管理员已赠送套餐「%s」给你。套餐额度：%s。", planName, core.FormatNanoUSD(purchase.Entitlement.CurrentQuotaNanoUSD))
	if !purchase.Entitlement.ExpiresAt.IsZero() {
		body += fmt.Sprintf("\n\n到期时间：%s", purchase.Entitlement.ExpiresAt.Local().Format("2006-01-02 15:04:05"))
	}
	now := time.Now().UTC()
	for range 3 {
		message := core.SiteMessage{
			ID:            generateSiteMessageID(),
			Title:         "套餐赠送到账",
			Body:          body,
			CreatedBy:     actorID,
			Enabled:       true,
			TargetUserIDs: []string{userID},
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if err := s.repo.CreateSiteMessage(message); err != nil {
			if errors.Is(err, storage.ErrBillingRequestConflict) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("message id conflict")
}

func (s *Service) grantBillingPlanPaidUsers(includeDisabled bool) []core.User {
	orders := s.repo.ListPaymentOrders(storage.PaymentOrderQuery{Status: core.PaymentOrderPaid})
	out := make([]core.User, 0, len(orders))
	seen := map[string]struct{}{}
	for _, order := range orders {
		userID := strings.TrimSpace(order.UserID)
		if userID == "" {
			continue
		}
		if _, ok := seen[userID]; ok {
			continue
		}
		user, err := s.repo.GetUser(userID)
		if err != nil || strings.TrimSpace(user.ID) == "" || (!includeDisabled && !user.Enabled) {
			continue
		}
		seen[user.ID] = struct{}{}
		out = append(out, user)
	}
	return out
}

func (s *Service) resolveBillingPlanGrantUsers(refs []string, includeDisabled bool) ([]core.User, []string) {
	out := make([]core.User, 0, len(refs))
	skipped := make([]string, 0)
	seen := map[string]struct{}{}
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		user, err := s.repo.GetUser(ref)
		if err != nil {
			user, err = s.repo.FindUserByUsername(ref)
		}
		if err != nil || strings.TrimSpace(user.ID) == "" || (!includeDisabled && !user.Enabled) {
			skipped = append(skipped, ref)
			continue
		}
		if _, ok := seen[user.ID]; ok {
			continue
		}
		seen[user.ID] = struct{}{}
		out = append(out, user)
	}
	return out, skipped
}

func (s *Service) ListUserPlanEntitlements(userID string) []core.UserPlanEntitlement {
	return s.repo.ListUserPlanEntitlements(strings.TrimSpace(userID))
}

func (s *Service) GetActiveUserPlanEntitlement(userID string) (core.UserPlanEntitlement, error) {
	return s.repo.GetActiveUserPlanEntitlement(strings.TrimSpace(userID))
}

func (s *Service) MoveUserPlanEntitlementPriority(user core.User, entitlementID, direction string) error {
	userID := strings.TrimSpace(user.ID)
	if userID == "" {
		return storage.ErrNotFound
	}
	return s.repo.MoveUserPlanEntitlementPriority(userID, strings.TrimSpace(entitlementID), strings.TrimSpace(direction))
}

func (s *Service) CancelUserPlanEntitlement(userID, entitlementID string) (core.UserPlanEntitlement, error) {
	userID = strings.TrimSpace(userID)
	entitlementID = strings.TrimSpace(entitlementID)
	if entitlementID == "" {
		return core.UserPlanEntitlement{}, storage.ErrNotFound
	}
	if userID == "" {
		for _, entitlement := range s.repo.ListUserPlanEntitlements("") {
			if strings.TrimSpace(entitlement.ID) == entitlementID {
				userID = strings.TrimSpace(entitlement.UserID)
				break
			}
		}
	}
	if userID == "" {
		return core.UserPlanEntitlement{}, storage.ErrNotFound
	}
	if _, err := s.repo.GetUser(userID); err != nil {
		return core.UserPlanEntitlement{}, err
	}
	return s.repo.CancelUserPlanEntitlement(userID, entitlementID)
}

func (s *Service) PlanSubscriptionsPage(filter PlanSubscriptionFilter) PlanSubscriptionsPage {
	filter = normalizePlanSubscriptionFilter(filter)
	query := storage.PlanSubscriptionQuery{
		UserID: filter.UserID,
		PlanID: filter.PlanID,
		Status: filter.Status,
		Offset: (filter.Page - 1) * filter.PageSize,
		Limit:  filter.PageSize,
	}
	rows, total := s.repo.ListPlanSubscriptionSummariesPage(query)
	lastPage := 1
	if total > 0 {
		lastPage = (total + filter.PageSize - 1) / filter.PageSize
	}
	if filter.Page > lastPage {
		filter.Page = lastPage
		query.Offset = (filter.Page - 1) * filter.PageSize
		rows, total = s.repo.ListPlanSubscriptionSummariesPage(query)
	}
	aggregateQuery := query
	aggregateQuery.Offset = 0
	aggregateQuery.Limit = 0
	stats := planSubscriptionStatsFromStorage(s.repo.PlanSubscriptionStats(aggregateQuery))
	page := PlanSubscriptionPageInfo{
		Total:    total,
		Page:     filter.Page,
		PageSize: filter.PageSize,
		HasPrev:  filter.Page > 1,
		PrevPage: filter.Page - 1,
		HasNext:  query.Offset+len(rows) < total,
		NextPage: filter.Page + 1,
	}
	return PlanSubscriptionsPage{
		Filter:        filter,
		Stats:         stats,
		PlanSummaries: planSubscriptionPlanSummariesFromStorage(s.repo.ListPlanSubscriptionPlanSummaries(aggregateQuery, 12)),
		Rows:          s.planSubscriptionSummariesFromStorage(rows),
		Page:          page,
	}
}

func normalizePlanSubscriptionFilter(filter PlanSubscriptionFilter) PlanSubscriptionFilter {
	filter.UserID = strings.TrimSpace(filter.UserID)
	filter.PlanID = strings.TrimSpace(filter.PlanID)
	filter.Status = core.UserPlanEntitlementStatus(strings.TrimSpace(string(filter.Status)))
	switch filter.Status {
	case "", core.UserPlanEntitlementActive, core.UserPlanEntitlementExpired, core.UserPlanEntitlementCancelled:
	default:
		filter.Status = ""
	}
	if filter.Page < 1 {
		filter.Page = 1
	}
	if filter.PageSize <= 0 {
		filter.PageSize = 25
	}
	if filter.PageSize > 100 {
		filter.PageSize = 100
	}
	return filter
}

func planSubscriptionStatsFromStorage(item storage.PlanSubscriptionStats) PlanSubscriptionStats {
	return PlanSubscriptionStats{
		TotalCount:             item.TotalCount,
		ActiveCount:            item.ActiveCount,
		ExpiredCount:           item.ExpiredCount,
		CancelledCount:         item.CancelledCount,
		RevenueNanoUSD:         item.RevenueNanoUSD,
		ActiveRemainingNanoUSD: item.ActiveRemainingNanoUSD,
		ActiveUsedNanoUSD:      item.ActiveUsedNanoUSD,
	}
}

func planSubscriptionPlanSummariesFromStorage(items []storage.PlanSubscriptionPlanSummary) []PlanSubscriptionPlanSummary {
	out := make([]PlanSubscriptionPlanSummary, 0, len(items))
	for _, item := range items {
		out = append(out, PlanSubscriptionPlanSummary{
			PlanID:                 strings.TrimSpace(item.PlanID),
			PlanName:               strings.TrimSpace(item.PlanName),
			PlanGroup:              strings.TrimSpace(item.PlanGroup),
			PurchaseCount:          item.PurchaseCount,
			ActiveCount:            item.ActiveCount,
			RevenueNanoUSD:         item.RevenueNanoUSD,
			ActiveRemainingNanoUSD: item.ActiveRemainingNanoUSD,
		})
	}
	return out
}

func (s *Service) planSubscriptionSummariesFromStorage(items []storage.PlanSubscriptionSummary) []PlanSubscriptionSummary {
	out := make([]PlanSubscriptionSummary, 0, len(items))
	usageDetailsByEntitlementID := make(map[string]PlanSubscriptionUsageDetails)
	for _, item := range items {
		username := strings.TrimSpace(item.Username)
		if username == "" {
			username = strings.TrimSpace(item.Entitlement.UserID)
		}
		entitlementID := strings.TrimSpace(item.Entitlement.ID)
		usageDetails, ok := usageDetailsByEntitlementID[entitlementID]
		if !ok {
			usageDetails = s.planSubscriptionUsageDetails(item.Entitlement, username)
			usageDetailsByEntitlementID[entitlementID] = usageDetails
		}
		out = append(out, PlanSubscriptionSummary{
			Entitlement:        item.Entitlement,
			Username:           username,
			PlanGroup:          strings.TrimSpace(item.PlanGroup),
			UserBalanceNanoUSD: item.UserBalanceNanoUSD,
			UsageDetails:       usageDetails,
		})
	}
	return out
}

func (s *Service) planSubscriptionUsageDetails(entitlement core.UserPlanEntitlement, username string) PlanSubscriptionUsageDetails {
	entitlement.ID = strings.TrimSpace(entitlement.ID)
	entitlement.UserID = strings.TrimSpace(entitlement.UserID)
	entitlement.PlanID = strings.TrimSpace(entitlement.PlanID)
	entitlement.PlanName = strings.TrimSpace(entitlement.PlanName)
	username = strings.TrimSpace(username)
	if username == "" {
		username = entitlement.UserID
	}
	currentRemaining := entitlement.CurrentQuotaNanoUSD
	if currentRemaining < 0 {
		currentRemaining = 0
	}
	currentUsed := entitlement.PeriodQuotaNanoUSD - entitlement.CurrentQuotaNanoUSD
	if currentUsed < 0 {
		currentUsed = 0
	}
	details := PlanSubscriptionUsageDetails{
		UserID:                        entitlement.UserID,
		Username:                      username,
		EntitlementID:                 entitlement.ID,
		PlanName:                      entitlement.PlanName,
		CurrentPeriodQuotaNanoUSD:     entitlement.PeriodQuotaNanoUSD,
		CurrentPeriodRemainingNanoUSD: currentRemaining,
		CurrentPeriodUsedNanoUSD:      currentUsed,
		CurrentPeriodStartedAt:        entitlement.CurrentPeriodStartedAt,
		CurrentPeriodEndsAt:           entitlement.CurrentPeriodEndsAt,
	}
	if details.PlanName == "" {
		details.PlanName = entitlement.PlanID
	}
	if entitlement.UserID == "" || entitlement.ID == "" {
		return details
	}
	query := storage.PlanQuotaUsageQuery{
		UserID:        entitlement.UserID,
		EntitlementID: entitlement.ID,
		Limit:         0,
	}
	details.Rows = planQuotaUsagePeriodSummaries(entitlement, username, s.repo.ListPlanQuotaUsageEvents(query))
	details.Stats = planQuotaUsageStatsFromSummaries(details.Rows)
	return details
}

func planQuotaUsagePeriodSummaries(entitlement core.UserPlanEntitlement, username string, events []storage.PlanQuotaUsageEvent) []PlanQuotaUsageSummary {
	entitlement.ID = strings.TrimSpace(entitlement.ID)
	entitlement.UserID = strings.TrimSpace(entitlement.UserID)
	entitlement.PlanID = strings.TrimSpace(entitlement.PlanID)
	entitlement.PlanName = strings.TrimSpace(entitlement.PlanName)
	username = strings.TrimSpace(username)
	if username == "" {
		username = entitlement.UserID
	}
	planName := entitlement.PlanName
	if strings.TrimSpace(planName) == "" {
		planName = entitlement.PlanID
	}
	duration := time.Duration(entitlement.PeriodDurationSec) * time.Second
	if duration <= 0 {
		duration = 24 * time.Hour
	}
	periodStart := entitlement.CurrentPeriodStartedAt
	if periodStart.IsZero() {
		periodStart = entitlement.PurchasedAt
	}
	if periodStart.IsZero() {
		for _, event := range events {
			if !event.CreatedAt.IsZero() && (periodStart.IsZero() || event.CreatedAt.Before(periodStart)) {
				periodStart = event.CreatedAt
			}
		}
	}
	if periodStart.IsZero() {
		return nil
	}
	periodEnd := entitlement.CurrentPeriodEndsAt
	if periodEnd.IsZero() || !periodStart.Before(periodEnd) {
		periodEnd = periodStart.Add(duration)
	}
	maxRows := entitlement.TotalPeriods
	if maxRows <= 0 {
		maxRows = entitlement.RemainingPeriods
	}
	if maxRows <= 0 {
		maxRows = 1
	}
	out := make([]PlanQuotaUsageSummary, 0, maxRows)
	for index := 0; index < maxRows; index++ {
		if !entitlement.PurchasedAt.IsZero() && !periodEnd.After(entitlement.PurchasedAt) {
			break
		}
		row := PlanQuotaUsageSummary{
			PeriodStartedAt: periodStart,
			PeriodEndsAt:    periodEnd,
			UserID:          entitlement.UserID,
			Username:        username,
			PlanID:          entitlement.PlanID,
			PlanName:        planName,
			EntitlementID:   entitlement.ID,
		}
		for _, event := range events {
			if !planQuotaUsageEventInPeriod(event, periodStart, periodEnd) {
				continue
			}
			addPlanQuotaUsageEventToSummary(&row, event)
		}
		currentPeriod := entitlement.Status == core.UserPlanEntitlementActive &&
			(periodStart.Equal(entitlement.CurrentPeriodStartedAt) || (entitlement.CurrentPeriodStartedAt.IsZero() && index == 0))
		if currentPeriod {
			applyCurrentPlanPeriodUsage(&row, entitlement)
		} else {
			closeCompletedPlanUsageSummary(&row)
		}
		out = append(out, row)
		if !entitlement.PurchasedAt.IsZero() && !periodStart.After(entitlement.PurchasedAt) {
			break
		}
		periodEnd = periodStart
		periodStart = periodStart.Add(-duration)
	}
	return out
}

func planQuotaUsageStatsFromSummaries(rows []PlanQuotaUsageSummary) PlanQuotaUsageStats {
	var stats PlanQuotaUsageStats
	for _, row := range rows {
		stats.GrantedNanoUSD = addSignedNanoUSDSaturating(stats.GrantedNanoUSD, row.GrantedNanoUSD)
		stats.UsedNanoUSD = addSignedNanoUSDSaturating(stats.UsedNanoUSD, row.UsedNanoUSD)
		stats.ReturnedNanoUSD = addSignedNanoUSDSaturating(stats.ReturnedNanoUSD, row.ReturnedNanoUSD)
		stats.ExpiredNanoUSD = addSignedNanoUSDSaturating(stats.ExpiredNanoUSD, row.ExpiredNanoUSD)
		stats.NetNanoUSD = addSignedNanoUSDSaturating(stats.NetNanoUSD, row.NetNanoUSD)
	}
	return stats
}

func planQuotaUsageEventInPeriod(event storage.PlanQuotaUsageEvent, startedAt, endedAt time.Time) bool {
	if event.CreatedAt.IsZero() || startedAt.IsZero() || endedAt.IsZero() || !startedAt.Before(endedAt) {
		return false
	}
	kind := strings.TrimSpace(event.Kind)
	if kind == "expire" || kind == "cancel" {
		if event.CreatedAt.Equal(endedAt) {
			return true
		}
		if event.CreatedAt.Equal(startedAt) {
			return false
		}
		return event.CreatedAt.After(startedAt) && event.CreatedAt.Before(endedAt)
	}
	return !event.CreatedAt.Before(startedAt) && event.CreatedAt.Before(endedAt)
}

func addPlanQuotaUsageEventToSummary(summary *PlanQuotaUsageSummary, event storage.PlanQuotaUsageEvent) {
	if summary == nil {
		return
	}
	summary.GrantedNanoUSD = addSignedNanoUSDSaturating(summary.GrantedNanoUSD, event.GrantedNanoUSD)
	usageDelta := addSignedNanoUSDSaturating(event.ReturnedNanoUSD, -event.UsedNanoUSD)
	usageNet := addSignedNanoUSDSaturating(addSignedNanoUSDSaturating(summary.ReturnedNanoUSD, -summary.UsedNanoUSD), usageDelta)
	if usageNet < 0 {
		summary.UsedNanoUSD = absControlNanoUSD(usageNet)
		summary.ReturnedNanoUSD = 0
	} else {
		summary.UsedNanoUSD = 0
		summary.ReturnedNanoUSD = usageNet
	}
	summary.ExpiredNanoUSD = addSignedNanoUSDSaturating(summary.ExpiredNanoUSD, event.ExpiredNanoUSD)
	summary.NetNanoUSD = addSignedNanoUSDSaturating(summary.NetNanoUSD, event.NetNanoUSD)
}

func closeCompletedPlanUsageSummary(summary *PlanQuotaUsageSummary) {
	if summary == nil {
		return
	}
	if summary.NetNanoUSD > 0 {
		summary.ExpiredNanoUSD = addSignedNanoUSDSaturating(summary.ExpiredNanoUSD, summary.NetNanoUSD)
		summary.NetNanoUSD = 0
		return
	}
	if summary.NetNanoUSD >= 0 {
		return
	}
	overused := absControlNanoUSD(summary.NetNanoUSD)
	summary.ReturnedNanoUSD = addSignedNanoUSDSaturating(summary.ReturnedNanoUSD, overused)
	summary.NetNanoUSD = 0
}

func absControlNanoUSD(value int64) int64 {
	if value == -1<<63 {
		return 1<<63 - 1
	}
	if value < 0 {
		return -value
	}
	return value
}

func applyCurrentPlanPeriodUsage(summary *PlanQuotaUsageSummary, entitlement core.UserPlanEntitlement) {
	if summary == nil {
		return
	}
	used := entitlement.PeriodQuotaNanoUSD - entitlement.CurrentQuotaNanoUSD
	if used < 0 {
		used = 0
	}
	summary.GrantedNanoUSD = entitlement.PeriodQuotaNanoUSD
	summary.UsedNanoUSD = used
	summary.ReturnedNanoUSD = 0
	summary.ExpiredNanoUSD = 0
	summary.NetNanoUSD = entitlement.PeriodQuotaNanoUSD - used
}

func normalizeControlBillingPlanGroup(group core.BillingPlanGroup) core.BillingPlanGroup {
	group.ID = strings.TrimSpace(group.ID)
	group.Name = strings.TrimSpace(group.Name)
	if group.ID == "" {
		group.ID = group.Name
	}
	if group.Name == "" {
		group.Name = group.ID
	}
	ratio, err := core.NormalizeBillingPlanGroupQuotaPriceRatio(group.QuotaPriceRatio)
	if err != nil {
		ratio = core.DefaultBillingPlanGroupQuotaPriceRatio
	}
	group.QuotaPriceRatio = ratio
	return group
}

func normalizeBillingPlanGroupInputRatio(raw string) (string, error) {
	ratio, err := core.NormalizeBillingPlanGroupQuotaPriceRatio(raw)
	if err != nil {
		return "", fmt.Errorf("plan group quota price ratio: %w", err)
	}
	return ratio, nil
}

func sortControlBillingPlanGroups(groups []core.BillingPlanGroup) []core.BillingPlanGroup {
	slices.SortFunc(groups, func(a, b core.BillingPlanGroup) int {
		a = normalizeControlBillingPlanGroup(a)
		b = normalizeControlBillingPlanGroup(b)
		if a.SortOrder != b.SortOrder {
			return a.SortOrder - b.SortOrder
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			if a.CreatedAt.IsZero() {
				return 1
			}
			if b.CreatedAt.IsZero() {
				return -1
			}
			return a.CreatedAt.Compare(b.CreatedAt)
		}
		if a.Name != b.Name {
			return strings.Compare(a.Name, b.Name)
		}
		return strings.Compare(a.ID, b.ID)
	})
	return groups
}
