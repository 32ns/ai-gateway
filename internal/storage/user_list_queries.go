package storage

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

const (
	userListSortSpend       = "spend"
	userListSortBalance     = "balance"
	userListSortCreatedAt   = "created_at"
	userListSortUpdatedAt   = "updated_at"
	userListSortLastLogin   = "last_login"
	userListSortUsername    = "username"
	userListSortRole        = "role"
	userListSortStatus      = "status"
	userListSortInviteCount = "invite_count"
	userListSortAsc         = "asc"
	userListSortDesc        = "desc"
)

func normalizeUserListQuery(query UserListQuery) UserListQuery {
	query.Query = strings.TrimSpace(query.Query)
	query.Role = core.UserRole(strings.TrimSpace(string(query.Role)))
	query.Status = strings.TrimSpace(query.Status)
	query.Inviter = strings.TrimSpace(query.Inviter)
	switch strings.TrimSpace(query.Sort) {
	case userListSortSpend, userListSortBalance, userListSortCreatedAt, userListSortUpdatedAt, userListSortLastLogin, userListSortUsername, userListSortRole, userListSortStatus, userListSortInviteCount:
		query.Sort = strings.TrimSpace(query.Sort)
	default:
		query.Sort = userListSortCreatedAt
	}
	switch strings.TrimSpace(query.Direction) {
	case userListSortAsc:
		query.Direction = userListSortAsc
	default:
		query.Direction = userListSortDesc
	}
	if query.Status != "enabled" && query.Status != "disabled" {
		query.Status = ""
	}
	if query.Offset < 0 {
		query.Offset = 0
	}
	if query.Limit <= 0 {
		query.Limit = 25
	}
	if query.Limit > 100 {
		query.Limit = 100
	}
	return query
}

func userListWhereSQL(query UserListQuery) (string, []any) {
	parts := []string{"1 = 1"}
	args := []any{}
	if query.Query != "" {
		pattern := billingSQLLikePattern(query.Query)
		parts = append(parts, `(LOWER(u.id) LIKE ? ESCAPE '\' OR LOWER(u.username) LIKE ? ESCAPE '\' OR LOWER(u.inviter_user_id) LIKE ? ESCAPE '\' OR LOWER(inv.username) LIKE ? ESCAPE '\')`)
		args = append(args, pattern, pattern, pattern, pattern)
	}
	if query.Role != "" {
		parts = append(parts, `u.role = ?`)
		args = append(args, string(query.Role))
	}
	switch query.Status {
	case "enabled":
		parts = append(parts, `u.enabled <> 0`)
	case "disabled":
		parts = append(parts, `u.enabled = 0`)
	}
	if query.Inviter != "" {
		pattern := billingSQLLikePattern(query.Inviter)
		parts = append(parts, `(LOWER(u.inviter_user_id) LIKE ? ESCAPE '\' OR LOWER(inv.username) LIKE ? ESCAPE '\')`)
		args = append(args, pattern, pattern)
	}
	return strings.Join(parts, " AND "), args
}

func userListHasFilters(query UserListQuery) bool {
	return strings.TrimSpace(query.Query) != "" ||
		strings.TrimSpace(string(query.Role)) != "" ||
		strings.TrimSpace(query.Status) != "" ||
		strings.TrimSpace(query.Inviter) != ""
}

func userListOrderSQL(query UserListQuery) string {
	direction := "DESC"
	if query.Direction == userListSortAsc {
		direction = "ASC"
	}
	expression := "u.created_at_ns"
	switch query.Sort {
	case userListSortSpend:
		expression = "COALESCE(f.spend_nano_usd, 0)"
	case userListSortBalance:
		expression = "COALESCE(b.balance_nano_usd, 0)"
	case userListSortUpdatedAt:
		expression = "u.updated_at_ns"
	case userListSortLastLogin:
		expression = "u.last_login_at_ns"
	case userListSortUsername:
		expression = "LOWER(u.username)"
	case userListSortRole:
		expression = "u.role"
	case userListSortStatus:
		expression = "u.enabled"
	case userListSortInviteCount:
		expression = "COALESCE(ic.invite_count, 0)"
	}
	return fmt.Sprintf("%s %s, LOWER(u.username) ASC, u.username ASC, u.id ASC", expression, direction)
}

