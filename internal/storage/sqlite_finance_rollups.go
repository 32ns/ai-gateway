package storage

import (
	"database/sql"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func upsertFinanceUserRollupIdentityTx(tx *sql.Tx, user core.User) error {
	_, err := tx.Exec(`
		INSERT INTO finance_user_rollups(user_id, username, balance_nano_usd)
		VALUES(?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			username = excluded.username,
			balance_nano_usd = excluded.balance_nano_usd
	`, strings.TrimSpace(user.ID), strings.TrimSpace(user.Username), user.BalanceNanoUSD)
	return err
}

func setFinanceUserRollupBalanceTx(tx *sql.Tx, userID string, balanceNanoUSD int64) error {
	_, err := tx.Exec(`
		INSERT INTO finance_user_rollups(user_id, balance_nano_usd)
		VALUES(?, ?)
		ON CONFLICT(user_id) DO UPDATE SET balance_nano_usd = excluded.balance_nano_usd
	`, strings.TrimSpace(userID), balanceNanoUSD)
	return err
}

func markFinanceUserRollupDeletedTx(tx *sql.Tx, userID string) error {
	_, err := tx.Exec(`UPDATE finance_user_rollups SET username = '' WHERE user_id = ?`, strings.TrimSpace(userID))
	return err
}

func addFinanceUserRechargeRollupTx(tx *sql.Tx, userID string, amountNanoUSD int64, when time.Time) error {
	if amountNanoUSD == 0 {
		return nil
	}
	_, err := tx.Exec(`
		INSERT INTO finance_user_rollups(user_id, recharge_nano_usd, last_payment_at_ns)
		VALUES(?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			recharge_nano_usd = finance_user_rollups.recharge_nano_usd + excluded.recharge_nano_usd,
			last_payment_at_ns = MAX(finance_user_rollups.last_payment_at_ns, excluded.last_payment_at_ns)
	`, strings.TrimSpace(userID), amountNanoUSD, sqliteTimeNS(when))
	return err
}

func addFinanceUserRefundRollupTx(tx *sql.Tx, userID string, amountNanoUSD int64) error {
	if amountNanoUSD == 0 {
		return nil
	}
	_, err := tx.Exec(`
		INSERT INTO finance_user_rollups(user_id, refund_nano_usd)
		VALUES(?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			refund_nano_usd = finance_user_rollups.refund_nano_usd + excluded.refund_nano_usd
	`, strings.TrimSpace(userID), amountNanoUSD)
	return err
}

func addFinanceUserRewardRollupTx(tx *sql.Tx, userID string, amountNanoUSD int64) error {
	if amountNanoUSD == 0 {
		return nil
	}
	_, err := tx.Exec(`
		INSERT INTO finance_user_rollups(user_id, reward_nano_usd)
		VALUES(?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			reward_nano_usd = finance_user_rollups.reward_nano_usd + excluded.reward_nano_usd
	`, strings.TrimSpace(userID), amountNanoUSD)
	return err
}

func addFinanceUserSpendRollupTx(tx *sql.Tx, userID string, deltaNanoUSD int64, when time.Time) error {
	if deltaNanoUSD == 0 {
		return nil
	}
	_, err := tx.Exec(`
		INSERT INTO finance_user_rollups(user_id, spend_nano_usd, last_spend_at_ns)
		VALUES(?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			spend_nano_usd = CASE
				WHEN finance_user_rollups.spend_nano_usd + excluded.spend_nano_usd < 0 THEN 0
				ELSE finance_user_rollups.spend_nano_usd + excluded.spend_nano_usd
			END,
			last_spend_at_ns = MAX(finance_user_rollups.last_spend_at_ns, excluded.last_spend_at_ns)
	`, strings.TrimSpace(userID), deltaNanoUSD, sqliteTimeNS(when))
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE finance_user_rollups SET spend_nano_usd = 0 WHERE user_id = ? AND spend_nano_usd < 0`, strings.TrimSpace(userID))
	return err
}

func addFinanceUserUsageRollupTx(tx *sql.Tx, userID string, deltaNanoUSD int64, planDeltaNanoUSD int64, when time.Time) error {
	if deltaNanoUSD == 0 && planDeltaNanoUSD == 0 {
		return nil
	}
	_, err := tx.Exec(`
		INSERT INTO finance_user_rollups(user_id, usage_spend_nano_usd, plan_spend_nano_usd, last_spend_at_ns)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			usage_spend_nano_usd = CASE
				WHEN finance_user_rollups.usage_spend_nano_usd + excluded.usage_spend_nano_usd < 0 THEN 0
				ELSE finance_user_rollups.usage_spend_nano_usd + excluded.usage_spend_nano_usd
			END,
			plan_spend_nano_usd = CASE
				WHEN finance_user_rollups.plan_spend_nano_usd + excluded.plan_spend_nano_usd < 0 THEN 0
				ELSE finance_user_rollups.plan_spend_nano_usd + excluded.plan_spend_nano_usd
			END,
			last_spend_at_ns = MAX(finance_user_rollups.last_spend_at_ns, excluded.last_spend_at_ns)
	`, strings.TrimSpace(userID), deltaNanoUSD, planDeltaNanoUSD, sqliteTimeNS(when))
	if err != nil {
		return err
	}
	_, err = tx.Exec(`
		UPDATE finance_user_rollups
		SET usage_spend_nano_usd = CASE WHEN usage_spend_nano_usd < 0 THEN 0 ELSE usage_spend_nano_usd END,
			plan_spend_nano_usd = CASE WHEN plan_spend_nano_usd < 0 THEN 0 ELSE plan_spend_nano_usd END
		WHERE user_id = ?
	`, strings.TrimSpace(userID))
	return err
}

func upsertFinanceClientRollupMetadataTx(tx *sql.Tx, client core.APIClient) error {
	_, err := tx.Exec(`
		INSERT INTO finance_client_rollups(client_id, client_name, owner_user_id, spend_limit_nano_usd)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(client_id) DO UPDATE SET
			client_name = excluded.client_name,
			owner_user_id = excluded.owner_user_id,
			spend_limit_nano_usd = excluded.spend_limit_nano_usd
	`, strings.TrimSpace(client.ID), strings.TrimSpace(client.Name), strings.TrimSpace(client.OwnerUserID), client.SpendLimitNanoUSD)
	return err
}

func addFinanceClientSpendRollupTx(tx *sql.Tx, clientID, clientName, ownerUserID string, spendLimitNanoUSD, deltaNanoUSD int64) error {
	if strings.TrimSpace(clientID) == "" || deltaNanoUSD == 0 {
		return nil
	}
	_, err := tx.Exec(`
		INSERT INTO finance_client_rollups(client_id, client_name, owner_user_id, spend_limit_nano_usd, spend_used_nano_usd)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(client_id) DO UPDATE SET
			client_name = COALESCE(NULLIF(excluded.client_name, ''), finance_client_rollups.client_name),
			owner_user_id = COALESCE(NULLIF(finance_client_rollups.owner_user_id, ''), NULLIF(excluded.owner_user_id, ''), ''),
			spend_limit_nano_usd = excluded.spend_limit_nano_usd,
			spend_used_nano_usd = CASE
				WHEN finance_client_rollups.spend_used_nano_usd + excluded.spend_used_nano_usd < 0 THEN 0
				ELSE finance_client_rollups.spend_used_nano_usd + excluded.spend_used_nano_usd
			END
	`, strings.TrimSpace(clientID), strings.TrimSpace(clientName), strings.TrimSpace(ownerUserID), spendLimitNanoUSD, deltaNanoUSD)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE finance_client_rollups SET spend_used_nano_usd = 0 WHERE client_id = ? AND spend_used_nano_usd < 0`, strings.TrimSpace(clientID))
	return err
}

