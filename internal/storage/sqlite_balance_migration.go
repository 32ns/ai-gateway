package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func (r *SQLiteRepository) CreateBalanceMigrationCode(code core.BalanceMigrationCode) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	user, err := r.balanceMigrationUserTx(tx, code.UserID)
	if err != nil {
		return err
	}
	if !user.Enabled || user.IsAdmin() {
		return ErrBalanceMigrationInvalid
	}
	if err := ensureBalanceMigrationCanStartTx(tx, user.ID); err != nil {
		return err
	}
	var balance int64
	if err := tx.QueryRow(`SELECT balance_nano_usd FROM user_balances WHERE user_id = ?`, user.ID).Scan(&balance); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrBalanceMigrationNoBalance
		}
		return err
	}
	if balance <= 0 {
		return ErrBalanceMigrationNoBalance
	}

	var existingStatus string
	err = tx.QueryRow(`SELECT status FROM user_balance_migration_codes WHERE user_id = ?`, user.ID).Scan(&existingStatus)
	if err == nil && existingStatus == string(core.BalanceMigrationClaimed) {
		return ErrBalanceMigrationClaimed
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO user_balance_migration_codes
			(id, user_id, code_hash, target_user_id, status, amount_nano_usd,
			 expires_at_ns, generated_at_ns, claimed_at_ns, updated_at_ns)
		 VALUES (?, ?, ?, '', ?, 0, ?, ?, 0, ?)
		 ON CONFLICT(user_id) DO UPDATE SET
			id = excluded.id,
			code_hash = excluded.code_hash,
			target_user_id = '',
			status = excluded.status,
			amount_nano_usd = 0,
			expires_at_ns = excluded.expires_at_ns,
			generated_at_ns = excluded.generated_at_ns,
			claimed_at_ns = 0,
			updated_at_ns = excluded.updated_at_ns`,
		code.ID,
		user.ID,
		code.CodeHash,
		string(core.BalanceMigrationPending),
		sqliteTimeNS(code.ExpiresAt),
		sqliteTimeNS(code.GeneratedAt),
		sqliteTimeNS(code.UpdatedAt),
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *SQLiteRepository) ClaimBalanceMigrationCode(codeHash, targetUserID string) (core.BalanceMigrationCode, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	targetUserID = normalizeBalanceMigrationTarget(targetUserID)
	if targetUserID == "" {
		return core.BalanceMigrationCode{}, ErrBalanceMigrationInvalid
	}
	tx, err := r.db.Begin()
	if err != nil {
		return core.BalanceMigrationCode{}, err
	}
	defer tx.Rollback()

	code, err := loadBalanceMigrationCodeTx(tx, codeHash)
	if err != nil {
		return core.BalanceMigrationCode{}, err
	}
	if code.Status == core.BalanceMigrationClaimed {
		if code.TargetUserID != targetUserID {
			return core.BalanceMigrationCode{}, ErrBalanceMigrationTargetMismatch
		}
		return code, tx.Commit()
	}
	if code.TargetUserID != "" && code.TargetUserID != targetUserID {
		return core.BalanceMigrationCode{}, ErrBalanceMigrationTargetMismatch
	}
	if code.Status == core.BalanceMigrationPending && !code.ExpiresAt.After(time.Now().UTC()) {
		return core.BalanceMigrationCode{}, ErrBalanceMigrationExpired
	}
	if err := ensureBalanceMigrationCanStartTx(tx, code.UserID); err != nil {
		return core.BalanceMigrationCode{}, err
	}

	user, err := r.balanceMigrationUserTx(tx, code.UserID)
	if err != nil {
		return core.BalanceMigrationCode{}, err
	}
	if user.IsAdmin() {
		return core.BalanceMigrationCode{}, ErrBalanceMigrationInvalid
	}
	now := time.Now().UTC()
	user.Enabled = false
	user.UpdatedAt = now
	if err := upsertUserRecordTx(tx, user, now); err != nil {
		return core.BalanceMigrationCode{}, err
	}
	if _, err := tx.Exec(`DELETE FROM user_sessions WHERE user_id = ?`, user.ID); err != nil {
		return core.BalanceMigrationCode{}, err
	}
	code.TargetUserID = targetUserID
	code.Status = core.BalanceMigrationDraining
	code.UpdatedAt = now

	var activeRequests int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM billing_requests WHERE user_id = ? AND status = ?`,
		user.ID,
		string(core.BillingRequestReserved),
	).Scan(&activeRequests); err != nil {
		return core.BalanceMigrationCode{}, err
	}
	if activeRequests > 0 {
		if err := saveBalanceMigrationCodeTx(tx, code); err != nil {
			return core.BalanceMigrationCode{}, err
		}
		if err := tx.Commit(); err != nil {
			return core.BalanceMigrationCode{}, err
		}
		return code, ErrBalanceMigrationDraining
	}

	var balance int64
	if err := tx.QueryRow(`SELECT balance_nano_usd FROM user_balances WHERE user_id = ?`, user.ID).Scan(&balance); err != nil {
		return core.BalanceMigrationCode{}, err
	}
	if balance <= 0 {
		user.Enabled = true
		user.UpdatedAt = now
		if err := upsertUserRecordTx(tx, user, now); err != nil {
			return core.BalanceMigrationCode{}, err
		}
		code.TargetUserID = ""
		code.Status = core.BalanceMigrationPending
		code.UpdatedAt = now
		if err := saveBalanceMigrationCodeTx(tx, code); err != nil {
			return core.BalanceMigrationCode{}, err
		}
		if err := tx.Commit(); err != nil {
			return core.BalanceMigrationCode{}, err
		}
		return core.BalanceMigrationCode{}, ErrBalanceMigrationNoBalance
	}
	if _, err := tx.Exec(
		`UPDATE user_balances SET balance_nano_usd = 0, updated_at_ns = ? WHERE user_id = ?`,
		now.UnixNano(),
		user.ID,
	); err != nil {
		return core.BalanceMigrationCode{}, err
	}
	if err := setFinanceUserRollupBalanceTx(tx, user.ID, 0); err != nil {
		return core.BalanceMigrationCode{}, err
	}
	if err := insertBillingLedgerTx(tx, core.BillingLedgerEntry{
		ID:                  billingLedgerID("balance_migration", code.ID, user.ID, now),
		UserID:              user.ID,
		Kind:                "balance_migration_out",
		AmountNanoUSD:       -balance,
		BalanceAfterNanoUSD: 0,
		Note:                "Balance migrated to the new site",
		CreatedAt:           now,
	}); err != nil {
		return core.BalanceMigrationCode{}, err
	}
	code.Status = core.BalanceMigrationClaimed
	code.AmountNanoUSD = balance
	code.ClaimedAt = &now
	code.UpdatedAt = now
	if err := saveBalanceMigrationCodeTx(tx, code); err != nil {
		return core.BalanceMigrationCode{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.BalanceMigrationCode{}, err
	}
	return code, nil
}

