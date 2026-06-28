package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestUserPageFilterDefaultsToCreatedAtDescending(t *testing.T) {
	req := httptest.NewRequest("GET", "/admin/users", nil)

	filter := userPageFilterFromRequest(req)

	if filter.Sort != userSortCreatedAt || filter.Direction != userSortDesc {
		t.Fatalf("sort = %q %q, want %q %q", filter.Sort, filter.Direction, userSortCreatedAt, userSortDesc)
	}
}

func TestUserPageFilterAcceptsInvitationCountSort(t *testing.T) {
	req := httptest.NewRequest("GET", "/admin/users?sort=invite_count&direction=asc", nil)

	filter := userPageFilterFromRequest(req)

	if filter.Sort != userSortInviteCount || filter.Direction != userSortAsc {
		t.Fatalf("sort = %q %q, want %q %q", filter.Sort, filter.Direction, userSortInviteCount, userSortAsc)
	}
}

func TestSortUsersForPageHistoricalSpendDescending(t *testing.T) {
	users := []core.User{
		{ID: "user_low", Username: "low"},
		{ID: "user_high", Username: "high"},
		{ID: "user_mid", Username: "mid"},
	}
	spendByUser := map[string]int64{
		"user_low":  10,
		"user_high": 30,
		"user_mid":  20,
	}

	got := userIDs(sortUsersForPage(users, userPageFilter{Sort: userSortHistoricalSpend, Direction: userSortDesc}, spendByUser, nil))

	want := []string{"user_high", "user_mid", "user_low"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %#v, want %#v", got, want)
	}
}

func TestSortUsersForPageBalanceAscending(t *testing.T) {
	users := []core.User{
		{ID: "user_high", Username: "high", BalanceNanoUSD: 300},
		{ID: "user_low", Username: "low", BalanceNanoUSD: 100},
		{ID: "user_mid", Username: "mid", BalanceNanoUSD: 200},
	}

	got := userIDs(sortUsersForPage(users, userPageFilter{Sort: userSortBalance, Direction: userSortAsc}, nil, nil))

	want := []string{"user_low", "user_mid", "user_high"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %#v, want %#v", got, want)
	}
}

func TestSortUsersForPageCreatedAtDescending(t *testing.T) {
	base := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	users := []core.User{
		{ID: "user_old", Username: "old", CreatedAt: base.Add(-2 * time.Hour)},
		{ID: "user_new", Username: "new", CreatedAt: base},
		{ID: "user_mid", Username: "mid", CreatedAt: base.Add(-time.Hour)},
	}

	got := userIDs(sortUsersForPage(users, userPageFilter{Sort: userSortCreatedAt, Direction: userSortDesc}, nil, nil))

	want := []string{"user_new", "user_mid", "user_old"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %#v, want %#v", got, want)
	}
}

func TestSortUsersForPageInvitationCountDescending(t *testing.T) {
	users := []core.User{
		{ID: "user_none", Username: "none"},
		{ID: "user_many", Username: "many"},
		{ID: "user_one", Username: "one"},
	}
	allUsers := append([]core.User(nil), users...)
	allUsers = append(allUsers,
		core.User{ID: "user_invited_a", Username: "invited-a", InviterUserID: "user_many"},
		core.User{ID: "user_invited_b", Username: "invited-b", InviterUserID: "user_many"},
		core.User{ID: "user_invited_c", Username: "invited-c", InviterUserID: "user_one"},
	)

	got := userIDs(sortUsersForPage(users, userPageFilter{Sort: userSortInviteCount, Direction: userSortDesc}, nil, inviteCountsByUser(allUsers)))

	want := []string{"user_many", "user_one", "user_none"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %#v, want %#v", got, want)
	}
}

func TestPaginateUsersForPageClampsAndReportsBounds(t *testing.T) {
	users := []core.User{
		{ID: "user_1", Username: "one"},
		{ID: "user_2", Username: "two"},
		{ID: "user_3", Username: "three"},
	}

	pageUsers, page := paginateUsersForPage(users, 2, 2)

	if got := userIDs(pageUsers); !reflect.DeepEqual(got, []string{"user_3"}) {
		t.Fatalf("page users = %#v, want user_3", got)
	}
	if page.Page != 2 || page.PageSize != 2 || page.Total != 3 || !page.HasPrev || page.PrevPage != 1 || page.HasNext {
		t.Fatalf("page info = %#v, want second final page", page)
	}
	if page.FirstItem != 3 || page.LastItem != 3 {
		t.Fatalf("page item bounds = %d..%d, want 3..3", page.FirstItem, page.LastItem)
	}
}

