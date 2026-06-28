package web

import (
	"net/http"
	"strings"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
)

func (s *Server) handleSupportPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/support" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	locale := resolveLocale(w, r)
	page := s.control.ListSupportTickets(user, controlplane.SupportTicketFilter{Page: 1, PageSize: 50})
	active, messages := selectSupportConversation(s.control, user, page.Tickets, r.URL.Query().Get("ticket"))
	data := withCSRFData(map[string]any{
		"TitleKey":        "page_title_support",
		"ActiveNav":       "support",
		"Locale":          locale,
		"SupportTickets":  page.Tickets,
		"SupportActive":   active,
		"SupportMessages": messages,
		"SupportAdmin":    false,
	}, r)
	s.render(w, "support.html", locale, data)
}

func (s *Server) handleAdminSupportPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/support" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, ok := currentUserFromContext(r.Context())
	if !ok || !user.IsAdmin() {
		s.writeAdminRequired(w, r)
		return
	}
	locale := resolveLocale(w, r)
	filter := controlplane.SupportTicketFilter{
		Page:     1,
		PageSize: 100,
		Query:    strings.TrimSpace(r.URL.Query().Get("q")),
	}
	if status := strings.TrimSpace(r.URL.Query().Get("status")); status != "" {
		filter.Status = core.SupportTicketStatus(status)
	}
	page := s.control.ListSupportTickets(user, filter)
	active, messages := selectSupportConversation(s.control, user, page.Tickets, r.URL.Query().Get("ticket"))
	data := withCSRFData(map[string]any{
		"TitleKey":        "page_title_support_admin",
		"ActiveNav":       "support",
		"Locale":          locale,
		"SupportTickets":  page.Tickets,
		"SupportActive":   active,
		"SupportMessages": messages,
		"SupportAdmin":    true,
		"SupportFilter":   filter,
	}, r)
	s.render(w, "admin_support.html", locale, data)
}

func selectSupportConversation(control *controlplane.Service, user core.User, tickets []core.SupportTicket, requested string) (core.SupportTicket, []core.SupportMessage) {
	if control == nil {
		return core.SupportTicket{}, nil
	}
	requested = strings.TrimSpace(requested)
	if requested == "" && len(tickets) > 0 {
		requested = tickets[0].ID
	}
	if requested == "" {
		return core.SupportTicket{}, nil
	}
	ticket, err := control.GetSupportTicket(user, requested)
	if err != nil {
		return core.SupportTicket{}, nil
	}
	messages, err := control.ListSupportMessages(user, ticket.ID, 200)
	if err != nil {
		return ticket, nil
	}
	return ticket, messages
}
