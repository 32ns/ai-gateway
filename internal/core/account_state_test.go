package core

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNormalizeAccountRuntimeStateClearsExpiredCooldown(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	expired := now.Add(-time.Minute)
	account := NormalizeAccountRuntimeState(Account{
		Status:           AccountStatusCooling,
		CooldownUntil:    &expired,
		ConsecutiveFails: 2,
	}, now)

	if account.Status != AccountStatusActive {
		t.Fatalf("status = %q, want %q", account.Status, AccountStatusActive)
	}
	if account.CooldownUntil != nil {
		t.Fatalf("cooldown = %#v, want nil", account.CooldownUntil)
	}
	if account.ConsecutiveFails != 0 {
		t.Fatalf("consecutive fails = %d, want reset failure count", account.ConsecutiveFails)
	}
}

func TestNormalizeAccountRuntimeStateKeepsActiveCooldown(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	future := now.Add(time.Minute)
	account := NormalizeAccountRuntimeState(Account{
		Status:        AccountStatusCooling,
		CooldownUntil: &future,
	}, now)

	if account.Status != AccountStatusCooling {
		t.Fatalf("status = %q, want %q", account.Status, AccountStatusCooling)
	}
	if account.CooldownUntil == nil || !account.CooldownUntil.Equal(future) {
		t.Fatalf("cooldown = %#v, want %v", account.CooldownUntil, future)
	}
}

func TestNormalizeAccountRuntimeStateClearsStaleRefreshing(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	account := NormalizeAccountRuntimeState(Account{
		Status: AccountStatusRefreshing,
	}, now)

	if account.Status != AccountStatusActive {
		t.Fatalf("status = %q, want %q", account.Status, AccountStatusActive)
	}
}

func TestNormalizeAccountRuntimeStateKeepsRefreshingLease(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	future := now.Add(time.Minute)
	account := NormalizeAccountRuntimeState(Account{
		Status:        AccountStatusRefreshing,
		CooldownUntil: &future,
	}, now)

	if account.Status != AccountStatusRefreshing {
		t.Fatalf("status = %q, want %q", account.Status, AccountStatusRefreshing)
	}
	if AccountRuntimeAvailable(account, now) {
		t.Fatal("refreshing account should not be runtime available while lease is active")
	}
}

func TestNormalizeAccountRuntimeStateDerivesCoolingFromFutureCooldown(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	future := now.Add(time.Minute)
	account := NormalizeAccountRuntimeState(Account{
		Status:        AccountStatusActive,
		CooldownUntil: &future,
	}, now)

	if account.Status != AccountStatusCooling {
		t.Fatalf("status = %q, want %q", account.Status, AccountStatusCooling)
	}
	if AccountRuntimeAvailable(account, now) {
		t.Fatal("future cooldown account should not be runtime available")
	}
}

func TestAccountRuntimeAvailableRejectsActiveQuotaLimit(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	account := accountWithQuota(t, AccountQuotaSnapshot{
		Primary: &AccountQuotaWindow{UsedPercent: 100, ResetsAt: &reset},
	})

	if AccountRuntimeAvailable(account, now) {
		t.Fatal("quota-limited account should not be runtime available")
	}
	if status := AccountQuotaLimitStatus(account, now); status != AccountRuntimeStatusTimeLimit {
		t.Fatalf("quota status = %q, want %q", status, AccountRuntimeStatusTimeLimit)
	}
}

func TestAccountRuntimeAvailableRejectsScopedRateLimitWithoutQuotaStatus(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	account := accountWithQuota(t, AccountQuotaSnapshot{
		Additional: map[string]AccountQuotaSnapshot{
			AccountQuotaRuntimeChatLimitID: {
				Source:      AccountQuotaRuntimeSource,
				LimitID:     AccountQuotaRuntimeChatLimitID,
				Primary:     &AccountQuotaWindow{Name: "rate_limit", UsedPercent: 100, ResetsAt: &reset},
				ReachedType: "primary_window",
			},
		},
	})

	if AccountRuntimeAvailable(account, now) {
		t.Fatal("scoped rate-limited account should not be runtime available")
	}
	if !AccountScopedRateLimitActive(account, now) {
		t.Fatal("scoped rate limit should be active")
	}
	if status := AccountQuotaLimitStatus(account, now); status != "" {
		t.Fatalf("quota status = %q, want empty for scoped runtime rate limit", status)
	}
	if got := AccountPoolStateFor(account, now); got != AccountPoolStateCooling {
		t.Fatalf("pool state = %q, want %q", got, AccountPoolStateCooling)
	}
}

