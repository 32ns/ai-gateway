package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/accounts"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/failover"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/routing"
	"github.com/32ns/ai-gateway/internal/storage"
)

type releaseGate struct {
	once sync.Once
	ch   chan struct{}
}

func newReleaseGate() *releaseGate {
	return &releaseGate{ch: make(chan struct{})}
}

func (g *releaseGate) Release() {
	if g == nil {
		return
	}
	g.once.Do(func() {
		close(g.ch)
	})
}

func (g *releaseGate) Done() <-chan struct{} {
	if g == nil {
		return nil
	}
	return g.ch
}

type blockingInvokeAdapter struct {
	entered     chan struct{}
	enteredOnce sync.Once
	releaseGate *releaseGate
}

func (a *blockingInvokeAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *blockingInvokeAdapter) DisplayName() string { return "Blocking Invoke" }

func (a *blockingInvokeAdapter) ListModels(context.Context) []core.ModelSpec {
	return []core.ModelSpec{{Name: "gpt-4.1", Provider: core.ProviderOpenAI}}
}

func (a *blockingInvokeAdapter) Invoke(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
	a.signalEntered()
	if a.releaseGate != nil {
		<-a.releaseGate.Done()
	}
	return &core.GatewayResponse{
		ID:           "resp_limit_invoke",
		Model:        decision.Model,
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Content:      "ok",
		FinishReason: "stop",
		CreatedAt:    time.Now().UTC(),
		Usage:        core.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}, nil
}

func (a *blockingInvokeAdapter) signalEntered() {
	if a == nil || a.entered == nil {
		return
	}
	a.enteredOnce.Do(func() {
		close(a.entered)
	})
}

type blockingStreamAdapter struct {
	entered     chan struct{}
	releaseGate *releaseGate
}

func (a *blockingStreamAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *blockingStreamAdapter) DisplayName() string { return "Blocking Stream" }

func (a *blockingStreamAdapter) ListModels(context.Context) []core.ModelSpec {
	return []core.ModelSpec{{Name: "gpt-4.1", Provider: core.ProviderOpenAI}}
}

func (a *blockingStreamAdapter) Invoke(context.Context, core.RouteDecision, *core.GatewayRequest) (*core.GatewayResponse, error) {
	return nil, errors.New("not used")
}

func (a *blockingStreamAdapter) OpenStream(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*providers.StreamSession, error) {
	return &providers.StreamSession{
		Response: &core.GatewayResponse{
			ID:           "resp_limit_stream",
			Model:        decision.Model,
			Provider:     decision.Provider,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			CreatedAt:    time.Now().UTC(),
		},
		Stream: &blockingStream{
			entered:     a.entered,
			releaseGate: a.releaseGate,
		},
	}, nil
}

type blockingStream struct {
	entered     chan struct{}
	enteredOnce sync.Once
	releaseGate *releaseGate
	emitted     bool
}

func (s *blockingStream) Next() (*core.StreamEvent, error) {
	s.signalEntered()
	if s.releaseGate != nil {
		<-s.releaseGate.Done()
	}
	if s.emitted {
		return nil, io.EOF
	}
	s.emitted = true
	return &core.StreamEvent{
		Delta:        "ok",
		FinishReason: "stop",
		Done:         true,
		Usage:        &core.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}, nil
}

func (s *blockingStream) Close() error {
	return nil
}

func (s *blockingStream) signalEntered() {
	if s == nil || s.entered == nil {
		return
	}
	s.enteredOnce.Do(func() {
		close(s.entered)
	})
}

