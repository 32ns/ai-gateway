package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
)

const accountBatchDetectItemPreviewLimit = 20

func (s *Server) handleAccountBatchAction(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/accounts/batch" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
		return
	}

	action := controlplane.AccountBatchAction(r.FormValue("action"))
	accountFilter := accountFilterReturnValue(r)
	if accountBatchWantsJobJSON(r) && accountBatchActionRunsAsJob(action) {
		s.handleAccountBatchJobStart(w, r, action)
		return
	}
	result, err := s.control.ApplyAccountBatch(r.Context(), controlplane.AccountBatchInput{
		Action:      action,
		AccountIDs:  r.Form["account_id"],
		TargetGroup: accountBatchTargetGroupValue(r),
	})
	if err != nil {
		s.recordAdminAudit(r, "error", "account.batch."+strings.TrimSpace(string(action)), "account", "", "", err.Error())
		s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilter), err)
		return
	}

	message := s.accountBatchResultMessage(resolveLocale(w, r), result)
	auditStatus := "ok"
	tone := "good"
	if result.Failed > 0 {
		tone = "bad"
		auditStatus = "error"
	}
	s.recordAdminAudit(r, auditStatus, "account.batch."+string(result.Action), "account", "", "", accountBatchAuditMessage(result))
	s.publishAccountPoolChanged()
	redirectGroup := accountGroupReturnKey(r)
	if result.Action == controlplane.AccountBatchActionMoveGroup {
		redirectGroup = accountGroupQueryKey(result.TargetGroup)
	}
	http.Redirect(w, r, accountBatchRedirect(redirectGroup, accountFilter, tone, message), http.StatusSeeOther)
}

func (s *Server) handleAccountBatchJobStart(w http.ResponseWriter, r *http.Request, action controlplane.AccountBatchAction) {
	locale := resolveLocale(w, r)
	snapshot, err := s.control.StartAccountBatch(r.Context(), controlplane.AccountBatchInput{
		Action:      action,
		AccountIDs:  r.Form["account_id"],
		TargetGroup: accountBatchTargetGroupValue(r),
	})
	if err != nil {
		s.recordAdminAudit(r, "error", "account.batch."+strings.TrimSpace(string(action)), "account", "", "", err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"status":  "error",
			"message": accountBatchJobErrorMessage(locale, err),
		})
		return
	}
	s.recordAdminAudit(r, "ok", "account.batch."+string(snapshot.Action)+".start", "account", snapshot.ID, "", accountBatchJobAuditMessage(snapshot))
	s.publishAccountBatchUpdated(locale, snapshot)
	s.watchAccountBatchJob(locale, snapshot.ID)
	writeJSON(w, http.StatusAccepted, s.accountBatchJobPayload(locale, snapshot))
}

func (s *Server) handleAccountBatchJobActions(w http.ResponseWriter, r *http.Request) {
	const prefix = "/admin/accounts/batch/jobs/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if rest == "" {
		http.NotFound(w, r)
		return
	}
	if rest == "active" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		snapshot, ok := s.control.ActiveAccountBatchJob()
		if !ok {
			writeJSON(w, http.StatusOK, map[string]any{"status": "idle"})
			return
		}
		writeJSON(w, http.StatusOK, s.accountBatchJobPayload(resolveLocale(w, r), snapshot))
		return
	}
	parts := strings.Split(rest, "/")
	jobID := strings.TrimSpace(parts[0])
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		snapshot, ok := s.control.GetAccountBatchJob(jobID)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"status": "error", "message": translate(resolveLocale(w, r), "account_batch_job_not_found")})
			return
		}
		writeJSON(w, http.StatusOK, s.accountBatchJobPayload(resolveLocale(w, r), snapshot))
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		snapshot, ok := s.control.CancelAccountBatchJob(jobID)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"status": "error", "message": translate(resolveLocale(w, r), "account_batch_job_not_found")})
			return
		}
		s.recordAdminAudit(r, "ok", "account.batch."+string(snapshot.Action)+".cancel", "account", snapshot.ID, "", accountBatchJobAuditMessage(snapshot))
		s.publishAccountBatchUpdated(resolveLocale(w, r), snapshot)
		writeJSON(w, http.StatusOK, s.accountBatchJobPayload(resolveLocale(w, r), snapshot))
		return
	}
	http.NotFound(w, r)
}

