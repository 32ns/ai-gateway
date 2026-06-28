package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/32ns/ai-gateway/internal/core"
)

func (r *SQLiteRepository) FindUserByOAuthIdentity(provider, subject string) (core.User, error) {
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

func (r *SQLiteRepository) FindUserByInvitationSignature(signature string) (core.User, error) {
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

func (r *SQLiteRepository) ListUsersByInviter(inviterID string) []core.User {
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

func (r *SQLiteRepository) CountUsersByInviter(inviterID string) int {
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

func (r *SQLiteRepository) CountEnabledAdminsExcluding(excludedIDs []string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	query := `SELECT COUNT(*) FROM users WHERE role = ? AND enabled <> 0`
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

func (r *SQLiteRepository) ListClientsByOwner(ownerUserID string) []core.APIClient {
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

func (r *MemoryRepository) FindUserByOAuthIdentity(provider, subject string) (core.User, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	subject = strings.TrimSpace(subject)
	if provider == "" || subject == "" {
		return core.User{}, ErrNotFound
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, user := range r.users {
		for _, identity := range user.OAuthIdentities {
			if strings.EqualFold(identity.Provider, provider) && strings.TrimSpace(identity.Subject) == subject {
				return cloneUser(user), nil
			}
		}
	}
	return core.User{}, ErrNotFound
}

func (r *MemoryRepository) FindUserByInvitationSignature(signature string) (core.User, error) {
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return core.User{}, ErrNotFound
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, user := range r.users {
		if core.UserInvitationSignature(user) == signature {
			return cloneUser(user), nil
		}
	}
	return core.User{}, ErrNotFound
}

func (r *MemoryRepository) ListUsersByInviter(inviterID string) []core.User {
	inviterID = strings.TrimSpace(inviterID)
	if inviterID == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]core.User, 0)
	for _, user := range r.users {
		if strings.TrimSpace(user.InviterUserID) == inviterID {
			out = append(out, cloneUser(user))
		}
	}
	return sortUsers(out)
}

func (r *MemoryRepository) CountUsersByInviter(inviterID string) int {
	inviterID = strings.TrimSpace(inviterID)
	if inviterID == "" {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	count := 0
	for _, user := range r.users {
		if strings.TrimSpace(user.InviterUserID) == inviterID {
			count++
		}
	}
	return count
}

func (r *MemoryRepository) CountEnabledAdminsExcluding(excludedIDs []string) int {
	excluded := stringSet(excludedIDs)
	r.mu.RLock()
	defer r.mu.RUnlock()

	count := 0
	for _, user := range r.users {
		if _, skip := excluded[strings.TrimSpace(user.ID)]; skip {
			continue
		}
		if user.Role == core.UserRoleAdmin && user.Enabled {
			count++
		}
	}
	return count
}

func (r *MemoryRepository) ListClientsByOwner(ownerUserID string) []core.APIClient {
	ownerUserID = strings.TrimSpace(ownerUserID)
	if ownerUserID == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]core.APIClient, 0)
	for _, client := range r.clients {
		if strings.TrimSpace(client.OwnerUserID) == ownerUserID {
			out = append(out, cloneClient(client))
		}
	}
	return sortClients(out)
}

func (r *MemoryRepository) ListClientSummariesByOwnerPage(ownerUserID string, offset, limit int) ([]core.APIClient, int) {
	clients := r.ListClientsByOwner(ownerUserID)
	return clientSummaryPage(clients, offset, limit)
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

func stringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range normalizedNonEmptyStrings(values) {
		out[value] = struct{}{}
	}
	return out
}
