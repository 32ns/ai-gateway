package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/accounts"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/failover"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/routing"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestRoutePolicyForClientAccountGroupIsDynamic(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_openai",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI",
		Group:    "Plus",
		Status:   core.AccountStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, routing.NewRouter(), nil)
	client := &core.APIClient{
		ID:           "client_plus",
		AccountGroup: "Plus",
		RoutePolicy:  core.RoutePolicy{DefaultProvider: core.ProviderOpenAI},
	}

	policy := service.routePolicyForClient(client)
	if policy.DefaultProvider != core.ProviderOpenAI || len(policy.FallbackProviders) != 0 {
		t.Fatalf("initial policy = %#v", policy)
	}

	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_claude",
		Provider: core.ProviderClaude,
		Label:    "Claude",
		Group:    "Plus",
		Status:   core.AccountStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	policy = service.routePolicyForClient(client)
	providers := append([]core.ProviderKind{policy.DefaultProvider}, policy.FallbackProviders...)
	if len(providers) != 2 || !containsRouteProvider(providers, core.ProviderOpenAI) || !containsRouteProvider(providers, core.ProviderClaude) {
		t.Fatalf("dynamic policy providers = %#v", providers)
	}
}

func TestRoutePolicyForDefaultClientUsesOnlyDefaultGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	hide := false
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_visible", Name: "Visible"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_hidden", Name: "Hidden", ShowInClientEditor: &hide}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_default", Provider: core.ProviderClaude, Group: core.DefaultAccountGroupName, Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_visible", Provider: core.ProviderOpenAI, Group: "Visible", Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_hidden", Provider: core.ProviderOpenAI, Group: "Hidden", Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, routing.NewRouter(), nil)

	policy := service.routePolicyForClient(&core.APIClient{ID: "client_default", RoutePolicy: core.RoutePolicy{DefaultProvider: core.ProviderOpenAI}})
	providers := append([]core.ProviderKind{policy.DefaultProvider}, policy.FallbackProviders...)
	if len(providers) != 1 || !containsRouteProvider(providers, core.ProviderClaude) {
		t.Fatalf("default client policy = %#v, want Default group provider only", policy)
	}
}

func TestRoutePolicyForModelProviderPreservesFallbackProviders(t *testing.T) {
	policy := routePolicyForModelProvider(core.RoutePolicy{
		DefaultProvider:   core.ProviderClaude,
		FallbackProviders: []core.ProviderKind{core.ProviderOpenAI},
	}, core.ProviderOpenAI)

	if policy.DefaultProvider != core.ProviderOpenAI {
		t.Fatalf("default provider = %q, want %q", policy.DefaultProvider, core.ProviderOpenAI)
	}
	if len(policy.FallbackProviders) != 1 || policy.FallbackProviders[0] != core.ProviderClaude {
		t.Fatalf("fallback providers = %#v, want Claude", policy.FallbackProviders)
	}
}

type echoAdapter struct{}

func (a *echoAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *echoAdapter) DisplayName() string { return "Echo" }

func (a *echoAdapter) ListModels(context.Context) []core.ModelSpec {
	return []core.ModelSpec{{Name: "gpt-4.1", Provider: core.ProviderOpenAI}}
}

func seedGatewayModel(t *testing.T, repo storage.Repository, id string, provider core.ProviderKind) {
	t.Helper()
	if err := repo.UpsertModel(core.ModelConfig{
		ID:         id,
		Provider:   provider,
		UpstreamID: id,
		Enabled:    true,
		VisibleGroups: []string{
			core.DefaultAccountGroupName,
		},
		Source: core.ModelSourceManual,
	}); err != nil {
		t.Fatalf("UpsertModel returned error: %v", err)
	}
}

func TestExecuteRejectsModelOutsideClientVisibleGroups(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_openai",
		Provider: core.ProviderOpenAI,
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			AccessToken: "openai-token",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:            "plus-model",
		Provider:      core.ProviderOpenAI,
		Enabled:       true,
		VisibleGroups: []string{"Plus"},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&echoAdapter{})),
	)

	_, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:    "plus-model",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Client:   &core.APIClient{ID: "client_basic", Enabled: true, AccountGroup: "Basic"},
	})
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("err = %T %v, want ErrModelUnavailable", err, err)
	}
}

func TestExecuteRejectsHiddenAccountGroupForNonVisibleOwner(t *testing.T) {
	repo := storage.NewMemoryRepository()
	hide := false
	if err := repo.UpsertUser(core.User{ID: "user_allowed", Username: "allowed", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertUser(core.User{ID: "user_denied", Username: "denied", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_hidden", Name: "Hidden", ShowInClientEditor: &hide, VisibleUserIDs: []string{"user_allowed"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_hidden", Provider: core.ProviderOpenAI, Group: "Hidden", Status: core.AccountStatusActive, Credential: core.Credential{AccessToken: "hidden-token"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{ID: "hidden-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{"Hidden"}}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_denied_hidden", Name: "Denied Hidden", APIKey: "gw_denied_hidden", OwnerUserID: "user_denied", Enabled: true, AccountGroup: "Hidden"}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&echoAdapter{})),
	)

	_, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:    "hidden-model",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Client:   &client,
	})
	var accessErr *AccessError
	if !errors.As(err, &accessErr) || accessErr.StatusCode != http.StatusForbidden || accessErr.Code != ErrorCodeAccountGroupForbidden {
		t.Fatalf("err = %#v, want account group forbidden access error", err)
	}
}

func TestExecuteAllowsHiddenAccountGroupForVisibleOwner(t *testing.T) {
	repo := storage.NewMemoryRepository()
	hide := false
	if err := repo.UpsertUser(core.User{ID: "user_allowed", Username: "allowed", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_hidden", Name: "Hidden", ShowInClientEditor: &hide, VisibleUserIDs: []string{"user_allowed"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_hidden", Provider: core.ProviderOpenAI, Group: "Hidden", Status: core.AccountStatusActive, Credential: core.Credential{AccessToken: "hidden-token"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{ID: "hidden-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{"Hidden"}}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_allowed_hidden", Name: "Allowed Hidden", APIKey: "gw_allowed_hidden", OwnerUserID: "user_allowed", Enabled: true, AccountGroup: "Hidden"}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&echoAdapter{})),
	)

	resp, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:    "hidden-model",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Client:   &client,
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if resp == nil || resp.AccountID != "acct_hidden" {
		t.Fatalf("response account = %#v, want acct_hidden", resp)
	}
}

func TestExecuteDefaultClientDoesNotUseOtherGroups(t *testing.T) {
	repo := storage.NewMemoryRepository()
	hide := false
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_hidden", Name: "Hidden", ShowInClientEditor: &hide}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:         "acct_hidden",
		Provider:   core.ProviderOpenAI,
		Label:      "Hidden",
		Group:      "Hidden",
		Status:     core.AccountStatusActive,
		Priority:   200,
		Weight:     100,
		Credential: core.Credential{AccessToken: "hidden-token"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:         "acct_plus",
		Provider:   core.ProviderOpenAI,
		Label:      "Plus",
		Group:      "Plus",
		Status:     core.AccountStatusActive,
		Priority:   100,
		Weight:     100,
		Credential: core.Credential{AccessToken: "plus-token"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:            "plus-model",
		Provider:      core.ProviderOpenAI,
		Enabled:       true,
		VisibleGroups: []string{"Plus"},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&echoAdapter{})),
	)

	resp, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:    "plus-model",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Client:   &core.APIClient{ID: "client_default", Enabled: true},
	})
	if resp != nil && resp.AccountID != "" {
		t.Fatalf("account = %q, want no routed account", resp.AccountID)
	}
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("err = %T %v, want ErrModelUnavailable", err, err)
	}
}

func (a *echoAdapter) Invoke(_ context.Context, decision core.RouteDecision, req *core.GatewayRequest) (*core.GatewayResponse, error) {
	return &core.GatewayResponse{
		ID:           "resp_test",
		Model:        decision.Model,
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Content:      req.Metadata["order"],
		FinishReason: "stop",
		CreatedAt:    time.Now().UTC(),
		Usage:        core.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
	}, nil
}

type monitorProbeAdapter struct {
	seen []string
}

func (a *monitorProbeAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *monitorProbeAdapter) DisplayName() string { return "Monitor Probe" }

func (a *monitorProbeAdapter) ListModels(context.Context) []core.ModelSpec {
	return []core.ModelSpec{{Name: "gpt-5", Provider: core.ProviderOpenAI}}
}

func (a *monitorProbeAdapter) Invoke(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
	a.seen = append(a.seen, decision.Account.ID)
	if decision.Account.ID == "acct_plus_primary" {
		return nil, &providers.InvokeError{
			Code:      "upstream_temporarily_unavailable",
			Temporary: true,
			Cooldown:  30 * time.Second,
			Err:       errors.New("primary probe failed"),
		}
	}
	return &core.GatewayResponse{
		ID:           "resp_monitor_probe",
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

func TestProbeMonitorTargetUsesRoutedGroupAndStopsAfterSuccess(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertUser(core.User{ID: "user_monitor", Username: "monitor", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:            "gpt-5",
		Provider:      core.ProviderOpenAI,
		UpstreamID:    "gpt-5",
		Enabled:       true,
		VisibleGroups: []string{"Plus"},
		Source:        core.ModelSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{
			ID:         "acct_default_high",
			Provider:   core.ProviderOpenAI,
			Label:      "Default High",
			Group:      core.DefaultAccountGroupName,
			Status:     core.AccountStatusActive,
			Priority:   500,
			Weight:     100,
			Credential: core.Credential{Mode: "manual-token", AccessToken: "default-token"},
		},
		{
			ID:         "acct_plus_primary",
			Provider:   core.ProviderOpenAI,
			Label:      "Plus Primary",
			Group:      "Plus",
			Status:     core.AccountStatusActive,
			Priority:   300,
			Weight:     100,
			Credential: core.Credential{Mode: "manual-token", AccessToken: "primary-token"},
		},
		{
			ID:         "acct_plus_secondary",
			Provider:   core.ProviderOpenAI,
			Label:      "Plus Secondary",
			Group:      "Plus",
			Status:     core.AccountStatusActive,
			Priority:   200,
			Weight:     100,
			Credential: core.Credential{Mode: "manual-token", AccessToken: "secondary-token"},
		},
		{
			ID:         "acct_plus_unused",
			Provider:   core.ProviderOpenAI,
			Label:      "Plus Unused",
			Group:      "Plus",
			Status:     core.AccountStatusActive,
			Priority:   100,
			Weight:     100,
			Credential: core.Credential{Mode: "manual-token", AccessToken: "unused-token"},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	adapter := &monitorProbeAdapter{}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter)),
	)

	result, err := service.ProbeMonitorTarget(context.Background(), MonitorProbeInput{
		TargetID:     "target_plus_gpt5",
		AccountGroup: "Plus",
		Model:        "gpt-5",
		Timeout:      time.Second,
	})
	if err != nil {
		t.Fatalf("ProbeMonitorTarget returned error: %v", err)
	}
	if result.Status != core.MonitorStatusOK {
		t.Fatalf("status = %q, want %q", result.Status, core.MonitorStatusOK)
	}
	if result.AccountID != "acct_plus_secondary" || result.AccountLabel != "Plus Secondary" {
		t.Fatalf("successful account = %q/%q, want acct_plus_secondary/Plus Secondary", result.AccountID, result.AccountLabel)
	}
	if got, want := adapter.seen, []string{"acct_plus_primary", "acct_plus_secondary"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("adapter accounts = %#v, want %#v", got, want)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %#v, want failed primary and successful secondary account", result.Attempts)
	}
	if result.Attempts[0].AccountID != "acct_plus_primary" || result.Attempts[0].Status != "invoke_error" {
		t.Fatalf("first attempt = %#v, want failed Plus primary", result.Attempts[0])
	}
	if result.Attempts[1].AccountID != "acct_plus_secondary" || result.Attempts[1].Status != "ok" {
		t.Fatalf("second attempt = %#v, want successful Plus secondary account", result.Attempts[1])
	}
	if audits := repo.ListAudit(10); len(audits) != 0 {
		t.Fatalf("probe should not write gateway audit events, got %#v", audits)
	}
	if requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{Limit: 10}); total != 0 || len(requests) != 0 {
		t.Fatalf("probe should not create billing requests, total=%d requests=%#v", total, requests)
	}
}

type timeoutThenSuccessMonitorProbeAdapter struct {
	monitorProbeAdapter
	alwaysTimeout bool
}

func (a *timeoutThenSuccessMonitorProbeAdapter) Invoke(ctx context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
	a.seen = append(a.seen, decision.Account.ID)
	if a.alwaysTimeout || decision.Account.ID == "acct_plus_primary" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &core.GatewayResponse{
		ID:           "resp_monitor_probe_timeout_recovered",
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

func seedMonitorTimeoutProbeService(t *testing.T, adapter providers.Adapter) *Service {
	t.Helper()
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:            "gpt-5",
		Provider:      core.ProviderOpenAI,
		UpstreamID:    "gpt-5",
		Enabled:       true,
		VisibleGroups: []string{"Plus"},
		Source:        core.ModelSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{
			ID:         "acct_plus_primary",
			Provider:   core.ProviderOpenAI,
			Label:      "Plus Primary",
			Group:      "Plus",
			Status:     core.AccountStatusActive,
			Priority:   300,
			Weight:     100,
			Credential: core.Credential{Mode: "manual-token", AccessToken: "primary-token"},
		},
		{
			ID:         "acct_plus_backup",
			Provider:   core.ProviderOpenAI,
			Label:      "Plus Backup",
			Group:      "Plus",
			Status:     core.AccountStatusActive,
			Backup:     true,
			Priority:   200,
			Weight:     100,
			Credential: core.Credential{Mode: "manual-token", AccessToken: "backup-token"},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	return New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter)),
	)
}

func TestProbeMonitorTargetTimeoutAppliesPerAccount(t *testing.T) {
	adapter := &timeoutThenSuccessMonitorProbeAdapter{}
	service := seedMonitorTimeoutProbeService(t, adapter)

	result, err := service.ProbeMonitorTarget(context.Background(), MonitorProbeInput{
		TargetID:     "target_plus_gpt5",
		AccountGroup: "Plus",
		Model:        "gpt-5",
		Timeout:      10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("ProbeMonitorTarget returned error: %v", err)
	}
	if result.Status != core.MonitorStatusDegraded {
		t.Fatalf("status = %q, want %q", result.Status, core.MonitorStatusDegraded)
	}
	if result.AccountID != "acct_plus_backup" {
		t.Fatalf("successful account = %q, want acct_plus_backup", result.AccountID)
	}
	if got, want := adapter.seen, []string{"acct_plus_primary", "acct_plus_backup"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("adapter accounts = %#v, want %#v", got, want)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %#v, want timed-out primary and successful backup", result.Attempts)
	}
	if result.Attempts[0].AccountID != "acct_plus_primary" || result.Attempts[0].ErrorCode != providers.ErrorCodeUpstreamTransportError {
		t.Fatalf("first attempt = %#v, want primary transport timeout", result.Attempts[0])
	}
	if result.Attempts[1].AccountID != "acct_plus_backup" || result.Attempts[1].Status != "ok" {
		t.Fatalf("second attempt = %#v, want backup ok", result.Attempts[1])
	}
}

func TestProbeMonitorTargetFailsAfterAllAccountTimeouts(t *testing.T) {
	adapter := &timeoutThenSuccessMonitorProbeAdapter{alwaysTimeout: true}
	service := seedMonitorTimeoutProbeService(t, adapter)

	result, err := service.ProbeMonitorTarget(context.Background(), MonitorProbeInput{
		TargetID:     "target_plus_gpt5",
		AccountGroup: "Plus",
		Model:        "gpt-5",
		Timeout:      10 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("ProbeMonitorTarget returned nil error, want all attempts failed")
	}
	if result.Status != core.MonitorStatusFailed {
		t.Fatalf("status = %q, want %q", result.Status, core.MonitorStatusFailed)
	}
	if got, want := adapter.seen, []string{"acct_plus_primary", "acct_plus_backup"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("adapter accounts = %#v, want %#v", got, want)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %#v, want two timed-out attempts", result.Attempts)
	}
	for i, attempt := range result.Attempts {
		if attempt.ErrorCode != providers.ErrorCodeUpstreamTransportError {
			t.Fatalf("attempt[%d] = %#v, want transport timeout", i, attempt)
		}
	}
}

func TestProbeMonitorTargetRejectsModelOutsideGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:            "gpt-5",
		Provider:      core.ProviderOpenAI,
		UpstreamID:    "gpt-5",
		Enabled:       true,
		VisibleGroups: []string{core.DefaultAccountGroupName},
		Source:        core.ModelSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:         "acct_plus",
		Provider:   core.ProviderOpenAI,
		Label:      "Plus",
		Group:      "Plus",
		Status:     core.AccountStatusActive,
		Priority:   100,
		Weight:     100,
		Credential: core.Credential{Mode: "manual-token", AccessToken: "plus-token"},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&monitorProbeAdapter{})),
	)

	result, err := service.ProbeMonitorTarget(context.Background(), MonitorProbeInput{
		TargetID:     "target_plus_gpt5",
		AccountGroup: "Plus",
		Model:        "gpt-5",
	})
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("err = %T %v, want ErrModelUnavailable", err, err)
	}
	if result.Status != core.MonitorStatusFailed || result.ErrorCode != "model_unavailable" {
		t.Fatalf("result = %#v, want failed model_unavailable", result)
	}
}

func TestProbeMonitorTargetRejectsNonTextModelBeforeAccountInvoke(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertModel(core.ModelConfig{
		ID:            "gpt-image-2",
		Provider:      core.ProviderOpenAI,
		Type:          core.ModelTypeImage,
		UpstreamID:    "gpt-image-2",
		Enabled:       true,
		VisibleGroups: []string{core.DefaultAccountGroupName},
		Source:        core.ModelSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:         "acct_default",
		Provider:   core.ProviderOpenAI,
		Label:      "Default",
		Group:      core.DefaultAccountGroupName,
		Status:     core.AccountStatusActive,
		Priority:   100,
		Weight:     100,
		Credential: core.Credential{Mode: "manual-token", AccessToken: "default-token"},
	}); err != nil {
		t.Fatal(err)
	}
	adapter := &monitorProbeAdapter{}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter)),
	)

	result, err := service.ProbeMonitorTarget(context.Background(), MonitorProbeInput{
		TargetID:     "target_image",
		AccountGroup: core.DefaultAccountGroupName,
		Model:        "gpt-image-2",
	})
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("err = %T %v, want ErrModelUnavailable", err, err)
	}
	if result.Status != core.MonitorStatusFailed || result.ErrorCode != "model_not_text" {
		t.Fatalf("result = %#v, want failed model_not_text", result)
	}
	if len(adapter.seen) != 0 {
		t.Fatalf("adapter saw accounts = %#v, want no upstream invoke", adapter.seen)
	}
}

type emptyMonitorProbeAdapter struct {
	monitorProbeAdapter
}

