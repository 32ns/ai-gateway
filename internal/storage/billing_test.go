package storage

import (
	"errors"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestMemoryRepositoryBillingRequiresClient(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_billing", Username: "billing", Enabled: true, BalanceNanoUSD: 1000}); err != nil {
		t.Fatal(err)
	}

	_, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_missing_client",
		ClientID:        "client_missing",
		UserID:          "user_billing",
		ReservedNanoUSD: 1,
		Fingerprint:     "missing",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ReserveBilling missing client err = %v", err)
	}
}

func TestMemoryRepositoryBillingRequiresClientOwner(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_a", Username: "a", Enabled: true, BalanceNanoUSD: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertUser(core.User{ID: "user_b", Username: "b", Enabled: true, BalanceNanoUSD: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_a", Name: "A", APIKey: "gw_a", OwnerUserID: "user_a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	_, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_owner",
		ClientID:        "client_a",
		UserID:          "user_b",
		ReservedNanoUSD: 1,
		Fingerprint:     "owner",
	})
	if !errors.Is(err, ErrBillingClientOwnerMismatch) {
		t.Fatalf("ReserveBilling owner mismatch err = %v", err)
	}
}

func TestMemoryRepositoryUpdateBillingAccountTracksReservedAttempt(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_account_update", Username: "account_update", Enabled: true, BalanceNanoUSD: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_account_update", Name: "Account Update", APIKey: "gw_account_update", OwnerUserID: "user_account_update", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:   "req_account_update",
		ClientID:    "client_account_update",
		UserID:      "user_account_update",
		Fingerprint: "account-update",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}

	if err := repo.UpdateBillingAccount(core.BillingAccountUpdateInput{
		RequestID:           "req_account_update",
		ClientID:            "client_account_update",
		AccountID:           "acct_good",
		AccountLabel:        "Good Account",
		FailedAccountLabels: []string{"Bad Account", "Bad Account"},
	}); err != nil {
		t.Fatalf("UpdateBillingAccount returned error: %v", err)
	}

	items, total := repo.ListBillingRequestsPage(BillingRequestQuery{ClientID: "client_account_update", Limit: 10})
	if total != 1 || len(items) != 1 {
		t.Fatalf("billing requests = total %d items %#v, want one", total, items)
	}
	if items[0].AccountID != "acct_good" || items[0].AccountLabel != "Good Account" {
		t.Fatalf("account = %q/%q, want acct_good/Good Account", items[0].AccountID, items[0].AccountLabel)
	}
	if got := items[0].FailedAccountLabels; len(got) != 1 || got[0] != "Bad Account" {
		t.Fatalf("failed account labels = %#v, want Bad Account", got)
	}
}

func TestMemoryRepositorySettleBillingRecordsClientSpendOverLimit(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_billing", Username: "billing", Enabled: true, BalanceNanoUSD: 10000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_billing", Name: "Billing", APIKey: "gw_billing", OwnerUserID: "user_billing", Enabled: true, SpendLimitNanoUSD: 6000}); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_billing",
		ClientID:        "client_billing",
		UserID:          "user_billing",
		ReservedNanoUSD: 5000,
		Fingerprint:     "fp",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}

	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_billing",
		ClientID:      "client_billing",
		ActualNanoUSD: 7000,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	spend, err := repo.GetClientSpend("client_billing")
	if err != nil {
		t.Fatal(err)
	}
	if spend.SpendUsedNanoUSD != 7000 {
		t.Fatalf("spend = %d, want 7000", spend.SpendUsedNanoUSD)
	}
}

