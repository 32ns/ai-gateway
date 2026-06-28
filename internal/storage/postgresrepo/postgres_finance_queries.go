package postgresrepo

import (
	"slices"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func (r *PostgresRepository) FinanceOverviewStats(startOfDay, endOfDay time.Time) FinanceOverviewStats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var stats FinanceOverviewStats
	_ = r.db.QueryRow(`SELECT COUNT(*) FROM finance_user_rollups WHERE user_id <> ''`).Scan(&stats.TotalUsers)
	_ = r.db.QueryRow(`SELECT COALESCE(SUM(balance_nano_usd), 0) FROM user_balances`).Scan(&stats.TotalBalanceNanoUSD)
	_ = r.db.QueryRow(`SELECT COUNT(*) FROM finance_client_rollups WHERE client_id <> ''`).Scan(&stats.TotalClients)
	_ = r.db.QueryRow(`SELECT COALESCE(SUM(CASE WHEN enabled THEN 1 ELSE 0 END), 0) FROM clients`).Scan(&stats.ActiveClients)
	_ = r.db.QueryRow(`
		SELECT COALESCE(SUM(amount_nano_usd), 0)
		FROM payment_orders
		WHERE status = ?
	`, string(core.PaymentOrderPaid)).Scan(&stats.PaidOrderNanoUSD)
	_ = r.db.QueryRow(`
		SELECT COALESCE(SUM(amount_nano_usd), 0)
		FROM payment_orders
		WHERE status = ?
			AND (
				(paid_at_ns > 0 AND paid_at_ns >= ? AND paid_at_ns < ?)
				OR (paid_at_ns = 0 AND updated_at_ns >= ? AND updated_at_ns < ?)
			)
	`, string(core.PaymentOrderPaid), sqliteTimeNS(startOfDay), sqliteTimeNS(endOfDay), sqliteTimeNS(startOfDay), sqliteTimeNS(endOfDay)).Scan(&stats.TodayIncomeNanoUSD)

	var todayRefunds int64
	_ = r.db.QueryRow(`
		SELECT COALESCE(SUM(amount_nano_usd), 0)
		FROM payment_refunds
		WHERE status = ?
			AND updated_at_ns >= ?
			AND updated_at_ns < ?
	`, string(core.PaymentRefundDone), sqliteTimeNS(startOfDay), sqliteTimeNS(endOfDay)).Scan(&todayRefunds)
	stats.TodayIncomeNanoUSD = addNanoUSDSaturating(stats.TodayIncomeNanoUSD, -todayRefunds)

	_ = r.db.QueryRow(`
		SELECT COALESCE(SUM(CASE
			WHEN status IN (?, ?, ?) THEN 0
			WHEN status = ? AND actual_nano_usd > 0 THEN actual_nano_usd
			WHEN actual_nano_usd > 0 THEN actual_nano_usd
			ELSE 0
		END), 0)
		FROM billing_requests
		WHERE created_at_ns >= ?
			AND created_at_ns < ?
	`, string(core.BillingRequestReleased), string(core.BillingRequestUsageMissing), string(core.BillingRequestReserved), string(core.BillingRequestSettled), sqliteTimeNS(startOfDay), sqliteTimeNS(endOfDay)).Scan(&stats.TodaySpendNanoUSD)
	_ = r.db.QueryRow(`
		SELECT COALESCE(SUM(CASE
			WHEN status IN (?, ?, ?) THEN 0
			WHEN status = ? AND actual_nano_usd > 0 THEN actual_nano_usd
			WHEN actual_nano_usd > 0 THEN actual_nano_usd
			ELSE 0
		END), 0)
		FROM billing_requests
		WHERE created_at_ns >= ?
			AND created_at_ns < ?
			AND billing_source = ?
	`, string(core.BillingRequestReleased), string(core.BillingRequestUsageMissing), string(core.BillingRequestReserved), string(core.BillingRequestSettled), sqliteTimeNS(startOfDay), sqliteTimeNS(endOfDay), string(core.ClientBillingSourcePlan)).Scan(&stats.TodayPlanSpendNanoUSD)
	stats.TodayBalanceSpendNanoUSD = addNanoUSDSaturating(stats.TodaySpendNanoUSD, -stats.TodayPlanSpendNanoUSD)
	_ = r.db.QueryRow(`SELECT COUNT(*) FROM billing_requests WHERE status = ?`, string(core.BillingRequestUsageMissing)).Scan(&stats.UsageMissingCount)
	_ = r.db.QueryRow(`SELECT COUNT(*) FROM billing_requests WHERE status = ?`, string(core.BillingRequestReserved)).Scan(&stats.UnsettledRequestCount)
	stats.ReconcileIssueCount = r.financeReconcileIssueCountLocked(time.Now().UTC())
	return stats
}