func (a *emptyMonitorProbeAdapter) Invoke(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
	a.seen = append(a.seen, decision.Account.ID)
	if decision.Account.ID == "acct_default_backup" {
		return &core.GatewayResponse{
			ID:           "resp_monitor_recovered",
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
	return &core.GatewayResponse{
		ID:           "resp_monitor_empty",
		Model:        decision.Model,
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		FinishReason: "stop",
		CreatedAt:    time.Now().UTC(),
		Usage:        core.Usage{PromptTokens: 1, CompletionTokens: 0, TotalTokens: 1},
	}, nil
}

type allEmptyMonitorProbeAdapter struct {
	monitorProbeAdapter
}

func (a *allEmptyMonitorProbeAdapter) Invoke(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
	a.seen = append(a.seen, decision.Account.ID)
	return &core.GatewayResponse{
		ID:           "resp_monitor_empty",
		Model:        decision.Model,
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		FinishReason: "stop",
		CreatedAt:    time.Now().UTC(),
		Usage:        core.Usage{PromptTokens: 1, CompletionTokens: 0, TotalTokens: 1},
	}, nil
}

func TestProbeMonitorTargetFallsBackAfterEmptyModelReply(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertModel(core.ModelConfig{
		ID:            "gpt-5",
		Provider:      core.ProviderOpenAI,
		UpstreamID:    "gpt-5",
		Enabled:       true,
		VisibleGroups: []string{core.DefaultAccountGroupName},
		Source:        core.ModelSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{
			ID:         "acct_default_empty",
			Provider:   core.ProviderOpenAI,
			Label:      "Default Empty",
			Group:      core.DefaultAccountGroupName,
			Status:     core.AccountStatusActive,
			Priority:   300,
			Weight:     100,
			Credential: core.Credential{Mode: "manual-token", AccessToken: "empty-token"},
		},
		{
			ID:         "acct_default_backup",
			Provider:   core.ProviderOpenAI,
			Label:      "Default Backup",
			Group:      core.DefaultAccountGroupName,
			Status:     core.AccountStatusActive,
			Backup:     true,
			Priority:   200,
			Weight:     100,
			Credential: core.Credential{Mode: "manual-token", AccessToken: "backup-token"},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	adapter := &emptyMonitorProbeAdapter{}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter)),
	)

	result, err := service.ProbeMonitorTarget(context.Background(), MonitorProbeInput{
		TargetID:     "target_empty",
		AccountGroup: core.DefaultAccountGroupName,
		Model:        "gpt-5",
	})
	if err != nil {
		t.Fatalf("ProbeMonitorTarget returned error: %v", err)
	}
	if result.Status != core.MonitorStatusDegraded {
		t.Fatalf("status = %q, want %q", result.Status, core.MonitorStatusDegraded)
	}
	if result.AccountID != "acct_default_backup" || result.AccountLabel != "Default Backup" {
		t.Fatalf("successful account = %q/%q, want acct_default_backup/Default Backup", result.AccountID, result.AccountLabel)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %#v, want empty primary and successful backup", result.Attempts)
	}
	if result.Attempts[0].AccountID != "acct_default_empty" || result.Attempts[0].ErrorCode != providers.ErrorCodeUpstreamEmptyResponse {
		t.Fatalf("first attempt = %#v, want empty primary failure", result.Attempts[0])
	}
	if result.Attempts[1].AccountID != "acct_default_backup" || result.Attempts[1].Status != "ok" {
		t.Fatalf("second attempt = %#v, want backup ok", result.Attempts[1])
	}
	if got, want := adapter.seen, []string{"acct_default_empty", "acct_default_backup"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("adapter accounts = %#v, want %#v", got, want)
	}
}

func TestProbeMonitorTargetReportsEmptyResponseAfterAllEmptyReplies(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertModel(core.ModelConfig{
		ID:            "gpt-5",
		Provider:      core.ProviderOpenAI,
		UpstreamID:    "gpt-5",
		Enabled:       true,
		VisibleGroups: []string{core.DefaultAccountGroupName},
		Source:        core.ModelSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{
			ID:         "acct_default_empty_a",
			Provider:   core.ProviderOpenAI,
			Label:      "Default Empty A",
			Group:      core.DefaultAccountGroupName,
			Status:     core.AccountStatusActive,
			Priority:   300,
			Weight:     100,
			Credential: core.Credential{Mode: "manual-token", AccessToken: "empty-a-token"},
		},
		{
			ID:         "acct_default_empty_b",
			Provider:   core.ProviderOpenAI,
			Label:      "Default Empty B",
			Group:      core.DefaultAccountGroupName,
			Status:     core.AccountStatusActive,
			Priority:   200,
			Weight:     100,
			Credential: core.Credential{Mode: "manual-token", AccessToken: "empty-b-token"},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	adapter := &allEmptyMonitorProbeAdapter{}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter)),
	)

	result, err := service.ProbeMonitorTarget(context.Background(), MonitorProbeInput{
		TargetID:     "target_empty_all",
		AccountGroup: core.DefaultAccountGroupName,
		Model:        "gpt-5",
	})
	if err == nil {
		t.Fatal("ProbeMonitorTarget returned nil error, want empty response error")
	}
	if result.Status != core.MonitorStatusFailed || result.ErrorCode != "empty_response" {
		t.Fatalf("result = %#v, want failed empty_response", result)
	}
	if result.ErrorMessage != "upstream returned an empty response" {
		t.Fatalf("error message = %q", result.ErrorMessage)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %#v, want two empty-response attempts", result.Attempts)
	}
	for i, attempt := range result.Attempts {
		if attempt.ErrorCode != providers.ErrorCodeUpstreamEmptyResponse {
			t.Fatalf("attempt[%d] = %#v, want upstream_empty_response", i, attempt)
		}
	}
	if got, want := adapter.seen, []string{"acct_default_empty_a", "acct_default_empty_b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("adapter accounts = %#v, want %#v", got, want)
	}
}

type billingEchoAdapter struct {
	echoAdapter
	usage       core.Usage
	serviceTier string
}

func (a *billingEchoAdapter) Invoke(_ context.Context, decision core.RouteDecision, req *core.GatewayRequest) (*core.GatewayResponse, error) {
	return &core.GatewayResponse{
		ID:           "resp_billing_test",
		Model:        decision.Model,
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Content:      req.Metadata["order"],
		FinishReason: "stop",
		CreatedAt:    time.Now().UTC(),
		Usage:        a.usage,
		ServiceTier:  a.serviceTier,
	}, nil
}

type quotaEchoAdapter struct {
	echoAdapter
	fetched chan string
}

func (a *quotaEchoAdapter) FetchQuota(_ context.Context, account core.Account) (core.AccountQuotaSnapshot, error) {
	if a.fetched != nil {
		a.fetched <- account.ID
	}
	balance := 18.5
	refreshedAt := time.Now().UTC()
	return core.AccountQuotaSnapshot{
		Source:      "test_quota",
		Plan:        "plus",
		Credits:     &core.AccountQuotaCredits{HasCredits: true, Balance: &balance},
		RefreshedAt: &refreshedAt,
	}, nil
}

type quotaAuthFailAdapter struct {
	echoAdapter
}

func (a *quotaAuthFailAdapter) FetchQuota(context.Context, core.Account) (core.AccountQuotaSnapshot, error) {
	return core.AccountQuotaSnapshot{}, &providers.InvokeError{
		Code:      "credential_expired",
		Temporary: false,
		Err:       errors.New("refresh token was already used"),
	}
}

type quotaRefreshAuthFailAdapter struct {
	echoAdapter
}

func (a *quotaRefreshAuthFailAdapter) Refresh(context.Context, core.Account) (core.Credential, error) {
	return core.Credential{}, &providers.InvokeError{
		Code:      "credential_expired",
		Temporary: false,
		Err:       errors.New("refresh token was already used"),
	}
}

func (a *quotaRefreshAuthFailAdapter) FetchQuota(context.Context, core.Account) (core.AccountQuotaSnapshot, error) {
	return core.AccountQuotaSnapshot{}, errors.New("FetchQuota should not be called after refresh failure")
}

type boundResponsesFallbackAdapter struct {
	echoAdapter
	seenRawByAccount map[string]string
	failCode         string
}

func (a *boundResponsesFallbackAdapter) InvokeResponses(_ context.Context, decision core.RouteDecision, req *core.ResponsesRequest) (*core.GatewayResponse, error) {
	if a.seenRawByAccount == nil {
		a.seenRawByAccount = make(map[string]string)
	}
	a.seenRawByAccount[decision.Account.ID] = string(req.RawBody)
	if decision.Account.ID == "acct_bound_bad" {
		code := strings.TrimSpace(a.failCode)
		if code == "" {
			code = "upstream_server_error"
		}
		return nil, &providers.InvokeError{
			Code:      code,
			Temporary: true,
			Cooldown:  30 * time.Second,
			Err:       errors.New("upstream returned HTML challenge response"),
		}
	}
	return &core.GatewayResponse{
		ID:           "resp_recovered",
		Model:        decision.Model,
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Content:      "recovered",
		FinishReason: "stop",
		CreatedAt:    time.Now().UTC(),
		Usage:        core.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}, nil
}

func (a *boundResponsesFallbackAdapter) OpenResponsesStream(_ context.Context, decision core.RouteDecision, req *core.ResponsesRequest) (*providers.StreamSession, error) {
	if a.seenRawByAccount == nil {
		a.seenRawByAccount = make(map[string]string)
	}
	a.seenRawByAccount[decision.Account.ID] = string(req.RawBody)
	if decision.Account.ID == "acct_bound_bad" {
		code := strings.TrimSpace(a.failCode)
		if code == "" {
			code = "upstream_server_error"
		}
		return nil, &providers.InvokeError{
			Code:      code,
			Temporary: true,
			Cooldown:  30 * time.Second,
			Err:       errors.New("upstream returned HTML challenge response"),
		}
	}
	return &providers.StreamSession{
		Response: &core.GatewayResponse{
			ID:           "resp_recovered_stream",
			Model:        decision.Model,
			Provider:     decision.Provider,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			CreatedAt:    time.Now().UTC(),
			Usage:        core.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		},
		Stream: &testStream{
			events: []*core.StreamEvent{
				{Delta: "recovered"},
				{FinishReason: "stop", Done: true, Usage: &core.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}},
			},
		},
	}, nil
}

func TestExecuteResponsesRetriesWithoutPreviousResponseWhenBoundAccountFails(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_bound", Username: "bound", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_bound", Name: "Bound", APIKey: "gw_bound", OwnerUserID: "user_bound", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{
			ID:       "acct_bound_bad",
			Provider: core.ProviderOpenAI,
			Label:    "Bound Bad",
			Status:   core.AccountStatusActive,
			Priority: 200,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "bad-token",
			},
		},
		{
			ID:       "acct_bound_good",
			Provider: core.ProviderOpenAI,
			Label:    "Bound Good",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "good-token",
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                     "responses-bound-model",
		Provider:               core.ProviderOpenAI,
		UpstreamID:             "responses-bound-model",
		Enabled:                true,
		VisibleGroups:          []string{core.DefaultAccountGroupName},
		Source:                 core.ModelSourceManual,
		InputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{
		ResponseID: "resp_previous",
		AccountID:  "acct_bound_bad",
		ClientID:   client.ID,
	}); err != nil {
		t.Fatal(err)
	}

	adapter := &boundResponsesFallbackAdapter{}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter)),
	)

	resp, err := service.ExecuteResponses(context.Background(), &core.ResponsesRequest{
		Model:              "responses-bound-model",
		RawBody:            json.RawMessage(`{"model":"responses-bound-model","input":"hello","previous_response_id":"resp_previous"}`),
		Client:             &client,
		PreviousResponseID: "resp_previous",
	})
	if err != nil {
		t.Fatalf("ExecuteResponses returned error: %v", err)
	}
	if resp.AccountID != "acct_bound_good" {
		t.Fatalf("account id = %q, want acct_bound_good", resp.AccountID)
	}
	if !strings.Contains(adapter.seenRawByAccount["acct_bound_bad"], "previous_response_id") {
		t.Fatalf("bound account raw body = %q, want previous_response_id", adapter.seenRawByAccount["acct_bound_bad"])
	}
	if strings.Contains(adapter.seenRawByAccount["acct_bound_good"], "previous_response_id") {
		t.Fatalf("fallback raw body = %q, want previous_response_id removed", adapter.seenRawByAccount["acct_bound_good"])
	}
	audits := repo.ListAudit(1)
	if len(audits) != 1 || audits[0].Status != "ok" {
		t.Fatalf("audit = %#v, want one ok audit", audits)
	}
	if len(audits[0].Attempts) != 2 {
		t.Fatalf("attempts = %#v, want failed bound attempt and ok fallback", audits[0].Attempts)
	}
	if audits[0].Attempts[0].AccountID != "acct_bound_bad" || audits[0].Attempts[0].ErrorCode != "upstream_server_error" {
		t.Fatalf("first attempt = %#v, want bound upstream_server_error", audits[0].Attempts[0])
	}
	if audits[0].Attempts[1].AccountID != "acct_bound_good" || audits[0].Attempts[1].Status != "ok" {
		t.Fatalf("second attempt = %#v, want fallback ok", audits[0].Attempts[1])
	}
}

func TestApplyResponsesAccountBindingRestoresPromptCacheKey(t *testing.T) {
	repo := storage.NewMemoryRepository()
	client := core.APIClient{ID: "client_cache", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{
		ResponseID:     "resp_cached",
		AccountID:      "acct_cached",
		ClientID:       client.ID,
		PromptCacheKey: "agpc_stored",
	}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, routing.NewRouter(), nil)

	req := &core.ResponsesRequest{
		RawBody:            json.RawMessage(`{"previous_response_id":"resp_cached"}`),
		Client:             &client,
		PreviousResponseID: "resp_cached",
	}
	if err := service.applyResponsesAccountBinding(req, &client); err != nil {
		t.Fatalf("applyResponsesAccountBinding returned error: %v", err)
	}
	if req.PromptCacheKey != "agpc_stored" {
		t.Fatalf("PromptCacheKey = %q, want stored key", req.PromptCacheKey)
	}
	if req.PreferredAccountID != "acct_cached" || !req.StrictAccountAffinity {
		t.Fatalf("account affinity = %q/%t", req.PreferredAccountID, req.StrictAccountAffinity)
	}

	explicitReq := &core.ResponsesRequest{
		RawBody:            json.RawMessage(`{"previous_response_id":"resp_cached","prompt_cache_key":"explicit"}`),
		Client:             &client,
		PreviousResponseID: "resp_cached",
		PromptCacheKey:     "explicit",
	}
	if err := service.applyResponsesAccountBinding(explicitReq, &client); err != nil {
		t.Fatalf("applyResponsesAccountBinding explicit returned error: %v", err)
	}
	if explicitReq.PromptCacheKey != "explicit" {
		t.Fatalf("explicit PromptCacheKey = %q, want explicit", explicitReq.PromptCacheKey)
	}
}

func TestExecuteResponsesPreviousFallbackExcludesBoundAccountWhenFailureDoesNotCoolImmediately(t *testing.T) {
	repo := storage.NewMemoryRepository()
	client := core.APIClient{ID: "client_bound_transport", Name: "Bound Transport", APIKey: "gw_bound_transport", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{
			ID:       "acct_bound_bad",
			Provider: core.ProviderOpenAI,
			Label:    "Bound Bad",
			Status:   core.AccountStatusActive,
			Priority: 200,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "bad-token",
			},
		},
		{
			ID:       "acct_bound_good",
			Provider: core.ProviderOpenAI,
			Label:    "Bound Good",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "good-token",
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:            "responses-bound-transport-model",
		Provider:      core.ProviderOpenAI,
		UpstreamID:    "responses-bound-transport-model",
		Enabled:       true,
		VisibleGroups: []string{core.DefaultAccountGroupName},
		Source:        core.ModelSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{
		ResponseID: "resp_previous_transport",
		AccountID:  "acct_bound_bad",
		ClientID:   client.ID,
	}); err != nil {
		t.Fatal(err)
	}

	adapter := &boundResponsesFallbackAdapter{failCode: "upstream_transport_error"}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter)),
	)

	resp, err := service.ExecuteResponses(context.Background(), &core.ResponsesRequest{
		Model:              "responses-bound-transport-model",
		RawBody:            json.RawMessage(`{"model":"responses-bound-transport-model","input":"hello","previous_response_id":"resp_previous_transport"}`),
		Client:             &client,
		PreviousResponseID: "resp_previous_transport",
	})
	if err != nil {
		t.Fatalf("ExecuteResponses returned error: %v", err)
	}
	if resp.AccountID != "acct_bound_good" {
		t.Fatalf("account id = %q, want acct_bound_good", resp.AccountID)
	}
	audits := repo.ListAudit(1)
	if len(audits) != 1 || len(audits[0].Attempts) != 2 {
		t.Fatalf("audit attempts = %#v, want two attempts", audits)
	}
	if audits[0].Attempts[0].AccountID != "acct_bound_bad" || audits[0].Attempts[0].ErrorCode != "upstream_transport_error" {
		t.Fatalf("first attempt = %#v, want bound transport error", audits[0].Attempts[0])
	}
	if audits[0].Attempts[1].AccountID != "acct_bound_good" || audits[0].Attempts[1].Status != "ok" {
		t.Fatalf("second attempt = %#v, want fallback good account", audits[0].Attempts[1])
	}
}

func TestExecuteResponsesStreamRetriesWithoutPreviousResponseWhenBoundAccountOpenFails(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_bound_stream", Username: "bound-stream", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_bound_stream", Name: "Bound Stream", APIKey: "gw_bound_stream", OwnerUserID: "user_bound_stream", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{
			ID:       "acct_bound_bad",
			Provider: core.ProviderOpenAI,
			Label:    "Bound Bad",
			Status:   core.AccountStatusActive,
			Priority: 200,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "bad-token",
			},
		},
		{
			ID:       "acct_bound_good",
			Provider: core.ProviderOpenAI,
			Label:    "Bound Good",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "good-token",
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                     "responses-bound-stream-model",
		Provider:               core.ProviderOpenAI,
		UpstreamID:             "responses-bound-stream-model",
		Enabled:                true,
		VisibleGroups:          []string{core.DefaultAccountGroupName},
		Source:                 core.ModelSourceManual,
		InputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{
		ResponseID: "resp_previous_stream",
		AccountID:  "acct_bound_bad",
		ClientID:   client.ID,
	}); err != nil {
		t.Fatal(err)
	}

	adapter := &boundResponsesFallbackAdapter{}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter)),
	)

	var content strings.Builder
	err := service.ExecuteResponsesStream(context.Background(), &core.ResponsesRequest{
		Model:              "responses-bound-stream-model",
		RawBody:            json.RawMessage(`{"model":"responses-bound-stream-model","input":"hello","previous_response_id":"resp_previous_stream"}`),
		Client:             &client,
		Stream:             true,
		PreviousResponseID: "resp_previous_stream",
	}, func(_ *core.GatewayResponse, event *core.StreamEvent) error {
		if event != nil {
			content.WriteString(event.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ExecuteResponsesStream returned error: %v", err)
	}
	if content.String() != "recovered" {
		t.Fatalf("content = %q, want recovered", content.String())
	}
	if strings.Contains(adapter.seenRawByAccount["acct_bound_good"], "previous_response_id") {
		t.Fatalf("fallback raw body = %q, want previous_response_id removed", adapter.seenRawByAccount["acct_bound_good"])
	}
	audits := repo.ListAudit(1)
	if len(audits) != 1 || audits[0].Status != "ok" {
		t.Fatalf("audit = %#v, want one ok audit", audits)
	}
	if len(audits[0].Attempts) != 2 || audits[0].Attempts[0].AccountID != "acct_bound_bad" || audits[0].Attempts[1].AccountID != "acct_bound_good" {
		t.Fatalf("attempts = %#v, want failed bound attempt and ok fallback", audits[0].Attempts)
	}
}

func TestShouldRetryResponsesWithoutPreviousResponseForBoundAccountErrors(t *testing.T) {
	req := &core.ResponsesRequest{
		PreviousResponseID:    "resp_previous",
		PreferredAccountID:    "acct_bound",
		StrictAccountAffinity: true,
	}
	retryCodes := []string{
		"missing_credential",
		"credential_expired",
		"missing_refresh_credential",
		"credential_refresh_not_supported",
		"upstream_auth_error",
		"upstream_provider_banned",
		"upstream_forbidden",
		"gateway_api_key_disabled",
		"upstream_rate_limited",
		"upstream_temporarily_unavailable",
		"upstream_transport_error",
		"upstream_read_error",
	}
	for _, code := range retryCodes {
		t.Run(code, func(t *testing.T) {
			err := &failover.ExecutionError{Attempts: []core.AttemptRecord{{
				AccountID:    "acct_bound",
				Status:       "invoke_error",
				ErrorCode:    code,
				ErrorMessage: code + ": failed",
			}}}
			if !shouldRetryResponsesWithoutPreviousResponse(req, err) {
				t.Fatalf("shouldRetryResponsesWithoutPreviousResponse(%q) = false, want true", code)
			}
		})
	}

	t.Run("model_scoped_upstream_not_found", func(t *testing.T) {
		err := &failover.ExecutionError{Attempts: []core.AttemptRecord{{
			AccountID:    "acct_bound",
			Status:       "invoke_error",
			ErrorCode:    "upstream_not_found",
			ErrorMessage: "upstream_not_found: Model 'gpt-5.5-openai-compact' is not supported by any configured account in this group",
		}}}
		if !shouldRetryResponsesWithoutPreviousResponse(req, err) {
			t.Fatal("shouldRetryResponsesWithoutPreviousResponse(model-scoped upstream_not_found) = false, want true")
		}
	})

	noRetryCodes := []string{
		"upstream_rejected",
		"upstream_not_found",
		"upstream_request_build_failed",
		"upstream_invalid_json",
	}
	for _, code := range noRetryCodes {
		t.Run("no_retry_"+code, func(t *testing.T) {
			err := &failover.ExecutionError{Attempts: []core.AttemptRecord{{
				AccountID:    "acct_bound",
				Status:       "invoke_error",
				ErrorCode:    code,
				ErrorMessage: code + ": failed",
			}}}
			if shouldRetryResponsesWithoutPreviousResponse(req, err) {
				t.Fatalf("shouldRetryResponsesWithoutPreviousResponse(%q) = true, want false", code)
			}
		})
	}

	t.Run("server_error_with_account_or_line_signal", func(t *testing.T) {
		err := &failover.ExecutionError{Attempts: []core.AttemptRecord{{
			AccountID:    "acct_bound",
			Status:       "invoke_error",
			ErrorCode:    "upstream_server_error",
			ErrorMessage: "upstream_server_error: upstream returned HTML challenge response",
		}}}
		if !shouldRetryResponsesWithoutPreviousResponse(req, err) {
			t.Fatalf("shouldRetryResponsesWithoutPreviousResponse(server HTML challenge) = false, want true")
		}
	})

	t.Run("generic_server_error_preserves_previous_response", func(t *testing.T) {
		err := &failover.ExecutionError{Attempts: []core.AttemptRecord{{
			AccountID:    "acct_bound",
			Status:       "invoke_error",
			ErrorCode:    "upstream_server_error",
			ErrorMessage: "upstream_server_error: upstream returned status 500",
		}}}
		if shouldRetryResponsesWithoutPreviousResponse(req, err) {
			t.Fatalf("shouldRetryResponsesWithoutPreviousResponse(generic server error) = true, want false")
		}
	})

	t.Run("last_relevant_bound_attempt_controls_fallback", func(t *testing.T) {
		err := &failover.ExecutionError{Attempts: []core.AttemptRecord{
			{
				AccountID:    "acct_bound",
				Status:       "invoke_error",
				ErrorCode:    "upstream_auth_error",
				ErrorMessage: "upstream_auth_error: expired",
			},
			{
				AccountID:    "acct_bound",
				Status:       "invoke_error",
				ErrorCode:    "upstream_rejected",
				ErrorMessage: "upstream_rejected: invalid request",
			},
		}}
		if shouldRetryResponsesWithoutPreviousResponse(req, err) {
			t.Fatalf("shouldRetryResponsesWithoutPreviousResponse(auth then rejected) = true, want false")
		}
	})
}

func TestShouldRetrySameAccountStreamBeforeOutputRejectsQuotaErrors(t *testing.T) {
	client := &core.APIClient{RouteAffinityKey: "client\x00gpt-4.1\x00session-id:sess"}
	err := &providers.InvokeError{
		Code:      "upstream_rate_limited",
		Temporary: true,
		Err:       errors.New("rate limited"),
	}
	if shouldRetrySameAccountStreamBeforeOutput(client, err, false) {
		t.Fatal("quota error should not retry same account before output")
	}
	if !shouldRetryStreamBeforeOutput(err, false, 0) {
		t.Fatal("quota error should retry on another account before output")
	}

	authErr := &providers.InvokeError{
		Code:      "gateway_api_key_disabled",
		Temporary: true,
		Err:       errors.New("API key is disabled"),
	}
	if shouldRetrySameAccountStreamBeforeOutput(client, authErr, false) {
		t.Fatal("gateway API key disabled should not retry same account before output")
	}
	if !shouldRetryStreamBeforeOutput(authErr, false, 0) {
		t.Fatal("gateway API key disabled should retry on another account before output")
	}
}

func TestStreamEventPreOutputErrorDetectsHTMLChallengeVariants(t *testing.T) {
	cases := []struct {
		name     string
		event    *core.StreamEvent
		wantCode string
	}{
		{
			name:     "raw_html_body",
			event:    &core.StreamEvent{RawData: []byte(`<body><div>Cloudflare challenge-platform</div></body>`)},
			wantCode: "upstream_server_error",
		},
		{
			name:     "json_string_html",
			event:    &core.StreamEvent{RawData: []byte(`"<!doctype html><html><title>Cloudflare</title>"`)},
			wantCode: "upstream_server_error",
		},
		{
			name:     "responses_created_embedded_html",
			event:    &core.StreamEvent{RawEvent: "response.created", RawData: []byte(`{"type":"response.created","body":"<html><title>Cloudflare</title></html>"}`)},
			wantCode: "upstream_server_error",
		},
		{
			name:     "top_level_json_error",
			event:    &core.StreamEvent{RawData: []byte(`{"error":{"type":"server_error","message":"temporary stream failure"}}`)},
			wantCode: "upstream_temporarily_unavailable",
		},
		{
			name:     "top_level_json_error_no_available_key",
			event:    &core.StreamEvent{RawData: []byte(`{"error":{"code":"no_available_key"}}`)},
			wantCode: "upstream_temporarily_unavailable",
		},
		{
			name:     "top_level_json_error_gateway_key_disabled",
			event:    &core.StreamEvent{RawData: []byte(`{"error":{"code":"API_KEY_DISABLED","message":"API key is disabled"}}`)},
			wantCode: "gateway_api_key_disabled",
		},
		{
			name:  "normal_responses_created",
			event: &core.StreamEvent{RawEvent: "response.created", RawData: []byte(`{"type":"response.created","response":{"status":"in_progress"}}`)},
		},
		{
			name:  "normal_responses_created_mentions_cloudflare",
			event: &core.StreamEvent{RawEvent: "response.created", RawData: []byte(`{"type":"response.created","metadata":{"note":"Cloudflare Workers endpoint"}}`)},
		},
		{
			name:  "normal_responses_created_mentions_cloudflare_challenge",
			event: &core.StreamEvent{RawEvent: "response.created", RawData: []byte(`{"type":"response.created","metadata":{"note":"Cloudflare challenge cf-ray"}}`)},
		},
		{
			name:  "first_output_text_not_blocked",
			event: &core.StreamEvent{FirstOutput: true, Delta: "just a moment", RawData: []byte(`{"type":"response.output_text.delta","delta":"just a moment"}`)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := streamEventPreOutputError(tc.event)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("streamEventPreOutputError returned %v, want nil", err)
				}
				return
			}
			if providers.ErrorCode(err) != tc.wantCode {
				t.Fatalf("error code = %q, want %q (err=%v)", providers.ErrorCode(err), tc.wantCode, err)
			}
		})
	}
}

