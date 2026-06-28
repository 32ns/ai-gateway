package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	_ "modernc.org/sqlite"
)

func TestMemoryRepositoryFinancePlanPurchaseUsesBalanceSpend(t *testing.T) {
	runFinancePlanPurchaseUsesBalanceSpend(t, NewMemoryRepository())
}

func TestSQLiteRepositoryFinancePlanPurchaseUsesBalanceSpend(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	runFinancePlanPurchaseUsesBalanceSpend(t, repo)
}

func TestSQLiteRepositoryFinanceAggregateQueries(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	userID := "user_finance_aggregate"
	clientID := "client_finance_aggregate"
	startBalance := int64(10 * core.NanoUSDPerUSD)
	paymentAmount := int64(5 * core.NanoUSDPerUSD)
	actualSpend := int64(2 * core.NanoUSDPerUSD)

	if err := repo.UpsertUser(core.User{ID: userID, Username: "finance-aggregate", Enabled: true, BalanceNanoUSD: startBalance}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: clientID, Name: "Finance Aggregate Client", APIKey: "gw_finance_aggregate", OwnerUserID: userID, Enabled: true}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}

	now := time.Now().UTC()
	order := core.PaymentOrder{
		ID:            "pay_finance_aggregate",
		OutTradeNo:    "out_finance_aggregate",
		UserID:        userID,
		Provider:      core.PaymentProviderAlipay,
		AmountNanoUSD: paymentAmount,
		Status:        core.PaymentOrderPending,
		CreatedAt:     now,
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}
	if _, credited, err := repo.CompletePaymentOrder(order.OutTradeNo, "trade_finance_aggregate", paymentAmount, now); err != nil || !credited {
		t.Fatalf("CompletePaymentOrder credited=%t err=%v", credited, err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_finance_aggregate",
		ClientID:        clientID,
		ClientName:      "Finance Aggregate Client",
		UserID:          userID,
		Model:           "gpt-4.1",
		ReservedNanoUSD: core.NanoUSDPerUSD,
		Fingerprint:     "req_finance_aggregate",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_finance_aggregate",
		ClientID:      clientID,
		Model:         "gpt-4.1",
		Usage:         core.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		ActualNanoUSD: actualSpend,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}

	startOfDay := time.Date(now.Local().Year(), now.Local().Month(), now.Local().Day(), 0, 0, 0, 0, now.Local().Location())
	stats := repo.FinanceOverviewStats(startOfDay, startOfDay.AddDate(0, 0, 1))
	if stats.TotalUsers != 1 || stats.TotalClients != 1 || stats.ActiveClients != 1 {
		t.Fatalf("counts = users %d clients %d active %d", stats.TotalUsers, stats.TotalClients, stats.ActiveClients)
	}
	if stats.TodayIncomeNanoUSD != paymentAmount || stats.PaidOrderNanoUSD != paymentAmount || stats.TodaySpendNanoUSD != actualSpend {
		t.Fatalf("stats = %#v, want income/paid %d spend %d", stats, paymentAmount, actualSpend)
	}
	if stats.TodayBalanceSpendNanoUSD != actualSpend || stats.TodayPlanSpendNanoUSD != 0 {
		t.Fatalf("spend split = balance %d plan %d, want balance %d plan 0", stats.TodayBalanceSpendNanoUSD, stats.TodayPlanSpendNanoUSD, actualSpend)
	}

	income := repo.ListPaymentIncomeByDay(startOfDay, 1)
	if len(income) != 1 || income[0].IncomeNanoUSD != paymentAmount || income[0].OrderCount != 1 {
		t.Fatalf("income = %#v, want one paid order %d", income, paymentAmount)
	}

	users, total := repo.ListFinanceUserSummariesPage(0, 10)
	if total != 1 || len(users) != 1 || users[0].UserID != userID || users[0].SpendNanoUSD != actualSpend || users[0].RechargeNanoUSD != paymentAmount {
		t.Fatalf("finance users total=%d users=%#v", total, users)
	}
	if users[0].BalanceNanoUSD != startBalance+paymentAmount-actualSpend {
		t.Fatalf("user balance = %d, want %d", users[0].BalanceNanoUSD, startBalance+paymentAmount-actualSpend)
	}

	clients := repo.ListFinanceTopClientsBySpend(5)
	if len(clients) != 1 || clients[0].ClientID != clientID || clients[0].ClientName != "Finance Aggregate Client" || clients[0].SpendUsedNanoUSD != actualSpend {
		t.Fatalf("top clients = %#v", clients)
	}

	if err := repo.DeleteUser(userID); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	stats = repo.FinanceOverviewStats(startOfDay, startOfDay.AddDate(0, 0, 1))
	if stats.TotalUsers != 1 || stats.TotalClients != 1 || stats.ActiveClients != 0 {
		t.Fatalf("deleted counts = users %d clients %d active %d", stats.TotalUsers, stats.TotalClients, stats.ActiveClients)
	}
	if stats.TotalBalanceNanoUSD != 0 {
		t.Fatalf("deleted total balance = %d, want current live balance 0", stats.TotalBalanceNanoUSD)
	}
	counts := repo.FinanceEntityCounts()
	if counts.TotalUsers != 1 || counts.TotalClients != 1 || counts.ActiveClients != 0 {
		t.Fatalf("deleted entity counts = %#v, want historical user/client and no active clients", counts)
	}

	users, total = repo.ListFinanceUserSummariesPage(0, 10)
	if total != 1 || len(users) != 1 || users[0].UserID != userID || users[0].Username != userID || users[0].SpendNanoUSD != actualSpend || users[0].RechargeNanoUSD != paymentAmount {
		t.Fatalf("deleted finance users total=%d users=%#v", total, users)
	}
	clients = repo.ListFinanceTopClientsBySpend(5)
	if len(clients) != 1 || clients[0].ClientID != clientID || clients[0].ClientName != "Finance Aggregate Client" || clients[0].OwnerUserID != userID || clients[0].SpendUsedNanoUSD != actualSpend {
		t.Fatalf("deleted top clients = %#v", clients)
	}
}