func TestAccountRuntimeAvailableRejectsLegacyScopedRateLimit(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	account := accountWithQuota(t, AccountQuotaSnapshot{
		Primary:     &AccountQuotaWindow{Name: "rate_limit", UsedPercent: 100, ResetsAt: &reset},
		ReachedType: "primary_window",
	})

	if AccountRuntimeAvailable(account, now) {
		t.Fatal("legacy scoped rate-limited account should not be runtime available")
	}
	if status := AccountQuotaLimitStatus(account, now); status != "" {
		t.Fatalf("quota status = %q, want empty for legacy scoped runtime rate limit", status)
	}
}

func TestAccountRuntimeAvailableIgnoresExpiredQuotaLimit(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(-time.Hour)
	account := accountWithQuota(t, AccountQuotaSnapshot{
		Secondary: &AccountQuotaWindow{UsedPercent: 100, ResetsAt: &reset},
	})

	if !AccountRuntimeAvailable(account, now) {
		t.Fatal("expired quota limit should be runtime available")
	}
	if status := AccountQuotaLimitStatus(account, now); status != "" {
		t.Fatalf("quota status = %q, want empty", status)
	}
}

func TestAccountAvailableForImageRoutingIgnoresChatQuotaLimit(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	account := accountWithQuota(t, AccountQuotaSnapshot{
		Primary: &AccountQuotaWindow{UsedPercent: 100, ResetsAt: &reset},
		Image:   &AccountImageQuota{Remaining: 3},
	})

	if AccountAvailableForRouting(account, now) {
		t.Fatal("chat routing should reject active chat quota limit")
	}
	if !AccountAvailableForImageRouting(account, now) {
		t.Fatal("image routing should ignore chat quota limit when image quota remains")
	}
}

func TestAccountAvailableForImageRoutingRejectsActiveImageQuotaLimit(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	account := accountWithQuota(t, AccountQuotaSnapshot{
		Image: &AccountImageQuota{Remaining: 0, ResetsAt: &reset},
	})

	if AccountAvailableForImageRouting(account, now) {
		t.Fatal("image routing should reject exhausted image quota before reset")
	}
}

func TestAccountAvailableForImageRoutingAllowsExpiredImageQuotaLimit(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(-time.Hour)
	account := accountWithQuota(t, AccountQuotaSnapshot{
		Image: &AccountImageQuota{Remaining: 0, ResetsAt: &reset},
	})

	if !AccountAvailableForImageRouting(account, now) {
		t.Fatal("expired image quota reset should be treated as available")
	}
}

func TestAccountPoolStateClassifiesCoolingQuota(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	account := accountWithQuota(t, AccountQuotaSnapshot{
		Primary: &AccountQuotaWindow{Name: "rate_limit", UsedPercent: 100, ResetsAt: &reset},
	})

	if got := AccountPoolStateFor(account, now); got != AccountPoolStateCooling {
		t.Fatalf("pool state = %q, want %q", got, AccountPoolStateCooling)
	}
}

func TestAccountPoolStateClassifiesDurableFailureAsAbnormal(t *testing.T) {
	account := Account{Status: AccountStatusProviderBanned}

	if got := AccountPoolStateFor(account, time.Now().UTC()); got != AccountPoolStateAbnormal {
		t.Fatalf("pool state = %q, want %q", got, AccountPoolStateAbnormal)
	}
}

