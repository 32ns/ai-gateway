package controlplane

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestAccountPoolViewsClassifiesAccounts(t *testing.T) {
	repo := storage.NewMemoryRepository()
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	accounts := []core.Account{
		{ID: "normal", Provider: core.ProviderOpenAI, Status: core.AccountStatusActive},
		accountRuntimeWithQuota(t, core.Account{ID: "cooling", Provider: core.ProviderOpenAI, Status: core.AccountStatusActive}, core.AccountQuotaSnapshot{
			Primary: &core.AccountQuotaWindow{Name: "rate_limit", UsedPercent: 100, ResetsAt: &reset},
		}),
		{ID: "abnormal", Provider: core.ProviderOpenAI, Status: core.AccountStatusProviderBanned},
	}
	for _, account := range accounts {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	views := New(repo, nil).AccountPoolViews(now)
	if len(views.Normal) != 1 || views.Normal[0].ID != "normal" {
		t.Fatalf("normal views = %#v, want normal account", views.Normal)
	}
	if len(views.Cooling) != 1 || views.Cooling[0].ID != "cooling" {
		t.Fatalf("cooling views = %#v, want cooling account", views.Cooling)
	}
	if len(views.Abnormal) != 1 || views.Abnormal[0].ID != "abnormal" {
		t.Fatalf("abnormal views = %#v, want abnormal account", views.Abnormal)
	}
}

func TestAccountPoolViewsClassifiesImageQuotaCooling(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(accountRuntimeWithQuota(t, core.Account{
		ID:       "image_cooling",
		Provider: core.ProviderOpenAI,
		Status:   core.AccountStatusActive,
	}, core.AccountQuotaSnapshot{
		Source: "openai_chatgpt_usage",
		Image:  &core.AccountImageQuota{Remaining: 0},
	})); err != nil {
		t.Fatal(err)
	}

	views := New(repo, nil).AccountPoolViews(time.Now().UTC())
	if len(views.Cooling) != 1 || views.Cooling[0].ID != "image_cooling" {
		t.Fatalf("cooling views = %#v, want image quota cooling account", views.Cooling)
	}
	if got := AccountFilterStatus(views.Cooling[0]); got != "cooling" {
		t.Fatalf("filter status = %q, want cooling", got)
	}
	if got := AccountRuntimeStatus(views.Cooling[0]); got != string(core.AccountStatusCooling) {
		t.Fatalf("runtime status = %q, want cooling", got)
	}
}

func TestReconcileAccountRuntimeStateReactivatesExpiredCooldown(t *testing.T) {
	repo := storage.NewMemoryRepository()
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	expired := now.Add(-time.Minute)
	if err := repo.UpsertAccount(core.Account{
		ID:               "acct_cooldown",
		Provider:         core.ProviderOpenAI,
		Status:           core.AccountStatusCooling,
		CooldownUntil:    &expired,
		ConsecutiveFails: 2,
	}); err != nil {
		t.Fatal(err)
	}

	report, err := New(repo, nil).ReconcileAccountRuntimeState(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Scanned != 1 || report.Updated != 1 || report.Reactivated != 1 {
		t.Fatalf("report = %#v, want scanned/updated/reactivated = 1", report)
	}
	account, err := repo.GetAccount("acct_cooldown")
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != core.AccountStatusActive || account.CooldownUntil != nil {
		t.Fatalf("account status=%q cooldown=%#v, want active without cooldown", account.Status, account.CooldownUntil)
	}
	if account.ConsecutiveFails != 0 {
		t.Fatalf("consecutive fails = %d, want reset value 0", account.ConsecutiveFails)
	}
}

func TestReconcileAccountRuntimeStateClearsExpiredScopedQuotaCooldown(t *testing.T) {
	repo := storage.NewMemoryRepository()
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(-time.Minute)
	if err := repo.UpsertAccount(accountRuntimeWithQuota(t, core.Account{
		ID:       "acct_quota",
		Provider: core.ProviderOpenAI,
		Status:   core.AccountStatusActive,
	}, core.AccountQuotaSnapshot{
		Primary:     &core.AccountQuotaWindow{Name: "rate_limit", UsedPercent: 100, ResetsAt: &reset},
		ReachedType: "primary_window",
	})); err != nil {
		t.Fatal(err)
	}

	report, err := New(repo, nil).ReconcileAccountRuntimeState(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Updated != 1 || report.QuotaCooldownCleared != 1 {
		t.Fatalf("report = %#v, want updated and quota cleared", report)
	}
	account, err := repo.GetAccount("acct_quota")
	if err != nil {
		t.Fatal(err)
	}
	if quota := core.ReadAccountQuota(account); quota != nil {
		t.Fatalf("quota = %#v, want cleared", quota)
	}
}

func TestReconcileAccountRuntimeStateDoesNotCountQuotaLimitedAccountAsReactivated(t *testing.T) {
	repo := storage.NewMemoryRepository()
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	if err := repo.UpsertAccount(accountRuntimeWithQuota(t, core.Account{
		ID:               "acct_quota_active",
		Provider:         core.ProviderOpenAI,
		Status:           core.AccountStatusCooling,
		CooldownUntil:    accountRuntimePtrTime(now.Add(-time.Minute)),
		ConsecutiveFails: 1,
	}, core.AccountQuotaSnapshot{
		Primary: &core.AccountQuotaWindow{UsedPercent: 100, ResetsAt: &reset},
	})); err != nil {
		t.Fatal(err)
	}

	report, err := New(repo, nil).ReconcileAccountRuntimeState(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Reactivated != 0 {
		t.Fatalf("reactivated = %d, want 0", report.Reactivated)
	}
}

func accountRuntimeWithQuota(t *testing.T, account core.Account, snapshot core.AccountQuotaSnapshot) core.Account {
	t.Helper()
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if account.Credential.Metadata == nil {
		account.Credential.Metadata = map[string]string{}
	}
	account.Credential.Metadata[core.AccountQuotaMetadataKey] = string(raw)
	return account
}

func accountRuntimePtrTime(t time.Time) *time.Time {
	return &t
}
