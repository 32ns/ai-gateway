package storage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestMemoryMonitorTargetAndResultsLifecycle(t *testing.T) {
	repo := NewMemoryRepository()

	target := core.MonitorTarget{
		ID:              "mon_default_gpt5",
		Name:            "Default GPT-5",
		AccountGroup:    "",
		Model:           "gpt-5",
		Enabled:         true,
		PublicVisible:   true,
		IntervalSeconds: 300,
		TimeoutSeconds:  30,
		Prompt:          "pong",
	}
	if err := repo.UpsertMonitorTarget(target); err != nil {
		t.Fatalf("UpsertMonitorTarget returned error: %v", err)
	}
	stored, err := repo.GetMonitorTarget(target.ID)
	if err != nil {
		t.Fatalf("GetMonitorTarget returned error: %v", err)
	}
	if stored.AccountGroup != core.DefaultAccountGroupName {
		t.Fatalf("account group = %q, want %q", stored.AccountGroup, core.DefaultAccountGroupName)
	}
	if stored.CreatedAt.IsZero() || stored.UpdatedAt.IsZero() {
		t.Fatalf("stored timestamps should be populated: %#v", stored)
	}

	createdAt := stored.CreatedAt
	stored.Name = "Default GPT-5 Canary"
	stored.PublicVisible = false
	if err := repo.UpsertMonitorTarget(stored); err != nil {
		t.Fatalf("second UpsertMonitorTarget returned error: %v", err)
	}
	updated, err := repo.GetMonitorTarget(target.ID)
	if err != nil {
		t.Fatalf("GetMonitorTarget after update returned error: %v", err)
	}
	if !updated.CreatedAt.Equal(createdAt) {
		t.Fatalf("CreatedAt changed from %s to %s", createdAt, updated.CreatedAt)
	}
	if updated.Name != "Default GPT-5 Canary" || updated.PublicVisible {
		t.Fatalf("updated target = %#v", updated)
	}

	base := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)
	results := []core.MonitorResult{
		{ID: "res_old", TargetID: target.ID, Status: core.MonitorStatusFailed, CheckedAt: base},
		{ID: "res_new", TargetID: target.ID, Status: core.MonitorStatusOK, CheckedAt: base.Add(time.Minute), Attempts: []core.AttemptRecord{{AccountID: "acct_ok", Status: "ok"}}},
		{ID: "res_other", TargetID: "mon_other", Status: core.MonitorStatusOK, CheckedAt: base.Add(2 * time.Minute)},
	}
	for _, result := range results {
		if err := repo.AppendMonitorResult(result); err != nil {
			t.Fatalf("AppendMonitorResult(%s) returned error: %v", result.ID, err)
		}
	}
	history := repo.ListMonitorResults(target.ID, 10)
	if len(history) != 2 {
		t.Fatalf("history length = %d, want 2: %#v", len(history), history)
	}
	if history[0].ID != "res_new" || history[1].ID != "res_old" {
		t.Fatalf("history order = %#v, want newest first", history)
	}
	history[0].Attempts[0].AccountID = "mutated"
	history = repo.ListMonitorResults(target.ID, 1)
	if len(history) != 1 || history[0].ID != "res_new" || history[0].Attempts[0].AccountID != "acct_ok" {
		t.Fatalf("history clone/limit = %#v", history)
	}
	latest := repo.ListLatestMonitorResults()
	if latest[target.ID].ID != "res_new" || latest["mon_other"].ID != "res_other" {
		t.Fatalf("latest = %#v", latest)
	}

	if err := repo.DeleteMonitorTarget(target.ID); err != nil {
		t.Fatalf("DeleteMonitorTarget returned error: %v", err)
	}
	if _, err := repo.GetMonitorTarget(target.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetMonitorTarget deleted err = %v, want ErrNotFound", err)
	}
	if history := repo.ListMonitorResults(target.ID, 10); len(history) != 0 {
		t.Fatalf("deleted target history = %#v, want empty", history)
	}
	if latest := repo.ListLatestMonitorResults(); latest[target.ID].ID != "" {
		t.Fatalf("latest after delete = %#v", latest)
	}
}