func TestShouldRetryResponsesWebSocketSendForGatewayAPIKeyDisabled(t *testing.T) {
	err := &providers.InvokeError{
		Code:      "gateway_api_key_disabled",
		Temporary: true,
		Err:       errors.New("API key is disabled"),
	}
	if !shouldRetryResponsesWebSocketSend(err) {
		t.Fatal("gateway API key disabled should reopen websocket on another account")
	}
}

func (a *echoAdapter) Embed(_ context.Context, decision core.RouteDecision, req *core.EmbeddingRequest) (*core.EmbeddingResponse, error) {
	return &core.EmbeddingResponse{
		Model:        decision.Model,
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Data: []core.EmbeddingObject{
			{Object: "embedding", Index: 0, Embedding: []float64{float64(len(req.Metadata["order"]))}},
		},
		Usage: core.Usage{
			PromptTokens: 1,
			TotalTokens:  1,
		},
	}, nil
}

func (a *echoAdapter) GenerateImage(_ context.Context, decision core.RouteDecision, req *core.ImageGenerationRequest) (*core.ImageGenerationResponse, error) {
	return &core.ImageGenerationResponse{
		Model:        decision.Model,
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Body:         []byte(`{"data":[]}`),
	}, nil
}

func (a *echoAdapter) OpenStream(_ context.Context, decision core.RouteDecision, req *core.GatewayRequest) (*providers.StreamSession, error) {
	return &providers.StreamSession{
		Response: &core.GatewayResponse{
			ID:           "resp_stream_test",
			Model:        decision.Model,
			Provider:     decision.Provider,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			CreatedAt:    time.Now().UTC(),
		},
		Stream: &testStream{
			events: []*core.StreamEvent{
				{Delta: req.Metadata["order"] + ">hello "},
				{Delta: "world"},
				{FinishReason: "stop", Usage: &core.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}, Done: true},
			},
		},
	}, nil
}

func TestExecuteRefreshesAccountQuotaAfterUse(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_quota", Username: "quota", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_quota", Name: "Quota", APIKey: "gw_quota", OwnerUserID: "user_quota", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_quota",
		Provider: core.ProviderOpenAI,
		Label:    "Quota Account",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{Metadata: map[string]string{
			"token_source": providers.OpenAIDeviceCodeTokenSourceValue(),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	seedGatewayModel(t, repo, "gpt-4.1", core.ProviderOpenAI)

	adapter := &quotaEchoAdapter{fetched: make(chan string, 1)}
	registry := providers.NewRegistry(adapter)
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	).WithQuotaRegistry(registry)

	resp, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Client:   &client,
		Metadata: map[string]string{"order": "quota"},
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if resp.AccountID != "acct_quota" {
		t.Fatalf("account id = %q, want acct_quota", resp.AccountID)
	}
	select {
	case accountID := <-adapter.fetched:
		if accountID != "acct_quota" {
			t.Fatalf("quota fetched for %q, want acct_quota", accountID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for quota refresh")
	}

	var raw string
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		account, err := repo.GetAccount("acct_quota")
		if err != nil {
			t.Fatal(err)
		}
		raw = strings.TrimSpace(account.Credential.Metadata[core.AccountQuotaMetadataKey])
		if raw != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if raw == "" {
		account, _ := repo.GetAccount("acct_quota")
		t.Fatalf("quota snapshot metadata was not stored: %#v", account.Credential.Metadata)
	}
	var snapshot core.AccountQuotaSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		t.Fatalf("quota snapshot json: %v", err)
	}
	if snapshot.Credits == nil || snapshot.Credits.Balance == nil || *snapshot.Credits.Balance != 18.5 {
		t.Fatalf("quota credits = %#v", snapshot.Credits)
	}
}

func TestExecuteQuotaFetchAuthFailureDoesNotMarkAccountExpired(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_quota_auth", Username: "quota_auth", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_quota_auth", Name: "Quota Auth", APIKey: "gw_quota_auth", OwnerUserID: "user_quota_auth", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_quota_auth",
		Provider: core.ProviderOpenAI,
		Label:    "Quota Auth Account",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{Metadata: map[string]string{
			"token_source":                          providers.OpenAIDeviceCodeTokenSourceValue(),
			core.AccountQuotaErrorStatusMetadataKey: string(core.AccountStatusExpired),
		}},
	}); err != nil {
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
		OutputPriceNanoUSDPer1M: 2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	registry := providers.NewRegistry(&quotaAuthFailAdapter{})
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	).WithQuotaRegistry(registry)

	resp, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Client:   &client,
		Metadata: map[string]string{"order": "quota_auth"},
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if resp.AccountID != "acct_quota_auth" {
		t.Fatalf("account id = %q, want acct_quota_auth", resp.AccountID)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		account, err := repo.GetAccount("acct_quota_auth")
		if err != nil {
			t.Fatal(err)
		}
		if account.Credential.Metadata[core.AccountQuotaErrorCodeMetadataKey] == "credential_expired" {
			if account.Status != core.AccountStatusActive {
				t.Fatalf("status = %q, want active", account.Status)
			}
			if account.Credential.Metadata[core.AccountQuotaErrorStatusMetadataKey] != "" {
				t.Fatalf("metadata = %#v, want no terminal quota error status", account.Credential.Metadata)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	account, _ := repo.GetAccount("acct_quota_auth")
	t.Fatalf("account = %#v, want active with credential_expired quota error metadata", account)
}

func TestRefreshAccountQuotaCredentialRefreshFailureMarksAccountExpired(t *testing.T) {
	repo := storage.NewMemoryRepository()
	expiredAt := time.Now().UTC().Add(-time.Minute)
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_quota_refresh_auth",
		Provider: core.ProviderOpenAI,
		Label:    "Quota Refresh Auth Account",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:         providers.OpenAIOAuthModeValue(),
			AccessToken:  "expired-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    &expiredAt,
			Metadata: map[string]string{
				"token_source": providers.OpenAIDeviceCodeTokenSourceValue(),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	registry := providers.NewRegistry(&quotaRefreshAuthFailAdapter{})
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	).WithQuotaRegistry(registry)

	if err := service.refreshAccountQuota(context.Background(), "acct_quota_refresh_auth"); err != nil {
		t.Fatalf("refreshAccountQuota returned error: %v", err)
	}

	account, err := repo.GetAccount("acct_quota_refresh_auth")
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != core.AccountStatusExpired ||
		account.Credential.Metadata[core.AccountQuotaErrorCodeMetadataKey] != "credential_expired" ||
		account.Credential.Metadata[core.AccountQuotaErrorStatusMetadataKey] != string(core.AccountStatusExpired) {
		t.Fatalf("account = %#v, want expired with credential refresh quota error", account)
	}
}

func TestGatewayCredentialRefreshFailurePreservesExistingTerminalStatus(t *testing.T) {
	future := time.Now().UTC().Add(time.Hour)
	for _, status := range []core.AccountStatus{core.AccountStatusExpired, core.AccountStatusProviderBanned} {
		account := providers.ApplyCredentialRefreshFailureStatus(core.Account{
			Status:        status,
			CooldownUntil: &future,
		}, &providers.InvokeError{
			Code:      "upstream_transport_error",
			Temporary: true,
			Err:       errors.New("temporary network error"),
		}, time.Now().UTC())
		if account.Status != status {
			t.Fatalf("status = %q, want %q", account.Status, status)
		}
		if account.CooldownUntil != nil {
			t.Fatalf("cooldown = %#v, want nil", account.CooldownUntil)
		}
	}
}

func TestAccountSupportsQuotaRefreshIncludesClaudeAPIKeyProviders(t *testing.T) {
	account := core.Account{
		Provider: core.ProviderClaude,
		Credential: core.Credential{
			Mode: "manual-token",
			Metadata: map[string]string{
				"account_login_method":   "api_key",
				"api_key_quota_provider": "sub2api",
				"base_url":               "https://claude-gateway.example",
			},
		},
	}
	if !accountSupportsQuotaRefresh(account) {
		t.Fatal("Claude API-key sub2api account should support automatic quota refresh")
	}
	account.Credential.Metadata["api_key_quota_provider"] = "gateway"
	if !accountSupportsQuotaRefresh(account) {
		t.Fatal("Claude API-key gateway account should support automatic quota refresh")
	}
	delete(account.Credential.Metadata, "base_url")
	if accountSupportsQuotaRefresh(account) {
		t.Fatal("Claude API-key quota refresh should require a base URL")
	}
}

func TestExecuteSettlesBillingFromUsage(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_billing", Username: "billing", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_billing", Name: "Billing", APIKey: "gw_billing", OwnerUserID: "user_billing", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_billing",
		Provider: core.ProviderOpenAI,
		Label:    "Billing Account",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "token",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "priced-model",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "priced-model",
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: 2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&echoAdapter{})),
	)
	maxTokens := 2
	if _, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:     "priced-model",
		Messages:  []core.Message{{Role: "user", Content: "hello"}},
		Client:    &client,
		MaxTokens: &maxTokens,
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	user, err := repo.GetUser("user_billing")
	if err != nil {
		t.Fatal(err)
	}
	const wantCost int64 = 5000
	if user.BalanceNanoUSD != core.NanoUSDPerUSD-wantCost {
		t.Fatalf("balance = %d, want %d", user.BalanceNanoUSD, core.NanoUSDPerUSD-wantCost)
	}
	spend, err := repo.GetClientSpend("client_billing")
	if err != nil {
		t.Fatal(err)
	}
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
}

func TestExecuteStreamRecordsFirstTokenTimeInBillingRequest(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_stream_billing", Username: "stream-billing", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_stream_billing", Name: "Stream Billing", APIKey: "gw_stream_billing", OwnerUserID: "user_stream_billing", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_stream_billing",
		Provider: core.ProviderOpenAI,
		Label:    "Stream Billing Account",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "token",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "stream-priced-model",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "stream-priced-model",
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: 2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&echoAdapter{})),
	)
	maxTokens := 2
	var streamed strings.Builder
	if err := service.ExecuteStream(context.Background(), &core.GatewayRequest{
		Model:     "stream-priced-model",
		Messages:  []core.Message{{Role: "user", Content: "hello"}},
		Client:    &client,
		MaxTokens: &maxTokens,
		Metadata:  map[string]string{"order": "stream"},
	}, func(_ *core.GatewayResponse, event *core.StreamEvent) error {
		if event != nil {
			streamed.WriteString(event.Delta)
		}
		return nil
	}); err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}
	if streamed.String() != "stream>hello world" {
		t.Fatalf("streamed = %q, want %q", streamed.String(), "stream>hello world")
	}
	requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: client.ID, Limit: 1})
	if total != 1 || len(requests) != 1 {
		t.Fatalf("billing requests = total %d items %#v, want one", total, requests)
	}
	if requests[0].Status != core.BillingRequestSettled {
		t.Fatalf("billing request status = %q, want %q", requests[0].Status, core.BillingRequestSettled)
	}
	if requests[0].FirstTokenMS <= 0 {
		t.Fatalf("first token ms = %d, want > 0", requests[0].FirstTokenMS)
	}
}

type firstOutputResponsesStreamAdapter struct {
	echoAdapter
}

func (a *firstOutputResponsesStreamAdapter) OpenResponsesStream(_ context.Context, decision core.RouteDecision, _ *core.ResponsesRequest) (*providers.StreamSession, error) {
	return &providers.StreamSession{
		Response: &core.GatewayResponse{
			ID:           "resp_responses_first_output",
			Model:        decision.Model,
			Provider:     decision.Provider,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			CreatedAt:    time.Now().UTC(),
		},
		Stream: &testStream{
			events: []*core.StreamEvent{
				{FirstOutput: true, RawEvent: "response.reasoning_summary_text.delta"},
				{FinishReason: "stop", Usage: &core.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}, Done: true, RawEvent: "response.completed"},
			},
		},
	}, nil
}

type noUsageResponsesStreamAdapter struct {
	echoAdapter
}

func (a *noUsageResponsesStreamAdapter) OpenResponsesStream(_ context.Context, decision core.RouteDecision, _ *core.ResponsesRequest) (*providers.StreamSession, error) {
	return &providers.StreamSession{
		Response: &core.GatewayResponse{
			ID:           "resp_no_usage",
			Model:        decision.Model,
			Provider:     decision.Provider,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			FinishReason: "stop",
			CreatedAt:    time.Now().UTC(),
		},
		Stream: &testStream{events: []*core.StreamEvent{
			{FinishReason: "stop", Done: true, RawEvent: "response.completed"},
		}},
	}, nil
}

func TestExecuteResponsesStreamSkipsBillingForSSEGenerateFalse(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_responses_generate_false", Username: "responses-generate-false", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_responses_generate_false", Name: "Responses Generate False", APIKey: "gw_responses_generate_false", OwnerUserID: "user_responses_generate_false", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_responses_generate_false",
		Provider: core.ProviderOpenAI,
		Label:    "Responses Generate False Account",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "token",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "responses-generate-false-model",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "responses-generate-false-model",
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: 2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&noUsageResponsesStreamAdapter{})),
	)
	generate := false
	if err := service.ExecuteResponsesStream(context.Background(), &core.ResponsesRequest{
		Model:     "responses-generate-false-model",
		RawBody:   json.RawMessage(`{"model":"responses-generate-false-model","stream":true,"generate":false,"input":"warmup"}`),
		Client:    &client,
		Stream:    true,
		Generate:  &generate,
		Transport: core.ResponsesTransportSSE,
	}, nil); err != nil {
		t.Fatalf("ExecuteResponsesStream returned error: %v", err)
	}
	if requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: client.ID, Limit: 1}); total != 0 || len(requests) != 0 {
		t.Fatalf("billing requests = total %d items %#v, want none", total, requests)
	}
}

type rawFirstOutputResponsesStreamAdapter struct {
	echoAdapter
}

func (a *rawFirstOutputResponsesStreamAdapter) OpenResponsesStream(_ context.Context, decision core.RouteDecision, _ *core.ResponsesRequest) (*providers.StreamSession, error) {
	return &providers.StreamSession{
		Response: &core.GatewayResponse{
			ID:           "resp_responses_raw_first_output",
			Model:        decision.Model,
			Provider:     decision.Provider,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			CreatedAt:    time.Now().UTC(),
		},
		Stream: &testStream{
			events: []*core.StreamEvent{
				{FirstOutput: true, RawEvent: "response.function_call_arguments.delta", RawData: []byte(`{"delta":"{\"cmd\""}`)},
				{FinishReason: "stop", Usage: &core.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}, Done: true, RawEvent: "response.completed"},
			},
		},
	}, nil
}

func TestExecuteResponsesStreamRecordsFirstOutputTimeInBillingRequest(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_responses_first_output", Username: "responses-first-output", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_responses_first_output", Name: "Responses First Output", APIKey: "gw_responses_first_output", OwnerUserID: "user_responses_first_output", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_responses_first_output",
		Provider: core.ProviderOpenAI,
		Label:    "Responses First Output Account",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "token",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "responses-first-output-model",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "responses-first-output-model",
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: 2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&firstOutputResponsesStreamAdapter{})),
	)
	var deltas strings.Builder
	if err := service.ExecuteResponsesStream(context.Background(), &core.ResponsesRequest{
		Model:   "responses-first-output-model",
		RawBody: json.RawMessage(`{"model":"responses-first-output-model","stream":true,"input":"hello"}`),
		Client:  &client,
		Stream:  true,
	}, func(_ *core.GatewayResponse, event *core.StreamEvent) error {
		if event != nil {
			deltas.WriteString(event.Delta)
		}
		return nil
	}); err != nil {
		t.Fatalf("ExecuteResponsesStream returned error: %v", err)
	}
	if deltas.String() != "" {
		t.Fatalf("deltas = %q, want empty", deltas.String())
	}
	requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: client.ID, Limit: 1})
	if total != 1 || len(requests) != 1 {
		t.Fatalf("billing requests = total %d items %#v, want one", total, requests)
	}
	if requests[0].Status != core.BillingRequestSettled {
		t.Fatalf("billing request status = %q, want %q", requests[0].Status, core.BillingRequestSettled)
	}
	if requests[0].FirstTokenMS <= 0 {
		t.Fatalf("first token ms = %d, want > 0", requests[0].FirstTokenMS)
	}
}

func TestExecuteResponsesStreamRecordsRawFirstOutputTimeInBillingRequest(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_responses_raw_first_output", Username: "responses-raw-first-output", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_responses_raw_first_output", Name: "Responses Raw First Output", APIKey: "gw_responses_raw_first_output", OwnerUserID: "user_responses_raw_first_output", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_responses_raw_first_output",
		Provider: core.ProviderOpenAI,
		Label:    "Responses Raw First Output Account",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "token",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "responses-raw-first-output-model",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "responses-raw-first-output-model",
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: 2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&rawFirstOutputResponsesStreamAdapter{})),
	)
	if err := service.ExecuteResponsesStream(context.Background(), &core.ResponsesRequest{
		Model:   "responses-raw-first-output-model",
		RawBody: json.RawMessage(`{"model":"responses-raw-first-output-model","stream":true,"input":"hello"}`),
		Client:  &client,
		Stream:  true,
	}, func(_ *core.GatewayResponse, _ *core.StreamEvent) error {
		return nil
	}); err != nil {
		t.Fatalf("ExecuteResponsesStream returned error: %v", err)
	}
	requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: client.ID, Limit: 1})
	if total != 1 || len(requests) != 1 {
		t.Fatalf("billing requests = total %d items %#v, want one", total, requests)
	}
	if requests[0].Status != core.BillingRequestSettled {
		t.Fatalf("billing request status = %q, want %q", requests[0].Status, core.BillingRequestSettled)
	}
	if requests[0].FirstTokenMS <= 0 {
		t.Fatalf("first token ms = %d, want > 0", requests[0].FirstTokenMS)
	}
}

func TestExecuteAppliesFastBillingMultiplier(t *testing.T) {
	tests := []struct {
		name        string
		model       string
		upstreamID  string
		serviceTier string
		wantCost    int64
		wantFast    bool
	}{
		{name: "gpt-5.5 fast", model: "gpt-5.5", serviceTier: "fast", wantCost: 12500, wantFast: true},
		{name: "gpt-5.5 priority", model: "gpt-5.5", serviceTier: "priority", wantCost: 12500, wantFast: true},
		{name: "gpt-5.4 fast", model: "gpt-5.4", serviceTier: "fast", wantCost: 10000, wantFast: true},
		{name: "gpt-5.5 upstream alias fast", model: "codex-alias", upstreamID: "gpt-5.5", serviceTier: "fast", wantCost: 12500, wantFast: true},
		{name: "gpt-5.5 raw body fast", model: "gpt-5.5", serviceTier: "raw:fast", wantCost: 12500, wantFast: true},
		{name: "gpt-5.5 standard", model: "gpt-5.5", serviceTier: "", wantCost: 5000},
		{name: "other fast", model: "gpt-4.1", serviceTier: "fast", wantCost: 5000, wantFast: true},
		{name: "other priority", model: "gpt-4.1", serviceTier: "priority", wantCost: 5000, wantFast: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := storage.NewMemoryRepository()
			if err := repo.UpsertUser(core.User{ID: "user_billing", Username: "billing", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
				t.Fatal(err)
			}
			client := core.APIClient{ID: "client_billing", Name: "Billing", APIKey: "gw_billing", OwnerUserID: "user_billing", Enabled: true}
			if err := repo.UpsertClient(client); err != nil {
				t.Fatal(err)
			}
			if err := repo.UpsertAccount(core.Account{
				ID:       "acct_billing",
				Provider: core.ProviderOpenAI,
				Label:    "Billing Account",
				Status:   core.AccountStatusActive,
				Priority: 100,
				Weight:   100,
				Credential: core.Credential{
					Mode:        "manual-token",
					AccessToken: "token",
				},
			}); err != nil {
				t.Fatal(err)
			}
			upstreamID := tt.upstreamID
			if upstreamID == "" {
				upstreamID = tt.model
			}
			if err := repo.UpsertModel(core.ModelConfig{
				ID:                      tt.model,
				Provider:                core.ProviderOpenAI,
				UpstreamID:              upstreamID,
				Enabled:                 true,
				VisibleGroups:           []string{core.DefaultAccountGroupName},
				Source:                  core.ModelSourceManual,
				InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
				OutputPriceNanoUSDPer1M: 2 * core.NanoUSDPerUSD,
			}); err != nil {
				t.Fatal(err)
			}

			service := New(
				repo,
				routing.NewRouter(),
				failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&echoAdapter{})),
			)
			maxTokens := 2
			serviceTier := tt.serviceTier
			var rawBody json.RawMessage
			if strings.HasPrefix(serviceTier, "raw:") {
				rawTier := strings.TrimPrefix(serviceTier, "raw:")
				serviceTier = ""
				rawBody = json.RawMessage(fmt.Sprintf(`{"model":%q,"input":"hello","service_tier":%q}`, tt.model, rawTier))
			}
			if _, err := service.Execute(context.Background(), &core.GatewayRequest{
				Model:       tt.model,
				Messages:    []core.Message{{Role: "user", Content: "hello"}},
				RawBody:     rawBody,
				Client:      &client,
				MaxTokens:   &maxTokens,
				ServiceTier: serviceTier,
			}); err != nil {
				t.Fatalf("Execute returned error: %v", err)
			}

			user, err := repo.GetUser("user_billing")
			if err != nil {
				t.Fatal(err)
			}
			if user.BalanceNanoUSD != core.NanoUSDPerUSD-tt.wantCost {
				t.Fatalf("balance = %d, want %d", user.BalanceNanoUSD, core.NanoUSDPerUSD-tt.wantCost)
			}
			spend, err := repo.GetClientSpend("client_billing")
			if err != nil {
				t.Fatal(err)
			}
			if spend.SpendUsedNanoUSD != tt.wantCost {
				t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, tt.wantCost)
			}
			requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: "client_billing", Limit: 1})
			if total != 1 || len(requests) != 1 {
				t.Fatalf("billing requests = total %d items %#v, want one", total, requests)
			}
			if requests[0].FastMode != tt.wantFast {
				t.Fatalf("FastMode = %v, want %v", requests[0].FastMode, tt.wantFast)
			}
		})
	}
}

