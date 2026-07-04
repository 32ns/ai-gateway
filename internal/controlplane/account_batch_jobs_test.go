package controlplane

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestStartAccountBatchJobRunsLiveActionsConcurrently(t *testing.T) {
	repo := storage.NewMemoryRepository()
	ids := testBatchJobAccounts(t, repo, 6)
	adapter := newBlockingBatchJobAdapter()
	service := New(repo, providers.NewRegistry(adapter))

	job, err := service.StartAccountBatch(context.Background(), AccountBatchInput{
		Action:     AccountBatchActionTest,
		AccountIDs: ids,
	})
	if err != nil {
		t.Fatalf("StartAccountBatch returned error: %v", err)
	}
	waitForBatchJobStarts(t, adapter, 2)
	close(adapter.release)

	job = waitForAccountBatchJob(t, service, job.ID)
	if job.Status != AccountBatchJobCompleted {
		t.Fatalf("job status = %q, want completed", job.Status)
	}
	if job.Succeeded != len(ids) || job.Done != len(ids) {
		t.Fatalf("job counts = done %d succeeded %d, want %d", job.Done, job.Succeeded, len(ids))
	}
	if max := atomic.LoadInt32(&adapter.maxActive); max < 2 {
		t.Fatalf("max active = %d, want concurrent workers", max)
	}
}

func TestStartAccountBatchJobCurrentUsesAccountLabel(t *testing.T) {
	repo := storage.NewMemoryRepository()
	ids := testBatchJobAccounts(t, repo, 1)
	adapter := newBlockingBatchJobAdapter()
	service := New(repo, providers.NewRegistry(adapter))

	job, err := service.StartAccountBatch(context.Background(), AccountBatchInput{
		Action:     AccountBatchActionTest,
		AccountIDs: ids,
	})
	if err != nil {
		t.Fatalf("StartAccountBatch returned error: %v", err)
	}
	wantCurrent := "Batch Job " + ids[0]
	if job.Current != wantCurrent {
		t.Fatalf("initial current = %q, want label %q", job.Current, wantCurrent)
	}

	waitForBatchJobStarts(t, adapter, 1)
	active, ok := service.ActiveAccountBatchJob()
	if !ok {
		t.Fatal("active account batch job not found")
	}
	if active.Current != wantCurrent {
		t.Fatalf("running current = %q, want label %q", active.Current, wantCurrent)
	}
	if _, ok := service.CancelAccountBatchJob(job.ID); !ok {
		t.Fatalf("CancelAccountBatchJob(%q) returned not found", job.ID)
	}
	waitForAccountBatchJob(t, service, job.ID)
}

func TestStartAccountBatchJobRejectsConcurrentLiveJob(t *testing.T) {
	repo := storage.NewMemoryRepository()
	ids := testBatchJobAccounts(t, repo, 2)
	adapter := newBlockingBatchJobAdapter()
	service := New(repo, providers.NewRegistry(adapter))

	job, err := service.StartAccountBatch(context.Background(), AccountBatchInput{
		Action:     AccountBatchActionTest,
		AccountIDs: ids,
	})
	if err != nil {
		t.Fatalf("StartAccountBatch returned error: %v", err)
	}
	waitForBatchJobStarts(t, adapter, 1)

	_, err = service.StartAccountBatch(context.Background(), AccountBatchInput{
		Action:     AccountBatchActionRefreshQuota,
		AccountIDs: ids,
	})
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second StartAccountBatch err = %v, want already running", err)
	}
	if _, ok := service.CancelAccountBatchJob(job.ID); !ok {
		t.Fatalf("CancelAccountBatchJob(%q) returned not found", job.ID)
	}
	job = waitForAccountBatchJob(t, service, job.ID)
	if job.Status != AccountBatchJobCancelled {
		t.Fatalf("job status = %q, want cancelled", job.Status)
	}
}

