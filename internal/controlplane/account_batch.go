package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

type AccountBatchAction string

const (
	AccountBatchActionDisable      AccountBatchAction = "disable"
	AccountBatchActionEnable       AccountBatchAction = "enable"
	AccountBatchActionDelete       AccountBatchAction = "delete"
	AccountBatchActionRefreshQuota AccountBatchAction = "refresh_quota"
	AccountBatchActionTest         AccountBatchAction = "test"
	AccountBatchActionMoveGroup    AccountBatchAction = "move_group"

	accountBatchMaxMutationAccounts = 500
	accountBatchDetectTimeout       = 15 * time.Second
	accountBatchQuotaRefreshTimeout = 30 * time.Second
	accountBatchJobConcurrency      = 5
	accountBatchJobRetention        = 30 * time.Minute
)

type AccountBatchInput struct {
	Action      AccountBatchAction
	AccountIDs  []string
	TargetGroup string
}

type AccountBatchResult struct {
	Action      AccountBatchAction
	TargetGroup string
	Total       int
	Succeeded   int
	Failed      int
	Skipped     int
	Items       []AccountBatchItemResult
}

type AccountBatchItemResult struct {
	AccountID string
	Label     string
	Status    string
	Message   string
}

type AccountBatchJobStatus string

const (
	AccountBatchJobQueued    AccountBatchJobStatus = "queued"
	AccountBatchJobRunning   AccountBatchJobStatus = "running"
	AccountBatchJobCompleted AccountBatchJobStatus = "completed"
	AccountBatchJobCancelled AccountBatchJobStatus = "cancelled"
)

type AccountBatchJobSnapshot struct {
	ID          string
	Action      AccountBatchAction
	TargetGroup string
	Status      AccountBatchJobStatus
	Total       int
	Done        int
	Succeeded   int
	Failed      int
	Skipped     int
	Current     string
	StartedAt   time.Time
	FinishedAt  *time.Time
	Items       []AccountBatchItemResult
}

func (j AccountBatchJobSnapshot) Result() AccountBatchResult {
	return AccountBatchResult{
		Action:      j.Action,
		TargetGroup: j.TargetGroup,
		Total:       j.Total,
		Succeeded:   j.Succeeded,
		Failed:      j.Failed,
		Skipped:     j.Skipped,
		Items:       append([]AccountBatchItemResult(nil), j.Items...),
	}
}

type accountBatchJob struct {
	id          string
	action      AccountBatchAction
	targetGroup string
	status      AccountBatchJobStatus
	ids         []string
	items       []AccountBatchItemResult
	startedAt   time.Time
	finishedAt  *time.Time
	cancel      context.CancelFunc
	ctx         context.Context
}

func (s *Service) ApplyAccountBatch(ctx context.Context, input AccountBatchInput) (AccountBatchResult, error) {
	action, ids, targetGroup, err := s.validateAccountBatchInput(input)
	if err != nil {
		return AccountBatchResult{}, err
	}

	result := AccountBatchResult{
		Action:      action,
		TargetGroup: targetGroup,
		Total:       len(ids),
		Items:       make([]AccountBatchItemResult, 0, len(ids)),
	}
	for _, id := range ids {
		item := s.applyAccountBatchItem(ctx, action, id, targetGroup)
		switch item.Status {
		case "ok":
			result.Succeeded++
		case "skipped":
			result.Skipped++
		default:
			result.Failed++
		}
		result.Items = append(result.Items, item)
	}
	return result, nil
}

