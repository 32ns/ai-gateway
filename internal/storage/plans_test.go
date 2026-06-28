package storage

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestMemoryRepositoryBillingPlanReserveAndSettle(t *testing.T) {
	runBillingPlanReserveAndSettle(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanReserveAndSettle(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanReserveAndSettle(t, repo)
}

func TestMemoryRepositoryRecoversPrematurelyExpiredBillingPlan(t *testing.T) {
	runRecoversPrematurelyExpiredBillingPlan(t, NewMemoryRepository())
}

func TestSQLiteRepositoryRecoversPrematurelyExpiredBillingPlan(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runRecoversPrematurelyExpiredBillingPlan(t, repo)
}

func TestMemoryRepositoryBillingPlanPriorityMoveSelectsActivePlan(t *testing.T) {
	runBillingPlanPriorityMoveSelectsActivePlan(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanPriorityMoveSelectsActivePlan(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanPriorityMoveSelectsActivePlan(t, repo)
}

func TestMemoryRepositoryBillingPlanReservationSkipsInsufficientPriorityPlan(t *testing.T) {
	runBillingPlanReservationSkipsInsufficientPriorityPlan(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanReservationSkipsInsufficientPriorityPlan(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanReservationSkipsInsufficientPriorityPlan(t, repo)
}

func TestMemoryRepositoryBillingPlanPurchaseMergeQuota(t *testing.T) {
	runBillingPlanPurchaseMergeQuota(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanPurchaseMergeQuota(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanPurchaseMergeQuota(t, repo)
}

func TestMemoryRepositoryBillingPlanGrantDoesNotChargeBalance(t *testing.T) {
	runBillingPlanGrantDoesNotChargeBalance(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanGrantDoesNotChargeBalance(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanGrantDoesNotChargeBalance(t, repo)
}

func TestMemoryRepositoryBillingPlanPurchaseMergeQuotaTarget(t *testing.T) {
	runBillingPlanPurchaseMergeQuotaTarget(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanPurchaseMergeQuotaTarget(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanPurchaseMergeQuotaTarget(t, repo)
}

func TestMemoryRepositoryBillingPlanPurchaseMergeQuotaAllowsRepeatedSamePlan(t *testing.T) {
	runBillingPlanPurchaseMergeQuotaAllowsRepeatedSamePlan(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanPurchaseMergeQuotaAllowsRepeatedSamePlan(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanPurchaseMergeQuotaAllowsRepeatedSamePlan(t, repo)
}

func TestMemoryRepositoryBillingPlanPurchaseMergeQuotaResetsToBaseQuota(t *testing.T) {
	runBillingPlanPurchaseMergeQuotaResetsToBaseQuota(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanPurchaseMergeQuotaResetsToBaseQuota(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanPurchaseMergeQuotaResetsToBaseQuota(t, repo)
}

func TestMemoryRepositoryBillingPlanPurchaseMergeQuotaAllowsPriceChange(t *testing.T) {
	runBillingPlanPurchaseMergeQuotaAllowsPriceChange(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanPurchaseMergeQuotaAllowsPriceChange(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanPurchaseMergeQuotaAllowsPriceChange(t, repo)
}

func TestMemoryRepositoryBillingPlanPurchaseMergeQuotaRejectsQuotaMismatch(t *testing.T) {
	runBillingPlanPurchaseMergeQuotaRejectsQuotaMismatch(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanPurchaseMergeQuotaRejectsQuotaMismatch(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanPurchaseMergeQuotaRejectsQuotaMismatch(t, repo)
}

func TestMemoryRepositoryBillingPlanPurchaseExtendPeriod(t *testing.T) {
	runBillingPlanPurchaseExtendPeriod(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanPurchaseExtendPeriod(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanPurchaseExtendPeriod(t, repo)
}

func TestMemoryRepositoryBillingPlanPurchaseExtendPeriodTarget(t *testing.T) {
	runBillingPlanPurchaseExtendPeriodTarget(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanPurchaseExtendPeriodTarget(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanPurchaseExtendPeriodTarget(t, repo)
}

func TestMemoryRepositoryBillingPlanPurchaseExtendPeriodAllowsPriceChange(t *testing.T) {
	runBillingPlanPurchaseExtendPeriodAllowsPriceChange(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanPurchaseExtendPeriodAllowsPriceChange(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanPurchaseExtendPeriodAllowsPriceChange(t, repo)
}

func TestMemoryRepositoryBillingPlanPurchaseExtendPeriodRejectsQuotaMismatch(t *testing.T) {
	runBillingPlanPurchaseExtendPeriodRejectsQuotaMismatch(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanPurchaseExtendPeriodRejectsQuotaMismatch(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanPurchaseExtendPeriodRejectsQuotaMismatch(t, repo)
}

func TestMemoryRepositoryBillingPlanPurchaseExtendPeriodRejectsMergedQuota(t *testing.T) {
	runBillingPlanPurchaseExtendPeriodRejectsMergedQuota(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanPurchaseExtendPeriodRejectsMergedQuota(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanPurchaseExtendPeriodRejectsMergedQuota(t, repo)
}

func TestMemoryRepositoryBillingPlanReleaseRestoresQuota(t *testing.T) {
	runBillingPlanReleaseRestoresQuota(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanReleaseRestoresQuota(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanReleaseRestoresQuota(t, repo)
}

func TestMemoryRepositoryDeleteClientReleasesReservedBillingPlanQuota(t *testing.T) {
	runDeleteClientReleasesReservedBillingPlanQuota(t, NewMemoryRepository())
}

func TestSQLiteRepositoryDeleteClientReleasesReservedBillingPlanQuota(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runDeleteClientReleasesReservedBillingPlanQuota(t, repo)
}

func TestMemoryRepositoryDeleteUserPreservesBillingPlanHistory(t *testing.T) {
	runDeleteUserPreservesBillingPlanHistory(t, NewMemoryRepository())
}

func TestSQLiteRepositoryDeleteUserPreservesBillingPlanHistory(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runDeleteUserPreservesBillingPlanHistory(t, repo)
}

func TestMemoryRepositoryCancelUserPlanEntitlement(t *testing.T) {
	runCancelUserPlanEntitlement(t, NewMemoryRepository())
}

func TestSQLiteRepositoryCancelUserPlanEntitlement(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runCancelUserPlanEntitlement(t, repo)
}

func TestMemoryRepositoryBillingPlansPreserveExplicitGroups(t *testing.T) {
	repo := NewMemoryRepository()
	for _, plan := range []core.BillingPlan{
		{ID: "year", Name: "Year", Enabled: true, PeriodDurationSec: 24 * 60 * 60, PeriodCount: 365},
		{ID: "day", Name: "Day", Enabled: true, PeriodDurationSec: 24 * 60 * 60, PeriodCount: 1},
		{ID: "other", Name: "Flex", Enabled: true, PeriodDurationSec: 24 * 60 * 60, PeriodCount: 14},
		{ID: "month", Name: "Month", Enabled: true, PeriodDurationSec: 24 * 60 * 60, PeriodCount: 30},
		{ID: "vip", Name: "VIP", Group: "VIP", Enabled: true, PeriodDurationSec: 24 * 60 * 60, PeriodCount: 1},
		{ID: "week", Name: "Week", Enabled: true, PeriodDurationSec: 24 * 60 * 60, PeriodCount: 7},
	} {
		if err := repo.UpsertBillingPlan(plan); err != nil {
			t.Fatalf("UpsertBillingPlan(%s) returned error: %v", plan.ID, err)
		}
	}

	plans := repo.ListBillingPlans()
	gotGroups := map[string]string{}
	for _, plan := range plans {
		gotGroups[plan.ID] = plan.Group
	}
	for _, id := range []string{"day", "week", "month", "year", "other"} {
		if gotGroups[id] != "" {
			t.Fatalf("group for %s = %q, want empty", id, gotGroups[id])
		}
	}
	if gotGroups["vip"] != "VIP" {
		t.Fatalf("group for vip = %q, want VIP", gotGroups["vip"])
	}
}

func TestMemoryRepositoryBillingPlanGroupCRUD(t *testing.T) {
	runBillingPlanGroupCRUD(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanGroupCRUD(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanGroupCRUD(t, repo)
}

func TestMemoryRepositoryBillingPlanGroupsSortByCreatedAtWhenSortOrderMatches(t *testing.T) {
	runBillingPlanGroupsSortByCreatedAtWhenSortOrderMatches(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanGroupsSortByCreatedAtWhenSortOrderMatches(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanGroupsSortByCreatedAtWhenSortOrderMatches(t, repo)
}

func TestMemoryRepositoryMergeUsersMovesBillingPlanEntitlements(t *testing.T) {
	runMergeUsersMovesBillingPlanEntitlements(t, NewMemoryRepository())
}

func TestSQLiteRepositoryMergeUsersMovesBillingPlanEntitlements(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runMergeUsersMovesBillingPlanEntitlements(t, repo)
}

func TestMemoryRepositoryMergeUsersRejectsTwoActiveBillingPlans(t *testing.T) {
	runMergeUsersRejectsTwoActiveBillingPlans(t, NewMemoryRepository())
}

func TestSQLiteRepositoryMergeUsersRejectsTwoActiveBillingPlans(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runMergeUsersRejectsTwoActiveBillingPlans(t, repo)
}

func TestMemoryRepositoryBillingPlanPeriodReset(t *testing.T) {
	repo := NewMemoryRepository()
	runBillingPlanPeriodReset(t, repo)
	entitlements := repo.ListUserPlanEntitlements("user_plan_reset")
	if len(entitlements) != 1 {
		t.Fatalf("entitlements len = %d, want 1", len(entitlements))
	}
}

func TestSQLiteRepositoryBillingPlanPeriodReset(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanPeriodReset(t, repo)
}

func TestMemoryRepositoryBillingPlanResetLedgerUsesPeriodBoundaryTime(t *testing.T) {
	runBillingPlanResetLedgerUsesPeriodBoundaryTimeAndExpiresRemainingQuota(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanResetLedgerUsesPeriodBoundaryTime(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanResetLedgerUsesPeriodBoundaryTimeAndExpiresRemainingQuota(t, repo)
}

func TestMemoryRepositoryBillingPlanResetDropsNegativeQuota(t *testing.T) {
	runBillingPlanResetDropsNegativeQuota(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanResetDropsNegativeQuota(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanResetDropsNegativeQuota(t, repo)
}

func TestMemoryRepositoryBillingPlanMergePurchaseDropsNegativeQuota(t *testing.T) {
	runBillingPlanMergePurchaseDropsNegativeQuota(t, NewMemoryRepository())
}

func TestSQLiteRepositoryBillingPlanMergePurchaseDropsNegativeQuota(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanMergePurchaseDropsNegativeQuota(t, repo)
}

func TestMemoryRepositoryDisabledBillingPlanDoesNotAffectPurchasedEntitlement(t *testing.T) {
	runDisabledBillingPlanDoesNotAffectPurchasedEntitlement(t, NewMemoryRepository())
}

func TestSQLiteRepositoryDisabledBillingPlanDoesNotAffectPurchasedEntitlement(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runDisabledBillingPlanDoesNotAffectPurchasedEntitlement(t, repo)
}

func TestMemoryRepositoryDisabledBillingPlanCannotBePurchased(t *testing.T) {
	runDisabledBillingPlanCannotBePurchased(t, NewMemoryRepository())
}

func TestSQLiteRepositoryDisabledBillingPlanCannotBePurchased(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runDisabledBillingPlanCannotBePurchased(t, repo)
}

func TestMemoryRepositoryDisabledBillingPlanGroupCannotBePurchased(t *testing.T) {
	runDisabledBillingPlanGroupCannotBePurchased(t, NewMemoryRepository())
}

func TestSQLiteRepositoryDisabledBillingPlanGroupCannotBePurchased(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runDisabledBillingPlanGroupCannotBePurchased(t, repo)
}

func TestMemoryRepositoryBillingPlanListEntitlementsResetsPeriod(t *testing.T) {
	repo := NewMemoryRepository()
	runBillingPlanListEntitlementsResetsPeriod(t, repo)
}

func TestSQLiteRepositoryBillingPlanListEntitlementsResetsPeriod(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runBillingPlanListEntitlementsResetsPeriod(t, repo)
}

func TestMemoryRepositoryPlanSubscriptionQueries(t *testing.T) {
	runPlanSubscriptionQueries(t, NewMemoryRepository())
}

func TestSQLiteRepositoryPlanSubscriptionQueries(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runPlanSubscriptionQueries(t, repo)
}

func TestMemoryRepositoryPlanQuotaUsageByDay(t *testing.T) {
	runPlanQuotaUsageByDay(t, NewMemoryRepository())
}

func TestSQLiteRepositoryPlanQuotaUsageByDay(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	runPlanQuotaUsageByDay(t, repo)
}

func TestMemoryRepositoryPlanQuotaUsageUsesRequestActualSpendByDay(t *testing.T) {
	repo := NewMemoryRepository()
	purchase := seedPlanQuotaActualSpendScenario(t, repo)
	base := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)
	settledAt := base.AddDate(0, 0, 1)

	repo.mu.Lock()
	requestKey := billingRequestKey("req_plan_quota_actual_day", "client_plan_quota_actual_day")
	request := repo.billing[requestKey]
	request.CreatedAt = base
	request.SettledAt = &settledAt
	repo.billing[requestKey] = request
	for index, entry := range repo.planLedger {
		if entry.EntitlementID != purchase.Entitlement.ID {
			continue
		}
		switch entry.Kind {
		case "purchase":
			entry.CreatedAt = base
		case "settle":
			entry.CreatedAt = settledAt
		}
		repo.planLedger[index] = entry
	}
	for key, allocation := range repo.allocations {
		if allocation.RequestID != "req_plan_quota_actual_day" || allocation.ClientID != "client_plan_quota_actual_day" {
			continue
		}
		allocation.CreatedAt = base
		allocation.UpdatedAt = settledAt
		repo.allocations[key] = allocation
	}
	repo.mu.Unlock()

	assertPlanQuotaActualSpendByDay(t, repo, purchase.Entitlement.ID, base, settledAt)
}

func TestSQLiteRepositoryPlanQuotaUsageUsesRequestActualSpendByDay(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	defer repo.Close()
	purchase := seedPlanQuotaActualSpendScenario(t, repo)
	base := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)
	settledAt := base.AddDate(0, 0, 1)

	repo.mu.Lock()
	if _, err := repo.db.Exec(`UPDATE billing_requests SET created_at_ns = ?, settled_at_ns = ? WHERE request_id = ? AND client_id = ?`, sqliteTimeNS(base), sqliteTimeNS(settledAt), "req_plan_quota_actual_day", "client_plan_quota_actual_day"); err != nil {
		repo.mu.Unlock()
		t.Fatalf("update billing request timestamps: %v", err)
	}
	if _, err := repo.db.Exec(`UPDATE billing_funding_allocations SET created_at_ns = ?, updated_at_ns = ? WHERE request_id = ? AND client_id = ?`, sqliteTimeNS(base), sqliteTimeNS(settledAt), "req_plan_quota_actual_day", "client_plan_quota_actual_day"); err != nil {
		repo.mu.Unlock()
		t.Fatalf("update funding allocation timestamps: %v", err)
	}
	if _, err := repo.db.Exec(`UPDATE plan_quota_ledger SET created_at_ns = ? WHERE entitlement_id = ? AND kind = 'purchase'`, sqliteTimeNS(base), purchase.Entitlement.ID); err != nil {
		repo.mu.Unlock()
		t.Fatalf("update purchase ledger timestamp: %v", err)
	}
	if _, err := repo.db.Exec(`UPDATE plan_quota_ledger SET created_at_ns = ? WHERE request_id = ? AND client_id = ? AND kind = 'settle'`, sqliteTimeNS(settledAt), "req_plan_quota_actual_day", "client_plan_quota_actual_day"); err != nil {
		repo.mu.Unlock()
		t.Fatalf("update settlement ledger timestamp: %v", err)
	}
	repo.mu.Unlock()

	assertPlanQuotaActualSpendByDay(t, repo, purchase.Entitlement.ID, base, settledAt)
}

func seedPlanQuotaActualSpendScenario(t *testing.T, repo Repository) core.BillingPlanPurchase {
	t.Helper()
	if err := repo.UpsertUser(core.User{ID: "user_plan_quota_actual_day", Username: "actual-day-buyer", Enabled: true, BalanceNanoUSD: usd(20)}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_plan_quota_actual_day", Name: "Actual Day Client", APIKey: "gw_actual_day", OwnerUserID: "user_plan_quota_actual_day", Enabled: true}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "actual_day_week",
		Name:               "Actual Day Week",
		Enabled:            true,
		PriceNanoUSD:       usd(10),
		PeriodQuotaNanoUSD: usd(100),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
	purchase, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_quota_actual_day", PlanID: "actual_day_week"})
	if err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_plan_quota_actual_day",
		ClientID:        "client_plan_quota_actual_day",
		UserID:          "user_plan_quota_actual_day",
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: usd(80),
		Fingerprint:     "plan-quota-actual-day",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_plan_quota_actual_day",
		ClientID:      "client_plan_quota_actual_day",
		ActualNanoUSD: usd(60),
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	return purchase
}

func assertPlanQuotaActualSpendByDay(t *testing.T, repo Repository, entitlementID string, startedAt, settledAt time.Time) {
	t.Helper()
	query := PlanQuotaUsageQuery{
		UserID:        "actual-day-buyer",
		PlanID:        "actual_day_week",
		EntitlementID: entitlementID,
		StartedAt:     startedAt.Add(-time.Hour),
		EndedAt:       settledAt.Add(24 * time.Hour),
		Limit:         10,
	}
	rows, total := repo.ListPlanQuotaUsageByDay(query)
	if total != 1 || len(rows) != 1 {
		t.Fatalf("usage rows total=%d len=%d rows=%#v, want one request-day row", total, len(rows), rows)
	}
	row := rows[0]
	if row.Date != billingDayKey(startedAt) || row.UserID != "user_plan_quota_actual_day" || row.Username != "actual-day-buyer" || row.PlanID != "actual_day_week" || row.PlanName != "Actual Day Week" || row.EntitlementID != entitlementID {
		t.Fatalf("usage row identity = %#v, want request-day entitlement metadata", row)
	}
	if row.GrantedNanoUSD != usd(100) || row.UsedNanoUSD != usd(60) || row.ReturnedNanoUSD != 0 || row.ExpiredNanoUSD != 0 || row.NetNanoUSD != usd(40) {
		t.Fatalf("usage row amounts = %#v, want granted 100 used actual 60 returned 0 expired 0 net 40", row)
	}
	stats := repo.PlanQuotaUsageStats(query)
	if stats.GrantedNanoUSD != usd(100) || stats.UsedNanoUSD != usd(60) || stats.ReturnedNanoUSD != 0 || stats.ExpiredNanoUSD != 0 || stats.NetNanoUSD != usd(40) {
		t.Fatalf("usage stats = %#v, want granted 100 used actual 60 returned 0 expired 0 net 40", stats)
	}
}

func runPlanQuotaUsageByDay(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_plan_quota_usage"
	clientID := "client_plan_quota_usage"
	planID := "usage_week"
	if err := repo.UpsertUser(core.User{ID: userID, Username: "quota-buyer", Enabled: true, BalanceNanoUSD: usd(20)}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: clientID, Name: "Quota Usage", APIKey: "gw_quota_usage", OwnerUserID: userID, Enabled: true}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 planID,
		Name:               "Usage Week",
		Enabled:            true,
		PriceNanoUSD:       usd(10),
		PeriodQuotaNanoUSD: usd(100),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
	startedAt := time.Now().Add(-time.Minute)
	firstPurchase, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: planID})
	if err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_plan_quota_usage",
		ClientID:        clientID,
		UserID:          userID,
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: usd(80),
		Fingerprint:     "plan-quota-usage",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_plan_quota_usage",
		ClientID:      clientID,
		ActualNanoUSD: usd(60),
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	secondPurchase, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: planID})
	if err != nil {
		t.Fatalf("second PurchaseBillingPlan returned error: %v", err)
	}
	endedAt := time.Now().Add(time.Minute)

	query := PlanQuotaUsageQuery{
		UserID:    "quota-buyer",
		PlanID:    planID,
		StartedAt: startedAt,
		EndedAt:   endedAt,
		Limit:     10,
	}
	rows, total := repo.ListPlanQuotaUsageByDay(query)
	if total != 1 || len(rows) != 1 {
		t.Fatalf("usage rows total=%d len=%d rows=%#v, want one daily row", total, len(rows), rows)
	}
	row := rows[0]
	if row.Date != billingDayKey(time.Now()) || row.UserID != userID || row.Username != "quota-buyer" || row.PlanID != planID || row.PlanName != "Usage Week" || row.EntitlementID != "" {
		t.Fatalf("usage row identity = %#v, want date/user/plan metadata", row)
	}
	if row.GrantedNanoUSD != usd(200) || row.UsedNanoUSD != usd(60) || row.ReturnedNanoUSD != 0 || row.ExpiredNanoUSD != 0 || row.NetNanoUSD != usd(140) {
		t.Fatalf("usage row amounts = %#v, want granted 200 used 60 returned 0 expired 0 net 140", row)
	}
	stats := repo.PlanQuotaUsageStats(query)
	if stats.GrantedNanoUSD != usd(200) || stats.UsedNanoUSD != usd(60) || stats.ReturnedNanoUSD != 0 || stats.ExpiredNanoUSD != 0 || stats.NetNanoUSD != usd(140) {
		t.Fatalf("usage stats = %#v, want granted 200 used 60 returned 0 expired 0 net 140", stats)
	}

	firstQuery := query
	firstQuery.EntitlementID = firstPurchase.Entitlement.ID
	firstRows, firstTotal := repo.ListPlanQuotaUsageByDay(firstQuery)
	if firstTotal != 1 || len(firstRows) != 1 {
		t.Fatalf("first entitlement rows total=%d len=%d rows=%#v, want one daily row", firstTotal, len(firstRows), firstRows)
	}
	firstRow := firstRows[0]
	if firstRow.EntitlementID != firstPurchase.Entitlement.ID || firstRow.GrantedNanoUSD != usd(100) || firstRow.UsedNanoUSD != usd(60) || firstRow.ReturnedNanoUSD != 0 || firstRow.ExpiredNanoUSD != 0 || firstRow.NetNanoUSD != usd(40) {
		t.Fatalf("first entitlement row = %#v, want only first purchase granted 100 used 60 net 40", firstRow)
	}
	firstStats := repo.PlanQuotaUsageStats(firstQuery)
	if firstStats.GrantedNanoUSD != usd(100) || firstStats.UsedNanoUSD != usd(60) || firstStats.ReturnedNanoUSD != 0 || firstStats.ExpiredNanoUSD != 0 || firstStats.NetNanoUSD != usd(40) {
		t.Fatalf("first entitlement stats = %#v, want granted 100 used 60 returned 0 expired 0 net 40", firstStats)
	}

	secondQuery := query
	secondQuery.EntitlementID = secondPurchase.Entitlement.ID
	secondRows, secondTotal := repo.ListPlanQuotaUsageByDay(secondQuery)
	if secondTotal != 1 || len(secondRows) != 1 {
		t.Fatalf("second entitlement rows total=%d len=%d rows=%#v, want one daily row", secondTotal, len(secondRows), secondRows)
	}
	secondRow := secondRows[0]
	if secondRow.EntitlementID != secondPurchase.Entitlement.ID || secondRow.GrantedNanoUSD != usd(100) || secondRow.UsedNanoUSD != 0 || secondRow.ReturnedNanoUSD != 0 || secondRow.ExpiredNanoUSD != 0 || secondRow.NetNanoUSD != usd(100) {
		t.Fatalf("second entitlement row = %#v, want only second purchase granted 100 net 100", secondRow)
	}

	emptyRows, emptyTotal := repo.ListPlanQuotaUsageByDay(PlanQuotaUsageQuery{UserID: "missing-user", StartedAt: startedAt, EndedAt: endedAt, Limit: 10})
	if emptyTotal != 0 || len(emptyRows) != 0 {
		t.Fatalf("missing user rows total=%d rows=%#v, want none", emptyTotal, emptyRows)
	}
}

func runBillingPlanReserveAndSettle(t *testing.T, repo Repository) {
	t.Helper()
	setupBillingPlanTestAccount(t, repo, "user_plan_settle", "client_plan_settle")
	purchase, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_settle", PlanID: "day"})
	if err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	if purchase.BalanceAfterNanoUSD != usd(10) {
		t.Fatalf("balance after purchase = %d, want %d", purchase.BalanceAfterNanoUSD, usd(10))
	}

	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_plan_1",
		ClientID:        "client_plan_settle",
		UserID:          "user_plan_settle",
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: usd(80),
		Fingerprint:     "fp1",
	}); err != nil {
		t.Fatalf("ReserveBilling req_plan_1 returned error: %v", err)
	}
	assertUserBalance(t, repo, "user_plan_settle", usd(10))
	assertActivePlanQuota(t, repo, "user_plan_settle", usd(100))

	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_plan_1",
		ClientID:      "client_plan_settle",
		ActualNanoUSD: usd(60),
	}); err != nil {
		t.Fatalf("SettleBilling req_plan_1 returned error: %v", err)
	}
	assertUserBalance(t, repo, "user_plan_settle", usd(10))
	assertActivePlanQuota(t, repo, "user_plan_settle", usd(40))

	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_plan_2",
		ClientID:        "client_plan_settle",
		UserID:          "user_plan_settle",
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: usd(40),
		Fingerprint:     "fp2",
	}); err != nil {
		t.Fatalf("ReserveBilling req_plan_2 returned error: %v", err)
	}
	assertUserBalance(t, repo, "user_plan_settle", usd(10))
	assertPlanEntitlementStatusQuota(t, repo, "user_plan_settle", core.UserPlanEntitlementActive, usd(40))

	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_plan_2",
		ClientID:      "client_plan_settle",
		ActualNanoUSD: usd(35),
	}); err != nil {
		t.Fatalf("SettleBilling req_plan_2 returned error: %v", err)
	}
	assertUserBalance(t, repo, "user_plan_settle", usd(10))
	assertActivePlanQuota(t, repo, "user_plan_settle", usd(5))

	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_plan_mixed",
		ClientID:        "client_plan_settle",
		UserID:          "user_plan_settle",
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: usd(6),
		Fingerprint:     "mixed",
	}); err != nil {
		t.Fatalf("ReserveBilling req_plan_mixed returned error: %v", err)
	}
	assertUserBalance(t, repo, "user_plan_settle", usd(10))
	assertPlanEntitlementStatusQuota(t, repo, "user_plan_settle", core.UserPlanEntitlementActive, usd(5))

	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_plan_mixed",
		ClientID:      "client_plan_settle",
		ActualNanoUSD: usd(8),
	}); err != nil {
		t.Fatalf("SettleBilling req_plan_mixed returned error: %v", err)
	}
	assertUserBalance(t, repo, "user_plan_settle", usd(10))
	assertPlanEntitlementStatusQuota(t, repo, "user_plan_settle", core.UserPlanEntitlementActive, -usd(3))

	stats := repo.PlanSubscriptionStats(PlanSubscriptionQuery{UserID: "user_plan_settle"})
	if stats.ActiveRemainingNanoUSD != 0 {
		t.Fatalf("active remaining with negative quota = %d, want 0", stats.ActiveRemainingNanoUSD)
	}
	planSummaries := repo.ListPlanSubscriptionPlanSummaries(PlanSubscriptionQuery{UserID: "user_plan_settle"}, 10)
	if len(planSummaries) != 1 || planSummaries[0].ActiveRemainingNanoUSD != 0 {
		t.Fatalf("plan summaries with negative quota = %#v, want one summary remaining 0", planSummaries)
	}

	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_plan_3",
		ClientID:        "client_plan_settle",
		UserID:          "user_plan_settle",
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: usd(8),
		Fingerprint:     "fp3",
	}); !errors.Is(err, ErrPlanQuotaExhausted) {
		t.Fatalf("ReserveBilling req_plan_3 error = %v, want ErrPlanQuotaExhausted", err)
	}
	assertUserBalance(t, repo, "user_plan_settle", usd(10))
	assertPlanEntitlementStatusQuota(t, repo, "user_plan_settle", core.UserPlanEntitlementActive, -usd(3))

	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_cash_1",
		ClientID:        "client_plan_settle",
		UserID:          "user_plan_settle",
		BillingSource:   core.ClientBillingSourceCash,
		ReservedNanoUSD: usd(10),
		Fingerprint:     "cash1",
	}); err != nil {
		t.Fatalf("ReserveBilling req_cash_1 returned error: %v", err)
	}
	assertUserBalance(t, repo, "user_plan_settle", usd(10))
	assertPlanEntitlementStatusQuota(t, repo, "user_plan_settle", core.UserPlanEntitlementActive, -usd(3))

	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_cash_1",
		ClientID:      "client_plan_settle",
		ActualNanoUSD: usd(10),
	}); err != nil {
		t.Fatalf("SettleBilling req_cash_1 returned error: %v", err)
	}
	assertUserBalance(t, repo, "user_plan_settle", 0)
	assertPlanEntitlementStatusQuota(t, repo, "user_plan_settle", core.UserPlanEntitlementActive, -usd(3))

	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_cash_2",
		ClientID:        "client_plan_settle",
		UserID:          "user_plan_settle",
		BillingSource:   core.ClientBillingSourceCash,
		ReservedNanoUSD: usd(50),
		Fingerprint:     "cash2",
	}); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("ReserveBilling req_cash_2 error = %v, want ErrInsufficientBalance", err)
	}
	assertUserBalance(t, repo, "user_plan_settle", 0)
	assertPlanEntitlementStatusQuota(t, repo, "user_plan_settle", core.UserPlanEntitlementActive, -usd(3))

	spend, err := repo.GetClientSpend("client_plan_settle")
	if err != nil {
		t.Fatalf("GetClientSpend returned error: %v", err)
	}
	if spend.SpendUsedNanoUSD != usd(113) {
		t.Fatalf("client spend = %d, want %d", spend.SpendUsedNanoUSD, usd(113))
	}
}

func runRecoversPrematurelyExpiredBillingPlan(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_plan_recover_expired"
	clientID := "client_plan_recover_expired"
	setupBillingPlanTestAccount(t, repo, userID, clientID)
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "month_recover",
		Name:               "Month Recover",
		Enabled:            true,
		PriceNanoUSD:       usd(10),
		PeriodQuotaNanoUSD: usd(100),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        30,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
	purchase, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: "month_recover"})
	if err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}

	now := time.Now().UTC()
	forcePrematurelyExpiredPlanEntitlement(t, repo, purchase.Entitlement.ID, now.Add(-time.Hour), now.Add(time.Hour), now.Add(29*24*time.Hour), 29, -usd(1))

	active, err := repo.GetActiveUserPlanEntitlement(userID)
	if err != nil {
		t.Fatalf("GetActiveUserPlanEntitlement returned error: %v", err)
	}
	if active.ID != purchase.Entitlement.ID {
		t.Fatalf("active entitlement id = %q, want %q", active.ID, purchase.Entitlement.ID)
	}
	if active.Status != core.UserPlanEntitlementActive || active.RemainingPeriods != 29 || active.CurrentQuotaNanoUSD != -usd(1) {
		t.Fatalf("recovered entitlement = %#v, want active remaining 29 quota %d", active, -usd(1))
	}

	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_plan_recover_expired",
		ClientID:        clientID,
		UserID:          userID,
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: 1,
		Fingerprint:     "recover-expired",
	}); !errors.Is(err, ErrPlanQuotaExhausted) {
		t.Fatalf("ReserveBilling error = %v, want ErrPlanQuotaExhausted", err)
	}
	assertPlanEntitlementStatusQuota(t, repo, userID, core.UserPlanEntitlementActive, -usd(1))
}

func runBillingPlanPriorityMoveSelectsActivePlan(t *testing.T, repo Repository) {
	t.Helper()
	setupBillingPlanTestAccount(t, repo, "user_plan_repurchase", "client_plan_repurchase")
	first, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_repurchase", PlanID: "day"})
	if err != nil {
		t.Fatalf("first PurchaseBillingPlan returned error: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_plan_repurchase_low",
		ClientID:        "client_plan_repurchase",
		UserID:          "user_plan_repurchase",
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: usd(95),
		Fingerprint:     "repurchase-low",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_plan_repurchase_low",
		ClientID:      "client_plan_repurchase",
		ActualNanoUSD: usd(95),
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	assertActivePlanQuota(t, repo, "user_plan_repurchase", usd(5))

	second, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_repurchase", PlanID: "day"})
	if err != nil {
		t.Fatalf("second PurchaseBillingPlan returned error: %v", err)
	}
	if second.BalanceAfterNanoUSD != 0 {
		t.Fatalf("balance after second purchase = %d, want 0", second.BalanceAfterNanoUSD)
	}
	entitlements := repo.ListUserPlanEntitlements("user_plan_repurchase")
	if len(entitlements) != 2 {
		t.Fatalf("entitlements len = %d, want 2", len(entitlements))
	}
	active, err := repo.GetActiveUserPlanEntitlement("user_plan_repurchase")
	if err != nil {
		t.Fatalf("GetActiveUserPlanEntitlement returned error: %v", err)
	}
	if active.ID != first.Entitlement.ID {
		t.Fatalf("active entitlement id = %q, want first purchase %q", active.ID, first.Entitlement.ID)
	}
	if err := repo.MoveUserPlanEntitlementPriority("user_plan_repurchase", second.Entitlement.ID, "up"); err != nil {
		t.Fatalf("MoveUserPlanEntitlementPriority returned error: %v", err)
	}
	active, err = repo.GetActiveUserPlanEntitlement("user_plan_repurchase")
	if err != nil {
		t.Fatalf("GetActiveUserPlanEntitlement after move returned error: %v", err)
	}
	if active.ID != second.Entitlement.ID {
		t.Fatalf("active entitlement after move = %q, want second purchase %q", active.ID, second.Entitlement.ID)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_plan_repurchase_priority",
		ClientID:        "client_plan_repurchase",
		UserID:          "user_plan_repurchase",
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: usd(10),
		Fingerprint:     "repurchase-priority",
	}); err != nil {
		t.Fatalf("ReserveBilling after move returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_plan_repurchase_priority",
		ClientID:      "client_plan_repurchase",
		ActualNanoUSD: usd(10),
	}); err != nil {
		t.Fatalf("SettleBilling after move returned error: %v", err)
	}
	entitlements = repo.ListUserPlanEntitlements("user_plan_repurchase")
	quotaByID := map[string]int64{}
	for _, entitlement := range entitlements {
		quotaByID[entitlement.ID] = entitlement.CurrentQuotaNanoUSD
	}
	if quotaByID[second.Entitlement.ID] != usd(90) {
		t.Fatalf("second quota after priority reserve = %d, want %d", quotaByID[second.Entitlement.ID], usd(90))
	}
	if quotaByID[first.Entitlement.ID] != usd(5) {
		t.Fatalf("first quota after priority reserve = %d, want %d", quotaByID[first.Entitlement.ID], usd(5))
	}
}

func runBillingPlanReservationSkipsInsufficientPriorityPlan(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_plan_skip_insufficient"
	clientID := "client_plan_skip_insufficient"
	setupBillingPlanTestAccount(t, repo, userID, clientID)
	first, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: "day"})
	if err != nil {
		t.Fatalf("first PurchaseBillingPlan returned error: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_plan_skip_first_low",
		ClientID:        clientID,
		UserID:          userID,
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: usd(95),
		Fingerprint:     "skip-first-low",
	}); err != nil {
		t.Fatalf("ReserveBilling first returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_plan_skip_first_low",
		ClientID:      clientID,
		ActualNanoUSD: usd(95),
	}); err != nil {
		t.Fatalf("SettleBilling first returned error: %v", err)
	}
	second, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: "day"})
	if err != nil {
		t.Fatalf("second PurchaseBillingPlan returned error: %v", err)
	}
	active, err := repo.GetActiveUserPlanEntitlement(userID)
	if err != nil {
		t.Fatalf("GetActiveUserPlanEntitlement returned error: %v", err)
	}
	if active.ID != first.Entitlement.ID {
		t.Fatalf("active entitlement = %q, want first entitlement %q", active.ID, first.Entitlement.ID)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_plan_skip_second",
		ClientID:        clientID,
		UserID:          userID,
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: usd(10),
		Fingerprint:     "skip-second",
	}); err != nil {
		t.Fatalf("ReserveBilling second returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_plan_skip_second",
		ClientID:      clientID,
		ActualNanoUSD: usd(10),
	}); err != nil {
		t.Fatalf("SettleBilling second returned error: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_plan_skip_third",
		ClientID:        clientID,
		UserID:          userID,
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: usd(10),
		Fingerprint:     "skip-third",
	}); err != nil {
		t.Fatalf("ReserveBilling third returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_plan_skip_third",
		ClientID:      clientID,
		ActualNanoUSD: usd(10),
	}); err != nil {
		t.Fatalf("SettleBilling third returned error: %v", err)
	}

	entitlements := repo.ListUserPlanEntitlements(userID)
	quotaByID := make(map[string]int64, len(entitlements))
	for _, entitlement := range entitlements {
		quotaByID[entitlement.ID] = entitlement.CurrentQuotaNanoUSD
	}
	if quotaByID[first.Entitlement.ID] != -usd(5) {
		t.Fatalf("first quota after overrun settle = %d, want %d", quotaByID[first.Entitlement.ID], -usd(5))
	}
	if quotaByID[second.Entitlement.ID] != usd(90) {
		t.Fatalf("second quota after fallback settle = %d, want %d", quotaByID[second.Entitlement.ID], usd(90))
	}
}

func runBillingPlanPurchaseMergeQuota(t *testing.T, repo Repository) {
	t.Helper()
	setupBillingPlanTestAccount(t, repo, "user_plan_merge_quota", "client_plan_merge_quota")
	first, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_merge_quota", PlanID: "day"})
	if err != nil {
		t.Fatalf("first PurchaseBillingPlan returned error: %v", err)
	}
	second, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{
		UserID: "user_plan_merge_quota",
		PlanID: "day",
		Mode:   core.BillingPlanPurchaseMergeQuota,
	})
	if err != nil {
		t.Fatalf("merge PurchaseBillingPlan returned error: %v", err)
	}
	if second.Entitlement.ID != first.Entitlement.ID {
		t.Fatalf("merged entitlement id = %q, want %q", second.Entitlement.ID, first.Entitlement.ID)
	}
	if second.BalanceAfterNanoUSD != 0 {
		t.Fatalf("balance after merge purchase = %d, want 0", second.BalanceAfterNanoUSD)
	}
	entitlements := repo.ListUserPlanEntitlements("user_plan_merge_quota")
	if len(entitlements) != 1 {
		t.Fatalf("entitlements len = %d, want 1", len(entitlements))
	}
	active, err := repo.GetActiveUserPlanEntitlement("user_plan_merge_quota")
	if err != nil {
		t.Fatalf("GetActiveUserPlanEntitlement returned error: %v", err)
	}
	if active.CurrentQuotaNanoUSD != usd(200) {
		t.Fatalf("merged quota = %d, want %d", active.CurrentQuotaNanoUSD, usd(200))
	}
	if active.PeriodQuotaNanoUSD != usd(200) {
		t.Fatalf("period quota = %d, want %d", active.PeriodQuotaNanoUSD, usd(200))
	}
}

func runBillingPlanGrantDoesNotChargeBalance(t *testing.T, repo Repository) {
	t.Helper()
	setupBillingPlanTestAccount(t, repo, "user_plan_grant", "client_plan_grant")

	purchase, err := repo.GrantBillingPlan(core.BillingPlanGrantInput{
		UserID: "user_plan_grant",
		PlanID: "day",
		Note:   "review test",
	})
	if err != nil {
		t.Fatalf("GrantBillingPlan returned error: %v", err)
	}
	if purchase.BalanceBeforeNanoUSD != usd(20) || purchase.BalanceAfterNanoUSD != usd(20) {
		t.Fatalf("grant balance before/after = %d/%d, want unchanged %d", purchase.BalanceBeforeNanoUSD, purchase.BalanceAfterNanoUSD, usd(20))
	}
	if purchase.Entitlement.PriceNanoUSD != 0 {
		t.Fatalf("grant entitlement price = %d, want 0", purchase.Entitlement.PriceNanoUSD)
	}
	if purchase.Entitlement.CurrentQuotaNanoUSD != usd(100) {
		t.Fatalf("grant quota = %d, want %d", purchase.Entitlement.CurrentQuotaNanoUSD, usd(100))
	}
	assertUserBalance(t, repo, "user_plan_grant", usd(20))
	assertActivePlanQuota(t, repo, "user_plan_grant", usd(100))
}

func runBillingPlanPurchaseMergeQuotaTarget(t *testing.T, repo Repository) {
	t.Helper()
	setupBillingPlanTestAccount(t, repo, "user_plan_merge_targeted", "client_plan_merge_targeted")
	if err := repo.SetUserBalance("user_plan_merge_targeted", usd(30)); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}
	first, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_merge_targeted", PlanID: "day"})
	if err != nil {
		t.Fatalf("first PurchaseBillingPlan returned error: %v", err)
	}
	second, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_merge_targeted", PlanID: "day"})
	if err != nil {
		t.Fatalf("second PurchaseBillingPlan returned error: %v", err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{
		UserID:              "user_plan_merge_targeted",
		PlanID:              "day",
		Mode:                core.BillingPlanPurchaseMergeQuota,
		TargetEntitlementID: second.Entitlement.ID,
	}); err != nil {
		t.Fatalf("targeted merge PurchaseBillingPlan returned error: %v", err)
	}
	entitlements := repo.ListUserPlanEntitlements("user_plan_merge_targeted")
	quotaByID := map[string]int64{}
	for _, entitlement := range entitlements {
		quotaByID[entitlement.ID] = entitlement.CurrentQuotaNanoUSD
	}
	if quotaByID[first.Entitlement.ID] != usd(100) {
		t.Fatalf("first quota after targeted merge = %d, want %d", quotaByID[first.Entitlement.ID], usd(100))
	}
	if quotaByID[second.Entitlement.ID] != usd(200) {
		t.Fatalf("second quota after targeted merge = %d, want %d", quotaByID[second.Entitlement.ID], usd(200))
	}
}

func runBillingPlanPurchaseMergeQuotaAllowsRepeatedSamePlan(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_plan_merge_repeated"
	setupBillingPlanTestAccount(t, repo, userID, "client_plan_merge_repeated")
	if err := repo.SetUserBalance(userID, usd(40)); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}
	first, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: "day"})
	if err != nil {
		t.Fatalf("first PurchaseBillingPlan returned error: %v", err)
	}
	for i := 0; i < 2; i++ {
		purchase, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{
			UserID: userID,
			PlanID: "day",
			Mode:   core.BillingPlanPurchaseMergeQuota,
		})
		if err != nil {
			t.Fatalf("merge purchase %d returned error: %v", i+1, err)
		}
		if purchase.Entitlement.ID != first.Entitlement.ID {
			t.Fatalf("merge purchase %d entitlement = %q, want %q", i+1, purchase.Entitlement.ID, first.Entitlement.ID)
		}
	}
	active, err := repo.GetActiveUserPlanEntitlement(userID)
	if err != nil {
		t.Fatalf("GetActiveUserPlanEntitlement returned error: %v", err)
	}
	if active.CurrentQuotaNanoUSD != usd(300) {
		t.Fatalf("repeated merged current quota = %d, want %d", active.CurrentQuotaNanoUSD, usd(300))
	}
	if active.PeriodQuotaNanoUSD != usd(300) {
		t.Fatalf("repeated merged period quota = %d, want %d", active.PeriodQuotaNanoUSD, usd(300))
	}
}

