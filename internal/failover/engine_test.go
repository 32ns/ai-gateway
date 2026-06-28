package failover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/accounts"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

type scriptedAdapter struct {
	kind     core.ProviderKind
	invokeFn func(context.Context, core.RouteDecision, *core.GatewayRequest) (*core.GatewayResponse, error)
}

func (a *scriptedAdapter) Kind() core.ProviderKind { return a.kind }

func (a *scriptedAdapter) DisplayName() string { return string(a.kind) }

func (a *scriptedAdapter) ListModels(context.Context) []core.ModelSpec { return nil }

func (a *scriptedAdapter) Invoke(ctx context.Context, decision core.RouteDecision, req *core.GatewayRequest) (*core.GatewayResponse, error) {
	if a.invokeFn != nil {
		return a.invokeFn(ctx, decision, req)
	}
	return &core.GatewayResponse{
		ID:           "resp_" + decision.Account.ID,
		Model:        decision.Model,
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Content:      "ok",
		FinishReason: "stop",
		CreatedAt:    time.Now().UTC(),
	}, nil
}

type scriptedEmbeddingAdapter struct {
	*scriptedAdapter
	embedFn func(context.Context, core.RouteDecision, *core.EmbeddingRequest) (*core.EmbeddingResponse, error)
}

func (a *scriptedEmbeddingAdapter) Embed(ctx context.Context, decision core.RouteDecision, req *core.EmbeddingRequest) (*core.EmbeddingResponse, error) {
	if a.embedFn != nil {
		return a.embedFn(ctx, decision, req)
	}
	return &core.EmbeddingResponse{
		Model:        decision.Model,
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Data:         []core.EmbeddingObject{{Object: "embedding", Index: 0, Embedding: []float64{0.1}}},
		Usage:        core.Usage{PromptTokens: 1, TotalTokens: 1},
	}, nil
}

type scriptedResponsesAdapter struct {
	*scriptedAdapter
	responsesFn func(context.Context, core.RouteDecision, *core.ResponsesRequest) (*core.GatewayResponse, error)
}

func (a *scriptedResponsesAdapter) InvokeResponses(ctx context.Context, decision core.RouteDecision, req *core.ResponsesRequest) (*core.GatewayResponse, error) {
	if a.responsesFn != nil {
		return a.responsesFn(ctx, decision, req)
	}
	return &core.GatewayResponse{
		ID:           "resp_" + decision.Account.ID,
		Model:        decision.Model,
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Content:      "ok",
		FinishReason: "stop",
		CreatedAt:    time.Now().UTC(),
	}, nil
}

type scriptedImageGenerationAdapter struct {
	*scriptedAdapter
	generateFn func(context.Context, core.RouteDecision, *core.ImageGenerationRequest) (*core.ImageGenerationResponse, error)
	refreshFn  func(context.Context, core.Account) (core.Credential, error)
}

func (a *scriptedImageGenerationAdapter) GenerateImage(ctx context.Context, decision core.RouteDecision, req *core.ImageGenerationRequest) (*core.ImageGenerationResponse, error) {
	if a.generateFn != nil {
		return a.generateFn(ctx, decision, req)
	}
	return &core.ImageGenerationResponse{
		Model:        decision.Model,
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Body:         []byte(`{"created":1710000000,"data":[{"url":"https://example.com/image.png"}]}`),
	}, nil
}

func (a *scriptedImageGenerationAdapter) Refresh(ctx context.Context, account core.Account) (core.Credential, error) {
	if a.refreshFn != nil {
		return a.refreshFn(ctx, account)
	}
	return account.Credential, nil
}

type scriptedImageMultipartAdapter struct {
	*scriptedAdapter
	processFn func(context.Context, core.RouteDecision, *core.ImageMultipartRequest) (*core.ImageMultipartResponse, error)
}

func (a *scriptedImageMultipartAdapter) ProcessImageMultipart(ctx context.Context, decision core.RouteDecision, req *core.ImageMultipartRequest) (*core.ImageMultipartResponse, error) {
	if a.processFn != nil {
		return a.processFn(ctx, decision, req)
	}
	return &core.ImageMultipartResponse{
		Model:        decision.Model,
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Body:         []byte(`{"created":1710000000,"data":[{"url":"https://example.com/image.png"}]}`),
		ContentType:  "application/json",
	}, nil
}

type scriptedRefreshingAdapter struct {
	*scriptedAdapter
	refreshFn func(context.Context, core.Account) (core.Credential, error)
}

func (a *scriptedRefreshingAdapter) Refresh(ctx context.Context, account core.Account) (core.Credential, error) {
	if a.refreshFn != nil {
		return a.refreshFn(ctx, account)
	}
	return account.Credential, nil
}

type recordingAttemptObserver struct {
	mu       sync.Mutex
	started  []core.AttemptRecord
	finished []core.AttemptRecord
}

func (o *recordingAttemptObserver) AttemptStarted(attempt core.AttemptRecord) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.started = append(o.started, attempt)
}

func (o *recordingAttemptObserver) AttemptFinished(attempt core.AttemptRecord) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.finished = append(o.finished, attempt)
}

func (o *recordingAttemptObserver) startedAttempts() []core.AttemptRecord {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]core.AttemptRecord(nil), o.started...)
}

func (o *recordingAttemptObserver) finishedAttempts() []core.AttemptRecord {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]core.AttemptRecord(nil), o.finished...)
}

func temporaryInvokeError(message string) error {
	return &providers.InvokeError{
		Code:      "upstream_temporarily_unavailable",
		Temporary: true,
		Cooldown:  45 * time.Second,
		Err:       errors.New(message),
	}
}

func permanentInvokeError(message string) error {
	return &providers.InvokeError{
		Code:      "upstream_rejected",
		Temporary: false,
		Cooldown:  2 * time.Minute,
		Err:       errors.New(message),
	}
}

func upstreamNotFoundInvokeError(message string) error {
	return &providers.InvokeError{
		Code:      providers.ErrorCodeUpstreamNotFound,
		Temporary: false,
		Cooldown:  2 * time.Minute,
		Err:       errors.New(message),
	}
}

func withNoSoftRetryDelay(t *testing.T) {
	t.Helper()
	previous := cacheAffinitySoftRetryDelayFunc
	cacheAffinitySoftRetryDelayFunc = func(int) time.Duration { return 0 }
	t.Cleanup(func() {
		cacheAffinitySoftRetryDelayFunc = previous
	})
}

func requireRouteBinding(t *testing.T, engine *Engine, provider core.ProviderKind, client *core.APIClient, want string) {
	t.Helper()
	key, ok := routeBindingKeyForClient(provider, client)
	if !ok {
		t.Fatal("route binding key not found")
	}
	got, ok := engine.boundRouteAccountID(key)
	if !ok {
		t.Fatalf("route binding missing, want %q", want)
	}
	if got != want {
		t.Fatalf("route binding = %q, want %q", got, want)
	}
}

func assertAccountCooling(t *testing.T, account core.Account) {
	t.Helper()
	if account.Status != core.AccountStatusCooling || account.CooldownUntil == nil {
		t.Fatalf("status=%q cooldown=%#v, want cooling with cooldown", account.Status, account.CooldownUntil)
	}
	if account.ConsecutiveFails == 0 || account.TotalFails == 0 {
		t.Fatalf("failure counters = consecutive:%d total:%d, want recorded failure", account.ConsecutiveFails, account.TotalFails)
	}
	if account.LastUsedAt == nil {
		t.Fatal("LastUsedAt = nil, want failure timestamp")
	}
}

func TestExecuteFallsBackToBackupAccount(t *testing.T) {
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
			AccessToken: "primary-token",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "backup",
		Provider: core.ProviderOpenAI,
		Label:    "Backup",
		Status:   core.AccountStatusActive,
		Priority: 80,
		Weight:   80,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "backup-token",
		},
	}); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
				if decision.Account.ID == "primary" {
					return nil, temporaryInvokeError("primary unavailable")
				}
				return &core.GatewayResponse{
					ID:           "resp_backup",
					Model:        decision.Model,
					Provider:     decision.Provider,
					AccountID:    decision.Account.ID,
					AccountLabel: decision.Account.Label,
					Content:      "ok",
					FinishReason: "stop",
					CreatedAt:    time.Now().UTC(),
				}, nil
			},
		}),
	)

	result, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}, nil, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	resp := result.Response
	if resp.AccountID != "backup" {
		t.Fatalf("expected backup account, got %s", resp.AccountID)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(result.Attempts))
	}
	if result.Attempts[0].Status != "invoke_error" {
		t.Fatalf("first attempt status = %q", result.Attempts[0].Status)
	}
	if result.Attempts[1].Status != "ok" || result.Attempts[1].AccountID != "backup" {
		t.Fatalf("unexpected success attempt: %#v", result.Attempts[1])
	}

	primary, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	if primary.Status != core.AccountStatusActive || primary.CooldownUntil != nil {
		t.Fatalf("status=%q cooldown=%#v, want active without cooldown", primary.Status, primary.CooldownUntil)
	}
	if primary.ConsecutiveFails != 0 || primary.TotalFails != 0 {
		t.Fatalf("failure counters = consecutive:%d total:%d, want 0", primary.ConsecutiveFails, primary.TotalFails)
	}
}

func TestExecuteFallsBackAfterForbiddenAccountError(t *testing.T) {
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
			AccessToken: "primary-token",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "backup",
		Provider: core.ProviderOpenAI,
		Label:    "Backup",
		Status:   core.AccountStatusActive,
		Priority: 80,
		Weight:   80,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "backup-token",
		},
	}); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
				if decision.Account.ID == "primary" {
					return nil, &providers.InvokeError{
						Code:      "upstream_forbidden",
						Temporary: false,
						Cooldown:  2 * time.Minute,
						Err:       errors.New("API Key group was deleted"),
					}
				}
				return &core.GatewayResponse{
					ID:           "resp_backup",
					Model:        decision.Model,
					Provider:     decision.Provider,
					AccountID:    decision.Account.ID,
					AccountLabel: decision.Account.Label,
					Content:      "ok",
					FinishReason: "stop",
					CreatedAt:    time.Now().UTC(),
				}, nil
			},
		}),
	)

	result, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-5.5",
		Reason:    "test",
	}, nil, &core.GatewayRequest{
		Model:    "gpt-5.5",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if result.Response.AccountID != "backup" {
		t.Fatalf("account id = %q, want backup", result.Response.AccountID)
	}
	if len(result.Attempts) != 2 || result.Attempts[0].ErrorCode != "upstream_forbidden" || result.Attempts[1].AccountID != "backup" {
		t.Fatalf("attempts = %#v, want forbidden primary then backup success", result.Attempts)
	}

	primary, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	assertAccountCooling(t, primary)
}

func TestExecuteTriesAllPrimaryCandidatesBeforeBackupEvenWhenBackupIsEarlier(t *testing.T) {
	repo := storage.NewMemoryRepository()
	candidates := []core.Account{
		{
			ID:       "primary_down",
			Provider: core.ProviderOpenAI,
			Label:    "Primary Down",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
		},
		{
			ID:       "backup",
			Provider: core.ProviderOpenAI,
			Label:    "Backup",
			Status:   core.AccountStatusActive,
			Backup:   true,
			Priority: 1000,
			Weight:   1000,
		},
		{
			ID:       "primary_good",
			Provider: core.ProviderOpenAI,
			Label:    "Primary Good",
			Status:   core.AccountStatusActive,
			Priority: 90,
			Weight:   90,
		},
	}
	for _, account := range candidates {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
		}),
	)
	invoked := make([]string, 0, len(candidates))
	outcome, attempts, err := executeAcrossAccounts[*core.GatewayResponse](engine, context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-5.5",
		Reason:    "test",
	}, nil, attemptExecutor[*core.GatewayResponse]{
		modality: failureModalityAccount,
		selectCandidates: func(*Engine, core.ProviderKind, *core.APIClient) []core.Account {
			return candidates
		},
		invoke: func(_ context.Context, _ providers.Adapter, decision core.RouteDecision) (*core.GatewayResponse, error) {
			invoked = append(invoked, decision.Account.ID)
			if decision.Account.ID == "primary_down" {
				return nil, temporaryInvokeError("primary unavailable")
			}
			if decision.Account.ID == "backup" {
				t.Fatal("backup account was attempted before all primary accounts")
			}
			return &core.GatewayResponse{
				ID:           "resp_" + decision.Account.ID,
				Model:        decision.Model,
				Provider:     decision.Provider,
				AccountID:    decision.Account.ID,
				AccountLabel: decision.Account.Label,
				Content:      "ok",
				FinishReason: "stop",
				CreatedAt:    time.Now().UTC(),
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("executeAcrossAccounts returned error: %v", err)
	}
	if outcome.response.AccountID != "primary_good" {
		t.Fatalf("response account = %q, want primary_good", outcome.response.AccountID)
	}
	if !reflect.DeepEqual(invoked, []string{"primary_down", "primary_good"}) {
		t.Fatalf("invoked accounts = %#v, want primary_down then primary_good", invoked)
	}
	if len(attempts) != 2 || attempts[1].AccountID != "primary_good" {
		t.Fatalf("attempts = %#v, want primary_down failure then primary_good success", attempts)
	}
}

func TestExecuteFailsOverModelScopedNotFoundToNextAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{
			ID:       "missing_model",
			Provider: core.ProviderOpenAI,
			Label:    "Missing Model",
			Status:   core.AccountStatusActive,
			Priority: 200,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "missing-token",
			},
		},
		{
			ID:       "supports_model",
			Provider: core.ProviderOpenAI,
			Label:    "Supports Model",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "supports-token",
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	invoked := []string{}
	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
				invoked = append(invoked, decision.Account.ID)
				if decision.Account.ID == "missing_model" {
					return nil, upstreamNotFoundInvokeError("Model 'gpt-5.5-openai-compact' is not supported by any configured account in this group")
				}
				return &core.GatewayResponse{
					ID:           "resp_" + decision.Account.ID,
					Model:        decision.Model,
					Provider:     decision.Provider,
					AccountID:    decision.Account.ID,
					AccountLabel: decision.Account.Label,
					Content:      "ok",
					FinishReason: "stop",
					CreatedAt:    time.Now().UTC(),
				}, nil
			},
		}),
	)

	result, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-5.5-openai-compact",
		Reason:    "test",
	}, nil, &core.GatewayRequest{Model: "gpt-5.5-openai-compact"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Response.AccountID != "supports_model" {
		t.Fatalf("response account = %q, want supports_model", result.Response.AccountID)
	}
	if !reflect.DeepEqual(invoked, []string{"missing_model", "supports_model"}) {
		t.Fatalf("invoked accounts = %#v, want missing_model then supports_model", invoked)
	}
	if len(result.Attempts) != 2 || result.Attempts[0].ErrorCode != providers.ErrorCodeUpstreamNotFound || result.Attempts[1].Status != "ok" {
		t.Fatalf("attempts = %#v, want not_found then ok", result.Attempts)
	}
	account, err := repo.GetAccount("missing_model")
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != core.AccountStatusActive || account.CooldownUntil != nil {
		t.Fatalf("missing-model account status=%q cooldown=%#v, want active without cooldown", account.Status, account.CooldownUntil)
	}
}