func TestMemoryRepositoryFinanceReconcileIssuesIncludeBalanceAndClientLimits(t *testing.T) {
	runFinanceReconcileIssuesIncludeBalanceAndClientLimits(t, NewMemoryRepository())
}

func TestSQLiteRepositoryFinanceReconcileIssuesIncludeBalanceAndClientLimits(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	runFinanceReconcileIssuesIncludeBalanceAndClientLimits(t, repo)
}

func runFinanceReconcileIssuesIncludeBalanceAndClientLimits(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_finance_issue"
	clientID := "client_finance_issue"
	if err := repo.UpsertUser(core.User{ID: userID, Username: "finance-issue", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:                clientID,
		Name:              "Finance Issue Client",
		APIKey:            "gw_finance_issue",
		OwnerUserID:       userID,
		Enabled:           true,
		SpendLimitNanoUSD: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:   "req_finance_issue",
		ClientID:    clientID,
		ClientName:  "Finance Issue Client",
		UserID:      userID,
		Model:       "gpt-4.1",
		Fingerprint: "req_finance_issue",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_finance_issue",
		ClientID:      clientID,
		Model:         "gpt-4.1",
		ActualNanoUSD: 2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}

	now := time.Now()
	stats := repo.FinanceOverviewStats(now.Add(-time.Hour), now.Add(time.Hour))
	if stats.ReconcileIssueCount < 2 {
		t.Fatalf("reconcile issue count = %d, want at least 2", stats.ReconcileIssueCount)
	}
	issues := repo.ListFinanceReconcileIssues(now)
	kinds := map[string]FinanceReconcileIssueSummary{}
	for _, issue := range issues {
		kinds[issue.Kind] = issue
	}
	if issue, ok := kinds["negative_balance"]; !ok || issue.ResourceID != userID || issue.DeltaNanoUSD != -core.NanoUSDPerUSD {
		t.Fatalf("negative balance issue = %#v ok=%t", issue, ok)
	}
	if issue, ok := kinds["client_spend_over_limit"]; !ok || issue.ResourceID != clientID || issue.DeltaNanoUSD != core.NanoUSDPerUSD {
		t.Fatalf("client over limit issue = %#v ok=%t", issue, ok)
	}
}

func TestMemoryRepositoryTokenUsageDailySummaries(t *testing.T) {
	runTokenUsageDailySummaries(t, NewMemoryRepository(), "")
}

func TestSQLiteRepositoryTokenUsageDailySummaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	repo, err := NewSQLiteRepository(path, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	runTokenUsageDailySummaries(t, repo, path)
}

func runTokenUsageDailySummaries(t *testing.T, repo Repository, sqlitePath string) {
	t.Helper()
	userA := "user_token_a"
	userB := "user_token_b"
	clientA := "client_token_a"
	clientB := "client_token_b"
	if err := repo.UpsertUser(core.User{ID: userA, Username: "token-a", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatalf("UpsertUser A returned error: %v", err)
	}
	if err := repo.UpsertUser(core.User{ID: userB, Username: "token-b", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatalf("UpsertUser B returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: clientA, Name: "Token A", APIKey: "gw_token_a", OwnerUserID: userA, Enabled: true}); err != nil {
		t.Fatalf("UpsertClient A returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: clientB, Name: "Token B", APIKey: "gw_token_b", OwnerUserID: userB, Enabled: true}); err != nil {
		t.Fatalf("UpsertClient B returned error: %v", err)
	}

	settle := func(requestID, clientID, userID string, usage core.Usage, missing bool) {
		t.Helper()
		if _, err := repo.ReserveBilling(core.BillingReservationInput{
			RequestID:   requestID,
			ClientID:    clientID,
			ClientName:  clientID,
			UserID:      userID,
			Model:       "gpt-token",
			Fingerprint: requestID,
		}); err != nil {
			t.Fatalf("ReserveBilling(%s) returned error: %v", requestID, err)
		}
		if _, err := repo.SettleBilling(core.BillingSettlementInput{
			RequestID:     requestID,
			ClientID:      clientID,
			Model:         "gpt-token",
			Usage:         usage,
			ActualNanoUSD: 1,
			MissingUsage:  missing,
		}); err != nil {
			t.Fatalf("SettleBilling(%s) returned error: %v", requestID, err)
		}
	}
	settle("req_token_a", clientA, userA, core.Usage{PromptTokens: 10, CachedPromptTokens: 3, CacheCreationTokens: 4, CompletionTokens: 5, ImageOutputTokens: 2, TotalTokens: 19}, false)
	settle("req_token_b", clientB, userB, core.Usage{PromptTokens: 7, CompletionTokens: 2}, false)
	settle("req_token_missing", clientA, userA, core.Usage{PromptTokens: 100, CompletionTokens: 100, TotalTokens: 200}, true)
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:   "req_token_reserved",
		ClientID:    clientA,
		ClientName:  clientA,
		UserID:      userA,
		Model:       "gpt-token",
		Fingerprint: "req_token_reserved",
	}); err != nil {
		t.Fatalf("ReserveBilling reserved returned error: %v", err)
	}

	today := time.Now()
	start := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location())
	site := repo.ListTokenUsageDailySummaries(start, 1, 1)
	if len(site) != 1 || site[0].RequestCount != 2 || site[0].PromptTokens != 17 || site[0].CachedTokens != 3 || site[0].CacheCreationTokens != 4 || site[0].CompletionTokens != 7 || site[0].ImageOutputTokens != 2 || site[0].TotalTokens != 28 {
		t.Fatalf("site token summary = %#v, want 2 requests prompt 17 cached 3 cache creation 4 completion 7 image 2 total 28", site)
	}
	users := repo.ListTokenUsageDailyUserSummaries(start, 1, 10)
	if len(users) != 2 {
		t.Fatalf("user token summaries = %#v, want 2 users", users)
	}
	byUser := map[string]TokenUsageDailySummary{}
	for _, summary := range users {
		byUser[summary.UserID] = summary
	}
	if got := byUser[userA]; got.RequestCount != 1 || got.PromptTokens != 10 || got.CachedTokens != 3 || got.CacheCreationTokens != 4 || got.CompletionTokens != 5 || got.ImageOutputTokens != 2 || got.TotalTokens != 19 || got.Username != "token-a" {
		t.Fatalf("user A token summary = %#v", got)
	}
	if got := byUser[userB]; got.RequestCount != 1 || got.PromptTokens != 7 || got.CompletionTokens != 2 || got.TotalTokens != 9 || got.Username != "token-b" {
		t.Fatalf("user B token summary = %#v", got)
	}

	if sqlitePath != "" {
		if closer, ok := repo.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
		reopened, err := NewSQLiteRepository(sqlitePath, "")
		if err != nil {
			t.Fatalf("reopen SQLite returned error: %v", err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		site = reopened.ListTokenUsageDailySummaries(start, 1, 1)
		if len(site) != 1 || site[0].CacheCreationTokens != 4 || site[0].ImageOutputTokens != 2 || site[0].TotalTokens != 28 {
			t.Fatalf("reopened site token summary = %#v, want cache creation 4 image 2 total 28", site)
		}
	}
}

func runFinancePlanPurchaseUsesBalanceSpend(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_finance_plan_purchase"
	startBalance := int64(20 * core.NanoUSDPerUSD)
	planPrice := int64(10 * core.NanoUSDPerUSD)
	now := time.Now()
	startOfDay := time.Date(now.Local().Year(), now.Local().Month(), now.Local().Day(), 0, 0, 0, 0, now.Local().Location())

	if err := repo.UpsertUser(core.User{ID: userID, Username: "finance-plan", Enabled: true, BalanceNanoUSD: startBalance}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "finance_plan",
		Name:               "Finance Plan",
		Enabled:            true,
		PriceNanoUSD:       planPrice,
		PeriodQuotaNanoUSD: 100 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: "finance_plan"}); err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}

	stats := repo.FinanceOverviewStats(startOfDay, startOfDay.AddDate(0, 0, 1))
	if stats.TodayIncomeNanoUSD != 0 || stats.PaidOrderNanoUSD != 0 || stats.TodaySpendNanoUSD != 0 {
		t.Fatalf("stats = %#v, want no payment income and no request spend", stats)
	}
	users, total := repo.ListFinanceUserSummariesPage(0, 10)
	if total != 1 || len(users) != 1 || users[0].UserID != userID {
		t.Fatalf("finance users total=%d users=%#v", total, users)
	}
	if users[0].SpendNanoUSD != planPrice {
		t.Fatalf("finance user spend = %d, want plan price %d", users[0].SpendNanoUSD, planPrice)
	}
	if got := repo.UserActualSpendTotal(userID); got != planPrice {
		t.Fatalf("user actual spend total = %d, want plan price %d", got, planPrice)
	}
	if got := repo.FinanceTotalSpendNanoUSD(); got != planPrice {
		t.Fatalf("finance total spend = %d, want plan price %d", got, planPrice)
	}
	if users[0].BalanceNanoUSD != startBalance-planPrice {
		t.Fatalf("finance user balance = %d, want %d", users[0].BalanceNanoUSD, startBalance-planPrice)
	}
	if users[0].RechargeNanoUSD != 0 {
		t.Fatalf("finance user recharge = %d, want 0", users[0].RechargeNanoUSD)
	}
}

func TestMemoryRepositoryFinanceOverviewSplitsBalanceAndPlanSpend(t *testing.T) {
	runFinanceOverviewSplitsBalanceAndPlanSpend(t, NewMemoryRepository())
}

func TestSQLiteRepositoryFinanceOverviewSplitsBalanceAndPlanSpend(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	runFinanceOverviewSplitsBalanceAndPlanSpend(t, repo)
}

func runFinanceOverviewSplitsBalanceAndPlanSpend(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_finance_spend_split"
	cashClientID := "client_finance_spend_split_cash"
	planClientID := "client_finance_spend_split_plan"
	balanceSpend := int64(2 * core.NanoUSDPerUSD)
	planSpend := int64(3 * core.NanoUSDPerUSD)
	now := time.Now()
	startOfDay := time.Date(now.Local().Year(), now.Local().Month(), now.Local().Day(), 0, 0, 0, 0, now.Local().Location())

	if err := repo.UpsertUser(core.User{ID: userID, Username: "finance-split", Enabled: true, BalanceNanoUSD: 20 * core.NanoUSDPerUSD}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: cashClientID, Name: "Finance Split Cash", APIKey: "gw_finance_split_cash", OwnerUserID: userID, Enabled: true, BillingSource: core.ClientBillingSourceCash}); err != nil {
		t.Fatalf("UpsertClient cash returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: planClientID, Name: "Finance Split Plan", APIKey: "gw_finance_split_plan", OwnerUserID: userID, Enabled: true, BillingSource: core.ClientBillingSourcePlan}); err != nil {
		t.Fatalf("UpsertClient plan returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "finance_split_plan",
		Name:               "Finance Split Plan",
		Enabled:            true,
		PriceNanoUSD:       core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 10 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: "finance_split_plan"}); err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}

	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_finance_spend_split_cash",
		ClientID:        cashClientID,
		UserID:          userID,
		BillingSource:   core.ClientBillingSourceCash,
		ReservedNanoUSD: core.NanoUSDPerUSD,
		Fingerprint:     "finance-spend-split-cash",
	}); err != nil {
		t.Fatalf("ReserveBilling cash returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_finance_spend_split_cash",
		ClientID:      cashClientID,
		ActualNanoUSD: balanceSpend,
	}); err != nil {
		t.Fatalf("SettleBilling cash returned error: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_finance_spend_split_plan",
		ClientID:        planClientID,
		UserID:          userID,
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: core.NanoUSDPerUSD,
		Fingerprint:     "finance-spend-split-plan",
	}); err != nil {
		t.Fatalf("ReserveBilling plan returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_finance_spend_split_plan",
		ClientID:      planClientID,
		ActualNanoUSD: planSpend,
	}); err != nil {
		t.Fatalf("SettleBilling plan returned error: %v", err)
	}

	stats := repo.FinanceOverviewStats(startOfDay, startOfDay.AddDate(0, 0, 1))
	if stats.TodaySpendNanoUSD != balanceSpend+planSpend {
		t.Fatalf("today spend = %d, want %d", stats.TodaySpendNanoUSD, balanceSpend+planSpend)
	}
	if stats.TodayBalanceSpendNanoUSD != balanceSpend || stats.TodayPlanSpendNanoUSD != planSpend {
		t.Fatalf("spend split = balance %d plan %d, want balance %d plan %d", stats.TodayBalanceSpendNanoUSD, stats.TodayPlanSpendNanoUSD, balanceSpend, planSpend)
	}
}

