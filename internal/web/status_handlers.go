package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/gateway"
)

func (s *Server) handleStatusPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/status" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s == nil {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	r, _ = s.withOptionalConsoleUser(w, r)
	locale := resolveLocale(w, r)
	page := controlplane.MonitorPage{}
	if s.control != nil {
		page = s.control.MonitorPage(false)
	}
	data := withCSRFData(map[string]any{
		"TitleKey":  "page_title_status",
		"ActiveNav": "status",
		"Locale":    locale,
		"Page":      page,
	}, r)
	if deferredPartialRequested(r, "status-page") {
		s.renderFragment(w, "status.html", "status_page_fragment", locale, data)
		return
	}
	s.render(w, "status.html", locale, data)
}

func (s *Server) handleAdminStatusPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/status" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":      "page_title_status_admin",
		"ActiveNav":     "status",
		"Locale":        locale,
		"Page":          s.control.MonitorPage(true),
		"MonitorNotice": monitorNoticeText(locale, r.URL.Query().Get("notice")),
	}, r)
	if deferredPartialRequested(r, "admin-status-summary") {
		s.renderFragment(w, "admin_status.html", "admin_status_summary_fragment", locale, data)
		return
	}
	if deferredPartialRequested(r, "admin-status-targets") {
		s.renderFragment(w, "admin_status.html", "admin_status_targets_fragment", locale, data)
		return
	}
	s.render(w, "admin_status.html", locale, data)
}

func (s *Server) handleAdminStatusTargetCreate(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/status/targets" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, "/admin/status", err)
		return
	}
	input := monitorTargetInputFromForm(r, "")
	target, err := s.control.UpsertMonitorTarget(input)
	if err != nil {
		s.recordAdminAudit(r, "error", "monitor.create", "monitor_target", "", strings.TrimSpace(input.Name), err.Error())
		s.redirectWithNoticeError(w, r, "/admin/status", err)
		return
	}
	s.recordAdminAudit(r, "ok", "monitor.create", "monitor_target", target.ID, target.Name, monitorTargetAuditMessage(target))
	http.Redirect(w, r, "/admin/status?notice=status_target_saved", http.StatusSeeOther)
}

func (s *Server) handleAdminStatusTargetActions(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/admin/status/targets/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) != 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	targetID, action := parts[0], parts[1]
	switch action {
	case "update":
		s.handleAdminStatusTargetUpdate(w, r, targetID)
	case "delete":
		target, err := s.control.DeleteMonitorTarget(targetID)
		if err != nil {
			s.recordAdminAudit(r, "error", "monitor.delete", "monitor_target", targetID, "", err.Error())
			s.redirectWithNoticeError(w, r, "/admin/status", err)
			return
		}
		s.recordAdminAudit(r, "ok", "monitor.delete", "monitor_target", target.ID, target.Name, monitorTargetAuditMessage(target))
		http.Redirect(w, r, "/admin/status?notice=status_target_deleted", http.StatusSeeOther)
	case "run":
		s.handleAdminStatusTargetRun(w, r, targetID)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleAdminStatusTargetUpdate(w http.ResponseWriter, r *http.Request, targetID string) {
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, "/admin/status", err)
		return
	}
	input := monitorTargetInputFromForm(r, targetID)
	target, err := s.control.UpsertMonitorTarget(input)
	if err != nil {
		s.recordAdminAudit(r, "error", "monitor.update", "monitor_target", targetID, strings.TrimSpace(input.Name), err.Error())
		s.redirectWithNoticeError(w, r, "/admin/status", err)
		return
	}
	s.recordAdminAudit(r, "ok", "monitor.update", "monitor_target", target.ID, target.Name, monitorTargetAuditMessage(target))
	http.Redirect(w, r, "/admin/status?notice=status_target_saved", http.StatusSeeOther)
}