func TestExecuteMarksFastModeFromUpstreamServiceTier(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_billing", Username: "billing", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_billing", Name: "Billing", APIKey: "gw_billing", OwnerUserID: "user_billing", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_billing",
		Provider: core.ProviderOpenAI,
		Label:    "Billing Account",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "token",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "gpt-5.5",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "gpt-5.5",
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: 2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&billingEchoAdapter{
			usage:       core.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
			serviceTier: "priority",
		})),
	)
	maxTokens := 2
	if _, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:     "gpt-5.5",
		Messages:  []core.Message{{Role: "user", Content: "hello"}},
		Client:    &client,
		MaxTokens: &maxTokens,
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	spend, err := repo.GetClientSpend("client_billing")
	if err != nil {
		t.Fatal(err)
	}
	if spend.SpendUsedNanoUSD != 12500 {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, int64(12500))
	}
	requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: "client_billing", Limit: 1})
	if total != 1 || len(requests) != 1 {
		t.Fatalf("billing requests = total %d items %#v, want one", total, requests)
	}
	if !requests[0].FastMode {
		t.Fatal("FastMode = false, want true")
	}
}

func TestSettleBillingPublishesBillingEvent(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_billing_event", Username: "billing-event", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_billing_event", Name: "Billing Event", APIKey: "gw_billing_event", OwnerUserID: "user_billing_event", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	model := core.ModelConfig{
		ID:                      "billing-event-model",
		Provider:                core.ProviderOpenAI,
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}
	var events []BillingEvent
	service := New(repo, routing.NewRouter(), nil).WithBillingEvents(func(event BillingEvent) {
		events = append(events, event)
	})

	hold, err := service.reserveGatewayBilling("req_billing_event", &client, model, model.ID, &core.GatewayRequest{
		Model:    model.ID,
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Client:   &client,
	})
	if err != nil {
		t.Fatalf("reserveGatewayBilling returned error: %v", err)
	}
	if hold == nil {
		t.Fatal("expected billing hold")
	}
	if err := service.settleGatewayBilling(hold, &core.GatewayResponse{
		Model:     model.ID,
		Provider:  core.ProviderOpenAI,
		AccountID: "acct_billing_event",
		Usage:     core.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
	}); err != nil {
		t.Fatalf("settleGatewayBilling returned error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %#v, want one", events)
	}
	if events[0].Reason != "usage_settled" || events[0].UserID != client.OwnerUserID || events[0].RequestID != "req_billing_event" || events[0].ClientID != client.ID {
		t.Fatalf("event = %#v", events[0])
	}
}

func TestReserveBillingCreatesHoldWhenEstimatedUsageCostIsZero(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_zero_estimate", Username: "zero-estimate", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_zero_estimate", Name: "Zero Estimate", APIKey: "gw_zero_estimate", OwnerUserID: "user_zero_estimate", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	model := core.ModelConfig{
		ID:                      "output-only-priced-model",
		Provider:                core.ProviderOpenAI,
		Enabled:                 true,
		OutputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}
	service := New(repo, routing.NewRouter(), nil)

	hold, err := service.reserveBilling("req_zero_estimate", &client, model, 0, 0, "zero-estimate", 10000, false, "")
	if err != nil {
		t.Fatalf("reserveBilling returned error: %v", err)
	}
	if hold == nil {
		t.Fatal("expected billing hold")
	}
	requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: client.ID, Limit: 1})
	if total != 1 || len(requests) != 1 {
		t.Fatalf("billing requests = total %d items %#v, want one pending request", total, requests)
	}
	if requests[0].ReservedNanoUSD != 0 {
		t.Fatalf("reserved amount = %d, want 0", requests[0].ReservedNanoUSD)
	}

	if err := service.settleGatewayBilling(hold, &core.GatewayResponse{
		Model:     model.ID,
		Provider:  core.ProviderOpenAI,
		AccountID: "acct_zero_estimate",
		Usage:     core.Usage{CompletionTokens: 2, TotalTokens: 2},
	}); err != nil {
		t.Fatalf("settleGatewayBilling returned error: %v", err)
	}
	spend, err := repo.GetClientSpend(client.ID)
	if err != nil {
		t.Fatal(err)
	}
	if spend.SpendUsedNanoUSD != 2000 {
		t.Fatalf("spend = %d, want 2000", spend.SpendUsedNanoUSD)
	}
	user, err := repo.GetUser("user_zero_estimate")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != core.NanoUSDPerUSD-2000 {
		t.Fatalf("balance = %d, want %d", user.BalanceNanoUSD, core.NanoUSDPerUSD-2000)
	}
}

func TestFastBillingMultiplierUsesRoutedModel(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_routed_billing", Username: "routed_billing", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_routed_billing", Name: "Routed Billing", APIKey: "gw_routed_billing", OwnerUserID: "user_routed_billing", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_routed_billing", Provider: core.ProviderOpenAI, Label: "Routed Billing", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&echoAdapter{})),
	)
	model := core.ModelConfig{
		ID:                      "gateway-alias",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "gpt-4.1",
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: 2 * core.NanoUSDPerUSD,
	}
	maxTokens := 2
	req := &core.GatewayRequest{
		Model:       "gateway-alias",
		Messages:    []core.Message{{Role: "user", Content: "hello"}},
		Client:      &client,
		MaxTokens:   &maxTokens,
		ServiceTier: "fast",
	}

	hold, err := service.reserveGatewayBilling("req_routed_fast", &client, model, "gpt-5.5", req)
	if err != nil {
		t.Fatalf("reserveGatewayBilling returned error: %v", err)
	}
	if hold == nil {
		t.Fatal("expected billing hold")
	}
	if err := service.settleGatewayBilling(hold, &core.GatewayResponse{
		Model:     "gpt-5.5",
		Provider:  core.ProviderOpenAI,
		AccountID: "acct_routed_billing",
		Usage:     core.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
	}); err != nil {
		t.Fatalf("settleGatewayBilling returned error: %v", err)
	}

	const wantCost int64 = 12500
	user, err := repo.GetUser("user_routed_billing")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != core.NanoUSDPerUSD-wantCost {
		t.Fatalf("balance = %d, want %d", user.BalanceNanoUSD, core.NanoUSDPerUSD-wantCost)
	}
}

func TestBillingCostSaturatesWhenComponentsOverflow(t *testing.T) {
	const wantMaxInt64 = int64(^uint64(0) >> 1)
	cost := billingCostNano(core.Usage{
		PromptTokens:       2_000_000,
		CachedPromptTokens: 1_000_000,
		CompletionTokens:   1_000_000,
		TotalTokens:        3_000_000,
	}, billingPricing{
		inputNanoUSDPer1M:       1 << 62,
		cachedInputNanoUSDPer1M: 1 << 62,
		outputNanoUSDPer1M:      1 << 62,
	})
	if cost != wantMaxInt64 {
		t.Fatalf("cost = %d, want saturated max int64", cost)
	}
}

func TestBillingCostChargesCacheCreationAndImageOutputPrices(t *testing.T) {
	cost := billingCostNano(core.Usage{
		PromptTokens:          100,
		CachedPromptTokens:    20,
		CacheCreationTokens:   30,
		CacheCreation5mTokens: 10,
		CacheCreation1hTokens: 5,
		CompletionTokens:      40,
		ImageOutputTokens:     12,
		TotalTokens:           170,
	}, billingPricing{
		inputNanoUSDPer1M:        1_000_000,
		cachedInputNanoUSDPer1M:  100_000,
		cacheWriteNanoUSDPer1M:   2_000_000,
		cacheWrite5mNanoUSDPer1M: 3_000_000,
		cacheWrite1hNanoUSDPer1M: 4_000_000,
		outputNanoUSDPer1M:       5_000_000,
		imageOutputNanoUSDPer1M:  6_000_000,
	})
	// 80 input + 20 cached + 15 cache write + 10 cache write 5m + 5 cache write 1h + 28 text output + 12 image output.
	const wantCost int64 = 374
	if cost != wantCost {
		t.Fatalf("cost = %d, want %d", cost, wantCost)
	}
}

func TestSettleBillingDoesNotChargeOpenAICacheCreationTokens(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_openai_cache_create", Username: "openai_cache_create", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_openai_cache_create", Name: "OpenAI Cache Create", APIKey: "gw_openai_cache_create", OwnerUserID: "user_openai_cache_create", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	model := core.ModelConfig{
		ID:                            "openai-cache-create-model",
		Provider:                      core.ProviderOpenAI,
		Enabled:                       true,
		InputPriceNanoUSDPer1M:        core.NanoUSDPerUSD,
		CachedInputPriceNanoUSDPer1M:  core.NanoUSDPerUSD / 2,
		CacheWritePriceNanoUSDPer1M:   10 * core.NanoUSDPerUSD,
		CacheWrite5mPriceNanoUSDPer1M: 20 * core.NanoUSDPerUSD,
		CacheWrite1hPriceNanoUSDPer1M: 30 * core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M:       2 * core.NanoUSDPerUSD,
		ImageOutputPriceNanoUSDPer1M:  3 * core.NanoUSDPerUSD,
	}
	service := New(repo, routing.NewRouter(), nil)

	hold, err := service.reserveBilling("req_openai_cache_create", &client, model, 0, 0, "openai-cache-create", 10000, false, "")
	if err != nil {
		t.Fatalf("reserveBilling returned error: %v", err)
	}
	if hold == nil {
		t.Fatal("expected billing hold")
	}
	if err := service.settleGatewayBilling(hold, &core.GatewayResponse{
		Model:     model.ID,
		Provider:  core.ProviderOpenAI,
		AccountID: "acct_openai_cache_create",
		Usage: core.Usage{
			PromptTokens:          100,
			CachedPromptTokens:    40,
			CacheCreationTokens:   30,
			CacheCreation5mTokens: 20,
			CacheCreation1hTokens: 10,
			CompletionTokens:      50,
			ImageOutputTokens:     10,
			TotalTokens:           180,
		},
	}); err != nil {
		t.Fatalf("settleGatewayBilling returned error: %v", err)
	}

	spend, err := repo.GetClientSpend(client.ID)
	if err != nil {
		t.Fatal(err)
	}
	const wantCost int64 = 190000
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
	requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: client.ID, Limit: 1})
	if total != 1 || len(requests) != 1 {
		t.Fatalf("billing requests = total %d items %#v, want one", total, requests)
	}
	if requests[0].CacheCreationTokens != 0 || requests[0].CacheCreation5mTokens != 0 || requests[0].CacheCreation1hTokens != 0 {
		t.Fatalf("stored OpenAI cache creation tokens = %d/%d/%d, want zero",
			requests[0].CacheCreationTokens,
			requests[0].CacheCreation5mTokens,
			requests[0].CacheCreation1hTokens,
		)
	}
}

func TestSettleBillingUsesReservedPricingWhenOnlyNewPriceFieldsAreSet(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_new_price_fields", Username: "new_price_fields", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_new_price_fields", Name: "New Price Fields", APIKey: "gw_new_price_fields", OwnerUserID: "user_new_price_fields", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	model := core.ModelConfig{
		ID:                            "new-price-fields-model",
		Provider:                      core.ProviderClaude,
		Enabled:                       true,
		CacheWritePriceNanoUSDPer1M:   core.NanoUSDPerUSD,
		CacheWrite5mPriceNanoUSDPer1M: 2 * core.NanoUSDPerUSD,
		CacheWrite1hPriceNanoUSDPer1M: 3 * core.NanoUSDPerUSD,
		ImageOutputPriceNanoUSDPer1M:  4 * core.NanoUSDPerUSD,
	}
	service := New(repo, routing.NewRouter(), nil)
	hold, err := service.reserveBilling("req_new_price_fields", &client, model, 0, 0, "new-price-fields", 10000, false, "")
	if err != nil {
		t.Fatalf("reserveBilling returned error: %v", err)
	}
	if hold == nil {
		t.Fatal("expected billing hold")
	}
	if err := service.settleGatewayBilling(hold, &core.GatewayResponse{
		Model:     model.ID,
		Provider:  core.ProviderClaude,
		AccountID: "acct_missing_from_snapshot",
		Usage: core.Usage{
			CacheCreationTokens:   6,
			CacheCreation5mTokens: 2,
			CacheCreation1hTokens: 1,
			CompletionTokens:      3,
			ImageOutputTokens:     3,
			TotalTokens:           9,
		},
	}); err != nil {
		t.Fatalf("settleGatewayBilling returned error: %v", err)
	}

	spend, err := repo.GetClientSpend(client.ID)
	if err != nil {
		t.Fatal(err)
	}
	const wantCost int64 = 22000
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
	requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: client.ID, Limit: 1})
	if total != 1 || len(requests) != 1 {
		t.Fatalf("billing requests = total %d items %#v, want one", total, requests)
	}
	if requests[0].CacheWritePriceNanoUSDPer1M != core.NanoUSDPerUSD ||
		requests[0].CacheWrite5mPriceNanoUSDPer1M != 2*core.NanoUSDPerUSD ||
		requests[0].CacheWrite1hPriceNanoUSDPer1M != 3*core.NanoUSDPerUSD ||
		requests[0].ImageOutputPriceNanoUSDPer1M != 4*core.NanoUSDPerUSD {
		t.Fatalf("settled new field prices = cache %d 5m %d 1h %d image %d",
			requests[0].CacheWritePriceNanoUSDPer1M,
			requests[0].CacheWrite5mPriceNanoUSDPer1M,
			requests[0].CacheWrite1hPriceNanoUSDPer1M,
			requests[0].ImageOutputPriceNanoUSDPer1M,
		)
	}
}

func TestExecuteBillsCachedPromptTokensAtCachedInputPrice(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_cached_billing", Username: "cached_billing", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_cached_billing", Name: "Cached Billing", APIKey: "gw_cached_billing", OwnerUserID: "user_cached_billing", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_cached_billing", Provider: core.ProviderOpenAI, Label: "Cached Billing", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                           "cached-priced-model",
		Provider:                     core.ProviderOpenAI,
		UpstreamID:                   "cached-priced-model",
		Enabled:                      true,
		VisibleGroups:                []string{core.DefaultAccountGroupName},
		Source:                       core.ModelSourceManual,
		InputPriceNanoUSDPer1M:       2 * core.NanoUSDPerUSD,
		CachedInputPriceNanoUSDPer1M: core.NanoUSDPerUSD / 2,
		OutputPriceNanoUSDPer1M:      4 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&billingEchoAdapter{usage: core.Usage{PromptTokens: 10, CachedPromptTokens: 6, CompletionTokens: 3, TotalTokens: 13}})),
	)
	maxTokens := 1
	if _, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:     "cached-priced-model",
		Messages:  []core.Message{{Role: "user", Content: "hello"}},
		Client:    &client,
		MaxTokens: &maxTokens,
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	spend, err := repo.GetClientSpend("client_cached_billing")
	if err != nil {
		t.Fatal(err)
	}
	const wantCost int64 = 23000
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
}

func TestExecuteBillsClaudeCacheReadAsPromptTokens(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_claude_cache_billing", Username: "claude_cache_billing", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_claude_cache_billing", Name: "Claude Cache Billing", APIKey: "gw_claude_cache_billing", OwnerUserID: "user_claude_cache_billing", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_claude_cache_billing", Provider: core.ProviderClaude, Label: "Claude Cache Billing", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                           "claude-cache-priced-model",
		Provider:                     core.ProviderClaude,
		UpstreamID:                   "claude-cache-priced-model",
		Enabled:                      true,
		VisibleGroups:                []string{core.DefaultAccountGroupName},
		Source:                       core.ModelSourceManual,
		InputPriceNanoUSDPer1M:       2 * core.NanoUSDPerUSD,
		CachedInputPriceNanoUSDPer1M: core.NanoUSDPerUSD / 2,
		OutputPriceNanoUSDPer1M:      4 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, routing.NewRouter(), nil)
	model, err := repo.GetModel("claude-cache-priced-model")
	if err != nil {
		t.Fatal(err)
	}
	maxTokens := 1
	hold, err := service.reserveGatewayBilling("req_claude_cache_billing", &client, model, model.ID, &core.GatewayRequest{
		Model:     "claude-cache-priced-model",
		Messages:  []core.Message{{Role: "user", Content: "hello"}},
		Client:    &client,
		MaxTokens: &maxTokens,
	})
	if err != nil {
		t.Fatalf("reserveGatewayBilling returned error: %v", err)
	}
	if hold == nil {
		t.Fatal("expected billing hold")
	}
	if err := service.settleGatewayBilling(hold, &core.GatewayResponse{
		Model:     model.ID,
		Provider:  core.ProviderClaude,
		AccountID: "acct_claude_cache_billing",
		Usage:     core.Usage{PromptTokens: 140, CachedPromptTokens: 40, CompletionTokens: 25, TotalTokens: 165},
	}); err != nil {
		t.Fatalf("settleGatewayBilling returned error: %v", err)
	}

	spend, err := repo.GetClientSpend("client_claude_cache_billing")
	if err != nil {
		t.Fatal(err)
	}
	const wantCost int64 = 320000
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
}

func TestExecuteBillsActualAccountGroupPricing(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_group_billing", Username: "group_billing", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_group_billing", Name: "Group Billing", APIKey: "gw_group_billing", OwnerUserID: "user_group_billing", Enabled: true, AccountGroup: "Premium"}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                           "group_premium",
		Name:                         "Premium",
		BillingMultiplierBps:         25000,
		InputPriceNanoUSDPer1M:       3 * core.NanoUSDPerUSD,
		CachedInputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M:      5 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_group_billing", Provider: core.ProviderOpenAI, Label: "Group Billing", Group: "Premium", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                           "group-priced-model",
		Provider:                     core.ProviderOpenAI,
		UpstreamID:                   "group-priced-model",
		Enabled:                      true,
		VisibleGroups:                []string{"Premium"},
		Source:                       core.ModelSourceManual,
		InputPriceNanoUSDPer1M:       core.NanoUSDPerUSD,
		CachedInputPriceNanoUSDPer1M: core.NanoUSDPerUSD / 4,
		OutputPriceNanoUSDPer1M:      2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&billingEchoAdapter{usage: core.Usage{PromptTokens: 10, CachedPromptTokens: 4, CompletionTokens: 2, TotalTokens: 12}})),
	)
	maxTokens := 1
	if _, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:     "group-priced-model",
		Messages:  []core.Message{{Role: "user", Content: "hello"}},
		Client:    &client,
		MaxTokens: &maxTokens,
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	spend, err := repo.GetClientSpend("client_group_billing")
	if err != nil {
		t.Fatal(err)
	}
	const wantCost int64 = 80000
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
}

func TestSettleBillingUsesReservedAccountGroupSnapshot(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_snapshot_billing", Username: "snapshot_billing", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{
		ID:           "client_snapshot_billing",
		Name:         "Snapshot Billing",
		APIKey:       "gw_snapshot_billing",
		OwnerUserID:  "user_snapshot_billing",
		Enabled:      true,
		AccountGroup: "Premium",
	}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                   "group_snapshot_premium",
		Name:                 "Premium",
		BillingMultiplierBps: 30000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_snapshot_billing", Provider: core.ProviderOpenAI, Label: "Snapshot Billing", Group: "Premium", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	model := core.ModelConfig{
		ID:                      "snapshot-priced-model",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "snapshot-priced-model",
		Enabled:                 true,
		VisibleGroups:           []string{"Premium"},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}
	service := New(repo, routing.NewRouter(), nil)
	maxTokens := 1
	hold, err := service.reserveGatewayBilling("req_snapshot_billing", &client, model, "", &core.GatewayRequest{
		Model:     model.ID,
		Messages:  []core.Message{{Role: "user", Content: "hello"}},
		Client:    &client,
		MaxTokens: &maxTokens,
	})
	if err != nil {
		t.Fatalf("reserveGatewayBilling returned error: %v", err)
	}
	if hold == nil {
		t.Fatal("expected billing hold")
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                      "group_snapshot_premium",
		Name:                    "Premium",
		BillingMultiplierBps:    90000,
		InputPriceNanoUSDPer1M:  10 * core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: 10 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.settleGatewayBilling(hold, &core.GatewayResponse{
		Model:     model.ID,
		Provider:  core.ProviderOpenAI,
		AccountID: "acct_snapshot_billing",
		Usage:     core.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}); err != nil {
		t.Fatalf("settleGatewayBilling returned error: %v", err)
	}

	const wantCost int64 = 6000
	spend, err := repo.GetClientSpend("client_snapshot_billing")
	if err != nil {
		t.Fatal(err)
	}
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
	requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: "client_snapshot_billing", Limit: 1})
	if total != 1 || len(requests) != 1 {
		t.Fatalf("billing requests = total %d items %#v, want one", total, requests)
	}
	if requests[0].AccountID != "acct_snapshot_billing" {
		t.Fatalf("account id = %q", requests[0].AccountID)
	}
	if requests[0].ClientName != "Snapshot Billing" {
		t.Fatalf("client name snapshot = %q", requests[0].ClientName)
	}
	if requests[0].AccountGroup != "Premium" || requests[0].AccountGroupMultiplierBps != 30000 {
		t.Fatalf("account group snapshot = %q multiplier %d, want Premium/30000", requests[0].AccountGroup, requests[0].AccountGroupMultiplierBps)
	}
	if requests[0].InputPriceNanoUSDPer1M != 3*core.NanoUSDPerUSD || requests[0].OutputPriceNanoUSDPer1M != 3*core.NanoUSDPerUSD {
		t.Fatalf("settlement prices = input %d output %d, want frozen 3x model prices", requests[0].InputPriceNanoUSDPer1M, requests[0].OutputPriceNanoUSDPer1M)
	}
}

