package storage

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func (r *SQLiteRepository) getPayloadByID(table, id string) (string, error) {
	var payload string
	err := r.db.QueryRow(fmt.Sprintf(`SELECT payload FROM %s WHERE id = ?`, table), id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return payload, err
}

func (r *SQLiteRepository) getPayloadByIDTx(tx *sql.Tx, table, id string) (string, error) {
	var payload string
	err := tx.QueryRow(fmt.Sprintf(`SELECT payload FROM %s WHERE id = ?`, table), id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return payload, err
}

func (r *SQLiteRepository) userBalanceLocked(userID string) (int64, error) {
	var balance int64
	err := r.db.QueryRow(`SELECT balance_nano_usd FROM user_balances WHERE user_id = ?`, userID).Scan(&balance)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return balance, err
}

func (r *SQLiteRepository) upsertPayload(table, id, payload string) error {
	_, err := r.db.Exec(
		fmt.Sprintf(`INSERT INTO %s(id, payload) VALUES(?, ?) ON CONFLICT(id) DO UPDATE SET payload = excluded.payload`, table),
		id,
		payload,
	)
	return err
}

func (r *SQLiteRepository) upsertAccountStatePayload(table, accountID, payload string) error {
	_, err := r.db.Exec(
		fmt.Sprintf(`INSERT INTO %s(account_id, payload) VALUES(?, ?) ON CONFLICT(account_id) DO UPDATE SET payload = excluded.payload`, table),
		accountID,
		payload,
	)
	return err
}

func (r *SQLiteRepository) accountLocked(id string) (core.Account, error) {
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

func (r *SQLiteRepository) upsertAccountPartsLocked(account core.Account) error {
	config, credentialState, runtimeState := splitAccountForStorage(account)
	configPayload, err := r.encodeAccountConfigPayload(config)
	if err != nil {
		return err
	}
	credentialPayload, err := r.encodeAccountCredentialPayload(credentialState)
	if err != nil {
		return err
	}
	runtimePayload, err := r.encodeAccountRuntimePayload(runtimeState)
	if err != nil {
		return err
	}

	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO accounts(id, payload) VALUES(?, ?)
		ON CONFLICT(id) DO UPDATE SET payload = excluded.payload
	`, account.ID, configPayload); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO account_credentials(account_id, payload) VALUES(?, ?)
		ON CONFLICT(account_id) DO UPDATE SET payload = excluded.payload
	`, account.ID, credentialPayload); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO account_runtime(account_id, payload) VALUES(?, ?)
		ON CONFLICT(account_id) DO UPDATE SET payload = excluded.payload
	`, account.ID, runtimePayload); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

type sqlExecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
}

type sqlScanner interface {
	Scan(dest ...any) error
}

func placeholders(count int) string {
	if count <= 0 {
		return ""
	}
	var builder strings.Builder
	for i := 0; i < count; i++ {
		if i > 0 {
			builder.WriteString(", ")
		}
		builder.WriteByte('?')
	}
	return builder.String()
}

func usernameKey(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

func closeRowsWithError(rows *sql.Rows, err error) error {
	if closeErr := rows.Close(); closeErr != nil {
		return fmt.Errorf("%w; close rows: %v", err, closeErr)
	}
	return err
}

func insertAuditTermsTx(tx *sql.Tx, seq int64, actorText, resourceText string) error {
	for _, term := range auditIndexTerms(actorText) {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO audit_terms(seq, term_type, term) VALUES(?, ?, ?)`,
			seq,
			"actor",
			term,
		); err != nil {
			return err
		}
	}
	for _, term := range auditIndexTerms(resourceText) {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO audit_terms(seq, term_type, term) VALUES(?, ?, ?)`,
			seq,
			"resource",
			term,
		); err != nil {
			return err
		}
	}
	return nil
}

func auditIndexTerms(text string) []string {
	text = strings.ToLower(text)
	terms := make([]string, 0)
	seen := map[string]bool{}
	var builder strings.Builder
	flush := func() {
		if builder.Len() == 0 {
			return
		}
		term := builder.String()
		builder.Reset()
		if seen[term] {
			return
		}
		seen[term] = true
		terms = append(terms, term)
	}
	for _, r := range text {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return terms
}

func upsertClientRecord(exec sqlExecutor, client core.APIClient, payload string) error {
	enabled := 0
	if client.Enabled {
		enabled = 1
	}
	_, err := exec.Exec(
		`INSERT INTO clients(id, payload, api_key_hash, name, owner_user_id, enabled)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			payload = excluded.payload,
			api_key_hash = excluded.api_key_hash,
			name = excluded.name,
			owner_user_id = excluded.owner_user_id,
			enabled = excluded.enabled`,
		client.ID,
		payload,
		clientAPIKeyHash(client.APIKey),
		strings.TrimSpace(client.Name),
		strings.TrimSpace(client.OwnerUserID),
		enabled,
	)
	return err
}

func upsertUserRecordTx(tx *sql.Tx, user core.User, updatedAt time.Time) error {
	if err := upsertUserMetadataTx(tx, user); err != nil {
		return err
	}
	if err := upsertUserBalanceTx(tx, user.ID, user.BalanceNanoUSD, updatedAt); err != nil {
		return err
	}
	if err := upsertFinanceUserRollupIdentityTx(tx, user); err != nil {
		return err
	}
	return syncUserIdentityIndexesTx(tx, user)
}

func syncUserIdentityIndexesTx(tx *sql.Tx, user core.User) error {
	userID := strings.TrimSpace(user.ID)
	if userID == "" {
		return nil
	}
	if _, err := tx.Exec(`DELETE FROM user_oauth_identities WHERE user_id = ?`, userID); err != nil {
		return err
	}
	for _, identity := range user.OAuthIdentities {
		provider := strings.ToLower(strings.TrimSpace(identity.Provider))
		subject := strings.TrimSpace(identity.Subject)
		if provider == "" || subject == "" {
			continue
		}
		if _, err := tx.Exec(`
			INSERT INTO user_oauth_identities(user_id, provider, subject, email, username, linked_at_ns)
			VALUES(?, ?, ?, ?, ?, ?)
		`, userID, provider, subject, strings.TrimSpace(identity.Email), strings.TrimSpace(identity.Username), sqliteTimeNS(identity.LinkedAt)); err != nil {
			return err
		}
	}

	signature := core.UserInvitationSignature(user)
	if signature == "" {
		_, err := tx.Exec(`DELETE FROM user_invitation_codes WHERE user_id = ?`, userID)
		return err
	}
	_, err := tx.Exec(`
		INSERT INTO user_invitation_codes(user_id, signature, updated_at_ns)
		VALUES(?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			signature = excluded.signature,
			updated_at_ns = excluded.updated_at_ns
	`, userID, signature, sqliteTimeNS(time.Now().UTC()))
	return err
}

func (r *SQLiteRepository) mergeUserClientsTx(tx *sql.Tx, sourceID, targetID string, now time.Time) error {
	rows, err := tx.Query(`SELECT id, payload FROM clients`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type clientUpdate struct {
		client  core.APIClient
		payload string
	}
	updates := make([]clientUpdate, 0)
	for rows.Next() {
		var id, payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return err
		}
		client, err := r.decodeClientPayload(payload)
		if err != nil {
			return err
		}
		if strings.TrimSpace(client.OwnerUserID) != sourceID {
			continue
		}
		client.OwnerUserID = targetID
		client.UpdatedAt = now
		nextPayload, err := r.encodeClientPayload(client)
		if err != nil {
			return err
		}
		updates = append(updates, clientUpdate{client: client, payload: nextPayload})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range updates {
		if err := upsertClientRecord(tx, item.client, item.payload); err != nil {
			return err
		}
		if err := upsertFinanceClientRollupMetadataTx(tx, item.client); err != nil {
			return err
		}
	}
	return nil
}

func (r *SQLiteRepository) mergeUserInvitersTx(tx *sql.Tx, sourceID, targetID string, now time.Time) error {
	rows, err := tx.Query(`SELECT id, payload FROM users`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type userUpdate struct {
		id      string
		payload string
	}
	updates := make([]userUpdate, 0)
	for rows.Next() {
		var id, payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return err
		}
		if strings.TrimSpace(id) == targetID {
			continue
		}
		var user core.User
		if err := json.Unmarshal([]byte(payload), &user); err != nil {
			return err
		}
		if strings.TrimSpace(user.InviterUserID) != sourceID {
			continue
		}
		user.InviterUserID = targetID
		user.UpdatedAt = now
		nextPayload, err := json.Marshal(cloneUser(user))
		if err != nil {
			return err
		}
		updates = append(updates, userUpdate{id: id, payload: string(nextPayload)})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range updates {
		if _, err := tx.Exec(`UPDATE users SET payload = ? WHERE id = ?`, item.payload, item.id); err != nil {
			return err
		}
	}
	return nil
}

func (r *SQLiteRepository) mergeUserSiteMessagesTx(tx *sql.Tx, sourceID, targetID string, now time.Time) error {
	return r.updateUserSiteMessageTargetsTx(tx, sourceID, targetID, now)
}

func (r *SQLiteRepository) removeUserSiteMessagesTx(tx *sql.Tx, userID string, now time.Time) error {
	return r.updateUserSiteMessageTargetsTx(tx, userID, "", now)
}

func (r *SQLiteRepository) updateUserSiteMessageTargetsTx(tx *sql.Tx, oldID, newID string, now time.Time) error {
	rows, err := tx.Query(`SELECT id, payload FROM site_messages`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type messageUpdate struct {
		id      string
		enabled int
		payload string
	}
	updates := make([]messageUpdate, 0)
	for rows.Next() {
		var id, payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return err
		}
		var message core.SiteMessage
		if err := json.Unmarshal([]byte(payload), &message); err != nil {
			return err
		}
		nextTargets := replaceUserIDList(message.TargetUserIDs, oldID, newID)
		if slices.Equal(nextTargets, message.TargetUserIDs) {
			continue
		}
		message.TargetUserIDs = nextTargets
		message.UpdatedAt = now
		nextPayload, err := json.Marshal(cloneSiteMessage(message))
		if err != nil {
			return err
		}
		updates = append(updates, messageUpdate{id: id, enabled: boolInt(message.Enabled), payload: string(nextPayload)})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range updates {
		if _, err := tx.Exec(`UPDATE site_messages SET enabled = ?, payload = ?, updated_at_ns = ? WHERE id = ?`, item.enabled, item.payload, sqliteTimeNS(now), item.id); err != nil {
			return err
		}
	}
	return nil
}

func mergeUserSiteMessageReadsTx(tx *sql.Tx, sourceID, targetID string) error {
	rows, err := tx.Query(`SELECT message_id, read_at_ns FROM site_message_reads WHERE user_id = ?`, sourceID)
	if err != nil {
		return err
	}
	defer rows.Close()

	type readItem struct {
		messageID string
		readAtNS  int64
	}
	reads := make([]readItem, 0)
	for rows.Next() {
		var item readItem
		if err := rows.Scan(&item.messageID, &item.readAtNS); err != nil {
			return err
		}
		reads = append(reads, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range reads {
		if _, err := tx.Exec(`INSERT INTO site_message_reads(message_id, user_id, read_at_ns) VALUES(?, ?, ?) ON CONFLICT(message_id, user_id) DO UPDATE SET read_at_ns = MAX(site_message_reads.read_at_ns, excluded.read_at_ns)`, item.messageID, targetID, item.readAtNS); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`DELETE FROM site_message_reads WHERE user_id = ?`, sourceID)
	return err
}

func sqliteTimeNS(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UTC().UnixNano()
}

func timeFromNS(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}

func timePtrFromNS(value int64) *time.Time {
	if value <= 0 {
		return nil
	}
	t := timeFromNS(value)
	return &t
}

func paymentOrderPaidAtNS(order core.PaymentOrder) int64 {
	if order.PaidAt == nil {
		return 0
	}
	return sqliteTimeNS(*order.PaidAt)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (r *SQLiteRepository) getOrCreateClientSpendTx(tx *sql.Tx, clientID string) (core.ClientSpend, error) {
	var spend core.ClientSpend
	var updatedAtNS int64
	err := tx.QueryRow(`SELECT client_id, spend_limit_nano_usd, spend_used_nano_usd, updated_at_ns FROM client_spend WHERE client_id = ?`, clientID).
		Scan(&spend.ClientID, &spend.SpendLimitNanoUSD, &spend.SpendUsedNanoUSD, &updatedAtNS)
	if err == nil {
		spend.UpdatedAt = timeFromNS(updatedAtNS)
		return spend, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return core.ClientSpend{}, err
	}
	var payload string
	if err := tx.QueryRow(`SELECT payload FROM clients WHERE id = ?`, clientID).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.ClientSpend{}, ErrNotFound
		}
		return core.ClientSpend{}, err
	}
	client, err := r.decodeClientPayload(payload)
	if err != nil {
		return core.ClientSpend{}, err
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(`INSERT INTO client_spend(client_id, spend_limit_nano_usd, spend_used_nano_usd, updated_at_ns) VALUES(?, ?, 0, ?)`, clientID, client.SpendLimitNanoUSD, now.UnixNano()); err != nil {
		return core.ClientSpend{}, err
	}
	return core.ClientSpend{ClientID: clientID, SpendLimitNanoUSD: client.SpendLimitNanoUSD, UpdatedAt: now}, nil
}

func getBillingRequestTx(tx *sql.Tx, requestID, clientID string) (core.BillingReservation, bool, error) {
	row := tx.QueryRow(`
		SELECT id, request_id, client_id, client_name, user_id, account_id, account_label, failed_account_labels, account_group, account_group_multiplier_bps, billing_source, provider, model, status,
			estimated_prompt_tokens, estimated_completion_tokens,
			prompt_tokens, cached_prompt_tokens, cache_creation_tokens, cache_creation_5m_tokens, cache_creation_1h_tokens, completion_tokens, image_output_tokens, total_tokens,
			input_price_nano_usd_per_1m, cached_input_price_nano_usd_per_1m, cache_write_price_nano_usd_per_1m, cache_write_5m_price_nano_usd_per_1m, cache_write_1h_price_nano_usd_per_1m, output_price_nano_usd_per_1m, image_output_price_nano_usd_per_1m,
			reserved_nano_usd, actual_nano_usd, first_token_ms, fingerprint, cache_diagnostics, fast_mode, created_at_ns, settled_at_ns
		FROM billing_requests
		WHERE request_id = ? AND client_id = ?
	`, requestID, clientID)
	request, err := scanBillingRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.BillingReservation{}, false, nil
	}
	if err != nil {
		return core.BillingReservation{}, false, err
	}
	return request, true, nil
}

func scanBillingRequest(scanner sqlScanner) (core.BillingReservation, error) {
	var request core.BillingReservation
	var provider string
	var status string
	var fastMode int
	var failedAccountLabels string
	var cacheDiagnostics string
	var createdAtNS int64
	var settledAtNS int64
	if err := scanner.Scan(
		&request.ID,
		&request.RequestID,
		&request.ClientID,
		&request.ClientName,
		&request.UserID,
		&request.AccountID,
		&request.AccountLabel,
		&failedAccountLabels,
		&request.AccountGroup,
		&request.AccountGroupMultiplierBps,
		&request.BillingSource,
		&provider,
		&request.Model,
		&status,
		&request.EstimatedPromptTokens,
		&request.EstimatedCompletionTokens,
		&request.PromptTokens,
		&request.CachedPromptTokens,
		&request.CacheCreationTokens,
		&request.CacheCreation5mTokens,
		&request.CacheCreation1hTokens,
		&request.CompletionTokens,
		&request.ImageOutputTokens,
		&request.TotalTokens,
		&request.InputPriceNanoUSDPer1M,
		&request.CachedInputPriceNanoUSDPer1M,
		&request.CacheWritePriceNanoUSDPer1M,
		&request.CacheWrite5mPriceNanoUSDPer1M,
		&request.CacheWrite1hPriceNanoUSDPer1M,
		&request.OutputPriceNanoUSDPer1M,
		&request.ImageOutputPriceNanoUSDPer1M,
		&request.ReservedNanoUSD,
		&request.ActualNanoUSD,
		&request.FirstTokenMS,
		&request.Fingerprint,
		&cacheDiagnostics,
		&fastMode,
		&createdAtNS,
		&settledAtNS,
	); err != nil {
		return core.BillingReservation{}, err
	}
	request.Provider = core.ProviderKind(provider)
	request.Status = core.BillingRequestStatus(status)
	request.BillingSource = core.NormalizeClientBillingSource(request.BillingSource)
	request.FailedAccountLabels = decodeBillingAccountLabels(failedAccountLabels)
	request.CacheDiagnostics = decodeBillingCacheDiagnostics(cacheDiagnostics)
	request.FastMode = fastMode != 0
	request.CreatedAt = timeFromNS(createdAtNS)
	request.SettledAt = timePtrFromNS(settledAtNS)
	return request, nil
}

func compactBillingAccountLabels(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func encodeBillingAccountLabels(values []string) string {
	values = compactBillingAccountLabels(values)
	if len(values) == 0 {
		return ""
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return ""
	}
	return string(raw)
}

func decodeBillingAccountLabels(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil
	}
	return compactBillingAccountLabels(values)
}

func (r *SQLiteRepository) getPaymentOrderByColumn(column, value string) (core.PaymentOrder, error) {
	if value == "" {
		return core.PaymentOrder{}, ErrNotFound
	}
	if err := expirePaymentOrdersDB(r.db, time.Now().UTC()); err != nil {
		return core.PaymentOrder{}, err
	}
	var payload string
	err := r.db.QueryRow(fmt.Sprintf(`SELECT payload FROM payment_orders WHERE %s = ?`, column), value).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return core.PaymentOrder{}, ErrNotFound
	}
	if err != nil {
		return core.PaymentOrder{}, err
	}
	var order core.PaymentOrder
	if err := json.Unmarshal([]byte(payload), &order); err != nil {
		return core.PaymentOrder{}, err
	}
	return clonePaymentOrder(order), nil
}

func getPaymentOrderByOutTradeNoTx(tx *sql.Tx, outTradeNo string) (core.PaymentOrder, bool, error) {
	var payload string
	err := tx.QueryRow(`SELECT payload FROM payment_orders WHERE out_trade_no = ?`, outTradeNo).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return core.PaymentOrder{}, false, nil
	}
	if err != nil {
		return core.PaymentOrder{}, false, err
	}
	var order core.PaymentOrder
	if err := json.Unmarshal([]byte(payload), &order); err != nil {
		return core.PaymentOrder{}, false, err
	}
	return clonePaymentOrder(order), true, nil
}

func getPaymentOrderByIDTx(tx *sql.Tx, id string) (core.PaymentOrder, bool, error) {
	var payload string
	err := tx.QueryRow(`SELECT payload FROM payment_orders WHERE id = ?`, strings.TrimSpace(id)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return core.PaymentOrder{}, false, nil
	}
	if err != nil {
		return core.PaymentOrder{}, false, err
	}
	var order core.PaymentOrder
	if err := json.Unmarshal([]byte(payload), &order); err != nil {
		return core.PaymentOrder{}, false, err
	}
	return clonePaymentOrder(order), true, nil
}

func getPaymentRefundByIDTx(tx *sql.Tx, id string) (core.PaymentRefund, bool, error) {
	var payload string
	err := tx.QueryRow(`SELECT payload FROM payment_refunds WHERE id = ?`, strings.TrimSpace(id)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return core.PaymentRefund{}, false, nil
	}
	if err != nil {
		return core.PaymentRefund{}, false, err
	}
	var refund core.PaymentRefund
	if err := json.Unmarshal([]byte(payload), &refund); err != nil {
		return core.PaymentRefund{}, false, err
	}
	return refund, true, nil
}

func refundablePaymentAmountTx(tx *sql.Tx, order core.PaymentOrder) (int64, error) {
	rows, err := tx.Query(`SELECT payload FROM payment_refunds WHERE order_id = ? AND status IN (?, ?)`, order.ID, string(core.PaymentRefundPending), string(core.PaymentRefundDone))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	refunded := int64(0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return 0, err
		}
		var refund core.PaymentRefund
		if err := json.Unmarshal([]byte(payload), &refund); err != nil {
			return 0, err
		}
		refunded, err = addNanoUSD(refunded, refund.AmountNanoUSD)
		if err != nil {
			return 0, err
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	remaining := order.AmountNanoUSD - refunded
	if remaining < 0 {
		return 0, nil
	}
	return remaining, nil
}

func pendingPaymentRefundAmountTx(tx *sql.Tx, userID string) (int64, error) {
	rows, err := tx.Query(`SELECT payload FROM payment_refunds WHERE user_id = ? AND status = ?`, strings.TrimSpace(userID), string(core.PaymentRefundPending))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	pending := int64(0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return 0, err
		}
		var refund core.PaymentRefund
		if err := json.Unmarshal([]byte(payload), &refund); err != nil {
			return 0, err
		}
		next, err := addNanoUSD(pending, refund.AmountNanoUSD)
		if err != nil {
			return 0, err
		}
		pending = next
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return pending, nil
}

func (r *SQLiteRepository) getSiteMessageLocked(id string) (core.SiteMessage, error) {
	if id == "" {
		return core.SiteMessage{}, ErrNotFound
	}
	var payload string
	err := r.db.QueryRow(`SELECT payload FROM site_messages WHERE id = ?`, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return core.SiteMessage{}, ErrNotFound
	}
	if err != nil {
		return core.SiteMessage{}, err
	}
	var message core.SiteMessage
	if err := json.Unmarshal([]byte(payload), &message); err != nil {
		return core.SiteMessage{}, err
	}
	return cloneSiteMessage(message), nil
}

func expirePaymentOrdersDB(db *sql.DB, now time.Time) error {
	cutoffNS := sqliteTimeNS(now.Add(-core.DefaultPaymentOrderPendingTTL))
	rows, err := db.Query(`SELECT id, payload FROM payment_orders WHERE status = ? AND created_at_ns <= ?`, string(core.PaymentOrderPending), cutoffNS)
	if err != nil {
		return err
	}
	type expiredPayment struct {
		id      string
		payload string
		updated int64
	}
	expired := make([]expiredPayment, 0)
	for rows.Next() {
		var id string
		var payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return closeRowsWithError(rows, err)
		}
		var order core.PaymentOrder
		if err := json.Unmarshal([]byte(payload), &order); err != nil {
			continue
		}
		order = expirePaymentOrderIfNeeded(order, now)
		if order.Status != core.PaymentOrderClosed {
			continue
		}
		nextPayload, err := json.Marshal(order)
		if err != nil {
			return closeRowsWithError(rows, err)
		}
		expired = append(expired, expiredPayment{id: id, payload: string(nextPayload), updated: sqliteTimeNS(order.UpdatedAt)})
	}
	if err := rows.Err(); err != nil {
		return closeRowsWithError(rows, err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range expired {
		if _, err := db.Exec(`UPDATE payment_orders SET status = ?, payload = ?, updated_at_ns = ? WHERE id = ? AND status = ?`, string(core.PaymentOrderClosed), item.payload, item.updated, item.id, string(core.PaymentOrderPending)); err != nil {
			return err
		}
	}
	return nil
}

func insertBillingRequestTx(tx *sql.Tx, request core.BillingReservation) error {
	request.BillingSource = core.NormalizeClientBillingSource(request.BillingSource)
	failedAccountLabels := encodeBillingAccountLabels(request.FailedAccountLabels)
	_, err := tx.Exec(`
		INSERT INTO billing_requests(
			id, request_id, client_id, client_name, user_id, account_id, account_label, failed_account_labels, account_group, account_group_multiplier_bps, billing_source, provider, model, status,
			estimated_prompt_tokens, estimated_completion_tokens,
			prompt_tokens, cached_prompt_tokens, cache_creation_tokens, cache_creation_5m_tokens, cache_creation_1h_tokens, completion_tokens, image_output_tokens, total_tokens,
			input_price_nano_usd_per_1m, cached_input_price_nano_usd_per_1m, cache_write_price_nano_usd_per_1m, cache_write_5m_price_nano_usd_per_1m, cache_write_1h_price_nano_usd_per_1m, output_price_nano_usd_per_1m, image_output_price_nano_usd_per_1m,
			reserved_nano_usd, actual_nano_usd, first_token_ms, fingerprint, cache_diagnostics, fast_mode, created_at_ns, settled_at_ns
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		request.ID,
		request.RequestID,
		request.ClientID,
		request.ClientName,
		request.UserID,
		request.AccountID,
		request.AccountLabel,
		failedAccountLabels,
		request.AccountGroup,
		request.AccountGroupMultiplierBps,
		request.BillingSource,
		string(request.Provider),
		request.Model,
		string(request.Status),
		request.EstimatedPromptTokens,
		request.EstimatedCompletionTokens,
		request.PromptTokens,
		request.CachedPromptTokens,
		request.CacheCreationTokens,
		request.CacheCreation5mTokens,
		request.CacheCreation1hTokens,
		request.CompletionTokens,
		request.ImageOutputTokens,
		request.TotalTokens,
		request.InputPriceNanoUSDPer1M,
		request.CachedInputPriceNanoUSDPer1M,
		request.CacheWritePriceNanoUSDPer1M,
		request.CacheWrite5mPriceNanoUSDPer1M,
		request.CacheWrite1hPriceNanoUSDPer1M,
		request.OutputPriceNanoUSDPer1M,
		request.ImageOutputPriceNanoUSDPer1M,
		request.ReservedNanoUSD,
		request.ActualNanoUSD,
		request.FirstTokenMS,
		request.Fingerprint,
		encodeBillingCacheDiagnostics(request.CacheDiagnostics),
		boolInt(request.FastMode),
		sqliteTimeNS(request.CreatedAt),
		sqliteTimeNS(valueTime(request.SettledAt)),
	)
	return err
}

func updateBillingRequestSettlementTx(tx *sql.Tx, request core.BillingReservation) error {
	request.BillingSource = core.NormalizeClientBillingSource(request.BillingSource)
	_, err := tx.Exec(`
		UPDATE billing_requests
		SET account_id = ?,
			account_label = ?,
			account_group = ?,
			account_group_multiplier_bps = ?,
			billing_source = ?,
			provider = ?,
			model = ?,
			fast_mode = ?,
			status = ?,
			prompt_tokens = ?,
			cached_prompt_tokens = ?,
			cache_creation_tokens = ?,
			cache_creation_5m_tokens = ?,
			cache_creation_1h_tokens = ?,
			completion_tokens = ?,
			image_output_tokens = ?,
			total_tokens = ?,
			input_price_nano_usd_per_1m = ?,
			cached_input_price_nano_usd_per_1m = ?,
			cache_write_price_nano_usd_per_1m = ?,
			cache_write_5m_price_nano_usd_per_1m = ?,
			cache_write_1h_price_nano_usd_per_1m = ?,
			output_price_nano_usd_per_1m = ?,
			image_output_price_nano_usd_per_1m = ?,
			actual_nano_usd = ?,
			first_token_ms = ?,
			cache_diagnostics = ?,
			settled_at_ns = ?
		WHERE request_id = ? AND client_id = ?
	`,
		request.AccountID,
		request.AccountLabel,
		request.AccountGroup,
		request.AccountGroupMultiplierBps,
		request.BillingSource,
		string(request.Provider),
		request.Model,
		boolInt(request.FastMode),
		string(request.Status),
		request.PromptTokens,
		request.CachedPromptTokens,
		request.CacheCreationTokens,
		request.CacheCreation5mTokens,
		request.CacheCreation1hTokens,
		request.CompletionTokens,
		request.ImageOutputTokens,
		request.TotalTokens,
		request.InputPriceNanoUSDPer1M,
		request.CachedInputPriceNanoUSDPer1M,
		request.CacheWritePriceNanoUSDPer1M,
		request.CacheWrite5mPriceNanoUSDPer1M,
		request.CacheWrite1hPriceNanoUSDPer1M,
		request.OutputPriceNanoUSDPer1M,
		request.ImageOutputPriceNanoUSDPer1M,
		request.ActualNanoUSD,
		request.FirstTokenMS,
		encodeBillingCacheDiagnostics(request.CacheDiagnostics),
		sqliteTimeNS(valueTime(request.SettledAt)),
		request.RequestID,
		request.ClientID,
	)
	return err
}

func decodeBillingCacheDiagnostics(raw string) core.BillingCacheDiagnostics {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return core.BillingCacheDiagnostics{}
	}
	var diag core.BillingCacheDiagnostics
	if err := json.Unmarshal([]byte(raw), &diag); err != nil {
		return core.BillingCacheDiagnostics{}
	}
	return diag
}

func encodeBillingCacheDiagnostics(diag core.BillingCacheDiagnostics) string {
	if strings.TrimSpace(diag.RequestShape) == "" &&
		strings.TrimSpace(diag.PromptCacheKeySource) == "" &&
		strings.TrimSpace(diag.PromptCacheKeyHash) == "" &&
		strings.TrimSpace(diag.RouteAffinityHash) == "" &&
		strings.TrimSpace(diag.SessionAffinityHash) == "" &&
		!diag.PromptCacheKeyPresent && !diag.RouteAffinityPresent && !diag.SessionAffinityPresent {
		return ""
	}
	raw, err := json.Marshal(diag)
	if err != nil {
		return ""
	}
	return string(raw)
}

func insertBillingLedgerTx(tx *sql.Tx, entry core.BillingLedgerEntry) error {
	_, err := tx.Exec(`
		INSERT INTO billing_ledger(id, user_id, client_id, request_id, kind, amount_nano_usd, balance_after_nano_usd, note, created_at_ns)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		entry.ID,
		entry.UserID,
		entry.ClientID,
		entry.RequestID,
		entry.Kind,
		entry.AmountNanoUSD,
		entry.BalanceAfterNanoUSD,
		entry.Note,
		sqliteTimeNS(entry.CreatedAt),
	)
	return err
}

func valueTime(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

func (r *SQLiteRepository) deleteByID(table, id string) error {
	result, err := r.db.Exec(fmt.Sprintf(`DELETE FROM %s WHERE id = ?`, table), id)
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

func queryExistsTx(tx *sql.Tx, query string, args ...any) (bool, error) {
	var marker int
	err := tx.QueryRow(query, args...).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *SQLiteRepository) trimAuditTx(tx *sql.Tx) error {
	if r.limit <= 0 {
		return nil
	}
	if _, err := tx.Exec(
		`DELETE FROM audit
		WHERE seq <= COALESCE((
			SELECT seq FROM audit ORDER BY seq DESC LIMIT 1 OFFSET ?
		), 0)`,
		r.limit,
	); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM audit_terms WHERE seq NOT IN (SELECT seq FROM audit)`)
	return err
}

func (r *SQLiteRepository) maybeTrimAuditTx(tx *sql.Tx, now time.Time, writes int) error {
	if writes < 1 {
		writes = 1
	}
	r.auditTrimOps += writes
	if !shouldRunAuditTrim(now, r.auditTrimAt, r.auditTrimOps, r.limit) {
		return nil
	}
	if err := r.trimAuditTx(tx); err != nil {
		return err
	}
	r.auditTrimAt = now
	r.auditTrimOps = 0
	return nil
}

func (r *SQLiteRepository) trimBillingRequestsTx(tx *sql.Tx, now time.Time) error {
	return trimBillingRequestsTx(tx, now, r.usageMaxAge)
}

func (r *SQLiteRepository) maybeTrimBillingRequestsTx(tx *sql.Tx, now time.Time) error {
	r.usageTrimOps++
	if !shouldRunRetentionTrim(now, r.usageTrimAt, r.usageTrimOps, r.usageMaxAge) {
		return nil
	}
	if err := r.trimBillingRequestsTx(tx, now); err != nil {
		return err
	}
	r.usageTrimAt = now
	r.usageTrimOps = 0
	return nil
}

func (r *SQLiteRepository) maybeTrimBillingRequestsLocked(now time.Time) error {
	r.usageTrimOps++
	if !shouldRunRetentionTrim(now, r.usageTrimAt, r.usageTrimOps, r.usageMaxAge) {
		return nil
	}
	if err := r.trimBillingRequestsLocked(now); err != nil {
		return err
	}
	r.usageTrimAt = now
	r.usageTrimOps = 0
	return nil
}

func (r *SQLiteRepository) trimBillingRequestsLocked(now time.Time) error {
	return trimBillingRequestsDB(r.db, now, r.usageMaxAge)
}

func (r *SQLiteRepository) maybeTrimBillingLedgerLocked(now time.Time) error {
	r.ledgerTrimOps++
	if !shouldRunAgeRetentionTrim(now, r.ledgerTrimAt, r.ledgerTrimOps) {
		return nil
	}
	if err := r.trimBillingLedgerLocked(now); err != nil {
		return err
	}
	r.ledgerTrimAt = now
	r.ledgerTrimOps = 0
	return nil
}

func (r *SQLiteRepository) trimBillingLedgerLocked(now time.Time) error {
	return trimBillingLedgerDB(r.db, now, r.ledgerMaxAge)
}

func trimBillingRequestsTx(tx *sql.Tx, now time.Time, maxAge time.Duration) error {
	if maxAge == 0 {
		if _, err := tx.Exec(`
			DELETE FROM billing_funding_allocations
			WHERE EXISTS (
				SELECT 1
				FROM billing_requests br
				WHERE br.request_id = billing_funding_allocations.request_id
					AND br.client_id = billing_funding_allocations.client_id
					AND br.status <> ?
			)
		`, string(core.BillingRequestReserved)); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM billing_requests WHERE status <> ?`, string(core.BillingRequestReserved))
		return err
	}
	if maxAge < 0 {
		maxAge = defaultUsageLogMaxAge
	}
	cutoffNS := now.UTC().Add(-maxAge).UnixNano()
	if _, err := tx.Exec(`
		DELETE FROM billing_funding_allocations
		WHERE EXISTS (
			SELECT 1
			FROM billing_requests br
			WHERE br.request_id = billing_funding_allocations.request_id
				AND br.client_id = billing_funding_allocations.client_id
				AND br.status <> ?
				AND br.created_at_ns < ?
		)
	`, string(core.BillingRequestReserved), cutoffNS); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM billing_requests WHERE status <> ? AND created_at_ns < ?`, string(core.BillingRequestReserved), cutoffNS); err != nil {
		return err
	}
	return nil
}

func trimBillingRequestsDB(db *sql.DB, now time.Time, maxAge time.Duration) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := trimBillingRequestsTx(tx, now, maxAge); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func trimBillingLedgerTx(tx *sql.Tx, now time.Time, maxAge time.Duration) error {
	if maxAge <= 0 {
		maxAge = defaultBillingLedgerRetentionAge
	}
	cutoffNS := now.UTC().Add(-maxAge).UnixNano()
	_, err := tx.Exec(`DELETE FROM billing_ledger WHERE created_at_ns < ?`, cutoffNS)
	return err
}

func trimBillingLedgerDB(db *sql.DB, now time.Time, maxAge time.Duration) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := trimBillingLedgerTx(tx, now, maxAge); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func shouldRunRetentionTrim(now, last time.Time, ops int, maxAge time.Duration) bool {
	if maxAge == 0 {
		return true
	}
	if last.IsZero() {
		return true
	}
	if ops >= retentionTrimOperationInterval {
		return true
	}
	return !now.Before(last.Add(retentionTrimInterval))
}

func shouldRunAgeRetentionTrim(now, last time.Time, ops int) bool {
	if last.IsZero() {
		return true
	}
	if ops >= retentionTrimOperationInterval {
		return true
	}
	return !now.Before(last.Add(retentionTrimInterval))
}

func shouldRunAuditTrim(now, last time.Time, ops int, limit int) bool {
	if limit <= 0 {
		return false
	}
	if last.IsZero() {
		return true
	}
	threshold := retentionTrimOperationInterval
	if limit < threshold {
		threshold = limit
	}
	if threshold < 1 {
		threshold = 1
	}
	if ops >= threshold {
		return true
	}
	return !now.Before(last.Add(retentionTrimInterval))
}

func (r *SQLiteRepository) encodeAccountConfigPayload(config storedAccountConfig) (string, error) {
	config.Group = core.NormalizeAccountGroupName(config.Group)
	payload, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func (r *SQLiteRepository) decodeAccountConfigPayload(payload string) (storedAccountConfig, error) {
	var config storedAccountConfig
	if err := json.Unmarshal([]byte(payload), &config); err != nil {
		return storedAccountConfig{}, err
	}
	config.Group = core.NormalizeAccountGroupName(config.Group)
	return config, nil
}

func (r *SQLiteRepository) encodeAccountCredentialPayload(state storedAccountCredentialState) (string, error) {
	encoded := state
	var err error
	if encoded.ProxyURL, err = r.codec.encryptValue(encoded.ProxyURL); err != nil {
		return "", err
	}
	if encoded.Credential, err = r.codec.EncryptCredential(encoded.Credential); err != nil {
		return "", err
	}
	payload, err := json.Marshal(encoded)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func (r *SQLiteRepository) decodeAccountCredentialPayload(payload string) (storedAccountCredentialState, error) {
	var state storedAccountCredentialState
	if err := json.Unmarshal([]byte(payload), &state); err != nil {
		return storedAccountCredentialState{}, err
	}
	var err error
	if state.ProxyURL, err = r.codec.decryptValue(state.ProxyURL); err != nil {
		return storedAccountCredentialState{}, err
	}
	if state.Credential, err = r.codec.DecryptCredential(state.Credential); err != nil {
		return storedAccountCredentialState{}, err
	}
	return state, nil
}

func (r *SQLiteRepository) encodeAccountRuntimePayload(state storedAccountRuntimeState) (string, error) {
	state.Status = normalizeStoredAccountStatus(state.Status)
	state.Metadata = runtimeMetadataOnly(state.Metadata)
	payload, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func (r *SQLiteRepository) decodeAccountRuntimePayload(payload string) (storedAccountRuntimeState, error) {
	var state storedAccountRuntimeState
	if err := json.Unmarshal([]byte(payload), &state); err != nil {
		return storedAccountRuntimeState{}, err
	}
	state.Status = normalizeStoredAccountStatus(state.Status)
	state.Metadata = runtimeMetadataOnly(state.Metadata)
	return state, nil
}

func (r *SQLiteRepository) decodeStoredAccountPayloads(configPayload string, credentialPayload, runtimePayload sql.NullString) (core.Account, error) {
	config, err := r.decodeAccountConfigPayload(configPayload)
	if err != nil {
		return core.Account{}, err
	}
	credentialState := storedAccountCredentialState{AccountID: config.ID, UpdatedAt: config.UpdatedAt}
	if credentialPayload.Valid && strings.TrimSpace(credentialPayload.String) != "" {
		credentialState, err = r.decodeAccountCredentialPayload(credentialPayload.String)
		if err != nil {
			return core.Account{}, err
		}
		if credentialState.AccountID == "" {
			credentialState.AccountID = config.ID
		}
	}
	runtimeState := storedAccountRuntimeState{AccountID: config.ID, Status: core.AccountStatusActive, UpdatedAt: config.UpdatedAt}
	if runtimePayload.Valid && strings.TrimSpace(runtimePayload.String) != "" {
		runtimeState, err = r.decodeAccountRuntimePayload(runtimePayload.String)
		if err != nil {
			return core.Account{}, err
		}
		if runtimeState.AccountID == "" {
			runtimeState.AccountID = config.ID
		}
	}
	return mergeStoredAccount(config, credentialState, runtimeState), nil
}

func (r *SQLiteRepository) encodeAccountGroupPayload(group core.AccountGroup) (string, error) {
	group = core.NormalizeAccountGroupBilling(group)
	encoded, err := r.codec.EncryptAccountGroup(group)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(encoded)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func (r *SQLiteRepository) decodeAccountGroupPayload(payload string) (core.AccountGroup, error) {
	var group core.AccountGroup
	if err := json.Unmarshal([]byte(payload), &group); err != nil {
		return core.AccountGroup{}, err
	}
	group, err := r.codec.DecryptAccountGroup(group)
	if err != nil {
		return core.AccountGroup{}, err
	}
	return core.NormalizeAccountGroupBilling(group), nil
}

func (r *SQLiteRepository) encodeClientPayload(client core.APIClient) (string, error) {
	client.AccountGroup = core.NormalizeAccountGroupName(client.AccountGroup)
	client.BillingSource = core.NormalizeClientBillingSource(client.BillingSource)
	encoded, err := r.codec.EncryptClient(client)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(encoded)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func (r *SQLiteRepository) decodeClientPayload(payload string) (core.APIClient, error) {
	var client core.APIClient
	if err := json.Unmarshal([]byte(payload), &client); err != nil {
		return core.APIClient{}, err
	}
	client, err := r.codec.DecryptClient(client)
	if err != nil {
		return core.APIClient{}, err
	}
	client.AccountGroup = core.NormalizeAccountGroupName(client.AccountGroup)
	client.BillingSource = core.NormalizeClientBillingSource(client.BillingSource)
	return client, nil
}

func (r *SQLiteRepository) encodeSystemSettingsPayload(settings core.SystemSettings) (string, error) {
	encoded, err := r.codec.EncryptSystemSettings(settings)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(encoded)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func (r *SQLiteRepository) decodeSystemSettingsPayload(payload string) (core.SystemSettings, error) {
	var settings core.SystemSettings
	if err := json.Unmarshal([]byte(payload), &settings); err != nil {
		return core.SystemSettings{}, err
	}
	settings, err := r.codec.DecryptSystemSettings(settings)
	if err != nil {
		return core.SystemSettings{}, err
	}
	return core.NormalizeSystemSettings(settings), nil
}

func (r *SQLiteRepository) decodeStartupSystemSettingsPayload(payload string) (core.SystemSettings, error) {
	var settings core.SystemSettings
	if err := json.Unmarshal([]byte(payload), &settings); err != nil {
		return core.SystemSettings{}, err
	}
	settings = core.StartupSystemSettingsFrom(settings)
	if !settings.Payment.PersonalPay.Enabled {
		settings.Payment.PersonalPay.AndroidToken = ""
		return core.StartupSystemSettingsFrom(settings), nil
	}
	var err error
	settings.Payment.PersonalPay.AndroidToken, err = r.codec.decryptValue(settings.Payment.PersonalPay.AndroidToken)
	if err != nil {
		return core.SystemSettings{}, err
	}
	return core.StartupSystemSettingsFrom(settings), nil
}

func clientAPIKeyHash(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])
}

func (r *SQLiteRepository) validateCredentialsLocked() error {
	encryptedSample, err := r.encryptedCredentialSampleLocked()
	if err != nil {
		return err
	}
	return validateEncryptedCredentialValue(r.codec, encryptedSample)
}

func (r *SQLiteRepository) encryptedCredentialSampleLocked() (string, error) {
	encryptedSample, err := r.validateAccountCredentialsPayloadsLocked()
	if err != nil {
		return "", err
	}
	if sample, err := r.validateClientCredentialsPayloadsLocked(); err != nil {
		return "", err
	} else if encryptedSample == "" {
		encryptedSample = sample
	}
	if sample, err := r.validateAccountGroupCredentialsPayloadsLocked(); err != nil {
		return "", err
	} else if encryptedSample == "" {
		encryptedSample = sample
	}
	if sample, err := r.validateSystemSettingsCredentialsPayloadsLocked(); err != nil {
		return "", err
	} else if encryptedSample == "" {
		encryptedSample = sample
	}
	return encryptedSample, nil
}

func (r *SQLiteRepository) validateAccountCredentialsPayloadsLocked() (string, error) {
	payload, err := r.firstEncryptedPayloadLocked("account_credentials")
	if err != nil || payload == "" {
		return "", err
	}
	var raw storedAccountCredentialState
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return "", err
	}
	if isEncryptedValue(raw.ProxyURL) {
		return raw.ProxyURL, nil
	}
	return encryptedCredentialSample(raw.Credential), nil
}

func (r *SQLiteRepository) validateAccountGroupCredentialsPayloadsLocked() (string, error) {
	payload, err := r.firstEncryptedPayloadLocked("account_groups")
	if err != nil || payload == "" {
		return "", err
	}
	var raw core.AccountGroup
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return "", err
	}
	return encryptedAccountGroupSample(raw), nil
}

func (r *SQLiteRepository) validateSystemSettingsCredentialsPayloadsLocked() (string, error) {
	payload, err := r.firstEncryptedPayloadLocked("system_settings")
	if err != nil || payload == "" {
		return "", err
	}
	var raw core.SystemSettings
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return "", err
	}
	return encryptedSystemSettingsSample(raw), nil
}

func (r *SQLiteRepository) validateClientCredentialsPayloadsLocked() (string, error) {
	payload, err := r.firstEncryptedPayloadLocked("clients")
	if err != nil || payload == "" {
		return "", err
	}
	var raw core.APIClient
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return "", err
	}
	return encryptedClientSample(raw), nil
}

func (r *SQLiteRepository) firstEncryptedPayloadLocked(table string) (string, error) {
	var payload string
	err := r.db.QueryRow(
		fmt.Sprintf(`SELECT payload FROM %s WHERE payload LIKE ? LIMIT 1`, table),
		"%"+encryptedValuePrefixV3+"%",
	).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return payload, err
}