func TestExecuteDoesNotFailOverPlainNotFound(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{
			ID:       "plain_not_found",
			Provider: core.ProviderOpenAI,
			Label:    "Plain Not Found",
			Status:   core.AccountStatusActive,
			Priority: 200,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "plain-token",
			},
		},
		{
			ID:       "unused",
			Provider: core.ProviderOpenAI,
			Label:    "Unused",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "unused-token",
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	invoked := []string{}
	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
				invoked = append(invoked, decision.Account.ID)
				if decision.Account.ID == "unused" {
					t.Fatal("plain not found should not attempt the next account")
				}
				return nil, upstreamNotFoundInvokeError("upstream route not found")
			},
		}),
	)

	_, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-5.5-openai-compact",
		Reason:    "test",
	}, nil, &core.GatewayRequest{Model: "gpt-5.5-openai-compact"})
	if err == nil {
		t.Fatal("Execute returned nil error, want failure")
	}
	if !reflect.DeepEqual(invoked, []string{"plain_not_found"}) {
		t.Fatalf("invoked accounts = %#v, want only plain_not_found", invoked)
	}
	var executionErr *ExecutionError
	if !errors.As(err, &executionErr) || len(executionErr.Attempts) != 1 {
		t.Fatalf("err = %v attempts = %#v, want one-attempt execution error", err, executionErr)
	}
}

func TestExecuteReusesSuccessfulFallbackForRouteAffinity(t *testing.T) {
	withNoSoftRetryDelay(t)
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{
			ID:       "primary",
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
			ID:       "backup",
			Provider: core.ProviderOpenAI,
			Label:    "Backup",
			Status:   core.AccountStatusActive,
			Priority: 80,
			Weight:   80,
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

	var failPrimary atomic.Bool
	failPrimary.Store(true)
	var invokedMu sync.Mutex
	invoked := make([]string, 0, 4)
	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
				invokedMu.Lock()
				invoked = append(invoked, decision.Account.ID)
				invokedMu.Unlock()
				if decision.Account.ID == "primary" && failPrimary.Load() {
					return nil, temporaryInvokeError("primary unavailable")
				}
				return &core.GatewayResponse{
					ID:           "resp_" + decision.Account.ID,
					Model:        decision.Model,
					Provider:     decision.Provider,
					AccountID:    decision.Account.ID,
					AccountLabel: decision.Account.Label,
					Content:      "ok",
					FinishReason: "stop",
					CreatedAt:    time.Now().UTC(),
				}, nil
			},
		}),
	)
	client := &core.APIClient{ID: "client", RouteAffinityKey: "client\x00gpt-4.1\x00session-id:sess_123"}
	plan := core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}
	req := &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	}

	first, err := engine.Execute(context.Background(), plan, client, req)
	if err != nil {
		t.Fatalf("first execute returned error: %v", err)
	}
	if first.Response.AccountID != "backup" {
		t.Fatalf("first account = %q, want backup", first.Response.AccountID)
	}
	if len(first.Attempts) != 3 || first.Attempts[0].AccountID != "primary" || first.Attempts[1].AccountID != "primary" || first.Attempts[2].AccountID != "backup" {
		t.Fatalf("first attempts = %#v, want primary failure, one primary retry, then backup success", first.Attempts)
	}

	failPrimary.Store(false)
	primary, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	primary.Status = core.AccountStatusActive
	primary.CooldownUntil = nil
	primary.ConsecutiveFails = 0
	if err := repo.UpsertAccount(primary); err != nil {
		t.Fatal(err)
	}

	second, err := engine.Execute(context.Background(), plan, client, req)
	if err != nil {
		t.Fatalf("second execute returned error: %v", err)
	}
	if second.Response.AccountID != "backup" {
		t.Fatalf("second account = %q, want backup", second.Response.AccountID)
	}
	if len(second.Attempts) != 1 || second.Attempts[0].Status != "ok" || second.Attempts[0].AccountID != "backup" {
		t.Fatalf("second attempts = %#v, want direct backup success", second.Attempts)
	}
	invokedMu.Lock()
	gotInvoked := append([]string(nil), invoked...)
	invokedMu.Unlock()
	wantInvoked := []string{"primary", "primary", "backup", "backup"}
	if !reflect.DeepEqual(gotInvoked, wantInvoked) {
		t.Fatalf("invoked accounts = %#v, want %#v", gotInvoked, wantInvoked)
	}
}

func TestExecuteUpdatesCacheAffinityBindingAfterFallbackSuccess(t *testing.T) {
	withNoSoftRetryDelay(t)
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{
			ID:       "primary",
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
			ID:       "backup",
			Provider: core.ProviderOpenAI,
			Label:    "Backup",
			Status:   core.AccountStatusActive,
			Priority: 80,
			Weight:   80,
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

	var failPrimary atomic.Bool
	var invokedMu sync.Mutex
	invoked := make([]string, 0, 7)
	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
				invokedMu.Lock()
				invoked = append(invoked, decision.Account.ID)
				invokedMu.Unlock()
				if decision.Account.ID == "primary" && failPrimary.Load() {
					return nil, temporaryInvokeError("primary unavailable")
				}
				return &core.GatewayResponse{
					ID:           "resp_" + decision.Account.ID,
					Model:        decision.Model,
					Provider:     decision.Provider,
					AccountID:    decision.Account.ID,
					AccountLabel: decision.Account.Label,
					Content:      "ok",
					FinishReason: "stop",
					CreatedAt:    time.Now().UTC(),
				}, nil
			},
		}),
	)
	client := &core.APIClient{ID: "client", RouteAffinityKey: "client\x00gpt-4.1\x00cache-key", CacheAffinityRoute: true}
	plan := core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}
	req := &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	}

	warm, err := engine.Execute(context.Background(), plan, client, req)
	if err != nil {
		t.Fatalf("warm execute returned error: %v", err)
	}
	if warm.Response.AccountID != "primary" {
		t.Fatalf("warm account = %q, want primary", warm.Response.AccountID)
	}
	requireRouteBinding(t, engine, core.ProviderOpenAI, client, "primary")

	failPrimary.Store(true)
	fallback, err := engine.Execute(context.Background(), plan, client, req)
	if err != nil {
		t.Fatalf("fallback execute returned error: %v", err)
	}
	if fallback.Response.AccountID != "backup" {
		t.Fatalf("fallback account = %q, want backup", fallback.Response.AccountID)
	}
	if len(fallback.Attempts) != 3 || fallback.Attempts[0].AccountID != "primary" || fallback.Attempts[1].AccountID != "primary" || fallback.Attempts[2].AccountID != "backup" {
		t.Fatalf("fallback attempts = %#v, want primary failure, one primary retry, then backup success", fallback.Attempts)
	}
	requireRouteBinding(t, engine, core.ProviderOpenAI, client, "backup")

	failPrimary.Store(false)
	primary, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	primary.Status = core.AccountStatusActive
	primary.CooldownUntil = nil
	primary.ConsecutiveFails = 0
	if err := repo.UpsertAccount(primary); err != nil {
		t.Fatal(err)
	}

	recovered, err := engine.Execute(context.Background(), plan, client, req)
	if err != nil {
		t.Fatalf("recovered execute returned error: %v", err)
	}
	if recovered.Response.AccountID != "backup" {
		t.Fatalf("recovered account = %q, want backup", recovered.Response.AccountID)
	}
	if len(recovered.Attempts) != 1 || recovered.Attempts[0].Status != "ok" || recovered.Attempts[0].AccountID != "backup" {
		t.Fatalf("recovered attempts = %#v, want direct backup success", recovered.Attempts)
	}
	invokedMu.Lock()
	gotInvoked := append([]string(nil), invoked...)
	invokedMu.Unlock()
	wantInvoked := []string{"primary", "primary", "primary", "backup", "backup"}
	if !reflect.DeepEqual(gotInvoked, wantInvoked) {
		t.Fatalf("invoked accounts = %#v, want %#v", gotInvoked, wantInvoked)
	}
}

func TestExecuteBindsFirstSuccessfulCacheAffinityFallback(t *testing.T) {
	withNoSoftRetryDelay(t)
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{ID: "primary", Provider: core.ProviderOpenAI, Label: "Primary", Status: core.AccountStatusActive, Priority: 100, Weight: 100},
		{ID: "backup", Provider: core.ProviderOpenAI, Label: "Backup", Status: core.AccountStatusActive, Priority: 80, Weight: 80},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	var failPrimary atomic.Bool
	failPrimary.Store(true)
	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
				if decision.Account.ID == "primary" && failPrimary.Load() {
					return nil, temporaryInvokeError("primary unavailable")
				}
				return &core.GatewayResponse{
					ID:           "resp_" + decision.Account.ID,
					Model:        decision.Model,
					Provider:     decision.Provider,
					AccountID:    decision.Account.ID,
					AccountLabel: decision.Account.Label,
					Content:      "ok",
					FinishReason: "stop",
					CreatedAt:    time.Now().UTC(),
				}, nil
			},
		}),
	)
	client := &core.APIClient{ID: "client", RouteAffinityKey: "client\x00gpt-4.1\x00cache-key", CacheAffinityRoute: true}
	plan := core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}
	req := &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	}

	first, err := engine.Execute(context.Background(), plan, client, req)
	if err != nil {
		t.Fatalf("first execute returned error: %v", err)
	}
	if first.Response.AccountID != "backup" {
		t.Fatalf("first account = %q, want backup", first.Response.AccountID)
	}
	requireRouteBinding(t, engine, core.ProviderOpenAI, client, "backup")

	failPrimary.Store(false)
	primary, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	primary.Status = core.AccountStatusActive
	primary.CooldownUntil = nil
	primary.ConsecutiveFails = 0
	if err := repo.UpsertAccount(primary); err != nil {
		t.Fatal(err)
	}

	second, err := engine.Execute(context.Background(), plan, client, req)
	if err != nil {
		t.Fatalf("second execute returned error: %v", err)
	}
	if second.Response.AccountID != "backup" {
		t.Fatalf("second account = %q, want backup", second.Response.AccountID)
	}
	requireRouteBinding(t, engine, core.ProviderOpenAI, client, "backup")
}

func TestExecuteDoesNotReuseBackupRouteBindingAfterPrimaryRecovers(t *testing.T) {
	withNoSoftRetryDelay(t)
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{
			ID:       "primary",
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
			ID:       "backup",
			Provider: core.ProviderOpenAI,
			Label:    "Backup",
			Status:   core.AccountStatusActive,
			Backup:   true,
			Priority: 1000,
			Weight:   1000,
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

	var failPrimary atomic.Bool
	failPrimary.Store(true)
	var invokedMu sync.Mutex
	invoked := make([]string, 0, 4)
	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
				invokedMu.Lock()
				invoked = append(invoked, decision.Account.ID)
				invokedMu.Unlock()
				if decision.Account.ID == "primary" && failPrimary.Load() {
					return nil, temporaryInvokeError("primary unavailable")
				}
				return &core.GatewayResponse{
					ID:           "resp_" + decision.Account.ID,
					Model:        decision.Model,
					Provider:     decision.Provider,
					AccountID:    decision.Account.ID,
					AccountLabel: decision.Account.Label,
					Content:      "ok",
					FinishReason: "stop",
					CreatedAt:    time.Now().UTC(),
				}, nil
			},
		}),
	)
	client := &core.APIClient{ID: "client", RouteAffinityKey: "client\x00gpt-4.1\x00session-id:sess_123"}
	plan := core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}
	req := &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	}

	first, err := engine.Execute(context.Background(), plan, client, req)
	if err != nil {
		t.Fatalf("first execute returned error: %v", err)
	}
	if first.Response.AccountID != "backup" {
		t.Fatalf("first account = %q, want backup", first.Response.AccountID)
	}
	if len(first.Attempts) != 3 || first.Attempts[0].AccountID != "primary" || first.Attempts[1].AccountID != "primary" || first.Attempts[2].AccountID != "backup" {
		t.Fatalf("first attempts = %#v, want primary failure, one primary retry, then backup success", first.Attempts)
	}

	failPrimary.Store(false)
	primary, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	primary.Status = core.AccountStatusActive
	primary.CooldownUntil = nil
	primary.ConsecutiveFails = 0
	if err := repo.UpsertAccount(primary); err != nil {
		t.Fatal(err)
	}

	second, err := engine.Execute(context.Background(), plan, client, req)
	if err != nil {
		t.Fatalf("second execute returned error: %v", err)
	}
	if second.Response.AccountID != "primary" {
		t.Fatalf("second account = %q, want primary after recovery", second.Response.AccountID)
	}
	if len(second.Attempts) != 1 || second.Attempts[0].Status != "ok" || second.Attempts[0].AccountID != "primary" {
		t.Fatalf("second attempts = %#v, want direct primary success", second.Attempts)
	}
	invokedMu.Lock()
	gotInvoked := append([]string(nil), invoked...)
	invokedMu.Unlock()
	wantInvoked := []string{"primary", "primary", "backup", "primary"}
	if !reflect.DeepEqual(gotInvoked, wantInvoked) {
		t.Fatalf("invoked accounts = %#v, want %#v", gotInvoked, wantInvoked)
	}
}

