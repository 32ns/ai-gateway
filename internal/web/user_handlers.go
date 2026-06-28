package web

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
)

const (
	userSortHistoricalSpend = "spend"
	userSortBalance         = "balance"
	userSortCreatedAt       = "created_at"
	userSortUpdatedAt       = "updated_at"
	userSortLastLogin       = "last_login"
	userSortUsername        = "username"
	userSortRole            = "role"
	userSortStatus          = "status"
	userSortInviteCount     = "invite_count"

	userSortAsc  = "asc"
	userSortDesc = "desc"

	userPageSize = 15
)

type userPageFilter struct {
	Query     string
	Role      core.UserRole
	Status    string
	Inviter   string
	Sort      string
	Direction string
}

type userPageInfo struct {
	Page      int
	PageSize  int
	Total     int
	HasPrev   bool
	PrevPage  int
	HasNext   bool
	NextPage  int
	FirstItem int
	LastItem  int
}

func (s *Server) handleUsersPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/users" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	locale := resolveLocale(w, r)
	if deferredPartialRequested(r, "users-panel") {
		data := s.usersPageData(r, locale)
		s.renderFragment(w, "users.html", "users_panel", locale, data)
		return
	}
	data := s.usersPageData(r, locale)
	s.render(w, "users.html", locale, data)
}

func (s *Server) usersPageData(r *http.Request, locale string) map[string]any {
	filter := userPageFilterFromRequest(r)
	result, _ := s.control.ListUsersPage(controlplane.UserListFilter{
		Query:     filter.Query,
		Role:      filter.Role,
		Status:    filter.Status,
		Inviter:   filter.Inviter,
		Sort:      filter.Sort,
		Direction: filter.Direction,
		Page:      parsePositiveInt(r.URL.Query().Get("page"), 1),
		PageSize:  userPageSize,
	})
	return s.usersPageDataFromListResult(r, locale, filter, result)
}

func (s *Server) usersPageDataFromListResult(r *http.Request, locale string, filter userPageFilter, result controlplane.UserListResult) map[string]any {
	users := make([]core.User, 0, len(result.Items))
	userSpendNanoUSD := make(map[string]int64, len(result.Items))
	userInviteCounts := make(map[string]int64, len(result.Items))
	userIdentityByID := make(map[string]core.User, len(result.Items))
	for _, item := range result.Items {
		user := item.User
		userID := strings.TrimSpace(user.ID)
		if userID == "" {
			continue
		}
		users = append(users, user)
		userSpendNanoUSD[userID] = item.SpendNanoUSD
		userInviteCounts[userID] = item.InviteCount
		userIdentityByID[userID] = user
		if inviterID := strings.TrimSpace(user.InviterUserID); inviterID != "" {
			if _, exists := userIdentityByID[inviterID]; exists {
				continue
			}
			if inviter, err := s.control.GetUser(inviterID); err == nil {
				userIdentityByID[inviter.ID] = inviter
			}
		}
	}
	return withCSRFData(map[string]any{
		"TitleKey":          "page_title_users",
		"ActiveNav":         "users",
		"Locale":            locale,
		"Users":             users,
		"UserFilter":        filter,
		"UserPage":          userPageInfoFromControlplane(result.Page),
		"UserFilteredCount": result.FilteredCount,
		"UserTotalCount":    result.TotalUserCount,
		"UserSpendNanoUSD":  userSpendNanoUSD,
		"UserInviteCounts":  userInviteCounts,
		"UserIdentityByID":  userIdentityByID,
	}, r)
}

func userPageInfoFromControlplane(page controlplane.UserListPage) userPageInfo {
	return userPageInfo{
		Page:      page.Page,
		PageSize:  page.PageSize,
		Total:     page.Total,
		HasPrev:   page.HasPrev,
		PrevPage:  page.PrevPage,
		HasNext:   page.HasNext,
		NextPage:  page.NextPage,
		FirstItem: page.FirstItem,
		LastItem:  page.LastItem,
	}
}

