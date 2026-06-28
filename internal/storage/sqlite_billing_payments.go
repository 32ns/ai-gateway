package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func (r *SQLiteRepository) GetClientSpend(clientID string) (core.ClientSpend, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var spend core.ClientSpend
	var updatedAtNS int64
	err := r.db.QueryRow(
		`SELECT client_id, spend_limit_nano_usd, spend_used_nano_usd, updated_at_ns FROM client_spend WHERE client_id = ?`,
		clientID,
	).Scan(&spend.ClientID, &spend.SpendLimitNanoUSD, &spend.SpendUsedNanoUSD, &updatedAtNS)
	if errors.Is(err, sql.ErrNoRows) {
		payload, err := r.getPayloadByID("clients", clientID)
		if err != nil {
			return core.ClientSpend{}, err
		}
		client, err := r.decodeClientPayload(payload)
		if err != nil {
			return core.ClientSpend{}, err
		}
		return core.ClientSpend{ClientID: client.ID, SpendLimitNanoUSD: client.SpendLimitNanoUSD}, nil
	}
	if err != nil {
		return core.ClientSpend{}, err
	}
	spend.UpdatedAt = timeFromNS(updatedAtNS)
	return spend, nil
}

func (r *SQLiteRepository) GetClientActualSpend(clientID string) (core.ClientSpend, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	clientID = strings.TrimSpace(clientID)
	payload, err := r.getPayloadByID("clients", clientID)
	if err != nil {
		return core.ClientSpend{}, err
	}
	client, err := r.decodeClientPayload(payload)
	if err != nil {
		return core.ClientSpend{}, err
	}

	spend := core.ClientSpend{
		ClientID:          client.ID,
		SpendLimitNanoUSD: client.SpendLimitNanoUSD,
	}
	var updatedAtNS int64
	if err := r.db.QueryRow(`SELECT spend_used_nano_usd, updated_at_ns FROM client_spend WHERE client_id = ?`, clientID).
		Scan(&spend.SpendUsedNanoUSD, &updatedAtNS); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return core.ClientSpend{}, err
	}
	spend.UpdatedAt = timeFromNS(updatedAtNS)
	return spend, nil
}

