package web

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/accounts"
	"github.com/32ns/ai-gateway/internal/backup"
	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/failover"
	"github.com/32ns/ai-gateway/internal/gateway"
	"github.com/32ns/ai-gateway/internal/payments"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/routing"
	"github.com/32ns/ai-gateway/internal/storage"
	personalpay "personalpay/sdk-go"
)

func newJSONUpstream(t *testing.T, expectedPath, headerName, headerValue string, status int, responseBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != expectedPath {
			t.Fatalf("path = %s, want %s", r.URL.Path, expectedPath)
		}
		if headerName != "" && r.Header.Get(headerName) != headerValue {
			t.Fatalf("%s = %q, want %q", headerName, r.Header.Get(headerName), headerValue)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(responseBody))
	}))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testUnsignedJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("Marshal JWT claims: %v", err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestGatewayEndpointsRequireAPIKey(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer gw_test_key")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, want %d", rec.Code, http.StatusOK)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-API-Key", "gw_test_key")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("x-api-key status = %d, want %d", rec.Code, http.StatusOK)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Basic gw_test_key")
	req.Header.Set("X-API-Key", "gw_test_key")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid authorization scheme status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestGatewayPageRendersExternalAPICatalog(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/gateway", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("gateway page status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`POST</span> <span class="endpoint-path">/v1/chat/completions`,
		`POST</span> <span class="endpoint-path">/v1/responses/compact`,
		`GET</span> <span class="endpoint-path">/v1/responses/{response_id}/input_items`,
		`POST</span> <span class="endpoint-path">/v1/images/edits`,
		`POST</span> <span class="endpoint-path">/v1/audio/transcriptions`,
		`POST</span> <span class="endpoint-path">/anthropic/v1/messages/count_tokens`,
		`GET</span> <span class="endpoint-path">/ag/v1/account/quota`,
		`POST</span> <span class="endpoint-path">/mcp`,
		`GET</span> <span class="endpoint-path">/healthz`,
		`data-group-settings-open="gateway-api-chat-completions"`,
		`id="gateway-api-chat-completions"`,
		`curl https://gateway.example.com/v1/chat/completions`,
		translate(localeEN, "gateway_error_codes"),
		`upstream_transport_error`,
		`gateway_api_key_disabled`,
		`plan_quota_exhausted`,
		`responses_websocket_not_supported`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("gateway page missing %q", want)
		}
	}
	errorCodesIndex := strings.Index(body, translate(localeEN, "gateway_error_codes"))
	auditIndex := strings.Index(body, translate(localeEN, "recent_gateway_audit"))
	if errorCodesIndex < 0 || auditIndex < 0 || errorCodesIndex > auditIndex {
		t.Fatalf("gateway error codes should render before gateway audit")
	}
}

func TestGatewayAccountQuotaEndpointReturnsClientQuota(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	if err := repo.SetUserBalance(admin.ID, 10*core.NanoUSDPerUSD); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:                "client_quota",
		Name:              "Quota Client",
		APIKey:            "gw_quota_key",
		OwnerUserID:       admin.ID,
		Enabled:           true,
		SpendLimitNanoUSD: 5 * core.NanoUSDPerUSD,
		RoutePolicy:       core.DefaultRoutePolicy(),
	}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/ag/v1/account/quota", nil)
	req.Header.Set("Authorization", "Bearer gw_quota_key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload controlplane.ClientQuota
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("Decode returned error: %v", err)
	}
	if payload.ClientID != "client_quota" || payload.ClientName != "Quota Client" || payload.UserID != admin.ID {
		t.Fatalf("client fields = %#v", payload)
	}
	if payload.BalanceNanoUSD != 10*core.NanoUSDPerUSD || payload.SpendLimitNanoUSD != 5*core.NanoUSDPerUSD {
		t.Fatalf("quota limits = %#v", payload)
	}
	if payload.RemainingNanoUSD != 5*core.NanoUSDPerUSD {
		t.Fatalf("remaining = %d, want %d", payload.RemainingNanoUSD, 5*core.NanoUSDPerUSD)
	}
}

func TestGatewayOwnedRoutesAreNotMountedUnderOpenAIV1(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")

	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	paths := []string{
		"/v1/account/quota",
		"/v1/dashboard/billing/subscription",
		"/v1/dashboard/billing/usage",
		"/v1/rerank",
		"/v1/realtime",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer gw_test_key")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
			}
		})
	}
}

func TestModelsEndpointUsesRegisteredProviderModels(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_claude", Name: "Claude", Type: core.AccountGroupTypeClaude}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_claude_models", Name: "Claude Models", APIKey: "gw_claude_models", Enabled: true, AccountGroup: "Claude"}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer gw_test_key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	ids := map[string]bool{}
	for _, item := range payload.Data {
		ids[item.ID] = true
	}

	for _, expected := range []string{"gpt-4.1", "text-embedding-3-small"} {
		if !ids[expected] {
			t.Fatalf("expected model %q in response", expected)
		}
	}
	if ids["claude-sonnet-4-0"] {
		t.Fatalf("default group /v1/models must not include Claude group models")
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer gw_claude_models")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("claude status = %d, want %d", rec.Code, http.StatusOK)
	}
	ids = map[string]bool{}
	payload.Data = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal claude response: %v", err)
	}
	for _, item := range payload.Data {
		ids[item.ID] = true
	}
	if !ids["claude-sonnet-4-0"] {
		t.Fatalf("claude group model missing from response: %#v", ids)
	}
}