func runBillingPlanPurchaseMergeQuotaResetsToBaseQuota(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_plan_merge_reset"
	clientID := "client_plan_merge_reset"
	setupBillingPlanTestAccount(t, repo, userID, clientID)
	if err := repo.SetUserBalance(userID, usd(70)); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "week_merge_reset",
		Name:               "Week Merge Reset",
		Enabled:            true,
		PriceNanoUSD:       usd(10),
		PeriodQuotaNanoUSD: usd(100),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
	first, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: "week_merge_reset"})
	if err != nil {
		t.Fatalf("first PurchaseBillingPlan returned error: %v", err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{
		UserID: userID,
		PlanID: "week_merge_reset",
		Mode:   core.BillingPlanPurchaseMergeQuota,
	}); err != nil {
		t.Fatalf("merge PurchaseBillingPlan returned error: %v", err)
	}
	active, err := repo.GetActiveUserPlanEntitlement(userID)
	if err != nil {
		t.Fatalf("GetActiveUserPlanEntitlement after merge returned error: %v", err)
	}
	if active.CurrentQuotaNanoUSD != usd(200) || active.PeriodQuotaNanoUSD != usd(200) {
		t.Fatalf("merged quota current/period = %d/%d, want 200/200", active.CurrentQuotaNanoUSD, active.PeriodQuotaNanoUSD)
	}
	beforeEnd := time.Now().UTC().Add(time.Hour).Round(0)
	forcePlanWindow(t, repo, first.Entitlement.ID, beforeEnd.Add(-23*time.Hour), beforeEnd, beforeEnd.Add(6*24*time.Hour), 7, usd(200))
	forcePlanPeriodEnded(t, repo, first.Entitlement.ID)
	active, err = repo.GetActiveUserPlanEntitlement(userID)
	if err != nil {
		t.Fatalf("GetActiveUserPlanEntitlement after reset returned error: %v", err)
	}
	if active.CurrentQuotaNanoUSD != usd(100) {
		t.Fatalf("quota after reset = %d, want base %d", active.CurrentQuotaNanoUSD, usd(100))
	}
	if active.PeriodQuotaNanoUSD != usd(100) {
		t.Fatalf("period quota after reset = %d, want base %d", active.PeriodQuotaNanoUSD, usd(100))
	}
	if active.RemainingPeriods != 6 {
		t.Fatalf("remaining periods after reset = %d, want 6", active.RemainingPeriods)
	}
}