func (r *SQLiteRepository) ListClientActualSpends() []core.ClientSpend {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`
		SELECT client_id, spend_limit_nano_usd, spend_used_nano_usd, updated_at_ns
		FROM client_spend
		ORDER BY client_id ASC
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]core.ClientSpend, 0)
	for rows.Next() {
		var spend core.ClientSpend
		var updatedAtNS int64
		if err := rows.Scan(&spend.ClientID, &spend.SpendLimitNanoUSD, &spend.SpendUsedNanoUSD, &updatedAtNS); err != nil {
			continue
		}
		spend.ClientID = strings.TrimSpace(spend.ClientID)
		if spend.ClientID == "" {
			continue
		}
		spend.UpdatedAt = timeFromNS(updatedAtNS)
		out = append(out, spend)
	}
	return out
}

func (r *SQLiteRepository) ReserveBilling(input core.BillingReservationInput) (core.BillingReservation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	input.RequestID = strings.TrimSpace(input.RequestID)
	input.ClientID = strings.TrimSpace(input.ClientID)
	input.UserID = strings.TrimSpace(input.UserID)
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.AccountLabel = strings.TrimSpace(input.AccountLabel)
	input.Model = strings.TrimSpace(input.Model)
	input.Fingerprint = strings.TrimSpace(input.Fingerprint)
	input.BillingSource = core.NormalizeClientBillingSource(input.BillingSource)
	if input.RequestID == "" || input.ClientID == "" || input.UserID == "" {
		return core.BillingReservation{}, fmt.Errorf("billing request, client, and user are required")
	}
	// Reservations only anchor a pending billing request; usage is charged on settlement.
	input.ReservedNanoUSD = 0

	tx, err := r.db.Begin()
	if err != nil {
		return core.BillingReservation{}, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	if err := r.maybeTrimBillingRequestsTx(tx, now); err != nil {
		return core.BillingReservation{}, err
	}

	existing, ok, err := getBillingRequestTx(tx, input.RequestID, input.ClientID)
	if err != nil {
		return core.BillingReservation{}, err
	}
	if ok {
		if strings.TrimSpace(existing.Fingerprint) != input.Fingerprint {
			return core.BillingReservation{}, ErrBillingRequestConflict
		}
		return existing, nil
	}

	var balance int64
	err = tx.QueryRow(`SELECT balance_nano_usd FROM user_balances WHERE user_id = ?`, input.UserID).Scan(&balance)
	if errors.Is(err, sql.ErrNoRows) {
		return core.BillingReservation{}, ErrNotFound
	}
	if err != nil {
		return core.BillingReservation{}, err
	}
	var client core.APIClient
	if clientPayload, err := r.getPayloadByIDTx(tx, "clients", input.ClientID); err != nil {
		return core.BillingReservation{}, err
	} else {
		client, err = r.decodeClientPayload(clientPayload)
		if err != nil {
			return core.BillingReservation{}, err
		}
		if ownerID := strings.TrimSpace(client.OwnerUserID); ownerID != "" && ownerID != input.UserID {
			return core.BillingReservation{}, ErrBillingClientOwnerMismatch
		}
	}
	var planEntitlement core.UserPlanEntitlement
	if input.BillingSource == core.ClientBillingSourcePlan {
		activeEntitlements, err := activeUserPlanEntitlementsTx(tx, input.UserID, now)
		if err != nil {
			return core.BillingReservation{}, err
		}
		var hasEntitlement bool
		planEntitlement, hasEntitlement = selectPlanEntitlementForReservation(activeEntitlements, 1)
		if !hasEntitlement {
			return core.BillingReservation{}, ErrPlanQuotaExhausted
		}
	} else if balance <= 0 {
		return core.BillingReservation{}, ErrInsufficientBalance
	}
	var spendUsedNanoUSD int64
	var spendUpdatedAtNS int64
	err = tx.QueryRow(`SELECT spend_used_nano_usd, updated_at_ns FROM client_spend WHERE client_id = ?`, input.ClientID).Scan(&spendUsedNanoUSD, &spendUpdatedAtNS)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return core.BillingReservation{}, err
		}
	}
	if client.SpendLimitNanoUSD > 0 && spendUsedNanoUSD >= client.SpendLimitNanoUSD {
		return core.BillingReservation{}, ErrClientSpendLimitExceeded
	}

	reservation := core.BillingReservation{
		ID:                            billingRequestID(input.RequestID, input.ClientID, now),
		RequestID:                     input.RequestID,
		ClientID:                      input.ClientID,
		ClientName:                    strings.TrimSpace(input.ClientName),
		UserID:                        input.UserID,
		AccountID:                     input.AccountID,
		AccountLabel:                  input.AccountLabel,
		FailedAccountLabels:           compactBillingAccountLabels(input.FailedAccountLabels),
		AccountGroup:                  strings.TrimSpace(input.AccountGroup),
		AccountGroupMultiplierBps:     input.AccountGroupMultiplierBps,
		BillingSource:                 input.BillingSource,
		Provider:                      input.Provider,
		Model:                         input.Model,
		FastMode:                      input.FastMode,
		Status:                        core.BillingRequestReserved,
		EstimatedPromptTokens:         input.EstimatedPromptTokens,
		EstimatedCompletionTokens:     input.EstimatedCompletionTokens,
		InputPriceNanoUSDPer1M:        input.InputPriceNanoUSDPer1M,
		CachedInputPriceNanoUSDPer1M:  input.CachedInputPriceNanoUSDPer1M,
		CacheWritePriceNanoUSDPer1M:   input.CacheWritePriceNanoUSDPer1M,
		CacheWrite5mPriceNanoUSDPer1M: input.CacheWrite5mPriceNanoUSDPer1M,
		CacheWrite1hPriceNanoUSDPer1M: input.CacheWrite1hPriceNanoUSDPer1M,
		OutputPriceNanoUSDPer1M:       input.OutputPriceNanoUSDPer1M,
		ImageOutputPriceNanoUSDPer1M:  input.ImageOutputPriceNanoUSDPer1M,
		ReservedNanoUSD:               input.ReservedNanoUSD,
		Fingerprint:                   input.Fingerprint,
		CacheDiagnostics:              input.CacheDiagnostics,
		CreatedAt:                     now,
	}
	if err := insertBillingRequestTx(tx, reservation); err != nil {
		return core.BillingReservation{}, err
	}
	if err := addFinanceModelRollupTx(tx, reservation.Model, 1, 0, 0, billingModelUsageSpendAmount(reservation.Status, reservation.ReservedNanoUSD, reservation.ActualNanoUSD)); err != nil {
		return core.BillingReservation{}, err
	}
	if strings.TrimSpace(planEntitlement.ID) != "" {
		if err := insertBillingFundingAllocationTx(tx, core.BillingFundingAllocation{
			ID:            billingAllocationID(input.RequestID, input.ClientID, core.BillingFundingSourcePlan, planEntitlement.ID),
			RequestID:     input.RequestID,
			ClientID:      input.ClientID,
			UserID:        input.UserID,
			Source:        core.BillingFundingSourcePlan,
			EntitlementID: planEntitlement.ID,
			CreatedAt:     now,
			UpdatedAt:     now,
		}); err != nil {
			return core.BillingReservation{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return core.BillingReservation{}, err
	}
	return reservation, nil
}

func (r *SQLiteRepository) UpdateBillingAccount(input core.BillingAccountUpdateInput) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	input.RequestID = strings.TrimSpace(input.RequestID)
	input.ClientID = strings.TrimSpace(input.ClientID)
	if input.RequestID == "" || input.ClientID == "" {
		return fmt.Errorf("billing request and client are required")
	}
	_, err := r.db.Exec(`
		UPDATE billing_requests
		SET account_id = ?,
			account_label = ?,
			failed_account_labels = ?
		WHERE request_id = ? AND client_id = ? AND status = ?
	`,
		strings.TrimSpace(input.AccountID),
		strings.TrimSpace(input.AccountLabel),
		encodeBillingAccountLabels(input.FailedAccountLabels),
		input.RequestID,
		input.ClientID,
		string(core.BillingRequestReserved),
	)
	return err
}

func (r *SQLiteRepository) SettleBilling(input core.BillingSettlementInput) (core.BillingSettlement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	input.RequestID = strings.TrimSpace(input.RequestID)
	input.ClientID = strings.TrimSpace(input.ClientID)
	if input.ActualNanoUSD < 0 {
		input.ActualNanoUSD = 0
	}

	tx, err := r.db.Begin()
	if err != nil {
		return core.BillingSettlement{}, err
	}
	defer tx.Rollback()

	request, ok, err := getBillingRequestTx(tx, input.RequestID, input.ClientID)
	if err != nil {
		return core.BillingSettlement{}, err
	}
	if !ok {
		return core.BillingSettlement{}, ErrNotFound
	}
	if request.Status != core.BillingRequestReserved {
		return core.BillingSettlement{Request: request}, nil
	}
	previousRequest := request
	billingSource := core.NormalizeClientBillingSource(request.BillingSource)
	if strings.TrimSpace(request.BillingSource) == "" {
		billingSource = core.NormalizeClientBillingSource(input.BillingSource)
	}
	var balance int64
	if err := tx.QueryRow(`SELECT balance_nano_usd FROM user_balances WHERE user_id = ?`, request.UserID).Scan(&balance); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.BillingSettlement{}, ErrNotFound
		}
		return core.BillingSettlement{}, err
	}

	now := time.Now().UTC()
	chargeNanoUSD := input.ActualNanoUSD
	spend, err := r.getOrCreateClientSpendTx(tx, request.ClientID)
	if err != nil {
		return core.BillingSettlement{}, err
	}
	nextSpend := spend.SpendUsedNanoUSD
	if chargeNanoUSD != 0 {
		nextSpend, err = addNanoUSD(spend.SpendUsedNanoUSD, chargeNanoUSD)
		if err != nil {
			return core.BillingSettlement{}, ErrAmountOverflow
		}
		if nextSpend < 0 {
			nextSpend = 0
		}
	}
	cashDelta := int64(0)
	planUsageDelta := int64(0)
	if chargeNanoUSD > 0 {
		if billingSource == core.ClientBillingSourcePlan {
			var entitlement core.UserPlanEntitlement
			hasEntitlement := false
			allocations, err := listBillingFundingAllocationsTx(tx, request.RequestID, request.ClientID)
			if err != nil {
				return core.BillingSettlement{}, err
			}
			for _, allocation := range allocations {
				if allocation.Source != core.BillingFundingSourcePlan || strings.TrimSpace(allocation.EntitlementID) == "" {
					continue
				}
				entitlement, hasEntitlement, err = getUserPlanEntitlementByIDTx(tx, allocation.EntitlementID)
				if err != nil {
					return core.BillingSettlement{}, err
				}
				if hasEntitlement {
					break
				}
			}
			if !hasEntitlement {
				entitlement, hasEntitlement, err = activeUserPlanEntitlementTx(tx, request.UserID, now)
				if err != nil {
					return core.BillingSettlement{}, err
				}
			}
			if !hasEntitlement {
				return core.BillingSettlement{}, ErrPlanQuotaExhausted
			}
			planDelta := int64(0)
			planDelta = chargeNanoUSD
			if planDelta > 0 {
				planUsageDelta = planDelta
				entitlement.CurrentQuotaNanoUSD -= planDelta
				entitlement.UpdatedAt = now
				if err := updateUserPlanEntitlementTx(tx, entitlement); err != nil {
					return core.BillingSettlement{}, err
				}
				if err := upsertBillingFundingAllocationActualDeltaTx(tx, request.RequestID, request.ClientID, request.UserID, core.BillingFundingSourcePlan, entitlement.ID, planDelta, now); err != nil {
					return core.BillingSettlement{}, err
				}
				if err := insertPlanQuotaLedgerTx(tx, core.PlanQuotaLedgerEntry{
					ID:                planQuotaLedgerID("settle", entitlement.ID, request.RequestID, request.ClientID, now, 0),
					EntitlementID:     entitlement.ID,
					UserID:            request.UserID,
					ClientID:          request.ClientID,
					RequestID:         request.RequestID,
					Kind:              "settle",
					AmountNanoUSD:     -planDelta,
					QuotaAfterNanoUSD: entitlement.CurrentQuotaNanoUSD,
					Note:              request.Model,
					CreatedAt:         now,
				}); err != nil {
					return core.BillingSettlement{}, err
				}
			}
		} else {
			cashDelta = chargeNanoUSD
			if err := upsertBillingFundingAllocationActualDeltaTx(tx, request.RequestID, request.ClientID, request.UserID, core.BillingFundingSourceCash, "", cashDelta, now); err != nil {
				return core.BillingSettlement{}, err
			}
		}
	}
	balanceAfter := balance
	if cashDelta != 0 {
		balanceAfter, err = addNanoUSD(balance, -cashDelta)
		if err != nil {
			return core.BillingSettlement{}, err
		}
	}
	if _, err := tx.Exec(`UPDATE user_balances SET balance_nano_usd = ?, updated_at_ns = ? WHERE user_id = ?`, balanceAfter, now.UnixNano(), request.UserID); err != nil {
		return core.BillingSettlement{}, err
	}
	if _, err := tx.Exec(
		`UPDATE client_spend
		SET spend_used_nano_usd = ?,
			updated_at_ns = ?
		WHERE client_id = ?`,
		nextSpend,
		now.UnixNano(),
		request.ClientID,
	); err != nil {
		return core.BillingSettlement{}, err
	}
	if err := setFinanceUserRollupBalanceTx(tx, request.UserID, balanceAfter); err != nil {
		return core.BillingSettlement{}, err
	}
	if err := addFinanceUserSpendRollupTx(tx, request.UserID, cashDelta, now); err != nil {
		return core.BillingSettlement{}, err
	}
	if err := addFinanceUserUsageRollupTx(tx, request.UserID, chargeNanoUSD, planUsageDelta, now); err != nil {
		return core.BillingSettlement{}, err
	}
	if err := addFinanceClientSpendRollupTx(tx, request.ClientID, request.ClientName, request.UserID, spend.SpendLimitNanoUSD, cashDelta); err != nil {
		return core.BillingSettlement{}, err
	}
	if err := addFinanceClientUsageRollupTx(tx, request.ClientID, request.ClientName, request.UserID, spend.SpendLimitNanoUSD, chargeNanoUSD, planUsageDelta); err != nil {
		return core.BillingSettlement{}, err
	}

	request.AccountID = strings.TrimSpace(input.AccountID)
	if label := strings.TrimSpace(input.AccountLabel); label != "" || strings.TrimSpace(request.AccountLabel) == "" {
		request.AccountLabel = label
	}
	request.AccountGroup = strings.TrimSpace(input.AccountGroup)
	request.AccountGroupMultiplierBps = input.AccountGroupMultiplierBps
	request.BillingSource = billingSource
	request.Provider = input.Provider
	if strings.TrimSpace(input.Model) != "" {
		request.Model = strings.TrimSpace(input.Model)
	}
	request.FastMode = request.FastMode || input.FastMode
	request.PromptTokens = input.Usage.PromptTokens
	request.CachedPromptTokens = input.Usage.CachedPromptTokens
	request.CacheCreationTokens = input.Usage.CacheCreationTokens
	request.CacheCreation5mTokens = input.Usage.CacheCreation5mTokens
	request.CacheCreation1hTokens = input.Usage.CacheCreation1hTokens
	request.CompletionTokens = input.Usage.CompletionTokens
	request.ImageOutputTokens = input.Usage.ImageOutputTokens
	request.TotalTokens = input.Usage.TotalTokens
	request.InputPriceNanoUSDPer1M = input.InputPriceNanoUSDPer1M
	request.CachedInputPriceNanoUSDPer1M = input.CachedInputPriceNanoUSDPer1M
	request.CacheWritePriceNanoUSDPer1M = input.CacheWritePriceNanoUSDPer1M
	request.CacheWrite5mPriceNanoUSDPer1M = input.CacheWrite5mPriceNanoUSDPer1M
	request.CacheWrite1hPriceNanoUSDPer1M = input.CacheWrite1hPriceNanoUSDPer1M
	request.OutputPriceNanoUSDPer1M = input.OutputPriceNanoUSDPer1M
	request.ImageOutputPriceNanoUSDPer1M = input.ImageOutputPriceNanoUSDPer1M
	request.ActualNanoUSD = input.ActualNanoUSD
	request.FirstTokenMS = input.FirstTokenMS
	request.Status = core.BillingRequestSettled
	if input.MissingUsage {
		request.Status = core.BillingRequestUsageMissing
	}
	request.SettledAt = &now
	if err := updateBillingRequestSettlementTx(tx, request); err != nil {
		return core.BillingSettlement{}, err
	}
	if err := updateFinanceModelRollupForBillingChangeTx(tx, previousRequest, request); err != nil {
		return core.BillingSettlement{}, err
	}
	if err := updateFinanceTokenDailyRollupForBillingChangeTx(tx, previousRequest, request); err != nil {
		return core.BillingSettlement{}, err
	}
	if cashDelta != 0 {
		if err := insertBillingLedgerTx(tx, core.BillingLedgerEntry{
			ID:                  billingLedgerID("settle", request.RequestID, request.ClientID, now),
			UserID:              request.UserID,
			ClientID:            request.ClientID,
			RequestID:           request.RequestID,
			Kind:                "settle",
			AmountNanoUSD:       -cashDelta,
			BalanceAfterNanoUSD: balanceAfter,
			Note:                request.Model,
			CreatedAt:           now,
		}); err != nil {
			return core.BillingSettlement{}, err
		}
	}
	if err := r.maybeTrimBillingRequestsTx(tx, now); err != nil {
		return core.BillingSettlement{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.BillingSettlement{}, err
	}
	return core.BillingSettlement{Request: request, DeltaNanoUSD: chargeNanoUSD, BalanceAfterNanoUSD: balanceAfter}, nil
}

func (r *SQLiteRepository) ReleaseBilling(input core.BillingReleaseInput) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	input.RequestID = strings.TrimSpace(input.RequestID)
	input.ClientID = strings.TrimSpace(input.ClientID)
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	request, ok, err := getBillingRequestTx(tx, input.RequestID, input.ClientID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if request.Status != core.BillingRequestReserved {
		return nil
	}
	now := time.Now().UTC()
	request.Status = core.BillingRequestReleased
	request.SettledAt = &now
	if err := updateBillingRequestSettlementTx(tx, request); err != nil {
		return err
	}
	if err := r.maybeTrimBillingRequestsTx(tx, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *SQLiteRepository) ListBillingLedger(userID string, limit int) []core.BillingLedgerEntry {
	now := time.Now().UTC()
	r.mu.Lock()
	if err := r.maybeTrimBillingLedgerLocked(now); err != nil {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	r.mu.RLock()
	defer r.mu.RUnlock()

	if limit <= 0 {
		limit = 100
	}
	query := `SELECT id, user_id, client_id, request_id, kind, amount_nano_usd, balance_after_nano_usd, note, created_at_ns FROM billing_ledger`
	args := []any{}
	if strings.TrimSpace(userID) != "" {
		query += ` WHERE user_id = ?`
		args = append(args, strings.TrimSpace(userID))
	}
	query += ` ORDER BY seq DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]core.BillingLedgerEntry, 0, limit)
	for rows.Next() {
		var entry core.BillingLedgerEntry
		var createdAtNS int64
		if err := rows.Scan(&entry.ID, &entry.UserID, &entry.ClientID, &entry.RequestID, &entry.Kind, &entry.AmountNanoUSD, &entry.BalanceAfterNanoUSD, &entry.Note, &createdAtNS); err != nil {
			continue
		}
		entry.CreatedAt = timeFromNS(createdAtNS)
		out = append(out, entry)
	}
	return out
}