const sqlitePageUserRollupCTETemplate = `
	WITH page_users AS (
		SELECT u.id, u.payload, COALESCE(b.balance_nano_usd, 0) AS balance_nano_usd,
			COALESCE(f.spend_nano_usd, 0) AS spend_nano_usd,
			ROW_NUMBER() OVER (ORDER BY %s) AS page_order
		FROM users u
		LEFT JOIN user_balances b ON b.user_id = u.id
		LEFT JOIN finance_user_rollups f ON f.user_id = u.id
		LEFT JOIN users inv ON inv.id = u.inviter_user_id
		WHERE %s
		ORDER BY %s
		LIMIT ? OFFSET ?
	),
	page_invite_counts AS (
		SELECT pu.id AS user_id, COUNT(invited.id) AS invite_count
		FROM page_users pu
		LEFT JOIN users invited ON invited.inviter_user_id = pu.id
		GROUP BY pu.id
	)
	SELECT pu.payload, pu.balance_nano_usd, COALESCE(pic.invite_count, 0), pu.spend_nano_usd
	FROM page_users pu
	LEFT JOIN page_invite_counts pic ON pic.user_id = pu.id
	ORDER BY pu.page_order ASC
`

const sqlitePageUserRollupWithInviteSortCTETemplate = `
	WITH invite_counts AS (
		SELECT inviter_user_id AS user_id, COUNT(*) AS invite_count
		FROM users
		WHERE inviter_user_id <> ''
		GROUP BY inviter_user_id
	),
	page_users AS (
		SELECT u.id, u.payload, COALESCE(b.balance_nano_usd, 0) AS balance_nano_usd, COALESCE(f.spend_nano_usd, 0) AS spend_nano_usd,
			COALESCE(ic.invite_count, 0) AS invite_count,
			ROW_NUMBER() OVER (ORDER BY %s) AS page_order
		FROM users u
		LEFT JOIN user_balances b ON b.user_id = u.id
		LEFT JOIN finance_user_rollups f ON f.user_id = u.id
		LEFT JOIN invite_counts ic ON ic.user_id = u.id
		LEFT JOIN users inv ON inv.id = u.inviter_user_id
		WHERE %s
		ORDER BY %s
		LIMIT ? OFFSET ?
	)
	SELECT pu.payload, pu.balance_nano_usd, pu.invite_count, pu.spend_nano_usd
	FROM page_users pu
	ORDER BY pu.page_order ASC
`

func (r *SQLiteRepository) ListUsersPage(query UserListQuery) ([]UserListItem, int, int) {
	query = normalizeUserListQuery(query)
	r.mu.RLock()
	defer r.mu.RUnlock()

	totalAll := 0
	_ = r.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&totalAll)

	whereSQL, whereArgs := userListWhereSQL(query)
	totalFiltered := totalAll
	if userListHasFilters(query) {
		countSQL := `
			SELECT COUNT(*)
			FROM users u
			LEFT JOIN users inv ON inv.id = u.inviter_user_id
			WHERE ` + whereSQL
		if err := r.db.QueryRow(countSQL, whereArgs...).Scan(&totalFiltered); err != nil {
			return nil, 0, totalAll
		}
	}
	return r.listUsersPageByHotColumnsLocked(query, whereSQL, whereArgs, totalFiltered, totalAll)
}

func (r *SQLiteRepository) listUsersPageByHotColumnsLocked(query UserListQuery, whereSQL string, whereArgs []any, totalFiltered, totalAll int) ([]UserListItem, int, int) {
	orderSQL := userListOrderSQL(query)
	args := append([]any(nil), whereArgs...)
	args = append(args, query.Limit, query.Offset)
	template := sqlitePageUserRollupCTETemplate
	if query.Sort == userListSortInviteCount {
		template = sqlitePageUserRollupWithInviteSortCTETemplate
	}
	rows, err := r.db.Query(fmt.Sprintf(template, orderSQL, whereSQL, orderSQL), args...)
	if err != nil {
		return nil, totalFiltered, totalAll
	}
	defer rows.Close()

	items := make([]UserListItem, 0, query.Limit)
	for rows.Next() {
		var payload string
		var balance, inviteCount, spend int64
		if err := rows.Scan(&payload, &balance, &inviteCount, &spend); err != nil {
			continue
		}
		var user core.User
		if err := json.Unmarshal([]byte(payload), &user); err != nil {
			continue
		}
		user.BalanceNanoUSD = balance
		items = append(items, UserListItem{
			User:         cloneUser(user),
			InviteCount:  inviteCount,
			SpendNanoUSD: spend,
		})
	}
	return items, totalFiltered, totalAll
}

func (r *MemoryRepository) ListUsersPage(query UserListQuery) ([]UserListItem, int, int) {
	query = normalizeUserListQuery(query)
	users := r.ListUsers()
	spendByUser := memoryUserActualSpendTotals(r)
	return listUsersPageFromUsers(users, query, spendByUser)
}