func TestSettleBillingFallbackDoesNotUnderchargeWhenAccountMissingFromSnapshot(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_snapshot_fallback", Username: "snapshot_fallback", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{
		ID:           "client_snapshot_fallback",
		Name:         "Snapshot Fallback",
		APIKey:       "gw_snapshot_fallback",
		OwnerUserID:  "user_snapshot_fallback",
		Enabled:      true,
		AccountGroup: "Premium",
	}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_snapshot_fallback", Name: "Premium", BillingMultiplierBps: 30000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_snapshot_fallback", Provider: core.ProviderOpenAI, Label: "Snapshot Fallback", Group: "Premium", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	model := core.ModelConfig{
		ID:                      "snapshot-fallback-model",
		Provider:                core.ProviderOpenAI,
		Enabled:                 true,
		VisibleGroups:           []string{"Premium"},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}
	service := New(repo, routing.NewRouter(), nil)
	maxTokens := 1
	hold, err := service.reserveGatewayBilling("req_snapshot_fallback", &client, model, "", &core.GatewayRequest{
		Model:     model.ID,
		Messages:  []core.Message{{Role: "user", Content: "hello"}},
		Client:    &client,
		MaxTokens: &maxTokens,
	})
	if err != nil {
		t.Fatalf("reserveGatewayBilling returned error: %v", err)
	}
	if hold == nil {
		t.Fatal("expected billing hold")
	}
	if err := service.settleGatewayBilling(hold, &core.GatewayResponse{
		Model:     model.ID,
		Provider:  core.ProviderOpenAI,
		AccountID: "acct_added_after_reserve",
		Usage:     core.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}); err != nil {
		t.Fatalf("settleGatewayBilling returned error: %v", err)
	}

	const wantCost int64 = 6000
	spend, err := repo.GetClientSpend("client_snapshot_fallback")
	if err != nil {
		t.Fatal(err)
	}
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
}

func TestSettleFixedBillingUsesReservedAccountGroupSnapshot(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_fixed_snapshot", Username: "fixed_snapshot", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{
		ID:           "client_fixed_snapshot",
		Name:         "Fixed Snapshot",
		APIKey:       "gw_fixed_snapshot",
		OwnerUserID:  "user_fixed_snapshot",
		Enabled:      true,
		AccountGroup: "Premium",
	}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                   "group_fixed_snapshot",
		Name:                 "Premium",
		BillingMultiplierBps: 30000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_fixed_snapshot", Provider: core.ProviderOpenAI, Label: "Fixed Snapshot", Group: "Premium", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	model := core.ModelConfig{
		ID:                  "fixed-snapshot-model",
		Provider:            core.ProviderOpenAI,
		UpstreamID:          "fixed-snapshot-model",
		Enabled:             true,
		VisibleGroups:       []string{"Premium"},
		Source:              core.ModelSourceManual,
		BillingMode:         core.ModelBillingModeRequest,
		RequestPriceNanoUSD: 1000,
	}
	service := New(repo, routing.NewRouter(), nil)
	hold, err := service.reserveGatewayBilling("req_fixed_snapshot", &client, model, "", &core.GatewayRequest{
		Model:    model.ID,
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Client:   &client,
	})
	if err != nil {
		t.Fatalf("reserveGatewayBilling returned error: %v", err)
	}
	if hold == nil {
		t.Fatal("expected billing hold")
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                   "group_fixed_snapshot",
		Name:                 "Premium",
		BillingMultiplierBps: 90000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.settleFixedBilling(hold, "acct_fixed_snapshot", core.ProviderOpenAI, model.ID); err != nil {
		t.Fatalf("settleFixedBilling returned error: %v", err)
	}

	const wantCost int64 = 3000
	spend, err := repo.GetClientSpend("client_fixed_snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
}

func TestReserveRequestBillingRecordsClientAccountGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_reserved_group", Username: "reserved_group", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{
		ID:           "client_reserved_group",
		Name:         "Reserved Group",
		APIKey:       "gw_reserved_group",
		OwnerUserID:  "user_reserved_group",
		Enabled:      true,
		AccountGroup: "Plus",
	}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                   "group_reserved_plus",
		Name:                 "Plus",
		BillingMultiplierBps: 2500,
	}); err != nil {
		t.Fatal(err)
	}
	model := core.ModelConfig{
		ID:                  "fixed-reserved-group-model",
		Provider:            core.ProviderOpenAI,
		Enabled:             true,
		VisibleGroups:       []string{"Plus"},
		Source:              core.ModelSourceManual,
		BillingMode:         core.ModelBillingModeRequest,
		RequestPriceNanoUSD: 4000,
	}
	service := New(repo, routing.NewRouter(), nil)
	hold, err := service.reserveRequestBilling("req_reserved_group", &client, model, core.BillingModalityImage, "image_generation:fixed-reserved-group-model", 10000, false, "")
	if err != nil {
		t.Fatalf("reserveRequestBilling returned error: %v", err)
	}
	if hold == nil {
		t.Fatal("expected billing hold")
	}

	requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: client.ID, Limit: 10})
	if total != 1 || len(requests) != 1 {
		t.Fatalf("billing requests = total %d rows %#v, want one", total, requests)
	}
	if requests[0].AccountGroup != "Plus" {
		t.Fatalf("reserved account group = %q, want Plus", requests[0].AccountGroup)
	}
	if requests[0].AccountGroupMultiplierBps != 2500 {
		t.Fatalf("reserved account group multiplier = %d, want 2500", requests[0].AccountGroupMultiplierBps)
	}
}

func TestSettleFixedBillingFallbackUsesReservedAmountWhenAccountMissingFromSnapshot(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_fixed_snapshot_fallback", Username: "fixed_snapshot_fallback", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{
		ID:           "client_fixed_snapshot_fallback",
		Name:         "Fixed Snapshot Fallback",
		APIKey:       "gw_fixed_snapshot_fallback",
		OwnerUserID:  "user_fixed_snapshot_fallback",
		Enabled:      true,
		AccountGroup: "Premium",
	}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_fixed_snapshot_fallback", Name: "Premium", BillingMultiplierBps: 30000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_fixed_snapshot_fallback", Provider: core.ProviderOpenAI, Label: "Fixed Snapshot Fallback", Group: "Premium", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	model := core.ModelConfig{
		ID:                  "fixed-snapshot-fallback-model",
		Provider:            core.ProviderOpenAI,
		Enabled:             true,
		VisibleGroups:       []string{"Premium"},
		Source:              core.ModelSourceManual,
		BillingMode:         core.ModelBillingModeRequest,
		RequestPriceNanoUSD: 1000,
	}
	service := New(repo, routing.NewRouter(), nil)
	hold, err := service.reserveGatewayBilling("req_fixed_snapshot_fallback", &client, model, "", &core.GatewayRequest{
		Model:    model.ID,
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Client:   &client,
	})
	if err != nil {
		t.Fatalf("reserveGatewayBilling returned error: %v", err)
	}
	if hold == nil {
		t.Fatal("expected billing hold")
	}
	if err := service.settleFixedBilling(hold, "acct_added_after_reserve", core.ProviderOpenAI, model.ID); err != nil {
		t.Fatalf("settleFixedBilling returned error: %v", err)
	}

	const wantCost int64 = 3000
	spend, err := repo.GetClientSpend("client_fixed_snapshot_fallback")
	if err != nil {
		t.Fatal(err)
	}
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
}

func TestBillingGroupSnapshotFreezesTimedMultiplier(t *testing.T) {
	now := time.Date(2026, 5, 18, 10, 15, 0, 0, time.UTC)
	snapshot := billingGroupSnapshot(core.AccountGroup{
		BillingMultiplierBps: 12000,
		TimedMultipliers: []core.AccountGroupTimedMultiplier{
			{
				ID:            "morning",
				Enabled:       true,
				MultiplierBps: 40000,
				StartDate:     "2026-05-18",
				EndDate:       "2026-05-18",
				StartTime:     "10:00",
				EndTime:       "10:30",
			},
		},
	}, now)

	if snapshot.BillingMultiplierBps != 40000 {
		t.Fatalf("snapshot multiplier = %d, want 40000", snapshot.BillingMultiplierBps)
	}
	if len(snapshot.TimedMultipliers) != 0 {
		t.Fatalf("snapshot kept timed rules: %#v", snapshot.TimedMultipliers)
	}
	if got := core.EffectiveAccountGroupMultiplier(snapshot, now.Add(24*time.Hour)); got != 40000 {
		t.Fatalf("later effective multiplier = %d, want frozen 40000", got)
	}
}

func TestBillingAccountGroupSnapshotKeepsUnconfiguredAccountGroupName(t *testing.T) {
	repo := storage.NewMemoryRepository()
	client := core.APIClient{
		ID:           "client_unconfigured_group_snapshot",
		OwnerUserID:  "user_unconfigured_group_snapshot",
		AccountGroup: "Unconfigured",
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_unconfigured_group_snapshot", Provider: core.ProviderOpenAI, Label: "Unconfigured", Group: "Unconfigured", Status: core.AccountStatusActive, Priority: 100, Weight: 100}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, routing.NewRouter(), nil)
	snapshot := service.billingAccountGroupSnapshot(core.ModelConfig{Provider: core.ProviderOpenAI}, &client)
	group := snapshot["acct_unconfigured_group_snapshot"]
	if group.Name != "Unconfigured" || group.BillingMultiplierBps != core.AccountGroupDefaultMultiplierBps {
		t.Fatalf("snapshot group = %#v, want name Unconfigured with default multiplier", group)
	}
}

func TestExecuteBillsBillingFixedIgnoresAccountGroupPricing(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_fixed_group_billing", Username: "fixed_group_billing", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_fixed_group_billing", Name: "Fixed Group Billing", APIKey: "gw_fixed_group_billing", OwnerUserID: "user_fixed_group_billing", Enabled: true, AccountGroup: "Premium"}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                           "group_fixed_premium",
		Name:                         "Premium",
		BillingMultiplierBps:         25000,
		InputPriceNanoUSDPer1M:       3 * core.NanoUSDPerUSD,
		CachedInputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M:      5 * core.NanoUSDPerUSD,
		TimedMultipliers: []core.AccountGroupTimedMultiplier{
			{ID: "always", Enabled: true, MultiplierBps: 30000},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_fixed_group_billing", Provider: core.ProviderOpenAI, Label: "Fixed Group Billing", Group: "Premium", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                           "fixed-group-priced-model",
		Provider:                     core.ProviderOpenAI,
		UpstreamID:                   "fixed-group-priced-model",
		Enabled:                      true,
		VisibleGroups:                []string{"Premium"},
		Source:                       core.ModelSourceManual,
		BillingFixed:                 true,
		InputPriceNanoUSDPer1M:       core.NanoUSDPerUSD,
		CachedInputPriceNanoUSDPer1M: core.NanoUSDPerUSD / 4,
		OutputPriceNanoUSDPer1M:      2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&billingEchoAdapter{usage: core.Usage{PromptTokens: 10, CachedPromptTokens: 4, CompletionTokens: 2, TotalTokens: 12}})),
	)
	maxTokens := 1
	if _, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:     "fixed-group-priced-model",
		Messages:  []core.Message{{Role: "user", Content: "hello"}},
		Client:    &client,
		MaxTokens: &maxTokens,
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	spend, err := repo.GetClientSpend("client_fixed_group_billing")
	if err != nil {
		t.Fatal(err)
	}
	const wantCost int64 = 11000
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
}

func TestExecuteBillsDefaultAccountGroupPricing(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_default_group_billing", Username: "default_group_billing", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_default_group_billing", Name: "Default Group Billing", APIKey: "gw_default_group_billing", OwnerUserID: "user_default_group_billing", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                           "default",
		Name:                         "",
		BillingMultiplierBps:         25000,
		InputPriceNanoUSDPer1M:       3 * core.NanoUSDPerUSD,
		CachedInputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M:      5 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                           "group_plus_default_guard",
		Name:                         "Plus",
		BillingMultiplierBps:         90000,
		InputPriceNanoUSDPer1M:       9 * core.NanoUSDPerUSD,
		CachedInputPriceNanoUSDPer1M: 9 * core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M:      9 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_plus_default_guard", Provider: core.ProviderOpenAI, Label: "Plus Guard", Group: "Plus", Status: core.AccountStatusActive, Priority: 500, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "plus-token"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_default_group_billing", Provider: core.ProviderOpenAI, Label: "Default Group Billing", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                           "default-group-priced-model",
		Provider:                     core.ProviderOpenAI,
		UpstreamID:                   "default-group-priced-model",
		Enabled:                      true,
		VisibleGroups:                []string{core.DefaultAccountGroupName},
		Source:                       core.ModelSourceManual,
		InputPriceNanoUSDPer1M:       core.NanoUSDPerUSD,
		CachedInputPriceNanoUSDPer1M: core.NanoUSDPerUSD / 4,
		OutputPriceNanoUSDPer1M:      2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&billingEchoAdapter{usage: core.Usage{PromptTokens: 10, CachedPromptTokens: 4, CompletionTokens: 2, TotalTokens: 12}})),
	)
	maxTokens := 1
	if _, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:     "default-group-priced-model",
		Messages:  []core.Message{{Role: "user", Content: "hello"}},
		Client:    &client,
		MaxTokens: &maxTokens,
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	spend, err := repo.GetClientSpend("client_default_group_billing")
	if err != nil {
		t.Fatal(err)
	}
	const wantCost int64 = 80000
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
	requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: "client_default_group_billing", Limit: 1})
	if total != 1 || len(requests) != 1 {
		t.Fatalf("billing requests = total %d rows %#v", total, requests)
	}
	if requests[0].AccountID != "acct_default_group_billing" || requests[0].AccountGroup != core.DefaultAccountGroupName || requests[0].AccountGroupMultiplierBps != 25000 {
		t.Fatalf("billing snapshot = account %q group %q multiplier %d, want Default account/group/25000", requests[0].AccountID, requests[0].AccountGroup, requests[0].AccountGroupMultiplierBps)
	}
}

func TestExecuteBillsClientBoundToAccountGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_client_group_billing", Username: "client_group_billing", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{
		ID:           "client_group_bound_billing",
		Name:         "Group Bound Billing",
		APIKey:       "gw_group_bound_billing",
		OwnerUserID:  "user_client_group_billing",
		Enabled:      true,
		AccountGroup: "Premium",
	}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                           "group_premium_bound",
		Name:                         "Premium",
		BillingMultiplierBps:         25000,
		InputPriceNanoUSDPer1M:       3 * core.NanoUSDPerUSD,
		CachedInputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M:      5 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_group_bound_billing", Provider: core.ProviderOpenAI, Label: "Group Bound Billing", Group: "Premium", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                           "client-group-priced-model",
		Provider:                     core.ProviderOpenAI,
		UpstreamID:                   "client-group-priced-model",
		Enabled:                      true,
		VisibleGroups:                []string{"Premium"},
		Source:                       core.ModelSourceManual,
		InputPriceNanoUSDPer1M:       core.NanoUSDPerUSD,
		CachedInputPriceNanoUSDPer1M: core.NanoUSDPerUSD / 4,
		OutputPriceNanoUSDPer1M:      2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&billingEchoAdapter{usage: core.Usage{PromptTokens: 10, CachedPromptTokens: 4, CompletionTokens: 2, TotalTokens: 12}})),
	)
	maxTokens := 1
	if _, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:     "client-group-priced-model",
		Messages:  []core.Message{{Role: "user", Content: "hello"}},
		Client:    &client,
		MaxTokens: &maxTokens,
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	spend, err := repo.GetClientSpend("client_group_bound_billing")
	if err != nil {
		t.Fatal(err)
	}
	const wantCost int64 = 80000
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
}

func TestExecuteBillsTimedAccountGroupMultiplier(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_timed_group_billing", Username: "timed_group_billing", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_timed_group_billing", Name: "Timed Group Billing", APIKey: "gw_timed_group_billing", OwnerUserID: "user_timed_group_billing", Enabled: true, AccountGroup: "Timed"}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                           "group_timed",
		Name:                         "Timed",
		BillingMultiplierBps:         10000,
		InputPriceNanoUSDPer1M:       3 * core.NanoUSDPerUSD,
		CachedInputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M:      5 * core.NanoUSDPerUSD,
		TimedMultipliers: []core.AccountGroupTimedMultiplier{
			{ID: "always", Enabled: true, MultiplierBps: 30000},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_timed_group_billing", Provider: core.ProviderOpenAI, Label: "Timed Group Billing", Group: "Timed", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                           "timed-group-priced-model",
		Provider:                     core.ProviderOpenAI,
		UpstreamID:                   "timed-group-priced-model",
		Enabled:                      true,
		VisibleGroups:                []string{"Timed"},
		Source:                       core.ModelSourceManual,
		InputPriceNanoUSDPer1M:       core.NanoUSDPerUSD,
		CachedInputPriceNanoUSDPer1M: core.NanoUSDPerUSD / 4,
		OutputPriceNanoUSDPer1M:      2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&billingEchoAdapter{usage: core.Usage{PromptTokens: 10, CachedPromptTokens: 4, CompletionTokens: 2, TotalTokens: 12}})),
	)
	maxTokens := 1
	if _, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:     "timed-group-priced-model",
		Messages:  []core.Message{{Role: "user", Content: "hello"}},
		Client:    &client,
		MaxTokens: &maxTokens,
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	spend, err := repo.GetClientSpend("client_timed_group_billing")
	if err != nil {
		t.Fatal(err)
	}
	const wantCost int64 = 96000
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
}

func TestExecutePlanBillingUsesPlanMultiplierOnly(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_plan_group_billing", Username: "plan_group_billing", Enabled: true, BalanceNanoUSD: 100 * core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "plan_gateway_group",
		Name:               "Gateway Plan",
		Enabled:            true,
		PriceNanoUSD:       10 * core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 100 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_group_billing", PlanID: "plan_gateway_group"}); err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	client := core.APIClient{
		ID:            "client_plan_group_billing",
		Name:          "Plan Group Billing",
		APIKey:        "gw_plan_group_billing",
		OwnerUserID:   "user_plan_group_billing",
		Enabled:       true,
		AccountGroup:  "PlanGroup",
		BillingSource: core.ClientBillingSourcePlan,
	}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                       "group_plan_gateway",
		Name:                     "PlanGroup",
		BillingMultiplierBps:     30000,
		PlanBillingMultiplierBps: 5000,
		TimedMultipliers: []core.AccountGroupTimedMultiplier{
			{ID: "always", Enabled: true, MultiplierBps: 90000},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_plan_group_billing", Provider: core.ProviderOpenAI, Label: "Plan Group Billing", Group: "PlanGroup", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "gpt-5.5-plan-priced-model",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "gpt-5.5",
		Enabled:                 true,
		VisibleGroups:           []string{"PlanGroup"},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: 2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&billingEchoAdapter{usage: core.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}})),
	)
	maxTokens := 1
	if _, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:       "gpt-5.5-plan-priced-model",
		Messages:    []core.Message{{Role: "user", Content: "hello"}},
		Client:      &client,
		MaxTokens:   &maxTokens,
		ServiceTier: "fast",
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	user, err := repo.GetUser("user_plan_group_billing")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != 90*core.NanoUSDPerUSD {
		t.Fatalf("balance = %d, want plan purchase only %d", user.BalanceNanoUSD, 90*core.NanoUSDPerUSD)
	}
	entitlement, err := repo.GetActiveUserPlanEntitlement("user_plan_group_billing")
	if err != nil {
		t.Fatalf("GetActiveUserPlanEntitlement returned error: %v", err)
	}
	const wantCost int64 = 7000
	if entitlement.CurrentQuotaNanoUSD != 100*core.NanoUSDPerUSD-wantCost {
		t.Fatalf("plan quota = %d, want %d", entitlement.CurrentQuotaNanoUSD, 100*core.NanoUSDPerUSD-wantCost)
	}
	spend, err := repo.GetClientSpend("client_plan_group_billing")
	if err != nil {
		t.Fatal(err)
	}
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
	requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: "client_plan_group_billing", Limit: 1})
	if total != 1 || len(requests) != 1 {
		t.Fatalf("billing requests = total %d rows %#v", total, requests)
	}
	if requests[0].BillingSource != core.ClientBillingSourcePlan || requests[0].AccountGroupMultiplierBps != 5000 || requests[0].FastMode != true {
		t.Fatalf("billing request source/multiplier/fast = %q/%d/%v, want plan/5000/true", requests[0].BillingSource, requests[0].AccountGroupMultiplierBps, requests[0].FastMode)
	}
}

func TestExecuteRejectsPlanBillingWhenAccountGroupDisablesPlanBilling(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_plan_disabled_group", Username: "plan_disabled_group", Enabled: true, BalanceNanoUSD: 100 * core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "plan_gateway_disabled_group",
		Name:               "Gateway Disabled Group Plan",
		Enabled:            true,
		PriceNanoUSD:       10 * core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 100 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_disabled_group", PlanID: "plan_gateway_disabled_group"}); err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	client := core.APIClient{
		ID:            "client_plan_disabled_group",
		Name:          "Plan Disabled Group",
		APIKey:        "gw_plan_disabled_group",
		OwnerUserID:   "user_plan_disabled_group",
		Enabled:       true,
		AccountGroup:  "CashOnly",
		BillingSource: core.ClientBillingSourcePlan,
	}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	planBillingEnabled := false
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                 "group_plan_disabled_gateway",
		Name:               "CashOnly",
		PlanBillingEnabled: &planBillingEnabled,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_plan_disabled_group", Provider: core.ProviderOpenAI, Label: "Plan Disabled Group", Group: "CashOnly", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "plan-disabled-group-model",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "plan-disabled-group-model",
		Enabled:                 true,
		VisibleGroups:           []string{"CashOnly"},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&billingEchoAdapter{usage: core.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}})),
	)
	maxTokens := 1
	_, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:     "plan-disabled-group-model",
		Messages:  []core.Message{{Role: "user", Content: "hello"}},
		Client:    &client,
		MaxTokens: &maxTokens,
	})
	var billingErr *BillingError
	if !errors.As(err, &billingErr) {
		t.Fatalf("expected BillingError, got %T %v", err, err)
	}
	if billingErr.Code != ErrorCodePlanBillingDisabled || billingErr.StatusCode != http.StatusForbidden {
		t.Fatalf("billing error = code %q status %d, want %q/%d", billingErr.Code, billingErr.StatusCode, ErrorCodePlanBillingDisabled, http.StatusForbidden)
	}
	if requests := repo.ListBillingRequests(storage.BillingRequestQuery{ClientID: "client_plan_disabled_group"}); len(requests) != 0 {
		t.Fatalf("billing requests = %#v, want none", requests)
	}
}

