package controlplane

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"
)

type FinanceOverview struct {
	TotalBalanceNanoUSD      int64
	TodayIncomeNanoUSD       int64
	TodaySpendNanoUSD        int64
	TodayBalanceSpendNanoUSD int64
	TodayPlanSpendNanoUSD    int64
	PendingOrderNanoUSD      int64
	PaidOrderNanoUSD         int64
	UsageMissingCount        int
	UnsettledRequestCount    int
	TotalUsers               int
	TotalClients             int
	ActiveClients            int
	RecentLedger             []core.BillingLedgerEntry
	TopUsersBySpend          []FinanceUserSummary
	TopClientsBySpend        []FinanceClientSpendSummary
	ReconcileIssues          []FinanceReconcileIssue
}

type FinanceUserSummary struct {
	User               core.User
	BalanceNanoUSD     int64
	RechargeNanoUSD    int64
	RewardNanoUSD      int64
	SpendNanoUSD       int64
	UsageSpendNanoUSD  int64
	PlanSpendNanoUSD   int64
	RefundNanoUSD      int64
	LastPaymentAt      *time.Time
	LastSpendAt        *time.Time
	ClientCount        int
	ClientSpendNanoUSD int64
	ClientUsageNanoUSD int64
	ClientPlanNanoUSD  int64
}

type FinanceClientSpendSummary struct {
	Client core.APIClient
	Spend  core.ClientSpend
}

type FinanceReconcileIssue struct {
	Kind         string
	Severity     string
	ResourceID   string
	Message      string
	DeltaNanoUSD int64
}

type ReleasedReservedBillingSummary struct {
	Count         int
	AmountNanoUSD int64
}

type FinanceDailySummary struct {
	Date                string
	IncomeNanoUSD       int64
	SpendNanoUSD        int64
	BalanceSpendNanoUSD int64
	PlanSpendNanoUSD    int64
	ProfitNanoUSD       int64
	OrderCount          int
	RequestCount        int
}

type FinanceModelSummary struct {
	Model            string
	RequestCount     int
	PromptTokens     int64
	CompletionTokens int64
	SpendNanoUSD     int64
}

type FinanceTokenDailySummary struct {
	Date                string
	UserID              string
	Username            string
	RequestCount        int
	PromptTokens        int64
	CachedTokens        int64
	CacheCreationTokens int64
	CompletionTokens    int64
	ImageOutputTokens   int64
	TotalTokens         int64
}

type FinancePage struct {
	Overview       FinanceOverview
	Daily          []FinanceDailySummary
	Models         []FinanceModelSummary
	TokenSiteDaily []FinanceTokenDailySummary
	TokenUserDaily []FinanceTokenDailySummary
	Users          []FinanceUserSummary
	UserPage       FinanceUserPage
	Orders         PaymentOrderPage
	OrderRefunds   map[string]PaymentOrderRefundSummary
	Usage          UsageLogPage
}

type FinanceUserFilter struct {
	Page     int
	PageSize int
}

type FinanceUserPage struct {
	Total    int
	Page     int
	PageSize int
	HasPrev  bool
	PrevPage int
	HasNext  bool
	NextPage int
}

func (s *Service) FinancePageForTab(ctx context.Context, tab string, userFilter FinanceUserFilter, orderFilter PaymentOrderFilter, usageFilter UsageLogFilter) FinancePage {
	admin := core.User{Role: core.UserRoleAdmin, Enabled: true}
	if userFilter.PageSize <= 0 {
		userFilter.PageSize = 25
	}
	if orderFilter.PageSize <= 0 {
		orderFilter.PageSize = 10
	}
	if usageFilter.PageSize <= 0 {
		usageFilter.PageSize = 10
	}

	switch strings.TrimSpace(tab) {
	case "users":
		userPage := s.financeUserSummariesPage(userFilter)
		overview := s.financePageOverviewBase()
		overview.TotalUsers = userPage.Page.Total
		return FinancePage{
			Overview: overview,
			Users:    userPage.Users,
			UserPage: userPage.Page,
		}
	case "orders":
		orders := s.PaymentOrderPage(admin, financePaymentOrderFilter(orderFilter))
		return FinancePage{
			Overview:     s.financePageOverviewBase(),
			Orders:       orders,
			OrderRefunds: s.PaymentOrderRefundSummaries(orders.Orders),
		}
	case "ledger":
		overview := s.financePageOverviewBase()
		overview.RecentLedger = s.repo.ListBillingLedger("", 20)
		return FinancePage{
			Overview: overview,
		}
	case "usage":
		return FinancePage{
			Overview: s.financePageOverviewBase(),
			Usage:    s.UsageLogPage(ctx, admin, usageFilter),
		}
	case "tokens":
		return FinancePage{
			Overview:       s.financePageOverviewBase(),
			TokenSiteDaily: s.financeTokenSiteDailySummaries(30),
			TokenUserDaily: s.financeTokenUserDailySummaries(30, 5000),
		}
	case "reconcile":
		overview := s.financePageOverviewBase()
		overview.ReconcileIssues = s.financeReconcileIssues()
		return FinancePage{
			Overview: overview,
		}
	default:
		return FinancePage{
			Overview: s.financePageOverviewBase(),
		}
	}
}