func listUsersPageFromUsers(users []core.User, query UserListQuery, spendByUser map[string]int64) ([]UserListItem, int, int) {
	query = normalizeUserListQuery(query)
	totalAll := len(users)
	inviteCounts := memoryInviteCountsByUser(users)
	if spendByUser == nil {
		spendByUser = map[string]int64{}
	}
	filtered := memoryFilterUsers(users, query)
	memorySortUsers(filtered, query, spendByUser, inviteCounts)
	totalFiltered := len(filtered)
	if query.Offset >= totalFiltered {
		return nil, totalFiltered, totalAll
	}
	end := query.Offset + query.Limit
	if end > totalFiltered {
		end = totalFiltered
	}
	items := make([]UserListItem, 0, end-query.Offset)
	for _, user := range filtered[query.Offset:end] {
		userID := strings.TrimSpace(user.ID)
		items = append(items, UserListItem{
			User:         cloneUser(user),
			InviteCount:  inviteCounts[userID],
			SpendNanoUSD: spendByUser[userID],
		})
	}
	return items, totalFiltered, totalAll
}

func memoryFilterUsers(users []core.User, query UserListQuery) []core.User {
	userLookup := make(map[string]core.User, len(users))
	for _, user := range users {
		userLookup[strings.TrimSpace(user.ID)] = user
	}
	out := make([]core.User, 0, len(users))
	for _, user := range users {
		if query.Role != "" && user.Role != query.Role {
			continue
		}
		if query.Status == "enabled" && !user.Enabled {
			continue
		}
		if query.Status == "disabled" && user.Enabled {
			continue
		}
		if query.Inviter != "" && !memoryUserInviterMatches(user, userLookup, query.Inviter) {
			continue
		}
		if query.Query != "" && !memoryUserMatchesQuery(user, userLookup, query.Query) {
			continue
		}
		out = append(out, user)
	}
	return out
}

func memorySortUsers(users []core.User, query UserListQuery, spendByUser map[string]int64, inviteCounts map[string]int64) {
	slices.SortStableFunc(users, func(a, b core.User) int {
		cmp := memoryCompareUsersBySort(a, b, query.Sort, spendByUser, inviteCounts)
		if query.Direction == userListSortDesc {
			cmp = -cmp
		}
		if cmp != 0 {
			return cmp
		}
		return memoryCompareUsersByName(a, b)
	})
}

func memoryCompareUsersBySort(a, b core.User, sort string, spendByUser map[string]int64, inviteCounts map[string]int64) int {
	switch sort {
	case userListSortBalance:
		return memoryCompareInt64Asc(a.BalanceNanoUSD, b.BalanceNanoUSD)
	case userListSortCreatedAt:
		return memoryCompareTimeAsc(a.CreatedAt, b.CreatedAt)
	case userListSortUpdatedAt:
		return memoryCompareTimeAsc(a.UpdatedAt, b.UpdatedAt)
	case userListSortLastLogin:
		return memoryCompareTimePtrAsc(a.LastLoginAt, b.LastLoginAt)
	case userListSortUsername:
		return memoryCompareUsersByName(a, b)
	case userListSortRole:
		return strings.Compare(string(a.Role), string(b.Role))
	case userListSortStatus:
		if a.Enabled == b.Enabled {
			return 0
		}
		if !a.Enabled {
			return -1
		}
		return 1
	case userListSortInviteCount:
		return memoryCompareInt64Asc(inviteCounts[strings.TrimSpace(a.ID)], inviteCounts[strings.TrimSpace(b.ID)])
	default:
		return memoryCompareInt64Asc(spendByUser[strings.TrimSpace(a.ID)], spendByUser[strings.TrimSpace(b.ID)])
	}
}

func memoryInviteCountsByUser(users []core.User) map[string]int64 {
	out := make(map[string]int64)
	for _, user := range users {
		inviterID := strings.TrimSpace(user.InviterUserID)
		if inviterID != "" {
			out[inviterID]++
		}
	}
	return out
}

func memoryUserActualSpendTotals(r *MemoryRepository) map[string]int64 {
	totals := make(map[string]int64)
	if r == nil {
		return totals
	}
	for _, summary := range r.ListBillingLedgerUserSummaries() {
		userID := strings.TrimSpace(summary.UserID)
		if userID == "" || summary.SpendNanoUSD <= 0 {
			continue
		}
		totals[userID] = addNanoUSDSaturating(totals[userID], summary.SpendNanoUSD)
	}
	for userID, spend := range memoryBillingLedgerRequestSpendTotals(r) {
		if userID == "" || spend <= 0 {
			continue
		}
		totals[userID] = addNanoUSDSaturating(totals[userID], spend)
	}
	return totals
}

