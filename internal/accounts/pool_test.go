package accounts

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestCandidatesSkipExpiredAccounts(t *testing.T) {
	repo := storage.NewMemoryRepository()
	_ = repo.UpsertAccount(core.Account{
		ID:       "expired",
		Provider: core.ProviderOpenAI,
		Label:    "Expired",
		Status:   core.AccountStatusExpired,
		Priority: 100,
		Weight:   100,
	})
	_ = repo.UpsertAccount(core.Account{
		ID:       "active",
		Provider: core.ProviderOpenAI,
		Label:    "Active",
		Status:   core.AccountStatusActive,
		Priority: 90,
		Weight:   90,
	})

	pool := NewPool(repo)
	candidates := pool.Candidates(core.ProviderOpenAI, nil)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].ID != "active" {
		t.Fatalf("expected active candidate, got %s", candidates[0].ID)
	}
}

func TestCandidatesSkipControlDisabledAccounts(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:              "disabled",
		Provider:        core.ProviderOpenAI,
		Label:           "Disabled",
		Status:          core.AccountStatusActive,
		ControlDisabled: true,
		Priority:        100,
		Weight:          100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "active",
		Provider: core.ProviderOpenAI,
		Label:    "Active",
		Status:   core.AccountStatusActive,
		Priority: 90,
		Weight:   90,
	}); err != nil {
		t.Fatal(err)
	}

	pool := NewPool(repo)
	candidates := pool.Candidates(core.ProviderOpenAI, nil)
	if len(candidates) != 1 || candidates[0].ID != "active" {
		t.Fatalf("candidates = %#v, want only active account", candidates)
	}
}

func TestCandidatesNormalizeExpiredCooldownAccounts(t *testing.T) {
	repo := storage.NewMemoryRepository()
	expiredCooldown := time.Now().UTC().Add(-time.Minute)
	if err := repo.UpsertAccount(core.Account{
		ID:               "acct_cooldown_expired",
		Provider:         core.ProviderOpenAI,
		Label:            "Expired Cooldown",
		Status:           core.AccountStatusCooling,
		CooldownUntil:    &expiredCooldown,
		ConsecutiveFails: 1,
		Priority:         100,
		Weight:           100,
	}); err != nil {
		t.Fatal(err)
	}

	pool := NewPool(repo)
	candidates := pool.Candidates(core.ProviderOpenAI, nil)
	if len(candidates) != 1 {
		t.Fatalf("candidate count = %d, want 1", len(candidates))
	}
	if candidates[0].Status != core.AccountStatusActive {
		t.Fatalf("candidate status = %q, want %q", candidates[0].Status, core.AccountStatusActive)
	}
	if candidates[0].CooldownUntil != nil {
		t.Fatalf("candidate cooldown = %#v, want nil", candidates[0].CooldownUntil)
	}
	if candidates[0].ConsecutiveFails != 0 {
		t.Fatalf("candidate consecutive fails = %d, want 0", candidates[0].ConsecutiveFails)
	}
}

func TestCandidatesSkipFutureCooldownAccounts(t *testing.T) {
	repo := storage.NewMemoryRepository()
	futureCooldown := time.Now().UTC().Add(time.Hour)
	if err := repo.UpsertAccount(core.Account{
		ID:            "acct_cooldown_future",
		Provider:      core.ProviderOpenAI,
		Label:         "Future Cooldown",
		Status:        core.AccountStatusActive,
		CooldownUntil: &futureCooldown,
		Priority:      100,
		Weight:        100,
	}); err != nil {
		t.Fatal(err)
	}

	pool := NewPool(repo)
	if candidates := pool.Candidates(core.ProviderOpenAI, nil); len(candidates) != 0 {
		t.Fatalf("candidates = %#v, want none while cooldown is active", candidates)
	}
}

