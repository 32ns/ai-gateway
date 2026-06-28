package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
)

func (s *Server) handleAuditPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	filter := controlplane.AuditFilter{
		Kind:     core.AuditKind(strings.TrimSpace(r.URL.Query().Get("kind"))),
		Status:   strings.TrimSpace(r.URL.Query().Get("status")),
		Actor:    strings.TrimSpace(r.URL.Query().Get("actor")),
		Resource: strings.TrimSpace(r.URL.Query().Get("resource")),
		Page:     parsePositiveInt(r.URL.Query().Get("page"), 1),
		PageSize: 25,
	}
	page := s.control.AuditPage(r.Context(), filter)

	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":  "page_title_audit",
		"ActiveNav": "audit",
		"Locale":    locale,
		"Audit":     page,
	}, r)
	if deferredPartialRequested(r, "audit-page") {
		s.renderFragment(w, "audit.html", "audit_page", locale, data)
		return
	}
	s.render(w, "audit.html", locale, data)
}

func (s *Server) handleUsageLogsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/logs" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		s.writeConsoleAuthRequired(w, r)
		return
	}

	query := r.URL.Query()
	startedAt := parseOptionalDateTime(query.Get("started_at"))
	endedAt := parseOptionalDateTime(query.Get("ended_at"))

	filter := controlplane.UsageLogFilter{
		UserID:    strings.TrimSpace(r.URL.Query().Get("user_id")),
		ClientID:  strings.TrimSpace(r.URL.Query().Get("client_id")),
		Model:     strings.TrimSpace(r.URL.Query().Get("model")),
		Status:    core.BillingRequestStatus(strings.TrimSpace(r.URL.Query().Get("status"))),
		StartedAt: startedAt,
		EndedAt:   endedAt,
		Page:      parsePositiveInt(r.URL.Query().Get("page"), 1),
		PageSize:  25,
	}
	page := s.control.UsageLogPage(r.Context(), user, filter)

	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":  "page_title_usage_logs",
		"ActiveNav": "logs",
		"Locale":    locale,
		"Logs":      page,
	}, r)
	if deferredPartialRequested(r, "usage-logs-page") {
		s.renderFragment(w, "usage_logs.html", "usage_logs_page", locale, data)
		return
	}
	s.render(w, "usage_logs.html", locale, data)
}

func defaultUsageLogDateRange(now time.Time) (time.Time, time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	localNow := now.Local()
	startLocal := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, localNow.Location())
	endLocal := startLocal.Add(24 * time.Hour).Add(-time.Second)
	return startLocal.UTC(), endLocal.UTC()
}