func userPageFilterFromRequest(r *http.Request) userPageFilter {
	sortField, sortDirection := normalizeUserPageSort(r.URL.Query().Get("sort"), r.URL.Query().Get("direction"))
	return userPageFilter{
		Query:     strings.TrimSpace(r.URL.Query().Get("q")),
		Role:      core.UserRole(strings.TrimSpace(r.URL.Query().Get("role"))),
		Status:    strings.TrimSpace(r.URL.Query().Get("status")),
		Inviter:   strings.TrimSpace(r.URL.Query().Get("inviter")),
		Sort:      sortField,
		Direction: sortDirection,
	}
}

func normalizeUserPageSort(field, direction string) (string, string) {
	switch strings.TrimSpace(field) {
	case userSortHistoricalSpend, userSortBalance, userSortCreatedAt, userSortUpdatedAt, userSortLastLogin, userSortUsername, userSortRole, userSortStatus, userSortInviteCount:
		field = strings.TrimSpace(field)
	default:
		field = userSortCreatedAt
	}
	switch strings.TrimSpace(direction) {
	case userSortAsc:
		direction = userSortAsc
	default:
		direction = userSortDesc
	}
	return field, direction
}

func sortUsersForPage(users []core.User, filter userPageFilter, spendByUser map[string]int64, inviteCountByUser map[string]int64) []core.User {
	field, direction := normalizeUserPageSort(filter.Sort, filter.Direction)
	out := append([]core.User(nil), users...)
	slices.SortStableFunc(out, func(a, b core.User) int {
		cmp := compareUsersBySort(a, b, field, spendByUser, inviteCountByUser)
		if direction == userSortDesc {
			cmp = -cmp
		}
		if cmp != 0 {
			return cmp
		}
		return compareUsersByName(a, b)
	})
	return out
}

func paginateUsersForPage(users []core.User, page, pageSize int) ([]core.User, userPageInfo) {
	if pageSize <= 0 {
		pageSize = userPageSize
	}
	if page < 1 {
		page = 1
	}
	total := len(users)
	lastPage := 1
	if total > 0 {
		lastPage = (total + pageSize - 1) / pageSize
	}
	if page > lastPage {
		page = lastPage
	}
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	info := userPageInfo{
		Page:      page,
		PageSize:  pageSize,
		Total:     total,
		HasPrev:   page > 1,
		PrevPage:  page - 1,
		HasNext:   end < total,
		NextPage:  page + 1,
		FirstItem: start + 1,
		LastItem:  end,
	}
	if total == 0 {
		info.FirstItem = 0
	}
	return append([]core.User(nil), users[start:end]...), info
}

func compareUsersBySort(a, b core.User, field string, spendByUser map[string]int64, inviteCountByUser map[string]int64) int {
	switch field {
	case userSortBalance:
		return compareInt64Asc(a.BalanceNanoUSD, b.BalanceNanoUSD)
	case userSortCreatedAt:
		return compareTimeAsc(a.CreatedAt, b.CreatedAt)
	case userSortUpdatedAt:
		return compareTimeAsc(a.UpdatedAt, b.UpdatedAt)
	case userSortLastLogin:
		return compareTimePtrAsc(a.LastLoginAt, b.LastLoginAt)
	case userSortUsername:
		return compareUsersByName(a, b)
	case userSortRole:
		return compareStringAsc(string(a.Role), string(b.Role))
	case userSortStatus:
		return compareBoolAsc(a.Enabled, b.Enabled)
	case userSortInviteCount:
		return compareInt64Asc(inviteCountByUser[strings.TrimSpace(a.ID)], inviteCountByUser[strings.TrimSpace(b.ID)])
	default:
		return compareInt64Asc(spendByUser[strings.TrimSpace(a.ID)], spendByUser[strings.TrimSpace(b.ID)])
	}
}

func inviteCountsByUser(users []core.User) map[string]int64 {
	out := make(map[string]int64)
	for _, user := range users {
		inviterID := strings.TrimSpace(user.InviterUserID)
		if inviterID == "" {
			continue
		}
		out[inviterID]++
	}
	return out
}

