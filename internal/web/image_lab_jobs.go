package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const imageLabJobRetention = 24 * time.Hour

const (
	imageLabTaskStatusRunning   = "running"
	imageLabTaskStatusCompleted = "completed"
	imageLabTaskStatusFailed    = "failed"
	imageLabTaskStatusCancelled = "cancelled"
)

type imageLabResultEvent struct {
	Index     int    `json:"index"`
	OK        bool   `json:"ok"`
	Image     string `json:"image,omitempty"`
	MIME      string `json:"mime,omitempty"`
	Text      string `json:"text,omitempty"`
	Error     string `json:"error,omitempty"`
	Status    int    `json:"status,omitempty"`
	ElapsedMS int64  `json:"elapsedMs,omitempty"`
	RemoteURL string `json:"remoteUrl,omitempty"`
	FilePath  string `json:"-"`
	B64JSON   string `json:"-"`
}

type imageLabTaskSnapshot struct {
	ID              string                 `json:"id"`
	UserID          string                 `json:"userId,omitempty"`
	ClientID        string                 `json:"clientId,omitempty"`
	CreatedAt       int64                  `json:"createdAt"`
	UpdatedAt       int64                  `json:"updatedAt"`
	Prompt          string                 `json:"prompt"`
	Ratio           string                 `json:"ratio"`
	Resolution      string                 `json:"resolution"`
	Size            string                 `json:"size"`
	APISize         string                 `json:"apiSize,omitempty"`
	Model           string                 `json:"model"`
	InputImageCount int                    `json:"inputImageCount"`
	Count           int                    `json:"count"`
	Status          string                 `json:"status"`
	Results         []*imageLabResultEvent `json:"results"`
	ElapsedMS       int64                  `json:"elapsedMs,omitempty"`
	Error           string                 `json:"error,omitempty"`
	Dismissed       bool                   `json:"dismissed,omitempty"`
}

type imageLabJobManager struct {
	mu   sync.Mutex
	jobs map[string]*imageLabJob
}

type imageLabJob struct {
	mu       sync.Mutex
	manager  *imageLabJobManager
	ctx      context.Context
	cancel   context.CancelFunc
	options  imageLabGenerateOptions
	snapshot imageLabTaskSnapshot
}

func newImageLabJobManager() *imageLabJobManager {
	return &imageLabJobManager{
		jobs: make(map[string]*imageLabJob),
	}
}

func (m *imageLabJobManager) StartDetached(s *Server, userID string, options imageLabGenerateOptions) (*imageLabJob, error) {
	if m == nil {
		return nil, fmt.Errorf("image lab job manager is nil")
	}
	job, err := newImageLabJob(userID, options)
	if err != nil {
		return nil, err
	}
	job.manager = m
	job.ctx, job.cancel = context.WithCancel(context.Background())

	m.cleanup(s, time.Now())
	m.mu.Lock()
	m.jobs[job.snapshot.ID] = job
	m.mu.Unlock()
	s.publishImageJobUpdated(job.snapshotCopy())

	go job.run(s)
	return job, nil
}

func (m *imageLabJobManager) Get(userID, jobID string) (imageLabTaskSnapshot, bool) {
	if m == nil {
		return imageLabTaskSnapshot{}, false
	}
	m.mu.Lock()
	job := m.jobs[strings.TrimSpace(jobID)]
	m.mu.Unlock()
	if job == nil || job.snapshotUserID() != strings.TrimSpace(userID) {
		return imageLabTaskSnapshot{}, false
	}
	return job.snapshotCopy(), true
}

func (m *imageLabJobManager) List(userID string) []imageLabTaskSnapshot {
	if m == nil {
		return nil
	}
	userID = strings.TrimSpace(userID)
	m.mu.Lock()
	jobs := make([]*imageLabJob, 0, len(m.jobs))
	for _, job := range m.jobs {
		if job.snapshotUserID() == userID && !job.isDismissed() {
			jobs = append(jobs, job)
		}
	}
	m.mu.Unlock()

	out := make([]imageLabTaskSnapshot, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, job.snapshotCopy())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt == out[j].CreatedAt {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt > out[j].CreatedAt
	})
	return out
}

func (m *imageLabJobManager) Delete(s *Server, userID, jobID string) (imageLabTaskSnapshot, bool) {
	if m == nil {
		return imageLabTaskSnapshot{}, false
	}
	userID = strings.TrimSpace(userID)
	jobID = strings.TrimSpace(jobID)
	m.mu.Lock()
	job := m.jobs[jobID]
	if job == nil || job.snapshotUserID() != userID || !job.isTerminal() {
		m.mu.Unlock()
		return imageLabTaskSnapshot{}, false
	}
	delete(m.jobs, jobID)
	m.mu.Unlock()

	snapshot, ok := job.dismiss()
	if ok && s != nil {
		s.removeImageLabJobFiles(job)
	}
	return snapshot, ok
}