func TestMemoryRepositoryListBillingUsageSpendByClientExcludesReserved(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_usage_summary", Username: "usage_summary", Enabled: true, BalanceNanoUSD: 10000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_usage_summary", Name: "Usage Summary", APIKey: "gw_usage_summary", OwnerUserID: "user_usage_summary", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_usage_settled",
		ClientID:        "client_usage_summary",
		UserID:          "user_usage_summary",
		ReservedNanoUSD: 5000,
		Fingerprint:     "usage-settled",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_usage_settled",
		ClientID:      "client_usage_summary",
		ActualNanoUSD: 7000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_usage_reserved",
		ClientID:        "client_usage_summary",
		UserID:          "user_usage_summary",
		ReservedNanoUSD: 1000,
		Fingerprint:     "usage-reserved",
	}); err != nil {
		t.Fatal(err)
	}

	summaries := repo.ListBillingUsageSpendByClient()
	if len(summaries) != 1 || summaries[0].UserID != "user_usage_summary" || summaries[0].ClientID != "client_usage_summary" || summaries[0].SpendNanoUSD != 7000 {
		t.Fatalf("usage spend summaries = %#v, want settled actual spend only", summaries)
	}
	repo.mu.Lock()
	repo.billing[billingRequestKey("req_usage_historical", "client_usage_summary")] = core.BillingReservation{
		ID:            "billing_req_usage_historical",
		RequestID:     "req_usage_historical",
		ClientID:      "client_usage_summary",
		UserID:        "user_usage_summary",
		Model:         "historical-model",
		Status:        core.BillingRequestSettled,
		ActualNanoUSD: 1100,
		Fingerprint:   "usage-historical",
		CreatedAt:     time.Now().UTC(),
	}
	repo.mu.Unlock()
	summaries = repo.ListBillingUsageSpendByClient()
	if len(summaries) != 1 || summaries[0].UserID != "user_usage_summary" || summaries[0].ClientID != "client_usage_summary" || summaries[0].SpendNanoUSD != 8100 {
		t.Fatalf("usage spend summaries with historical request = %#v, want settled request spend", summaries)
	}
	if err := repo.UpsertUser(core.User{ID: "user_usage_other", Username: "usage_other", Enabled: true, BalanceNanoUSD: 10000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_usage_other", Name: "Usage Other", APIKey: "gw_usage_other", OwnerUserID: "user_usage_other", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_usage_other",
		ClientID:        "client_usage_other",
		UserID:          "user_usage_other",
		ReservedNanoUSD: 2000,
		Fingerprint:     "usage-other",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_usage_other",
		ClientID:      "client_usage_other",
		ActualNanoUSD: 3000,
	}); err != nil {
		t.Fatal(err)
	}

	summaries = repo.ListBillingUsageSpendByClientForUser("user_usage_summary")
	if len(summaries) != 1 || summaries[0].UserID != "user_usage_summary" || summaries[0].ClientID != "client_usage_summary" || summaries[0].SpendNanoUSD != 8100 {
		t.Fatalf("filtered usage spend summaries = %#v, want only user_usage_summary", summaries)
	}
}

func TestMemoryRepositoryBillingUsageSpendNanoUSDUsesLedgerWindow(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_usage_window", Username: "usage_window", Enabled: true, BalanceNanoUSD: 20000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_usage_window", Name: "Usage Window", APIKey: "gw_usage_window", OwnerUserID: "user_usage_window", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	start := time.Now().UTC().Add(-time.Second)
	if _, _, err := repo.AdjustUserBalance("user_usage_window", 1234, "manual credit"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_usage_window_settled",
		ClientID:        "client_usage_window",
		UserID:          "user_usage_window",
		ReservedNanoUSD: 5000,
		Fingerprint:     "usage-window-settled",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_usage_window_settled",
		ClientID:      "client_usage_window",
		ActualNanoUSD: 7000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_usage_window_reserved",
		ClientID:        "client_usage_window",
		UserID:          "user_usage_window",
		ReservedNanoUSD: 3000,
		Fingerprint:     "usage-window-reserved",
	}); err != nil {
		t.Fatal(err)
	}
	end := time.Now().UTC().Add(time.Second)

	if got := repo.BillingUsageSpendNanoUSD(start, end); got != 7000 {
		t.Fatalf("usage spend = %d, want settled actual only", got)
	}
	if got := repo.BillingUsageSpendNanoUSD(end, time.Time{}); got != 0 {
		t.Fatalf("future usage spend = %d, want 0", got)
	}
}

func TestMemoryRepositoryListBillingUsageSpendByDayUsesLedgerWindow(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_usage_day", Username: "usage_day", Enabled: true, BalanceNanoUSD: 20000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_usage_day", Name: "Usage Day", APIKey: "gw_usage_day", OwnerUserID: "user_usage_day", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if _, _, err := repo.AdjustUserBalance("user_usage_day", 1234, "manual credit"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_usage_day_settled",
		ClientID:        "client_usage_day",
		UserID:          "user_usage_day",
		ReservedNanoUSD: 5000,
		Fingerprint:     "usage-day-settled",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_usage_day_settled",
		ClientID:      "client_usage_day",
		ActualNanoUSD: 7000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_usage_day_reserved",
		ClientID:        "client_usage_day",
		UserID:          "user_usage_day",
		ReservedNanoUSD: 3000,
		Fingerprint:     "usage-day-reserved",
	}); err != nil {
		t.Fatal(err)
	}

	summaries := repo.ListBillingUsageSpendByDay(startOfDay, 1)
	if len(summaries) != 1 || summaries[0].Date != startOfDay.Format("2006-01-02") || summaries[0].SpendNanoUSD != 7000 {
		t.Fatalf("daily usage spend summaries = %#v, want one day with settled actual only", summaries)
	}
}