func TestCandidatesUseBackupOnlyWhenPrimaryUnavailable(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 10,
		Weight:   10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "backup",
		Provider: core.ProviderOpenAI,
		Label:    "Backup",
		Status:   core.AccountStatusActive,
		Backup:   true,
		Priority: 1000,
		Weight:   1000,
	}); err != nil {
		t.Fatal(err)
	}

	pool := NewPool(repo)
	candidates := pool.Candidates(core.ProviderOpenAI, nil)
	if len(candidates) != 2 || candidates[0].ID != "primary" || candidates[1].ID != "backup" {
		t.Fatalf("candidates = %#v, want primary before backup", candidates)
	}

	until := time.Now().UTC().Add(time.Hour)
	primary, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	primary.Status = core.AccountStatusCooling
	primary.CooldownUntil = &until
	if err := repo.UpsertAccount(primary); err != nil {
		t.Fatal(err)
	}
	candidates = pool.Candidates(core.ProviderOpenAI, nil)
	if len(candidates) != 1 || candidates[0].ID != "backup" {
		t.Fatalf("candidates = %#v, want backup while primary is unavailable", candidates)
	}

	primary.Status = core.AccountStatusActive
	primary.CooldownUntil = nil
	if err := repo.UpsertAccount(primary); err != nil {
		t.Fatal(err)
	}
	candidates = pool.Candidates(core.ProviderOpenAI, nil)
	if len(candidates) != 2 || candidates[0].ID != "primary" || candidates[1].ID != "backup" {
		t.Fatalf("candidates = %#v, want primary before backup after recovery", candidates)
	}
}