func TestModelsEndpointFiltersModelsByClientAccountGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_basic", Name: "Basic"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_plus", Name: "Plus", APIKey: "gw_plus", Enabled: true, AccountGroup: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_basic", Name: "Basic", APIKey: "gw_basic", Enabled: true, AccountGroup: "Basic"}); err != nil {
		t.Fatal(err)
	}
	for _, model := range []core.ModelConfig{
		{ID: "plus-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{"Plus"}},
		{ID: "basic-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{"Basic"}},
		{ID: "hidden-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{}},
	} {
		if err := repo.UpsertModel(model); err != nil {
			t.Fatal(err)
		}
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer gw_plus")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"id":"plus-model"`) {
		t.Fatalf("plus model missing: %s", body)
	}
	for _, hidden := range []string{`"id":"basic-model"`, `"id":"hidden-model"`} {
		if strings.Contains(body, hidden) {
			t.Fatalf("response contains %s: %s", hidden, body)
		}
	}
}

func TestModelsEndpointFiltersModelsByChineseClientAccountGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	groupName := "遇到故障请使用该分组"
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_cn", Name: groupName}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_cn", Name: "避险", APIKey: "gw_cn", Enabled: true, AccountGroup: groupName}); err != nil {
		t.Fatal(err)
	}
	for _, model := range []core.ModelConfig{
		{ID: "cn-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{groupName}},
		{ID: "default-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{core.DefaultAccountGroupName}},
	} {
		if err := repo.UpsertModel(model); err != nil {
			t.Fatal(err)
		}
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer gw_cn")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"id":"cn-model"`) {
		t.Fatalf("chinese group model missing: %s", body)
	}
	if strings.Contains(body, `"id":"default-model"`) {
		t.Fatalf("response contains default model: %s", body)
	}
}

func TestPublicModelsPageRendersWithoutLogin(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	hide := false
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_hidden", Name: "Hidden", ShowInClientEditor: &hide}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                           "public-default-model",
		Provider:                     core.ProviderOpenAI,
		Enabled:                      true,
		VisibleGroups:                []string{core.DefaultAccountGroupName},
		InputPriceNanoUSDPer1M:       core.NanoUSDPerUSD,
		CachedInputPriceNanoUSDPer1M: core.NanoUSDPerUSD / 10,
		OutputPriceNanoUSDPer1M:      2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:            "private-hidden-model",
		Provider:      core.ProviderOpenAI,
		Enabled:       true,
		VisibleGroups: []string{"Hidden"},
	}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(control, nil, "data/state.db")
	req := httptest.NewRequest(http.MethodGet, "/models", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Model List", `href="/models"`, `data-copy-value="public-default-model"`, "Default", "1x", "$1.00 / 1M", "$2.00 / 1M"} {
		if !strings.Contains(body, want) {
			t.Fatalf("public models page missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "private-hidden-model") || strings.Contains(body, "Hidden") {
		t.Fatalf("public models page should not show hidden client-editor group pricing: %s", body)
	}
}

func TestPublicPlansPageRendersCardsWithoutLogin(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	settings := core.DefaultSystemSettings()
	settings.Payment.CNYPerUSD = "7.23"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertBillingPlanGroup(core.BillingPlanGroup{ID: "group_week_cards", Name: "Week Cards"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "public-week-card",
		Name:               "Week Card",
		Description:        "Daily quota for one week",
		Group:              "group_week_cards",
		Enabled:            true,
		PriceNanoUSD:       10 * core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 30 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "orphan-card",
		Name:               "Orphan Card",
		Group:              "missing_group",
		Enabled:            true,
		PriceNanoUSD:       1 * core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 1 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(control, nil, "data/state.db")
	req := httptest.NewRequest(http.MethodGet, "/plans", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`href="/plans"`, `class="plan-card-grid"`, "CNY/$ Rate $1 = ¥7.23", "Week Cards", "Week Card", "$10", "$30", "Log In to Buy", `href="/login?next=%2Fplans"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("public plans page missing %q: %s", want, body)
		}
	}
	for _, hidden := range []string{`action="/plans/purchase"`, "No active plan.", "No plan purchases yet."} {
		if strings.Contains(body, hidden) {
			t.Fatalf("public plans page should not render %q: %s", hidden, body)
		}
	}
	for _, hidden := range []string{"Orphan Card", "missing_group"} {
		if strings.Contains(body, hidden) {
			t.Fatalf("public plans page should not render plan without an existing group %q: %s", hidden, body)
		}
	}
}

func TestPlansPageAllowsPurchaseWithActivePlan(t *testing.T) {
	repo := storage.NewMemoryRepository()
	user := core.User{ID: "plan_page_user", Username: "plan-page-user", Enabled: true, BalanceNanoUSD: 20 * core.NanoUSDPerUSD}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertBillingPlanGroup(core.BillingPlanGroup{ID: "group_page_plans", Name: "Page Plans"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "page-day-card",
		Name:               "Page Day Card",
		Group:              "group_page_plans",
		Enabled:            true,
		PriceNanoUSD:       10 * core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 100 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: user.ID, PlanID: "page-day-card"}); err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}

	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/plans", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`action="/plans/purchase"`, `data-plan-purchase-form`, `data-purchase-confirm="Buy Page Day Card and deduct $10 from your balance?"`, `name="purchase_mode" value="separate"`, "Page Day Card", "$100 / $100"} {
		if !strings.Contains(body, want) {
			t.Fatalf("plans page with active plan missing %q: %s", want, body)
		}
	}
	for _, hidden := range []string{`<button type="submit" disabled`, "Active Plan Exists"} {
		if strings.Contains(body, hidden) {
			t.Fatalf("plans page should not block repurchase with %q: %s", hidden, body)
		}
	}
}

func TestAdminPlanGroupSaleSwitchHidesPlansButKeepsEntitlementUsable(t *testing.T) {
	repo := storage.NewMemoryRepository()
	user := core.User{ID: "plan_group_switch_user", Username: "plan-group-switch-user", Enabled: true, BalanceNanoUSD: 30 * core.NanoUSDPerUSD}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_group_switch_user", Name: "Group Switch Client", APIKey: "gw_group_switch_user", OwnerUserID: user.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertBillingPlanGroup(core.BillingPlanGroup{ID: "group_switch_cards", Name: "Switch Cards"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "switch-week-card",
		Name:               "Switch Week Card",
		Group:              "group_switch_cards",
		Enabled:            true,
		PriceNanoUSD:       10 * core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 100 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	}); err != nil {
		t.Fatal(err)
	}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	userHandler := authenticatedUserHandler(t, control, user, server.Handler())

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("plan_id", "switch-week-card")
	form.Set("purchase_mode", "separate")
	req := httptest.NewRequest(http.MethodPost, "/plans/purchase", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	userHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("initial purchase status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	before, err := repo.GetUser(user.ID)
	if err != nil {
		t.Fatal(err)
	}

	adminHandler := authenticatedAdminHandler(t, control, server.Handler())
	form = url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("name", "Switch Cards")
	form.Set("sort_order", "0")
	req = httptest.NewRequest(http.MethodPost, "/admin/plan-groups/group_switch_cards/update", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("group update status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	group, err := control.GetBillingPlanGroup("group_switch_cards")
	if err != nil {
		t.Fatal(err)
	}
	if !group.SaleDisabled {
		t.Fatalf("group SaleDisabled = false, want true")
	}

	req = httptest.NewRequest(http.MethodGet, "/plans", nil)
	rec = httptest.NewRecorder()
	userHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("plans page status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"No plans are available.", "$100 / $100", "Switch Week Card"} {
		if !strings.Contains(body, want) {
			t.Fatalf("plans page after group disabled missing %q: %s", want, body)
		}
	}
	for _, hidden := range []string{`data-plan-id="switch-week-card"`, `action="/plans/purchase"`} {
		if strings.Contains(body, hidden) {
			t.Fatalf("plans page after group disabled should not render purchase affordance %q: %s", hidden, body)
		}
	}

	form = url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("plan_id", "switch-week-card")
	form.Set("purchase_mode", "separate")
	req = httptest.NewRequest(http.MethodPost, "/plans/purchase", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	userHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("repurchase disabled group status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	after, err := repo.GetUser(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.BalanceNanoUSD != before.BalanceNanoUSD {
		t.Fatalf("balance after disabled repurchase = %d, want unchanged %d", after.BalanceNanoUSD, before.BalanceNanoUSD)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_group_switch_existing",
		ClientID:        "client_group_switch_user",
		UserID:          user.ID,
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: 25 * core.NanoUSDPerUSD,
		Fingerprint:     "group-switch-existing",
	}); err != nil {
		t.Fatalf("ReserveBilling after group disabled returned error: %v", err)
	}
}

func TestAdminPlansPageCreatesGroupsAndUsesGroupSelect(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/plans?tab=plans", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin plans status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Add Group", `action="/admin/plan-groups"`, "No package groups yet."} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin plans page missing %q: %s", want, body)
		}
	}
	for _, hidden := range []string{`name="group"`, `id="plan-create"`} {
		if strings.Contains(body, hidden) {
			t.Fatalf("admin plans page should not render %q before package groups exist: %s", hidden, body)
		}
	}
	if strings.Contains(body, `<input name="group"`) {
		t.Fatalf("admin plans page should use a select for package groups: %s", body)
	}

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("name", "VIP Cards")
	form.Set("quota_price_ratio", "1:0.8")
	form.Set("sort_order", "15")
	req = httptest.NewRequest(http.MethodPost, "/admin/plan-groups", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create plan group status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	custom, err := control.GetBillingPlanGroup("group_vip_cards")
	if err != nil {
		t.Fatalf("GetBillingPlanGroup returned error: %v", err)
	}
	if custom.QuotaPriceRatio != "1:0.8" {
		t.Fatalf("created group ratio = %q, want %q", custom.QuotaPriceRatio, "1:0.8")
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/plans?tab=plans&group="+url.QueryEscape(custom.ID), nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin plans with group status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = rec.Body.String()
	for _, want := range []string{"VIP Cards", `name="group"`, `value="group_vip_cards"`, `data-quota-price-ratio="1:0.8"`, `name="quota_price_ratio"`, `data-plan-price-sync-button`, `id="plan-create"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin plans page with group missing %q: %s", want, body)
		}
	}

	form = url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("current_group", custom.ID)
	form.Set("name", "VIP Cards")
	form.Set("quota_price_ratio", "1:0.75")
	form.Set("sort_order", "15")
	form.Set("sale_enabled", "on")
	req = httptest.NewRequest(http.MethodPost, "/admin/plan-groups/"+custom.ID+"/update", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("update plan group status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	custom, err = control.GetBillingPlanGroup(custom.ID)
	if err != nil {
		t.Fatalf("GetBillingPlanGroup after update returned error: %v", err)
	}
	if custom.QuotaPriceRatio != "1:0.75" {
		t.Fatalf("updated group ratio = %q, want %q", custom.QuotaPriceRatio, "1:0.75")
	}

	form = url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("name", "VIP Week")
	form.Set("group", custom.ID)
	form.Set("enabled", "on")
	form.Set("price_usd", "13")
	form.Set("period_quota_usd", "2")
	form.Set("period_duration_hours", "24")
	form.Set("period_count", "7")
	form.Set("sort_order", "0")
	req = httptest.NewRequest(http.MethodPost, "/admin/plans", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create plan status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	plans := control.ListBillingPlans(true)
	if len(plans) != 1 || plans[0].Group != custom.ID {
		t.Fatalf("plans = %#v, want one plan in group %q", plans, custom.ID)
	}
	if plans[0].PriceNanoUSD != 13*core.NanoUSDPerUSD || plans[0].PeriodQuotaNanoUSD != 2*core.NanoUSDPerUSD {
		t.Fatalf("plan price/quota = %d/%d, want %d/%d", plans[0].PriceNanoUSD, plans[0].PeriodQuotaNanoUSD, 13*core.NanoUSDPerUSD, 2*core.NanoUSDPerUSD)
	}

	form = url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("name", "VIP Free Week")
	form.Set("group", custom.ID)
	form.Set("enabled", "on")
	form.Set("price_usd", "")
	form.Set("period_quota_usd", "2")
	form.Set("period_duration_hours", "24")
	form.Set("period_count", "7")
	form.Set("sort_order", "1")
	req = httptest.NewRequest(http.MethodPost, "/admin/plans", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create plan with blank price status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	plans = control.ListBillingPlans(true)
	if len(plans) != 2 {
		t.Fatalf("plans after blank price create = %#v, want two plans", plans)
	}
	var blankPricePlan core.BillingPlan
	for _, plan := range plans {
		if plan.Name == "VIP Free Week" {
			blankPricePlan = plan
			break
		}
	}
	if blankPricePlan.ID == "" {
		t.Fatalf("blank price plan not found in %#v", plans)
	}
	if blankPricePlan.PriceNanoUSD != 0 || blankPricePlan.PeriodQuotaNanoUSD != 2*core.NanoUSDPerUSD {
		t.Fatalf("blank price plan price/quota = %d/%d, want 0/%d", blankPricePlan.PriceNanoUSD, blankPricePlan.PeriodQuotaNanoUSD, 2*core.NanoUSDPerUSD)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/plans?tab=plans", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin plans after create status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = rec.Body.String()
	for _, want := range []string{"VIP Cards", `value="group_vip_cards"`, `selected`} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin plans page after create missing %q: %s", want, body)
		}
	}
}

func TestAdminPlansSubscriptionsPageShowsUserPlanStats(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if err := repo.UpsertBillingPlanGroup(core.BillingPlanGroup{ID: "group_subscription_admin", Name: "Subscription Admin"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "admin-subscription-week",
		Name:               "Admin Subscription Week",
		Group:              "group_subscription_admin",
		Enabled:            true,
		PriceNanoUSD:       10 * core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 100 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertUser(core.User{ID: "user_admin_subscription", Username: "admin-plan-buyer", Enabled: true, BalanceNanoUSD: 20 * core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_admin_subscription", Name: "Admin Subscription Client", APIKey: "gw_admin_subscription", OwnerUserID: "user_admin_subscription", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_admin_subscription", PlanID: "admin-subscription-week"}); err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_admin_subscription",
		ClientID:        "client_admin_subscription",
		UserID:          "user_admin_subscription",
		BillingSource:   core.ClientBillingSourcePlan,
		ReservedNanoUSD: 25 * core.NanoUSDPerUSD,
		Fingerprint:     "admin-subscription",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_admin_subscription",
		ClientID:      "client_admin_subscription",
		ActualNanoUSD: 25 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/admin/plans", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin plan subscriptions status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"User Plans",
		"Plan Revenue",
		"Active Used Quota",
		"Plan Summary",
		"User Plan Details",
		"Plan Usage Details",
		"Current period and history are grouped by the plan&#39;s rolling cycle.",
		"Current Period",
		"Used This Period",
		"Plan History",
		"Period Usage",
		"Period",
		"admin-plan-buyer",
		"Admin Subscription Week",
		"Subscription Admin",
		"$10.00",
		"$100.00",
		"$75.00 / $100.00",
		"$25.00",
		"&#43;$75.00",
		`name="subscription_status"`,
		`data-group-settings-open="plan-usage-`,
		"1 usage records",
		`value="active" selected`,
		`href="/admin/plans"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin plan subscriptions page missing %q: %s", want, body)
		}
	}
	for _, hidden := range []string{"Add Group", `id="plan-create"`} {
		if strings.Contains(body, hidden) {
			t.Fatalf("subscription tab should not render config action %q: %s", hidden, body)
		}
	}
	for _, hidden := range []string{
		"Plan Quota Daily Usage",
		`name="quota_user_id"`,
		`name="quota_plan_id"`,
		`name="quota_days"`,
		`name="quota_started_at"`,
		`name="quota_ended_at"`,
	} {
		if strings.Contains(body, hidden) {
			t.Fatalf("subscription tab should not render quota report control %q: %s", hidden, body)
		}
	}
	for _, hidden := range []string{"group_subscription_admin", "user_admin_subscription"} {
		if strings.Contains(body, hidden) {
			t.Fatalf("subscription tab should not expose %q: %s", hidden, body)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/plans?tab=subscriptions&subscription_status=", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("all filter status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = rec.Body.String()
	if strings.Contains(body, `value="active" selected`) {
		t.Fatalf("explicit all filter should not select active status: %s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/plans?tab=subscriptions&subscription_status=expired", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expired filter status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = rec.Body.String()
	if !strings.Contains(body, "No plan purchases yet.") {
		t.Fatalf("expired filter missing empty state: %s", body)
	}
	if strings.Contains(body, "$75.00 / $100.00") {
		t.Fatalf("expired filter should not show active detail row: %s", body)
	}
}

func TestAdminPlanEntitlementCancel(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "admin-cancel-week",
		Name:               "Admin Cancel Week",
		Enabled:            true,
		PriceNanoUSD:       10 * core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 100 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertUser(core.User{ID: "user_admin_cancel", Username: "admin-cancel-buyer", Enabled: true, BalanceNanoUSD: 20 * core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	purchase, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_admin_cancel", PlanID: "admin-cancel-week"})
	if err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/admin/plans?tab=subscriptions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin plan subscriptions status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`action="/admin/plans/entitlements/cancel"`,
		`data-confirm="Cancel this user&#39;s active plan? Remaining plan quota will be cleared."`,
		`data-confirm-tone="danger"`,
		`name="entitlement_id" value="` + purchase.Entitlement.ID + `"`,
		">Cancel</button>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin plan cancel UI missing %q: %s", want, body)
		}
	}

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("entitlement_id", purchase.Entitlement.ID)
	req = httptest.NewRequest(http.MethodPost, "/admin/plans/entitlements/cancel", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("cancel status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/admin/plans?tab=subscriptions&notice=plan_cancelled" {
		t.Fatalf("cancel redirect = %q", got)
	}
	entitlements := repo.ListUserPlanEntitlements("user_admin_cancel")
	if len(entitlements) != 1 || entitlements[0].Status != core.UserPlanEntitlementCancelled || entitlements[0].CurrentQuotaNanoUSD != 0 {
		t.Fatalf("cancelled entitlements = %#v, want cancelled entitlement with zero quota", entitlements)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/plans?tab=subscriptions&subscription_status=cancelled&notice=plan_cancelled", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancelled subscriptions status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = rec.Body.String()
	for _, want := range []string{"Plan cancelled.", "admin-cancel-buyer", "Cancelled", "$0.00 / $100.00"} {
		if !strings.Contains(body, want) {
			t.Fatalf("cancelled subscriptions page missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, `action="/admin/plans/entitlements/cancel"`) {
		t.Fatalf("cancelled subscriptions page should not show cancel action: %s", body)
	}
}

func TestAdminPlanGrantUsesMessageStyleUserPicker(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if err := repo.UpsertBillingPlanGroup(core.BillingPlanGroup{ID: "group_grant_admin", Name: "Grant Admin"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "admin-grant-week",
		Name:               "Admin Grant Week",
		Group:              "group_grant_admin",
		Enabled:            true,
		PriceNanoUSD:       10 * core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 100 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertUser(core.User{ID: "user_admin_grant", Username: "admin-grant-target", Enabled: true, BalanceNanoUSD: 0}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/admin/plans", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin plan subscriptions status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-group-settings-open="plan-grant"`,
		`id="plan-grant"`,
		`data-message-target`,
		`data-message-user-search-url="/messages/users/search"`,
		`data-message-selected-users`,
		`<option value="paid">`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin plan grant UI missing %q: %s", want, body)
		}
	}

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("plan_id", "admin-grant-week")
	form.Set("target_mode", "user")
	form.Add("target_user_id", "user_admin_grant")
	form.Set("note", "test grant")
	req = httptest.NewRequest(http.MethodPost, "/admin/plans/grant", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("grant status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/admin/plans?tab=subscriptions&notice=plan_granted" {
		t.Fatalf("grant redirect = %q", got)
	}
	entitlements := repo.ListUserPlanEntitlements("user_admin_grant")
	if len(entitlements) != 1 {
		t.Fatalf("granted entitlements len = %d, want 1: %#v", len(entitlements), entitlements)
	}
	if entitlements[0].PriceNanoUSD != 0 || entitlements[0].CurrentQuotaNanoUSD != 100*core.NanoUSDPerUSD {
		t.Fatalf("granted entitlement = %#v, want free quota package", entitlements[0])
	}
	deliveries := control.ListSiteMessages(core.User{ID: "user_admin_grant", Username: "admin-grant-target", Enabled: true})
	if len(deliveries) != 1 || deliveries[0].Message.Title != "套餐赠送到账" || !strings.Contains(deliveries[0].Message.Body, "Admin Grant Week") {
		t.Fatalf("grant message deliveries = %#v, want plan grant notice", deliveries)
	}
	req = httptest.NewRequest(http.MethodGet, "/messages", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin messages status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "套餐赠送到账") || strings.Contains(body, "Admin Grant Week") {
		t.Fatalf("admin inbox should not show grant notice targeted to another user: %s", body)
	}
}

func TestAdminPlanGrantTargetsPaidUsers(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if err := repo.UpsertBillingPlanGroup(core.BillingPlanGroup{ID: "group_grant_paid_admin", Name: "Grant Paid Admin"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "admin-grant-paid-week",
		Name:               "Admin Grant Paid Week",
		Group:              "group_grant_paid_admin",
		Enabled:            true,
		PriceNanoUSD:       10 * core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 100 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []core.User{
		{ID: "user_admin_paid_grant", Username: "admin-paid-grant", Enabled: true},
		{ID: "user_admin_unpaid_grant", Username: "admin-unpaid-grant", Enabled: true},
	} {
		if err := repo.UpsertUser(user); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	order := core.PaymentOrder{
		ID:            "pay_admin_paid_grant",
		OutTradeNo:    "out_admin_paid_grant",
		UserID:        "user_admin_paid_grant",
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: core.NanoUSDPerUSD,
		Currency:      "USD",
		Status:        core.PaymentOrderPending,
		CreatedAt:     now,
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}
	if _, _, err := repo.CompletePaymentOrder(order.OutTradeNo, "trade_admin_paid_grant", order.AmountNanoUSD, now); err != nil {
		t.Fatalf("CompletePaymentOrder returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("plan_id", "admin-grant-paid-week")
	form.Set("target_mode", "paid")
	form.Add("target_user_id", "user_admin_unpaid_grant")
	form.Set("note", "paid users grant")
	req := httptest.NewRequest(http.MethodPost, "/admin/plans/grant", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("grant status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/admin/plans?tab=subscriptions&notice=plan_granted" {
		t.Fatalf("grant redirect = %q", got)
	}
	if entitlements := repo.ListUserPlanEntitlements("user_admin_paid_grant"); len(entitlements) != 1 {
		t.Fatalf("paid user entitlements len = %d, want 1: %#v", len(entitlements), entitlements)
	}
	if entitlements := repo.ListUserPlanEntitlements("user_admin_unpaid_grant"); len(entitlements) != 0 {
		t.Fatalf("unpaid hidden target entitlements len = %d, want 0: %#v", len(entitlements), entitlements)
	}
	deliveries := control.ListSiteMessages(core.User{ID: "user_admin_paid_grant", Username: "admin-paid-grant", Enabled: true})
	if len(deliveries) != 1 || deliveries[0].Message.Title != "套餐赠送到账" || !strings.Contains(deliveries[0].Message.Body, "Admin Grant Paid Week") {
		t.Fatalf("paid user message deliveries = %#v, want plan grant notice", deliveries)
	}
	if deliveries := control.ListSiteMessages(core.User{ID: "user_admin_unpaid_grant", Username: "admin-unpaid-grant", Enabled: true}); len(deliveries) != 0 {
		t.Fatalf("unpaid hidden target message deliveries len = %d, want 0: %#v", len(deliveries), deliveries)
	}
}

func TestPublicHomeLinksToModelsPage(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`href="/models"`, `href="/images"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("home page missing public link %q: %s", want, body)
		}
	}
	if strings.Contains(body, `home-tool-rail-compact`) {
		t.Fatalf("home page should use full tool rail while image lab is visible: %s", body)
	}
}

func TestPublicHomeHidesImageLabWhenDisabled(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	disabled := false
	settings := core.DefaultSystemSettings()
	settings.Image.UserConsoleEnabled = &disabled
	if _, err := control.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `href="/models"`) {
		t.Fatalf("home page missing models link: %s", body)
	}
	if strings.Contains(body, `href="/images"`) {
		t.Fatalf("home page should hide image lab links when disabled: %s", body)
	}
	if !strings.Contains(body, `home-tool-rail-compact`) {
		t.Fatalf("home page should use compact tool rail when image lab is disabled: %s", body)
	}
}

func TestPublicImageLabRequiresLogin(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	req := httptest.NewRequest(http.MethodGet, "/images", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/login?") || !strings.Contains(location, "next=%2Fimages") {
		t.Fatalf("Location = %q, want login redirect back to /images", location)
	}
}

func TestModelsPageRendersVisibilityAndBillingControls(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                           "gpt-visible",
		Provider:                     core.ProviderOpenAI,
		UpstreamID:                   "gpt-visible-upstream",
		Enabled:                      true,
		VisibleGroups:                []string{"Plus"},
		Source:                       core.ModelSourceManual,
		InputPriceNanoUSDPer1M:       1_750_000_000,
		CachedInputPriceNanoUSDPer1M: 175_000_000,
		OutputPriceNanoUSDPer1M:      14_000_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:          "gpt-same",
		DisplayName: "gpt-same",
		Provider:    core.ProviderOpenAI,
		UpstreamID:  "gpt-same",
		Enabled:     true,
		Source:      core.ModelSourceManual,
	}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/admin/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, expected := range []string{"gpt-visible", "Model Type", "Auto Detect", "Text", "Video", "Visible Groups", "Edit Billing", "model-group-popover", "model-billing-dialog", `name="billing_fixed"`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("models page missing %q: %s", expected, body)
		}
	}
	if strings.Contains(body, `name="billing_fixed" checked`) {
		t.Fatalf("fixed billing checkbox should default unchecked: %s", body)
	}
	if strings.Contains(body, "model-type-form") || strings.Contains(body, "/type") || strings.Contains(body, "Save Type") {
		t.Fatalf("models page should not expose post-create model type edits: %s", body)
	}
	if strings.Contains(body, `<span class="muted">gpt-same</span>`) {
		t.Fatalf("models page should hide duplicate display name: %s", body)
	}
	if !strings.Contains(body, `<code>gpt-same</code>`) {
		t.Fatalf("models page should show upstream model id: %s", body)
	}
}

func TestModelPriceSubmitPersistsBillingFixed(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	if err := repo.UpsertModel(core.ModelConfig{
		ID:            "gpt-fixed",
		Provider:      core.ProviderOpenAI,
		UpstreamID:    "gpt-fixed",
		Enabled:       true,
		VisibleGroups: []string{core.DefaultAccountGroupName},
		Source:        core.ModelSourceManual,
	}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("billing_mode", core.ModelBillingModeToken)
	form.Set("billing_fixed", "on")
	form.Set("input_price_usd_per_1m", "1.25")
	form.Set("cached_input_price_usd_per_1m", "0.25")
	form.Set("output_price_usd_per_1m", "2.5")
	form.Set("request_price_usd", "0")

	req := httptest.NewRequest(http.MethodPost, "/admin/models/gpt-fixed/price", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	model, err := repo.GetModel("gpt-fixed")
	if err != nil {
		t.Fatal(err)
	}
	if !model.BillingFixed {
		t.Fatalf("BillingFixed = false, want true")
	}
	if model.InputPriceNanoUSDPer1M != 1250000000 || model.CachedInputPriceNanoUSDPer1M != 250000000 || model.OutputPriceNanoUSDPer1M != 2500000000 {
		t.Fatalf("prices = input %d cached %d output %d", model.InputPriceNanoUSDPer1M, model.CachedInputPriceNanoUSDPer1M, model.OutputPriceNanoUSDPer1M)
	}

	form.Del("billing_fixed")
	req = httptest.NewRequest(http.MethodPost, "/admin/models/gpt-fixed/price", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	model, err = repo.GetModel("gpt-fixed")
	if err != nil {
		t.Fatal(err)
	}
	if model.BillingFixed {
		t.Fatalf("BillingFixed = true, want false after unchecked submit")
	}
}

func TestModelsEndpointRetrievesModelByID(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/v1/models/gpt-4.1", nil)
	req.Header.Set("Authorization", "Bearer gw_test_key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var payload modelObject
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.ID != "gpt-4.1" || payload.Object != "model" || payload.OwnedBy != string(core.ProviderOpenAI) {
		t.Fatalf("model payload = %+v", payload)
	}
}

func TestSettingsPageUpdatesSystemSettings(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/settings", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"System Settings", "name=\"system_proxy_url\"", "href=\"#settings-image\"", "name=\"github_login_client_id\"", "name=\"google_login_client_id\"", "#settings-users", "#settings-home", `id="settings-registration"`, `id="settings-registration-security"`, `id="settings-email-verification"`, `id="settings-invitation"`, `id="settings-third-party-login"`, "name=\"allow_public_registration\"", "name=\"registration_email_allowlist_enabled\"", "name=\"registration_email_allowlist\"", "name=\"new_user_reward_enabled\"", "name=\"registration_username_min_length\"", "name=\"register_ip_hourly_limit\"", "name=\"email_code_ip_hourly_limit\"", "name=\"turnstile_site_key\"", "name=\"turnstile_secret_key\"", "name=\"invitation_enabled\"", "name=\"require_invitation_code\"", "name=\"inviter_recharge_reward_percent\"", "name=\"user_dashboard_custom_panel_enabled\"", "name=\"user_dashboard_custom_panel_html\"", "name=\"home_brand_title\"", "name=\"home_brand_subtitle\"", "name=\"home_heading\"", "name=\"home_cost_multiplier\"", "name=\"email_provider\"", "data-email-provider-select", `data-email-provider-field="cloudmail"`, `data-email-provider-field="smtp"`, `name="smtp_port" value="465"`, "name=\"cloudmail_base_url\"", "data-email-test", "/admin/email-test", "/admin/settings", "/admin/backup", "name=\"image_user_console_enabled\"", "name=\"image_backend\"", "name=\"payment_min_recharge_usd\"", "name=\"payment_max_recharge_usd\""} {
		if !strings.Contains(body, want) {
			t.Fatalf("settings page missing %q", want)
		}
	}
	runtimeIndex := strings.Index(body, `id="settings-runtime"`)
	homeSectionIndex := strings.Index(body, `id="settings-home"`)
	userDashboardIndex := strings.Index(body, `name="user_dashboard_custom_panel_enabled"`)
	if runtimeIndex == -1 || homeSectionIndex == -1 || userDashboardIndex == -1 || userDashboardIndex < runtimeIndex || userDashboardIndex > homeSectionIndex {
		t.Fatalf("user dashboard settings should render inside runtime section: runtime=%d custom=%d home=%d body=%s", runtimeIndex, userDashboardIndex, homeSectionIndex, body)
	}
	usersIndex := strings.Index(body, `href="#settings-users"`)
	imageIndex := strings.Index(body, `href="#settings-image"`)
	if usersIndex == -1 || imageIndex == -1 || imageIndex < usersIndex {
		t.Fatalf("image settings tab should appear after user settings tab: users=%d image=%d body=%s", usersIndex, imageIndex, body)
	}
	if strings.Contains(body, `href="/payments"`) || strings.Contains(body, `class="nav-link " href="/admin/backup"`) || strings.Contains(body, `class="nav-link active" href="/admin/backup"`) {
		t.Fatalf("admin top navigation should not expose payments or backup as top-level links: %s", body)
	}
	csrf := extractCSRFToken(t, body)

	form := url.Values{}
	form.Set("csrf_token", csrf)
	form.Set("public_base_url", "https://gateway.example.com/")
	form.Set("allow_public_registration", "on")
	form.Set("registration_email_allowlist_enabled", "on")
	form.Set("registration_email_allowlist", " Example.COM\n@qq.com @example.com ")
	form.Set("new_user_reward_enabled", "on")
	form.Set("new_user_reward_usd", "2.25")
	form.Set("registration_username_min_length", "5")
	form.Set("register_ip_hourly_limit", "12")
	form.Set("email_code_ip_hourly_limit", "6")
	form.Set("turnstile_enabled", "on")
	form.Set("turnstile_site_key", "turnstile-site")
	form.Set("turnstile_secret_key", "turnstile-secret")
	form.Set("system_proxy_url", "http://127.0.0.1:7890")
	form.Set("openai_oauth_enabled", "on")
	form.Set("github_login_enabled", "on")
	form.Set("github_login_client_id", "github-client")
	form.Set("github_login_secret", "github-secret")
	form.Set("google_login_enabled", "on")
	form.Set("google_login_client_id", "google-client")
	form.Set("google_login_secret", "google-secret")
	form.Set("login_oauth_auto_create_user", "on")
	form.Set("email_verify_on_register", "on")
	form.Set("email_provider", "smtp")
	form.Set("smtp_host", "smtp.example.com")
	form.Set("smtp_port", "465")
	form.Set("smtp_username", "smtp-user")
	form.Set("smtp_password", "smtp-secret")
	form.Set("smtp_from_email", "noreply@example.com")
	form.Set("smtp_from_name", "AI Gateway")
	form.Set("email_code_ttl_seconds", "900")
	form.Set("email_send_cooldown_seconds", "90")
	form.Set("email_hourly_send_limit", "8")
	form.Set("email_max_attempts", "4")
	form.Set("home_brand_title", "Custom Gateway")
	form.Set("home_brand_subtitle", "Custom homepage subtitle")
	form.Set("home_heading", "Custom heading")
	form.Set("home_summary", `Custom summary\nSecond line`)
	form.Set("home_availability_key", "Uptime")
	form.Set("home_availability", "99.99%")
	form.Set("home_cost_key", "Rate")
	form.Set("home_cost_multiplier", "≤0.2x")
	form.Set("home_latency_key", "Delay")
	form.Set("home_latency", "≤0.10s")
	form.Set("home_capability_key", "Quality")
	form.Set("home_capability", "No downgrade")
	form.Set("user_dashboard_custom_panel_enabled", "on")
	form.Set("user_dashboard_custom_panel_html", `<strong>Console notice</strong>`)
	form.Set("image_user_console_enabled", "on")
	form.Set("image_backend", "official")
				form.Set("payment_min_recharge_usd", "2.50")
	form.Set("payment_max_recharge_usd", "200")
	form.Set("invitation_enabled", "on")
	form.Set("inviter_recharge_reward_percent", "5")
	form.Set("invitee_reward_usd", "1.25")
	form.Set("audit_limit", "3")
	form.Set("wechat_pay_enabled", "on")
	form.Set("wechat_pay_app_id", "wx_app")
	form.Set("wechat_pay_mch_id", "mch_1")
	form.Set("wechat_pay_api_v3_key", "0123456789abcdef0123456789abcdef")
	form.Set("wechat_pay_merchant_serial_no", "serial_1")
	form.Set("wechat_pay_public_key_id", "PUB_KEY_ID_1")
	form.Set("wechat_pay_merchant_private_key_pem", "wechat-private")
	form.Set("wechat_pay_public_key_pem", "wechat-public")
	form.Set("alipay_enabled", "on")
	form.Set("alipay_app_id", "ali_app")
	form.Set("alipay_gateway_url", "https://openapi.alipay.com/gateway.do")
	form.Set("alipay_sign_type", "RSA2")
	form.Set("alipay_return_url", "https://gateway.example.com/payments/return/alipay")
	form.Set("alipay_private_key_pem", "ali-private")
	form.Set("alipay_public_key_pem", "ali-public")
	postReq := httptest.NewRequest(http.MethodPost, "/admin/settings", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range rec.Result().Cookies() {
		postReq.AddCookie(cookie)
	}
	postRec := httptest.NewRecorder()
	handler.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", postRec.Code, http.StatusSeeOther, postRec.Body.String())
	}
	if got := postRec.Header().Get("Location"); got != "/admin/settings?saved=1" {
		t.Fatalf("settings save redirect location = %q", got)
	}
	savedReq := httptest.NewRequest(http.MethodGet, "/admin/settings?saved=1", nil)
	for _, cookie := range rec.Result().Cookies() {
		savedReq.AddCookie(cookie)
	}
	savedRec := httptest.NewRecorder()
	handler.ServeHTTP(savedRec, savedReq)
	if savedRec.Code != http.StatusOK {
		t.Fatalf("saved status = %d, want %d body=%s", savedRec.Code, http.StatusOK, savedRec.Body.String())
	}
	if !strings.Contains(savedRec.Body.String(), `data-clear-url-params="saved"`) || !strings.Contains(savedRec.Body.String(), "Settings saved") {
		t.Fatalf("settings saved page should render saved notice: %s", savedRec.Body.String())
	}
	if strings.Contains(savedRec.Body.String(), "Password updated.") {
		t.Fatalf("settings saved page should not render password notice: %s", savedRec.Body.String())
	}
	for _, want := range []string{`<div class="brand-title">Custom Gateway</div>`, `<div class="brand-subtitle">Custom homepage subtitle</div>`} {
		if !strings.Contains(savedRec.Body.String(), want) {
			t.Fatalf("settings page header missing configured brand value %q: %s", want, savedRec.Body.String())
		}
	}

	settings, err := repo.GetSystemSettings()
	if err != nil {
		t.Fatalf("GetSystemSettings returned error: %v", err)
	}
	if settings.Runtime.PublicBaseURL != "https://gateway.example.com" {
		t.Fatalf("PublicBaseURL = %q", settings.Runtime.PublicBaseURL)
	}
	if !settings.Runtime.AllowPublicRegistration {
		t.Fatalf("AllowPublicRegistration = false, want true")
	}
	if !settings.Runtime.RegistrationEmailAllowlistEnabled || len(settings.Runtime.RegistrationEmailAllowlist) != 2 || settings.Runtime.RegistrationEmailAllowlist[0] != "@example.com" || settings.Runtime.RegistrationEmailAllowlist[1] != "@qq.com" {
		t.Fatalf("RegistrationEmailAllowlist = %#v", settings.Runtime)
	}
	if !settings.Registration.NewUserRewardEnabled || settings.Registration.NewUserRewardNanoUSD != 2250000000 {
		t.Fatalf("Registration settings = %#v", settings.Registration)
	}
	if settings.Registration.RequireInvitationCode || settings.Registration.UsernameMinLength != 5 || settings.Registration.RegisterIPHourlyLimit != 12 || settings.Registration.EmailCodeIPHourlyLimit != 6 {
		t.Fatalf("Registration security settings = %#v", settings.Registration)
	}
	if !settings.Registration.TurnstileEnabled || settings.Registration.TurnstileSiteKey != "turnstile-site" || settings.Registration.TurnstileSecretKey != "turnstile-secret" {
		t.Fatalf("Turnstile settings = %#v", settings.Registration)
	}
	if settings.OAuth.ClaudeEnabled {
		t.Fatalf("ClaudeEnabled = true, want false")
	}
	if !settings.OAuth.GitHubLoginEnabled || settings.OAuth.GitHubLoginClientID != "github-client" || settings.OAuth.GitHubLoginSecret != "github-secret" {
		t.Fatalf("GitHub login settings = %#v", settings.OAuth)
	}
	if !settings.OAuth.GoogleLoginEnabled || settings.OAuth.GoogleLoginClientID != "google-client" || settings.OAuth.GoogleLoginSecret != "google-secret" || !settings.OAuth.LoginAutoCreateUser {
		t.Fatalf("Google login settings = %#v", settings.OAuth)
	}
	if !settings.Email.RegistrationVerificationEnabled || settings.Email.SMTPHost != "smtp.example.com" || settings.Email.SMTPPort != 465 {
		t.Fatalf("Email settings = %#v", settings.Email)
	}
	if settings.Email.SMTPUsername != "smtp-user" || settings.Email.SMTPPassword != "smtp-secret" || settings.Email.FromEmail != "noreply@example.com" || settings.Email.FromName != "AI Gateway" {
		t.Fatalf("Email SMTP settings = %#v", settings.Email)
	}
	if settings.Email.CodeTTLSeconds != 900 || settings.Email.SendCooldownSeconds != 90 || settings.Email.HourlySendLimit != 8 || settings.Email.MaxAttempts != 4 {
		t.Fatalf("Email verification limits = %#v", settings.Email)
	}
	if settings.Home.BrandTitle != "Custom Gateway" || settings.Home.BrandSubtitle != "Custom homepage subtitle" {
		t.Fatalf("Home brand settings = %#v", settings.Home)
	}
	if settings.Home.Heading != "Custom heading" || settings.Home.Summary != "Custom summary\nSecond line" {
		t.Fatalf("Home copy settings = %#v", settings.Home)
	}
	if settings.Home.AvailabilityKey != "Uptime" || settings.Home.Availability != "99.99%" || settings.Home.CostKey != "Rate" || settings.Home.CostMultiplier != "≤0.2x" || settings.Home.LatencyKey != "Delay" || settings.Home.Latency != "≤0.10s" || settings.Home.CapabilityKey != "Quality" || settings.Home.Capability != "No downgrade" {
		t.Fatalf("Home metric settings = %#v", settings.Home)
	}
	if !settings.Invitation.Enabled || settings.Invitation.InviterRechargeRewardBps != 500 || settings.Invitation.InviteeRewardNanoUSD != 1250000000 {
		t.Fatalf("Invitation settings = %#v", settings.Invitation)
	}
	if !settings.UserDashboard.CustomPanelEnabled || settings.UserDashboard.CustomPanelHTML != `<strong>Console notice</strong>` {
		t.Fatalf("User dashboard settings = %#v", settings.UserDashboard)
	}
	if settings.Network.SystemProxyURL != "http://127.0.0.1:7890" {
		t.Fatalf("SystemProxyURL = %q", settings.Network.SystemProxyURL)
	}
	if settings.Image.Backend != core.ImageBackendOfficial || !core.ImageUserConsoleEnabled(settings.Image)  {
		t.Fatalf("Image settings = %#v", settings.Image)
	}
	if settings.Payment.MinRechargeNanoUSD != 2500000000 || settings.Payment.MaxRechargeNanoUSD != 200*core.NanoUSDPerUSD {
		t.Fatalf("Payment recharge limits = %#v", settings.Payment)
	}
	if !settings.Payment.WeChatPay.Enabled || settings.Payment.WeChatPay.AppID != "wx_app" || settings.Payment.WeChatPay.MchID != "mch_1" {
		t.Fatalf("WeChatPay settings = %#v", settings.Payment.WeChatPay)
	}
	if settings.Payment.WeChatPay.NotifyURL != "" {
		t.Fatalf("WeChatPay NotifyURL = %q", settings.Payment.WeChatPay.NotifyURL)
	}
	if settings.Payment.WeChatPay.WeChatPayPublicKeyID != "PUB_KEY_ID_1" {
		t.Fatalf("WeChatPay PublicKeyID = %q", settings.Payment.WeChatPay.WeChatPayPublicKeyID)
	}
	if !settings.Payment.Alipay.Enabled || settings.Payment.Alipay.AppID != "ali_app" || settings.Payment.Alipay.SignType != "RSA2" {
		t.Fatalf("Alipay settings = %#v", settings.Payment.Alipay)
	}
	if settings.Payment.Alipay.NotifyURL != "" || settings.Payment.Alipay.ReturnURL != "https://gateway.example.com/payments/return/alipay" {
		t.Fatalf("Alipay callback settings = %#v", settings.Payment.Alipay)
	}
	homeReq := httptest.NewRequest(http.MethodGet, "/", nil)
	homeRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(homeRec, homeReq)
	if homeRec.Code != http.StatusOK {
		t.Fatalf("home status = %d, want %d body=%s", homeRec.Code, http.StatusOK, homeRec.Body.String())
	}
	homeBody := homeRec.Body.String()
	for _, want := range []string{"Custom Gateway", "Custom homepage subtitle", "Custom heading", "Custom summary\nSecond line", "Get Key", "Create account", "Uptime", "99.99%", "Rate", "≤0.2x", "Delay", "≤0.10s", "Quality", "No downgrade"} {
		if !strings.Contains(homeBody, want) {
			t.Fatalf("home missing configured value %q: %s", want, homeBody)
		}
	}

	accountsReq := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	accountsRec := httptest.NewRecorder()
	handler.ServeHTTP(accountsRec, accountsReq)
	if accountsRec.Code != http.StatusOK {
		t.Fatalf("accounts status = %d, want %d body=%s", accountsRec.Code, http.StatusOK, accountsRec.Body.String())
	}
	for _, want := range []string{`<div class="brand-title">Custom Gateway</div>`, `<div class="brand-subtitle">Custom homepage subtitle</div>`} {
		if !strings.Contains(accountsRec.Body.String(), want) {
			t.Fatalf("accounts page header missing configured brand value %q: %s", want, accountsRec.Body.String())
		}
	}
}

func TestPreserveBlankLoginOAuthSecrets(t *testing.T) {
	input := core.SystemSettings{
		OAuth: core.SystemOAuthSettings{
			GitHubLoginSecret: "",
			GoogleLoginSecret: "next-google",
		},
		Email: core.SystemEmailSettings{
			SMTPPassword:       "",
			CloudMailPassword:  "",
			CloudMailAccountID: 2,
		},
		Registration: core.SystemRegistrationSettings{
			TurnstileSecretKey: "",
		},
		Payment: core.SystemPaymentSettings{
			WeChatPay: core.WeChatPaySettings{
				APIV3Key:              "",
				MerchantPrivateKeyPEM: "",
			},
			Alipay: core.AlipaySettings{
				PrivateKeyPEM: "",
			},
		},
	}
	existing := core.SystemSettings{
		OAuth: core.SystemOAuthSettings{
			GitHubLoginSecret: "current-github",
			GoogleLoginSecret: "current-google",
		},
		Email: core.SystemEmailSettings{
			SMTPPassword:       "current-smtp",
			CloudMailPassword:  "current-cloudmail",
			CloudMailAccountID: 1,
		},
		Registration: core.SystemRegistrationSettings{
			TurnstileSecretKey: "current-turnstile",
		},
		Payment: core.SystemPaymentSettings{
			WeChatPay: core.WeChatPaySettings{
				APIV3Key:              "current-wechat-key",
				MerchantPrivateKeyPEM: "current-wechat-private",
			},
			Alipay: core.AlipaySettings{
				PrivateKeyPEM: "current-alipay-private",
			},
		},
	}

	preserveBlankLoginOAuthSecrets(&input, existing)

	if input.OAuth.GitHubLoginSecret != "current-github" {
		t.Fatalf("GitHubLoginSecret = %q", input.OAuth.GitHubLoginSecret)
	}
	if input.OAuth.GoogleLoginSecret != "next-google" {
		t.Fatalf("GoogleLoginSecret = %q", input.OAuth.GoogleLoginSecret)
	}
	if input.Email.SMTPPassword != "current-smtp" {
		t.Fatalf("SMTPPassword = %q", input.Email.SMTPPassword)
	}
	if input.Email.CloudMailPassword != "current-cloudmail" {
		t.Fatalf("CloudMailPassword = %q", input.Email.CloudMailPassword)
	}
	if input.Email.CloudMailAccountID != 1 {
		t.Fatalf("CloudMailAccountID = %d", input.Email.CloudMailAccountID)
	}
	if input.Registration.TurnstileSecretKey != "current-turnstile" {
		t.Fatalf("TurnstileSecretKey = %q", input.Registration.TurnstileSecretKey)
	}
	if input.Payment.WeChatPay.APIV3Key != "current-wechat-key" {
		t.Fatalf("WeChatPay.APIV3Key = %q", input.Payment.WeChatPay.APIV3Key)
	}
	if input.Payment.WeChatPay.MerchantPrivateKeyPEM != "current-wechat-private" {
		t.Fatalf("WeChatPay.MerchantPrivateKeyPEM = %q", input.Payment.WeChatPay.MerchantPrivateKeyPEM)
	}
	if input.Payment.Alipay.PrivateKeyPEM != "current-alipay-private" {
		t.Fatalf("Alipay.PrivateKeyPEM = %q", input.Payment.Alipay.PrivateKeyPEM)
	}
}

func TestSettingsPageUpdatesCloudMailEmailSettings(t *testing.T) {
	repo := storage.NewMemoryRepository()
	existing := core.DefaultSystemSettings()
	existing.Email.SMTPHost = "smtp.example.com"
	existing.Email.SMTPPort = 465
	existing.Email.SMTPUsername = "smtp-user"
	existing.Email.SMTPPassword = "smtp-secret"
	existing.Email.FromEmail = "noreply@example.com"
	if err := repo.UpsertSystemSettings(existing); err != nil {
		t.Fatalf("UpsertSystemSettings returned error: %v", err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/settings", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	csrf := extractCSRFToken(t, rec.Body.String())

	form := url.Values{}
	form.Set("csrf_token", csrf)
	form.Set("email_verify_on_register", "on")
	form.Set("email_provider", "cloudmail")
	form.Set("cloudmail_base_url", " https://mail.example.com/ ")
	form.Set("cloudmail_email", " Mail@Example.COM ")
	form.Set("cloudmail_password", "cloudmail-secret")
	form.Set("cloudmail_account_id", "2")
	form.Set("email_code_ttl_seconds", "600")
	form.Set("email_send_cooldown_seconds", "60")
	form.Set("email_hourly_send_limit", "5")
	form.Set("email_max_attempts", "5")
	postReq := httptest.NewRequest(http.MethodPost, "/admin/settings", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range rec.Result().Cookies() {
		postReq.AddCookie(cookie)
	}
	postRec := httptest.NewRecorder()
	handler.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", postRec.Code, http.StatusSeeOther, postRec.Body.String())
	}
	if got := postRec.Header().Get("Location"); got != "/admin/settings?saved=1" {
		t.Fatalf("settings save redirect location = %q", got)
	}
	settings, err := repo.GetSystemSettings()
	if err != nil {
		t.Fatalf("GetSystemSettings returned error: %v", err)
	}
	if !settings.Email.RegistrationVerificationEnabled || settings.Email.Provider != core.EmailProviderCloudMail {
		t.Fatalf("email provider settings = %#v", settings.Email)
	}
	if settings.Email.CloudMailBaseURL != "https://mail.example.com" || settings.Email.CloudMailEmail != "mail@example.com" || settings.Email.CloudMailPassword != "cloudmail-secret" || settings.Email.CloudMailAccountID != 2 {
		t.Fatalf("CloudMail settings = %#v", settings.Email)
	}
	if settings.Email.SMTPHost != "smtp.example.com" || settings.Email.SMTPPort != 465 || settings.Email.SMTPUsername != "smtp-user" || settings.Email.SMTPPassword != "smtp-secret" || settings.Email.FromEmail != "noreply@example.com" {
		t.Fatalf("preserved SMTP settings = %#v", settings.Email)
	}
}

func TestSettingsEmailTestUsesSubmittedCloudMailSettings(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	loginCount := 0
	accountListCount := 0
	sendCount := 0
	cloudmail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/login":
			loginCount++
			var payload struct {
				Email    string `json:"email"`
				Password string `json:"password"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode login payload: %v", err)
			}
			if payload.Email != "mail@example.com" || payload.Password != "secret" {
				t.Errorf("login payload = %+v", payload)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 200,
				"data": map[string]any{"token": "token-123"},
			})
		case "/api/account/list":
			accountListCount++
			if got := r.Header.Get("Authorization"); got != "token-123" {
				t.Errorf("Authorization = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 200,
				"data": []map[string]any{
					{"accountId": 2, "email": "mail@example.com"},
				},
			})
		case "/api/email/send":
			sendCount++
			if got := r.Header.Get("Authorization"); got != "token-123" {
				t.Errorf("Authorization = %q", got)
			}
			var payload struct {
				AccountID    int      `json:"accountId"`
				ReceiveEmail []string `json:"receiveEmail"`
				Subject      string   `json:"subject"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode send payload: %v", err)
			}
			if payload.AccountID != 2 || len(payload.ReceiveEmail) != 1 || payload.ReceiveEmail[0] != "alice@example.com" || payload.Subject == "" {
				t.Errorf("send payload = %+v", payload)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200})
		default:
			http.NotFound(w, r)
		}
	}))
	defer cloudmail.Close()

	req := httptest.NewRequest(http.MethodGet, "/admin/settings", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	csrf := extractCSRFToken(t, rec.Body.String())

	form := url.Values{}
	form.Set("csrf_token", csrf)
	form.Set("email_provider", "cloudmail")
	form.Set("cloudmail_base_url", cloudmail.URL)
	form.Set("cloudmail_email", "mail@example.com")
	form.Set("cloudmail_password", "secret")
	form.Set("cloudmail_account_id", "2")
	form.Set("smtp_from_name", "AI Gateway")
	form.Set("email_code_ttl_seconds", "600")
	form.Set("email_send_cooldown_seconds", "60")
	form.Set("email_hourly_send_limit", "5")
	form.Set("email_max_attempts", "5")
	form.Set("test_email_to", "Alice@Example.COM")
	postReq := httptest.NewRequest(http.MethodPost, "/admin/email-test", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range rec.Result().Cookies() {
		postReq.AddCookie(cookie)
	}
	postRec := httptest.NewRecorder()
	handler.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", postRec.Code, http.StatusOK, postRec.Body.String())
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(postRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.Status != "ok" {
		t.Fatalf("payload status = %q", payload.Status)
	}
	if loginCount != 1 || accountListCount != 1 || sendCount != 1 {
		t.Fatalf("loginCount=%d accountListCount=%d sendCount=%d", loginCount, accountListCount, sendCount)
	}
	settings, err := repo.GetSystemSettings()
	if err != nil {
		t.Fatalf("GetSystemSettings returned error: %v", err)
	}
	if settings.Email.Provider == core.EmailProviderCloudMail {
		t.Fatalf("email test should not save submitted settings")
	}
}

func TestPaymentsPageScopesOrdersAndQRCode(t *testing.T) {
	repo := storage.NewMemoryRepository()
	alice := core.User{ID: "alice", Username: "alice", Role: core.UserRoleUser, Enabled: true}
	bob := core.User{ID: "bob", Username: "bob", Role: core.UserRoleUser, Enabled: true}
	for _, user := range []core.User{alice, bob} {
		if err := repo.UpsertUser(user); err != nil {
			t.Fatal(err)
		}
	}
	aliceOrder := core.PaymentOrder{
		ID:                    "pay_alice",
		OutTradeNo:            "out_alice",
		UserID:                alice.ID,
		Provider:              core.PaymentProviderWeChatPay,
		Channel:               core.PaymentChannelNative,
		AmountNanoUSD:         core.NanoUSDPerUSD,
		Currency:              "USD",
		ProviderAmountCents:   720,
		ProviderCurrency:      "CNY",
		ExchangeRateCNYPerUSD: "7.20",
		Status:                core.PaymentOrderPending,
		CodeURL:               "weixin://wxpay/bizpayurl?pr=alice",
		CreatedAt:             time.Now().UTC(),
	}
	bobOrder := aliceOrder
	bobOrder.ID = "pay_bob"
	bobOrder.OutTradeNo = "out_bob"
	bobOrder.UserID = bob.ID
	bobOrder.CodeURL = "weixin://wxpay/bizpayurl?pr=bob"
	paidAliceOrder := aliceOrder
	paidAliceOrder.ID = "pay_alice_paid"
	paidAliceOrder.OutTradeNo = "out_alice_paid"
	paidAliceOrder.Status = core.PaymentOrderPaid
	paidAliceOrder.CodeURL = "weixin://wxpay/bizpayurl?pr=paid"
	paidAliceOrder.ProviderTradeNo = "trade_paid"
	if err := repo.CreatePaymentOrder(aliceOrder); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreatePaymentOrder(bobOrder); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreatePaymentOrder(paidAliceOrder); err != nil {
		t.Fatal(err)
	}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, alice, server.Handler())

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/payments", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("payments status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "out_alice") || strings.Contains(body, "out_bob") {
		t.Fatalf("payment page scope mismatch: %s", body)
	}
	if !strings.Contains(body, "out_alice_paid") {
		t.Fatalf("paid payment order missing from page: %s", body)
	}
	if strings.Contains(body, "payment-qr-pay_alice_paid") || strings.Contains(body, "pr=paid") {
		t.Fatalf("paid payment order should not render qr action: %s", body)
	}
	if strings.Contains(body, "Show QR Code") {
		t.Fatalf("pending qr dialog should use payment wording: %s", body)
	}
	if !strings.Contains(body, `data-payment-order-modal`) || !strings.Contains(body, `data-payment-order-qr`) {
		t.Fatalf("pending qr dialog should support live qr refresh: %s", body)
	}
	if !strings.Contains(body, "Payment Method") {
		t.Fatalf("payment page should show payment method labels: %s", body)
	}
	if !strings.Contains(body, "WeChat Pay") {
		t.Fatalf("pending qr dialog should render selected payment method: %s", body)
	}
	if !strings.Contains(body, "Exchange Rate") || !strings.Contains(body, "$1 = ¥7.20 / ¥1 = $0.1389") {
		t.Fatalf("payment page should render order exchange rate: %s", body)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/payments/qr?id=pay_alice", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" || rec.Body.Len() == 0 {
		t.Fatalf("qr response status=%d content-type=%q len=%d", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/payments/status?id=pay_alice", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("payment status response status=%d body=%s", rec.Code, rec.Body.String())
	}
	var statusPayload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusPayload); err != nil {
		t.Fatalf("decode payment status: %v", err)
	}
	if statusPayload["has_code_url"] != true || strings.TrimSpace(fmt.Sprint(statusPayload["code_version"])) == "" {
		t.Fatalf("payment status should expose qr version: %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/payments/qr?id=pay_bob", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign qr status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/payments/qr?id=pay_alice_paid", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("paid qr status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	req := httptest.NewRequest(http.MethodPost, "/payments/cancel", strings.NewReader(`{"id":"pay_alice"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", testConsoleCSRFToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"deleted":false`) || !strings.Contains(rec.Body.String(), `"order_status":"pending"`) {
		t.Fatalf("cancel response status=%d body=%s", rec.Code, rec.Body.String())
	}
	if order, err := repo.GetPaymentOrder("pay_alice"); err != nil || order.Status != core.PaymentOrderPending {
		t.Fatalf("cancelled order=%#v err=%v, want pending order", order, err)
	}
}

func TestPaymentRefreshErrorRedirectsWithNotice(t *testing.T) {
	repo := storage.NewMemoryRepository()
	alice := core.User{ID: "alice", Username: "alice", Role: core.UserRoleUser, Enabled: true}
	if err := repo.UpsertUser(alice); err != nil {
		t.Fatal(err)
	}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, alice, server.Handler())

	form := url.Values{}
	form.Set("id", "missing_order")
	req := httptest.NewRequest(http.MethodPost, "/payments/refresh", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/payments?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("location = %q, want payments notice redirect", location)
	}
	noticeReq := httptest.NewRequest(http.MethodGet, location, nil)
	noticeRec := httptest.NewRecorder()
	handler.ServeHTTP(noticeRec, noticeReq)
	if noticeRec.Code != http.StatusOK {
		t.Fatalf("notice status = %d, want %d", noticeRec.Code, http.StatusOK)
	}
	if !strings.Contains(noticeRec.Body.String(), `data-clear-url-params="notice_error"`) {
		t.Fatalf("notice missing refresh error: %s", noticeRec.Body.String())
	}
}

func TestPaymentReturnRedirectsToPaymentsWithNotice(t *testing.T) {
	repo := storage.NewMemoryRepository()
	alice := core.User{ID: "alice", Username: "alice", Role: core.UserRoleUser, Enabled: true}
	if err := repo.UpsertUser(alice); err != nil {
		t.Fatal(err)
	}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, alice, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/payments/return/alipay?out_trade_no=pay_return", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/payments?") || !strings.Contains(location, "notice=payment_return") || !strings.Contains(location, "order_id=pay_return") {
		t.Fatalf("location = %q, want payment return notice", location)
	}

	noticeReq := httptest.NewRequest(http.MethodGet, location, nil)
	noticeRec := httptest.NewRecorder()
	handler.ServeHTTP(noticeRec, noticeReq)
	if noticeRec.Code != http.StatusOK {
		t.Fatalf("notice status = %d, want %d body=%s", noticeRec.Code, http.StatusOK, noticeRec.Body.String())
	}
	if body := noticeRec.Body.String(); !strings.Contains(body, "Returned from payment provider.") || !strings.Contains(body, `data-clear-url-params="notice,order_id"`) {
		t.Fatalf("payment return notice missing: %s", body)
	}
}

func TestPaymentNotifyRejectsOversizedBody(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()
	body := strings.Repeat("x", paymentNotifyBodyLimit+1)

	for _, tc := range []struct {
		name        string
		path        string
		contentType string
	}{
		{name: "alipay", path: "/payments/notify/alipay", contentType: "application/x-www-form-urlencoded"},
		{name: "wechatpay", path: "/payments/notify/wechatpay", contentType: "application/json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
			req.Header.Set("Content-Type", tc.contentType)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "request body too large") {
				t.Fatalf("body = %q, want body limit error", rec.Body.String())
			}
		})
	}
}

func TestDashboardShowsConfiguredPaymentProviders(t *testing.T) {
	repo := storage.NewMemoryRepository()
	user := core.User{ID: "pay_user", Username: "pay-user", Role: core.UserRoleUser, Enabled: true}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatal(err)
	}
	settings := core.DefaultSystemSettings()
	settings.Payment.WeChatPay.Enabled = true
	settings.Payment.Alipay.Enabled = true
	settings.UserDashboard.CustomPanelEnabled = true
	settings.UserDashboard.CustomPanelHTML = `<strong>Console notice</strong>`
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`name="provider" value="wechatpay"`, `name="provider" value="alipay"`, "/static/alipay-logo.png", "/static/wechat-pay-logo.png", `class="metric user-dashboard-custom-panel"`, `<strong>Console notice</strong>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q: %s", want, body)
		}
	}
}

func TestAdminDashboardShowsUsersIncomeAndPersonalPay(t *testing.T) {
	repo := storage.NewMemoryRepository()
	admin := core.User{ID: "admin_dashboard", Username: "admin-dashboard", Role: core.UserRoleAdmin, Enabled: true}
	user := core.User{ID: "user_dashboard", Username: "user-dashboard", Role: core.UserRoleUser, Enabled: true}
	if err := repo.UpsertUser(admin); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_dashboard", Name: "Dashboard Client", APIKey: "gw_dashboard", OwnerUserID: user.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	settings := core.DefaultSystemSettings()
	settings.Payment.PersonalPay.Enabled = true
	settings.Payment.PersonalPay.AndroidToken = "android-token"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := repo.CreatePaymentOrder(core.PaymentOrder{
		ID:            "pay_dashboard",
		OutTradeNo:    "trade_dashboard",
		UserID:        user.ID,
		Provider:      core.PaymentProviderPersonalPay,
		Channel:       core.PaymentChannelWeChat,
		AmountNanoUSD: 7 * core.NanoUSDPerUSD,
		Status:        core.PaymentOrderPaid,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatal(err)
	}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	control.SetPaymentClient(&fakeSettingsPaymentClient{
		runtime: payments.PersonalPayRuntime{
			Accounts: []personalpay.Account{
				{ID: "idle_wechat", Channel: personalpay.ChannelWeChat, Status: personalpay.AccountIdle},
				{ID: "occupied_wechat", Channel: personalpay.ChannelWeChat, Status: personalpay.AccountOccupied},
			},
			Summary: payments.PersonalPayRuntimeSummary{DeviceCount: 1, AccountCount: 2, IdleAccountCount: 1, OccupiedCount: 1},
		},
	})
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Users", "Today&#39;s Income", "$7.00", "PersonalPay", "<strong>2</strong>", "Idle Accounts: 1", "Occupied Accounts: 1", "Android Devices: 1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q: %s", want, body)
		}
	}
	for _, unwanted := range []string{"Manage accounts, routing, and audit flow from a single local service.", "API Keys", "Expiring Soon"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("dashboard should not contain %q: %s", unwanted, body)
		}
	}
}

func TestPaymentCreateUnauthenticatedFetchReturnsJSON(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodPost, "/payments/create", strings.NewReader(`{"provider":"alipay","epay_type":"","amount_usd":"1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") || !strings.Contains(rec.Body.String(), "Session expired") {
		t.Fatalf("unauthenticated payment fetch should return JSON notice: content-type=%q body=%s", rec.Header().Get("Content-Type"), rec.Body.String())
	}
}

func TestPaymentCreateUsesCNYRechargeInputMode(t *testing.T) {
	repo := storage.NewMemoryRepository()
	user := core.User{ID: "pay_cny_user", Username: "pay-cny", Role: core.UserRoleUser, Enabled: true}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatal(err)
	}
	settings := core.DefaultSystemSettings()
	settings.Payment.CNYPerUSD = "0.1"
	settings.Payment.RechargeInputMode = core.RechargeInputModePaymentCNY
	settings.Payment.PersonalPay.Enabled = true
	settings.Payment.PersonalPay.AndroidToken = "android-token"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	control.SetPaymentClient(&fakeSettingsPaymentClient{})
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/payments/create", strings.NewReader(`{"provider":"personalpay","amount_usd":"1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-CSRF-Token", testConsoleCSRFToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		Status string            `json:"status"`
		Mode   string            `json:"mode"`
		Order  core.PaymentOrder `json:"order"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payment response: %v body=%s", err, rec.Body.String())
	}
	if payload.Status != "ok" || payload.Mode != core.RechargeInputModePaymentCNY {
		t.Fatalf("payload status/mode = %#v", payload)
	}
	if payload.Order.ProviderAmountCents != 100 {
		t.Fatalf("provider amount cents = %d, want 100", payload.Order.ProviderAmountCents)
	}
	if payload.Order.AmountNanoUSD != 10*core.NanoUSDPerUSD {
		t.Fatalf("amount nano usd = %d, want %d", payload.Order.AmountNanoUSD, 10*core.NanoUSDPerUSD)
	}
	if payload.Order.ExchangeRateCNYPerUSD != "0.1" {
		t.Fatalf("exchange rate = %q, want 0.1", payload.Order.ExchangeRateCNYPerUSD)
	}
}

func TestPaymentCreateReturnsRechargeLimitError(t *testing.T) {
	repo := storage.NewMemoryRepository()
	user := core.User{ID: "pay_limit_user", Username: "pay-limit", Role: core.UserRoleUser, Enabled: true}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatal(err)
	}
	settings := core.DefaultSystemSettings()
	settings.Payment.MinRechargeNanoUSD = 2 * core.NanoUSDPerUSD
	settings.Payment.MaxRechargeNanoUSD = 5 * core.NanoUSDPerUSD
	settings.Payment.PersonalPay.Enabled = true
	settings.Payment.PersonalPay.AndroidToken = "android-token"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	control.SetPaymentClient(&fakeSettingsPaymentClient{})
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/payments/create", strings.NewReader(`{"provider":"personalpay","amount_usd":"1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-CSRF-Token", testConsoleCSRFToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "below the minimum") {
		t.Fatalf("payment limit error should be specific: %s", rec.Body.String())
	}
}

func TestFinancePageRendersOverview(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	if err := repo.SetUserBalance(admin.ID, 25*core.NanoUSDPerUSD); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:          "client_finance",
		Name:        "Finance Client",
		APIKey:      "gw_finance",
		OwnerUserID: admin.ID,
		Enabled:     true,
	}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	now := time.Now().UTC()
	if err := repo.CreatePaymentOrder(core.PaymentOrder{
		ID:            "pay_finance",
		OutTradeNo:    "trade_finance",
		UserID:        admin.ID,
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: 5 * core.NanoUSDPerUSD,
		Status:        core.PaymentOrderPaid,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}
	if err := repo.CreatePaymentOrder(core.PaymentOrder{
		ID:            "pay_finance_closed",
		OutTradeNo:    "trade_finance_closed",
		UserID:        admin.ID,
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: 5 * core.NanoUSDPerUSD,
		Status:        core.PaymentOrderClosed,
		CreatedAt:     now.Add(time.Second),
		UpdatedAt:     now.Add(time.Second),
	}); err != nil {
		t.Fatalf("CreatePaymentOrder closed returned error: %v", err)
	}
	if _, _, err := repo.AdjustUserBalance(admin.ID, -2*core.NanoUSDPerUSD, "finance adjustment"); err != nil {
		t.Fatalf("AdjustUserBalance returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/admin/finance", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `data-deferred-url="/admin/finance?partial=finance-page"`) {
		t.Fatalf("finance page should not render an empty deferred shell: %s", body)
	}
	for _, want := range []string{"Total Balance", "Balance Spend", "Plan Spend", "Daily Report", "Model Usage", "Payment Orders", "Usage Charges", "Reconciliation"} {
		if !strings.Contains(body, want) {
			t.Fatalf("finance page missing %q: %s", want, body)
		}
	}
	for _, unexpected := range []string{"trade_finance"} {
		if strings.Contains(body, unexpected) {
			t.Fatalf("overview tab should not render %q: %s", unexpected, body)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/finance?partial=finance-page", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("partial status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = rec.Body.String()
	for _, want := range []string{"Total Balance", "Daily Report", "Model Usage", "Payment Orders", "Usage Charges", "Reconciliation"} {
		if !strings.Contains(body, want) {
			t.Fatalf("finance partial missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "trade_finance") {
		t.Fatalf("overview tab should not render payment order rows: %s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/finance?tab=orders", nil)
	req.Header.Set("X-Requested-With", "fetch")
	req.Header.Set(ajaxPartialHeader, "finance-page")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("orders status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = rec.Body.String()
	if strings.Contains(body, "<!DOCTYPE html>") || strings.Contains(body, `data-deferred-url="/admin/finance?tab=orders&amp;partial=finance-page"`) {
		t.Fatalf("orders ajax partial should render finance page directly: %s", body)
	}
	if !strings.Contains(body, "trade_finance") {
		t.Fatalf("orders tab missing payment row: %s", body)
	}
	if strings.Contains(body, "trade_finance_closed") {
		t.Fatalf("orders tab should default to paid orders only: %s", body)
	}
	if !strings.Contains(body, `value="paid" selected`) {
		t.Fatalf("orders tab should select paid status by default: %s", body)
	}
	if !strings.Contains(body, ">admin</strong>") || strings.Contains(body, `<code>`+admin.ID+`</code>`) {
		t.Fatalf("orders tab should render username instead of visible user id: %s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/finance?tab=orders&order_status=", nil)
	req.Header.Set("X-Requested-With", "fetch")
	req.Header.Set(ajaxPartialHeader, "finance-page")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("orders all status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = rec.Body.String()
	if !strings.Contains(body, "trade_finance") || !strings.Contains(body, "trade_finance_closed") {
		t.Fatalf("orders tab with explicit empty status should render all non-pending statuses: %s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/finance?tab=orders&order_user_id="+url.QueryEscape(admin.ID), nil)
	req.Header.Set("X-Requested-With", "fetch")
	req.Header.Set(ajaxPartialHeader, "finance-page")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("orders filter status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `name="order_user_id" value="admin"`) || strings.Contains(body, `name="order_user_id" value="`+admin.ID+`"`) {
		t.Fatalf("orders user filter should echo username, not user id: %s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/finance?tab=reconcile&notice=reconcile_release_ok", nil)
	req.Header.Set("X-Requested-With", "fetch")
	req.Header.Set(ajaxPartialHeader, "finance-page")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reconcile notice status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `data-clear-url-params="notice"`) {
		t.Fatalf("finance ajax partial should preserve release notice: %s", body)
	}

}

func TestFinanceAdjustUpdatesBalance(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	if err := repo.SetUserBalance(admin.ID, 10*core.NanoUSDPerUSD); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("user_id", admin.ID)
	form.Set("direction", "debit")
	form.Set("amount_usd", "3.25")
	form.Set("reason", "finance correction")
	req := httptest.NewRequest(http.MethodPost, "/admin/finance/adjust", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/finance" {
		t.Fatalf("status=%d location=%q body=%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	updated, err := repo.GetUser(admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(675 * core.NanoUSDPerUSD / 100); updated.BalanceNanoUSD != want {
		t.Fatalf("balance = %d, want %d", updated.BalanceNanoUSD, want)
	}
	ledger := repo.ListBillingLedger(admin.ID, 1)
	if len(ledger) != 1 || ledger[0].Kind != "manual_debit" || ledger[0].Note != "finance correction" {
		t.Fatalf("ledger = %#v", ledger)
	}
}

func TestFinanceReleaseReservedBillingRequest(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	startBalance := int64(10 * core.NanoUSDPerUSD)
	reserved := int64(2 * core.NanoUSDPerUSD)
	if err := repo.SetUserBalance(admin.ID, startBalance); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:          "client_reserved_release",
		Name:        "Reserved Release",
		APIKey:      "gw_reserved_release",
		OwnerUserID: admin.ID,
		Enabled:     true,
	}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_reserved_release",
		ClientID:        "client_reserved_release",
		UserID:          admin.ID,
		Model:           "gpt-4.1",
		ReservedNanoUSD: reserved,
		Fingerprint:     "req_reserved_release",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("request_id", "req_reserved_release")
	form.Set("client_id", "client_reserved_release")
	req := httptest.NewRequest(http.MethodPost, "/admin/finance/reconcile/release", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/finance?tab=reconcile&notice=reconcile_release_ok" {
		t.Fatalf("status=%d location=%q body=%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	updated, err := repo.GetUser(admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.BalanceNanoUSD != startBalance {
		t.Fatalf("balance = %d, want %d", updated.BalanceNanoUSD, startBalance)
	}
	requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{Status: core.BillingRequestReleased, Limit: 10})
	if total != 1 || len(requests) != 1 || requests[0].RequestID != "req_reserved_release" {
		t.Fatalf("released requests total=%d requests=%#v", total, requests)
	}
	ledger := repo.ListBillingLedger(admin.ID, 1)
	if len(ledger) != 0 {
		t.Fatalf("ledger = %#v, want no refund ledger for pending request cancellation", ledger)
	}
}

func TestFinanceExportDownloadsCSV(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	if err := repo.SetUserBalance(admin.ID, 12*core.NanoUSDPerUSD); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/admin/finance/export", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/csv") {
		t.Fatalf("content-type = %q, want csv", got)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "finance-users-") || !strings.Contains(got, ".csv") {
		t.Fatalf("content-disposition = %q", got)
	}
	body := rec.Body.String()
	for _, want := range []string{"user_id,username,role,enabled,balance_usd", admin.ID, "admin", "12"} {
		if !strings.Contains(body, want) {
			t.Fatalf("finance export missing %q: %s", want, body)
		}
	}
}

func TestAdminBackupPageAndExport(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	statePath := filepath.Join(dir, "state.db")
	if err := os.WriteFile(configPath, []byte(`{"port":"8088"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sqliteRepo, err := storage.NewSQLiteRepository(statePath, "test-master-key")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqliteRepo.Close() })
	if err := sqliteRepo.UpsertSystemSettings(core.DefaultSystemSettings()); err != nil {
		t.Fatal(err)
	}
	if err := sqliteRepo.UpsertUser(core.User{ID: "user_backup", Username: "backup", Role: core.UserRoleUser, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServerWithOptions(control, nil, ServerOptions{
		ConfigPath: configPath,
		StatePath:  statePath,
	})
	handler := authenticatedAdminHandler(t, control, server.Handler())

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/backup", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/settings#settings-backup" {
		t.Fatalf("backup page status=%d location=%q body=%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("settings backup tab status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="backup_data" value="settings"`) || !strings.Contains(body, `name="restore_data" value="settings"`) || !strings.Contains(body, `name="backup_data" value="users"`) || !strings.Contains(body, `name="backup_master_key"`) || !strings.Contains(body, `data-backup-restore-form`) || strings.Contains(body, configPath) {
		t.Fatalf("settings backup tab missing logical data options or leaked paths: %s", body)
	}
	if strings.Contains(body, "Restart the service") || strings.Contains(body, "重启服务") {
		t.Fatalf("backup restore panel should not ask for service restart: %s", body)
	}

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Add("backup_data", "settings")
	form.Add("backup_data", "users")
	req := httptest.NewRequest(http.MethodPost, "/admin/backup/export", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Disposition"), ".agbak") {
		t.Fatalf("backup export status=%d disposition=%q", rec.Code, rec.Header().Get("Content-Disposition"))
	}
	manifest, err := backup.Inspect(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("inspect exported backup: %v", err)
	}
	if !manifest.Includes.Data || manifest.Includes.Database || manifest.Includes.Config || len(manifest.Includes.DataSets) != 2 || manifest.Includes.DataSets[0] != "settings" || manifest.Includes.DataSets[1] != "users" {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestAdminBackupInspectReportsEncryptedData(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")
	sqliteRepo, err := storage.NewSQLiteRepository(statePath, "source-key")
	if err != nil {
		t.Fatal(err)
	}
	settings := core.DefaultSystemSettings()
	settings.Email.SMTPPassword = "smtp-secret"
	if err := sqliteRepo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	if err := sqliteRepo.Close(); err != nil {
		t.Fatal(err)
	}

	var backupBody bytes.Buffer
	if _, err := backup.Write(&backupBody, backup.Options{StatePath: statePath, DataSets: []string{backup.DataSetSettings}}); err != nil {
		t.Fatal(err)
	}
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServerWithOptions(control, nil, ServerOptions{StatePath: statePath, MasterKey: "source-key"})
	handler := authenticatedAdminHandler(t, control, server.Handler())

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("csrf_token", testConsoleCSRFToken); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("restore_data", "settings"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("backup", "backup.agbak")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(backupBody.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/backup/inspect", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("inspect status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"encrypted":true`) {
		t.Fatalf("inspect body = %s, want encrypted true", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"requires_source_master_key":false`) {
		t.Fatalf("inspect body = %s, want current master key to work", rec.Body.String())
	}
}

func TestAdminBackupRestorePassesSelectedDataSets(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	statePath := filepath.Join(dir, "state.db")
	if err := os.WriteFile(configPath, []byte(`{"port":"8088"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	var gotOptions backup.Options
	server := NewServerWithOptions(control, nil, ServerOptions{
		ConfigPath: configPath,
		StatePath:  statePath,
		RestoreBackup: func(path string, opts backup.Options, preRestoreDir string) (string, error) {
			gotOptions = opts
			return filepath.Join(preRestoreDir, "pre.agbak"), nil
		},
	})
	handler := authenticatedAdminHandler(t, control, server.Handler())

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("csrf_token", testConsoleCSRFToken); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("confirm", "RESTORE"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("restore_data", "settings"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("restore_data", "users"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("backup_master_key", "source-key"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("backup", "backup.agbak")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("placeholder")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/backup/restore", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("restore status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(gotOptions.DataSets) != 2 || gotOptions.DataSets[0] != "settings" || gotOptions.DataSets[1] != "users" {
		t.Fatalf("restore data sets = %#v", gotOptions.DataSets)
	}
	if gotOptions.SourceMasterKey != "source-key" {
		t.Fatalf("source master key = %q, want source-key", gotOptions.SourceMasterKey)
	}

	noticeReq := httptest.NewRequest(http.MethodGet, rec.Header().Get("Location"), nil)
	noticeRec := httptest.NewRecorder()
	handler.ServeHTTP(noticeRec, noticeReq)
	if noticeRec.Code != http.StatusOK {
		t.Fatalf("restore notice status = %d body=%s", noticeRec.Code, noticeRec.Body.String())
	}
	if body := noticeRec.Body.String(); !strings.Contains(body, `data-clear-url-params="restored,backup"`) || !strings.Contains(body, "Restore completed") {
		t.Fatalf("restore notice should clear transient URL params: %s", body)
	}
}

func TestAdminBackupRestoreRequiresConfirmation(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	statePath := filepath.Join(dir, "state.db")
	if err := os.WriteFile(configPath, []byte(`{"port":"8088"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sqliteRepo, err := storage.NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := sqliteRepo.Close(); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(dir, "backup.agbak")
	if _, err := backup.Create(backupPath, backup.Options{ConfigPath: configPath, StatePath: statePath}); err != nil {
		t.Fatal(err)
	}
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServerWithOptions(control, nil, ServerOptions{ConfigPath: configPath, StatePath: statePath})
	handler := authenticatedAdminHandler(t, control, server.Handler())

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("csrf_token", testConsoleCSRFToken); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("confirm", "WRONG"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("backup", "backup.agbak")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/backup/restore", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("restore status = %d body=%s", rec.Code, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); !strings.Contains(location, "notice_error=") {
		t.Fatalf("location = %q, want notice error", location)
	}
}

func TestReadConsoleCSRFTokenReadsMultipartForm(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("csrf_token", "multipart-csrf-token"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/backup/restore", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	rec := httptest.NewRecorder()
	token, err := readConsoleCSRFToken(rec, req)
	if err != nil {
		t.Fatalf("readConsoleCSRFToken returned error: %v", err)
	}
	if token != "multipart-csrf-token" {
		t.Fatalf("token = %q", token)
	}
}

func TestReadConsoleCSRFTokenIgnoresURLQuery(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/login?csrf_token=query-token", strings.NewReader("next=/"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	token, err := readConsoleCSRFToken(rec, req)
	if err != nil {
		t.Fatalf("readConsoleCSRFToken returned error: %v", err)
	}
	if token != "" {
		t.Fatalf("token = %q, want empty token from request body only", token)
	}
}

func TestReadConsoleCSRFTokenIgnoresMultipartURLQuery(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("other", "value"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/backup/restore?csrf_token=query-token", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	rec := httptest.NewRecorder()
	token, err := readConsoleCSRFToken(rec, req)
	if err != nil {
		t.Fatalf("readConsoleCSRFToken returned error: %v", err)
	}
	if token != "" {
		t.Fatalf("token = %q, want empty token from multipart body only", token)
	}
}

func TestOpenAICompletionsErrorHidesUpstreamDetails(t *testing.T) {
	upstream := newJSONUpstream(
		t,
		"/v1/chat/completions",
		"Authorization",
		"Bearer openai-token",
		http.StatusServiceUnavailable,
		`{"error":{"message":"temporary upstream outage"}}`,
	)
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}

	var payload struct {
		Error struct {
			Type     string               `json:"type"`
			Message  string               `json:"message"`
			Attempts []core.AttemptRecord `json:"attempts,omitempty"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.Error.Type != gatewayProtocolErrorType {
		t.Fatalf("error.type = %q", payload.Error.Type)
	}
	if payload.Error.Message != gatewayProtocolErrorMessage {
		t.Fatalf("error.message = %q, want gateway wrapper", payload.Error.Message)
	}
	if len(payload.Error.Attempts) != 0 {
		t.Fatalf("attempts should not be exposed: %#v", payload.Error.Attempts)
	}
	body := rec.Body.String()
	for _, leaked := range []string{"temporary upstream outage", "upstream_server_error", "primary", "Primary", upstream.URL} {
		if strings.Contains(body, leaked) {
			t.Fatalf("response leaked upstream detail %q in %s", leaked, body)
		}
	}
}

func TestOpenAIStreamingOpenFailureKeepsHTTPErrorStatus(t *testing.T) {
	upstream := newJSONUpstream(
		t,
		"/v1/chat/completions",
		"Authorization",
		"Bearer openai-token",
		http.StatusServiceUnavailable,
		`{"error":{"message":"temporary upstream outage"}}`,
	)
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4.1","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "data:") {
		t.Fatalf("open failure should return JSON error, got SSE body %q", rec.Body.String())
	}
}

func TestOpenAIResponsesEndpointProxiesStatelessly(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s, want /v1/responses", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer openai-token" {
			t.Fatalf("authorization = %q", got)
		}
		for key, want := range map[string]string{
			"session-id":                             "sess_web",
			"thread-id":                              "thread_web",
			"x-codex-installation-id":                "install_web",
			"x-codex-window-id":                      "window_web",
			"x-openai-internal-codex-responses-lite": "true",
			"x-openai-subagent":                      "subagent_web",
			"User-Agent":                             "codex-web-test/1.0",
			"originator":                             "codex_cli_rs",
		} {
			if got := r.Header.Get(key); got != want {
				t.Fatalf("%s = %q, want %q", key, got, want)
			}
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		if body["input"] != "hello responses" {
			t.Fatalf("input = %#v", body["input"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_test","object":"response","created_at":1710000000,"status":"completed","model":"gpt-4.1","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello from responses","annotations":[]}]}],"output_text":"hello from responses","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	client, err := repo.FindClientByAPIKey("gw_test_key")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{
		ResponseID: "resp_prev",
		AccountID:  "primary",
		ClientID:   client.ID,
	}); err != nil {
		t.Fatal(err)
	}
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-4.1","input":"hello responses"}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session-id", "sess_web")
	req.Header.Set("thread-id", "thread_web")
	req.Header.Set("x-codex-installation-id", "install_web")
	req.Header.Set("x-codex-window-id", "window_web")
	req.Header.Set("x-openai-internal-codex-responses-lite", "true")
	req.Header.Set("x-openai-subagent", "subagent_web")
	req.Header.Set("User-Agent", "codex-web-test/1.0")
	req.Header.Set("originator", "codex_cli_rs")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		ID         string         `json:"id"`
		Object     string         `json:"object"`
		OutputText string         `json:"output_text"`
		Metadata   map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.ID != "resp_test" || payload.Object != "response" || payload.OutputText != "hello from responses" {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Metadata != nil {
		t.Fatalf("metadata should not be injected into raw OpenAI response: %#v", payload.Metadata)
	}
	binding, err := repo.GetOpenAIResponseBinding("resp_test")
	if err != nil {
		t.Fatalf("response binding missing: %v", err)
	}
	if binding.AccountID != "primary" || strings.TrimSpace(binding.ClientID) == "" {
		t.Fatalf("binding = %+v", binding)
	}
}

func TestOpenAIResponsesRequiresModelForBilling(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")

	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"input":"hello without model"}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model is required") {
		t.Fatalf("response = %s", rec.Body.String())
	}
}

func TestOpenAIResponseProxyDefaultClientUsesOnlyDefaultGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	hide := false
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_visible", Name: "Visible"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_hidden", Name: "Hidden", ShowInClientEditor: &hide}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_default",
		Provider: core.ProviderOpenAI,
		Label:    "Default",
		Status:   core.AccountStatusActive,
		Priority: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_visible",
		Provider: core.ProviderOpenAI,
		Label:    "Visible",
		Group:    "Visible",
		Status:   core.AccountStatusActive,
		Priority: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_hidden",
		Provider: core.ProviderOpenAI,
		Label:    "Hidden",
		Group:    "Hidden",
		Status:   core.AccountStatusActive,
		Priority: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_default", Name: "Default Key", APIKey: "gw_default", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	client, err := repo.GetClient("client_default")
	if err != nil {
		t.Fatal(err)
	}

	account, err := server.openAIProxyAccount(&client)
	if err != nil {
		t.Fatalf("openAIProxyAccount returned error: %v", err)
	}
	if account.ID != "acct_default" {
		t.Fatalf("account = %q, want Default account", account.ID)
	}
}

func TestOpenAIResponseProxyUsesBackupOnlyWhenPrimaryUnavailable(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_backup",
		Provider: core.ProviderOpenAI,
		Label:    "Backup",
		Status:   core.AccountStatusActive,
		Backup:   true,
		Priority: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_default", Name: "Default Key", APIKey: "gw_default", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	client, err := repo.GetClient("client_default")
	if err != nil {
		t.Fatal(err)
	}

	account, err := server.openAIProxyAccount(&client)
	if err != nil {
		t.Fatalf("openAIProxyAccount returned error: %v", err)
	}
	if account.ID != "acct_primary" {
		t.Fatalf("account = %q, want primary before backup", account.ID)
	}

	primary, err := repo.GetAccount("acct_primary")
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().UTC().Add(time.Hour)
	primary.Status = core.AccountStatusCooling
	primary.CooldownUntil = &until
	if err := repo.UpsertAccount(primary); err != nil {
		t.Fatal(err)
	}

	account, err = server.openAIProxyAccount(&client)
	if err != nil {
		t.Fatalf("openAIProxyAccount returned error after primary cooldown: %v", err)
	}
	if account.ID != "acct_backup" {
		t.Fatalf("account = %q, want backup while primary unavailable", account.ID)
	}
}

func TestOpenAIResponseProxyBoundAccountHonorsClientGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	hide := false
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_hidden", Name: "Hidden", ShowInClientEditor: &hide}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_hidden",
		Provider: core.ProviderOpenAI,
		Label:    "Hidden",
		Group:    "Hidden",
		Status:   core.AccountStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	defaultClient := core.APIClient{ID: "client_default", Name: "Default Key", APIKey: "gw_default", Enabled: true}
	if err := repo.UpsertClient(defaultClient); err != nil {
		t.Fatal(err)
	}
	hiddenClient := core.APIClient{ID: "client_hidden", Name: "Hidden Key", APIKey: "gw_hidden", Enabled: true, AccountGroup: "Hidden"}
	if err := repo.UpsertClient(hiddenClient); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{
		ResponseID: "resp_default_hidden",
		AccountID:  "acct_hidden",
		ClientID:   defaultClient.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{
		ResponseID: "resp_explicit_hidden",
		AccountID:  "acct_hidden",
		ClientID:   hiddenClient.ID,
	}); err != nil {
		t.Fatal(err)
	}

	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	if _, err := server.openAIProxyAccountForResponse(&defaultClient, "resp_default_hidden"); err == nil {
		t.Fatal("default client should not access hidden-group response binding")
	}
	account, err := server.openAIProxyAccountForResponse(&hiddenClient, "resp_explicit_hidden")
	if err != nil {
		t.Fatalf("hidden-group client should access its bound account: %v", err)
	}
	if account.ID != "acct_hidden" {
		t.Fatalf("account = %q, want hidden account", account.ID)
	}
}

func TestOpenAIResponsesCompactEndpointProxiesStatelessly(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses/compact" {
			t.Fatalf("path = %s, want /v1/responses/compact", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer openai-token" {
			t.Fatalf("authorization = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		if _, exists := body["previous_response_id"]; exists {
			t.Fatalf("previous_response_id should not be sent to compact endpoint: %#v", body)
		}
		if _, exists := body["stream"]; exists {
			t.Fatalf("stream should not be sent to compact endpoint: %#v", body)
		}
		if _, ok := body["input"].([]any); !ok {
			t.Fatalf("input = %#v", body["input"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_compact","object":"response.compaction","created_at":1710000000,"status":"completed","model":"gpt-4.1","usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}`))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	client, err := repo.FindClientByAPIKey("gw_test_key")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{
		ResponseID: "resp_prev",
		AccountID:  "missing_compact_bound_account",
		ClientID:   client.ID,
	}); err != nil {
		t.Fatal(err)
	}
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewBufferString(`{"model":"gpt-4.1","input":"compact responses","previous_response_id":"resp_prev"}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		ID       string         `json:"id"`
		Object   string         `json:"object"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.ID != "resp_compact" || payload.Object != "response.compaction" {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Metadata != nil {
		t.Fatalf("metadata should not be injected into raw OpenAI response: %#v", payload.Metadata)
	}
	if _, err := repo.GetOpenAIResponseBinding("resp_compact"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("compact response binding err = %v, want ErrNotFound", err)
	}
}

func TestOpenAIResponseResourceProxiesRawResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses/resp_123/input_items" {
			t.Fatalf("path = %s, want /v1/responses/resp_123/input_items", r.URL.Path)
		}
		if r.URL.RawQuery != "after=item_1" {
			t.Fatalf("query = %q", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer openai-token" {
			t.Fatalf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"item_2","type":"message"}],"has_more":false}`))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	client, err := repo.FindClientByAPIKey("gw_test_key")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{
		ResponseID: "resp_123",
		AccountID:  "primary",
		ClientID:   client.ID,
	}); err != nil {
		t.Fatal(err)
	}
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/v1/responses/resp_123/input_items?after=item_1", nil)
	req.Header.Set("Authorization", "Bearer gw_test_key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"object":"list","data":[{"id":"item_2","type":"message"}],"has_more":false}` {
		t.Fatalf("body = %s", got)
	}
}

func TestOpenAIResponseInputTokensProxiesToOpenAI(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses/input_tokens" {
			t.Fatalf("path = %s, want /v1/responses/input_tokens", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer openai-token" {
			t.Fatalf("authorization = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["model"] != "gpt-4.1" || body["input"] != "count me" {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":3}`))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", bytes.NewBufferString(`{"model":"gpt-4.1","input":"count me"}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload["input_tokens"] != float64(3) {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestOpenAIEmbeddingsAcceptTokenArrayInput(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Fatalf("path = %s, want /v1/embeddings", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		input, ok := body["input"].([]any)
		if !ok || len(input) != 3 || input[0] != float64(10) || input[2] != float64(30) {
			t.Fatalf("input = %#v", body["input"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","model":"text-embedding-3-small","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":3,"total_tokens":3}}`))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewBufferString(`{"model":"text-embedding-3-small","input":[10,20,30]}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload openAIEmbeddingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Data) != 1 || payload.Usage.PromptTokens != 3 {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestOpenAICompletionsPreservesRawToolCallResponse(t *testing.T) {
	upstream := newJSONUpstream(
		t,
		"/v1/chat/completions",
		"Authorization",
		"Bearer openai-token",
		http.StatusOK,
		`{"id":"chatcmpl_tool","model":"gpt-4.1","created":1710000000,"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"weather\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
	)
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	body := `{"model":"gpt-4.1","messages":[{"role":"user","content":"use tool"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":"auto"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	choices, ok := payload["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("choices = %#v", payload["choices"])
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if _, ok := message["tool_calls"].([]any); !ok {
		t.Fatalf("message.tool_calls = %#v", message["tool_calls"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %#v", choice["finish_reason"])
	}
}

func TestOpenAICompletionsAcceptsMultimodalMessageContent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		messages, ok := body["messages"].([]any)
		if !ok || len(messages) != 1 {
			t.Fatalf("messages = %#v", body["messages"])
		}
		message, _ := messages[0].(map[string]any)
		content, ok := message["content"].([]any)
		if !ok || len(content) != 2 {
			t.Fatalf("message content = %#v", message["content"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl_multimodal",
			"model":   "gpt-4.1",
			"created": 1710000000,
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"},
			},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 1, "total_tokens": 11},
		})
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	body := `{"model":"gpt-4.1","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestOpenAICompletionExtraDoesNotForwardInternalCacheAffinityKey(t *testing.T) {
	raw := map[string]json.RawMessage{
		"model":                json.RawMessage(`"gpt-4.1"`),
		"messages":             json.RawMessage(`[{"role":"user","content":"hello"}]`),
		"prompt_cache_key":     json.RawMessage(`"cache-public"`),
		"route_affinity_key":   json.RawMessage(`"route-only"`),
		"cache_affinity_key":   json.RawMessage(`"internal-only"`),
		"route_affinity_model": json.RawMessage(`"gpt-4.1"`),
		"custom_field":         json.RawMessage(`true`),
	}
	extra := openAICompletionExtra(raw)
	if _, exists := extra["route_affinity_key"]; exists {
		t.Fatalf("route_affinity_key should not be forwarded: %#v", extra)
	}
	if _, exists := extra["cache_affinity_key"]; exists {
		t.Fatalf("cache_affinity_key should not be forwarded: %#v", extra)
	}
	if _, exists := extra["route_affinity_model"]; exists {
		t.Fatalf("route_affinity_model should not be forwarded: %#v", extra)
	}
	if _, exists := extra["prompt_cache_key"]; exists {
		t.Fatalf("prompt_cache_key should be parsed as a known field: %#v", extra)
	}
	if _, exists := extra["custom_field"]; !exists {
		t.Fatalf("custom_field should be forwarded: %#v", extra)
	}
}

func TestOpenAIEmbeddingsReturnsOpenAICompatibleShape(t *testing.T) {
	upstream := newJSONUpstream(
		t,
		"/v1/embeddings",
		"Authorization",
		"Bearer openai-token",
		http.StatusOK,
		`{"object":"list","model":"text-embedding-3-small","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]},{"object":"embedding","index":1,"embedding":[0.4,0.5,0.6]}],"usage":{"prompt_tokens":8,"total_tokens":8}}`,
	)
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewBufferString(`{"model":"text-embedding-3-small","input":["alpha","beta"],"dimensions":3}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var payload struct {
		Object string `json:"object"`
		Model  string `json:"model"`
		Data   []struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage core.Usage `json:"usage"`
		Meta  any        `json:"meta"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.Object != "list" {
		t.Fatalf("object = %q", payload.Object)
	}
	if payload.Model != "text-embedding-3-small" {
		t.Fatalf("model = %q", payload.Model)
	}
	if len(payload.Data) != 2 || len(payload.Data[0].Embedding) != 3 {
		t.Fatalf("data = %#v", payload.Data)
	}
	if payload.Meta != nil {
		t.Fatalf("meta should not be injected into OpenAI response: %#v", payload.Meta)
	}
	if payload.Usage.TotalTokens == 0 {
		t.Fatalf("usage = %#v", payload.Usage)
	}
}

func TestOpenAIEmbeddingsAcceptsBase64EncodingFormat(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Fatalf("path = %s, want /v1/embeddings", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		if body["encoding_format"] != "base64" {
			t.Fatalf("encoding_format = %#v", body["encoding_format"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","model":"text-embedding-3-small","data":[{"object":"embedding","index":0,"embedding":"SGVsbG8="}],"usage":{"prompt_tokens":1,"total_tokens":1}}`))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewBufferString(`{"model":"text-embedding-3-small","input":"alpha","encoding_format":"base64"}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	data, _ := payload["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data = %#v", payload["data"])
	}
	first, _ := data[0].(map[string]any)
	if first["embedding"] != "SGVsbG8=" {
		t.Fatalf("embedding = %#v", first["embedding"])
	}
}

func TestOpenAIModerationsProxiesToOpenAI(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/moderations" {
			t.Fatalf("path = %s, want /v1/moderations", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer openai-token" {
			t.Fatalf("authorization = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["model"] != "omni-moderation-latest" || body["input"] != "hello" {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"modr_test","model":"omni-moderation-latest","results":[{"flagged":false,"categories":{"violence":false},"category_scores":{"violence":0.001}}]}`))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	if _, err := control.CreateModel(controlplane.ModelInput{
		ID:            "omni-moderation-latest",
		Provider:      core.ProviderOpenAI,
		Enabled:       true,
		VisibleGroups: []string{core.DefaultAccountGroupName},
	}); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("CreateModel returned error: %v", err)
	}
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/moderations", bytes.NewBufferString(`{"input":"hello"}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Results []struct {
			Flagged bool `json:"flagged"`
		} `json:"results"`
		Meta any `json:"meta"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.ID != "modr_test" || payload.Model != "omni-moderation-latest" || len(payload.Results) != 1 || payload.Results[0].Flagged {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Meta != nil {
		t.Fatalf("meta should not be injected into OpenAI response: %#v", payload.Meta)
	}
}

func TestOpenAIImageGenerationsProxiesToOpenAI(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			t.Fatalf("path = %s, want /v1/images/generations", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer openai-token" {
			t.Fatalf("authorization = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["model"] != "gpt-image-2" || body["prompt"] != "a gateway diagram" || body["size"] != "2048x2048" || body["output_format"] != "webp" || body["quality"] != "high" {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1710000000,"data":[{"url":"https://example.com/image.png"}]}`))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	if _, err := control.CreateModel(controlplane.ModelInput{
		ID:            "gpt-image-2",
		Provider:      core.ProviderOpenAI,
		Enabled:       true,
		VisibleGroups: []string{core.DefaultAccountGroupName},
	}); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("CreateModel returned error: %v", err)
	}
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewBufferString(`{"prompt":"a gateway diagram","size":"2048x2048","output_format":"webp","quality":"high"}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		Created int64 `json:"created"`
		Data    []struct {
			URL string `json:"url"`
		} `json:"data"`
		Meta any `json:"meta"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.Created != 1710000000 || len(payload.Data) != 1 || payload.Data[0].URL == "" {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Meta != nil {
		t.Fatalf("meta should not be injected into OpenAI response: %#v", payload.Meta)
	}
}

func TestOpenAIImageGenerationsHidesUpstreamErrorMessage(t *testing.T) {
	officialMessage := "The model `gpt-image-2` does not exist or you do not have access to it."
	handler := newImageProtocolTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			t.Fatalf("path = %s, want /v1/images/generations", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"` + officialMessage + `","code":"model_not_found"}}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewBufferString(`{"model":"gpt-image-2","prompt":"a gateway diagram"}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	var payload struct {
		Error struct {
			Type     string               `json:"type"`
			Message  string               `json:"message"`
			Attempts []core.AttemptRecord `json:"attempts,omitempty"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.Error.Type != gatewayProtocolErrorType {
		t.Fatalf("error.type = %q", payload.Error.Type)
	}
	if payload.Error.Message != gatewayProtocolErrorMessage {
		t.Fatalf("error message = %q, want gateway wrapper", payload.Error.Message)
	}
	if len(payload.Error.Attempts) != 0 {
		t.Fatalf("attempts should not be exposed: %#v", payload.Error.Attempts)
	}
	body := rec.Body.String()
	for _, leaked := range []string{officialMessage, "upstream_rejected", "model_not_found"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("response leaked upstream detail %q in %s", leaked, body)
		}
	}
}

func TestOpenAIImageGenerationsStreamHidesUpstreamErrorMessage(t *testing.T) {
	officialMessage := "The model `gpt-image-2` does not exist or you do not have access to it."
	handler := newImageProtocolTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			t.Fatalf("path = %s, want /v1/images/generations", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"` + officialMessage + `","code":"model_not_found"}}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewBufferString(`{"model":"gpt-image-2","prompt":"a gateway diagram","stream":true}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"event: image_generation.started", "event: error", gatewayProtocolErrorType, gatewayProtocolErrorMessage} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q in %q", want, body)
		}
	}
	for _, leaked := range []string{officialMessage, "upstream_rejected", "model_not_found"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("stream response leaked upstream detail %q in %q", leaked, body)
		}
	}
}

func TestOpenAIImageGenerationsRejectsUnsupportedGPTImage2Options(t *testing.T) {
	handler := newImageProtocolTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream should not be called for invalid image generation request")
	})

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "transparent_background",
			body: `{"model":"gpt-image-2","prompt":"a gateway diagram","background":"transparent"}`,
			want: "transparent background",
		},
		{
			name: "snapshot_transparent_background",
			body: `{"model":"gpt-image-2-2026-04-21","prompt":"a gateway diagram","background":"transparent"}`,
			want: "transparent background",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewBufferString(tc.body))
			req.Header.Set("Authorization", "Bearer gw_test_key")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("body = %s, want substring %q", rec.Body.String(), tc.want)
			}
		})
	}
}

func TestOpenAIImageGenerationsStreamsSSE(t *testing.T) {
	handler := newImageProtocolTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			t.Fatalf("path = %s, want /v1/images/generations", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Fatalf("accept = %q, want text/event-stream", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["model"] != "gpt-image-2" || body["prompt"] != "a gateway diagram" || body["stream"] != true || body["partial_images"] != float64(1) {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: image_generation.partial_image\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"abc\"}\n\n")
		_, _ = fmt.Fprint(w, "event: image_generation.completed\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"image_generation.completed\"}\n\n")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewBufferString(`{"prompt":"a gateway diagram","stream":true,"partial_images":1}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("content-type = %q", got)
	}
	if !strings.Contains(rec.Body.String(), "event: image_generation.partial_image") || !strings.Contains(rec.Body.String(), `"b64_json":"abc"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func newImageProtocolTestHandler(t *testing.T, upstreamHandler http.HandlerFunc) http.Handler {
	t.Helper()
	upstream := httptest.NewServer(upstreamHandler)
	t.Cleanup(upstream.Close)

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	for _, modelID := range []string{"gpt-image-2", "gpt-image-2-2026-04-21", "codex-gpt-image-2", "dall-e-2"} {
		if _, err := control.CreateModel(controlplane.ModelInput{
			ID:            modelID,
			Provider:      core.ProviderOpenAI,
			Enabled:       true,
			VisibleGroups: []string{core.DefaultAccountGroupName},
		}); err != nil && !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("CreateModel(%s) returned error: %v", modelID, err)
		}
	}
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	return authenticatedAdminHandler(t, control, server.Handler())
}

func TestOpenAIAudioSpeechProxiesBinaryResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" {
			t.Fatalf("path = %s, want /v1/audio/speech", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer openai-token" {
			t.Fatalf("authorization = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		voice, _ := body["voice"].(map[string]any)
		if body["model"] != "gpt-4o-mini-tts" || body["input"] != "hello audio" || voice["id"] != "voice_custom" || body["format"] != "mp3" {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("MP3DATA"))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	if _, err := control.CreateModel(controlplane.ModelInput{
		ID:            "gpt-4o-mini-tts",
		Provider:      core.ProviderOpenAI,
		Enabled:       true,
		VisibleGroups: []string{core.DefaultAccountGroupName},
	}); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("CreateModel returned error: %v", err)
	}
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewBufferString(`{"model":"gpt-4o-mini-tts","input":"hello audio","voice":{"id":"voice_custom"},"format":"mp3"}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/mpeg" {
		t.Fatalf("content-type = %q, want audio/mpeg", got)
	}
	if rec.Body.String() != "MP3DATA" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if rec.Header().Get("X-AI-Gateway-Provider") != "" || rec.Header().Get("X-AI-Gateway-Account") != "" {
		t.Fatalf("gateway headers should not be injected: provider=%q account=%q", rec.Header().Get("X-AI-Gateway-Provider"), rec.Header().Get("X-AI-Gateway-Account"))
	}
}

func TestOpenAIAudioMultipartProxiesAndRewritesModel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
	}{
		{name: "transcription", endpoint: "/v1/audio/transcriptions"},
		{name: "translation", endpoint: "/v1/audio/translations"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.endpoint {
					t.Fatalf("path = %s, want %s", r.URL.Path, tc.endpoint)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer openai-token" {
					t.Fatalf("authorization = %q", got)
				}
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Fatalf("ParseMultipartForm returned error: %v", err)
				}
				if got := r.FormValue("model"); got != "whisper-1-upstream" {
					t.Fatalf("model = %q, want whisper-1-upstream", got)
				}
				if got := r.FormValue("language"); got != "en" {
					t.Fatalf("language = %q, want en", got)
				}
				file, _, err := r.FormFile("file")
				if err != nil {
					t.Fatalf("FormFile returned error: %v", err)
				}
				defer file.Close()
				fileBytes, _ := io.ReadAll(file)
				if string(fileBytes) != "AUDIO" {
					t.Fatalf("file bytes = %q", string(fileBytes))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"text":"hello transcript"}`))
			}))
			defer upstream.Close()

			repo := storage.NewMemoryRepository()
			if err := repo.UpsertAccount(core.Account{
				ID:       "primary",
				Provider: core.ProviderOpenAI,
				Label:    "Primary",
				Status:   core.AccountStatusActive,
				Priority: 100,
				Weight:   100,
				Credential: core.Credential{
					Mode:        "manual-token",
					AccessToken: "openai-token",
					Metadata:    map[string]string{"base_url": upstream.URL},
				},
			}); err != nil {
				t.Fatal(err)
			}
			registry := providers.NewRegistry(&providers.OpenAIAdapter{})
			control := controlplane.New(repo, registry)
			mustSeedProtocolClient(t, control, "gw_test_key")
			if _, err := control.CreateModel(controlplane.ModelInput{
				ID:            "whisper-1",
				Provider:      core.ProviderOpenAI,
				UpstreamID:    "whisper-1-upstream",
				Enabled:       true,
				VisibleGroups: []string{core.DefaultAccountGroupName},
			}); err != nil && !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("CreateModel returned error: %v", err)
			}
			gatewayService := gateway.New(
				repo,
				routing.NewRouter(),
				failover.NewEngine(accounts.NewPool(repo), registry),
			)

			server := NewServer(control, gatewayService, "data/state.db")
			handler := authenticatedAdminHandler(t, control, server.Handler())

			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			if err := writer.WriteField("model", "whisper-1"); err != nil {
				t.Fatal(err)
			}
			if err := writer.WriteField("language", "en"); err != nil {
				t.Fatal(err)
			}
			fileWriter, err := writer.CreateFormFile("file", "audio.wav")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fileWriter.Write([]byte("AUDIO")); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest(http.MethodPost, tc.endpoint, &body)
			req.Header.Set("Authorization", "Bearer gw_test_key")
			req.Header.Set("Content-Type", writer.FormDataContentType())
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
				t.Fatalf("content-type = %q", got)
			}
			if !strings.Contains(rec.Body.String(), `"text":"hello transcript"`) {
				t.Fatalf("body = %s", rec.Body.String())
			}
			if rec.Header().Get("X-AI-Gateway-Provider") != "" || rec.Header().Get("X-AI-Gateway-Account") != "" {
				t.Fatalf("gateway headers should not be injected: provider=%q account=%q", rec.Header().Get("X-AI-Gateway-Provider"), rec.Header().Get("X-AI-Gateway-Account"))
			}
		})
	}
}

func TestOpenAIImageEditsProxiesAndRewritesModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/edits" {
			t.Fatalf("path = %s, want /v1/images/edits", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer openai-token" {
			t.Fatalf("authorization = %q", got)
		}
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			t.Fatalf("ParseMultipartForm returned error: %v", err)
		}
		if got := r.FormValue("model"); got != "gpt-image-upstream" {
			t.Fatalf("model = %q, want gpt-image-upstream", got)
		}
		if got := r.FormValue("prompt"); got != "make it brighter" {
			t.Fatalf("prompt = %q, want make it brighter", got)
		}
		file, _, err := r.FormFile("image")
		if err != nil {
			t.Fatalf("FormFile returned error: %v", err)
		}
		defer file.Close()
		fileBytes, _ := io.ReadAll(file)
		if string(fileBytes) != "PNGDATA" {
			t.Fatalf("image bytes = %q", string(fileBytes))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1710000001,"data":[{"url":"https://example.com/edit.png"}]}`))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	if _, err := control.CreateModel(controlplane.ModelInput{
		ID:            "gpt-image-2",
		Provider:      core.ProviderOpenAI,
		UpstreamID:    "gpt-image-upstream",
		Enabled:       true,
		VisibleGroups: []string{core.DefaultAccountGroupName},
	}); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("CreateModel returned error: %v", err)
	}
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("prompt", "make it brighter"); err != nil {
		t.Fatal(err)
	}
	fileWriter, err := writer.CreateFormFile("image", "image.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fileWriter.Write([]byte("PNGDATA")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("content-type = %q", got)
	}
	if !strings.Contains(rec.Body.String(), `"url":"https://example.com/edit.png"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if rec.Header().Get("X-AI-Gateway-Provider") != "" || rec.Header().Get("X-AI-Gateway-Account") != "" {
		t.Fatalf("gateway headers should not be injected: provider=%q account=%q", rec.Header().Get("X-AI-Gateway-Provider"), rec.Header().Get("X-AI-Gateway-Account"))
	}
}

func TestOpenAIImageEditsHidesUpstreamErrorMessage(t *testing.T) {
	officialMessage := "Invalid image input: expected PNG, JPEG, or WebP."
	handler := newImageProtocolTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/edits" {
			t.Fatalf("path = %s, want /v1/images/edits", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"` + officialMessage + `","code":"invalid_image"}}`))
	})

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "gpt-image-2"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("prompt", "make it brighter"); err != nil {
		t.Fatal(err)
	}
	fileWriter, err := writer.CreateFormFile("image", "image.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fileWriter.Write([]byte("PNGDATA")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.Error.Type != gatewayProtocolErrorType {
		t.Fatalf("error.type = %q", payload.Error.Type)
	}
	if payload.Error.Message != gatewayProtocolErrorMessage {
		t.Fatalf("error message = %q, want gateway wrapper", payload.Error.Message)
	}
	if strings.Contains(rec.Body.String(), officialMessage) || strings.Contains(rec.Body.String(), "invalid_image") {
		t.Fatalf("response leaked upstream detail: %s", rec.Body.String())
	}
}

func TestOpenAIImageEditsStreamHidesUpstreamErrorMessage(t *testing.T) {
	officialMessage := "Invalid image input: expected PNG, JPEG, or WebP."
	handler := newImageProtocolTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/edits" {
			t.Fatalf("path = %s, want /v1/images/edits", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"` + officialMessage + `","code":"invalid_image"}}`))
	})

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range map[string]string{
		"model":  "gpt-image-2",
		"prompt": "make it brighter",
		"stream": "true",
	} {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	fileWriter, err := writer.CreateFormFile("image", "image.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fileWriter.Write([]byte("PNGDATA")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	bodyText := rec.Body.String()
	for _, want := range []string{"event: image_generation.started", "event: error", gatewayProtocolErrorType, gatewayProtocolErrorMessage} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("response missing %q in %q", want, bodyText)
		}
	}
	for _, leaked := range []string{officialMessage, "upstream_rejected", "invalid_image"} {
		if strings.Contains(bodyText, leaked) {
			t.Fatalf("stream response leaked upstream detail %q in %q", leaked, bodyText)
		}
	}
}

func TestOpenAIImageMultipartRejectsUnsupportedGPTImage2Options(t *testing.T) {
	handler := newImageProtocolTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream should not be called for invalid image multipart request")
	})

	t.Run("edit_input_fidelity", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.WriteField("prompt", "make it brighter"); err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteField("input_fidelity", "high"); err != nil {
			t.Fatal(err)
		}
		fileWriter, err := writer.CreateFormFile("image", "image.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fileWriter.Write([]byte("PNGDATA")); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}

		req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
		req.Header.Set("Authorization", "Bearer gw_test_key")
		req.Header.Set("Content-Type", writer.FormDataContentType())
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "input_fidelity") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})

	t.Run("edit_transparent_background", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.WriteField("prompt", "make it brighter"); err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteField("background", "transparent"); err != nil {
			t.Fatal(err)
		}
		fileWriter, err := writer.CreateFormFile("image", "image.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fileWriter.Write([]byte("PNGDATA")); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}

		req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
		req.Header.Set("Authorization", "Bearer gw_test_key")
		req.Header.Set("Content-Type", writer.FormDataContentType())
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "transparent background") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})

}

func TestOpenAIImageEditsStreamsSSE(t *testing.T) {
	handler := newImageProtocolTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/edits" {
			t.Fatalf("path = %s, want /v1/images/edits", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Fatalf("accept = %q, want text/event-stream", got)
		}
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			t.Fatalf("ParseMultipartForm returned error: %v", err)
		}
		if got := r.FormValue("model"); got != "gpt-image-2" {
			t.Fatalf("model = %q, want gpt-image-2", got)
		}
		if got := r.FormValue("stream"); got != "true" {
			t.Fatalf("stream = %q, want true", got)
		}
		if got := r.FormValue("prompt"); got != "make it brighter" {
			t.Fatalf("prompt = %q, want make it brighter", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: image_generation.partial_image\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"edit\"}\n\n")
		_, _ = fmt.Fprint(w, "event: image_generation.completed\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"image_generation.completed\"}\n\n")
	})

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("prompt", "make it brighter"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("stream", "true"); err != nil {
		t.Fatal(err)
	}
	fileWriter, err := writer.CreateFormFile("image", "image.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fileWriter.Write([]byte("PNGDATA")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("content-type = %q", got)
	}
	if !strings.Contains(rec.Body.String(), "event: image_generation.partial_image") || !strings.Contains(rec.Body.String(), `"b64_json":"edit"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestHealthEndpointReturnsStructuredReport(t *testing.T) {
	repo := storage.NewMemoryRepository()
	expiresAt := time.Now().UTC().Add(10 * time.Minute).Truncate(time.Second)
	for _, account := range []core.Account{
		{
			ID:       "acct_openai",
			Provider: core.ProviderOpenAI,
			Label:    "OpenAI",
			Status:   core.AccountStatusActive,
			Credential: core.Credential{
				AccessToken: "openai-token",
				ExpiresAt:   &expiresAt,
			},
		},
		{
			ID:       "acct_claude",
			Provider: core.ProviderClaude,
			Label:    "Claude",
			Status:   core.AccountStatusBlocked,
			Credential: core.Credential{
				AccessToken: "claude-token",
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), `"available_accounts"`) || !strings.Contains(rec.Body.String(), `"healthy_provider_count"`) {
		t.Fatalf("unexpected health payload shape: %q", rec.Body.String())
	}

	var report controlplane.HealthReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if report.Status != "degraded" {
		t.Fatalf("status = %q, want degraded", report.Status)
	}
	if report.AvailableAccounts != 1 || report.TotalAccounts != 2 {
		t.Fatalf("account counts = %#v", report)
	}
	if report.ExpiringSoonCount != 1 {
		t.Fatalf("expiring soon = %d, want 1", report.ExpiringSoonCount)
	}
	if report.HealthyProviderCount != 1 || report.DegradedProviderCount != 1 {
		t.Fatalf("provider counts = healthy:%d degraded:%d", report.HealthyProviderCount, report.DegradedProviderCount)
	}
	if report.GeneratedAt.IsZero() {
		t.Fatal("expected generated timestamp")
	}
	if len(report.Providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(report.Providers))
	}
}

func TestHealthEndpointReturns503WhenNoAvailableAccounts(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_blocked",
		Provider: core.ProviderOpenAI,
		Label:    "Blocked",
		Status:   core.AccountStatusBlocked,
		Credential: core.Credential{
			AccessToken: "openai-token",
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	var report controlplane.HealthReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if report.Status != "error" {
		t.Fatalf("status = %q, want error", report.Status)
	}
	if report.Reason != "no available accounts" {
		t.Fatalf("reason = %q", report.Reason)
	}
}

func TestHealthEndpointReturns200WhenNoAccountsConfigured(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var report controlplane.HealthReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if report.Status != "setup" {
		t.Fatalf("status = %q, want setup", report.Status)
	}
	if report.Reason != "no accounts configured" {
		t.Fatalf("reason = %q", report.Reason)
	}
}

func TestAccountEditPageRendersExistingValues(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_core", Name: "Core"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_edit",
		Provider: core.ProviderOpenAI,
		Label:    "Editable",
		Remark:   "existing remark",
		Group:    "Core",
		Status:   core.AccountStatusActive,
		Priority: 90,
		Weight:   80,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "secret-token",
			Metadata: map[string]string{
				"base_url": "https://example.com",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts/acct_edit/edit", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{"Editable", "Core", "existing remark", `name="remark"`, "https://example.com", "name=\"status\"", "data-confirm=", "Delete account"} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q", want)
		}
	}
}

func TestAccountEditUpdatesBaseURLAndPreservesToken(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_core", Name: "Core"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_shared", Name: "Shared"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_edit",
		Provider: core.ProviderOpenAI,
		Label:    "Editable",
		Group:    "Core",
		Status:   core.AccountStatusActive,
		Priority: 90,
		Weight:   80,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "secret-token",
			Metadata: map[string]string{
				"base_url": "https://example.com",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	expiresAt := "2026-05-01T10:00:00Z"
	form := strings.NewReader("label=Updated&remark=operator%20note&group=Shared&access_token=&refresh_token=&session_token=&expires_at=" + url.QueryEscape(expiresAt) + "&base_url=https%3A%2F%2Fproxy.example.com&priority=120&weight=70&status=active")
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/acct_edit/edit", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}

	account, err := repo.GetAccount("acct_edit")
	if err != nil {
		t.Fatal(err)
	}
	if account.Label != "Updated" {
		t.Fatalf("label = %q, want %q", account.Label, "Updated")
	}
	if account.Group != "Shared" {
		t.Fatalf("group = %q, want %q", account.Group, "Shared")
	}
	if account.Remark != "operator note" {
		t.Fatalf("remark = %q, want %q", account.Remark, "operator note")
	}
	if account.Credential.AccessToken != "secret-token" {
		t.Fatalf("token = %q, want %q", account.Credential.AccessToken, "secret-token")
	}
	if account.Credential.Metadata["base_url"] != "https://proxy.example.com" {
		t.Fatalf("base_url = %q", account.Credential.Metadata["base_url"])
	}
	if account.Credential.ExpiresAt == nil || account.Credential.ExpiresAt.UTC().Format(time.RFC3339) != expiresAt {
		t.Fatalf("expires_at = %#v", account.Credential.ExpiresAt)
	}
	if account.Status != core.AccountStatusActive {
		t.Fatalf("status = %q, want %q", account.Status, core.AccountStatusActive)
	}
	if !account.ControlDisabled {
		t.Fatal("account should be locally disabled when control_enabled is unchecked")
	}
}

func TestAccountEditAcceptsExplicitDefaultGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_core", Name: "Core"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:         "acct_default_edit",
		Provider:   core.ProviderOpenAI,
		Label:      "Editable",
		Group:      "Core",
		Status:     core.AccountStatusActive,
		Priority:   100,
		Weight:     100,
		Credential: core.Credential{AccessToken: "secret-token"},
	}); err != nil {
		t.Fatal(err)
	}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	form := url.Values{}
	form.Set("label", "Editable")
	form.Set("group", core.DefaultAccountGroupName)
	form.Set("priority", "100")
	form.Set("weight", "100")
	form.Set("status", string(core.AccountStatusActive))
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/acct_default_edit/edit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	account, err := repo.GetAccount("acct_default_edit")
	if err != nil {
		t.Fatal(err)
	}
	if account.Group != core.DefaultAccountGroupName {
		t.Fatalf("group = %q, want default group", account.Group)
	}
}

func TestAccountEditInvalidTimestampShowsFormError(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_core", Name: "Core"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_edit_error",
		Provider: core.ProviderOpenAI,
		Label:    "Editable",
		Group:    "Core",
		Status:   core.AccountStatusActive,
		Priority: 90,
		Weight:   80,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "secret-token",
		},
	}); err != nil {
		t.Fatal(err)
	}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	form := url.Values{}
	form.Set("label", "Still Editable")
	form.Set("group", "Core")
	form.Set("expires_at", "not-a-time")
	form.Set("priority", "100")
	form.Set("weight", "100")
	form.Set("status", string(core.AccountStatusActive))
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/acct_edit_error/edit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/admin/accounts/acct_edit_error/edit?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("location = %q, want account edit notice redirect", location)
	}
}

func TestAccountsPageRendersGroupedSections(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, group := range []core.AccountGroup{
		{ID: "group_alpha", Name: "Alpha"},
		{ID: "group_beta", Name: "Beta"},
	} {
		if err := repo.UpsertAccountGroup(group); err != nil {
			t.Fatal(err)
		}
	}
	for _, account := range []core.Account{
		{ID: "acct_1", Provider: core.ProviderOpenAI, Label: "Alpha One", Group: "Alpha", Status: core.AccountStatusActive, Credential: core.Credential{AccessToken: "token-a"}},
		{ID: "acct_2", Provider: core.ProviderClaude, Label: "Beta One", Group: "Default", Status: core.AccountStatusActive, Backup: true, Credential: core.Credential{AccessToken: "token-b"}},
		{ID: "acct_3", Provider: core.ProviderOpenAI, Label: "Loose", Status: core.AccountStatusActive, Credential: core.Credential{AccessToken: "token-c"}},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.UpsertUser(core.User{ID: "user_backup_today", Username: "backup-user", Role: core.UserRoleUser, Enabled: true, BalanceNanoUSD: 10_000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_backup_today", Name: "Client Backup", OwnerUserID: "user_backup_today", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_backup_today",
		ClientID:        "client_backup_today",
		ClientName:      "Client Backup",
		UserID:          "user_backup_today",
		AccountID:       "acct_2",
		AccountLabel:    "Beta One",
		Provider:        core.ProviderClaude,
		Model:           "claude-3.5-sonnet",
		ReservedNanoUSD: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_backup_today",
		ClientID:      "client_backup_today",
		AccountID:     "acct_2",
		AccountLabel:  "Beta One",
		Provider:      core.ProviderClaude,
		Model:         "claude-3.5-sonnet",
		Usage:         core.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		ActualNanoUSD: 100,
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{"Alpha", "Beta", "Default", "Loose", `data-group-settings-open="account-group-create-modal"`, `name="type"`, "/admin/accounts?group=Default", "/admin/accounts?group=Alpha"} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q", want)
		}
	}
	if !strings.Contains(body, "today calls") || !strings.Contains(body, "1") {
		t.Fatalf("expected backup account today calls in response: %s", body)
	}
	if strings.Contains(body, "Alpha One") {
		t.Fatal("expected non-active group content to stay hidden")
	}
}

func TestAccountsPageSelectsRequestedGroupTab(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts?group=Plus", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{"No accounts in this group yet.", "aria-selected=\"true\"", "Plus", `<option value="Plus" selected>Plus</option>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q", want)
		}
	}
}

func TestAccountsPageDisablesOpenAIQuotaProviderByDefault(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="api_key_quota_provider"`) || !strings.Contains(body, `value="" selected`) {
		t.Fatalf("openai connect panel should default quota provider to disabled: %s", body)
	}
}

func TestClaudeConnectPageRendersAPIKeyQuotaProvider(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/connect/claude", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{`name="account_login_method"`, `value="api_key" selected`, `name="api_key_quota_provider"`, `value="sub2api"`, `value="gateway"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, `value="new-api"`) {
		t.Fatalf("claude connect page should not render new-api quota provider: %s", body)
	}
}

func TestAccountsPageRendersDefaultGroupSettings(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts?group=Default", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{"Default", "/admin/account-groups/default/proxy", "/admin/account-groups/default/billing", `value="1"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q", want)
		}
	}
	if strings.Contains(body, "/admin/account-groups/default/delete") {
		t.Fatal("default group should not render delete action")
	}
}

func TestFaviconICOReturnsIcon(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")

	req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "image/x-icon") {
		t.Fatalf("content-type = %q", got)
	}
	body := rec.Body.Bytes()
	if len(body) < 6 || body[2] != 1 || body[4] != 1 {
		t.Fatalf("invalid ico header: %v", body[:min(len(body), 6)])
	}
}

func TestDashboardIncludesFaviconICO(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if strings.Contains(body, `rel="shortcut icon"`) || !strings.Contains(body, `rel="icon"`) || !strings.Contains(body, `href="/favicon.ico"`) {
		t.Fatalf("dashboard missing favicon link: %s", body)
	}
	if strings.Contains(body, "favicon.svg") || strings.Contains(body, "favicon.png") {
		t.Fatalf("dashboard should only reference ico favicon: %s", body)
	}
}

func TestAccountDetectActionChecksStatus(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_test",
		Provider: core.ProviderOpenAI,
		Label:    "Test Account",
		Group:    "Plus",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			AccessToken: "token",
			Metadata: map[string]string{
				"token_source": providers.OpenAIDeviceCodeTokenSourceValue(),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	adapter := &testWebQuotaAdapter{}
	registry := providers.NewRegistry(adapter)
	control := controlplane.New(repo, registry)

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts?group=Plus&filter=normal", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("page status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "/admin/accounts/acct_test/test") {
		t.Fatal("account detect action missing from account page")
	}
	if !strings.Contains(rec.Body.String(), ">Detect<") && !strings.Contains(rec.Body.String(), ">检测<") {
		t.Fatal("account detect button label missing from account page")
	}
	if !strings.Contains(rec.Body.String(), `name="current_group" value="Plus"`) {
		t.Fatal("account action forms should preserve the active group")
	}
	if !strings.Contains(rec.Body.String(), `name="current_filter" value="normal"`) {
		t.Fatal("account action forms should preserve the active filter")
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/accounts/acct_test/test", strings.NewReader("current_group=Plus&current_filter=normal"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("test status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.Contains(location, "test_tone=good") {
		t.Fatalf("redirect location = %q, want success notice", location)
	}
	if !strings.Contains(location, "group=Plus") {
		t.Fatalf("redirect location = %q, want active group to be preserved", location)
	}
	if !strings.Contains(location, "filter=normal") {
		t.Fatalf("redirect location = %q, want active filter to be preserved", location)
	}
	if !strings.Contains(location, "Detection+passed%3A+Test+Account+reached+upstream") {
		t.Fatalf("redirect location = %q, want account success notice", location)
	}

	req = httptest.NewRequest(http.MethodGet, location, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("notice page status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-clear-url-params="test_tone,test_message"`) {
		t.Fatalf("account test notice should clear transient URL params: %s", rec.Body.String())
	}
}

func TestAccountGroupCreateAddsGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	form := strings.NewReader("name=Shared")
	req := httptest.NewRequest(http.MethodPost, "/admin/account-groups", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	groups := repo.ListAccountGroups()
	if len(groups) != 1 || groups[0].Name != "Shared" {
		t.Fatalf("groups = %#v", groups)
	}
}

func TestAccountGroupCreateDuplicateRedirectsWithNotice(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_shared", Name: "Shared"}); err != nil {
		t.Fatal(err)
	}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	form := strings.NewReader("name=Shared")
	req := httptest.NewRequest(http.MethodPost, "/admin/account-groups", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/admin/accounts?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("location = %q, want accounts notice redirect", location)
	}
	noticeReq := httptest.NewRequest(http.MethodGet, location, nil)
	noticeRec := httptest.NewRecorder()
	handler.ServeHTTP(noticeRec, noticeReq)
	if noticeRec.Code != http.StatusOK {
		t.Fatalf("notice status = %d, want %d", noticeRec.Code, http.StatusOK)
	}
	body := noticeRec.Body.String()
	if !strings.Contains(body, `data-clear-url-params="notice_error"`) || !strings.Contains(body, "already exists") {
		t.Fatalf("notice missing duplicate error: %s", body)
	}
}

func TestAccountGroupVisibilitySavesVisibleUsers(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, user := range []core.User{
		{ID: "user_group_a", Username: "group-a", Role: core.UserRoleUser, Enabled: true},
		{ID: "user_group_b", Username: "group-b", Role: core.UserRoleUser, Enabled: true},
	} {
		if err := repo.UpsertUser(user); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_private", Name: "Private"}); err != nil {
		t.Fatal(err)
	}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts?group=Private", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`data-message-target`, `data-message-user-search-url="/messages/users/search"`, `data-user-input-name="visible_user_id"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("account group visibility page missing %q: %s", want, body)
		}
	}

	form := url.Values{}
	form.Set("current_group", "Private")
	form.Set("current_filter", "all")
	form.Add("visible_user_id", "user_group_a")
	form.Add("visible_user_id", "user_group_b")
	req = httptest.NewRequest(http.MethodPost, "/admin/account-groups/group_private/visibility", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	groups := repo.ListAccountGroups()
	if len(groups) != 1 || len(groups[0].VisibleUserIDs) != 2 || groups[0].VisibleUserIDs[0] != "user_group_a" || groups[0].VisibleUserIDs[1] != "user_group_b" {
		t.Fatalf("groups = %#v, want two visible users", groups)
	}
	if groups[0].ShowInClientEditor == nil || *groups[0].ShowInClientEditor {
		t.Fatalf("ShowInClientEditor = %#v, want false when checkbox omitted", groups[0].ShowInClientEditor)
	}
	req = httptest.NewRequest(http.MethodGet, "/admin/accounts?group=Private", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status after save = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, want := range []string{`name="visible_user_id" value="user_group_a"`, `name="visible_user_id" value="user_group_b"`, `data-message-user-option data-user-id="user_group_a"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("saved visibility page missing %q: %s", want, rec.Body.String())
		}
	}
}

func TestAccountGroupRemarkSavesAndRendersInSettings(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_research", Name: "Research", Remark: "Initial note"}); err != nil {
		t.Fatal(err)
	}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts?group=Research", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, want := range []string{`/admin/account-groups/group_research/profile`, `name="remark" value="Initial note"`, `maxlength="120"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("settings page missing %q: %s", want, rec.Body.String())
		}
	}

	form := url.Values{}
	form.Set("current_group", "Research")
	form.Set("current_filter", "all")
	form.Set("remark", "  Only   for\nresearch workloads  ")
	req = httptest.NewRequest(http.MethodPost, "/admin/account-groups/group_research/remark", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	groups := repo.ListAccountGroups()
	if len(groups) != 1 || groups[0].Remark != "Only for research workloads" {
		t.Fatalf("groups = %#v, want normalized remark", groups)
	}
}

func TestAccountGroupCreateReservedNameRedirectsWithNotice(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/admin/account-groups", strings.NewReader("name=Default"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/admin/accounts?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("location = %q, want accounts notice redirect", location)
	}
	if groups := repo.ListAccountGroups(); len(groups) != 0 {
		t.Fatalf("groups = %#v, want none", groups)
	}
}

func TestAccountDeleteKeepsClientGroupBinding(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{ID: "acct_a", Provider: core.ProviderOpenAI, Label: "A", Group: "Plus", Status: core.AccountStatusActive},
		{ID: "acct_b", Provider: core.ProviderOpenAI, Label: "B", Group: "Plus", Status: core.AccountStatusActive},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:           "client_a",
		Name:         "Client A",
		APIKey:       "gw_key",
		Enabled:      true,
		AccountGroup: "Plus",
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/acct_a/delete", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if _, err := repo.GetAccount("acct_a"); err == nil {
		t.Fatal("expected account to be deleted")
	}
	client, err := repo.GetClient("client_a")
	if err != nil {
		t.Fatal(err)
	}
	if client.AccountGroup != "Plus" {
		t.Fatalf("account group = %q", client.AccountGroup)
	}
}

func TestAccountRecoverResetsFailureStateAndWritesAudit(t *testing.T) {
	repo := storage.NewMemoryRepository()
	cooldownUntil := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	if err := repo.UpsertAccount(core.Account{
		ID:               "acct_recover",
		Provider:         core.ProviderOpenAI,
		Label:            "Recover",
		Status:           core.AccountStatusCooling,
		CooldownUntil:    &cooldownUntil,
		ConsecutiveFails: 3,
		TotalFails:       9,
		Credential: core.Credential{
			AccessToken: "openai-token",
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/acct_recover/recover", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}

	account, err := repo.GetAccount("acct_recover")
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != core.AccountStatusActive {
		t.Fatalf("status = %q, want %q", account.Status, core.AccountStatusActive)
	}
	if account.CooldownUntil != nil {
		t.Fatalf("cooldown = %#v, want nil", account.CooldownUntil)
	}
	if account.ConsecutiveFails != 0 || account.TotalFails != 9 {
		t.Fatalf("failure state = consecutive:%d total:%d", account.ConsecutiveFails, account.TotalFails)
	}

	audits := repo.ListAudit(1)
	if len(audits) != 1 {
		t.Fatalf("audits = %d, want 1", len(audits))
	}
	if audits[0].Action != "account.recover" {
		t.Fatalf("action = %q, want account.recover", audits[0].Action)
	}
	if audits[0].Status != "ok" {
		t.Fatalf("audit status = %q, want ok", audits[0].Status)
	}
}

func TestModelSyncFailureRedirectsWithNotice(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	form := url.Values{}
	form.Set("provider", string(core.ProviderOpenAI))
	req := httptest.NewRequest(http.MethodPost, "/admin/models/sync", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/admin/models?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("location = %q, want models notice redirect", location)
	}
	noticeReq := httptest.NewRequest(http.MethodGet, location, nil)
	noticeRec := httptest.NewRecorder()
	handler.ServeHTTP(noticeRec, noticeReq)
	if noticeRec.Code != http.StatusOK {
		t.Fatalf("notice status = %d, want %d", noticeRec.Code, http.StatusOK)
	}
	body := noticeRec.Body.String()
	if !strings.Contains(body, `data-clear-url-params="notice_error"`) || !strings.Contains(body, "has no available account for model sync") {
		t.Fatalf("notice missing sync error: %s", body)
	}
}

func TestOpenAIStreamingUsesUpstreamDeltas(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4.1","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"\"content\":\"hello \"",
		"\"content\":\"world\"",
		"\"finish_reason\":\"stop\"",
		"data: [DONE]",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q in %q", want, body)
		}
	}
}

func TestOpenAIStreamingTerminalToolCallRecordsFirstToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_tool\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_tool\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	client, err := repo.FindClientByAPIKey("gw_test_key")
	if err != nil {
		t.Fatal(err)
	}
	user, err := control.CreateUser(controlplane.UserInput{
		Username:       "stream-tool-first-token",
		Password:       "stream-tool-secret",
		Role:           core.UserRoleUser,
		Enabled:        true,
		BalanceNanoUSD: core.NanoUSDPerUSD,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	client.OwnerUserID = user.ID
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "gpt-4.1",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "gpt-4.1",
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4.1","stream":true,"messages":[{"role":"user","content":"use tool"}]}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"tool_calls"`, `"finish_reason":"tool_calls"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q in %q", want, body)
		}
	}
	requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: client.ID, Limit: 1})
	if total != 1 || len(requests) != 1 {
		t.Fatalf("billing requests = total %d items %#v, want one", total, requests)
	}
	if requests[0].Status != core.BillingRequestSettled {
		t.Fatalf("billing status = %q, want %q", requests[0].Status, core.BillingRequestSettled)
	}
	if requests[0].FirstTokenMS <= 0 {
		t.Fatalf("first token ms = %d, want > 0", requests[0].FirstTokenMS)
	}
}

func TestOpenAIStreamingSendsRoleChunkWithFirstUpstreamOutput(t *testing.T) {
	upstreamReady := make(chan struct{})
	releaseUpstream := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseUpstream)
		}
	}()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream does not support flushing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		close(upstreamReady)

		select {
		case <-releaseUpstream:
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting to release upstream stream")
		}

		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	gatewayServer := httptest.NewServer(handler)
	defer gatewayServer.Close()

	req, err := http.NewRequest(http.MethodPost, gatewayServer.URL+"/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4.1","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("NewRequest returned error: %v", err)
	}
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := gatewayServer.Client().Do(req)
	if err != nil {
		t.Fatalf("Do returned error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d body=%s", resp.StatusCode, http.StatusOK, body)
	}

	select {
	case <-upstreamReady:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream stream to open")
	}

	firstChunk := make(chan string, 1)
	firstErr := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(resp.Body)
		var chunk strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				firstErr <- err
				return
			}
			chunk.WriteString(line)
			if strings.TrimSpace(line) == "" {
				firstChunk <- chunk.String()
				return
			}
		}
	}()

	select {
	case chunk := <-firstChunk:
		t.Fatalf("received gateway chunk before upstream output: %q", chunk)
	case err := <-firstErr:
		t.Fatalf("reading first chunk returned error: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	close(releaseUpstream)
	released = true

	var firstOutputChunk string
	select {
	case chunk := <-firstChunk:
		firstOutputChunk = chunk
	case err := <-firstErr:
		t.Fatalf("reading first output chunk returned error: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first gateway chunk after upstream output")
	}
	if !strings.Contains(firstOutputChunk, `"role":"assistant"`) {
		t.Fatalf("first output chunk = %q, want assistant role chunk", firstOutputChunk)
	}
	if strings.Contains(firstOutputChunk, `"content":"hello"`) {
		t.Fatalf("first output chunk should send role before content delta: %q", firstOutputChunk)
	}

	remaining, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll remaining response returned error: %v", err)
	}
	for _, want := range []string{`"content":"hello"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(string(remaining), want) {
			t.Fatalf("remaining response missing %q in %q", want, remaining)
		}
	}
}

func TestOpenAIStreamingPreOutputHTMLDoesNotLeakToClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: <body><div>Cloudflare challenge-platform</div></body>\n\n"))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4.1","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, forbidden := range []string{"<body>", "Cloudflare", "challenge-platform"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("stream leaked upstream HTML %q in %q", forbidden, body)
		}
	}
	for _, want := range []string{`"type":"` + gatewayProtocolErrorType + `"`, gatewayProtocolErrorMessage} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream response missing %q in %q", want, body)
		}
	}
	if strings.Contains(body, "upstream returned HTML challenge response") {
		t.Fatalf("stream leaked upstream error detail in %q", body)
	}
}

func TestOpenAIStreamingSuppressesUsageResponseButRequestsUpstreamUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		streamOptions, _ := body["stream_options"].(map[string]any)
		if streamOptions["include_usage"] != true {
			t.Fatalf("stream_options = %#v", body["stream_options"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4.1","stream":true,"stream_options":{"include_usage":false},"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if strings.Contains(body, `"usage"`) {
		t.Fatalf("did not expect usage chunk in %q", body)
	}
}

func TestOpenAIStreamingEmitsErrorEventAfterStreamStarts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {invalid json}\n\n"))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4.1","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"\"content\":\"hello\"",
		"\"error\":{",
		"\"type\":\"" + gatewayProtocolErrorType + "\"",
		gatewayProtocolErrorMessage,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q in %q", want, body)
		}
	}
	for _, leaked := range []string{"upstream_invalid_stream_chunk", "invalid character"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("stream response leaked upstream detail %q in %q", leaked, body)
		}
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Fatalf("did not expect DONE marker in %q", body)
	}
}

func TestAnthropicMessagesAliasPreservesRawContentBlocks(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path = %s, want /v1/messages", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "claude-token" {
			t.Fatalf("x-api-key = %q, want claude-token", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Fatalf("anthropic-version = %q, want 2023-06-01", got)
		}
		if got := r.Header.Get("anthropic-beta"); got != "tools-2024-04-04" {
			t.Fatalf("anthropic-beta = %q, want tools-2024-04-04", got)
		}
		if got := r.Header.Get("x-claude-code-agent-id"); got != "agent_123" {
			t.Fatalf("x-claude-code-agent-id = %q, want agent_123", got)
		}
		if got := r.Header.Get("x-claude-code-parent-agent-id"); got != "agent_parent_456" {
			t.Fatalf("x-claude-code-parent-agent-id = %q, want agent_parent_456", got)
		}
		if got := r.Header.Get("x-claude-code-session-id"); got != "session_789" {
			t.Fatalf("x-claude-code-session-id = %q, want session_789", got)
		}
		if got := r.Header.Get("x-client-request-id"); got != "claude_req_123" {
			t.Fatalf("x-client-request-id = %q, want claude_req_123", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal upstream body: %v", err)
		}
		if payload["model"] != "claude-sonnet-4-0" || payload["stream"] != false {
			t.Fatalf("upstream model/stream payload = %s", string(body))
		}
		messages, _ := payload["messages"].([]any)
		if len(messages) != 1 {
			t.Fatalf("messages payload = %s", string(body))
		}
		message, _ := messages[0].(map[string]any)
		content, _ := message["content"].([]any)
		if len(content) != 2 {
			t.Fatalf("content payload = %s", string(body))
		}
		imageBlock, _ := content[1].(map[string]any)
		if imageBlock["type"] != "image" {
			t.Fatalf("image block was not preserved: %s", string(body))
		}
		if _, ok := payload["tools"].([]any); !ok {
			t.Fatalf("tools were not preserved: %s", string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_raw","type":"message","role":"assistant","model":"claude-upstream-returned","content":[{"type":"text","text":"using tool"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x"}}],"stop_reason":"tool_use","usage":{"input_tokens":12,"output_tokens":4}}`))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderClaude,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "claude-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClientInGroup(t, repo, control, "gw_test_key", "Claude")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	body := `{"model":"claude-sonnet-4-0","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]}],"tools":[{"name":"lookup","description":"lookup docs","input_schema":{"type":"object"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("X-API-Key", "gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "tools-2024-04-04")
	req.Header.Set("x-claude-code-agent-id", "agent_123")
	req.Header.Set("x-claude-code-parent-agent-id", "agent_parent_456")
	req.Header.Set("x-claude-code-session-id", "session_789")
	req.Header.Set("x-client-request-id", "claude_req_123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, want := range []string{`"id":"msg_raw"`, `"model":"claude-upstream-returned"`, `"type":"tool_use"`, `"stop_reason":"tool_use"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("response missing %q in %q", want, rec.Body.String())
		}
	}
}

func TestOpenAIResponsesToClaudeReturnsUnsupported(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path = %s, want /v1/messages", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if payload["model"] != "claude-sonnet-4-0" {
			t.Fatalf("model = %v, want claude-sonnet-4-0", payload["model"])
		}
		if _, exists := payload["input"]; exists {
			t.Fatalf("Claude payload should not include OpenAI Responses input: %#v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_claude","type":"message","role":"assistant","model":"claude-sonnet-4-0","content":[{"type":"text","text":"hello from claude"}],"stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":4}}`))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "claude_primary",
		Provider: core.ProviderClaude,
		Label:    "Claude Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "claude-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClientInGroup(t, repo, control, "gw_test_key", "Claude")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"claude-sonnet-4-0","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadGateway, body)
	}
	for _, want := range []string{`"type":"` + gatewayProtocolErrorType + `"`, gatewayProtocolErrorMessage} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q in %s", want, body)
		}
	}
	for _, leaked := range []string{`responses_not_supported`, `provider \"claude\" does not support responses`} {
		if strings.Contains(body, leaked) {
			t.Fatalf("response leaked upstream detail %q in %s", leaked, body)
		}
	}
}

func TestOpenAIResponsesToClaudeToolsReturnUnsupported(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path = %s, want /v1/messages", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if _, ok := payload["tools"].([]any); !ok {
			t.Fatalf("tools were not translated: %#v", payload)
		}
		messages, _ := payload["messages"].([]any)
		foundToolResult := false
		for _, messageValue := range messages {
			message, _ := messageValue.(map[string]any)
			for _, blockValue := range message["content"].([]any) {
				block, _ := blockValue.(map[string]any)
				if block["type"] == "tool_result" && block["tool_use_id"] == "call_1" {
					foundToolResult = true
				}
			}
		}
		if !foundToolResult {
			t.Fatalf("tool result was not translated: %#v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_tool","type":"message","role":"assistant","model":"claude-sonnet-4-0","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x"}}],"stop_reason":"tool_use","usage":{"input_tokens":9,"output_tokens":3}}`))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "claude_primary",
		Provider: core.ProviderClaude,
		Label:    "Claude Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "claude-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClientInGroup(t, repo, control, "gw_test_key", "Claude")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	body := `{"model":"claude-sonnet-4-0","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"use tool"}]},{"type":"function_call_output","call_id":"call_1","output":"42"}],"tools":[{"type":"function","name":"lookup","description":"Lookup","parameters":{"type":"object"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	bodyText := rec.Body.String()
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadGateway, bodyText)
	}
	for _, want := range []string{`"type":"` + gatewayProtocolErrorType + `"`, gatewayProtocolErrorMessage} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("response missing %q in %s", want, bodyText)
		}
	}
	for _, leaked := range []string{`responses_not_supported`, `provider \"claude\" does not support responses`} {
		if strings.Contains(bodyText, leaked) {
			t.Fatalf("response leaked upstream detail %q in %s", leaked, bodyText)
		}
	}
}