func (s *Server) accountBatchResultMessage(locale string, result controlplane.AccountBatchResult) string {
	if result.Action == controlplane.AccountBatchActionTest {
		return accountBatchDetectResultMessage(locale, result)
	}
	return translatef(
		locale,
		"account_batch_result",
		accountBatchActionText(locale, result.Action),
		result.Succeeded,
		result.Failed,
		result.Skipped,
	)
}

func accountBatchDetectResultMessage(locale string, result controlplane.AccountBatchResult) string {
	lines := []string{
		translatef(locale, "account_batch_detect_result", result.Succeeded, result.Failed, result.Skipped),
	}
	if section := accountBatchDetectSectionLines(locale, "account_batch_detect_passed_items", accountBatchDetectItemsByStatus(result, "ok"), false); len(section) > 0 {
		lines = append(lines, section...)
	}
	if section := accountBatchDetectSectionLines(locale, "account_batch_detect_failed_items", accountBatchDetectItemsByStatus(result, "failed"), true); len(section) > 0 {
		lines = append(lines, section...)
	}
	if section := accountBatchDetectSectionLines(locale, "account_batch_detect_skipped_items", accountBatchDetectItemsByStatus(result, "skipped"), false); len(section) > 0 {
		lines = append(lines, section...)
	}
	switch {
	case result.Failed > 0:
		lines = append(lines, translate(locale, "account_batch_detect_failure_hint"))
	case result.Skipped > 0:
		lines = append(lines, translate(locale, "account_batch_detect_skipped_hint"))
	default:
		lines = append(lines, translate(locale, "account_batch_detect_all_ok_hint"))
	}
	return truncateNoticeMessage(strings.Join(lines, "\n"))
}

func accountBatchDetectSectionLines(locale string, labelKey string, items []controlplane.AccountBatchItemResult, includeReason bool) []string {
	if len(items) == 0 {
		return nil
	}
	lines := []string{accountBatchDetectSectionTitle(locale, labelKey)}
	limit := accountBatchDetectItemPreviewLimit
	if limit <= 0 || limit > len(items) {
		limit = len(items)
	}
	for index, item := range items {
		if index >= limit {
			break
		}
		summary := accountBatchItemLabel(item)
		if includeReason {
			if reason := accountCheckReasonText(locale, item.Message); reason != "" {
				summary += " - " + reason
			}
		}
		lines = append(lines, "- "+summary)
	}
	if len(items) > limit {
		if locale == localeZH {
			lines = append(lines, fmt.Sprintf("\u8fd8\u6709 %d \u4e2a\u672a\u663e\u793a", len(items)-limit))
		} else {
			lines = append(lines, fmt.Sprintf("%d more not shown", len(items)-limit))
		}
	}
	return lines
}

func accountBatchDetectSectionTitle(locale string, labelKey string) string {
	suffix := ":"
	if locale == localeZH {
		suffix = "\uff1a"
	}
	return translate(locale, labelKey) + suffix
}

func accountBatchDetectItemsByStatus(result controlplane.AccountBatchResult, status string) []controlplane.AccountBatchItemResult {
	if len(result.Items) == 0 {
		return nil
	}
	status = strings.TrimSpace(status)
	out := make([]controlplane.AccountBatchItemResult, 0, len(result.Items))
	for _, item := range result.Items {
		if strings.EqualFold(strings.TrimSpace(item.Status), status) {
			out = append(out, item)
		}
	}
	return out
}

