package postgresrepo

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/32ns/ai-gateway/internal/core"
)

func (r *PostgresRepository) FindUserByOAuthIdentity(provider, subject string) (core.User, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	subject = strings.TrimSpace(subject)
	if provider == "" || subject == "" {
		return core.User{}, ErrNotFound
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	var payload string
	var balance int64
	err := r.db.QueryRow(`
		SELECT u.payload, COALESCE(b.balance_nano_usd, 0)
		FROM user_oauth_identities oi
		JOIN users u ON u.id = oi.user_id
		LEFT JOIN user_balances b ON b.user_id = u.id
		WHERE oi.provider = ? AND oi.subject = ?
	`, provider, subject).Scan(&payload, &balance)
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
	user.BalanceNanoUSD = balance
	return cloneUser(user), nil
}

func (r *PostgresRepository) FindUserByEmail(email string) (core.User, error) {
	email = emailKey(email)
	if email == "" {
		return core.User{}, ErrNotFound
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	user, found, err := r.findUniqueUserByEmailQueryLocked(email)
	if err != nil {
		return core.User{}, err
	}
	if found {
		return user, nil
	}
	return r.findUniqueUserByEmailScanLocked(email)
}

func (r *PostgresRepository) findUniqueUserByEmailQueryLocked(email string) (core.User, bool, error) {
	rows, err := r.db.Query(`
		SELECT u.payload, COALESCE(b.balance_nano_usd, 0)
		FROM users u
		LEFT JOIN user_balances b ON b.user_id = u.id
		WHERE u.email_key = ? OR u.email_key = ''
	`, email)
	if err != nil {
		return core.User{}, false, err
	}
	defer rows.Close()
	return uniqueUserFromRows(rows, email)
}

func (r *PostgresRepository) findUniqueUserByEmailScanLocked(email string) (core.User, error) {
	rows, err := r.db.Query(`
		SELECT u.payload, COALESCE(b.balance_nano_usd, 0)
		FROM users u
		LEFT JOIN user_balances b ON b.user_id = u.id
	`)
	if err != nil {
		return core.User{}, err
	}
	defer rows.Close()
	user, found, err := uniqueUserFromRows(rows, email)
	if err != nil {
		return core.User{}, err
	}
	if !found {
		return core.User{}, ErrNotFound
	}
	return user, nil
}

func uniqueUserFromRows(rows *sql.Rows, email string) (core.User, bool, error) {
	var matched core.User
	count := 0
	for rows.Next() {
		var payload string
		var balance int64
		if err := rows.Scan(&payload, &balance); err != nil {
			return core.User{}, false, err
		}
		var user core.User
		if err := json.Unmarshal([]byte(payload), &user); err != nil {
			return core.User{}, false, err
		}
		if emailKey(user.Email) != email {
			continue
		}
		user.BalanceNanoUSD = balance
		matched = cloneUser(user)
		count++
	}
	if err := rows.Err(); err != nil {
		return core.User{}, false, err
	}
	if count != 1 {
		return core.User{}, false, nil
	}
	return matched, true, nil
}

func (r *PostgresRepository) FindUserByInvitationSignature(signature string) (core.User, error) {
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return core.User{}, ErrNotFound
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	var payload string
	var balance int64
	err := r.db.QueryRow(`
		SELECT u.payload, COALESCE(b.balance_nano_usd, 0)
		FROM user_invitation_codes ic
		JOIN users u ON u.id = ic.user_id
		LEFT JOIN user_balances b ON b.user_id = u.id
		WHERE ic.signature = ?
	`, signature).Scan(&payload, &balance)
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
	user.BalanceNanoUSD = balance
	return cloneUser(user), nil
}

func (r *PostgresRepository) ListUsersByInviter(inviterID string) []core.User {
	inviterID = strings.TrimSpace(inviterID)
	if inviterID == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`
		SELECT u.payload, COALESCE(b.balance_nano_usd, 0)
		FROM users u
		LEFT JOIN user_balances b ON b.user_id = u.id
		WHERE u.inviter_user_id = ?
	`, inviterID)
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

func (r *PostgresRepository) CountUsersByInviter(inviterID string) int {
	inviterID = strings.TrimSpace(inviterID)
	if inviterID == "" {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	var count int
	_ = r.db.QueryRow(`SELECT COUNT(*) FROM users WHERE inviter_user_id = ?`, inviterID).Scan(&count)
	return count
}

func (r *PostgresRepository) CountEnabledAdminsExcluding(excludedIDs []string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	query := `SELECT COUNT(*) FROM users WHERE role = ? AND enabled`
	args := []any{string(core.UserRoleAdmin)}
	ids := normalizedNonEmptyStrings(excludedIDs)
	if len(ids) > 0 {
		query += ` AND id NOT IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	var count int
	_ = r.db.QueryRow(query, args...).Scan(&count)
	return count
}

func (r *PostgresRepository) ListClientsByOwner(ownerUserID string) []core.APIClient {
	ownerUserID = strings.TrimSpace(ownerUserID)
	if ownerUserID == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`SELECT payload FROM clients WHERE owner_user_id = ?`, ownerUserID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]core.APIClient, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		client, err := r.decodeClientPayload(payload)
		if err != nil {
			continue
		}
		out = append(out, client)
	}
	return sortClients(out)
}

func normalizedNonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
