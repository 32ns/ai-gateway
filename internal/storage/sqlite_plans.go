package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func (r *SQLiteRepository) ListBillingPlans() []core.BillingPlan {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`
		SELECT id, name, description, group_name, enabled, price_nano_usd, period_quota_nano_usd,
			period_duration_sec, period_count, sort_order, created_at_ns, updated_at_ns
		FROM billing_plans
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]core.BillingPlan, 0)
	for rows.Next() {
		plan, err := scanBillingPlan(rows)
		if err == nil {
			out = append(out, plan)
		}
	}
	return sortBillingPlans(out)
}

func (r *SQLiteRepository) GetBillingPlan(id string) (core.BillingPlan, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return getBillingPlanTx(r.db, strings.TrimSpace(id))
}

func (r *SQLiteRepository) UpsertBillingPlan(plan core.BillingPlan) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	plan = normalizeBillingPlan(plan)
	if plan.ID == "" {
		return fmt.Errorf("billing plan id is required")
	}
	if plan.Name == "" {
		return fmt.Errorf("billing plan name is required")
	}
	now := time.Now().UTC()
	if plan.CreatedAt.IsZero() {
		if existing, err := getBillingPlanTx(r.db, plan.ID); err == nil && !existing.CreatedAt.IsZero() {
			plan.CreatedAt = existing.CreatedAt
		} else {
			plan.CreatedAt = now
		}
	}
	plan.UpdatedAt = now
	_, err := r.db.Exec(`
		INSERT INTO billing_plans(
			id, name, description, group_name, enabled, price_nano_usd, period_quota_nano_usd,
			period_duration_sec, period_count, sort_order, created_at_ns, updated_at_ns
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			description = excluded.description,
			group_name = excluded.group_name,
			enabled = excluded.enabled,
			price_nano_usd = excluded.price_nano_usd,
			period_quota_nano_usd = excluded.period_quota_nano_usd,
			period_duration_sec = excluded.period_duration_sec,
			period_count = excluded.period_count,
			sort_order = excluded.sort_order,
			updated_at_ns = excluded.updated_at_ns
	`, plan.ID, plan.Name, plan.Description, plan.Group, boolInt(plan.Enabled), plan.PriceNanoUSD, plan.PeriodQuotaNanoUSD, plan.PeriodDurationSec, plan.PeriodCount, plan.SortOrder, sqliteTimeNS(plan.CreatedAt), sqliteTimeNS(plan.UpdatedAt))
	return err
}

func (r *SQLiteRepository) DeleteBillingPlan(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deleteByID("billing_plans", strings.TrimSpace(id))
}

func (r *SQLiteRepository) ListBillingPlanGroups() []core.BillingPlanGroup {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`
		SELECT id, name, sale_disabled, quota_price_ratio, sort_order, created_at_ns, updated_at_ns
		FROM billing_plan_groups
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]core.BillingPlanGroup, 0)
	for rows.Next() {
		group, err := scanBillingPlanGroup(rows)
		if err == nil {
			out = append(out, group)
		}
	}
	return sortBillingPlanGroups(out)
}

func (r *SQLiteRepository) GetBillingPlanGroup(id string) (core.BillingPlanGroup, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	row := r.db.QueryRow(`
		SELECT id, name, sale_disabled, quota_price_ratio, sort_order, created_at_ns, updated_at_ns
		FROM billing_plan_groups
		WHERE id = ?
	`, strings.TrimSpace(id))
	group, err := scanBillingPlanGroup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.BillingPlanGroup{}, ErrNotFound
	}
	return group, err
}

func (r *SQLiteRepository) UpsertBillingPlanGroup(group core.BillingPlanGroup) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	group = normalizeBillingPlanGroup(group)
	if group.ID == "" {
		return fmt.Errorf("billing plan group id is required")
	}
	if group.Name == "" {
		return fmt.Errorf("billing plan group name is required")
	}
	now := time.Now().UTC()
	if group.CreatedAt.IsZero() {
		if existing, err := getBillingPlanGroupTx(r.db, group.ID); err == nil && !existing.CreatedAt.IsZero() {
			group.CreatedAt = existing.CreatedAt
		} else {
			group.CreatedAt = now
		}
	}
	group.UpdatedAt = now
	_, err := r.db.Exec(`
		INSERT INTO billing_plan_groups(id, name, sale_disabled, quota_price_ratio, sort_order, created_at_ns, updated_at_ns)
		VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			sale_disabled = excluded.sale_disabled,
			quota_price_ratio = excluded.quota_price_ratio,
			sort_order = excluded.sort_order,
			updated_at_ns = excluded.updated_at_ns
	`, group.ID, group.Name, boolInt(group.SaleDisabled), group.QuotaPriceRatio, group.SortOrder, sqliteTimeNS(group.CreatedAt), sqliteTimeNS(group.UpdatedAt))
	return err
}

func (r *SQLiteRepository) DeleteBillingPlanGroup(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deleteByID("billing_plan_groups", strings.TrimSpace(id))
}