func TestCandidatesUseBackupWithinClientAccountGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	until := time.Now().UTC().Add(time.Hour)
	for _, account := range []core.Account{
		{
			ID:            "plus_primary",
			Provider:      core.ProviderOpenAI,
			Label:         "Plus Primary",
			Group:         "Plus",
			Status:        core.AccountStatusCooling,
			CooldownUntil: &until,
			Priority:      100,
			Weight:        100,
		},
		{
			ID:       "plus_backup",
			Provider: core.ProviderOpenAI,
			Label:    "Plus Backup",
			Group:    "Plus",
			Status:   core.AccountStatusActive,
			Backup:   true,
			Priority: 1000,
			Weight:   1000,
		},
		{
			ID:       "default_primary",
			Provider: core.ProviderOpenAI,
			Label:    "Default Primary",
			Group:    core.DefaultAccountGroupName,
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	candidates := NewPool(repo).Candidates(core.ProviderOpenAI, &core.APIClient{ID: "client_plus", AccountGroup: "Plus"})
	if len(candidates) != 1 || candidates[0].ID != "plus_backup" {
		t.Fatalf("candidates = %#v, want Plus backup only", candidates)
	}
}

func TestCandidatesSkipQuotaLimitedAccountsUntilReset(t *testing.T) {
	repo := storage.NewMemoryRepository()
	now := time.Now().UTC()
	resetFuture := now.Add(time.Hour)
	resetPast := now.Add(-time.Hour)
	if err := repo.UpsertAccount(accountWithQuotaSnapshot(t, core.Account{
		ID:       "acct_limited",
		Provider: core.ProviderOpenAI,
		Label:    "Limited",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
	}, core.AccountQuotaSnapshot{Primary: &core.AccountQuotaWindow{UsedPercent: 100, ResetsAt: &resetFuture}})); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(accountWithQuotaSnapshot(t, core.Account{
		ID:       "acct_reset",
		Provider: core.ProviderOpenAI,
		Label:    "Reset",
		Status:   core.AccountStatusActive,
		Priority: 90,
		Weight:   100,
	}, core.AccountQuotaSnapshot{Secondary: &core.AccountQuotaWindow{UsedPercent: 100, ResetsAt: &resetPast}})); err != nil {
		t.Fatal(err)
	}

	pool := NewPool(repo)
	candidates := pool.Candidates(core.ProviderOpenAI, nil)
	if len(candidates) != 1 || candidates[0].ID != "acct_reset" {
		t.Fatalf("candidates = %#v, want only reset account", candidates)
	}
}

func TestImageCandidatesIgnoreChatQuotaLimit(t *testing.T) {
	repo := storage.NewMemoryRepository()
	resetFuture := time.Now().UTC().Add(time.Hour)
	if err := repo.UpsertAccount(accountWithQuotaSnapshot(t, core.Account{
		ID:       "acct_chat_limited_image_ready",
		Provider: core.ProviderOpenAI,
		Label:    "Chat Limited Image Ready",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
	}, core.AccountQuotaSnapshot{
		Primary: &core.AccountQuotaWindow{UsedPercent: 100, ResetsAt: &resetFuture},
		Image:   &core.AccountImageQuota{Remaining: 2},
	})); err != nil {
		t.Fatal(err)
	}

	pool := NewPool(repo)
	if candidates := pool.Candidates(core.ProviderOpenAI, nil); len(candidates) != 0 {
		t.Fatalf("chat candidates = %#v, want none while chat quota is limited", candidates)
	}
	imageCandidates := pool.ImageCandidates(core.ProviderOpenAI, nil)
	if len(imageCandidates) != 1 || imageCandidates[0].ID != "acct_chat_limited_image_ready" {
		t.Fatalf("image candidates = %#v, want chat-limited image-ready account", imageCandidates)
	}
}

func TestScopedRateLimitDoesNotOverwriteProviderQuotaSnapshot(t *testing.T) {
	repo := storage.NewMemoryRepository()
	resetFuture := time.Now().UTC().Add(time.Hour)
	refreshedAt := time.Now().UTC().Add(-time.Hour)
	account := accountWithQuotaSnapshot(t, core.Account{
		ID:       "acct_provider_quota",
		Provider: core.ProviderOpenAI,
		Label:    "Provider Quota",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
	}, core.AccountQuotaSnapshot{
		Source:      "openai_chatgpt_usage",
		Plan:        "plus",
		Primary:     &core.AccountQuotaWindow{Name: "primary", UsedPercent: 42, ResetsAt: &resetFuture},
		Secondary:   &core.AccountQuotaWindow{Name: "secondary", UsedPercent: 20, ResetsAt: &resetFuture},
		Image:       &core.AccountImageQuota{Remaining: 3},
		RefreshedAt: &refreshedAt,
	})
	if err := repo.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}

	pool := NewPool(repo)
	if err := pool.MarkChatQuotaLimited(account, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := pool.MarkImageQuotaLimited(account, time.Minute); err != nil {
		t.Fatal(err)
	}

	stored, err := repo.GetAccount("acct_provider_quota")
	if err != nil {
		t.Fatal(err)
	}
	quota := core.ReadAccountQuota(stored)
	if quota == nil {
		t.Fatal("quota snapshot is missing")
	}
	if quota.Source != "openai_chatgpt_usage" || quota.Plan != "plus" {
		t.Fatalf("provider quota metadata = %#v, want preserved", quota)
	}
	if quota.Primary == nil || quota.Primary.Name != "primary" || quota.Primary.UsedPercent != 42 {
		t.Fatalf("primary quota = %#v, want provider quota preserved", quota.Primary)
	}
	if quota.Secondary == nil || quota.Secondary.Name != "secondary" || quota.Secondary.UsedPercent != 20 {
		t.Fatalf("secondary quota = %#v, want provider quota preserved", quota.Secondary)
	}
	if quota.Image == nil || quota.Image.Remaining != 3 {
		t.Fatalf("image quota = %#v, want provider quota preserved", quota.Image)
	}
	if quota.RefreshedAt == nil || !quota.RefreshedAt.Equal(refreshedAt) {
		t.Fatalf("refreshed at = %#v, want provider timestamp %s", quota.RefreshedAt, refreshedAt)
	}
	if quota.Additional == nil ||
		quota.Additional[core.AccountQuotaRuntimeChatLimitID].Primary == nil ||
		quota.Additional[core.AccountQuotaRuntimeImageLimitID].Image == nil {
		t.Fatalf("runtime limits = %#v, want chat and image entries", quota.Additional)
	}
}

func TestImageCandidatesSkipActiveImageQuotaLimit(t *testing.T) {
	repo := storage.NewMemoryRepository()
	resetFuture := time.Now().UTC().Add(time.Hour)
	if err := repo.UpsertAccount(accountWithQuotaSnapshot(t, core.Account{
		ID:       "acct_image_exhausted",
		Provider: core.ProviderOpenAI,
		Label:    "Image Exhausted",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
	}, core.AccountQuotaSnapshot{
		Image: &core.AccountImageQuota{Remaining: 0, ResetsAt: &resetFuture},
	})); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(accountWithQuotaSnapshot(t, core.Account{
		ID:       "acct_image_ready",
		Provider: core.ProviderOpenAI,
		Label:    "Image Ready",
		Status:   core.AccountStatusActive,
		Priority: 90,
		Weight:   100,
	}, core.AccountQuotaSnapshot{
		Image: &core.AccountImageQuota{Remaining: 1},
	})); err != nil {
		t.Fatal(err)
	}

	pool := NewPool(repo)
	candidates := pool.ImageCandidates(core.ProviderOpenAI, nil)
	if len(candidates) != 1 || candidates[0].ID != "acct_image_ready" {
		t.Fatalf("image candidates = %#v, want only image-ready account", candidates)
	}
}

func TestImageCandidatesRankKnownRemainingBeforeUnknown(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(accountWithQuotaSnapshot(t, core.Account{
		ID:       "acct_unknown",
		Provider: core.ProviderOpenAI,
		Label:    "Unknown",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
	}, core.AccountQuotaSnapshot{
		Image: &core.AccountImageQuota{Unknown: true},
	})); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(accountWithQuotaSnapshot(t, core.Account{
		ID:       "acct_remaining_2",
		Provider: core.ProviderOpenAI,
		Label:    "Remaining 2",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
	}, core.AccountQuotaSnapshot{
		Image: &core.AccountImageQuota{Remaining: 2},
	})); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(accountWithQuotaSnapshot(t, core.Account{
		ID:       "acct_remaining_5",
		Provider: core.ProviderOpenAI,
		Label:    "Remaining 5",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
	}, core.AccountQuotaSnapshot{
		Image: &core.AccountImageQuota{Remaining: 5},
	})); err != nil {
		t.Fatal(err)
	}

	pool := NewPool(repo)
	candidates := pool.ImageCandidates(core.ProviderOpenAI, nil)
	if len(candidates) < 3 {
		t.Fatalf("candidate count = %d, want at least 3", len(candidates))
	}
	got := []string{candidates[0].ID, candidates[1].ID, candidates[2].ID}
	want := []string{"acct_remaining_5", "acct_remaining_2", "acct_unknown"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ranked image candidates = %v, want %v", got, want)
		}
	}
}