func addFinanceClientUsageRollupTx(tx *sql.Tx, clientID, clientName, ownerUserID string, spendLimitNanoUSD, deltaNanoUSD, planDeltaNanoUSD int64) error {
	if strings.TrimSpace(clientID) == "" || (deltaNanoUSD == 0 && planDeltaNanoUSD == 0) {
		return nil
	}
	_, err := tx.Exec(`
		INSERT INTO finance_client_rollups(client_id, client_name, owner_user_id, spend_limit_nano_usd, usage_nano_usd, plan_nano_usd)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(client_id) DO UPDATE SET
			client_name = COALESCE(NULLIF(excluded.client_name, ''), finance_client_rollups.client_name),
			owner_user_id = COALESCE(NULLIF(finance_client_rollups.owner_user_id, ''), NULLIF(excluded.owner_user_id, ''), ''),
			spend_limit_nano_usd = excluded.spend_limit_nano_usd,
			usage_nano_usd = CASE
				WHEN finance_client_rollups.usage_nano_usd + excluded.usage_nano_usd < 0 THEN 0
				ELSE finance_client_rollups.usage_nano_usd + excluded.usage_nano_usd
			END,
			plan_nano_usd = CASE
				WHEN finance_client_rollups.plan_nano_usd + excluded.plan_nano_usd < 0 THEN 0
				ELSE finance_client_rollups.plan_nano_usd + excluded.plan_nano_usd
			END
	`, strings.TrimSpace(clientID), strings.TrimSpace(clientName), strings.TrimSpace(ownerUserID), spendLimitNanoUSD, deltaNanoUSD, planDeltaNanoUSD)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`
		UPDATE finance_client_rollups
		SET usage_nano_usd = CASE WHEN usage_nano_usd < 0 THEN 0 ELSE usage_nano_usd END,
			plan_nano_usd = CASE WHEN plan_nano_usd < 0 THEN 0 ELSE plan_nano_usd END
		WHERE client_id = ?
	`, strings.TrimSpace(clientID))
	return err
}