func memoryBillingLedgerRequestSpendTotals(r *MemoryRepository) map[string]int64 {
	totals, _ := memoryBillingRequestCashSpendTotals(r)
	return totals
}

func memoryBillingRequestCashSpendTotals(r *MemoryRepository) (map[string]int64, map[string]int64) {
	byUser := make(map[string]int64)
	byClient := make(map[string]int64)
	if r == nil {
		return byUser, byClient
	}
	requestKeys := make(map[string]struct{})
	for _, request := range r.ListBillingRequests(BillingRequestQuery{}) {
		clientID := strings.TrimSpace(request.ClientID)
		requestKeys[billingRequestKey(request.RequestID, clientID)] = struct{}{}
		if core.NormalizeClientBillingSource(request.BillingSource) == core.ClientBillingSourcePlan {
			continue
		}
		spend := billingRequestUsageSpendAmount(request.Status, request.ReservedNanoUSD, request.ActualNanoUSD)
		if spend <= 0 {
			continue
		}
		userID := strings.TrimSpace(request.UserID)
		if userID == "" {
			continue
		}
		byUser[userID] = addNanoUSDSaturating(byUser[userID], spend)
		if clientID != "" {
			byClient[clientID] = addNanoUSDSaturating(byClient[clientID], spend)
		}
	}
	for _, entry := range r.ListBillingLedger("", 0) {
		if entry.RequestID == "" || entry.ClientID == "" {
			continue
		}
		if _, ok := requestKeys[billingRequestKey(entry.RequestID, entry.ClientID)]; ok {
			continue
		}
		spend := billingLedgerUsageSpendDelta(entry.Kind, entry.AmountNanoUSD)
		if spend == 0 {
			continue
		}
		userID := strings.TrimSpace(entry.UserID)
		if userID == "" {
			continue
		}
		clientID := strings.TrimSpace(entry.ClientID)
		byUser[userID] = addNanoUSDSaturating(byUser[userID], spend)
		if byUser[userID] < 0 {
			byUser[userID] = 0
		}
		if clientID != "" {
			byClient[clientID] = addNanoUSDSaturating(byClient[clientID], spend)
			if byClient[clientID] < 0 {
				byClient[clientID] = 0
			}
		}
	}
	return byUser, byClient
}

func memoryUserMatchesQuery(user core.User, users map[string]core.User, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	fields := []string{user.ID, user.Username}
	fields = append(fields, memoryUserInviterSearchFields(user, users)...)
	return memoryFieldsContainQuery(fields, query)
}

func memoryUserInviterMatches(user core.User, users map[string]core.User, query string) bool {
	return memoryFieldsContainQuery(memoryUserInviterSearchFields(user, users), strings.ToLower(strings.TrimSpace(query)))
}

func memoryUserInviterSearchFields(user core.User, users map[string]core.User) []string {
	inviterID := strings.TrimSpace(user.InviterUserID)
	if inviterID == "" {
		return nil
	}
	fields := []string{inviterID}
	if inviter, ok := users[inviterID]; ok {
		fields = append(fields, inviter.Username)
	}
	return fields
}

func memoryFieldsContainQuery(fields []string, query string) bool {
	for _, field := range fields {
		if strings.Contains(strings.ToLower(strings.TrimSpace(field)), query) {
			return true
		}
	}
	return false
}

func memoryCompareUsersByName(a, b core.User) int {
	if cmp := strings.Compare(strings.ToLower(a.Username), strings.ToLower(b.Username)); cmp != 0 {
		return cmp
	}
	if cmp := strings.Compare(a.Username, b.Username); cmp != 0 {
		return cmp
	}
	if cmp := memoryCompareTimeAsc(a.CreatedAt, b.CreatedAt); cmp != 0 {
		return cmp
	}
	return strings.Compare(a.ID, b.ID)
}

func memoryCompareInt64Asc(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func memoryCompareTimeAsc(a, b time.Time) int {
	if a.Before(b) {
		return -1
	}
	if a.After(b) {
		return 1
	}
	return 0
}

func memoryCompareTimePtrAsc(a, b *time.Time) int {
	var left, right time.Time
	if a != nil {
		left = *a
	}
	if b != nil {
		right = *b
	}
	return memoryCompareTimeAsc(left, right)
}