func TestCancelAccountBatchJobAllowsStartingNextJob(t *testing.T) {
	repo := storage.NewMemoryRepository()
	ids := testBatchJobAccounts(t, repo, 1)
	adapter := newBlockingBatchJobAdapter()
	service := New(repo, providers.NewRegistry(adapter))

	job, err := service.StartAccountBatch(context.Background(), AccountBatchInput{
		Action:     AccountBatchActionTest,
		AccountIDs: ids,
	})
	if err != nil {
		t.Fatalf("StartAccountBatch returned error: %v", err)
	}
	waitForBatchJobStarts(t, adapter, 1)
	if _, ok := service.CancelAccountBatchJob(job.ID); !ok {
		t.Fatalf("CancelAccountBatchJob(%q) returned not found", job.ID)
	}
	job = waitForAccountBatchJob(t, service, job.ID)
	if job.Status != AccountBatchJobCancelled {
		t.Fatalf("job status = %q, want cancelled", job.Status)
	}

	next, err := service.StartAccountBatch(context.Background(), AccountBatchInput{
		Action:     AccountBatchActionTest,
		AccountIDs: ids,
	})
	if err != nil {
		t.Fatalf("second StartAccountBatch returned error: %v", err)
	}
	close(adapter.release)
	next = waitForAccountBatchJob(t, service, next.ID)
	if next.Status != AccountBatchJobCompleted {
		t.Fatalf("next job status = %q, want completed", next.Status)
	}
}