func transferFinanceClientOwnerSpendRollupTx(tx *sql.Tx, clientID, previousOwnerID, nextOwnerID string, when time.Time) error {
	clientID = strings.TrimSpace(clientID)
	previousOwnerID = strings.TrimSpace(previousOwnerID)
	nextOwnerID = strings.TrimSpace(nextOwnerID)
	if clientID == "" || previousOwnerID == nextOwnerID {
		return nil
	}
	var spendNanoUSD int64
	var usageNanoUSD int64
	var planNanoUSD int64
	err := tx.QueryRow(`
		SELECT COALESCE(spend_used_nano_usd, 0), COALESCE(usage_nano_usd, 0), COALESCE(plan_nano_usd, 0)
		FROM finance_client_rollups
		WHERE client_id = ?
	`, clientID).Scan(&spendNanoUSD, &usageNanoUSD, &planNanoUSD)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if spendNanoUSD == 0 && usageNanoUSD == 0 && planNanoUSD == 0 {
		return nil
	}
	if previousOwnerID != "" {
		if err := addFinanceUserSpendRollupTx(tx, previousOwnerID, -spendNanoUSD, when); err != nil {
			return err
		}
		if err := addFinanceUserUsageRollupTx(tx, previousOwnerID, -usageNanoUSD, -planNanoUSD, when); err != nil {
			return err
		}
	}
	if nextOwnerID != "" {
		if err := addFinanceUserSpendRollupTx(tx, nextOwnerID, spendNanoUSD, when); err != nil {
			return err
		}
		if err := addFinanceUserUsageRollupTx(tx, nextOwnerID, usageNanoUSD, planNanoUSD, when); err != nil {
			return err
		}
	}
	return nil
}

func financeModelRollupName(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return "unknown"
	}
	return model
}