func runBillingPlanPurchaseMergeQuotaAllowsPriceChange(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_plan_merge_price_change"
	setupBillingPlanTestAccount(t, repo, userID, "client_plan_merge_price_change")
	if err := repo.SetUserBalance(userID, usd(40)); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}
	first, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: "day"})
	if err != nil {
		t.Fatalf("first PurchaseBillingPlan returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "day",
		Name:               "Day",
		Enabled:            true,
		PriceNanoUSD:       usd(5),
		PeriodQuotaNanoUSD: usd(100),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan discounted returned error: %v", err)
	}
	merged, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{
		UserID:              userID,
		PlanID:              "day",
		Mode:                core.BillingPlanPurchaseMergeQuota,
		TargetEntitlementID: first.Entitlement.ID,
	})
	if err != nil {
		t.Fatalf("merge after price change returned error: %v", err)
	}
	if merged.Entitlement.ID != first.Entitlement.ID {
		t.Fatalf("merged entitlement id = %q, want %q", merged.Entitlement.ID, first.Entitlement.ID)
	}
	if merged.Entitlement.CurrentQuotaNanoUSD != usd(200) {
		t.Fatalf("merged quota after price change = %d, want %d", merged.Entitlement.CurrentQuotaNanoUSD, usd(200))
	}
}