func (m *imageLabJobManager) Cancel(userID, jobID string) (imageLabTaskSnapshot, bool) {
	if m == nil {
		return imageLabTaskSnapshot{}, false
	}
	userID = strings.TrimSpace(userID)
	jobID = strings.TrimSpace(jobID)
	m.mu.Lock()
	job := m.jobs[jobID]
	m.mu.Unlock()
	if job == nil || job.snapshotUserID() != userID || !job.cancelRunning() {
		return imageLabTaskSnapshot{}, false
	}
	return job.snapshotCopy(), true
}

func (m *imageLabJobManager) ResultFile(userID, jobID string, index int) (string, string, int64, bool) {
	if m == nil {
		return "", "", 0, false
	}
	userID = strings.TrimSpace(userID)
	jobID = strings.TrimSpace(jobID)
	m.mu.Lock()
	job := m.jobs[jobID]
	m.mu.Unlock()
	if job == nil || job.snapshotUserID() != userID {
		return "", "", 0, false
	}
	return job.resultFile(index)
}

func (m *imageLabJobManager) cleanup(s *Server, now time.Time) {
	if m == nil {
		return
	}
	expiredJobs := make([]*imageLabJob, 0)
	m.mu.Lock()
	for id, job := range m.jobs {
		if job == nil || job.expired(now) {
			delete(m.jobs, id)
			if job != nil {
				expiredJobs = append(expiredJobs, job)
			}
		}
	}
	m.mu.Unlock()
	if s != nil {
		for _, job := range expiredJobs {
			s.removeImageLabJobFiles(job)
		}
	}
}

func newImageLabJob(userID string, options imageLabGenerateOptions) (*imageLabJob, error) {
	jobID, err := generateImageLabJobID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	results := make([]*imageLabResultEvent, options.Count)
	job := &imageLabJob{
		options: options,
		snapshot: imageLabTaskSnapshot{
			ID:              jobID,
			UserID:          strings.TrimSpace(userID),
			ClientID:        strings.TrimSpace(options.Client.ID),
			CreatedAt:       now,
			UpdatedAt:       now,
			Prompt:          options.Prompt,
			Ratio:           options.Ratio,
			Resolution:      options.Resolution,
			Size:            options.DisplaySize,
			APISize:         options.APISize,
			Model:           options.Model,
			InputImageCount: len(options.InputImages),
			Count:           options.Count,
			Status:          imageLabTaskStatusRunning,
			Results:         results,
		},
	}
	return job, nil
}

func (j *imageLabJob) run(s *Server) {
	defer j.releaseContext()

	started := time.Now()
	ctx := j.runContext()
	count := j.snapshot.Count
	results := make(chan imageLabResultEvent, count)
	jobs := make(chan int)
	pending := j.pendingIndexes()
	workers := imageLabJobWorkerCount(s, j.options, count)
	if len(pending) < workers {
		workers = len(pending)
	}
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				if j.isTerminal() {
					continue
				}
				result := s.runImageLabItem(ctx, j.options, index)
				if s != nil {
					result = s.storeImageLabResult(j.snapshotCopy(), result)
				}
				results <- result
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, index := range pending {
			if j.isTerminal() {
				return
			}
			select {
			case jobs <- index:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	for result := range results {
		j.setResult(result)
		snapshot := j.snapshotCopy()
		s.publishImageJobUpdated(snapshot)
	}

	j.finish(time.Since(started))
	snapshot := j.snapshotCopy()
	s.publishImageJobUpdated(snapshot)
	j.discardOptions()
}

func (j *imageLabJob) pendingIndexes() []int {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]int, 0, j.snapshot.Count)
	for index := 0; index < j.snapshot.Count; index++ {
		if index < len(j.snapshot.Results) {
			result := j.snapshot.Results[index]
			if result != nil && result.OK && (result.Image != "" || result.RemoteURL != "") {
				continue
			}
		}
		out = append(out, index)
	}
	return out
}

func imageLabJobWorkerCount(s *Server, options imageLabGenerateOptions, count int) int {
	if count <= 1 {
		return count
	}
	workers := 1
	if s != nil && s.gateway != nil {
		if candidates := s.gateway.ImageCandidateCount(&options.Client, options.Model, len(options.InputImages) > 0); candidates > 0 {
			workers = candidates
		}
	}
	if workers < 1 {
		return 1
	}
	if workers > count {
		return count
	}
	return workers
}

func (j *imageLabJob) runContext() context.Context {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.ctx != nil {
		return j.ctx
	}
	return context.Background()
}