func TestMemoryRepositoryListBillingUsageSpendByHourForUser(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_usage_hour", Username: "usage_hour", Enabled: true, BalanceNanoUSD: 20000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_usage_hour", Name: "Usage Hour", APIKey: "gw_usage_hour", OwnerUserID: "user_usage_hour", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_usage_hour_settled",
		ClientID:        "client_usage_hour",
		UserID:          "user_usage_hour",
		ReservedNanoUSD: 5000,
		Fingerprint:     "usage-hour-settled",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_usage_hour_settled",
		ClientID:      "client_usage_hour",
		ActualNanoUSD: 7000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_usage_hour_reserved",
		ClientID:        "client_usage_hour",
		UserID:          "user_usage_hour",
		ReservedNanoUSD: 3000,
		Fingerprint:     "usage-hour-reserved",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_usage_hour_released",
		ClientID:        "client_usage_hour",
		UserID:          "user_usage_hour",
		ReservedNanoUSD: 1000,
		Fingerprint:     "usage-hour-released",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReleaseBilling(core.BillingReleaseInput{
		RequestID: "req_usage_hour_released",
		ClientID:  "client_usage_hour",
		Reason:    "test",
	}); err != nil {
		t.Fatal(err)
	}

	summaries := repo.ListBillingUsageSpendByHourForUser("user_usage_hour", startOfDay, startOfDay.AddDate(0, 0, 1))
	if len(summaries) != 1 || summaries[0].Hour != now.Local().Hour() || summaries[0].SpendNanoUSD != 7000 {
		t.Fatalf("hourly usage spend summaries = %#v, want current hour with settled actual only", summaries)
	}
}

func TestMemoryRepositorySettleBillingRecordsNegativeBalance(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_billing", Username: "billing", Enabled: true, BalanceNanoUSD: 6000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_billing", Name: "Billing", APIKey: "gw_billing", OwnerUserID: "user_billing", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_billing",
		ClientID:        "client_billing",
		UserID:          "user_billing",
		ReservedNanoUSD: 5000,
		Fingerprint:     "fp",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}

	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_billing",
		ClientID:      "client_billing",
		ActualNanoUSD: 7000,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	user, err := repo.GetUser("user_billing")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != -1000 {
		t.Fatalf("balance = %d, want -1000", user.BalanceNanoUSD)
	}
}

func TestMemoryRepositoryUpsertUserAllowsExistingNegativeBalance(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_negative_upsert", Username: "negative-upsert", Enabled: true, BalanceNanoUSD: 6000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_negative_upsert", Name: "Negative Upsert", APIKey: "gw_negative_upsert", OwnerUserID: "user_negative_upsert", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_negative_upsert",
		ClientID:        "client_negative_upsert",
		UserID:          "user_negative_upsert",
		ReservedNanoUSD: 5000,
		Fingerprint:     "negative-upsert",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_negative_upsert",
		ClientID:      "client_negative_upsert",
		ActualNanoUSD: 7000,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	user, err := repo.GetUser("user_negative_upsert")
	if err != nil {
		t.Fatal(err)
	}
	user.Username = "negative-upsert-renamed"
	if err := repo.UpsertUser(user); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	user, err = repo.GetUser("user_negative_upsert")
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != "negative-upsert-renamed" {
		t.Fatalf("username = %q", user.Username)
	}
	if user.BalanceNanoUSD != -1000 {
		t.Fatalf("balance = %d, want -1000", user.BalanceNanoUSD)
	}
}

func TestMemoryRepositorySettleBillingCanMarkFastMode(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_billing", Username: "billing", Enabled: true, BalanceNanoUSD: 10000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_billing", Name: "Billing", APIKey: "gw_billing", OwnerUserID: "user_billing", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_fast_settle",
		ClientID:        "client_billing",
		UserID:          "user_billing",
		ReservedNanoUSD: 1000,
		Fingerprint:     "fp",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}

	settlement, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_fast_settle",
		ClientID:      "client_billing",
		FastMode:      true,
		ActualNanoUSD: 1000,
	})
	if err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	if !settlement.Request.FastMode {
		t.Fatal("settlement FastMode = false, want true")
	}
}

func TestMemoryRepositoryRejectsBalanceOverflow(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_negative", Username: "negative", Enabled: true, BalanceNanoUSD: -1}); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("upsert negative balance err = %v, want ErrInsufficientBalance", err)
	}
	if err := repo.UpsertUser(core.User{ID: "user_overflow", Username: "overflow", Enabled: true, BalanceNanoUSD: maxInt64}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetUserBalance("user_overflow", -1); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("set negative balance err = %v, want ErrInsufficientBalance", err)
	}
	if _, _, err := repo.AdjustUserBalance("user_overflow", 1, "too much"); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("adjust overflow err = %v, want ErrAmountOverflow", err)
	}
	user, err := repo.GetUser("user_overflow")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != maxInt64 {
		t.Fatalf("balance changed after overflow: %d", user.BalanceNanoUSD)
	}
}

func TestMemoryRepositoryRejectsSpendOverflow(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_overflow", Username: "overflow", Enabled: true, BalanceNanoUSD: maxInt64}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_negative", Name: "Negative", APIKey: "gw_negative", OwnerUserID: "user_overflow", Enabled: true, SpendLimitNanoUSD: -1}); !errors.Is(err, ErrClientSpendLimitExceeded) {
		t.Fatalf("upsert negative spend limit err = %v, want ErrClientSpendLimitExceeded", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_overflow", Name: "Overflow", APIKey: "gw_overflow", OwnerUserID: "user_overflow", Enabled: true, SpendLimitNanoUSD: maxInt64}); err != nil {
		t.Fatal(err)
	}
	repo.clientSpend["client_overflow"] = core.ClientSpend{ClientID: "client_overflow", SpendLimitNanoUSD: maxInt64, SpendUsedNanoUSD: maxInt64 - 5}

	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_overflow",
		ClientID:        "client_overflow",
		UserID:          "user_overflow",
		ReservedNanoUSD: 10,
		Fingerprint:     "overflow",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	_, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_overflow",
		ClientID:      "client_overflow",
		ActualNanoUSD: 10,
	})
	if !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("settle overflow err = %v, want ErrAmountOverflow", err)
	}
	user, err := repo.GetUser("user_overflow")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != maxInt64 {
		t.Fatalf("balance changed after overflow: %d", user.BalanceNanoUSD)
	}
}