func TestMonitorResultsTrimPerTarget(t *testing.T) {
	cases := []struct {
		name string
		repo func(t *testing.T) Repository
	}{
		{
			name: "memory",
			repo: func(t *testing.T) Repository {
				t.Helper()
				return NewMemoryRepository()
			},
		},
		{
			name: "sqlite",
			repo: func(t *testing.T) Repository {
				t.Helper()
				repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
				if err != nil {
					t.Fatalf("NewSQLiteRepository returned error: %v", err)
				}
				t.Cleanup(func() { _ = repo.Close() })
				return repo
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := tc.repo(t)
			base := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)
			for i := 0; i < maxMonitorResultsPerTarget+2; i++ {
				result := core.MonitorResult{
					ID:        "res_default_" + storageIDPart(time.Unix(int64(i), 0).Format(time.RFC3339Nano)),
					TargetID:  "mon_default",
					Status:    core.MonitorStatusOK,
					CheckedAt: base.Add(time.Duration(i) * 30 * time.Second),
				}
				if err := repo.AppendMonitorResult(result); err != nil {
					t.Fatalf("AppendMonitorResult(%d) returned error: %v", i, err)
				}
			}
			if err := repo.AppendMonitorResult(core.MonitorResult{
				ID:        "res_other",
				TargetID:  "mon_other",
				Status:    core.MonitorStatusFailed,
				CheckedAt: base.Add(time.Hour),
			}); err != nil {
				t.Fatalf("AppendMonitorResult(other) returned error: %v", err)
			}

			history := repo.ListMonitorResults("mon_default", maxMonitorResultsPerTarget+10)
			if len(history) != maxMonitorResultsPerTarget {
				t.Fatalf("history length = %d, want %d", len(history), maxMonitorResultsPerTarget)
			}
			if history[len(history)-1].CheckedAt != base.Add(time.Minute) {
				t.Fatalf("oldest retained checked_at = %s, want %s", history[len(history)-1].CheckedAt, base.Add(time.Minute))
			}
			if err := repo.AppendMonitorResult(core.MonitorResult{
				ID:        "res_default_very_old",
				TargetID:  "mon_default",
				Status:    core.MonitorStatusFailed,
				CheckedAt: base.Add(-time.Hour),
			}); err != nil {
				t.Fatalf("AppendMonitorResult(very old) returned error: %v", err)
			}
			history = repo.ListMonitorResults("mon_default", maxMonitorResultsPerTarget+10)
			if len(history) != maxMonitorResultsPerTarget || history[len(history)-1].ID == "res_default_very_old" {
				t.Fatalf("trimmed history after old append = len %d oldest %#v", len(history), history[len(history)-1])
			}
			for i := 0; i <= 30; i++ {
				result := core.MonitorResult{
					ID:        "res_window_" + storageIDPart(time.Unix(int64(i), 0).Format(time.RFC3339Nano)),
					TargetID:  "mon_window",
					Status:    core.MonitorStatusOK,
					CheckedAt: base.Add(time.Duration(i) * time.Hour),
				}
				if err := repo.AppendMonitorResult(result); err != nil {
					t.Fatalf("AppendMonitorResult(window %d) returned error: %v", i, err)
				}
			}
			window := repo.ListMonitorResults("mon_window", 40)
			if len(window) != 25 {
				t.Fatalf("window history length = %d, want 25", len(window))
			}
			if window[len(window)-1].CheckedAt != base.Add(6*time.Hour) {
				t.Fatalf("oldest window checked_at = %s, want %s", window[len(window)-1].CheckedAt, base.Add(6*time.Hour))
			}
			other := repo.ListMonitorResults("mon_other", 10)
			if len(other) != 1 || other[0].ID != "res_other" {
				t.Fatalf("other target history = %#v, want res_other", other)
			}
		})
	}
}
