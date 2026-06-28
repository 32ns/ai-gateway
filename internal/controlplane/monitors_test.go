package controlplane

import (
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestMonitorTargetsRequireTextModels(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if err := repo.UpsertModel(core.ModelConfig{
		ID:            "gpt-image-2",
		Provider:      core.ProviderOpenAI,
		Type:          core.ModelTypeImage,
		Enabled:       true,
		VisibleGroups: []string{core.DefaultAccountGroupName},
	}); err != nil {
		t.Fatal(err)
	}

	_, err := service.UpsertMonitorTarget(MonitorTargetInput{
		Name:          "Image Monitor",
		AccountGroup:  core.DefaultAccountGroupName,
		Model:         "gpt-image-2",
		Enabled:       true,
		PublicVisible: true,
	})
	if err == nil || !strings.Contains(err.Error(), "not a text model") {
		t.Fatalf("UpsertMonitorTarget error = %v, want text model validation", err)
	}
}

func TestMonitorPageKeepsRecent24HourHistoryWindow(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if err := repo.UpsertModel(core.ModelConfig{
		ID:            "gpt-5",
		Provider:      core.ProviderOpenAI,
		Type:          core.ModelTypeText,
		Enabled:       true,
		VisibleGroups: []string{core.DefaultAccountGroupName},
	}); err != nil {
		t.Fatal(err)
	}
	target, err := service.UpsertMonitorTarget(MonitorTargetInput{
		Name:            "Default Gpt-5",
		AccountGroup:    core.DefaultAccountGroupName,
		Model:           "gpt-5",
		Enabled:         true,
		PublicVisible:   true,
		IntervalSeconds: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	results := []core.MonitorResult{
		{ID: "res_old_failed", TargetID: target.ID, Status: core.MonitorStatusFailed, CheckedAt: base.Add(-25 * time.Hour)},
		{ID: "res_boundary_ok", TargetID: target.ID, Status: core.MonitorStatusOK, CheckedAt: base.Add(-24 * time.Hour)},
		{ID: "res_recent_ok", TargetID: target.ID, Status: core.MonitorStatusOK, CheckedAt: base},
	}
	for _, result := range results {
		if err := service.AppendMonitorResult(result); err != nil {
			t.Fatal(err)
		}
	}

	page := service.monitorPage(true, base)
	if len(page.Targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(page.Targets))
	}
	history := page.Targets[0].History
	if got := len(history); got != 2 {
		t.Fatalf("history length = %d, want 2: %#v", got, history)
	}
	if history[0].ID != "res_recent_ok" || history[1].ID != "res_boundary_ok" {
		t.Fatalf("history = %#v, want only latest 24h results newest first", history)
	}
	if page.Targets[0].Availability != 100 {
		t.Fatalf("availability = %d, want 100 from the 24h window", page.Targets[0].Availability)
	}

	page = service.monitorPage(true, base.Add(49*time.Hour))
	if len(page.Targets) != 1 {
		t.Fatalf("stale targets = %d, want 1", len(page.Targets))
	}
	if got := len(page.Targets[0].History); got != 0 {
		t.Fatalf("stale history length = %d, want 0", got)
	}
	if page.Targets[0].Availability != 0 {
		t.Fatalf("stale availability = %d, want 0", page.Targets[0].Availability)
	}
}

func TestMonitorPageModelOptionsIncludeOnlyTextModels(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	for _, model := range []core.ModelConfig{
		{ID: "gpt-5", Provider: core.ProviderOpenAI, Type: core.ModelTypeText, Enabled: true},
		{ID: "gpt-4-disabled", Provider: core.ProviderOpenAI, Type: core.ModelTypeText, Enabled: false},
		{ID: "gpt-image-2", Provider: core.ProviderOpenAI, Type: core.ModelTypeImage, Enabled: true},
		{ID: "text-embedding-3-large", Provider: core.ProviderOpenAI, Type: core.ModelTypeEmbedding, Enabled: true},
	} {
		if err := repo.UpsertModel(model); err != nil {
			t.Fatal(err)
		}
	}

	page := service.MonitorPage(true)
	if len(page.Models) != 1 || page.Models[0].ID != "gpt-5" {
		t.Fatalf("monitor model options = %#v, want only gpt-5", page.Models)
	}
}

func TestMonitorPageMarksRunningTargets(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if err := repo.UpsertModel(core.ModelConfig{
		ID:            "gpt-5",
		Provider:      core.ProviderOpenAI,
		Type:          core.ModelTypeText,
		Enabled:       true,
		VisibleGroups: []string{core.DefaultAccountGroupName},
	}); err != nil {
		t.Fatal(err)
	}
	target, err := service.UpsertMonitorTarget(MonitorTargetInput{
		Name:          "Default Gpt-5",
		AccountGroup:  core.DefaultAccountGroupName,
		Model:         "gpt-5",
		Enabled:       true,
		PublicVisible: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !service.BeginMonitorRun(target.ID) {
		t.Fatal("BeginMonitorRun returned false for idle target")
	}
	if service.BeginMonitorRun(target.ID) {
		t.Fatal("BeginMonitorRun returned true for already running target")
	}
	page := service.MonitorPage(true)
	if len(page.Targets) != 1 || !page.Targets[0].Running {
		t.Fatalf("running target view = %#v, want Running=true", page.Targets)
	}

	service.EndMonitorRun(target.ID)
	page = service.MonitorPage(true)
	if len(page.Targets) != 1 || page.Targets[0].Running {
		t.Fatalf("running target view after end = %#v, want Running=false", page.Targets)
	}
}
