package web

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
)

func TestQuotaWindowPercentDoesNotGuessFractionalOpenAIValues(t *testing.T) {
	window := &core.AccountQuotaWindow{UsedPercent: 0.4}
	if got := quotaWindowPercent(window); got != "0.40" {
		t.Fatalf("quotaWindowPercent = %q, want 0.40", got)
	}
	if got := quotaPercentText(window.UsedPercent); got != "0%" {
		t.Fatalf("quotaPercentText = %q, want 0%%", got)
	}
}

func TestQuotaWindowPercentClampsRange(t *testing.T) {
	if got := quotaWindowPercent(&core.AccountQuotaWindow{UsedPercent: 130}); got != "100.00" {
		t.Fatalf("quotaWindowPercent high = %q, want 100.00", got)
	}
	if got := quotaWindowPercent(&core.AccountQuotaWindow{UsedPercent: -1}); got != "0.00" {
		t.Fatalf("quotaWindowPercent low = %q, want 0.00", got)
	}
}

func TestQuotaWindowDisplayTreatsExpiredResetAsZero(t *testing.T) {
	reset := time.Now().UTC().Add(-time.Minute)
	window := &core.AccountQuotaWindow{UsedPercent: 1, WindowMinutes: 300, ResetsAt: &reset}

	if got := quotaWindowPercent(window); got != "0.00" {
		t.Fatalf("quotaWindowPercent = %q, want 0.00", got)
	}
	if got := quotaWindowSummary(window); got != "0% / 5h" {
		t.Fatalf("quotaWindowSummary = %q, want 0%% / 5h", got)
	}
}

func TestAccountFilterStatus(t *testing.T) {
	tests := []struct {
		name    string
		account core.Account
		want    string
	}{
		{
			name:    "normal active account",
			account: core.Account{Status: core.AccountStatusActive},
			want:    "normal",
		},
		{
			name:    "local disabled active account remains normal",
			account: core.Account{Status: core.AccountStatusActive, ControlDisabled: true},
			want:    "normal",
		},
		{
			name:    "local disabled quota limit keeps limit filter",
			account: localDisabledAccount(accountWithQuota(t, core.AccountQuotaSnapshot{Primary: &core.AccountQuotaWindow{UsedPercent: 100}})),
			want:    "time_limit",
		},
		{
			name: "quota refresh error stays normal",
			account: core.Account{
				Status: core.AccountStatusActive,
				Credential: core.Credential{Metadata: map[string]string{
					core.AccountQuotaErrorMetadataKey: "quota unavailable",
				}},
			},
			want: "normal",
		},
		{
			name: "quota refresh error with usable snapshot stays normal",
			account: accountWithQuotaAndError(t, core.AccountQuotaSnapshot{
				Primary:   &core.AccountQuotaWindow{UsedPercent: 49},
				Secondary: &core.AccountQuotaWindow{UsedPercent: 30},
			}),
			want: "normal",
		},
		{
			name: "runtime cooldown stays cooling",
			account: core.Account{
				Status:        core.AccountStatusCooling,
				CooldownUntil: ptrTime(time.Now().UTC().Add(time.Minute)),
			},
			want: "cooling",
		},
		{
			name: "runtime refreshing stays cooling",
			account: core.Account{
				Status:        core.AccountStatusRefreshing,
				CooldownUntil: ptrTime(time.Now().UTC().Add(time.Minute)),
			},
			want: "cooling",
		},
		{
			name: "scoped rate limit stays cooling",
			account: accountWithQuota(t, core.AccountQuotaSnapshot{
				Additional: map[string]core.AccountQuotaSnapshot{
					core.AccountQuotaRuntimeChatLimitID: {
						Source:      core.AccountQuotaRuntimeSource,
						LimitID:     core.AccountQuotaRuntimeChatLimitID,
						Primary:     &core.AccountQuotaWindow{Name: "rate_limit", UsedPercent: 100, ResetsAt: ptrTime(time.Now().UTC().Add(time.Minute))},
						ReachedType: "primary_window",
					},
				},
			}),
			want: "cooling",
		},
		{
			name: "scoped image rate limit stays cooling",
			account: accountWithQuota(t, core.AccountQuotaSnapshot{
				Additional: map[string]core.AccountQuotaSnapshot{
					core.AccountQuotaRuntimeImageLimitID: {
						Source:  core.AccountQuotaRuntimeSource,
						LimitID: core.AccountQuotaRuntimeImageLimitID,
						Image:   &core.AccountImageQuota{Remaining: 0, ResetsAt: ptrTime(time.Now().UTC().Add(time.Minute))},
					},
				},
			}),
			want: "cooling",
		},
		{
			name:    "expired account is exception",
			account: core.Account{Status: core.AccountStatusExpired},
			want:    "exception",
		},
		{
			name:    "primary quota reached is time limit",
			account: accountWithQuota(t, core.AccountQuotaSnapshot{Primary: &core.AccountQuotaWindow{UsedPercent: 100}}),
			want:    "time_limit",
		},
		{
			name:    "secondary quota reached is weekly limit",
			account: accountWithQuota(t, core.AccountQuotaSnapshot{Secondary: &core.AccountQuotaWindow{UsedPercent: 100}}),
			want:    "week_limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := controlplane.AccountFilterStatus(tt.account); got != tt.want {
				t.Fatalf("AccountFilterStatus = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestScopedRateLimitRuntimeStatusText(t *testing.T) {
	account := accountWithQuota(t, core.AccountQuotaSnapshot{
		Additional: map[string]core.AccountQuotaSnapshot{
			core.AccountQuotaRuntimeChatLimitID: {
				Source:      core.AccountQuotaRuntimeSource,
				LimitID:     core.AccountQuotaRuntimeChatLimitID,
				Primary:     &core.AccountQuotaWindow{Name: "rate_limit", UsedPercent: 100, ResetsAt: ptrTime(time.Now().UTC().Add(time.Minute))},
				ReachedType: "primary_window",
			},
		},
	})
	status := controlplane.AccountRuntimeStatus(account)

	if got := accountRuntimeStatusText(localeEN, status); got != "cooldown" {
		t.Fatalf("english runtime status text = %q, want cooldown", got)
	}
	if got := accountRuntimeStatusText(localeZH, status); got != "冷却中" {
		t.Fatalf("chinese runtime status text = %q, want 冷却中", got)
	}
}

func localDisabledAccount(account core.Account) core.Account {
	account.ControlDisabled = true
	return account
}

func accountWithQuotaAndError(t *testing.T, snapshot core.AccountQuotaSnapshot) core.Account {
	return accountWithQuotaAndErrorMessage(t, snapshot, "quota backend unavailable")
}

func accountWithQuotaAndErrorMessage(t *testing.T, snapshot core.AccountQuotaSnapshot, message string) core.Account {
	account := accountWithQuota(t, snapshot)
	account.Credential.Metadata[core.AccountQuotaErrorMetadataKey] = message
	return account
}

func accountWithQuota(t *testing.T, snapshot core.AccountQuotaSnapshot) core.Account {
	t.Helper()
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return core.Account{
		Status: core.AccountStatusActive,
		Credential: core.Credential{Metadata: map[string]string{
			core.AccountQuotaMetadataKey: string(raw),
		}},
	}
}

func ptrTime(value time.Time) *time.Time {
	return &value
}