func (r *PostgresRepository) FinanceEntityCounts() FinanceEntityCounts {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var counts FinanceEntityCounts
	_ = r.db.QueryRow(`SELECT COUNT(*) FROM finance_user_rollups WHERE user_id <> ''`).Scan(&counts.TotalUsers)
	_ = r.db.QueryRow(`SELECT COUNT(*) FROM finance_client_rollups WHERE client_id <> ''`).Scan(&counts.TotalClients)
	_ = r.db.QueryRow(`SELECT COALESCE(SUM(CASE WHEN enabled THEN 1 ELSE 0 END), 0) FROM clients`).Scan(&counts.ActiveClients)
	return counts
}

func (r *PostgresRepository) FinanceTotalSpendNanoUSD() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var total int64
	_ = r.db.QueryRow(`SELECT COALESCE(SUM(spend_nano_usd), 0) FROM finance_user_rollups WHERE user_id <> ''`).Scan(&total)
	return total
}

func (r *PostgresRepository) financeReconcileIssueCountLocked(now time.Time) int {
	var staleReserved int
	cutoff := now.Add(-time.Hour)
	_ = r.db.QueryRow(`
		SELECT COUNT(*)
		FROM billing_requests
		WHERE status = ? AND created_at_ns < ?
	`, string(core.BillingRequestReserved), sqliteTimeNS(cutoff)).Scan(&staleReserved)
	var negativeBalances int
	_ = r.db.QueryRow(`SELECT COUNT(*) FROM user_balances WHERE balance_nano_usd < 0`).Scan(&negativeBalances)
	var clientSpendOverLimit int
	_ = r.db.QueryRow(`
		SELECT COUNT(*)
		FROM client_spend
		WHERE spend_limit_nano_usd > 0 AND spend_used_nano_usd > spend_limit_nano_usd
	`).Scan(&clientSpendOverLimit)
	return negativeBalances + clientSpendOverLimit + staleReserved
}

