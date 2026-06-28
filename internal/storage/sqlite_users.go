package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func (r *SQLiteRepository) ListUsers() []core.User {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`
		SELECT u.payload, COALESCE(b.balance_nano_usd, 0)
		FROM users u
		LEFT JOIN user_balances b ON b.user_id = u.id
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]core.User, 0)
	for rows.Next() {
		var payload string
		var balance int64
		if err := rows.Scan(&payload, &balance); err != nil {
			continue
		}
		var user core.User
		if err := json.Unmarshal([]byte(payload), &user); err != nil {
			continue
		}
		user.BalanceNanoUSD = balance
		out = append(out, cloneUser(user))
	}
	return sortUsers(out)
}

func (r *SQLiteRepository) GetUser(id string) (core.User, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	payload, err := r.getPayloadByID("users", id)
	if err != nil {
		return core.User{}, err
	}
	var user core.User
	if err := json.Unmarshal([]byte(payload), &user); err != nil {
		return core.User{}, err
	}
	user.BalanceNanoUSD, _ = r.userBalanceLocked(user.ID)
	return cloneUser(user), nil
}

func (r *SQLiteRepository) FindUserByUsername(username string) (core.User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return core.User{}, ErrNotFound
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	var payload string
	err := r.db.QueryRow(`SELECT payload FROM users WHERE username_key = ?`, usernameKey(username)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return core.User{}, ErrNotFound
	}
	if err != nil {
		return core.User{}, err
	}
	var user core.User
	if err := json.Unmarshal([]byte(payload), &user); err != nil {
		return core.User{}, err
	}
	user.BalanceNanoUSD, _ = r.userBalanceLocked(user.ID)
	return cloneUser(user), nil
}

func (r *SQLiteRepository) UpsertUser(user core.User) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	exists, err := queryExistsTx(tx, `SELECT 1 FROM users WHERE id = ? LIMIT 1`, user.ID)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if !exists && user.BalanceNanoUSD < 0 {
		_ = tx.Rollback()
		return ErrInsufficientBalance
	}
	now := time.Now().UTC()
	if exists {
		var createdAtNS int64
		if err := tx.QueryRow(`SELECT created_at_ns FROM users WHERE id = ?`, user.ID).Scan(&createdAtNS); err != nil {
			_ = tx.Rollback()
			return err
		}
		user.CreatedAt = timeFromNS(createdAtNS)
		if user.CreatedAt.IsZero() {
			user.CreatedAt = now
		}
		balance, err := userBalanceTx(tx, user.ID)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		user.BalanceNanoUSD = balance
	} else if user.CreatedAt.IsZero() {
		user.CreatedAt = now
	}
	user.UpdatedAt = now
	if err := upsertUserMetadataTx(tx, user); err != nil {
		_ = tx.Rollback()
		return err
	}
	if !exists {
		if err := upsertUserBalanceTx(tx, user.ID, user.BalanceNanoUSD, now); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := upsertFinanceUserRollupIdentityTx(tx, user); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := syncUserIdentityIndexesTx(tx, user); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (r *SQLiteRepository) UpdateUserMetadata(user core.User) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	user.ID = strings.TrimSpace(user.ID)
	if user.ID == "" {
		return ErrNotFound
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	var createdAtNS int64
	if err := tx.QueryRow(`SELECT created_at_ns FROM users WHERE id = ?`, user.ID).Scan(&createdAtNS); err != nil {
		_ = tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if user.CreatedAt.IsZero() {
		user.CreatedAt = timeFromNS(createdAtNS)
	}
	balance, err := userBalanceTx(tx, user.ID)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	user.BalanceNanoUSD = balance
	user.UpdatedAt = time.Now().UTC()
	if err := upsertUserMetadataTx(tx, user); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := upsertFinanceUserRollupIdentityTx(tx, user); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := syncUserIdentityIndexesTx(tx, user); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func upsertUserMetadataTx(tx *sql.Tx, user core.User) error {
	payload, err := json.Marshal(cloneUser(user))
	if err != nil {
		return err
	}
	lastLoginAtNS := int64(0)
	if user.LastLoginAt != nil {
		lastLoginAtNS = sqliteTimeNS(*user.LastLoginAt)
	}
	enabled := 0
	if user.Enabled {
		enabled = 1
	}
	_, err = tx.Exec(
		`INSERT INTO users(id, username_key, username, role, enabled, inviter_user_id, created_at_ns, updated_at_ns, last_login_at_ns, payload)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			username_key = excluded.username_key,
			username = excluded.username,
			role = excluded.role,
			enabled = excluded.enabled,
			inviter_user_id = excluded.inviter_user_id,
			created_at_ns = excluded.created_at_ns,
			updated_at_ns = excluded.updated_at_ns,
			last_login_at_ns = excluded.last_login_at_ns,
			payload = excluded.payload`,
		user.ID,
		usernameKey(user.Username),
		strings.TrimSpace(user.Username),
		string(user.Role),
		enabled,
		strings.TrimSpace(user.InviterUserID),
		sqliteTimeNS(user.CreatedAt),
		sqliteTimeNS(user.UpdatedAt),
		lastLoginAtNS,
		string(payload),
	)
	return err
}

func userBalanceTx(tx *sql.Tx, userID string) (int64, error) {
	var balance int64
	err := tx.QueryRow(`SELECT balance_nano_usd FROM user_balances WHERE user_id = ?`, userID).Scan(&balance)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return balance, err
}

func upsertUserBalanceTx(tx *sql.Tx, userID string, balanceNanoUSD int64, updatedAt time.Time) error {
	if _, err := tx.Exec(
		`INSERT INTO user_balances(user_id, balance_nano_usd, updated_at_ns)
		VALUES(?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			balance_nano_usd = excluded.balance_nano_usd,
			updated_at_ns = excluded.updated_at_ns`,
		userID,
		balanceNanoUSD,
		sqliteTimeNS(updatedAt),
	); err != nil {
		return err
	}
	return nil
}

func (r *SQLiteRepository) TouchUserLastUsedAt(userID string, usedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ErrNotFound
	}
	if usedAt.IsZero() {
		usedAt = time.Now().UTC()
	} else {
		usedAt = usedAt.UTC()
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	var payload string
	var lastLoginAtNS int64
	if err := tx.QueryRow(`SELECT payload, last_login_at_ns FROM users WHERE id = ?`, userID).Scan(&payload, &lastLoginAtNS); err != nil {
		_ = tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	current := timeFromNS(lastLoginAtNS)
	if !current.IsZero() && !usedAt.After(current) {
		_ = tx.Rollback()
		return nil
	}
	var user core.User
	if err := json.Unmarshal([]byte(payload), &user); err != nil {
		_ = tx.Rollback()
		return err
	}
	if balance, err := userBalanceTx(tx, userID); err != nil {
		_ = tx.Rollback()
		return err
	} else {
		user.BalanceNanoUSD = balance
	}
	user.LastLoginAt = &usedAt
	nextPayload, err := json.Marshal(cloneUser(user))
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	result, err := tx.Exec(`UPDATE users SET last_login_at_ns = ?, payload = ? WHERE id = ?`, sqliteTimeNS(usedAt), string(nextPayload), userID)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		_ = tx.Rollback()
		return ErrNotFound
	}
	return tx.Commit()
}

func (r *SQLiteRepository) MergeUsers(source core.User, target core.User) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	source.ID = strings.TrimSpace(source.ID)
	target.ID = strings.TrimSpace(target.ID)
	if source.ID == "" || target.ID == "" || source.ID == target.ID {
		return fmt.Errorf("source and target users are required")
	}
	if source.BalanceNanoUSD < 0 || target.BalanceNanoUSD < 0 {
		return ErrInsufficientBalance
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	if exists, err := queryExistsTx(tx, `SELECT 1 FROM users WHERE id = ? LIMIT 1`, source.ID); err != nil {
		_ = tx.Rollback()
		return err
	} else if !exists {
		_ = tx.Rollback()
		return ErrNotFound
	}
	if exists, err := queryExistsTx(tx, `SELECT 1 FROM users WHERE id = ? LIMIT 1`, target.ID); err != nil {
		_ = tx.Rollback()
		return err
	} else if !exists {
		_ = tx.Rollback()
		return ErrNotFound
	}
	var previousSourceBalance, previousTargetBalance int64
	if err := tx.QueryRow(`SELECT balance_nano_usd FROM user_balances WHERE user_id = ?`, source.ID).Scan(&previousSourceBalance); err != nil && !errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return err
	}
	if err := tx.QueryRow(`SELECT balance_nano_usd FROM user_balances WHERE user_id = ?`, target.ID).Scan(&previousTargetBalance); err != nil && !errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return err
	}
	now := time.Now().UTC()
	if err := mergeUserPlanEntitlementsTx(tx, source.ID, target.ID, now); err != nil {
		_ = tx.Rollback()
		return err
	}
	source.UpdatedAt = now
	target.UpdatedAt = now
	if err := upsertUserRecordTx(tx, source, now); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := upsertUserRecordTx(tx, target, now); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM user_sessions WHERE user_id = ?`, source.ID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := r.mergeUserClientsTx(tx, source.ID, target.ID, now); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := r.mergeUserInvitersTx(tx, source.ID, target.ID, now); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := r.mergeUserSiteMessagesTx(tx, source.ID, target.ID, now); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := mergeUserSiteMessageReadsTx(tx, source.ID, target.ID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if previousSourceBalance > 0 && target.BalanceNanoUSD != previousTargetBalance {
		if err := insertBillingLedgerTx(tx, core.BillingLedgerEntry{
			ID:                  billingLedgerID("account_merge", source.ID, target.ID, now),
			UserID:              target.ID,
			Kind:                "account_merge",
			AmountNanoUSD:       previousSourceBalance,
			BalanceAfterNanoUSD: target.BalanceNanoUSD,
			Note:                source.Username,
			CreatedAt:           now,
		}); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := addFinanceUserRewardRollupTx(tx, target.ID, previousSourceBalance); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (r *SQLiteRepository) SetUserBalance(userID string, balanceNanoUSD int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if balanceNanoUSD < 0 {
		return ErrInsufficientBalance
	}
	if _, err := r.getPayloadByID("users", userID); err != nil {
		return err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO user_balances(user_id, balance_nano_usd, updated_at_ns)
		VALUES(?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			balance_nano_usd = excluded.balance_nano_usd,
			updated_at_ns = excluded.updated_at_ns`,
		userID,
		balanceNanoUSD,
		time.Now().UTC().UnixNano(),
	); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := setFinanceUserRollupBalanceTx(tx, userID, balanceNanoUSD); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (r *SQLiteRepository) AdjustUserBalance(userID string, deltaNanoUSD int64, note string) (int64, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	tx, err := r.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	var existingUserID string
	if err := tx.QueryRow(`SELECT id FROM users WHERE id = ?`, userID).Scan(&existingUserID); err != nil {
		_ = tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, ErrNotFound
		}
		return 0, 0, err
	}
	var previousBalance int64
	if err := tx.QueryRow(`SELECT balance_nano_usd FROM user_balances WHERE user_id = ?`, userID).Scan(&previousBalance); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			_ = tx.Rollback()
			return 0, 0, err
		}
		previousBalance = 0
	}
	nextBalance, err := addNanoUSD(previousBalance, deltaNanoUSD)
	if err != nil {
		_ = tx.Rollback()
		return previousBalance, previousBalance, err
	}
	if nextBalance < 0 {
		_ = tx.Rollback()
		return previousBalance, previousBalance, ErrInsufficientBalance
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(
		`INSERT INTO user_balances(user_id, balance_nano_usd, updated_at_ns)
		VALUES(?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			balance_nano_usd = excluded.balance_nano_usd,
			updated_at_ns = excluded.updated_at_ns`,
		userID,
		nextBalance,
		now.UnixNano(),
	); err != nil {
		_ = tx.Rollback()
		return 0, 0, err
	}
	kind := "manual_credit"
	if deltaNanoUSD < 0 {
		kind = "manual_debit"
	}
	if err := insertBillingLedgerTx(tx, core.BillingLedgerEntry{
		ID:                  billingLedgerID(kind, "manual", userID, now),
		UserID:              userID,
		Kind:                kind,
		AmountNanoUSD:       deltaNanoUSD,
		BalanceAfterNanoUSD: nextBalance,
		Note:                strings.TrimSpace(note),
		CreatedAt:           now,
	}); err != nil {
		_ = tx.Rollback()
		return 0, 0, err
	}
	if err := setFinanceUserRollupBalanceTx(tx, userID, nextBalance); err != nil {
		_ = tx.Rollback()
		return 0, 0, err
	}
	if deltaNanoUSD > 0 {
		if err := addFinanceUserRewardRollupTx(tx, userID, deltaNanoUSD); err != nil {
			_ = tx.Rollback()
			return 0, 0, err
		}
	} else if deltaNanoUSD < 0 {
		if err := addFinanceUserSpendRollupTx(tx, userID, -deltaNanoUSD, now); err != nil {
			_ = tx.Rollback()
			return 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return previousBalance, nextBalance, nil
}

func (r *SQLiteRepository) DeleteUser(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	payload, err := r.getPayloadByIDTx(tx, "users", id)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	var user core.User
	if err := json.Unmarshal([]byte(payload), &user); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := r.deleteUserDataTx(tx, id, emailKey(user.Email), nil, now); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := markFinanceUserRollupDeletedTx(tx, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	result, err := tx.Exec(`DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if affected == 0 {
		_ = tx.Rollback()
		return ErrNotFound
	}
	return tx.Commit()
}

func (r *SQLiteRepository) deleteUserDataTx(tx *sql.Tx, userID, email string, clientIDs []string, now time.Time) error {
	if err := r.releaseReservedBillingForDeletedDataTx(tx, userID, clientIDs, now); err != nil {
		return err
	}
	if err := cancelUserPlanEntitlementsTx(tx, userID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM user_sessions WHERE user_id = ?`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM site_message_reads WHERE user_id = ?`, userID); err != nil {
		return err
	}
	if err := r.removeUserSiteMessagesTx(tx, userID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM user_balances WHERE user_id = ?`, userID); err != nil {
		return err
	}
	if strings.TrimSpace(email) != "" {
		if _, err := tx.Exec(`DELETE FROM email_verification_codes WHERE email_key = ?`, email); err != nil {
			return err
		}
	}
	if strings.TrimSpace(userID) != "" {
		if _, err := tx.Exec(`DELETE FROM openai_response_bindings WHERE client_id IN (SELECT id FROM clients WHERE owner_user_id = ?)`, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM client_spend WHERE client_id IN (SELECT id FROM clients WHERE owner_user_id = ?)`, userID); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM clients WHERE owner_user_id = ?`, userID)
		return err
	}
	for _, clientID := range clientIDs {
		if err := r.deleteClientDataTx(tx, clientID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM clients WHERE id = ?`, clientID); err != nil {
			return err
		}
	}
	return nil
}

func (r *SQLiteRepository) deleteClientDataTx(tx *sql.Tx, clientID string) error {
	if err := r.releaseReservedBillingForDeletedDataTx(tx, "", []string{clientID}, time.Now().UTC()); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM openai_response_bindings WHERE client_id = ?`, clientID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM client_spend WHERE client_id = ?`, clientID); err != nil {
		return err
	}
	return nil
}

func (r *SQLiteRepository) releaseReservedBillingForDeletedDataTx(tx *sql.Tx, userID string, clientIDs []string, now time.Time) error {
	userID = strings.TrimSpace(userID)
	clientSet := make(map[string]struct{}, len(clientIDs))
	filterClientIDs := make([]string, 0, len(clientIDs))
	for _, clientID := range clientIDs {
		clientID = strings.TrimSpace(clientID)
		if clientID != "" {
			if _, ok := clientSet[clientID]; !ok {
				clientSet[clientID] = struct{}{}
				filterClientIDs = append(filterClientIDs, clientID)
			}
		}
	}
	if userID == "" && len(filterClientIDs) == 0 {
		return nil
	}

	matchParts := make([]string, 0, 2)
	args := []any{string(core.BillingRequestReleased), sqliteTimeNS(now), string(core.BillingRequestReserved)}
	if userID != "" {
		matchParts = append(matchParts, `user_id = ?`)
		args = append(args, userID)
	}
	if userID != "" {
		matchParts = append(matchParts, `client_id IN (SELECT id FROM clients WHERE owner_user_id = ?)`)
		args = append(args, userID)
	} else if len(filterClientIDs) > 0 {
		matchParts = append(matchParts, `client_id IN (`+placeholders(len(filterClientIDs))+`)`)
		for _, clientID := range filterClientIDs {
			args = append(args, clientID)
		}
	}

	query := `
		UPDATE billing_requests
		SET status = ?, settled_at_ns = ?
		WHERE status = ? AND (` + strings.Join(matchParts, ` OR `) + `)`
	_, err := tx.Exec(query, args...)
	return err
}

func (r *SQLiteRepository) UpsertUserSession(session core.UserSession) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	session.UpdatedAt = now
	payload, err := json.Marshal(cloneUserSession(session))
	if err != nil {
		return err
	}
	_, err = r.db.Exec(
		`INSERT INTO user_sessions(token_hash, user_id, expires_at_ns, payload)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(token_hash) DO UPDATE SET
			user_id = excluded.user_id,
			expires_at_ns = excluded.expires_at_ns,
			payload = excluded.payload`,
		session.TokenHash,
		session.UserID,
		sqliteTimeNS(session.ExpiresAt),
		string(payload),
	)
	return err
}

func (r *SQLiteRepository) GetUserSession(tokenHash string) (core.UserSession, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var payload string
	err := r.db.QueryRow(`SELECT payload FROM user_sessions WHERE token_hash = ?`, tokenHash).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return core.UserSession{}, ErrNotFound
	}
	if err != nil {
		return core.UserSession{}, err
	}
	var session core.UserSession
	if err := json.Unmarshal([]byte(payload), &session); err != nil {
		return core.UserSession{}, err
	}
	return cloneUserSession(session), nil
}

func (r *SQLiteRepository) DeleteUserSession(tokenHash string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	result, err := r.db.Exec(`DELETE FROM user_sessions WHERE token_hash = ?`, tokenHash)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *SQLiteRepository) DeleteExpiredUserSessions(now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, err := r.db.Exec(`DELETE FROM user_sessions WHERE expires_at_ns <= ?`, sqliteTimeNS(now.UTC()))
	return err
}

func (r *SQLiteRepository) CreateEmailVerificationCode(code core.EmailVerificationCode) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	code.ID = strings.TrimSpace(code.ID)
	code.Purpose = strings.TrimSpace(code.Purpose)
	code.Email = strings.ToLower(strings.TrimSpace(code.Email))
	if code.ID == "" || code.Purpose == "" || code.Email == "" {
		return fmt.Errorf("email verification code is incomplete")
	}
	if code.CreatedAt.IsZero() {
		code.CreatedAt = now
	}
	code.UpdatedAt = now
	payload, err := json.Marshal(cloneEmailVerificationCode(code))
	if err != nil {
		return err
	}
	_, err = r.db.Exec(
		`INSERT INTO email_verification_codes(id, purpose, email_key, created_at_ns, payload) VALUES(?, ?, ?, ?, ?)`,
		code.ID,
		code.Purpose,
		emailKey(code.Email),
		sqliteTimeNS(code.CreatedAt),
		string(payload),
	)
	return err
}

func (r *SQLiteRepository) LatestEmailVerificationCode(purpose, email string) (core.EmailVerificationCode, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var payload string
	err := r.db.QueryRow(
		`SELECT payload FROM email_verification_codes WHERE purpose = ? AND email_key = ? ORDER BY created_at_ns DESC LIMIT 1`,
		strings.TrimSpace(purpose),
		emailKey(email),
	).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return core.EmailVerificationCode{}, ErrNotFound
	}
	if err != nil {
		return core.EmailVerificationCode{}, err
	}
	var code core.EmailVerificationCode
	if err := json.Unmarshal([]byte(payload), &code); err != nil {
		return core.EmailVerificationCode{}, err
	}
	return cloneEmailVerificationCode(code), nil
}

