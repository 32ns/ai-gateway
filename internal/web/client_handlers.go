package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
)

const clientPageSize = 25

type clientPageInfo struct {
	Page      int
	PageSize  int
	Total     int
	HasPrev   bool
	PrevPage  int
	HasNext   bool
	NextPage  int
	FirstItem int
	LastItem  int
}

func (s *Server) handleClientsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/clients" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, _ := currentUserFromContext(r.Context())
	requestedPage := parsePositiveInt(r.URL.Query().Get("page"), 1)
	pageClients, totalClients := s.control.ClientsForUserPage(user, clientPageOffset(requestedPage, clientPageSize), clientPageSize)
	pageInfo := clientPageInfoForTotal(requestedPage, clientPageSize, totalClients)
	if pageInfo.Page != requestedPage {
		pageClients, totalClients = s.control.ClientsForUserPage(user, clientPageOffset(pageInfo.Page, clientPageSize), clientPageSize)
		pageInfo = clientPageInfoForTotal(pageInfo.Page, clientPageSize, totalClients)
	}
	state := controlplane.Dashboard{
		Clients:      s.hydrateClientPage(pageClients),
		Users:        []core.User{user},
		TotalUsers:   1,
		TotalClients: totalClients,
	}
	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":      "page_title_clients",
		"ActiveNav":     "clients",
		"Locale":        locale,
		"State":         state,
		"ClientPage":    pageInfo,
		"ClientTotal":   totalClients,
		"ClientCreated": r.URL.Query().Get("created") == "1",
		"ClaudeBaseURL": s.claudeBaseURL(r),
		"CodexBaseURL":  s.codexBaseURL(r),
	}, r)
	s.render(w, "clients.html", locale, data)
}

func (s *Server) hydrateClientPage(clients []core.APIClient) []core.APIClient {
	out := make([]core.APIClient, 0, len(clients))
	for _, client := range clients {
		fullClient, err := s.control.GetClient(client.ID)
		if err == nil {
			client = fullClient
		}
		out = append(out, client)
	}
	return out
}

func clientPageOffset(page, pageSize int) int {
	if pageSize <= 0 {
		pageSize = clientPageSize
	}
	if page < 1 {
		page = 1
	}
	return (page - 1) * pageSize
}

func clientPageInfoForTotal(page, pageSize, total int) clientPageInfo {
	if pageSize <= 0 {
		pageSize = clientPageSize
	}
	if page < 1 {
		page = 1
	}
	lastPage := 1
	if total > 0 {
		lastPage = (total + pageSize - 1) / pageSize
	}
	if page > lastPage {
		page = lastPage
	}
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	info := clientPageInfo{
		Page:      page,
		PageSize:  pageSize,
		Total:     total,
		HasPrev:   page > 1,
		PrevPage:  page - 1,
		HasNext:   end < total,
		NextPage:  page + 1,
		FirstItem: start + 1,
		LastItem:  end,
	}
	if total == 0 {
		info.FirstItem = 0
	}
	return info
}

func (s *Server) gatewayBaseURL(r *http.Request) string {
	baseURL := strings.TrimRight(strings.TrimSpace(s.control.PublicBaseURL()), "/")
	if baseURL == "" {
		baseURL = strings.TrimRight(requestBaseURL(r), "/")
	}
	return baseURL
}

func (s *Server) claudeBaseURL(r *http.Request) string {
	return s.gatewayBaseURL(r)
}

func (s *Server) codexBaseURL(r *http.Request) string {
	return s.gatewayBaseURL(r) + "/v1"
}