func TestExecuteSoftRetriesCacheAffinityAccountBeforeFallback(t *testing.T) {
	withNoSoftRetryDelay(t)
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{
			ID:       "primary",
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
			ID:       "backup",
			Provider: core.ProviderOpenAI,
			Label:    "Backup",
			Status:   core.AccountStatusActive,
			Priority: 90,
			Weight:   100,
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

	var calls atomic.Int32
	var invokedMu sync.Mutex
	invoked := make([]string, 0, 4)
	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
				invokedMu.Lock()
				invoked = append(invoked, decision.Account.ID)
				invokedMu.Unlock()
				if decision.Account.ID == "primary" && calls.Add(1) <= int32(cacheAffinitySoftRetryMaxRetries) {
					return nil, &providers.InvokeError{
						Code:      "upstream_transport_error",
						Temporary: true,
						Cooldown:  20 * time.Second,
						Err:       errors.New("connection reset by peer"),
					}
				}
				return &core.GatewayResponse{
					ID:           "resp_" + decision.Account.ID,
					Model:        decision.Model,
					Provider:     decision.Provider,
					AccountID:    decision.Account.ID,
					AccountLabel: decision.Account.Label,
					Content:      "ok",
					FinishReason: "stop",
					CreatedAt:    time.Now().UTC(),
				}, nil
			},
		}),
	)

	result, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}, &core.APIClient{ID: "client", RouteAffinityKey: "client\x00gpt-4.1\x00cache-key"}, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Response.AccountID != "primary" {
		t.Fatalf("response account = %q, want primary", result.Response.AccountID)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %#v, want one primary failure then primary success", result.Attempts)
	}
	if result.Attempts[0].Status != "invoke_error" || result.Attempts[0].AccountID != "primary" {
		t.Fatalf("attempts = %#v, want one primary failure then primary success", result.Attempts)
	}
	if result.Attempts[1].Status != "ok" || result.Attempts[1].AccountID != "primary" {
		t.Fatalf("attempts = %#v, want one primary failure then primary success", result.Attempts)
	}
	invokedMu.Lock()
	gotInvoked := append([]string(nil), invoked...)
	invokedMu.Unlock()
	wantInvoked := []string{"primary", "primary"}
	if !reflect.DeepEqual(gotInvoked, wantInvoked) {
		t.Fatalf("invoked accounts = %#v, want %#v", gotInvoked, wantInvoked)
	}
}

func TestExecuteDoesNotSoftRetryWithoutCacheAffinity(t *testing.T) {
	withNoSoftRetryDelay(t)
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{ID: "primary", Provider: core.ProviderOpenAI, Label: "Primary", Status: core.AccountStatusActive, Priority: 100, Weight: 100},
		{ID: "backup", Provider: core.ProviderOpenAI, Label: "Backup", Status: core.AccountStatusActive, Priority: 90, Weight: 100},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	var invokedMu sync.Mutex
	invoked := make([]string, 0, 3)
	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
				invokedMu.Lock()
				invoked = append(invoked, decision.Account.ID)
				invokedMu.Unlock()
				if decision.Account.ID == "primary" {
					return nil, &providers.InvokeError{
						Code:      "upstream_transport_error",
						Temporary: true,
						Cooldown:  20 * time.Second,
						Err:       errors.New("connection reset by peer"),
					}
				}
				return &core.GatewayResponse{
					ID:           "resp_backup",
					Model:        decision.Model,
					Provider:     decision.Provider,
					AccountID:    decision.Account.ID,
					AccountLabel: decision.Account.Label,
					Content:      "ok",
					FinishReason: "stop",
					CreatedAt:    time.Now().UTC(),
				}, nil
			},
		}),
	)

	result, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}, nil, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Response.AccountID != "backup" {
		t.Fatalf("response account = %q, want backup", result.Response.AccountID)
	}
	invokedMu.Lock()
	gotInvoked := append([]string(nil), invoked...)
	invokedMu.Unlock()
	wantInvoked := []string{"primary", "backup"}
	if !reflect.DeepEqual(gotInvoked, wantInvoked) {
		t.Fatalf("invoked accounts = %#v, want %#v", gotInvoked, wantInvoked)
	}
}

func TestExecuteDoesNotTreatUserMetadataAsMonitorProbe(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
	}); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
				return &core.GatewayResponse{
					ID:           "resp_empty",
					Model:        decision.Model,
					Provider:     decision.Provider,
					AccountID:    decision.Account.ID,
					AccountLabel: decision.Account.Label,
					FinishReason: "stop",
					CreatedAt:    time.Now().UTC(),
				}, nil
			},
		}),
	)

	result, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}, &core.APIClient{ID: "client", Name: "User Client"}, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Metadata: map[string]string{"purpose": "status_monitor"},
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Response == nil || result.Response.AccountID != "primary" || strings.TrimSpace(result.Response.Content) != "" {
		t.Fatalf("response = %#v, want successful empty user response", result.Response)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].Status != "ok" {
		t.Fatalf("attempts = %#v, want one ok attempt", result.Attempts)
	}
}

func TestExecuteFallsBackAfterUpstreamTransportDeadline(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{ID: "primary", Provider: core.ProviderOpenAI, Label: "Primary", Status: core.AccountStatusActive, Priority: 100, Weight: 100},
		{ID: "backup", Provider: core.ProviderOpenAI, Label: "Backup", Status: core.AccountStatusActive, Priority: 90, Weight: 100},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	var invokedMu sync.Mutex
	invoked := make([]string, 0, 2)
	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
				invokedMu.Lock()
				invoked = append(invoked, decision.Account.ID)
				invokedMu.Unlock()
				if decision.Account.ID == "primary" {
					return nil, &providers.InvokeError{
						Code:      providers.ErrorCodeUpstreamTransportError,
						Temporary: true,
						Cooldown:  20 * time.Second,
						Err:       fmt.Errorf("request failed: %w", context.DeadlineExceeded),
					}
				}
				return &core.GatewayResponse{
					ID:           "resp_backup",
					Model:        decision.Model,
					Provider:     decision.Provider,
					AccountID:    decision.Account.ID,
					AccountLabel: decision.Account.Label,
					Content:      "ok",
					FinishReason: "stop",
					CreatedAt:    time.Now().UTC(),
				}, nil
			},
		}),
	)

	result, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-5.5",
		Reason:    "test",
	}, nil, &core.GatewayRequest{
		Model:    "gpt-5.5",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Response.AccountID != "backup" {
		t.Fatalf("response account = %q, want backup", result.Response.AccountID)
	}
	if len(result.Attempts) != 2 || result.Attempts[0].ErrorCode != providers.ErrorCodeUpstreamTransportError || result.Attempts[1].AccountID != "backup" {
		t.Fatalf("attempts = %#v, want primary transport deadline then backup success", result.Attempts)
	}
	invokedMu.Lock()
	gotInvoked := append([]string(nil), invoked...)
	invokedMu.Unlock()
	if !reflect.DeepEqual(gotInvoked, []string{"primary", "backup"}) {
		t.Fatalf("invoked accounts = %#v, want primary then backup", gotInvoked)
	}
}

func TestExecuteDoesNotSoftRetryQuotaErrors(t *testing.T) {
	withNoSoftRetryDelay(t)
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{ID: "primary", Provider: core.ProviderOpenAI, Label: "Primary", Status: core.AccountStatusActive, Priority: 100, Weight: 100},
		{ID: "backup", Provider: core.ProviderOpenAI, Label: "Backup", Status: core.AccountStatusActive, Priority: 90, Weight: 100},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	var invokedMu sync.Mutex
	invoked := make([]string, 0, 3)
	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
				invokedMu.Lock()
				invoked = append(invoked, decision.Account.ID)
				invokedMu.Unlock()
				if decision.Account.ID == "primary" {
					return nil, rateLimitError()
				}
				return &core.GatewayResponse{
					ID:           "resp_backup",
					Model:        decision.Model,
					Provider:     decision.Provider,
					AccountID:    decision.Account.ID,
					AccountLabel: decision.Account.Label,
					Content:      "ok",
					FinishReason: "stop",
					CreatedAt:    time.Now().UTC(),
				}, nil
			},
		}),
	)

	result, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}, &core.APIClient{ID: "client", RouteAffinityKey: "client\x00gpt-4.1\x00cache-key"}, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Response.AccountID != "backup" {
		t.Fatalf("response account = %q, want backup", result.Response.AccountID)
	}
	invokedMu.Lock()
	gotInvoked := append([]string(nil), invoked...)
	invokedMu.Unlock()
	wantInvoked := []string{"primary", "backup"}
	if !reflect.DeepEqual(gotInvoked, wantInvoked) {
		t.Fatalf("invoked accounts = %#v, want %#v", gotInvoked, wantInvoked)
	}
}

func TestExecuteImageGenerationReusesSuccessfulFallbackForRouteAffinity(t *testing.T) {
	withNoSoftRetryDelay(t)
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{
			ID:       "primary",
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
			ID:       "backup",
			Provider: core.ProviderOpenAI,
			Label:    "Backup",
			Status:   core.AccountStatusActive,
			Priority: 80,
			Weight:   80,
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

	var failPrimary atomic.Bool
	failPrimary.Store(true)
	var invokedMu sync.Mutex
	invoked := make([]string, 0, 4)
	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedImageGenerationAdapter{
			scriptedAdapter: &scriptedAdapter{kind: core.ProviderOpenAI},
			generateFn: func(_ context.Context, decision core.RouteDecision, _ *core.ImageGenerationRequest) (*core.ImageGenerationResponse, error) {
				invokedMu.Lock()
				invoked = append(invoked, decision.Account.ID)
				invokedMu.Unlock()
				if decision.Account.ID == "primary" && failPrimary.Load() {
					return nil, temporaryInvokeError("primary unavailable")
				}
				return &core.ImageGenerationResponse{
					Model:        decision.Model,
					Provider:     decision.Provider,
					AccountID:    decision.Account.ID,
					AccountLabel: decision.Account.Label,
					Body:         []byte(`{"created":1710000000,"data":[{"url":"https://example.com/image.png"}]}`),
				}, nil
			},
		}),
	)
	client := &core.APIClient{ID: "client", RouteAffinityKey: "client\x00gpt-image-2\x00session-id:sess_123"}
	plan := core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-image-2",
		Reason:    "test",
	}
	req := &core.ImageGenerationRequest{Model: "gpt-image-2", Prompt: "test"}

	first, err := engine.ExecuteImageGeneration(context.Background(), plan, client, req)
	if err != nil {
		t.Fatalf("first ExecuteImageGeneration returned error: %v", err)
	}
	if first.Response.AccountID != "backup" {
		t.Fatalf("first account = %q, want backup", first.Response.AccountID)
	}
	if len(first.Attempts) != 3 || first.Attempts[0].AccountID != "primary" || first.Attempts[1].AccountID != "primary" || first.Attempts[2].AccountID != "backup" {
		t.Fatalf("first attempts = %#v, want primary failure, one primary retry, then backup success", first.Attempts)
	}

	failPrimary.Store(false)
	primary, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	primary.Status = core.AccountStatusActive
	primary.CooldownUntil = nil
	primary.ConsecutiveFails = 0
	if err := repo.UpsertAccount(primary); err != nil {
		t.Fatal(err)
	}

	second, err := engine.ExecuteImageGeneration(context.Background(), plan, client, req)
	if err != nil {
		t.Fatalf("second ExecuteImageGeneration returned error: %v", err)
	}
	if second.Response.AccountID != "backup" {
		t.Fatalf("second account = %q, want backup", second.Response.AccountID)
	}
	if len(second.Attempts) != 1 || second.Attempts[0].Status != "ok" || second.Attempts[0].AccountID != "backup" {
		t.Fatalf("second attempts = %#v, want direct backup success", second.Attempts)
	}
	invokedMu.Lock()
	gotInvoked := append([]string(nil), invoked...)
	invokedMu.Unlock()
	wantInvoked := []string{"primary", "primary", "backup", "backup"}
	if !reflect.DeepEqual(gotInvoked, wantInvoked) {
		t.Fatalf("invoked accounts = %#v, want %#v", gotInvoked, wantInvoked)
	}
}

func TestExecuteDoesNotPenalizeAccountWhenRequestContextIsCanceled(t *testing.T) {
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
			AccessToken: "primary-token",
		},
	}); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(context.Context, core.RouteDecision, *core.GatewayRequest) (*core.GatewayResponse, error) {
				return nil, &providers.InvokeError{
					Code:      "upstream_transport_error",
					Temporary: true,
					Cooldown:  20 * time.Second,
					Err:       context.Canceled,
				}
			},
		}),
	)

	_, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}, nil, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	primary, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	if primary.Status != core.AccountStatusActive {
		t.Fatalf("status = %q, want %q", primary.Status, core.AccountStatusActive)
	}
	if primary.ConsecutiveFails != 0 || primary.TotalFails != 0 {
		t.Fatalf("failure counters = consecutive:%d total:%d, want 0", primary.ConsecutiveFails, primary.TotalFails)
	}
}