func TestImageCandidatesPreferMostRemainingBeforePriority(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(accountWithQuotaSnapshot(t, core.Account{
		ID:       "acct_priority_high",
		Provider: core.ProviderOpenAI,
		Label:    "Priority High",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
	}, core.AccountQuotaSnapshot{
		Image: &core.AccountImageQuota{Remaining: 1},
	})); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(accountWithQuotaSnapshot(t, core.Account{
		ID:       "acct_quota_high",
		Provider: core.ProviderOpenAI,
		Label:    "Quota High",
		Status:   core.AccountStatusActive,
		Priority: 10,
		Weight:   100,
	}, core.AccountQuotaSnapshot{
		Image: &core.AccountImageQuota{Remaining: 9},
	})); err != nil {
		t.Fatal(err)
	}

	candidates := NewPool(repo).ImageCandidates(core.ProviderOpenAI, nil)
	if len(candidates) < 2 {
		t.Fatalf("candidate count = %d, want at least 2", len(candidates))
	}
	if candidates[0].ID != "acct_quota_high" {
		t.Fatalf("first image candidate = %q, want quota-rich account", candidates[0].ID)
	}
}

func TestCandidatesNormalizeStaleRefreshingAccounts(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_refreshing",
		Provider: core.ProviderOpenAI,
		Label:    "Refreshing",
		Status:   core.AccountStatusRefreshing,
		Priority: 100,
		Weight:   100,
	}); err != nil {
		t.Fatal(err)
	}

	pool := NewPool(repo)
	candidates := pool.Candidates(core.ProviderOpenAI, nil)
	if len(candidates) != 1 {
		t.Fatalf("candidate count = %d, want 1", len(candidates))
	}
	if candidates[0].Status != core.AccountStatusActive {
		t.Fatalf("candidate status = %q, want active", candidates[0].Status)
	}
}

func TestCandidatesRespectClientAccountGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	_ = repo.UpsertAccount(core.Account{
		ID:       "acct_a",
		Provider: core.ProviderOpenAI,
		Label:    "Account A",
		Group:    "Basic",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
	})
	_ = repo.UpsertAccount(core.Account{
		ID:       "acct_b",
		Provider: core.ProviderOpenAI,
		Label:    "Account B",
		Group:    "Plus",
		Status:   core.AccountStatusActive,
		Priority: 90,
		Weight:   90,
	})

	pool := NewPool(repo)
	client := &core.APIClient{
		ID:           "client_a",
		AccountGroup: "Plus",
	}
	candidates := pool.Candidates(core.ProviderOpenAI, client)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].ID != "acct_b" {
		t.Fatalf("expected acct_b candidate, got %s", candidates[0].ID)
	}
}