func TestMemoryRepositoryListBillingRequestsPageFiltersAndPaginates(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_a", Username: "a", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertUser(core.User{ID: "user_b", Username: "b", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_a", Name: "key", APIKey: "gw_a", OwnerUserID: "user_a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_b", Name: "Gamma", APIKey: "gw_b", OwnerUserID: "user_b", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	seedBillingRequest(t, repo, "req_a_1", "client_a", "user_a", "gpt-4.1", 1100)
	time.Sleep(time.Millisecond)
	seedBillingRequest(t, repo, "req_a_2", "client_a", "user_a", "claude-sonnet-4-0", 1200)
	time.Sleep(time.Millisecond)
	seedBillingRequest(t, repo, "req_b_1", "client_b", "user_b", "gpt-4.1", 1300)

	items, total := repo.ListBillingRequestsPage(BillingRequestQuery{
		UserID: "user_a",
		Limit:  1,
	})
	if total != 2 || len(items) != 1 || items[0].RequestID != "req_a_2" {
		t.Fatalf("page 1 = total %d items %#v", total, items)
	}
	items, total = repo.ListBillingRequestsPage(BillingRequestQuery{
		UserID: "user_a",
		Model:  "gpt",
		Limit:  10,
	})
	if total != 1 || len(items) != 1 || items[0].RequestID != "req_a_1" {
		t.Fatalf("model filter = total %d items %#v", total, items)
	}
	items, total = repo.ListBillingRequestsPage(BillingRequestQuery{
		UserID: "a",
		Limit:  10,
	})
	if total != 2 || len(items) != 2 || items[0].RequestID != "req_a_2" || items[1].RequestID != "req_a_1" {
		t.Fatalf("username filter = total %d items %#v", total, items)
	}

	items, total = repo.ListBillingRequestsPage(BillingRequestQuery{
		ClientID: "key",
		Limit:    10,
	})
	if total != 2 || len(items) != 2 || items[0].RequestID != "req_a_2" || items[1].RequestID != "req_a_1" {
		t.Fatalf("client name filter = total %d items %#v", total, items)
	}
}

func TestMemoryRepositoryListBillingRequestsPageDoesNotMatchSubstringClientID(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_a", Username: "a", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_xxx", Name: "key", APIKey: "gw_xxx", OwnerUserID: "user_a", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	seedBillingRequest(t, repo, "req_xxx", "client_xxx", "user_a", "gpt-4.1", 1100)

	items, total := repo.ListBillingRequestsPage(BillingRequestQuery{ClientID: "xxx", Limit: 10})
	if total != 0 || len(items) != 0 {
		t.Fatalf("substring client id filter = total %d items %#v, want none", total, items)
	}

	items, total = repo.ListBillingRequestsPage(BillingRequestQuery{ClientID: "client_xxx", Limit: 10})
	if total != 1 || len(items) != 1 || items[0].RequestID != "req_xxx" {
		t.Fatalf("exact client id filter = total %d items %#v", total, items)
	}
}

func TestMemoryRepositoryTrimsUsageLogsAfterOneDay(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_logs", Username: "logs", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_logs", Name: "Logs", APIKey: "gw_logs", OwnerUserID: "user_logs", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	oldAt := time.Now().UTC().Add(-25 * time.Hour)
	seedBillingRequest(t, repo, "req_old", "client_logs", "user_logs", "gpt-4.1", 1100)
	oldKey := billingRequestKey("req_old", "client_logs")
	old := repo.billing[oldKey]
	old.CreatedAt = oldAt
	repo.billing[oldKey] = old
	seedZeroCostBillingRequest(t, repo, "req_old_zero", "client_logs", "user_logs", "gpt-4.1")
	oldZeroKey := billingRequestKey("req_old_zero", "client_logs")
	oldZero := repo.billing[oldZeroKey]
	oldZero.CreatedAt = oldAt
	repo.billing[oldZeroKey] = oldZero
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_old_reserved",
		ClientID:        "client_logs",
		UserID:          "user_logs",
		Model:           "gpt-4.1",
		ReservedNanoUSD: 100,
		Fingerprint:     "req_old_reserved",
	}); err != nil {
		t.Fatal(err)
	}
	oldReservedKey := billingRequestKey("req_old_reserved", "client_logs")
	oldReserved := repo.billing[oldReservedKey]
	oldReserved.CreatedAt = oldAt.Add(time.Second)
	repo.billing[oldReservedKey] = oldReserved

	seedBillingRequest(t, repo, "req_new", "client_logs", "user_logs", "gpt-4.1", 1200)

	items, total := repo.ListBillingRequestsPage(BillingRequestQuery{UserID: "user_logs", Limit: 10})
	if total != 2 || len(items) != 2 || items[0].RequestID != "req_new" || items[1].RequestID != "req_old_reserved" {
		t.Fatalf("usage logs = total %d items %#v, want newest request plus pending request", total, items)
	}
	if _, ok := repo.billing[oldKey]; ok {
		t.Fatal("expired financial usage log should be trimmed")
	}
	if _, ok := repo.billing[oldZeroKey]; ok {
		t.Fatal("expired zero-cost usage log should be trimmed")
	}
	if len(repo.ListBillingLedger("user_logs", 10)) == 0 {
		t.Fatal("billing ledger should not be trimmed with usage logs")
	}
	actual, err := repo.GetClientActualSpend("client_logs")
	if err != nil {
		t.Fatal(err)
	}
	if actual.SpendUsedNanoUSD != 2300 {
		t.Fatalf("actual spend = %d, want 2300", actual.SpendUsedNanoUSD)
	}
}