func (r *PostgresRepository) ListFinanceReconcileIssues(now time.Time) []FinanceReconcileIssueSummary {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	issues := make([]FinanceReconcileIssueSummary, 0)
	rows, err := r.db.Query(`
		SELECT b.user_id,
			COALESCE(NULLIF(TRIM(u.username), ''), b.user_id) AS username,
			b.balance_nano_usd
		FROM user_balances b
		LEFT JOIN users u ON u.id = b.user_id
		WHERE b.balance_nano_usd < 0
		ORDER BY b.balance_nano_usd ASC, b.user_id ASC
	`)
	if err == nil {
		for rows.Next() {
			var userID, username string
			var balance int64
			if err := rows.Scan(&userID, &username, &balance); err != nil {
				continue
			}
			issues = append(issues, FinanceReconcileIssueSummary{
				Kind:         "negative_balance",
				Severity:     "error",
				ResourceID:   userID,
				Message:      username,
				DeltaNanoUSD: balance,
			})
		}
		_ = rows.Close()
	}

	rows, err = r.db.Query(`
		SELECT cs.client_id,
			COALESCE(NULLIF(TRIM(c.name), ''), cs.client_id) AS client_name,
			cs.spend_used_nano_usd - cs.spend_limit_nano_usd
		FROM client_spend cs
		LEFT JOIN clients c ON c.id = cs.client_id
		WHERE cs.spend_limit_nano_usd > 0 AND cs.spend_used_nano_usd > cs.spend_limit_nano_usd
		ORDER BY (cs.spend_used_nano_usd - cs.spend_limit_nano_usd) DESC, cs.client_id ASC
	`)
	if err == nil {
		for rows.Next() {
			var clientID, clientName string
			var overage int64
			if err := rows.Scan(&clientID, &clientName, &overage); err != nil {
				continue
			}
			issues = append(issues, FinanceReconcileIssueSummary{
				Kind:         "client_spend_over_limit",
				Severity:     "warning",
				ResourceID:   clientID,
				Message:      clientName,
				DeltaNanoUSD: overage,
			})
		}
		_ = rows.Close()
	}

	cutoff := now.Add(-time.Hour)
	rows, err = r.db.Query(`
		SELECT request_id, client_id
		FROM billing_requests
		WHERE status = ? AND created_at_ns < ?
		ORDER BY created_at_ns ASC, id ASC
	`, string(core.BillingRequestReserved), sqliteTimeNS(cutoff))
	if err == nil {
		for rows.Next() {
			var requestID, clientID string
			if err := rows.Scan(&requestID, &clientID); err != nil {
				continue
			}
			issues = append(issues, FinanceReconcileIssueSummary{
				Kind:       "stale_reserved_request",
				Severity:   "warning",
				ResourceID: requestID,
				Message:    clientID,
			})
		}
		_ = rows.Close()
	}
	return issues
}

