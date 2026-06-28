package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestClientsPagePaginatesOwnedClients(t *testing.T) {
	repo := &clientOwnerFullListPanicRepository{MemoryRepository: storage.NewMemoryRepository()}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "alice",
		Password: "alice-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	for i := range 30 {
		if err := repo.UpsertClient(core.APIClient{
			ID:          fmt.Sprintf("client_paged_%02d", i),
			Name:        fmt.Sprintf("Paged Key %02d", i),
			APIKey:      fmt.Sprintf("gw_paged_%02d", i),
			OwnerUserID: user.ID,
			Enabled:     true,
			RoutePolicy: core.DefaultRoutePolicy(),
		}); err != nil {
			t.Fatalf("UpsertClient(%d) returned error: %v", i, err)
		}
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/clients?page=2", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Paged Key 25") || !strings.Contains(body, "Paged Key 29") {
		t.Fatalf("second page missing expected clients: %s", body)
	}
	if strings.Contains(body, "Paged Key 00") {
		t.Fatalf("second page should not render first page clients: %s", body)
	}
	if !strings.Contains(body, `/clients?page=1`) {
		t.Fatalf("previous page link missing: %s", body)
	}
}

func TestClientsPageShowsFullBillingOptionLabel(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "billing-label-user",
		Password: "billing-label-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                       core.DefaultAccountGroupID,
		Name:                     core.DefaultAccountGroupName,
		BillingMultiplierBps:     15000,
		PlanBillingMultiplierBps: 5000,
	}); err != nil {
		t.Fatalf("UpsertAccountGroup returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:            "client_billing_label",
		Name:          "Billing Label Key",
		APIKey:        "gw_billing_label",
		OwnerUserID:   user.ID,
		Enabled:       true,
		AccountGroup:  core.DefaultAccountGroupName,
		BillingSource: core.ClientBillingSourcePlan,
		RoutePolicy:   core.DefaultRoutePolicy(),
	}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/clients?lang=zh-CN", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Default (0.5x) [套餐扣费]") {
		t.Fatalf("clients page missing full billing option label: %s", body)
	}
}

func TestClientEditorAccountGroupOptionsRenderRemarks(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "group-remark-user",
		Password: "group-remark-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:     "group_research",
		Name:   "Research",
		Remark: "低价慢速，适合批处理",
	}); err != nil {
		t.Fatalf("UpsertAccountGroup returned error: %v", err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_research",
		Provider: core.ProviderOpenAI,
		Label:    "Research Account",
		Group:    "Research",
		Status:   core.AccountStatusActive,
	}); err != nil {
		t.Fatalf("UpsertAccount returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/clients/new?lang=zh-CN", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`value="Research"`, `data-select-description="低价慢速，适合批处理"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("client editor missing account group remark option %q: %s", want, body)
		}
	}
}

func TestClientEditorHidesPlanBillingForDisabledGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "plan-disabled-group-user",
		Password: "plan-disabled-group-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if err := repo.SetUserBalance(user.ID, 20*core.NanoUSDPerUSD); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "web_disabled_group_plan",
		Name:               "Web Disabled Group Plan",
		Enabled:            true,
		PriceNanoUSD:       core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 10 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: user.ID, PlanID: "web_disabled_group_plan"}); err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	planBillingEnabled := false
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_cash_only_web", Name: "Cash Only Web", PlanBillingEnabled: &planBillingEnabled}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_cash_only_web", Provider: core.ProviderOpenAI, Label: "Cash Only Web", Group: "Cash Only Web", Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/clients/new", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `plan|Cash&#43;Only&#43;Web`) || strings.Contains(body, `plan|Cash+Only+Web`) || strings.Contains(body, `plan|Cash%20Only%20Web`) {
		t.Fatalf("client editor rendered plan billing option for disabled group: %s", body)
	}
	if !strings.Contains(body, `cash|Cash&#43;Only&#43;Web`) || !strings.Contains(body, `Cash Only Web (1x) [Balance billing]`) {
		t.Fatalf("client editor missing cash billing option for disabled group: %s", body)
	}
}

func TestClientsPageRendersAccountGroupRemark(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "client-group-remark-user",
		Password: "client-group-remark-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:     "group_pro",
		Name:   "Pro",
		Remark: "GPT Plus 专用",
	}); err != nil {
		t.Fatalf("UpsertAccountGroup returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:           "client_group_remark",
		Name:         "Remarked Key",
		APIKey:       "gw_group_remark",
		OwnerUserID:  user.ID,
		Enabled:      true,
		AccountGroup: "Pro",
		RoutePolicy:  core.DefaultRoutePolicy(),
	}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/clients?lang=zh-CN", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`client-group-value`, `GPT Plus 专用`} {
		if !strings.Contains(body, want) {
			t.Fatalf("clients page missing account group remark %q: %s", want, body)
		}
	}
	if strings.Contains(body, `class="client-group-remark"`) {
		t.Fatalf("clients page should not repeat account group remark in the card header: %s", body)
	}
}

func TestClientEditorPagesAvoidFullDashboardQueries(t *testing.T) {
	repo := &clientEditorDashboardPanicRepository{MemoryRepository: storage.NewMemoryRepository()}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "editor-user",
		Password: "editor-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:          "client_editor",
		Name:        "Editor Key",
		APIKey:      "gw_editor",
		OwnerUserID: user.ID,
		Enabled:     true,
		RoutePolicy: core.DefaultRoutePolicy(),
	}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())
	for _, path := range []string{"/clients/new", "/clients/client_editor/edit"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want %d body=%s", path, rec.Code, http.StatusOK, rec.Body.String())
		}
	}
}

type clientOwnerFullListPanicRepository struct {
	*storage.MemoryRepository
}

func (r *clientOwnerFullListPanicRepository) ListClientsByOwner(ownerUserID string) []core.APIClient {
	panic("clients page should use ListClientSummariesByOwnerPage instead of full ListClientsByOwner")
}

func (r *clientOwnerFullListPanicRepository) ListClientSummariesByOwnerPage(ownerUserID string, offset, limit int) ([]core.APIClient, int) {
	clients := r.MemoryRepository.ListClientsByOwner(ownerUserID)
	total := len(clients)
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = clientPageSize
	}
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	out := make([]core.APIClient, 0, end-offset)
	for _, client := range clients[offset:end] {
		client.APIKey = ""
		client.RouteAffinityKey = ""
		out = append(out, client)
	}
	return out, total
}

type clientEditorDashboardPanicRepository struct {
	*storage.MemoryRepository
}

func (r *clientEditorDashboardPanicRepository) ListUsersPage(query storage.UserListQuery) ([]storage.UserListItem, int, int) {
	panic("client editor should not query dashboard user counts")
}

func (r *clientEditorDashboardPanicRepository) ListClientSummariesPage(offset, limit int) ([]core.APIClient, int) {
	panic("client editor should not query dashboard client preview")
}

func (r *clientEditorDashboardPanicRepository) ListAuditSummaries(limit int) []core.AuditEvent {
	panic("client editor should not query dashboard audit summaries")
}