func TestSQLiteRepositoryFinanceClientOwnerTransferMovesPlanUsage(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	userA := "user_finance_owner_plan_a"
	userB := "user_finance_owner_plan_b"
	clientID := "client_finance_owner_plan"
	planSpend := int64(3 * core.NanoUSDPerUSD)
	if err := repo.UpsertUser(core.User{ID: userA, Username: "owner-plan-a", Enabled: true, BalanceNanoUSD: 20 * core.NanoUSDPerUSD}); err != nil {
		t.Fatalf("UpsertUser A returned error: %v", err)
	}
	if err := repo.UpsertUser(core.User{ID: userB, Username: "owner-plan-b", Enabled: true, BalanceNanoUSD: 20 * core.NanoUSDPerUSD}); err != nil {
		t.Fatalf("UpsertUser B returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: clientID, Name: "Owner Plan", APIKey: "gw_owner_plan", OwnerUserID: userA, Enabled: true, BillingSource: core.ClientBillingSourcePlan}); err != nil {
		t.Fatalf("UpsertClient A returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "owner_transfer_plan",
		Name:               "Owner Transfer Plan",
		Enabled:            true,
		PriceNanoUSD:       core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 10 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userA, PlanID: "owner_transfer_plan"}); err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_owner_transfer_plan",
		ClientID:        clientID,
		UserID:          userA,
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: core.NanoUSDPerUSD,
		Fingerprint:     "req_owner_transfer_plan",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_owner_transfer_plan",
		ClientID:      clientID,
		ActualNanoUSD: planSpend,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: clientID, Name: "Owner Plan", APIKey: "gw_owner_plan", OwnerUserID: userB, Enabled: true, BillingSource: core.ClientBillingSourcePlan}); err != nil {
		t.Fatalf("UpsertClient B returned error: %v", err)
	}

	users, total := repo.ListFinanceUserSummariesPage(0, 10)
	if total != 2 {
		t.Fatalf("finance users total=%d users=%#v, want 2", total, users)
	}
	byID := make(map[string]FinanceUserSummary, len(users))
	for _, user := range users {
		byID[user.UserID] = user
	}
	if got := byID[userA].PlanSpendNanoUSD; got != 0 {
		t.Fatalf("previous owner plan spend = %d, want 0", got)
	}
	if got := byID[userA].UsageSpendNanoUSD; got != 0 {
		t.Fatalf("previous owner usage spend = %d, want 0", got)
	}
	if got := byID[userB].PlanSpendNanoUSD; got != planSpend {
		t.Fatalf("next owner plan spend = %d, want %d", got, planSpend)
	}
	if got := byID[userB].UsageSpendNanoUSD; got != planSpend {
		t.Fatalf("next owner usage spend = %d, want %d", got, planSpend)
	}
	if got := byID[userB].SpendNanoUSD; got != 0 {
		t.Fatalf("next owner cash spend = %d, want 0", got)
	}
}

