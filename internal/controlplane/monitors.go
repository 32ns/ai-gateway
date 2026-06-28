package controlplane

import (
	"fmt"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"
)

const (
	DefaultMonitorIntervalSeconds = 300
	DefaultMonitorTimeoutSeconds  = 30
	DefaultMonitorPrompt          = "Reply with pong."
	MonitorHistoryLimit           = 2881
	MonitorHistoryWindow          = 24 * time.Hour
)

type MonitorTargetInput struct {
	ID              string
	Name            string
	AccountGroup    string
	Model           string
	Enabled         bool
	PublicVisible   bool
	IntervalSeconds int
	TimeoutSeconds  int
	Prompt          string
}

type MonitorPage struct {
	Targets       []MonitorTargetView
	AccountGroups []core.AccountGroup
	Models        []core.ModelConfig
	Summary       MonitorSummary
}

type MonitorTargetView struct {
	Target       core.MonitorTarget
	Latest       core.MonitorResult
	HasLatest    bool
	History      []core.MonitorResult
	Availability int
	Running      bool
}

type MonitorSummary struct {
	Total    int
	OK       int
	Degraded int
	Failed   int
	Unknown  int
}

func (s *Service) MonitorPage(includePrivate bool) MonitorPage {
	return s.monitorPage(includePrivate, time.Now().UTC())
}

func (s *Service) monitorPage(includePrivate bool, now time.Time) MonitorPage {
	targets := s.repo.ListMonitorTargets()
	latest := s.repo.ListLatestMonitorResults()
	views := make([]MonitorTargetView, 0, len(targets))
	for _, target := range targets {
		if !includePrivate && (!target.Enabled || !target.PublicVisible) {
			continue
		}
		history := monitorHistoryWindow(s.repo.ListMonitorResults(target.ID, MonitorHistoryLimit), now)
		view := MonitorTargetView{
			Target:       target,
			History:      history,
			Availability: monitorAvailability(history),
			Running:      s.IsMonitorRunning(target.ID),
		}
		if result, ok := latest[target.ID]; ok {
			view.Latest = result
			view.HasLatest = true
		}
		views = append(views, view)
	}
	return MonitorPage{
		Targets:       views,
		AccountGroups: accountGroupsWithDefault(s.repo.ListAccountGroups()),
		Models:        monitorTextModels(s.repo.ListModels()),
		Summary:       monitorSummary(views),
	}
}

func (s *Service) BeginMonitorRun(targetID string) bool {
	if s == nil {
		return false
	}
	targetID = strings.TrimSpace(targetID)
	if targetID == "" {
		return false
	}
	s.monitorRunMu.Lock()
	defer s.monitorRunMu.Unlock()
	if s.monitorRunning == nil {
		s.monitorRunning = make(map[string]time.Time)
	}
	if _, exists := s.monitorRunning[targetID]; exists {
		return false
	}
	s.monitorRunning[targetID] = time.Now().UTC()
	return true
}

func (s *Service) EndMonitorRun(targetID string) {
	if s == nil {
		return
	}
	targetID = strings.TrimSpace(targetID)
	if targetID == "" {
		return
	}
	s.monitorRunMu.Lock()
	delete(s.monitorRunning, targetID)
	s.monitorRunMu.Unlock()
}

func (s *Service) IsMonitorRunning(targetID string) bool {
	if s == nil {
		return false
	}
	targetID = strings.TrimSpace(targetID)
	if targetID == "" {
		return false
	}
	s.monitorRunMu.Lock()
	defer s.monitorRunMu.Unlock()
	_, ok := s.monitorRunning[targetID]
	return ok
}

func (s *Service) ListMonitorTargets() []core.MonitorTarget {
	return s.repo.ListMonitorTargets()
}

func (s *Service) GetMonitorTarget(id string) (core.MonitorTarget, error) {
	return s.repo.GetMonitorTarget(id)
}

func (s *Service) UpsertMonitorTarget(input MonitorTargetInput) (core.MonitorTarget, error) {
	target, err := s.normalizeMonitorTargetInput(input)
	if err != nil {
		return core.MonitorTarget{}, err
	}
	if err := s.repo.UpsertMonitorTarget(target); err != nil {
		return core.MonitorTarget{}, err
	}
	return s.repo.GetMonitorTarget(target.ID)
}

func (s *Service) DeleteMonitorTarget(id string) (core.MonitorTarget, error) {
	target, err := s.repo.GetMonitorTarget(id)
	if err != nil {
		return core.MonitorTarget{}, err
	}
	if err := s.repo.DeleteMonitorTarget(target.ID); err != nil {
		return core.MonitorTarget{}, err
	}
	return target, nil
}

func (s *Service) AppendMonitorResult(result core.MonitorResult) error {
	if strings.TrimSpace(result.ID) == "" {
		result.ID = newMonitorID("monres")
	}
	return s.repo.AppendMonitorResult(result)
}

func (s *Service) NewMonitorResultID() string {
	return newMonitorID("monres")
}