func TestAccountPoolStateIgnoresQuotaRefreshErrorWithoutSnapshot(t *testing.T) {
	account := Account{
		Status: AccountStatusActive,
		Credential: Credential{Metadata: map[string]string{
			AccountQuotaErrorMetadataKey: "quota refresh failed",
		}},
	}

	if got := AccountPoolStateFor(account, time.Now().UTC()); got != AccountPoolStateNormal {
		t.Fatalf("pool state = %q, want %q", got, AccountPoolStateNormal)
	}
}

func TestClearExpiredAccountQuotaCooldownsRemovesScopedChatLimit(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(-time.Second)
	account := accountWithQuota(t, AccountQuotaSnapshot{
		Additional: map[string]AccountQuotaSnapshot{
			AccountQuotaRuntimeChatLimitID: {
				Source:      AccountQuotaRuntimeSource,
				LimitID:     AccountQuotaRuntimeChatLimitID,
				Primary:     &AccountQuotaWindow{Name: "rate_limit", UsedPercent: 100, ResetsAt: &reset},
				ReachedType: "primary_window",
			},
		},
	})

	updated, changed := ClearExpiredAccountQuotaCooldowns(account, now)
	if !changed {
		t.Fatal("ClearExpiredAccountQuotaCooldowns changed = false, want true")
	}
	if quota := ReadAccountQuota(updated); quota != nil {
		t.Fatalf("quota = %#v, want cleared", quota)
	}
	if got := AccountPoolStateFor(updated, now); got != AccountPoolStateNormal {
		t.Fatalf("pool state = %q, want %q", got, AccountPoolStateNormal)
	}
}

func TestClearExpiredAccountQuotaCooldownsRemovesRuntimeOnlyTimestamp(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(-time.Second)
	refreshedAt := now.Add(-time.Minute)
	account := accountWithQuota(t, AccountQuotaSnapshot{
		Additional: map[string]AccountQuotaSnapshot{
			AccountQuotaRuntimeChatLimitID: {
				Source:      AccountQuotaRuntimeSource,
				LimitID:     AccountQuotaRuntimeChatLimitID,
				Primary:     &AccountQuotaWindow{Name: "rate_limit", UsedPercent: 100, ResetsAt: &reset},
				ReachedType: "primary_window",
			},
		},
		RefreshedAt: &refreshedAt,
	})

	updated, changed := ClearExpiredAccountQuotaCooldowns(account, now)
	if !changed {
		t.Fatal("ClearExpiredAccountQuotaCooldowns changed = false, want true")
	}
	if quota := ReadAccountQuota(updated); quota != nil {
		t.Fatalf("quota = %#v, want cleared", quota)
	}
}

func TestClearExpiredAccountQuotaCooldownsDropsProviderImageSnapshot(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(-time.Second)
	account := accountWithQuota(t, AccountQuotaSnapshot{
		Source: "openai_chatgpt_usage",
		Image:  &AccountImageQuota{Remaining: 0, ResetsAt: &reset},
	})

	updated, changed := ClearExpiredAccountQuotaCooldowns(account, now)
	if changed {
		t.Fatal("ClearExpiredAccountQuotaCooldowns changed = true, want false for provider snapshot")
	}
	if quota := ReadAccountQuota(updated); quota == nil || quota.Image == nil {
		t.Fatalf("quota = %#v, want provider image quota preserved", quota)
	}
}