func (s *Service) StartAccountBatch(ctx context.Context, input AccountBatchInput) (AccountBatchJobSnapshot, error) {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return AccountBatchJobSnapshot{}, ctx.Err()
		default:
		}
	}
	action, ids, targetGroup, err := s.validateAccountBatchInput(input)
	if err != nil {
		return AccountBatchJobSnapshot{}, err
	}
	if !accountBatchActionRunsAsJob(action) {
		return AccountBatchJobSnapshot{}, fmt.Errorf("batch action %s cannot run as a background job", action)
	}
	s.cleanupAccountBatchJobs(time.Now().UTC())

	jobID, err := generateAccountBatchJobID()
	if err != nil {
		return AccountBatchJobSnapshot{}, err
	}
	jobCtx, cancel := context.WithCancel(context.Background())
	job := &accountBatchJob{
		id:          jobID,
		action:      action,
		targetGroup: targetGroup,
		status:      AccountBatchJobQueued,
		ids:         ids,
		items:       make([]AccountBatchItemResult, len(ids)),
		cancel:      cancel,
		ctx:         jobCtx,
	}

	s.accountBatchMu.Lock()
	if s.accountBatchJobs == nil {
		s.accountBatchJobs = make(map[string]*accountBatchJob)
	}
	if active := s.activeAccountBatchLocked(); active != nil {
		s.accountBatchMu.Unlock()
		cancel()
		return AccountBatchJobSnapshot{}, fmt.Errorf("account batch job %s is already running", active.id)
	}
	s.accountBatchJobs[job.id] = job
	s.activeAccountBatchJobID = job.id
	s.accountBatchMu.Unlock()

	snapshot := job.snapshot()
	go s.runAccountBatchJob(job)
	return snapshot, nil
}

func (s *Service) GetAccountBatchJob(id string) (AccountBatchJobSnapshot, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return AccountBatchJobSnapshot{}, false
	}
	s.accountBatchMu.Lock()
	defer s.accountBatchMu.Unlock()
	job := s.accountBatchJobs[id]
	if job == nil {
		return AccountBatchJobSnapshot{}, false
	}
	return job.snapshot(), true
}

func (s *Service) ActiveAccountBatchJob() (AccountBatchJobSnapshot, bool) {
	s.accountBatchMu.Lock()
	defer s.accountBatchMu.Unlock()
	job := s.activeAccountBatchLocked()
	if job == nil {
		return AccountBatchJobSnapshot{}, false
	}
	return job.snapshot(), true
}

func (s *Service) CancelAccountBatchJob(id string) (AccountBatchJobSnapshot, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return AccountBatchJobSnapshot{}, false
	}
	s.accountBatchMu.Lock()
	job := s.accountBatchJobs[id]
	if job == nil {
		s.accountBatchMu.Unlock()
		return AccountBatchJobSnapshot{}, false
	}
	cancel := job.cancel
	s.accountBatchMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return s.GetAccountBatchJob(id)
}

func (s *Service) validateAccountBatchInput(input AccountBatchInput) (AccountBatchAction, []string, string, error) {
	action := AccountBatchAction(strings.TrimSpace(string(input.Action)))
	if !accountBatchActionSupported(action) {
		return "", nil, "", fmt.Errorf("unsupported account batch action %q", action)
	}
	ids := uniqueAccountBatchIDs(input.AccountIDs)
	if len(ids) == 0 {
		return "", nil, "", fmt.Errorf("select at least one account")
	}
	if max := accountBatchMaxForAction(action); max > 0 && len(ids) > max {
		return "", nil, "", fmt.Errorf("batch action %s supports at most %d accounts at a time", action, max)
	}
	targetGroup := ""
	if action == AccountBatchActionMoveGroup {
		var err error
		targetGroup, err = s.normalizeAccountBatchTargetGroup(input.TargetGroup)
		if err != nil {
			return "", nil, "", err
		}
	}
	return action, ids, targetGroup, nil
}