func TestSQLiteRepositoryDeleteClientReleasesPlanReservationFinanceRollups(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	runDeleteClientReleasesPlanReservationFinanceRollups(t, repo)
}

func runDeleteClientReleasesPlanReservationFinanceRollups(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_finance_delete_plan_reservation"
	clientID := "client_finance_delete_plan_reservation"
	planID := "finance_delete_plan_reservation"
	planPrice := int64(core.NanoUSDPerUSD)
	planReservation := int64(3 * core.NanoUSDPerUSD)
	startBalance := int64(20 * core.NanoUSDPerUSD)

	if err := repo.UpsertUser(core.User{ID: userID, Username: "delete-plan-reservation", Enabled: true, BalanceNanoUSD: startBalance}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:            clientID,
		Name:          "Delete Plan Reservation",
		APIKey:        "gw_delete_plan_reservation",
		OwnerUserID:   userID,
		Enabled:       true,
		BillingSource: core.ClientBillingSourcePlan,
	}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 planID,
		Name:               "Delete Plan Reservation",
		Enabled:            true,
		PriceNanoUSD:       planPrice,
		PeriodQuotaNanoUSD: 10 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: planID}); err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_finance_delete_plan_reservation",
		ClientID:        clientID,
		ClientName:      "Delete Plan Reservation",
		UserID:          userID,
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: planReservation,
		Fingerprint:     "req_finance_delete_plan_reservation",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}

	users, total := repo.ListFinanceUserSummariesPage(0, 10)
	if total != 1 || len(users) != 1 || users[0].UserID != userID {
		t.Fatalf("finance users before delete total=%d users=%#v", total, users)
	}
	if users[0].SpendNanoUSD != planPrice || users[0].UsageSpendNanoUSD != 0 || users[0].PlanSpendNanoUSD != 0 {
		t.Fatalf("finance user before delete = %#v, want cash spend %d and zero pending request usage", users[0], planPrice)
	}

	if err := repo.DeleteClient(clientID); err != nil {
		t.Fatalf("DeleteClient returned error: %v", err)
	}

	users, total = repo.ListFinanceUserSummariesPage(0, 10)
	if total != 1 || len(users) != 1 || users[0].UserID != userID {
		t.Fatalf("finance users after delete total=%d users=%#v", total, users)
	}
	if users[0].SpendNanoUSD != planPrice {
		t.Fatalf("cash spend after delete = %d, want plan purchase spend %d", users[0].SpendNanoUSD, planPrice)
	}
	if users[0].UsageSpendNanoUSD != 0 || users[0].PlanSpendNanoUSD != 0 {
		t.Fatalf("request usage after delete = usage %d plan %d, want 0", users[0].UsageSpendNanoUSD, users[0].PlanSpendNanoUSD)
	}
	if users[0].BalanceNanoUSD != startBalance-planPrice {
		t.Fatalf("balance after delete = %d, want %d", users[0].BalanceNanoUSD, startBalance-planPrice)
	}

	clients := repo.ListFinanceTopClientsBySpend(10)
	if len(clients) != 1 || clients[0].ClientID != clientID {
		t.Fatalf("finance clients after delete = %#v, want deleted client history", clients)
	}
	if clients[0].SpendUsedNanoUSD != 0 || clients[0].UsageNanoUSD != 0 || clients[0].PlanNanoUSD != 0 {
		t.Fatalf("client usage after delete = %#v, want zero request usage", clients[0])
	}
}
