package storage

import (
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func (r *MemoryRepository) CreateBalanceMigrationCode(code core.BalanceMigrationCode) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	user, ok := r.users[code.UserID]
	if !ok || !user.Enabled || user.IsAdmin() {
		return ErrBalanceMigrationInvalid
	}
	if user.BalanceNanoUSD <= 0 {
		return ErrBalanceMigrationNoBalance
	}
	if memoryBalanceMigrationBlocked(r, user.ID) {
		return ErrBalanceMigrationBlocked
	}
	for hash, existing := range r.balanceMigrations {
		if existing.UserID != user.ID {
			continue
		}
		if existing.Status == core.BalanceMigrationClaimed {
			return ErrBalanceMigrationClaimed
		}
		delete(r.balanceMigrations, hash)
	}
	r.balanceMigrations[code.CodeHash] = code
	return nil
}

func (r *MemoryRepository) ClaimBalanceMigrationCode(codeHash, targetUserID string) (core.BalanceMigrationCode, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	targetUserID = normalizeBalanceMigrationTarget(targetUserID)
	if targetUserID == "" {
		return core.BalanceMigrationCode{}, ErrBalanceMigrationInvalid
	}
	code, ok := r.balanceMigrations[strings.TrimSpace(codeHash)]
	if !ok {
		return core.BalanceMigrationCode{}, ErrBalanceMigrationInvalid
	}
	if code.Status == core.BalanceMigrationClaimed {
		if code.TargetUserID != targetUserID {
			return core.BalanceMigrationCode{}, ErrBalanceMigrationTargetMismatch
		}
		return code, nil
	}
	if code.TargetUserID != "" && code.TargetUserID != targetUserID {
		return core.BalanceMigrationCode{}, ErrBalanceMigrationTargetMismatch
	}
	if code.Status == core.BalanceMigrationPending && !code.ExpiresAt.After(time.Now().UTC()) {
		return core.BalanceMigrationCode{}, ErrBalanceMigrationExpired
	}
	if memoryBalanceMigrationBlocked(r, code.UserID) {
		return core.BalanceMigrationCode{}, ErrBalanceMigrationBlocked
	}
	user, ok := r.users[code.UserID]
	if !ok || user.IsAdmin() {
		return core.BalanceMigrationCode{}, ErrBalanceMigrationInvalid
	}
	now := time.Now().UTC()
	user.Enabled = false
	user.UpdatedAt = now
	r.users[user.ID] = user
	for tokenHash, session := range r.sessions {
		if session.UserID == user.ID {
			delete(r.sessions, tokenHash)
		}
	}
	code.TargetUserID = targetUserID
	code.Status = core.BalanceMigrationDraining
	code.UpdatedAt = now
	for _, request := range r.billing {
		if request.UserID == user.ID && request.Status == core.BillingRequestReserved {
			r.balanceMigrations[code.CodeHash] = code
			return code, ErrBalanceMigrationDraining
		}
	}
	if user.BalanceNanoUSD <= 0 {
		user.Enabled = true
		user.UpdatedAt = now
		r.users[user.ID] = user
		code.TargetUserID = ""
		code.Status = core.BalanceMigrationPending
		r.balanceMigrations[code.CodeHash] = code
		return core.BalanceMigrationCode{}, ErrBalanceMigrationNoBalance
	}
	amount := user.BalanceNanoUSD
	user.BalanceNanoUSD = 0
	user.UpdatedAt = now
	r.users[user.ID] = user
	r.ledger = append(r.ledger, core.BillingLedgerEntry{
		ID:                  "balance_migration:" + code.ID,
		UserID:              user.ID,
		Kind:                "balance_migration_out",
		AmountNanoUSD:       -amount,
		BalanceAfterNanoUSD: 0,
		Note:                "Balance migrated to the new site",
		CreatedAt:           now,
	})
	code.Status = core.BalanceMigrationClaimed
	code.AmountNanoUSD = amount
	code.ClaimedAt = &now
	code.UpdatedAt = now
	r.balanceMigrations[code.CodeHash] = code
	return code, nil
}

func memoryBalanceMigrationBlocked(r *MemoryRepository, userID string) bool {
	for _, order := range r.payments {
		if order.UserID == userID && order.Status == core.PaymentOrderPending {
			return true
		}
	}
	for _, refund := range r.refunds {
		if refund.UserID == userID && refund.Status == core.PaymentRefundPending {
			return true
		}
	}
	return false
}
