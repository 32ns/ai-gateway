package web

import (
	"strings"

	"github.com/32ns/ai-gateway/internal/core"
)

func supportStatusText(locale string, status core.SupportTicketStatus) string {
	switch status {
	case core.SupportTicketPendingAdmin:
		return translate(locale, "support_status_pending_admin")
	case core.SupportTicketPendingUser:
		return translate(locale, "support_status_pending_user")
	case core.SupportTicketResolved:
		return translate(locale, "support_status_resolved")
	case core.SupportTicketClosed:
		return translate(locale, "support_status_closed")
	default:
		if strings.TrimSpace(string(status)) == "" {
			return translate(locale, "support_status_open")
		}
		return translate(locale, "support_status_open")
	}
}

func avatarInitial(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	for _, r := range value {
		return strings.ToUpper(string(r))
	}
	return "-"
}