func TestMemoryRepositoryListBillingRequestsTrimsExpiredUsageLogs(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_logs", Username: "logs", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_logs", Name: "Logs", APIKey: "gw_logs", OwnerUserID: "user_logs", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	seedZeroCostBillingRequest(t, repo, "req_old", "client_logs", "user_logs", "gpt-4.1")
	oldKey := billingRequestKey("req_old", "client_logs")
	old := repo.billing[oldKey]
	old.CreatedAt = time.Now().UTC().Add(-25 * time.Hour)
	repo.billing[oldKey] = old

	items, total := repo.ListBillingRequestsPage(BillingRequestQuery{UserID: "user_logs", Limit: 10})
	if total != 0 || len(items) != 0 {
		t.Fatalf("expired usage logs = total %d items %#v, want none", total, items)
	}
	if _, ok := repo.billing[oldKey]; ok {
		t.Fatal("expired usage log should be removed when listing")
	}
}

func TestMemoryRepositoryUsageLogRetentionTrimsFundingAllocations(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_logs", Username: "logs", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_logs", Name: "Logs", APIKey: "gw_logs", OwnerUserID: "user_logs", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	createdAts := map[string]time.Time{
		"req_alloc_old": now.Add(-25 * time.Hour),
		"req_alloc_new": now.Add(-time.Hour),
	}
	for _, requestID := range []string{"req_alloc_old", "req_alloc_new"} {
		seedZeroCostBillingRequest(t, repo, requestID, "client_logs", "user_logs", "gpt-4.1")
		key := billingRequestKey(requestID, "client_logs")
		request := repo.billing[key]
		request.CreatedAt = createdAts[requestID]
		repo.billing[key] = request
		allocation := core.BillingFundingAllocation{
			ID:            billingAllocationID(requestID, "client_logs", core.BillingFundingSourceCash, ""),
			RequestID:     requestID,
			ClientID:      "client_logs",
			UserID:        "user_logs",
			Source:        core.BillingFundingSourceCash,
			CreatedAt:     request.CreatedAt,
			UpdatedAt:     request.CreatedAt,
			ActualNanoUSD: 1,
		}
		repo.allocations[billingAllocationKey(allocation.RequestID, allocation.ClientID, allocation.Source, allocation.EntitlementID)] = allocation
	}

	if err := repo.ConfigureUsageLogRetention(1); err != nil {
		t.Fatalf("ConfigureUsageLogRetention returned error: %v", err)
	}

	if _, ok := repo.allocations[billingAllocationKey("req_alloc_old", "client_logs", core.BillingFundingSourceCash, "")]; ok {
		t.Fatal("funding allocation for trimmed usage log should be removed")
	}
	if _, ok := repo.allocations[billingAllocationKey("req_alloc_new", "client_logs", core.BillingFundingSourceCash, "")]; !ok {
		t.Fatal("funding allocation for retained usage log should remain")
	}
}

func TestMemoryRepositoryBillingLedgerRetention(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	repo.ledger = []core.BillingLedgerEntry{
		{ID: "ledger_new", UserID: "user_ledger", Kind: "manual_credit", AmountNanoUSD: 1000, CreatedAt: now.Add(-2 * 24 * time.Hour)},
		{ID: "ledger_old", UserID: "user_ledger", Kind: "manual_credit", AmountNanoUSD: 1000, CreatedAt: now.Add(-4 * 24 * time.Hour)},
	}

	if err := repo.ConfigureBillingLedgerRetention(1); err != nil {
		t.Fatalf("ConfigureBillingLedgerRetention returned error: %v", err)
	}

	ledger := repo.ListBillingLedger("user_ledger", 10)
	if len(ledger) != 1 || ledger[0].ID != "ledger_new" {
		t.Fatalf("ledger = %#v, want only non-expired entry", ledger)
	}
}

