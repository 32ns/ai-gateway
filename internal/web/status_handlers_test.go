package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/accounts"
	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/failover"
	"github.com/32ns/ai-gateway/internal/gateway"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/routing"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestStatusPageShowsOnlyPublicTargetsWithoutAccountDetails(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	seedStatusPageMonitorData(t, repo)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Public Default", "gpt-5", "normal", "42ms", "Next Check", `data-monitor-countdown`, `data-next-check-at=`} {
		if !strings.Contains(body, want) {
			t.Fatalf("public status page missing %q: %s", want, body)
		}
	}
	for _, hidden := range []string{"Private Plus", "Disabled Public", "Internal Account Label", "acct_internal"} {
		if strings.Contains(body, hidden) {
			t.Fatalf("public status page leaked %q: %s", hidden, body)
		}
	}
}

func TestStatusPageWithoutControlDoesNotPanic(t *testing.T) {
	server := NewServer(nil, nil, "data/state.db")
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "No public monitors") {
		t.Fatalf("empty status page missing empty state: %s", rec.Body.String())
	}
}

func TestAdminStatusPageShowsPrivateTargetsAndAttempts(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	seedStatusPageMonitorData(t, repo)

	req := httptest.NewRequest(http.MethodGet, "/admin/status", nil)
	req = req.WithContext(withConsoleUser(req.Context(), core.User{ID: "admin", Username: "admin", Role: core.UserRoleAdmin, Enabled: true}))
	rec := httptest.NewRecorder()
	server.handleAdminStatusPage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Public Default", "Private Plus", "Internal Account Label", "upstream failed", "Add Monitor", "upstream_server_error: upstream failed", "Fallback reason: openai / Primary Account - invoke_error - rate_limit: rate limited", "Recovered via: openai / Backup Account"} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin status page missing %q: %s", want, body)
		}
	}
}

func TestStatusPageHistoryTooltipsDoNotExposeAccountDetails(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	seedStatusPageMonitorData(t, repo)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, hidden := range []string{"Primary Account", "Backup Account", "rate_limit: rate limited", "Fallback reason", "Recovered via"} {
		if strings.Contains(body, hidden) {
			t.Fatalf("public status page leaked tooltip detail %q: %s", hidden, body)
		}
	}
}

func TestStatusPageRendersPartialForAutoRefresh(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	seedStatusPageMonitorData(t, repo)

	req := httptest.NewRequest(http.MethodGet, "/status?partial=status-page", nil)
	req.Header.Set(ajaxPartialHeader, "status-page")
	req.Header.Set("X-Requested-With", "fetch")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("partial status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`data-partial="status-page"`, `data-status-auto-refresh`, `Public Default`, `Default / gpt-5`, `data-monitor-countdown`, `data-next-check-at=`, `monitor-availability-value monitor-availability-good`} {
		if !strings.Contains(body, want) {
			t.Fatalf("status partial missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "<!DOCTYPE html>") || strings.Contains(body, `<script type="module"`) {
		t.Fatalf("status partial should not include full layout: %s", body)
	}
	if strings.Contains(body, `<p class="panel-note">Default / gpt-5</p>`) {
		t.Fatalf("status partial should render group/model as inline subtitle: %s", body)
	}
}

func TestAdminStatusPageRendersPartialsForAutoRefresh(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	seedStatusPageMonitorData(t, repo)
	if !control.BeginMonitorRun("mon_public") {
		t.Fatal("BeginMonitorRun returned false")
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/status?partial=admin-status-targets", nil)
	req = req.WithContext(withConsoleUser(req.Context(), core.User{ID: "admin", Username: "admin", Role: core.UserRoleAdmin, Enabled: true}))
	req.Header.Set(ajaxPartialHeader, "admin-status-targets")
	req.Header.Set("X-Requested-With", "fetch")
	rec := httptest.NewRecorder()
	server.handleAdminStatusPage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("targets partial status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`data-partial="admin-status-targets"`, `data-status-auto-refresh`, `Public Default`, `Default / gpt-5`, `Internal Account Label`, `Next Check`, `data-monitor-countdown`, `monitor-run-indicator`, `monitor-run-spinner`, `monitor-availability-value monitor-availability-good`, `monitor-availability-value monitor-availability-bad`} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin targets partial missing %q: %s", want, body)
		}
	}
	if got := strings.Count(body, `monitor-run-button`); got != 2 {
		t.Fatalf("admin targets partial monitor run buttons = %d, want 2 for non-running targets: %s", got, body)
	}
	for _, unwanted := range []string{"<!DOCTYPE html>", `class="monitor-create-form"`, `<script type="module"`, `<details class="monitor-details">`, `<summary>`} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("admin targets partial should not include %q: %s", unwanted, body)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/status?partial=admin-status-summary", nil)
	req = req.WithContext(withConsoleUser(req.Context(), core.User{ID: "admin", Username: "admin", Role: core.UserRoleAdmin, Enabled: true}))
	req.Header.Set(ajaxPartialHeader, "admin-status-summary")
	req.Header.Set("X-Requested-With", "fetch")
	rec = httptest.NewRecorder()
	server.handleAdminStatusPage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("summary partial status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = rec.Body.String()
	if !strings.Contains(body, `data-partial="admin-status-summary"`) || !strings.Contains(body, `data-status-auto-refresh`) {
		t.Fatalf("admin summary partial missing refresh markers: %s", body)
	}
	if strings.Contains(body, `class="monitor-create-form"`) || strings.Contains(body, "<!DOCTYPE html>") {
		t.Fatalf("admin summary partial should not include form or full layout: %s", body)
	}
}

func TestRunStatusProbeWithoutControlDoesNotPanic(t *testing.T) {
	var server *Server
	result, err := server.runStatusProbe(context.Background(), core.MonitorTarget{
		ID:    "mon_missing_control",
		Model: "gpt-5",
	})
	if err == nil {
		t.Fatal("runStatusProbe returned nil error, want control plane unavailable")
	}
	if result.ErrorCode != "controlplane_unavailable" || result.Status != core.MonitorStatusFailed {
		t.Fatalf("result = %#v, want controlplane_unavailable failed result", result)
	}
}

type cancelStatusAdapter struct{}

func (a *cancelStatusAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *cancelStatusAdapter) DisplayName() string { return "Cancel Status" }

func (a *cancelStatusAdapter) ListModels(context.Context) []core.ModelSpec {
	return []core.ModelSpec{{Name: "gpt-5", Provider: core.ProviderOpenAI}}
}

func (a *cancelStatusAdapter) Invoke(ctx context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return &core.GatewayResponse{
			ID:           "resp_status_cancel",
			Model:        decision.Model,
			Provider:     decision.Provider,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			Content:      "pong",
			FinishReason: "stop",
			CreatedAt:    time.Now().UTC(),
			Usage:        core.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		}, nil
	}
}

func TestRunStatusProbeSkipsCanceledResults(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&cancelStatusAdapter{})
	control := controlplane.New(repo, registry)
	if err := repo.UpsertModel(core.ModelConfig{
		ID:            "gpt-5",
		Provider:      core.ProviderOpenAI,
		UpstreamID:    "gpt-5",
		Enabled:       true,
		VisibleGroups: []string{core.DefaultAccountGroupName},
		Source:        core.ModelSourceManual,
	}); err != nil {
		t.Fatalf("UpsertModel returned error: %v", err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_cancel",
		Provider: core.ProviderOpenAI,
		Label:    "Cancel Account",
		Group:    core.DefaultAccountGroupName,
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "token",
		},
	}); err != nil {
		t.Fatalf("UpsertAccount returned error: %v", err)
	}
	server := NewServer(control, gateway.New(repo, routing.NewRouter(), failover.NewEngine(accounts.NewPool(repo), registry)), "data/state.db")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := server.runStatusProbe(ctx, core.MonitorTarget{
		ID:              "mon_cancel",
		AccountGroup:    core.DefaultAccountGroupName,
		Model:           "gpt-5",
		TimeoutSeconds:  30,
		IntervalSeconds: 300,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runStatusProbe error = %v, want context.Canceled", err)
	}
	if history := repo.ListMonitorResults("mon_cancel", 10); len(history) != 0 {
		t.Fatalf("history after canceled probe = %#v, want empty", history)
	}
}

