package storage

import (
	"strings"

	"github.com/32ns/ai-gateway/internal/core"
)

type AuditQuery struct {
	Kind     core.AuditKind
	Status   string
	Actor    string
	Resource string
	Offset   int
	Limit    int
}

func normalizeAuditQuery(query AuditQuery) AuditQuery {
	query.Kind = core.AuditKind(strings.TrimSpace(string(query.Kind)))
	query.Status = strings.TrimSpace(query.Status)
	query.Actor = strings.TrimSpace(query.Actor)
	query.Resource = strings.TrimSpace(query.Resource)
	if query.Offset < 0 {
		query.Offset = 0
	}
	if query.Limit <= 0 {
		query.Limit = 25
	}
	return query
}

func auditMatchesQuery(event core.AuditEvent, query AuditQuery) bool {
	if query.Kind != "" && event.EffectiveKind() != query.Kind {
		return false
	}
	if query.Status != "" && event.Status != query.Status {
		return false
	}
	if query.Actor != "" && !strings.Contains(strings.ToLower(event.Actor), strings.ToLower(query.Actor)) {
		return false
	}
	if query.Resource != "" && !strings.Contains(auditResourceIndexText(event), strings.ToLower(query.Resource)) {
		return false
	}
	return true
}

func auditIndexValues(event core.AuditEvent) (kind string, status string, actorText string, resourceText string) {
	return string(event.EffectiveKind()), event.Status, strings.ToLower(event.Actor), auditResourceIndexText(event)
}

func auditResourceIndexText(event core.AuditEvent) string {
	return strings.ToLower(strings.Join([]string{
		event.ResourceType,
		event.ResourceID,
		event.ResourceName,
		string(event.Provider),
		event.AccountID,
		event.ClientID,
		event.ClientName,
		event.Model,
	}, " "))
}