func TestClearAccountScopedRateLimitsPreservesProviderQuota(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	account := accountWithQuota(t, AccountQuotaSnapshot{
		Source:    "openai_chatgpt_usage",
		Plan:      "plus",
		Secondary: &AccountQuotaWindow{Name: "weekly", UsedPercent: 40},
		Image:     &AccountImageQuota{Remaining: 0, ResetsAt: &reset},
		Additional: map[string]AccountQuotaSnapshot{
			AccountQuotaRuntimeChatLimitID: {
				Source:      AccountQuotaRuntimeSource,
				LimitID:     AccountQuotaRuntimeChatLimitID,
				Primary:     &AccountQuotaWindow{Name: "rate_limit", UsedPercent: 100, ResetsAt: &reset},
				ReachedType: "primary_window",
			},
			AccountQuotaRuntimeImageLimitID: {
				Source:  AccountQuotaRuntimeSource,
				LimitID: AccountQuotaRuntimeImageLimitID,
				Image:   &AccountImageQuota{Remaining: 0, ResetsAt: &reset},
			},
		},
	})

	updated := ClearAccountScopedRateLimits(account)
	quota := ReadAccountQuota(updated)
	if quota == nil {
		t.Fatal("quota should be preserved")
	}
	if quota.Primary != nil {
		t.Fatalf("primary scoped rate limit = %#v, want cleared", quota.Primary)
	}
	if quota.Secondary == nil || quota.Secondary.Name != "weekly" {
		t.Fatalf("secondary quota = %#v, want preserved", quota.Secondary)
	}
	if quota.Image == nil {
		t.Fatalf("provider image quota should be preserved: %#v", quota)
	}
	if quota.Additional != nil {
		t.Fatalf("additional runtime limits = %#v, want cleared", quota.Additional)
	}
}

func TestAccountAvailableForImageRoutingIgnoresChatGPTUsageChatLimitWithoutImageSnapshot(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	account := accountWithQuota(t, AccountQuotaSnapshot{
		Source:  "openai_chatgpt_usage",
		Primary: &AccountQuotaWindow{UsedPercent: 100, ResetsAt: &reset},
	})

	if !AccountAvailableForImageRouting(account, now) {
		t.Fatal("ChatGPT Web image routing should ignore chat windows even when image quota is unknown")
	}
}

func TestAccountAvailableForImageRoutingRespectsAPIKeyBillingLimit(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	account := accountWithQuota(t, AccountQuotaSnapshot{
		Source:  "new_api_key_billing",
		Primary: &AccountQuotaWindow{UsedPercent: 100, ResetsAt: &reset},
	})

	if AccountAvailableForImageRouting(account, now) {
		t.Fatal("API key image routing should still respect shared billing limits")
	}
}

func TestAccountAvailableForImageRoutingTreatsScopedChatRateLimitAsIndependent(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	account := accountWithQuota(t, AccountQuotaSnapshot{
		Additional: map[string]AccountQuotaSnapshot{
			AccountQuotaRuntimeChatLimitID: {
				Source:      AccountQuotaRuntimeSource,
				LimitID:     AccountQuotaRuntimeChatLimitID,
				Primary:     &AccountQuotaWindow{Name: "rate_limit", UsedPercent: 100, ResetsAt: &reset},
				ReachedType: "primary_window",
			},
		},
	})

	if !AccountAvailableForImageRouting(account, now) {
		t.Fatal("scoped chat rate limits should not block image routing")
	}
}

func TestAccountAvailableForImageRoutingRejectsScopedImageRateLimit(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	account := accountWithQuota(t, AccountQuotaSnapshot{
		Image: &AccountImageQuota{Remaining: 3},
		Additional: map[string]AccountQuotaSnapshot{
			AccountQuotaRuntimeImageLimitID: {
				Source:  AccountQuotaRuntimeSource,
				LimitID: AccountQuotaRuntimeImageLimitID,
				Image:   &AccountImageQuota{Remaining: 0, ResetsAt: &reset},
			},
		},
	})

	if AccountAvailableForImageRouting(account, now) {
		t.Fatal("scoped image rate limits should block image routing")
	}
	if got := AccountPoolStateFor(account, now); got != AccountPoolStateCooling {
		t.Fatalf("pool state = %q, want %q", got, AccountPoolStateCooling)
	}
}

func TestAccountQuotaWindowUsedPercentTreatsExpiredWindowAsReset(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(-time.Second)
	window := &AccountQuotaWindow{UsedPercent: 1, ResetsAt: &reset}

	if got := AccountQuotaWindowUsedPercent(window, now); got != 0 {
		t.Fatalf("used percent = %v, want 0", got)
	}
	if AccountQuotaWindowResetActive(window, now) {
		t.Fatal("expired quota window reset should not be active")
	}
}