func (s *Service) FinanceOverviewDailySummaries(days int) []FinanceDailySummary {
	return s.financeDailySummariesForOverview(days)
}

func (s *Service) FinanceOverviewModelSummaries(limit int) []FinanceModelSummary {
	return s.financeModelSummaries(limit)
}

func (s *Service) FinanceOverviewTopUsersBySpend(limit int) FinanceOverview {
	counts := s.financeEntityCounts()
	overview := FinanceOverview{TotalUsers: counts.TotalUsers}
	overview.TopUsersBySpend = s.financeTopUsersBySpend(limit)
	return overview
}

func (s *Service) FinanceOverviewTopClientsBySpend(limit int) FinanceOverview {
	counts := s.financeEntityCounts()
	overview := FinanceOverview{
		TotalClients:  counts.TotalClients,
		ActiveClients: counts.ActiveClients,
	}
	overview.TopClientsBySpend = s.financeTopClientsBySpendFast(limit)
	return overview
}

func (s *Service) DashboardFinancePage() FinancePage {
	return FinancePage{Overview: s.financeDashboardOverview()}
}

func (s *Service) financeDashboardOverview() FinanceOverview {
	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	endOfDay := startOfDay.AddDate(0, 0, 1)
	stats := s.repo.FinanceOverviewStats(startOfDay, endOfDay)
	overview := FinanceOverview{
		TodayIncomeNanoUSD:       stats.TodayIncomeNanoUSD,
		TodaySpendNanoUSD:        stats.TodaySpendNanoUSD,
		TodayBalanceSpendNanoUSD: stats.TodayBalanceSpendNanoUSD,
		TodayPlanSpendNanoUSD:    stats.TodayPlanSpendNanoUSD,
	}
	return overview
}

func (s *Service) financePageOverviewBase() FinanceOverview {
	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	endOfDay := startOfDay.AddDate(0, 0, 1)
	return financeOverviewFromStats(s.repo.FinanceOverviewStats(startOfDay, endOfDay))
}

func financeOverviewFromStats(stats storage.FinanceOverviewStats) FinanceOverview {
	return FinanceOverview{
		TotalBalanceNanoUSD:      stats.TotalBalanceNanoUSD,
		TodayIncomeNanoUSD:       stats.TodayIncomeNanoUSD,
		TodaySpendNanoUSD:        stats.TodaySpendNanoUSD,
		TodayBalanceSpendNanoUSD: stats.TodayBalanceSpendNanoUSD,
		TodayPlanSpendNanoUSD:    stats.TodayPlanSpendNanoUSD,
		PaidOrderNanoUSD:         stats.PaidOrderNanoUSD,
		UsageMissingCount:        stats.UsageMissingCount,
		UnsettledRequestCount:    stats.UnsettledRequestCount,
		TotalUsers:               stats.TotalUsers,
		TotalClients:             stats.TotalClients,
		ActiveClients:            stats.ActiveClients,
		ReconcileIssues:          financeReconcileIssuePlaceholders(stats.ReconcileIssueCount),
	}
}

func financeReconcileIssuePlaceholders(count int) []FinanceReconcileIssue {
	if count <= 0 {
		return nil
	}
	return make([]FinanceReconcileIssue, count)
}

func (s *Service) financeEntityCounts() storage.FinanceEntityCounts {
	return s.repo.FinanceEntityCounts()
}

func (s *Service) FinanceTotalSpendNanoUSD() int64 {
	if s == nil || s.repo == nil {
		return 0
	}
	return s.repo.FinanceTotalSpendNanoUSD()
}

