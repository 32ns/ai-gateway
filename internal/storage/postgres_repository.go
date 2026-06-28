package storage

import (
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage/postgresrepo"
)

type PostgresRepository struct {
	*postgresrepo.PostgresRepository
}

func NewPostgresRepository(dsn, masterKey string) (*PostgresRepository, error) {
	repo, err := postgresrepo.NewPostgresRepository(dsn, masterKey)
	if err != nil {
		return nil, err
	}
	return &PostgresRepository{PostgresRepository: repo}, nil
}

func (r *PostgresRepository) UpdateBillingAccount(input core.BillingAccountUpdateInput) error {
	return r.PostgresRepository.UpdateBillingAccount(input)
}

func (r *PostgresRepository) ListBillingRequestsPage(query BillingRequestQuery) ([]core.BillingReservation, int) {
	return r.PostgresRepository.ListBillingRequestsPage(postgresrepo.BillingRequestQuery{
		UserID:    query.UserID,
		ClientID:  query.ClientID,
		Model:     query.Model,
		Status:    query.Status,
		StartedAt: query.StartedAt,
		EndedAt:   query.EndedAt,
		Offset:    query.Offset,
		Limit:     query.Limit,
	})
}

func (r *PostgresRepository) ListBillingRequests(query BillingRequestQuery) []core.BillingReservation {
	return r.PostgresRepository.ListBillingRequests(postgresrepo.BillingRequestQuery{
		UserID:    query.UserID,
		ClientID:  query.ClientID,
		Model:     query.Model,
		Status:    query.Status,
		StartedAt: query.StartedAt,
		EndedAt:   query.EndedAt,
		Offset:    query.Offset,
		Limit:     query.Limit,
	})
}

func (r *PostgresRepository) BillingUsageSpendNanoUSD(startedAt, endedAt time.Time) int64 {
	return r.PostgresRepository.BillingUsageSpendNanoUSD(startedAt, endedAt)
}

func (r *PostgresRepository) ListBillingUsageSpendByDay(startedAt time.Time, days int) []BillingUsageSpendDaySummary {
	items := r.PostgresRepository.ListBillingUsageSpendByDay(startedAt, days)
	out := make([]BillingUsageSpendDaySummary, 0, len(items))
	for _, item := range items {
		out = append(out, BillingUsageSpendDaySummary{
			Date:         item.Date,
			SpendNanoUSD: item.SpendNanoUSD,
		})
	}
	return out
}

func (r *PostgresRepository) ListBillingRequestCountByDay(startedAt time.Time, days int) []BillingRequestDayCountSummary {
	items := r.PostgresRepository.ListBillingRequestCountByDay(startedAt, days)
	out := make([]BillingRequestDayCountSummary, 0, len(items))
	for _, item := range items {
		out = append(out, BillingRequestDayCountSummary{
			Date:  item.Date,
			Count: item.Count,
		})
	}
	return out
}

func (r *PostgresRepository) FinanceOverviewStats(startOfDay, endOfDay time.Time) FinanceOverviewStats {
	item := r.PostgresRepository.FinanceOverviewStats(startOfDay, endOfDay)
	return FinanceOverviewStats{
		TotalBalanceNanoUSD:      item.TotalBalanceNanoUSD,
		TodayIncomeNanoUSD:       item.TodayIncomeNanoUSD,
		TodaySpendNanoUSD:        item.TodaySpendNanoUSD,
		TodayBalanceSpendNanoUSD: item.TodayBalanceSpendNanoUSD,
		TodayPlanSpendNanoUSD:    item.TodayPlanSpendNanoUSD,
		PaidOrderNanoUSD:         item.PaidOrderNanoUSD,
		UsageMissingCount:        item.UsageMissingCount,
		UnsettledRequestCount:    item.UnsettledRequestCount,
		ReconcileIssueCount:      item.ReconcileIssueCount,
		TotalUsers:               item.TotalUsers,
		TotalClients:             item.TotalClients,
		ActiveClients:            item.ActiveClients,
	}
}

func (r *PostgresRepository) FinanceEntityCounts() FinanceEntityCounts {
	item := r.PostgresRepository.FinanceEntityCounts()
	return FinanceEntityCounts{
		TotalUsers:    item.TotalUsers,
		TotalClients:  item.TotalClients,
		ActiveClients: item.ActiveClients,
	}
}

func (r *PostgresRepository) FinanceTotalSpendNanoUSD() int64 {
	return r.PostgresRepository.FinanceTotalSpendNanoUSD()
}