func (s *Service) runAccountBatchJob(job *accountBatchJob) {
	now := time.Now().UTC()
	s.accountBatchMu.Lock()
	job.status = AccountBatchJobRunning
	job.startedAt = now
	s.accountBatchMu.Unlock()

	workerCount := accountBatchJobConcurrency
	if len(job.ids) < workerCount {
		workerCount = len(job.ids)
	}
	if workerCount < 1 {
		workerCount = 1
	}

	type workItem struct {
		index int
		id    string
	}
	work := make(chan workItem)
	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range work {
				select {
				case <-job.ctx.Done():
					s.setAccountBatchJobItem(job, item.index, AccountBatchItemResult{
						AccountID: strings.TrimSpace(item.id),
						Status:    "skipped",
						Message:   "cancelled",
					})
					continue
				default:
				}
				result := s.applyAccountBatchItem(job.ctx, job.action, item.id, job.targetGroup)
				if job.ctx.Err() != nil && result.Status != "ok" {
					result = AccountBatchItemResult{
						AccountID: strings.TrimSpace(item.id),
						Label:     strings.TrimSpace(result.Label),
						Status:    "skipped",
						Message:   "cancelled",
					}
				}
				s.setAccountBatchJobItem(job, item.index, result)
			}
		}()
	}

	for index, id := range job.ids {
		select {
		case <-job.ctx.Done():
			s.setAccountBatchJobItem(job, index, AccountBatchItemResult{
				AccountID: strings.TrimSpace(id),
				Status:    "skipped",
				Message:   "cancelled",
			})
		case work <- workItem{index: index, id: id}:
		}
	}
	close(work)
	wg.Wait()

	finishedAt := time.Now().UTC()
	s.accountBatchMu.Lock()
	if job.ctx.Err() != nil {
		job.status = AccountBatchJobCancelled
	} else {
		job.status = AccountBatchJobCompleted
	}
	job.finishedAt = &finishedAt
	if s.activeAccountBatchJobID == job.id {
		s.activeAccountBatchJobID = ""
	}
	s.accountBatchMu.Unlock()
}

func (s *Service) setAccountBatchJobItem(job *accountBatchJob, index int, item AccountBatchItemResult) {
	if job == nil || index < 0 {
		return
	}
	s.accountBatchMu.Lock()
	defer s.accountBatchMu.Unlock()
	if index >= len(job.items) {
		return
	}
	job.items[index] = item
}

func (j *accountBatchJob) snapshot() AccountBatchJobSnapshot {
	if j == nil {
		return AccountBatchJobSnapshot{}
	}
	items := make([]AccountBatchItemResult, 0, len(j.items))
	done := 0
	succeeded := 0
	failed := 0
	skipped := 0
	current := ""
	for index, item := range j.items {
		if strings.TrimSpace(item.Status) == "" {
			if current == "" && index < len(j.ids) {
				current = strings.TrimSpace(j.ids[index])
			}
			continue
		}
		items = append(items, item)
		done++
		switch item.Status {
		case "ok":
			succeeded++
		case "skipped":
			skipped++
		default:
			failed++
		}
	}
	startedAt := j.startedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	return AccountBatchJobSnapshot{
		ID:          j.id,
		Action:      j.action,
		TargetGroup: j.targetGroup,
		Status:      j.status,
		Total:       len(j.ids),
		Done:        done,
		Succeeded:   succeeded,
		Failed:      failed,
		Skipped:     skipped,
		Current:     current,
		StartedAt:   startedAt,
		FinishedAt:  cloneTimePtr(j.finishedAt),
		Items:       items,
	}
}

func (s *Service) cleanupAccountBatchJobs(now time.Time) {
	s.accountBatchMu.Lock()
	defer s.accountBatchMu.Unlock()
	for id, job := range s.accountBatchJobs {
		if job == nil || job.finishedAt == nil {
			continue
		}
		if now.Sub(*job.finishedAt) > accountBatchJobRetention {
			delete(s.accountBatchJobs, id)
		}
	}
}

func (s *Service) activeAccountBatchLocked() *accountBatchJob {
	if s == nil || strings.TrimSpace(s.activeAccountBatchJobID) == "" {
		return nil
	}
	job := s.accountBatchJobs[s.activeAccountBatchJobID]
	if job == nil {
		s.activeAccountBatchJobID = ""
		return nil
	}
	if job.status == AccountBatchJobQueued || job.status == AccountBatchJobRunning {
		return job
	}
	s.activeAccountBatchJobID = ""
	return nil
}

func accountBatchActionSupported(action AccountBatchAction) bool {
	switch action {
	case AccountBatchActionDisable,
		AccountBatchActionEnable,
		AccountBatchActionDelete,
		AccountBatchActionRefreshQuota,
		AccountBatchActionTest,
		AccountBatchActionMoveGroup:
		return true
	default:
		return false
	}
}

func accountBatchActionRunsAsJob(action AccountBatchAction) bool {
	switch action {
	case AccountBatchActionRefreshQuota,
		AccountBatchActionTest,
		AccountBatchActionMoveGroup:
		return true
	default:
		return false
	}
}

