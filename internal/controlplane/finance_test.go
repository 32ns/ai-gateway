package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func financeTestPage(service *Service, orderFilter PaymentOrderFilter, usageFilter UsageLogFilter) FinancePage {
	page := FinancePage{
		Overview: service.financePageOverviewBase(),
		Daily:    service.FinanceOverviewDailySummaries(14),
		Models:   service.FinanceOverviewModelSummaries(10),
		Users:    service.FinanceUserSummariesForExport(),
		Orders: service.FinancePageForTab(
			context.Background(),
			"orders",
			FinanceUserFilter{PageSize: 1},
			orderFilter,
			UsageLogFilter{PageSize: 1},
		).Orders,
		Usage: service.FinancePageForTab(
			context.Background(),
			"usage",
			FinanceUserFilter{PageSize: 1},
			PaymentOrderFilter{PageSize: 1},
			usageFilter,
		).Usage,
	}
	page.Overview.TopUsersBySpend = service.financeTopUsersBySpend(5)
	page.Overview.TopClientsBySpend = service.financeTopClientsBySpendFast(5)
	return page
}

func TestFinancePageAggregatesBillingAndPayments(t *testing.T) {
	repo := storage.NewMemoryRepository()
	userID := "user_finance"
	clientID := "client_finance"
	startBalance := int64(100 * core.NanoUSDPerUSD)
	paymentAmount := int64(5 * core.NanoUSDPerUSD)
	actualSpend := int64(2 * core.NanoUSDPerUSD)

	if err := repo.UpsertUser(core.User{ID: userID, Username: "finance", Role: core.UserRoleUser, Enabled: true, BalanceNanoUSD: startBalance}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: clientID, Name: "Finance Client", APIKey: "gw_finance", OwnerUserID: userID, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	order := core.PaymentOrder{
		ID:            "pay_finance",
		OutTradeNo:    "out_finance",
		UserID:        userID,
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: paymentAmount,
		Currency:      "USD",
		Status:        core.PaymentOrderPending,
		CreatedAt:     now,
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}
	if _, credited, err := repo.CompletePaymentOrder(order.OutTradeNo, "trade_finance", paymentAmount, now); err != nil || !credited {
		t.Fatalf("CompletePaymentOrder credited=%t err=%v", credited, err)
	}
	pendingOrder := core.PaymentOrder{
		ID:            "pay_pending",
		OutTradeNo:    "out_pending",
		UserID:        userID,
		Provider:      core.PaymentProviderAlipay,
		AmountNanoUSD: 7 * core.NanoUSDPerUSD,
		Currency:      "USD",
		Status:        core.PaymentOrderPending,
		CreatedAt:     now,
	}
	if err := repo.CreatePaymentOrder(pendingOrder); err != nil {
		t.Fatalf("Create pending payment order returned error: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_finance",
		ClientID:        clientID,
		UserID:          userID,
		Model:           "gpt-4.1",
		ReservedNanoUSD: core.NanoUSDPerUSD,
		Fingerprint:     "req_finance",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_finance",
		ClientID:      clientID,
		Model:         "gpt-4.1",
		Usage:         core.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		ActualNanoUSD: actualSpend,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	page := financeTestPage(service, PaymentOrderFilter{PageSize: 10}, UsageLogFilter{PageSize: 10})

	if got, want := page.Overview.TotalBalanceNanoUSD, startBalance+paymentAmount-actualSpend; got != want {
		t.Fatalf("total balance = %d, want %d", got, want)
	}
	if got := page.Overview.TodayIncomeNanoUSD; got != paymentAmount {
		t.Fatalf("today income = %d, want %d", got, paymentAmount)
	}
	if got := page.Overview.TodaySpendNanoUSD; got != actualSpend {
		t.Fatalf("today spend = %d, want %d", got, actualSpend)
	}
	if got := page.Overview.TodayTotalTokens; got != 15 {
		t.Fatalf("today total tokens = %d, want 15", got)
	}
	if len(page.Daily) != 14 {
		t.Fatalf("daily len = %d, want 14", len(page.Daily))
	}
	if page.Daily[0].IncomeNanoUSD != paymentAmount || page.Daily[0].SpendNanoUSD != actualSpend || page.Daily[0].ProfitNanoUSD != paymentAmount-actualSpend {
		t.Fatalf("today daily summary = %#v", page.Daily[0])
	}
	if len(page.Models) != 1 || page.Models[0].Model != "gpt-4.1" || page.Models[0].SpendNanoUSD != actualSpend || page.Models[0].PromptTokens != 10 || page.Models[0].CompletionTokens != 5 {
		t.Fatalf("models = %#v", page.Models)
	}
	if page.Orders.Total != 1 || len(page.Orders.Orders) != 1 || page.Orders.Orders[0].OutTradeNo != order.OutTradeNo {
		t.Fatalf("orders page total=%d orders=%#v", page.Orders.Total, page.Orders.Orders)
	}
	pendingPage := financeTestPage(service, PaymentOrderFilter{Status: core.PaymentOrderPending, PageSize: 10}, UsageLogFilter{PageSize: 10})
	if pendingPage.Orders.Total != 0 || len(pendingPage.Orders.Orders) != 0 {
		t.Fatalf("pending finance orders total=%d orders=%#v, want empty", pendingPage.Orders.Total, pendingPage.Orders.Orders)
	}
	if page.Overview.PendingOrderNanoUSD != 0 {
		t.Fatalf("pending order amount = %d, want 0", page.Overview.PendingOrderNanoUSD)
	}
	if page.Usage.Total != 1 || len(page.Usage.Rows) != 1 || page.Usage.Rows[0].Request.RequestID != "req_finance" {
		t.Fatalf("usage page total=%d rows=%#v", page.Usage.Total, page.Usage.Rows)
	}
	if len(page.Users) != 1 || page.Users[0].SpendNanoUSD != actualSpend || page.Users[0].RechargeNanoUSD != paymentAmount {
		t.Fatalf("users = %#v", page.Users)
	}
	if len(page.Overview.TopClientsBySpend) != 1 || page.Overview.TopClientsBySpend[0].Spend.SpendUsedNanoUSD != actualSpend {
		t.Fatalf("top clients = %#v", page.Overview.TopClientsBySpend)
	}
}

func TestFinancePageForTabOnlyBuildsRequestedTab(t *testing.T) {
	repo := storage.NewMemoryRepository()
	userID := "user_finance_tab"
	clientID := "client_finance_tab"
	paymentAmount := int64(5 * core.NanoUSDPerUSD)
	actualSpend := int64(2 * core.NanoUSDPerUSD)
	now := time.Now().UTC()

	if err := repo.UpsertUser(core.User{ID: userID, Username: "finance-tab", Role: core.UserRoleUser, Enabled: true, BalanceNanoUSD: 10 * core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: clientID, Name: "Finance Tab Client", APIKey: "gw_finance_tab", OwnerUserID: userID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:            "pay_finance_tab",
		OutTradeNo:    "out_finance_tab",
		UserID:        userID,
		Provider:      core.PaymentProviderAlipay,
		AmountNanoUSD: paymentAmount,
		Status:        core.PaymentOrderPending,
		CreatedAt:     now,
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	if _, credited, err := repo.CompletePaymentOrder(order.OutTradeNo, "trade_finance_tab", paymentAmount, now); err != nil || !credited {
		t.Fatalf("CompletePaymentOrder credited=%t err=%v", credited, err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_finance_tab",
		ClientID:        clientID,
		UserID:          userID,
		Model:           "gpt-4.1",
		ReservedNanoUSD: core.NanoUSDPerUSD,
		Fingerprint:     "req_finance_tab",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_finance_tab",
		ClientID:      clientID,
		Model:         "gpt-4.1",
		ActualNanoUSD: actualSpend,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	page := service.FinancePageForTab(context.Background(), "orders", FinanceUserFilter{PageSize: 10}, PaymentOrderFilter{PageSize: 10}, UsageLogFilter{PageSize: 10})

	if page.Overview.TodayIncomeNanoUSD != paymentAmount || page.Overview.TodaySpendNanoUSD != actualSpend || page.Overview.PaidOrderNanoUSD != paymentAmount {
		t.Fatalf("overview = %#v", page.Overview)
	}
	if page.Orders.Total != 1 || len(page.Orders.Orders) != 1 {
		t.Fatalf("orders = total %d rows %#v", page.Orders.Total, page.Orders.Orders)
	}
	if len(page.Daily) != 0 || len(page.Models) != 0 || len(page.Users) != 0 || len(page.Usage.Rows) != 0 {
		t.Fatalf("orders tab should not prebuild other tabs: daily=%d models=%d users=%d usage=%d", len(page.Daily), len(page.Models), len(page.Users), len(page.Usage.Rows))
	}
}

func TestFinancePageForTabAvoidsFullListFallbacks(t *testing.T) {
	base := storage.NewMemoryRepository()
	if err := base.UpsertUser(core.User{ID: "user_finance_fast_tab", Username: "finance-fast-tab", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	if err := base.UpsertClient(core.APIClient{ID: "client_finance_fast_tab", Name: "Finance Fast Tab", APIKey: "gw_finance_fast_tab", OwnerUserID: "user_finance_fast_tab", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	repo := &financeFullListPanicRepository{MemoryRepository: base}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	for _, tab := range []string{"", "users", "orders", "ledger", "usage", "reconcile"} {
		t.Run(tab, func(t *testing.T) {
			_ = service.FinancePageForTab(context.Background(), tab, FinanceUserFilter{Page: 1, PageSize: 10}, PaymentOrderFilter{PageSize: 10}, UsageLogFilter{PageSize: 10})
		})
	}
}

type financeFullListPanicRepository struct {
	*storage.MemoryRepository
}

func (r *financeFullListPanicRepository) ListUsers() []core.User {
	panic("FinancePageForTab should not use full ListUsers fallback")
}

func (r *financeFullListPanicRepository) ListClients() []core.APIClient {
	panic("FinancePageForTab should not use full ListClients fallback")
}

func (r *financeFullListPanicRepository) ListBillingUsageSpendByClient() []storage.BillingUsageSpendSummary {
	panic("FinancePageForTab should not use full ListBillingUsageSpendByClient fallback")
}

func (r *financeFullListPanicRepository) ListBillingLedgerUserSummaries() []storage.BillingLedgerUserSummary {
	panic("FinancePageForTab should not use full ListBillingLedgerUserSummaries fallback")
}

func (r *financeFullListPanicRepository) ListClientActualSpends() []core.ClientSpend {
	panic("FinancePageForTab should not use full ListClientActualSpends fallback")
}

func (r *financeFullListPanicRepository) ListPaymentOrders(query storage.PaymentOrderQuery) []core.PaymentOrder {
	panic("FinancePageForTab should not use full ListPaymentOrders fallback")
}

func (r *financeFullListPanicRepository) ListBillingRequests(query storage.BillingRequestQuery) []core.BillingReservation {
	panic("FinancePageForTab should not use full ListBillingRequests fallback")
}

func (r *financeFullListPanicRepository) ListBillingRequestsPage(query storage.BillingRequestQuery) ([]core.BillingReservation, int) {
	return r.MemoryRepository.ListBillingRequestsPage(query)
}

func (r *financeFullListPanicRepository) ListFinanceReconcileIssues(now time.Time) []storage.FinanceReconcileIssueSummary {
	return nil
}

func TestFinanceUsersTabPaginatesSummaries(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for i := range 30 {
		if err := repo.UpsertUser(core.User{
			ID:       fmt.Sprintf("user_finance_page_%02d", i),
			Username: fmt.Sprintf("finance-page-%02d", i),
			Role:     core.UserRoleUser,
			Enabled:  true,
		}); err != nil {
			t.Fatalf("UpsertUser(%d) returned error: %v", i, err)
		}
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	page := service.FinancePageForTab(context.Background(), "users", FinanceUserFilter{Page: 2, PageSize: 10}, PaymentOrderFilter{PageSize: 1}, UsageLogFilter{PageSize: 1})

	if page.UserPage.Total != 30 || page.UserPage.Page != 2 || page.UserPage.PageSize != 10 || !page.UserPage.HasPrev || !page.UserPage.HasNext {
		t.Fatalf("user page = %#v, want middle page of 30", page.UserPage)
	}
	if len(page.Users) != 10 {
		t.Fatalf("users len = %d, want 10", len(page.Users))
	}
	if page.Users[0].User.Username != "finance-page-10" || page.Users[9].User.Username != "finance-page-19" {
		t.Fatalf("paged users = %#v, want finance-page-10..19", page.Users)
	}
}

func TestFinancePageKeepsSpendAfterClientDeleteAndUsageLogTrim(t *testing.T) {
	repo := storage.NewMemoryRepository()
	userID := "user_finance_trimmed"
	clientID := "client_finance_trimmed"
	actualSpend := int64(3 * core.NanoUSDPerUSD)

	if err := repo.UpsertUser(core.User{ID: userID, Username: "finance-trimmed", Role: core.UserRoleUser, Enabled: true, BalanceNanoUSD: 20 * core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: clientID, Name: "Finance Trimmed", APIKey: "gw_finance_trimmed", OwnerUserID: userID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_finance_trimmed",
		ClientID:        clientID,
		ClientName:      "Finance Trimmed",
		UserID:          userID,
		Model:           "gpt-4.1",
		ReservedNanoUSD: core.NanoUSDPerUSD,
		Fingerprint:     "req_finance_trimmed",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_finance_trimmed",
		ClientID:      clientID,
		Model:         "gpt-4.1",
		Usage:         core.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		ActualNanoUSD: actualSpend,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	if err := repo.DeleteClient(clientID); err != nil {
		t.Fatalf("DeleteClient returned error: %v", err)
	}
	if err := repo.ConfigureUsageLogRetention(0); err != nil {
		t.Fatalf("ConfigureUsageLogRetention returned error: %v", err)
	}
	if _, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: clientID, Limit: 10}); total != 0 {
		t.Fatalf("billing requests total = %d, want trimmed", total)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	page := financeTestPage(service, PaymentOrderFilter{PageSize: 10}, UsageLogFilter{PageSize: 10})

	if got := page.Overview.TodaySpendNanoUSD; got != actualSpend {
		t.Fatalf("today spend = %d, want %d", got, actualSpend)
	}
	if len(page.Daily) == 0 || page.Daily[0].SpendNanoUSD != actualSpend {
		t.Fatalf("daily = %#v, want spend %d", page.Daily, actualSpend)
	}
	if len(page.Users) != 1 || page.Users[0].SpendNanoUSD != actualSpend {
		t.Fatalf("users = %#v, want spend %d", page.Users, actualSpend)
	}
	var clientSummary *FinanceClientSpendSummary
	for i := range page.Overview.TopClientsBySpend {
		if page.Overview.TopClientsBySpend[i].Client.ID == clientID {
			clientSummary = &page.Overview.TopClientsBySpend[i]
			break
		}
	}
	if clientSummary == nil || clientSummary.Spend.SpendUsedNanoUSD != actualSpend {
		t.Fatalf("top clients = %#v, want deleted client spend %d", page.Overview.TopClientsBySpend, actualSpend)
	}
}

func TestFinancePageKeepsDeletedUserAndClientInFinancialSummaries(t *testing.T) {
	repo := storage.NewMemoryRepository()
	userID := "user_deleted_finance"
	clientID := "client_deleted_finance"
	paymentAmount := int64(5 * core.NanoUSDPerUSD)
	actualSpend := int64(2 * core.NanoUSDPerUSD)
	now := time.Now().UTC()

	if err := repo.UpsertUser(core.User{ID: userID, Username: "deleted-finance", Role: core.UserRoleUser, Enabled: true, BalanceNanoUSD: 10 * core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: clientID, Name: "Deleted Finance Client", APIKey: "gw_deleted_finance", OwnerUserID: userID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:            "pay_deleted_finance",
		OutTradeNo:    "out_deleted_finance",
		UserID:        userID,
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: paymentAmount,
		Status:        core.PaymentOrderPending,
		CreatedAt:     now,
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	if _, credited, err := repo.CompletePaymentOrder(order.OutTradeNo, "trade_deleted_finance", paymentAmount, now); err != nil || !credited {
		t.Fatalf("CompletePaymentOrder credited=%t err=%v", credited, err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_deleted_finance",
		ClientID:        clientID,
		ClientName:      "Deleted Finance Client",
		UserID:          userID,
		Model:           "gpt-4.1",
		ReservedNanoUSD: core.NanoUSDPerUSD,
		Fingerprint:     "req_deleted_finance",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_deleted_finance",
		ClientID:      clientID,
		Model:         "gpt-4.1",
		Usage:         core.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		ActualNanoUSD: actualSpend,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if _, err := service.DeleteUser(userID); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	page := financeTestPage(service, PaymentOrderFilter{PageSize: 10}, UsageLogFilter{PageSize: 10})

	if page.Overview.TotalUsers != 1 || page.Overview.TotalClients != 1 || page.Overview.ActiveClients != 0 {
		t.Fatalf("deleted overview counts = users %d clients %d active %d", page.Overview.TotalUsers, page.Overview.TotalClients, page.Overview.ActiveClients)
	}

	var userSummary *FinanceUserSummary
	for i := range page.Users {
		if page.Users[i].User.ID == userID {
			userSummary = &page.Users[i]
			break
		}
	}
	if userSummary == nil || userSummary.User.Username != userID || userSummary.SpendNanoUSD != actualSpend || userSummary.RechargeNanoUSD != paymentAmount {
		t.Fatalf("deleted user summary = %#v, users=%#v", userSummary, page.Users)
	}
	var clientSummary *FinanceClientSpendSummary
	for i := range page.Overview.TopClientsBySpend {
		if page.Overview.TopClientsBySpend[i].Client.ID == clientID {
			clientSummary = &page.Overview.TopClientsBySpend[i]
			break
		}
	}
	if clientSummary == nil || clientSummary.Client.Name != "Deleted Finance Client" || clientSummary.Client.OwnerUserID != userID || clientSummary.Spend.SpendUsedNanoUSD != actualSpend {
		t.Fatalf("deleted client summary = %#v, top clients=%#v", clientSummary, page.Overview.TopClientsBySpend)
	}
}

func TestFinancePageAggregatesPaymentRefunds(t *testing.T) {
	repo := storage.NewMemoryRepository()
	userID := "user_refund_finance"
	amount := int64(5 * core.NanoUSDPerUSD)
	refundAmount := int64(2 * core.NanoUSDPerUSD)
	now := time.Now().UTC()

	if err := repo.UpsertUser(core.User{ID: userID, Username: "refund-finance", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:            "pay_refund_finance",
		OutTradeNo:    "out_refund_finance",
		UserID:        userID,
		Provider:      core.PaymentProviderAlipay,
		AmountNanoUSD: amount,
		Status:        core.PaymentOrderPending,
		CreatedAt:     now,
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	if _, credited, err := repo.CompletePaymentOrder(order.OutTradeNo, "trade_refund_finance", amount, now); err != nil || !credited {
		t.Fatalf("CompletePaymentOrder credited=%t err=%v", credited, err)
	}
	refund := core.PaymentRefund{
		ID:            "refund_finance",
		OrderID:       order.ID,
		OutTradeNo:    order.OutTradeNo,
		UserID:        userID,
		Provider:      order.Provider,
		AmountNanoUSD: refundAmount,
		Status:        core.PaymentRefundPending,
		CreatedAt:     now,
	}
	if err := repo.CreatePaymentRefund(refund); err != nil {
		t.Fatal(err)
	}
	if _, debited, err := repo.CompletePaymentRefund(refund.ID, "provider_refund_finance", "raw"); err != nil || !debited {
		t.Fatalf("CompletePaymentRefund debited=%t err=%v", debited, err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	page := financeTestPage(service, PaymentOrderFilter{PageSize: 10}, UsageLogFilter{PageSize: 10})
	if len(page.Users) != 1 || page.Users[0].RechargeNanoUSD != amount || page.Users[0].RefundNanoUSD != refundAmount {
		t.Fatalf("users = %#v", page.Users)
	}
	if page.Overview.PaidOrderNanoUSD != amount {
		t.Fatalf("paid order amount = %d, want %d", page.Overview.PaidOrderNanoUSD, amount)
	}
	if page.Overview.TodayIncomeNanoUSD != amount-refundAmount {
		t.Fatalf("today income = %d, want %d", page.Overview.TodayIncomeNanoUSD, amount-refundAmount)
	}
	if page.Daily[0].IncomeNanoUSD != amount-refundAmount || page.Daily[0].ProfitNanoUSD != amount-refundAmount {
		t.Fatalf("today daily summary = %#v", page.Daily[0])
	}
}

func TestFinancePageExcludesFullyRefundedOrdersFromRefundableList(t *testing.T) {
	repo := storage.NewMemoryRepository()
	userID := "user_full_refund_finance"
	amount := int64(5 * core.NanoUSDPerUSD)
	now := time.Now().UTC()

	if err := repo.UpsertUser(core.User{ID: userID, Username: "full-refund-finance", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:            "pay_full_refund_finance",
		OutTradeNo:    "out_full_refund_finance",
		UserID:        userID,
		Provider:      core.PaymentProviderAlipay,
		AmountNanoUSD: amount,
		Status:        core.PaymentOrderPending,
		CreatedAt:     now,
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	if _, credited, err := repo.CompletePaymentOrder(order.OutTradeNo, "trade_full_refund_finance", amount, now); err != nil || !credited {
		t.Fatalf("CompletePaymentOrder credited=%t err=%v", credited, err)
	}
	refund := core.PaymentRefund{
		ID:            "refund_full_finance",
		OrderID:       order.ID,
		OutTradeNo:    order.OutTradeNo,
		UserID:        userID,
		Provider:      order.Provider,
		AmountNanoUSD: amount,
		Status:        core.PaymentRefundPending,
		CreatedAt:     now,
	}
	if err := repo.CreatePaymentRefund(refund); err != nil {
		t.Fatal(err)
	}
	if _, debited, err := repo.CompletePaymentRefund(refund.ID, "provider_full_refund_finance", "raw"); err != nil || !debited {
		t.Fatalf("CompletePaymentRefund debited=%t err=%v", debited, err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	page := financeTestPage(service, PaymentOrderFilter{PageSize: 10}, UsageLogFilter{PageSize: 10})
	if page.Overview.PaidOrderNanoUSD != amount {
		t.Fatalf("paid order amount = %d, want %d", page.Overview.PaidOrderNanoUSD, amount)
	}
}

func TestFinancePageUsesSettledBillingCostForUserSpend(t *testing.T) {
	repo := storage.NewMemoryRepository()
	userID := "user_over_reserved_finance"
	clientID := "client_over_reserved_finance"
	reserved := int64(10 * core.NanoUSDPerUSD)
	actual := int64(2 * core.NanoUSDPerUSD)

	if err := repo.UpsertUser(core.User{ID: userID, Username: "over-reserved", Enabled: true, BalanceNanoUSD: 20 * core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: clientID, Name: "Over Reserved", APIKey: "gw_over_reserved", OwnerUserID: userID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_over_reserved",
		ClientID:        clientID,
		UserID:          userID,
		Model:           "gpt-4.1",
		ReservedNanoUSD: reserved,
		Fingerprint:     "req_over_reserved",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_over_reserved",
		ClientID:      clientID,
		Model:         "gpt-4.1",
		Usage:         core.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		ActualNanoUSD: actual,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_missing_usage",
		ClientID:        clientID,
		UserID:          userID,
		Model:           "gpt-4.1",
		ReservedNanoUSD: reserved,
		Fingerprint:     "req_missing_usage",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_missing_usage",
		ClientID:      clientID,
		Model:         "gpt-4.1",
		ActualNanoUSD: 0,
		MissingUsage:  true,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	page := financeTestPage(service, PaymentOrderFilter{PageSize: 10}, UsageLogFilter{PageSize: 10})
	if len(page.Users) != 1 || page.Users[0].SpendNanoUSD != actual {
		t.Fatalf("users = %#v, want spend %d", page.Users, actual)
	}
	if page.Overview.TodaySpendNanoUSD != actual || page.Daily[0].SpendNanoUSD != actual {
		t.Fatalf("overview spend=%d daily=%#v, want %d", page.Overview.TodaySpendNanoUSD, page.Daily[0], actual)
	}
}

func TestFinanceDailySpendSplitPaginatesBillingRequests(t *testing.T) {
	repo := storage.NewMemoryRepository()
	userID := "user_finance_daily_paged"
	cashClientID := "client_finance_daily_paged_cash"
	planClientID := "client_finance_daily_paged_plan"
	cashSpend := int64(core.NanoUSDPerUSD)
	planSpend := int64(core.NanoUSDPerUSD)
	paymentAmount := int64(200 * core.NanoUSDPerUSD)
	planRequests := 120

	if err := repo.UpsertUser(core.User{ID: userID, Username: "daily-paged", Enabled: true, BalanceNanoUSD: paymentAmount}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: cashClientID, Name: "Daily Paged Cash", APIKey: "gw_daily_paged_cash", OwnerUserID: userID, Enabled: true, BillingSource: core.ClientBillingSourceCash}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: planClientID, Name: "Daily Paged Plan", APIKey: "gw_daily_paged_plan", OwnerUserID: userID, Enabled: true, BillingSource: core.ClientBillingSourcePlan}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "daily_paged_plan",
		Name:               "Daily Paged Plan",
		Enabled:            true,
		PriceNanoUSD:       core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: int64(planRequests+1) * planSpend,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:            "pay_daily_paged",
		OutTradeNo:    "out_daily_paged",
		UserID:        userID,
		Provider:      core.PaymentProviderAlipay,
		AmountNanoUSD: paymentAmount,
		Status:        core.PaymentOrderPending,
		CreatedAt:     time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	if _, credited, err := repo.CompletePaymentOrder(order.OutTradeNo, "trade_daily_paged", paymentAmount, time.Now().UTC()); err != nil || !credited {
		t.Fatalf("CompletePaymentOrder credited=%t err=%v", credited, err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: userID, PlanID: "daily_paged_plan"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_daily_paged_cash",
		ClientID:        cashClientID,
		UserID:          userID,
		BillingSource:   core.ClientBillingSourceCash,
		ReservedNanoUSD: cashSpend,
		Fingerprint:     "req_daily_paged_cash",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{RequestID: "req_daily_paged_cash", ClientID: cashClientID, ActualNanoUSD: cashSpend}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < planRequests; i++ {
		requestID := fmt.Sprintf("req_daily_paged_plan_%03d", i)
		if _, err := repo.ReserveBilling(core.BillingReservationInput{
			RequestID:       requestID,
			ClientID:        planClientID,
			UserID:          userID,
			BillingSource:   core.ClientBillingSourcePlan,
			ReservedNanoUSD: planSpend,
			Fingerprint:     requestID,
		}); err != nil {
			t.Fatalf("ReserveBilling(%d) returned error: %v", i, err)
		}
		if _, err := repo.SettleBilling(core.BillingSettlementInput{RequestID: requestID, ClientID: planClientID, ActualNanoUSD: planSpend}); err != nil {
			t.Fatalf("SettleBilling(%d) returned error: %v", i, err)
		}
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	daily := service.FinanceOverviewDailySummaries(1)
	wantPlanSpend := int64(planRequests) * planSpend
	wantTotalSpend := cashSpend + wantPlanSpend
	if len(daily) != 1 {
		t.Fatalf("daily len = %d, want 1", len(daily))
	}
	if daily[0].SpendNanoUSD != wantTotalSpend || daily[0].BalanceSpendNanoUSD != cashSpend || daily[0].PlanSpendNanoUSD != wantPlanSpend {
		t.Fatalf("daily split = %#v, want total %d balance %d plan %d", daily[0], wantTotalSpend, cashSpend, wantPlanSpend)
	}
	if daily[0].ProfitNanoUSD != paymentAmount-cashSpend {
		t.Fatalf("daily profit = %d, want %d", daily[0].ProfitNanoUSD, paymentAmount-cashSpend)
	}
}

func TestFinanceUsersIncludeLedgerSummariesBeyondRecentLedgerLimit(t *testing.T) {
	repo := storage.NewMemoryRepository()
	userID := "user_finance_many_ledger"

	if err := repo.UpsertUser(core.User{ID: userID, Username: "many-ledger", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10001; i++ {
		if _, _, err := repo.AdjustUserBalance(userID, 1, "reward"); err != nil {
			t.Fatalf("AdjustUserBalance(%d) returned error: %v", i, err)
		}
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	page := service.FinancePageForTab(context.Background(), "users", FinanceUserFilter{Page: 1, PageSize: 10}, PaymentOrderFilter{PageSize: 10}, UsageLogFilter{PageSize: 10})

	if len(page.Users) != 1 || page.Users[0].RewardNanoUSD != 10001 {
		t.Fatalf("users = %#v, want all ledger rewards included", page.Users)
	}
}

func TestReleaseAbandonedGatewayReservationsReleasesOldReservedRequests(t *testing.T) {
	repo := storage.NewMemoryRepository()
	userID := "user_abandoned_reserved"
	clientID := "client_abandoned_reserved"
	startingBalance := int64(core.NanoUSDPerUSD)

	if err := repo.UpsertUser(core.User{ID: userID, Username: "abandoned", Enabled: true, BalanceNanoUSD: startingBalance}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: clientID, Name: "Abandoned", APIKey: "gw_abandoned", OwnerUserID: userID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_abandoned_reserved",
		ClientID:        clientID,
		UserID:          userID,
		Model:           "gpt-image-2",
		ReservedNanoUSD: 100,
		Fingerprint:     "req_abandoned_reserved",
	}); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().UTC().Add(time.Second)

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	summary, err := service.ReleaseAbandonedGatewayReservations(cutoff, "test restart")
	if err != nil {
		t.Fatalf("ReleaseAbandonedGatewayReservations returned error: %v", err)
	}
	if summary.Count != 1 || summary.AmountNanoUSD != 0 {
		t.Fatalf("summary = %#v, want one released pending request with zero amount", summary)
	}

	requests, _ := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: clientID, Limit: 10})
	byID := make(map[string]core.BillingReservation, len(requests))
	for _, request := range requests {
		byID[request.RequestID] = request
	}
	if byID["req_abandoned_reserved"].Status != core.BillingRequestReleased {
		t.Fatalf("abandoned request status = %s, want released", byID["req_abandoned_reserved"].Status)
	}
	user, err := repo.GetUser(userID)
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != startingBalance {
		t.Fatalf("balance = %d, want %d", user.BalanceNanoUSD, startingBalance)
	}
}