func (r *SQLiteRepository) BillingUsageSpendNanoUSD(startedAt, endedAt time.Time) int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	query := `
		SELECT COALESCE(SUM(CASE
			WHEN status IN (?, ?, ?) THEN 0
			WHEN status = ? AND actual_nano_usd > 0 THEN actual_nano_usd
			WHEN actual_nano_usd > 0 THEN actual_nano_usd
			ELSE 0
		END), 0)
		FROM billing_requests
		WHERE 1 = 1
	`
	args := []any{
		string(core.BillingRequestReleased),
		string(core.BillingRequestUsageMissing),
		string(core.BillingRequestReserved),
		string(core.BillingRequestSettled),
	}
	if !startedAt.IsZero() {
		query += ` AND created_at_ns >= ?`
		args = append(args, sqliteTimeNS(startedAt))
	}
	if !endedAt.IsZero() {
		query += ` AND created_at_ns < ?`
		args = append(args, sqliteTimeNS(endedAt))
	}
	var total int64
	if err := r.db.QueryRow(query, args...).Scan(&total); err != nil {
		return 0
	}
	ledgerQuery := `
		SELECT COALESCE(SUM(-amount_nano_usd), 0)
		FROM billing_ledger bl
		WHERE bl.request_id <> ''
			AND bl.client_id <> ''
			AND bl.kind = 'settle'
			AND NOT EXISTS (
				SELECT 1 FROM billing_requests br
				WHERE br.request_id = bl.request_id AND br.client_id = bl.client_id
			)
	`
	ledgerArgs := []any{}
	if !startedAt.IsZero() {
		ledgerQuery += ` AND bl.created_at_ns >= ?`
		ledgerArgs = append(ledgerArgs, sqliteTimeNS(startedAt))
	}
	if !endedAt.IsZero() {
		ledgerQuery += ` AND bl.created_at_ns < ?`
		ledgerArgs = append(ledgerArgs, sqliteTimeNS(endedAt))
	}
	var ledgerTotal int64
	_ = r.db.QueryRow(ledgerQuery, ledgerArgs...).Scan(&ledgerTotal)
	total = addNanoUSDSaturating(total, ledgerTotal)
	planQuery := `
		SELECT COALESCE(SUM(-amount_nano_usd), 0)
		FROM plan_quota_ledger pql
		WHERE pql.request_id <> ''
			AND pql.client_id <> ''
			AND pql.kind = 'settle'
			AND NOT EXISTS (
				SELECT 1 FROM billing_requests br
				WHERE br.request_id = pql.request_id AND br.client_id = pql.client_id
			)
	`
	planArgs := []any{}
	if !startedAt.IsZero() {
		planQuery += ` AND pql.created_at_ns >= ?`
		planArgs = append(planArgs, sqliteTimeNS(startedAt))
	}
	if !endedAt.IsZero() {
		planQuery += ` AND pql.created_at_ns < ?`
		planArgs = append(planArgs, sqliteTimeNS(endedAt))
	}
	var planTotal int64
	_ = r.db.QueryRow(planQuery, planArgs...).Scan(&planTotal)
	total = addNanoUSDSaturating(total, planTotal)
	return total
}