func TestExecuteDoesNotBlockRefreshableAccountOnAuthError(t *testing.T) {
	repo := storage.NewMemoryRepository()
	future := time.Now().UTC().Add(time.Hour)
	if err := repo.UpsertAccount(core.Account{
		ID:       "oauth",
		Provider: core.ProviderOpenAI,
		Label:    "OAuth",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:         "oauth",
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    &future,
		},
	}); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(context.Context, core.RouteDecision, *core.GatewayRequest) (*core.GatewayResponse, error) {
				return nil, &providers.InvokeError{
					Code:      "upstream_auth_error",
					Temporary: false,
					Cooldown:  2 * time.Minute,
					Err:       errors.New("token rejected"),
				}
			},
		}),
	)

	_, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}, nil, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	account, err := repo.GetAccount("oauth")
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != core.AccountStatusCooling {
		t.Fatalf("status = %q, want %q", account.Status, core.AccountStatusCooling)
	}
	if account.Credential.ExpiresAt == nil || account.Credential.ExpiresAt.After(time.Now().UTC()) {
		t.Fatalf("expires_at should be forced into the past, got %#v", account.Credential.ExpiresAt)
	}
}

func TestExecuteMarksProviderBannedAccount(t *testing.T) {
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
			AccessToken: "primary-token",
		},
	}); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(context.Context, core.RouteDecision, *core.GatewayRequest) (*core.GatewayResponse, error) {
				return nil, &providers.InvokeError{
					Code:      "upstream_provider_banned",
					Temporary: false,
					Err:       errors.New("account has been suspended"),
				}
			},
		}),
	)

	_, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}, nil, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	account, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != core.AccountStatusProviderBanned {
		t.Fatalf("status = %q, want %q", account.Status, core.AccountStatusProviderBanned)
	}
	if account.CooldownUntil != nil {
		t.Fatalf("provider-banned account should not have cooldown, got %#v", account.CooldownUntil)
	}
}

func TestExecuteBreaksSharedUpstreamScopeForTransientNetworkErrors(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{
			ID:       "primary",
			Provider: core.ProviderOpenAI,
			Label:    "Primary",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
			ProxyURL: "http://proxy.shared.test:8080",
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "primary-token",
			},
		},
		{
			ID:       "same_scope",
			Provider: core.ProviderOpenAI,
			Label:    "Same Scope",
			Status:   core.AccountStatusActive,
			Priority: 90,
			Weight:   100,
			ProxyURL: "http://proxy.shared.test:8080",
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "same-scope-token",
			},
		},
		{
			ID:       "other_scope",
			Provider: core.ProviderOpenAI,
			Label:    "Other Scope",
			Status:   core.AccountStatusActive,
			Priority: 80,
			Weight:   100,
			ProxyURL: "http://proxy.other.test:8080",
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "other-scope-token",
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	var invoked []string
	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
				invoked = append(invoked, decision.Account.ID)
				if decision.Account.ID == "other_scope" {
					return &core.GatewayResponse{
						ID:           "resp_other_scope",
						Model:        decision.Model,
						Provider:     decision.Provider,
						AccountID:    decision.Account.ID,
						AccountLabel: decision.Account.Label,
						Content:      "ok",
						FinishReason: "stop",
						CreatedAt:    time.Now().UTC(),
					}, nil
				}
				return nil, &providers.InvokeError{
					Code:      "upstream_transport_error",
					Temporary: true,
					Cooldown:  20 * time.Second,
					Err:       errors.New("unexpected EOF"),
				}
			},
		}),
	)

	req := &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	}
	plan := core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}

	result, err := engine.Execute(context.Background(), plan, nil, req)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Response.AccountID != "other_scope" {
		t.Fatalf("response account = %q, want other_scope", result.Response.AccountID)
	}
	if !reflect.DeepEqual(invoked, []string{"primary", "other_scope"}) {
		t.Fatalf("invoked accounts = %#v, want primary then other_scope", invoked)
	}

	for _, id := range []string{"primary", "same_scope"} {
		account, err := repo.GetAccount(id)
		if err != nil {
			t.Fatal(err)
		}
		if account.Status != core.AccountStatusActive || account.CooldownUntil != nil {
			t.Fatalf("%s status=%q cooldown=%#v, want active without cooldown", id, account.Status, account.CooldownUntil)
		}
		if account.ConsecutiveFails != 0 || account.TotalFails != 0 {
			t.Fatalf("%s failure counters = consecutive:%d total:%d, want 0", id, account.ConsecutiveFails, account.TotalFails)
		}
	}
}

func TestExecuteTriesBreakerPrimaryBeforeBackup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{
			ID:       "primary",
			Provider: core.ProviderOpenAI,
			Label:    "Primary",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
			ProxyURL: "http://proxy.primary.test:8080",
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "primary-token",
			},
		},
		{
			ID:       "same_scope",
			Provider: core.ProviderOpenAI,
			Label:    "Same Scope",
			Status:   core.AccountStatusActive,
			Priority: 90,
			Weight:   100,
			ProxyURL: "http://proxy.primary.test:8080",
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "same-scope-token",
			},
		},
		{
			ID:       "backup",
			Provider: core.ProviderOpenAI,
			Label:    "Backup",
			Status:   core.AccountStatusActive,
			Backup:   true,
			Priority: 1000,
			Weight:   1000,
			ProxyURL: "http://proxy.backup.test:8080",
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

	var invoked []string
	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
				invoked = append(invoked, decision.Account.ID)
				if decision.Account.ID == "same_scope" {
					return &core.GatewayResponse{
						ID:           "resp_same_scope",
						Model:        decision.Model,
						Provider:     decision.Provider,
						AccountID:    decision.Account.ID,
						AccountLabel: decision.Account.Label,
						Content:      "ok",
						FinishReason: "stop",
						CreatedAt:    time.Now().UTC(),
					}, nil
				}
				return nil, &providers.InvokeError{
					Code:      "upstream_transport_error",
					Temporary: true,
					Cooldown:  20 * time.Second,
					Err:       errors.New("unexpected EOF"),
				}
			},
		}),
	)

	result, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}, nil, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Response.AccountID != "same_scope" {
		t.Fatalf("response account = %q, want same_scope", result.Response.AccountID)
	}
	if !reflect.DeepEqual(invoked, []string{"primary", "same_scope"}) {
		t.Fatalf("invoked accounts = %#v, want primary then same_scope", invoked)
	}

	sameScope, err := repo.GetAccount("same_scope")
	if err != nil {
		t.Fatal(err)
	}
	if sameScope.LastUsedAt == nil {
		t.Fatal("same-scope primary account was not attempted")
	}
}

func TestMarkFailureDoesNotCoolAccountForSharedUpstreamFailuresWithoutCustomScope(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
	}); err != nil {
		t.Fatal(err)
	}
	stale, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(accounts.NewPool(repo), providers.NewRegistry())
	networkErr := &providers.InvokeError{
		Code:      "upstream_transport_error",
		Temporary: true,
		Cooldown:  20 * time.Second,
		Err:       errors.New("unexpected EOF"),
	}

	engine.markFailure(stale, networkErr)

	account, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != core.AccountStatusActive || account.CooldownUntil != nil {
		t.Fatalf("status=%q cooldown=%#v, want active without cooldown", account.Status, account.CooldownUntil)
	}
	if account.ConsecutiveFails != 0 || account.TotalFails != 0 {
		t.Fatalf("failure counters = consecutive:%d total:%d, want 0", account.ConsecutiveFails, account.TotalFails)
	}
}

func TestMarkFailureScopesRateLimitToChatQuota(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
	}); err != nil {
		t.Fatal(err)
	}
	pool := accounts.NewPool(repo)
	engine := NewEngine(pool, providers.NewRegistry())

	engine.markFailure(core.Account{ID: "primary"}, rateLimitError())

	account, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != core.AccountStatusActive || account.CooldownUntil != nil {
		t.Fatalf("status=%q cooldown=%#v, want active without global cooldown", account.Status, account.CooldownUntil)
	}
	quota := core.ReadAccountQuota(account)
	if quota == nil ||
		quota.Additional == nil ||
		quota.Additional[core.AccountQuotaRuntimeChatLimitID].Primary == nil ||
		quota.Additional[core.AccountQuotaRuntimeChatLimitID].Primary.UsedPercent != 100 ||
		quota.Additional[core.AccountQuotaRuntimeChatLimitID].Primary.ResetsAt == nil {
		t.Fatalf("chat quota snapshot = %#v", quota)
	}
	if candidates := pool.Candidates(core.ProviderOpenAI, nil); len(candidates) != 0 {
		t.Fatalf("chat candidates = %#v, want none while chat quota is scoped limited", candidates)
	}
	if candidates := pool.ImageCandidates(core.ProviderOpenAI, nil); len(candidates) != 1 || candidates[0].ID != "primary" {
		t.Fatalf("image candidates = %#v, want image routing unaffected by chat rate limit", candidates)
	}
}

func TestSub2APIGatewayServerErrorFailsOverWithoutCoolingOuterAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		sub2APIGatewayTestAccount("primary", 100),
		{
			ID:       "backup",
			Provider: core.ProviderOpenAI,
			Label:    "Backup",
			Status:   core.AccountStatusActive,
			Priority: 90,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "backup-token",
				Metadata: map[string]string{
					"account_login_method": "api_key",
				},
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
				if decision.Account.ID == "primary" {
					return nil, &providers.InvokeError{
						Code:      "upstream_server_error",
						Temporary: true,
						Cooldown:  30 * time.Second,
						Err:       errors.New("Upstream service temporarily unavailable"),
					}
				}
				return &core.GatewayResponse{
					ID:           "resp_backup",
					Model:        decision.Model,
					Provider:     decision.Provider,
					AccountID:    decision.Account.ID,
					AccountLabel: decision.Account.Label,
					Content:      "ok",
					FinishReason: "stop",
					CreatedAt:    time.Now().UTC(),
				}, nil
			},
		}),
	)

	result, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}, nil, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Response.AccountID != "backup" {
		t.Fatalf("response account = %q, want backup", result.Response.AccountID)
	}

	primary, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	if primary.Status != core.AccountStatusActive || primary.CooldownUntil != nil {
		t.Fatalf("primary status=%q cooldown=%#v, want active without cooldown", primary.Status, primary.CooldownUntil)
	}
	if primary.ConsecutiveFails != 0 || primary.TotalFails != 0 {
		t.Fatalf("failure counters = consecutive:%d total:%d, want 0", primary.ConsecutiveFails, primary.TotalFails)
	}
}

func TestExecuteDoesNotReturnNoAccountWhenSharedBreakerCoversAllCandidates(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		sub2APIGatewayTestAccount("low_cost", 120),
		sub2APIGatewayTestAccount("fallback", 110),
		{
			ID:       "backup",
			Provider: core.ProviderOpenAI,
			Label:    "Backup",
			Status:   core.AccountStatusActive,
			Backup:   true,
			Priority: 10,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "backup-token",
				Metadata: map[string]string{
					"account_login_method":   "api_key",
					"api_key_quota_provider": "new-api",
					"base_url":               "https://new-api.example.test",
				},
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
				return nil, &providers.InvokeError{
					Code:      "upstream_temporarily_unavailable",
					Temporary: true,
					Cooldown:  30 * time.Second,
					Err:       errors.New("upstream service temporarily unavailable"),
				}
			},
		}),
	)

	_, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}, nil, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var executionErr *ExecutionError
	if !errors.As(err, &executionErr) {
		t.Fatalf("err = %T %v, want ExecutionError", err, err)
	}
	if len(executionErr.Attempts) != 3 {
		t.Fatalf("attempts = %#v, want all candidates attempted", executionErr.Attempts)
	}
	for i, attempt := range executionErr.Attempts {
		if attempt.Status == "no_account" || attempt.AccountID == "" || attempt.AccountLabel == "" {
			t.Fatalf("attempt %d = %#v, want concrete account failure", i, attempt)
		}
	}
}

func TestImageGenerationDoesNotReturnNoAccountWhenSharedBreakerCoversAllCandidates(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		accountWithQuotaSnapshot(t, sub2APIGatewayTestAccount("low_cost", 120), core.AccountQuotaSnapshot{Image: &core.AccountImageQuota{Remaining: 4}}),
		accountWithQuotaSnapshot(t, sub2APIGatewayTestAccount("fallback", 110), core.AccountQuotaSnapshot{Image: &core.AccountImageQuota{Remaining: 4}}),
		accountWithQuotaSnapshot(t, core.Account{
			ID:       "backup",
			Provider: core.ProviderOpenAI,
			Label:    "Backup",
			Status:   core.AccountStatusActive,
			Backup:   true,
			Priority: 10,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "backup-token",
				Metadata: map[string]string{
					"account_login_method":   "api_key",
					"api_key_quota_provider": "new-api",
					"base_url":               "https://new-api.example.test",
				},
			},
		}, core.AccountQuotaSnapshot{Image: &core.AccountImageQuota{Remaining: 4}}),
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedImageGenerationAdapter{
			scriptedAdapter: &scriptedAdapter{kind: core.ProviderOpenAI},
			generateFn: func(_ context.Context, decision core.RouteDecision, _ *core.ImageGenerationRequest) (*core.ImageGenerationResponse, error) {
				return nil, &providers.InvokeError{
					Code:      "upstream_temporarily_unavailable",
					Temporary: true,
					Cooldown:  30 * time.Second,
					Err:       fmt.Errorf("%s upstream temporarily unavailable", decision.Account.ID),
				}
			},
		}),
	)

	_, err := engine.ExecuteImageGeneration(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-image-2",
		Reason:    "test",
	}, nil, &core.ImageGenerationRequest{Model: "gpt-image-2", Prompt: "test"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var executionErr *ExecutionError
	if !errors.As(err, &executionErr) {
		t.Fatalf("err = %T %v, want ExecutionError", err, err)
	}
	if len(executionErr.Attempts) != 3 {
		t.Fatalf("attempts = %#v, want all image candidates attempted", executionErr.Attempts)
	}
	for i, attempt := range executionErr.Attempts {
		if attempt.Status == "no_account" || attempt.AccountID == "" || attempt.AccountLabel == "" {
			t.Fatalf("attempt %d = %#v, want concrete account failure", i, attempt)
		}
	}
	if count := engine.ImageGenerationCandidateCount(core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-image-2",
		Reason:    "test",
	}, nil); count != 3 {
		t.Fatalf("image candidate count = %d, want 3 despite shared breaker", count)
	}
}

