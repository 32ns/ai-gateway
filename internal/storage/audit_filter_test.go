package storage

import (
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