func (s *Server) accountBatchJobPayload(locale string, snapshot controlplane.AccountBatchJobSnapshot) map[string]any {
	result := snapshot.Result()
	message := s.accountBatchResultMessage(locale, result)
	if snapshot.Status == controlplane.AccountBatchJobQueued {
		message = translatef(locale, "account_batch_job_queued", accountBatchActionText(locale, snapshot.Action), snapshot.Total)
	}
	if snapshot.Status == controlplane.AccountBatchJobRunning {
		message = translatef(locale, "account_batch_job_running", accountBatchActionText(locale, snapshot.Action), snapshot.Done, snapshot.Total)
	}
	if snapshot.Status == controlplane.AccountBatchJobCancelled {
		message = translatef(locale, "account_batch_job_cancelled", snapshot.Done, snapshot.Total)
	}
	tone := "good"
	if snapshot.Failed > 0 || snapshot.Status == controlplane.AccountBatchJobCancelled {
		tone = "bad"
	}
	items := make([]map[string]string, 0, len(snapshot.Items))
	for _, item := range snapshot.Items {
		items = append(items, map[string]string{
			"account_id": item.AccountID,
			"label":      item.Label,
			"status":     item.Status,
			"message":    item.Message,
		})
	}
	percent := 0
	if snapshot.Total > 0 {
		percent = int(float64(snapshot.Done) / float64(snapshot.Total) * 100)
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	currentText := ""
	if strings.TrimSpace(snapshot.Current) != "" {
		currentText = translatef(locale, "account_batch_job_current", snapshot.Current)
	}
	return map[string]any{
		"status":       "ok",
		"id":           snapshot.ID,
		"action":       string(snapshot.Action),
		"action_text":  accountBatchActionText(locale, snapshot.Action),
		"target_group": snapshot.TargetGroup,
		"state":        string(snapshot.Status),
		"total":        snapshot.Total,
		"done":         snapshot.Done,
		"succeeded":    snapshot.Succeeded,
		"failed":       snapshot.Failed,
		"skipped":      snapshot.Skipped,
		"current":      snapshot.Current,
		"percent":      percent,
		"tone":         tone,
		"message":      message,
		"counts_text":  translatef(locale, "account_batch_job_counts", snapshot.Succeeded, snapshot.Failed, snapshot.Skipped),
		"current_text": currentText,
		"elapsed_text": accountBatchJobElapsedText(locale, snapshot),
		"items":        items,
	}
}

func accountBatchJobElapsedText(locale string, snapshot controlplane.AccountBatchJobSnapshot) string {
	start := snapshot.StartedAt
	if start.IsZero() {
		return ""
	}
	end := time.Now().UTC()
	if snapshot.FinishedAt != nil {
		end = *snapshot.FinishedAt
	}
	seconds := int(end.Sub(start).Seconds())
	if seconds < 0 {
		seconds = 0
	}
	minutes := seconds / 60
	seconds = seconds % 60
	return translatef(locale, "account_batch_job_elapsed", strconv.Itoa(minutes), fmt.Sprintf("%02d", seconds))
}

func accountBatchActionRunsAsJob(action controlplane.AccountBatchAction) bool {
	switch action {
	case controlplane.AccountBatchActionRefreshQuota,
		controlplane.AccountBatchActionTest,
		controlplane.AccountBatchActionMoveGroup:
		return true
	default:
		return false
	}
}

func accountBatchWantsJobJSON(r *http.Request) bool {
	if r == nil {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func accountBatchJobErrorMessage(locale string, err error) string {
	message := ""
	if err != nil {
		message = strings.TrimSpace(err.Error())
	}
	if message == "" {
		return translate(locale, "account_batch_error_unknown")
	}
	if message == "select at least one account" {
		return translate(locale, "account_batch_error_no_selection")
	}
	if strings.Contains(message, "already running") {
		return translate(locale, "account_batch_job_already_running")
	}
	if strings.Contains(message, "cannot run as a background job") {
		return translate(locale, "account_batch_job_not_live_action")
	}
	if strings.HasPrefix(message, "unsupported account batch action") {
		return translate(locale, "account_batch_error_unsupported")
	}
	var action string
	var max int
	if _, scanErr := fmt.Sscanf(message, "batch action %s supports at most %d accounts at a time", &action, &max); scanErr == nil && max > 0 {
		return translatef(locale, "account_batch_error_too_many", accountBatchActionText(locale, controlplane.AccountBatchAction(action)), max)
	}
	return message
}

func accountBatchItemLabel(item controlplane.AccountBatchItemResult) string {
	if label := strings.TrimSpace(item.Label); label != "" {
		return label
	}
	return strings.TrimSpace(item.AccountID)
}

func accountBatchActionText(locale string, action controlplane.AccountBatchAction) string {
	switch action {
	case controlplane.AccountBatchActionDisable:
		return translate(locale, "account_batch_action_disable")
	case controlplane.AccountBatchActionEnable:
		return translate(locale, "account_batch_action_enable")
	case controlplane.AccountBatchActionDelete:
		return translate(locale, "account_batch_action_delete")
	case controlplane.AccountBatchActionRefreshQuota:
		return translate(locale, "account_batch_action_refresh_quota")
	case controlplane.AccountBatchActionTest:
		return translate(locale, "account_batch_action_test")
	case controlplane.AccountBatchActionMoveGroup:
		return translate(locale, "account_batch_action_move_group")
	default:
		value := strings.TrimSpace(string(action))
		if value == "" {
			return translate(locale, "action")
		}
		return value
	}
}

func accountBatchRedirect(groupKey, filterKey, tone, message string) string {
	message = truncateNoticeMessage(message)
	values := url.Values{}
	if groupKey = strings.TrimSpace(groupKey); groupKey != "" {
		values.Set("group", groupKey)
	}
	if filterKey = normalizeAccountFilterValue(filterKey); filterKey != "all" {
		values.Set("filter", filterKey)
	}
	values.Set("batch_tone", strings.TrimSpace(tone))
	values.Set("batch_message", message)
	return "/admin/accounts?" + values.Encode()
}

func truncateNoticeMessage(message string) string {
	return truncateRunes(strings.TrimSpace(message), 520)
}

func accountsGroupFilterHref(groupKey, filterKey string) string {
	values := url.Values{}
	if groupKey = strings.TrimSpace(groupKey); groupKey != "" {
		values.Set("group", groupKey)
	}
	if filterKey = normalizeAccountFilterValue(filterKey); filterKey != "all" {
		values.Set("filter", filterKey)
	}
	if len(values) == 0 {
		return "/admin/accounts"
	}
	return "/admin/accounts?" + values.Encode()
}

func accountFilterReturnValue(r *http.Request) string {
	if r == nil {
		return "all"
	}
	if filter := strings.TrimSpace(r.FormValue("current_filter")); filter != "" {
		return normalizeAccountFilterValue(filter)
	}
	return normalizeAccountFilterValue(r.URL.Query().Get("filter"))
}

func accountBatchTargetGroupValue(r *http.Request) string {
	if r == nil {
		return ""
	}
	return strings.TrimSpace(r.FormValue("target_group"))
}

func normalizeAccountFilterValue(value string) string {
	switch strings.TrimSpace(value) {
	case "normal", "cooling", "time_limit", "week_limit", "exception", "disabled":
		return strings.TrimSpace(value)
	default:
		return "all"
	}
}

func accountBatchAuditMessage(result controlplane.AccountBatchResult) string {
	message := fmt.Sprintf("action=%s total=%d succeeded=%d failed=%d skipped=%d", result.Action, result.Total, result.Succeeded, result.Failed, result.Skipped)
	if result.Action == controlplane.AccountBatchActionMoveGroup {
		message += fmt.Sprintf(" target_group=%q", result.TargetGroup)
	}
	return message
}

func accountBatchJobAuditMessage(snapshot controlplane.AccountBatchJobSnapshot) string {
	return fmt.Sprintf(
		"job=%s action=%s status=%s total=%d done=%d succeeded=%d failed=%d skipped=%d",
		snapshot.ID,
		snapshot.Action,
		snapshot.Status,
		snapshot.Total,
		snapshot.Done,
		snapshot.Succeeded,
		snapshot.Failed,
		snapshot.Skipped,
	)
}
