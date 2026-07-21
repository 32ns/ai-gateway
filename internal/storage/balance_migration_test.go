package storage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestSQLiteBalanceMigrationClaimIsIdempotentAndDisablesLegacyUser(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	const userID = "migration_user"
	const amount = int64(123456789)
	if err := repo.UpsertUser(core.User{
		ID:             userID,
		Username:       "migration-user",
		Enabled:        true,
		BalanceNanoUSD: amount,
	}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.UpsertUserSession(core.UserSession{
		TokenHash: "migration-session",
		UserID:    userID,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("UpsertUserSession returned error: %v", err)
	}
	now := time.Now().UTC()
	if err := repo.CreateBalanceMigrationCode(core.BalanceMigrationCode{
		ID:          "migration_receipt",
		UserID:      userID,
		CodeHash:    "migration-code-hash",
		Status:      core.BalanceMigrationPending,
		ExpiresAt:   now.Add(time.Minute),
		GeneratedAt: now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("CreateBalanceMigrationCode returned error: %v", err)
	}

	claimed, err := repo.ClaimBalanceMigrationCode("migration-code-hash", "new-site-user")
	if err != nil {
		t.Fatalf("ClaimBalanceMigrationCode returned error: %v", err)
	}
	if claimed.ID != "migration_receipt" || claimed.Status != core.BalanceMigrationClaimed || claimed.AmountNanoUSD != amount {
		t.Fatalf("claimed code = %#v", claimed)
	}
	legacyUser, err := repo.GetUser(userID)
	if err != nil {
		t.Fatalf("GetUser returned error: %v", err)
	}
	if legacyUser.Enabled || legacyUser.BalanceNanoUSD != 0 {
		t.Fatalf("legacy user after claim = %#v", legacyUser)
	}
	if _, err := repo.GetUserSession("migration-session"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUserSession after claim error = %v, want ErrNotFound", err)
	}
	ledger := repo.ListBillingLedger(userID, 10)
	if len(ledger) != 1 || ledger[0].Kind != "balance_migration_out" || ledger[0].AmountNanoUSD != -amount || ledger[0].BalanceAfterNanoUSD != 0 {
		t.Fatalf("migration ledger = %#v", ledger)
	}

	retry, err := repo.ClaimBalanceMigrationCode("migration-code-hash", "new-site-user")
	if err != nil {
		t.Fatalf("retry ClaimBalanceMigrationCode returned error: %v", err)
	}
	if retry.ID != claimed.ID || retry.AmountNanoUSD != claimed.AmountNanoUSD {
		t.Fatalf("retry result = %#v, want %#v", retry, claimed)
	}
	if _, err := repo.ClaimBalanceMigrationCode("migration-code-hash", "another-new-user"); !errors.Is(err, ErrBalanceMigrationTargetMismatch) {
		t.Fatalf("target mismatch error = %v, want ErrBalanceMigrationTargetMismatch", err)
	}
}

func TestSQLiteBalanceMigrationRejectsEveryUnpaidPaymentOrder(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	const userID = "unpaid_migration_user"
	if err := repo.UpsertUser(core.User{
		ID:             userID,
		Username:       "unpaid-migration-user",
		Enabled:        true,
		BalanceNanoUSD: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	for _, status := range []core.PaymentOrderStatus{
		core.PaymentOrderPending,
		core.PaymentOrderClosed,
		core.PaymentOrderFailed,
	} {
		if err := repo.CreatePaymentOrder(core.PaymentOrder{
			ID:            "payment_" + string(status),
			OutTradeNo:    "trade_" + string(status),
			UserID:        userID,
			Provider:      core.PaymentProviderAlipay,
			AmountNanoUSD: core.NanoUSDPerUSD,
			Status:        status,
			CreatedAt:     time.Now().UTC(),
		}); err != nil {
			t.Fatalf("CreatePaymentOrder(%s) returned error: %v", status, err)
		}
	}
	now := time.Now().UTC()
	err = repo.CreateBalanceMigrationCode(core.BalanceMigrationCode{
		ID:          "blocked_migration",
		UserID:      userID,
		CodeHash:    "blocked-migration-code-hash",
		Status:      core.BalanceMigrationPending,
		ExpiresAt:   now.Add(time.Minute),
		GeneratedAt: now,
		UpdatedAt:   now,
	})
	if !errors.Is(err, ErrBalanceMigrationBlocked) {
		t.Fatalf("CreateBalanceMigrationCode error = %v, want ErrBalanceMigrationBlocked", err)
	}
}