func TestStartAccountBatchJobDoesNotMarkFailedDetectionAbnormal(t *testing.T) {
	repo := storage.NewMemoryRepository()
	ids := testBatchJobAccounts(t, repo, 2)
	adapter := newBlockingBatchJobAdapter()
	adapter.failID = ids[1]
	service := New(repo, providers.NewRegistry(adapter))

	job, err := service.StartAccountBatch(context.Background(), AccountBatchInput{
		Action:     AccountBatchActionTest,
		AccountIDs: ids,
	})
	if err != nil {
		t.Fatalf("StartAccountBatch returned error: %v", err)
	}
	waitForBatchJobStarts(t, adapter, 2)
	close(adapter.release)

	job = waitForAccountBatchJob(t, service, job.ID)
	if job.Status != AccountBatchJobCompleted || job.Succeeded != 1 || job.Failed != 1 {
		t.Fatalf("job = %#v, want completed with one failure", job)
	}
	account, err := repo.GetAccount(ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != core.AccountStatusActive {
		t.Fatalf("failed account status = %q, want %q", account.Status, core.AccountStatusActive)
	}
	if got := AccountFilterStatus(account); got != "normal" {
		t.Fatalf("failed account filter status = %q, want normal", got)
	}
}

func TestCancelAccountBatchJobDoesNotMarkActiveDetectionAbnormal(t *testing.T) {
	repo := storage.NewMemoryRepository()
	ids := testBatchJobAccounts(t, repo, 1)
	adapter := newBlockingBatchJobAdapter()
	service := New(repo, providers.NewRegistry(adapter))

	job, err := service.StartAccountBatch(context.Background(), AccountBatchInput{
		Action:     AccountBatchActionTest,
		AccountIDs: ids,
	})
	if err != nil {
		t.Fatalf("StartAccountBatch returned error: %v", err)
	}
	waitForBatchJobStarts(t, adapter, 1)
	if _, ok := service.CancelAccountBatchJob(job.ID); !ok {
		t.Fatalf("CancelAccountBatchJob(%q) returned not found", job.ID)
	}
	job = waitForAccountBatchJob(t, service, job.ID)
	if job.Status != AccountBatchJobCancelled || job.Failed != 0 {
		t.Fatalf("job = %#v, want cancelled without failures", job)
	}
	account, err := repo.GetAccount(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != core.AccountStatusActive {
		t.Fatalf("cancelled account status = %q, want active", account.Status)
	}
}

func TestStartAccountBatchJobMovesAccountsBetweenGroups(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	ids := testBatchJobAccounts(t, repo, 3)
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	job, err := service.StartAccountBatch(context.Background(), AccountBatchInput{
		Action:      AccountBatchActionMoveGroup,
		AccountIDs:  ids,
		TargetGroup: "Plus",
	})
	if err != nil {
		t.Fatalf("StartAccountBatch returned error: %v", err)
	}
	if job.Action != AccountBatchActionMoveGroup || job.TargetGroup != "Plus" {
		t.Fatalf("job = %#v, want move group to Plus", job)
	}

	job = waitForAccountBatchJob(t, service, job.ID)
	if job.Status != AccountBatchJobCompleted {
		t.Fatalf("job status = %q, want completed", job.Status)
	}
	if job.Succeeded != len(ids) || job.Done != len(ids) {
		t.Fatalf("job counts = done %d succeeded %d, want %d", job.Done, job.Succeeded, len(ids))
	}
	for _, id := range ids {
		account, err := repo.GetAccount(id)
		if err != nil {
			t.Fatal(err)
		}
		if account.Group != "Plus" {
			t.Fatalf("account %s group = %q, want Plus", id, account.Group)
		}
	}
}

func TestApplyAccountBatchRejectsEmptyMoveGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	ids := testBatchJobAccounts(t, repo, 1)
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	_, err := service.ApplyAccountBatch(context.Background(), AccountBatchInput{
		Action:     AccountBatchActionMoveGroup,
		AccountIDs: ids,
	})
	if err == nil || !strings.Contains(err.Error(), "account group is required") {
		t.Fatalf("ApplyAccountBatch err = %v, want account group required", err)
	}
}

func TestValidateAccountBatchAllowsUnlimitedDetectionSelection(t *testing.T) {
	repo := storage.NewMemoryRepository()
	ids := make([]string, accountBatchMaxMutationAccounts+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("acct_detection_%d", i)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	action, normalized, _, err := service.validateAccountBatchInput(AccountBatchInput{
		Action:     AccountBatchActionTest,
		AccountIDs: ids,
	})
	if err != nil {
		t.Fatalf("validateAccountBatchInput returned error: %v", err)
	}
	if action != AccountBatchActionTest || len(normalized) != len(ids) {
		t.Fatalf("action=%q ids=%d, want test ids=%d", action, len(normalized), len(ids))
	}
}

func testBatchJobAccounts(t *testing.T, repo storage.Repository, count int) []string {
	t.Helper()
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("acct_batch_job_%d", i)
		ids = append(ids, id)
		if err := repo.UpsertAccount(core.Account{
			ID:       id,
			Provider: core.ProviderOpenAI,
			Label:    "Batch Job " + id,
			Group:    core.DefaultAccountGroupName,
			Status:   core.AccountStatusActive,
			Credential: core.Credential{
				AccessToken: "token_" + id,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

func waitForAccountBatchJob(t *testing.T, service *Service, id string) AccountBatchJobSnapshot {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for account batch job %q", id)
		default:
		}
		job, ok := service.GetAccountBatchJob(id)
		if !ok {
			t.Fatalf("account batch job %q not found", id)
		}
		if job.Status == AccountBatchJobCompleted || job.Status == AccountBatchJobCancelled {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForBatchJobStarts(t *testing.T, adapter *blockingBatchJobAdapter, want int) {
	t.Helper()
	for i := 0; i < want; i++ {
		select {
		case <-adapter.started:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %d batch job starts", want)
		}
	}
}

type blockingBatchJobAdapter struct {
	started   chan struct{}
	release   chan struct{}
	failID    string
	active    int32
	maxActive int32
}

func newBlockingBatchJobAdapter() *blockingBatchJobAdapter {
	return &blockingBatchJobAdapter{
		started: make(chan struct{}, accountBatchJobConcurrency*2),
		release: make(chan struct{}),
	}
}

func (a *blockingBatchJobAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *blockingBatchJobAdapter) DisplayName() string { return "OpenAI" }

func (a *blockingBatchJobAdapter) ListModels(context.Context) []core.ModelSpec { return nil }

func (a *blockingBatchJobAdapter) Invoke(ctx context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
	if err := a.waitForRelease(ctx); err != nil {
		return nil, err
	}
	return &core.GatewayResponse{
		ID:           "resp_" + decision.Account.ID,
		Model:        decision.Model,
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Content:      "pong",
		CreatedAt:    time.Now().UTC(),
	}, nil
}

func (a *blockingBatchJobAdapter) OpenStream(ctx context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*providers.StreamSession, error) {
	if err := a.waitForRelease(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.failID) == decision.Account.ID {
		return nil, fmt.Errorf("ping backend unavailable")
	}
	return testOpenAICompletedStreamSession(decision), nil
}

func (a *blockingBatchJobAdapter) waitForRelease(ctx context.Context) error {
	active := atomic.AddInt32(&a.active, 1)
	defer atomic.AddInt32(&a.active, -1)
	for {
		max := atomic.LoadInt32(&a.maxActive)
		if active <= max || atomic.CompareAndSwapInt32(&a.maxActive, max, active) {
			break
		}
	}
	select {
	case a.started <- struct{}{}:
	default:
	}
	select {
	case <-a.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