func TestReserveUserConcurrentRequestSlotUsesUserOverride(t *testing.T) {
	repo := storage.NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.Runtime.UserConcurrentRequestLimit = 1
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	limit := 2
	if err := repo.UpsertUser(core.User{
		ID:                             "user_override",
		Username:                       "override",
		Enabled:                        true,
		ConcurrentRequestLimitOverride: &limit,
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{repo: repo}
	client := &core.APIClient{ID: "client_override", OwnerUserID: "user_override"}

	releaseOne, err := service.reserveUserConcurrentRequestSlot(client)
	if err != nil {
		t.Fatalf("first reserve returned error: %v", err)
	}
	defer releaseOne()
	releaseTwo, err := service.reserveUserConcurrentRequestSlot(client)
	if err != nil {
		t.Fatalf("second reserve returned error: %v", err)
	}
	defer releaseTwo()
	releaseThree, err := service.reserveUserConcurrentRequestSlot(client)
	if err == nil {
		releaseThree()
		t.Fatal("expected third reserve to be rejected by user override")
	}
	var limitErr *ConcurrencyLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("err = %T %v, want ConcurrencyLimitError", err, err)
	}
	if limitErr.Limit != 2 || limitErr.Active != 2 {
		t.Fatalf("limit error = limit %d active %d, want 2/2", limitErr.Limit, limitErr.Active)
	}
}

func TestReserveUserConcurrentRequestSlotAllowsUnlimitedUserOverride(t *testing.T) {
	repo := storage.NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.Runtime.UserConcurrentRequestLimit = 1
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	unlimited := 0
	if err := repo.UpsertUser(core.User{
		ID:                             "user_unlimited",
		Username:                       "unlimited",
		Enabled:                        true,
		ConcurrentRequestLimitOverride: &unlimited,
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{repo: repo}
	client := &core.APIClient{ID: "client_unlimited", OwnerUserID: "user_unlimited"}

	releaseOne, err := service.reserveUserConcurrentRequestSlot(client)
	if err != nil {
		t.Fatalf("first reserve returned error: %v", err)
	}
	defer releaseOne()
	releaseTwo, err := service.reserveUserConcurrentRequestSlot(client)
	if err != nil {
		t.Fatalf("second reserve returned error: %v", err)
	}
	defer releaseTwo()
}

func TestReserveUserConcurrentRequestSlotAppliesPlanLimitOnlyToPlanBilling(t *testing.T) {
	repo := storage.NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.Runtime.UserConcurrentRequestLimit = 0
	settings.Runtime.PlanConcurrentRequestLimit = 1
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	service := &Service{repo: repo}
	planClient := &core.APIClient{
		ID:            "client_plan_limit",
		OwnerUserID:   "user_plan_limit",
		BillingSource: core.ClientBillingSourcePlan,
	}

	releaseOne, err := service.reserveUserConcurrentRequestSlot(planClient)
	if err != nil {
		t.Fatalf("first plan reserve returned error: %v", err)
	}
	defer releaseOne()
	releaseTwo, err := service.reserveUserConcurrentRequestSlot(planClient)
	if err == nil {
		releaseTwo()
		t.Fatal("expected second plan reserve to be rejected")
	}
	var limitErr *ConcurrencyLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("err = %T %v, want ConcurrencyLimitError", err, err)
	}
	if !errors.Is(err, ErrPlanConcurrentRequestLimitExceeded) {
		t.Fatalf("err = %v, want ErrPlanConcurrentRequestLimitExceeded", err)
	}
	if limitErr.Scope != "plan" || limitErr.Limit != 1 || limitErr.Active != 1 {
		t.Fatalf("limit error = scope %q limit %d active %d, want plan/1/1", limitErr.Scope, limitErr.Limit, limitErr.Active)
	}

	cashClient := &core.APIClient{
		ID:            "client_cash_limit",
		OwnerUserID:   "user_cash_limit",
		BillingSource: core.ClientBillingSourceCash,
	}
	releaseCashOne, err := service.reserveUserConcurrentRequestSlot(cashClient)
	if err != nil {
		t.Fatalf("first cash reserve returned error: %v", err)
	}
	defer releaseCashOne()
	releaseCashTwo, err := service.reserveUserConcurrentRequestSlot(cashClient)
	if err != nil {
		t.Fatalf("second cash reserve returned error: %v", err)
	}
	defer releaseCashTwo()
}

func TestReserveUserConcurrentRequestSlotReleasesUserSlotWhenPlanLimitFails(t *testing.T) {
	repo := storage.NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.Runtime.UserConcurrentRequestLimit = 2
	settings.Runtime.PlanConcurrentRequestLimit = 1
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	service := &Service{repo: repo}
	client := &core.APIClient{
		ID:            "client_plan_release",
		OwnerUserID:   "user_plan_release",
		BillingSource: core.ClientBillingSourcePlan,
	}

	releaseOne, err := service.reserveUserConcurrentRequestSlot(client)
	if err != nil {
		t.Fatalf("first reserve returned error: %v", err)
	}
	defer releaseOne()
	releaseTwo, err := service.reserveUserConcurrentRequestSlot(client)
	if err == nil {
		releaseTwo()
		t.Fatal("expected second reserve to be rejected by plan limit")
	}
	var limitErr *ConcurrencyLimitError
	if !errors.As(err, &limitErr) || limitErr.Scope != "plan" {
		t.Fatalf("err = %T %v, want plan ConcurrencyLimitError", err, err)
	}

	service.userConcurrencyMu.Lock()
	userActive := service.userConcurrency["user_plan_release"]
	planActive := service.planConcurrency["user_plan_release"]
	service.userConcurrencyMu.Unlock()
	if userActive != 1 || planActive != 1 {
		t.Fatalf("active slots after failed plan reserve = user %d plan %d, want 1/1", userActive, planActive)
	}
}

func TestWaitUserRequestRateDelaysRepeatedRequests(t *testing.T) {
	repo := storage.NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.Runtime.UserRequestRateLimitPerMinute = 6000
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	service := New(repo, nil, nil)
	client := &core.APIClient{ID: "client_rate", OwnerUserID: "user_rate"}

	if err := service.waitUserRequestRate(context.Background(), client); err != nil {
		t.Fatalf("first wait returned error: %v", err)
	}
	start := time.Now()
	if err := service.waitUserRequestRate(context.Background(), client); err != nil {
		t.Fatalf("second wait returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 5*time.Millisecond {
		t.Fatalf("second wait elapsed %s, want at least 5ms", elapsed)
	}
}

func TestWaitUserRequestRateUsesUserOverride(t *testing.T) {
	repo := storage.NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.Runtime.UserRequestRateLimitPerMinute = 6000
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	unlimited := 0
	if err := repo.UpsertUser(core.User{
		ID:                                "user_rate_override",
		Username:                          "rate-override",
		Enabled:                           true,
		RequestRateLimitPerMinuteOverride: &unlimited,
	}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, nil, nil)
	client := &core.APIClient{ID: "client_rate_override", OwnerUserID: "user_rate_override"}

	if err := service.waitUserRequestRate(context.Background(), client); err != nil {
		t.Fatalf("first wait returned error: %v", err)
	}
	start := time.Now()
	if err := service.waitUserRequestRate(context.Background(), client); err != nil {
		t.Fatalf("second wait returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Millisecond {
		t.Fatalf("second wait elapsed %s, want no pacing delay", elapsed)
	}
}

func TestWaitUserRequestRateRespectsContextCancellation(t *testing.T) {
	repo := storage.NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.Runtime.UserRequestRateLimitPerMinute = 60
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	service := New(repo, nil, nil)
	client := &core.APIClient{ID: "client_rate_cancel", OwnerUserID: "user_rate_cancel"}

	if err := service.waitUserRequestRate(context.Background(), client); err != nil {
		t.Fatalf("first wait returned error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := service.waitUserRequestRate(ctx, client)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second wait err = %v, want context deadline exceeded", err)
	}
}

func TestReserveUserRequestSlotReleasesConcurrencyWhenRateWaitCanceled(t *testing.T) {
	repo := storage.NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.Runtime.UserConcurrentRequestLimit = 1
	settings.Runtime.UserRequestRateLimitPerMinute = 60
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	service := New(repo, nil, nil)
	client := &core.APIClient{ID: "client_rate_release", OwnerUserID: "user_rate_release"}

	release, err := service.reserveUserRequestSlot(context.Background(), client)
	if err != nil {
		t.Fatalf("first reserve returned error: %v", err)
	}
	release()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	release, err = service.reserveUserRequestSlot(ctx, client)
	if !errors.Is(err, context.DeadlineExceeded) {
		if err == nil {
			release()
		}
		t.Fatalf("second reserve err = %v, want context deadline exceeded", err)
	}

	service.userConcurrencyMu.Lock()
	active := service.userConcurrency["user_rate_release"]
	service.userConcurrencyMu.Unlock()
	if active != 0 {
		t.Fatalf("active concurrency after canceled wait = %d, want 0", active)
	}
}

func TestExecuteRejectsSecondConcurrentRequestFromSameUser(t *testing.T) {
	repo := storage.NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.Runtime.UserConcurrentRequestLimit = 1
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
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

	gate := newReleaseGate()
	entered := make(chan struct{})
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&blockingInvokeAdapter{
			entered:     entered,
			releaseGate: gate,
		})),
	)
	client := &core.APIClient{
		ID:           "client_limit",
		Name:         "Client Limit",
		APIKey:       "gw_limit",
		OwnerUserID:  "user_limit",
		Enabled:      true,
		RoutePolicy:  core.DefaultRoutePolicy(),
		AccountGroup: core.DefaultAccountGroupName,
	}

	firstDone := make(chan error, 1)
	go func() {
		_, err := service.Execute(context.Background(), &core.GatewayRequest{
			Model:    "gpt-4.1",
			Messages: []core.Message{{Role: "user", Content: "hello"}},
			Client:   client,
		})
		firstDone <- err
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first request to enter the adapter")
	}

	_, err := service.Execute(context.Background(), &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello again"}},
		Client:   client,
	})
	if err == nil {
		t.Fatal("expected concurrent request to be rejected")
	}
	var limitErr *ConcurrencyLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("err = %T %v, want ConcurrencyLimitError", err, err)
	}
	if limitErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", limitErr.StatusCode, http.StatusTooManyRequests)
	}
	if limitErr.Code != "rate_limit_exceeded" {
		t.Fatalf("code = %q, want rate_limit_exceeded", limitErr.Code)
	}

	gate.Release()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first request returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first request to finish")
	}
}