func TestUsersPagePaginatesUserList(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	for i := range 30 {
		if _, err := control.CreateUser(controlplane.UserInput{
			Username: fmt.Sprintf("paged-user-%02d", i),
			Password: "paged-secret",
			Role:     core.UserRoleUser,
			Enabled:  true,
		}); err != nil {
			t.Fatalf("CreateUser(%d) returned error: %v", i, err)
		}
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/admin/users?page=2&sort=username&direction=asc&partial=users-panel", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "paged-user-14") || !strings.Contains(body, "paged-user-28") {
		t.Fatalf("second page missing expected users: %s", body)
	}
	if strings.Contains(body, "paged-user-00") {
		t.Fatalf("second page should not render first page users: %s", body)
	}
	if !strings.Contains(body, `/admin/users?sort=username&amp;direction=asc&amp;page=1`) {
		t.Fatalf("previous page link missing preserved sort parameters: %s", body)
	}
}

func TestUsersPageDataUsesBackendPagination(t *testing.T) {
	base := storage.NewMemoryRepository()
	now := time.Now().UTC()
	for i := range 3 {
		if err := base.UpsertUser(core.User{
			ID:        fmt.Sprintf("user_backend_page_%d", i),
			Username:  fmt.Sprintf("backend-page-%d", i),
			Role:      core.UserRoleUser,
			Enabled:   true,
			CreatedAt: now.Add(time.Duration(i) * time.Minute),
			UpdatedAt: now.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("UpsertUser(%d) returned error: %v", i, err)
		}
	}
	repo := &userPageFullListPanicRepository{MemoryRepository: base}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	req := httptest.NewRequest(http.MethodGet, "/admin/users?page=1", nil)

	data := server.usersPageData(req, "en")

	users, ok := data["Users"].([]core.User)
	if !ok || len(users) != 3 {
		t.Fatalf("Users = %#v, want three backend-paginated users", data["Users"])
	}
	if data["UserTotalCount"] != 3 || data["UserFilteredCount"] != 3 {
		t.Fatalf("counts total=%#v filtered=%#v, want 3/3", data["UserTotalCount"], data["UserFilteredCount"])
	}
}

type userPageFullListPanicRepository struct {
	*storage.MemoryRepository
}

func (r *userPageFullListPanicRepository) ListUsers() []core.User {
	panic("users page should use backend pagination instead of full ListUsers")
}

func TestUserEditPageCountsInvitesWithoutFullList(t *testing.T) {
	base := storage.NewMemoryRepository()
	for _, user := range []core.User{
		{ID: "admin", Username: "admin", Role: core.UserRoleAdmin, Enabled: true},
		{ID: "user_edit_inviter", Username: "edit-inviter", Role: core.UserRoleUser, Enabled: true},
		{ID: "user_edit_invited", Username: "edit-invited", Role: core.UserRoleUser, Enabled: true, InviterUserID: "user_edit_inviter"},
	} {
		if err := base.UpsertUser(user); err != nil {
			t.Fatalf("UpsertUser(%s) returned error: %v", user.ID, err)
		}
	}
	repo := &userPageFullListPanicRepository{MemoryRepository: base}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	sessionToken, _, err := control.CreateUserSession("admin")
	if err != nil {
		t.Fatalf("CreateUserSession returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	req := httptest.NewRequest(http.MethodGet, "/admin/users/user_edit_inviter/edit", nil)
	req.AddCookie(&http.Cookie{Name: consoleSessionCookieName, Value: sessionToken})
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestUsersPageKeepsHistoricalSpendAfterClientDelete(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user, err := control.CreateUser(controlplane.UserInput{
		Username:       "spender",
		Password:       "spender-secret",
		Role:           core.UserRoleUser,
		Enabled:        true,
		BalanceNanoUSD: 10 * core.NanoUSDPerUSD,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	const clientID = "client_deleted_history"
	if err := repo.UpsertClient(core.APIClient{ID: clientID, Name: "Deleted History", APIKey: "gw_deleted_history", OwnerUserID: user.ID, Enabled: true}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	actualSpend := int64(123) * core.NanoUSDPerUSD / 100
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_deleted_history",
		ClientID:        clientID,
		UserID:          user.ID,
		ClientName:      "Deleted History",
		Model:           "gpt-4.1",
		ReservedNanoUSD: actualSpend,
		Fingerprint:     "deleted-history",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_deleted_history",
		ClientID:      clientID,
		Model:         "gpt-4.1",
		ActualNanoUSD: actualSpend,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	if err := repo.DeleteClient(clientID); err != nil {
		t.Fatalf("DeleteClient returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/admin/users?partial=users-panel", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "$1.23") {
		t.Fatalf("response missing deleted client historical spend: %s", body)
	}
}

func TestDashboardKeepsHistoricalSpendAfterClientDelete(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user, err := control.CreateUser(controlplane.UserInput{
		Username:       "dashboard-spender",
		Password:       "spender-secret",
		Role:           core.UserRoleUser,
		Enabled:        true,
		BalanceNanoUSD: 10 * core.NanoUSDPerUSD,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	const clientID = "client_dashboard_deleted_history"
	if err := repo.UpsertClient(core.APIClient{ID: clientID, Name: "Dashboard Deleted History", APIKey: "gw_dashboard_deleted_history", OwnerUserID: user.ID, Enabled: true}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	actualSpend := int64(123) * core.NanoUSDPerUSD / 100
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_dashboard_deleted_history",
		ClientID:        clientID,
		UserID:          user.ID,
		ClientName:      "Dashboard Deleted History",
		Model:           "gpt-4.1",
		ReservedNanoUSD: actualSpend,
		Fingerprint:     "dashboard-deleted-history",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_dashboard_deleted_history",
		ClientID:      clientID,
		Model:         "gpt-4.1",
		ActualNanoUSD: actualSpend,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	if err := repo.DeleteClient(clientID); err != nil {
		t.Fatalf("DeleteClient returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "$1.23") {
		t.Fatalf("dashboard missing deleted client historical spend: %s", body)
	}
}

func TestAdminDashboardKeepsTodaySpendAfterClientDeleteAndUsageLogTrim(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user, err := control.CreateUser(controlplane.UserInput{
		Username:       "today-spender",
		Password:       "spender-secret",
		Role:           core.UserRoleUser,
		Enabled:        true,
		BalanceNanoUSD: 10 * core.NanoUSDPerUSD,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	const clientID = "client_dashboard_deleted_today"
	if err := repo.UpsertClient(core.APIClient{ID: clientID, Name: "Dashboard Deleted Today", APIKey: "gw_dashboard_deleted_today", OwnerUserID: user.ID, Enabled: true}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	actualSpend := int64(456) * core.NanoUSDPerUSD / 100
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_dashboard_deleted_today",
		ClientID:        clientID,
		UserID:          user.ID,
		ClientName:      "Dashboard Deleted Today",
		Model:           "gpt-4.1",
		ReservedNanoUSD: core.NanoUSDPerUSD,
		Fingerprint:     "dashboard-deleted-today",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_dashboard_deleted_today",
		ClientID:      clientID,
		Model:         "gpt-4.1",
		ActualNanoUSD: actualSpend,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	if err := repo.DeleteClient(clientID); err != nil {
		t.Fatalf("DeleteClient returned error: %v", err)
	}
	if err := repo.ConfigureUsageLogRetention(0); err != nil {
		t.Fatalf("ConfigureUsageLogRetention returned error: %v", err)
	}
	if _, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: clientID, Limit: 10}); total != 0 {
		t.Fatalf("billing requests total = %d, want trimmed", total)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "Today&#39;s Spend: $4.56") {
		t.Fatalf("dashboard missing ledger-backed today spend: %s", body)
	}
}

func TestAdminDashboardUserCountUsesCurrentUsers(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	if _, err := control.CreateUser(controlplane.UserInput{
		Username: "live-user",
		Password: "live-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	}); err != nil {
		t.Fatalf("Create live user returned error: %v", err)
	}
	deleted, err := control.CreateUser(controlplane.UserInput{
		Username:       "deleted-finance-user",
		Password:       "deleted-secret",
		Role:           core.UserRoleUser,
		Enabled:        true,
		BalanceNanoUSD: 10 * core.NanoUSDPerUSD,
	})
	if err != nil {
		t.Fatalf("Create deleted user returned error: %v", err)
	}
	const deletedClientID = "client_deleted_finance_dashboard_count"
	if err := repo.UpsertClient(core.APIClient{ID: deletedClientID, Name: "Deleted Finance Dashboard Count", APIKey: "gw_deleted_finance_dashboard_count", OwnerUserID: deleted.ID, Enabled: true}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_deleted_finance_dashboard_count",
		ClientID:        deletedClientID,
		UserID:          deleted.ID,
		ClientName:      "Deleted Finance Dashboard Count",
		Model:           "gpt-4.1",
		ReservedNanoUSD: core.NanoUSDPerUSD,
		Fingerprint:     "deleted-finance-dashboard-count",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_deleted_finance_dashboard_count",
		ClientID:      deletedClientID,
		Model:         "gpt-4.1",
		ActualNanoUSD: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	if _, err := control.DeleteUser(deleted.ID); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	if currentUsers := len(control.ListUsers()); currentUsers != 2 {
		t.Fatalf("current users = %d, want 2", currentUsers)
	}
	financeUsers := control.FinanceUserSummariesForExport()
	if len(financeUsers) != 3 {
		t.Fatalf("finance users = %d, want 3 including deleted financial user", len(financeUsers))
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	userMetric := regexp.MustCompile(`<span class="metric-label">Users</span>\s*<strong>2</strong>`)
	if body := rec.Body.String(); !userMetric.MatchString(body) {
		t.Fatalf("dashboard should render current user count 2 instead of finance count 3: %s", body)
	}
}

func TestUsersPageLazyLoadsUserDetails(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "lazy-details",
		Password: "lazy-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `data-deferred-url="/admin/users?partial=users-panel"`) {
		t.Fatalf("users page should not render an empty deferred shell: %s", body)
	}
	if !strings.Contains(body, `data-user-details-url="/admin/users/`+user.ID+`/details"`) {
		t.Fatalf("users page missing lazy details url: %s", body)
	}
	if !strings.Contains(body, "Loading user details") {
		t.Fatalf("users page missing user-details lazy placeholder: %s", body)
	}
	for _, unexpected := range []string{"usage-chart-bar", "Balance Log", "No balance changes yet."} {
		if strings.Contains(body, unexpected) {
			t.Fatalf("users page should not eagerly render %q: %s", unexpected, body)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	req.Header.Set("X-Requested-With", "fetch")
	req.Header.Set(ajaxPartialHeader, "users-panel")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("partial status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = rec.Body.String()
	if strings.Contains(body, "<!DOCTYPE html>") || strings.Contains(body, `data-deferred-url="/admin/users?partial=users-panel"`) {
		t.Fatalf("users ajax partial should render panel directly: %s", body)
	}
	if !strings.Contains(body, `data-user-details-url="/admin/users/`+user.ID+`/details"`) {
		t.Fatalf("users panel missing lazy details url: %s", body)
	}
	if !strings.Contains(body, "Loading user details") {
		t.Fatalf("users panel missing lazy details placeholder: %s", body)
	}
	for _, unexpected := range []string{"usage-chart-bar", "Balance Log", "No balance changes yet."} {
		if strings.Contains(body, unexpected) {
			t.Fatalf("users panel should not eagerly render %q: %s", unexpected, body)
		}
	}
}

func TestUserDetailsFragmentRendersUsageAndLedger(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user, err := control.CreateUser(controlplane.UserInput{
		Username:       "detail-user",
		Password:       "detail-secret",
		Role:           core.UserRoleUser,
		Enabled:        true,
		BalanceNanoUSD: 10 * core.NanoUSDPerUSD,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if _, _, err := control.AdjustUserBalance(user.ID, controlplane.UserBalanceAdjustment{
		AmountNanoUSD: 2 * core.NanoUSDPerUSD,
		Reason:        "manual top up",
	}); err != nil {
		t.Fatalf("AdjustUserBalance returned error: %v", err)
	}
	rechargeAmount := int64(5 * core.NanoUSDPerUSD)
	paidAt := time.Now().UTC()
	paymentOrder := core.PaymentOrder{
		ID:            "pay_user_details_fragment",
		OutTradeNo:    "out_user_details_fragment",
		UserID:        user.ID,
		Provider:      core.PaymentProviderPersonalPay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: rechargeAmount,
		Status:        core.PaymentOrderPending,
		CreatedAt:     paidAt.Add(-time.Minute),
	}
	if err := repo.CreatePaymentOrder(paymentOrder); err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}
	if _, credited, err := repo.CompletePaymentOrder(paymentOrder.OutTradeNo, "trade_user_details_fragment", rechargeAmount, paidAt); err != nil || !credited {
		t.Fatalf("CompletePaymentOrder credited=%t err=%v", credited, err)
	}
	if err := repo.CreatePaymentOrder(core.PaymentOrder{
		ID:            "pay_user_details_pending",
		OutTradeNo:    "out_user_details_pending",
		UserID:        user.ID,
		Provider:      core.PaymentProviderPersonalPay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: core.NanoUSDPerUSD,
		Status:        core.PaymentOrderPending,
		CreatedAt:     paidAt,
	}); err != nil {
		t.Fatalf("Create pending payment order returned error: %v", err)
	}
	const clientID = "client_user_details_fragment"
	if err := repo.UpsertClient(core.APIClient{ID: clientID, Name: "Details Client", APIKey: "gw_details_fragment", OwnerUserID: user.ID, Enabled: true}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	actualSpend := int64(123) * core.NanoUSDPerUSD / 100
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_user_details_fragment",
		ClientID:        clientID,
		UserID:          user.ID,
		ClientName:      "Details Client",
		Model:           "gpt-4.1",
		ReservedNanoUSD: actualSpend,
		Fingerprint:     "user-details-fragment",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_user_details_fragment",
		ClientID:      clientID,
		Model:         "gpt-4.1",
		ActualNanoUSD: actualSpend,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "detail-usage-plan",
		Name:               "Detail Usage Plan",
		Enabled:            true,
		PriceNanoUSD:       0,
		PeriodQuotaNanoUSD: 10 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
	if _, err := control.GrantBillingPlan(controlplane.BillingPlanGrantInput{
		PlanID:         "detail-usage-plan",
		TargetUserRefs: []string{user.ID},
		Note:           "detail usage grant",
	}); err != nil {
		t.Fatalf("GrantBillingPlan returned error: %v", err)
	}
	const planClientID = "client_user_details_fragment_plan"
	if err := repo.UpsertClient(core.APIClient{ID: planClientID, Name: "Details Plan Client", APIKey: "gw_details_fragment_plan", OwnerUserID: user.ID, Enabled: true, BillingSource: core.ClientBillingSourcePlan}); err != nil {
		t.Fatalf("UpsertClient plan returned error: %v", err)
	}
	planSpend := int64(345) * core.NanoUSDPerUSD / 100
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:     "req_user_details_fragment_plan",
		ClientID:      planClientID,
		UserID:        user.ID,
		ClientName:    "Details Plan Client",
		Model:         "gpt-4.1",
		BillingSource: core.ClientBillingSourcePlan,
		Fingerprint:   "user-details-fragment-plan",
	}); err != nil {
		t.Fatalf("ReserveBilling plan returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_user_details_fragment_plan",
		ClientID:      planClientID,
		Model:         "gpt-4.1",
		BillingSource: core.ClientBillingSourcePlan,
		ActualNanoUSD: planSpend,
	}); err != nil {
		t.Fatalf("SettleBilling plan returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/admin/users/"+user.ID+"/details", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"detail-user",
		"Hourly Spend",
		`<div class="kv"><span>Historical Recharge</span><strong>$5.00</strong></div>`,
		`<div class="kv"><span>Historical Spend</span><strong>$1.23</strong></div>`,
		"Managed Clients",
		"Details Client",
		"gw_d...ment",
		`data-copy-value="gw_details_fragment"`,
		"Details Plan Client",
		"gw_d...plan",
		`data-copy-value="gw_details_fragment_plan"`,
		"Payment Order",
		"out_user_details_fragment",
		"paid",
		"Balance Log",
		"manual top up",
		"$15.77",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("details fragment missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "out_user_details_pending") {
		t.Fatalf("details fragment should only render paid payment orders: %s", body)
	}
}

func TestUserDetailsFragmentRendersActivePlans(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "detail-plan-user",
		Password: "detail-plan-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if err := repo.UpsertBillingPlanGroup(core.BillingPlanGroup{ID: "group_detail_plans", Name: "Detail Plans"}); err != nil {
		t.Fatalf("UpsertBillingPlanGroup returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "detail-week-plan",
		Name:               "Detail Week Plan",
		Group:              "group_detail_plans",
		Enabled:            true,
		PriceNanoUSD:       7 * core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 25 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        4,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
	if _, err := control.GrantBillingPlan(controlplane.BillingPlanGrantInput{
		PlanID:         "detail-week-plan",
		TargetUserRefs: []string{user.ID},
		Note:           "detail test grant",
	}); err != nil {
		t.Fatalf("GrantBillingPlan returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/admin/users/"+user.ID+"/details", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Active Plan", "Detail Week Plan", "Remaining Package Quota", "$25.00 / $25.00", "Used Quota", "$0.00", "4 / 4"} {
		if !strings.Contains(body, want) {
			t.Fatalf("details fragment missing %q: %s", want, body)
		}
	}
}

func userIDs(users []core.User) []string {
	out := make([]string, 0, len(users))
	for _, user := range users {
		out = append(out, user.ID)
	}
	return out
}