func TestMemoryRepositoryDeleteUserPreservesBillingHistory(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_history", Username: "history", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_history", Name: "History", APIKey: "gw_history", OwnerUserID: "user_history", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	seedBillingRequest(t, repo, "req_history", "client_history", "user_history", "gpt-4.1", 1100)

	if err := repo.DeleteUser("user_history"); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	if _, err := repo.GetUser("user_history"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUser err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetClient("client_history"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetClient err = %v, want ErrNotFound", err)
	}
	if items, total := repo.ListBillingRequestsPage(BillingRequestQuery{UserID: "user_history", Limit: 10}); total != 1 || len(items) != 1 {
		t.Fatalf("billing requests after user delete = total %d items %#v, want preserved", total, items)
	}
	if ledger := repo.ListBillingLedger("user_history", 10); len(ledger) == 0 {
		t.Fatal("billing ledger should remain after user delete")
	}
}

func TestMemoryRepositoryDeleteUserRemovesOwnedClients(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_delete", Username: "delete", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_delete", Name: "Delete", APIKey: "gw_delete", OwnerUserID: "user_delete", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{ResponseID: "resp_delete", AccountID: "account_delete", ClientID: "client_delete", PromptCacheKey: "agpc_delete"}); err != nil {
		t.Fatal(err)
	}

	if err := repo.DeleteUser("user_delete"); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	if _, err := repo.GetClient("client_delete"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetClient err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetOpenAIResponseBinding("resp_delete"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetOpenAIResponseBinding err = %v, want ErrNotFound", err)
	}
}

func TestMemoryRepositoryOpenAIResponseBindingPromptCacheKeyRoundTrip(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{
		ResponseID:     "resp_cache",
		AccountID:      "account_cache",
		ClientID:       "client_cache",
		PromptCacheKey: "agpc_cache",
	}); err != nil {
		t.Fatal(err)
	}

	binding, err := repo.GetOpenAIResponseBinding("resp_cache")
	if err != nil {
		t.Fatalf("GetOpenAIResponseBinding returned error: %v", err)
	}
	if binding.PromptCacheKey != "agpc_cache" {
		t.Fatalf("PromptCacheKey = %q, want agpc_cache", binding.PromptCacheKey)
	}
}