func (r *PostgresRepository) ListPaymentIncomeByDay(startedAt time.Time, days int) []PaymentIncomeDaySummary {
	if days <= 0 || startedAt.IsZero() {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	endedAt := startedAt.AddDate(0, 0, days)
	rows, err := r.db.Query(`
		SELECT CASE WHEN paid_at_ns > 0 THEN paid_at_ns ELSE updated_at_ns END, amount_nano_usd, 1
		FROM payment_orders
		WHERE status = ?
			AND (
				(paid_at_ns > 0 AND paid_at_ns >= ? AND paid_at_ns < ?)
				OR (paid_at_ns = 0 AND updated_at_ns >= ? AND updated_at_ns < ?)
			)
		UNION ALL
		SELECT updated_at_ns, -amount_nano_usd, 0
		FROM payment_refunds
		WHERE status = ?
			AND updated_at_ns >= ?
			AND updated_at_ns < ?
	`, string(core.PaymentOrderPaid), sqliteTimeNS(startedAt), sqliteTimeNS(endedAt), sqliteTimeNS(startedAt), sqliteTimeNS(endedAt), string(core.PaymentRefundDone), sqliteTimeNS(startedAt), sqliteTimeNS(endedAt))
	if err != nil {
		return nil
	}
	defer rows.Close()

	byDate := make(map[string]*PaymentIncomeDaySummary, days)
	for rows.Next() {
		var whenNS int64
		var amount int64
		var orderCount int
		if err := rows.Scan(&whenNS, &amount, &orderCount); err != nil {
			continue
		}
		date := billingDayKey(timeFromNS(whenNS))
		if date == "" {
			continue
		}
		summary := byDate[date]
		if summary == nil {
			summary = &PaymentIncomeDaySummary{Date: date}
			byDate[date] = summary
		}
		summary.IncomeNanoUSD = addNanoUSDSaturating(summary.IncomeNanoUSD, amount)
		summary.OrderCount += orderCount
	}
	out := make([]PaymentIncomeDaySummary, 0, len(byDate))
	for _, summary := range byDate {
		out = append(out, *summary)
	}
	slices.SortFunc(out, func(a, b PaymentIncomeDaySummary) int {
		return strings.Compare(a.Date, b.Date)
	})
	return out
}

func (r *PostgresRepository) ListFinanceTopUsersBySpend(limit int) []FinanceUserSummary {
	if limit <= 0 {
		limit = 5
	}
	if limit > 100 {
		limit = 100
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`
		WITH top_users AS (
			SELECT user_id, username, balance_nano_usd, recharge_nano_usd, reward_nano_usd,
				spend_nano_usd, usage_spend_nano_usd, plan_spend_nano_usd,
				refund_nano_usd, last_payment_at_ns, last_spend_at_ns
			FROM finance_user_rollups
			WHERE user_id <> ''
			ORDER BY spend_nano_usd DESC, user_id ASC
			LIMIT ?
		)
		SELECT tu.user_id,
			COALESCE(NULLIF(TRIM(u.username), ''), NULLIF(TRIM(tu.username), ''), tu.user_id) AS username,
			COALESCE(b.balance_nano_usd, tu.balance_nano_usd, 0) AS balance_nano_usd,
			COALESCE(tu.recharge_nano_usd, 0) AS recharge_nano_usd,
			COALESCE(tu.reward_nano_usd, 0) AS reward_nano_usd,
			COALESCE(tu.spend_nano_usd, 0) AS spend_nano_usd,
			COALESCE(tu.usage_spend_nano_usd, 0) AS usage_spend_nano_usd,
			COALESCE(tu.plan_spend_nano_usd, 0) AS plan_spend_nano_usd,
			COALESCE(tu.refund_nano_usd, 0) AS refund_nano_usd,
			COALESCE(tu.last_payment_at_ns, 0) AS last_payment_at_ns,
			COALESCE(tu.last_spend_at_ns, 0) AS last_spend_at_ns,
			0 AS client_count,
			0 AS client_spend_nano_usd,
			0 AS client_usage_nano_usd,
			0 AS client_plan_nano_usd
		FROM top_users tu
		LEFT JOIN users u ON u.id = tu.user_id
		LEFT JOIN user_balances b ON b.user_id = tu.user_id
		ORDER BY tu.spend_nano_usd DESC, username ASC, tu.user_id ASC
	`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]FinanceUserSummary, 0, limit)
	for rows.Next() {
		var summary FinanceUserSummary
		var lastPaymentAtNS int64
		var lastSpendAtNS int64
		if err := rows.Scan(&summary.UserID, &summary.Username, &summary.BalanceNanoUSD, &summary.RechargeNanoUSD, &summary.RewardNanoUSD, &summary.SpendNanoUSD, &summary.UsageSpendNanoUSD, &summary.PlanSpendNanoUSD, &summary.RefundNanoUSD, &lastPaymentAtNS, &lastSpendAtNS, &summary.ClientCount, &summary.ClientSpendNanoUSD, &summary.ClientUsageNanoUSD, &summary.ClientPlanNanoUSD); err != nil {
			continue
		}
		summary.LastPaymentAt = timePtrFromNS(lastPaymentAtNS)
		summary.LastSpendAt = timePtrFromNS(lastSpendAtNS)
		out = append(out, summary)
	}
	return out
}