func TestImageMultipartDoesNotReturnNoAccountWhenSharedBreakerCoversAllCandidates(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		accountWithQuotaSnapshot(t, sub2APIGatewayTestAccount("low_cost", 120), core.AccountQuotaSnapshot{Image: &core.AccountImageQuota{Remaining: 4}}),
		accountWithQuotaSnapshot(t, sub2APIGatewayTestAccount("fallback", 110), core.AccountQuotaSnapshot{Image: &core.AccountImageQuota{Remaining: 4}}),
		accountWithQuotaSnapshot(t, core.Account{
			ID:       "backup",
			Provider: core.ProviderOpenAI,
			Label:    "Backup",
			Status:   core.AccountStatusActive,
			Backup:   true,
			Priority: 10,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "backup-token",
				Metadata: map[string]string{
					"account_login_method":   "api_key",
					"api_key_quota_provider": "new-api",
					"base_url":               "https://new-api.example.test",
				},
			},
		}, core.AccountQuotaSnapshot{Image: &core.AccountImageQuota{Remaining: 4}}),
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedImageMultipartAdapter{
			scriptedAdapter: &scriptedAdapter{kind: core.ProviderOpenAI},
			processFn: func(_ context.Context, decision core.RouteDecision, _ *core.ImageMultipartRequest) (*core.ImageMultipartResponse, error) {
				return nil, &providers.InvokeError{
					Code:      "upstream_temporarily_unavailable",
					Temporary: true,
					Cooldown:  30 * time.Second,
					Err:       fmt.Errorf("%s upstream temporarily unavailable", decision.Account.ID),
				}
			},
		}),
	)

	_, err := engine.ExecuteImageMultipart(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-image-2",
		Reason:    "test",
	}, nil, &core.ImageMultipartRequest{Model: "gpt-image-2", Endpoint: "/v1/images/edits", ContentType: "multipart/form-data; boundary=x"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var executionErr *ExecutionError
	if !errors.As(err, &executionErr) {
		t.Fatalf("err = %T %v, want ExecutionError", err, err)
	}
	if len(executionErr.Attempts) != 3 {
		t.Fatalf("attempts = %#v, want all image multipart candidates attempted", executionErr.Attempts)
	}
	for i, attempt := range executionErr.Attempts {
		if attempt.Status == "no_account" || attempt.AccountID == "" || attempt.AccountLabel == "" {
			t.Fatalf("attempt %d = %#v, want concrete account failure", i, attempt)
		}
	}
	if count := engine.ImageMultipartCandidateCount(core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-image-2",
		Reason:    "test",
	}, nil); count != 3 {
		t.Fatalf("image multipart candidate count = %d, want 3 despite shared breaker", count)
	}
}

func TestGatewayMiddleLayerRateLimitDoesNotWriteChatQuotaCooldown(t *testing.T) {
	repo := storage.NewMemoryRepository()
	account := sub2APIGatewayTestAccount("primary", 100)
	if err := repo.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}
	pool := accounts.NewPool(repo)
	engine := NewEngine(pool, providers.NewRegistry())

	engine.markFailure(account, &providers.InvokeError{
		Code:      "upstream_temporarily_unavailable",
		Temporary: true,
		Cooldown:  30 * time.Second,
		Err:       errors.New("Upstream rate limit exceeded, please retry later"),
	})

	stored, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != core.AccountStatusActive || stored.CooldownUntil != nil {
		t.Fatalf("status=%q cooldown=%#v, want active without global cooldown", stored.Status, stored.CooldownUntil)
	}
	if quota := core.ReadAccountQuota(stored); quota != nil {
		t.Fatalf("quota snapshot = %#v, want nil", quota)
	}
	if candidates := pool.Candidates(core.ProviderOpenAI, nil); len(candidates) != 1 || candidates[0].ID != "primary" {
		t.Fatalf("chat candidates = %#v, want primary still routable", candidates)
	}
}

func TestSub2APIGatewayOuterKeyRateLimitStillWritesChatQuotaCooldown(t *testing.T) {
	repo := storage.NewMemoryRepository()
	account := sub2APIGatewayTestAccount("primary", 100)
	if err := repo.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}
	pool := accounts.NewPool(repo)
	engine := NewEngine(pool, providers.NewRegistry())

	engine.markFailure(account, &providers.InvokeError{
		Code:      "upstream_rate_limited",
		Temporary: true,
		Cooldown:  45 * time.Second,
		Err:       errors.New("api key daily limit exhausted"),
	})

	stored, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != core.AccountStatusActive || stored.CooldownUntil != nil {
		t.Fatalf("status=%q cooldown=%#v, want active without global cooldown", stored.Status, stored.CooldownUntil)
	}
	quota := core.ReadAccountQuota(stored)
	if quota == nil ||
		quota.Additional == nil ||
		quota.Additional[core.AccountQuotaRuntimeChatLimitID].Primary == nil ||
		quota.Additional[core.AccountQuotaRuntimeChatLimitID].Primary.ResetsAt == nil {
		t.Fatalf("chat quota snapshot = %#v", quota)
	}
	if candidates := pool.Candidates(core.ProviderOpenAI, nil); len(candidates) != 0 {
		t.Fatalf("chat candidates = %#v, want none while outer key rate limit is active", candidates)
	}
}

func TestOfficialUpstreamServerErrorDoesNotCoolAccount(t *testing.T) {
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
			AccessToken: "primary-token",
			Metadata: map[string]string{
				"account_login_method": "api_key",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(accounts.NewPool(repo), providers.NewRegistry())

	engine.markFailure(core.Account{ID: "primary"}, &providers.InvokeError{
		Code:      "upstream_server_error",
		Temporary: true,
		Cooldown:  30 * time.Second,
		Err:       errors.New("Upstream service temporarily unavailable"),
	})

	stored, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != core.AccountStatusActive || stored.CooldownUntil != nil {
		t.Fatalf("status=%q cooldown=%#v, want active without cooldown", stored.Status, stored.CooldownUntil)
	}
	if stored.ConsecutiveFails != 0 || stored.TotalFails != 0 {
		t.Fatalf("failure counters = consecutive:%d total:%d, want 0", stored.ConsecutiveFails, stored.TotalFails)
	}
}

func TestSub2APIGatewayGenericServerErrorDoesNotCoolAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	account := sub2APIGatewayTestAccount("primary", 100)
	if err := repo.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(accounts.NewPool(repo), providers.NewRegistry())

	engine.markFailure(account, &providers.InvokeError{
		Code:      "upstream_server_error",
		Temporary: true,
		Cooldown:  30 * time.Second,
		Err:       errors.New("bad gateway"),
	})

	stored, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != core.AccountStatusActive || stored.CooldownUntil != nil {
		t.Fatalf("status=%q cooldown=%#v, want active without cooldown", stored.Status, stored.CooldownUntil)
	}
	if stored.ConsecutiveFails != 0 || stored.TotalFails != 0 {
		t.Fatalf("failure counters = consecutive:%d total:%d, want 0", stored.ConsecutiveFails, stored.TotalFails)
	}
}

func TestNewAPITopLevelChannelExhaustionFallsBackToBackupAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{
			ID:       "primary",
			Provider: core.ProviderOpenAI,
			Label:    "Primary",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "primary-token",
				Metadata: map[string]string{
					"account_login_method":   "api_key",
					"api_key_quota_provider": "new-api",
					"base_url":               "https://new-api.example.test",
				},
			},
		},
		{
			ID:       "backup",
			Provider: core.ProviderOpenAI,
			Label:    "Backup",
			Status:   core.AccountStatusActive,
			Priority: 90,
			Weight:   90,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "backup-token",
				Metadata: map[string]string{
					"account_login_method": "api_key",
				},
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
				if decision.Account.ID == "primary" {
					return nil, &providers.InvokeError{
						Code:      "upstream_temporarily_unavailable",
						Temporary: true,
						Cooldown:  30 * time.Second,
						Err:       errors.New("no available channel"),
					}
				}
				return &core.GatewayResponse{
					ID:           "resp_backup",
					Model:        decision.Model,
					Provider:     decision.Provider,
					AccountID:    decision.Account.ID,
					AccountLabel: decision.Account.Label,
					Content:      "ok",
					FinishReason: "stop",
					CreatedAt:    time.Now().UTC(),
				}, nil
			},
		}),
	)

	result, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}, nil, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Response.AccountID != "backup" {
		t.Fatalf("response account = %q, want backup", result.Response.AccountID)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %#v, want primary failure then backup success", result.Attempts)
	}
	if result.Attempts[0].ErrorCode != "upstream_temporarily_unavailable" {
		t.Fatalf("first attempt = %#v", result.Attempts[0])
	}
}

func TestSub2APIGatewayTransportErrorDoesNotCoolAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	account := sub2APIGatewayTestAccount("primary", 100)
	if err := repo.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(accounts.NewPool(repo), providers.NewRegistry())
	err := &providers.InvokeError{
		Code:      "upstream_transport_error",
		Temporary: true,
		Cooldown:  20 * time.Second,
		Err:       errors.New("connection refused"),
	}

	engine.markFailure(account, err)

	stored, getErr := repo.GetAccount("primary")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.Status != core.AccountStatusActive || stored.CooldownUntil != nil {
		t.Fatalf("status=%q cooldown=%#v, want active without cooldown", stored.Status, stored.CooldownUntil)
	}
	if stored.ConsecutiveFails != 0 || stored.TotalFails != 0 {
		t.Fatalf("failure counters = consecutive:%d total:%d, want 0", stored.ConsecutiveFails, stored.TotalFails)
	}
}

func TestMarkImageFailureScopesRateLimitToImageQuota(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
	}); err != nil {
		t.Fatal(err)
	}
	pool := accounts.NewPool(repo)
	engine := NewEngine(pool, providers.NewRegistry())

	engine.markImageFailure(core.Account{ID: "primary"}, rateLimitError())

	account, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != core.AccountStatusActive || account.CooldownUntil != nil {
		t.Fatalf("status=%q cooldown=%#v, want active without global cooldown", account.Status, account.CooldownUntil)
	}
	quota := core.ReadAccountQuota(account)
	if quota == nil ||
		quota.Additional == nil ||
		quota.Additional[core.AccountQuotaRuntimeImageLimitID].Image == nil ||
		quota.Additional[core.AccountQuotaRuntimeImageLimitID].Image.Remaining != 0 ||
		quota.Additional[core.AccountQuotaRuntimeImageLimitID].Image.ResetsAt == nil {
		t.Fatalf("image quota snapshot = %#v", quota)
	}
	if candidates := pool.ImageCandidates(core.ProviderOpenAI, nil); len(candidates) != 0 {
		t.Fatalf("image candidates = %#v, want none while image quota is scoped limited", candidates)
	}
	if candidates := pool.Candidates(core.ProviderOpenAI, nil); len(candidates) != 1 || candidates[0].ID != "primary" {
		t.Fatalf("chat candidates = %#v, want chat routing unaffected by image rate limit", candidates)
	}
}

func TestRecordImageStreamFailureScopesRateLimitToImageQuota(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
	}); err != nil {
		t.Fatal(err)
	}
	pool := accounts.NewPool(repo)
	engine := NewEngine(pool, providers.NewRegistry())

	engine.RecordStreamFailure(&StreamOpenResult{
		Modality: failureModalityImage,
		Attempts: []core.AttemptRecord{{
			Provider:     core.ProviderOpenAI,
			AccountID:    "primary",
			AccountLabel: "Primary",
			Status:       "ok",
		}},
	}, rateLimitError())

	account, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != core.AccountStatusActive || account.CooldownUntil != nil {
		t.Fatalf("status=%q cooldown=%#v, want active without global cooldown", account.Status, account.CooldownUntil)
	}
	quota := core.ReadAccountQuota(account)
	if quota == nil ||
		quota.Additional == nil ||
		quota.Additional[core.AccountQuotaRuntimeImageLimitID].Image == nil ||
		quota.Additional[core.AccountQuotaRuntimeImageLimitID].Image.Remaining != 0 ||
		quota.Additional[core.AccountQuotaRuntimeImageLimitID].Image.ResetsAt == nil {
		t.Fatalf("image quota snapshot = %#v", quota)
	}
	if candidates := pool.ImageCandidates(core.ProviderOpenAI, nil); len(candidates) != 0 {
		t.Fatalf("image candidates = %#v, want none while image stream quota is scoped limited", candidates)
	}
	if candidates := pool.Candidates(core.ProviderOpenAI, nil); len(candidates) != 1 || candidates[0].ID != "primary" {
		t.Fatalf("chat candidates = %#v, want chat routing unaffected by image stream rate limit", candidates)
	}
}