func (r *SQLiteRepository) ListBillingUsageSpendByDay(startedAt time.Time, days int) []BillingUsageSpendDaySummary {
	if days <= 0 || startedAt.IsZero() {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	endedAt := startedAt.AddDate(0, 0, days)
	rows, err := r.db.Query(`
		SELECT created_at_ns,
			CASE
				WHEN status IN (?, ?, ?) THEN 0
				WHEN status = ? AND actual_nano_usd > 0 THEN actual_nano_usd
				WHEN actual_nano_usd > 0 THEN actual_nano_usd
				ELSE 0
			END
		FROM billing_requests
		WHERE created_at_ns >= ?
			AND created_at_ns < ?
	`, string(core.BillingRequestReleased), string(core.BillingRequestUsageMissing), string(core.BillingRequestReserved), string(core.BillingRequestSettled), sqliteTimeNS(startedAt), sqliteTimeNS(endedAt))
	if err != nil {
		return nil
	}
	defer rows.Close()

	byDate := make(map[string]int64, days)
	for rows.Next() {
		var createdAtNS int64
		var amountNanoUSD int64
		if err := rows.Scan(&createdAtNS, &amountNanoUSD); err != nil {
			continue
		}
		if amountNanoUSD == 0 {
			continue
		}
		date := billingDayKey(timeFromNS(createdAtNS))
		if date == "" {
			continue
		}
		byDate[date] = addNanoUSDSaturating(byDate[date], amountNanoUSD)
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	if err := rows.Close(); err != nil {
		return nil
	}
	rows, err = r.db.Query(`
		SELECT bl.created_at_ns, -bl.amount_nano_usd
		FROM billing_ledger bl
		WHERE bl.request_id <> ''
			AND bl.client_id <> ''
			AND bl.kind = 'settle'
			AND bl.created_at_ns >= ?
			AND bl.created_at_ns < ?
			AND NOT EXISTS (
				SELECT 1 FROM billing_requests br
				WHERE br.request_id = bl.request_id AND br.client_id = bl.client_id
			)
	`, sqliteTimeNS(startedAt), sqliteTimeNS(endedAt))
	if err != nil {
		return nil
	}
	for rows.Next() {
		var createdAtNS int64
		var amountNanoUSD int64
		if err := rows.Scan(&createdAtNS, &amountNanoUSD); err != nil {
			continue
		}
		if amountNanoUSD == 0 {
			continue
		}
		date := billingDayKey(timeFromNS(createdAtNS))
		if date == "" {
			continue
		}
		byDate[date] = addNanoUSDSaturating(byDate[date], amountNanoUSD)
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	if err := rows.Close(); err != nil {
		return nil
	}
	rows, err = r.db.Query(`
		SELECT pql.created_at_ns, -pql.amount_nano_usd
		FROM plan_quota_ledger pql
		WHERE pql.request_id <> ''
			AND pql.client_id <> ''
			AND pql.kind = 'settle'
			AND pql.created_at_ns >= ?
			AND pql.created_at_ns < ?
			AND NOT EXISTS (
				SELECT 1 FROM billing_requests br
				WHERE br.request_id = pql.request_id AND br.client_id = pql.client_id
			)
	`, sqliteTimeNS(startedAt), sqliteTimeNS(endedAt))
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var createdAtNS int64
		var amountNanoUSD int64
		if err := rows.Scan(&createdAtNS, &amountNanoUSD); err != nil {
			continue
		}
		if amountNanoUSD == 0 {
			continue
		}
		date := billingDayKey(timeFromNS(createdAtNS))
		if date == "" {
			continue
		}
		byDate[date] = addNanoUSDSaturating(byDate[date], amountNanoUSD)
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	out := make([]BillingUsageSpendDaySummary, 0, len(byDate))
	for date, spend := range byDate {
		out = append(out, BillingUsageSpendDaySummary{Date: date, SpendNanoUSD: spend})
	}
	slices.SortFunc(out, func(a, b BillingUsageSpendDaySummary) int {
		return strings.Compare(a.Date, b.Date)
	})
	return out
}

func (r *SQLiteRepository) ListBillingRequestCountByDay(startedAt time.Time, days int) []BillingRequestDayCountSummary {
	if days <= 0 || startedAt.IsZero() {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	endedAt := startedAt.AddDate(0, 0, days)
	rows, err := r.db.Query(`
		SELECT created_at_ns
		FROM billing_requests
		WHERE created_at_ns >= ? AND created_at_ns < ?
	`, sqliteTimeNS(startedAt), sqliteTimeNS(endedAt))
	if err != nil {
		return nil
	}
	defer rows.Close()

	byDate := make(map[string]int, days)
	for rows.Next() {
		var createdAtNS int64
		if err := rows.Scan(&createdAtNS); err != nil {
			continue
		}
		date := billingDayKey(timeFromNS(createdAtNS))
		if date == "" {
			continue
		}
		byDate[date]++
	}
	out := make([]BillingRequestDayCountSummary, 0, len(byDate))
	for date, count := range byDate {
		out = append(out, BillingRequestDayCountSummary{Date: date, Count: count})
	}
	slices.SortFunc(out, func(a, b BillingRequestDayCountSummary) int {
		return strings.Compare(a.Date, b.Date)
	})
	return out
}

func (r *SQLiteRepository) ListBillingUsageSpendByHourForUser(userID string, startedAt, endedAt time.Time) []BillingUsageSpendHourSummary {
	userID = strings.TrimSpace(userID)
	if userID == "" || startedAt.IsZero() || endedAt.IsZero() || !startedAt.Before(endedAt) {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`
		SELECT created_at_ns, status, reserved_nano_usd, actual_nano_usd
		FROM billing_requests
		WHERE user_id = ? AND created_at_ns >= ? AND created_at_ns < ?
	`, userID, sqliteTimeNS(startedAt), sqliteTimeNS(endedAt))
	if err != nil {
		return nil
	}
	defer rows.Close()

	byHour := make(map[int]int64)
	for rows.Next() {
		var createdAtNS int64
		var status string
		var reservedNanoUSD int64
		var actualNanoUSD int64
		if err := rows.Scan(&createdAtNS, &status, &reservedNanoUSD, &actualNanoUSD); err != nil {
			continue
		}
		amount := billingRequestUsageSpendAmount(core.BillingRequestStatus(status), reservedNanoUSD, actualNanoUSD)
		if amount <= 0 {
			continue
		}
		hour := timeFromNS(createdAtNS).Local().Hour()
		if hour < 0 || hour > 23 {
			continue
		}
		byHour[hour] = addNanoUSDSaturating(byHour[hour], amount)
	}
	out := make([]BillingUsageSpendHourSummary, 0, len(byHour))
	for hour, spend := range byHour {
		out = append(out, BillingUsageSpendHourSummary{Hour: hour, SpendNanoUSD: spend})
	}
	slices.SortFunc(out, func(a, b BillingUsageSpendHourSummary) int {
		return a.Hour - b.Hour
	})
	return out
}

func (r *SQLiteRepository) ListBillingUsageSpendByClient() []BillingUsageSpendSummary {
	return r.listBillingUsageSpendByClient("")
}

func (r *SQLiteRepository) ListBillingUsageSpendByClientForUser(userID string) []BillingUsageSpendSummary {
	return r.listBillingUsageSpendByClient(strings.TrimSpace(userID))
}

func (r *SQLiteRepository) listBillingUsageSpendByClient(userID string) []BillingUsageSpendSummary {
	r.mu.RLock()
	defer r.mu.RUnlock()

	byKey := make(map[string]BillingUsageSpendSummary)
	addSpend := func(userID, clientID string, amount int64) {
		userID = strings.TrimSpace(userID)
		clientID = strings.TrimSpace(clientID)
		if userID == "" || clientID == "" || amount == 0 {
			return
		}
		key := userID + "\x00" + clientID
		summary := byKey[key]
		summary.UserID = userID
		summary.ClientID = clientID
		next, err := addNanoUSD(summary.SpendNanoUSD, amount)
		if err != nil {
			if amount > 0 {
				next = maxInt64
			} else {
				next = minInt64
			}
		}
		summary.SpendNanoUSD = next
		byKey[key] = summary
	}

	requestQuery := `
		SELECT user_id, client_id,
			COALESCE(SUM(CASE
				WHEN status = ? AND actual_nano_usd > 0 THEN actual_nano_usd
				WHEN status NOT IN (?, ?, ?) AND actual_nano_usd > 0 THEN actual_nano_usd
				ELSE 0
			END), 0)
		FROM billing_requests
		WHERE client_id <> ''
	`
	requestArgs := []any{
		string(core.BillingRequestSettled),
		string(core.BillingRequestReserved),
		string(core.BillingRequestReleased),
		string(core.BillingRequestUsageMissing),
	}
	if userID != "" {
		requestQuery += ` AND user_id = ?`
		requestArgs = append(requestArgs, userID)
	}
	requestQuery += ` GROUP BY user_id, client_id`

	rows, err := r.db.Query(requestQuery, requestArgs...)
	if err != nil {
		return nil
	}
	for rows.Next() {
		var userID, clientID string
		var amountNanoUSD int64
		if err := rows.Scan(&userID, &clientID, &amountNanoUSD); err != nil {
			continue
		}
		addSpend(userID, clientID, amountNanoUSD)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil
	}
	if err := rows.Close(); err != nil {
		return nil
	}
	ledgerQuery := `
		SELECT bl.user_id, bl.client_id,
			COALESCE(SUM(-bl.amount_nano_usd), 0)
		FROM billing_ledger bl
		WHERE bl.request_id <> ''
			AND bl.client_id <> ''
			AND bl.kind = 'settle'
			AND NOT EXISTS (
				SELECT 1 FROM billing_requests br
				WHERE br.request_id = bl.request_id AND br.client_id = bl.client_id
			)
	`
	ledgerArgs := []any{}
	if userID != "" {
		ledgerQuery += ` AND bl.user_id = ?`
		ledgerArgs = append(ledgerArgs, userID)
	}
	ledgerQuery += ` GROUP BY bl.user_id, bl.client_id`
	rows, err = r.db.Query(ledgerQuery, ledgerArgs...)
	if err != nil {
		return nil
	}
	for rows.Next() {
		var userID, clientID string
		var amountNanoUSD int64
		if err := rows.Scan(&userID, &clientID, &amountNanoUSD); err != nil {
			continue
		}
		addSpend(userID, clientID, amountNanoUSD)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil
	}
	if err := rows.Close(); err != nil {
		return nil
	}
	planQuery := `
		SELECT pql.user_id, pql.client_id,
			COALESCE(SUM(-pql.amount_nano_usd), 0)
		FROM plan_quota_ledger pql
		WHERE pql.request_id <> ''
			AND pql.client_id <> ''
			AND pql.kind = 'settle'
			AND NOT EXISTS (
				SELECT 1 FROM billing_requests br
				WHERE br.request_id = pql.request_id AND br.client_id = pql.client_id
			)
	`
	planArgs := []any{}
	if userID != "" {
		planQuery += ` AND pql.user_id = ?`
		planArgs = append(planArgs, userID)
	}
	planQuery += ` GROUP BY pql.user_id, pql.client_id`
	rows, err = r.db.Query(planQuery, planArgs...)
	if err != nil {
		return nil
	}
	for rows.Next() {
		var userID, clientID string
		var amountNanoUSD int64
		if err := rows.Scan(&userID, &clientID, &amountNanoUSD); err != nil {
			continue
		}
		addSpend(userID, clientID, amountNanoUSD)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil
	}
	if err := rows.Close(); err != nil {
		return nil
	}

	out := make([]BillingUsageSpendSummary, 0, len(byKey))
	for _, summary := range byKey {
		if summary.SpendNanoUSD <= 0 {
			continue
		}
		out = append(out, summary)
	}
	slices.SortFunc(out, func(a, b BillingUsageSpendSummary) int {
		if a.UserID < b.UserID {
			return -1
		}
		if a.UserID > b.UserID {
			return 1
		}
		if a.ClientID < b.ClientID {
			return -1
		}
		if a.ClientID > b.ClientID {
			return 1
		}
		return 0
	})
	return out
}

func (r *SQLiteRepository) ListBillingLedgerUserSummaries() []BillingLedgerUserSummary {
	now := time.Now().UTC()
	r.mu.Lock()
	if err := r.maybeTrimBillingLedgerLocked(now); err != nil {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`
		SELECT user_id,
			COALESCE(SUM(CASE
				WHEN kind IN ('manual_credit', 'account_merge') THEN amount_nano_usd
				ELSE 0
			END), 0),
			COALESCE(SUM(CASE
				WHEN kind = 'manual_debit' AND amount_nano_usd < 0 THEN -amount_nano_usd
				WHEN kind = 'plan_purchase' AND amount_nano_usd < 0 THEN -amount_nano_usd
				ELSE 0
			END), 0),
			0,
			COALESCE(MAX(CASE
				WHEN kind = 'manual_debit' AND amount_nano_usd < 0 THEN created_at_ns
				WHEN kind = 'plan_purchase' AND amount_nano_usd < 0 THEN created_at_ns
				WHEN kind = 'settle' AND -amount_nano_usd > 0 THEN created_at_ns
				ELSE 0
			END), 0)
		FROM billing_ledger
		WHERE kind IN ('manual_credit', 'account_merge', 'manual_debit', 'plan_purchase', 'settle')
		GROUP BY user_id
		ORDER BY user_id ASC
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]BillingLedgerUserSummary, 0)
	for rows.Next() {
		var summary BillingLedgerUserSummary
		var lastSpendAtNS int64
		if err := rows.Scan(&summary.UserID, &summary.RewardNanoUSD, &summary.SpendNanoUSD, &summary.RefundNanoUSD, &lastSpendAtNS); err != nil {
			continue
		}
		if lastSpendAtNS > 0 {
			value := timeFromNS(lastSpendAtNS)
			summary.LastSpendAt = &value
		}
		out = append(out, summary)
	}
	return out
}

func (r *SQLiteRepository) ListBillingRequestsPage(query BillingRequestQuery) ([]core.BillingReservation, int) {
	query = normalizeBillingRequestQuery(query)

	now := time.Now().UTC()
	r.mu.Lock()
	if err := r.maybeTrimBillingRequestsLocked(now); err != nil {
		r.mu.Unlock()
		return nil, 0
	}
	r.mu.Unlock()

	r.mu.RLock()
	defer r.mu.RUnlock()

	where, args := billingRequestWhere(query)
	total := 0
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM billing_requests`+where, args...).Scan(&total); err != nil {
		return nil, 0
	}

	selectArgs := append([]any(nil), args...)
	selectArgs = append(selectArgs, query.Limit, query.Offset)
	rows, err := r.db.Query(`
		SELECT id, request_id, client_id, client_name, user_id, account_id, account_label, failed_account_labels, account_group, account_group_multiplier_bps, billing_source, provider, model, status,
			estimated_prompt_tokens, estimated_completion_tokens,
			prompt_tokens, cached_prompt_tokens, cache_creation_tokens, cache_creation_5m_tokens, cache_creation_1h_tokens, completion_tokens, image_output_tokens, total_tokens,
			input_price_nano_usd_per_1m, cached_input_price_nano_usd_per_1m, cache_write_price_nano_usd_per_1m, cache_write_5m_price_nano_usd_per_1m, cache_write_1h_price_nano_usd_per_1m, output_price_nano_usd_per_1m, image_output_price_nano_usd_per_1m,
			reserved_nano_usd, actual_nano_usd, first_token_ms, fingerprint, cache_diagnostics, fast_mode, created_at_ns, settled_at_ns
		FROM billing_requests`+where+`
		ORDER BY created_at_ns DESC, id ASC
		LIMIT ? OFFSET ?`, selectArgs...)
	if err != nil {
		return nil, total
	}
	defer rows.Close()

	out := make([]core.BillingReservation, 0, query.Limit)
	for rows.Next() {
		request, err := scanBillingRequest(rows)
		if err != nil {
			continue
		}
		out = append(out, request)
	}
	return out, total
}

func (r *SQLiteRepository) ListBillingRequests(query BillingRequestQuery) []core.BillingReservation {
	query = normalizeBillingRequestQuery(query)

	now := time.Now().UTC()
	r.mu.Lock()
	if err := r.maybeTrimBillingRequestsLocked(now); err != nil {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	r.mu.RLock()
	defer r.mu.RUnlock()

	where, args := billingRequestWhere(query)
	rows, err := r.db.Query(`
		SELECT id, request_id, client_id, client_name, user_id, account_id, account_label, failed_account_labels, account_group, account_group_multiplier_bps, billing_source, provider, model, status,
			estimated_prompt_tokens, estimated_completion_tokens,
			prompt_tokens, cached_prompt_tokens, cache_creation_tokens, cache_creation_5m_tokens, cache_creation_1h_tokens, completion_tokens, image_output_tokens, total_tokens,
			input_price_nano_usd_per_1m, cached_input_price_nano_usd_per_1m, cache_write_price_nano_usd_per_1m, cache_write_5m_price_nano_usd_per_1m, cache_write_1h_price_nano_usd_per_1m, output_price_nano_usd_per_1m, image_output_price_nano_usd_per_1m,
			reserved_nano_usd, actual_nano_usd, first_token_ms, fingerprint, cache_diagnostics, fast_mode, created_at_ns, settled_at_ns
		FROM billing_requests`+where+`
		ORDER BY created_at_ns DESC, id ASC`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]core.BillingReservation, 0)
	for rows.Next() {
		request, err := scanBillingRequest(rows)
		if err != nil {
			continue
		}
		out = append(out, request)
	}
	return out
}

func (r *SQLiteRepository) ListBillingModelUsageSummaries(limit int) []BillingModelUsageSummary {
	r.mu.RLock()
	defer r.mu.RUnlock()

	query := `
		SELECT model, request_count, prompt_tokens, completion_tokens, spend_nano_usd
		FROM finance_model_rollups
		ORDER BY spend_nano_usd DESC, model ASC
	`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]BillingModelUsageSummary, 0)
	for rows.Next() {
		var summary BillingModelUsageSummary
		if err := rows.Scan(&summary.Model, &summary.RequestCount, &summary.PromptTokens, &summary.CompletionTokens, &summary.SpendNanoUSD); err != nil {
			continue
		}
		out = append(out, summary)
	}
	return out
}

func (r *SQLiteRepository) CreatePaymentOrder(order core.PaymentOrder) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	order.ID = strings.TrimSpace(order.ID)
	order.OutTradeNo = strings.TrimSpace(order.OutTradeNo)
	order.UserID = strings.TrimSpace(order.UserID)
	if order.ID == "" || order.OutTradeNo == "" || order.UserID == "" {
		return fmt.Errorf("payment id, trade number, and user are required")
	}
	if order.AmountNanoUSD <= 0 {
		return ErrInsufficientBalance
	}
	if order.AmountNanoUSD%(core.NanoUSDPerUSD/100) != 0 {
		return fmt.Errorf("payment amount must be accurate to cents")
	}
	if _, err := r.getPayloadByID("users", order.UserID); err != nil {
		return err
	}
	now := time.Now().UTC()
	if order.CreatedAt.IsZero() {
		order.CreatedAt = now
	}
	order.UpdatedAt = now
	if order.Status == "" {
		order.Status = core.PaymentOrderPending
	}
	if strings.TrimSpace(order.Currency) == "" {
		order.Currency = "USD"
	}
	if strings.TrimSpace(order.ProviderCurrency) == "" {
		order.ProviderCurrency = "CNY"
	}
	payload, err := json.Marshal(order)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(
		`INSERT INTO payment_orders(id, out_trade_no, user_id, provider, status, amount_nano_usd, paid_at_ns, payload, created_at_ns, updated_at_ns)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		order.ID,
		order.OutTradeNo,
		order.UserID,
		string(order.Provider),
		string(order.Status),
		order.AmountNanoUSD,
		paymentOrderPaidAtNS(order),
		string(payload),
		sqliteTimeNS(order.CreatedAt),
		sqliteTimeNS(order.UpdatedAt),
	)
	return err
}

func (r *SQLiteRepository) GetPaymentOrder(id string) (core.PaymentOrder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.getPaymentOrderByColumn("id", strings.TrimSpace(id))
}

func (r *SQLiteRepository) GetPaymentOrderByOutTradeNo(outTradeNo string) (core.PaymentOrder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.getPaymentOrderByColumn("out_trade_no", strings.TrimSpace(outTradeNo))
}

func (r *SQLiteRepository) ListPaymentOrdersPage(query PaymentOrderQuery) ([]core.PaymentOrder, int) {
	query = normalizePaymentOrderQuery(query)

	r.mu.Lock()
	if err := expirePaymentOrdersDB(r.db, time.Now().UTC()); err != nil {
		r.mu.Unlock()
		return nil, 0
	}
	r.mu.Unlock()

	r.mu.RLock()
	defer r.mu.RUnlock()

	where, args := paymentOrderWhere(query)
	total := 0
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM payment_orders`+where, args...).Scan(&total); err != nil {
		return nil, 0
	}
	selectArgs := append([]any(nil), args...)
	selectArgs = append(selectArgs, query.Limit, query.Offset)
	rows, err := r.db.Query(`
		SELECT payload
		FROM payment_orders`+where+`
		ORDER BY created_at_ns DESC, id ASC
		LIMIT ? OFFSET ?`, selectArgs...)
	if err != nil {
		return nil, total
	}
	defer rows.Close()

	out := make([]core.PaymentOrder, 0, query.Limit)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		var order core.PaymentOrder
		if err := json.Unmarshal([]byte(payload), &order); err != nil {
			continue
		}
		out = append(out, clonePaymentOrder(order))
	}
	return out, total
}

