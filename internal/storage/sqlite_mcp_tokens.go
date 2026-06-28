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

func (r *SQLiteRepository) UpsertMCPToken(token core.MCPToken) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	token.ID = strings.TrimSpace(token.ID)
	token.TokenHash = strings.TrimSpace(token.TokenHash)
	token.OwnerUserID = strings.TrimSpace(token.OwnerUserID)
	if token.ID == "" || token.TokenHash == "" || token.OwnerUserID == "" {
		return fmt.Errorf("mcp token id, hash, and owner are required")
	}
	now := time.Now().UTC()
	if token.CreatedAt.IsZero() {
		token.CreatedAt = now
	}
	token.UpdatedAt = now
	payload, err := json.Marshal(cloneMCPToken(token))
	if err != nil {
		return err
	}
	_, err = r.db.Exec(
		`INSERT INTO mcp_tokens(id, token_hash, owner_user_id, enabled, expires_at_ns, last_used_at_ns, revoked_at_ns, created_at_ns, updated_at_ns, payload)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			token_hash = excluded.token_hash,
			owner_user_id = excluded.owner_user_id,
			enabled = excluded.enabled,
			expires_at_ns = excluded.expires_at_ns,
			last_used_at_ns = excluded.last_used_at_ns,
			revoked_at_ns = excluded.revoked_at_ns,
			updated_at_ns = excluded.updated_at_ns,
			payload = excluded.payload`,
		token.ID,
		token.TokenHash,
		token.OwnerUserID,
		boolInt(token.Enabled),
		sqliteTimeNS(valueTime(token.ExpiresAt)),
		sqliteTimeNS(valueTime(token.LastUsedAt)),
		sqliteTimeNS(valueTime(token.RevokedAt)),
		sqliteTimeNS(token.CreatedAt),
		sqliteTimeNS(token.UpdatedAt),
		string(payload),
	)
	return err
}

func (r *SQLiteRepository) GetMCPToken(id string) (core.MCPToken, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.getMCPTokenByColumnLocked("id", strings.TrimSpace(id))
}

func (r *SQLiteRepository) GetMCPTokenByHash(tokenHash string) (core.MCPToken, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.getMCPTokenByColumnLocked("token_hash", strings.TrimSpace(tokenHash))
}

func (r *SQLiteRepository) ListMCPTokens(ownerUserID string) []core.MCPToken {
	r.mu.RLock()
	defer r.mu.RUnlock()

	query := `SELECT payload FROM mcp_tokens ORDER BY created_at_ns DESC, id ASC`
	args := []any{}
	if strings.TrimSpace(ownerUserID) != "" {
		query = `SELECT payload FROM mcp_tokens WHERE owner_user_id = ? ORDER BY created_at_ns DESC, id ASC`
		args = append(args, strings.TrimSpace(ownerUserID))
	}
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]core.MCPToken, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		token, err := decodeMCPTokenPayload(payload)
		if err != nil {
			continue
		}
		out = append(out, token)
	}
	return out
}

func (r *SQLiteRepository) DeleteMCPToken(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = strings.TrimSpace(id)
	if id == "" {
		return ErrNotFound
	}
	result, err := r.db.Exec(`DELETE FROM mcp_tokens WHERE id = ?`, id)
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

func (r *SQLiteRepository) getMCPTokenByColumnLocked(column, value string) (core.MCPToken, error) {
	if value == "" {
		return core.MCPToken{}, ErrNotFound
	}
	var payload string
	err := r.db.QueryRow(fmt.Sprintf(`SELECT payload FROM mcp_tokens WHERE %s = ?`, column), value).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return core.MCPToken{}, ErrNotFound
	}
	if err != nil {
		return core.MCPToken{}, err
	}
	return decodeMCPTokenPayload(payload)
}

func decodeMCPTokenPayload(payload string) (core.MCPToken, error) {
	var token core.MCPToken
	if err := json.Unmarshal([]byte(payload), &token); err != nil {
		return core.MCPToken{}, err
	}
	return cloneMCPToken(token), nil
}