func runBillingPlanPurchaseMergeQuotaRejectsQuotaMismatch(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_plan_merge_mismatch"
	clientID := "client_plan_merge_mismatch"
	setupBillingPlanTestAccount(t, repo, userID, clientID)
	if err := repo.SetUserBalance(userID, usd(40)); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "day",
		Name:               "Day",
		Enabled:            true,
		PriceNanoUSD:       usd(20),
		PeriodQuotaNanoUSD: usd(200),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan 20 returned error: %v", err)
	}
	first, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: "day"})
	if err != nil {
		t.Fatalf("first PurchaseBillingPlan returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "day",
		Name:               "Day",
		Enabled:            true,
		PriceNanoUSD:       usd(10),
		PeriodQuotaNanoUSD: usd(100),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan 10 returned error: %v", err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{
		UserID:              userID,
		PlanID:              "day",
		Mode:                core.BillingPlanPurchaseMergeQuota,
		TargetEntitlementID: first.Entitlement.ID,
	}); err == nil {
		t.Fatal("targeted merge returned nil error, want quota mismatch rejection")
	}
}

func runBillingPlanPurchaseExtendPeriod(t *testing.T, repo Repository) {
	t.Helper()
	setupBillingPlanTestAccount(t, repo, "user_plan_extend_period", "client_plan_extend_period")
	purchase, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_extend_period", PlanID: "day"})
	if err != nil {
		t.Fatalf("first PurchaseBillingPlan returned error: %v", err)
	}
	beforeEnd := time.Now().UTC().Add(time.Hour).Round(0)
	forcePlanWindow(t, repo, purchase.Entitlement.ID, beforeEnd.Add(-23*time.Hour), beforeEnd, beforeEnd, 1, usd(100))
	extended, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{
		UserID: "user_plan_extend_period",
		PlanID: "day",
		Mode:   core.BillingPlanPurchaseExtendPeriod,
	})
	if err != nil {
		t.Fatalf("extend PurchaseBillingPlan returned error: %v", err)
	}
	if extended.Entitlement.ID != purchase.Entitlement.ID {
		t.Fatalf("extended entitlement id = %q, want %q", extended.Entitlement.ID, purchase.Entitlement.ID)
	}
	active, err := repo.GetActiveUserPlanEntitlement("user_plan_extend_period")
	if err != nil {
		t.Fatalf("GetActiveUserPlanEntitlement returned error: %v", err)
	}
	wantEnd := beforeEnd.Add(24 * time.Hour)
	if !active.CurrentPeriodEndsAt.Equal(beforeEnd) {
		t.Fatalf("period end = %s, want current window end %s", active.CurrentPeriodEndsAt, beforeEnd)
	}
	if !active.ExpiresAt.Equal(wantEnd) {
		t.Fatalf("expires at = %s, want %s", active.ExpiresAt, wantEnd)
	}
	if active.RemainingPeriods != 2 {
		t.Fatalf("remaining periods = %d, want 2", active.RemainingPeriods)
	}
	if active.CurrentQuotaNanoUSD != usd(100) {
		t.Fatalf("current quota = %d, want %d", active.CurrentQuotaNanoUSD, usd(100))
	}
	entitlements := repo.ListUserPlanEntitlements("user_plan_extend_period")
	if len(entitlements) != 1 {
		t.Fatalf("entitlements len = %d, want 1", len(entitlements))
	}
}

func runBillingPlanPurchaseExtendPeriodTarget(t *testing.T, repo Repository) {
	t.Helper()
	setupBillingPlanTestAccount(t, repo, "user_plan_extend_targeted", "client_plan_extend_targeted")
	if err := repo.SetUserBalance("user_plan_extend_targeted", usd(30)); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}
	first, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_extend_targeted", PlanID: "day"})
	if err != nil {
		t.Fatalf("first PurchaseBillingPlan returned error: %v", err)
	}
	second, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_extend_targeted", PlanID: "day"})
	if err != nil {
		t.Fatalf("second PurchaseBillingPlan returned error: %v", err)
	}
	beforeEnd := time.Now().UTC().Add(time.Hour).Round(0)
	forcePlanWindow(t, repo, first.Entitlement.ID, beforeEnd.Add(-23*time.Hour), beforeEnd, beforeEnd, 1, usd(100))
	secondEnd := beforeEnd.Add(time.Hour)
	forcePlanWindow(t, repo, second.Entitlement.ID, secondEnd.Add(-23*time.Hour), secondEnd, secondEnd, 1, usd(100))
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{
		UserID:              "user_plan_extend_targeted",
		PlanID:              "day",
		Mode:                core.BillingPlanPurchaseExtendPeriod,
		TargetEntitlementID: second.Entitlement.ID,
	}); err != nil {
		t.Fatalf("targeted extend PurchaseBillingPlan returned error: %v", err)
	}
	entitlements := repo.ListUserPlanEntitlements("user_plan_extend_targeted")
	expiresByID := map[string]time.Time{}
	for _, entitlement := range entitlements {
		expiresByID[entitlement.ID] = entitlement.ExpiresAt
	}
	if !expiresByID[first.Entitlement.ID].Equal(beforeEnd) {
		t.Fatalf("first expires after targeted extend = %s, want %s", expiresByID[first.Entitlement.ID], beforeEnd)
	}
	wantSecondExpires := secondEnd.Add(24 * time.Hour)
	if !expiresByID[second.Entitlement.ID].Equal(wantSecondExpires) {
		t.Fatalf("second expires after targeted extend = %s, want %s", expiresByID[second.Entitlement.ID], wantSecondExpires)
	}
}

