package controlplane

import (
	"context"
	"errors"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

type AccountPoolViews struct {
	Normal   []core.Account
	Cooling  []core.Account
	Abnormal []core.Account
}

type AccountRuntimeReconcileReport struct {
	Scanned              int
	Updated              int
	Reactivated          int
	QuotaCooldownCleared int
}

func (s *Service) AccountPoolViews(now time.Time) AccountPoolViews {
	if s == nil || s.repo == nil {
		return AccountPoolViews{}
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return buildAccountPoolViews(s.repo.ListAccounts(), now)
}

func (s *Service) ReconcileAccountRuntimeState(ctx context.Context, now time.Time) (AccountRuntimeReconcileReport, error) {
	if s == nil || s.repo == nil {
		return AccountRuntimeReconcileReport{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	report := AccountRuntimeReconcileReport{}
	var errs []error
	for _, account := range s.repo.ListAccounts() {
		select {
		case <-ctx.Done():
			return report, errors.Join(append(errs, ctx.Err())...)
		default:
		}
		report.Scanned++
		before := account
		account = core.NormalizeAccountRuntimeState(account, now)
		var quotaCleared bool
		account, quotaCleared = core.ClearExpiredAccountQuotaCooldowns(account, now)
		if !accountRuntimeChanged(before, account) {
			continue
		}
		if err := s.upsertAccountRuntimeState(account); err != nil {
			errs = append(errs, err)
			continue
		}
		report.Updated++
		if before.Status != core.AccountStatusActive &&
			account.Status == core.AccountStatusActive &&
			core.AccountPoolStateFor(account, now) == core.AccountPoolStateNormal {
			report.Reactivated++
		}
		if quotaCleared {
			report.QuotaCooldownCleared++
		}
	}
	return report, errors.Join(errs...)
}

func buildAccountPoolViews(accounts []core.Account, now time.Time) AccountPoolViews {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	views := AccountPoolViews{}
	for _, account := range accounts {
		account = core.NormalizeAccountRuntimeState(account, now)
		switch core.AccountPoolStateFor(account, now) {
		case core.AccountPoolStateAbnormal:
			views.Abnormal = append(views.Abnormal, account)
		case core.AccountPoolStateCooling:
			views.Cooling = append(views.Cooling, account)
		default:
			views.Normal = append(views.Normal, account)
		}
	}
	return views
}

func accountRuntimeChanged(before, after core.Account) bool {
	if before.Status != after.Status ||
		before.ControlDisabled != after.ControlDisabled ||
		before.ConsecutiveFails != after.ConsecutiveFails {
		return true
	}
	if !sameTimePointer(before.CooldownUntil, after.CooldownUntil) {
		return true
	}
	beforeQuota := ""
	afterQuota := ""
	if before.Credential.Metadata != nil {
		beforeQuota = before.Credential.Metadata[core.AccountQuotaMetadataKey]
	}
	if after.Credential.Metadata != nil {
		afterQuota = after.Credential.Metadata[core.AccountQuotaMetadataKey]
	}
	return beforeQuota != afterQuota
}

func sameTimePointer(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