func compareUsersByName(a, b core.User) int {
	if cmp := compareStringAsc(strings.ToLower(a.Username), strings.ToLower(b.Username)); cmp != 0 {
		return cmp
	}
	if cmp := compareStringAsc(a.Username, b.Username); cmp != 0 {
		return cmp
	}
	if cmp := compareTimeAsc(a.CreatedAt, b.CreatedAt); cmp != 0 {
		return cmp
	}
	return compareStringAsc(a.ID, b.ID)
}

func compareInt64Asc(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func compareTimeAsc(a, b time.Time) int {
	if a.Before(b) {
		return -1
	}
	if a.After(b) {
		return 1
	}
	return 0
}

func compareTimePtrAsc(a, b *time.Time) int {
	var left, right time.Time
	if a != nil {
		left = *a
	}
	if b != nil {
		right = *b
	}
	return compareTimeAsc(left, right)
}

func compareStringAsc(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func compareBoolAsc(a, b bool) int {
	if a == b {
		return 0
	}
	if !a {
		return -1
	}
	return 1
}

func (s *Server) handleUserActions(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/admin/users/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 1 && parts[0] == "new" {
		switch r.Method {
		case http.MethodGet:
			s.handleUserCreatePage(w, r)
		case http.MethodPost:
			s.handleUserCreateSubmit(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	switch parts[1] {
	case "details":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleUserDetailsFragment(w, r, parts[0])
	case "edit":
		switch r.Method {
		case http.MethodGet:
			s.handleUserEditPage(w, r, parts[0])
		case http.MethodPost:
			s.handleUserEditSubmit(w, r, parts[0])
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case "balance":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleUserBalanceSubmit(w, r, parts[0])
	case "delete":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			s.redirectWithNoticeError(w, r, "/admin/users", err)
			return
		}
		result, err := s.control.DeleteUserWithOptions(parts[0], controlplane.DeleteUserOptions{
			DeleteInvitedUsers: r.FormValue("delete_invited_users") != "",
		})
		if err != nil {
			s.recordAdminAudit(r, "error", "user.delete", "user", parts[0], "", err.Error())
			s.redirectWithNoticeError(w, r, "/admin/users", err)
			return
		}
		user := result.User
		detail := fmt.Sprintf("role=%s enabled=%t", user.Role, user.Enabled)
		if len(result.InvitedUsers) > 0 {
			detail += fmt.Sprintf(" deleted_invited_users=%d", len(result.InvitedUsers))
		}
		s.recordAdminAudit(r, "ok", "user.delete", "user", user.ID, user.Username, detail)
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleUserDetailsFragment(w http.ResponseWriter, r *http.Request, userID string) {
	user, err := s.control.GetUser(userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	locale := resolveLocale(w, r)
	data := map[string]any{
		"Locale":              locale,
		"User":                user,
		"Clients":             s.hydrateClientPage(s.control.ClientsForUser(user)),
		"UserRechargeNanoUSD": s.control.UserPaidRechargeTotal(user),
		"UserSpendNanoUSD":    s.control.UserBalanceSpendTotal(user),
		"PaymentOrders":       s.control.UserPaidPaymentOrders(user, 8),
		"ActivePlans":         activePlansForTemplate(s.control.ListUserPlanEntitlements(user.ID)),
		"Chart":               s.control.UsageCostChartForUser(r.Context(), user, time.Now()),
		"Ledger":              s.control.ListBillingLedger(user.ID, 8),
	}
	s.renderFragment(w, "users.html", "user_details_content", locale, data)
}

func (s *Server) handleUserCreatePage(w http.ResponseWriter, r *http.Request) {
	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":  "page_title_user_create",
		"ActiveNav": "users",
		"Locale":    locale,
		"Mode":      "create",
		"User": core.User{
			Enabled: true,
			Role:    core.UserRoleUser,
		},
		"Roles": userRoleOptions(),
	}, r)
	s.render(w, "user_edit.html", locale, data)
}

func (s *Server) handleUserCreateSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, "/admin/users", err)
		return
	}
	input, err := userInputFromForm(r)
	if err != nil {
		s.redirectWithNoticeError(w, r, "/admin/users", err)
		return
	}
	user, err := s.control.CreateUser(input)
	if err != nil {
		s.recordAdminAudit(r, "error", "user.create", "user", "", strings.TrimSpace(r.FormValue("username")), err.Error())
		s.redirectWithNoticeError(w, r, "/admin/users", err)
		return
	}
	detail := fmt.Sprintf("role=%s enabled=%t", user.Role, user.Enabled)
	if user.ConcurrentRequestLimitOverride != nil {
		detail += " user_concurrent_request_limit_override=" + auditOptionalIntValue(user.ConcurrentRequestLimitOverride)
	}
	if user.RequestRateLimitPerMinuteOverride != nil {
		detail += " user_request_rate_limit_override=" + auditOptionalIntValue(user.RequestRateLimitPerMinuteOverride)
	}
	s.recordAdminAudit(r, "ok", "user.create", "user", user.ID, user.Username, detail)
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

func (s *Server) handleUserEditPage(w http.ResponseWriter, r *http.Request, userID string) {
	user, err := s.control.GetUser(userID)
	if err != nil {
		s.redirectWithNoticeError(w, r, "/admin/users", err)
		return
	}
	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":         "page_title_user_edit",
		"ActiveNav":        "users",
		"Locale":           locale,
		"Mode":             "edit",
		"User":             user,
		"Roles":            userRoleOptions(),
		"InvitedUserCount": s.control.CountUsersByInviter(user.ID),
	}, r)
	s.render(w, "user_edit.html", locale, data)
}

func (s *Server) handleUserEditSubmit(w http.ResponseWriter, r *http.Request, userID string) {
	if err := r.ParseForm(); err != nil {
		s.renderUserEditFormError(w, r, userID, err)
		return
	}
	before, beforeErr := s.control.GetUser(userID)
	input, err := userInputFromForm(r)
	if err != nil {
		s.renderUserEditFormError(w, r, userID, err)
		return
	}
	user, err := s.control.UpdateUser(userID, input)
	if err != nil {
		s.recordAdminAudit(r, "error", "user.update", "user", userID, strings.TrimSpace(r.FormValue("username")), err.Error())
		s.renderUserEditFormError(w, r, userID, err)
		return
	}
	s.recordAdminAudit(r, "ok", "user.update", "user", user.ID, user.Username, auditUserUpdateChangeMessage(before, user, beforeErr == nil, strings.TrimSpace(r.FormValue("password")) != ""))
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

func (s *Server) renderUserEditFormError(w http.ResponseWriter, r *http.Request, userID string, err error) {
	s.redirectWithNoticeError(w, r, "/admin/users/"+url.PathEscape(userID)+"/edit", err)
}

func (s *Server) handleUserBalanceSubmit(w http.ResponseWriter, r *http.Request, userID string) {
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, "/admin/users", err)
		return
	}
	amount, err := parseNanoUSDFormValue(r, "amount_usd")
	if err != nil {
		s.redirectWithNoticeError(w, r, "/admin/users", err)
		return
	}
	if amount <= 0 {
		s.redirectWithNoticeError(w, r, "/admin/users", fmt.Errorf("amount must be greater than zero"))
		return
	}
	switch r.FormValue("direction") {
	case "credit":
	case "debit":
		amount = -amount
	default:
		s.redirectWithNoticeError(w, r, "/admin/users", fmt.Errorf("direction is required"))
		return
	}
	user, previousBalance, err := s.control.AdjustUserBalance(userID, controlplane.UserBalanceAdjustment{
		AmountNanoUSD: amount,
		Reason:        r.FormValue("reason"),
	})
	if err != nil {
		s.recordAdminAudit(r, "error", "user.balance", "user", userID, "", err.Error())
		s.redirectWithNoticeError(w, r, "/admin/users", err)
		return
	}
	s.recordAdminAudit(r, "ok", "user.balance", "user", user.ID, user.Username, fmt.Sprintf("previous=%s current=%s delta=%s reason=%s", core.FormatNanoUSD(previousBalance), core.FormatNanoUSD(user.BalanceNanoUSD), core.FormatNanoUSD(amount), strings.TrimSpace(r.FormValue("reason"))))
	s.publishBalanceUpdated(user.ID)
	s.publishFinanceChanged("balance_adjustment", user.ID)
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}