func (r *SQLiteRepository) balanceMigrationUserTx(tx *sql.Tx, userID string) (core.User, error) {
	payload, err := r.getPayloadByIDTx(tx, "users", userID)
	if err != nil {
		return core.User{}, err
	}
	var user core.User
	if err := json.Unmarshal([]byte(payload), &user); err != nil {
		return core.User{}, err
	}
	return user, nil
}

func ensureBalanceMigrationCanStartTx(tx *sql.Tx, userID string) error {
	var pendingPayments int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM payment_orders WHERE user_id = ? AND status <> 'paid'`,
		userID,
	).Scan(&pendingPayments); err != nil {
		return err
	}
	var pendingRefunds int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM payment_refunds WHERE user_id = ? AND status = 'pending'`,
		userID,
	).Scan(&pendingRefunds); err != nil {
		return err
	}
	if pendingPayments > 0 || pendingRefunds > 0 {
		return ErrBalanceMigrationBlocked
	}
	return nil
}

func loadBalanceMigrationCodeTx(tx *sql.Tx, codeHash string) (core.BalanceMigrationCode, error) {
	var code core.BalanceMigrationCode
	var status string
	var expiresAtNS, generatedAtNS, claimedAtNS, updatedAtNS int64
	err := tx.QueryRow(
		`SELECT id, user_id, code_hash, target_user_id, status, amount_nano_usd,
			expires_at_ns, generated_at_ns, claimed_at_ns, updated_at_ns
		 FROM user_balance_migration_codes WHERE code_hash = ?`,
		strings.TrimSpace(codeHash),
	).Scan(
		&code.ID,
		&code.UserID,
		&code.CodeHash,
		&code.TargetUserID,
		&status,
		&code.AmountNanoUSD,
		&expiresAtNS,
		&generatedAtNS,
		&claimedAtNS,
		&updatedAtNS,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return core.BalanceMigrationCode{}, ErrBalanceMigrationInvalid
	}
	if err != nil {
		return core.BalanceMigrationCode{}, err
	}
	code.Status = core.BalanceMigrationStatus(status)
	code.ExpiresAt = timeFromNS(expiresAtNS)
	code.GeneratedAt = timeFromNS(generatedAtNS)
	code.UpdatedAt = timeFromNS(updatedAtNS)
	if claimedAtNS > 0 {
		claimedAt := timeFromNS(claimedAtNS)
		code.ClaimedAt = &claimedAt
	}
	return code, nil
}

func saveBalanceMigrationCodeTx(tx *sql.Tx, code core.BalanceMigrationCode) error {
	claimedAtNS := int64(0)
	if code.ClaimedAt != nil {
		claimedAtNS = sqliteTimeNS(*code.ClaimedAt)
	}
	_, err := tx.Exec(
		`UPDATE user_balance_migration_codes
		 SET target_user_id = ?, status = ?, amount_nano_usd = ?, claimed_at_ns = ?, updated_at_ns = ?
		 WHERE id = ?`,
		code.TargetUserID,
		string(code.Status),
		code.AmountNanoUSD,
		claimedAtNS,
		sqliteTimeNS(code.UpdatedAt),
		code.ID,
	)
	return err
}

func balanceMigrationClaimedTx(tx *sql.Tx, userID string) (bool, error) {
	var status string
	err := tx.QueryRow(
		`SELECT status FROM user_balance_migration_codes WHERE user_id = ?`,
		userID,
	).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return status == string(core.BalanceMigrationClaimed), nil
}