func TestOpenAIResponsesStreamToClaudeReturnsUnsupported(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path = %s, want /v1/messages", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_stream\",\"model\":\"claude-sonnet-4-0\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello claude\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":8,\"output_tokens\":4}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "claude_primary",
		Provider: core.ProviderClaude,
		Label:    "Claude Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "claude-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClientInGroup(t, repo, control, "gw_test_key", "Claude")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"claude-sonnet-4-0","stream":true,"input":"hello"}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadGateway, body)
	}
	for _, want := range []string{`"type":"` + gatewayProtocolErrorType + `"`, gatewayProtocolErrorMessage} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q in %q", want, body)
		}
	}
	for _, leaked := range []string{`responses_streaming_not_supported`, `provider \"claude\" does not support responses streaming`} {
		if strings.Contains(body, leaked) {
			t.Fatalf("response leaked upstream detail %q in %q", leaked, body)
		}
	}
}

func TestAnthropicOfficialV1MessagesAliasUsesSameGatewayPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages":
			if got := r.Header.Get("x-api-key"); got != "claude-token" {
				t.Fatalf("x-api-key = %q, want claude-token", got)
			}
			if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
				t.Fatalf("anthropic-version = %q, want 2023-06-01", got)
			}
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode messages body: %v", err)
			}
			if payload["model"] != "claude-sonnet-4-0" || payload["stream"] != false {
				t.Fatalf("messages payload = %#v", payload)
			}
			if _, ok := payload["tools"].([]any); !ok {
				t.Fatalf("tools were not preserved: %#v", payload)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_root","type":"message","role":"assistant","model":"claude-sonnet-4-0","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`))
		case "/v1/messages/count_tokens":
			if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
				t.Fatalf("anthropic-version = %q, want 2023-06-01", got)
			}
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode count body: %v", err)
			}
			if payload["model"] != "claude-sonnet-4-0" {
				t.Fatalf("count payload = %#v", payload)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"input_tokens":9}`))
		default:
			t.Fatalf("path = %s, want Anthropic messages endpoint", r.URL.Path)
		}
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderClaude,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "claude-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClientInGroup(t, repo, control, "gw_test_key", "Claude")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	cases := []struct {
		name     string
		path     string
		body     string
		wantBody string
	}{
		{
			name:     "messages",
			path:     "/v1/messages",
			body:     `{"model":"claude-sonnet-4-0","max_tokens":64,"messages":[{"role":"user","content":"use tool"}],"tools":[{"name":"lookup","description":"lookup docs","input_schema":{"type":"object"}}]}`,
			wantBody: `"id":"msg_root"`,
		},
		{
			name:     "count tokens",
			path:     "/v1/messages/count_tokens",
			body:     `{"model":"claude-sonnet-4-0","messages":[{"role":"user","content":"hello"}]}`,
			wantBody: `"input_tokens":9`,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, bytes.NewBufferString(tt.body))
			req.Header.Set("Authorization", "Bearer gw_test_key")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("anthropic-version", "2023-06-01")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Fatalf("body = %s, want %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestAnthropicAPIPrefixMessagesAliasUsesSameGatewayPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path = %s, want /v1/messages", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if payload["model"] != "claude-sonnet-4-0" {
			t.Fatalf("model = %v", payload["model"])
		}
		if payload["max_tokens"] != float64(1024) {
			t.Fatalf("default max_tokens = %#v, want 1024", payload["max_tokens"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_api","type":"message","role":"assistant","model":"claude-sonnet-4-0","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderClaude,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "claude-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClientInGroup(t, repo, control, "gw_test_key", "Claude")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/messages", bytes.NewBufferString(`{"model":"claude-sonnet-4-0","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"id":"msg_api"`) {
		t.Fatalf("body = %s, want msg_api", rec.Body.String())
	}
}

func TestAnthropicProtocolRoutesUseAnthropicAuthErrors(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClientInGroup(t, repo, control, "gw_test_key", "Claude")

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	for _, path := range []string{"/v1/messages", "/anthropic/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{}`))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusUnauthorized, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"type":"error"`) || !strings.Contains(rec.Body.String(), `"type":"authentication_error"`) {
				t.Fatalf("body = %s, want Anthropic authentication error", rec.Body.String())
			}
		})
	}
}

func TestAnthropicProtocolRoutesUseAnthropicGatewayErrors(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClientInGroup(t, repo, control, "gw_test_key", "Claude")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"missing-model","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"type":"error"`) || !strings.Contains(rec.Body.String(), `"type":"invalid_request_error"`) {
		t.Fatalf("body = %s, want Anthropic invalid request error", rec.Body.String())
	}
}

