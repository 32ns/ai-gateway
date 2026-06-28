package postgresrepo

import (
	"encoding/json"
	"fmt"
	"strings"

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
		parts = append(parts, `u.enabled`)
	case "disabled":
		parts = append(parts, `NOT u.enabled`)
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

const postgresPageUserRollupCTETemplate = `
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

const postgresPageUserRollupWithInviteSortCTETemplate = `
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

func (r *PostgresRepository) ListUsersPage(query UserListQuery) ([]UserListItem, int, int) {
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

func (r *PostgresRepository) listUsersPageByHotColumnsLocked(query UserListQuery, whereSQL string, whereArgs []any, totalFiltered, totalAll int) ([]UserListItem, int, int) {
	orderSQL := userListOrderSQL(query)
	args := append([]any(nil), whereArgs...)
	args = append(args, query.Limit, query.Offset)
	template := postgresPageUserRollupCTETemplate
	if query.Sort == userListSortInviteCount {
		template = postgresPageUserRollupWithInviteSortCTETemplate
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