func accountWithQuotaSnapshot(t *testing.T, account core.Account, snapshot core.AccountQuotaSnapshot) core.Account {
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

func TestCandidatesRespectClientAccountGroupDynamically(t *testing.T) {
	repo := storage.NewMemoryRepository()
	_ = repo.UpsertAccount(core.Account{
		ID:       "acct_basic",
		Provider: core.ProviderOpenAI,
		Label:    "Basic",
		Group:    "Basic",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
	})
	_ = repo.UpsertAccount(core.Account{
		ID:       "acct_plus",
		Provider: core.ProviderOpenAI,
		Label:    "Plus",
		Group:    "Plus",
		Status:   core.AccountStatusActive,
		Priority: 90,
		Weight:   90,
	})

	pool := NewPool(repo)
	client := &core.APIClient{
		ID:           "client_plus",
		AccountGroup: "Plus",
	}
	candidates := pool.Candidates(core.ProviderOpenAI, client)
	if len(candidates) != 1 || candidates[0].ID != "acct_plus" {
		t.Fatalf("candidates = %#v, want acct_plus", candidates)
	}

	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_plus_new",
		Provider: core.ProviderOpenAI,
		Label:    "Plus New",
		Group:    "Plus",
		Status:   core.AccountStatusActive,
		Priority: 80,
		Weight:   80,
	}); err != nil {
		t.Fatal(err)
	}
	candidates = pool.Candidates(core.ProviderOpenAI, client)
	if len(candidates) != 2 {
		t.Fatalf("candidate count after adding group account = %d, want 2", len(candidates))
	}
}

func TestDefaultClientCandidatesUseOnlyDefaultGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	hide := false
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_hidden", Name: "Hidden", ShowInClientEditor: &hide}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{ID: "acct_hidden", Provider: core.ProviderOpenAI, Label: "Hidden", Group: "Hidden", Status: core.AccountStatusActive, Priority: 300, Weight: 100},
		{ID: "acct_plus", Provider: core.ProviderOpenAI, Label: "Plus", Group: "Plus", Status: core.AccountStatusActive, Priority: 200, Weight: 100},
		{ID: "acct_default", Provider: core.ProviderOpenAI, Label: "Default", Status: core.AccountStatusActive, Priority: 100, Weight: 100},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	pool := NewPool(repo)
	candidates := pool.Candidates(core.ProviderOpenAI, &core.APIClient{ID: "client_default"})
	if len(candidates) != 1 {
		t.Fatalf("candidates = %#v, want Default group account only", candidates)
	}
	got := map[string]bool{}
	for _, candidate := range candidates {
		got[candidate.ID] = true
	}
	if !got["acct_default"] || got["acct_plus"] || got["acct_hidden"] {
		t.Fatalf("candidate ids = %#v, want Default account only", got)
	}
}

func TestCandidatesApplyGroupProxyAndAccountOverride(t *testing.T) {
	repo := storage.NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.Network.SystemProxyURL = "http://127.0.0.1:18080"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	_ = repo.UpsertAccountGroup(core.AccountGroup{
		ID:       "group_core",
		Name:     "Core",
		ProxyURL: "http://127.0.0.1:7890",
	})
	_ = repo.UpsertAccount(core.Account{
		ID:       "acct_group",
		Provider: core.ProviderOpenAI,
		Label:    "Group Proxy",
		Group:    "Core",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
	})
	_ = repo.UpsertAccount(core.Account{
		ID:       "acct_system",
		Provider: core.ProviderOpenAI,
		Label:    "System Proxy",
		Status:   core.AccountStatusActive,
		Priority: 95,
		Weight:   95,
	})
	_ = repo.UpsertAccount(core.Account{
		ID:       "acct_account",
		Provider: core.ProviderOpenAI,
		Label:    "Account Proxy",
		Group:    "Core",
		ProxyURL: "socks5://127.0.0.1:1080",
		Status:   core.AccountStatusActive,
		Priority: 90,
		Weight:   90,
	})

	pool := NewPool(repo)
	candidates := pool.Candidates(core.ProviderOpenAI, nil)
	if len(candidates) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(candidates))
	}
	byID := map[string]core.Account{}
	for _, candidate := range candidates {
		byID[candidate.ID] = candidate
	}
	if got := byID["acct_group"].EffectiveProxyURL; got != "http://127.0.0.1:7890" {
		t.Fatalf("group proxy = %q", got)
	}
	if got := byID["acct_system"].EffectiveProxyURL; got != "http://127.0.0.1:18080" {
		t.Fatalf("system proxy = %q", got)
	}
	if got := byID["acct_account"].EffectiveProxyURL; got != "socks5://127.0.0.1:1080" {
		t.Fatalf("account proxy = %q", got)
	}
	stored, err := pool.GetAccount("acct_group")
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.EffectiveProxyURL; got != "http://127.0.0.1:7890" {
		t.Fatalf("GetAccount effective proxy = %q", got)
	}
}