func TestAnthropicStreamingFallbackUsesCompleteMessageStart(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s, want /v1/chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_1\",\"model\":\"gpt-4.1\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_1\",\"model\":\"gpt-4.1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "openai_primary",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"gpt-4.1","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: ping",
		"event: message_start",
		`"type":"message_start"`,
		`"type":"message"`,
		`"content":[]`,
		`"stop_reason":null`,
		`"usage":{"input_tokens":0,"output_tokens":0}`,
		`"text":"hello"`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q in %q", want, body)
		}
	}
}

func TestAnthropicStreamingEmitsSSEErrorAfterInitialPing(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClientInGroup(t, repo, control, "gw_test_key", "Claude")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"missing-model","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"event: ping", `"type":"ping"`, "event: error", `"type":"` + gatewayProtocolErrorType + `"`, gatewayProtocolErrorMessage} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q in %q", want, body)
		}
	}
	if strings.Contains(body, "missing-model") {
		t.Fatalf("stream leaked upstream model detail in %q", body)
	}
	if strings.Contains(body, `"invalid_request_error"`) {
		t.Fatalf("streaming error should be SSE after ping, got %q", body)
	}
}

func TestAnthropicMessagesToOpenAIGPT5UsesResponsesAndReturnsAnthropicEnvelope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s, want /v1/responses", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if payload["model"] != "gpt-5.5" {
			t.Fatalf("model = %v, want gpt-5.5", payload["model"])
		}
		if _, exists := payload["messages"]; exists {
			t.Fatalf("responses payload should not include Anthropic messages: %#v", payload)
		}
		if _, exists := payload["input"]; !exists {
			t.Fatalf("responses payload missing input: %#v", payload)
		}
		if metadata, _ := payload["metadata"].(map[string]any); metadata != nil {
			if _, exists := metadata["anthropic_version"]; exists {
				t.Fatalf("anthropic protocol metadata leaked into OpenAI metadata: %#v", metadata)
			}
			if _, exists := metadata["x-claude-code-agent-id"]; exists {
				t.Fatalf("claude code metadata leaked into OpenAI metadata: %#v", metadata)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_gpt55","object":"response","created_at":1710000000,"status":"completed","model":"gpt-5.5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello from responses"}]}],"usage":{"input_tokens":8,"output_tokens":4,"total_tokens":12}}`))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "openai_primary",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	if _, err := control.CreateModel(controlplane.ModelInput{ID: "gpt-5.5", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{core.DefaultAccountGroupName}}); err != nil {
		t.Fatalf("CreateModel returned error: %v", err)
	}
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"gpt-5.5","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-claude-code-agent-id", "agent_bridge")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"type":"message"`, `"model":"gpt-5.5"`, `"text":"hello from responses"`, `"stop_reason":"end_turn"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, `"object":"response"`) {
		t.Fatalf("Anthropic response leaked OpenAI raw body: %s", body)
	}
}