func TestExecuteStreamRejectsSecondConcurrentRequestFromSameUser(t *testing.T) {
	repo := storage.NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.Runtime.UserConcurrentRequestLimit = 1
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
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

	gate := newReleaseGate()
	entered := make(chan struct{})
	service := New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), providers.NewRegistry(&blockingStreamAdapter{
			entered:     entered,
			releaseGate: gate,
		})),
	)
	client := &core.APIClient{
		ID:          "client_limit",
		Name:        "Client Limit",
		APIKey:      "gw_limit",
		OwnerUserID: "user_limit",
		Enabled:     true,
		RoutePolicy: core.DefaultRoutePolicy(),
	}

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- service.ExecuteStream(context.Background(), &core.GatewayRequest{
			Model:    "gpt-4.1",
			Messages: []core.Message{{Role: "user", Content: "hello"}},
			Client:   client,
		}, nil)
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first stream request to enter the adapter")
	}

	err := service.ExecuteStream(context.Background(), &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello again"}},
		Client:   client,
	}, nil)
	if err == nil {
		t.Fatal("expected concurrent stream request to be rejected")
	}
	var limitErr *ConcurrencyLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("err = %T %v, want ConcurrencyLimitError", err, err)
	}
	if limitErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", limitErr.StatusCode, http.StatusTooManyRequests)
	}
	if limitErr.Code != "rate_limit_exceeded" {
		t.Fatalf("code = %q, want rate_limit_exceeded", limitErr.Code)
	}

	gate.Release()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first stream request returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first stream request to finish")
	}
}