func (s *Server) handleAdminStatusTargetRun(w http.ResponseWriter, r *http.Request, targetID string) {
	target, err := s.control.GetMonitorTarget(targetID)
	if err != nil {
		s.recordAdminAudit(r, "error", "monitor.run", "monitor_target", targetID, "", err.Error())
		s.redirectWithNoticeError(w, r, "/admin/status", err)
		return
	}
	if !s.control.BeginMonitorRun(target.ID) {
		query := url.Values{}
		query.Set("notice", "status_probe_running")
		query.Set("target", target.ID)
		http.Redirect(w, r, "/admin/status?"+query.Encode(), http.StatusSeeOther)
		return
	}
	defer s.control.EndMonitorRun(target.ID)
	result, err := s.runStatusProbe(r.Context(), target)
	status := "ok"
	if err != nil {
		status = "error"
	}
	message := fmt.Sprintf("status=%s latency_ms=%d attempts=%d", result.Status, result.LatencyMS, len(result.Attempts))
	if err != nil {
		message += " error=" + err.Error()
	}
	s.recordAdminAudit(r, status, "monitor.run", "monitor_target", target.ID, target.Name, message)
	query := url.Values{}
	query.Set("notice", "status_probe_done")
	query.Set("target", target.ID)
	http.Redirect(w, r, "/admin/status?"+query.Encode(), http.StatusSeeOther)
}

func (s *Server) runStatusProbe(ctx context.Context, target core.MonitorTarget) (core.MonitorResult, error) {
	result := core.MonitorResult{
		TargetID:  target.ID,
		Status:    core.MonitorStatusFailed,
		CheckedAt: time.Now().UTC(),
	}
	if s == nil || s.control == nil {
		result.ErrorCode = "controlplane_unavailable"
		result.ErrorMessage = "control plane is unavailable"
		return result, errors.New(result.ErrorMessage)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		result.ErrorCode = "probe_canceled"
		result.ErrorMessage = ctx.Err().Error()
		return result, ctx.Err()
	}
	result.ID = s.control.NewMonitorResultID()
	if s.gateway == nil {
		result.ErrorCode = "gateway_unavailable"
		result.ErrorMessage = "gateway service is unavailable"
		_ = s.control.AppendMonitorResult(result)
		return result, errors.New(result.ErrorMessage)
	}
	timeout := time.Duration(target.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Duration(controlplane.DefaultMonitorTimeoutSeconds) * time.Second
	}
	probe, err := s.gateway.ProbeMonitorTarget(ctx, gateway.MonitorProbeInput{
		TargetID:     target.ID,
		AccountGroup: target.AccountGroup,
		Model:        target.Model,
		Prompt:       target.Prompt,
		Timeout:      timeout,
	})
	result.Status = probe.Status
	result.LatencyMS = probe.LatencyMS
	result.Provider = probe.Provider
	result.AccountID = probe.AccountID
	result.AccountLabel = probe.AccountLabel
	result.Attempts = probe.Attempts
	result.ErrorCode = probe.ErrorCode
	result.ErrorMessage = probe.ErrorMessage
	result.CheckedAt = probe.CheckedAt
	if result.Status == "" {
		result.Status = core.MonitorStatusFailed
	}
	if result.CheckedAt.IsZero() {
		result.CheckedAt = time.Now().UTC()
	}
	if errors.Is(err, context.Canceled) {
		return result, err
	}
	if appendErr := s.control.AppendMonitorResult(result); appendErr != nil {
		return result, appendErr
	}
	return result, err
}

func monitorTargetInputFromForm(r *http.Request, id string) controlplane.MonitorTargetInput {
	return controlplane.MonitorTargetInput{
		ID:              id,
		Name:            r.FormValue("name"),
		AccountGroup:    r.FormValue("account_group"),
		Model:           r.FormValue("model"),
		Enabled:         r.FormValue("enabled") != "",
		PublicVisible:   r.FormValue("public_visible") != "",
		IntervalSeconds: parsePositiveInt(r.FormValue("interval_seconds"), controlplane.DefaultMonitorIntervalSeconds),
		TimeoutSeconds:  parsePositiveInt(r.FormValue("timeout_seconds"), controlplane.DefaultMonitorTimeoutSeconds),
		Prompt:          r.FormValue("prompt"),
	}
}

func monitorTargetAuditMessage(target core.MonitorTarget) string {
	return fmt.Sprintf("group=%s model=%s enabled=%t public=%t interval=%ds timeout=%ds", target.AccountGroup, target.Model, target.Enabled, target.PublicVisible, target.IntervalSeconds, target.TimeoutSeconds)
}