func TestCandidatesPreferStableAccountForClient(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for index := range 4 {
		_ = repo.UpsertAccount(core.Account{
			ID:       fmt.Sprintf("acct_%d", index),
			Provider: core.ProviderOpenAI,
			Label:    fmt.Sprintf("Account %d", index),
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
		})
	}

	pool := NewPool(repo)
	client := &core.APIClient{ID: "client_cache_sensitive"}
	first := pool.Candidates(core.ProviderOpenAI, client)
	second := pool.Candidates(core.ProviderOpenAI, client)
	if len(first) != 4 || len(second) != 4 {
		t.Fatalf("candidate counts = %d/%d, want 4/4", len(first), len(second))
	}
	if first[0].ID != second[0].ID {
		t.Fatalf("sticky candidate changed from %s to %s", first[0].ID, second[0].ID)
	}
}

func TestCandidatesStickyAccountFallsBackWhenPreferredAccountUnavailable(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for index := range 4 {
		_ = repo.UpsertAccount(core.Account{
			ID:       fmt.Sprintf("acct_%d", index),
			Provider: core.ProviderOpenAI,
			Label:    fmt.Sprintf("Account %d", index),
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
		})
	}

	pool := NewPool(repo)
	client := &core.APIClient{ID: "client_cache_sensitive"}
	candidates := pool.Candidates(core.ProviderOpenAI, client)
	if len(candidates) != 4 {
		t.Fatalf("candidate count = %d, want 4", len(candidates))
	}
	preferred := candidates[0]
	preferred.Status = core.AccountStatusBlocked
	if err := repo.UpsertAccount(preferred); err != nil {
		t.Fatal(err)
	}
	providerBanned := candidates[1]
	providerBanned.Status = core.AccountStatusProviderBanned
	if err := repo.UpsertAccount(providerBanned); err != nil {
		t.Fatal(err)
	}

	fallback := pool.Candidates(core.ProviderOpenAI, client)
	if len(fallback) != 2 {
		t.Fatalf("fallback count = %d, want 2", len(fallback))
	}
	if fallback[0].ID == preferred.ID {
		t.Fatalf("blocked preferred account %s was still selected", preferred.ID)
	}
	for _, account := range fallback {
		if account.ID == providerBanned.ID {
			t.Fatalf("provider-banned account %s was still selected", providerBanned.ID)
		}
	}
}

func TestCandidatesDeprioritizeStickyAccountAfterFailures(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for index := range 4 {
		_ = repo.UpsertAccount(core.Account{
			ID:       fmt.Sprintf("acct_%d", index),
			Provider: core.ProviderOpenAI,
			Label:    fmt.Sprintf("Account %d", index),
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
		})
	}

	pool := NewPool(repo)
	client := &core.APIClient{ID: "client_cache_sensitive"}
	candidates := pool.Candidates(core.ProviderOpenAI, client)
	if len(candidates) != 4 {
		t.Fatalf("candidate count = %d, want 4", len(candidates))
	}
	preferred := candidates[0]
	preferred.ConsecutiveFails = 1
	if err := repo.UpsertAccount(preferred); err != nil {
		t.Fatal(err)
	}

	afterFailure := pool.Candidates(core.ProviderOpenAI, client)
	if afterFailure[0].ID == preferred.ID {
		t.Fatalf("failed sticky account %s should be deprioritized", preferred.ID)
	}
}

func TestCandidatesPriorityDominatesClientStickiness(t *testing.T) {
	repo := storage.NewMemoryRepository()
	_ = repo.UpsertAccount(core.Account{
		ID:       "acct_high",
		Provider: core.ProviderOpenAI,
		Label:    "High Priority",
		Status:   core.AccountStatusActive,
		Priority: 200,
		Weight:   1,
	})
	for index := range 4 {
		_ = repo.UpsertAccount(core.Account{
			ID:       fmt.Sprintf("acct_normal_%d", index),
			Provider: core.ProviderOpenAI,
			Label:    fmt.Sprintf("Normal %d", index),
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
		})
	}

	pool := NewPool(repo)
	client := &core.APIClient{ID: "client_cache_sensitive"}
	candidates := pool.Candidates(core.ProviderOpenAI, client)
	if len(candidates) != 5 {
		t.Fatalf("candidate count = %d, want 5", len(candidates))
	}
	if candidates[0].ID != "acct_high" {
		t.Fatalf("first candidate = %s, want acct_high", candidates[0].ID)
	}
}