func TestAnthropicMessagesToOpenAIGPT5TranslatesTools(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s, want /v1/responses", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if _, ok := payload["tools"].([]any); !ok {
			t.Fatalf("tools were not translated: %#v", payload)
		}
		input, _ := payload["input"].([]any)
		foundToolOutput := false
		for _, itemValue := range input {
			item, _ := itemValue.(map[string]any)
			if item["type"] == "function_call_output" && item["call_id"] == "toolu_1" {
				foundToolOutput = true
			}
		}
		if !foundToolOutput {
			t.Fatalf("tool result was not translated: %#v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_tool","object":"response","created_at":1710000000,"status":"completed","model":"gpt-5.5","output":[{"id":"fc_1","type":"function_call","call_id":"call_2","name":"lookup","arguments":"{\"q\":\"x\"}"}],"usage":{"input_tokens":9,"output_tokens":3,"total_tokens":12}}`))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "openai_primary",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	if _, err := control.CreateModel(controlplane.ModelInput{ID: "gpt-5.5", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{core.DefaultAccountGroupName}}); err != nil {
		t.Fatalf("CreateModel returned error: %v", err)
	}
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	body := `{"model":"gpt-5.5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"use tool"},{"type":"tool_result","tool_use_id":"toolu_1","content":"42"}]}],"tools":[{"name":"lookup","description":"Lookup","input_schema":{"type":"object"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, want := range []string{`"type":"tool_use"`, `"id":"call_2"`, `"name":"lookup"`, `"stop_reason":"tool_use"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("response missing %q in %s", want, rec.Body.String())
		}
	}
}

func TestAnthropicStreamingToOpenAIGPT5RewrapsResponsesEvents(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s, want /v1/responses", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stream\",\"model\":\"gpt-5.5\"}}\n\n"))
		_, _ = w.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"usage\":{\"input_tokens\":8,\"output_tokens\":4,\"total_tokens\":12}}}\n\n"))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "openai_primary",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")
	if _, err := control.CreateModel(controlplane.ModelInput{ID: "gpt-5.5", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{core.DefaultAccountGroupName}}); err != nil {
		t.Fatalf("CreateModel returned error: %v", err)
	}
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"gpt-5.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: ping",
		"event: message_start",
		"event: content_block_start",
		`"text":"hello"`,
		"event: message_delta",
		`"stop_reason":"end_turn"`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q in %q", want, body)
		}
	}
	for _, forbidden := range []string{"event: response.created", "event: response.output_text.delta", "response.completed"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("response leaked OpenAI Responses event %q in %q", forbidden, body)
		}
	}
}

func TestAnthropicMessagesRequiresOfficialFields(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_test_key")

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "model",
			body: `{"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`,
			want: "model is required",
		},
		{
			name: "messages",
			body: `{"model":"claude-sonnet-4-0","max_tokens":64}`,
			want: "messages is required",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(tt.body))
			req.Header.Set("Authorization", "Bearer gw_test_key")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.want) {
				t.Fatalf("body = %s, want %q", rec.Body.String(), tt.want)
			}
		})
	}
}

func TestAnthropicCountTokensProxiesToClaude(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Fatalf("path = %s, want /v1/messages/count_tokens", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "claude-token" {
			t.Fatalf("x-api-key = %q, want claude-token", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Fatalf("anthropic-version = %q, want 2023-06-01", got)
		}
		if got := r.Header.Get("anthropic-beta"); got != "tools-2024-04-04" {
			t.Fatalf("anthropic-beta = %q, want tools-2024-04-04", got)
		}
		if got := r.Header.Get("x-claude-code-agent-id"); got != "agent_count_1" {
			t.Fatalf("x-claude-code-agent-id = %q, want agent_count_1", got)
		}
		if got := r.Header.Get("x-client-request-id"); got != "claude_count_req_1" {
			t.Fatalf("x-client-request-id = %q, want claude_count_req_1", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["model"] != "claude-sonnet-4-0" {
			t.Fatalf("model = %v", body["model"])
		}
		messages, _ := body["messages"].([]any)
		if len(messages) != 1 {
			t.Fatalf("messages = %#v", body["messages"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":17}`))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderClaude,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "claude-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClientInGroup(t, repo, control, "gw_test_key", "Claude")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages/count_tokens", bytes.NewBufferString(`{"model":"claude-sonnet-4-0","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "tools-2024-04-04")
	req.Header.Set("x-claude-code-agent-id", "agent_count_1")
	req.Header.Set("x-client-request-id", "claude_count_req_1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		InputTokens int               `json:"input_tokens"`
		Meta        map[string]string `json:"meta"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.InputTokens != 17 {
		t.Fatalf("input_tokens = %d", payload.InputTokens)
	}
	if payload.Meta != nil {
		t.Fatalf("meta should not be injected into Anthropic response: %#v", payload.Meta)
	}
}

func TestAnthropicCountTokensRequiresMessages(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClientInGroup(t, repo, control, "gw_test_key", "Claude")

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages/count_tokens", bytes.NewBufferString(`{"model":"claude-sonnet-4-0"}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "messages is required") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestAnthropicStreamingUsesUpstreamDeltas(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"message\":{\"id\":\"msg_stream\",\"model\":\"claude-sonnet-4-0\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"hello \"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"claude\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":12,\"output_tokens\":5}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderClaude,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "claude-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClientInGroup(t, repo, control, "gw_test_key", "Claude")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(`{"model":"claude-sonnet-4-0","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"event: message_start",
		"\"text\":\"hello \"",
		"\"text\":\"claude\"",
		"\"stop_reason\":\"end_turn\"",
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q in %q", want, body)
		}
	}
}

func TestAnthropicStreamingAliasPreservesRawEvents(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path = %s, want /v1/messages", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_stream\",\"model\":\"claude-sonnet-4-0\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"lookup\",\"input\":{}}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"input_tokens\":12,\"output_tokens\":5}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderClaude,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "claude-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClientInGroup(t, repo, control, "gw_test_key", "Claude")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(`{"model":"claude-sonnet-4-0","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"use tool"}]}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"\"type\":\"tool_use\"",
		"event: content_block_delta",
		"\"type\":\"input_json_delta\"",
		"\"stop_reason\":\"tool_use\"",
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q in %q", want, body)
		}
	}
}

func TestAnthropicStreamingEmitsErrorEventAfterStreamStarts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"message\":{\"id\":\"msg_stream\",\"model\":\"claude-sonnet-4-0\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_delta\ndata: {invalid json}\n\n"))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderClaude,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "claude-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClientInGroup(t, repo, control, "gw_test_key", "Claude")
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)

	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(`{"model":"claude-sonnet-4-0","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"event: message_start",
		"\"text\":\"hello\"",
		"event: error",
		"\"type\":\"" + gatewayProtocolErrorType + "\"",
		gatewayProtocolErrorMessage,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q in %q", want, body)
		}
	}
	if strings.Contains(body, "upstream_invalid_stream_chunk") || strings.Contains(body, "invalid character") {
		t.Fatalf("stream leaked upstream error detail in %q", body)
	}
	if strings.Contains(body, "event: message_stop") {
		t.Fatalf("did not expect message_stop in %q", body)
	}
}

func TestGatewayEndpointRejectsDisabledClient(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertClient(core.APIClient{
		ID:      "client_disabled",
		Name:    "Disabled",
		APIKey:  "gw_test_key",
		Enabled: false,
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer gw_test_key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if !strings.Contains(rec.Body.String(), "disabled") {
		t.Fatalf("expected disabled error body, got %q", rec.Body.String())
	}
}

func TestClientEditUpdatesSpendLimitAndKey(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "admin_user", Username: "admin", Role: core.UserRoleAdmin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{
			ID:       "acct_openai",
			Provider: core.ProviderOpenAI,
			Label:    "OpenAI",
			Status:   core.AccountStatusActive,
			Group:    "Plus",
		},
		{
			ID:       "acct_claude",
			Provider: core.ProviderClaude,
			Label:    "Claude",
			Status:   core.AccountStatusActive,
			Group:    "Plus",
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:                "client_edit",
		Name:              "Client A",
		APIKey:            "old_key",
		OwnerUserID:       "admin_user",
		Enabled:           true,
		SpendLimitNanoUSD: 100 * core.NanoUSDPerUSD,
		RoutePolicy:       core.DefaultRoutePolicy(),
		AccountGroup:      "Plus",
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	form := strings.NewReader("name=Client+B&spend_limit_usd=250&account_group=Plus&enabled=on")
	req := httptest.NewRequest(http.MethodPost, "/clients/client_edit/edit", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}

	client, err := repo.GetClient("client_edit")
	if err != nil {
		t.Fatal(err)
	}
	if client.Name != "Client B" {
		t.Fatalf("name = %q", client.Name)
	}
	if client.APIKey != "old_key" {
		t.Fatalf("api key = %q", client.APIKey)
	}
	if client.SpendLimitNanoUSD != 250*core.NanoUSDPerUSD {
		t.Fatalf("client spend limit = %d", client.SpendLimitNanoUSD)
	}
	routeProviders := append([]core.ProviderKind{client.RoutePolicy.DefaultProvider}, client.RoutePolicy.FallbackProviders...)
	if len(routeProviders) != 2 || !containsProvider(routeProviders, core.ProviderOpenAI) || !containsProvider(routeProviders, core.ProviderClaude) {
		t.Fatalf("route providers = %#v", routeProviders)
	}
	if client.AccountGroup != "Plus" {
		t.Fatalf("account group = %q", client.AccountGroup)
	}
}

func TestUserClientEditRendersAndUpdatesSpendLimit(t *testing.T) {
	repo := storage.NewMemoryRepository()
	user := core.User{ID: "user_client", Username: "alice", Role: core.UserRoleUser, Enabled: true}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_openai",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI",
		Status:   core.AccountStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:                "client_user",
		Name:              "Client A",
		APIKey:            "old_key",
		OwnerUserID:       user.ID,
		Enabled:           true,
		SpendLimitNanoUSD: 100 * core.NanoUSDPerUSD,
		RoutePolicy:       core.DefaultRoutePolicy(),
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/clients/client_user/edit", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="spend_limit_usd"`) || !strings.Contains(body, `value="100"`) {
		t.Fatalf("response missing spend limit field: %s", body)
	}

	form := url.Values{}
	form.Set("name", "Client B")
	form.Set("spend_limit_usd", "25.5")
	form.Set("account_group", core.DefaultAccountGroupName)
	form.Set("enabled", "on")
	req = httptest.NewRequest(http.MethodPost, "/clients/client_user/edit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	client, err := repo.GetClient("client_user")
	if err != nil {
		t.Fatal(err)
	}
	if client.Name != "Client B" {
		t.Fatalf("name = %q", client.Name)
	}
	if client.SpendLimitNanoUSD != 25500*core.NanoUSDPerUSD/1000 {
		t.Fatalf("client spend limit = %d", client.SpendLimitNanoUSD)
	}

	form = url.Values{}
	form.Set("name", "Client C")
	form.Set("spend_limit_usd", "7.25")
	form.Set("account_group", core.DefaultAccountGroupName)
	form.Set("enabled", "on")
	req = httptest.NewRequest(http.MethodPost, "/clients/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	var created core.APIClient
	for _, candidate := range repo.ListClients() {
		if candidate.Name == "Client C" {
			created = candidate
			break
		}
	}
	if created.ID == "" {
		t.Fatal("created client not found")
	}
	if created.OwnerUserID != user.ID {
		t.Fatalf("created owner = %q", created.OwnerUserID)
	}
	if created.SpendLimitNanoUSD != 7250*core.NanoUSDPerUSD/1000 {
		t.Fatalf("created spend limit = %d", created.SpendLimitNanoUSD)
	}
}

func TestClientEditPageRendersAccountGroupOptions(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "admin_user", Username: "admin", Role: core.UserRoleAdmin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{
			ID:       "acct_openai",
			Provider: core.ProviderOpenAI,
			Label:    "OpenAI",
			Status:   core.AccountStatusActive,
			Group:    "Plus",
		},
		{
			ID:       "acct_claude",
			Provider: core.ProviderClaude,
			Label:    "Claude",
			Status:   core.AccountStatusActive,
			Group:    "Plus",
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:           "client_edit",
		Name:         "Client A",
		APIKey:       "old_key",
		OwnerUserID:  "admin_user",
		Enabled:      true,
		RoutePolicy:  core.DefaultRoutePolicy(),
		AccountGroup: "Plus",
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/clients/client_edit/edit", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"name=\"account_group\"",
		"value=\"Plus\" selected",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q", want)
		}
	}
	for _, unwanted := range []string{
		"name=\"allowed_account_ids\"",
		"name=\"default_provider\"",
		"name=\"fallback_provider\"",
		"name=\"api_key\"",
	} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("response contains removed field %q", unwanted)
		}
	}
}

func TestClientEditPageRendersEffectiveTimedAccountGroupMultiplier(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "admin_user", Username: "admin", Role: core.UserRoleAdmin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                   "default",
		Name:                 "",
		BillingMultiplierBps: 10000,
		TimedMultipliers: []core.AccountGroupTimedMultiplier{
			{ID: "half", Name: "限时活动", Enabled: true, MultiplierBps: 5000},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:          "client_edit",
		Name:        "Client A",
		APIKey:      "old_key",
		OwnerUserID: "admin_user",
		Enabled:     true,
		RoutePolicy: core.DefaultRoutePolicy(),
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/clients/client_edit/edit", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Default ([限时活动] 0.5x)") {
		t.Fatalf("response missing effective timed multiplier: %s", body)
	}
}

func TestClientDeleteRemovesClient(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "admin_user", Username: "admin", Role: core.UserRoleAdmin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:          "client_a",
		Name:        "Client A",
		APIKey:      "gw_key",
		OwnerUserID: "admin_user",
		Enabled:     true,
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/clients/client_a/delete", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if _, err := repo.GetClient("client_a"); err == nil {
		t.Fatal("expected client to be deleted")
	}
}

func TestClientDeleteWithBillingHistoryPreservesFinanceRecords(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "admin_user", Username: "admin", Role: core.UserRoleAdmin, Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:          "client_history",
		Name:        "Client History",
		APIKey:      "gw_history",
		OwnerUserID: "admin_user",
		Enabled:     true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_client_history",
		ClientID:        "client_history",
		ClientName:      "Client History",
		UserID:          "admin_user",
		Model:           "gpt-4.1",
		ReservedNanoUSD: 100,
		Fingerprint:     "req_client_history",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_client_history",
		ClientID:      "client_history",
		Model:         "gpt-4.1",
		ActualNanoUSD: 100,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	pageReq := httptest.NewRequest(http.MethodGet, "/clients", nil)
	pageRec := httptest.NewRecorder()
	handler.ServeHTTP(pageRec, pageReq)
	if pageRec.Code != http.StatusOK {
		t.Fatalf("clients status = %d, want %d body=%s", pageRec.Code, http.StatusOK, pageRec.Body.String())
	}
	body := pageRec.Body.String()
	if !strings.Contains(body, `action="/clients/client_history/delete"`) {
		t.Fatalf("delete form missing for client with billing history: %s", body)
	}
	if strings.Contains(body, `disabled`) {
		t.Fatalf("delete button should remain clickable: %s", body)
	}

	req := httptest.NewRequest(http.MethodPost, "/clients/client_history/delete", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if location != "/clients" {
		t.Fatalf("delete location = %q, want /clients", location)
	}
	if _, err := repo.GetClient("client_history"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetClient err = %v, want ErrNotFound", err)
	}
	requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: "client_history", Limit: 10})
	if total != 1 || len(requests) != 1 || requests[0].RequestID != "req_client_history" {
		t.Fatalf("billing records after client delete = total %d items %#v, want preserved req_client_history", total, requests)
	}
}

func mustSeedProtocolClient(t *testing.T, control *controlplane.Service, apiKey string) {
	t.Helper()
	if _, _, err := control.EnsureProtocolClient(apiKey); err != nil {
		t.Fatalf("EnsureProtocolClient returned error: %v", err)
	}
	claudeGroupExists := false
	for _, group := range control.ListAccountGroups() {
		if strings.EqualFold(group.Name, "Claude") {
			claudeGroupExists = true
			break
		}
	}
	if !claudeGroupExists {
		if _, err := control.CreateAccountGroup("Claude", core.AccountGroupTypeClaude); err != nil {
			t.Fatalf("CreateAccountGroup(Claude) returned error: %v", err)
		}
	}
	for _, model := range []controlplane.ModelInput{
		{ID: "gpt-4.1", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{core.DefaultAccountGroupName}},
		{ID: "text-embedding-3-small", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{core.DefaultAccountGroupName}},
		{ID: "claude-sonnet-4-0", Provider: core.ProviderClaude, Enabled: true, VisibleGroups: []string{"Claude"}},
	} {
		if _, err := control.CreateModel(model); err != nil {
			if strings.Contains(err.Error(), "is not registered") {
				continue
			}
			t.Fatalf("CreateModel(%s) returned error: %v", model.ID, err)
		}
	}

}

func mustSeedProtocolClientInGroup(t *testing.T, repo storage.Repository, control *controlplane.Service, apiKey, group string) {
	t.Helper()
	mustSeedProtocolClient(t, control, apiKey)
	var provider core.ProviderKind
	switch strings.ToLower(strings.TrimSpace(group)) {
	case "claude":
		provider = core.ProviderClaude
	case strings.ToLower(core.DefaultAccountGroupName), "openai":
		provider = core.ProviderOpenAI
	}
	for _, account := range control.ListAccounts() {
		if provider != "" && account.Provider != provider {
			continue
		}
		if strings.EqualFold(account.Group, group) {
			continue
		}
		account.Group = group
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatalf("UpsertAccount returned error: %v", err)
		}
	}
	client, err := control.GetClient("client_default")
	if err != nil {
		t.Fatalf("GetClient returned error: %v", err)
	}
	client.AccountGroup = group
	switch provider {
	case core.ProviderClaude:
		client.RoutePolicy.DefaultProvider = core.ProviderClaude
		client.RoutePolicy.FallbackProviders = nil
	case core.ProviderOpenAI:
		client.RoutePolicy.DefaultProvider = core.ProviderOpenAI
		client.RoutePolicy.FallbackProviders = nil
	}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
}

const testConsoleCSRFToken = "test-admin-csrf-token"

func authenticatedAdminHandler(t *testing.T, control *controlplane.Service, handler http.Handler) http.Handler {
	t.Helper()
	var sessionCookie *http.Cookie
	csrfCookie := &http.Cookie{Name: consoleCSRFCookieName, Value: testConsoleCSRFToken, Path: "/"}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAdminTestRequest(r) {
			handler.ServeHTTP(w, r)
			return
		}
		if sessionCookie == nil {
			sessionCookie = &http.Cookie{Name: consoleSessionCookieName, Value: mustConsoleSessionToken(t, control), Path: "/"}
		}
		http.SetCookie(w, sessionCookie)
		http.SetCookie(w, csrfCookie)
		if _, err := r.Cookie(consoleSessionCookieName); err != nil {
			r.AddCookie(sessionCookie)
		}
		if _, err := r.Cookie(consoleCSRFCookieName); err != nil {
			r.AddCookie(csrfCookie)
		}
		if isStateChangingMethod(r.Method) && strings.TrimSpace(r.Header.Get("X-CSRF-Token")) == "" {
			r.Header.Set("X-CSRF-Token", testConsoleCSRFToken)
		}
		handler.ServeHTTP(w, r)
	})
}

func isAdminTestRequest(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	return r.URL.Path == "/" || r.URL.Path == "/login" || r.URL.Path == "/logout" || r.URL.Path == "/clients" || strings.HasPrefix(r.URL.Path, "/clients/") || r.URL.Path == "/models" || r.URL.Path == "/images" || strings.HasPrefix(r.URL.Path, "/images/") || r.URL.Path == "/payments" || strings.HasPrefix(r.URL.Path, "/payments/") || r.URL.Path == "/messages" || strings.HasPrefix(r.URL.Path, "/messages/") || strings.HasPrefix(r.URL.Path, "/admin/")
}

func authenticatedUserHandler(t *testing.T, control *controlplane.Service, user core.User, handler http.Handler) http.Handler {
	t.Helper()
	sessionToken, _, err := control.CreateUserSession(user.ID)
	if err != nil {
		t.Fatalf("CreateUserSession returned error: %v", err)
	}
	sessionCookie := &http.Cookie{Name: consoleSessionCookieName, Value: sessionToken, Path: "/"}
	csrfCookie := &http.Cookie{Name: consoleCSRFCookieName, Value: testConsoleCSRFToken, Path: "/"}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, sessionCookie)
		http.SetCookie(w, csrfCookie)
		if _, err := r.Cookie(consoleSessionCookieName); err != nil {
			r.AddCookie(sessionCookie)
		}
		if _, err := r.Cookie(consoleCSRFCookieName); err != nil {
			r.AddCookie(csrfCookie)
		}
		if isStateChangingMethod(r.Method) && strings.TrimSpace(r.Header.Get("X-CSRF-Token")) == "" {
			r.Header.Set("X-CSRF-Token", testConsoleCSRFToken)
		}
		handler.ServeHTTP(w, r)
	})
}

func mustConsoleSessionToken(t *testing.T, control *controlplane.Service) string {
	t.Helper()
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	sessionToken, _, err := control.CreateUserSession(admin.ID)
	if err != nil {
		t.Fatalf("CreateUserSession returned error: %v", err)
	}
	return sessionToken
}

func TestClientRoutePolicyControlsProviderSelection(t *testing.T) {
	openAIUpstream := newJSONUpstream(
		t,
		"/v1/chat/completions",
		"Authorization",
		"Bearer openai-token",
		http.StatusOK,
		`{"id":"chatcmpl_openai","model":"custom-model","created":1710000000,"choices":[{"message":{"content":"openai route"},"finish_reason":"stop"}],"usage":{"prompt_tokens":6,"completion_tokens":2,"total_tokens":8}}`,
	)
	defer openAIUpstream.Close()
	claudeUpstream := newJSONUpstream(
		t,
		"/v1/messages",
		"x-api-key",
		"claude-token",
		http.StatusOK,
		`{"id":"msg_claude","model":"custom-model","content":[{"type":"text","text":"claude route"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3}}`,
	)
	defer claudeUpstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_openai",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": openAIUpstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_claude",
		Provider: core.ProviderClaude,
		Label:    "Claude",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "claude-token",
			Metadata:    map[string]string{"base_url": claudeUpstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:      "client_route",
		Name:    "Route Client",
		APIKey:  "gw_test_key",
		Enabled: true,
		RoutePolicy: core.RoutePolicy{
			DefaultProvider:   core.ProviderClaude,
			FallbackProviders: []core.ProviderKind{core.ProviderOpenAI},
			Rules:             core.DefaultRoutePolicy().Rules,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:            "custom-model",
		Provider:      core.ProviderClaude,
		UpstreamID:    "custom-model",
		Enabled:       true,
		VisibleGroups: []string{core.DefaultAccountGroupName},
		Source:        core.ModelSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)
	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"custom-model","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer gw_test_key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var payload struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Choices) != 1 || payload.Choices[0].Message.Content != "claude route" {
		t.Fatalf("choices = %#v", payload.Choices)
	}
}

func TestDashboardRequiresAdminCredentials(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if location := rec.Header().Get("Location"); !strings.HasPrefix(location, "/login") {
		t.Fatalf("Location = %q, want login redirect", location)
	}
}

func TestDashboardAcceptsAdminCredentials(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if _, ok := responseCookie(rec.Result(), consoleCSRFCookieName); !ok {
		t.Fatal("expected csrf cookie")
	}
}

func TestLoginFailurePreservesUsernameAndShowsPasswordToggle(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	getReq := httptest.NewRequest(http.MethodGet, "/login", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	csrfCookie, ok := responseCookie(getRec.Result(), consoleCSRFCookieName)
	if !ok {
		t.Fatal("expected csrf cookie")
	}

	form := url.Values{}
	form.Set("csrf_token", csrfCookie.Value)
	form.Set("username", "admin")
	form.Set("password", "wrong")
	postReq := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.AddCookie(csrfCookie)
	postRec := httptest.NewRecorder()
	handler.ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", postRec.Code, http.StatusSeeOther)
	}
	if location := postRec.Header().Get("Location"); !strings.HasPrefix(location, "/login?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("location = %q, want login notice redirect", location)
	}
}