func (r *PostgresRepository) ListFinanceReconcileIssues(now time.Time) []FinanceReconcileIssueSummary {
	items := r.PostgresRepository.ListFinanceReconcileIssues(now)
	out := make([]FinanceReconcileIssueSummary, 0, len(items))
	for _, item := range items {
		out = append(out, FinanceReconcileIssueSummary{
			Kind:         item.Kind,
			Severity:     item.Severity,
			ResourceID:   item.ResourceID,
			Message:      item.Message,
			DeltaNanoUSD: item.DeltaNanoUSD,
		})
	}
	return out
}

func (r *PostgresRepository) ListPaymentIncomeByDay(startedAt time.Time, days int) []PaymentIncomeDaySummary {
	items := r.PostgresRepository.ListPaymentIncomeByDay(startedAt, days)
	out := make([]PaymentIncomeDaySummary, 0, len(items))
	for _, item := range items {
		out = append(out, PaymentIncomeDaySummary{
			Date:          item.Date,
			IncomeNanoUSD: item.IncomeNanoUSD,
			OrderCount:    item.OrderCount,
		})
	}
	return out
}

func (r *PostgresRepository) ListBillingModelUsageSummaries(limit int) []BillingModelUsageSummary {
	items := r.PostgresRepository.ListBillingModelUsageSummaries(limit)
	out := make([]BillingModelUsageSummary, 0, len(items))
	for _, item := range items {
		out = append(out, BillingModelUsageSummary{
			Model:            item.Model,
			RequestCount:     item.RequestCount,
			PromptTokens:     item.PromptTokens,
			CompletionTokens: item.CompletionTokens,
			SpendNanoUSD:     item.SpendNanoUSD,
		})
	}
	return out
}

func (r *PostgresRepository) ListTokenUsageDailySummaries(startedAt time.Time, days int, limit int) []TokenUsageDailySummary {
	items := r.PostgresRepository.ListTokenUsageDailySummaries(startedAt, days, limit)
	return postgresTokenUsageDailySummaries(items)
}

func (r *PostgresRepository) ListTokenUsageDailyUserSummaries(startedAt time.Time, days int, limit int) []TokenUsageDailySummary {
	items := r.PostgresRepository.ListTokenUsageDailyUserSummaries(startedAt, days, limit)
	return postgresTokenUsageDailySummaries(items)
}

func postgresTokenUsageDailySummaries(items []postgresrepo.TokenUsageDailySummary) []TokenUsageDailySummary {
	out := make([]TokenUsageDailySummary, 0, len(items))
	for _, item := range items {
		out = append(out, TokenUsageDailySummary{
			Date:                item.Date,
			UserID:              item.UserID,
			Username:            item.Username,
			RequestCount:        item.RequestCount,
			PromptTokens:        item.PromptTokens,
			CachedTokens:        item.CachedTokens,
			CacheCreationTokens: item.CacheCreationTokens,
			CompletionTokens:    item.CompletionTokens,
			ImageOutputTokens:   item.ImageOutputTokens,
			TotalTokens:         item.TotalTokens,
		})
	}
	return out
}

func (r *PostgresRepository) ListFinanceTopUsersBySpend(limit int) []FinanceUserSummary {
	items := r.PostgresRepository.ListFinanceTopUsersBySpend(limit)
	return postgresFinanceUserSummaries(items)
}

func (r *PostgresRepository) ListFinanceUserSummariesPage(offset, limit int) ([]FinanceUserSummary, int) {
	items, total := r.PostgresRepository.ListFinanceUserSummariesPage(offset, limit)
	return postgresFinanceUserSummaries(items), total
}