func TestCandidatesUseRouteAffinityKeyBeforeClientID(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for index := range 16 {
		_ = repo.UpsertAccount(core.Account{
			ID:       fmt.Sprintf("acct_%02d", index),
			Provider: core.ProviderOpenAI,
			Label:    fmt.Sprintf("Account %d", index),
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
		})
	}

	pool := NewPool(repo)
	clientA := &core.APIClient{ID: "client_cache_sensitive", RouteAffinityKey: "client_cache_sensitive\x00prefix-a"}
	clientB := &core.APIClient{ID: "client_cache_sensitive", RouteAffinityKey: "client_cache_sensitive\x00prefix-b"}
	candidatesA := pool.Candidates(core.ProviderOpenAI, clientA)
	candidatesB := pool.Candidates(core.ProviderOpenAI, clientB)
	if len(candidatesA) != 16 || len(candidatesB) != 16 {
		t.Fatalf("candidate counts = %d/%d, want 16/16", len(candidatesA), len(candidatesB))
	}
	if candidatesA[0].ID == candidatesB[0].ID {
		t.Fatalf("different route affinity keys both selected %s", candidatesA[0].ID)
	}
}

func TestCandidatesCapsProviderCandidateSet(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for index := range maxProviderCandidates + 8 {
		_ = repo.UpsertAccount(core.Account{
			ID:       fmt.Sprintf("acct_%02d", index),
			Provider: core.ProviderOpenAI,
			Label:    fmt.Sprintf("Account %d", index),
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
		})
	}

	pool := NewPool(repo)
	candidates := pool.Candidates(core.ProviderOpenAI, &core.APIClient{ID: "client_cache_sensitive"})
	if len(candidates) != maxProviderCandidates {
		t.Fatalf("candidate count = %d, want cap %d", len(candidates), maxProviderCandidates)
	}
}

func TestCandidatesDoNotAppendBackupWhenPrimaryCandidateSetIsCapped(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for index := range maxProviderCandidates + 1 {
		_ = repo.UpsertAccount(core.Account{
			ID:       fmt.Sprintf("primary_%02d", index),
			Provider: core.ProviderOpenAI,
			Label:    fmt.Sprintf("Primary %d", index),
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
		})
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "backup",
		Provider: core.ProviderOpenAI,
		Label:    "Backup",
		Status:   core.AccountStatusActive,
		Backup:   true,
		Priority: 1000,
		Weight:   1000,
	}); err != nil {
		t.Fatal(err)
	}

	candidates := NewPool(repo).Candidates(core.ProviderOpenAI, &core.APIClient{ID: "client_cache_sensitive"})
	if len(candidates) != maxProviderCandidates {
		t.Fatalf("candidate count = %d, want cap %d", len(candidates), maxProviderCandidates)
	}
	for _, candidate := range candidates {
		if candidate.Backup {
			t.Fatalf("candidates = %#v, backup must not be included while uncapped primary accounts remain", candidates)
		}
	}
}

func TestMonitorCandidatesIncludeBackupsWhenPrimaryCandidateSetWouldBeCapped(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for index := range maxProviderCandidates + 1 {
		_ = repo.UpsertAccount(core.Account{
			ID:       fmt.Sprintf("primary_%02d", index),
			Provider: core.ProviderOpenAI,
			Label:    fmt.Sprintf("Primary %d", index),
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
		})
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "backup",
		Provider: core.ProviderOpenAI,
		Label:    "Backup",
		Status:   core.AccountStatusActive,
		Backup:   true,
		Priority: 1000,
		Weight:   1000,
	}); err != nil {
		t.Fatal(err)
	}

	candidates := NewPool(repo).MonitorCandidates(core.ProviderOpenAI, &core.APIClient{ID: "monitor:default"})
	if len(candidates) != maxProviderCandidates+2 {
		t.Fatalf("monitor candidate count = %d, want %d", len(candidates), maxProviderCandidates+2)
	}
	if candidates[len(candidates)-1].ID != "backup" {
		t.Fatalf("last monitor candidate = %q, want backup after all primaries", candidates[len(candidates)-1].ID)
	}
}

func TestCandidatesAppendBackupWhenPrimaryCandidatesAllUnavailable(t *testing.T) {
	repo := storage.NewMemoryRepository()
	until := time.Now().UTC().Add(time.Hour)
	for index := range maxProviderCandidates + 1 {
		_ = repo.UpsertAccount(core.Account{
			ID:            fmt.Sprintf("primary_%02d", index),
			Provider:      core.ProviderOpenAI,
			Label:         fmt.Sprintf("Primary %d", index),
			Status:        core.AccountStatusCooling,
			CooldownUntil: &until,
			Priority:      100,
			Weight:        100,
		})
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "backup",
		Provider: core.ProviderOpenAI,
		Label:    "Backup",
		Status:   core.AccountStatusActive,
		Backup:   true,
		Priority: 1000,
		Weight:   1000,
	}); err != nil {
		t.Fatal(err)
	}

	candidates := NewPool(repo).Candidates(core.ProviderOpenAI, nil)
	if len(candidates) != 1 || candidates[0].ID != "backup" {
		t.Fatalf("candidates = %#v, want backup when all primaries are unavailable", candidates)
	}
}