func (j *imageLabJob) releaseContext() {
	j.mu.Lock()
	cancel := j.cancel
	j.ctx = nil
	j.cancel = nil
	j.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (j *imageLabJob) setResult(result imageLabResultEvent) {
	j.mu.Lock()
	if result.Index < 0 || result.Index >= len(j.snapshot.Results) {
		j.mu.Unlock()
		return
	}
	copyResult := result
	j.snapshot.Results[result.Index] = &copyResult
	j.snapshot.UpdatedAt = time.Now().UnixMilli()
	j.mu.Unlock()
}

func (j *imageLabJob) finish(elapsed time.Duration) {
	j.mu.Lock()
	j.snapshot.ElapsedMS = elapsed.Milliseconds()
	j.snapshot.UpdatedAt = time.Now().UnixMilli()
	if j.snapshot.Status == imageLabTaskStatusRunning {
		okCount, _ := j.summaryLocked()
		if okCount > 0 {
			j.snapshot.Status = imageLabTaskStatusCompleted
		} else {
			j.snapshot.Status = imageLabTaskStatusFailed
			if strings.TrimSpace(j.snapshot.Error) == "" {
				j.snapshot.Error = imageLabTaskErrorSummary(j.snapshot.Results)
			}
		}
	}
	if j.snapshot.Status == imageLabTaskStatusCancelled && strings.TrimSpace(j.snapshot.Error) == "" {
		j.snapshot.Error = "任务已停止"
	}
	j.mu.Unlock()
}

func (j *imageLabJob) summaryLocked() (int, int) {
	return imageLabResultsSummary(j.snapshot.Results)
}

func imageLabResultsSummary(results []*imageLabResultEvent) (int, int) {
	okCount := 0
	finishedCount := 0
	for _, result := range results {
		if result == nil {
			continue
		}
		finishedCount++
		if result.OK && (result.Image != "" || result.RemoteURL != "") {
			okCount++
		}
	}
	return okCount, finishedCount
}

func imageLabTaskErrorSummary(results []*imageLabResultEvent) string {
	for _, result := range results {
		if result == nil || result.OK {
			continue
		}
		if message := strings.TrimSpace(result.Error); message != "" {
			return message
		}
	}
	return "任务结束，但没有成功图片。"
}

func (j *imageLabJob) snapshotCopy() imageLabTaskSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.snapshotCopyLocked()
}

func (j *imageLabJob) snapshotCopyLocked() imageLabTaskSnapshot {
	copySnapshot := j.snapshot
	if len(j.snapshot.Results) > 0 {
		copySnapshot.Results = make([]*imageLabResultEvent, len(j.snapshot.Results))
		for index, result := range j.snapshot.Results {
			if result == nil {
				continue
			}
			copyResult := *result
			copySnapshot.Results[index] = &copyResult
		}
	}
	return copySnapshot
}

func (j *imageLabJob) discardOptions() {
	j.options = imageLabGenerateOptions{}
}

func (j *imageLabJob) snapshotUserID() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return strings.TrimSpace(j.snapshot.UserID)
}

func (j *imageLabJob) snapshotID() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return strings.TrimSpace(j.snapshot.ID)
}

func (j *imageLabJob) resultFilePaths() []string {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	paths := make([]string, 0, len(j.snapshot.Results))
	for _, result := range j.snapshot.Results {
		if result == nil {
			continue
		}
		if path := strings.TrimSpace(result.FilePath); path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func (j *imageLabJob) resultFile(index int) (string, string, int64, bool) {
	if j == nil {
		return "", "", 0, false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if index < 0 || index >= len(j.snapshot.Results) {
		return "", "", 0, false
	}
	result := j.snapshot.Results[index]
	if result == nil || !result.OK {
		return "", "", 0, false
	}
	path := strings.TrimSpace(result.FilePath)
	if path == "" {
		return "", "", 0, false
	}
	return path, strings.TrimSpace(result.MIME), j.snapshot.UpdatedAt, true
}

func (j *imageLabJob) isTerminal() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.snapshot.Status != imageLabTaskStatusRunning
}

func (j *imageLabJob) isDismissed() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.snapshot.Dismissed
}

func (j *imageLabJob) dismiss() (imageLabTaskSnapshot, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.snapshot.Status == imageLabTaskStatusRunning {
		return imageLabTaskSnapshot{}, false
	}
	j.snapshot.Dismissed = true
	j.snapshot.UpdatedAt = time.Now().UnixMilli()
	return j.snapshotCopyLocked(), true
}

func (j *imageLabJob) cancelRunning() bool {
	j.mu.Lock()
	if j.snapshot.Status != imageLabTaskStatusRunning {
		j.mu.Unlock()
		return false
	}
	j.snapshot.Status = imageLabTaskStatusCancelled
	j.snapshot.Error = "任务已停止"
	j.snapshot.UpdatedAt = time.Now().UnixMilli()
	cancel := j.cancel
	j.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	return true
}

func (j *imageLabJob) expired(now time.Time) bool {
	if j == nil {
		return true
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.snapshot.Status == imageLabTaskStatusRunning {
		return false
	}
	updatedAt := j.snapshot.UpdatedAt
	if updatedAt <= 0 {
		updatedAt = j.snapshot.CreatedAt
	}
	if updatedAt <= 0 {
		return true
	}
	return now.After(time.UnixMilli(updatedAt).Add(imageLabJobRetention))
}

func generateImageLabJobID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate image lab job id: %w", err)
	}
	return "imglab_" + hex.EncodeToString(buf), nil
}
