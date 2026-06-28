package postgresrepo

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func (r *PostgresRepository) ListAccounts() []core.Account {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`
		SELECT a.payload, c.payload, ar.payload
		FROM accounts a
		LEFT JOIN account_credentials c ON c.account_id = a.id
		LEFT JOIN account_runtime ar ON ar.account_id = a.id
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]core.Account, 0)
	for rows.Next() {
		var configPayload string
		var credentialPayload, runtimePayload sql.NullString
		if err := rows.Scan(&configPayload, &credentialPayload, &runtimePayload); err != nil {
			continue
		}
		account, err := r.decodeStoredAccountPayloads(configPayload, credentialPayload, runtimePayload)
		if err != nil {
			continue
		}
		out = append(out, account)
	}
	return sortAccounts(out)
}

func (r *PostgresRepository) GetAccount(id string) (core.Account, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var configPayload string
	var credentialPayload, runtimePayload sql.NullString
	err := r.db.QueryRow(`
		SELECT a.payload, c.payload, ar.payload
		FROM accounts a
		LEFT JOIN account_credentials c ON c.account_id = a.id
		LEFT JOIN account_runtime ar ON ar.account_id = a.id
		WHERE a.id = ?
	`, id).Scan(&configPayload, &credentialPayload, &runtimePayload)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Account{}, ErrNotFound
	}
	if err != nil {
		return core.Account{}, err
	}
	return r.decodeStoredAccountPayloads(configPayload, credentialPayload, runtimePayload)
}

func (r *PostgresRepository) UpsertAccount(account core.Account) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	account.Group = core.NormalizeAccountGroupName(account.Group)
	now := time.Now().UTC()
	if account.CreatedAt.IsZero() {
		account.CreatedAt = now
	}
	account.UpdatedAt = now

	return r.upsertAccountPartsLocked(account)
}

func (r *PostgresRepository) UpsertRuntimeAccount(account core.Account) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, err := r.getPayloadByID("accounts", account.ID); err != nil {
		return err
	}
	now := time.Now().UTC()
	account.UpdatedAt = now
	_, _, runtimeState := splitAccountForStorage(account)
	runtimeState.UpdatedAt = now
	payload, err := r.encodeAccountRuntimePayload(runtimeState)
	if err != nil {
		return err
	}
	return r.upsertAccountStatePayload("account_runtime", account.ID, payload)
}

func (r *PostgresRepository) DeleteAccount(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, err := r.db.Exec(`DELETE FROM account_credentials WHERE account_id = ?`, id); err != nil {
		return err
	}
	if _, err := r.db.Exec(`DELETE FROM account_runtime WHERE account_id = ?`, id); err != nil {
		return err
	}
	return r.deleteByID("accounts", id)
}

func (r *PostgresRepository) UpdateAccountStatus(id string, status core.AccountStatus) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	account, err := r.accountLocked(id)
	if err != nil {
		return err
	}
	account.Status = status
	account.UpdatedAt = time.Now().UTC()

	_, _, runtimeState := splitAccountForStorage(account)
	payload, err := r.encodeAccountRuntimePayload(runtimeState)
	if err != nil {
		return err
	}
	return r.upsertAccountStatePayload("account_runtime", id, payload)
}

func (r *PostgresRepository) ListAccountGroups() []core.AccountGroup {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`SELECT payload FROM account_groups`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]core.AccountGroup, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		group, err := r.decodeAccountGroupPayload(payload)
		if err != nil {
			continue
		}
		out = append(out, group)
	}
	return sortAccountGroups(out)
}

func (r *PostgresRepository) UpsertAccountGroup(group core.AccountGroup) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	if group.CreatedAt.IsZero() {
		group.CreatedAt = now
	}
	group.UpdatedAt = now

	payload, err := r.encodeAccountGroupPayload(group)
	if err != nil {
		return err
	}
	return r.upsertPayload("account_groups", group.ID, payload)
}

func (r *PostgresRepository) DeleteAccountGroup(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.deleteByID("account_groups", id)
}

func (r *PostgresRepository) ListModels() []core.ModelConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`SELECT payload FROM models`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]core.ModelConfig, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		var model core.ModelConfig
		if err := json.Unmarshal([]byte(payload), &model); err != nil {
			continue
		}
		out = append(out, model)
	}
	return sortModels(out)
}

func (r *PostgresRepository) GetModel(id string) (core.ModelConfig, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	payload, err := r.getPayloadByID("models", id)
	if err != nil {
		return core.ModelConfig{}, err
	}
	var model core.ModelConfig
	if err := json.Unmarshal([]byte(payload), &model); err != nil {
		return core.ModelConfig{}, err
	}
	return model, nil
}

func (r *PostgresRepository) UpsertModel(model core.ModelConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	if model.CreatedAt.IsZero() {
		model.CreatedAt = now
	}
	model.UpdatedAt = now

	payload, err := json.Marshal(cloneModel(model))
	if err != nil {
		return err
	}
	return r.upsertPayload("models", model.ID, string(payload))
}

func (r *PostgresRepository) DeleteModel(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.deleteByID("models", id)
}

func (r *PostgresRepository) ListClients() []core.APIClient {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`SELECT payload FROM clients`)
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

func (r *PostgresRepository) ListClientSummaries() []core.APIClient {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`SELECT id, name, owner_user_id, enabled, payload FROM clients`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]core.APIClient, 0)
	for rows.Next() {
		var id, name, ownerUserID, payload string
		var enabled bool
		if err := rows.Scan(&id, &name, &ownerUserID, &enabled, &payload); err != nil {
			continue
		}
		client, err := clientSummaryFromPayload(id, name, ownerUserID, enabled, payload)
		if err != nil {
			continue
		}
		out = append(out, client)
	}
	return sortClients(out)
}

func (r *PostgresRepository) ListClientSummariesPage(offset, limit int) ([]core.APIClient, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 25
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	total := 0
	_ = r.db.QueryRow(`SELECT COUNT(*) FROM clients`).Scan(&total)
	rows, err := r.db.Query(
		`SELECT id, name, owner_user_id, enabled, payload
		FROM clients
		ORDER BY enabled DESC, name ASC, id ASC
		LIMIT ? OFFSET ?`,
		limit,
		offset,
	)
	if err != nil {
		return nil, total
	}
	defer rows.Close()

	out := make([]core.APIClient, 0, limit)
	for rows.Next() {
		var id, name, ownerUserID, payload string
		var enabled bool
		if err := rows.Scan(&id, &name, &ownerUserID, &enabled, &payload); err != nil {
			continue
		}
		client, err := clientSummaryFromPayload(id, name, ownerUserID, enabled, payload)
		if err != nil {
			continue
		}
		out = append(out, client)
	}
	return out, total
}

func (r *PostgresRepository) ListClientSummariesByOwnerPage(ownerUserID string, offset, limit int) ([]core.APIClient, int) {
	ownerUserID = strings.TrimSpace(ownerUserID)
	if ownerUserID == "" {
		return nil, 0
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 25
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	total := 0
	_ = r.db.QueryRow(`SELECT COUNT(*) FROM clients WHERE owner_user_id = ?`, ownerUserID).Scan(&total)
	rows, err := r.db.Query(
		`SELECT id, name, owner_user_id, enabled, payload
		FROM clients
		WHERE owner_user_id = ?
		ORDER BY enabled DESC, name ASC, id ASC
		LIMIT ? OFFSET ?`,
		ownerUserID,
		limit,
		offset,
	)
	if err != nil {
		return nil, total
	}
	defer rows.Close()

	out := make([]core.APIClient, 0, limit)
	for rows.Next() {
		var id, name, ownerID, payload string
		var enabled bool
		if err := rows.Scan(&id, &name, &ownerID, &enabled, &payload); err != nil {
			continue
		}
		client, err := clientSummaryFromPayload(id, name, ownerID, enabled, payload)
		if err != nil {
			continue
		}
		out = append(out, client)
	}
	return out, total
}

func clientSummaryFromPayload(id, name, ownerUserID string, enabled bool, payload string) (core.APIClient, error) {
	var client core.APIClient
	if err := json.Unmarshal([]byte(payload), &client); err != nil {
		return core.APIClient{}, err
	}
	if value := strings.TrimSpace(id); value != "" {
		client.ID = value
	}
	if value := strings.TrimSpace(name); value != "" {
		client.Name = value
	}
	if value := strings.TrimSpace(ownerUserID); value != "" {
		client.OwnerUserID = value
	}
	client.Enabled = enabled
	client.APIKey = ""
	client.RouteAffinityKey = ""
	client.AccountGroup = core.NormalizeAccountGroupName(client.AccountGroup)
	return client, nil
}

func (r *PostgresRepository) GetClient(id string) (core.APIClient, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	payload, err := r.getPayloadByID("clients", id)
	if err != nil {
		return core.APIClient{}, err
	}
	return r.decodeClientPayload(payload)
}

func (r *PostgresRepository) FindClientByAPIKey(apiKey string) (core.APIClient, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return core.APIClient{}, ErrNotFound
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`SELECT payload FROM clients WHERE api_key_hash = ?`, clientAPIKeyHash(apiKey))
	if err != nil {
		return core.APIClient{}, err
	}
	defer rows.Close()

	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		client, err := r.decodeClientPayload(payload)
		if err != nil {
			continue
		}
		if client.APIKey == apiKey {
			return client, nil
		}
	}
	if err := rows.Err(); err != nil {
		return core.APIClient{}, err
	}
	return core.APIClient{}, ErrNotFound
}

func (r *PostgresRepository) UpsertClient(client core.APIClient) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	client.AccountGroup = core.NormalizeAccountGroupName(client.AccountGroup)
	client.BillingSource = core.NormalizeClientBillingSource(client.BillingSource)
	if client.SpendLimitNanoUSD < 0 {
		return ErrClientSpendLimitExceeded
	}
	now := time.Now().UTC()
	if client.CreatedAt.IsZero() {
		client.CreatedAt = now
	}
	client.UpdatedAt = now

	payload, err := r.encodeClientPayload(client)
	if err != nil {
		return err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	previousOwnerID := ""
	if previousPayload, err := r.getPayloadByIDTx(tx, "clients", client.ID); err == nil {
		previousClient, err := r.decodeClientPayload(previousPayload)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		previousOwnerID = strings.TrimSpace(previousClient.OwnerUserID)
	} else if !errors.Is(err, ErrNotFound) {
		_ = tx.Rollback()
		return err
	}
	if err := upsertClientRecord(tx, client, payload); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO client_spend(client_id, spend_limit_nano_usd, spend_used_nano_usd, updated_at_ns)
		VALUES(?, ?, 0, ?)
		ON CONFLICT(client_id) DO UPDATE SET
			spend_limit_nano_usd = excluded.spend_limit_nano_usd,
			updated_at_ns = excluded.updated_at_ns`,
		client.ID,
		client.SpendLimitNanoUSD,
		now.UnixNano(),
	); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := transferFinanceClientOwnerSpendRollupTx(tx, client.ID, previousOwnerID, client.OwnerUserID, now); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := upsertFinanceClientRollupMetadataTx(tx, client); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (r *PostgresRepository) DeleteClient(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	exists, err := queryExistsTx(tx, `SELECT 1 FROM clients WHERE id = ? LIMIT 1`, id)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if !exists {
		_ = tx.Rollback()
		return ErrNotFound
	}
	if err := r.releaseReservedBillingForDeletedDataTx(tx, "", []string{id}, time.Now().UTC()); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM openai_response_bindings WHERE client_id = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM client_spend WHERE client_id = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	result, err := tx.Exec(`DELETE FROM clients WHERE id = ?`, id)
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

func (r *PostgresRepository) UpsertOpenAIResponseBinding(binding core.OpenAIResponseBinding) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	binding.ResponseID = strings.TrimSpace(binding.ResponseID)
	binding.AccountID = strings.TrimSpace(binding.AccountID)
	binding.ClientID = strings.TrimSpace(binding.ClientID)
	binding.PromptCacheKey = strings.TrimSpace(binding.PromptCacheKey)
	if binding.ResponseID == "" || binding.AccountID == "" {
		return fmt.Errorf("response id and account id are required")
	}
	now := time.Now().UTC()
	if binding.CreatedAt.IsZero() {
		var createdNS int64
		err := r.db.QueryRow(`SELECT created_at_ns FROM openai_response_bindings WHERE response_id = ?`, binding.ResponseID).Scan(&createdNS)
		if err == nil && createdNS > 0 {
			binding.CreatedAt = time.Unix(0, createdNS).UTC()
		} else {
			binding.CreatedAt = now
		}
	}
	binding.UpdatedAt = now
	_, err := r.db.Exec(
		`INSERT INTO openai_response_bindings(response_id, account_id, client_id, prompt_cache_key, created_at_ns, updated_at_ns)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(response_id) DO UPDATE SET account_id = excluded.account_id, client_id = excluded.client_id, prompt_cache_key = excluded.prompt_cache_key, updated_at_ns = excluded.updated_at_ns`,
		binding.ResponseID,
		binding.AccountID,
		binding.ClientID,
		binding.PromptCacheKey,
		sqliteTimeNS(binding.CreatedAt),
		sqliteTimeNS(binding.UpdatedAt),
	)
	return err
}

func (r *PostgresRepository) GetOpenAIResponseBinding(responseID string) (core.OpenAIResponseBinding, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var binding core.OpenAIResponseBinding
	var createdNS, updatedNS int64
	err := r.db.QueryRow(
		`SELECT response_id, account_id, client_id, prompt_cache_key, created_at_ns, updated_at_ns FROM openai_response_bindings WHERE response_id = ?`,
		strings.TrimSpace(responseID),
	).Scan(&binding.ResponseID, &binding.AccountID, &binding.ClientID, &binding.PromptCacheKey, &createdNS, &updatedNS)
	if errors.Is(err, sql.ErrNoRows) {
		return core.OpenAIResponseBinding{}, ErrNotFound
	}
	if err != nil {
		return core.OpenAIResponseBinding{}, err
	}
	if createdNS > 0 {
		binding.CreatedAt = time.Unix(0, createdNS).UTC()
	}
	if updatedNS > 0 {
		binding.UpdatedAt = time.Unix(0, updatedNS).UTC()
	}
	return binding, nil
}