func (r *SQLiteRepository) ListPaymentOrders(query PaymentOrderQuery) []core.PaymentOrder {
	query = normalizePaymentOrderQuery(query)

	r.mu.Lock()
	if err := expirePaymentOrdersDB(r.db, time.Now().UTC()); err != nil {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	r.mu.RLock()
	defer r.mu.RUnlock()

	where, args := paymentOrderWhere(query)
	rows, err := r.db.Query(`
		SELECT payload
		FROM payment_orders`+where+`
		ORDER BY created_at_ns DESC, id ASC`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]core.PaymentOrder, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		var order core.PaymentOrder
		if err := json.Unmarshal([]byte(payload), &order); err != nil {
			continue
		}
		out = append(out, clonePaymentOrder(order))
	}
	return out
}

func (r *SQLiteRepository) UpdatePaymentOrderStatus(id string, status core.PaymentOrderStatus, providerTradeNo string, paidAt *time.Time) (core.PaymentOrder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	order, err := r.getPaymentOrderByColumn("id", strings.TrimSpace(id))
	if err != nil {
		return core.PaymentOrder{}, err
	}
	if order.Status == core.PaymentOrderPaid {
		return order, nil
	}
	order.Status = status
	order.ProviderTradeNo = strings.TrimSpace(providerTradeNo)
	if paidAt != nil && !paidAt.IsZero() {
		value := paidAt.UTC()
		order.PaidAt = &value
	}
	order.UpdatedAt = time.Now().UTC()
	payload, err := json.Marshal(order)
	if err != nil {
		return core.PaymentOrder{}, err
	}
	if _, err := r.db.Exec(`UPDATE payment_orders SET status = ?, amount_nano_usd = ?, paid_at_ns = ?, payload = ?, updated_at_ns = ? WHERE id = ?`, string(order.Status), order.AmountNanoUSD, paymentOrderPaidAtNS(order), string(payload), sqliteTimeNS(order.UpdatedAt), order.ID); err != nil {
		return core.PaymentOrder{}, err
	}
	return clonePaymentOrder(order), nil
}