func TestLoginFailureRateLimitBlocksRepeatedFailures(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	getReq := httptest.NewRequest(http.MethodGet, "/login", nil)
	getReq.RemoteAddr = "203.0.113.10:1234"
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	csrfCookie, ok := responseCookie(getRec.Result(), consoleCSRFCookieName)
	if !ok {
		t.Fatal("expected csrf cookie")
	}

	submit := func(password string) *httptest.ResponseRecorder {
		form := url.Values{}
		form.Set("csrf_token", csrfCookie.Value)
		form.Set("username", "admin")
		form.Set("password", password)
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.RemoteAddr = "203.0.113.10:1234"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(csrfCookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	notice := func(rec *httptest.ResponseRecorder) string {
		t.Helper()
		location := rec.Header().Get("Location")
		parsed, err := url.Parse(location)
		if err != nil {
			t.Fatalf("parse Location %q: %v", location, err)
		}
		return parsed.Query().Get("notice_error")
	}

	for i := 0; i < loginFailureUserLimit; i++ {
		rec := submit("wrong-secret")
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("failure %d status = %d, want %d", i+1, rec.Code, http.StatusSeeOther)
		}
		if got, want := notice(rec), translate(localeEN, "login_failed"); got != want {
			t.Fatalf("failure %d notice = %q, want %q", i+1, got, want)
		}
	}

	rec := submit("admin-secret")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("limited status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if got, want := notice(rec), translate(localeEN, "login_rate_limited"); got != want {
		t.Fatalf("limited notice = %q, want %q", got, want)
	}
	if _, ok := responseCookie(rec.Result(), consoleSessionCookieName); ok {
		t.Fatal("rate-limited login should not create a session")
	}
}

func TestLoginPageShowsEnabledOAuthProviders(t *testing.T) {
	repo := storage.NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.OAuth.GitHubLoginEnabled = true
	settings.OAuth.GitHubLoginClientID = "github-client"
	settings.OAuth.GitHubLoginSecret = "github-secret"
	settings.OAuth.GoogleLoginEnabled = true
	settings.OAuth.GoogleLoginClientID = "google-client"
	settings.OAuth.GoogleLoginSecret = "google-secret"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatalf("UpsertSystemSettings returned error: %v", err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/login?next=%2Fclients", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"/login/oauth/github?next=%2Fclients", "/login/oauth/google?next=%2Fclients", "Sign in with GitHub", "Sign in with Google"} {
		if !strings.Contains(body, want) {
			t.Fatalf("login page missing %q: %s", want, body)
		}
	}
}

func TestLoginOAuthStartRedirectsToProvider(t *testing.T) {
	repo := storage.NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.OAuth.GitHubLoginEnabled = true
	settings.OAuth.GitHubLoginClientID = "github-client"
	settings.OAuth.GitHubLoginSecret = "github-secret"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatalf("UpsertSystemSettings returned error: %v", err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/login/oauth/github?next=%2Fclients", nil)
	req.Host = "auth.example.com"
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	for _, want := range []string{"https://github.com/login/oauth/authorize?", "client_id=github-client", "redirect_uri=https%3A%2F%2Fauth.example.com%2Flogin%2Foauth%2Fgithub%2Fcallback", "scope=read%3Auser+user%3Aemail"} {
		if !strings.Contains(location, want) {
			t.Fatalf("Location missing %q: %s", want, location)
		}
	}
	if _, ok := responseCookie(rec.Result(), loginOAuthStateCookieName); !ok {
		t.Fatal("expected oauth state cookie")
	}
}

func TestLoginOAuthStartUsesPublicBaseURL(t *testing.T) {
	repo := storage.NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.Runtime.PublicBaseURL = "https://example.com"
	settings.OAuth.GoogleLoginEnabled = true
	settings.OAuth.GoogleLoginClientID = "google-client"
	settings.OAuth.GoogleLoginSecret = "google-secret"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatalf("UpsertSystemSettings returned error: %v", err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/login/oauth/google?next=%2Fclients", nil)
	req.Host = "wrong.example.com"
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	want := "redirect_uri=https%3A%2F%2Fexample.com%2Flogin%2Foauth%2Fgoogle%2Fcallback"
	if !strings.Contains(location, want) {
		t.Fatalf("Location missing %q: %s", want, location)
	}
	if strings.Contains(location, "wrong.example.com") {
		t.Fatalf("Location should not use request host when PublicBaseURL is configured: %s", location)
	}
}

func TestLoginOAuthUserInputIncludesRegistrationMetadata(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/login/oauth/github/callback?state=login-state&code=test-code", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.88")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("Accept-Encoding", "gzip")

	input := loginOAuthUserInputFromRequest(loginOAuthProfile{
		Provider:      "github",
		Subject:       "12345",
		Email:         "alice@example.com",
		EmailVerified: true,
		Username:      "alice",
	}, req)

	expectedFingerprintSum := sha256.Sum256([]byte("Mozilla/5.0\nzh-CN\ngzip"))
	expectedFingerprint := hex.EncodeToString(expectedFingerprintSum[:])
	if input.RegistrationIP != "203.0.113.88" || input.RegistrationBrowserFingerprint != expectedFingerprint {
		t.Fatalf("registration metadata = ip %q fingerprint %q", input.RegistrationIP, input.RegistrationBrowserFingerprint)
	}
}

func TestLoginOAuthCallbackIgnoresStaleProfileOAuthCookie(t *testing.T) {
	repo := storage.NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.OAuth.GitHubLoginEnabled = true
	settings.OAuth.GitHubLoginClientID = "github-client"
	settings.OAuth.GitHubLoginSecret = "github-secret"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatalf("UpsertSystemSettings returned error: %v", err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/login/oauth/github/callback?state=login-state", nil)
	req.AddCookie(&http.Cookie{
		Name:  loginOAuthStateCookieName,
		Value: "github|login-state|" + base64.RawURLEncoding.EncodeToString([]byte("/clients")),
		Path:  "/login/oauth/",
	})
	req.AddCookie(&http.Cookie{
		Name:  profileOAuthStateCookieName,
		Value: "github|stale-profile-state|" + base64.RawURLEncoding.EncodeToString([]byte("/profile/oauth")),
		Path:  "/",
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if strings.HasPrefix(location, "/profile/oauth") {
		t.Fatalf("stale profile oauth cookie should not route login callback to profile flow: %s", location)
	}
	if !strings.HasPrefix(location, "/login?notice_error=") {
		t.Fatalf("expected login flow error redirect after token exchange attempt, got %q", location)
	}
}

func TestLoginOAuthStartRedirectsAuthenticatedUser(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "alice",
		Password: "alice-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/login/oauth/github?next=%2Fclients", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/clients" {
		t.Fatalf("Location = %q, want /clients", got)
	}
}

func TestLoginOAuthStateCookieUsesForwardedHTTPS(t *testing.T) {
	repo := storage.NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.OAuth.GoogleLoginEnabled = true
	settings.OAuth.GoogleLoginClientID = "google-client"
	settings.OAuth.GoogleLoginSecret = "google-secret"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatalf("UpsertSystemSettings returned error: %v", err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/login/oauth/google?next=%2Fclients%3Ftab%3Done", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	cookie, ok := responseCookie(rec.Result(), loginOAuthStateCookieName)
	if !ok {
		t.Fatal("expected oauth state cookie")
	}
	if !cookie.Secure {
		t.Fatal("oauth state cookie Secure = false, want true")
	}
	_, next, ok := loginOAuthStateFromCookie(&http.Request{Header: http.Header{"Cookie": []string{cookie.String()}}}, "google")
	if !ok || next != "/clients?tab=one" {
		t.Fatalf("oauth state next = %q ok=%t", next, ok)
	}
}

func TestLoginInvalidCSRFShowsFormError(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	getReq := httptest.NewRequest(http.MethodGet, "/login", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	csrfCookie, ok := responseCookie(getRec.Result(), consoleCSRFCookieName)
	if !ok {
		t.Fatal("expected csrf cookie")
	}

	form := url.Values{}
	form.Set("csrf_token", "stale-token")
	form.Set("username", "admin")
	form.Set("password", "admin-secret")
	postReq := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.AddCookie(csrfCookie)
	postRec := httptest.NewRecorder()
	handler.ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", postRec.Code, http.StatusSeeOther, postRec.Body.String())
	}
	if location := postRec.Header().Get("Location"); !strings.HasPrefix(location, "/login?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("location = %q, want login notice redirect", location)
	}
}

func TestConsoleInvalidCSRFRedirectsWithNotice(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	sessionToken := mustConsoleSessionToken(t, control)
	form := url.Values{}
	form.Set("csrf_token", "stale-token")
	req := httptest.NewRequest(http.MethodPost, "/clients/missing/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", "http://example.com/clients")
	req.AddCookie(&http.Cookie{Name: consoleSessionCookieName, Value: sessionToken, Path: "/"})
	req.AddCookie(&http.Cookie{Name: consoleCSRFCookieName, Value: testConsoleCSRFToken, Path: "/"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/clients?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("location = %q, want clients notice redirect", location)
	}

	noticeReq := httptest.NewRequest(http.MethodGet, location, nil)
	noticeReq.AddCookie(&http.Cookie{Name: consoleSessionCookieName, Value: sessionToken, Path: "/"})
	noticeReq.AddCookie(&http.Cookie{Name: consoleCSRFCookieName, Value: testConsoleCSRFToken, Path: "/"})
	noticeRec := httptest.NewRecorder()
	handler.ServeHTTP(noticeRec, noticeReq)
	if noticeRec.Code != http.StatusOK {
		t.Fatalf("notice status = %d, want %d", noticeRec.Code, http.StatusOK)
	}
	body := noticeRec.Body.String()
	if !strings.Contains(body, `data-clear-url-params="notice_error"`) || !strings.Contains(body, "Form expired. Please try again.") {
		t.Fatalf("notice missing csrf error: %s", body)
	}
}

func TestUserCanChangeOwnPassword(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	alice, err := control.CreateUser(controlplane.UserInput{
		Username: "alice",
		Password: "old-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, alice, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, want := range []string{`data-group-settings-open="password-modal"`, `id="password-modal"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("dashboard should render password modal content %q: %s", want, rec.Body.String())
		}
	}
	for _, want := range []string{`data-group-settings-open="oauth-bind-modal"`, `id="oauth-bind-modal"`, `return_to`, "Third-party Accounts", "GitHub", "Google"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("dashboard should render third-party account binding modal content %q: %s", want, rec.Body.String())
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/profile/password", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("password page status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	mainBody := body
	if parts := strings.SplitN(body, `<main class="workspace">`, 2); len(parts) == 2 {
		mainBody = parts[1]
	}
	for _, want := range []string{"Change Password", `name="current_password"`, `name="new_password"`, `name="new_password_confirm"`, `data-password-toggle`} {
		if !strings.Contains(mainBody, want) {
			t.Fatalf("password page missing %q: %s", want, body)
		}
	}
	if strings.Contains(mainBody, "Third-party Accounts") || strings.Contains(mainBody, `id="oauth-bind"`) {
		t.Fatalf("password page should not render third-party account binding section: %s", body)
	}

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("current_password", "wrong-secret")
	form.Set("new_password", "new-secret")
	form.Set("new_password_confirm", "new-secret")
	req = httptest.NewRequest(http.MethodPost, "/profile/password", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "fetch")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong current status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Current password is invalid.") {
		t.Fatalf("wrong current response missing validation message: %s", rec.Body.String())
	}
	if _, err := control.AuthenticateUser("alice", "old-secret"); err != nil {
		t.Fatalf("old password should still work after failed change: %v", err)
	}

	form.Set("current_password", "old-secret")
	form.Set("new_password_confirm", "different-secret")
	req = httptest.NewRequest(http.MethodPost, "/profile/password", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "fetch")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatch status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Passwords do not match.") {
		t.Fatalf("mismatch response missing validation message: %s", rec.Body.String())
	}

	form.Set("new_password_confirm", "new-secret")
	req = httptest.NewRequest(http.MethodPost, "/profile/password", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "fetch")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("change status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) || !strings.Contains(rec.Body.String(), "Password updated.") {
		t.Fatalf("change response should be JSON success: %s", rec.Body.String())
	}
	if _, err := control.AuthenticateUser("alice", "old-secret"); !errors.Is(err, controlplane.ErrInvalidCredentials) {
		t.Fatalf("old password err = %v, want invalid credentials", err)
	}
	if _, err := control.AuthenticateUser("alice", "new-secret"); err != nil {
		t.Fatalf("new password should authenticate: %v", err)
	}

	req = httptest.NewRequest(http.MethodGet, "/profile/password?saved=1", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("saved page status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-clear-url-params="saved"`) {
		t.Fatalf("password saved notice should clear transient URL params: %s", rec.Body.String())
	}

	form.Set("current_password", "new-secret")
	form.Set("new_password", "newer-secret")
	form.Set("new_password_confirm", "newer-secret")
	form.Set("return_to", "/?open_modal=password-modal")
	req = httptest.NewRequest(http.MethodPost, "/profile/password", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("modal password redirect status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); location != "/?open_modal=password-modal&saved=1" && location != "/?saved=1&open_modal=password-modal" {
		t.Fatalf("modal password redirect Location = %q", location)
	}
	req = httptest.NewRequest(http.MethodGet, rec.Header().Get("Location"), nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("modal password notice status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Password updated.") {
		t.Fatalf("modal password saved page should render password notice: %s", rec.Body.String())
	}
}

func TestDefaultAdminMustChangePasswordBeforeUsingConsole(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")

	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{}))
	admin, created, err := control.EnsureAdminUser("", "")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	if !created || !admin.ForcePasswordChange {
		t.Fatalf("admin created=%v ForcePasswordChange=%v, want forced default admin", created, admin.ForcePasswordChange)
	}
	token, _, err := control.CreateUserSession(admin.ID)
	if err != nil {
		t.Fatalf("CreateUserSession returned error: %v", err)
	}
	sessionCookie := &http.Cookie{Name: consoleSessionCookieName, Value: token, Path: "/"}
	csrfCookie := &http.Cookie{Name: consoleCSRFCookieName, Value: testConsoleCSRFToken, Path: "/"}
	server := NewServerWithOptions(control, nil, ServerOptions{
		StatePath: statePath,
	})
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	req.AddCookie(sessionCookie)
	req.AddCookie(csrfCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); !strings.HasPrefix(location, "/profile/password?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("location = %q, want forced password redirect", location)
	}

	req = httptest.NewRequest(http.MethodGet, "/profile/password", nil)
	req.AddCookie(sessionCookie)
	req.AddCookie(csrfCookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("password page status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Change the default admin password before continuing.") {
		t.Fatalf("password page missing force-change notice: %s", rec.Body.String())
	}

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("current_password", "toor")
	form.Set("new_password", "admin-secret")
	form.Set("new_password_confirm", "admin-secret")
	req = httptest.NewRequest(http.MethodPost, "/profile/password", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sessionCookie)
	req.AddCookie(csrfCookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("password update status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); location != "/admin/settings" {
		t.Fatalf("password update Location = %q, want /admin/settings", location)
	}
	updated, err := control.GetUser(admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ForcePasswordChange {
		t.Fatal("ForcePasswordChange should be cleared after password update")
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	req.AddCookie(sessionCookie)
	req.AddCookie(csrfCookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("accounts status after password change = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestProfileOAuthPageRendersBindingState(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	settings := core.DefaultSystemSettings()
	settings.OAuth.GitHubLoginEnabled = true
	settings.OAuth.GitHubLoginClientID = "github-client"
	settings.OAuth.GitHubLoginSecret = "github-secret"
	if _, err := control.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "alice",
		Password: "secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if _, err := control.LinkOAuthIdentity(user.ID, controlplane.OAuthUserInput{
		Provider: "google",
		Subject:  "google-subject",
		Email:    "alice@example.com",
	}); err != nil {
		t.Fatalf("LinkOAuthIdentity returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/profile/oauth", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("profile oauth page status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	mainBody := body
	if parts := strings.SplitN(body, `<main class="workspace">`, 2); len(parts) == 2 {
		mainBody = parts[1]
	}
	for _, want := range []string{"Third-party Accounts", `/profile/oauth/github`, `/profile/oauth/google/unlink`, "alice@example.com"} {
		if !strings.Contains(mainBody, want) {
			t.Fatalf("profile oauth page missing %q: %s", want, body)
		}
	}
	if strings.Contains(mainBody, `name="current_password"`) || strings.Contains(mainBody, `name="new_password"`) {
		t.Fatalf("profile oauth page should not render password fields: %s", body)
	}
}

func TestProfileOAuthStartUsesLoginCallback(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	settings := core.DefaultSystemSettings()
	settings.Runtime.PublicBaseURL = "https://example.com"
	settings.OAuth.GitHubLoginEnabled = true
	settings.OAuth.GitHubLoginClientID = "github-client"
	settings.OAuth.GitHubLoginSecret = "github-secret"
	settings.OAuth.GoogleLoginEnabled = true
	settings.OAuth.GoogleLoginClientID = "google-client"
	settings.OAuth.GoogleLoginSecret = "google-secret"
	if _, err := control.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "alice",
		Password: "secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())

	for _, provider := range []string{"github", "google"} {
		req := httptest.NewRequest(http.MethodGet, "/profile/oauth/"+provider, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s status = %d, want %d body=%s", provider, rec.Code, http.StatusSeeOther, rec.Body.String())
		}
		location := rec.Header().Get("Location")
		want := "redirect_uri=https%3A%2F%2Fexample.com%2Flogin%2Foauth%2F" + provider + "%2Fcallback"
		if !strings.Contains(location, want) {
			t.Fatalf("%s Location missing %q: %s", provider, want, location)
		}
		if strings.Contains(location, "%2Fprofile%2Foauth%2F"+provider+"%2Fcallback") {
			t.Fatalf("%s profile oauth start should not use a separate profile callback: %s", provider, location)
		}
		cookie, ok := responseCookie(rec.Result(), profileOAuthStateCookieName)
		if !ok {
			t.Fatalf("%s expected profile oauth state cookie", provider)
		}
		if cookie.Path != "/" {
			t.Fatalf("%s profile oauth state cookie path = %q, want /", provider, cookie.Path)
		}
	}
}

func TestProfileOAuthUnlinkRemovesIdentity(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "alice",
		Password: "secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if _, err := control.LinkOAuthIdentity(user.ID, controlplane.OAuthUserInput{Provider: "github", Subject: "12345"}); err != nil {
		t.Fatalf("LinkOAuthIdentity returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	req := httptest.NewRequest(http.MethodPost, "/profile/oauth/github/unlink", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unlink status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/profile/oauth?oauth_unlinked=github" {
		t.Fatalf("Location = %q", got)
	}
	updated, err := repo.GetUser(user.ID)
	if err != nil {
		t.Fatalf("GetUser returned error: %v", err)
	}
	if len(updated.OAuthIdentities) != 0 {
		t.Fatalf("OAuthIdentities after unlink = %#v", updated.OAuthIdentities)
	}

	if _, err := control.LinkOAuthIdentity(user.ID, controlplane.OAuthUserInput{Provider: "github", Subject: "12345"}); err != nil {
		t.Fatalf("LinkOAuthIdentity second time returned error: %v", err)
	}
	form.Set("return_to", "/?open_modal=oauth-bind-modal")
	req = httptest.NewRequest(http.MethodPost, "/profile/oauth/github/unlink", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("modal unlink status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse modal unlink Location: %v", err)
	}
	if location.Path != "/" || location.Query().Get("open_modal") != "oauth-bind-modal" || location.Query().Get("oauth_unlinked") != "github" {
		t.Fatalf("modal unlink Location = %q", rec.Header().Get("Location"))
	}
}

func TestProfileOAuthMergeConfirmMergesOAuthAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	target, err := control.CreateUser(controlplane.UserInput{
		Username:       "alice",
		Password:       "secret",
		Role:           core.UserRoleUser,
		Enabled:        true,
		BalanceNanoUSD: 1000,
	})
	if err != nil {
		t.Fatalf("CreateUser target returned error: %v", err)
	}
	source, err := control.CreateUser(controlplane.UserInput{
		Username:       "google_alice",
		Password:       "secret",
		Role:           core.UserRoleUser,
		Enabled:        true,
		BalanceNanoUSD: 2500,
	})
	if err != nil {
		t.Fatalf("CreateUser source returned error: %v", err)
	}
	if _, err := control.LinkOAuthIdentity(source.ID, controlplane.OAuthUserInput{Provider: "google", Subject: "google-subject"}); err != nil {
		t.Fatalf("LinkOAuthIdentity returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	state := profileOAuthMergeState{
		ID:        "merge-state",
		Provider:  "google",
		Subject:   "google-subject",
		SourceID:  source.ID,
		TargetID:  target.ID,
		ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	server.oauthMergeStates[state.ID] = state
	handler := authenticatedUserHandler(t, control, target, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/profile/oauth/google/merge?state=merge-state", nil)
	req.AddCookie(&http.Cookie{Name: profileOAuthMergeCookieName, Value: "merge-state", Path: "/profile/oauth/"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("merge page status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Merge Accounts") || !strings.Contains(rec.Body.String(), "google_alice") {
		t.Fatalf("merge page missing account details: %s", rec.Body.String())
	}

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("merge_state", "merge-state")
	req = httptest.NewRequest(http.MethodPost, "/profile/oauth/google/merge", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: profileOAuthMergeCookieName, Value: "merge-state", Path: "/profile/oauth/"})
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("merge status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/profile/oauth?oauth_merged=google" {
		t.Fatalf("Location = %q", got)
	}
	merged, err := repo.GetUser(target.ID)
	if err != nil {
		t.Fatalf("GetUser target returned error: %v", err)
	}
	if merged.BalanceNanoUSD != 3500 || len(merged.OAuthIdentities) != 1 {
		t.Fatalf("merged user = %#v", merged)
	}
	disabled, err := repo.GetUser(source.ID)
	if err != nil {
		t.Fatalf("GetUser source returned error: %v", err)
	}
	if disabled.Enabled || disabled.BalanceNanoUSD != 0 || len(disabled.OAuthIdentities) != 0 {
		t.Fatalf("disabled source = %#v", disabled)
	}
}

func TestPasswordUnauthenticatedFetchReturnsJSON(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	form := url.Values{}
	form.Set("current_password", "old-secret")
	form.Set("new_password", "new-secret")
	form.Set("new_password_confirm", "new-secret")
	req := httptest.NewRequest(http.MethodPost, "/profile/password", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "fetch")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") || !strings.Contains(rec.Body.String(), "Session expired") {
		t.Fatalf("unauthenticated password fetch should return JSON notice: content-type=%q body=%s", rec.Header().Get("Content-Type"), rec.Body.String())
	}
}

func TestUsersPageFormatsBalanceToTwoDecimals(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	inviter, err := control.CreateUser(controlplane.UserInput{
		Username: "alice",
		Password: "alice-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser(inviter) returned error: %v", err)
	}
	priced, err := control.CreateUser(controlplane.UserInput{
		Username:       "priced",
		Password:       "priced-secret",
		Role:           core.UserRoleUser,
		Enabled:        true,
		BalanceNanoUSD: 50*core.NanoUSDPerUSD + 259999000,
		InviterUserID:  inviter.ID,
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
	if !strings.Contains(body, "$50.26") || !strings.Contains(body, "alice") {
		t.Fatalf("users page should render user rows on first response: %s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/users?partial=users-panel", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("partial status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = rec.Body.String()
	if !strings.Contains(body, "$50.26") {
		t.Fatalf("response missing rounded balance: %s", body)
	}
	if !strings.Contains(body, "alice") || !strings.Contains(body, "Inviter") {
		t.Fatalf("response missing inviter: %s", body)
	}
	if strings.Contains(body, "Registration Domain") {
		t.Fatalf("response should not render registration domain: %s", body)
	}
	if strings.Contains(body, "$50.25") || strings.Contains(body, "$50.259999") {
		t.Fatalf("response should not truncate or include full precision balance: %s", body)
	}
	if strings.Contains(body, "User ID") || strings.Contains(body, "用户 ID") || strings.Contains(body, `<code>`+priced.ID+`</code>`) {
		t.Fatalf("users page should not render user IDs as visible details: %s", body)
	}
}

func TestUsersPageFiltersByQueryRoleAndStatus(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	inviter, err := control.CreateUser(controlplane.UserInput{
		Username: "alice",
		Password: "alice-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser(inviter) returned error: %v", err)
	}
	if _, err := control.CreateUser(controlplane.UserInput{
		Username:      "priced",
		Password:      "priced-secret",
		Role:          core.UserRoleUser,
		Enabled:       true,
		InviterUserID: inviter.ID,
	}); err != nil {
		t.Fatalf("CreateUser(priced) returned error: %v", err)
	}
	if _, err := control.CreateUser(controlplane.UserInput{
		Username:      "blocked-user",
		Password:      "blocked-secret",
		Role:          core.UserRoleUser,
		Enabled:       false,
		InviterUserID: inviter.ID,
	}); err != nil {
		t.Fatalf("CreateUser(blocked-user) returned error: %v", err)
	}
	if _, err := control.CreateUser(controlplane.UserInput{
		Username: "32ns",
		Password: "32ns-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	}); err != nil {
		t.Fatalf("CreateUser(32ns) returned error: %v", err)
	}
	shadow, err := control.CreateUser(controlplane.UserInput{
		Username: "shadow",
		Password: "shadow-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser(shadow) returned error: %v", err)
	}
	shadow.Email = "shadow@32ns.com"
	shadow.OAuthIdentities = []core.UserOAuthIdentity{{Provider: "github", Email: "shadow@32ns.com", Username: "32ns-shadow"}}
	if err := repo.UpsertUser(shadow); err != nil {
		t.Fatalf("UpsertUser(shadow) returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/users?inviter=alice&role=user&status=enabled&partial=users-panel", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"User Filters", "alice", `name="inviter" value="alice"`, `value="invite_count"`, "Invitation Count", "1 of 6 users"} {
		if !strings.Contains(body, want) {
			t.Fatalf("filtered users page missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "blocked-user") || strings.Contains(body, "<td><strong>32ns</strong></td>") {
		t.Fatalf("filtered users page includes disabled non-match: %s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/users?status=disabled&partial=users-panel", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("disabled status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = rec.Body.String()
	if !strings.Contains(body, "blocked-user") || strings.Contains(body, "<td><strong>priced</strong></td>") {
		t.Fatalf("disabled filter mismatch: %s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/users?q=32ns&partial=users-panel", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("32ns status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = rec.Body.String()
	if !strings.Contains(body, ">32ns<") || !strings.Contains(body, "1 of 6 users") {
		t.Fatalf("32ns filter missing visible user match: %s", body)
	}
	if strings.Contains(body, "<td><strong>shadow</strong></td>") || strings.Contains(body, "shadow@32ns.com") || strings.Contains(body, "32ns-shadow") {
		t.Fatalf("32ns filter should not match hidden identity fields: %s", body)
	}
}

func TestRegisterPageRendersPublicForm(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServerWithOptions(control, nil, ServerOptions{
		StatePath:               "data/state.db",
		AllowPublicRegistration: true,
	})
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/register", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`action="/register"`, `name="browser_fingerprint"`, `minlength="1"`, `name="password_confirm"`, "Create Account"} {
		if !strings.Contains(body, want) {
			t.Fatalf("register page missing %q: %s", want, body)
		}
	}
	if _, ok := responseCookie(rec.Result(), consoleCSRFCookieName); !ok {
		t.Fatal("expected csrf cookie")
	}
}

func TestRegisterPageShowsEnabledOAuthProviders(t *testing.T) {
	repo := storage.NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.Runtime.AllowPublicRegistration = true
	settings.OAuth.GitHubLoginEnabled = true
	settings.OAuth.GitHubLoginClientID = "github-client"
	settings.OAuth.GitHubLoginSecret = "github-secret"
	settings.OAuth.GoogleLoginEnabled = true
	settings.OAuth.GoogleLoginClientID = "google-client"
	settings.OAuth.GoogleLoginSecret = "google-secret"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatalf("UpsertSystemSettings returned error: %v", err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/register?next=%2Fclients", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"/login/oauth/github?next=%2Fclients", "/login/oauth/google?next=%2Fclients", "Sign in with GitHub", "Sign in with Google"} {
		if !strings.Contains(body, want) {
			t.Fatalf("register page missing %q: %s", want, body)
		}
	}
}

func TestRegisterWithEmailVerificationRequiresAndStoresVerifiedEmail(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	settings := core.DefaultSystemSettings()
	settings.Runtime.AllowPublicRegistration = true
	settings.Email.RegistrationVerificationEnabled = true
	settings.Email.SMTPHost = "smtp.example.com"
	settings.Email.SMTPPort = 465
	settings.Email.FromEmail = "noreply@example.com"
	if _, err := control.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	getReq := httptest.NewRequest(http.MethodGet, "/register", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d body=%s", getRec.Code, http.StatusOK, getRec.Body.String())
	}
	if !strings.Contains(getRec.Body.String(), `name="email_code"`) || !strings.Contains(getRec.Body.String(), `data-email-code-send`) {
		t.Fatalf("register page missing email verification controls: %s", getRec.Body.String())
	}
	csrfCookie, ok := responseCookie(getRec.Result(), consoleCSRFCookieName)
	if !ok {
		t.Fatal("expected csrf cookie")
	}

	form := url.Values{}
	form.Set("csrf_token", csrfCookie.Value)
	form.Set("username", "emailreg")
	form.Set("email", "EmailReg@Example.COM")
	form.Set("password", "emailreg-secret")
	form.Set("password_confirm", "emailreg-secret")
	missingCodeReq := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	missingCodeReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	missingCodeReq.AddCookie(csrfCookie)
	missingCodeRec := httptest.NewRecorder()
	handler.ServeHTTP(missingCodeRec, missingCodeReq)
	if missingCodeRec.Code != http.StatusSeeOther {
		t.Fatalf("missing code status = %d, want %d body=%s", missingCodeRec.Code, http.StatusSeeOther, missingCodeRec.Body.String())
	}
	if location := missingCodeRec.Header().Get("Location"); !strings.HasPrefix(location, "/register?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("missing code location = %q, want register notice redirect", location)
	}

	codeRecord := core.EmailVerificationCode{
		ID:          "email_code_web_test",
		Purpose:     controlplane.EmailVerificationPurposeRegister,
		Email:       "emailreg@example.com",
		MaxAttempts: 5,
		ExpiresAt:   time.Now().UTC().Add(time.Minute),
		CreatedAt:   time.Now().UTC(),
	}
	codeRecord.CodeHash = testEmailVerificationCodeHash(codeRecord.Purpose, codeRecord.Email, "123456", codeRecord.ID)
	if err := repo.CreateEmailVerificationCode(codeRecord); err != nil {
		t.Fatalf("CreateEmailVerificationCode returned error: %v", err)
	}
	form.Set("email_code", "123456")
	postReq := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	postReq.Host = "signup.example.com"
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.AddCookie(csrfCookie)
	postRec := httptest.NewRecorder()
	handler.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("post status = %d, want %d body=%s", postRec.Code, http.StatusSeeOther, postRec.Body.String())
	}
	user, err := repo.FindUserByUsername("emailreg")
	if err != nil {
		t.Fatal(err)
	}
	if user.Email != "emailreg@example.com" || !user.EmailVerified || user.InviterUserID != "" {
		t.Fatalf("registered user = %#v", user)
	}
}

func TestRegisterEmailAllowlistRequiresListedEmail(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	settings := core.DefaultSystemSettings()
	settings.Runtime.AllowPublicRegistration = true
	settings.Runtime.RegistrationEmailAllowlistEnabled = true
	settings.Runtime.RegistrationEmailAllowlist = []string{"@allowed.com"}
	if _, err := control.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	getReq := httptest.NewRequest(http.MethodGet, "/register", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d body=%s", getRec.Code, http.StatusOK, getRec.Body.String())
	}
	if !strings.Contains(getRec.Body.String(), `name="email_local"`) ||
		!strings.Contains(getRec.Body.String(), `name="email_domain"`) ||
		!strings.Contains(getRec.Body.String(), `value="@allowed.com"`) ||
		strings.Contains(getRec.Body.String(), `name="email_code"`) {
		t.Fatalf("register page should show email without code controls: %s", getRec.Body.String())
	}
	csrfCookie, ok := responseCookie(getRec.Result(), consoleCSRFCookieName)
	if !ok {
		t.Fatal("expected csrf cookie")
	}

	form := url.Values{}
	form.Set("csrf_token", csrfCookie.Value)
	form.Set("username", "blocked")
	form.Set("email", "blocked@example.com")
	form.Set("password", "blocked-secret")
	form.Set("password_confirm", "blocked-secret")
	blockedReq := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	blockedReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	blockedReq.AddCookie(csrfCookie)
	blockedRec := httptest.NewRecorder()
	handler.ServeHTTP(blockedRec, blockedReq)
	if blockedRec.Code != http.StatusSeeOther {
		t.Fatalf("blocked status = %d body=%s", blockedRec.Code, blockedRec.Body.String())
	}
	if location := blockedRec.Header().Get("Location"); !strings.HasPrefix(location, "/register?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("blocked location = %q, want register notice redirect", location)
	}

	form.Set("username", "allowed")
	form.Set("email", "user@Allowed.COM")
	form.Set("password", "allowed-secret")
	form.Set("password_confirm", "allowed-secret")
	allowedReq := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	allowedReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	allowedReq.AddCookie(csrfCookie)
	allowedRec := httptest.NewRecorder()
	handler.ServeHTTP(allowedRec, allowedReq)
	if allowedRec.Code != http.StatusSeeOther {
		t.Fatalf("allowed status = %d want %d body=%s", allowedRec.Code, http.StatusSeeOther, allowedRec.Body.String())
	}
	user, err := repo.FindUserByUsername("allowed")
	if err != nil {
		t.Fatal(err)
	}
	if user.Email != "user@allowed.com" {
		t.Fatalf("Email = %q", user.Email)
	}

	form = url.Values{}
	form.Set("csrf_token", csrfCookie.Value)
	form.Set("username", "splitallowed")
	form.Set("email_local", "split")
	form.Set("email_domain", "@allowed.com")
	form.Set("password", "split-secret")
	form.Set("password_confirm", "split-secret")
	splitReq := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	splitReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	splitReq.AddCookie(csrfCookie)
	splitRec := httptest.NewRecorder()
	handler.ServeHTTP(splitRec, splitReq)
	if splitRec.Code != http.StatusSeeOther {
		t.Fatalf("split status = %d want %d body=%s", splitRec.Code, http.StatusSeeOther, splitRec.Body.String())
	}
	splitUser, err := repo.FindUserByUsername("splitallowed")
	if err != nil {
		t.Fatal(err)
	}
	if splitUser.Email != "split@allowed.com" {
		t.Fatalf("split Email = %q", splitUser.Email)
	}
}

func testEmailVerificationCodeHash(purpose, email, code, salt string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(purpose) + "\x00" + strings.ToLower(strings.TrimSpace(email)) + "\x00" + strings.TrimSpace(code) + "\x00" + strings.TrimSpace(salt)))
	return hex.EncodeToString(sum[:])
}

func TestPublicRegistrationDisabledByDefault(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/register", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	loginReq := httptest.NewRequest(http.MethodGet, "/login", nil)
	loginRec := httptest.NewRecorder()
	handler.ServeHTTP(loginRec, loginReq)
	if strings.Contains(loginRec.Body.String(), `href="/register"`) {
		t.Fatalf("login page should not link to disabled public registration: %s", loginRec.Body.String())
	}
}

func TestPublicRegistrationCanBeEnabledFromSettings(t *testing.T) {
	repo := storage.NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.Runtime.AllowPublicRegistration = true
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatalf("UpsertSystemSettings returned error: %v", err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	loginReq := httptest.NewRequest(http.MethodGet, "/login", nil)
	loginRec := httptest.NewRecorder()
	handler.ServeHTTP(loginRec, loginReq)
	if !strings.Contains(loginRec.Body.String(), `href="/register"`) {
		t.Fatalf("login page should link to enabled public registration: %s", loginRec.Body.String())
	}

	registerReq := httptest.NewRequest(http.MethodGet, "/register", nil)
	registerRec := httptest.NewRecorder()
	handler.ServeHTTP(registerRec, registerReq)
	if registerRec.Code != http.StatusOK {
		t.Fatalf("register status = %d, want %d body=%s", registerRec.Code, http.StatusOK, registerRec.Body.String())
	}
}

func TestPublicRegistrationCanRequireInvitationCode(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	inviter, err := control.CreateUser(controlplane.UserInput{
		Username: "inviter",
		Password: "inviter-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser(inviter) returned error: %v", err)
	}
	settings := core.DefaultSystemSettings()
	settings.Runtime.AllowPublicRegistration = true
	settings.Invitation.Enabled = true
	settings.Registration.RequireInvitationCode = true
	if _, err := control.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	loginReq := httptest.NewRequest(http.MethodGet, "/login", nil)
	loginRec := httptest.NewRecorder()
	handler.ServeHTTP(loginRec, loginReq)
	if strings.Contains(loginRec.Body.String(), `href="/register"`) {
		t.Fatalf("login page should not expose public register link when invitations are required: %s", loginRec.Body.String())
	}

	registerReq := httptest.NewRequest(http.MethodGet, "/register", nil)
	registerRec := httptest.NewRecorder()
	handler.ServeHTTP(registerRec, registerReq)
	if registerRec.Code != http.StatusNotFound {
		t.Fatalf("register without invite status = %d, want %d body=%s", registerRec.Code, http.StatusNotFound, registerRec.Body.String())
	}

	code := control.InvitationCodeForUser(inviter)
	invitedReq := httptest.NewRequest(http.MethodGet, "/register?invite="+url.QueryEscape(code), nil)
	invitedRec := httptest.NewRecorder()
	handler.ServeHTTP(invitedRec, invitedReq)
	if invitedRec.Code != http.StatusOK {
		t.Fatalf("register with invite status = %d, want %d body=%s", invitedRec.Code, http.StatusOK, invitedRec.Body.String())
	}
}

func TestRegisterWithInvitationCreditsRewards(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	inviter, err := control.CreateUser(controlplane.UserInput{
		Username: "alice",
		Password: "alice-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser(inviter) returned error: %v", err)
	}
	settings := core.DefaultSystemSettings()
	settings.Invitation.Enabled = true
	settings.Invitation.InviterRechargeRewardBps = 500
	settings.Invitation.InviteeRewardNanoUSD = 1500000000
	if _, err := control.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()
	code := control.InvitationCodeForUser(inviter)

	getReq := httptest.NewRequest(http.MethodGet, "/register?invite="+url.QueryEscape(code), nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d body=%s", getRec.Code, http.StatusOK, getRec.Body.String())
	}
	csrfCookie, ok := responseCookie(getRec.Result(), consoleCSRFCookieName)
	if !ok {
		t.Fatal("expected csrf cookie")
	}
	if !strings.Contains(getRec.Body.String(), `name="invite_code" value="`+code+`"`) {
		t.Fatalf("register page should preserve invite code: %s", getRec.Body.String())
	}

	form := url.Values{}
	form.Set("csrf_token", csrfCookie.Value)
	form.Set("invite_code", code)
	form.Set("username", "bob")
	form.Set("password", "bob-secret")
	form.Set("password_confirm", "bob-secret")
	postReq := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	postReq.Host = "signup.example.com"
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.AddCookie(csrfCookie)
	postRec := httptest.NewRecorder()
	handler.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("post status = %d, want %d body=%s", postRec.Code, http.StatusSeeOther, postRec.Body.String())
	}

	creditedInviter, err := control.GetUser(inviter.ID)
	if err != nil {
		t.Fatal(err)
	}
	if creditedInviter.BalanceNanoUSD != 0 {
		t.Fatalf("inviter balance = %d, want no registration reward", creditedInviter.BalanceNanoUSD)
	}
	invitee, err := repo.FindUserByUsername("bob")
	if err != nil {
		t.Fatal(err)
	}
	if invitee.BalanceNanoUSD != 1500000000 {
		t.Fatalf("invitee balance = %d", invitee.BalanceNanoUSD)
	}
	if invitee.InviterUserID != inviter.ID {
		t.Fatalf("InviterUserID = %q, want %q", invitee.InviterUserID, inviter.ID)
	}
}

func TestRegisterWithoutInviteStillRequiresPublicRegistration(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	settings := core.DefaultSystemSettings()
	settings.Invitation.Enabled = true
	if _, err := control.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/register", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestRegisterWithInvalidInviteRequiresPublicRegistration(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	settings := core.DefaultSystemSettings()
	settings.Invitation.Enabled = true
	if _, err := control.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/register?invite=i_invalid", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestRegisterIPRateLimitBlocksExcessAttempts(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	settings := core.DefaultSystemSettings()
	settings.Runtime.AllowPublicRegistration = true
	settings.Registration.RegisterIPHourlyLimit = 1
	if _, err := control.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	getReq := httptest.NewRequest(http.MethodGet, "/register", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	csrfCookie, ok := responseCookie(getRec.Result(), consoleCSRFCookieName)
	if !ok {
		t.Fatal("expected csrf cookie")
	}
	form := url.Values{}
	form.Set("csrf_token", csrfCookie.Value)
	form.Set("username", "limited")
	form.Set("password", "one")
	form.Set("password_confirm", "two")

	firstReq := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	firstReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	firstReq.Header.Set("X-Forwarded-For", "203.0.113.10")
	firstReq.RemoteAddr = "127.0.0.1:1234"
	firstReq.AddCookie(csrfCookie)
	firstRec := httptest.NewRecorder()
	handler.ServeHTTP(firstRec, firstReq)
	if firstRec.Code != http.StatusSeeOther {
		t.Fatalf("first status = %d, want %d body=%s", firstRec.Code, http.StatusSeeOther, firstRec.Body.String())
	}

	form.Set("password_confirm", "one")
	secondReq := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	secondReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	secondReq.Header.Set("X-Forwarded-For", "203.0.113.10")
	secondReq.RemoteAddr = "127.0.0.1:1234"
	secondReq.AddCookie(csrfCookie)
	secondRec := httptest.NewRecorder()
	handler.ServeHTTP(secondRec, secondReq)
	if secondRec.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d body=%s", secondRec.Code, http.StatusTooManyRequests, secondRec.Body.String())
	}
	if !strings.Contains(secondRec.Body.String(), "Too many registration attempts") {
		t.Fatalf("rate-limited response missing message: %s", secondRec.Body.String())
	}
}

func TestRegisterIPRateLimitIgnoresSpoofedForwardedForWithoutTrustedProxy(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	settings := core.DefaultSystemSettings()
	settings.Runtime.AllowPublicRegistration = true
	settings.Registration.RegisterIPHourlyLimit = 1
	if _, err := control.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	getReq := httptest.NewRequest(http.MethodGet, "/register", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	csrfCookie, ok := responseCookie(getRec.Result(), consoleCSRFCookieName)
	if !ok {
		t.Fatal("expected csrf cookie")
	}
	form := url.Values{}
	form.Set("csrf_token", csrfCookie.Value)
	form.Set("username", "spoofed")
	form.Set("password", "one")
	form.Set("password_confirm", "two")

	firstReq := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	firstReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	firstReq.Header.Set("X-Forwarded-For", "203.0.113.20")
	firstReq.RemoteAddr = "198.51.100.10:54321"
	firstReq.AddCookie(csrfCookie)
	firstRec := httptest.NewRecorder()
	handler.ServeHTTP(firstRec, firstReq)
	if firstRec.Code != http.StatusSeeOther {
		t.Fatalf("first status = %d, want %d body=%s", firstRec.Code, http.StatusSeeOther, firstRec.Body.String())
	}

	secondReq := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	secondReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	secondReq.Header.Set("X-Forwarded-For", "203.0.113.21")
	secondReq.RemoteAddr = "198.51.100.10:54322"
	secondReq.AddCookie(csrfCookie)
	secondRec := httptest.NewRecorder()
	handler.ServeHTTP(secondRec, secondReq)
	if secondRec.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d body=%s", secondRec.Code, http.StatusTooManyRequests, secondRec.Body.String())
	}
}

func TestRegisterEmailCodeSendIPRateLimitBlocksRotatingEmails(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	settings := core.DefaultSystemSettings()
	settings.Runtime.AllowPublicRegistration = true
	settings.Registration.EmailCodeIPHourlyLimit = 1
	if _, err := control.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	getReq := httptest.NewRequest(http.MethodGet, "/register", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	csrfCookie, ok := responseCookie(getRec.Result(), consoleCSRFCookieName)
	if !ok {
		t.Fatal("expected csrf cookie")
	}

	form := url.Values{}
	form.Set("csrf_token", csrfCookie.Value)
	form.Set("email", "first@example.com")
	firstReq := httptest.NewRequest(http.MethodPost, "/register/email-code/send", strings.NewReader(form.Encode()))
	firstReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	firstReq.Header.Set("X-Forwarded-For", "203.0.113.11")
	firstReq.RemoteAddr = "127.0.0.1:1234"
	firstReq.AddCookie(csrfCookie)
	firstRec := httptest.NewRecorder()
	handler.ServeHTTP(firstRec, firstReq)
	if firstRec.Code != http.StatusBadRequest {
		t.Fatalf("first status = %d, want %d body=%s", firstRec.Code, http.StatusBadRequest, firstRec.Body.String())
	}

	form.Set("email", "second@example.com")
	secondReq := httptest.NewRequest(http.MethodPost, "/register/email-code/send", strings.NewReader(form.Encode()))
	secondReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	secondReq.Header.Set("X-Forwarded-For", "203.0.113.11")
	secondReq.RemoteAddr = "127.0.0.1:1234"
	secondReq.AddCookie(csrfCookie)
	secondRec := httptest.NewRecorder()
	handler.ServeHTTP(secondRec, secondReq)
	if secondRec.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d body=%s", secondRec.Code, http.StatusTooManyRequests, secondRec.Body.String())
	}
	if !strings.Contains(secondRec.Body.String(), "Too many verification code requests") {
		t.Fatalf("rate-limited response missing message: %s", secondRec.Body.String())
	}
}

func TestRegisterRequiresTurnstileWhenEnabled(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	settings := core.DefaultSystemSettings()
	settings.Runtime.AllowPublicRegistration = true
	settings.Registration.TurnstileEnabled = true
	settings.Registration.TurnstileSiteKey = "site-key"
	settings.Registration.TurnstileSecretKey = "secret-key"
	if _, err := control.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	verified := make(chan url.Values, 1)
	verifyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		verified <- r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer verifyServer.Close()
	oldEndpoint := turnstileVerifyEndpoint
	oldClient := turnstileHTTPClient
	turnstileVerifyEndpoint = verifyServer.URL
	turnstileHTTPClient = verifyServer.Client()
	defer func() {
		turnstileVerifyEndpoint = oldEndpoint
		turnstileHTTPClient = oldClient
	}()

	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()
	getReq := httptest.NewRequest(http.MethodGet, "/register", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d body=%s", getRec.Code, http.StatusOK, getRec.Body.String())
	}
	if !strings.Contains(getRec.Body.String(), `class="cf-turnstile"`) || !strings.Contains(getRec.Body.String(), `data-sitekey="site-key"`) {
		t.Fatalf("register page missing Turnstile widget: %s", getRec.Body.String())
	}
	csrfCookie, ok := responseCookie(getRec.Result(), consoleCSRFCookieName)
	if !ok {
		t.Fatal("expected csrf cookie")
	}
	form := url.Values{}
	form.Set("csrf_token", csrfCookie.Value)
	form.Set("username", "turnstile")
	form.Set("password", "turnstile-secret")
	form.Set("password_confirm", "turnstile-secret")

	missingReq := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	missingReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	missingReq.Header.Set("X-Forwarded-For", "203.0.113.12")
	missingReq.RemoteAddr = "127.0.0.1:1234"
	missingReq.AddCookie(csrfCookie)
	missingRec := httptest.NewRecorder()
	handler.ServeHTTP(missingRec, missingReq)
	if missingRec.Code != http.StatusSeeOther {
		t.Fatalf("missing status = %d, want %d body=%s", missingRec.Code, http.StatusSeeOther, missingRec.Body.String())
	}
	if location := missingRec.Header().Get("Location"); !strings.Contains(location, "notice_error=") {
		t.Fatalf("missing captcha location = %q", location)
	}

	form.Set("cf-turnstile-response", "valid-token")
	validReq := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	validReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	validReq.Header.Set("X-Forwarded-For", "203.0.113.12")
	validReq.RemoteAddr = "127.0.0.1:1234"
	validReq.AddCookie(csrfCookie)
	validRec := httptest.NewRecorder()
	handler.ServeHTTP(validRec, validReq)
	if validRec.Code != http.StatusSeeOther {
		t.Fatalf("valid status = %d, want %d body=%s", validRec.Code, http.StatusSeeOther, validRec.Body.String())
	}
	select {
	case form := <-verified:
		if form.Get("secret") != "secret-key" || form.Get("response") != "valid-token" || form.Get("remoteip") != "203.0.113.12" {
			t.Fatalf("turnstile form = %#v", form)
		}
	default:
		t.Fatal("turnstile verification endpoint was not called")
	}
	if _, err := repo.FindUserByUsername("turnstile"); err != nil {
		t.Fatalf("FindUserByUsername returned error: %v", err)
	}
}

func TestDashboardInvitationLinkUsesRequestDomain(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "alice",
		Password: "alice-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	settings := core.DefaultSystemSettings()
	settings.Invitation.Enabled = true
	settings.Invitation.InviterRechargeRewardBps = 500
	settings.Invitation.InviteeRewardNanoUSD = 1500000000
	settings.Registration.NewUserRewardEnabled = true
	settings.Registration.NewUserRewardNanoUSD = 500000000
	if _, err := control.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "internal.local"
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "invite.example.com, proxy.local")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	code := control.InvitationCodeForUser(user)
	want := "https://invite.example.com/register?invite=" + url.QueryEscape(code)
	if !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("dashboard missing invitation link %q: %s", want, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "inviter gets 5% of invited recharges") || !strings.Contains(rec.Body.String(), "invitee gets $2.00 reward") {
		t.Fatalf("dashboard missing invitation reward hint: %s", rec.Body.String())
	}
}

func TestRegisterCreatesEnabledUserSession(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServerWithOptions(control, nil, ServerOptions{
		StatePath:               "data/state.db",
		AllowPublicRegistration: true,
	})
	handler := server.Handler()

	getReq := httptest.NewRequest(http.MethodGet, "/register", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	csrfCookie, ok := responseCookie(getRec.Result(), consoleCSRFCookieName)
	if !ok {
		t.Fatal("expected csrf cookie")
	}

	form := url.Values{}
	form.Set("csrf_token", csrfCookie.Value)
	form.Set("username", "alice")
	form.Set("password", "alice-secret")
	form.Set("password_confirm", "alice-secret")
	form.Set("browser_fingerprint", "fingerprint-alice")
	form.Set("role", string(core.UserRoleAdmin))
	postReq := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	postReq.RemoteAddr = "203.0.113.77:12345"
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.AddCookie(csrfCookie)
	postRec := httptest.NewRecorder()
	handler.ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", postRec.Code, http.StatusSeeOther, postRec.Body.String())
	}
	if got := postRec.Header().Get("Location"); got != "/" {
		t.Fatalf("Location = %q, want /", got)
	}
	sessionCookie, ok := responseCookie(postRec.Result(), consoleSessionCookieName)
	if !ok {
		t.Fatal("expected session cookie")
	}
	user, err := repo.FindUserByUsername("alice")
	if err != nil {
		t.Fatalf("FindUserByUsername returned error: %v", err)
	}
	if user.Role != core.UserRoleUser || !user.Enabled {
		t.Fatalf("registered user role/enabled = %s/%t, want user/true", user.Role, user.Enabled)
	}
	if user.RegistrationIP != "203.0.113.77" || user.RegistrationBrowserFingerprint != "fingerprint-alice" {
		t.Fatalf("registration metadata = ip %q fingerprint %q", user.RegistrationIP, user.RegistrationBrowserFingerprint)
	}
	if user.LastLoginAt == nil {
		t.Fatal("registered user LastLoginAt is nil")
	}

	clientsReq := httptest.NewRequest(http.MethodGet, "/clients", nil)
	clientsReq.AddCookie(sessionCookie)
	clientsRec := httptest.NewRecorder()
	handler.ServeHTTP(clientsRec, clientsReq)
	if clientsRec.Code != http.StatusOK {
		t.Fatalf("clients status = %d, want %d body=%s", clientsRec.Code, http.StatusOK, clientsRec.Body.String())
	}
}

func TestRegisterRejectsUsernameBelowConfiguredMinimum(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	settings := core.DefaultSystemSettings()
	settings.Runtime.AllowPublicRegistration = true
	settings.Registration.UsernameMinLength = 6
	if _, err := control.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	getReq := httptest.NewRequest(http.MethodGet, "/register", nil)
	getReq.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	csrfCookie, ok := responseCookie(getRec.Result(), consoleCSRFCookieName)
	if !ok {
		t.Fatal("expected csrf cookie")
	}
	if !strings.Contains(getRec.Body.String(), `minlength="6"`) {
		t.Fatalf("register page missing configured username minlength: %s", getRec.Body.String())
	}
	if !strings.Contains(getRec.Body.String(), `pattern="[A-Za-z0-9_.-]+"`) {
		t.Fatalf("register page missing username character pattern: %s", getRec.Body.String())
	}

	form := url.Values{}
	form.Set("csrf_token", csrfCookie.Value)
	form.Set("username", "abc")
	form.Set("password", "abc-secret")
	form.Set("password_confirm", "abc-secret")
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.AddCookie(csrfCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse redirect location %q: %v", location, err)
	}
	if got := parsed.Query().Get("notice_error"); got != "用户名至少需要 6 个字符。" {
		t.Fatalf("notice_error = %q, want configured minimum message", got)
	}
	if _, err := repo.FindUserByUsername("abc"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("short username should not create user, err=%v", err)
	}
}

func TestRegisterRejectsUsernameInvalidCharacters(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	settings := core.DefaultSystemSettings()
	settings.Runtime.AllowPublicRegistration = true
	settings.Registration.UsernameMinLength = 10
	if _, err := control.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	getReq := httptest.NewRequest(http.MethodGet, "/register", nil)
	getReq.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	csrfCookie, ok := responseCookie(getRec.Result(), consoleCSRFCookieName)
	if !ok {
		t.Fatal("expected csrf cookie")
	}

	form := url.Values{}
	form.Set("csrf_token", csrfCookie.Value)
	form.Set("username", "bad@name")
	form.Set("password", "bad-secret")
	form.Set("password_confirm", "bad-secret")
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.AddCookie(csrfCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse redirect location %q: %v", location, err)
	}
	if got := parsed.Query().Get("notice_error"); got != "用户名只能包含英文字母、数字、下划线、短横线和点号。" {
		t.Fatalf("notice_error = %q, want invalid username message", got)
	}
	if _, err := repo.FindUserByUsername("bad@name"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("invalid username should not create user, err=%v", err)
	}
}

func TestRegisterRejectsPasswordMismatchAndDuplicateUsername(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServerWithOptions(control, nil, ServerOptions{
		StatePath:               "data/state.db",
		AllowPublicRegistration: true,
	})
	handler := server.Handler()

	getReq := httptest.NewRequest(http.MethodGet, "/register", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	csrfCookie, ok := responseCookie(getRec.Result(), consoleCSRFCookieName)
	if !ok {
		t.Fatal("expected csrf cookie")
	}

	form := url.Values{}
	form.Set("csrf_token", csrfCookie.Value)
	form.Set("username", "bob")
	form.Set("password", "one")
	form.Set("password_confirm", "two")
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(csrfCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("mismatch status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if strings.Contains(rec.Header().Get("Set-Cookie"), consoleSessionCookieName) {
		t.Fatal("password mismatch should not set session cookie")
	}
	if location := rec.Header().Get("Location"); !strings.HasPrefix(location, "/register?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("mismatch location = %q, want register notice redirect", location)
	}

	if _, err := control.CreateUser(controlplane.UserInput{Username: "bob", Password: "bob-secret", Role: core.UserRoleUser, Enabled: true}); err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	form.Set("password", "bob-secret")
	form.Set("password_confirm", "bob-secret")
	req = httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(csrfCookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("duplicate status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if location := rec.Header().Get("Location"); !strings.HasPrefix(location, "/register?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("duplicate location = %q, want register notice redirect", location)
	}
}

func TestRegularUserOnlySeesOwnedClients(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	alice, err := control.CreateUser(controlplane.UserInput{
		Username: "alice",
		Password: "alice-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser(alice) returned error: %v", err)
	}
	bob, err := control.CreateUser(controlplane.UserInput{
		Username: "bob",
		Password: "bob-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser(bob) returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:          "client_alice",
		Name:        "Alice Key",
		APIKey:      "gw_alice",
		OwnerUserID: alice.ID,
		Enabled:     true,
		RoutePolicy: core.DefaultRoutePolicy(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:          "client_bob",
		Name:        "Bob Key",
		APIKey:      "gw_bob",
		OwnerUserID: bob.ID,
		Enabled:     true,
		RoutePolicy: core.DefaultRoutePolicy(),
	}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, alice, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("accounts status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if location := rec.Header().Get("Location"); !strings.HasPrefix(location, "/?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("accounts location = %q, want dashboard notice redirect", location)
	}

	req = httptest.NewRequest(http.MethodGet, "/clients", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("clients status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Alice Key") {
		t.Fatal("owned client missing from client page")
	}
	for _, want := range []string{
		"<span>Claude</span>",
		`data-copy-value="http://example.com"`,
		"<span>Codex</span>",
		`data-copy-value="http://example.com/v1"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("client page missing protocol server address %q: %s", want, body)
		}
	}
	if strings.Contains(body, "Bob Key") {
		t.Fatal("foreign client leaked into client page")
	}

	req = httptest.NewRequest(http.MethodPost, "/clients/client_bob/delete", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete foreign client status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if location := rec.Header().Get("Location"); !strings.HasPrefix(location, "/clients?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("delete foreign client location = %q, want clients notice redirect", location)
	}
}

func TestClientsPagePrefersPublicBaseURLForProtocolServerAddresses(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	alice, err := control.CreateUser(controlplane.UserInput{
		Username: "alice",
		Password: "alice-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	settings := core.DefaultSystemSettings()
	settings.Runtime.PublicBaseURL = "https://gateway.example.com/"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatalf("UpsertSystemSettings returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:          "client_alice",
		Name:        "Alice Key",
		APIKey:      "gw_alice",
		OwnerUserID: alice.ID,
		Enabled:     true,
		RoutePolicy: core.DefaultRoutePolicy(),
	}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, alice, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/clients", nil)
	req.Host = "runtime.example.test"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("clients status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<span>Claude</span>`,
		`data-copy-value="https://gateway.example.com"`,
		`<span>Codex</span>`,
		`data-copy-value="https://gateway.example.com/v1"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("client page should render public protocol address %q: %s", want, body)
		}
	}
}

func TestAdminOnlySeesAndManagesOwnedClients(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	other, err := control.CreateUser(controlplane.UserInput{
		Username: "other",
		Password: "other-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:          "client_admin",
		Name:        "Admin Key",
		APIKey:      "gw_admin",
		OwnerUserID: admin.ID,
		Enabled:     true,
		RoutePolicy: core.DefaultRoutePolicy(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:          "client_other",
		Name:        "Other Key",
		APIKey:      "gw_other",
		OwnerUserID: other.ID,
		Enabled:     true,
		RoutePolicy: core.DefaultRoutePolicy(),
	}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, admin, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/clients", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("clients status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Admin Key") {
		t.Fatal("owned admin client missing from client page")
	}
	if strings.Contains(body, "Other Key") {
		t.Fatal("foreign client leaked into admin client page")
	}

	req = httptest.NewRequest(http.MethodPost, "/clients/client_other/delete", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete foreign client status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if location := rec.Header().Get("Location"); !strings.HasPrefix(location, "/clients?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("delete foreign client location = %q, want clients notice redirect", location)
	}
}

func TestClientConsoleRoutesAreNotUnderAdminPrefix(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/clients", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/clients status = %d, want %d", rec.Code, http.StatusOK)
	}

	for _, path := range []string{"/admin/clients", "/admin/clients/new", "/admin/clients/client_a/edit"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want %d", path, rec.Code, http.StatusNotFound)
		}
	}
}

func TestRegularUserDashboardShowsBalanceSpendAndRecentUsage(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	alice, err := control.CreateUser(controlplane.UserInput{
		Username:       "alice",
		Password:       "alice-secret",
		Role:           core.UserRoleUser,
		Enabled:        true,
		BalanceNanoUSD: 10*core.NanoUSDPerUSD + 519998000,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	client := core.APIClient{
		ID:           "client_alice",
		Name:         "Alice Key",
		APIKey:       "gw_alice",
		OwnerUserID:  alice.ID,
		Enabled:      true,
		AccountGroup: "Plus",
		RoutePolicy:  core.DefaultRoutePolicy(),
	}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                           "group_plus",
		Name:                         "Plus",
		BillingMultiplierBps:         10000,
		InputPriceNanoUSDPer1M:       2 * core.NanoUSDPerUSD,
		CachedInputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M:      6 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                   "group_pro",
		Name:                 "Pro",
		BillingMultiplierBps: 10000,
	}); err != nil {
		t.Fatal(err)
	}
	hide := false
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                 "group_hidden",
		Name:               "Hidden",
		ShowInClientEditor: &hide,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                           "gpt-4.1",
		Provider:                     core.ProviderOpenAI,
		Enabled:                      true,
		VisibleGroups:                []string{core.DefaultAccountGroupName},
		BillingMode:                  core.ModelBillingModeToken,
		InputPriceNanoUSDPer1M:       core.NanoUSDPerUSD + core.NanoUSDPerUSD/4,
		CachedInputPriceNanoUSDPer1M: core.NanoUSDPerUSD / 2,
		OutputPriceNanoUSDPer1M:      5*core.NanoUSDPerUSD + core.NanoUSDPerUSD/2,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "claude-3.5",
		Provider:                core.ProviderClaude,
		Enabled:                 true,
		VisibleGroups:           []string{"Plus"},
		BillingMode:             core.ModelBillingModeToken,
		DisplayName:             "Claude Plus",
		OutputPriceNanoUSDPer1M: 7 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:            "hidden-model",
		Provider:      core.ProviderOpenAI,
		Enabled:       true,
		VisibleGroups: []string{"Hidden"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                  "gpt-image-2",
		Provider:            core.ProviderOpenAI,
		Enabled:             true,
		VisibleGroups:       []string{core.DefaultAccountGroupName},
		BillingMode:         core.ModelBillingModeRequest,
		RequestPriceNanoUSD: core.NanoUSDPerUSD / 20,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_alice",
		ClientID:        client.ID,
		UserID:          alice.ID,
		Provider:        core.ProviderOpenAI,
		Model:           "gpt-4.1",
		ReservedNanoUSD: 3 * core.NanoUSDPerUSD,
		Fingerprint:     "alice-dashboard",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_alice",
		ClientID:      client.ID,
		Provider:      core.ProviderOpenAI,
		Model:         "gpt-4.1",
		Usage:         core.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
		ActualNanoUSD: 2*core.NanoUSDPerUSD + 259999000,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	settings := core.DefaultSystemSettings()
	settings.Payment.Alipay.Enabled = true
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatalf("UpsertSystemSettings returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, alice, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"My Console", "Hourly Spend", "$8.26", "$2.26", "Alice Key", "gpt-4.1", "usage-chart", "recharge-action-button", `data-group-settings-open="payment-recharge"`, `href="/models"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q: %s", want, body)
		}
	}
	for _, unwanted := range []string{"user-model-table", "claude-3.5", "gpt-image-2"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("dashboard should not render model list content, found %q: %s", unwanted, body)
		}
	}
	if strings.Contains(body, "balance-recharge-button") {
		t.Fatalf("dashboard should not render recharge button inside balance card: %s", body)
	}
	if strings.Contains(body, "user-dashboard-custom-panel") {
		t.Fatalf("dashboard should not render custom panel when disabled: %s", body)
	}
	for _, unwanted := range []string{"$8.25", "$2.25", "$8.259999", "$2.259999"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("dashboard should round currency display, found %q: %s", unwanted, body)
		}
	}
	for _, unwanted := range []string{"hidden-model", "Hidden"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("dashboard should not leak hidden group model pricing, found %q: %s", unwanted, body)
		}
	}

	modelsReq := httptest.NewRequest(http.MethodGet, "/models", nil)
	modelsRec := httptest.NewRecorder()
	handler.ServeHTTP(modelsRec, modelsReq)

	if modelsRec.Code != http.StatusOK {
		t.Fatalf("models status = %d, want %d body=%s", modelsRec.Code, http.StatusOK, modelsRec.Body.String())
	}
	modelsBody := modelsRec.Body.String()
	for _, want := range []string{"Model List", "user-model-panel", "user-model-table", `data-copy-value="claude-3.5"`, "Copy model ID", "$1.25 / 1M", "$5.50 / 1M", "claude-3.5", "Default", "Plus", "$6.00 / 1M", "gpt-image-2", "$0.05 / Request"} {
		if !strings.Contains(modelsBody, want) {
			t.Fatalf("models page missing %q: %s", want, modelsBody)
		}
	}
	for _, unwanted := range []string{"hidden-model", "<span>Hidden</span>"} {
		if strings.Contains(modelsBody, unwanted) {
			t.Fatalf("models page should not show hidden client-editor group pricing, found %q: %s", unwanted, modelsBody)
		}
	}
}

func TestUserModelsPageHidesEnabledModelsForHiddenClientEditorGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "group-user",
		Password: "secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	hide := false
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_hidden", Name: "Hidden", ShowInClientEditor: &hide}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:            "hidden-model",
		Provider:      core.ProviderOpenAI,
		Enabled:       true,
		VisibleGroups: []string{"Hidden"},
	}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("models status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "hidden-model") || strings.Contains(body, "Hidden") {
		t.Fatalf("models page showed hidden client-editor group model: %s", body)
	}
}

func TestClientCreateAssignsCurrentUserOwner(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	other, err := control.CreateUser(controlplane.UserInput{
		Username: "other",
		Password: "other-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_openai",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI",
		Status:   core.AccountStatusActive,
		Group:    "Plus",
	}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	getReq := httptest.NewRequest(http.MethodGet, "/clients/new", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("create page status = %d, want %d", getRec.Code, http.StatusOK)
	}
	if strings.Contains(getRec.Body.String(), `name="owner_user_id"`) {
		t.Fatal("create page must not expose owner_user_id selector")
	}

	form := url.Values{}
	form.Set("name", "Admin Created Key")
	form.Set("owner_user_id", other.ID)
	form.Set("enabled", "on")
	form.Set("account_group", "Plus")
	postReq := httptest.NewRequest(http.MethodPost, "/clients/new", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postRec := httptest.NewRecorder()
	handler.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("create submit status = %d, want %d body=%s", postRec.Code, http.StatusSeeOther, postRec.Body.String())
	}
	if location := postRec.Header().Get("Location"); location != "/clients?created=1" {
		t.Fatalf("create redirect = %q, want /clients?created=1", location)
	}

	var client core.APIClient
	for _, candidate := range repo.ListClients() {
		if candidate.Name == "Admin Created Key" {
			client = candidate
			break
		}
	}
	if client.ID == "" {
		t.Fatal("created client not found")
	}
	if client.APIKey == "" {
		t.Fatal("expected generated API key")
	}
	if !strings.HasPrefix(client.APIKey, "sk-") {
		t.Fatalf("api key = %q, want sk- prefix", client.APIKey)
	}
	if client.OwnerUserID != admin.ID {
		t.Fatalf("owner = %q, want creator %q; posted owner was %q", client.OwnerUserID, admin.ID, other.ID)
	}
	if client.AccountGroup != "Plus" {
		t.Fatalf("account group = %q", client.AccountGroup)
	}
	noticeReq := httptest.NewRequest(http.MethodGet, "/clients?created=1", nil)
	noticeRec := httptest.NewRecorder()
	handler.ServeHTTP(noticeRec, noticeReq)
	if noticeRec.Code != http.StatusOK {
		t.Fatalf("clients notice page status = %d, want %d", noticeRec.Code, http.StatusOK)
	}
	if !strings.Contains(noticeRec.Body.String(), `data-clear-url-params="created"`) || !strings.Contains(noticeRec.Body.String(), "API key created") {
		t.Fatalf("clients notice missing: %s", noticeRec.Body.String())
	}

	editReq := httptest.NewRequest(http.MethodGet, "/clients/"+client.ID+"/edit", nil)
	editRec := httptest.NewRecorder()
	handler.ServeHTTP(editRec, editReq)
	if editRec.Code != http.StatusOK {
		t.Fatalf("edit page status = %d, want %d", editRec.Code, http.StatusOK)
	}
	if strings.Contains(editRec.Body.String(), `name="owner_user_id"`) {
		t.Fatal("edit page must not expose owner_user_id selector")
	}

	editForm := url.Values{}
	editForm.Set("name", "Renamed Key")
	editForm.Set("owner_user_id", other.ID)
	editForm.Set("enabled", "on")
	editForm.Set("account_group", "Plus")
	editSubmit := httptest.NewRequest(http.MethodPost, "/clients/"+client.ID+"/edit", strings.NewReader(editForm.Encode()))
	editSubmit.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	editSubmitRec := httptest.NewRecorder()
	handler.ServeHTTP(editSubmitRec, editSubmit)
	if editSubmitRec.Code != http.StatusSeeOther {
		t.Fatalf("edit submit status = %d, want %d body=%s", editSubmitRec.Code, http.StatusSeeOther, editSubmitRec.Body.String())
	}
	client, err = repo.GetClient(client.ID)
	if err != nil {
		t.Fatalf("GetClient returned error: %v", err)
	}
	if client.OwnerUserID != admin.ID {
		t.Fatalf("owner changed to %q, want creator %q", client.OwnerUserID, admin.ID)
	}
}

func TestClientCreateDefaultsToDefaultAccountGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_default",
		Provider: core.ProviderOpenAI,
		Label:    "Default OpenAI",
		Status:   core.AccountStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	getReq := httptest.NewRequest(http.MethodGet, "/clients/new", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("create page status = %d, want %d body=%s", getRec.Code, http.StatusOK, getRec.Body.String())
	}
	body := getRec.Body.String()
	for _, want := range []string{
		`name="account_group"`,
		`value="Default" selected`,
		`Default (1x)`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("create page missing %q: %s", want, body)
		}
	}

	form := url.Values{}
	form.Set("name", "Default Key")
	form.Set("enabled", "on")
	form.Set("account_group", core.DefaultAccountGroupName)
	postReq := httptest.NewRequest(http.MethodPost, "/clients/new", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postRec := httptest.NewRecorder()
	handler.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("create submit status = %d, want %d body=%s", postRec.Code, http.StatusSeeOther, postRec.Body.String())
	}

	var client core.APIClient
	for _, candidate := range repo.ListClients() {
		if candidate.Name == "Default Key" {
			client = candidate
			break
		}
	}
	if client.ID == "" {
		t.Fatal("created client not found")
	}
	if client.OwnerUserID != admin.ID {
		t.Fatalf("owner = %q, want %q", client.OwnerUserID, admin.ID)
	}
	if client.AccountGroup != core.DefaultAccountGroupName {
		t.Fatalf("account group = %q, want default", client.AccountGroup)
	}
	if client.RoutePolicy.DefaultProvider != core.ProviderOpenAI {
		t.Fatalf("default provider = %q, want openai", client.RoutePolicy.DefaultProvider)
	}
}

func TestClientsPageRendersDefaultAccountGroupLabel(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_default_label", Name: "Default Label Key", APIKey: "gw_default_label", OwnerUserID: admin.ID, Enabled: true, AccountGroup: core.DefaultAccountGroupName}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/clients", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("clients status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Default Label Key") || !strings.Contains(body, "<strong>Default</strong>") {
		t.Fatalf("clients page should render Default account group: %s", body)
	}
	if strings.Contains(body, "<strong></strong>") {
		t.Fatalf("clients page rendered an empty account group label: %s", body)
	}
}

func TestUsageLogsPageRendersDefaultAccountGroupLabel(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	user := core.User{ID: "user_default_log", Username: "default-log", Role: core.UserRoleUser, Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_default_log", Name: "Default Log Key", APIKey: "gw_default_log", OwnerUserID: user.ID, Enabled: true, AccountGroup: core.DefaultAccountGroupName}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_default_log", Provider: core.ProviderOpenAI, Label: "Default Log Account", Group: core.DefaultAccountGroupName, Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_default_log",
		ClientID:        "client_default_log",
		ClientName:      "Default Log Key",
		UserID:          user.ID,
		AccountID:       "acct_default_log",
		Provider:        core.ProviderOpenAI,
		Model:           "gpt-4.1",
		ReservedNanoUSD: 100,
		Fingerprint:     "req_default_log",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:                 "req_default_log",
		ClientID:                  "client_default_log",
		AccountID:                 "acct_default_log",
		AccountGroup:              core.DefaultAccountGroupName,
		AccountGroupMultiplierBps: core.AccountGroupDefaultMultiplierBps,
		Provider:                  core.ProviderOpenAI,
		Model:                     "gpt-4.1",
		Usage:                     core.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		ActualNanoUSD:             120,
		FirstTokenMS:              237,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/logs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logs status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "req_default_log") || strings.Contains(body, "Request ID") {
		t.Fatalf("usage logs page should not render request id column: %s", body)
	}
	if !strings.Contains(body, "First Token") && !strings.Contains(body, "首 token") {
		t.Fatalf("usage logs page should render first token column for regular users: %s", body)
	}
	if !strings.Contains(body, "237 ms") {
		t.Fatalf("usage logs page should render first token value for regular users: %s", body)
	}
	if strings.Contains(body, "Default Log Account") || strings.Contains(body, "acct_default_log") {
		t.Fatalf("usage logs page should not render account column for regular users: %s", body)
	}
	if !strings.Contains(body, `data-label="Account Group">Default</td>`) {
		t.Fatalf("usage logs page should render Default account group: %s", body)
	}
}

func TestAdminUsageLogsPageRendersUserBeforeClient(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user := core.User{ID: "user_admin_log", Username: "usage-owner", Role: core.UserRoleUser, Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_admin_log", Name: "Admin Log Key", APIKey: "gw_admin_log", OwnerUserID: user.ID, Enabled: true, AccountGroup: core.DefaultAccountGroupName}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_admin_log", Provider: core.ProviderOpenAI, Label: "Admin Log Account", Group: core.DefaultAccountGroupName, Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_admin_log",
		ClientID:        "client_admin_log",
		ClientName:      "Admin Log Key",
		UserID:          user.ID,
		AccountID:       "acct_admin_log",
		Provider:        core.ProviderOpenAI,
		Model:           "gpt-5.5",
		ReservedNanoUSD: 100,
		Fingerprint:     "req_admin_log",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:                 "req_admin_log",
		ClientID:                  "client_admin_log",
		AccountID:                 "acct_admin_log",
		AccountGroup:              core.DefaultAccountGroupName,
		AccountGroupMultiplierBps: core.AccountGroupDefaultMultiplierBps,
		Provider:                  core.ProviderOpenAI,
		Model:                     "gpt-5.5",
		Usage:                     core.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		ActualNanoUSD:             120,
		FirstTokenMS:              1234,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, admin, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/logs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin logs status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "req_admin_log") || strings.Contains(body, "Request ID") {
		t.Fatalf("admin usage logs page should not render request id column: %s", body)
	}
	if !strings.Contains(body, "First Token") || !strings.Contains(body, "1.2 s") {
		t.Fatalf("admin usage logs page should render first token column: %s", body)
	}
	if !strings.Contains(body, "Admin Log Account") {
		t.Fatalf("admin usage logs page should render account label: %s", body)
	}
	if strings.Contains(body, "<code>acct_admin_log</code>") {
		t.Fatalf("admin usage logs page should not render internal account id: %s", body)
	}
	if !strings.Contains(body, `<a class="usage-user-link" href="/admin/users?q=user_admin_log"><strong>usage-owner</strong></a>`) {
		t.Fatalf("admin usage logs page should link usage user to users page: %s", body)
	}
	userIndex := strings.Index(body, "<strong>usage-owner</strong>")
	clientIndex := strings.Index(body, "<strong>Admin Log Key</strong>")
	accountIndex := strings.Index(body, "Admin Log Account")
	if userIndex < 0 || clientIndex < 0 || accountIndex < 0 || userIndex > clientIndex || clientIndex > accountIndex {
		t.Fatalf("admin usage logs page should render user before client before account: %s", body)
	}
}

func TestClientCreateDefaultUsesOnlyDefaultAccountGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	hide := false
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_hidden", Name: "Hidden", ShowInClientEditor: &hide}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_default", Provider: core.ProviderClaude, Label: "Default", Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_plus", Provider: core.ProviderOpenAI, Label: "Plus", Group: "Plus", Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_hidden", Provider: core.ProviderClaude, Label: "Hidden", Group: "Hidden", Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	getReq := httptest.NewRequest(http.MethodGet, "/clients/new", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("create page status = %d, want %d body=%s", getRec.Code, http.StatusOK, getRec.Body.String())
	}
	body := getRec.Body.String()
	for _, want := range []string{`value="Default" selected`, `value="Plus"`, `Default (1x)`} {
		if !strings.Contains(body, want) {
			t.Fatalf("create page missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, `value="Hidden"`) {
		t.Fatalf("hidden group should not be selectable for new keys: %s", body)
	}

	form := url.Values{}
	form.Set("name", "Inherited Default Key")
	form.Set("enabled", "on")
	form.Set("account_group", core.DefaultAccountGroupName)
	postReq := httptest.NewRequest(http.MethodPost, "/clients/new", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postRec := httptest.NewRecorder()
	handler.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("create submit status = %d, want %d body=%s", postRec.Code, http.StatusSeeOther, postRec.Body.String())
	}

	var client core.APIClient
	for _, candidate := range repo.ListClients() {
		if candidate.Name == "Inherited Default Key" {
			client = candidate
			break
		}
	}
	if client.ID == "" {
		t.Fatal("created client not found")
	}
	if client.OwnerUserID != admin.ID {
		t.Fatalf("owner = %q, want %q", client.OwnerUserID, admin.ID)
	}
	if client.AccountGroup != core.DefaultAccountGroupName {
		t.Fatalf("account group = %q, want default", client.AccountGroup)
	}
	routeProviders := append([]core.ProviderKind{client.RoutePolicy.DefaultProvider}, client.RoutePolicy.FallbackProviders...)
	if len(routeProviders) != 1 || !containsProvider(routeProviders, core.ProviderClaude) {
		t.Fatalf("route policy = %#v, want Default group provider only", client.RoutePolicy)
	}
}

func TestClientCreateEmptyAccountGroupShowsFormError(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_aa", Name: "aa"}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	form := url.Values{}
	form.Set("name", "Key for empty group")
	form.Set("enabled", "on")
	form.Set("account_group", "aa")
	req := httptest.NewRequest(http.MethodPost, "/clients/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); !strings.HasPrefix(location, "/clients/new?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("location = %q, want client create notice redirect", location)
	}
}

func TestClientCreateMalformedFormShowsClientFormError(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/clients/new", strings.NewReader("%"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); !strings.HasPrefix(location, "/clients/new?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("location = %q, want client create notice redirect", location)
	}
}

func TestClientEditEmptyAccountGroupShowsFormError(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_aa", Name: "aa"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_openai",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI",
		Status:   core.AccountStatusActive,
		Group:    "Plus",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:           "client_plus",
		Name:         "Plus Key",
		APIKey:       "old_key",
		OwnerUserID:  admin.ID,
		Enabled:      true,
		RoutePolicy:  core.DefaultRoutePolicy(),
		AccountGroup: "Plus",
	}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	form := url.Values{}
	form.Set("name", "Moved Key")
	form.Set("enabled", "on")
	form.Set("account_group", "aa")
	req := httptest.NewRequest(http.MethodPost, "/clients/client_plus/edit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); !strings.HasPrefix(location, "/clients/client_plus/edit?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("location = %q, want client edit notice redirect", location)
	}
	client, err := repo.GetClient("client_plus")
	if err != nil {
		t.Fatal(err)
	}
	if client.AccountGroup != "Plus" {
		t.Fatalf("account group = %q, want Plus", client.AccountGroup)
	}
}

func TestConnectClaudePageShowsWebAuthorizationLink(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/connect/claude", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "/admin/connect/claude/oauth") {
		t.Fatalf("body missing claude oauth link: %s", rec.Body.String())
	}
}

func TestConnectOpenAIPageShowsAccountUploadButton(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/connect/openai", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if strings.Contains(body, `codex-import-dir`) || strings.Contains(body, `name="path"`) {
		t.Fatalf("body should not show server path import controls: %s", body)
	}
	for _, want := range []string{`action="/admin/connect/openai/codex-import-upload`, `type="file"`, `name="accounts"`, `multiple`, `Upload Account`, `value="Default" selected`, `value="Plus"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, `<option value="">Default</option>`) {
		t.Fatalf("Default account group option should use an explicit value: %s", body)
	}
}

func TestConnectOpenAIErrorClearsTransientURLParam(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/connect/openai?error="+url.QueryEscape("import failed"), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "import failed") {
		t.Fatalf("body missing error message: %s", body)
	}
	if !strings.Contains(body, `data-clear-url-params="error"`) {
		t.Fatalf("connect error should clear transient URL params: %s", body)
	}
}

func TestConnectCompleteRedirectsToAccountsWithCreatedNotice(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	form := url.Values{}
	form.Set("provider", string(core.ProviderOpenAI))
	form.Set("label", "Primary OpenAI")
	form.Set("group", core.DefaultAccountGroupName)
	form.Set("access_token", "openai-token")
	form.Set("priority", "100")
	form.Set("weight", "100")
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/connect", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("connect submit status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/admin/accounts?created=1" {
		t.Fatalf("connect redirect = %q, want /admin/accounts?created=1", got)
	}
	accounts := repo.ListAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	if accounts[0].Group != core.DefaultAccountGroupName {
		t.Fatalf("account group = %q, want default group", accounts[0].Group)
	}

	noticeReq := httptest.NewRequest(http.MethodGet, "/admin/accounts?created=1", nil)
	noticeRec := httptest.NewRecorder()
	handler.ServeHTTP(noticeRec, noticeReq)
	if noticeRec.Code != http.StatusOK {
		t.Fatalf("accounts notice status = %d, want %d", noticeRec.Code, http.StatusOK)
	}
	body := noticeRec.Body.String()
	if !strings.Contains(body, `data-clear-url-params="created,imported"`) || !strings.Contains(body, "Account added") {
		t.Fatalf("accounts notice missing: %s", body)
	}
}

func TestConnectCompleteRedirectsToSelectedAccountGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	form := url.Values{}
	form.Set("provider", string(core.ProviderClaude))
	form.Set("label", "Plus Claude")
	form.Set("group", "Plus")
	form.Set("access_token", "claude-token")
	form.Set("priority", "100")
	form.Set("weight", "100")
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/connect", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("connect submit status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/admin/accounts?created=1&group=Plus" {
		t.Fatalf("connect redirect = %q, want selected account group", got)
	}

	noticeReq := httptest.NewRequest(http.MethodGet, rec.Header().Get("Location"), nil)
	noticeRec := httptest.NewRecorder()
	handler.ServeHTTP(noticeRec, noticeReq)
	if noticeRec.Code != http.StatusOK {
		t.Fatalf("accounts notice status = %d, want %d", noticeRec.Code, http.StatusOK)
	}
	body := noticeRec.Body.String()
	for _, want := range []string{`aria-selected="true"`, `Plus Claude`, "Account added"} {
		if !strings.Contains(body, want) {
			t.Fatalf("accounts page missing %q: %s", want, body)
		}
	}
}

func TestOpenAICodexImportUploadCreatesAccounts(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()
	sessionToken := mustConsoleSessionToken(t, control)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("csrf_token", testConsoleCSRFToken); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("group", "Plus"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("accounts", "accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name      string
		email     string
		accountID string
	}{
		{name: "codex-a.json", email: "a@example.com", accountID: "acct_a"},
		{name: "codex-b.json", email: "b@example.com", accountID: "acct_b"},
	} {
		payload := fmt.Sprintf(`{
  "type": "codex",
  "email": %q,
  "account_id": %q,
  "access_token": "header.payload.signature",
  "refresh_token": "refresh-%s",
  "id_token": "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJleHAiOjQxMDI0NDQ4MDB9.c2ln"
}`, item.email, item.accountID, item.accountID)
		if _, err := part.Write([]byte(payload)); err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte("\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/connect/openai/codex-import-upload", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: consoleSessionCookieName, Value: sessionToken, Path: "/"})
	req.AddCookie(&http.Cookie{Name: consoleCSRFCookieName, Value: testConsoleCSRFToken, Path: "/"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/admin/accounts?created=1&imported=2" {
		t.Fatalf("location = %q", got)
	}
	authenticatedHandler := authenticatedAdminHandler(t, control, handler)
	noticeReq := httptest.NewRequest(http.MethodGet, rec.Header().Get("Location"), nil)
	noticeRec := httptest.NewRecorder()
	authenticatedHandler.ServeHTTP(noticeRec, noticeReq)
	if noticeRec.Code != http.StatusOK {
		t.Fatalf("notice status = %d body=%s", noticeRec.Code, noticeRec.Body.String())
	}
	if body := noticeRec.Body.String(); !strings.Contains(body, `data-clear-url-params="created,imported"`) || !strings.Contains(body, "Account added") {
		t.Fatalf("account import notice should clear transient URL params: %s", body)
	}
	if accounts := repo.ListAccounts(); len(accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(accounts))
	} else {
		for _, account := range accounts {
			if account.Group != "Plus" {
				t.Fatalf("account %s group = %q, want Plus", account.ID, account.Group)
			}
			if got := account.Credential.Metadata[providers.OpenAICodexAuthPathMetadataKey]; got != "" {
				t.Fatalf("uploaded account should not store uploaded file path metadata, got %q", got)
			}
		}
	}
}

func TestClaudeOAuthStartPageRendersAuthorizationURL(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/connect/claude/oauth", nil)
	req.Host = "127.0.0.1:8088"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "https://claude.ai/oauth/authorize") {
		t.Fatalf("body missing claude authorize url: %s", body)
	}
	if !strings.Contains(body, "http%3A%2F%2F127.0.0.1%3A8088%2Fadmin%2Fconnect%2Fclaude%2Foauth%2Fcallback") {
		t.Fatalf("body missing encoded callback url: %s", body)
	}
}

func TestOpenAIOAuthPageRendersCompletionSummary(t *testing.T) {
	providers.ConfigureHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://auth.openai.com/api/accounts/deviceauth/usercode" {
			t.Fatalf("unexpected oauth request url: %s", req.URL.String())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"device_auth_id":"device_test","user_code":"USER-TEST","expires_in":900,"interval":5}`)),
			Request:    req,
		}, nil
	})})
	t.Cleanup(providers.ConfigureHTTPTransport)

	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/connect/openai/oauth", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Next Step") && !strings.Contains(body, "下一步") {
		t.Fatalf("body missing next step label: %s", body)
	}
	if !strings.Contains(body, "imported credential review") && !strings.Contains(body, "导入凭据确认页") {
		t.Fatalf("body missing completion summary: %s", body)
	}
}

func TestAccountEditPageShowsOAuthTokenUpdateAction(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_oauth",
		Provider: core.ProviderOpenAI,
		Label:    "OAuth Account",
		Status:   core.AccountStatusExpired,
		Credential: core.Credential{
			Mode:        providers.OpenAIOAuthModeValue(),
			AccessToken: "old-access",
			Metadata: map[string]string{
				"token_source": providers.OpenAIDeviceCodeTokenSourceValue(),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_api",
		Provider: core.ProviderOpenAI,
		Label:    "API Account",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "sk-test",
			Metadata: map[string]string{
				"account_login_method": "api_key",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts/acct_oauth/edit", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("oauth edit status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/admin/connect/openai/oauth?account_id=acct_oauth") {
		t.Fatalf("oauth token update link missing: %s", body)
	}
	if !strings.Contains(body, "Update Token") && !strings.Contains(body, "更新 Token") {
		t.Fatalf("oauth token update label missing: %s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/accounts/acct_api/edit", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("api edit status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "/admin/connect/openai/oauth?account_id=acct_api") {
		t.Fatalf("api key account should not render oauth token update link: %s", rec.Body.String())
	}
}

func TestOpenAIOAuthPollUpdatesExistingAccountToken(t *testing.T) {
	idToken := testUnsignedJWT(t, map[string]any{
		"email":              "new@example.com",
		"chatgpt_account_id": "acct_new_identity",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_new_identity",
		},
	})
	providers.ConfigureHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{}`
		switch req.URL.String() {
		case "https://auth.openai.com/api/accounts/deviceauth/usercode":
			body = `{"device_auth_id":"device_test","user_code":"USER-TEST","expires_in":900,"interval":5}`
		case "https://auth.openai.com/api/accounts/deviceauth/token":
			body = `{"authorization_code":"auth-code","code_verifier":"verifier"}`
		case "https://auth.openai.com/oauth/token":
			body = fmt.Sprintf(`{"access_token":"new-access","refresh_token":"new-refresh","id_token":%q,"expires_in":3600}`, idToken)
		default:
			t.Fatalf("unexpected oauth request url: %s", req.URL.String())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})})
	t.Cleanup(providers.ConfigureHTTPTransport)

	repo := storage.NewMemoryRepository()
	cooldownUntil := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	if err := repo.UpsertAccount(core.Account{
		ID:               "acct_oauth",
		Provider:         core.ProviderOpenAI,
		Label:            "OAuth Account",
		Status:           core.AccountStatusExpired,
		CooldownUntil:    &cooldownUntil,
		ConsecutiveFails: 3,
		Credential: core.Credential{
			Mode:         providers.OpenAIOAuthModeValue(),
			AccessToken:  "old-access",
			RefreshToken: "old-refresh",
			Metadata: map[string]string{
				"token_source":                    providers.OpenAIDeviceCodeTokenSourceValue(),
				core.AccountQuotaErrorMetadataKey: "credential_expired: refresh token was already used",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/connect/openai/oauth?account_id=acct_oauth", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("oauth start status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/admin/accounts/acct_oauth/edit") {
		t.Fatalf("oauth update page should return to account edit: %s", rec.Body.String())
	}

	form := url.Values{"device_code": {"device_test"}}
	req = httptest.NewRequest(http.MethodPost, "/admin/connect/openai/oauth/poll", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("oauth poll status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("Decode poll response: %v", err)
	}
	if payload["redirect"] != "/admin/accounts/acct_oauth/edit?token_updated=1" {
		t.Fatalf("redirect = %q", payload["redirect"])
	}
	account, err := repo.GetAccount("acct_oauth")
	if err != nil {
		t.Fatal(err)
	}
	if account.Credential.AccessToken != "new-access" || account.Credential.RefreshToken != "new-refresh" {
		t.Fatalf("tokens were not updated: %#v", account.Credential)
	}
	if account.Status != core.AccountStatusActive || account.CooldownUntil != nil || account.ConsecutiveFails != 0 {
		t.Fatalf("account state was not recovered: status=%s cooldown=%#v fails=%d", account.Status, account.CooldownUntil, account.ConsecutiveFails)
	}
	if account.Credential.Metadata["oauth_account_id"] != "acct_new_identity" ||
		account.Credential.Metadata["email"] != "new@example.com" ||
		account.Credential.Metadata[core.AccountQuotaErrorMetadataKey] != "" {
		t.Fatalf("metadata = %#v", account.Credential.Metadata)
	}
}

func TestClaudeOAuthCallbackRedirectsErrorsBackToStartPage(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/connect/claude/oauth/callback?error=access_denied&error_description=user%20cancelled", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if got := rec.Header().Get("Location"); got != "/admin/connect/claude/oauth?error=user+cancelled" {
		t.Fatalf("location = %q", got)
	}

	req = httptest.NewRequest(http.MethodGet, rec.Header().Get("Location"), nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("oauth error page status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `data-clear-url-params="error"`) {
		t.Fatalf("oauth error should clear transient URL params: %s", rec.Body.String())
	}
}

func TestDashboardRendersHealthAndRecoverAction(t *testing.T) {
	repo := storage.NewMemoryRepository()
	cooldownUntil := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	expiresAt := time.Now().UTC().Add(10 * time.Minute).Truncate(time.Second)
	for _, account := range []core.Account{
		{
			ID:               "acct_openai",
			Provider:         core.ProviderOpenAI,
			Label:            "OpenAI",
			Status:           core.AccountStatusCooling,
			CooldownUntil:    &cooldownUntil,
			ConsecutiveFails: 2,
			TotalFails:       4,
			Credential: core.Credential{
				AccessToken: "openai-token",
				ExpiresAt:   &expiresAt,
			},
		},
		{
			ID:       "acct_claude",
			Provider: core.ProviderClaude,
			Label:    "Claude",
			Status:   core.AccountStatusActive,
			Credential: core.Credential{
				AccessToken: "claude-token",
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"OpenAI",
		"Claude",
		"/admin/accounts/acct_openai/recover",
		"Recover",
		"data-confirm=",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q", want)
		}
	}
}

func TestAccountsPageRendersQuotaSnapshotAndRefreshAction(t *testing.T) {
	repo := storage.NewMemoryRepository()
	refreshedAt := time.Now().UTC().Truncate(time.Second)
	snapshotBody, err := json.Marshal(core.AccountQuotaSnapshot{
		Source:      "test",
		Plan:        "pro",
		Primary:     &core.AccountQuotaWindow{Name: "primary", UsedPercent: 42, WindowMinutes: 300},
		RefreshedAt: &refreshedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_openai_oauth",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI OAuth",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			AccessToken: "oauth-token",
			Metadata: map[string]string{
				"token_source":   "openai_device_code",
				"quota_snapshot": string(snapshotBody),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{"Refresh Quota", "pro", "/admin/accounts/acct_openai_oauth/refresh-quota", `data-account-filter-select`, `data-account-filter="normal"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q", want)
		}
	}
	if !strings.Contains(body, `account-pool-cooling`) || !strings.Contains(body, `filter=cooling`) {
		t.Fatalf("response missing cooling pool link: %s", body)
	}
}

func TestAccountsPageDisablesCaching(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache-control = %q, want no-store", got)
	}
}

func TestAccountsPageRendersControlAndRuntimeStatusTogether(t *testing.T) {
	repo := storage.NewMemoryRepository()
	reset := time.Now().UTC().Add(time.Hour)
	createdAt := time.Date(2026, 5, 10, 12, 30, 0, 0, time.UTC)
	snapshotBody, err := json.Marshal(core.AccountQuotaSnapshot{
		Primary: &core.AccountQuotaWindow{UsedPercent: 100, ResetsAt: &reset},
	})
	if err != nil {
		t.Fatal(err)
	}
	weekSnapshotBody, err := json.Marshal(core.AccountQuotaSnapshot{
		Secondary: &core.AccountQuotaWindow{UsedPercent: 100, ResetsAt: &reset},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:              "acct_disabled_limited",
		Provider:        core.ProviderOpenAI,
		Label:           "Disabled Limited",
		Status:          core.AccountStatusActive,
		ControlDisabled: true,
		CreatedAt:       createdAt,
		Credential: core.Credential{Metadata: map[string]string{
			core.AccountQuotaMetadataKey: string(snapshotBody),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_week_limited",
		Provider: core.ProviderOpenAI,
		Label:    "Week Limited",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{Metadata: map[string]string{
			core.AccountQuotaMetadataKey: string(weekSnapshotBody),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts?filter=time_limit", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{`data-account-filter="time_limit"`, "disabled", "5h limit", "Program state", "Server state"} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q: %s", want, body)
		}
	}
	if !strings.Contains(body, `<option value="cooling"`) {
		t.Fatalf("response missing cooling filter option: %s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/accounts?filter=disabled", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("disabled filter status = %d, want %d", rec.Code, http.StatusOK)
	}
	body = rec.Body.String()
	for _, want := range []string{`<option value="disabled" selected>`, `data-account-control-filter="disabled"`, "Added At", "Updated At", "Last Used", "Expires At", "Cooldown"} {
		if !strings.Contains(body, want) {
			t.Fatalf("disabled filter response missing %q: %s", want, body)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/accounts?filter=cooling", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cooling filter status = %d, want %d", rec.Code, http.StatusOK)
	}
	body = rec.Body.String()
	for _, want := range []string{`<option value="cooling" selected>`, `data-account-filter="time_limit"`, `data-account-filter="week_limit"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("cooling filter response missing %q: %s", want, body)
		}
	}
}

func TestAccountsPageRendersBatchControls(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_archive", Name: "Archive"}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{ID: "acct_a", Provider: core.ProviderOpenAI, Label: "A", Status: core.AccountStatusActive},
		{ID: "acct_b", Provider: core.ProviderOpenAI, Label: "B", Status: core.AccountStatusActive, ControlDisabled: true},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-account-batch-form`,
		`data-account-batch-job`,
		`data-active-url="/admin/accounts/batch/jobs/active"`,
		`data-refresh-url="/admin/accounts?group=Default"`,
		`action="/admin/accounts/batch"`,
		`data-account-select`,
		`name="current_filter" value="all"`,
		`data-account-id="acct_a"`,
		`name="action" value="enable"`,
		`name="action" value="disable"`,
		`name="action" value="refresh_quota"`,
		`name="action" value="test"`,
		`data-group-settings-open="account-batch-move-group"`,
		`name="action" value="move_group"`,
		`name="target_group"`,
		`<option value="Archive"`,
		`value="delete"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q", want)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/accounts?filter=exception", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered status = %d, want %d", rec.Code, http.StatusOK)
	}
	body = rec.Body.String()
	if !strings.Contains(body, `name="current_filter" value="exception"`) || !strings.Contains(body, `<option value="exception" selected>`) {
		t.Fatalf("account filter selection was not preserved: %s", body)
	}
}

func TestAccountBatchActionsDisableAndDeleteSelected(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_archive", Name: "Archive"}); err != nil {
		t.Fatal(err)
	}
	cooldownUntil := time.Now().UTC().Add(5 * time.Minute)
	for _, account := range []core.Account{
		{ID: "acct_a", Provider: core.ProviderOpenAI, Label: "A", Group: "Plus", Status: core.AccountStatusActive},
		{ID: "acct_b", Provider: core.ProviderOpenAI, Label: "B", Group: "Plus", Status: core.AccountStatusCooling, CooldownUntil: &cooldownUntil},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	form := url.Values{}
	form.Set("current_group", "Plus")
	form.Set("current_filter", "exception")
	form.Set("action", "disable")
	form.Add("account_id", "acct_a")
	form.Add("account_id", "acct_b")
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/batch", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("disable status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.Contains(location, "group=Plus") || !strings.Contains(location, "filter=exception") || !strings.Contains(location, "batch_tone=good") {
		t.Fatalf("disable redirect = %q", location)
	}
	for _, id := range []string{"acct_a", "acct_b"} {
		account, err := repo.GetAccount(id)
		if err != nil {
			t.Fatal(err)
		}
		if !account.ControlDisabled {
			t.Fatalf("%s account = %#v, want control disabled", id, account)
		}
		if id == "acct_b" && (account.Status != core.AccountStatusCooling || account.CooldownUntil == nil) {
			t.Fatalf("%s account = %#v, want runtime cooldown preserved", id, account)
		}
	}

	form = url.Values{}
	form.Set("current_group", "Plus")
	form.Set("action", "enable")
	form.Add("account_id", "acct_b")
	req = httptest.NewRequest(http.MethodPost, "/admin/accounts/batch", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("enable status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	account, err := repo.GetAccount("acct_b")
	if err != nil {
		t.Fatal(err)
	}
	if account.ControlDisabled {
		t.Fatalf("acct_b account = %#v, want control enabled", account)
	}
	if account.Status != core.AccountStatusCooling || account.CooldownUntil == nil {
		t.Fatalf("acct_b account = %#v, want runtime cooldown preserved", account)
	}

	form = url.Values{}
	form.Set("current_group", "Plus")
	form.Set("current_filter", "exception")
	form.Set("target_group", "Archive")
	form.Set("action", "move_group")
	form.Add("account_id", "acct_b")
	req = httptest.NewRequest(http.MethodPost, "/admin/accounts/batch", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("move group status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location = rec.Header().Get("Location")
	if !strings.Contains(location, "group=Archive") || !strings.Contains(location, "filter=exception") || !strings.Contains(location, "batch_tone=good") {
		t.Fatalf("move group redirect = %q", location)
	}
	account, err = repo.GetAccount("acct_b")
	if err != nil {
		t.Fatal(err)
	}
	if account.Group != "Archive" || account.Status != core.AccountStatusCooling || account.CooldownUntil == nil {
		t.Fatalf("acct_b account = %#v, want group moved and runtime state preserved", account)
	}

	form = url.Values{}
	form.Set("current_group", "Plus")
	form.Set("action", "delete")
	form.Add("account_id", "acct_a")
	req = httptest.NewRequest(http.MethodPost, "/admin/accounts/batch", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if _, err := repo.GetAccount("acct_a"); err == nil {
		t.Fatal("expected acct_a to be deleted")
	}
	if _, err := repo.GetAccount("acct_b"); err != nil {
		t.Fatal("acct_b should remain")
	}
}

func TestAccountBatchActionRequiresSelection(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	form := url.Values{}
	form.Set("current_group", "Plus")
	form.Set("action", "disable")
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/batch", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/admin/accounts?") || !strings.Contains(location, "notice_error=") || !strings.Contains(location, "group=Plus") {
		t.Fatalf("location = %q, want accounts notice redirect", location)
	}
}

func TestAccountBatchLiveActionStartsJobForJSON(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_batch_job",
		Provider: core.ProviderOpenAI,
		Label:    "Batch Job",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			AccessToken: "token",
			Metadata: map[string]string{
				"token_source": "api_key",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	control := controlplane.New(repo, providers.NewRegistry(&testWebQuotaAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	form := url.Values{}
	form.Set("action", "test")
	form.Add("account_id", "acct_batch_job")
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/batch", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start status = %d, want %d body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode start payload: %v", err)
	}
	jobID, _ := payload["id"].(string)
	if !strings.HasPrefix(jobID, "acctbatch_") {
		t.Fatalf("job id = %q", jobID)
	}

	waitForWebBatchJob(t, handler, jobID)
	req = httptest.NewRequest(http.MethodGet, "/admin/accounts/batch/jobs/"+jobID, nil)
	req.Header.Set("Accept", "application/json")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode status payload: %v", err)
	}
	if payload["state"] != string(controlplane.AccountBatchJobCompleted) || payload["succeeded"] != float64(1) {
		t.Fatalf("payload = %#v, want completed success", payload)
	}
}

func waitForWebBatchJob(t *testing.T, handler http.Handler, jobID string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for web batch job %q", jobID)
		default:
		}
		req := httptest.NewRequest(http.MethodGet, "/admin/accounts/batch/jobs/"+jobID, nil)
		req.Header.Set("Accept", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			var payload map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode batch job payload: %v", err)
			}
			state, _ := payload["state"].(string)
			if state == string(controlplane.AccountBatchJobCompleted) || state == string(controlplane.AccountBatchJobCancelled) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAccountsPageUsesRuntimeStatusForQuotaAndStaleRefreshing(t *testing.T) {
	repo := storage.NewMemoryRepository()
	reset := time.Now().UTC().Add(time.Hour)
	quotaBody, err := json.Marshal(core.AccountQuotaSnapshot{
		Plan:    "plus",
		Primary: &core.AccountQuotaWindow{Name: "primary", UsedPercent: 100, WindowMinutes: 300, ResetsAt: &reset},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{
			ID:       "acct_quota_limit",
			Provider: core.ProviderOpenAI,
			Label:    "Quota Limited",
			Status:   core.AccountStatusActive,
			Credential: core.Credential{Metadata: map[string]string{
				"token_source":                     "openai_device_code",
				core.AccountQuotaMetadataKey:       string(quotaBody),
				"account_type":                     "official",
				"openai_account_subscription_plan": "plus",
			}},
		},
		{
			ID:       "acct_stale_refreshing",
			Provider: core.ProviderOpenAI,
			Label:    "Stale Refreshing",
			Status:   core.AccountStatusRefreshing,
			Credential: core.Credential{Metadata: map[string]string{
				"token_source": "openai_device_code",
			}},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{"Quota Limited", "5h limit", `data-account-filter="time_limit"`, "Stale Refreshing", "active"} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q", want)
		}
	}
	if strings.Contains(body, ">refreshing<") {
		t.Fatalf("stale refreshing account should render as active")
	}
}

func TestAccountRefreshQuotaActionStoresSnapshot(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_oauth",
		Provider: core.ProviderOpenAI,
		Label:    "OAuth Account",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			AccessToken: "oauth-token",
			Metadata: map[string]string{
				"token_source": "openai_device_code",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&testWebQuotaAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/acct_oauth/refresh-quota", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}

	account, err := repo.GetAccount("acct_oauth")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := controlplane.ReadAccountQuota(account)
	if snapshot == nil || snapshot.Plan != "pro" {
		t.Fatalf("quota snapshot = %#v", snapshot)
	}
}

func TestAccountRefreshQuotaErrorRedirectsWithNotice(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_oauth_error",
		Provider: core.ProviderOpenAI,
		Label:    "OAuth Account",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			AccessToken: "oauth-token",
			Metadata: map[string]string{
				"token_source": "openai_device_code",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	control := controlplane.New(repo, providers.NewRegistry(&failingWebQuotaAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/acct_oauth_error/refresh-quota", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/admin/accounts?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("location = %q, want accounts notice redirect", location)
	}
	noticeReq := httptest.NewRequest(http.MethodGet, location, nil)
	noticeRec := httptest.NewRecorder()
	handler.ServeHTTP(noticeRec, noticeReq)
	if noticeRec.Code != http.StatusOK {
		t.Fatalf("notice status = %d, want %d", noticeRec.Code, http.StatusOK)
	}
	body := noticeRec.Body.String()
	if !strings.Contains(body, `data-clear-url-params="notice_error"`) || !strings.Contains(body, "quota unavailable") {
		t.Fatalf("notice missing quota error: %s", body)
	}
}

func TestAccountsPageRendersQuotaRefreshErrorWithoutExceptionFilter(t *testing.T) {
	repo := storage.NewMemoryRepository()
	errorAt := time.Now().UTC().Truncate(time.Second)
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_openai_oauth",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI OAuth",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			AccessToken: "oauth-token",
			Metadata: map[string]string{
				"token_source":           "openai_device_code",
				"quota_refresh_error":    "quota backend unavailable",
				"quota_refresh_error_at": errorAt.Format(time.RFC3339),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{"quota backend unavailable", "Quota Error", `data-account-filter="normal"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q", want)
		}
	}
}

func TestAccountsPageDoesNotTreatQuotaRefreshErrorWithUsableSnapshotAsException(t *testing.T) {
	repo := storage.NewMemoryRepository()
	snapshotBody, err := json.Marshal(core.AccountQuotaSnapshot{
		Plan:      "plus",
		Primary:   &core.AccountQuotaWindow{Name: "primary", UsedPercent: 49, WindowMinutes: 300},
		Secondary: &core.AccountQuotaWindow{Name: "secondary", UsedPercent: 30, WindowMinutes: 10080},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_openai_normal_with_quota_error",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI Normal With Quota Error",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			AccessToken: "oauth-token",
			Metadata: map[string]string{
				"token_source":                    "openai_device_code",
				core.AccountQuotaMetadataKey:      string(snapshotBody),
				core.AccountQuotaErrorMetadataKey: "transient quota refresh failed",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{"OpenAI Normal With Quota Error", `data-account-filter="normal"`, "transient quota refresh failed"} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q", want)
		}
	}
}

func TestAuditPageFiltersAdminEvents(t *testing.T) {
	repo := storage.NewMemoryRepository()
	now := time.Now().UTC()
	for _, event := range []core.AuditEvent{
		{
			ID:           "audit_admin_1",
			Kind:         core.AuditKindAdmin,
			Actor:        "admin",
			Action:       "user.update",
			ResourceType: "user",
			ResourceID:   "user_admin",
			ResourceName: "admin",
			Status:       "ok",
			Message:      "enabled=true",
			CreatedAt:    now,
		},
		{
			ID:          "audit_gateway_1",
			Kind:        core.AuditKindGateway,
			ClientID:    "client_a",
			ClientName:  "Client A",
			Provider:    core.ProviderOpenAI,
			Model:       "gpt-4.1",
			Status:      "ok",
			Message:     "hello world",
			RequestBody: "{\n  \"model\": \"gpt-4.1\"\n}",
			CreatedAt:   now.Add(-time.Second),
		},
	} {
		if err := repo.AppendAudit(event); err != nil {
			t.Fatal(err)
		}
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/audit?kind=admin&actor=admin", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{"admin", "update user", "Details", "User", "Enabled:", "yes"} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q", want)
		}
	}
	if strings.Contains(body, "enabled=true") {
		t.Fatalf("response should not expose raw audit key/value text: %s", body)
	}
	if !strings.Contains(body, "name=\"resource\"") {
		t.Fatalf("response should include resource filter: %q", body)
	}
	if strings.Contains(body, "Client A") {
		t.Fatalf("response should not include filtered gateway event: %q", body)
	}
}

func TestAuditPageDoesNotRenderRequestBody(t *testing.T) {
	repo := storage.NewMemoryRepository()
	now := time.Now().UTC()
	if err := repo.AppendAudit(core.AuditEvent{
		ID:          "audit_gateway_1",
		Kind:        core.AuditKindGateway,
		ClientID:    "client_a",
		ClientName:  "Client A",
		Provider:    core.ProviderOpenAI,
		Model:       "gpt-4.1",
		Status:      "ok",
		Message:     "hello world",
		RequestBody: "{\n  \"requested_model\": \"gpt-4.1\",\n  \"upstream_model\": \"gpt-5.4\"\n}",
		CreatedAt:   now,
	}); err != nil {
		t.Fatal(err)
	}
	registry := providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{})
	control := controlplane.New(repo, registry)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	req := httptest.NewRequest(http.MethodGet, "/admin/audit?kind=gateway", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, unwanted := range []string{"requested_model", "upstream_model"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("response should not include request detail content %q", unwanted)
		}
	}
	for _, want := range []string{"Details", "hello world"} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q: %s", want, body)
		}
	}
}

func TestAuditMessagePreviewDoesNotSplitUTF8(t *testing.T) {
	message := strings.Repeat("排查", 80)
	preview := auditMessagePreview(core.AuditEvent{Message: message})
	if strings.Contains(preview, "\uFFFD") {
		t.Fatalf("preview contains replacement character: %q", preview)
	}
	if !strings.HasSuffix(preview, "...") {
		t.Fatalf("preview should be truncated: %q", preview)
	}
}

func TestAuditDetailLinesHumanizeAdminFields(t *testing.T) {
	event := core.AuditEvent{
		Kind:         core.AuditKindAdmin,
		Action:       "billing.release_reserved",
		ResourceType: "billing_request",
		ResourceID:   "req_1778389400113078736_122",
		ResourceName: "client_default",
		Status:       "ok",
		Message:      "user=user_1778278375159771731 amount=1.39474 model=gpt-5.5",
	}
	lines := auditDetailLines(localeZH, event)
	text := auditDetailLinesTestText(lines)
	for _, want := range []string{"密钥:client_default", "请求 ID:req_1778389400113078736_122", "用户:user_1778278375159771731", "金额:$1.39474", "模型:gpt-5.5"} {
		if !strings.Contains(text, want) {
			t.Fatalf("detail lines %q missing %q", text, want)
		}
	}
	for _, unwanted := range []string{"user=", "amount=", "model="} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("detail lines should not expose raw key/value %q: %q", unwanted, text)
		}
	}

	event = core.AuditEvent{
		Kind:         core.AuditKindAdmin,
		Action:       "account.toggle",
		ResourceType: "account",
		ResourceID:   "acct_openai_1",
		ResourceName: "OpenAI Account",
		Status:       "ok",
		Message:      "control_disabled=false status=active",
	}
	lines = auditDetailLines(localeZH, event)
	text = auditDetailLinesTestText(lines)
	for _, want := range []string{"账号:OpenAI Account", "账号 ID:acct_openai_1", "变更:账号已启用，当前状态：正常"} {
		if !strings.Contains(text, want) {
			t.Fatalf("detail lines %q missing %q", text, want)
		}
	}
	if strings.Contains(text, "control_disabled") || strings.Contains(text, "status=active") {
		t.Fatalf("detail lines should not expose raw account state fields: %q", text)
	}

	event = core.AuditEvent{
		Kind:    core.AuditKindAdmin,
		Action:  "account.batch.refresh_quota",
		Status:  "ok",
		Message: "action=refresh_quota total=7 succeeded=7 failed=0 skipped=0",
	}
	lines = auditDetailLines(localeZH, event)
	text = auditDetailLinesTestText(lines)
	for _, want := range []string{"结果:刷新配额：共 7 个，成功 7 个，失败 0 个，跳过 0 个"} {
		if !strings.Contains(text, want) {
			t.Fatalf("detail lines %q missing %q", text, want)
		}
	}
	for _, unwanted := range []string{"action:", "total:", "succeeded:", "failed:", "skipped:", "refresh_quota"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("detail lines should not expose raw batch field %q: %q", unwanted, text)
		}
	}

	event = core.AuditEvent{
		Kind:         core.AuditKindAdmin,
		Action:       "system_settings.update",
		ResourceType: "system_settings",
		ResourceID:   "global",
		Status:       "ok",
		Message:      "audit_limit=512",
	}
	lines = auditDetailLines(localeZH, event)
	text = auditDetailLinesTestText(lines)
	for _, want := range []string{"设置项:全局设置", "变更:审计保留上限：512"} {
		if !strings.Contains(text, want) {
			t.Fatalf("detail lines %q missing %q", text, want)
		}
	}
	if strings.Contains(text, "global") || strings.Contains(text, "audit_limit") {
		t.Fatalf("detail lines should not expose raw settings fields: %q", text)
	}

	event = core.AuditEvent{
		Kind:         core.AuditKindAdmin,
		Action:       "system_settings.update",
		ResourceType: "system_settings",
		ResourceID:   "global",
		Status:       "ok",
		Message:      "user_concurrent_request_limit=3",
	}
	lines = auditDetailLines(localeZH, event)
	text = auditDetailLinesTestText(lines)
	for _, want := range []string{"设置项:全局设置", "变更:用户并发请求上限：3"} {
		if !strings.Contains(text, want) {
			t.Fatalf("detail lines %q missing %q", text, want)
		}
	}
	if strings.Contains(text, "user_concurrent_request_limit") {
		t.Fatalf("detail lines should not expose raw concurrency fields: %q", text)
	}

	event = core.AuditEvent{
		Kind:         core.AuditKindAdmin,
		Action:       "system_settings.update",
		ResourceType: "system_settings",
		ResourceID:   "global",
		Status:       "ok",
		Message:      "plan_concurrent_request_limit=2",
	}
	lines = auditDetailLines(localeZH, event)
	text = auditDetailLinesTestText(lines)
	for _, want := range []string{"设置项:全局设置", "变更:套餐并发请求上限：2"} {
		if !strings.Contains(text, want) {
			t.Fatalf("detail lines %q missing %q", text, want)
		}
	}
	if strings.Contains(text, "plan_concurrent_request_limit") {
		t.Fatalf("detail lines should not expose raw plan concurrency fields: %q", text)
	}

}

func TestAuditDetailLinesShowBeforeAfterChanges(t *testing.T) {
	event := core.AuditEvent{
		Kind:         core.AuditKindAdmin,
		Action:       "system_settings.update",
		ResourceType: "system_settings",
		ResourceID:   "global",
		Status:       "ok",
		Message: auditChangeMessage(
			auditIntChange("audit_limit", 128, 512),
			auditBoolChange("allow_public_registration", false, true),
			auditTextChange("brand_title", "AI Gateway", "32NS Gateway"),
		),
	}
	text := auditDetailLinesTestText(auditDetailLines(localeZH, event))
	for _, want := range []string{"设置项:全局设置", "审计保留上限:128 -> 512", "公开注册:否 -> 是", "品牌标题:AI Gateway -> 32NS Gateway"} {
		if !strings.Contains(text, want) {
			t.Fatalf("detail lines %q missing %q", text, want)
		}
	}
	for _, unwanted := range []string{"audit_limit_from", "allow_public_registration_to", "brand_title_from"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("detail lines should not expose raw change field %q: %q", unwanted, text)
		}
	}

	event = core.AuditEvent{
		Kind:         core.AuditKindAdmin,
		Action:       "system_settings.update",
		ResourceType: "system_settings",
		ResourceID:   "global",
		Status:       "ok",
		Message: auditChangeMessage(
			auditIntChange("user_concurrent_request_limit", 0, 3),
		),
	}
	text = auditDetailLinesTestText(auditDetailLines(localeZH, event))
	for _, want := range []string{"设置项:全局设置", "用户并发请求上限:0 -> 3"} {
		if !strings.Contains(text, want) {
			t.Fatalf("detail lines %q missing %q", text, want)
		}
	}
	if strings.Contains(text, "user_concurrent_request_limit_from") {
		t.Fatalf("detail lines should not expose raw concurrency change field: %q", text)
	}

	event = core.AuditEvent{
		Kind:         core.AuditKindAdmin,
		Action:       "system_settings.update",
		ResourceType: "system_settings",
		ResourceID:   "global",
		Status:       "ok",
		Message: auditChangeMessage(
			auditIntChange("plan_concurrent_request_limit", 0, 2),
		),
	}
	text = auditDetailLinesTestText(auditDetailLines(localeZH, event))
	for _, want := range []string{"设置项:全局设置", "套餐并发请求上限:0 -> 2"} {
		if !strings.Contains(text, want) {
			t.Fatalf("detail lines %q missing %q", text, want)
		}
	}
	if strings.Contains(text, "plan_concurrent_request_limit_from") {
		t.Fatalf("detail lines should not expose raw plan concurrency change field: %q", text)
	}

	event = core.AuditEvent{
		Kind:         core.AuditKindAdmin,
		Action:       "model.price",
		ResourceType: "model",
		ResourceID:   "gpt-5.5",
		ResourceName: "GPT-5.5",
		Status:       "ok",
		Message: auditChangeMessage(
			auditAmountChange("input", core.NanoUSDPerUSD, 2500000000),
			auditAmountChange("request", 0, 10000000),
			auditBoolChange("fixed", false, true),
			auditIntChange("tiers", 1, 3),
		),
	}
	text = auditDetailLinesTestText(auditDetailLines(localeZH, event))
	for _, want := range []string{"模型:GPT-5.5", "输入价:$1 -> $2.5", "请求价:$0 -> $0.01", "固定:否 -> 是", "价格阶梯数:1 -> 3"} {
		if !strings.Contains(text, want) {
			t.Fatalf("detail lines %q missing %q", text, want)
		}
	}

	event = core.AuditEvent{
		Kind:         core.AuditKindAdmin,
		Action:       "user.update",
		ResourceType: "user",
		ResourceID:   "user_1",
		ResourceName: "alice",
		Status:       "ok",
		Message: auditChangeMessage(
			auditTextChange("role", string(core.UserRoleUser), string(core.UserRoleAdmin)),
			auditBoolChange("enabled", true, false),
			auditBoolChange("password_changed", false, true),
		),
	}
	text = auditDetailLinesTestText(auditDetailLines(localeZH, event))
	for _, want := range []string{"用户:alice", "角色:用户 -> 管理员", "启用:是 -> 否", "密码已更新:否 -> 是"} {
		if !strings.Contains(text, want) {
			t.Fatalf("detail lines %q missing %q", text, want)
		}
	}
}

func auditDetailLinesTestText(lines []auditDetailLine) string {
	parts := make([]string, 0, len(lines))
	for _, line := range lines {
		parts = append(parts, line.Label+":"+line.Value)
	}
	return strings.Join(parts, "\n")
}

func TestAuditOperationTextShowsUserBalanceAmount(t *testing.T) {
	event := core.AuditEvent{
		Kind:         core.AuditKindAdmin,
		Action:       "user.balance",
		ResourceName: "alice",
		Message:      "previous=1 current=3.5 delta=2.5 reason=manual top up",
	}
	text := auditOperationText(localeZH, event)
	for _, want := range []string{"充值", "alice", "+$2.50", "$1.00 -> $3.50", "manual top up"} {
		if !strings.Contains(text, want) {
			t.Fatalf("operation text %q missing %q", text, want)
		}
	}

	event.Message = "previous=3.5 current=2 delta=-1.5 reason=correction"
	text = auditOperationText(localeZH, event)
	for _, want := range []string{"扣减", "-$1.50", "$3.50 -> $2.00"} {
		if !strings.Contains(text, want) {
			t.Fatalf("operation text %q missing %q", text, want)
		}
	}

	event.Message = "previous=2 current=4 delta=2 reason=contains current=marker delta=marker"
	text = auditOperationText(localeZH, event)
	if !strings.Contains(text, "contains current=marker delta=marker") {
		t.Fatalf("operation text %q should preserve full reason", text)
	}
}

func TestAuditActionTextLocalizesCurrentAdminActions(t *testing.T) {
	actions := recordedAdminAuditActions(t)
	for _, action := range []controlplane.AccountBatchAction{
		controlplane.AccountBatchActionDisable,
		controlplane.AccountBatchActionEnable,
		controlplane.AccountBatchActionDelete,
		controlplane.AccountBatchActionRefreshQuota,
		controlplane.AccountBatchActionTest,
		controlplane.AccountBatchActionMoveGroup,
	} {
		actions["account.batch."+string(action)] = struct{}{}
	}
	for action := range actions {
		if got := auditActionText(localeZH, action); got == action {
			t.Fatalf("auditActionText(%q) is not localized for zh-CN", action)
		}
		if got := auditActionText(localeEN, action); got == action {
			t.Fatalf("auditActionText(%q) is not localized for en", action)
		}
	}
}

func recordedAdminAuditActions(t *testing.T) map[string]struct{} {
	t.Helper()
	actions := map[string]struct{}{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read web package dir: %v", err)
	}
	files := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || !isRecordAdminAuditCall(call.Fun) || len(call.Args) < 3 {
				return true
			}
			lit, ok := call.Args[2].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			action, err := strconv.Unquote(lit.Value)
			if err != nil || strings.TrimSpace(action) == "" {
				return true
			}
			actions[action] = struct{}{}
			return true
		})
	}
	return actions
}

func isRecordAdminAuditCall(expr ast.Expr) bool {
	switch value := expr.(type) {
	case *ast.SelectorExpr:
		return value.Sel.Name == "recordAdminAudit"
	case *ast.Ident:
		return value.Name == "recordAdminAudit"
	default:
		return false
	}
}

func TestAdminDeleteUserPreservesBillingHistory(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	target, err := control.CreateUser(controlplane.UserInput{
		Username:       "force-delete-target",
		Password:       "target-secret",
		Role:           core.UserRoleUser,
		Enabled:        true,
		BalanceNanoUSD: 100000,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_delete_preserve", Name: "Delete Preserve", APIKey: "gw_delete_preserve", OwnerUserID: target.ID, Enabled: true}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_delete_preserve",
		ClientID:        "client_delete_preserve",
		UserID:          target.ID,
		Model:           "gpt-4.1",
		ReservedNanoUSD: 100,
		Fingerprint:     "req_delete_preserve",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	targetPath := "/admin/users/" + target.ID + "/delete"

	form := url.Values{}
	req := httptest.NewRequest(http.MethodPost, targetPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if _, err := repo.GetUser(target.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetUser err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetClient("client_delete_preserve"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetClient err = %v, want ErrNotFound", err)
	}
	if items, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{UserID: target.ID, Limit: 10}); total != 1 || len(items) != 1 {
		t.Fatalf("billing requests after user delete = total %d items %#v, want preserved", total, items)
	}
}

func TestAdminDeleteUserCanDeleteInvitedUsers(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	inviter, err := control.CreateUser(controlplane.UserInput{
		Username: "abusive-inviter",
		Password: "inviter-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser inviter returned error: %v", err)
	}
	invited, err := control.CreateUser(controlplane.UserInput{
		Username:      "invited-alt",
		Password:      "invited-secret",
		Role:          core.UserRoleUser,
		Enabled:       true,
		InviterUserID: inviter.ID,
	})
	if err != nil {
		t.Fatalf("CreateUser invited returned error: %v", err)
	}
	unrelated, err := control.CreateUser(controlplane.UserInput{
		Username: "unrelated-user",
		Password: "unrelated-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser unrelated returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	pageReq := httptest.NewRequest(http.MethodGet, "/admin/users?partial=users-panel", nil)
	pageRec := httptest.NewRecorder()
	handler.ServeHTTP(pageRec, pageReq)
	if pageRec.Code != http.StatusOK {
		t.Fatalf("users page status = %d, want %d body=%s", pageRec.Code, http.StatusOK, pageRec.Body.String())
	}
	pageBody := pageRec.Body.String()
	if !strings.Contains(pageBody, `data-confirm-option-name="delete_invited_users"`) || !strings.Contains(pageBody, "Delete invited users") && !strings.Contains(pageBody, "删除邀请用户") {
		t.Fatalf("users page missing delete invited option: %s", pageBody)
	}

	form := url.Values{"delete_invited_users": {"1"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/users/"+inviter.ID+"/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	for _, id := range []string{inviter.ID, invited.ID} {
		if _, err := repo.GetUser(id); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("GetUser(%s) err = %v, want ErrNotFound", id, err)
		}
	}
	if _, err := repo.GetUser(unrelated.ID); err != nil {
		t.Fatalf("unrelated user should remain: %v", err)
	}
}

func responseCookie(resp *http.Response, name string) (*http.Cookie, bool) {
	for _, cookie := range resp.Cookies() {
		if cookie.Name == name {
			return cookie, true
		}
	}
	return nil, false
}

func extractCSRFToken(t *testing.T, body string) string {
	t.Helper()
	marker := `name="csrf_token" value="`
	idx := strings.Index(body, marker)
	if idx < 0 {
		t.Fatalf("response missing csrf token field: %q", body)
	}
	start := idx + len(marker)
	end := strings.Index(body[start:], `"`)
	if end < 0 {
		t.Fatalf("response has malformed csrf token field: %q", body[start:])
	}
	return body[start : start+end]
}

type testWebQuotaAdapter struct{}

func (a *testWebQuotaAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *testWebQuotaAdapter) DisplayName() string { return "OpenAI" }

func (a *testWebQuotaAdapter) ListModels(context.Context) []core.ModelSpec { return nil }

func (a *testWebQuotaAdapter) Invoke(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
	return &core.GatewayResponse{
		ID:           "resp_detect",
		Model:        decision.Model,
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Content:      "pong",
		CreatedAt:    time.Now().UTC(),
	}, nil
}

func (a *testWebQuotaAdapter) OpenStream(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*providers.StreamSession, error) {
	return testWebStreamSession(decision,
		&core.StreamEvent{
			Delta:    "pong",
			RawEvent: "response.output_text.delta",
			RawData:  []byte(`{"type":"response.output_text.delta","delta":"pong"}`),
		},
		&core.StreamEvent{
			FinishReason: "stop",
			Done:         true,
			RawEvent:     "response.completed",
			RawData:      []byte(`{"type":"response.completed","response":{"status":"completed"}}`),
		},
	), nil
}

func (a *testWebQuotaAdapter) FetchModels(_ context.Context, account core.Account) ([]providers.UpstreamModel, error) {
	return []providers.UpstreamModel{{ID: "gpt-test", OwnedBy: string(account.Provider)}}, nil
}

func (a *testWebQuotaAdapter) FetchQuota(context.Context, core.Account) (core.AccountQuotaSnapshot, error) {
	refreshedAt := time.Now().UTC()
	return core.AccountQuotaSnapshot{
		Source:      "test",
		Plan:        "pro",
		RefreshedAt: &refreshedAt,
	}, nil
}

type failingWebQuotaAdapter struct{}

func (a *failingWebQuotaAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *failingWebQuotaAdapter) DisplayName() string { return "OpenAI" }

func (a *failingWebQuotaAdapter) ListModels(context.Context) []core.ModelSpec { return nil }

func (a *failingWebQuotaAdapter) Invoke(context.Context, core.RouteDecision, *core.GatewayRequest) (*core.GatewayResponse, error) {
	return nil, fmt.Errorf("ping unavailable")
}

func (a *failingWebQuotaAdapter) OpenStream(context.Context, core.RouteDecision, *core.GatewayRequest) (*providers.StreamSession, error) {
	return nil, fmt.Errorf("ping unavailable")
}

func (a *failingWebQuotaAdapter) FetchModels(context.Context, core.Account) ([]providers.UpstreamModel, error) {
	return nil, fmt.Errorf("ping unavailable")
}

func (a *failingWebQuotaAdapter) FetchQuota(context.Context, core.Account) (core.AccountQuotaSnapshot, error) {
	return core.AccountQuotaSnapshot{}, fmt.Errorf("quota unavailable")
}

func testWebStreamSession(decision core.RouteDecision, events ...*core.StreamEvent) *providers.StreamSession {
	return &providers.StreamSession{
		Response: &core.GatewayResponse{
			ID:           "resp_stream_test",
			Model:        decision.Model,
			Provider:     decision.Provider,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			CreatedAt:    time.Now().UTC(),
		},
		Stream: &testWebStream{events: events},
	}
}

type testWebStream struct {
	events []*core.StreamEvent
	index  int
}

func (s *testWebStream) Next() (*core.StreamEvent, error) {
	if s.index >= len(s.events) {
		return nil, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *testWebStream) Close() error {
	return nil
}