func TestRecordResponsesWebSocketFailureMarksSelectedAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
	}); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(accounts.NewPool(repo), providers.NewRegistry())

	engine.RecordResponsesWebSocketFailure(core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "primary",
			Provider: core.ProviderOpenAI,
			Label:    "Primary",
		},
	}, &providers.InvokeError{
		Code:      "upstream_read_error",
		Temporary: true,
		Cooldown:  20 * time.Second,
		Err:       errors.New("websocket read failed"),
	})

	account, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != core.AccountStatusActive || account.CooldownUntil != nil {
		t.Fatalf("status=%q cooldown=%#v, want active without cooldown", account.Status, account.CooldownUntil)
	}
	if account.ConsecutiveFails != 0 || account.TotalFails != 0 {
		t.Fatalf("failure counters = consecutive:%d total:%d, want 0", account.ConsecutiveFails, account.TotalFails)
	}
}

func TestImageRefreshRateLimitUsesAccountCooldown(t *testing.T) {
	repo := storage.NewMemoryRepository()
	expired := time.Now().UTC().Add(-time.Minute)
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:         providers.OpenAIOAuthModeValue(),
			AccessToken:  "expired-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    &expired,
			Metadata:     map[string]string{"token_source": providers.OpenAIDeviceCodeTokenSourceValue()},
		},
	}); err != nil {
		t.Fatal(err)
	}
	pool := accounts.NewPool(repo)
	engine := NewEngine(
		pool,
		providers.NewRegistry(&scriptedImageGenerationAdapter{
			scriptedAdapter: &scriptedAdapter{kind: core.ProviderOpenAI},
			refreshFn: func(context.Context, core.Account) (core.Credential, error) {
				return core.Credential{}, rateLimitError()
			},
		}),
	)

	_, err := engine.ExecuteImageGeneration(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-image-2",
		Reason:    "test",
	}, nil, &core.ImageGenerationRequest{Model: "gpt-image-2", Prompt: "test"})
	if err == nil {
		t.Fatal("ExecuteImageGeneration returned nil error, want refresh failure")
	}
	account, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != core.AccountStatusCooling || account.CooldownUntil == nil || !account.CooldownUntil.After(time.Now().UTC()) {
		t.Fatalf("status=%q cooldown=%#v, want account-level cooldown", account.Status, account.CooldownUntil)
	}
	quota := core.ReadAccountQuota(account)
	if quota != nil && quota.Image != nil {
		t.Fatalf("image quota should not be marked for credential refresh rate limits: %#v", quota)
	}
	if candidates := pool.ImageCandidates(core.ProviderOpenAI, nil); len(candidates) != 0 {
		t.Fatalf("image candidates = %#v, want none while account refresh is cooling", candidates)
	}
	if candidates := pool.Candidates(core.ProviderOpenAI, nil); len(candidates) != 0 {
		t.Fatalf("chat candidates = %#v, want none while account refresh is cooling", candidates)
	}
}

func TestImageRefreshErrorNotifiesAttemptFinished(t *testing.T) {
	repo := storage.NewMemoryRepository()
	expired := time.Now().UTC().Add(-time.Minute)
	if err := repo.UpsertAccount(core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:         providers.OpenAIOAuthModeValue(),
			AccessToken:  "expired-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    &expired,
			Metadata:     map[string]string{"token_source": providers.OpenAIDeviceCodeTokenSourceValue()},
		},
	}); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedImageGenerationAdapter{
			scriptedAdapter: &scriptedAdapter{kind: core.ProviderOpenAI},
			refreshFn: func(context.Context, core.Account) (core.Credential, error) {
				return core.Credential{}, rateLimitError()
			},
		}),
	)
	observer := &recordingAttemptObserver{}
	ctx := WithAttemptObserver(context.Background(), observer)

	_, err := engine.ExecuteImageGeneration(ctx, core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-image-2",
		Reason:    "test",
	}, nil, &core.ImageGenerationRequest{Model: "gpt-image-2", Prompt: "test"})
	if err == nil {
		t.Fatal("ExecuteImageGeneration returned nil error, want refresh failure")
	}
	started := observer.startedAttempts()
	if len(started) != 1 || started[0].Status != "running" || started[0].AccountID != "primary" {
		t.Fatalf("started attempts = %#v, want primary running before refresh", started)
	}
	finished := observer.finishedAttempts()
	if len(finished) != 1 {
		t.Fatalf("finished attempts = %#v, want one refresh_error", finished)
	}
	if finished[0].Status != "refresh_error" || finished[0].AccountID != "primary" || finished[0].ErrorCode != "upstream_rate_limited" {
		t.Fatalf("finished attempt = %#v, want primary refresh_error", finished[0])
	}
}

func TestImageGenerationSuccessDecrementsImageQuotaByRequestCount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(accountWithQuotaSnapshot(t, core.Account{
		ID:       "primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
	}, core.AccountQuotaSnapshot{
		Image: &core.AccountImageQuota{Remaining: 5},
	})); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedImageGenerationAdapter{
			scriptedAdapter: &scriptedAdapter{kind: core.ProviderOpenAI},
		}),
	)

	result, err := engine.ExecuteImageGeneration(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-image-2",
		Reason:    "test",
	}, nil, &core.ImageGenerationRequest{
		Model:  "gpt-image-2",
		Prompt: "test",
		Extra:  map[string]json.RawMessage{"n": json.RawMessage("2")},
	})
	if err != nil {
		t.Fatalf("ExecuteImageGeneration returned error: %v", err)
	}
	if result.Response.AccountID != "primary" {
		t.Fatalf("account = %q, want primary", result.Response.AccountID)
	}
	account, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	quota := core.ReadAccountQuota(account)
	if quota == nil || quota.Image == nil || quota.Image.Remaining != 3 {
		t.Fatalf("image quota after success = %#v, want remaining 3", quota)
	}
}

func TestImageGenerationConcurrentRequestsUseSeparateImageAccounts(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		accountWithQuotaSnapshot(t, core.Account{
			ID:       "acct_remaining_9",
			Provider: core.ProviderOpenAI,
			Label:    "Remaining 9",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
		}, core.AccountQuotaSnapshot{Image: &core.AccountImageQuota{Remaining: 9}}),
		accountWithQuotaSnapshot(t, core.Account{
			ID:       "acct_remaining_4",
			Provider: core.ProviderOpenAI,
			Label:    "Remaining 4",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
		}, core.AccountQuotaSnapshot{Image: &core.AccountImageQuota{Remaining: 4}}),
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	started := make(chan string, 2)
	release := make(chan struct{})
	adapter := &scriptedImageGenerationAdapter{
		scriptedAdapter: &scriptedAdapter{kind: core.ProviderOpenAI},
		generateFn: func(ctx context.Context, decision core.RouteDecision, _ *core.ImageGenerationRequest) (*core.ImageGenerationResponse, error) {
			started <- decision.Account.ID
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &core.ImageGenerationResponse{
				Model:        decision.Model,
				Provider:     decision.Provider,
				AccountID:    decision.Account.ID,
				AccountLabel: decision.Account.Label,
				Body:         []byte(`{"created":1710000000,"data":[{"url":"https://example.com/image.png"}]}`),
			}, nil
		},
	}
	engine := NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := engine.ExecuteImageGeneration(ctx, core.RoutePlan{
				Providers: []core.ProviderKind{core.ProviderOpenAI},
				Model:     "gpt-image-2",
				Reason:    "test",
			}, nil, &core.ImageGenerationRequest{Model: "gpt-image-2", Prompt: "test"})
			errs <- err
		}()
	}

	seen := make(map[string]struct{}, 2)
	for len(seen) < 2 {
		select {
		case accountID := <-started:
			seen[accountID] = struct{}{}
		case <-time.After(time.Second):
			close(release)
			t.Fatalf("started accounts = %#v, want two distinct accounts", seen)
		}
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("ExecuteImageGeneration returned error: %v", err)
		}
	}
	if _, ok := seen["acct_remaining_9"]; !ok {
		t.Fatalf("started accounts = %#v, want quota-rich account included", seen)
	}
	if _, ok := seen["acct_remaining_4"]; !ok {
		t.Fatalf("started accounts = %#v, want second account included", seen)
	}
}

func TestImageGenerationConcurrentRequestsWaitForBusyImageAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(accountWithQuotaSnapshot(t, core.Account{
		ID:       "acct_single",
		Provider: core.ProviderOpenAI,
		Label:    "Single",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
	}, core.AccountQuotaSnapshot{Image: &core.AccountImageQuota{Remaining: 4}})); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	active := 0
	maxActive := 0
	calls := 0
	adapter := &scriptedImageGenerationAdapter{
		scriptedAdapter: &scriptedAdapter{kind: core.ProviderOpenAI},
		generateFn: func(ctx context.Context, decision core.RouteDecision, _ *core.ImageGenerationRequest) (*core.ImageGenerationResponse, error) {
			mu.Lock()
			active++
			calls++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			select {
			case <-time.After(50 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			mu.Lock()
			active--
			mu.Unlock()
			return &core.ImageGenerationResponse{
				Model:        decision.Model,
				Provider:     decision.Provider,
				AccountID:    decision.Account.ID,
				AccountLabel: decision.Account.Label,
				Body:         []byte(`{"created":1710000000,"data":[{"url":"https://example.com/image.png"}]}`),
			}, nil
		},
	}
	engine := NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := engine.ExecuteImageGeneration(ctx, core.RoutePlan{
				Providers: []core.ProviderKind{core.ProviderOpenAI},
				Model:     "gpt-image-2",
				Reason:    "test",
			}, nil, &core.ImageGenerationRequest{Model: "gpt-image-2", Prompt: "test"})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("ExecuteImageGeneration returned error: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if maxActive != 1 {
		t.Fatalf("max active calls = %d, want one in-flight request per account", maxActive)
	}
}

func TestImageGenerationWaitsForBusyPrimaryBeforeUsingBackup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		accountWithQuotaSnapshot(t, core.Account{
			ID:       "primary",
			Provider: core.ProviderOpenAI,
			Label:    "Primary",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
		}, core.AccountQuotaSnapshot{Image: &core.AccountImageQuota{Remaining: 4}}),
		accountWithQuotaSnapshot(t, core.Account{
			ID:       "backup",
			Provider: core.ProviderOpenAI,
			Label:    "Backup",
			Status:   core.AccountStatusActive,
			Backup:   true,
			Priority: 1000,
			Weight:   1000,
		}, core.AccountQuotaSnapshot{Image: &core.AccountImageQuota{Remaining: 4}}),
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	started := make(chan string, 4)
	release := make(chan struct{})
	adapter := &scriptedImageGenerationAdapter{
		scriptedAdapter: &scriptedAdapter{kind: core.ProviderOpenAI},
		generateFn: func(ctx context.Context, decision core.RouteDecision, _ *core.ImageGenerationRequest) (*core.ImageGenerationResponse, error) {
			started <- decision.Account.ID
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &core.ImageGenerationResponse{
				Model:        decision.Model,
				Provider:     decision.Provider,
				AccountID:    decision.Account.ID,
				AccountLabel: decision.Account.Label,
				Body:         []byte(`{"created":1710000000,"data":[{"url":"https://example.com/image.png"}]}`),
			}, nil
		},
	}
	engine := NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	errs := make(chan error, 2)
	run := func() {
		_, err := engine.ExecuteImageGeneration(ctx, core.RoutePlan{
			Providers: []core.ProviderKind{core.ProviderOpenAI},
			Model:     "gpt-image-2",
			Reason:    "test",
		}, nil, &core.ImageGenerationRequest{Model: "gpt-image-2", Prompt: "test"})
		errs <- err
	}

	go run()
	select {
	case accountID := <-started:
		if accountID != "primary" {
			close(release)
			t.Fatalf("first account = %q, want primary", accountID)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("first image request did not start")
	}

	go run()
	select {
	case accountID := <-started:
		close(release)
		t.Fatalf("second request started %q while primary was busy; want it to wait", accountID)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case accountID := <-started:
		if accountID != "primary" {
			t.Fatalf("second account = %q, want primary after release", accountID)
		}
	case <-time.After(time.Second):
		t.Fatal("second image request did not start after primary release")
	}

	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("ExecuteImageGeneration returned error: %v", err)
		}
	}
}