func TestExecuteUsesTieredPricing(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_rule_billing", Username: "rule_billing", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_rule_billing", Name: "Rule Billing", APIKey: "gw_rule_billing", OwnerUserID: "user_rule_billing", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_rule_billing", Provider: core.ProviderOpenAI, Label: "Rule Billing", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "rule-priced-model",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "rule-priced-model",
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
		BillingMode:             core.ModelBillingModeTieredExpr,
		PricingTiers: []core.ModelPricingTier{
			{
				Name:               "long",
				MaxInputTokens:     100,
				InputPriceNanoUSD:  3 * core.NanoUSDPerUSD,
				OutputPriceNanoUSD: 7 * core.NanoUSDPerUSD,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&billingEchoAdapter{usage: core.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}})),
	)
	maxTokens := 1
	if _, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:     "rule-priced-model",
		Messages:  []core.Message{{Role: "user", Content: "hello"}},
		Client:    &client,
		MaxTokens: &maxTokens,
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	spend, err := repo.GetClientSpend("client_rule_billing")
	if err != nil {
		t.Fatal(err)
	}
	const wantCost int64 = 370000
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
}

func TestGenerateImageBillsRequestPricingRule(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_image_billing", Username: "image_billing", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_image_billing", Name: "Image Billing", APIKey: "gw_image_billing", OwnerUserID: "user_image_billing", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_image_billing", Provider: core.ProviderOpenAI, Label: "Image Billing", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                  "image-priced-model",
		Provider:            core.ProviderOpenAI,
		UpstreamID:          "image-priced-model",
		Enabled:             true,
		VisibleGroups:       []string{core.DefaultAccountGroupName},
		Source:              core.ModelSourceManual,
		BillingMode:         core.ModelBillingModeRequest,
		RequestPriceNanoUSD: core.NanoUSDPerUSD / 100,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&echoAdapter{})),
	)
	if _, err := service.GenerateImage(context.Background(), &core.ImageGenerationRequest{
		Model:  "image-priced-model",
		Prompt: "draw",
		Client: &client,
	}); err != nil {
		t.Fatalf("GenerateImage returned error: %v", err)
	}

	spend, err := repo.GetClientSpend("client_image_billing")
	if err != nil {
		t.Fatal(err)
	}
	const wantCost int64 = core.NanoUSDPerUSD / 100
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
}

func TestGenerateImageDecrementsCachedImageQuota(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_image_quota", Username: "image_quota", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_image_quota", Name: "Image Quota", APIKey: "gw_image_quota", OwnerUserID: "user_image_quota", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	quotaRaw, err := json.Marshal(core.AccountQuotaSnapshot{
		Source: "openai_chatgpt_usage",
		Image:  &core.AccountImageQuota{Remaining: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_image_quota",
		Provider: core.ProviderOpenAI,
		Label:    "Image Quota",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "token",
			Metadata:    map[string]string{core.AccountQuotaMetadataKey: string(quotaRaw)},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                  "image-quota-model",
		Provider:            core.ProviderOpenAI,
		UpstreamID:          "image-quota-model",
		Enabled:             true,
		VisibleGroups:       []string{core.DefaultAccountGroupName},
		Source:              core.ModelSourceManual,
		BillingMode:         core.ModelBillingModeRequest,
		RequestPriceNanoUSD: core.NanoUSDPerUSD / 100,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&echoAdapter{})),
	)
	if _, err := service.GenerateImage(context.Background(), &core.ImageGenerationRequest{
		Model:  "image-quota-model",
		Prompt: "draw",
		Client: &client,
		Extra:  map[string]json.RawMessage{"n": json.RawMessage(`2`)},
	}); err != nil {
		t.Fatalf("GenerateImage returned error: %v", err)
	}

	account, err := repo.GetAccount("acct_image_quota")
	if err != nil {
		t.Fatal(err)
	}
	quota := core.ReadAccountQuota(account)
	if quota == nil || quota.Image == nil || quota.Image.Remaining != 1 {
		var remaining int64 = -1
		if quota != nil && quota.Image != nil {
			remaining = quota.Image.Remaining
		}
		t.Fatalf("image quota remaining = %d, quota = %#v, want 1", remaining, quota)
	}
}

func TestExecuteBillsRequestModeAtFixedPrice(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_request_billing", Username: "request_billing", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_request_billing", Name: "Request Billing", APIKey: "gw_request_billing", OwnerUserID: "user_request_billing", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_request_billing", Provider: core.ProviderOpenAI, Label: "Request Billing", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                  "request-priced-model",
		Provider:            core.ProviderOpenAI,
		UpstreamID:          "request-priced-model",
		Enabled:             true,
		VisibleGroups:       []string{core.DefaultAccountGroupName},
		Source:              core.ModelSourceManual,
		BillingMode:         core.ModelBillingModeRequest,
		RequestPriceNanoUSD: core.NanoUSDPerUSD / 20,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&billingEchoAdapter{usage: core.Usage{PromptTokens: 1000, CompletionTokens: 1000, TotalTokens: 2000}})),
	)
	if _, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:    "request-priced-model",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Client:   &client,
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	spend, err := repo.GetClientSpend("client_request_billing")
	if err != nil {
		t.Fatal(err)
	}
	const wantCost int64 = core.NanoUSDPerUSD / 20
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
}

func TestExecuteBillsRequestModeWithDefaultAccountGroupPricing(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_default_request_billing", Username: "default_request_billing", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_default_request_billing", Name: "Default Request Billing", APIKey: "gw_default_request_billing", OwnerUserID: "user_default_request_billing", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                   "default",
		Name:                 "",
		BillingMultiplierBps: 30000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_default_request_billing", Provider: core.ProviderOpenAI, Label: "Default Request Billing", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                  "default-request-priced-model",
		Provider:            core.ProviderOpenAI,
		UpstreamID:          "default-request-priced-model",
		Enabled:             true,
		VisibleGroups:       []string{core.DefaultAccountGroupName},
		Source:              core.ModelSourceManual,
		BillingMode:         core.ModelBillingModeRequest,
		RequestPriceNanoUSD: core.NanoUSDPerUSD / 20,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&billingEchoAdapter{usage: core.Usage{PromptTokens: 1000, CompletionTokens: 1000, TotalTokens: 2000}})),
	)
	if _, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:    "default-request-priced-model",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Client:   &client,
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	spend, err := repo.GetClientSpend("client_default_request_billing")
	if err != nil {
		t.Fatal(err)
	}
	const wantCost int64 = 3 * core.NanoUSDPerUSD / 20
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
}

func TestExecuteBillsRequestModeBillingFixedIgnoresDefaultAccountGroupPricing(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_fixed_default_request_billing", Username: "fixed_default_request_billing", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_fixed_default_request_billing", Name: "Fixed Default Request Billing", APIKey: "gw_fixed_default_request_billing", OwnerUserID: "user_fixed_default_request_billing", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                   "default",
		Name:                 "",
		BillingMultiplierBps: 30000,
		TimedMultipliers: []core.AccountGroupTimedMultiplier{
			{ID: "always", Enabled: true, MultiplierBps: 40000},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_fixed_default_request_billing", Provider: core.ProviderOpenAI, Label: "Fixed Default Request Billing", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                  "fixed-default-request-priced-model",
		Provider:            core.ProviderOpenAI,
		UpstreamID:          "fixed-default-request-priced-model",
		Enabled:             true,
		VisibleGroups:       []string{core.DefaultAccountGroupName},
		Source:              core.ModelSourceManual,
		BillingMode:         core.ModelBillingModeRequest,
		BillingFixed:        true,
		RequestPriceNanoUSD: core.NanoUSDPerUSD / 20,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&billingEchoAdapter{usage: core.Usage{PromptTokens: 1000, CompletionTokens: 1000, TotalTokens: 2000}})),
	)
	if _, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:    "fixed-default-request-priced-model",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Client:   &client,
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	spend, err := repo.GetClientSpend("client_fixed_default_request_billing")
	if err != nil {
		t.Fatal(err)
	}
	const wantCost int64 = core.NanoUSDPerUSD / 20
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
}

func TestExecuteRejectsInsufficientBillingBalance(t *testing.T) {
	repo := storage.NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.Runtime.PublicBaseURL = "https://gateway.example.com"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertUser(core.User{ID: "user_billing", Username: "billing", Enabled: true, BalanceNanoUSD: 0}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_billing", Name: "Billing", APIKey: "gw_billing", OwnerUserID: "user_billing", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_billing",
		Provider: core.ProviderOpenAI,
		Label:    "Billing Account",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "token",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "priced-model",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "priced-model",
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&echoAdapter{})),
	)
	maxTokens := 2
	_, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:     "priced-model",
		Messages:  []core.Message{{Role: "user", Content: "hello"}},
		Client:    &client,
		MaxTokens: &maxTokens,
	})
	if err == nil {
		t.Fatal("expected billing error")
	}
	var billingErr *BillingError
	if !errors.As(err, &billingErr) {
		t.Fatalf("expected BillingError, got %T", err)
	}
	if billingErr.StatusCode != 402 {
		t.Fatalf("status = %d, want 402", billingErr.StatusCode)
	}
	wantMessage := "Insufficient account balance. Please visit https://gateway.example.com to recharge before continuing."
	if billingErr.Code != "quota_error" || billingErr.Message != wantMessage {
		t.Fatalf("billing error = %q/%q, want quota_error/%q", billingErr.Code, billingErr.Message, wantMessage)
	}
	if billingErr.PublicURL != "https://gateway.example.com" {
		t.Fatalf("public url = %q, want https://gateway.example.com", billingErr.PublicURL)
	}
}

func TestInsufficientBalanceMessageFallsBackWithoutPublicURL(t *testing.T) {
	got := insufficientBalanceMessage("")
	want := "Insufficient account balance. Please recharge before continuing."
	if got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestExecuteRejectsExhaustedPlanBillingQuota(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_plan_exhausted", Username: "plan_exhausted", Enabled: true, BalanceNanoUSD: 100 * core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{
		ID:            "client_plan_exhausted",
		Name:          "Plan Exhausted",
		APIKey:        "gw_plan_exhausted",
		OwnerUserID:   "user_plan_exhausted",
		Enabled:       true,
		BillingSource: core.ClientBillingSourcePlan,
	}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_plan_exhausted",
		Provider: core.ProviderOpenAI,
		Label:    "Plan Exhausted Account",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "token",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "plan-exhausted-priced-model",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "plan-exhausted-priced-model",
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&echoAdapter{})),
	)
	maxTokens := 2
	_, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:     "plan-exhausted-priced-model",
		Messages:  []core.Message{{Role: "user", Content: "hello"}},
		Client:    &client,
		MaxTokens: &maxTokens,
	})
	if err == nil {
		t.Fatal("expected billing error")
	}
	var billingErr *BillingError
	if !errors.As(err, &billingErr) {
		t.Fatalf("expected BillingError, got %T", err)
	}
	if billingErr.StatusCode != 402 {
		t.Fatalf("status = %d, want 402", billingErr.StatusCode)
	}
	if billingErr.Code != "plan_quota_exhausted" || billingErr.Message != "plan quota exhausted; purchase a plan or switch this API key to balance billing" {
		t.Fatalf("billing error = %q/%q, want plan_quota_exhausted/plan quota exhausted", billingErr.Code, billingErr.Message)
	}
}

func TestExecuteSettlesWhenActualCostExceedsSpendLimit(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_limit_settle", Username: "limit_settle", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_limit_settle", Name: "Limit Settle", APIKey: "gw_limit_settle", OwnerUserID: "user_limit_settle", Enabled: true, SpendLimitNanoUSD: 5000}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_limit_settle", Provider: core.ProviderOpenAI, Label: "Limit Settle", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "limit-settle-model",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "limit-settle-model",
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&billingEchoAdapter{usage: core.Usage{PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20}})),
	)
	maxTokens := 1
	if _, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:     "limit-settle-model",
		Messages:  []core.Message{{Role: "user", Content: "hello"}},
		Client:    &client,
		MaxTokens: &maxTokens,
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	user, err := repo.GetUser("user_limit_settle")
	if err != nil {
		t.Fatal(err)
	}
	const wantCost int64 = 20000
	if user.BalanceNanoUSD != core.NanoUSDPerUSD-wantCost {
		t.Fatalf("balance = %d, want %d", user.BalanceNanoUSD, core.NanoUSDPerUSD-wantCost)
	}
	spend, err := repo.GetClientSpend("client_limit_settle")
	if err != nil {
		t.Fatal(err)
	}
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
}

func TestExecuteSettlesWhenActualCostExceedsBalance(t *testing.T) {
	repo := storage.NewMemoryRepository()
	const initialBalance int64 = 5000
	if err := repo.UpsertUser(core.User{ID: "user_balance_settle", Username: "balance_settle", Enabled: true, BalanceNanoUSD: initialBalance}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_balance_settle", Name: "Balance Settle", APIKey: "gw_balance_settle", OwnerUserID: "user_balance_settle", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_balance_settle", Provider: core.ProviderOpenAI, Label: "Balance Settle", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "token"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "balance-settle-model",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "balance-settle-model",
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&billingEchoAdapter{usage: core.Usage{PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20}})),
	)
	maxTokens := 1
	if _, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:     "balance-settle-model",
		Messages:  []core.Message{{Role: "user", Content: "hello"}},
		Client:    &client,
		MaxTokens: &maxTokens,
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	user, err := repo.GetUser("user_balance_settle")
	if err != nil {
		t.Fatal(err)
	}
	const wantCost int64 = 20000
	if user.BalanceNanoUSD != initialBalance-wantCost {
		t.Fatalf("balance = %d, want %d", user.BalanceNanoUSD, initialBalance-wantCost)
	}
	spend, err := repo.GetClientSpend("client_balance_settle")
	if err != nil {
		t.Fatal(err)
	}
	if spend.SpendUsedNanoUSD != wantCost {
		t.Fatalf("spend = %d, want %d", spend.SpendUsedNanoUSD, wantCost)
	}
}

type failoverAuditAdapter struct{}

func (a *failoverAuditAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *failoverAuditAdapter) DisplayName() string { return "Failover Audit" }

func (a *failoverAuditAdapter) ListModels(context.Context) []core.ModelSpec {
	return []core.ModelSpec{{Name: "gpt-4.1", Provider: core.ProviderOpenAI}}
}

func (a *failoverAuditAdapter) Invoke(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
	if decision.Account.ID == "acct_primary" {
		return nil, &providers.InvokeError{
			Code:      "upstream_temporarily_unavailable",
			Temporary: true,
			Cooldown:  45 * time.Second,
			Err:       errors.New("primary unavailable"),
		}
	}
	return &core.GatewayResponse{
		ID:           "resp_backup",
		Model:        decision.Model,
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Content:      "backup reply",
		FinishReason: "stop",
		CreatedAt:    time.Now().UTC(),
	}, nil
}

type failingAdapter struct{}

func (a *failingAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *failingAdapter) DisplayName() string { return "Failing" }

func (a *failingAdapter) ListModels(context.Context) []core.ModelSpec {
	return []core.ModelSpec{{Name: "gpt-4.1", Provider: core.ProviderOpenAI}}
}

func (a *failingAdapter) Invoke(_ context.Context, _ core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
	return nil, &providers.InvokeError{
		Code:      "upstream_rejected",
		Temporary: false,
		Err:       errors.New("request rejected"),
	}
}

type assertingAdapter struct {
	t             *testing.T
	wantModel     string
	streamContent string
}

func (a *assertingAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *assertingAdapter) DisplayName() string { return "Asserting" }

func (a *assertingAdapter) ListModels(context.Context) []core.ModelSpec {
	return []core.ModelSpec{{Name: a.wantModel, Provider: core.ProviderOpenAI}}
}

func (a *assertingAdapter) Invoke(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
	if decision.Model != a.wantModel {
		a.t.Fatalf("decision.Model = %q, want %q", decision.Model, a.wantModel)
	}
	return &core.GatewayResponse{
		ID:           "resp_assert",
		Model:        decision.Model,
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Content:      "ok",
		FinishReason: "stop",
		CreatedAt:    time.Now().UTC(),
	}, nil
}

func (a *assertingAdapter) OpenStream(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*providers.StreamSession, error) {
	if decision.Model != a.wantModel {
		a.t.Fatalf("decision.Model = %q, want %q", decision.Model, a.wantModel)
	}
	return &providers.StreamSession{
		Response: &core.GatewayResponse{
			ID:           "resp_assert_stream",
			Model:        decision.Model,
			Provider:     decision.Provider,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			CreatedAt:    time.Now().UTC(),
		},
		Stream: &testStream{
			events: []*core.StreamEvent{
				{Delta: a.streamContent},
				{FinishReason: "stop", Done: true},
			},
		},
	}, nil
}

type streamFailureAdapter struct{}

func (a *streamFailureAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *streamFailureAdapter) DisplayName() string { return "Stream Failure" }

func (a *streamFailureAdapter) ListModels(context.Context) []core.ModelSpec {
	return []core.ModelSpec{{Name: "gpt-4.1", Provider: core.ProviderOpenAI}}
}

func (a *streamFailureAdapter) Invoke(context.Context, core.RouteDecision, *core.GatewayRequest) (*core.GatewayResponse, error) {
	return nil, errors.New("not used")
}

func (a *streamFailureAdapter) OpenStream(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*providers.StreamSession, error) {
	return &providers.StreamSession{
		Response: &core.GatewayResponse{
			ID:           "resp_stream_failure",
			Model:        decision.Model,
			Provider:     decision.Provider,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			CreatedAt:    time.Now().UTC(),
		},
		Stream: &testStream{
			events: []*core.StreamEvent{{Delta: "partial"}},
			err: &providers.InvokeError{
				Code:      "upstream_read_error",
				Temporary: true,
				Cooldown:  45 * time.Second,
				Err:       errors.New("stream broke"),
			},
		},
	}, nil
}

type preOutputRetryStreamAdapter struct {
	echoAdapter
	seen []string
}

func (a *preOutputRetryStreamAdapter) OpenStream(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*providers.StreamSession, error) {
	a.seen = append(a.seen, decision.Account.ID)
	if decision.Account.ID == "acct_low_cost" {
		return &providers.StreamSession{
			Response: &core.GatewayResponse{
				ID:           "resp_low_cost",
				Model:        decision.Model,
				Provider:     decision.Provider,
				AccountID:    decision.Account.ID,
				AccountLabel: decision.Account.Label,
				CreatedAt:    time.Now().UTC(),
			},
			Stream: &testStream{
				events: []*core.StreamEvent{
					{RawEvent: "response.created", RawData: []byte(`{"type":"response.created"}`)},
				},
				err: &providers.InvokeError{
					Code:      "upstream_read_error",
					Temporary: true,
					Cooldown:  45 * time.Second,
					Err:       errors.New("stream broke before output"),
				},
			},
		}, nil
	}
	return &providers.StreamSession{
		Response: &core.GatewayResponse{
			ID:           "resp_fallback",
			Model:        decision.Model,
			Provider:     decision.Provider,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			CreatedAt:    time.Now().UTC(),
		},
		Stream: &testStream{
			events: []*core.StreamEvent{
				{Delta: "fallback ok"},
				{FinishReason: "stop", Usage: &core.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}, Done: true},
			},
		},
	}, nil
}

type preOutputHTMLStreamAdapter struct {
	echoAdapter
	seen []string
}

func (a *preOutputHTMLStreamAdapter) OpenStream(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*providers.StreamSession, error) {
	a.seen = append(a.seen, decision.Account.ID)
	if decision.Account.ID == "acct_low_cost" {
		return &providers.StreamSession{
			Response: &core.GatewayResponse{
				ID:           "resp_low_cost_html",
				Model:        decision.Model,
				Provider:     decision.Provider,
				AccountID:    decision.Account.ID,
				AccountLabel: decision.Account.Label,
				CreatedAt:    time.Now().UTC(),
			},
			Stream: &testStream{
				events: []*core.StreamEvent{
					{RawData: []byte(`<!doctype html><html><head><title>Cloudflare challenge</title></head><body>Just a moment</body></html>`)},
				},
			},
		}, nil
	}
	return &providers.StreamSession{
		Response: &core.GatewayResponse{
			ID:           "resp_fallback_html",
			Model:        decision.Model,
			Provider:     decision.Provider,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			CreatedAt:    time.Now().UTC(),
		},
		Stream: &testStream{
			events: []*core.StreamEvent{
				{Delta: "fallback ok"},
				{FinishReason: "stop", Usage: &core.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}, Done: true},
			},
		},
	}, nil
}

type preOutputInvalidRawStreamAdapter struct {
	echoAdapter
	seen []string
}

func (a *preOutputInvalidRawStreamAdapter) OpenStream(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*providers.StreamSession, error) {
	a.seen = append(a.seen, decision.Account.ID)
	if decision.Account.ID == "acct_low_cost" {
		return &providers.StreamSession{
			Response: &core.GatewayResponse{
				ID:           "resp_low_cost_invalid_raw",
				Model:        decision.Model,
				Provider:     decision.Provider,
				AccountID:    decision.Account.ID,
				AccountLabel: decision.Account.Label,
				CreatedAt:    time.Now().UTC(),
			},
			Stream: &testStream{
				events: []*core.StreamEvent{
					{RawData: []byte(`this is not an sse json chunk`)},
				},
			},
		}, nil
	}
	return &providers.StreamSession{
		Response: &core.GatewayResponse{
			ID:           "resp_fallback_invalid_raw",
			Model:        decision.Model,
			Provider:     decision.Provider,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			CreatedAt:    time.Now().UTC(),
		},
		Stream: &testStream{
			events: []*core.StreamEvent{
				{Delta: "fallback ok"},
				{FinishReason: "stop", Usage: &core.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}, Done: true},
			},
		},
	}, nil
}

type preOutputRateLimitedStreamAdapter struct {
	echoAdapter
	seen []string
}

func (a *preOutputRateLimitedStreamAdapter) OpenStream(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*providers.StreamSession, error) {
	a.seen = append(a.seen, decision.Account.ID)
	if decision.Account.ID == "acct_low_cost" {
		return &providers.StreamSession{
			Response: &core.GatewayResponse{
				ID:           "resp_low_cost_rate_limited",
				Model:        decision.Model,
				Provider:     decision.Provider,
				AccountID:    decision.Account.ID,
				AccountLabel: decision.Account.Label,
				CreatedAt:    time.Now().UTC(),
			},
			Stream: &testStream{
				events: []*core.StreamEvent{
					{RawData: []byte(`{"error":{"type":"rate_limit_exceeded","message":"rate limit exceeded"}}`)},
				},
			},
		}, nil
	}
	return &providers.StreamSession{
		Response: &core.GatewayResponse{
			ID:           "resp_fallback_rate_limited",
			Model:        decision.Model,
			Provider:     decision.Provider,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			CreatedAt:    time.Now().UTC(),
		},
		Stream: &testStream{
			events: []*core.StreamEvent{
				{Delta: "fallback ok"},
				{FinishReason: "stop", Usage: &core.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}, Done: true},
			},
		},
	}, nil
}