func runBillingPlanPurchaseExtendPeriodAllowsPriceChange(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_plan_extend_price_change"
	setupBillingPlanTestAccount(t, repo, userID, "client_plan_extend_price_change")
	if err := repo.SetUserBalance(userID, usd(40)); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}
	first, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: "day"})
	if err != nil {
		t.Fatalf("first PurchaseBillingPlan returned error: %v", err)
	}
	beforeEnd := time.Now().UTC().Add(time.Hour).Round(0)
	forcePlanWindow(t, repo, first.Entitlement.ID, beforeEnd.Add(-23*time.Hour), beforeEnd, beforeEnd, 1, usd(100))
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "day",
		Name:               "Day",
		Enabled:            true,
		PriceNanoUSD:       usd(5),
		PeriodQuotaNanoUSD: usd(100),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan discounted returned error: %v", err)
	}
	extended, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{
		UserID:              userID,
		PlanID:              "day",
		Mode:                core.BillingPlanPurchaseExtendPeriod,
		TargetEntitlementID: first.Entitlement.ID,
	})
	if err != nil {
		t.Fatalf("extend after price change returned error: %v", err)
	}
	if extended.Entitlement.ID != first.Entitlement.ID {
		t.Fatalf("extended entitlement id = %q, want %q", extended.Entitlement.ID, first.Entitlement.ID)
	}
	wantEnd := beforeEnd.Add(24 * time.Hour)
	if !extended.Entitlement.ExpiresAt.Equal(wantEnd) {
		t.Fatalf("expires after price change extend = %s, want %s", extended.Entitlement.ExpiresAt, wantEnd)
	}
}

func runBillingPlanPurchaseExtendPeriodRejectsQuotaMismatch(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_plan_extend_mismatch"
	clientID := "client_plan_extend_mismatch"
	setupBillingPlanTestAccount(t, repo, userID, clientID)
	if err := repo.SetUserBalance(userID, usd(40)); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "day",
		Name:               "Day",
		Enabled:            true,
		PriceNanoUSD:       usd(20),
		PeriodQuotaNanoUSD: usd(200),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan 20 returned error: %v", err)
	}
	first, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: "day"})
	if err != nil {
		t.Fatalf("first PurchaseBillingPlan returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "day",
		Name:               "Day",
		Enabled:            true,
		PriceNanoUSD:       usd(10),
		PeriodQuotaNanoUSD: usd(100),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan 10 returned error: %v", err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{
		UserID:              userID,
		PlanID:              "day",
		Mode:                core.BillingPlanPurchaseExtendPeriod,
		TargetEntitlementID: first.Entitlement.ID,
	}); err == nil {
		t.Fatal("targeted extend returned nil error, want quota mismatch rejection")
	}
}

func runBillingPlanPurchaseExtendPeriodRejectsMergedQuota(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_plan_extend_merged_quota"
	setupBillingPlanTestAccount(t, repo, userID, "client_plan_extend_merged_quota")
	if err := repo.SetUserBalance(userID, usd(40)); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}
	first, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: "day"})
	if err != nil {
		t.Fatalf("first PurchaseBillingPlan returned error: %v", err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{
		UserID: userID,
		PlanID: "day",
		Mode:   core.BillingPlanPurchaseMergeQuota,
	}); err != nil {
		t.Fatalf("merge PurchaseBillingPlan returned error: %v", err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{
		UserID:              userID,
		PlanID:              "day",
		Mode:                core.BillingPlanPurchaseExtendPeriod,
		TargetEntitlementID: first.Entitlement.ID,
	}); err == nil {
		t.Fatal("targeted extend returned nil error, want merged quota rejection")
	}
}

func runPlanSubscriptionQueries(t *testing.T, repo Repository) {
	t.Helper()
	if err := repo.UpsertBillingPlanGroup(core.BillingPlanGroup{ID: "group_subscription_query", Name: "Subscription Query", SortOrder: 10}); err != nil {
		t.Fatalf("UpsertBillingPlanGroup returned error: %v", err)
	}
	for _, plan := range []core.BillingPlan{
		{
			ID:                 "subscription_week",
			Name:               "Subscription Week",
			Group:              "group_subscription_query",
			Enabled:            true,
			PriceNanoUSD:       usd(10),
			PeriodQuotaNanoUSD: usd(100),
			PeriodDurationSec:  24 * 60 * 60,
			PeriodCount:        7,
		},
		{
			ID:                 "subscription_month",
			Name:               "Subscription Month",
			Group:              "group_subscription_query",
			Enabled:            true,
			PriceNanoUSD:       usd(50),
			PeriodQuotaNanoUSD: usd(200),
			PeriodDurationSec:  24 * 60 * 60,
			PeriodCount:        30,
		},
	} {
		if err := repo.UpsertBillingPlan(plan); err != nil {
			t.Fatalf("UpsertBillingPlan(%s) returned error: %v", plan.ID, err)
		}
	}
	for _, user := range []core.User{
		{ID: "user_subscription_active", Username: "active-buyer", Enabled: true, BalanceNanoUSD: usd(100)},
		{ID: "user_subscription_cancelled", Username: "cancelled-buyer", Enabled: true, BalanceNanoUSD: usd(100)},
		{ID: "user_subscription_expired", Username: "expired-buyer", Enabled: true, BalanceNanoUSD: usd(100)},
	} {
		if err := repo.UpsertUser(user); err != nil {
			t.Fatalf("UpsertUser(%s) returned error: %v", user.ID, err)
		}
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_subscription_active", Name: "Subscription Active", APIKey: "gw_subscription_active", OwnerUserID: "user_subscription_active", Enabled: true}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}

	activePurchase, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_subscription_active", PlanID: "subscription_week"})
	if err != nil {
		t.Fatalf("PurchaseBillingPlan active returned error: %v", err)
	}
	cancelledPurchase, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_subscription_cancelled", PlanID: "subscription_week"})
	if err != nil {
		t.Fatalf("PurchaseBillingPlan cancelled returned error: %v", err)
	}
	expiredPurchase, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_subscription_expired", PlanID: "subscription_month"})
	if err != nil {
		t.Fatalf("PurchaseBillingPlan expired returned error: %v", err)
	}

	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_subscription_active",
		ClientID:        "client_subscription_active",
		UserID:          "user_subscription_active",
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: usd(40),
		Fingerprint:     "subscription-active",
	}); err != nil {
		t.Fatalf("ReserveBilling active returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_subscription_active",
		ClientID:      "client_subscription_active",
		ActualNanoUSD: usd(40),
	}); err != nil {
		t.Fatalf("SettleBilling active returned error: %v", err)
	}
	forcePlanSubscriptionStatus(t, repo, cancelledPurchase.Entitlement.ID, core.UserPlanEntitlementCancelled)
	forcePlanSubscriptionStatus(t, repo, expiredPurchase.Entitlement.ID, core.UserPlanEntitlementExpired)

	stats := repo.PlanSubscriptionStats(PlanSubscriptionQuery{})
	if stats.TotalCount != 3 || stats.ActiveCount != 1 || stats.ExpiredCount != 1 || stats.CancelledCount != 1 {
		t.Fatalf("stats counts = %#v, want total 3 active 1 expired 1 cancelled 1", stats)
	}
	if stats.RevenueNanoUSD != usd(70) || stats.ActiveRemainingNanoUSD != usd(60) || stats.ActiveUsedNanoUSD != usd(40) {
		t.Fatalf("stats amounts = %#v, want revenue 70 remaining 60 used 40", stats)
	}

	planSummaries := repo.ListPlanSubscriptionPlanSummaries(PlanSubscriptionQuery{}, 10)
	if len(planSummaries) != 2 {
		t.Fatalf("plan summaries len = %d, want 2: %#v", len(planSummaries), planSummaries)
	}
	if planSummaries[0].PlanID != "subscription_month" || planSummaries[0].PurchaseCount != 1 || planSummaries[0].RevenueNanoUSD != usd(50) {
		t.Fatalf("first plan summary = %#v, want subscription_month revenue first", planSummaries[0])
	}
	if planSummaries[1].PlanID != "subscription_week" || planSummaries[1].PurchaseCount != 2 || planSummaries[1].ActiveCount != 1 || planSummaries[1].ActiveRemainingNanoUSD != usd(60) {
		t.Fatalf("second plan summary = %#v, want subscription_week purchases 2 active 1 remaining 60", planSummaries[1])
	}

	activeRows, activeTotal := repo.ListPlanSubscriptionSummariesPage(PlanSubscriptionQuery{Status: core.UserPlanEntitlementActive, Limit: 10})
	if activeTotal != 1 || len(activeRows) != 1 || activeRows[0].Entitlement.ID != activePurchase.Entitlement.ID {
		t.Fatalf("active rows total=%d rows=%#v, want active purchase", activeTotal, activeRows)
	}
	if activeRows[0].PlanGroup != "group_subscription_query" || activeRows[0].Username != "active-buyer" || activeRows[0].UserBalanceNanoUSD != usd(90) {
		t.Fatalf("active row metadata = %#v, want group, username and balance", activeRows[0])
	}

	weekStats := repo.PlanSubscriptionStats(PlanSubscriptionQuery{PlanID: "subscription_week"})
	if weekStats.TotalCount != 2 || weekStats.ActiveCount != 1 || weekStats.CancelledCount != 1 || weekStats.RevenueNanoUSD != usd(20) {
		t.Fatalf("week stats = %#v, want two week purchases and revenue 20", weekStats)
	}
	userRows, userTotal := repo.ListPlanSubscriptionSummariesPage(PlanSubscriptionQuery{UserID: "user_subscription_expired", Limit: 10})
	if userTotal != 1 || len(userRows) != 1 || userRows[0].Entitlement.Status != core.UserPlanEntitlementExpired {
		t.Fatalf("expired user rows total=%d rows=%#v, want expired purchase", userTotal, userRows)
	}
	usernameRows, usernameTotal := repo.ListPlanSubscriptionSummariesPage(PlanSubscriptionQuery{UserID: "expired-buyer", Limit: 10})
	if usernameTotal != 1 || len(usernameRows) != 1 || usernameRows[0].Entitlement.UserID != "user_subscription_expired" {
		t.Fatalf("username rows total=%d rows=%#v, want expired buyer purchase", usernameTotal, usernameRows)
	}
	usernameStats := repo.PlanSubscriptionStats(PlanSubscriptionQuery{UserID: "expired-buyer"})
	if usernameStats.TotalCount != 1 || usernameStats.ExpiredCount != 1 {
		t.Fatalf("username stats = %#v, want one expired purchase", usernameStats)
	}
	usernamePlanSummaries := repo.ListPlanSubscriptionPlanSummaries(PlanSubscriptionQuery{UserID: "expired-buyer"}, 10)
	if len(usernamePlanSummaries) != 1 || usernamePlanSummaries[0].PlanID != "subscription_month" {
		t.Fatalf("username plan summaries = %#v, want subscription_month", usernamePlanSummaries)
	}
	pageRows, pageTotal := repo.ListPlanSubscriptionSummariesPage(PlanSubscriptionQuery{Limit: 2})
	if pageTotal != 3 || len(pageRows) != 2 {
		t.Fatalf("paged rows total=%d len=%d rows=%#v, want total 3 page len 2", pageTotal, len(pageRows), pageRows)
	}
}

func runBillingPlanReleaseRestoresQuota(t *testing.T, repo Repository) {
	t.Helper()
	setupBillingPlanTestAccount(t, repo, "user_plan_release", "client_plan_release")
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_release", PlanID: "day"}); err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_plan_release",
		ClientID:        "client_plan_release",
		UserID:          "user_plan_release",
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: usd(80),
		Fingerprint:     "release",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if err := repo.ReleaseBilling(core.BillingReleaseInput{
		RequestID: "req_plan_release",
		ClientID:  "client_plan_release",
		Reason:    "test",
	}); err != nil {
		t.Fatalf("ReleaseBilling returned error: %v", err)
	}
	assertUserBalance(t, repo, "user_plan_release", usd(10))
	assertActivePlanQuota(t, repo, "user_plan_release", usd(100))
	spend, err := repo.GetClientSpend("client_plan_release")
	if err != nil {
		t.Fatalf("GetClientSpend returned error: %v", err)
	}
	if spend.SpendUsedNanoUSD != 0 {
		t.Fatalf("client spend = %d, want 0", spend.SpendUsedNanoUSD)
	}
}

func runDeleteClientReleasesReservedBillingPlanQuota(t *testing.T, repo Repository) {
	t.Helper()
	setupBillingPlanTestAccount(t, repo, "user_plan_delete", "client_plan_delete")
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_delete", PlanID: "day"}); err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_plan_delete",
		ClientID:        "client_plan_delete",
		UserID:          "user_plan_delete",
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: usd(100),
		Fingerprint:     "delete",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	assertUserBalance(t, repo, "user_plan_delete", usd(10))
	assertPlanEntitlementStatusQuota(t, repo, "user_plan_delete", core.UserPlanEntitlementActive, usd(100))

	if err := repo.DeleteClient("client_plan_delete"); err != nil {
		t.Fatalf("DeleteClient returned error: %v", err)
	}
	assertUserBalance(t, repo, "user_plan_delete", usd(10))
	assertActivePlanQuota(t, repo, "user_plan_delete", usd(100))
	requests, total := repo.ListBillingRequestsPage(BillingRequestQuery{ClientID: "client_plan_delete", Limit: 10})
	if total != 1 || len(requests) != 1 || requests[0].Status != core.BillingRequestReleased {
		t.Fatalf("billing requests after client delete = total %d items %#v, want released", total, requests)
	}
}