func (r *SQLiteRepository) CountEmailVerificationCodesSince(purpose, email string, since time.Time) int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var count int
	if err := r.db.QueryRow(
		`SELECT COUNT(*) FROM email_verification_codes WHERE purpose = ? AND email_key = ? AND created_at_ns >= ?`,
		strings.TrimSpace(purpose),
		emailKey(email),
		sqliteTimeNS(since),
	).Scan(&count); err != nil {
		return 0
	}
	return count
}

func (r *SQLiteRepository) UpdateEmailVerificationCode(code core.EmailVerificationCode) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	code.ID = strings.TrimSpace(code.ID)
	if code.ID == "" {
		return fmt.Errorf("email verification code id is required")
	}
	code.Email = strings.ToLower(strings.TrimSpace(code.Email))
	code.UpdatedAt = time.Now().UTC()
	payload, err := json.Marshal(cloneEmailVerificationCode(code))
	if err != nil {
		return err
	}
	result, err := r.db.Exec(
		`UPDATE email_verification_codes SET purpose = ?, email_key = ?, created_at_ns = ?, payload = ? WHERE id = ?`,
		code.Purpose,
		emailKey(code.Email),
		sqliteTimeNS(code.CreatedAt),
		string(payload),
		code.ID,
	)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *SQLiteRepository) DeleteEmailVerificationCode(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("email verification code id is required")
	}
	result, err := r.db.Exec(`DELETE FROM email_verification_codes WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return ErrNotFound
	}
	return nil
}