func TestExecuteStreamRetriesBeforeFirstOutputOnAnotherAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_stream_retry", Username: "stream-retry", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_stream_retry", Name: "Stream Retry", APIKey: "gw_stream_retry", OwnerUserID: "user_stream_retry", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                  "stream-retry-model",
		Provider:            core.ProviderOpenAI,
		UpstreamID:          "stream-retry-model",
		Enabled:             true,
		VisibleGroups:       []string{core.DefaultAccountGroupName},
		Source:              core.ModelSourceManual,
		BillingMode:         core.ModelBillingModeRequest,
		RequestPriceNanoUSD: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{
			ID:       "acct_low_cost",
			Provider: core.ProviderOpenAI,
			Label:    "Low Cost",
			Status:   core.AccountStatusActive,
			Priority: 200,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "low-token",
			},
		},
		{
			ID:       "acct_fallback",
			Provider: core.ProviderOpenAI,
			Label:    "Fallback",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "fallback-token",
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	adapter := &preOutputRetryStreamAdapter{}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter)),
	)
	var content strings.Builder
	err := service.ExecuteStream(context.Background(), &core.GatewayRequest{
		Model:    "stream-retry-model",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Client:   &client,
		Metadata: map[string]string{
			"route_affinity_key":   "session-id:sess_stream_retry",
			"route_affinity_model": "stream-retry-model",
		},
	}, func(_ *core.GatewayResponse, event *core.StreamEvent) error {
		if event != nil {
			content.WriteString(event.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}
	if got, want := adapter.seen, []string{"acct_low_cost", "acct_low_cost", "acct_fallback"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("seen accounts = %#v, want %#v", got, want)
	}
	if content.String() != "fallback ok" {
		t.Fatalf("content = %q, want fallback ok", content.String())
	}
	lowCost, err := repo.GetAccount("acct_low_cost")
	if err != nil {
		t.Fatal(err)
	}
	if lowCost.Status != core.AccountStatusActive || lowCost.CooldownUntil != nil {
		t.Fatalf("low cost status=%q cooldown=%#v, want active without cooldown after shared upstream failure", lowCost.Status, lowCost.CooldownUntil)
	}
	requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: client.ID, Limit: 1})
	if total != 1 || len(requests) != 1 {
		t.Fatalf("billing requests = total %d rows %#v, want one", total, requests)
	}
	if requests[0].Status != core.BillingRequestSettled || requests[0].AccountID != "acct_fallback" {
		t.Fatalf("billing request = %#v, want settled fallback account", requests[0])
	}
	audits := repo.ListAudit(1)
	if len(audits) != 1 || audits[0].Status != "ok" {
		t.Fatalf("audit = %#v, want one ok audit", audits)
	}
	if len(audits[0].Attempts) != 5 ||
		audits[0].Attempts[1].Status != "stream_error" ||
		audits[0].Attempts[1].AccountID != "acct_low_cost" ||
		audits[0].Attempts[2].Status != "ok" ||
		audits[0].Attempts[2].AccountID != "acct_low_cost" ||
		audits[0].Attempts[3].Status != "stream_error" ||
		audits[0].Attempts[3].AccountID != "acct_low_cost" ||
		audits[0].Attempts[4].Status != "ok" ||
		audits[0].Attempts[4].AccountID != "acct_fallback" {
		t.Fatalf("attempts = %#v, want one low cost retry then fallback ok", audits[0].Attempts)
	}
}

func TestExecuteStreamRateLimitBeforeOutputSkipsSameAccountRetry(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_stream_rate_retry", Username: "stream-rate-retry", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_stream_rate_retry", Name: "Stream Rate Retry", APIKey: "gw_stream_rate_retry", OwnerUserID: "user_stream_rate_retry", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                  "stream-rate-retry-model",
		Provider:            core.ProviderOpenAI,
		UpstreamID:          "stream-rate-retry-model",
		Enabled:             true,
		VisibleGroups:       []string{core.DefaultAccountGroupName},
		Source:              core.ModelSourceManual,
		BillingMode:         core.ModelBillingModeRequest,
		RequestPriceNanoUSD: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{ID: "acct_low_cost", Provider: core.ProviderOpenAI, Label: "Low Cost", Status: core.AccountStatusActive, Priority: 200, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "low-token"}},
		{ID: "acct_fallback", Provider: core.ProviderOpenAI, Label: "Fallback", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "fallback-token"}},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	adapter := &preOutputRateLimitedStreamAdapter{}
	service := New(repo, routing.NewRouter(), failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter)))
	var content strings.Builder
	err := service.ExecuteStream(context.Background(), &core.GatewayRequest{
		Model:    "stream-rate-retry-model",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Client:   &client,
		Metadata: map[string]string{
			"route_affinity_key":   "session-id:sess_stream_rate_retry",
			"route_affinity_model": "stream-rate-retry-model",
		},
	}, func(_ *core.GatewayResponse, event *core.StreamEvent) error {
		if event != nil {
			content.WriteString(event.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}
	if got, want := adapter.seen, []string{"acct_low_cost", "acct_fallback"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("seen accounts = %#v, want %#v", got, want)
	}
	if content.String() != "fallback ok" {
		t.Fatalf("content = %q, want fallback ok", content.String())
	}
	audits := repo.ListAudit(1)
	if len(audits) != 1 || audits[0].Status != "ok" {
		t.Fatalf("audit = %#v, want one ok audit", audits)
	}
	if len(audits[0].Attempts) != 3 ||
		audits[0].Attempts[1].ErrorCode != "upstream_rate_limited" ||
		audits[0].Attempts[2].AccountID != "acct_fallback" {
		t.Fatalf("attempts = %#v, want rate limit then fallback ok", audits[0].Attempts)
	}
}

func TestExecuteStreamRetriesPreOutputInvalidRawChunk(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_stream_invalid_retry", Username: "stream-invalid-retry", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_stream_invalid_retry", Name: "Stream Invalid Retry", APIKey: "gw_stream_invalid_retry", OwnerUserID: "user_stream_invalid_retry", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                  "stream-invalid-retry-model",
		Provider:            core.ProviderOpenAI,
		UpstreamID:          "stream-invalid-retry-model",
		Enabled:             true,
		VisibleGroups:       []string{core.DefaultAccountGroupName},
		Source:              core.ModelSourceManual,
		BillingMode:         core.ModelBillingModeRequest,
		RequestPriceNanoUSD: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{ID: "acct_low_cost", Provider: core.ProviderOpenAI, Label: "Low Cost", Status: core.AccountStatusActive, Priority: 200, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "low-token"}},
		{ID: "acct_fallback", Provider: core.ProviderOpenAI, Label: "Fallback", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "fallback-token"}},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	adapter := &preOutputInvalidRawStreamAdapter{}
	service := New(repo, routing.NewRouter(), failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter)))
	var emitted strings.Builder
	err := service.ExecuteStream(context.Background(), &core.GatewayRequest{
		Model:    "stream-invalid-retry-model",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Client:   &client,
		Metadata: map[string]string{
			"route_affinity_key":   "session-id:sess_stream_invalid_retry",
			"route_affinity_model": "stream-invalid-retry-model",
		},
	}, func(_ *core.GatewayResponse, event *core.StreamEvent) error {
		if event != nil {
			emitted.WriteString(event.Delta)
			emitted.Write(event.RawData)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}
	if got, want := adapter.seen, []string{"acct_low_cost", "acct_low_cost", "acct_fallback"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("seen accounts = %#v, want %#v", got, want)
	}
	if emitted.String() != "fallback ok" {
		t.Fatalf("emitted = %q, want fallback ok", emitted.String())
	}
	audits := repo.ListAudit(1)
	if len(audits) != 1 || audits[0].Status != "ok" {
		t.Fatalf("audit = %#v, want one ok audit", audits)
	}
	if len(audits[0].Attempts) != 5 ||
		audits[0].Attempts[1].ErrorCode != "upstream_invalid_stream_chunk" ||
		audits[0].Attempts[3].ErrorCode != "upstream_invalid_stream_chunk" ||
		audits[0].Attempts[4].AccountID != "acct_fallback" {
		t.Fatalf("attempts = %#v, want invalid raw errors then fallback ok", audits[0].Attempts)
	}
}

func TestExecuteStreamDoesNotCommitPreOutputHTMLChallenge(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_stream_html_retry", Username: "stream-html-retry", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_stream_html_retry", Name: "Stream HTML Retry", APIKey: "gw_stream_html_retry", OwnerUserID: "user_stream_html_retry", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                  "stream-html-retry-model",
		Provider:            core.ProviderOpenAI,
		UpstreamID:          "stream-html-retry-model",
		Enabled:             true,
		VisibleGroups:       []string{core.DefaultAccountGroupName},
		Source:              core.ModelSourceManual,
		BillingMode:         core.ModelBillingModeRequest,
		RequestPriceNanoUSD: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{ID: "acct_low_cost", Provider: core.ProviderOpenAI, Label: "Low Cost", Status: core.AccountStatusActive, Priority: 200, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "low-token"}},
		{ID: "acct_fallback", Provider: core.ProviderOpenAI, Label: "Fallback", Status: core.AccountStatusActive, Priority: 100, Weight: 100, Credential: core.Credential{Mode: "manual-token", AccessToken: "fallback-token"}},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	adapter := &preOutputHTMLStreamAdapter{}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter)),
	)
	var emitted strings.Builder
	err := service.ExecuteStream(context.Background(), &core.GatewayRequest{
		Model:    "stream-html-retry-model",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Client:   &client,
		Metadata: map[string]string{
			"route_affinity_key":   "session-id:sess_stream_html_retry",
			"route_affinity_model": "stream-html-retry-model",
		},
	}, func(_ *core.GatewayResponse, event *core.StreamEvent) error {
		if event != nil {
			emitted.WriteString(event.Delta)
			emitted.Write(event.RawData)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}
	if got, want := adapter.seen, []string{"acct_low_cost", "acct_low_cost", "acct_fallback"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("seen accounts = %#v, want %#v", got, want)
	}
	if strings.Contains(strings.ToLower(emitted.String()), "<html") {
		t.Fatalf("emitted HTML challenge: %q", emitted.String())
	}
	if emitted.String() != "fallback ok" {
		t.Fatalf("emitted = %q, want fallback ok", emitted.String())
	}
	audits := repo.ListAudit(1)
	if len(audits) != 1 || audits[0].Status != "ok" {
		t.Fatalf("audit = %#v, want one ok audit", audits)
	}
	if len(audits[0].Attempts) != 5 ||
		audits[0].Attempts[1].ErrorCode != "upstream_server_error" ||
		audits[0].Attempts[3].ErrorCode != "upstream_server_error" ||
		audits[0].Attempts[4].AccountID != "acct_fallback" {
		t.Fatalf("attempts = %#v, want HTML errors then fallback ok", audits[0].Attempts)
	}
}

type incompleteResponsesStreamAdapter struct {
	echoAdapter
}

func (a *incompleteResponsesStreamAdapter) OpenResponsesStream(_ context.Context, decision core.RouteDecision, _ *core.ResponsesRequest) (*providers.StreamSession, error) {
	return &providers.StreamSession{
		Response: &core.GatewayResponse{
			ID:           "resp_incomplete_responses",
			Model:        decision.Model,
			Provider:     decision.Provider,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			CreatedAt:    time.Now().UTC(),
		},
		Stream: &testStream{
			events: []*core.StreamEvent{{Delta: "partial response"}},
		},
	}, nil
}

type preOutputRetryResponsesStreamAdapter struct {
	echoAdapter
	seen []string
}

func (a *preOutputRetryResponsesStreamAdapter) OpenResponsesStream(_ context.Context, decision core.RouteDecision, _ *core.ResponsesRequest) (*providers.StreamSession, error) {
	a.seen = append(a.seen, decision.Account.ID)
	if len(a.seen) == 1 {
		return &providers.StreamSession{
			Response: &core.GatewayResponse{
				ID:           "resp_low_cost_responses",
				Model:        decision.Model,
				Provider:     decision.Provider,
				AccountID:    decision.Account.ID,
				AccountLabel: decision.Account.Label,
				CreatedAt:    time.Now().UTC(),
			},
			Stream: &testStream{
				events: []*core.StreamEvent{
					{RawEvent: "response.created", RawData: []byte(`{"type":"response.created"}`)},
				},
				err: &providers.InvokeError{
					Code:      "upstream_read_error",
					Temporary: true,
					Cooldown:  45 * time.Second,
					Err:       errors.New("stream broke before output"),
				},
			},
		}, nil
	}
	return &providers.StreamSession{
		Response: &core.GatewayResponse{
			ID:           "resp_fallback_responses",
			Model:        decision.Model,
			Provider:     decision.Provider,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			CreatedAt:    time.Now().UTC(),
		},
		Stream: &testStream{
			events: []*core.StreamEvent{
				{Delta: "fallback response"},
				{FinishReason: "stop", Done: true, Usage: &core.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}},
			},
		},
	}, nil
}

type preOutputResponsesWebSocketSendRetryAdapter struct {
	echoAdapter
	seen []string
}

func (a *preOutputResponsesWebSocketSendRetryAdapter) OpenResponsesWebSocketSession(_ context.Context, decision core.RouteDecision, _ *core.ResponsesRequest) (providers.ResponsesWebSocketSession, error) {
	a.seen = append(a.seen, decision.Account.ID)
	return &preOutputResponsesWebSocketSendRetrySession{
		provider: decision.Provider,
		account:  decision.Account,
		model:    decision.Model,
		failSend: decision.Account.ID == "acct_low_cost",
	}, nil
}

type preOutputResponsesWebSocketSendRetrySession struct {
	provider core.ProviderKind
	account  core.Account
	model    string
	failSend bool
}

func (s *preOutputResponsesWebSocketSendRetrySession) SendRequest(context.Context, string, *core.ResponsesRequest) (*providers.StreamSession, error) {
	if s.failSend {
		return nil, io.EOF
	}
	return &providers.StreamSession{
		Response: &core.GatewayResponse{
			ID:           "resp_ws_fallback",
			Model:        s.model,
			Provider:     s.provider,
			AccountID:    s.account.ID,
			AccountLabel: s.account.Label,
			CreatedAt:    time.Now().UTC(),
		},
		Stream: &testStream{
			events: []*core.StreamEvent{
				{Delta: "fallback ws response"},
				{FinishReason: "stop", Done: true, Usage: &core.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}, RawEvent: "response.completed"},
			},
		},
	}, nil
}

func (s *preOutputResponsesWebSocketSendRetrySession) SendRaw(context.Context, []byte) error {
	return nil
}

func (s *preOutputResponsesWebSocketSendRetrySession) Close() error {
	return nil
}

type preOutputFailedResponsesStreamAdapter struct {
	echoAdapter
	seen []string
}

func (a *preOutputFailedResponsesStreamAdapter) OpenResponsesStream(_ context.Context, decision core.RouteDecision, _ *core.ResponsesRequest) (*providers.StreamSession, error) {
	a.seen = append(a.seen, decision.Account.ID)
	if decision.Account.ID == "acct_low_cost" {
		return &providers.StreamSession{
			Response: &core.GatewayResponse{
				ID:           "resp_failed_low_cost",
				Model:        decision.Model,
				Provider:     decision.Provider,
				AccountID:    decision.Account.ID,
				AccountLabel: decision.Account.Label,
				CreatedAt:    time.Now().UTC(),
			},
			Stream: &testStream{
				events: []*core.StreamEvent{
					{
						RawEvent: "response.created",
						RawData:  []byte(`{"type":"response.created","response":{"id":"resp_failed_low_cost","status":"in_progress"}}`),
					},
					{
						RawEvent:     "response.failed",
						RawData:      []byte(`{"type":"response.failed","response":{"id":"resp_failed_low_cost","status":"failed","error":{"type":"server_error","message":"upstream service temporarily unavailable"}}}`),
						FinishReason: "failed",
						Done:         true,
					},
				},
			},
		}, nil
	}
	return &providers.StreamSession{
		Response: &core.GatewayResponse{
			ID:           "resp_failed_fallback",
			Model:        decision.Model,
			Provider:     decision.Provider,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			CreatedAt:    time.Now().UTC(),
		},
		Stream: &testStream{
			events: []*core.StreamEvent{
				{Delta: "fallback after failed event"},
				{FinishReason: "stop", Done: true, Usage: &core.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}, RawEvent: "response.completed"},
			},
		},
	}, nil
}