func (s *Service) normalizeMonitorTargetInput(input MonitorTargetInput) (core.MonitorTarget, error) {
	id := strings.TrimSpace(input.ID)
	if id == "" {
		id = newMonitorID("mon")
	}
	accountGroup := core.NormalizeAccountGroupName(input.AccountGroup)
	model := strings.TrimSpace(input.Model)
	if model == "" {
		return core.MonitorTarget{}, fmt.Errorf("model is required")
	}
	if err := s.validateMonitorGroup(accountGroup); err != nil {
		return core.MonitorTarget{}, err
	}
	if err := s.validateMonitorModel(accountGroup, model); err != nil {
		return core.MonitorTarget{}, err
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = accountGroup + " / " + model
	}
	interval := input.IntervalSeconds
	if interval <= 0 {
		interval = DefaultMonitorIntervalSeconds
	}
	if interval < 30 {
		interval = 30
	}
	timeout := input.TimeoutSeconds
	if timeout <= 0 {
		timeout = DefaultMonitorTimeoutSeconds
	}
	if timeout < 5 {
		timeout = 5
	}
	if timeout > interval {
		timeout = interval
	}
	prompt := strings.TrimSpace(input.Prompt)
	if prompt == "" {
		prompt = DefaultMonitorPrompt
	}
	existing, err := s.repo.GetMonitorTarget(id)
	if err != nil && err != storage.ErrNotFound {
		return core.MonitorTarget{}, err
	}
	createdAt := existing.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	return core.MonitorTarget{
		ID:              id,
		Name:            name,
		AccountGroup:    accountGroup,
		Model:           model,
		Enabled:         input.Enabled,
		PublicVisible:   input.PublicVisible,
		IntervalSeconds: interval,
		TimeoutSeconds:  timeout,
		Prompt:          prompt,
		CreatedAt:       createdAt,
	}, nil
}

func newMonitorID(prefix string) string {
	suffix, err := randomHex(8)
	if err != nil || suffix == "" {
		return fmt.Sprintf("%s_%d", strings.TrimSpace(prefix), time.Now().UnixNano())
	}
	return strings.TrimSpace(prefix) + "_" + suffix
}

func (s *Service) validateMonitorGroup(group string) error {
	group = core.NormalizeAccountGroupName(group)
	if strings.EqualFold(group, core.DefaultAccountGroupName) {
		return nil
	}
	for _, existing := range accountGroupsWithDefault(s.repo.ListAccountGroups()) {
		if strings.EqualFold(core.NormalizeAccountGroupName(existing.Name), group) {
			return nil
		}
	}
	return fmt.Errorf("account group %q does not exist", group)
}

func (s *Service) validateMonitorModel(group, modelID string) error {
	modelID = strings.TrimSpace(modelID)
	for _, model := range s.repo.ListModels() {
		if model.ID != modelID || !model.Enabled {
			continue
		}
		if core.NormalizeModelType(model.Type, model.ID) != core.ModelTypeText {
			return fmt.Errorf("model %q is not a text model", modelID)
		}
		for _, visibleGroup := range model.VisibleGroups {
			if strings.EqualFold(core.NormalizeAccountGroupName(visibleGroup), core.NormalizeAccountGroupName(group)) {
				return nil
			}
		}
		return fmt.Errorf("model %q is not visible to group %q", modelID, group)
	}
	return fmt.Errorf("model %q does not exist or is disabled", modelID)
}

func monitorTextModels(models []core.ModelConfig) []core.ModelConfig {
	out := make([]core.ModelConfig, 0, len(models))
	for _, model := range models {
		if !model.Enabled {
			continue
		}
		if core.NormalizeModelType(model.Type, model.ID) != core.ModelTypeText {
			continue
		}
		out = append(out, model)
	}
	return out
}

func monitorAvailability(history []core.MonitorResult) int {
	if len(history) == 0 {
		return 0
	}
	ok := 0
	for _, result := range history {
		if result.Status == core.MonitorStatusOK || result.Status == core.MonitorStatusDegraded {
			ok++
		}
	}
	return (ok*100 + len(history)/2) / len(history)
}

func monitorHistoryWindow(history []core.MonitorResult, now time.Time) []core.MonitorResult {
	if len(history) == 0 {
		return nil
	}
	if now.IsZero() {
		return append([]core.MonitorResult(nil), history...)
	}
	cutoff := now.UTC().Add(-MonitorHistoryWindow)
	out := make([]core.MonitorResult, 0, len(history))
	for _, result := range history {
		if result.CheckedAt.IsZero() || !result.CheckedAt.UTC().Before(cutoff) {
			out = append(out, result)
		}
	}
	return out
}

func monitorSummary(views []MonitorTargetView) MonitorSummary {
	summary := MonitorSummary{Total: len(views)}
	for _, view := range views {
		if !view.HasLatest {
			summary.Unknown++
			continue
		}
		switch view.Latest.Status {
		case core.MonitorStatusOK:
			summary.OK++
		case core.MonitorStatusDegraded:
			summary.Degraded++
		case core.MonitorStatusFailed:
			summary.Failed++
		default:
			summary.Unknown++
		}
	}
	return summary
}