func TestAccountQuotaWindowUsedPercentKeepsCurrentWindow(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	window := &AccountQuotaWindow{UsedPercent: 1, ResetsAt: &reset}

	if got := AccountQuotaWindowUsedPercent(window, now); got != 1 {
		t.Fatalf("used percent = %v, want 1", got)
	}
	if !AccountQuotaWindowResetActive(window, now) {
		t.Fatal("future quota window reset should be active")
	}
}

func accountWithQuota(t *testing.T, snapshot AccountQuotaSnapshot) Account {
	t.Helper()
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return Account{
		Status: AccountStatusActive,
		Credential: Credential{Metadata: map[string]string{
			AccountQuotaMetadataKey: string(raw),
		}},
	}
}

func TestNormalizeAccountRuntimeStateLetsDurableStatusWin(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	future := now.Add(time.Minute)
	account := NormalizeAccountRuntimeState(Account{
		Status:        AccountStatusBlocked,
		CooldownUntil: &future,
	}, now)

	if account.Status != AccountStatusBlocked {
		t.Fatalf("status = %q, want %q", account.Status, AccountStatusBlocked)
	}
	if account.CooldownUntil != nil {
		t.Fatalf("cooldown = %#v, want nil", account.CooldownUntil)
	}
	if AccountRuntimeAvailable(account, now) {
		t.Fatal("blocked account should not be runtime available")
	}
}

func TestNormalizeAccountRuntimeStateIgnoresQuotaRefreshErrorMessage(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	account := NormalizeAccountRuntimeState(Account{
		Status: AccountStatusActive,
		Credential: Credential{Metadata: map[string]string{
			AccountQuotaErrorMetadataKey: "credential_expired: refresh token was already used",
		}},
	}, now)

	if account.Status != AccountStatusActive {
		t.Fatalf("status = %q, want %q", account.Status, AccountStatusActive)
	}
	if !AccountRuntimeAvailable(account, now) {
		t.Fatal("quota refresh error metadata should not affect runtime availability")
	}
}

func TestNormalizeAccountRuntimeStateIgnoresQuotaRefreshFailureStatusForRouting(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	account := NormalizeAccountRuntimeState(Account{
		Status:        AccountStatusCooling,
		CooldownUntil: &future,
		Credential: Credential{Metadata: map[string]string{
			AccountQuotaErrorStatusMetadataKey: string(AccountStatusExpired),
		}},
	}, now)

	if account.Status != AccountStatusCooling {
		t.Fatalf("status = %q, want %q", account.Status, AccountStatusCooling)
	}
	if account.CooldownUntil == nil || !account.CooldownUntil.Equal(future) {
		t.Fatalf("cooldown = %#v, want %v", account.CooldownUntil, future)
	}
	if status := AccountQuotaRefreshFailureStatus(account); status != AccountStatusExpired {
		t.Fatalf("quota refresh failure status = %q, want %q", status, AccountStatusExpired)
	}
}

func TestNormalizeAccountRuntimeStateKeepsTransientQuotaRefreshErrorActive(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	account := NormalizeAccountRuntimeState(Account{
		Status: AccountStatusActive,
		Credential: Credential{Metadata: map[string]string{
			AccountQuotaErrorMetadataKey: "quota backend unavailable",
		}},
	}, now)

	if account.Status != AccountStatusActive {
		t.Fatalf("status = %q, want %q", account.Status, AccountStatusActive)
	}
}

func TestAccountAvailableForRoutingRejectsControlDisabledRuntimeHealthyAccount(t *testing.T) {
	now := time.Date(2026, 5, 10, 17, 20, 0, 0, time.UTC)
	account := Account{
		Status:          AccountStatusActive,
		ControlDisabled: true,
	}

	if !AccountRuntimeAvailable(account, now) {
		t.Fatal("runtime availability should ignore local control disabled state")
	}
	if AccountAvailableForRouting(account, now) {
		t.Fatal("routing availability should include local control disabled state")
	}
}