func addFinanceModelRollupTx(tx *sql.Tx, model string, requestDelta int, promptDelta, completionDelta, spendDelta int64) error {
	model = financeModelRollupName(model)
	if requestDelta == 0 && promptDelta == 0 && completionDelta == 0 && spendDelta == 0 {
		return nil
	}
	_, err := tx.Exec(`
		INSERT INTO finance_model_rollups(model, request_count, prompt_tokens, completion_tokens, spend_nano_usd)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(model) DO UPDATE SET
			request_count = finance_model_rollups.request_count + excluded.request_count,
			prompt_tokens = finance_model_rollups.prompt_tokens + excluded.prompt_tokens,
			completion_tokens = finance_model_rollups.completion_tokens + excluded.completion_tokens,
			spend_nano_usd = finance_model_rollups.spend_nano_usd + excluded.spend_nano_usd
	`, model, requestDelta, promptDelta, completionDelta, spendDelta)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`
		UPDATE finance_model_rollups
		SET request_count = CASE WHEN request_count < 0 THEN 0 ELSE request_count END,
			prompt_tokens = CASE WHEN prompt_tokens < 0 THEN 0 ELSE prompt_tokens END,
			completion_tokens = CASE WHEN completion_tokens < 0 THEN 0 ELSE completion_tokens END,
			spend_nano_usd = CASE WHEN spend_nano_usd < 0 THEN 0 ELSE spend_nano_usd END
		WHERE model = ?
	`, model)
	return err
}

func updateFinanceModelRollupForBillingChangeTx(tx *sql.Tx, before, after core.BillingReservation) error {
	beforeModel := financeModelRollupName(before.Model)
	afterModel := financeModelRollupName(after.Model)
	beforeSpend := billingModelUsageSpendAmount(before.Status, before.ReservedNanoUSD, before.ActualNanoUSD)
	afterSpend := billingModelUsageSpendAmount(after.Status, after.ReservedNanoUSD, after.ActualNanoUSD)
	if beforeModel == afterModel {
		return addFinanceModelRollupTx(
			tx,
			afterModel,
			0,
			int64(after.PromptTokens-before.PromptTokens),
			int64(after.CompletionTokens-before.CompletionTokens),
			afterSpend-beforeSpend,
		)
	}
	if err := addFinanceModelRollupTx(tx, beforeModel, -1, -int64(before.PromptTokens), -int64(before.CompletionTokens), -beforeSpend); err != nil {
		return err
	}
	return addFinanceModelRollupTx(tx, afterModel, 1, int64(after.PromptTokens), int64(after.CompletionTokens), afterSpend)
}

func addFinanceTokenDailyRollupTx(tx *sql.Tx, date, userID, username string, requestDelta int, promptDelta, cachedDelta, cacheCreationDelta, completionDelta, imageOutputDelta, totalDelta int64) error {
	date = strings.TrimSpace(date)
	userID = strings.TrimSpace(userID)
	username = strings.TrimSpace(username)
	if date == "" || (requestDelta == 0 && promptDelta == 0 && cachedDelta == 0 && cacheCreationDelta == 0 && completionDelta == 0 && imageOutputDelta == 0 && totalDelta == 0) {
		return nil
	}
	_, err := tx.Exec(`
		INSERT INTO finance_token_daily_rollups(date, user_id, username, request_count, prompt_tokens, cached_tokens, cache_creation_tokens, completion_tokens, image_output_tokens, total_tokens)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(date, user_id) DO UPDATE SET
			username = COALESCE(NULLIF(excluded.username, ''), finance_token_daily_rollups.username),
			request_count = finance_token_daily_rollups.request_count + excluded.request_count,
			prompt_tokens = finance_token_daily_rollups.prompt_tokens + excluded.prompt_tokens,
			cached_tokens = finance_token_daily_rollups.cached_tokens + excluded.cached_tokens,
			cache_creation_tokens = finance_token_daily_rollups.cache_creation_tokens + excluded.cache_creation_tokens,
			completion_tokens = finance_token_daily_rollups.completion_tokens + excluded.completion_tokens,
			image_output_tokens = finance_token_daily_rollups.image_output_tokens + excluded.image_output_tokens,
			total_tokens = finance_token_daily_rollups.total_tokens + excluded.total_tokens
	`, date, userID, username, requestDelta, promptDelta, cachedDelta, cacheCreationDelta, completionDelta, imageOutputDelta, totalDelta)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`
		UPDATE finance_token_daily_rollups
		SET request_count = CASE WHEN request_count < 0 THEN 0 ELSE request_count END,
			prompt_tokens = CASE WHEN prompt_tokens < 0 THEN 0 ELSE prompt_tokens END,
			cached_tokens = CASE WHEN cached_tokens < 0 THEN 0 ELSE cached_tokens END,
			cache_creation_tokens = CASE WHEN cache_creation_tokens < 0 THEN 0 ELSE cache_creation_tokens END,
			completion_tokens = CASE WHEN completion_tokens < 0 THEN 0 ELSE completion_tokens END,
			image_output_tokens = CASE WHEN image_output_tokens < 0 THEN 0 ELSE image_output_tokens END,
			total_tokens = CASE WHEN total_tokens < 0 THEN 0 ELSE total_tokens END
		WHERE date = ? AND user_id = ?
	`, date, userID)
	return err
}