func (r *SQLiteRepository) PurchaseBillingPlan(input core.BillingPlanPurchaseInput) (core.BillingPlanPurchase, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	userID := strings.TrimSpace(input.UserID)
	planID := strings.TrimSpace(input.PlanID)
	mode := normalizeBillingPlanPurchaseMode(input.Mode)
	if userID == "" || planID == "" {
		return core.BillingPlanPurchase{}, fmt.Errorf("user and plan are required")
	}
	tx, err := r.db.Begin()
	if err != nil {
		return core.BillingPlanPurchase{}, err
	}
	defer tx.Rollback()

	plan, err := getBillingPlanTx(tx, planID)
	if err != nil {
		return core.BillingPlanPurchase{}, err
	}
	plan = normalizeBillingPlan(plan)
	if !plan.Enabled {
		return core.BillingPlanPurchase{}, ErrNotFound
	}
	if core.NormalizeBillingPlanGroup(plan.Group) != "" {
		group, err := getBillingPlanGroupTx(tx, plan.Group)
		if err != nil {
			return core.BillingPlanPurchase{}, err
		}
		if !billingPlanGroupSaleEnabled(group) {
			return core.BillingPlanPurchase{}, ErrNotFound
		}
	}
	var balance int64
	err = tx.QueryRow(`SELECT balance_nano_usd FROM user_balances WHERE user_id = ?`, userID).Scan(&balance)
	if errors.Is(err, sql.ErrNoRows) {
		return core.BillingPlanPurchase{}, ErrNotFound
	}
	if err != nil {
		return core.BillingPlanPurchase{}, err
	}
	now := time.Now().UTC()
	activeEntitlements, err := activeUserPlanEntitlementsTx(tx, userID, now)
	if err != nil {
		return core.BillingPlanPurchase{}, err
	}
	activeEntitlement := core.UserPlanEntitlement{}
	hasMatchingActiveEntitlement := false
	if len(activeEntitlements) == 0 {
		mode = core.BillingPlanPurchaseSeparate
	}
	if mode == core.BillingPlanPurchaseMergeQuota {
		activeEntitlement, hasMatchingActiveEntitlement = selectBillingPlanCombineEntitlement(plan, activeEntitlements, input.TargetEntitlementID)
		if !hasMatchingActiveEntitlement {
			return core.BillingPlanPurchase{}, fmt.Errorf("active plan must match selected plan to merge or extend")
		}
	} else if mode == core.BillingPlanPurchaseExtendPeriod {
		activeEntitlement, hasMatchingActiveEntitlement = selectBillingPlanExtendEntitlement(plan, activeEntitlements, input.TargetEntitlementID)
		if !hasMatchingActiveEntitlement {
			return core.BillingPlanPurchase{}, fmt.Errorf("active plan must match selected plan quota and period to extend")
		}
	}
	if balance < plan.PriceNanoUSD {
		return core.BillingPlanPurchase{}, ErrInsufficientBalance
	}

	var entitlement core.UserPlanEntitlement
	if mode == core.BillingPlanPurchaseMergeQuota {
		entitlement = mergeBillingPlanQuota(activeEntitlement, plan, now)
		if err := updateUserPlanEntitlementTx(tx, entitlement); err != nil {
			return core.BillingPlanPurchase{}, err
		}
	} else if mode == core.BillingPlanPurchaseExtendPeriod {
		entitlement = extendBillingPlanPeriod(activeEntitlement, plan, now)
		if err := updateUserPlanEntitlementTx(tx, entitlement); err != nil {
			return core.BillingPlanPurchase{}, err
		}
	} else {
		for attempt := 0; attempt < 1000; attempt++ {
			candidate := newUserPlanEntitlement(userID, plan, now.Add(time.Duration(attempt)*time.Nanosecond))
			if _, exists, err := getUserPlanEntitlementByIDTx(tx, candidate.ID); err != nil {
				return core.BillingPlanPurchase{}, err
			} else if !exists {
				entitlement = candidate
				break
			}
		}
		if entitlement.ID == "" {
			return core.BillingPlanPurchase{}, fmt.Errorf("failed to generate unique plan entitlement id")
		}
		entitlement.Priority, err = nextUserPlanEntitlementPriorityTx(tx, userID, now)
		if err != nil {
			return core.BillingPlanPurchase{}, err
		}
	}
	balanceAfter := balance - plan.PriceNanoUSD
	if _, err := tx.Exec(`UPDATE user_balances SET balance_nano_usd = ?, updated_at_ns = ? WHERE user_id = ?`, balanceAfter, sqliteTimeNS(now), userID); err != nil {
		return core.BillingPlanPurchase{}, err
	}
	if err := setFinanceUserRollupBalanceTx(tx, userID, balanceAfter); err != nil {
		return core.BillingPlanPurchase{}, err
	}
	if plan.PriceNanoUSD > 0 {
		if err := addFinanceUserSpendRollupTx(tx, userID, plan.PriceNanoUSD, now); err != nil {
			return core.BillingPlanPurchase{}, err
		}
	}
	if mode == core.BillingPlanPurchaseSeparate {
		if err := insertUserPlanEntitlementTx(tx, entitlement); err != nil {
			return core.BillingPlanPurchase{}, err
		}
	}
	ledgerIDKind := "plan_purchase"
	if mode == core.BillingPlanPurchaseMergeQuota {
		ledgerIDKind = "plan_purchase_merge"
	} else if mode == core.BillingPlanPurchaseExtendPeriod {
		ledgerIDKind = "plan_purchase_extend"
	}
	if plan.PriceNanoUSD != 0 {
		ledgerID, err := uniqueBillingLedgerIDTx(tx, ledgerIDKind, entitlement.ID, userID, now)
		if err != nil {
			return core.BillingPlanPurchase{}, err
		}
		if err := insertBillingLedgerTx(tx, core.BillingLedgerEntry{
			ID:                  ledgerID,
			UserID:              userID,
			Kind:                "plan_purchase",
			AmountNanoUSD:       -plan.PriceNanoUSD,
			BalanceAfterNanoUSD: balanceAfter,
			Note:                plan.Name,
			CreatedAt:           now,
		}); err != nil {
			return core.BillingPlanPurchase{}, err
		}
	}
	quotaLedgerAmount := entitlement.CurrentQuotaNanoUSD
	quotaLedgerKind := "purchase"
	if mode == core.BillingPlanPurchaseMergeQuota {
		quotaLedgerAmount = plan.PeriodQuotaNanoUSD
		quotaLedgerKind = "merge_purchase"
	} else if mode == core.BillingPlanPurchaseExtendPeriod {
		quotaLedgerAmount = 0
		quotaLedgerKind = "extend_purchase"
	}
	if mode == core.BillingPlanPurchaseExtendPeriod || quotaLedgerAmount != 0 {
		quotaLedgerID, err := uniquePlanQuotaLedgerIDTx(tx, quotaLedgerKind, entitlement.ID, "", "", now)
		if err != nil {
			return core.BillingPlanPurchase{}, err
		}
		if err := insertPlanQuotaLedgerTx(tx, core.PlanQuotaLedgerEntry{
			ID:                quotaLedgerID,
			EntitlementID:     entitlement.ID,
			UserID:            userID,
			Kind:              quotaLedgerKind,
			AmountNanoUSD:     quotaLedgerAmount,
			QuotaAfterNanoUSD: entitlement.CurrentQuotaNanoUSD,
			Note:              plan.Name,
			CreatedAt:         now,
		}); err != nil {
			return core.BillingPlanPurchase{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return core.BillingPlanPurchase{}, err
	}
	return core.BillingPlanPurchase{
		Plan:                 plan,
		Entitlement:          entitlement,
		BalanceBeforeNanoUSD: balance,
		BalanceAfterNanoUSD:  balanceAfter,
	}, nil
}

func uniqueBillingLedgerIDTx(tx *sql.Tx, kind, requestID, clientID string, ts time.Time) (string, error) {
	for attempt := 0; attempt < 1000; attempt++ {
		id := billingLedgerID(kind, requestID, ledgerIDPartWithAttempt(clientID, attempt), ts)
		var exists int
		err := tx.QueryRow(`SELECT 1 FROM billing_ledger WHERE id = ?`, id).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return id, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("failed to generate unique billing ledger id")
}

func uniquePlanQuotaLedgerIDTx(tx *sql.Tx, kind, entitlementID, requestID, clientID string, ts time.Time) (string, error) {
	for attempt := 0; attempt < 1000; attempt++ {
		id := planQuotaLedgerID(kind, entitlementID, requestID, clientID, ts, attempt)
		var exists int
		err := tx.QueryRow(`SELECT 1 FROM plan_quota_ledger WHERE id = ?`, id).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return id, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("failed to generate unique plan quota ledger id")
}

func (r *SQLiteRepository) GrantBillingPlan(input core.BillingPlanGrantInput) (core.BillingPlanPurchase, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	userID := strings.TrimSpace(input.UserID)
	planID := strings.TrimSpace(input.PlanID)
	if userID == "" || planID == "" {
		return core.BillingPlanPurchase{}, fmt.Errorf("user and plan are required")
	}
	tx, err := r.db.Begin()
	if err != nil {
		return core.BillingPlanPurchase{}, err
	}
	defer tx.Rollback()

	plan, err := getBillingPlanTx(tx, planID)
	if err != nil {
		return core.BillingPlanPurchase{}, err
	}
	plan = normalizeBillingPlan(plan)
	if !plan.Enabled {
		return core.BillingPlanPurchase{}, ErrNotFound
	}
	var balance int64
	err = tx.QueryRow(`SELECT balance_nano_usd FROM user_balances WHERE user_id = ?`, userID).Scan(&balance)
	if errors.Is(err, sql.ErrNoRows) {
		return core.BillingPlanPurchase{}, ErrNotFound
	}
	if err != nil {
		return core.BillingPlanPurchase{}, err
	}
	now := time.Now().UTC()
	entitlement := core.UserPlanEntitlement{}
	for attempt := 0; attempt < 1000; attempt++ {
		candidate := newUserPlanEntitlement(userID, plan, now.Add(time.Duration(attempt)*time.Nanosecond))
		candidate.PriceNanoUSD = 0
		candidate = normalizeUserPlanEntitlement(candidate)
		if _, exists, err := getUserPlanEntitlementByIDTx(tx, candidate.ID); err != nil {
			return core.BillingPlanPurchase{}, err
		} else if !exists {
			entitlement = candidate
			break
		}
	}
	if entitlement.ID == "" {
		return core.BillingPlanPurchase{}, fmt.Errorf("failed to generate unique plan entitlement id")
	}
	entitlement.Priority, err = nextUserPlanEntitlementPriorityTx(tx, userID, now)
	if err != nil {
		return core.BillingPlanPurchase{}, err
	}
	if err := insertUserPlanEntitlementTx(tx, entitlement); err != nil {
		return core.BillingPlanPurchase{}, err
	}
	note := strings.TrimSpace(input.Note)
	if note == "" {
		note = "admin grant"
	}
	if entitlement.CurrentQuotaNanoUSD != 0 {
		if err := insertPlanQuotaLedgerTx(tx, core.PlanQuotaLedgerEntry{
			ID:                planQuotaLedgerID("grant", entitlement.ID, "", "", now, 0),
			EntitlementID:     entitlement.ID,
			UserID:            userID,
			Kind:              "grant",
			AmountNanoUSD:     entitlement.CurrentQuotaNanoUSD,
			QuotaAfterNanoUSD: entitlement.CurrentQuotaNanoUSD,
			Note:              strings.TrimSpace(plan.Name + " " + note),
			CreatedAt:         now,
		}); err != nil {
			return core.BillingPlanPurchase{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return core.BillingPlanPurchase{}, err
	}
	return core.BillingPlanPurchase{
		Plan:                 plan,
		Entitlement:          entitlement,
		BalanceBeforeNanoUSD: balance,
		BalanceAfterNanoUSD:  balance,
	}, nil
}

func (r *SQLiteRepository) ListUserPlanEntitlements(userID string) []core.UserPlanEntitlement {
	r.mu.Lock()
	defer r.mu.Unlock()

	tx, err := r.db.Begin()
	if err != nil {
		return nil
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	query := `
		SELECT id, user_id, plan_id, plan_name, status, price_nano_usd, period_quota_nano_usd, base_period_quota_nano_usd,
			period_duration_sec, total_periods, remaining_periods, current_quota_nano_usd,
			priority, current_period_started_at_ns, current_period_ends_at_ns, expires_at_ns, purchased_at_ns, updated_at_ns
		FROM user_plan_entitlements
	`
	args := []any{}
	if strings.TrimSpace(userID) != "" {
		query += ` WHERE user_id = ?`
		args = append(args, strings.TrimSpace(userID))
	}
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil
	}
	entitlements := make([]core.UserPlanEntitlement, 0)
	for rows.Next() {
		entitlement, err := scanUserPlanEntitlement(rows)
		if err != nil {
			continue
		}
		entitlements = append(entitlements, entitlement)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil
	}
	if err := rows.Close(); err != nil {
		return nil
	}
	out := make([]core.UserPlanEntitlement, 0, len(entitlements))
	for _, entitlement := range entitlements {
		entitlement, entries, changed := advanceUserPlanEntitlement(entitlement, now)
		if changed {
			if err := updateUserPlanEntitlementTx(tx, entitlement); err != nil {
				return nil
			}
			for _, entry := range entries {
				if err := insertPlanQuotaLedgerTx(tx, entry); err != nil {
					return nil
				}
			}
		}
		out = append(out, entitlement)
	}
	if err := tx.Commit(); err != nil {
		return nil
	}
	return sortUserPlanEntitlements(out)
}

func (r *SQLiteRepository) GetActiveUserPlanEntitlement(userID string) (core.UserPlanEntitlement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	tx, err := r.db.Begin()
	if err != nil {
		return core.UserPlanEntitlement{}, err
	}
	defer tx.Rollback()
	entitlement, ok, err := activeUserPlanEntitlementTx(tx, strings.TrimSpace(userID), time.Now().UTC())
	if err != nil {
		return core.UserPlanEntitlement{}, err
	}
	if !ok {
		return core.UserPlanEntitlement{}, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return core.UserPlanEntitlement{}, err
	}
	return entitlement, nil
}

func (r *SQLiteRepository) MoveUserPlanEntitlementPriority(userID, entitlementID, direction string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	entitlements, err := activeUserPlanEntitlementsTx(tx, userID, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := moveUserPlanEntitlementPriorityTx(tx, entitlements, entitlementID, direction); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *SQLiteRepository) CancelUserPlanEntitlement(userID, entitlementID string) (core.UserPlanEntitlement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	tx, err := r.db.Begin()
	if err != nil {
		return core.UserPlanEntitlement{}, err
	}
	defer tx.Rollback()
	entitlement, err := cancelUserPlanEntitlementTx(tx, userID, entitlementID, time.Now().UTC())
	if err != nil {
		return core.UserPlanEntitlement{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.UserPlanEntitlement{}, err
	}
	return entitlement, nil
}

func (r *SQLiteRepository) PlanSubscriptionStats(query PlanSubscriptionQuery) PlanSubscriptionStats {
	query = normalizePlanSubscriptionQuery(query)
	_ = r.ListUserPlanEntitlements("")

	r.mu.RLock()
	defer r.mu.RUnlock()

	where, args := planSubscriptionSQLWhere(query)
	var stats PlanSubscriptionStats
	err := r.db.QueryRow(`
		SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN upe.status = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN upe.status = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN upe.status = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(upe.price_nano_usd), 0),
			COALESCE(SUM(CASE WHEN upe.status = ? AND upe.current_quota_nano_usd > 0 THEN upe.current_quota_nano_usd ELSE 0 END), 0),
			COALESCE(SUM(CASE
				WHEN upe.status = ? AND upe.period_quota_nano_usd > 0 THEN
					CASE
						WHEN upe.current_quota_nano_usd <= 0 THEN upe.period_quota_nano_usd
						WHEN upe.current_quota_nano_usd >= upe.period_quota_nano_usd THEN 0
						ELSE upe.period_quota_nano_usd - upe.current_quota_nano_usd
					END
				ELSE 0
			END), 0)
		FROM user_plan_entitlements upe
		LEFT JOIN users u ON u.id = upe.user_id
		`+where,
		append([]any{
			string(core.UserPlanEntitlementActive),
			string(core.UserPlanEntitlementExpired),
			string(core.UserPlanEntitlementCancelled),
			string(core.UserPlanEntitlementActive),
			string(core.UserPlanEntitlementActive),
		}, args...)...,
	).Scan(&stats.TotalCount, &stats.ActiveCount, &stats.ExpiredCount, &stats.CancelledCount, &stats.RevenueNanoUSD, &stats.ActiveRemainingNanoUSD, &stats.ActiveUsedNanoUSD)
	if err != nil {
		return PlanSubscriptionStats{}
	}
	return stats
}

func (r *SQLiteRepository) ListPlanSubscriptionPlanSummaries(query PlanSubscriptionQuery, limit int) []PlanSubscriptionPlanSummary {
	query = normalizePlanSubscriptionQuery(query)
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	_ = r.ListUserPlanEntitlements("")

	r.mu.RLock()
	defer r.mu.RUnlock()

	where, args := planSubscriptionSQLWhere(query)
	queryArgs := []any{string(core.UserPlanEntitlementActive), string(core.UserPlanEntitlementActive)}
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, limit)
	rows, err := r.db.Query(`
		SELECT upe.plan_id,
			COALESCE(NULLIF(TRIM(upe.plan_name), ''), upe.plan_id) AS plan_name,
			COALESCE(bp.group_name, '') AS plan_group,
			COUNT(*) AS purchase_count,
			COALESCE(SUM(CASE WHEN upe.status = ? THEN 1 ELSE 0 END), 0) AS active_count,
			COALESCE(SUM(upe.price_nano_usd), 0) AS revenue_nano_usd,
			COALESCE(SUM(CASE WHEN upe.status = ? AND upe.current_quota_nano_usd > 0 THEN upe.current_quota_nano_usd ELSE 0 END), 0) AS active_remaining_nano_usd
		FROM user_plan_entitlements upe
		LEFT JOIN users u ON u.id = upe.user_id
		LEFT JOIN billing_plans bp ON bp.id = upe.plan_id
		`+where+`
		GROUP BY upe.plan_id, COALESCE(NULLIF(TRIM(upe.plan_name), ''), upe.plan_id), COALESCE(bp.group_name, '')
		ORDER BY revenue_nano_usd DESC, purchase_count DESC, plan_name ASC, upe.plan_id ASC
		LIMIT ?
	`, queryArgs...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]PlanSubscriptionPlanSummary, 0, limit)
	for rows.Next() {
		var summary PlanSubscriptionPlanSummary
		if err := rows.Scan(&summary.PlanID, &summary.PlanName, &summary.PlanGroup, &summary.PurchaseCount, &summary.ActiveCount, &summary.RevenueNanoUSD, &summary.ActiveRemainingNanoUSD); err != nil {
			continue
		}
		out = append(out, summary)
	}
	return out
}

func (r *SQLiteRepository) ListPlanSubscriptionSummariesPage(query PlanSubscriptionQuery) ([]PlanSubscriptionSummary, int) {
	query = normalizePlanSubscriptionQuery(query)
	_ = r.ListUserPlanEntitlements("")

	r.mu.RLock()
	defer r.mu.RUnlock()

	where, args := planSubscriptionSQLWhere(query)
	total := 0
	if err := r.db.QueryRow(`
		SELECT COUNT(*)
		FROM user_plan_entitlements upe
		LEFT JOIN users u ON u.id = upe.user_id
		`+where, args...).Scan(&total); err != nil {
		return nil, 0
	}
	pageArgs := append(append([]any{}, args...), query.Limit, query.Offset)
	rows, err := r.db.Query(`
		SELECT upe.id, upe.user_id,
			COALESCE(NULLIF(TRIM(u.username), ''), upe.user_id) AS username,
			upe.plan_id, upe.plan_name,
			COALESCE(bp.group_name, '') AS plan_group,
			upe.status, upe.price_nano_usd, upe.period_quota_nano_usd, upe.base_period_quota_nano_usd,
			upe.period_duration_sec, upe.total_periods, upe.remaining_periods,
			upe.current_quota_nano_usd, upe.priority, upe.current_period_started_at_ns,
			upe.current_period_ends_at_ns, upe.expires_at_ns, upe.purchased_at_ns,
			upe.updated_at_ns, COALESCE(ub.balance_nano_usd, 0) AS balance_nano_usd
		FROM user_plan_entitlements upe
		LEFT JOIN users u ON u.id = upe.user_id
		LEFT JOIN billing_plans bp ON bp.id = upe.plan_id
		LEFT JOIN user_balances ub ON ub.user_id = upe.user_id
		`+where+`
		ORDER BY upe.purchased_at_ns DESC, upe.id ASC
		LIMIT ? OFFSET ?
	`, pageArgs...)
	if err != nil {
		return nil, 0
	}
	defer rows.Close()

	out := make([]PlanSubscriptionSummary, 0, query.Limit)
	for rows.Next() {
		item, err := scanPlanSubscriptionSummary(rows)
		if err != nil {
			continue
		}
		out = append(out, item)
	}
	return out, total
}

func (r *SQLiteRepository) PlanQuotaUsageStats(query PlanQuotaUsageQuery) PlanQuotaUsageStats {
	return planQuotaUsageStatsFromRows(r.planQuotaUsageRows(query))
}

func (r *SQLiteRepository) ListPlanQuotaUsageByDay(query PlanQuotaUsageQuery) ([]PlanQuotaUsageDaySummary, int) {
	query = normalizePlanQuotaUsageQuery(query)
	items := r.planQuotaUsageRows(query)
	total := len(items)
	if query.Offset >= total {
		return nil, total
	}
	end := total
	if query.Limit > 0 {
		end = query.Offset + query.Limit
		if end > total {
			end = total
		}
	}
	out := append([]PlanQuotaUsageDaySummary(nil), items[query.Offset:end]...)
	return out, total
}

func (r *SQLiteRepository) ListPlanQuotaUsageEvents(query PlanQuotaUsageQuery) []PlanQuotaUsageEvent {
	query = normalizePlanQuotaUsageQuery(query)
	_ = r.ListUserPlanEntitlements("")

	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]PlanQuotaUsageEvent, 0)
	sqlQuery := `
		SELECT pql.user_id,
			COALESCE(NULLIF(TRIM(u.username), ''), pql.user_id) AS username,
			COALESCE(upe.plan_id, '') AS plan_id,
			COALESCE(NULLIF(TRIM(upe.plan_name), ''), NULLIF(TRIM(bp.name), ''), NULLIF(TRIM(upe.plan_id), ''), NULLIF(TRIM(pql.note), ''), '') AS plan_name,
			pql.entitlement_id,
			pql.kind,
			pql.amount_nano_usd,
			pql.created_at_ns
		FROM plan_quota_ledger pql
		LEFT JOIN user_plan_entitlements upe ON upe.id = pql.entitlement_id
		LEFT JOIN users u ON u.id = pql.user_id
		LEFT JOIN billing_plans bp ON bp.id = upe.plan_id
		WHERE pql.user_id <> ''
			AND NOT (
				pql.kind = 'settle'
				AND pql.request_id <> ''
				AND pql.client_id <> ''
				AND EXISTS (
					SELECT 1 FROM billing_requests br
					WHERE br.request_id = pql.request_id AND br.client_id = pql.client_id
				)
				AND EXISTS (
					SELECT 1 FROM billing_funding_allocations bfa
					WHERE bfa.request_id = pql.request_id
						AND bfa.client_id = pql.client_id
						AND bfa.source = ?
						AND bfa.entitlement_id = pql.entitlement_id
				)
			)
	`
	args := []any{core.BillingFundingSourcePlan}
	if query.UserID != "" {
		sqlQuery += ` AND (LOWER(pql.user_id) = LOWER(?) OR LOWER(COALESCE(u.username, '')) = LOWER(?))`
		args = append(args, query.UserID, query.UserID)
	}
	if query.PlanID != "" {
		sqlQuery += ` AND COALESCE(upe.plan_id, '') = ?`
		args = append(args, query.PlanID)
	}
	if query.EntitlementID != "" {
		sqlQuery += ` AND pql.entitlement_id = ?`
		args = append(args, query.EntitlementID)
	}
	if !query.StartedAt.IsZero() {
		sqlQuery += ` AND pql.created_at_ns >= ?`
		args = append(args, sqliteTimeNS(query.StartedAt))
	}
	if !query.EndedAt.IsZero() {
		sqlQuery += ` AND pql.created_at_ns < ?`
		args = append(args, sqliteTimeNS(query.EndedAt))
	}

	rows, err := r.db.Query(sqlQuery, args...)
	if err != nil {
		return nil
	}
	for rows.Next() {
		var userID, username, planID, planName, entitlementID, kind string
		var amountNanoUSD, createdAtNS int64
		if err := rows.Scan(&userID, &username, &planID, &planName, &entitlementID, &kind, &amountNanoUSD, &createdAtNS); err != nil {
			continue
		}
		if amountNanoUSD == 0 {
			continue
		}
		userID = strings.TrimSpace(userID)
		if userID == "" {
			continue
		}
		username = strings.TrimSpace(username)
		if username == "" {
			username = userID
		}
		planID = strings.TrimSpace(planID)
		planName = strings.TrimSpace(planName)
		if planName == "" {
			planName = planID
		}
		event := PlanQuotaUsageEvent{
			UserID:        userID,
			Username:      username,
			PlanID:        planID,
			PlanName:      planName,
			EntitlementID: strings.TrimSpace(entitlementID),
			Kind:          strings.TrimSpace(kind),
			CreatedAt:     timeFromNS(createdAtNS),
		}
		addPlanQuotaUsageEventLedgerAmount(&event, kind, amountNanoUSD)
		if planQuotaUsageEventHasAmount(event) {
			out = append(out, event)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil
	}
	if err := rows.Close(); err != nil {
		return nil
	}

	allocationQuery := `
		SELECT COALESCE(NULLIF(TRIM(bfa.user_id), ''), br.user_id) AS user_id,
			COALESCE(NULLIF(TRIM(u.username), ''), COALESCE(NULLIF(TRIM(bfa.user_id), ''), br.user_id)) AS username,
			COALESCE(upe.plan_id, '') AS plan_id,
			COALESCE(NULLIF(TRIM(upe.plan_name), ''), NULLIF(TRIM(bp.name), ''), NULLIF(TRIM(upe.plan_id), ''), '') AS plan_name,
			bfa.entitlement_id,
			br.status,
			bfa.reserved_nano_usd,
			bfa.actual_nano_usd,
			br.created_at_ns
		FROM billing_funding_allocations bfa
		JOIN billing_requests br ON br.request_id = bfa.request_id AND br.client_id = bfa.client_id
		LEFT JOIN user_plan_entitlements upe ON upe.id = bfa.entitlement_id
		LEFT JOIN users u ON u.id = COALESCE(NULLIF(TRIM(bfa.user_id), ''), br.user_id)
		LEFT JOIN billing_plans bp ON bp.id = upe.plan_id
		WHERE bfa.source = ?
			AND bfa.entitlement_id <> ''
	`
	allocationArgs := []any{core.BillingFundingSourcePlan}
	if query.UserID != "" {
		allocationQuery += ` AND (LOWER(COALESCE(NULLIF(TRIM(bfa.user_id), ''), br.user_id)) = LOWER(?) OR LOWER(COALESCE(u.username, '')) = LOWER(?))`
		allocationArgs = append(allocationArgs, query.UserID, query.UserID)
	}
	if query.PlanID != "" {
		allocationQuery += ` AND COALESCE(upe.plan_id, '') = ?`
		allocationArgs = append(allocationArgs, query.PlanID)
	}
	if query.EntitlementID != "" {
		allocationQuery += ` AND bfa.entitlement_id = ?`
		allocationArgs = append(allocationArgs, query.EntitlementID)
	}
	if !query.StartedAt.IsZero() {
		allocationQuery += ` AND br.created_at_ns >= ?`
		allocationArgs = append(allocationArgs, sqliteTimeNS(query.StartedAt))
	}
	if !query.EndedAt.IsZero() {
		allocationQuery += ` AND br.created_at_ns < ?`
		allocationArgs = append(allocationArgs, sqliteTimeNS(query.EndedAt))
	}
	rows, err = r.db.Query(allocationQuery, allocationArgs...)
	if err != nil {
		return nil
	}
	for rows.Next() {
		var userID, username, planID, planName, entitlementID, status string
		var reservedNanoUSD, actualNanoUSD, createdAtNS int64
		if err := rows.Scan(&userID, &username, &planID, &planName, &entitlementID, &status, &reservedNanoUSD, &actualNanoUSD, &createdAtNS); err != nil {
			continue
		}
		amountNanoUSD := billingRequestUsageSpendAmount(core.BillingRequestStatus(status), reservedNanoUSD, actualNanoUSD)
		if amountNanoUSD <= 0 {
			continue
		}
		userID = strings.TrimSpace(userID)
		if userID == "" {
			continue
		}
		username = strings.TrimSpace(username)
		if username == "" {
			username = userID
		}
		planID = strings.TrimSpace(planID)
		planName = strings.TrimSpace(planName)
		if planName == "" {
			planName = planID
		}
		out = append(out, PlanQuotaUsageEvent{
			UserID:        userID,
			Username:      username,
			PlanID:        planID,
			PlanName:      planName,
			EntitlementID: strings.TrimSpace(entitlementID),
			Kind:          "usage",
			UsedNanoUSD:   amountNanoUSD,
			NetNanoUSD:    -amountNanoUSD,
			CreatedAt:     timeFromNS(createdAtNS),
		})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil
	}
	if err := rows.Close(); err != nil {
		return nil
	}
	return sortPlanQuotaUsageEvents(out)
}

func (r *SQLiteRepository) planQuotaUsageRows(query PlanQuotaUsageQuery) []PlanQuotaUsageDaySummary {
	query = normalizePlanQuotaUsageQuery(query)
	_ = r.ListUserPlanEntitlements("")

	r.mu.RLock()
	defer r.mu.RUnlock()

	byKey := make(map[string]*PlanQuotaUsageDaySummary)
	summaryFor := func(date, userID, username, planID, planName, entitlementID string, lastCreatedAt time.Time) *PlanQuotaUsageDaySummary {
		date = strings.TrimSpace(date)
		userID = strings.TrimSpace(userID)
		username = strings.TrimSpace(username)
		planID = strings.TrimSpace(planID)
		planName = strings.TrimSpace(planName)
		entitlementID = strings.TrimSpace(entitlementID)
		if date == "" || userID == "" {
			return nil
		}
		if username == "" {
			username = userID
		}
		if planName == "" {
			planName = planID
		}
		if query.UserID != "" && !strings.EqualFold(userID, query.UserID) && !strings.EqualFold(username, query.UserID) {
			return nil
		}
		if query.EntitlementID != "" && entitlementID != query.EntitlementID {
			return nil
		}
		if query.PlanID != "" && planID != query.PlanID {
			return nil
		}
		keyEntitlementID := ""
		if query.EntitlementID != "" {
			keyEntitlementID = entitlementID
		}
		key := date + "\x00" + userID + "\x00" + planID + "\x00" + keyEntitlementID
		summary := byKey[key]
		if summary == nil {
			summary = &PlanQuotaUsageDaySummary{
				Date:          date,
				UserID:        userID,
				Username:      username,
				PlanID:        planID,
				PlanName:      planName,
				EntitlementID: keyEntitlementID,
			}
			byKey[key] = summary
		}
		if lastCreatedAt.After(summary.LastLedgerCreatedAt) {
			summary.LastLedgerCreatedAt = lastCreatedAt
		}
		return summary
	}

	sqlQuery := `
		SELECT pql.user_id,
			COALESCE(NULLIF(TRIM(u.username), ''), pql.user_id) AS username,
			COALESCE(upe.plan_id, '') AS plan_id,
			COALESCE(NULLIF(TRIM(upe.plan_name), ''), NULLIF(TRIM(bp.name), ''), NULLIF(TRIM(upe.plan_id), ''), NULLIF(TRIM(pql.note), ''), '') AS plan_name,
			pql.entitlement_id,
			pql.kind,
			pql.amount_nano_usd,
			pql.created_at_ns
		FROM plan_quota_ledger pql
		LEFT JOIN user_plan_entitlements upe ON upe.id = pql.entitlement_id
		LEFT JOIN users u ON u.id = pql.user_id
		LEFT JOIN billing_plans bp ON bp.id = upe.plan_id
		WHERE pql.user_id <> ''
			AND NOT (
				pql.kind = 'settle'
				AND pql.request_id <> ''
				AND pql.client_id <> ''
				AND EXISTS (
					SELECT 1 FROM billing_requests br
					WHERE br.request_id = pql.request_id AND br.client_id = pql.client_id
				)
				AND EXISTS (
					SELECT 1 FROM billing_funding_allocations bfa
					WHERE bfa.request_id = pql.request_id
						AND bfa.client_id = pql.client_id
						AND bfa.source = ?
						AND bfa.entitlement_id = pql.entitlement_id
				)
			)
	`
	args := []any{core.BillingFundingSourcePlan}
	if query.UserID != "" {
		sqlQuery += ` AND (LOWER(pql.user_id) = LOWER(?) OR LOWER(COALESCE(u.username, '')) = LOWER(?))`
		args = append(args, query.UserID, query.UserID)
	}
	if query.PlanID != "" {
		sqlQuery += ` AND COALESCE(upe.plan_id, '') = ?`
		args = append(args, query.PlanID)
	}
	if query.EntitlementID != "" {
		sqlQuery += ` AND pql.entitlement_id = ?`
		args = append(args, query.EntitlementID)
	}
	if !query.StartedAt.IsZero() {
		sqlQuery += ` AND pql.created_at_ns >= ?`
		args = append(args, sqliteTimeNS(query.StartedAt))
	}
	if !query.EndedAt.IsZero() {
		sqlQuery += ` AND pql.created_at_ns < ?`
		args = append(args, sqliteTimeNS(query.EndedAt))
	}

	rows, err := r.db.Query(sqlQuery, args...)
	if err != nil {
		return nil
	}

	for rows.Next() {
		var userID, username, planID, planName, entitlementID, kind string
		var amountNanoUSD, createdAtNS int64
		if err := rows.Scan(&userID, &username, &planID, &planName, &entitlementID, &kind, &amountNanoUSD, &createdAtNS); err != nil {
			continue
		}
		if amountNanoUSD == 0 {
			continue
		}
		userID = strings.TrimSpace(userID)
		if userID == "" {
			continue
		}
		username = strings.TrimSpace(username)
		if username == "" {
			username = userID
		}
		planID = strings.TrimSpace(planID)
		planName = strings.TrimSpace(planName)
		entitlementID = strings.TrimSpace(entitlementID)
		if planName == "" {
			planName = planID
		}
		createdAt := timeFromNS(createdAtNS)
		summary := summaryFor(billingDayKey(createdAt), userID, username, planID, planName, entitlementID, createdAt)
		if summary == nil {
			continue
		}
		addPlanQuotaUsageLedgerAmount(summary, kind, amountNanoUSD)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil
	}
	if err := rows.Close(); err != nil {
		return nil
	}

	allocationQuery := `
		SELECT COALESCE(NULLIF(TRIM(bfa.user_id), ''), br.user_id) AS user_id,
			COALESCE(NULLIF(TRIM(u.username), ''), COALESCE(NULLIF(TRIM(bfa.user_id), ''), br.user_id)) AS username,
			COALESCE(upe.plan_id, '') AS plan_id,
			COALESCE(NULLIF(TRIM(upe.plan_name), ''), NULLIF(TRIM(bp.name), ''), NULLIF(TRIM(upe.plan_id), ''), '') AS plan_name,
			bfa.entitlement_id,
			br.status,
			bfa.reserved_nano_usd,
			bfa.actual_nano_usd,
			br.created_at_ns
		FROM billing_funding_allocations bfa
		JOIN billing_requests br ON br.request_id = bfa.request_id AND br.client_id = bfa.client_id
		LEFT JOIN user_plan_entitlements upe ON upe.id = bfa.entitlement_id
		LEFT JOIN users u ON u.id = COALESCE(NULLIF(TRIM(bfa.user_id), ''), br.user_id)
		LEFT JOIN billing_plans bp ON bp.id = upe.plan_id
		WHERE bfa.source = ?
			AND bfa.entitlement_id <> ''
	`
	allocationArgs := []any{core.BillingFundingSourcePlan}
	if query.UserID != "" {
		allocationQuery += ` AND (LOWER(COALESCE(NULLIF(TRIM(bfa.user_id), ''), br.user_id)) = LOWER(?) OR LOWER(COALESCE(u.username, '')) = LOWER(?))`
		allocationArgs = append(allocationArgs, query.UserID, query.UserID)
	}
	if query.PlanID != "" {
		allocationQuery += ` AND COALESCE(upe.plan_id, '') = ?`
		allocationArgs = append(allocationArgs, query.PlanID)
	}
	if query.EntitlementID != "" {
		allocationQuery += ` AND bfa.entitlement_id = ?`
		allocationArgs = append(allocationArgs, query.EntitlementID)
	}
	if !query.StartedAt.IsZero() {
		allocationQuery += ` AND br.created_at_ns >= ?`
		allocationArgs = append(allocationArgs, sqliteTimeNS(query.StartedAt))
	}
	if !query.EndedAt.IsZero() {
		allocationQuery += ` AND br.created_at_ns < ?`
		allocationArgs = append(allocationArgs, sqliteTimeNS(query.EndedAt))
	}
	rows, err = r.db.Query(allocationQuery, allocationArgs...)
	if err != nil {
		return nil
	}
	for rows.Next() {
		var userID, username, planID, planName, entitlementID, status string
		var reservedNanoUSD, actualNanoUSD, createdAtNS int64
		if err := rows.Scan(&userID, &username, &planID, &planName, &entitlementID, &status, &reservedNanoUSD, &actualNanoUSD, &createdAtNS); err != nil {
			continue
		}
		amountNanoUSD := billingRequestUsageSpendAmount(core.BillingRequestStatus(status), reservedNanoUSD, actualNanoUSD)
		if amountNanoUSD <= 0 {
			continue
		}
		createdAt := timeFromNS(createdAtNS)
		summary := summaryFor(billingDayKey(createdAt), userID, username, planID, planName, entitlementID, createdAt)
		if summary == nil {
			continue
		}
		addPlanQuotaUsageUsedAmount(summary, amountNanoUSD)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil
	}
	if err := rows.Close(); err != nil {
		return nil
	}

	out := make([]PlanQuotaUsageDaySummary, 0, len(byKey))
	for _, summary := range byKey {
		out = append(out, *summary)
	}
	return sortPlanQuotaUsageDaySummaries(out)
}

func planSubscriptionSQLWhere(query PlanSubscriptionQuery) (string, []any) {
	query = normalizePlanSubscriptionQuery(query)
	clauses := []string{"upe.user_id <> ''"}
	args := []any{}
	if query.UserID != "" {
		clauses = append(clauses, "(LOWER(upe.user_id) = LOWER(?) OR LOWER(COALESCE(u.username, '')) = LOWER(?))")
		args = append(args, query.UserID, query.UserID)
	}
	if query.PlanID != "" {
		clauses = append(clauses, "upe.plan_id = ?")
		args = append(args, query.PlanID)
	}
	if query.Status != "" {
		clauses = append(clauses, "upe.status = ?")
		args = append(args, string(query.Status))
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

type billingPlanScanner interface {
	Scan(dest ...any) error
}

func getBillingPlanTx(queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}, id string) (core.BillingPlan, error) {
	row := queryer.QueryRow(`
		SELECT id, name, description, group_name, enabled, price_nano_usd, period_quota_nano_usd,
			period_duration_sec, period_count, sort_order, created_at_ns, updated_at_ns
		FROM billing_plans
		WHERE id = ?
	`, strings.TrimSpace(id))
	plan, err := scanBillingPlan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.BillingPlan{}, ErrNotFound
	}
	return plan, err
}

func scanBillingPlan(scanner billingPlanScanner) (core.BillingPlan, error) {
	var plan core.BillingPlan
	var enabled int
	var createdAtNS, updatedAtNS int64
	if err := scanner.Scan(&plan.ID, &plan.Name, &plan.Description, &plan.Group, &enabled, &plan.PriceNanoUSD, &plan.PeriodQuotaNanoUSD, &plan.PeriodDurationSec, &plan.PeriodCount, &plan.SortOrder, &createdAtNS, &updatedAtNS); err != nil {
		return core.BillingPlan{}, err
	}
	plan.Enabled = enabled != 0
	plan.CreatedAt = timeFromNS(createdAtNS)
	plan.UpdatedAt = timeFromNS(updatedAtNS)
	return normalizeBillingPlan(plan), nil
}

func getBillingPlanGroupTx(queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}, id string) (core.BillingPlanGroup, error) {
	row := queryer.QueryRow(`
		SELECT id, name, sale_disabled, quota_price_ratio, sort_order, created_at_ns, updated_at_ns
		FROM billing_plan_groups
		WHERE id = ?
	`, strings.TrimSpace(id))
	group, err := scanBillingPlanGroup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.BillingPlanGroup{}, ErrNotFound
	}
	return group, err
}

func scanBillingPlanGroup(scanner billingPlanScanner) (core.BillingPlanGroup, error) {
	var group core.BillingPlanGroup
	var saleDisabled int
	var createdAtNS, updatedAtNS int64
	if err := scanner.Scan(&group.ID, &group.Name, &saleDisabled, &group.QuotaPriceRatio, &group.SortOrder, &createdAtNS, &updatedAtNS); err != nil {
		return core.BillingPlanGroup{}, err
	}
	group.SaleDisabled = saleDisabled != 0
	group.CreatedAt = timeFromNS(createdAtNS)
	group.UpdatedAt = timeFromNS(updatedAtNS)
	return normalizeBillingPlanGroup(group), nil
}

func activeUserPlanEntitlementTx(tx *sql.Tx, userID string, now time.Time) (core.UserPlanEntitlement, bool, error) {
	entitlements, err := activeUserPlanEntitlementsTx(tx, userID, now)
	if err != nil {
		return core.UserPlanEntitlement{}, false, err
	}
	var active core.UserPlanEntitlement
	found := false
	for _, entitlement := range entitlements {
		if entitlement.Status == core.UserPlanEntitlementActive {
			if !found || planEntitlementPriorityLess(active, entitlement) {
				active = entitlement
				found = true
			}
		}
	}
	return active, found, nil
}

func activeUserPlanEntitlementsTx(tx *sql.Tx, userID string, now time.Time) ([]core.UserPlanEntitlement, error) {
	rows, err := tx.Query(`
		SELECT id, user_id, plan_id, plan_name, status, price_nano_usd, period_quota_nano_usd, base_period_quota_nano_usd,
			period_duration_sec, total_periods, remaining_periods, current_quota_nano_usd,
			priority, current_period_started_at_ns, current_period_ends_at_ns, expires_at_ns, purchased_at_ns, updated_at_ns
		FROM user_plan_entitlements
		WHERE user_id = ? AND status IN (?, ?)
		ORDER BY CASE WHEN priority > 0 THEN 0 ELSE 1 END ASC, priority ASC, purchased_at_ns ASC, id ASC
	`, strings.TrimSpace(userID), string(core.UserPlanEntitlementActive), string(core.UserPlanEntitlementExpired))
	if err != nil {
		return nil, err
	}
	entitlements := make([]core.UserPlanEntitlement, 0)
	for rows.Next() {
		entitlement, err := scanUserPlanEntitlement(rows)
		if err != nil {
			continue
		}
		entitlements = append(entitlements, entitlement)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]core.UserPlanEntitlement, 0, len(entitlements))
	for _, entitlement := range entitlements {
		entitlement, entries, changed := advanceUserPlanEntitlement(entitlement, now)
		if changed {
			if err := updateUserPlanEntitlementTx(tx, entitlement); err != nil {
				return nil, err
			}
			for _, entry := range entries {
				if err := insertPlanQuotaLedgerTx(tx, entry); err != nil {
					return nil, err
				}
			}
		}
		if entitlement.Status == core.UserPlanEntitlementActive {
			out = append(out, entitlement)
		}
	}
	return sortUserPlanEntitlements(out), nil
}

func nextUserPlanEntitlementPriorityTx(tx *sql.Tx, userID string, now time.Time) (int, error) {
	entitlements, err := activeUserPlanEntitlementsTx(tx, userID, now)
	if err != nil {
		return 0, err
	}
	maxPriority := 0
	for _, entitlement := range entitlements {
		if entitlement.Priority > maxPriority {
			maxPriority = entitlement.Priority
		}
	}
	if maxPriority > 0 {
		return maxPriority + 1, nil
	}
	return len(entitlements) + 1, nil
}

func moveUserPlanEntitlementPriorityTx(tx *sql.Tx, entitlements []core.UserPlanEntitlement, entitlementID, direction string) error {
	entitlementID = strings.TrimSpace(entitlementID)
	index := -1
	for i, entitlement := range entitlements {
		if entitlement.ID == entitlementID {
			index = i
			break
		}
	}
	if index < 0 {
		return ErrNotFound
	}
	target := index
	switch strings.ToLower(strings.TrimSpace(direction)) {
	case "up":
		target = index - 1
	case "down":
		target = index + 1
	default:
		return fmt.Errorf("unsupported priority direction")
	}
	if target < 0 || target >= len(entitlements) {
		return nil
	}
	entitlements[index], entitlements[target] = entitlements[target], entitlements[index]
	for i, entitlement := range entitlements {
		entitlement.Priority = i + 1
		if err := updateUserPlanEntitlementTx(tx, entitlement); err != nil {
			return err
		}
	}
	return nil
}

func scanUserPlanEntitlement(scanner billingPlanScanner) (core.UserPlanEntitlement, error) {
	var entitlement core.UserPlanEntitlement
	var status string
	var startedAtNS, endsAtNS, expiresAtNS, purchasedAtNS, updatedAtNS int64
	if err := scanner.Scan(
		&entitlement.ID,
		&entitlement.UserID,
		&entitlement.PlanID,
		&entitlement.PlanName,
		&status,
		&entitlement.PriceNanoUSD,
		&entitlement.PeriodQuotaNanoUSD,
		&entitlement.BasePeriodQuotaNanoUSD,
		&entitlement.PeriodDurationSec,
		&entitlement.TotalPeriods,
		&entitlement.RemainingPeriods,
		&entitlement.CurrentQuotaNanoUSD,
		&entitlement.Priority,
		&startedAtNS,
		&endsAtNS,
		&expiresAtNS,
		&purchasedAtNS,
		&updatedAtNS,
	); err != nil {
		return core.UserPlanEntitlement{}, err
	}
	entitlement.Status = core.UserPlanEntitlementStatus(status)
	entitlement.CurrentPeriodStartedAt = timeFromNS(startedAtNS)
	entitlement.CurrentPeriodEndsAt = timeFromNS(endsAtNS)
	entitlement.ExpiresAt = timeFromNS(expiresAtNS)
	entitlement.PurchasedAt = timeFromNS(purchasedAtNS)
	entitlement.UpdatedAt = timeFromNS(updatedAtNS)
	return normalizeUserPlanEntitlement(entitlement), nil
}

func scanPlanSubscriptionSummary(scanner billingPlanScanner) (PlanSubscriptionSummary, error) {
	var item PlanSubscriptionSummary
	var status string
	var startedAtNS, endsAtNS, expiresAtNS, purchasedAtNS, updatedAtNS int64
	if err := scanner.Scan(
		&item.Entitlement.ID,
		&item.Entitlement.UserID,
		&item.Username,
		&item.Entitlement.PlanID,
		&item.Entitlement.PlanName,
		&item.PlanGroup,
		&status,
		&item.Entitlement.PriceNanoUSD,
		&item.Entitlement.PeriodQuotaNanoUSD,
		&item.Entitlement.BasePeriodQuotaNanoUSD,
		&item.Entitlement.PeriodDurationSec,
		&item.Entitlement.TotalPeriods,
		&item.Entitlement.RemainingPeriods,
		&item.Entitlement.CurrentQuotaNanoUSD,
		&item.Entitlement.Priority,
		&startedAtNS,
		&endsAtNS,
		&expiresAtNS,
		&purchasedAtNS,
		&updatedAtNS,
		&item.UserBalanceNanoUSD,
	); err != nil {
		return PlanSubscriptionSummary{}, err
	}
	item.Entitlement.Status = core.UserPlanEntitlementStatus(status)
	item.Entitlement.CurrentPeriodStartedAt = timeFromNS(startedAtNS)
	item.Entitlement.CurrentPeriodEndsAt = timeFromNS(endsAtNS)
	item.Entitlement.ExpiresAt = timeFromNS(expiresAtNS)
	item.Entitlement.PurchasedAt = timeFromNS(purchasedAtNS)
	item.Entitlement.UpdatedAt = timeFromNS(updatedAtNS)
	item.Entitlement = normalizeUserPlanEntitlement(item.Entitlement)
	item.Username = strings.TrimSpace(item.Username)
	if item.Username == "" {
		item.Username = item.Entitlement.UserID
	}
	item.PlanGroup = strings.TrimSpace(item.PlanGroup)
	return item, nil
}

func insertUserPlanEntitlementTx(tx *sql.Tx, entitlement core.UserPlanEntitlement) error {
	_, err := tx.Exec(`
		INSERT INTO user_plan_entitlements(
			id, user_id, plan_id, plan_name, status, price_nano_usd, period_quota_nano_usd, base_period_quota_nano_usd,
			period_duration_sec, total_periods, remaining_periods, current_quota_nano_usd,
			priority, current_period_started_at_ns, current_period_ends_at_ns, expires_at_ns, purchased_at_ns, updated_at_ns
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, entitlement.ID, entitlement.UserID, entitlement.PlanID, entitlement.PlanName, string(entitlement.Status), entitlement.PriceNanoUSD, entitlement.PeriodQuotaNanoUSD, entitlement.BasePeriodQuotaNanoUSD, entitlement.PeriodDurationSec, entitlement.TotalPeriods, entitlement.RemainingPeriods, entitlement.CurrentQuotaNanoUSD, entitlement.Priority, sqliteTimeNS(entitlement.CurrentPeriodStartedAt), sqliteTimeNS(entitlement.CurrentPeriodEndsAt), sqliteTimeNS(entitlement.ExpiresAt), sqliteTimeNS(entitlement.PurchasedAt), sqliteTimeNS(entitlement.UpdatedAt))
	return err
}

func updateUserPlanEntitlementTx(tx *sql.Tx, entitlement core.UserPlanEntitlement) error {
	_, err := tx.Exec(`
		UPDATE user_plan_entitlements
		SET status = ?,
			period_quota_nano_usd = ?,
			base_period_quota_nano_usd = ?,
			total_periods = ?,
			remaining_periods = ?,
			current_quota_nano_usd = ?,
			priority = ?,
			current_period_started_at_ns = ?,
			current_period_ends_at_ns = ?,
			expires_at_ns = ?,
			updated_at_ns = ?
		WHERE id = ?
	`, string(entitlement.Status), entitlement.PeriodQuotaNanoUSD, entitlement.BasePeriodQuotaNanoUSD, entitlement.TotalPeriods, entitlement.RemainingPeriods, entitlement.CurrentQuotaNanoUSD, entitlement.Priority, sqliteTimeNS(entitlement.CurrentPeriodStartedAt), sqliteTimeNS(entitlement.CurrentPeriodEndsAt), sqliteTimeNS(entitlement.ExpiresAt), sqliteTimeNS(entitlement.UpdatedAt), entitlement.ID)
	return err
}

func cancelUserPlanEntitlementTx(tx *sql.Tx, userID, entitlementID string, now time.Time) (core.UserPlanEntitlement, error) {
	userID = strings.TrimSpace(userID)
	entitlementID = strings.TrimSpace(entitlementID)
	if userID == "" || entitlementID == "" {
		return core.UserPlanEntitlement{}, ErrNotFound
	}
	entitlement, ok, err := getUserPlanEntitlementByIDTx(tx, entitlementID)
	if err != nil {
		return core.UserPlanEntitlement{}, err
	}
	if !ok || strings.TrimSpace(entitlement.UserID) != userID {
		return core.UserPlanEntitlement{}, ErrNotFound
	}
	entitlement, entries, changed := advanceUserPlanEntitlement(entitlement, now)
	if changed {
		if err := updateUserPlanEntitlementTx(tx, entitlement); err != nil {
			return core.UserPlanEntitlement{}, err
		}
		for _, entry := range entries {
			if err := insertPlanQuotaLedgerTx(tx, entry); err != nil {
				return core.UserPlanEntitlement{}, err
			}
		}
	}
	if entitlement.Status != core.UserPlanEntitlementActive {
		return core.UserPlanEntitlement{}, ErrNotFound
	}
	cancelled, entry, hasEntry := cancelUserPlanEntitlement(entitlement, now, 0)
	if err := updateUserPlanEntitlementTx(tx, cancelled); err != nil {
		return core.UserPlanEntitlement{}, err
	}
	if hasEntry {
		if err := insertPlanQuotaLedgerTx(tx, entry); err != nil {
			return core.UserPlanEntitlement{}, err
		}
	}
	return cancelled, nil
}

func cancelUserPlanEntitlementsTx(tx *sql.Tx, userID string, now time.Time) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil
	}
	rows, err := tx.Query(`
		SELECT id, user_id, plan_id, plan_name, status, price_nano_usd, period_quota_nano_usd, base_period_quota_nano_usd,
			period_duration_sec, total_periods, remaining_periods, current_quota_nano_usd,
			priority, current_period_started_at_ns, current_period_ends_at_ns, expires_at_ns, purchased_at_ns, updated_at_ns
		FROM user_plan_entitlements
		WHERE user_id = ? AND status = ?
	`, userID, string(core.UserPlanEntitlementActive))
	if err != nil {
		return err
	}
	entitlements := make([]core.UserPlanEntitlement, 0)
	for rows.Next() {
		entitlement, err := scanUserPlanEntitlement(rows)
		if err != nil {
			_ = rows.Close()
			return err
		}
		entitlements = append(entitlements, entitlement)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for index, entitlement := range entitlements {
		entitlement, entry, hasEntry := cancelUserPlanEntitlement(entitlement, now, index)
		if err := updateUserPlanEntitlementTx(tx, entitlement); err != nil {
			return err
		}
		if !hasEntry {
			continue
		}
		if err := insertPlanQuotaLedgerTx(tx, entry); err != nil {
			return err
		}
	}
	return nil
}

func mergeUserPlanEntitlementsTx(tx *sql.Tx, sourceID, targetID string, now time.Time) error {
	sourceID = strings.TrimSpace(sourceID)
	targetID = strings.TrimSpace(targetID)
	if sourceID == "" || targetID == "" || sourceID == targetID {
		return nil
	}
	if _, sourceActive, err := activeUserPlanEntitlementTx(tx, sourceID, now); err != nil {
		return err
	} else if sourceActive {
		if _, targetActive, err := activeUserPlanEntitlementTx(tx, targetID, now); err != nil {
			return err
		} else if targetActive {
			return ErrBillingRequestConflict
		}
	}
	_, err := tx.Exec(`
		UPDATE user_plan_entitlements
		SET user_id = ?,
			updated_at_ns = ?
		WHERE user_id = ?
	`, targetID, sqliteTimeNS(now), sourceID)
	return err
}

func insertPlanQuotaLedgerTx(tx *sql.Tx, entry core.PlanQuotaLedgerEntry) error {
	_, err := tx.Exec(`
		INSERT INTO plan_quota_ledger(id, entitlement_id, user_id, client_id, request_id, kind, amount_nano_usd, quota_after_nano_usd, note, created_at_ns)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, entry.ID, entry.EntitlementID, entry.UserID, entry.ClientID, entry.RequestID, entry.Kind, entry.AmountNanoUSD, entry.QuotaAfterNanoUSD, entry.Note, sqliteTimeNS(entry.CreatedAt))
	return err
}

func getUserPlanEntitlementByIDTx(tx *sql.Tx, id string) (core.UserPlanEntitlement, bool, error) {
	row := tx.QueryRow(`
		SELECT id, user_id, plan_id, plan_name, status, price_nano_usd, period_quota_nano_usd, base_period_quota_nano_usd,
			period_duration_sec, total_periods, remaining_periods, current_quota_nano_usd,
			priority, current_period_started_at_ns, current_period_ends_at_ns, expires_at_ns, purchased_at_ns, updated_at_ns
		FROM user_plan_entitlements
		WHERE id = ?
	`, strings.TrimSpace(id))
	entitlement, err := scanUserPlanEntitlement(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.UserPlanEntitlement{}, false, nil
	}
	return entitlement, err == nil, err
}

func insertBillingFundingAllocationTx(tx *sql.Tx, allocation core.BillingFundingAllocation) error {
	_, err := tx.Exec(`
		INSERT INTO billing_funding_allocations(
			id, request_id, client_id, user_id, source, entitlement_id,
			reserved_nano_usd, actual_nano_usd, created_at_ns, updated_at_ns
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(request_id, client_id, source, entitlement_id) DO UPDATE SET
			reserved_nano_usd = excluded.reserved_nano_usd,
			actual_nano_usd = excluded.actual_nano_usd,
			updated_at_ns = excluded.updated_at_ns
	`, allocation.ID, allocation.RequestID, allocation.ClientID, allocation.UserID, allocation.Source, allocation.EntitlementID, allocation.ReservedNanoUSD, allocation.ActualNanoUSD, sqliteTimeNS(allocation.CreatedAt), sqliteTimeNS(allocation.UpdatedAt))
	return err
}

func upsertBillingFundingAllocationActualDeltaTx(tx *sql.Tx, requestID, clientID, userID, source, entitlementID string, delta int64, now time.Time) error {
	if delta == 0 {
		return nil
	}
	allocation, ok, err := getBillingFundingAllocationTx(tx, requestID, clientID, source, entitlementID)
	if err != nil {
		return err
	}
	if !ok {
		allocation = core.BillingFundingAllocation{
			ID:            billingAllocationID(requestID, clientID, source, entitlementID),
			RequestID:     strings.TrimSpace(requestID),
			ClientID:      strings.TrimSpace(clientID),
			UserID:        strings.TrimSpace(userID),
			Source:        strings.TrimSpace(source),
			EntitlementID: strings.TrimSpace(entitlementID),
			CreatedAt:     now,
		}
	}
	allocation.ActualNanoUSD += delta
	if allocation.ActualNanoUSD < 0 {
		allocation.ActualNanoUSD = 0
	}
	allocation.UpdatedAt = now
	return insertBillingFundingAllocationTx(tx, allocation)
}

func getBillingFundingAllocationTx(tx *sql.Tx, requestID, clientID, source, entitlementID string) (core.BillingFundingAllocation, bool, error) {
	row := tx.QueryRow(`
		SELECT id, request_id, client_id, user_id, source, entitlement_id,
			reserved_nano_usd, actual_nano_usd, created_at_ns, updated_at_ns
		FROM billing_funding_allocations
		WHERE request_id = ? AND client_id = ? AND source = ? AND entitlement_id = ?
	`, strings.TrimSpace(requestID), strings.TrimSpace(clientID), strings.TrimSpace(source), strings.TrimSpace(entitlementID))
	allocation, err := scanBillingFundingAllocation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.BillingFundingAllocation{}, false, nil
	}
	return allocation, err == nil, err
}

func listBillingFundingAllocationsTx(tx *sql.Tx, requestID, clientID string) ([]core.BillingFundingAllocation, error) {
	rows, err := tx.Query(`
		SELECT id, request_id, client_id, user_id, source, entitlement_id,
			reserved_nano_usd, actual_nano_usd, created_at_ns, updated_at_ns
		FROM billing_funding_allocations
		WHERE request_id = ? AND client_id = ?
	`, strings.TrimSpace(requestID), strings.TrimSpace(clientID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.BillingFundingAllocation, 0, 2)
	for rows.Next() {
		allocation, err := scanBillingFundingAllocation(rows)
		if err == nil {
			out = append(out, allocation)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func scanBillingFundingAllocation(scanner billingPlanScanner) (core.BillingFundingAllocation, error) {
	var allocation core.BillingFundingAllocation
	var createdAtNS, updatedAtNS int64
	if err := scanner.Scan(
		&allocation.ID,
		&allocation.RequestID,
		&allocation.ClientID,
		&allocation.UserID,
		&allocation.Source,
		&allocation.EntitlementID,
		&allocation.ReservedNanoUSD,
		&allocation.ActualNanoUSD,
		&createdAtNS,
		&updatedAtNS,
	); err != nil {
		return core.BillingFundingAllocation{}, err
	}
	allocation.CreatedAt = timeFromNS(createdAtNS)
	allocation.UpdatedAt = timeFromNS(updatedAtNS)
	return allocation, nil
}