func accountBatchMaxForAction(action AccountBatchAction) int {
	switch action {
	case AccountBatchActionTest:
		return 0
	case AccountBatchActionRefreshQuota:
		return accountBatchMaxMutationAccounts
	default:
		return accountBatchMaxMutationAccounts
	}
}

func uniqueAccountBatchIDs(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		id := strings.TrimSpace(value)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func (s *Service) applyAccountBatchItem(ctx context.Context, action AccountBatchAction, accountID string, targetGroup string) AccountBatchItemResult {
	switch action {
	case AccountBatchActionDisable:
		return s.disableAccountBatchItem(accountID)
	case AccountBatchActionEnable:
		return s.enableAccountBatchItem(accountID)
	case AccountBatchActionDelete:
		return s.deleteAccountBatchItem(accountID)
	case AccountBatchActionRefreshQuota:
		return s.refreshAccountQuotaBatchItem(ctx, accountID)
	case AccountBatchActionTest:
		return s.testAccountBatchItem(ctx, accountID)
	case AccountBatchActionMoveGroup:
		return s.moveAccountGroupBatchItem(accountID, targetGroup)
	default:
		return accountBatchFailedItem(accountID, "", fmt.Errorf("unsupported account batch action %q", action))
	}
}

func (s *Service) normalizeAccountBatchTargetGroup(value string) (string, error) {
	target := normalizeAccountGroup(value)
	if target == "" {
		return "", fmt.Errorf("account group is required")
	}
	for _, group := range accountGroupsWithDefault(s.repo.ListAccountGroups()) {
		name := normalizeAccountGroup(group.Name)
		if strings.EqualFold(name, target) {
			return name, nil
		}
	}
	return "", fmt.Errorf("account group %q does not exist", target)
}

func (s *Service) disableAccountBatchItem(accountID string) AccountBatchItemResult {
	account, err := s.repo.GetAccount(accountID)
	if err != nil {
		return accountBatchFailedItem(accountID, "", err)
	}
	account.ControlDisabled = true
	if err := s.repo.UpsertAccount(account); err != nil {
		return accountBatchFailedItem(account.ID, account.Label, err)
	}
	return AccountBatchItemResult{
		AccountID: account.ID,
		Label:     account.Label,
		Status:    "ok",
		Message:   "disabled",
	}
}

func (s *Service) enableAccountBatchItem(accountID string) AccountBatchItemResult {
	account, err := s.repo.GetAccount(accountID)
	if err != nil {
		return accountBatchFailedItem(accountID, "", err)
	}
	account.ControlDisabled = false
	if err := s.repo.UpsertAccount(account); err != nil {
		return accountBatchFailedItem(account.ID, account.Label, err)
	}
	return AccountBatchItemResult{
		AccountID: account.ID,
		Label:     account.Label,
		Status:    "ok",
		Message:   "enabled",
	}
}

func (s *Service) deleteAccountBatchItem(accountID string) AccountBatchItemResult {
	account, err := s.repo.GetAccount(accountID)
	if err != nil {
		return accountBatchFailedItem(accountID, "", err)
	}
	if err := s.repo.DeleteAccount(accountID); err != nil {
		return accountBatchFailedItem(account.ID, account.Label, err)
	}
	return AccountBatchItemResult{
		AccountID: account.ID,
		Label:     account.Label,
		Status:    "ok",
		Message:   "deleted",
	}
}

func (s *Service) moveAccountGroupBatchItem(accountID, targetGroup string) AccountBatchItemResult {
	account, err := s.repo.GetAccount(accountID)
	if err != nil {
		return accountBatchFailedItem(accountID, "", err)
	}
	targetGroup = normalizeAccountGroup(targetGroup)
	if err := s.ensureAccountGroupAllowsProvider(targetGroup, account.Provider); err != nil {
		return accountBatchFailedItem(account.ID, account.Label, err)
	}
	account.Group = targetGroup
	if err := s.repo.UpsertAccount(account); err != nil {
		return accountBatchFailedItem(account.ID, account.Label, err)
	}
	return AccountBatchItemResult{
		AccountID: account.ID,
		Label:     account.Label,
		Status:    "ok",
		Message:   "moved to group",
	}
}

func (s *Service) refreshAccountQuotaBatchItem(ctx context.Context, accountID string) AccountBatchItemResult {
	account, err := s.repo.GetAccount(accountID)
	if err != nil {
		return accountBatchFailedItem(accountID, "", err)
	}
	if !s.SupportsQuotaRefresh(account) {
		return AccountBatchItemResult{
			AccountID: account.ID,
			Label:     account.Label,
			Status:    "skipped",
			Message:   "quota refresh is not supported",
		}
	}

	itemCtx, cancel := context.WithTimeout(ctx, accountBatchQuotaRefreshTimeout)
	defer cancel()
	_, snapshot, err := s.RefreshAccountQuota(itemCtx, accountID)
	if err != nil {
		return accountBatchFailedItem(account.ID, account.Label, err)
	}
	message := "quota refreshed"
	if plan := strings.TrimSpace(snapshot.Plan); plan != "" {
		message += " plan=" + plan
	}
	return AccountBatchItemResult{
		AccountID: account.ID,
		Label:     account.Label,
		Status:    "ok",
		Message:   message,
	}
}

func (s *Service) testAccountBatchItem(ctx context.Context, accountID string) AccountBatchItemResult {
	account, err := s.repo.GetAccount(accountID)
	if err != nil {
		return accountBatchFailedItem(accountID, "", err)
	}

	itemCtx, cancel := context.WithTimeout(ctx, accountBatchDetectTimeout)
	defer cancel()
	detectedAccount, err := s.DetectAccountStatus(itemCtx, accountID)
	if err != nil {
		if strings.TrimSpace(detectedAccount.ID) != "" {
			account = detectedAccount
		}
		return accountBatchFailedItem(account.ID, account.Label, err)
	}
	account = detectedAccount
	if strings.EqualFold(string(account.Status), string(core.AccountStatusBlocked)) {
		var markErr error
		account, markErr = s.markAccountBatchTestSuccessActive(account)
		if markErr != nil {
			return accountBatchFailedItem(account.ID, account.Label, fmt.Errorf("failed to mark account active: %w", markErr))
		}
	}
	message := "ping ok"
	if status := strings.TrimSpace(string(account.Status)); status != "" {
		message += " status=" + status
	}
	return AccountBatchItemResult{
		AccountID: account.ID,
		Label:     account.Label,
		Status:    "ok",
		Message:   message,
	}
}

func (s *Service) markAccountBatchTestFailureAbnormal(account core.Account) (core.Account, error) {
	account = core.NormalizeAccountRuntimeState(account, time.Now().UTC())
	switch account.Status {
	case core.AccountStatusBlocked,
		core.AccountStatusExpired,
		core.AccountStatusProviderBanned:
		account.CooldownUntil = nil
	default:
		account.Status = core.AccountStatusBlocked
		account.CooldownUntil = nil
	}
	if err := s.upsertAccountRuntimeState(account); err != nil {
		return account, err
	}
	saved, err := s.repo.GetAccount(account.ID)
	if err != nil {
		return account, err
	}
	return saved, nil
}

func (s *Service) markAccountBatchTestSuccessActive(account core.Account) (core.Account, error) {
	account = core.NormalizeAccountRuntimeState(account, time.Now().UTC())
	account.Status = core.AccountStatusActive
	account.CooldownUntil = nil
	account.ConsecutiveFails = 0
	if err := s.upsertAccountRuntimeState(account); err != nil {
		return account, err
	}
	saved, err := s.repo.GetAccount(account.ID)
	if err != nil {
		return account, err
	}
	return saved, nil
}

func accountBatchFailedItem(accountID, label string, err error) AccountBatchItemResult {
	message := ""
	if err != nil {
		message = strings.TrimSpace(err.Error())
	}
	if message == "" {
		message = "failed"
	}
	return AccountBatchItemResult{
		AccountID: strings.TrimSpace(accountID),
		Label:     strings.TrimSpace(label),
		Status:    "failed",
		Message:   message,
	}
}

func generateAccountBatchJobID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "acctbatch_" + hex.EncodeToString(buf[:]), nil
}