func TestExecuteResponsesStreamRetriesPreOutputFailedEventOnAnotherAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_responses_failed_retry", Username: "responses-failed-retry", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_responses_failed_retry", Name: "Responses Failed Retry", APIKey: "gw_responses_failed_retry", OwnerUserID: "user_responses_failed_retry", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "responses-failed-retry-model",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "responses-failed-retry-model",
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: 2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{
			ID:       "acct_low_cost",
			Provider: core.ProviderOpenAI,
			Label:    "Low Cost",
			Status:   core.AccountStatusActive,
			Priority: 200,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "low-token",
			},
		},
		{
			ID:       "acct_fallback",
			Provider: core.ProviderOpenAI,
			Label:    "Fallback",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "fallback-token",
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	adapter := &preOutputFailedResponsesStreamAdapter{}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter)),
	)
	emitted := 0
	var content strings.Builder
	err := service.ExecuteResponsesStream(context.Background(), &core.ResponsesRequest{
		Model:   "responses-failed-retry-model",
		RawBody: json.RawMessage(`{"model":"responses-failed-retry-model","stream":true,"input":"hello"}`),
		Client:  &client,
		Stream:  true,
		Metadata: map[string]string{
			"route_affinity_key":   "session-id:sess_responses_failed_retry",
			"route_affinity_model": "responses-failed-retry-model",
		},
	}, func(_ *core.GatewayResponse, event *core.StreamEvent) error {
		emitted++
		if event != nil {
			content.WriteString(event.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ExecuteResponsesStream returned error: %v", err)
	}
	if got, want := adapter.seen, []string{"acct_low_cost", "acct_low_cost", "acct_fallback"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("seen accounts = %#v, want %#v", got, want)
	}
	if content.String() != "fallback after failed event" {
		t.Fatalf("content = %q, want fallback after failed event", content.String())
	}
	if emitted != 2 {
		t.Fatalf("emitted events = %d, want only fallback delta and terminal", emitted)
	}
	lowCost, err := repo.GetAccount("acct_low_cost")
	if err != nil {
		t.Fatal(err)
	}
	if lowCost.Status != core.AccountStatusActive || lowCost.CooldownUntil != nil {
		t.Fatalf("low cost status=%q cooldown=%#v, want active without cooldown after shared upstream failure", lowCost.Status, lowCost.CooldownUntil)
	}
	audits := repo.ListAudit(1)
	if len(audits) != 1 || audits[0].Status != "ok" {
		t.Fatalf("audit = %#v, want one ok audit", audits)
	}
	if len(audits[0].Attempts) != 5 ||
		audits[0].Attempts[1].Status != "stream_error" ||
		audits[0].Attempts[1].ErrorCode != "upstream_temporarily_unavailable" ||
		audits[0].Attempts[1].AccountID != "acct_low_cost" ||
		audits[0].Attempts[2].Status != "ok" ||
		audits[0].Attempts[2].AccountID != "acct_low_cost" ||
		audits[0].Attempts[3].Status != "stream_error" ||
		audits[0].Attempts[3].ErrorCode != "upstream_temporarily_unavailable" ||
		audits[0].Attempts[3].AccountID != "acct_low_cost" ||
		audits[0].Attempts[4].Status != "ok" ||
		audits[0].Attempts[4].AccountID != "acct_fallback" {
		t.Fatalf("attempts = %#v, want one low cost failed-event retry then fallback ok", audits[0].Attempts)
	}
}

func TestExecuteResponsesStreamRetriesBeforeFirstOutputOnAnotherAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_responses_retry", Username: "responses-retry", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_responses_retry", Name: "Responses Retry", APIKey: "gw_responses_retry", OwnerUserID: "user_responses_retry", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "responses-retry-model",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "responses-retry-model",
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: 2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{
			ID:       "acct_low_cost",
			Provider: core.ProviderOpenAI,
			Label:    "Low Cost",
			Status:   core.AccountStatusActive,
			Priority: 200,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "low-token",
			},
		},
		{
			ID:       "acct_fallback",
			Provider: core.ProviderOpenAI,
			Label:    "Fallback",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "fallback-token",
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	adapter := &preOutputRetryResponsesStreamAdapter{}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter)),
	)
	var content strings.Builder
	err := service.ExecuteResponsesStream(context.Background(), &core.ResponsesRequest{
		Model:   "responses-retry-model",
		RawBody: json.RawMessage(`{"model":"responses-retry-model","stream":true,"input":"hello"}`),
		Client:  &client,
		Stream:  true,
		Metadata: map[string]string{
			"route_affinity_key":   "session-id:sess_responses_retry",
			"route_affinity_model": "responses-retry-model",
		},
	}, func(_ *core.GatewayResponse, event *core.StreamEvent) error {
		if event != nil {
			content.WriteString(event.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ExecuteResponsesStream returned error: %v", err)
	}
	if got, want := adapter.seen, []string{"acct_low_cost", "acct_low_cost"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("seen accounts = %#v, want %#v", got, want)
	}
	if content.String() != "fallback response" {
		t.Fatalf("content = %q, want fallback response", content.String())
	}
	lowCost, err := repo.GetAccount("acct_low_cost")
	if err != nil {
		t.Fatal(err)
	}
	if lowCost.Status != core.AccountStatusActive || lowCost.CooldownUntil != nil {
		t.Fatalf("low cost status=%q cooldown=%#v, want active without cooldown after same-account recovery", lowCost.Status, lowCost.CooldownUntil)
	}
	audits := repo.ListAudit(1)
	if len(audits) != 1 || audits[0].Status != "ok" {
		t.Fatalf("audit = %#v, want one ok audit", audits)
	}
	if len(audits[0].Attempts) != 3 ||
		audits[0].Attempts[1].Status != "stream_error" ||
		audits[0].Attempts[1].AccountID != "acct_low_cost" ||
		audits[0].Attempts[2].Status != "ok" ||
		audits[0].Attempts[2].AccountID != "acct_low_cost" {
		t.Fatalf("attempts = %#v, want low cost stream_error then same-account recovery", audits[0].Attempts)
	}
}

func TestExecuteResponsesWebSocketRetriesBeforeSendOnAnotherAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_responses_ws_retry", Username: "responses-ws-retry", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_responses_ws_retry", Name: "Responses WS Retry", APIKey: "gw_responses_ws_retry", OwnerUserID: "user_responses_ws_retry", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "responses-ws-retry-model",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "responses-ws-retry-model",
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: 2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{
			ID:       "acct_low_cost",
			Provider: core.ProviderOpenAI,
			Label:    "Low Cost",
			Status:   core.AccountStatusActive,
			Priority: 200,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "low-token",
			},
		},
		{
			ID:       "acct_fallback",
			Provider: core.ProviderOpenAI,
			Label:    "Fallback",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "fallback-token",
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	adapter := &preOutputResponsesWebSocketSendRetryAdapter{}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter)),
	)
	var content strings.Builder
	session := service.NewResponsesWebSocketSession(nil, &client)
	err := session.Execute(context.Background(), &core.ResponsesRequest{
		Model:   "responses-ws-retry-model",
		RawBody: json.RawMessage(`{"model":"responses-ws-retry-model","stream":true,"input":"hello"}`),
		Client:  &client,
		Stream:  true,
	}, func(_ *core.GatewayResponse, event *core.StreamEvent) error {
		if event != nil {
			content.WriteString(event.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Responses websocket Execute returned error: %v", err)
	}
	if got, want := adapter.seen, []string{"acct_low_cost", "acct_fallback"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("seen accounts = %#v, want %#v", got, want)
	}
	if content.String() != "fallback ws response" {
		t.Fatalf("content = %q, want fallback ws response", content.String())
	}
	lowCost, err := repo.GetAccount("acct_low_cost")
	if err != nil {
		t.Fatal(err)
	}
	if lowCost.Status != core.AccountStatusActive || lowCost.CooldownUntil != nil {
		t.Fatalf("low cost status=%q cooldown=%#v, want active without cooldown after shared upstream failure", lowCost.Status, lowCost.CooldownUntil)
	}
	audits := repo.ListAudit(1)
	if len(audits) != 1 || audits[0].Status != "ok" {
		t.Fatalf("audit = %#v, want one ok audit", audits)
	}
	if len(audits[0].Attempts) != 3 ||
		audits[0].Attempts[1].Status != "invoke_error" ||
		audits[0].Attempts[1].AccountID != "acct_low_cost" ||
		audits[0].Attempts[2].Status != "ok" ||
		audits[0].Attempts[2].AccountID != "acct_fallback" {
		t.Fatalf("attempts = %#v, want low cost invoke_error then fallback ok", audits[0].Attempts)
	}
}

func TestExecuteResponsesStreamRetriesPreviousResponseBindingBeforeFirstOutput(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_responses_binding_retry", Username: "responses-binding-retry", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_responses_binding_retry", Name: "Responses Binding Retry", APIKey: "gw_responses_binding_retry", OwnerUserID: "user_responses_binding_retry", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "responses-binding-retry-model",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "responses-binding-retry-model",
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: 2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{
			ID:       "acct_bound_bad",
			Provider: core.ProviderOpenAI,
			Label:    "Bound Bad",
			Status:   core.AccountStatusActive,
			Priority: 200,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "bad-token",
			},
		},
		{
			ID:       "acct_bound_good",
			Provider: core.ProviderOpenAI,
			Label:    "Bound Good",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "good-token",
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{
		ResponseID: "resp_previous_retry",
		AccountID:  "acct_bound_bad",
		ClientID:   client.ID,
	}); err != nil {
		t.Fatal(err)
	}

	adapter := &preOutputRetryResponsesStreamAdapter{}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter)),
	)
	var content strings.Builder
	err := service.ExecuteResponsesStream(context.Background(), &core.ResponsesRequest{
		Model:              "responses-binding-retry-model",
		RawBody:            json.RawMessage(`{"model":"responses-binding-retry-model","stream":true,"input":"hello","previous_response_id":"resp_previous_retry"}`),
		Client:             &client,
		Stream:             true,
		PreviousResponseID: "resp_previous_retry",
	}, func(_ *core.GatewayResponse, event *core.StreamEvent) error {
		if event != nil {
			content.WriteString(event.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ExecuteResponsesStream returned error: %v", err)
	}
	if got, want := adapter.seen, []string{"acct_bound_bad", "acct_bound_good"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("seen accounts = %#v, want %#v", got, want)
	}
	if content.String() != "fallback response" {
		t.Fatalf("content = %q, want fallback response", content.String())
	}
	audits := repo.ListAudit(1)
	if len(audits) != 1 || audits[0].Status != "ok" {
		t.Fatalf("audit = %#v, want one ok audit", audits)
	}
	if len(audits[0].Attempts) != 3 ||
		audits[0].Attempts[1].AccountID != "acct_bound_bad" ||
		audits[0].Attempts[1].Status != "stream_error" ||
		audits[0].Attempts[2].AccountID != "acct_bound_good" ||
		audits[0].Attempts[2].Status != "ok" {
		t.Fatalf("attempts = %#v, want bound bad stream_error then good ok", audits[0].Attempts)
	}
}

func TestExecuteResponsesStreamReleasesWhenTerminalEventMissing(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_responses_stream", Username: "responses-stream", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_responses_stream", Name: "Responses Stream", APIKey: "gw_responses_stream", OwnerUserID: "user_responses_stream", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_responses_stream",
		Provider: core.ProviderOpenAI,
		Label:    "Responses Stream Account",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "token",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "responses-priced-model",
		Provider:                core.ProviderOpenAI,
		UpstreamID:              "responses-priced-model",
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		Source:                  core.ModelSourceManual,
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: 2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&incompleteResponsesStreamAdapter{})),
	)

	err := service.ExecuteResponsesStream(context.Background(), &core.ResponsesRequest{
		Model:   "responses-priced-model",
		RawBody: json.RawMessage(`{"model":"responses-priced-model","input":"hello"}`),
		Client:  &client,
		Stream:  true,
	}, func(*core.GatewayResponse, *core.StreamEvent) error {
		return nil
	})
	if providers.ErrorCode(err) != "upstream_read_error" {
		t.Fatalf("ExecuteResponsesStream err = %v, want upstream_read_error", err)
	}

	user, err := repo.GetUser("user_responses_stream")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != core.NanoUSDPerUSD {
		t.Fatalf("balance = %d, want unchanged %d", user.BalanceNanoUSD, core.NanoUSDPerUSD)
	}
	spend, err := repo.GetClientSpend("client_responses_stream")
	if err != nil {
		t.Fatal(err)
	}
	if spend.SpendUsedNanoUSD != 0 {
		t.Fatalf("spend = %d, want 0 after release", spend.SpendUsedNanoUSD)
	}
	requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: "client_responses_stream", Limit: 1})
	if total != 1 || len(requests) != 1 {
		t.Fatalf("billing requests = total %d rows %#v, want one", total, requests)
	}
	if requests[0].Status != core.BillingRequestReleased {
		t.Fatalf("billing status = %q, want released", requests[0].Status)
	}
	if _, err := repo.GetOpenAIResponseBinding("resp_incomplete_responses"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("response binding err = %v, want ErrNotFound", err)
	}
	audits := repo.ListAudit(1)
	if len(audits) != 1 || audits[0].Status != "error" {
		t.Fatalf("audit = %#v, want one error audit", audits)
	}
}

type failingImageStreamAdapter struct{}

func (a *failingImageStreamAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *failingImageStreamAdapter) DisplayName() string { return "Failing Image Stream" }

func (a *failingImageStreamAdapter) ListModels(context.Context) []core.ModelSpec {
	return []core.ModelSpec{{Name: "gpt-image-2", Provider: core.ProviderOpenAI}}
}

func (a *failingImageStreamAdapter) Invoke(context.Context, core.RouteDecision, *core.GatewayRequest) (*core.GatewayResponse, error) {
	return nil, errors.New("not used")
}

func (a *failingImageStreamAdapter) OpenImageGenerationStream(context.Context, core.RouteDecision, *core.ImageGenerationRequest) (*providers.StreamSession, error) {
	return nil, &providers.InvokeError{
		Code:      "image_generation_rejected",
		Temporary: false,
		Err:       errors.New("policy denied"),
	}
}

func TestStreamImageGenerationReturnsAttemptSummaryOnOpenFailure(t *testing.T) {
	repo := storage.NewMemoryRepository()
	seedGatewayModel(t, repo, "gpt-image-2", core.ProviderOpenAI)
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "primary-token",
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&failingImageStreamAdapter{})),
	)

	err := service.StreamImageGeneration(context.Background(), &core.ImageGenerationRequest{
		Model:  "gpt-image-2",
		Prompt: "blocked prompt",
	}, nil)
	if err == nil {
		t.Fatal("expected image stream error")
	}
	if !strings.Contains(err.Error(), "all upstream attempts failed:") ||
		!strings.Contains(err.Error(), "image_generation_rejected") ||
		!strings.Contains(err.Error(), "policy denied") {
		t.Fatalf("err = %v", err)
	}
	var executionErr *failover.ExecutionError
	if !errors.As(err, &executionErr) {
		t.Fatalf("err = %T %v, want wrapped ExecutionError", err, err)
	}
}

func TestClientForMetadataUsesPromptCacheKeyForRouteAffinity(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertClient(core.APIClient{
		ID:      "client_a",
		Name:    "Client A",
		APIKey:  "key_a",
		Enabled: true,
		RoutePolicy: core.RoutePolicy{
			DefaultProvider: core.ProviderOpenAI,
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&providers.OpenAIAdapter{})),
	)

	client, _, _ := service.clientForMetadata(map[string]string{
		"client_id":            "client_a",
		"route_affinity_model": "gpt-4.1",
		"prompt_cache_key":     "cache-prefix-a",
	})
	if client == nil {
		t.Fatal("client is nil")
	}
	if client.RouteAffinityKey != "client_a\x00gpt-4.1\x00cache-prefix-a" {
		t.Fatalf("route affinity key = %q", client.RouteAffinityKey)
	}
	if !client.CacheAffinityRoute {
		t.Fatal("CacheAffinityRoute = false, want true")
	}
}

func TestClientForMetadataPrefersPromptCacheKeyOverRouteAffinityKey(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertClient(core.APIClient{
		ID:      "client_a",
		Name:    "Client A",
		APIKey:  "key_a",
		Enabled: true,
		RoutePolicy: core.RoutePolicy{
			DefaultProvider: core.ProviderOpenAI,
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&providers.OpenAIAdapter{})),
	)

	client, _, _ := service.clientForMetadata(map[string]string{
		"client_id":            "client_a",
		"route_affinity_model": "gpt-4.1",
		"prompt_cache_key":     "cache-prefix-a",
		"route_affinity_key":   "session-id:sess_123",
	})
	if client == nil {
		t.Fatal("client is nil")
	}
	if client.RouteAffinityKey != "client_a\x00gpt-4.1\x00cache-prefix-a" {
		t.Fatalf("route affinity key = %q", client.RouteAffinityKey)
	}
	if !client.CacheAffinityRoute {
		t.Fatal("CacheAffinityRoute = false, want true")
	}
}

func TestClientForMetadataUsesRouteAffinityKey(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertClient(core.APIClient{
		ID:      "client_a",
		Name:    "Client A",
		APIKey:  "key_a",
		Enabled: true,
		RoutePolicy: core.RoutePolicy{
			DefaultProvider: core.ProviderOpenAI,
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&providers.OpenAIAdapter{})),
	)

	client, _, _ := service.clientForMetadata(map[string]string{
		"client_id":            "client_a",
		"route_affinity_model": "gpt-5.5",
		"route_affinity_key":   "session-id:sess_123",
	})
	if client == nil {
		t.Fatal("client is nil")
	}
	if client.RouteAffinityKey != "client_a\x00gpt-5.5\x00session-id:sess_123" {
		t.Fatalf("route affinity key = %q", client.RouteAffinityKey)
	}
	if !client.CacheAffinityRoute {
		t.Fatal("CacheAffinityRoute = false, want true")
	}
}

func TestClientForRequestReusesClientPointerWithoutRouteAffinity(t *testing.T) {
	service := New(
		storage.NewMemoryRepository(),
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(storage.NewMemoryRepository()), providers.NewRegistry(&providers.OpenAIAdapter{})),
	)
	requestClient := &core.APIClient{
		ID:      "client_a",
		Name:    "Client A",
		Enabled: true,
		RoutePolicy: core.RoutePolicy{
			DefaultProvider: core.ProviderOpenAI,
		},
	}

	client, policy, err := service.clientForRequest(map[string]string{"client_id": "client_a"}, requestClient)
	if err != nil {
		t.Fatalf("clientForRequest returned error: %v", err)
	}
	if client != requestClient {
		t.Fatal("expected clientForRequest to reuse request client pointer when no route affinity is needed")
	}
	if policy.DefaultProvider != core.ProviderOpenAI {
		t.Fatalf("default provider = %q", policy.DefaultProvider)
	}
}

func TestExecuteAuditsFailoverAttemptsOnSuccessfulFallback(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_failover_billing", Username: "failover_billing", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	client := core.APIClient{ID: "client_failover_billing", Name: "Client A", APIKey: "gw_failover_billing", OwnerUserID: "user_failover_billing", Enabled: true}
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
		t.Fatalf("UpsertModel returned error: %v", err)
	}
	for _, account := range []core.Account{
		{
			ID:       "acct_primary",
			Provider: core.ProviderOpenAI,
			Label:    "Primary",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "primary-token",
			},
		},
		{
			ID:       "acct_backup",
			Provider: core.ProviderOpenAI,
			Label:    "Backup",
			Status:   core.AccountStatusActive,
			Priority: 90,
			Weight:   90,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "backup-token",
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&failoverAuditAdapter{})),
	)

	resp, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Client:   &client,
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if resp.AccountID != "acct_backup" {
		t.Fatalf("resp.AccountID = %q, want %q", resp.AccountID, "acct_backup")
	}

	audits := repo.ListAudit(1)
	if len(audits) != 1 {
		t.Fatalf("len(audits) = %d, want 1", len(audits))
	}
	if len(audits[0].Attempts) != 2 {
		t.Fatalf("len(audit attempts) = %d, want 2", len(audits[0].Attempts))
	}
	if audits[0].Attempts[0].AccountID != "acct_primary" || audits[0].Attempts[0].Status != "invoke_error" {
		t.Fatalf("unexpected first audit attempt: %#v", audits[0].Attempts[0])
	}
	if audits[0].Attempts[1].AccountID != "acct_backup" || audits[0].Attempts[1].Status != "ok" {
		t.Fatalf("unexpected second audit attempt: %#v", audits[0].Attempts[1])
	}
	requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: client.ID, Limit: 10})
	if total != 1 || len(requests) != 1 {
		t.Fatalf("billing requests = total %d items %#v, want one", total, requests)
	}
	if requests[0].AccountID != "acct_backup" || requests[0].AccountLabel != "Backup" {
		t.Fatalf("billing account = %q/%q, want acct_backup/Backup", requests[0].AccountID, requests[0].AccountLabel)
	}
	wantFailed := "Primary upstream_temporarily_unavailable: primary unavailable"
	if got := requests[0].FailedAccountLabels; len(got) != 1 || got[0] != wantFailed {
		t.Fatalf("failed account labels = %#v, want %s", got, wantFailed)
	}
}

func TestExecutePreservesRequestedModelWhileUsingMappedUpstreamModel(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertModel(core.ModelConfig{
		ID:         "gateway-model",
		Provider:   core.ProviderOpenAI,
		UpstreamID: "gpt-5.4",
		Enabled:    true,
		VisibleGroups: []string{
			core.DefaultAccountGroupName,
		},
		Source: core.ModelSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "primary-token",
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&echoAdapter{})),
	)

	resp, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:    "gateway-model",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if resp.Model != "gateway-model" {
		t.Fatalf("resp.Model = %q, want %q", resp.Model, "gateway-model")
	}

	audits := repo.ListAudit(1)
	if len(audits) != 1 {
		t.Fatalf("len(audits) = %d, want 1", len(audits))
	}
	if audits[0].Model != "gateway-model" {
		t.Fatalf("audit model = %q, want %q", audits[0].Model, "gateway-model")
	}
	if audits[0].Attempts[0].Status != "ok" {
		t.Fatalf("unexpected audit attempts: %#v", audits[0].Attempts)
	}
}

func TestExecuteErrorAuditPreservesRequestedModelWhenUpstreamModelDiffers(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertModel(core.ModelConfig{
		ID:         "gateway-model",
		Provider:   core.ProviderOpenAI,
		UpstreamID: "gpt-5.4",
		Enabled:    true,
		VisibleGroups: []string{
			core.DefaultAccountGroupName,
		},
		Source: core.ModelSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "primary-token",
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&failingAdapter{})),
	)

	_, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:    "gateway-model",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err == nil {
		t.Fatal("expected Execute to return error")
	}

	audits := repo.ListAudit(1)
	if len(audits) != 1 {
		t.Fatalf("len(audits) = %d, want 1", len(audits))
	}
	if audits[0].Model != "gateway-model" {
		t.Fatalf("audit model = %q, want %q", audits[0].Model, "gateway-model")
	}
	if len(audits[0].Attempts) != 1 || audits[0].Attempts[0].Status != "invoke_error" {
		t.Fatalf("unexpected audit attempts: %#v", audits[0].Attempts)
	}
}

func TestExecuteStreamPreservesRequestedModelWhenUpstreamModelDiffers(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertModel(core.ModelConfig{
		ID:         "gateway-model",
		Provider:   core.ProviderOpenAI,
		UpstreamID: "gpt-5.4",
		Enabled:    true,
		VisibleGroups: []string{
			core.DefaultAccountGroupName,
		},
		Source: core.ModelSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "primary-token",
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&echoAdapter{})),
	)

	var streamedModel string
	err := service.ExecuteStream(context.Background(), &core.GatewayRequest{
		Model:    "gateway-model",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	}, func(resp *core.GatewayResponse, _ *core.StreamEvent) error {
		if resp != nil {
			streamedModel = resp.Model
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}
	if streamedModel != "gateway-model" {
		t.Fatalf("streamed model = %q, want %q", streamedModel, "gateway-model")
	}

	audits := repo.ListAudit(1)
	if len(audits) != 1 {
		t.Fatalf("len(audits) = %d, want 1", len(audits))
	}
	if audits[0].Model != "gateway-model" {
		t.Fatalf("audit model = %q, want %q", audits[0].Model, "gateway-model")
	}
}

func TestExecuteStreamCapsCollectedAuditContent(t *testing.T) {
	repo := storage.NewMemoryRepository()
	seedGatewayModel(t, repo, "gpt-4.1", core.ProviderOpenAI)
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "primary-token",
		},
	}); err != nil {
		t.Fatal(err)
	}
	largeContent := strings.Repeat("x", maxStreamCollectedContentRunes+512)
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&assertingAdapter{
			t:             t,
			wantModel:     "gpt-4.1",
			streamContent: largeContent,
		})),
	)

	var streamed strings.Builder
	err := service.ExecuteStream(context.Background(), &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	}, func(_ *core.GatewayResponse, event *core.StreamEvent) error {
		if event != nil {
			streamed.WriteString(event.Delta)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}
	if streamed.String() != largeContent {
		t.Fatal("streamed content should not be capped")
	}
	audits := repo.ListAudit(1)
	if len(audits) != 1 {
		t.Fatalf("len(audits) = %d, want 1", len(audits))
	}
	if !strings.Contains(audits[0].Message, "[truncated]") {
		t.Fatalf("audit message should be capped, got len=%d", len([]rune(audits[0].Message)))
	}
}

func TestExecuteAuditOmitsRequestBody(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertModel(core.ModelConfig{
		ID:         "gateway-model",
		Provider:   core.ProviderOpenAI,
		UpstreamID: "gpt-5.4",
		Enabled:    true,
		VisibleGroups: []string{
			core.DefaultAccountGroupName,
		},
		Source: core.ModelSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "primary-token",
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&echoAdapter{})),
	)

	maxTokens := 64
	temperature := 0.3
	resp, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:       "gateway-model",
		Messages:    []core.Message{{Role: "user", Content: "hello"}},
		MaxTokens:   &maxTokens,
		Temperature: &temperature,
		Metadata:    map[string]string{"client_id": "client_a"},
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected response")
	}

	audits := repo.ListAudit(1)
	if len(audits) != 1 {
		t.Fatalf("len(audits) = %d, want 1", len(audits))
	}
	if audits[0].RequestBody != "" {
		t.Fatalf("audit request body = %q, want empty", audits[0].RequestBody)
	}
}

func TestExecuteResponsesAuditOmitsRequestBodyForOAuthAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertModel(core.ModelConfig{
		ID:         "gateway-model",
		Provider:   core.ProviderOpenAI,
		UpstreamID: "gpt-5.4",
		Enabled:    true,
		VisibleGroups: []string{
			core.DefaultAccountGroupName,
		},
		Source: core.ModelSourceManual,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        providers.OpenAIOAuthModeValue(),
			AccessToken: "oauth-token",
			Metadata: map[string]string{
				"token_source":     providers.OpenAIDeviceCodeTokenSourceValue(),
				"oauth_account_id": "acct_chatgpt",
				"codex_base_url":   "https://chatgpt.com/backend-api/codex",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&echoAdapter{})),
	)

	resp, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:    "gateway-model",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Metadata: map[string]string{"client_id": "client_a"},
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected response")
	}

	audits := repo.ListAudit(1)
	if len(audits) != 1 {
		t.Fatalf("len(audits) = %d, want 1", len(audits))
	}

	if audits[0].RequestBody != "" {
		t.Fatalf("audit request body = %q, want empty", audits[0].RequestBody)
	}
}

func TestExecuteStreamMarksAccountFailureWhenUpstreamReadFails(t *testing.T) {
	repo := storage.NewMemoryRepository()
	seedGatewayModel(t, repo, "gpt-4.1", core.ProviderOpenAI)
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "primary-token",
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&streamFailureAdapter{})),
	)

	err := service.ExecuteStream(context.Background(), &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	}, func(*core.GatewayResponse, *core.StreamEvent) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected stream error")
	}

	account, err := repo.GetAccount("acct_primary")
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != core.AccountStatusActive || account.CooldownUntil != nil {
		t.Fatalf("account status=%q cooldown=%#v, want active without cooldown after shared upstream failure", account.Status, account.CooldownUntil)
	}
	if account.ConsecutiveFails != 0 || account.TotalFails != 0 {
		t.Fatalf("failure counters = consecutive:%d total:%d, want no account penalty for shared upstream failure", account.ConsecutiveFails, account.TotalFails)
	}
}

type testStream struct {
	events []*core.StreamEvent
	index  int
	err    error
}

func (s *testStream) Next() (*core.StreamEvent, error) {
	if s.index >= len(s.events) {
		if s.err != nil {
			return nil, s.err
		}
		return nil, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *testStream) Close() error {
	return nil
}