func updateFinanceTokenDailyRollupForBillingChangeTx(tx *sql.Tx, before, after core.BillingReservation) error {
	if err := applyFinanceTokenDailyRollupRequestTx(tx, before, -1); err != nil {
		return err
	}
	return applyFinanceTokenDailyRollupRequestTx(tx, after, 1)
}

func applyFinanceTokenDailyRollupRequestTx(tx *sql.Tx, request core.BillingReservation, sign int) error {
	date := billingDayKey(request.CreatedAt)
	if date == "" || sign == 0 {
		return nil
	}
	prompt, cached, cacheCreation, completion, imageOutput, total := billingRequestTokenUsageAmount(request.Status, request.PromptTokens, request.CachedPromptTokens, request.CacheCreationTokens, request.CompletionTokens, request.ImageOutputTokens, request.TotalTokens)
	if prompt == 0 && cached == 0 && cacheCreation == 0 && completion == 0 && imageOutput == 0 && total == 0 {
		return nil
	}
	requestDelta := sign
	prompt *= int64(sign)
	cached *= int64(sign)
	cacheCreation *= int64(sign)
	completion *= int64(sign)
	imageOutput *= int64(sign)
	total *= int64(sign)
	if err := addFinanceTokenDailyRollupTx(tx, date, "", "", requestDelta, prompt, cached, cacheCreation, completion, imageOutput, total); err != nil {
		return err
	}
	return addFinanceTokenDailyRollupTx(tx, date, request.UserID, "", requestDelta, prompt, cached, cacheCreation, completion, imageOutput, total)
}

func (r *SQLiteRepository) rebuildFinanceTokenDailyRollups() error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM finance_token_daily_rollups`); err != nil {
		return err
	}
	rows, err := tx.Query(`
		SELECT br.user_id,
			COALESCE(NULLIF(TRIM(u.username), ''), br.user_id) AS username,
			br.status,
			br.prompt_tokens,
			br.cached_prompt_tokens,
			br.cache_creation_tokens,
			br.completion_tokens,
			br.image_output_tokens,
			br.total_tokens,
			br.created_at_ns
		FROM billing_requests br
		LEFT JOIN users u ON u.id = br.user_id
	`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var userID, username, status string
		var promptTokens, cachedTokens, cacheCreationTokens, completionTokens, imageOutputTokens, totalTokens int
		var createdAtNS int64
		if err := rows.Scan(&userID, &username, &status, &promptTokens, &cachedTokens, &cacheCreationTokens, &completionTokens, &imageOutputTokens, &totalTokens, &createdAtNS); err != nil {
			return err
		}
		request := core.BillingReservation{
			UserID:              userID,
			Status:              core.BillingRequestStatus(status),
			PromptTokens:        promptTokens,
			CachedPromptTokens:  cachedTokens,
			CacheCreationTokens: cacheCreationTokens,
			CompletionTokens:    completionTokens,
			ImageOutputTokens:   imageOutputTokens,
			TotalTokens:         totalTokens,
			CreatedAt:           timeFromNS(createdAtNS),
		}
		if err := applyFinanceTokenDailyRollupRequestTx(tx, request, 1); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tx.Commit()
}