func (r *SQLiteRepository) UpdatePaymentOrderProviderState(id string, update core.PaymentOrderProviderUpdate) (core.PaymentOrder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	order, err := r.getPaymentOrderByColumn("id", strings.TrimSpace(id))
	if err != nil {
		return core.PaymentOrder{}, err
	}
	if order.Status == core.PaymentOrderPaid {
		return order, nil
	}
	if update.Status != "" {
		order.Status = update.Status
	}
	if strings.TrimSpace(update.ProviderStatus) != "" {
		order.ProviderStatus = strings.TrimSpace(update.ProviderStatus)
	}
	if strings.TrimSpace(update.ProviderTradeNo) != "" {
		order.ProviderTradeNo = strings.TrimSpace(update.ProviderTradeNo)
	}
	if strings.TrimSpace(update.CodeURL) != "" {
		order.CodeURL = strings.TrimSpace(update.CodeURL)
	}
	if strings.TrimSpace(update.PayURL) != "" {
		order.PayURL = strings.TrimSpace(update.PayURL)
	}
	if strings.TrimSpace(update.PrepayID) != "" {
		order.PrepayID = strings.TrimSpace(update.PrepayID)
	}
	if strings.TrimSpace(update.RawResponse) != "" {
		order.RawResponse = update.RawResponse
	}
	if update.PaidAt != nil && !update.PaidAt.IsZero() {
		value := update.PaidAt.UTC()
		order.PaidAt = &value
	}
	order.UpdatedAt = time.Now().UTC()
	payload, err := json.Marshal(order)
	if err != nil {
		return core.PaymentOrder{}, err
	}
	if _, err := r.db.Exec(`UPDATE payment_orders SET status = ?, amount_nano_usd = ?, paid_at_ns = ?, payload = ?, updated_at_ns = ? WHERE id = ?`, string(order.Status), order.AmountNanoUSD, paymentOrderPaidAtNS(order), string(payload), sqliteTimeNS(order.UpdatedAt), order.ID); err != nil {
		return core.PaymentOrder{}, err
	}
	return clonePaymentOrder(order), nil
}

