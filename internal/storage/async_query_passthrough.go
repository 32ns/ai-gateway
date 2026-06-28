package storage

import (
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func (r *AsyncAuditRepository) ListBillingRequests(query BillingRequestQuery) []core.BillingReservation {
	return r.Repository.ListBillingRequests(query)
}

func (r *AsyncAuditRepository) ListPaymentOrders(query PaymentOrderQuery) []core.PaymentOrder {
	return r.Repository.ListPaymentOrders(query)
}

func (r *AsyncAuditRepository) ListBillingUsageSpendByClientForUser(userID string) []BillingUsageSpendSummary {
	return r.Repository.ListBillingUsageSpendByClientForUser(userID)
}

func (r *AsyncAuditRepository) PlanSubscriptionStats(query PlanSubscriptionQuery) PlanSubscriptionStats {
	return r.Repository.PlanSubscriptionStats(query)
}

func (r *AsyncAuditRepository) ListPlanSubscriptionPlanSummaries(query PlanSubscriptionQuery, limit int) []PlanSubscriptionPlanSummary {
	return r.Repository.ListPlanSubscriptionPlanSummaries(query, limit)
}

func (r *AsyncAuditRepository) ListPlanSubscriptionSummariesPage(query PlanSubscriptionQuery) ([]PlanSubscriptionSummary, int) {
	return r.Repository.ListPlanSubscriptionSummariesPage(query)
}

func (r *AsyncAuditRepository) PlanQuotaUsageStats(query PlanQuotaUsageQuery) PlanQuotaUsageStats {
	return r.Repository.PlanQuotaUsageStats(query)
}

func (r *AsyncAuditRepository) ListPlanQuotaUsageByDay(query PlanQuotaUsageQuery) ([]PlanQuotaUsageDaySummary, int) {
	return r.Repository.ListPlanQuotaUsageByDay(query)
}

func (r *AsyncAuditRepository) ListPlanQuotaUsageEvents(query PlanQuotaUsageQuery) []PlanQuotaUsageEvent {
	return r.Repository.ListPlanQuotaUsageEvents(query)
}

func (r *AsyncAuditRepository) UserActualSpendTotal(userID string) int64 {
	return r.Repository.UserActualSpendTotal(userID)
}

func (r *AsyncAuditRepository) FinanceTotalSpendNanoUSD() int64 {
	return r.Repository.FinanceTotalSpendNanoUSD()
}

func (r *AsyncAuditRepository) ListClientActualSpends() []core.ClientSpend {
	return r.Repository.ListClientActualSpends()
}

func (r *AsyncAuditRepository) ListBillingRequestCountByDay(startedAt time.Time, days int) []BillingRequestDayCountSummary {
	return r.Repository.ListBillingRequestCountByDay(startedAt, days)
}

func (r *AsyncAuditRepository) ListBillingModelUsageSummaries(limit int) []BillingModelUsageSummary {
	return r.Repository.ListBillingModelUsageSummaries(limit)
}

func (r *AsyncAuditRepository) ListTokenUsageDailySummaries(startedAt time.Time, days int, limit int) []TokenUsageDailySummary {
	return r.Repository.ListTokenUsageDailySummaries(startedAt, days, limit)
}

func (r *AsyncAuditRepository) ListTokenUsageDailyUserSummaries(startedAt time.Time, days int, limit int) []TokenUsageDailySummary {
	return r.Repository.ListTokenUsageDailyUserSummaries(startedAt, days, limit)
}

func (r *AsyncAuditRepository) FinanceOverviewStats(startOfDay, endOfDay time.Time) FinanceOverviewStats {
	return r.Repository.FinanceOverviewStats(startOfDay, endOfDay)
}

func (r *AsyncAuditRepository) FinanceEntityCounts() FinanceEntityCounts {
	return r.Repository.FinanceEntityCounts()
}

func (r *AsyncAuditRepository) ListFinanceReconcileIssues(now time.Time) []FinanceReconcileIssueSummary {
	return r.Repository.ListFinanceReconcileIssues(now)
}

func (r *AsyncAuditRepository) ListPaymentIncomeByDay(startedAt time.Time, days int) []PaymentIncomeDaySummary {
	return r.Repository.ListPaymentIncomeByDay(startedAt, days)
}

func (r *AsyncAuditRepository) ListFinanceTopUsersBySpend(limit int) []FinanceUserSummary {
	return r.Repository.ListFinanceTopUsersBySpend(limit)
}

func (r *AsyncAuditRepository) ListFinanceUserSummariesPage(offset, limit int) ([]FinanceUserSummary, int) {
	return r.Repository.ListFinanceUserSummariesPage(offset, limit)
}

func (r *AsyncAuditRepository) ListUsersPage(query UserListQuery) ([]UserListItem, int, int) {
	return r.Repository.ListUsersPage(query)
}

func (r *AsyncAuditRepository) ListFinanceTopClientsBySpend(limit int) []FinanceClientSpendSummary {
	return r.Repository.ListFinanceTopClientsBySpend(limit)
}

type clientSummaryPager interface {
	ListClientSummariesPage(offset, limit int) ([]core.APIClient, int)
}

type clientSummaryOwnerPager interface {
	ListClientSummariesByOwnerPage(ownerUserID string, offset, limit int) ([]core.APIClient, int)
}

type siteMessageDeliveryPager interface {
	ListSiteMessageDeliveriesPage(userID string, includeDisabled bool, offset, limit int) ([]core.SiteMessageDelivery, int)
}

type documentPager interface {
	ListDocumentsPage(status core.DocumentStatus, seoOnly bool, offset, limit int) ([]core.Document, int)
}

type documentSearcher interface {
	SearchDocumentsPage(query string, status core.DocumentStatus, seoOnly bool, offset, limit int) ([]core.Document, int)
}

func (r *AsyncAuditRepository) ListClientSummariesPage(offset, limit int) ([]core.APIClient, int) {
	if pager, ok := r.Repository.(clientSummaryPager); ok {
		return pager.ListClientSummariesPage(offset, limit)
	}
	clients := listCachedClientSummaries(r.Repository)
	return clientSummaryPage(clients, offset, limit)
}

func (r *AsyncAuditRepository) ListClientSummariesByOwnerPage(ownerUserID string, offset, limit int) ([]core.APIClient, int) {
	if pager, ok := r.Repository.(clientSummaryOwnerPager); ok {
		return pager.ListClientSummariesByOwnerPage(ownerUserID, offset, limit)
	}
	clients := r.Repository.ListClientsByOwner(ownerUserID)
	return clientSummaryPage(clients, offset, limit)
}

func (r *AsyncAuditRepository) ListSiteMessageDeliveriesPage(userID string, includeDisabled bool, offset, limit int) ([]core.SiteMessageDelivery, int) {
	if pager, ok := r.Repository.(siteMessageDeliveryPager); ok {
		return pager.ListSiteMessageDeliveriesPage(userID, includeDisabled, offset, limit)
	}
	deliveries := r.Repository.ListSiteMessageDeliveries(userID, includeDisabled)
	return siteMessageDeliveryPage(deliveries, offset, limit)
}

func (r *AsyncAuditRepository) ListVisibleSiteMessageDeliveriesPage(query SiteMessageVisibilityQuery) ([]core.SiteMessageDelivery, int) {
	if pager, ok := r.Repository.(SiteMessageVisibleDeliveryPager); ok {
		return pager.ListVisibleSiteMessageDeliveriesPage(query)
	}
	query = normalizeSiteMessageVisibilityQuery(query)
	deliveries := r.Repository.ListSiteMessageDeliveries(query.UserID, false)
	return visibleSiteMessageDeliveriesPage(deliveries, query)
}

func (r *AsyncAuditRepository) VisibleSiteMessageUnreadCount(query SiteMessageVisibilityQuery) int {
	if pager, ok := r.Repository.(SiteMessageVisibleDeliveryPager); ok {
		return pager.VisibleSiteMessageUnreadCount(query)
	}
	query = normalizeSiteMessageVisibilityQuery(query)
	deliveries := r.Repository.ListSiteMessageDeliveries(query.UserID, false)
	return visibleSiteMessageUnreadCount(deliveries, query)
}

func (r *AsyncAuditRepository) ListDocumentsPage(status core.DocumentStatus, seoOnly bool, offset, limit int) ([]core.Document, int) {
	if pager, ok := r.Repository.(documentPager); ok {
		return pager.ListDocumentsPage(status, seoOnly, offset, limit)
	}
	return documentPage(r.Repository.ListDocuments(), status, seoOnly, offset, limit)
}

func (r *AsyncAuditRepository) SearchDocumentsPage(query string, status core.DocumentStatus, seoOnly bool, offset, limit int) ([]core.Document, int) {
	if searcher, ok := r.Repository.(documentSearcher); ok {
		return searcher.SearchDocumentsPage(query, status, seoOnly, offset, limit)
	}
	return documentSearchPage(r.Repository.ListDocuments(), query, status, seoOnly, offset, limit)
}