func (r *PostgresRepository) ListFinanceUserSummariesPage(offset, limit int) ([]FinanceUserSummary, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 25
	}
	if limit > 100 {
		limit = 100
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	total := 0
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM finance_user_rollups WHERE user_id <> ''`).Scan(&total); err != nil {
		return nil, 0
	}

	rows, err := r.db.Query(`
		WITH page_users AS (
			SELECT user_id, username, balance_nano_usd, recharge_nano_usd, reward_nano_usd,
				spend_nano_usd, usage_spend_nano_usd, plan_spend_nano_usd,
				refund_nano_usd, last_payment_at_ns, last_spend_at_ns
			FROM finance_user_rollups
			WHERE user_id <> ''
			ORDER BY spend_nano_usd DESC, user_id ASC
			LIMIT ? OFFSET ?
		),
		client_counts AS (
			SELECT owner_user_id AS user_id,
				COUNT(*) AS client_count,
				COALESCE(SUM(spend_used_nano_usd), 0) AS client_spend_nano_usd,
				COALESCE(SUM(usage_nano_usd), 0) AS client_usage_nano_usd,
				COALESCE(SUM(plan_nano_usd), 0) AS client_plan_nano_usd
			FROM finance_client_rollups
			WHERE owner_user_id IN (SELECT user_id FROM page_users)
			GROUP BY owner_user_id
		)
		SELECT pu.user_id,
			COALESCE(NULLIF(TRIM(u.username), ''), NULLIF(TRIM(pu.username), ''), pu.user_id) AS username,
			COALESCE(b.balance_nano_usd, pu.balance_nano_usd, 0) AS balance_nano_usd,
			COALESCE(pu.recharge_nano_usd, 0) AS recharge_nano_usd,
			COALESCE(pu.reward_nano_usd, 0) AS reward_nano_usd,
			COALESCE(pu.spend_nano_usd, 0) AS spend_nano_usd,
			COALESCE(pu.usage_spend_nano_usd, 0) AS usage_spend_nano_usd,
			COALESCE(pu.plan_spend_nano_usd, 0) AS plan_spend_nano_usd,
			COALESCE(pu.refund_nano_usd, 0) AS refund_nano_usd,
			COALESCE(pu.last_payment_at_ns, 0) AS last_payment_at_ns,
			COALESCE(pu.last_spend_at_ns, 0) AS last_spend_at_ns,
			COALESCE(cc.client_count, 0) AS client_count,
			COALESCE(cc.client_spend_nano_usd, 0) AS client_spend_nano_usd,
			COALESCE(cc.client_usage_nano_usd, 0) AS client_usage_nano_usd,
			COALESCE(cc.client_plan_nano_usd, 0) AS client_plan_nano_usd
		FROM page_users pu
		LEFT JOIN users u ON u.id = pu.user_id
		LEFT JOIN user_balances b ON b.user_id = pu.user_id
		LEFT JOIN client_counts cc ON cc.user_id = pu.user_id
		ORDER BY pu.spend_nano_usd DESC, username ASC, pu.user_id ASC
	`, limit, offset)
	if err != nil {
		return nil, 0
	}
	defer rows.Close()

	out := make([]FinanceUserSummary, 0, limit)
	for rows.Next() {
		var summary FinanceUserSummary
		var lastPaymentAtNS int64
		var lastSpendAtNS int64
		if err := rows.Scan(&summary.UserID, &summary.Username, &summary.BalanceNanoUSD, &summary.RechargeNanoUSD, &summary.RewardNanoUSD, &summary.SpendNanoUSD, &summary.UsageSpendNanoUSD, &summary.PlanSpendNanoUSD, &summary.RefundNanoUSD, &lastPaymentAtNS, &lastSpendAtNS, &summary.ClientCount, &summary.ClientSpendNanoUSD, &summary.ClientUsageNanoUSD, &summary.ClientPlanNanoUSD); err != nil {
			continue
		}
		summary.LastPaymentAt = timePtrFromNS(lastPaymentAtNS)
		summary.LastSpendAt = timePtrFromNS(lastSpendAtNS)
		out = append(out, summary)
	}
	return out, total
}

func (r *PostgresRepository) ListFinanceTopClientsBySpend(limit int) []FinanceClientSpendSummary {
	if limit <= 0 {
		limit = 5
	}
	if limit > 100 {
		limit = 100
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`
		WITH top_clients AS (
			SELECT client_id, client_name, owner_user_id, spend_limit_nano_usd,
				spend_used_nano_usd, usage_nano_usd, plan_nano_usd
			FROM finance_client_rollups
			WHERE client_id <> ''
			ORDER BY spend_used_nano_usd DESC, client_id ASC
			LIMIT ?
		)
		SELECT tc.client_id,
			COALESCE(NULLIF(TRIM(c.name), ''), NULLIF(TRIM(tc.client_name), ''), tc.client_id) AS client_name,
			COALESCE(NULLIF(TRIM(c.owner_user_id), ''), NULLIF(TRIM(tc.owner_user_id), ''), '') AS owner_user_id,
			COALESCE(cs.spend_limit_nano_usd, tc.spend_limit_nano_usd, 0) AS spend_limit_nano_usd,
			COALESCE(tc.spend_used_nano_usd, 0) AS spend_used_nano_usd,
			COALESCE(tc.usage_nano_usd, 0) AS usage_nano_usd,
			COALESCE(tc.plan_nano_usd, 0) AS plan_nano_usd
		FROM top_clients tc
		LEFT JOIN clients c ON c.id = tc.client_id
		LEFT JOIN client_spend cs ON cs.client_id = tc.client_id
		ORDER BY tc.spend_used_nano_usd DESC, tc.client_id ASC
	`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]FinanceClientSpendSummary, 0, limit)
	for rows.Next() {
		var summary FinanceClientSpendSummary
		if err := rows.Scan(&summary.ClientID, &summary.ClientName, &summary.OwnerUserID, &summary.SpendLimitNanoUSD, &summary.SpendUsedNanoUSD, &summary.UsageNanoUSD, &summary.PlanNanoUSD); err != nil {
			continue
		}
		out = append(out, summary)
	}
	return out
}

func (r *PostgresRepository) ListTokenUsageDailySummaries(startedAt time.Time, days int, limit int) []TokenUsageDailySummary {
	if days <= 0 || startedAt.IsZero() {
		return nil
	}
	if limit <= 0 || limit > days {
		limit = days
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	endedAt := startedAt.AddDate(0, 0, days)
	rows, err := r.db.Query(`
		SELECT date, request_count, prompt_tokens, cached_tokens, cache_creation_tokens, completion_tokens, image_output_tokens, total_tokens
		FROM finance_token_daily_rollups
		WHERE user_id = ''
			AND date >= ?
			AND date < ?
		ORDER BY date DESC
		LIMIT ?
	`, startedAt.Format("2006-01-02"), endedAt.Format("2006-01-02"), limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]TokenUsageDailySummary, 0, limit)
	for rows.Next() {
		var summary TokenUsageDailySummary
		if err := rows.Scan(&summary.Date, &summary.RequestCount, &summary.PromptTokens, &summary.CachedTokens, &summary.CacheCreationTokens, &summary.CompletionTokens, &summary.ImageOutputTokens, &summary.TotalTokens); err != nil {
			continue
		}
		out = append(out, summary)
	}
	return out
}

func (r *PostgresRepository) ListTokenUsageDailyUserSummaries(startedAt time.Time, days int, limit int) []TokenUsageDailySummary {
	if days <= 0 || startedAt.IsZero() {
		return nil
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 5000 {
		limit = 5000
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	endedAt := startedAt.AddDate(0, 0, days)
	rows, err := r.db.Query(`
		SELECT r.date,
			r.user_id,
			COALESCE(NULLIF(TRIM(u.username), ''), NULLIF(TRIM(r.username), ''), r.user_id) AS username,
			r.request_count,
			r.prompt_tokens,
			r.cached_tokens,
			r.cache_creation_tokens,
			r.completion_tokens,
			r.image_output_tokens,
			r.total_tokens
		FROM finance_token_daily_rollups r
		LEFT JOIN users u ON u.id = r.user_id
		WHERE r.user_id <> ''
			AND r.date >= ?
			AND r.date < ?
		ORDER BY r.date DESC, r.total_tokens DESC, username ASC, r.user_id ASC
		LIMIT ?
	`, startedAt.Format("2006-01-02"), endedAt.Format("2006-01-02"), limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]TokenUsageDailySummary, 0, limit)
	for rows.Next() {
		var summary TokenUsageDailySummary
		if err := rows.Scan(&summary.Date, &summary.UserID, &summary.Username, &summary.RequestCount, &summary.PromptTokens, &summary.CachedTokens, &summary.CacheCreationTokens, &summary.CompletionTokens, &summary.ImageOutputTokens, &summary.TotalTokens); err != nil {
			continue
		}
		out = append(out, summary)
	}
	return out
}