func TestStickyAccountRankUsesUnsignedHash(t *testing.T) {
	rank := stickyAccountRank("client", core.ProviderOpenAI, "account")
	if rank == 0 {
		t.Fatal("rank should not be zero for non-empty input")
	}
}

func TestMarkFailureSkipsDuplicateCooldownWrites(t *testing.T) {
	repo := storage.NewMemoryRepository()
	account := core.Account{
		ID:       "acct_a",
		Provider: core.ProviderOpenAI,
		Status:   core.AccountStatusActive,
	}
	if err := repo.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}
	stale, err := repo.GetAccount(account.ID)
	if err != nil {
		t.Fatal(err)
	}

	pool := NewPool(repo)
	if err := pool.MarkFailure(stale, core.AccountStatusCooling, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := pool.MarkFailure(stale, core.AccountStatusCooling, time.Minute); err != nil {
		t.Fatal(err)
	}

	stored, err := repo.GetAccount(account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TotalFails != 1 || stored.ConsecutiveFails != 1 {
		t.Fatalf("failure counters = total:%d consecutive:%d, want 1/1", stored.TotalFails, stored.ConsecutiveFails)
	}
	if stored.Status != core.AccountStatusCooling || stored.CooldownUntil == nil {
		t.Fatalf("status=%s cooldown=%#v, want cooling with cooldown", stored.Status, stored.CooldownUntil)
	}
}

func TestMarkFailureRecordsRefreshingAccountFailure(t *testing.T) {
	repo := storage.NewMemoryRepository()
	account := core.Account{
		ID:       "acct_refresh_failure",
		Provider: core.ProviderOpenAI,
		Status:   core.AccountStatusActive,
	}
	if err := repo.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}

	pool := NewPool(repo)
	if err := pool.MarkRefreshing(account); err != nil {
		t.Fatal(err)
	}
	if err := pool.MarkFailure(account, core.AccountStatusCooling, time.Minute); err != nil {
		t.Fatal(err)
	}

	stored, err := repo.GetAccount(account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != core.AccountStatusCooling || stored.CooldownUntil == nil {
		t.Fatalf("status=%s cooldown=%#v, want cooling with cooldown", stored.Status, stored.CooldownUntil)
	}
	if stored.TotalFails != 1 || stored.ConsecutiveFails != 1 {
		t.Fatalf("failure counters = total:%d consecutive:%d, want 1/1", stored.TotalFails, stored.ConsecutiveFails)
	}
}

func TestMarkRefreshingUsesExpiringLease(t *testing.T) {
	repo := storage.NewMemoryRepository()
	account := core.Account{
		ID:       "acct_refresh",
		Provider: core.ProviderOpenAI,
		Status:   core.AccountStatusActive,
	}
	if err := repo.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}

	pool := NewPool(repo)
	if err := pool.MarkRefreshing(account); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.GetAccount(account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != core.AccountStatusRefreshing {
		t.Fatalf("status = %q, want refreshing", stored.Status)
	}
	if stored.CooldownUntil == nil || !stored.CooldownUntil.After(time.Now().UTC()) {
		t.Fatalf("refresh deadline = %#v, want future deadline", stored.CooldownUntil)
	}
}

func TestRuntimeAccountUpdatesPreserveControlDisabledState(t *testing.T) {
	repo := storage.NewMemoryRepository()
	account := core.Account{
		ID:              "acct_refresh",
		Provider:        core.ProviderOpenAI,
		Status:          core.AccountStatusActive,
		ControlDisabled: true,
		Backup:          true,
	}
	if err := repo.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}
	stale := account
	stale.ControlDisabled = false
	stale.Backup = false

	pool := NewPool(repo)
	updated, err := pool.MarkCredentialRefreshed(stale, core.Credential{AccessToken: "fresh"})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.ControlDisabled {
		t.Fatalf("returned account = %#v, want control disabled preserved", updated)
	}
	if !updated.Backup {
		t.Fatalf("returned account = %#v, want backup flag preserved", updated)
	}
	stored, err := repo.GetAccount(account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.ControlDisabled || !stored.Backup || stored.Credential.AccessToken != "fresh" {
		t.Fatalf("stored account = %#v, want control disabled and backup with fresh credential", stored)
	}
}