func runDeleteUserPreservesBillingPlanHistory(t *testing.T, repo Repository) {
	t.Helper()
	setupBillingPlanTestAccount(t, repo, "user_plan_history", "client_plan_history")
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_history", PlanID: "day"}); err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	if err := repo.DeleteUser("user_plan_history"); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	entitlements := repo.ListUserPlanEntitlements("user_plan_history")
	if len(entitlements) != 1 {
		t.Fatalf("entitlements len = %d, want preserved purchase history", len(entitlements))
	}
	if entitlements[0].UserID != "user_plan_history" || entitlements[0].PlanID != "day" || entitlements[0].PlanName != "Day" {
		t.Fatalf("entitlement after user delete = %#v", entitlements[0])
	}
	if entitlements[0].Status != core.UserPlanEntitlementCancelled || entitlements[0].CurrentQuotaNanoUSD != 0 || entitlements[0].RemainingPeriods != 0 {
		t.Fatalf("entitlement after user delete should be cancelled with no quota, got %#v", entitlements[0])
	}
	if _, err := repo.GetActiveUserPlanEntitlement("user_plan_history"); err != ErrNotFound {
		t.Fatalf("GetActiveUserPlanEntitlement error = %v, want ErrNotFound", err)
	}
}

func runCancelUserPlanEntitlement(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_plan_cancel"
	planID := "cancel_week"
	if err := repo.UpsertUser(core.User{ID: userID, Username: "plan-cancel", Enabled: true, BalanceNanoUSD: usd(20)}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.UpsertUser(core.User{ID: "other_plan_cancel", Username: "other-plan-cancel", Enabled: true, BalanceNanoUSD: usd(20)}); err != nil {
		t.Fatalf("UpsertUser other returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 planID,
		Name:               "Cancel Week",
		Enabled:            true,
		PriceNanoUSD:       usd(10),
		PeriodQuotaNanoUSD: usd(100),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
	purchase, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: planID})
	if err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	if _, err := repo.CancelUserPlanEntitlement("other_plan_cancel", purchase.Entitlement.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CancelUserPlanEntitlement wrong user error = %v, want ErrNotFound", err)
	}
	cancelled, err := repo.CancelUserPlanEntitlement(userID, purchase.Entitlement.ID)
	if err != nil {
		t.Fatalf("CancelUserPlanEntitlement returned error: %v", err)
	}
	if cancelled.Status != core.UserPlanEntitlementCancelled {
		t.Fatalf("cancelled status = %q, want cancelled", cancelled.Status)
	}
	if cancelled.CurrentQuotaNanoUSD != 0 || cancelled.RemainingPeriods != 0 {
		t.Fatalf("cancelled quota/periods = %d/%d, want zero", cancelled.CurrentQuotaNanoUSD, cancelled.RemainingPeriods)
	}
	if cancelled.ExpiresAt.IsZero() || cancelled.ExpiresAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("cancelled ExpiresAt = %v, want current timestamp", cancelled.ExpiresAt)
	}
	if _, err := repo.GetActiveUserPlanEntitlement(userID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetActiveUserPlanEntitlement error = %v, want ErrNotFound", err)
	}
	entitlements := repo.ListUserPlanEntitlements(userID)
	if len(entitlements) != 1 || entitlements[0].Status != core.UserPlanEntitlementCancelled || entitlements[0].CurrentQuotaNanoUSD != 0 {
		t.Fatalf("ListUserPlanEntitlements = %#v, want one cancelled entitlement with zero quota", entitlements)
	}
	stats := repo.PlanQuotaUsageStats(PlanQuotaUsageQuery{UserID: userID, PlanID: planID, EntitlementID: purchase.Entitlement.ID, Limit: 10})
	if stats.GrantedNanoUSD != usd(100) || stats.ExpiredNanoUSD != usd(100) || stats.NetNanoUSD != 0 {
		t.Fatalf("PlanQuotaUsageStats = %#v, want granted 100 expired/cancelled 100 net 0", stats)
	}
	if _, err := repo.CancelUserPlanEntitlement(userID, purchase.Entitlement.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second CancelUserPlanEntitlement error = %v, want ErrNotFound", err)
	}
}

func runMergeUsersMovesBillingPlanEntitlements(t *testing.T, repo Repository) {
	t.Helper()
	setupBillingPlanTestAccount(t, repo, "user_plan_merge_source", "client_plan_merge_source")
	if err := repo.UpsertUser(core.User{ID: "user_plan_merge_target", Username: "merge-target", Enabled: true, BalanceNanoUSD: usd(5)}); err != nil {
		t.Fatalf("UpsertUser target returned error: %v", err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_merge_source", PlanID: "day"}); err != nil {
		t.Fatalf("PurchaseBillingPlan source returned error: %v", err)
	}
	source, err := repo.GetUser("user_plan_merge_source")
	if err != nil {
		t.Fatalf("GetUser source returned error: %v", err)
	}
	target, err := repo.GetUser("user_plan_merge_target")
	if err != nil {
		t.Fatalf("GetUser target returned error: %v", err)
	}
	target.BalanceNanoUSD += source.BalanceNanoUSD
	source.BalanceNanoUSD = 0
	source.Enabled = false
	if err := repo.MergeUsers(source, target); err != nil {
		t.Fatalf("MergeUsers returned error: %v", err)
	}
	if entitlements := repo.ListUserPlanEntitlements(source.ID); len(entitlements) != 0 {
		t.Fatalf("source entitlements = %#v, want moved away", entitlements)
	}
	entitlements := repo.ListUserPlanEntitlements(target.ID)
	if len(entitlements) != 1 {
		t.Fatalf("target entitlements len = %d, want 1", len(entitlements))
	}
	if entitlements[0].UserID != target.ID || entitlements[0].Status != core.UserPlanEntitlementActive {
		t.Fatalf("target entitlement = %#v", entitlements[0])
	}
	assertActivePlanQuota(t, repo, target.ID, usd(100))
	assertUserBalance(t, repo, target.ID, usd(15))
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_plan_merge_after",
		ClientID:        "client_plan_merge_source",
		UserID:          target.ID,
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: usd(40),
		Fingerprint:     "merge-after",
	}); err != nil {
		t.Fatalf("ReserveBilling after merge returned error: %v", err)
	}
	assertUserBalance(t, repo, target.ID, usd(15))
	assertActivePlanQuota(t, repo, target.ID, usd(100))
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_plan_merge_after",
		ClientID:      "client_plan_merge_source",
		ActualNanoUSD: usd(40),
	}); err != nil {
		t.Fatalf("SettleBilling after merge returned error: %v", err)
	}
	assertActivePlanQuota(t, repo, target.ID, usd(60))
}

func runMergeUsersRejectsTwoActiveBillingPlans(t *testing.T, repo Repository) {
	t.Helper()
	setupBillingPlanTestAccount(t, repo, "user_plan_merge_conflict_source", "client_plan_merge_conflict_source")
	if err := repo.UpsertUser(core.User{ID: "user_plan_merge_conflict_target", Username: "merge-conflict-target", Enabled: true, BalanceNanoUSD: usd(20)}); err != nil {
		t.Fatalf("UpsertUser target returned error: %v", err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_merge_conflict_source", PlanID: "day"}); err != nil {
		t.Fatalf("PurchaseBillingPlan source returned error: %v", err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_merge_conflict_target", PlanID: "day"}); err != nil {
		t.Fatalf("PurchaseBillingPlan target returned error: %v", err)
	}
	source, err := repo.GetUser("user_plan_merge_conflict_source")
	if err != nil {
		t.Fatalf("GetUser source returned error: %v", err)
	}
	target, err := repo.GetUser("user_plan_merge_conflict_target")
	if err != nil {
		t.Fatalf("GetUser target returned error: %v", err)
	}
	target.BalanceNanoUSD += source.BalanceNanoUSD
	source.BalanceNanoUSD = 0
	source.Enabled = false
	if err := repo.MergeUsers(source, target); !errors.Is(err, ErrBillingRequestConflict) {
		t.Fatalf("MergeUsers error = %v, want ErrBillingRequestConflict", err)
	}
	if entitlements := repo.ListUserPlanEntitlements(source.ID); len(entitlements) != 1 || entitlements[0].UserID != source.ID || entitlements[0].Status != core.UserPlanEntitlementActive {
		t.Fatalf("source entitlements after conflict = %#v", entitlements)
	}
	if entitlements := repo.ListUserPlanEntitlements(target.ID); len(entitlements) != 1 || entitlements[0].UserID != target.ID || entitlements[0].Status != core.UserPlanEntitlementActive {
		t.Fatalf("target entitlements after conflict = %#v", entitlements)
	}
}

func runBillingPlanListEntitlementsResetsPeriod(t *testing.T, repo Repository) {
	t.Helper()
	setupBillingPlanTestAccount(t, repo, "user_plan_list_reset", "client_plan_list_reset")
	if err := repo.SetUserBalance("user_plan_list_reset", usd(100)); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "list_week",
		Name:               "List Week",
		Enabled:            true,
		PriceNanoUSD:       usd(50),
		PeriodQuotaNanoUSD: usd(100),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
	purchase, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_list_reset", PlanID: "list_week"})
	if err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	forcePlanPeriodEnded(t, repo, purchase.Entitlement.ID)

	entitlements := repo.ListUserPlanEntitlements("user_plan_list_reset")
	if len(entitlements) != 1 {
		t.Fatalf("entitlements len = %d, want 1", len(entitlements))
	}
	if entitlements[0].CurrentQuotaNanoUSD != usd(100) {
		t.Fatalf("current quota = %d, want %d", entitlements[0].CurrentQuotaNanoUSD, usd(100))
	}
	if entitlements[0].RemainingPeriods != 6 {
		t.Fatalf("remaining periods = %d, want 6", entitlements[0].RemainingPeriods)
	}
}

func runBillingPlanPeriodReset(t *testing.T, repo Repository) {
	t.Helper()
	setupBillingPlanTestAccount(t, repo, "user_plan_reset", "client_plan_reset")
	if err := repo.SetUserBalance("user_plan_reset", usd(100)); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "week",
		Name:               "Week",
		Enabled:            true,
		PriceNanoUSD:       usd(50),
		PeriodQuotaNanoUSD: usd(100),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
	purchase, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_reset", PlanID: "week"})
	if err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	forcePlanPeriodEnded(t, repo, purchase.Entitlement.ID)

	entitlement, err := repo.GetActiveUserPlanEntitlement("user_plan_reset")
	if err != nil {
		t.Fatalf("GetActiveUserPlanEntitlement returned error: %v", err)
	}
	if entitlement.CurrentQuotaNanoUSD != usd(100) {
		t.Fatalf("current quota = %d, want %d", entitlement.CurrentQuotaNanoUSD, usd(100))
	}
	if entitlement.RemainingPeriods != 6 {
		t.Fatalf("remaining periods = %d, want 6", entitlement.RemainingPeriods)
	}
}

func runBillingPlanResetLedgerUsesPeriodBoundaryTimeAndExpiresRemainingQuota(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_plan_reset_boundary"
	clientID := "client_plan_reset_boundary"
	setupBillingPlanTestAccount(t, repo, userID, clientID)
	if err := repo.SetUserBalance(userID, usd(100)); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "reset_boundary",
		Name:               "Reset Boundary",
		Enabled:            true,
		PriceNanoUSD:       usd(50),
		PeriodQuotaNanoUSD: usd(100),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
	purchase, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: "reset_boundary"})
	if err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	resetAt := time.Unix(0, time.Now().UTC().Add(-time.Hour).UnixNano()).UTC()
	forcePlanWindow(t, repo, purchase.Entitlement.ID, resetAt.Add(-24*time.Hour), resetAt, resetAt.Add(6*24*time.Hour), 7, usd(10))

	entitlement, err := repo.GetActiveUserPlanEntitlement(userID)
	if err != nil {
		t.Fatalf("GetActiveUserPlanEntitlement returned error: %v", err)
	}
	if entitlement.CurrentQuotaNanoUSD != usd(100) || entitlement.RemainingPeriods != 6 {
		t.Fatalf("reset entitlement quota/periods = %d/%d, want %d/6", entitlement.CurrentQuotaNanoUSD, entitlement.RemainingPeriods, usd(100))
	}
	assertPlanResetBoundaryLedger(t, repo, purchase.Entitlement.ID, resetAt, usd(10), usd(100))
}

