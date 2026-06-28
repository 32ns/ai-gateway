package web

import (
	"strconv"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
)

func monitorViewStatus(view controlplane.MonitorTargetView) core.MonitorStatus {
	if !view.HasLatest {
		return core.MonitorStatusUnknown
	}
	return view.Latest.Status
}

func monitorHistoryBars(history []core.MonitorResult) []core.MonitorResult {
	out := make([]core.MonitorResult, 0, len(history))
	for i := len(history) - 1; i >= 0; i-- {
		out = append(out, history[i])
	}
	return out
}

func monitorAdminHistoryTitle(locale string, result core.MonitorResult) string {
	parts := []string{monitorStatusText(locale, result.Status) + " - " + formatTime(result.CheckedAt)}
	switch result.Status {
	case core.MonitorStatusFailed:
		if reason := monitorResultFailureReason(result); reason != "" {
			parts = append(parts, reason)
		}
	case core.MonitorStatusDegraded:
		if reason := monitorResultFallbackReason(locale, result); reason != "" {
			parts = append(parts, reason)
		}
	}
	return strings.Join(parts, " | ")
}

func monitorResultFailureReason(result core.MonitorResult) string {
	for i := len(result.Attempts) - 1; i >= 0; i-- {
		attempt := result.Attempts[i]
		if monitorAttemptFailed(attempt) {
			return monitorAttemptDetail(attempt)
		}
	}
	if reason := monitorErrorText(result.ErrorCode, result.ErrorMessage); reason != "" {
		return reason
	}
	return ""
}

func monitorResultFallbackReason(locale string, result core.MonitorResult) string {
	var parts []string
	if attempt := firstFailedMonitorAttempt(result.Attempts); attempt != nil {
		if detail := monitorAttemptDetail(*attempt); detail != "" {
			if locale == localeZH {
				parts = append(parts, "切换原因: "+detail)
			} else {
				parts = append(parts, "Fallback reason: "+detail)
			}
		}
	}
	if label := lastSuccessfulMonitorAttemptLabel(result.Attempts); label != "" {
		if locale == localeZH {
			parts = append(parts, "切换到: "+label)
		} else {
			parts = append(parts, "Recovered via: "+label)
		}
	}
	return strings.Join(parts, "; ")
}

func firstFailedMonitorAttempt(attempts []core.AttemptRecord) *core.AttemptRecord {
	for i := range attempts {
		if monitorAttemptFailed(attempts[i]) {
			return &attempts[i]
		}
	}
	return nil
}

func lastSuccessfulMonitorAttemptLabel(attempts []core.AttemptRecord) string {
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].Status == "ok" {
			return monitorAttemptLabel(attempts[i])
		}
	}
	return ""
}

func monitorAttemptFailed(attempt core.AttemptRecord) bool {
	return attempt.Status != "" && attempt.Status != "ok" && attempt.Status != "running"
}

func monitorAttemptDetail(attempt core.AttemptRecord) string {
	parts := []string{monitorAttemptLabel(attempt)}
	if attempt.Status != "" {
		parts = append(parts, attempt.Status)
	}
	if reason := monitorErrorText(attempt.ErrorCode, attempt.ErrorMessage); reason != "" {
		parts = append(parts, reason)
	}
	return strings.Join(nonEmptyStrings(parts), " - ")
}

func monitorAttemptLabel(attempt core.AttemptRecord) string {
	parts := make([]string, 0, 2)
	if attempt.Provider != "" {
		parts = append(parts, string(attempt.Provider))
	}
	if attempt.AccountLabel != "" {
		parts = append(parts, attempt.AccountLabel)
	} else if attempt.AccountID != "" {
		parts = append(parts, attempt.AccountID)
	}
	return strings.Join(parts, " / ")
}

func monitorErrorText(code, message string) string {
	code = strings.TrimSpace(code)
	message = strings.TrimSpace(message)
	switch {
	case code != "" && message != "":
		return code + ": " + message
	case code != "":
		return code
	default:
		return message
	}
}

func nonEmptyStrings(values []string) []string {
	out := values[:0]
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
}

func monitorNextCheckAt(view controlplane.MonitorTargetView) time.Time {
	if !view.Target.Enabled || !view.HasLatest || view.Latest.CheckedAt.IsZero() {
		return time.Time{}
	}
	interval := view.Target.IntervalSeconds
	if interval <= 0 {
		interval = controlplane.DefaultMonitorIntervalSeconds
	}
	return view.Latest.CheckedAt.UTC().Add(time.Duration(interval) * time.Second)
}

func monitorNextCheckUnix(view controlplane.MonitorTargetView) int64 {
	next := monitorNextCheckAt(view)
	if next.IsZero() {
		return 0
	}
	return next.Unix()
}

func monitorAvailabilityClass(view controlplane.MonitorTargetView) string {
	if len(view.History) == 0 {
		return "monitor-availability-muted"
	}
	switch {
	case view.Availability >= 99:
		return "monitor-availability-good"
	case view.Availability >= 95:
		return "monitor-availability-warn"
	default:
		return "monitor-availability-bad"
	}
}

func monitorNextCheckText(view controlplane.MonitorTargetView) string {
	next := monitorNextCheckAt(view)
	if next.IsZero() {
		return "-"
	}
	remaining := time.Until(next)
	if remaining <= 0 {
		return "0s"
	}
	return compactDurationText(remaining)
}

func compactDurationText(duration time.Duration) string {
	seconds := int64((duration + time.Second - time.Nanosecond) / time.Second)
	if seconds < 0 {
		seconds = 0
	}
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	secs := seconds % 60
	if hours > 0 {
		return timeTextPart(hours, "h") + " " + timeTextPart(minutes, "m")
	}
	if minutes > 0 {
		return timeTextPart(minutes, "m") + " " + timeTextPart(secs, "s")
	}
	return timeTextPart(secs, "s")
}

func timeTextPart(value int64, unit string) string {
	return strconv.FormatInt(value, 10) + unit
}
