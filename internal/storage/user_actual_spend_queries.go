package storage

import (
	"strings"
)

func (r *SQLiteRepository) UserActualSpendTotal(userID string) int64 {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	var total int64
	if err := r.db.QueryRow(`SELECT COALESCE(spend_nano_usd, 0) FROM finance_user_rollups WHERE user_id = ?`, userID).Scan(&total); err != nil {
		return 0
	}
	return total
}

func (r *MemoryRepository) UserActualSpendTotal(userID string) int64 {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return 0
	}
	return memoryUserActualSpendTotals(r)[userID]
}