func TestWaitForImageAccountReturnsWhenPendingAccountAlreadyFree(t *testing.T) {
	engine := NewEngine(nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := engine.waitForImageAccount(ctx, []string{"acct_free"}); err != nil {
		t.Fatalf("waitForImageAccount returned error: %v", err)
	}
}

func TestImageGenerationStopsFailoverOnRequestScopedFailure(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{ID: "primary", Provider: core.ProviderOpenAI, Label: "Primary", Status: core.AccountStatusActive, Priority: 100, Weight: 100},
		{ID: "secondary", Provider: core.ProviderOpenAI, Label: "Secondary", Status: core.AccountStatusActive, Priority: 90, Weight: 100},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedImageGenerationAdapter{
			scriptedAdapter: &scriptedAdapter{kind: core.ProviderOpenAI},
			generateFn: func(context.Context, core.RouteDecision, *core.ImageGenerationRequest) (*core.ImageGenerationResponse, error) {
				calls.Add(1)
				return nil, &providers.InvokeError{
					Code:      "image_generation_rejected",
					Temporary: false,
					Err:       errors.New("content policy rejected the image request"),
				}
			},
		}),
	)

	_, err := engine.ExecuteImageGeneration(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-image-2",
		Reason:    "test",
	}, nil, &core.ImageGenerationRequest{Model: "gpt-image-2", Prompt: "blocked"})
	if err == nil {
		t.Fatal("ExecuteImageGeneration returned nil error, want rejection")
	}
	var executionErr *ExecutionError
	if !errors.As(err, &executionErr) {
		t.Fatalf("err = %T %v, want ExecutionError", err, err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
	if len(executionErr.Attempts) != 1 || executionErr.Attempts[0].AccountID != "primary" || executionErr.Attempts[0].ErrorCode != "image_generation_rejected" {
		t.Fatalf("attempts = %#v, want only primary rejection", executionErr.Attempts)
	}
}

func TestImageGenerationSkipsBackendMismatchAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{
			ID:       "api_key",
			Provider: core.ProviderOpenAI,
			Label:    "API Key",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
		},
		{
			ID:       "oauth",
			Provider: core.ProviderOpenAI,
			Label:    "OAuth",
			Status:   core.AccountStatusActive,
			Priority: 90,
			Weight:   100,
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedImageGenerationAdapter{
			scriptedAdapter: &scriptedAdapter{kind: core.ProviderOpenAI},
			generateFn: func(_ context.Context, decision core.RouteDecision, _ *core.ImageGenerationRequest) (*core.ImageGenerationResponse, error) {
				if decision.Account.ID == "api_key" {
					return nil, &providers.InvokeError{
						Code:      "image_backend_requires_oauth",
						Temporary: false,
						Err:       errors.New("image backend requires oauth"),
					}
				}
				return &core.ImageGenerationResponse{
					Model:        decision.Model,
					Provider:     decision.Provider,
					AccountID:    decision.Account.ID,
					AccountLabel: decision.Account.Label,
					Body:         []byte(`{"created":1710000000,"data":[{"url":"https://example.com/image.png"}]}`),
				}, nil
			},
		}),
	)

	result, err := engine.ExecuteImageGeneration(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-image-2",
		Reason:    "test",
	}, nil, &core.ImageGenerationRequest{Model: "gpt-image-2", Prompt: "test"})
	if err != nil {
		t.Fatalf("ExecuteImageGeneration returned error: %v", err)
	}
	if result.Response.AccountID != "oauth" {
		t.Fatalf("account id = %q, want oauth", result.Response.AccountID)
	}
	if len(result.Attempts) != 2 || result.Attempts[0].ErrorCode != "image_backend_requires_oauth" || result.Attempts[1].AccountID != "oauth" {
		t.Fatalf("attempts = %#v", result.Attempts)
	}
	apiKey, err := repo.GetAccount("api_key")
	if err != nil {
		t.Fatal(err)
	}
	if apiKey.ConsecutiveFails != 0 || apiKey.TotalFails != 0 || apiKey.CooldownUntil != nil {
		t.Fatalf("api key account was penalized: %#v", apiKey)
	}
}

func rateLimitError() error {
	return &providers.InvokeError{
		Code:      "upstream_rate_limited",
		Temporary: true,
		Cooldown:  45 * time.Second,
		Err:       errors.New("rate limited"),
	}
}

func sub2APIGatewayTestAccount(id string, priority int) core.Account {
	return core.Account{
		ID:       id,
		Provider: core.ProviderOpenAI,
		Label:    id,
		Status:   core.AccountStatusActive,
		Priority: priority,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: id + "-token",
			Metadata: map[string]string{
				"account_login_method":   "api_key",
				"api_key_quota_provider": "sub2api",
				"base_url":               "https://sub2api.example.test",
			},
		},
	}
}

func accountWithQuotaSnapshot(t *testing.T, account core.Account, snapshot core.AccountQuotaSnapshot) core.Account {
	t.Helper()
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if account.Credential.Metadata == nil {
		account.Credential.Metadata = map[string]string{}
	}
	account.Credential.Metadata[core.AccountQuotaMetadataKey] = string(raw)
	return account
}

func TestExecuteReturnsStructuredAttemptsWhenAllUpstreamsFail(t *testing.T) {
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
			AccessToken: "primary-token",
		},
	}); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(context.Context, core.RouteDecision, *core.GatewayRequest) (*core.GatewayResponse, error) {
				return nil, temporaryInvokeError("primary unavailable")
			},
		}),
	)

	_, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI, core.ProviderClaude},
		Model:     "gpt-4.1",
		Reason:    "test",
	}, nil, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var executionErr *ExecutionError
	if !errors.As(err, &executionErr) {
		t.Fatalf("expected ExecutionError, got %T", err)
	}
	if len(executionErr.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(executionErr.Attempts))
	}
	if executionErr.Attempts[0].ErrorCode != "upstream_temporarily_unavailable" {
		t.Fatalf("unexpected first error code: %q", executionErr.Attempts[0].ErrorCode)
	}
	if executionErr.Attempts[1].Status != "no_adapter" {
		t.Fatalf("unexpected second attempt status: %q", executionErr.Attempts[1].Status)
	}
}

func TestExecuteFallsBackAndCoolsGatewayAPIKeyDisabled(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{
			ID:       "primary",
			Provider: core.ProviderOpenAI,
			Label:    "Primary",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "primary-token",
				Metadata: map[string]string{
					"account_login_method":   "api_key",
					"api_key_quota_provider": "sub2api",
					"base_url":               "https://gpt.qinnaonao.com",
				},
			},
		},
		{
			ID:       "backup",
			Provider: core.ProviderOpenAI,
			Label:    "Backup",
			Status:   core.AccountStatusActive,
			Priority: 50,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "backup-token",
				Metadata: map[string]string{
					"account_login_method": "api_key",
				},
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
				if decision.Account.ID == "primary" {
					return nil, &providers.InvokeError{
						Code:      "gateway_api_key_disabled",
						Temporary: true,
						Cooldown:  2 * time.Minute,
						Err:       errors.New("API key is disabled"),
					}
				}
				return &core.GatewayResponse{
					ID:           "resp_backup",
					Model:        decision.Model,
					Provider:     decision.Provider,
					AccountID:    decision.Account.ID,
					AccountLabel: decision.Account.Label,
					Content:      "ok",
					FinishReason: "stop",
					CreatedAt:    time.Now().UTC(),
				}, nil
			},
		}),
	)

	result, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-5.4",
		Reason:    "test",
	}, nil, &core.GatewayRequest{Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Response.AccountID != "backup" {
		t.Fatalf("response account = %q, want backup", result.Response.AccountID)
	}
	if len(result.Attempts) != 2 || result.Attempts[0].ErrorCode != "gateway_api_key_disabled" || result.Attempts[1].AccountID != "backup" {
		t.Fatalf("attempts = %#v", result.Attempts)
	}
	primary, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	if primary.Status != core.AccountStatusCooling || primary.CooldownUntil == nil {
		t.Fatalf("primary status = %q cooldown=%v, want cooling with cooldown", primary.Status, primary.CooldownUntil)
	}
	if primary.Status == core.AccountStatusExpired {
		t.Fatal("gateway API key disabled must not expire the account")
	}
}

func TestExecuteFallsBackAndCoolsAccountScopedUpstreamRejected(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{
			ID:       "primary",
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
			ID:       "backup",
			Provider: core.ProviderOpenAI,
			Label:    "Backup",
			Status:   core.AccountStatusActive,
			Priority: 50,
			Weight:   100,
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

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
				if decision.Account.ID == "primary" {
					return nil, &providers.InvokeError{
						Code:      "upstream_rejected",
						Temporary: false,
						Cooldown:  2 * time.Minute,
						Err:       errors.New("antigravity auth missing project_id: PERMISSION_DENIED: This service has been disabled in this account"),
					}
				}
				return &core.GatewayResponse{
					ID:           "resp_backup",
					Model:        decision.Model,
					Provider:     decision.Provider,
					AccountID:    decision.Account.ID,
					AccountLabel: decision.Account.Label,
					Content:      "ok",
					FinishReason: "stop",
					CreatedAt:    time.Now().UTC(),
				}, nil
			},
		}),
	)

	result, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-5.5",
		Reason:    "test",
	}, nil, &core.GatewayRequest{Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Response.AccountID != "backup" {
		t.Fatalf("response account = %q, want backup", result.Response.AccountID)
	}
	if len(result.Attempts) != 2 || result.Attempts[0].ErrorCode != "upstream_rejected" || result.Attempts[1].AccountID != "backup" {
		t.Fatalf("attempts = %#v", result.Attempts)
	}
	primary, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	if primary.Status != core.AccountStatusCooling || primary.CooldownUntil == nil {
		t.Fatalf("primary status = %q cooldown=%v, want cooling with cooldown", primary.Status, primary.CooldownUntil)
	}
}

func TestExecuteDoesNotCoolAccountOnRequestScopedRejection(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{
			ID:       "primary",
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
			ID:       "backup",
			Provider: core.ProviderOpenAI,
			Label:    "Backup",
			Status:   core.AccountStatusActive,
			Priority: 90,
			Weight:   100,
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

	var calls atomic.Int32
	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(context.Context, core.RouteDecision, *core.GatewayRequest) (*core.GatewayResponse, error) {
				calls.Add(1)
				return nil, permanentInvokeError("primary rejected")
			},
		}),
	)

	_, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}, nil, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var executionErr *ExecutionError
	if !errors.As(err, &executionErr) {
		t.Fatalf("err = %T %v, want ExecutionError", err, err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
	if len(executionErr.Attempts) != 1 || executionErr.Attempts[0].AccountID != "primary" {
		t.Fatalf("attempts = %#v, want only primary rejection", executionErr.Attempts)
	}

	primary, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	if primary.Status != core.AccountStatusActive {
		t.Fatalf("expected primary to remain active, got %s", primary.Status)
	}
	if primary.CooldownUntil != nil {
		t.Fatalf("expected no cooldown, got %#v", primary.CooldownUntil)
	}
	if primary.ConsecutiveFails != 0 || primary.TotalFails != 0 {
		t.Fatalf("failure counters = consecutive:%d total:%d, want 0", primary.ConsecutiveFails, primary.TotalFails)
	}
	backup, err := repo.GetAccount("backup")
	if err != nil {
		t.Fatal(err)
	}
	if backup.LastUsedAt != nil {
		t.Fatalf("backup was attempted unexpectedly: %#v", backup.LastUsedAt)
	}
}

func TestExecuteDoesNotFailOverOnRequestScopedTemporaryError(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{
			ID:       "primary",
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
			ID:       "backup",
			Provider: core.ProviderOpenAI,
			Label:    "Backup",
			Status:   core.AccountStatusActive,
			Priority: 90,
			Weight:   100,
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

	var calls atomic.Int32
	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(context.Context, core.RouteDecision, *core.GatewayRequest) (*core.GatewayResponse, error) {
				calls.Add(1)
				return nil, &providers.InvokeError{
					Code:      "upstream_request_build_failed",
					Temporary: true,
					Cooldown:  10 * time.Second,
					Err:       errors.New("invalid request body"),
				}
			},
		}),
	)

	_, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}, nil, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var executionErr *ExecutionError
	if !errors.As(err, &executionErr) {
		t.Fatalf("err = %T %v, want ExecutionError", err, err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
	if len(executionErr.Attempts) != 1 || executionErr.Attempts[0].AccountID != "primary" {
		t.Fatalf("attempts = %#v, want only primary request error", executionErr.Attempts)
	}

	primary, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	if primary.ConsecutiveFails != 0 || primary.TotalFails != 0 {
		t.Fatalf("failure counters = consecutive:%d total:%d, want 0", primary.ConsecutiveFails, primary.TotalFails)
	}
	backup, err := repo.GetAccount("backup")
	if err != nil {
		t.Fatal(err)
	}
	if backup.LastUsedAt != nil {
		t.Fatalf("backup was attempted unexpectedly: %#v", backup.LastUsedAt)
	}
}

func TestExecuteExpiresAccountWhenCredentialMissing(t *testing.T) {
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
			AccessToken: "",
		},
	}); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&providers.OpenAIAdapter{}),
	)

	_, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}, nil, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	primary, err := repo.GetAccount("primary")
	if err != nil {
		t.Fatal(err)
	}
	if primary.Status != core.AccountStatusExpired {
		t.Fatalf("expected primary to be expired, got %s", primary.Status)
	}
}

func TestExecuteRespectsClientAccountGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_primary",
		Provider: core.ProviderOpenAI,
		Label:    "Primary",
		Group:    "Basic",
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
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_bound",
		Provider: core.ProviderOpenAI,
		Label:    "Bound",
		Group:    "Plus",
		Status:   core.AccountStatusActive,
		Priority: 50,
		Weight:   50,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "bound-token",
		},
	}); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{kind: core.ProviderOpenAI}),
	)

	result, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}, &core.APIClient{
		ID:           "client_bound",
		AccountGroup: "Plus",
	}, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	resp := result.Response
	if resp.AccountID != "acct_bound" {
		t.Fatalf("expected bound account, got %s", resp.AccountID)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].Status != "ok" {
		t.Fatalf("unexpected attempts: %#v", result.Attempts)
	}
}

func TestExecuteDoesNotEscapeClientAccountGroupOnFailure(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_bound",
		Provider: core.ProviderOpenAI,
		Label:    "Bound",
		Group:    "Plus",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "bound-token",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_unbound",
		Provider: core.ProviderOpenAI,
		Label:    "Unbound",
		Group:    "Basic",
		Status:   core.AccountStatusActive,
		Priority: 90,
		Weight:   90,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "unbound-token",
		},
	}); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{
			kind: core.ProviderOpenAI,
			invokeFn: func(context.Context, core.RouteDecision, *core.GatewayRequest) (*core.GatewayResponse, error) {
				return nil, temporaryInvokeError("bound account unavailable")
			},
		}),
	)

	_, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}, &core.APIClient{
		ID:           "client_bound",
		AccountGroup: "Plus",
	}, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var executionErr *ExecutionError
	if !errors.As(err, &executionErr) {
		t.Fatalf("expected ExecutionError, got %T", err)
	}
	if len(executionErr.Attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %d", len(executionErr.Attempts))
	}
	if executionErr.Attempts[0].AccountID != "acct_bound" {
		t.Fatalf("attempt account = %q, want %q", executionErr.Attempts[0].AccountID, "acct_bound")
	}
}