func runBillingPlanResetDropsNegativeQuota(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_plan_reset_negative"
	clientID := "client_plan_reset_negative"
	setupBillingPlanTestAccount(t, repo, userID, clientID)
	if err := repo.SetUserBalance(userID, usd(100)); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "reset_negative",
		Name:               "Reset Negative",
		Enabled:            true,
		PriceNanoUSD:       usd(50),
		PeriodQuotaNanoUSD: usd(100),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
	purchase, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: "reset_negative"})
	if err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	resetAt := time.Unix(0, time.Now().UTC().Add(-time.Hour).UnixNano()).UTC()
	forcePlanWindow(t, repo, purchase.Entitlement.ID, resetAt.Add(-24*time.Hour), resetAt, resetAt.Add(6*24*time.Hour), 7, -usd(3))

	entitlement, err := repo.GetActiveUserPlanEntitlement(userID)
	if err != nil {
		t.Fatalf("GetActiveUserPlanEntitlement returned error: %v", err)
	}
	if entitlement.CurrentQuotaNanoUSD != usd(100) || entitlement.RemainingPeriods != 6 {
		t.Fatalf("reset entitlement quota/periods = %d/%d, want %d/6", entitlement.CurrentQuotaNanoUSD, entitlement.RemainingPeriods, usd(100))
	}
	assertPlanResetLedgerWithoutExpire(t, repo, purchase.Entitlement.ID, resetAt, usd(100), usd(100))
}

func runBillingPlanMergePurchaseDropsNegativeQuota(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_plan_merge_negative"
	clientID := "client_plan_merge_negative"
	setupBillingPlanTestAccount(t, repo, userID, clientID)
	if err := repo.SetUserBalance(userID, usd(100)); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "merge_negative",
		Name:               "Merge Negative",
		Enabled:            true,
		PriceNanoUSD:       usd(10),
		PeriodQuotaNanoUSD: usd(100),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
	purchase, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: "merge_negative"})
	if err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	forcePlanWindow(t, repo, purchase.Entitlement.ID, purchase.Entitlement.CurrentPeriodStartedAt, purchase.Entitlement.CurrentPeriodEndsAt, purchase.Entitlement.ExpiresAt, purchase.Entitlement.RemainingPeriods, -usd(3))

	merged, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{
		UserID: userID,
		PlanID: "merge_negative",
		Mode:   core.BillingPlanPurchaseMergeQuota,
	})
	if err != nil {
		t.Fatalf("merge PurchaseBillingPlan returned error: %v", err)
	}
	if merged.Entitlement.ID != purchase.Entitlement.ID {
		t.Fatalf("merged entitlement = %q, want %q", merged.Entitlement.ID, purchase.Entitlement.ID)
	}
	if merged.Entitlement.CurrentQuotaNanoUSD != usd(100) {
		t.Fatalf("merged current quota = %d, want %d", merged.Entitlement.CurrentQuotaNanoUSD, usd(100))
	}
	if merged.Entitlement.PeriodQuotaNanoUSD != usd(200) {
		t.Fatalf("merged period quota = %d, want %d", merged.Entitlement.PeriodQuotaNanoUSD, usd(200))
	}
}

func runDisabledBillingPlanDoesNotAffectPurchasedEntitlement(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_plan_disabled_existing"
	clientID := "client_plan_disabled_existing"
	setupBillingPlanTestAccount(t, repo, userID, clientID)
	if err := repo.SetUserBalance(userID, usd(100)); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "disabled_week",
		Name:               "Disabled Week",
		Enabled:            true,
		PriceNanoUSD:       usd(10),
		PeriodQuotaNanoUSD: usd(100),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan enabled returned error: %v", err)
	}
	purchase, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: "disabled_week"})
	if err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "disabled_week",
		Name:               "Disabled Week",
		Enabled:            false,
		PriceNanoUSD:       usd(10),
		PeriodQuotaNanoUSD: usd(100),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan disabled returned error: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_disabled_plan_existing",
		ClientID:        clientID,
		UserID:          userID,
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: usd(25),
		Fingerprint:     "disabled-existing",
	}); err != nil {
		t.Fatalf("ReserveBilling after plan disabled returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_disabled_plan_existing",
		ClientID:      clientID,
		ActualNanoUSD: usd(25),
	}); err != nil {
		t.Fatalf("SettleBilling after plan disabled returned error: %v", err)
	}
	assertActivePlanQuota(t, repo, userID, usd(75))

	forcePlanPeriodEnded(t, repo, purchase.Entitlement.ID)
	entitlement, err := repo.GetActiveUserPlanEntitlement(userID)
	if err != nil {
		t.Fatalf("GetActiveUserPlanEntitlement after disabled reset returned error: %v", err)
	}
	if entitlement.CurrentQuotaNanoUSD != usd(100) {
		t.Fatalf("current quota after disabled reset = %d, want %d", entitlement.CurrentQuotaNanoUSD, usd(100))
	}
	if entitlement.RemainingPeriods != 6 {
		t.Fatalf("remaining periods after disabled reset = %d, want 6", entitlement.RemainingPeriods)
	}
}

func runDisabledBillingPlanCannotBePurchased(t *testing.T, repo Repository) {
	t.Helper()
	setupBillingPlanTestAccount(t, repo, "user_plan_disabled_purchase", "client_plan_disabled_purchase")
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "disabled_purchase",
		Name:               "Disabled Purchase",
		Enabled:            false,
		PriceNanoUSD:       usd(10),
		PeriodQuotaNanoUSD: usd(100),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_disabled_purchase", PlanID: "disabled_purchase"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PurchaseBillingPlan disabled error = %v, want ErrNotFound", err)
	}
}

func runDisabledBillingPlanGroupCannotBePurchased(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_plan_disabled_group"
	clientID := "client_plan_disabled_group"
	groupID := "group_disabled_sale"
	if err := repo.UpsertUser(core.User{ID: userID, Username: userID, Enabled: true, BalanceNanoUSD: usd(100)}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: clientID, Name: clientID, APIKey: "gw_" + clientID, OwnerUserID: userID, Enabled: true}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if err := repo.UpsertBillingPlanGroup(core.BillingPlanGroup{ID: groupID, Name: "Disabled Sale"}); err != nil {
		t.Fatalf("UpsertBillingPlanGroup returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "group_sale_week",
		Name:               "Group Sale Week",
		Group:              groupID,
		Enabled:            true,
		PriceNanoUSD:       usd(10),
		PeriodQuotaNanoUSD: usd(100),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
	purchase, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: "group_sale_week"})
	if err != nil {
		t.Fatalf("initial PurchaseBillingPlan returned error: %v", err)
	}
	if err := repo.UpsertBillingPlanGroup(core.BillingPlanGroup{ID: groupID, Name: "Disabled Sale", SaleDisabled: true}); err != nil {
		t.Fatalf("UpsertBillingPlanGroup disabled returned error: %v", err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: "group_sale_week"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PurchaseBillingPlan disabled group error = %v, want ErrNotFound", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_disabled_group_existing",
		ClientID:        clientID,
		UserID:          userID,
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: usd(25),
		Fingerprint:     "disabled-group-existing",
	}); err != nil {
		t.Fatalf("ReserveBilling after group disabled returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_disabled_group_existing",
		ClientID:      clientID,
		ActualNanoUSD: usd(25),
	}); err != nil {
		t.Fatalf("SettleBilling after group disabled returned error: %v", err)
	}
	assertActivePlanQuota(t, repo, userID, usd(75))
	forcePlanPeriodEnded(t, repo, purchase.Entitlement.ID)
	if entitlement, err := repo.GetActiveUserPlanEntitlement(userID); err != nil || entitlement.CurrentQuotaNanoUSD != usd(100) {
		t.Fatalf("GetActiveUserPlanEntitlement after group disabled reset = %#v, %v; want quota %d", entitlement, err, usd(100))
	}
}

func runBillingPlanGroupCRUD(t *testing.T, repo Repository) {
	t.Helper()
	group := core.BillingPlanGroup{ID: "group_vip", Name: "VIP Cards", QuotaPriceRatio: "1:0.8", SortOrder: 15}
	if err := repo.UpsertBillingPlanGroup(group); err != nil {
		t.Fatalf("UpsertBillingPlanGroup returned error: %v", err)
	}
	got, err := repo.GetBillingPlanGroup(group.ID)
	if err != nil {
		t.Fatalf("GetBillingPlanGroup returned error: %v", err)
	}
	if got.ID != group.ID || got.Name != group.Name || got.QuotaPriceRatio != group.QuotaPriceRatio || got.SortOrder != group.SortOrder {
		t.Fatalf("group = %#v, want %#v", got, group)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("group timestamps were not set: %#v", got)
	}
	groups := repo.ListBillingPlanGroups()
	if len(groups) != 1 || groups[0].ID != group.ID {
		t.Fatalf("groups = %#v, want one custom group", groups)
	}
	group.Name = "VIP Plus"
	group.QuotaPriceRatio = "2:1.5"
	group.SortOrder = 5
	if err := repo.UpsertBillingPlanGroup(group); err != nil {
		t.Fatalf("UpsertBillingPlanGroup update returned error: %v", err)
	}
	got, err = repo.GetBillingPlanGroup(group.ID)
	if err != nil {
		t.Fatalf("GetBillingPlanGroup updated returned error: %v", err)
	}
	if got.Name != group.Name || got.QuotaPriceRatio != group.QuotaPriceRatio || got.SortOrder != group.SortOrder {
		t.Fatalf("updated group = %#v, want name %q ratio %q sort %d", got, group.Name, group.QuotaPriceRatio, group.SortOrder)
	}
	if err := repo.DeleteBillingPlanGroup(group.ID); err != nil {
		t.Fatalf("DeleteBillingPlanGroup returned error: %v", err)
	}
	if _, err := repo.GetBillingPlanGroup(group.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetBillingPlanGroup after delete error = %v, want ErrNotFound", err)
	}
}

func runBillingPlanGroupsSortByCreatedAtWhenSortOrderMatches(t *testing.T, repo Repository) {
	t.Helper()
	base := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	for index, group := range []core.BillingPlanGroup{
		{ID: "group_day", Name: "日卡"},
		{ID: "group_week", Name: "周卡"},
		{ID: "group_month", Name: "月卡"},
		{ID: "group_year", Name: "年卡"},
	} {
		group.CreatedAt = base.Add(time.Duration(index) * time.Minute)
		if err := repo.UpsertBillingPlanGroup(group); err != nil {
			t.Fatalf("UpsertBillingPlanGroup(%s) returned error: %v", group.ID, err)
		}
	}

	groups := repo.ListBillingPlanGroups()
	got := make([]string, 0, len(groups))
	for _, group := range groups {
		got = append(got, group.Name)
	}
	want := []string{"日卡", "周卡", "月卡", "年卡"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("groups = %#v, want %#v", got, want)
	}
}

func setupBillingPlanTestAccount(t *testing.T, repo Repository, userID, clientID string) {
	t.Helper()
	if err := repo.UpsertUser(core.User{ID: userID, Username: userID, Enabled: true, BalanceNanoUSD: usd(20)}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: clientID, Name: clientID, APIKey: "gw_" + clientID, OwnerUserID: userID, Enabled: true}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "day",
		Name:               "Day",
		Enabled:            true,
		PriceNanoUSD:       usd(10),
		PeriodQuotaNanoUSD: usd(100),
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
}

func forcePlanSubscriptionStatus(t *testing.T, repo Repository, entitlementID string, status core.UserPlanEntitlementStatus) {
	t.Helper()
	now := time.Now().UTC()
	switch typed := repo.(type) {
	case *MemoryRepository:
		typed.mu.Lock()
		defer typed.mu.Unlock()
		entitlement, ok := typed.entitlements[entitlementID]
		if !ok {
			t.Fatalf("entitlement %q not found", entitlementID)
		}
		entitlement.Status = status
		entitlement.RemainingPeriods = 0
		entitlement.CurrentQuotaNanoUSD = 0
		entitlement.ExpiresAt = now
		entitlement.UpdatedAt = now
		typed.entitlements[entitlementID] = entitlement
	case *SQLiteRepository:
		if _, err := typed.db.Exec(`
			UPDATE user_plan_entitlements
			SET status = ?,
				remaining_periods = 0,
				current_quota_nano_usd = 0,
				expires_at_ns = ?,
				updated_at_ns = ?
			WHERE id = ?
		`, string(status), sqliteTimeNS(now), sqliteTimeNS(now), entitlementID); err != nil {
			t.Fatalf("force plan subscription status: %v", err)
		}
	default:
		t.Fatalf("unsupported repo type %T", repo)
	}
}

func forcePlanPeriodEnded(t *testing.T, repo Repository, entitlementID string) {
	t.Helper()
	now := time.Now().UTC()
	switch typed := repo.(type) {
	case *MemoryRepository:
		typed.mu.Lock()
		defer typed.mu.Unlock()
		entitlement, ok := typed.entitlements[entitlementID]
		if !ok {
			t.Fatalf("entitlement %q not found", entitlementID)
		}
		entitlement.CurrentQuotaNanoUSD = usd(10)
		entitlement.RemainingPeriods = 7
		entitlement.CurrentPeriodStartedAt = now.Add(-24*time.Hour - time.Second)
		entitlement.CurrentPeriodEndsAt = now.Add(-time.Second)
		typed.entitlements[entitlementID] = entitlement
	case *SQLiteRepository:
		if _, err := typed.db.Exec(`
			UPDATE user_plan_entitlements
			SET current_quota_nano_usd = ?,
				remaining_periods = ?,
				current_period_started_at_ns = ?,
				current_period_ends_at_ns = ?
			WHERE id = ?
		`, usd(10), 7, sqliteTimeNS(now.Add(-24*time.Hour-time.Second)), sqliteTimeNS(now.Add(-time.Second)), entitlementID); err != nil {
			t.Fatalf("force plan period ended: %v", err)
		}
	default:
		t.Fatalf("unsupported repo type %T", repo)
	}
}

