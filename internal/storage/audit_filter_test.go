package storage

import (
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestGatewayAuditFilterDropsGatewayEventsOnly(t *testing.T) {
	base := NewMemoryRepository()
	repo := NewGatewayAuditFilterRepository(base, false)

	if err := repo.AppendAudit(core.AuditEvent{ID: "gateway", Kind: core.AuditKindGateway, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := repo.AppendAudit(core.AuditEvent{ID: "admin", Kind: core.AuditKindAdmin, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	audits := base.ListAudit(10)
	if len(audits) != 1 || audits[0].ID != "admin" {
		t.Fatalf("audits = %#v, want only admin event", audits)
	}
}

func TestGatewayAuditFilterKeepsGatewayErrorsOnly(t *testing.T) {
	base := NewMemoryRepository()
	repo := NewGatewayAuditFilterRepositoryWithOptions(base, GatewayAuditFilterOptions{ErrorsOnly: true})
	now := time.Now().UTC()

	events := []core.AuditEvent{
		{ID: "gateway_ok", Kind: core.AuditKindGateway, Status: "ok", CreatedAt: now},
		{ID: "gateway_error", Kind: core.AuditKindGateway, Status: "error", CreatedAt: now.Add(time.Second)},
		{ID: "admin_ok", Kind: core.AuditKindAdmin, Status: "ok", CreatedAt: now.Add(2 * time.Second)},
	}
	for _, event := range events {
		if err := repo.AppendAudit(event); err != nil {
			t.Fatalf("AppendAudit(%s) returned error: %v", event.ID, err)
		}
	}

	audits := base.ListAudit(10)
	if got := auditEventIDs(audits); got != "admin_ok,gateway_error" {
		t.Fatalf("audits = %s, want admin_ok,gateway_error", got)
	}
}

func TestGatewayAuditFilterKeepsGatewayErrorsOnlyInBatch(t *testing.T) {
	base := NewMemoryRepository()
	repo := NewGatewayAuditFilterRepositoryWithOptions(base, GatewayAuditFilterOptions{ErrorsOnly: true})
	now := time.Now().UTC()

	if err := repo.(*GatewayAuditFilterRepository).AppendAuditBatch([]core.AuditEvent{
		{ID: "gateway_ok", Kind: core.AuditKindGateway, Status: "ok", CreatedAt: now},
		{ID: "gateway_error", Kind: core.AuditKindGateway, Status: " ERROR ", CreatedAt: now.Add(time.Second)},
		{ID: "admin_ok", Kind: core.AuditKindAdmin, Status: "ok", CreatedAt: now.Add(2 * time.Second)},
	}); err != nil {
		t.Fatalf("AppendAuditBatch returned error: %v", err)
	}

	audits := base.ListAudit(10)
	if got := auditEventIDs(audits); got != "admin_ok,gateway_error" {
		t.Fatalf("audits = %s, want admin_ok,gateway_error", got)
	}
}

func TestGatewayAuditFilterEnabledKeepsAllGatewayEvents(t *testing.T) {
	base := NewMemoryRepository()
	repo := NewGatewayAuditFilterRepositoryWithOptions(base, GatewayAuditFilterOptions{Enabled: true, ErrorsOnly: true})

	if err := repo.AppendAudit(core.AuditEvent{ID: "gateway_ok", Kind: core.AuditKindGateway, Status: "ok", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	audits := base.ListAudit(10)
	if len(audits) != 1 || audits[0].ID != "gateway_ok" {
		t.Fatalf("audits = %#v, want gateway_ok", audits)
	}
}

func TestGatewayAuditFilterUsesDynamicSettings(t *testing.T) {
	base := NewMemoryRepository()
	repo := NewGatewayAuditFilterRepositoryWithOptions(base, GatewayAuditFilterOptions{ErrorsOnly: false})
	now := time.Now().UTC()

	if err := repo.AppendAudit(core.AuditEvent{ID: "fallback_error", Kind: core.AuditKindGateway, Status: "error", CreatedAt: now}); err != nil {
		t.Fatalf("AppendAudit(fallback_error) returned error: %v", err)
	}
	if got := auditEventIDs(base.ListAudit(10)); got != "" {
		t.Fatalf("audits = %s, want none before dynamic setting is enabled", got)
	}

	settings := core.DefaultSystemSettings()
	settings.Retention.GatewayAuditErrors = true
	settings.Retention.GatewayAuditRetentionDays = 3
	if err := base.UpsertSystemSettings(settings); err != nil {
		t.Fatalf("UpsertSystemSettings enable returned error: %v", err)
	}
	if err := repo.AppendAudit(core.AuditEvent{ID: "dynamic_ok", Kind: core.AuditKindGateway, Status: "ok", CreatedAt: now.Add(time.Second)}); err != nil {
		t.Fatalf("AppendAudit(dynamic_ok) returned error: %v", err)
	}
	if err := repo.AppendAudit(core.AuditEvent{ID: "dynamic_error", Kind: core.AuditKindGateway, Status: "error", CreatedAt: now.Add(2 * time.Second)}); err != nil {
		t.Fatalf("AppendAudit(dynamic_error) returned error: %v", err)
	}
	if got := auditEventIDs(base.ListAudit(10)); got != "dynamic_error" {
		t.Fatalf("audits = %s, want dynamic_error", got)
	}

	settings.Retention.GatewayAuditErrors = false
	if err := base.UpsertSystemSettings(settings); err != nil {
		t.Fatalf("UpsertSystemSettings disable returned error: %v", err)
	}
	if err := repo.AppendAudit(core.AuditEvent{ID: "disabled_error", Kind: core.AuditKindGateway, Status: "error", CreatedAt: now.Add(3 * time.Second)}); err != nil {
		t.Fatalf("AppendAudit(disabled_error) returned error: %v", err)
	}
	if got := auditEventIDs(base.ListAudit(10)); got != "dynamic_error" {
		t.Fatalf("audits = %s, want dynamic_error after disabling dynamic setting", got)
	}
}

func TestMemoryRepositoryGatewayAuditRetentionTrimsOnlyOldGatewayAudit(t *testing.T) {
	repo := NewMemoryRepository()
	now := time.Now().UTC()
	repo.audit = []core.AuditEvent{
		{ID: "recent_gateway", Kind: core.AuditKindGateway, Status: "error", CreatedAt: now.Add(-time.Hour)},
		{ID: "old_admin", Kind: core.AuditKindAdmin, Status: "ok", CreatedAt: now.Add(-48 * time.Hour)},
		{ID: "old_gateway_ok", Kind: core.AuditKindGateway, Status: "ok", CreatedAt: now.Add(-48 * time.Hour)},
		{ID: "old_gateway_spaced_error", Kind: core.AuditKindGateway, Status: " ERROR ", CreatedAt: now.Add(-48 * time.Hour)},
		{ID: "old_gateway", Kind: core.AuditKindGateway, Status: "error", CreatedAt: now.Add(-48 * time.Hour)},
	}

	if err := repo.ConfigureGatewayAuditRetention(1); err != nil {
		t.Fatalf("ConfigureGatewayAuditRetention returned error: %v", err)
	}

	audits := repo.ListAudit(10)
	if got := auditEventIDs(audits); got != "recent_gateway,old_admin,old_gateway_ok" {
		t.Fatalf("audits = %s, want recent_gateway,old_admin,old_gateway_ok", got)
	}
}

func auditEventIDs(events []core.AuditEvent) string {
	ids := make([]string, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.ID)
	}
	return strings.Join(ids, ",")
}