func seedStatusPageMonitorData(t *testing.T, repo storage.Repository) {
	t.Helper()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	for _, model := range []core.ModelConfig{
		{ID: "gpt-5", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{core.DefaultAccountGroupName}},
		{ID: "plus-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{"Plus"}},
	} {
		if err := repo.UpsertModel(model); err != nil {
			t.Fatal(err)
		}
	}
	targets := []core.MonitorTarget{
		{
			ID:              "mon_public",
			Name:            "Public Default",
			AccountGroup:    core.DefaultAccountGroupName,
			Model:           "gpt-5",
			Enabled:         true,
			PublicVisible:   true,
			IntervalSeconds: 300,
			TimeoutSeconds:  30,
		},
		{
			ID:              "mon_private",
			Name:            "Private Plus",
			AccountGroup:    "Plus",
			Model:           "plus-model",
			Enabled:         true,
			PublicVisible:   false,
			IntervalSeconds: 300,
			TimeoutSeconds:  30,
		},
		{
			ID:              "mon_disabled_public",
			Name:            "Disabled Public",
			AccountGroup:    core.DefaultAccountGroupName,
			Model:           "gpt-5",
			Enabled:         false,
			PublicVisible:   true,
			IntervalSeconds: 300,
			TimeoutSeconds:  30,
		},
	}
	for _, target := range targets {
		if err := repo.UpsertMonitorTarget(target); err != nil {
			t.Fatal(err)
		}
	}
	checkedAt := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Second)
	for _, result := range []core.MonitorResult{
		{
			ID:        "res_public",
			TargetID:  "mon_public",
			Status:    core.MonitorStatusDegraded,
			LatencyMS: 42,
			Provider:  core.ProviderOpenAI,
			AccountID: "acct_public",
			Attempts: []core.AttemptRecord{
				{
					Provider:     core.ProviderOpenAI,
					AccountID:    "acct_primary",
					AccountLabel: "Primary Account",
					Status:       "invoke_error",
					ErrorCode:    "rate_limit",
					ErrorMessage: "rate limited",
				},
				{
					Provider:     core.ProviderOpenAI,
					AccountID:    "acct_public",
					AccountLabel: "Backup Account",
					Status:       "ok",
				},
			},
			CheckedAt: checkedAt,
		},
		{
			ID:           "res_private",
			TargetID:     "mon_private",
			Status:       core.MonitorStatusFailed,
			LatencyMS:    88,
			Provider:     core.ProviderOpenAI,
			AccountID:    "acct_internal",
			AccountLabel: "Internal Account Label",
			Attempts: []core.AttemptRecord{{
				Provider:     core.ProviderOpenAI,
				AccountID:    "acct_internal",
				AccountLabel: "Internal Account Label",
				Status:       "invoke_error",
				ErrorCode:    "upstream_server_error",
				ErrorMessage: "upstream failed",
			}},
			ErrorMessage: "upstream failed",
			CheckedAt:    checkedAt.Add(time.Minute),
		},
	} {
		if err := repo.AppendMonitorResult(result); err != nil {
			t.Fatal(err)
		}
	}
}