func forcePlanWindow(t *testing.T, repo Repository, entitlementID string, startedAt, endsAt, expiresAt time.Time, remainingPeriods int, quota int64) {
	t.Helper()
	now := time.Now().UTC()
	switch typed := repo.(type) {
	case *MemoryRepository:
		typed.mu.Lock()
		defer typed.mu.Unlock()
		entitlement, ok := typed.entitlements[entitlementID]
		if !ok {
			t.Fatalf("entitlement %q not found", entitlementID)
		}
		entitlement.CurrentPeriodStartedAt = startedAt
		entitlement.CurrentPeriodEndsAt = endsAt
		entitlement.ExpiresAt = expiresAt
		entitlement.RemainingPeriods = remainingPeriods
		entitlement.CurrentQuotaNanoUSD = quota
		entitlement.UpdatedAt = now
		typed.entitlements[entitlementID] = entitlement
	case *SQLiteRepository:
		if _, err := typed.db.Exec(`
			UPDATE user_plan_entitlements
			SET current_period_started_at_ns = ?,
				current_period_ends_at_ns = ?,
				expires_at_ns = ?,
				remaining_periods = ?,
				current_quota_nano_usd = ?,
				updated_at_ns = ?
			WHERE id = ?
		`, sqliteTimeNS(startedAt), sqliteTimeNS(endsAt), sqliteTimeNS(expiresAt), remainingPeriods, quota, sqliteTimeNS(now), entitlementID); err != nil {
			t.Fatalf("force plan window: %v", err)
		}
	default:
		t.Fatalf("unsupported repo type %T", repo)
	}
}

func forcePrematurelyExpiredPlanEntitlement(t *testing.T, repo Repository, entitlementID string, startedAt, endsAt, expiresAt time.Time, remainingPeriods int, quota int64) {
	t.Helper()
	now := time.Now().UTC()
	switch typed := repo.(type) {
	case *MemoryRepository:
		typed.mu.Lock()
		defer typed.mu.Unlock()
		entitlement, ok := typed.entitlements[entitlementID]
		if !ok {
			t.Fatalf("entitlement %q not found", entitlementID)
		}
		entitlement.Status = core.UserPlanEntitlementExpired
		entitlement.CurrentPeriodStartedAt = startedAt
		entitlement.CurrentPeriodEndsAt = endsAt
		entitlement.ExpiresAt = expiresAt
		entitlement.RemainingPeriods = remainingPeriods
		entitlement.CurrentQuotaNanoUSD = quota
		entitlement.UpdatedAt = now
		typed.entitlements[entitlementID] = entitlement
	case *SQLiteRepository:
		if _, err := typed.db.Exec(`
			UPDATE user_plan_entitlements
			SET status = ?,
				current_period_started_at_ns = ?,
				current_period_ends_at_ns = ?,
				expires_at_ns = ?,
				remaining_periods = ?,
				current_quota_nano_usd = ?,
				updated_at_ns = ?
			WHERE id = ?
		`, string(core.UserPlanEntitlementExpired), sqliteTimeNS(startedAt), sqliteTimeNS(endsAt), sqliteTimeNS(expiresAt), remainingPeriods, quota, sqliteTimeNS(now), entitlementID); err != nil {
			t.Fatalf("force prematurely expired plan entitlement: %v", err)
		}
	default:
		t.Fatalf("unsupported repo type %T", repo)
	}
}

func assertPlanResetBoundaryLedger(t *testing.T, repo Repository, entitlementID string, wantCreatedAt time.Time, wantExpiredNanoUSD, wantResetNanoUSD int64) {
	t.Helper()
	switch typed := repo.(type) {
	case *MemoryRepository:
		typed.mu.RLock()
		defer typed.mu.RUnlock()
		foundExpire := false
		foundReset := false
		for _, entry := range typed.planLedger {
			if entry.EntitlementID != entitlementID {
				continue
			}
			switch entry.Kind {
			case "expire":
				if foundExpire {
					t.Fatalf("found multiple expire ledger entries for %s", entitlementID)
				}
				foundExpire = true
				if !entry.CreatedAt.Equal(wantCreatedAt) {
					t.Fatalf("expire ledger created_at = %s, want %s", entry.CreatedAt, wantCreatedAt)
				}
				if entry.AmountNanoUSD != -wantExpiredNanoUSD || entry.QuotaAfterNanoUSD != 0 {
					t.Fatalf("expire ledger amount/quota_after = %d/%d, want %d/0", entry.AmountNanoUSD, entry.QuotaAfterNanoUSD, -wantExpiredNanoUSD)
				}
			case "reset":
				if foundReset {
					t.Fatalf("found multiple reset ledger entries for %s", entitlementID)
				}
				foundReset = true
				if !entry.CreatedAt.Equal(wantCreatedAt) {
					t.Fatalf("reset ledger created_at = %s, want %s", entry.CreatedAt, wantCreatedAt)
				}
				if entry.AmountNanoUSD != wantResetNanoUSD || entry.QuotaAfterNanoUSD != wantResetNanoUSD {
					t.Fatalf("reset ledger amount/quota_after = %d/%d, want %d/%d", entry.AmountNanoUSD, entry.QuotaAfterNanoUSD, wantResetNanoUSD, wantResetNanoUSD)
				}
			}
		}
		if !foundExpire || !foundReset {
			t.Fatalf("boundary ledger for %s found expire/reset = %t/%t, want both", entitlementID, foundExpire, foundReset)
		}
	case *SQLiteRepository:
		rows, err := typed.db.Query(`
			SELECT kind, created_at_ns, amount_nano_usd, quota_after_nano_usd
			FROM plan_quota_ledger
			WHERE entitlement_id = ? AND kind IN ('expire', 'reset')
		`, entitlementID)
		if err != nil {
			t.Fatalf("query reset boundary ledger: %v", err)
		}
		defer rows.Close()
		found := map[string]core.PlanQuotaLedgerEntry{}
		for rows.Next() {
			var entry core.PlanQuotaLedgerEntry
			var createdAtNS int64
			if err := rows.Scan(&entry.Kind, &createdAtNS, &entry.AmountNanoUSD, &entry.QuotaAfterNanoUSD); err != nil {
				t.Fatalf("scan reset boundary ledger: %v", err)
			}
			entry.CreatedAt = timeFromNS(createdAtNS)
			if _, exists := found[entry.Kind]; exists {
				t.Fatalf("found multiple %s ledger entries for %s", entry.Kind, entitlementID)
			}
			found[entry.Kind] = entry
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate reset boundary ledger: %v", err)
		}
		expireEntry, foundExpire := found["expire"]
		resetEntry, foundReset := found["reset"]
		if !foundExpire || !foundReset {
			t.Fatalf("boundary ledger for %s found expire/reset = %t/%t, want both", entitlementID, foundExpire, foundReset)
		}
		if !expireEntry.CreatedAt.Equal(wantCreatedAt) {
			t.Fatalf("expire ledger created_at = %s, want %s", expireEntry.CreatedAt, wantCreatedAt)
		}
		if expireEntry.AmountNanoUSD != -wantExpiredNanoUSD || expireEntry.QuotaAfterNanoUSD != 0 {
			t.Fatalf("expire ledger amount/quota_after = %d/%d, want %d/0", expireEntry.AmountNanoUSD, expireEntry.QuotaAfterNanoUSD, -wantExpiredNanoUSD)
		}
		if !resetEntry.CreatedAt.Equal(wantCreatedAt) {
			t.Fatalf("reset ledger created_at = %s, want %s", resetEntry.CreatedAt, wantCreatedAt)
		}
		if resetEntry.AmountNanoUSD != wantResetNanoUSD || resetEntry.QuotaAfterNanoUSD != wantResetNanoUSD {
			t.Fatalf("reset ledger amount/quota_after = %d/%d, want %d/%d", resetEntry.AmountNanoUSD, resetEntry.QuotaAfterNanoUSD, wantResetNanoUSD, wantResetNanoUSD)
		}
	default:
		t.Fatalf("unsupported repo type %T", repo)
	}
}

func assertPlanResetLedgerWithoutExpire(t *testing.T, repo Repository, entitlementID string, wantCreatedAt time.Time, wantResetNanoUSD, wantQuotaAfterNanoUSD int64) {
	t.Helper()
	switch typed := repo.(type) {
	case *MemoryRepository:
		typed.mu.RLock()
		defer typed.mu.RUnlock()
		foundReset := false
		for _, entry := range typed.planLedger {
			if entry.EntitlementID != entitlementID {
				continue
			}
			if entry.Kind == "expire" {
				t.Fatalf("unexpected expire ledger for negative reset: %#v", entry)
			}
			if entry.Kind != "reset" {
				continue
			}
			if foundReset {
				t.Fatalf("found multiple reset ledger entries for %s", entitlementID)
			}
			foundReset = true
			if !entry.CreatedAt.Equal(wantCreatedAt) {
				t.Fatalf("reset ledger created_at = %s, want %s", entry.CreatedAt, wantCreatedAt)
			}
			if entry.AmountNanoUSD != wantResetNanoUSD || entry.QuotaAfterNanoUSD != wantQuotaAfterNanoUSD {
				t.Fatalf("reset ledger amount/quota_after = %d/%d, want %d/%d", entry.AmountNanoUSD, entry.QuotaAfterNanoUSD, wantResetNanoUSD, wantQuotaAfterNanoUSD)
			}
		}
		if !foundReset {
			t.Fatalf("reset ledger entry for %s not found", entitlementID)
		}
	case *SQLiteRepository:
		rows, err := typed.db.Query(`
			SELECT kind, created_at_ns, amount_nano_usd, quota_after_nano_usd
			FROM plan_quota_ledger
			WHERE entitlement_id = ? AND kind IN ('expire', 'reset')
		`, entitlementID)
		if err != nil {
			t.Fatalf("query reset ledger: %v", err)
		}
		defer rows.Close()
		foundReset := false
		for rows.Next() {
			var kind string
			var createdAtNS, amountNanoUSD, quotaAfterNanoUSD int64
			if err := rows.Scan(&kind, &createdAtNS, &amountNanoUSD, &quotaAfterNanoUSD); err != nil {
				t.Fatalf("scan reset ledger: %v", err)
			}
			if kind == "expire" {
				t.Fatalf("unexpected expire ledger for negative reset amount=%d quota_after=%d", amountNanoUSD, quotaAfterNanoUSD)
			}
			if kind != "reset" {
				continue
			}
			if foundReset {
				t.Fatalf("found multiple reset ledger entries for %s", entitlementID)
			}
			foundReset = true
			gotCreatedAt := timeFromNS(createdAtNS)
			if !gotCreatedAt.Equal(wantCreatedAt) {
				t.Fatalf("reset ledger created_at = %s, want %s", gotCreatedAt, wantCreatedAt)
			}
			if amountNanoUSD != wantResetNanoUSD || quotaAfterNanoUSD != wantQuotaAfterNanoUSD {
				t.Fatalf("reset ledger amount/quota_after = %d/%d, want %d/%d", amountNanoUSD, quotaAfterNanoUSD, wantResetNanoUSD, wantQuotaAfterNanoUSD)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate reset ledger: %v", err)
		}
		if !foundReset {
			t.Fatalf("reset ledger entry for %s not found", entitlementID)
		}
	default:
		t.Fatalf("unsupported repo type %T", repo)
	}
}

func assertUserBalance(t *testing.T, repo Repository, userID string, want int64) {
	t.Helper()
	user, err := repo.GetUser(userID)
	if err != nil {
		t.Fatalf("GetUser returned error: %v", err)
	}
	if user.BalanceNanoUSD != want {
		t.Fatalf("user balance = %d, want %d", user.BalanceNanoUSD, want)
	}
}

func assertActivePlanQuota(t *testing.T, repo Repository, userID string, want int64) {
	t.Helper()
	entitlement, err := repo.GetActiveUserPlanEntitlement(userID)
	if err != nil {
		t.Fatalf("GetActiveUserPlanEntitlement returned error: %v", err)
	}
	if entitlement.CurrentQuotaNanoUSD != want {
		t.Fatalf("plan quota = %d, want %d", entitlement.CurrentQuotaNanoUSD, want)
	}
}

func assertPlanEntitlementStatusQuota(t *testing.T, repo Repository, userID string, wantStatus core.UserPlanEntitlementStatus, wantQuota int64) {
	t.Helper()
	entitlements := repo.ListUserPlanEntitlements(userID)
	if len(entitlements) != 1 {
		t.Fatalf("entitlements len = %d, want 1: %#v", len(entitlements), entitlements)
	}
	if entitlements[0].Status != wantStatus || entitlements[0].CurrentQuotaNanoUSD != wantQuota {
		t.Fatalf("entitlement status/quota = %s/%d, want %s/%d", entitlements[0].Status, entitlements[0].CurrentQuotaNanoUSD, wantStatus, wantQuota)
	}
}

func usd(value int64) int64 {
	return value * core.NanoUSDPerUSD
}