func (r *PostgresRepository) PlanSubscriptionStats(query PlanSubscriptionQuery) PlanSubscriptionStats {
	item := r.PostgresRepository.PlanSubscriptionStats(postgresrepo.PlanSubscriptionQuery{
		UserID: query.UserID,
		PlanID: query.PlanID,
		Status: query.Status,
		Offset: query.Offset,
		Limit:  query.Limit,
	})
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

func (r *PostgresRepository) ListPlanSubscriptionPlanSummaries(query PlanSubscriptionQuery, limit int) []PlanSubscriptionPlanSummary {
	items := r.PostgresRepository.ListPlanSubscriptionPlanSummaries(postgresrepo.PlanSubscriptionQuery{
		UserID: query.UserID,
		PlanID: query.PlanID,
		Status: query.Status,
		Offset: query.Offset,
		Limit:  query.Limit,
	}, limit)
	out := make([]PlanSubscriptionPlanSummary, 0, len(items))
	for _, item := range items {
		out = append(out, PlanSubscriptionPlanSummary{
			PlanID:                 item.PlanID,
			PlanName:               item.PlanName,
			PlanGroup:              item.PlanGroup,
			PurchaseCount:          item.PurchaseCount,
			ActiveCount:            item.ActiveCount,
			RevenueNanoUSD:         item.RevenueNanoUSD,
			ActiveRemainingNanoUSD: item.ActiveRemainingNanoUSD,
		})
	}
	return out
}

func (r *PostgresRepository) ListPlanSubscriptionSummariesPage(query PlanSubscriptionQuery) ([]PlanSubscriptionSummary, int) {
	items, total := r.PostgresRepository.ListPlanSubscriptionSummariesPage(postgresrepo.PlanSubscriptionQuery{
		UserID: query.UserID,
		PlanID: query.PlanID,
		Status: query.Status,
		Offset: query.Offset,
		Limit:  query.Limit,
	})
	out := make([]PlanSubscriptionSummary, 0, len(items))
	for _, item := range items {
		out = append(out, PlanSubscriptionSummary{
			Entitlement:        item.Entitlement,
			Username:           item.Username,
			PlanGroup:          item.PlanGroup,
			UserBalanceNanoUSD: item.UserBalanceNanoUSD,
		})
	}
	return out, total
}

func (r *PostgresRepository) CancelUserPlanEntitlement(userID, entitlementID string) (core.UserPlanEntitlement, error) {
	return r.PostgresRepository.CancelUserPlanEntitlement(userID, entitlementID)
}

func (r *PostgresRepository) PlanQuotaUsageStats(query PlanQuotaUsageQuery) PlanQuotaUsageStats {
	item := r.PostgresRepository.PlanQuotaUsageStats(postgresrepo.PlanQuotaUsageQuery{
		UserID:        query.UserID,
		PlanID:        query.PlanID,
		EntitlementID: query.EntitlementID,
		StartedAt:     query.StartedAt,
		EndedAt:       query.EndedAt,
		Offset:        query.Offset,
		Limit:         query.Limit,
	})
	return PlanQuotaUsageStats{
		GrantedNanoUSD:  item.GrantedNanoUSD,
		UsedNanoUSD:     item.UsedNanoUSD,
		ReturnedNanoUSD: item.ReturnedNanoUSD,
		ExpiredNanoUSD:  item.ExpiredNanoUSD,
		NetNanoUSD:      item.NetNanoUSD,
	}
}

func (r *PostgresRepository) ListPlanQuotaUsageByDay(query PlanQuotaUsageQuery) ([]PlanQuotaUsageDaySummary, int) {
	items, total := r.PostgresRepository.ListPlanQuotaUsageByDay(postgresrepo.PlanQuotaUsageQuery{
		UserID:        query.UserID,
		PlanID:        query.PlanID,
		EntitlementID: query.EntitlementID,
		StartedAt:     query.StartedAt,
		EndedAt:       query.EndedAt,
		Offset:        query.Offset,
		Limit:         query.Limit,
	})
	out := make([]PlanQuotaUsageDaySummary, 0, len(items))
	for _, item := range items {
		out = append(out, PlanQuotaUsageDaySummary{
			Date:                item.Date,
			UserID:              item.UserID,
			Username:            item.Username,
			PlanID:              item.PlanID,
			PlanName:            item.PlanName,
			EntitlementID:       item.EntitlementID,
			GrantedNanoUSD:      item.GrantedNanoUSD,
			UsedNanoUSD:         item.UsedNanoUSD,
			ReturnedNanoUSD:     item.ReturnedNanoUSD,
			ExpiredNanoUSD:      item.ExpiredNanoUSD,
			NetNanoUSD:          item.NetNanoUSD,
			LastLedgerCreatedAt: item.LastLedgerCreatedAt,
		})
	}
	return out, total
}

func (r *PostgresRepository) ListPlanQuotaUsageEvents(query PlanQuotaUsageQuery) []PlanQuotaUsageEvent {
	items := r.PostgresRepository.ListPlanQuotaUsageEvents(postgresrepo.PlanQuotaUsageQuery{
		UserID:        query.UserID,
		PlanID:        query.PlanID,
		EntitlementID: query.EntitlementID,
		StartedAt:     query.StartedAt,
		EndedAt:       query.EndedAt,
		Offset:        query.Offset,
		Limit:         query.Limit,
	})
	out := make([]PlanQuotaUsageEvent, 0, len(items))
	for _, item := range items {
		out = append(out, PlanQuotaUsageEvent{
			UserID:          item.UserID,
			Username:        item.Username,
			PlanID:          item.PlanID,
			PlanName:        item.PlanName,
			EntitlementID:   item.EntitlementID,
			Kind:            item.Kind,
			GrantedNanoUSD:  item.GrantedNanoUSD,
			UsedNanoUSD:     item.UsedNanoUSD,
			ReturnedNanoUSD: item.ReturnedNanoUSD,
			ExpiredNanoUSD:  item.ExpiredNanoUSD,
			NetNanoUSD:      item.NetNanoUSD,
			CreatedAt:       item.CreatedAt,
		})
	}
	return out
}

func (r *PostgresRepository) ListUsersPage(query UserListQuery) ([]UserListItem, int, int) {
	items, filtered, total := r.PostgresRepository.ListUsersPage(postgresrepo.UserListQuery{
		Query:     query.Query,
		Role:      query.Role,
		Status:    query.Status,
		Inviter:   query.Inviter,
		Sort:      query.Sort,
		Direction: query.Direction,
		Offset:    query.Offset,
		Limit:     query.Limit,
	})
	out := make([]UserListItem, 0, len(items))
	for _, item := range items {
		out = append(out, UserListItem{
			User:         item.User,
			InviteCount:  item.InviteCount,
			SpendNanoUSD: item.SpendNanoUSD,
		})
	}
	return out, filtered, total
}

func (r *PostgresRepository) ListVisibleSiteMessageDeliveriesPage(query SiteMessageVisibilityQuery) ([]core.SiteMessageDelivery, int) {
	query = normalizeSiteMessageVisibilityQuery(query)
	return r.PostgresRepository.ListVisibleSiteMessageDeliveriesPage(query.UserID, query.AccountGroups, query.Offset, query.Limit)
}

func (r *PostgresRepository) VisibleSiteMessageUnreadCount(query SiteMessageVisibilityQuery) int {
	query = normalizeSiteMessageVisibilityQuery(query)
	return r.PostgresRepository.VisibleSiteMessageUnreadCount(query.UserID, query.AccountGroups)
}

func (r *PostgresRepository) ListSupportTicketsPage(query SupportTicketQuery) ([]core.SupportTicket, int) {
	return r.PostgresRepository.ListSupportTicketsPage(postgresrepo.SupportTicketQuery{
		UserID: query.UserID,
		Status: query.Status,
		Query:  query.Query,
		Offset: query.Offset,
		Limit:  query.Limit,
	})
}

func (r *PostgresRepository) FindUserByOAuthIdentity(provider, subject string) (core.User, error) {
	return r.PostgresRepository.FindUserByOAuthIdentity(provider, subject)
}

func (r *PostgresRepository) FindUserByInvitationSignature(signature string) (core.User, error) {
	return r.PostgresRepository.FindUserByInvitationSignature(signature)
}

func (r *PostgresRepository) ListUsersByInviter(inviterID string) []core.User {
	return r.PostgresRepository.ListUsersByInviter(inviterID)
}

func (r *PostgresRepository) CountUsersByInviter(inviterID string) int {
	return r.PostgresRepository.CountUsersByInviter(inviterID)
}

func (r *PostgresRepository) CountEnabledAdminsExcluding(excludedIDs []string) int {
	return r.PostgresRepository.CountEnabledAdminsExcluding(excludedIDs)
}

func (r *PostgresRepository) ListClientsByOwner(ownerUserID string) []core.APIClient {
	return r.PostgresRepository.ListClientsByOwner(ownerUserID)
}

func postgresFinanceUserSummaries(items []postgresrepo.FinanceUserSummary) []FinanceUserSummary {
	out := make([]FinanceUserSummary, 0, len(items))
	for _, item := range items {
		out = append(out, FinanceUserSummary{
			UserID:             item.UserID,
			Username:           item.Username,
			BalanceNanoUSD:     item.BalanceNanoUSD,
			RechargeNanoUSD:    item.RechargeNanoUSD,
			RewardNanoUSD:      item.RewardNanoUSD,
			SpendNanoUSD:       item.SpendNanoUSD,
			UsageSpendNanoUSD:  item.UsageSpendNanoUSD,
			PlanSpendNanoUSD:   item.PlanSpendNanoUSD,
			RefundNanoUSD:      item.RefundNanoUSD,
			LastPaymentAt:      item.LastPaymentAt,
			LastSpendAt:        item.LastSpendAt,
			ClientCount:        item.ClientCount,
			ClientSpendNanoUSD: item.ClientSpendNanoUSD,
			ClientUsageNanoUSD: item.ClientUsageNanoUSD,
			ClientPlanNanoUSD:  item.ClientPlanNanoUSD,
		})
	}
	return out
}

func (r *PostgresRepository) ListFinanceTopClientsBySpend(limit int) []FinanceClientSpendSummary {
	items := r.PostgresRepository.ListFinanceTopClientsBySpend(limit)
	out := make([]FinanceClientSpendSummary, 0, len(items))
	for _, item := range items {
		out = append(out, FinanceClientSpendSummary{
			ClientID:          item.ClientID,
			ClientName:        item.ClientName,
			OwnerUserID:       item.OwnerUserID,
			SpendLimitNanoUSD: item.SpendLimitNanoUSD,
			SpendUsedNanoUSD:  item.SpendUsedNanoUSD,
			UsageNanoUSD:      item.UsageNanoUSD,
			PlanNanoUSD:       item.PlanNanoUSD,
		})
	}
	return out
}

func (r *PostgresRepository) ListBillingUsageSpendByHourForUser(userID string, startedAt, endedAt time.Time) []BillingUsageSpendHourSummary {
	items := r.PostgresRepository.ListBillingUsageSpendByHourForUser(userID, startedAt, endedAt)
	out := make([]BillingUsageSpendHourSummary, 0, len(items))
	for _, item := range items {
		out = append(out, BillingUsageSpendHourSummary{
			Hour:         item.Hour,
			SpendNanoUSD: item.SpendNanoUSD,
		})
	}
	return out
}

func (r *PostgresRepository) ListBillingUsageSpendByClient() []BillingUsageSpendSummary {
	items := r.PostgresRepository.ListBillingUsageSpendByClient()
	return postgresBillingUsageSpendSummaries(items)
}

func (r *PostgresRepository) ListBillingUsageSpendByClientForUser(userID string) []BillingUsageSpendSummary {
	items := r.PostgresRepository.ListBillingUsageSpendByClientForUser(userID)
	return postgresBillingUsageSpendSummaries(items)
}

func (r *PostgresRepository) UserActualSpendTotal(userID string) int64 {
	return r.PostgresRepository.UserActualSpendTotal(userID)
}

func (r *PostgresRepository) ListClientActualSpends() []core.ClientSpend {
	return r.PostgresRepository.ListClientActualSpends()
}

func postgresBillingUsageSpendSummaries(items []postgresrepo.BillingUsageSpendSummary) []BillingUsageSpendSummary {
	out := make([]BillingUsageSpendSummary, 0, len(items))
	for _, item := range items {
		out = append(out, BillingUsageSpendSummary{
			UserID:       item.UserID,
			ClientID:     item.ClientID,
			SpendNanoUSD: item.SpendNanoUSD,
		})
	}
	return out
}

func (r *PostgresRepository) ListBillingLedgerUserSummaries() []BillingLedgerUserSummary {
	items := r.PostgresRepository.ListBillingLedgerUserSummaries()
	out := make([]BillingLedgerUserSummary, 0, len(items))
	for _, item := range items {
		out = append(out, BillingLedgerUserSummary{
			UserID:          item.UserID,
			RechargeNanoUSD: item.RechargeNanoUSD,
			RewardNanoUSD:   item.RewardNanoUSD,
			SpendNanoUSD:    item.SpendNanoUSD,
			RefundNanoUSD:   item.RefundNanoUSD,
			LastPaymentAt:   item.LastPaymentAt,
			LastSpendAt:     item.LastSpendAt,
		})
	}
	return out
}

func (r *PostgresRepository) ListPaymentOrdersPage(query PaymentOrderQuery) ([]core.PaymentOrder, int) {
	return r.PostgresRepository.ListPaymentOrdersPage(postgresrepo.PaymentOrderQuery{
		UserID:         query.UserID,
		Provider:       query.Provider,
		Status:         query.Status,
		ExcludePending: query.ExcludePending,
		Offset:         query.Offset,
		Limit:          query.Limit,
	})
}

func (r *PostgresRepository) ListPaymentOrders(query PaymentOrderQuery) []core.PaymentOrder {
	return r.PostgresRepository.ListPaymentOrders(postgresrepo.PaymentOrderQuery{
		UserID:         query.UserID,
		Provider:       query.Provider,
		Status:         query.Status,
		ExcludePending: query.ExcludePending,
		Offset:         query.Offset,
		Limit:          query.Limit,
	})
}

func (r *PostgresRepository) ListAuditSummariesPage(query AuditQuery) ([]core.AuditEvent, int) {
	return r.PostgresRepository.ListAuditSummariesPage(postgresrepo.AuditQuery{
		Kind:     query.Kind,
		Status:   query.Status,
		Actor:    query.Actor,
		Resource: query.Resource,
		Offset:   query.Offset,
		Limit:    query.Limit,
	})
}
