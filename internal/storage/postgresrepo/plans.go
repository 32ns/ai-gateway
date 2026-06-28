package postgresrepo

import (
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

const defaultBillingPlanPeriodSec int64 = 24 * 60 * 60

func normalizeBillingPlan(plan core.BillingPlan) core.BillingPlan {
	plan.ID = strings.TrimSpace(plan.ID)
	plan.Name = strings.TrimSpace(plan.Name)
	plan.Description = strings.TrimSpace(plan.Description)
	plan.Group = core.NormalizeBillingPlanGroup(plan.Group)
	if plan.PeriodDurationSec <= 0 {
		plan.PeriodDurationSec = defaultBillingPlanPeriodSec
	}
	if plan.PeriodCount <= 0 {
		plan.PeriodCount = 1
	}
	if plan.PriceNanoUSD < 0 {
		plan.PriceNanoUSD = 0
	}
	if plan.PeriodQuotaNanoUSD < 0 {
		plan.PeriodQuotaNanoUSD = 0
	}
	return plan
}

func normalizeBillingPlanGroup(group core.BillingPlanGroup) core.BillingPlanGroup {
	group.ID = strings.TrimSpace(group.ID)
	group.Name = strings.TrimSpace(group.Name)
	ratio, err := core.NormalizeBillingPlanGroupQuotaPriceRatio(group.QuotaPriceRatio)
	if err != nil {
		ratio = core.DefaultBillingPlanGroupQuotaPriceRatio
	}
	group.QuotaPriceRatio = ratio
	return group
}

func billingPlanGroupSaleEnabled(group core.BillingPlanGroup) bool {
	return strings.TrimSpace(group.ID) != "" && !group.SaleDisabled
}

func sortBillingPlanGroups(groups []core.BillingPlanGroup) []core.BillingPlanGroup {
	slices.SortFunc(groups, func(a, b core.BillingPlanGroup) int {
		a = normalizeBillingPlanGroup(a)
		b = normalizeBillingPlanGroup(b)
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

func sortBillingPlans(plans []core.BillingPlan) []core.BillingPlan {
	slices.SortFunc(plans, func(a, b core.BillingPlan) int {
		if a.Group != b.Group {
			return strings.Compare(a.Group, b.Group)
		}
		if a.SortOrder != b.SortOrder {
			return a.SortOrder - b.SortOrder
		}
		if a.Enabled != b.Enabled {
			if a.Enabled {
				return -1
			}
			return 1
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			if a.CreatedAt.Before(b.CreatedAt) {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return plans
}

func sortUserPlanEntitlements(entitlements []core.UserPlanEntitlement) []core.UserPlanEntitlement {
	slices.SortFunc(entitlements, func(a, b core.UserPlanEntitlement) int {
		if a.Status == core.UserPlanEntitlementActive && b.Status == core.UserPlanEntitlementActive {
			if a.Priority > 0 || b.Priority > 0 {
				if a.Priority <= 0 {
					return 1
				}
				if b.Priority <= 0 {
					return -1
				}
				if a.Priority != b.Priority {
					return a.Priority - b.Priority
				}
			}
		}
		if !a.PurchasedAt.Equal(b.PurchasedAt) {
			if a.PurchasedAt.After(b.PurchasedAt) {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return entitlements
}

func sortPlanQuotaUsageDaySummaries(items []PlanQuotaUsageDaySummary) []PlanQuotaUsageDaySummary {
	slices.SortFunc(items, func(a, b PlanQuotaUsageDaySummary) int {
		if a.Date != b.Date {
			return strings.Compare(b.Date, a.Date)
		}
		if !a.LastLedgerCreatedAt.Equal(b.LastLedgerCreatedAt) {
			if a.LastLedgerCreatedAt.After(b.LastLedgerCreatedAt) {
				return -1
			}
			return 1
		}
		if a.Username != b.Username {
			return strings.Compare(a.Username, b.Username)
		}
		if a.UserID != b.UserID {
			return strings.Compare(a.UserID, b.UserID)
		}
		if a.PlanName != b.PlanName {
			return strings.Compare(a.PlanName, b.PlanName)
		}
		if a.PlanID != b.PlanID {
			return strings.Compare(a.PlanID, b.PlanID)
		}
		return strings.Compare(a.EntitlementID, b.EntitlementID)
	})
	return items
}

func sortPlanQuotaUsageEvents(items []PlanQuotaUsageEvent) []PlanQuotaUsageEvent {
	slices.SortFunc(items, func(a, b PlanQuotaUsageEvent) int {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			if a.CreatedAt.After(b.CreatedAt) {
				return -1
			}
			return 1
		}
		if a.Username != b.Username {
			return strings.Compare(a.Username, b.Username)
		}
		if a.UserID != b.UserID {
			return strings.Compare(a.UserID, b.UserID)
		}
		if a.PlanName != b.PlanName {
			return strings.Compare(a.PlanName, b.PlanName)
		}
		if a.PlanID != b.PlanID {
			return strings.Compare(a.PlanID, b.PlanID)
		}
		return strings.Compare(a.EntitlementID, b.EntitlementID)
	})
	return items
}

func planEntitlementPriorityLess(current, candidate core.UserPlanEntitlement) bool {
	if current.Priority > 0 || candidate.Priority > 0 {
		if current.Priority <= 0 {
			return false
		}
		if candidate.Priority <= 0 {
			return true
		}
		if current.Priority != candidate.Priority {
			return current.Priority > candidate.Priority
		}
	}
	if !current.PurchasedAt.Equal(candidate.PurchasedAt) {
		return current.PurchasedAt.After(candidate.PurchasedAt)
	}
	return strings.Compare(current.ID, candidate.ID) > 0
}

func selectPlanEntitlementForReservation(entitlements []core.UserPlanEntitlement, amountNanoUSD int64) (core.UserPlanEntitlement, bool) {
	if amountNanoUSD <= 0 {
		return core.UserPlanEntitlement{}, false
	}
	for _, entitlement := range sortUserPlanEntitlements(entitlements) {
		if entitlement.Status != core.UserPlanEntitlementActive {
			continue
		}
		if entitlement.CurrentQuotaNanoUSD <= 0 {
			continue
		}
		return entitlement, true
	}
	return core.UserPlanEntitlement{}, false
}

func normalizeUserPlanEntitlement(entitlement core.UserPlanEntitlement) core.UserPlanEntitlement {
	entitlement.ID = strings.TrimSpace(entitlement.ID)
	entitlement.UserID = strings.TrimSpace(entitlement.UserID)
	entitlement.PlanID = strings.TrimSpace(entitlement.PlanID)
	entitlement.PlanName = strings.TrimSpace(entitlement.PlanName)
	entitlement.Status = core.UserPlanEntitlementStatus(strings.TrimSpace(string(entitlement.Status)))
	if entitlement.Status == "" {
		entitlement.Status = core.UserPlanEntitlementActive
	}
	if entitlement.PeriodDurationSec <= 0 {
		entitlement.PeriodDurationSec = defaultBillingPlanPeriodSec
	}
	if entitlement.TotalPeriods <= 0 {
		entitlement.TotalPeriods = max(1, entitlement.RemainingPeriods)
	}
	if entitlement.RemainingPeriods < 0 {
		entitlement.RemainingPeriods = 0
	}
	if entitlement.PriceNanoUSD < 0 {
		entitlement.PriceNanoUSD = 0
	}
	if entitlement.PeriodQuotaNanoUSD < 0 {
		entitlement.PeriodQuotaNanoUSD = 0
	}
	if entitlement.BasePeriodQuotaNanoUSD < 0 {
		entitlement.BasePeriodQuotaNanoUSD = 0
	}
	if entitlement.BasePeriodQuotaNanoUSD == 0 {
		entitlement.BasePeriodQuotaNanoUSD = entitlement.PeriodQuotaNanoUSD
	}
	return entitlement
}

func normalizeBillingPlanPurchaseMode(mode core.BillingPlanPurchaseMode) core.BillingPlanPurchaseMode {
	switch mode {
	case core.BillingPlanPurchaseMergeQuota, core.BillingPlanPurchaseExtendPeriod:
		return mode
	default:
		return core.BillingPlanPurchaseSeparate
	}
}

func billingPlanCanCombineEntitlement(plan core.BillingPlan, entitlement core.UserPlanEntitlement) bool {
	plan = normalizeBillingPlan(plan)
	entitlement = normalizeUserPlanEntitlement(entitlement)
	return plan.ID != "" &&
		entitlement.Status == core.UserPlanEntitlementActive &&
		strings.TrimSpace(entitlement.PlanID) == plan.ID &&
		entitlement.BasePeriodQuotaNanoUSD == plan.PeriodQuotaNanoUSD &&
		entitlement.PeriodDurationSec == plan.PeriodDurationSec
}

func billingPlanCanExtendEntitlement(plan core.BillingPlan, entitlement core.UserPlanEntitlement) bool {
	plan = normalizeBillingPlan(plan)
	entitlement = normalizeUserPlanEntitlement(entitlement)
	return billingPlanCanCombineEntitlement(plan, entitlement) &&
		entitlement.PeriodQuotaNanoUSD == plan.PeriodQuotaNanoUSD
}

func selectBillingPlanCombineEntitlement(plan core.BillingPlan, entitlements []core.UserPlanEntitlement, targetEntitlementID string) (core.UserPlanEntitlement, bool) {
	targetEntitlementID = strings.TrimSpace(targetEntitlementID)
	for _, entitlement := range entitlements {
		if targetEntitlementID != "" && strings.TrimSpace(entitlement.ID) != targetEntitlementID {
			continue
		}
		if billingPlanCanCombineEntitlement(plan, entitlement) {
			return entitlement, true
		}
	}
	if targetEntitlementID != "" {
		return core.UserPlanEntitlement{}, false
	}
	for _, entitlement := range entitlements {
		if billingPlanCanCombineEntitlement(plan, entitlement) {
			return entitlement, true
		}
	}
	return core.UserPlanEntitlement{}, false
}

func selectBillingPlanExtendEntitlement(plan core.BillingPlan, entitlements []core.UserPlanEntitlement, targetEntitlementID string) (core.UserPlanEntitlement, bool) {
	targetEntitlementID = strings.TrimSpace(targetEntitlementID)
	for _, entitlement := range entitlements {
		if targetEntitlementID != "" && strings.TrimSpace(entitlement.ID) != targetEntitlementID {
			continue
		}
		if billingPlanCanExtendEntitlement(plan, entitlement) {
			return entitlement, true
		}
	}
	if targetEntitlementID != "" {
		return core.UserPlanEntitlement{}, false
	}
	for _, entitlement := range entitlements {
		if billingPlanCanExtendEntitlement(plan, entitlement) {
			return entitlement, true
		}
	}
	return core.UserPlanEntitlement{}, false
}

func mergeBillingPlanQuota(entitlement core.UserPlanEntitlement, plan core.BillingPlan, now time.Time) core.UserPlanEntitlement {
	entitlement = normalizeUserPlanEntitlement(entitlement)
	plan = normalizeBillingPlan(plan)
	entitlement.PeriodQuotaNanoUSD = addNanoUSDSaturating(entitlement.PeriodQuotaNanoUSD, plan.PeriodQuotaNanoUSD)
	currentQuota := entitlement.CurrentQuotaNanoUSD
	if currentQuota < 0 {
		currentQuota = 0
	}
	entitlement.CurrentQuotaNanoUSD = addNanoUSDSaturating(currentQuota, plan.PeriodQuotaNanoUSD)
	entitlement.UpdatedAt = now
	return entitlement
}

func extendBillingPlanPeriod(entitlement core.UserPlanEntitlement, plan core.BillingPlan, now time.Time) core.UserPlanEntitlement {
	entitlement = normalizeUserPlanEntitlement(entitlement)
	plan = normalizeBillingPlan(plan)
	duration := time.Duration(plan.PeriodDurationSec) * time.Second
	if duration <= 0 {
		duration = time.Duration(defaultBillingPlanPeriodSec) * time.Second
	}
	periods := plan.PeriodCount
	if periods <= 0 {
		periods = 1
	}
	extension := duration * time.Duration(periods)
	base := entitlement.ExpiresAt
	if base.IsZero() || base.Before(now) {
		base = now
	}
	entitlement.ExpiresAt = base.Add(extension)
	entitlement.RemainingPeriods += periods
	entitlement.TotalPeriods += periods
	entitlement.UpdatedAt = now
	return entitlement
}

func newUserPlanEntitlement(userID string, plan core.BillingPlan, now time.Time) core.UserPlanEntitlement {
	plan = normalizeBillingPlan(plan)
	duration := time.Duration(plan.PeriodDurationSec) * time.Second
	entitlement := core.UserPlanEntitlement{
		ID:                     planEntitlementID(userID, plan.ID, now),
		UserID:                 strings.TrimSpace(userID),
		PlanID:                 plan.ID,
		PlanName:               plan.Name,
		Status:                 core.UserPlanEntitlementActive,
		PriceNanoUSD:           plan.PriceNanoUSD,
		PeriodQuotaNanoUSD:     plan.PeriodQuotaNanoUSD,
		BasePeriodQuotaNanoUSD: plan.PeriodQuotaNanoUSD,
		PeriodDurationSec:      plan.PeriodDurationSec,
		TotalPeriods:           plan.PeriodCount,
		RemainingPeriods:       plan.PeriodCount,
		CurrentQuotaNanoUSD:    plan.PeriodQuotaNanoUSD,
		CurrentPeriodStartedAt: now,
		CurrentPeriodEndsAt:    now.Add(duration),
		ExpiresAt:              now.Add(duration * time.Duration(plan.PeriodCount)),
		PurchasedAt:            now,
		UpdatedAt:              now,
	}
	return normalizeUserPlanEntitlement(entitlement)
}

func advanceUserPlanEntitlement(entitlement core.UserPlanEntitlement, now time.Time) (core.UserPlanEntitlement, []core.PlanQuotaLedgerEntry, bool) {
	entitlement = normalizeUserPlanEntitlement(entitlement)
	if now.IsZero() {
		now = time.Now().UTC()
	}
	entitlement, recovered := recoverPrematurelyExpiredPlanEntitlement(entitlement, now)
	if entitlement.Status != core.UserPlanEntitlementActive {
		return entitlement, nil, recovered
	}
	if entitlement.RemainingPeriods <= 0 {
		expired, entries, changed := expireUserPlanEntitlement(entitlement, now, now)
		return expired, entries, recovered || changed
	}
	duration := time.Duration(entitlement.PeriodDurationSec) * time.Second
	if duration <= 0 {
		duration = time.Duration(defaultBillingPlanPeriodSec) * time.Second
	}
	if entitlement.CurrentPeriodStartedAt.IsZero() {
		entitlement.CurrentPeriodStartedAt = entitlement.PurchasedAt
		if entitlement.CurrentPeriodStartedAt.IsZero() {
			entitlement.CurrentPeriodStartedAt = now
		}
	}
	if entitlement.CurrentPeriodEndsAt.IsZero() {
		entitlement.CurrentPeriodEndsAt = entitlement.CurrentPeriodStartedAt.Add(duration)
	}

	changed := recovered
	entries := []core.PlanQuotaLedgerEntry{}
	for !now.Before(entitlement.CurrentPeriodEndsAt) {
		if entitlement.RemainingPeriods <= 1 {
			expired, expireEntries, expireChanged := expireUserPlanEntitlement(entitlement, entitlement.CurrentPeriodEndsAt, now)
			if expireChanged {
				entries = append(entries, expireEntries...)
			}
			return expired, entries, changed || expireChanged
		}
		resetAt := entitlement.CurrentPeriodEndsAt
		previousQuota := entitlement.CurrentQuotaNanoUSD
		nextQuota := entitlement.BasePeriodQuotaNanoUSD
		entitlement.RemainingPeriods--
		entitlement.CurrentPeriodStartedAt = resetAt
		entitlement.CurrentPeriodEndsAt = resetAt.Add(duration)
		entitlement.PeriodQuotaNanoUSD = entitlement.BasePeriodQuotaNanoUSD
		entitlement.CurrentQuotaNanoUSD = nextQuota
		entitlement.UpdatedAt = now
		changed = true
		if previousQuota > 0 {
			entries = append(entries, core.PlanQuotaLedgerEntry{
				ID:                planQuotaLedgerID("expire", entitlement.ID, "", "", resetAt, len(entries)),
				EntitlementID:     entitlement.ID,
				UserID:            entitlement.UserID,
				Kind:              "expire",
				AmountNanoUSD:     -previousQuota,
				QuotaAfterNanoUSD: 0,
				Note:              entitlement.PlanName,
				CreatedAt:         resetAt,
			})
		}
		entries = append(entries, core.PlanQuotaLedgerEntry{
			ID:                planQuotaLedgerID("reset", entitlement.ID, "", "", resetAt, len(entries)),
			EntitlementID:     entitlement.ID,
			UserID:            entitlement.UserID,
			Kind:              "reset",
			AmountNanoUSD:     entitlement.BasePeriodQuotaNanoUSD,
			QuotaAfterNanoUSD: entitlement.CurrentQuotaNanoUSD,
			Note:              entitlement.PlanName,
			CreatedAt:         resetAt,
		})
	}
	return entitlement, entries, changed
}

func expireUserPlanEntitlement(entitlement core.UserPlanEntitlement, eventAt, updatedAt time.Time) (core.UserPlanEntitlement, []core.PlanQuotaLedgerEntry, bool) {
	if entitlement.Status != core.UserPlanEntitlementActive {
		return entitlement, nil, false
	}
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	if eventAt.IsZero() {
		eventAt = updatedAt
	}
	previousQuota := entitlement.CurrentQuotaNanoUSD
	entitlement.Status = core.UserPlanEntitlementExpired
	entitlement.RemainingPeriods = 0
	entitlement.CurrentQuotaNanoUSD = 0
	entitlement.UpdatedAt = updatedAt
	if entitlement.ExpiresAt.IsZero() {
		entitlement.ExpiresAt = eventAt
	}
	entries := []core.PlanQuotaLedgerEntry{}
	if previousQuota > 0 {
		entries = append(entries, core.PlanQuotaLedgerEntry{
			ID:                planQuotaLedgerID("expire", entitlement.ID, "", "", eventAt, 0),
			EntitlementID:     entitlement.ID,
			UserID:            entitlement.UserID,
			Kind:              "expire",
			AmountNanoUSD:     -previousQuota,
			QuotaAfterNanoUSD: 0,
			Note:              entitlement.PlanName,
			CreatedAt:         eventAt,
		})
	}
	return entitlement, entries, true
}

func cancelUserPlanEntitlement(entitlement core.UserPlanEntitlement, now time.Time, index int) (core.UserPlanEntitlement, core.PlanQuotaLedgerEntry, bool) {
	entitlement = normalizeUserPlanEntitlement(entitlement)
	if now.IsZero() {
		now = time.Now().UTC()
	}
	previousQuota := entitlement.CurrentQuotaNanoUSD
	entitlement.Status = core.UserPlanEntitlementCancelled
	entitlement.RemainingPeriods = 0
	entitlement.CurrentQuotaNanoUSD = 0
	entitlement.ExpiresAt = now
	entitlement.UpdatedAt = now
	if previousQuota <= 0 {
		return entitlement, core.PlanQuotaLedgerEntry{}, false
	}
	return entitlement, core.PlanQuotaLedgerEntry{
		ID:                planQuotaLedgerID("cancel", entitlement.ID, "", "", now, index),
		EntitlementID:     entitlement.ID,
		UserID:            entitlement.UserID,
		Kind:              "cancel",
		AmountNanoUSD:     -previousQuota,
		QuotaAfterNanoUSD: 0,
		Note:              entitlement.PlanName,
		CreatedAt:         now,
	}, true
}

func recoverPrematurelyExpiredPlanEntitlement(entitlement core.UserPlanEntitlement, now time.Time) (core.UserPlanEntitlement, bool) {
	if entitlement.Status != core.UserPlanEntitlementExpired || entitlement.RemainingPeriods <= 0 {
		return entitlement, false
	}
	if !entitlement.ExpiresAt.IsZero() && !now.Before(entitlement.ExpiresAt) {
		return entitlement, false
	}
	entitlement.Status = core.UserPlanEntitlementActive
	entitlement.UpdatedAt = now
	return entitlement, true
}

func planEntitlementID(userID, planID string, ts time.Time) string {
	return fmt.Sprintf("upe_%x", hashID(strings.TrimSpace(userID)+"\x00"+strings.TrimSpace(planID)+"\x00"+fmt.Sprint(ts.UnixNano())))
}

func planQuotaLedgerID(kind, entitlementID, requestID, clientID string, ts time.Time, index int) string {
	parts := strings.Join([]string{
		strings.TrimSpace(kind),
		strings.TrimSpace(entitlementID),
		strings.TrimSpace(requestID),
		strings.TrimSpace(clientID),
		fmt.Sprint(ts.UnixNano()),
		fmt.Sprint(index),
	}, "\x00")
	return fmt.Sprintf("pql_%x", hashID(parts))
}

func billingAllocationID(requestID, clientID, source, entitlementID string) string {
	parts := strings.Join([]string{
		strings.TrimSpace(requestID),
		strings.TrimSpace(clientID),
		strings.TrimSpace(source),
		strings.TrimSpace(entitlementID),
	}, "\x00")
	return fmt.Sprintf("bfa_%x", hashID(parts))
}

func hashID(value string) uint64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(value))
	return hash.Sum64()
}