func (s *Server) handleClientActions(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/clients/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 1 && parts[0] == "new" {
		switch r.Method {
		case http.MethodGet:
			s.handleClientCreatePage(w, r)
		case http.MethodPost:
			s.handleClientCreateSubmit(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}

	switch parts[1] {
	case "toggle":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		user, _ := currentUserFromContext(r.Context())
		before, beforeErr := s.control.GetClient(parts[0])
		if err := s.control.ToggleClientForUser(user, parts[0]); err != nil {
			s.recordAdminAudit(r, "error", "client.toggle", "client", parts[0], "", err.Error())
			if isAccessError(err) {
				s.redirectWithNoticeError(w, r, "/clients", err)
				return
			}
			s.redirectWithNoticeError(w, r, "/clients", err)
			return
		}
		if client, err := s.control.GetClient(parts[0]); err == nil {
			message := auditFieldsMessage(auditMessageField{Key: "enabled", Value: fmt.Sprintf("%t", client.Enabled)})
			if beforeErr == nil {
				message = auditChangeMessage(auditBoolChange("enabled", before.Enabled, client.Enabled))
			}
			s.recordAdminAudit(r, "ok", "client.toggle", "client", client.ID, client.Name, message)
		}
		http.Redirect(w, r, "/clients", http.StatusSeeOther)
	case "delete":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		user, _ := currentUserFromContext(r.Context())
		client, err := s.control.DeleteClientForUser(user, parts[0])
		if err != nil {
			s.recordAdminAudit(r, "error", "client.delete", "client", parts[0], "", err.Error())
			if isAccessError(err) {
				s.redirectWithNoticeError(w, r, "/clients", err)
				return
			}
			s.redirectWithNoticeError(w, r, "/clients", err)
			return
		}
		s.recordAdminAudit(r, "ok", "client.delete", "client", client.ID, client.Name, fmt.Sprintf("enabled=%t", client.Enabled))
		http.Redirect(w, r, "/clients", http.StatusSeeOther)
	case "edit":
		switch r.Method {
		case http.MethodGet:
			s.handleClientEditPage(w, r, parts[0])
		case http.MethodPost:
			s.handleClientEditSubmit(w, r, parts[0])
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleClientCreatePage(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUserFromContext(r.Context())
	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":  "page_title_client_create",
		"ActiveNav": "clients",
		"Locale":    locale,
		"Editor":    s.control.NewClientEditorForUser(user),
		"Mode":      "create",
		"Providers": s.control.ProviderSummaries(r.Context()),
	}, r)
	s.render(w, "client_edit.html", locale, data)
}

func (s *Server) handleClientCreateSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderClientCreateFormError(w, r, err)
		return
	}

	spendLimit, err := parseNanoUSDFormValue(r, "spend_limit_usd")
	if err != nil {
		s.renderClientCreateFormError(w, r, err)
		return
	}
	user, _ := currentUserFromContext(r.Context())
	selection, err := s.clientBillingSelectionFromForm(r, s.control.NewClientEditorForUser(user).AvailableAccountGroups)
	if err != nil {
		s.renderClientCreateFormError(w, r, err)
		return
	}
	defaultProvider, fallback := s.routeProvidersForAccountGroup(selection.AccountGroup)
	client, err := s.control.CreateClientInBillingSourceForUser(
		user,
		r.FormValue("name"),
		spendLimit,
		r.FormValue("enabled") == "on",
		selection.AccountGroup,
		selection.BillingSource,
		defaultProvider,
		fallback,
	)
	if err != nil {
		s.recordAdminAudit(r, "error", "client.create", "client", "", strings.TrimSpace(r.FormValue("name")), err.Error())
		s.renderClientCreateFormError(w, r, err)
		return
	}
	s.recordAdminAudit(r, "ok", "client.create", "client", client.ID, client.Name, fmt.Sprintf("enabled=%t route=%s scope=%s", client.Enabled, auditRouteText(client.RoutePolicy), auditClientScopeText(client)))

	http.Redirect(w, r, "/clients?created=1", http.StatusSeeOther)
}

func (s *Server) handleClientEditPage(w http.ResponseWriter, r *http.Request, clientID string) {
	user, _ := currentUserFromContext(r.Context())
	editor, err := s.control.GetClientEditorForUser(clientID, user)
	if err != nil {
		s.redirectWithNoticeError(w, r, "/clients", err)
		return
	}
	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":  "page_title_client_edit",
		"ActiveNav": "clients",
		"Locale":    locale,
		"Editor":    editor,
		"Mode":      "edit",
		"Providers": s.control.ProviderSummaries(r.Context()),
	}, r)
	s.render(w, "client_edit.html", locale, data)
}

func (s *Server) handleClientEditSubmit(w http.ResponseWriter, r *http.Request, clientID string) {
	if err := r.ParseForm(); err != nil {
		s.renderClientEditFormError(w, r, clientID, err)
		return
	}

	user, _ := currentUserFromContext(r.Context())
	editor, err := s.control.GetClientEditorForUser(clientID, user)
	if err != nil {
		s.renderClientEditFormError(w, r, clientID, err)
		return
	}

	spendLimit, err := parseNanoUSDFormValue(r, "spend_limit_usd")
	if err != nil {
		s.renderClientEditFormError(w, r, clientID, err)
		return
	}
	selection, err := s.clientBillingSelectionFromForm(r, editor.AvailableAccountGroups)
	if err != nil {
		s.renderClientEditFormError(w, r, clientID, err)
		return
	}
	defaultProvider, fallback := s.routeProvidersForAccountGroup(selection.AccountGroup)
	if err := s.control.UpdateClientBillingSourceForUser(
		user,
		clientID,
		r.FormValue("name"),
		spendLimit,
		r.FormValue("enabled") == "on",
		selection.AccountGroup,
		selection.BillingSource,
		defaultProvider,
		fallback,
	); err != nil {
		s.recordAdminAudit(r, "error", "client.update", "client", clientID, strings.TrimSpace(r.FormValue("name")), err.Error())
		if isAccessError(err) {
			s.redirectWithNoticeError(w, r, "/clients", err)
			return
		}
		s.renderClientEditFormError(w, r, clientID, err)
		return
	}
	if client, err := s.control.GetClient(clientID); err == nil {
		s.recordAdminAudit(r, "ok", "client.update", "client", client.ID, client.Name, auditClientUpdateChangeMessage(editor.Client, client, true))
	}

	http.Redirect(w, r, "/clients", http.StatusSeeOther)
}

func (s *Server) renderClientCreateFormError(w http.ResponseWriter, r *http.Request, err error) {
	s.redirectWithNoticeError(w, r, "/clients/new", err)
}

func (s *Server) renderClientEditFormError(w http.ResponseWriter, r *http.Request, clientID string, err error) {
	if isAccessError(err) {
		s.redirectWithNoticeError(w, r, "/clients", err)
		return
	}
	s.redirectWithNoticeError(w, r, "/clients/"+url.PathEscape(clientID)+"/edit", err)
}