func TestExecuteEmbeddingFallsBackAfterUnsupportedProvider(t *testing.T) {
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
		},
	}); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(
			&scriptedEmbeddingAdapter{scriptedAdapter: &scriptedAdapter{kind: core.ProviderOpenAI}},
			&scriptedAdapter{kind: core.ProviderClaude},
		),
	)

	result, err := engine.ExecuteEmbedding(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderClaude, core.ProviderOpenAI},
		Model:     "text-embedding-3-small",
		Reason:    "test",
	}, nil, &core.EmbeddingRequest{
		Model:      "text-embedding-3-small",
		Input:      []string{"hello"},
		Dimensions: intPtr(3),
	})
	if err != nil {
		t.Fatalf("ExecuteEmbedding returned error: %v", err)
	}
	if result.Response.AccountID != "acct_openai" {
		t.Fatalf("account id = %q", result.Response.AccountID)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %#v", result.Attempts)
	}
	if result.Attempts[0].Status != "embedding_unsupported" || result.Attempts[0].Provider != core.ProviderClaude {
		t.Fatalf("unexpected first attempt: %#v", result.Attempts[0])
	}
	if result.Attempts[1].Status != "ok" || result.Attempts[1].AccountID != "acct_openai" {
		t.Fatalf("unexpected second attempt: %#v", result.Attempts[1])
	}
}

func TestExecuteResponsesStrictAccountAffinityBypassesCandidateLimit(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for i := 0; i < 20; i++ {
		account := core.Account{
			ID:       fmt.Sprintf("acct_%02d", i),
			Provider: core.ProviderOpenAI,
			Label:    fmt.Sprintf("Account %02d", i),
			Group:    "Plus",
			Status:   core.AccountStatusActive,
			Priority: 100 - i,
			Weight:   100,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: fmt.Sprintf("token_%02d", i),
			},
		}
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedResponsesAdapter{
			scriptedAdapter: &scriptedAdapter{kind: core.ProviderOpenAI},
		}),
	)
	result, err := engine.ExecuteResponses(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-5.4",
		Reason:    "test",
	}, &core.APIClient{
		ID:           "client_plus",
		AccountGroup: "Plus",
	}, &core.ResponsesRequest{
		Model:                 "gpt-5.4",
		StrictAccountAffinity: true,
		PreferredAccountID:    "acct_19",
	})
	if err != nil {
		t.Fatalf("ExecuteResponses returned error: %v", err)
	}
	if result.Response.AccountID != "acct_19" {
		t.Fatalf("account id = %q, want acct_19", result.Response.AccountID)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].AccountID != "acct_19" {
		t.Fatalf("attempts = %#v", result.Attempts)
	}
}

func TestExecuteRefreshesExpiredCredentialBeforeInvoke(t *testing.T) {
	repo := storage.NewMemoryRepository()
	expiredAt := time.Now().UTC().Add(-1 * time.Minute)
	if err := repo.UpsertAccount(core.Account{
		ID:       "expired",
		Provider: core.ProviderOpenAI,
		Label:    "Expired",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:         providers.OpenAIOAuthModeValue(),
			AccessToken:  "expired-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    &expiredAt,
			Metadata:     map[string]string{"token_source": providers.OpenAIDeviceCodeTokenSourceValue()},
		},
	}); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedRefreshingAdapter{
			scriptedAdapter: &scriptedAdapter{kind: core.ProviderOpenAI},
			refreshFn: func(_ context.Context, account core.Account) (core.Credential, error) {
				credential := account.Credential
				credential.AccessToken = "refreshed-token"
				expiresAt := time.Now().UTC().Add(1 * time.Hour)
				credential.ExpiresAt = &expiresAt
				return credential, nil
			},
		}),
	)

	result, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}, nil, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if result.Response.AccountID != "expired" {
		t.Fatalf("account id = %q", result.Response.AccountID)
	}

	account, err := repo.GetAccount("expired")
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != core.AccountStatusActive {
		t.Fatalf("status = %q", account.Status)
	}
	if account.Credential.ExpiresAt == nil || !account.Credential.ExpiresAt.After(time.Now().UTC()) {
		t.Fatalf("expires_at = %#v", account.Credential.ExpiresAt)
	}
	if account.Credential.AccessToken == "expired-token" {
		t.Fatalf("access token was not refreshed: %q", account.Credential.AccessToken)
	}
}

func TestPrepareAccountCoalescesConcurrentRefreshes(t *testing.T) {
	repo := storage.NewMemoryRepository()
	expiredAt := time.Now().UTC().Add(-1 * time.Minute)
	account := core.Account{
		ID:       "expired",
		Provider: core.ProviderOpenAI,
		Label:    "Expired",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:         providers.OpenAIOAuthModeValue(),
			AccessToken:  "expired-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    &expiredAt,
			Metadata:     map[string]string{"token_source": providers.OpenAIDeviceCodeTokenSourceValue()},
		},
	}
	if err := repo.UpsertAccount(account); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var refreshCount atomic.Int64
	adapter := &scriptedRefreshingAdapter{
		scriptedAdapter: &scriptedAdapter{kind: core.ProviderOpenAI},
		refreshFn: func(_ context.Context, account core.Account) (core.Credential, error) {
			if refreshCount.Add(1) == 1 {
				close(started)
			}
			<-release
			credential := account.Credential
			credential.AccessToken = "refreshed-token"
			expiresAt := time.Now().UTC().Add(time.Hour)
			credential.ExpiresAt = &expiresAt
			return credential, nil
		},
	}
	engine := NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter))

	firstDone := make(chan error, 1)
	go func() {
		refreshed, err := engine.prepareAccount(context.Background(), adapter, account)
		if err != nil {
			firstDone <- err
			return
		}
		if refreshed.Credential.AccessToken != "refreshed-token" {
			firstDone <- fmt.Errorf("access token = %q", refreshed.Credential.AccessToken)
			return
		}
		firstDone <- nil
	}()
	<-started

	secondDone := make(chan error, 1)
	go func() {
		refreshed, err := engine.prepareAccount(context.Background(), adapter, account)
		if err != nil {
			secondDone <- err
			return
		}
		if refreshed.Credential.AccessToken != "refreshed-token" {
			secondDone <- fmt.Errorf("access token = %q", refreshed.Credential.AccessToken)
			return
		}
		secondDone <- nil
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("second refresh returned before first completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if got := refreshCount.Load(); got != 1 {
		t.Fatalf("refresh count = %d, want 1", got)
	}
}

func TestExecuteFallsBackWhenExpiredCredentialCannotRefresh(t *testing.T) {
	repo := storage.NewMemoryRepository()
	expiredAt := time.Now().UTC().Add(-1 * time.Minute)
	if err := repo.UpsertAccount(core.Account{
		ID:       "expired",
		Provider: core.ProviderOpenAI,
		Label:    "Expired",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        providers.OpenAIOAuthModeValue(),
			AccessToken: "expired-token",
			ExpiresAt:   &expiredAt,
			Metadata:    map[string]string{"token_source": providers.OpenAIDeviceCodeTokenSourceValue()},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "backup",
		Provider: core.ProviderOpenAI,
		Label:    "Backup",
		Status:   core.AccountStatusActive,
		Priority: 90,
		Weight:   90,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "backup-token",
		},
	}); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(
		accounts.NewPool(repo),
		providers.NewRegistry(&scriptedAdapter{kind: core.ProviderOpenAI}),
	)

	result, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-4.1",
		Reason:    "test",
	}, nil, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if result.Response.AccountID != "backup" {
		t.Fatalf("account id = %q", result.Response.AccountID)
	}
	if len(result.Attempts) != 2 || result.Attempts[0].Status != "refresh_error" || result.Attempts[0].ErrorCode != "credential_expired" {
		t.Fatalf("attempts = %#v", result.Attempts)
	}

	account, err := repo.GetAccount("expired")
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != core.AccountStatusExpired {
		t.Fatalf("status = %q", account.Status)
	}
}

func TestExecuteRefreshesAndRetriesAfterInvokeAuthError(t *testing.T) {
	repo := storage.NewMemoryRepository()
	expiresAt := time.Now().UTC().Add(time.Hour)
	if err := repo.UpsertAccount(core.Account{
		ID:       "oauth",
		Provider: core.ProviderOpenAI,
		Label:    "OAuth",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:         "openai-oauth-device",
			AccessToken:  "invalidated-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    &expiresAt,
			Metadata:     map[string]string{"token_source": "openai_oauth"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var invokeCount atomic.Int32
	var refreshCount atomic.Int32
	adapter := &scriptedRefreshingAdapter{
		scriptedAdapter: &scriptedAdapter{kind: core.ProviderOpenAI},
		refreshFn: func(_ context.Context, account core.Account) (core.Credential, error) {
			refreshCount.Add(1)
			if account.Credential.Metadata["force_oauth_refresh"] != "true" {
				t.Fatalf("force_oauth_refresh = %q", account.Credential.Metadata["force_oauth_refresh"])
			}
			refreshed := account.Credential
			refreshed.AccessToken = "refreshed-token"
			refreshed.Metadata = map[string]string{"token_source": "openai_oauth"}
			return refreshed, nil
		},
	}
	adapter.invokeFn = func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
		invokeCount.Add(1)
		if decision.Account.Credential.AccessToken == "invalidated-token" {
			return nil, &providers.InvokeError{
				Code:      "upstream_auth_error",
				Temporary: false,
				Cooldown:  2 * time.Minute,
				Err:       errors.New("Your authentication token has been invalidated. Please try signing in again."),
			}
		}
		if decision.Account.Credential.AccessToken != "refreshed-token" {
			t.Fatalf("access token = %q", decision.Account.Credential.AccessToken)
		}
		return &core.GatewayResponse{
			ID:        "resp",
			Model:     decision.Model,
			Provider:  decision.Provider,
			AccountID: decision.Account.ID,
			Content:   "ok",
			CreatedAt: time.Now().UTC(),
		}, nil
	}

	engine := NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter))
	result, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-5.4",
		Reason:    "test",
	}, nil, &core.GatewayRequest{
		Model:    "gpt-5.4",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if result.Response.AccountID != "oauth" {
		t.Fatalf("account id = %q", result.Response.AccountID)
	}
	if invokeCount.Load() != 2 || refreshCount.Load() != 1 {
		t.Fatalf("invokeCount=%d refreshCount=%d", invokeCount.Load(), refreshCount.Load())
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %#v", result.Attempts)
	}
	if result.Attempts[0].Status != "invoke_error" || result.Attempts[0].ErrorCode != "upstream_auth_error" {
		t.Fatalf("first attempt = %#v", result.Attempts[0])
	}
	if result.Attempts[1].Status != "ok" || result.Attempts[1].AccountID != "oauth" {
		t.Fatalf("second attempt = %#v", result.Attempts[1])
	}
	saved, err := repo.GetAccount("oauth")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Credential.AccessToken != "refreshed-token" || saved.Status != core.AccountStatusActive {
		t.Fatalf("saved account = %#v", saved)
	}
	if saved.Credential.Metadata["force_oauth_refresh"] != "" {
		t.Fatalf("force flag should not be persisted: %#v", saved.Credential.Metadata)
	}
}

func TestExecuteUsesCurrentFreshCredentialAfterInvokeAuthError(t *testing.T) {
	repo := storage.NewMemoryRepository()
	expiresAt := time.Now().UTC().Add(time.Hour)
	if err := repo.UpsertAccount(core.Account{
		ID:       "oauth",
		Provider: core.ProviderOpenAI,
		Label:    "OAuth",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:         "openai-oauth-device",
			AccessToken:  "invalidated-token",
			RefreshToken: "stale-refresh-token",
			ExpiresAt:    &expiresAt,
			Metadata:     map[string]string{"token_source": "openai_oauth"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var invokeCount atomic.Int32
	var refreshCount atomic.Int32
	adapter := &scriptedRefreshingAdapter{
		scriptedAdapter: &scriptedAdapter{kind: core.ProviderOpenAI},
		refreshFn: func(context.Context, core.Account) (core.Credential, error) {
			refreshCount.Add(1)
			return core.Credential{}, errors.New("refresh should not be called")
		},
	}
	adapter.invokeFn = func(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
		invokeCount.Add(1)
		if decision.Account.Credential.AccessToken == "invalidated-token" {
			current := decision.Account
			current.Credential.AccessToken = "externally-refreshed-token"
			current.Credential.RefreshToken = "externally-refreshed-refresh-token"
			current.Credential.ExpiresAt = &expiresAt
			if err := repo.UpsertAccount(current); err != nil {
				t.Fatalf("UpsertAccount returned error: %v", err)
			}
			return nil, &providers.InvokeError{
				Code:      "upstream_auth_error",
				Temporary: false,
				Cooldown:  2 * time.Minute,
				Err:       errors.New("stale access token"),
			}
		}
		if decision.Account.Credential.AccessToken != "externally-refreshed-token" {
			t.Fatalf("access token = %q", decision.Account.Credential.AccessToken)
		}
		return &core.GatewayResponse{
			ID:        "resp",
			Model:     decision.Model,
			Provider:  decision.Provider,
			AccountID: decision.Account.ID,
			Content:   "ok",
			CreatedAt: time.Now().UTC(),
		}, nil
	}

	engine := NewEngine(accounts.NewPool(repo), providers.NewRegistry(adapter))
	result, err := engine.Execute(context.Background(), core.RoutePlan{
		Providers: []core.ProviderKind{core.ProviderOpenAI},
		Model:     "gpt-5.4",
		Reason:    "test",
	}, nil, &core.GatewayRequest{
		Model:    "gpt-5.4",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if result.Response.AccountID != "oauth" {
		t.Fatalf("account id = %q", result.Response.AccountID)
	}
	if invokeCount.Load() != 2 || refreshCount.Load() != 0 {
		t.Fatalf("invokeCount=%d refreshCount=%d", invokeCount.Load(), refreshCount.Load())
	}
}

func intPtr(value int) *int {
	return &value
}