func (r *SQLiteRepository) DeletePendingPaymentOrder(id string) (core.PaymentOrder, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = strings.TrimSpace(id)
	if id == "" {
		return core.PaymentOrder{}, false, ErrNotFound
	}
	var payload string
	err := r.db.QueryRow(`SELECT payload FROM payment_orders WHERE id = ?`, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return core.PaymentOrder{}, false, ErrNotFound
	}
	if err != nil {
		return core.PaymentOrder{}, false, err
	}
	var order core.PaymentOrder
	if err := json.Unmarshal([]byte(payload), &order); err != nil {
		return core.PaymentOrder{}, false, err
	}
	if order.Status != core.PaymentOrderPending {
		return clonePaymentOrder(order), false, nil
	}
	result, err := r.db.Exec(`DELETE FROM payment_orders WHERE id = ? AND status = ?`, order.ID, string(core.PaymentOrderPending))
	if err != nil {
		return core.PaymentOrder{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return core.PaymentOrder{}, false, err
	}
	if affected == 0 {
		current, currentErr := r.getPaymentOrderByColumn("id", id)
		if currentErr != nil {
			return order, false, nil
		}
		return clonePaymentOrder(current), false, nil
	}
	return clonePaymentOrder(order), true, nil
}

func (r *SQLiteRepository) CompletePaymentOrder(outTradeNo, providerTradeNo string, paidAmountNanoUSD int64, paidAt time.Time) (core.PaymentOrder, bool, error) {
	return r.CompletePaymentOrderWithCredits(outTradeNo, providerTradeNo, paidAmountNanoUSD, paidAt, nil)
}

func (r *SQLiteRepository) CompletePaymentOrderWithCredits(outTradeNo, providerTradeNo string, paidAmountNanoUSD int64, paidAt time.Time, credits []core.PaymentOrderBalanceCredit) (core.PaymentOrder, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	outTradeNo = strings.TrimSpace(outTradeNo)
	tx, err := r.db.Begin()
	if err != nil {
		return core.PaymentOrder{}, false, err
	}
	defer tx.Rollback()

	order, ok, err := getPaymentOrderByOutTradeNoTx(tx, outTradeNo)
	if err != nil {
		return core.PaymentOrder{}, false, err
	}
	if !ok {
		return core.PaymentOrder{}, false, ErrNotFound
	}
	if order.Status == core.PaymentOrderPaid {
		if !paidPaymentReplayMatches(order, providerTradeNo, paidAmountNanoUSD) {
			return core.PaymentOrder{}, false, ErrBillingRequestConflict
		}
		return order, false, nil
	}
	if paidAmountNanoUSD != order.AmountNanoUSD {
		return core.PaymentOrder{}, false, ErrBillingRequestConflict
	}
	var balance int64
	if err := tx.QueryRow(`SELECT balance_nano_usd FROM user_balances WHERE user_id = ?`, order.UserID).Scan(&balance); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.PaymentOrder{}, false, ErrNotFound
		}
		return core.PaymentOrder{}, false, err
	}
	nextBalance, err := addNanoUSD(balance, order.AmountNanoUSD)
	if err != nil {
		return core.PaymentOrder{}, false, err
	}
	now := time.Now().UTC()
	if paidAt.IsZero() {
		paidAt = now
	}
	paymentBalanceAfter := nextBalance
	normalizedCredits, err := normalizePaymentOrderBalanceCredits(order.OutTradeNo, credits)
	if err != nil {
		return core.PaymentOrder{}, false, err
	}
	creditBalances := make(map[string]int64, len(normalizedCredits))
	creditBalanceAfter := make([]int64, len(normalizedCredits))
	for index, credit := range normalizedCredits {
		var creditBalance int64
		if balance, ok := creditBalances[credit.UserID]; ok {
			creditBalance = balance
		} else if credit.UserID == order.UserID {
			creditBalance = nextBalance
		} else if err := tx.QueryRow(`SELECT balance_nano_usd FROM user_balances WHERE user_id = ?`, credit.UserID).Scan(&creditBalance); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return core.PaymentOrder{}, false, ErrNotFound
			}
			return core.PaymentOrder{}, false, err
		}
		creditBalance, err = addNanoUSD(creditBalance, credit.AmountNanoUSD)
		if err != nil {
			return core.PaymentOrder{}, false, err
		}
		creditBalanceAfter[index] = creditBalance
		creditBalances[credit.UserID] = creditBalance
		if credit.UserID == order.UserID {
			nextBalance = creditBalance
		}
	}
	order.Status = core.PaymentOrderPaid
	order.ProviderTradeNo = strings.TrimSpace(providerTradeNo)
	order.PaidAt = &paidAt
	order.UpdatedAt = now
	payload, err := json.Marshal(order)
	if err != nil {
		return core.PaymentOrder{}, false, err
	}
	if _, err := tx.Exec(`UPDATE user_balances SET balance_nano_usd = ?, updated_at_ns = ? WHERE user_id = ?`, nextBalance, now.UnixNano(), order.UserID); err != nil {
		return core.PaymentOrder{}, false, err
	}
	for _, credit := range normalizedCredits {
		if credit.UserID == order.UserID {
			continue
		}
		if _, err := tx.Exec(`UPDATE user_balances SET balance_nano_usd = ?, updated_at_ns = ? WHERE user_id = ?`, creditBalances[credit.UserID], now.UnixNano(), credit.UserID); err != nil {
			return core.PaymentOrder{}, false, err
		}
	}
	if _, err := tx.Exec(`UPDATE payment_orders SET status = ?, amount_nano_usd = ?, paid_at_ns = ?, payload = ?, updated_at_ns = ? WHERE id = ?`, string(order.Status), order.AmountNanoUSD, paymentOrderPaidAtNS(order), string(payload), now.UnixNano(), order.ID); err != nil {
		return core.PaymentOrder{}, false, err
	}
	if err := setFinanceUserRollupBalanceTx(tx, order.UserID, nextBalance); err != nil {
		return core.PaymentOrder{}, false, err
	}
	for _, credit := range normalizedCredits {
		if credit.UserID == order.UserID {
			continue
		}
		if err := setFinanceUserRollupBalanceTx(tx, credit.UserID, creditBalances[credit.UserID]); err != nil {
			return core.PaymentOrder{}, false, err
		}
	}
	if err := addFinanceUserRechargeRollupTx(tx, order.UserID, order.AmountNanoUSD, paidAt); err != nil {
		return core.PaymentOrder{}, false, err
	}
	if err := insertBillingLedgerTx(tx, core.BillingLedgerEntry{
		ID:                  billingLedgerID("payment", order.OutTradeNo, string(order.Provider), now),
		UserID:              order.UserID,
		Kind:                "payment",
		AmountNanoUSD:       order.AmountNanoUSD,
		BalanceAfterNanoUSD: paymentBalanceAfter,
		Note:                fmt.Sprintf("%s %s", order.Provider, order.OutTradeNo),
		CreatedAt:           now,
	}); err != nil {
		return core.PaymentOrder{}, false, err
	}
	for index, credit := range normalizedCredits {
		if err := insertBillingLedgerTx(tx, core.BillingLedgerEntry{
			ID:                  credit.LedgerID,
			UserID:              credit.UserID,
			Kind:                credit.Kind,
			AmountNanoUSD:       credit.AmountNanoUSD,
			BalanceAfterNanoUSD: creditBalanceAfter[index],
			Note:                credit.Note,
			CreatedAt:           now,
		}); err != nil {
			return core.PaymentOrder{}, false, err
		}
		if credit.AmountNanoUSD > 0 {
			if err := addFinanceUserRewardRollupTx(tx, credit.UserID, credit.AmountNanoUSD); err != nil {
				return core.PaymentOrder{}, false, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return core.PaymentOrder{}, false, err
	}
	return order, true, nil
}

func (r *SQLiteRepository) CreatePaymentRefund(refund core.PaymentRefund) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	refund.ID = strings.TrimSpace(refund.ID)
	refund.OrderID = strings.TrimSpace(refund.OrderID)
	refund.OutTradeNo = strings.TrimSpace(refund.OutTradeNo)
	refund.UserID = strings.TrimSpace(refund.UserID)
	if refund.ID == "" || refund.OrderID == "" || refund.OutTradeNo == "" || refund.UserID == "" || refund.AmountNanoUSD <= 0 {
		return fmt.Errorf("payment refund is incomplete")
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	order, ok, err := getPaymentOrderByIDTx(tx, refund.OrderID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if order.Status != core.PaymentOrderPaid || order.OutTradeNo != refund.OutTradeNo || order.UserID != refund.UserID || order.Provider != refund.Provider {
		return ErrBillingRequestConflict
	}
	var balance int64
	if err := tx.QueryRow(`SELECT balance_nano_usd FROM user_balances WHERE user_id = ?`, refund.UserID).Scan(&balance); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	pendingBalance, err := pendingPaymentRefundAmountTx(tx, refund.UserID)
	if err != nil {
		return err
	}
	availableBalance := balance - pendingBalance
	if availableBalance < 0 {
		availableBalance = 0
	}
	if refund.AmountNanoUSD > availableBalance {
		return ErrInsufficientBalance
	}
	remaining, err := refundablePaymentAmountTx(tx, order)
	if err != nil {
		return err
	}
	if refund.AmountNanoUSD > remaining {
		return ErrInsufficientBalance
	}
	now := time.Now().UTC()
	if refund.CreatedAt.IsZero() {
		refund.CreatedAt = now
	}
	refund.UpdatedAt = now
	if refund.Status == "" {
		refund.Status = core.PaymentRefundPending
	}
	if refund.Status != core.PaymentRefundPending {
		return ErrBillingRequestConflict
	}
	payload, err := json.Marshal(refund)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO payment_refunds(id, order_id, out_trade_no, user_id, provider, status, amount_nano_usd, payload, created_at_ns, updated_at_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		refund.ID, refund.OrderID, refund.OutTradeNo, refund.UserID, string(refund.Provider), string(refund.Status), refund.AmountNanoUSD, string(payload), sqliteTimeNS(refund.CreatedAt), sqliteTimeNS(refund.UpdatedAt),
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *SQLiteRepository) CompletePaymentRefund(id, providerRefundNo, rawResponse string) (core.PaymentRefund, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	tx, err := r.db.Begin()
	if err != nil {
		return core.PaymentRefund{}, false, err
	}
	defer tx.Rollback()

	refund, ok, err := getPaymentRefundByIDTx(tx, strings.TrimSpace(id))
	if err != nil {
		return core.PaymentRefund{}, false, err
	}
	if !ok {
		return core.PaymentRefund{}, false, ErrNotFound
	}
	if refund.Status == core.PaymentRefundDone {
		return refund, false, nil
	}
	if refund.Status != core.PaymentRefundPending {
		return core.PaymentRefund{}, false, ErrBillingRequestConflict
	}
	var balance int64
	if err := tx.QueryRow(`SELECT balance_nano_usd FROM user_balances WHERE user_id = ?`, refund.UserID).Scan(&balance); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.PaymentRefund{}, false, ErrNotFound
		}
		return core.PaymentRefund{}, false, err
	}
	if balance < refund.AmountNanoUSD {
		return core.PaymentRefund{}, false, ErrInsufficientBalance
	}
	now := time.Now().UTC()
	balanceAfter := balance - refund.AmountNanoUSD
	if _, err := tx.Exec(`UPDATE user_balances SET balance_nano_usd = ?, updated_at_ns = ? WHERE user_id = ?`, balanceAfter, sqliteTimeNS(now), refund.UserID); err != nil {
		return core.PaymentRefund{}, false, err
	}
	refund.Status = core.PaymentRefundDone
	refund.ProviderRefundNo = strings.TrimSpace(providerRefundNo)
	refund.RawResponse = rawResponse
	if refund.ManualPayoutRef == "" {
		refund.ManualPayoutRef = refund.ProviderRefundNo
	}
	if refund.ManualPayoutNote == "" {
		refund.ManualPayoutNote = strings.TrimSpace(rawResponse)
	}
	if refund.ManualPayoutAt == nil {
		refund.ManualPayoutAt = &now
	}
	refund.UpdatedAt = now
	payload, err := json.Marshal(refund)
	if err != nil {
		return core.PaymentRefund{}, false, err
	}
	if _, err := tx.Exec(`UPDATE payment_refunds SET status = ?, amount_nano_usd = ?, payload = ?, updated_at_ns = ? WHERE id = ?`, string(refund.Status), refund.AmountNanoUSD, string(payload), sqliteTimeNS(now), refund.ID); err != nil {
		return core.PaymentRefund{}, false, err
	}
	if err := setFinanceUserRollupBalanceTx(tx, refund.UserID, balanceAfter); err != nil {
		return core.PaymentRefund{}, false, err
	}
	if err := addFinanceUserRefundRollupTx(tx, refund.UserID, refund.AmountNanoUSD); err != nil {
		return core.PaymentRefund{}, false, err
	}
	if err := insertBillingLedgerTx(tx, core.BillingLedgerEntry{
		ID:                  billingLedgerID("payment_refund", refund.ID, refund.OrderID, now),
		UserID:              refund.UserID,
		Kind:                "payment_refund",
		AmountNanoUSD:       -refund.AmountNanoUSD,
		BalanceAfterNanoUSD: balanceAfter,
		Note:                refund.OutTradeNo,
		CreatedAt:           now,
	}); err != nil {
		return core.PaymentRefund{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return core.PaymentRefund{}, false, err
	}
	return refund, true, nil
}

func (r *SQLiteRepository) FailPaymentRefund(id, rawResponse string) (core.PaymentRefund, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	tx, err := r.db.Begin()
	if err != nil {
		return core.PaymentRefund{}, err
	}
	defer tx.Rollback()
	refund, ok, err := getPaymentRefundByIDTx(tx, strings.TrimSpace(id))
	if err != nil {
		return core.PaymentRefund{}, err
	}
	if !ok {
		return core.PaymentRefund{}, ErrNotFound
	}
	if refund.Status == core.PaymentRefundDone {
		return core.PaymentRefund{}, ErrBillingRequestConflict
	}
	if refund.Status != core.PaymentRefundFailed {
		refund.Status = core.PaymentRefundFailed
		refund.RawResponse = rawResponse
		refund.UpdatedAt = time.Now().UTC()
		payload, err := json.Marshal(refund)
		if err != nil {
			return core.PaymentRefund{}, err
		}
		if _, err := tx.Exec(`UPDATE payment_refunds SET status = ?, amount_nano_usd = ?, payload = ?, updated_at_ns = ? WHERE id = ?`, string(refund.Status), refund.AmountNanoUSD, string(payload), sqliteTimeNS(refund.UpdatedAt), refund.ID); err != nil {
			return core.PaymentRefund{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return core.PaymentRefund{}, err
	}
	return refund, nil
}

func (r *SQLiteRepository) ListPaymentRefunds(orderID string) []core.PaymentRefund {
	r.mu.RLock()
	defer r.mu.RUnlock()

	query := `SELECT payload FROM payment_refunds`
	args := []any{}
	if strings.TrimSpace(orderID) != "" {
		query += ` WHERE order_id = ?`
		args = append(args, strings.TrimSpace(orderID))
	}
	query += ` ORDER BY created_at_ns DESC, id ASC`
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []core.PaymentRefund{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		var refund core.PaymentRefund
		if err := json.Unmarshal([]byte(payload), &refund); err != nil {
			continue
		}
		out = append(out, refund)
	}
	return out
}

func paymentOrderWhere(query PaymentOrderQuery) (string, []any) {
	clauses := make([]string, 0, 3)
	args := make([]any, 0, 3)
	if query.UserID != "" {
		clauses = append(clauses, `(LOWER(user_id) = LOWER(?) OR user_id IN (SELECT id FROM users WHERE LOWER(username) = LOWER(?)))`)
		args = append(args, query.UserID, query.UserID)
	}
	if query.Provider != "" {
		clauses = append(clauses, `provider = ?`)
		args = append(args, string(query.Provider))
	}
	if query.Status != "" {
		clauses = append(clauses, `status = ?`)
		args = append(args, string(query.Status))
	}
	if query.ExcludePending {
		clauses = append(clauses, `status <> ?`)
		args = append(args, string(core.PaymentOrderPending))
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func billingRequestWhere(query BillingRequestQuery) (string, []any) {
	clauses := make([]string, 0, 6)
	args := make([]any, 0, 6)
	if query.UserID != "" {
		clauses = append(clauses, `(LOWER(user_id) = LOWER(?) OR user_id IN (SELECT id FROM users WHERE LOWER(username) = LOWER(?)))`)
		args = append(args, query.UserID, query.UserID)
	}
	if query.ClientID != "" {
		clauses = append(clauses, `(LOWER(client_name) = ? OR LOWER(client_id) = ?)`)
		value := strings.ToLower(strings.TrimSpace(query.ClientID))
		args = append(args, value, value)
	}
	if query.Model != "" {
		clauses = append(clauses, `LOWER(model) LIKE ? ESCAPE '\'`)
		args = append(args, billingSQLLikePattern(query.Model))
	}
	if query.Status != "" {
		clauses = append(clauses, `status = ?`)
		args = append(args, string(query.Status))
	}
	if !query.StartedAt.IsZero() {
		clauses = append(clauses, `created_at_ns >= ?`)
		args = append(args, sqliteTimeNS(query.StartedAt))
	}
	if !query.EndedAt.IsZero() {
		clauses = append(clauses, `created_at_ns <= ?`)
		args = append(args, sqliteTimeNS(query.EndedAt))
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}