func (s *Service) financeBillingRequestCountByDay(startedAt time.Time, days int) []storage.BillingRequestDayCountSummary {
	if days <= 0 || startedAt.IsZero() {
		return nil
	}
	return s.repo.ListBillingRequestCountByDay(startedAt, days)
}

func (s *Service) financeTopUsersBySpend(limit int) []FinanceUserSummary {
	return financeUserSummariesFromStorage(s.repo.ListFinanceTopUsersBySpend(limit))
}

func financeUserSummariesFromStorage(items []storage.FinanceUserSummary) []FinanceUserSummary {
	out := make([]FinanceUserSummary, 0, len(items))
	for _, item := range items {
		username := strings.TrimSpace(item.Username)
		if username == "" {
			username = strings.TrimSpace(item.UserID)
		}
		out = append(out, FinanceUserSummary{
			User: core.User{
				ID:       strings.TrimSpace(item.UserID),
				Username: username,
			},
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

func financePaymentOrderFilter(filter PaymentOrderFilter) PaymentOrderFilter {
	filter.ExcludePending = true
	return filter
}

type financeUserPageResult struct {
	Users []FinanceUserSummary
	Page  FinanceUserPage
}

func (s *Service) financeUserSummariesPage(filter FinanceUserFilter) financeUserPageResult {
	if filter.PageSize <= 0 {
		filter.PageSize = 25
	}
	if filter.PageSize > 100 {
		filter.PageSize = 100
	}
	if filter.Page < 1 {
		filter.Page = 1
	}
	offset := (filter.Page - 1) * filter.PageSize
	items, total := s.repo.ListFinanceUserSummariesPage(offset, filter.PageSize)
	lastPage := 1
	if total > 0 {
		lastPage = (total + filter.PageSize - 1) / filter.PageSize
	}
	if filter.Page > lastPage {
		filter.Page = lastPage
		offset = (filter.Page - 1) * filter.PageSize
		items, total = s.repo.ListFinanceUserSummariesPage(offset, filter.PageSize)
	}
	page := FinanceUserPage{
		Total:    total,
		Page:     filter.Page,
		PageSize: filter.PageSize,
		HasPrev:  filter.Page > 1,
		PrevPage: filter.Page - 1,
		HasNext:  offset+len(items) < total,
		NextPage: filter.Page + 1,
	}
	return financeUserPageResult{Users: financeUserSummariesFromStorage(items), Page: page}
}

func (s *Service) FinanceUserSummariesForExport() []FinanceUserSummary {
	out := make([]FinanceUserSummary, 0)
	_ = s.ForEachFinanceUserSummaryForExport(func(summary FinanceUserSummary) error {
		out = append(out, summary)
		return nil
	})
	return out
}

func (s *Service) ForEachFinanceUserSummaryForExport(fn func(FinanceUserSummary) error) error {
	if s == nil || s.repo == nil || fn == nil {
		return nil
	}
	const pageSize = 500
	for offset := 0; ; {
		items, total := s.repo.ListFinanceUserSummariesPage(offset, pageSize)
		summaries := financeUserSummariesFromStorage(items)
		for _, summary := range summaries {
			if err := fn(summary); err != nil {
				return err
			}
		}
		if len(items) == 0 || offset+len(items) >= total {
			break
		}
		offset += len(items)
	}
	return nil
}

func (s *Service) financeTopClientsBySpendFast(limit int) []FinanceClientSpendSummary {
	return financeClientSpendSummariesFromStorage(s.repo.ListFinanceTopClientsBySpend(limit))
}

func financeClientSpendSummariesFromStorage(items []storage.FinanceClientSpendSummary) []FinanceClientSpendSummary {
	out := make([]FinanceClientSpendSummary, 0, len(items))
	for _, item := range items {
		clientID := strings.TrimSpace(item.ClientID)
		name := strings.TrimSpace(item.ClientName)
		if name == "" {
			name = clientID
		}
		out = append(out, FinanceClientSpendSummary{
			Client: core.APIClient{
				ID:          clientID,
				Name:        name,
				OwnerUserID: strings.TrimSpace(item.OwnerUserID),
			},
			Spend: core.ClientSpend{
				ClientID:          clientID,
				SpendLimitNanoUSD: item.SpendLimitNanoUSD,
				SpendUsedNanoUSD:  item.SpendUsedNanoUSD,
			},
		})
	}
	return out
}

func (s *Service) financeReconcileIssues() []FinanceReconcileIssue {
	items := s.repo.ListFinanceReconcileIssues(time.Now().UTC())
	issues := make([]FinanceReconcileIssue, 0, len(items))
	for _, item := range items {
		issues = append(issues, FinanceReconcileIssue{
			Kind:         item.Kind,
			Severity:     item.Severity,
			ResourceID:   item.ResourceID,
			Message:      item.Message,
			DeltaNanoUSD: item.DeltaNanoUSD,
		})
	}
	return issues
}

func (s *Service) ReleaseReservedBillingRequest(requestID, clientID, reason string) (core.BillingReservation, error) {
	requestID = strings.TrimSpace(requestID)
	clientID = strings.TrimSpace(clientID)
	if requestID == "" {
		return core.BillingReservation{}, fmt.Errorf("billing request is required")
	}
	request, err := s.findReservedBillingRequest(requestID, clientID)
	if err != nil {
		return core.BillingReservation{}, err
	}
	if strings.TrimSpace(reason) == "" {
		reason = "released from finance reconciliation"
	}
	if err := s.repo.ReleaseBilling(core.BillingReleaseInput{
		RequestID: request.RequestID,
		ClientID:  request.ClientID,
		Reason:    reason,
	}); err != nil {
		return core.BillingReservation{}, err
	}
	now := time.Now().UTC()
	request.Status = core.BillingRequestReleased
	request.SettledAt = &now
	return request, nil
}

func (s *Service) ReleaseAbandonedGatewayReservations(cutoff time.Time, reason string) (ReleasedReservedBillingSummary, error) {
	if cutoff.IsZero() {
		cutoff = time.Now().UTC()
	} else {
		cutoff = cutoff.UTC()
	}
	if strings.TrimSpace(reason) == "" {
		reason = "released after gateway restart"
	}
	candidates := make([]core.BillingReservation, 0)
	for offset := 0; ; offset += 100 {
		requests, total := s.repo.ListBillingRequestsPage(storage.BillingRequestQuery{
			Status: core.BillingRequestReserved,
			Offset: offset,
			Limit:  100,
		})
		for _, request := range requests {
			if request.CreatedAt.IsZero() || request.CreatedAt.Before(cutoff) {
				candidates = append(candidates, request)
			}
		}
		if offset+len(requests) >= total || len(requests) == 0 {
			break
		}
	}

	var summary ReleasedReservedBillingSummary
	for _, request := range candidates {
		if err := s.repo.ReleaseBilling(core.BillingReleaseInput{
			RequestID: request.RequestID,
			ClientID:  request.ClientID,
			Reason:    reason,
		}); err != nil {
			return summary, err
		}
		summary.Count++
	}
	return summary, nil
}

func (s *Service) findReservedBillingRequest(requestID, clientID string) (core.BillingReservation, error) {
	var match *core.BillingReservation
	for offset := 0; ; offset += 100 {
		requests, total := s.repo.ListBillingRequestsPage(storage.BillingRequestQuery{
			ClientID: clientID,
			Status:   core.BillingRequestReserved,
			Offset:   offset,
			Limit:    100,
		})
		for _, request := range requests {
			if request.RequestID != requestID {
				continue
			}
			if match != nil {
				return core.BillingReservation{}, fmt.Errorf("billing request is ambiguous; provide client id")
			}
			copied := request
			match = &copied
		}
		if offset+len(requests) >= total || len(requests) == 0 {
			break
		}
	}
	if match == nil {
		return core.BillingReservation{}, storage.ErrNotFound
	}
	return *match, nil
}

func (s *Service) financeDailySummariesForOverview(days int) []FinanceDailySummary {
	if days <= 0 {
		days = 14
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	start := today.AddDate(0, 0, -(days - 1))
	summaries := make([]FinanceDailySummary, days)
	byDate := make(map[string]*FinanceDailySummary, days)
	for i := range summaries {
		day := start.AddDate(0, 0, i)
		summaries[i].Date = day.Format("2006-01-02")
		byDate[summaries[i].Date] = &summaries[i]
	}
	for _, income := range s.repo.ListPaymentIncomeByDay(start, days) {
		summary := byDate[income.Date]
		if summary == nil {
			continue
		}
		summary.IncomeNanoUSD = income.IncomeNanoUSD
		summary.OrderCount = income.OrderCount
	}
	for _, spend := range s.repo.ListBillingUsageSpendByDay(start, days) {
		summary := byDate[spend.Date]
		if summary != nil {
			summary.SpendNanoUSD = spend.SpendNanoUSD
		}
	}
	endedAt := today.AddDate(0, 0, 1)
	for offset := 0; ; offset += 100 {
		requests, total := s.repo.ListBillingRequestsPage(storage.BillingRequestQuery{StartedAt: start, EndedAt: endedAt, Offset: offset, Limit: 100})
		for _, request := range requests {
			date := request.CreatedAt.Local().Format("2006-01-02")
			summary := byDate[date]
			if summary == nil {
				continue
			}
			spend := financeBillingRequestSpendAmount(request)
			if spend <= 0 {
				continue
			}
			if core.NormalizeClientBillingSource(request.BillingSource) == core.ClientBillingSourcePlan {
				summary.PlanSpendNanoUSD = addSignedNanoUSDSaturating(summary.PlanSpendNanoUSD, spend)
			} else {
				summary.BalanceSpendNanoUSD = addSignedNanoUSDSaturating(summary.BalanceSpendNanoUSD, spend)
			}
		}
		if len(requests) == 0 || offset+len(requests) >= total {
			break
		}
	}
	for _, count := range s.financeBillingRequestCountByDay(start, days) {
		summary := byDate[count.Date]
		if summary != nil {
			summary.RequestCount = count.Count
		}
	}
	for i := range summaries {
		summaries[i].ProfitNanoUSD = addSignedNanoUSDSaturating(summaries[i].IncomeNanoUSD, -summaries[i].BalanceSpendNanoUSD)
	}
	slices.Reverse(summaries)
	return summaries
}

func financeBillingRequestSpendAmount(request core.BillingReservation) int64 {
	switch request.Status {
	case core.BillingRequestReserved, core.BillingRequestReleased, core.BillingRequestUsageMissing:
		return 0
	case core.BillingRequestSettled:
		if request.ActualNanoUSD > 0 {
			return request.ActualNanoUSD
		}
		return 0
	default:
		if request.ActualNanoUSD > 0 {
			return request.ActualNanoUSD
		}
		return 0
	}
}

func (s *Service) financeModelSummaries(limit int) []FinanceModelSummary {
	items := s.repo.ListBillingModelUsageSummaries(limit)
	out := make([]FinanceModelSummary, 0, len(items))
	for _, item := range items {
		model := strings.TrimSpace(item.Model)
		if model == "" {
			model = "unknown"
		}
		out = append(out, FinanceModelSummary{
			Model:            model,
			RequestCount:     item.RequestCount,
			PromptTokens:     item.PromptTokens,
			CompletionTokens: item.CompletionTokens,
			SpendNanoUSD:     item.SpendNanoUSD,
		})
	}
	return out
}

func (s *Service) financeTokenSiteDailySummaries(days int) []FinanceTokenDailySummary {
	start := financeTokenReportStart(days)
	return financeTokenDailySummariesFromStorage(s.repo.ListTokenUsageDailySummaries(start, days, days))
}

func (s *Service) financeTokenUserDailySummaries(days, limit int) []FinanceTokenDailySummary {
	start := financeTokenReportStart(days)
	return financeTokenDailySummariesFromStorage(s.repo.ListTokenUsageDailyUserSummaries(start, days, limit))
}

func financeTokenReportStart(days int) time.Time {
	if days <= 0 {
		days = 30
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return today.AddDate(0, 0, -(days - 1))
}

func financeTokenDailySummariesFromStorage(items []storage.TokenUsageDailySummary) []FinanceTokenDailySummary {
	out := make([]FinanceTokenDailySummary, 0, len(items))
	for _, item := range items {
		username := strings.TrimSpace(item.Username)
		if username == "" {
			username = strings.TrimSpace(item.UserID)
		}
		out = append(out, FinanceTokenDailySummary{
			Date:                strings.TrimSpace(item.Date),
			UserID:              strings.TrimSpace(item.UserID),
			Username:            username,
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

func addSignedNanoUSDSaturating(a, b int64) int64 {
	if b > 0 && a > (1<<63-1)-b {
		return 1<<63 - 1
	}
	if b < 0 && a < (-1<<63)-b {
		return -1 << 63
	}
	return a + b
}