func TestMemoryRepositoryDeleteClientRemovesResponseBindings(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertClient(core.APIClient{ID: "client_delete", Name: "Delete", APIKey: "gw_delete", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{ResponseID: "resp_delete", AccountID: "account_delete", ClientID: "client_delete", PromptCacheKey: "agpc_delete"}); err != nil {
		t.Fatal(err)
	}

	if err := repo.DeleteClient("client_delete"); err != nil {
		t.Fatalf("DeleteClient returned error: %v", err)
	}
	if _, err := repo.GetOpenAIResponseBinding("resp_delete"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetOpenAIResponseBinding err = %v, want ErrNotFound", err)
	}
}

func TestMemoryRepositoryDeleteUserPreservesHistoryAndRemovesOwnedData(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_force", Username: "force", Email: "force@example.com", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_force", Name: "Force", APIKey: "gw_force", OwnerUserID: "user_force", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{ResponseID: "resp_force", AccountID: "account_force", ClientID: "client_force", PromptCacheKey: "agpc_force"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateEmailVerificationCode(core.EmailVerificationCode{ID: "code_force", Purpose: "register", Email: "force@example.com", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	seedBillingRequest(t, repo, "req_force", "client_force", "user_force", "gpt-4.1", 1100)

	if err := repo.DeleteUser("user_force"); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	if _, err := repo.GetUser("user_force"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUser err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetClient("client_force"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetClient err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetOpenAIResponseBinding("resp_force"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetOpenAIResponseBinding err = %v, want ErrNotFound", err)
	}
	if items, total := repo.ListBillingRequestsPage(BillingRequestQuery{UserID: "user_force", Limit: 10}); total != 1 || len(items) != 1 {
		t.Fatalf("billing requests after user delete = total %d items %#v, want preserved", total, items)
	}
	if ledger := repo.ListBillingLedger("user_force", 10); len(ledger) == 0 {
		t.Fatalf("billing ledger after user delete = %#v, want preserved", ledger)
	}
	if _, err := repo.LatestEmailVerificationCode("register", "force@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestEmailVerificationCode err = %v, want ErrNotFound", err)
	}
}

func TestMemoryRepositoryDeleteUserAllowsUnpaidPaymentOrder(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_unpaid_order", Username: "unpaid_order", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreatePaymentOrder(core.PaymentOrder{
		ID:            "pay_unpaid",
		OutTradeNo:    "out_unpaid",
		UserID:        "user_unpaid_order",
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: core.NanoUSDPerUSD / 100,
		Status:        core.PaymentOrderPending,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateSiteMessage(core.SiteMessage{
		ID:            "msg_unpaid_user",
		Title:         "Notice",
		Body:          "Body",
		Enabled:       true,
		TargetUserIDs: []string{"user_unpaid_order"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkSiteMessageRead("msg_unpaid_user", "user_unpaid_order", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	if err := repo.DeleteUser("user_unpaid_order"); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	if _, err := repo.GetUser("user_unpaid_order"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUser err = %v, want ErrNotFound", err)
	}
	if order, err := repo.GetPaymentOrder("pay_unpaid"); err != nil || order.UserID != "user_unpaid_order" {
		t.Fatalf("GetPaymentOrder = %#v err=%v, want preserved static order", order, err)
	}
	messages := repo.ListSiteMessages()
	if len(messages) != 1 || len(messages[0].TargetUserIDs) != 0 {
		t.Fatalf("site message targets after delete = %#v", messages)
	}
	deliveries := repo.ListSiteMessageDeliveries("user_unpaid_order", false)
	if len(deliveries) != 1 || deliveries[0].Read {
		t.Fatalf("site message read state after delete = %#v", deliveries)
	}
}

func TestMemoryRepositoryDeleteUserPreservesPaidPaymentHistory(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_payment_history", Username: "payment_history", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreatePaymentOrder(core.PaymentOrder{
		ID:            "pay_history",
		OutTradeNo:    "out_history",
		UserID:        "user_payment_history",
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: core.NanoUSDPerUSD / 100,
		Status:        core.PaymentOrderPending,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.CompletePaymentOrder("out_history", "trade_history", core.NanoUSDPerUSD/100, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	if err := repo.DeleteUser("user_payment_history"); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	if _, err := repo.GetUser("user_payment_history"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUser err = %v, want ErrNotFound", err)
	}
	if order, err := repo.GetPaymentOrder("pay_history"); err != nil || order.Status != core.PaymentOrderPaid || order.UserID != "user_payment_history" {
		t.Fatalf("payment order after delete = %#v err=%v, want preserved paid order", order, err)
	}
}

func TestMemoryRepositoryDeleteUserPreservesOwnedClientBillingHistory(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_owner_history", Username: "owner_history", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_owner_history", Name: "Owner History", APIKey: "gw_owner_history", OwnerUserID: "user_owner_history", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	seedBillingRequest(t, repo, "req_owner_history", "client_owner_history", "user_owner_history", "gpt-4.1", 1100)
	if err := repo.SetUserBalance("user_owner_history", 0); err != nil {
		t.Fatal(err)
	}
	repo.ledger = nil
	for key, request := range repo.billing {
		request.UserID = "archived_user"
		repo.billing[key] = request
	}

	if err := repo.DeleteUser("user_owner_history"); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	if _, err := repo.GetUser("user_owner_history"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUser err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetClient("client_owner_history"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetClient err = %v, want ErrNotFound", err)
	}
	if items, total := repo.ListBillingRequestsPage(BillingRequestQuery{ClientID: "client_owner_history", Limit: 10}); total != 1 || len(items) != 1 {
		t.Fatalf("billing requests after user delete = total %d items %#v, want preserved", total, items)
	}
}

func TestMemoryRepositoryDeleteClientPreservesBillingHistory(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_history", Username: "history", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_history", Name: "History", APIKey: "gw_history", OwnerUserID: "user_history", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	seedBillingRequest(t, repo, "req_history", "client_history", "user_history", "gpt-4.1", 1100)

	if err := repo.DeleteClient("client_history"); err != nil {
		t.Fatalf("DeleteClient returned error: %v", err)
	}
	if _, err := repo.GetClient("client_history"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetClient err = %v, want ErrNotFound", err)
	}
	requests, total := repo.ListBillingRequestsPage(BillingRequestQuery{ClientID: "client_history", Limit: 10})
	if total != 1 || len(requests) != 1 || requests[0].RequestID != "req_history" || requests[0].ClientID != "client_history" {
		t.Fatalf("billing requests after client delete = total %d items %#v, want preserved req_history", total, requests)
	}
	if ledger := repo.ListBillingLedger("user_history", 10); len(ledger) == 0 {
		t.Fatalf("billing ledger should remain after client delete")
	}
}

func TestMemoryRepositoryDeleteClientReleasesReservedBilling(t *testing.T) {
	repo := NewMemoryRepository()
	const startingBalance int64 = 100000
	if err := repo.UpsertUser(core.User{ID: "user_reserved_client_delete", Username: "reserved", Enabled: true, BalanceNanoUSD: startingBalance}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_reserved_delete", Name: "Reserved Delete", APIKey: "gw_reserved_delete", OwnerUserID: "user_reserved_client_delete", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_reserved_delete",
		ClientID:        "client_reserved_delete",
		UserID:          "user_reserved_client_delete",
		Model:           "gpt-4.1",
		ReservedNanoUSD: 5000,
		Fingerprint:     "reserved-delete",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteClient("client_reserved_delete"); err != nil {
		t.Fatalf("DeleteClient returned error: %v", err)
	}
	user, err := repo.GetUser("user_reserved_client_delete")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != startingBalance {
		t.Fatalf("balance after client delete = %d, want %d", user.BalanceNanoUSD, startingBalance)
	}
	requests, total := repo.ListBillingRequestsPage(BillingRequestQuery{ClientID: "client_reserved_delete", Limit: 10})
	if total != 1 || len(requests) != 1 || requests[0].Status != core.BillingRequestReleased {
		t.Fatalf("billing requests after client delete = total %d items %#v, want released", total, requests)
	}
}

func TestMemoryRepositoryDeleteUserReleasesReservedBilling(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_reserved_delete", Username: "reserved", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_owned_reserved_delete", Name: "Owned Reserved Delete", APIKey: "gw_owned_reserved_delete", OwnerUserID: "user_reserved_delete", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_owned_reserved_delete",
		ClientID:        "client_owned_reserved_delete",
		UserID:          "user_reserved_delete",
		Model:           "gpt-4.1",
		ReservedNanoUSD: 5000,
		Fingerprint:     "owned-reserved-delete",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteUser("user_reserved_delete"); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	requests, total := repo.ListBillingRequestsPage(BillingRequestQuery{ClientID: "client_owned_reserved_delete", Limit: 10})
	if total != 1 || len(requests) != 1 || requests[0].Status != core.BillingRequestReleased {
		t.Fatalf("billing requests after user delete = total %d items %#v, want released", total, requests)
	}
	if _, err := repo.GetClient("client_owned_reserved_delete"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetClient err = %v, want ErrNotFound", err)
	}
}

func TestMemoryRepositoryDeleteClientPreservesZeroCostUsageLog(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_zero_cost", Username: "zero_cost", Enabled: true, BalanceNanoUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_zero_cost", Name: "Zero Cost", APIKey: "gw_zero_cost", OwnerUserID: "user_zero_cost", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:   "req_zero_cost",
		ClientID:    "client_zero_cost",
		UserID:      "user_zero_cost",
		Model:       "gpt-4.1",
		Fingerprint: "zero-cost",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_zero_cost",
		ClientID:      "client_zero_cost",
		Model:         "gpt-4.1",
		ActualNanoUSD: 0,
	}); err != nil {
		t.Fatal(err)
	}

	if err := repo.DeleteClient("client_zero_cost"); err != nil {
		t.Fatalf("DeleteClient returned error: %v", err)
	}
	if _, total := repo.ListBillingRequestsPage(BillingRequestQuery{ClientID: "client_zero_cost", Limit: 10}); total != 1 {
		t.Fatalf("zero-cost billing requests after delete = %d, want 1", total)
	}
}

func TestMemoryRepositoryClientActualSpendExcludesReservedRequests(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_billing", Username: "billing", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_billing", Name: "Billing", APIKey: "gw_billing", OwnerUserID: "user_billing", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_settled",
		ClientID:        "client_billing",
		UserID:          "user_billing",
		Model:           "gpt-4.1",
		ReservedNanoUSD: 5000,
		Fingerprint:     "settled",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_settled",
		ClientID:      "client_billing",
		Model:         "gpt-4.1",
		Usage:         core.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		ActualNanoUSD: 1200,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_pending",
		ClientID:        "client_billing",
		UserID:          "user_billing",
		Model:           "gpt-4.1",
		ReservedNanoUSD: 9000,
		Fingerprint:     "pending",
	}); err != nil {
		t.Fatal(err)
	}

	spend, err := repo.GetClientSpend("client_billing")
	if err != nil {
		t.Fatal(err)
	}
	if spend.SpendUsedNanoUSD != 1200 {
		t.Fatalf("limit spend used = %d, want 1200", spend.SpendUsedNanoUSD)
	}
	actual, err := repo.GetClientActualSpend("client_billing")
	if err != nil {
		t.Fatal(err)
	}
	if actual.SpendUsedNanoUSD != 1200 {
		t.Fatalf("actual spend used = %d, want 1200", actual.SpendUsedNanoUSD)
	}
}

func seedBillingRequest(t *testing.T, repo Repository, requestID, clientID, userID, model string, actualNanoUSD int64) {
	t.Helper()
	client, err := repo.GetClient(clientID)
	if err != nil {
		t.Fatalf("GetClient(%s): %v", clientID, err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       requestID,
		ClientID:        clientID,
		ClientName:      client.Name,
		UserID:          userID,
		Model:           model,
		ReservedNanoUSD: 100,
		Fingerprint:     requestID,
	}); err != nil {
		t.Fatalf("ReserveBilling(%s): %v", requestID, err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     requestID,
		ClientID:      clientID,
		Model:         model,
		Usage:         core.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		ActualNanoUSD: actualNanoUSD,
	}); err != nil {
		t.Fatalf("SettleBilling(%s): %v", requestID, err)
	}
}

func seedZeroCostBillingRequest(t *testing.T, repo Repository, requestID, clientID, userID, model string) {
	t.Helper()
	client, err := repo.GetClient(clientID)
	if err != nil {
		t.Fatalf("GetClient(%s): %v", clientID, err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:   requestID,
		ClientID:    clientID,
		ClientName:  client.Name,
		UserID:      userID,
		Model:       model,
		Fingerprint: requestID,
	}); err != nil {
		t.Fatalf("ReserveBilling(%s): %v", requestID, err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     requestID,
		ClientID:      clientID,
		Model:         model,
		ActualNanoUSD: 0,
	}); err != nil {
		t.Fatalf("SettleBilling(%s): %v", requestID, err)
	}
}
