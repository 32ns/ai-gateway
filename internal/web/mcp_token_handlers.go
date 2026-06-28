package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
)

type mcpTokenScopeOption struct {
	Value   string
	Label   string
	Hint    string
	Checked bool
}

func (s *Server) handleMCPTokensPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/mcp-tokens" {
		http.NotFound(w, r)
		return
	}
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.renderMCPTokens(w, r, user, "", "")
	case http.MethodPost:
		s.handleMCPTokenCreate(w, r, user)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleMCPTokenActions(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/mcp-tokens/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch parts[1] {
	case "update":
		s.handleMCPTokenUpdate(w, r, user, parts[0])
	case "delete":
		s.handleMCPTokenDelete(w, r, user, parts[0])
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleMCPTokenCreate(w http.ResponseWriter, r *http.Request, user core.User) {
	if err := r.ParseForm(); err != nil {
		s.renderMCPTokens(w, r, user, "", err.Error())
		return
	}
	expiresAt, err := mcpTokenExpiresAtFromForm(r.FormValue("expires_days"))
	if err != nil {
		s.recordAdminAudit(r, "error", "mcp_token.create", "mcp_token", "", strings.TrimSpace(r.FormValue("name")), err.Error())
		s.renderMCPTokens(w, r, user, "", err.Error())
		return
	}
	rawToken, token, err := s.control.CreateMCPToken(user, controlplane.MCPTokenInput{
		Name:      r.FormValue("name"),
		Scopes:    r.Form["scopes"],
		ExpiresAt: expiresAt,
	})
	if err != nil {
		s.recordAdminAudit(r, "error", "mcp_token.create", "mcp_token", "", strings.TrimSpace(r.FormValue("name")), err.Error())
		s.renderMCPTokens(w, r, user, "", err.Error())
		return
	}
	s.recordAdminAudit(r, "ok", "mcp_token.create", "mcp_token", token.ID, token.Name, fmt.Sprintf("scopes=%s", strings.Join(token.Scopes, ",")))
	s.renderMCPTokens(w, r, user, rawToken, "")
}

func (s *Server) handleMCPTokenUpdate(w http.ResponseWriter, r *http.Request, user core.User, tokenID string) {
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, "/admin/mcp-tokens", err)
		return
	}
	expiresAt, updateExpiry, err := mcpTokenUpdateExpiresAtFromForm(r.FormValue("expires_days"))
	if err != nil {
		s.recordAdminAudit(r, "error", "mcp_token.update", "mcp_token", tokenID, strings.TrimSpace(r.FormValue("name")), err.Error())
		s.redirectWithNoticeError(w, r, "/admin/mcp-tokens", err)
		return
	}
	token, err := s.control.UpdateMCPToken(user, tokenID, controlplane.MCPTokenUpdateInput{
		Name:         r.FormValue("name"),
		Scopes:       r.Form["scopes"],
		ExpiresAt:    expiresAt,
		UpdateExpiry: updateExpiry,
	})
	if err != nil {
		s.recordAdminAudit(r, "error", "mcp_token.update", "mcp_token", tokenID, strings.TrimSpace(r.FormValue("name")), err.Error())
		s.redirectWithNoticeError(w, r, "/admin/mcp-tokens", err)
		return
	}
	s.recordAdminAudit(r, "ok", "mcp_token.update", "mcp_token", token.ID, token.Name, fmt.Sprintf("scopes=%s", strings.Join(token.Scopes, ",")))
	http.Redirect(w, r, "/admin/mcp-tokens", http.StatusSeeOther)
}

func (s *Server) handleMCPTokenDelete(w http.ResponseWriter, r *http.Request, user core.User, tokenID string) {
	token, err := s.control.DeleteMCPToken(user, tokenID)
	if err != nil {
		s.recordAdminAudit(r, "error", "mcp_token.delete", "mcp_token", tokenID, "", err.Error())
		s.redirectWithNoticeError(w, r, "/admin/mcp-tokens", err)
		return
	}
	s.recordAdminAudit(r, "ok", "mcp_token.delete", "mcp_token", token.ID, token.Name, "")
	http.Redirect(w, r, "/admin/mcp-tokens", http.StatusSeeOther)
}

func (s *Server) renderMCPTokens(w http.ResponseWriter, r *http.Request, user core.User, rawToken string, formError string) {
	locale := resolveLocale(w, r)
	newTokenMCPConfigJSON := ""
	if strings.TrimSpace(rawToken) != "" {
		newTokenMCPConfigJSON = s.mcpClientConfigJSON(r, rawToken)
	}
	data := withCSRFData(map[string]any{
		"TitleKey":              "page_title_mcp_tokens",
		"ActiveNav":             "settings",
		"Locale":                locale,
		"Tokens":                s.control.ListMCPTokens(user),
		"ScopeOptions":          mcpTokenScopeOptions(locale),
		"NewToken":              rawToken,
		"NewTokenMCPConfigJSON": newTokenMCPConfigJSON,
		"Error":                 formError,
	}, r)
	s.render(w, "mcp_tokens.html", locale, data)
}

func (s *Server) mcpClientConfigJSON(r *http.Request, rawToken string) string {
	token := strings.TrimSpace(rawToken)
	if token == "" {
		return ""
	}
	baseURL := strings.TrimRight(s.publicDocsBaseURL(r), "/")
	config := map[string]any{
		"mcpServers": map[string]any{
			"ag-toos": map[string]any{
				"type": "http",
				"url":  baseURL + "/mcp",
				"headers": map[string]string{
					"Authorization": "Bearer " + token,
				},
			},
		},
	}
	body, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return ""
	}
	return string(body)
}

func mcpTokenExpiresAtFromForm(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	days, err := strconv.Atoi(value)
	if err != nil || days <= 0 || days > 3660 {
		return nil, fmt.Errorf("expiration days must be between 1 and 3660")
	}
	expiresAt := time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour)
	return &expiresAt, nil
}

func mcpTokenUpdateExpiresAtFromForm(value string) (*time.Time, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, false, nil
	}
	expiresAt, err := mcpTokenExpiresAtFromForm(value)
	return expiresAt, true, err
}

func mcpTokenScopeOptions(locale string) []mcpTokenScopeOption {
	return []mcpTokenScopeOption{
		{
			Value:   core.MCPTokenScopeDocsPrivate,
			Label:   translate(locale, "mcp_scope_docs_private"),
			Hint:    translate(locale, "mcp_scope_docs_private_hint"),
			Checked: true,
		},
		{
			Value:   core.MCPTokenScopeDocsWrite,
			Label:   translate(locale, "mcp_scope_docs_write"),
			Hint:    translate(locale, "mcp_scope_docs_write_hint"),
			Checked: true,
		},
		{
			Value:   core.MCPTokenScopeDocsPublish,
			Label:   translate(locale, "mcp_scope_docs_publish"),
			Hint:    translate(locale, "mcp_scope_docs_publish_hint"),
			Checked: true,
		},
		{
			Value:   core.MCPTokenScopeDocsArchive,
			Label:   translate(locale, "mcp_scope_docs_archive"),
			Hint:    translate(locale, "mcp_scope_docs_archive_hint"),
			Checked: true,
		},
		{
			Value:   core.MCPTokenScopeDocsPin,
			Label:   translate(locale, "mcp_scope_docs_pin"),
			Hint:    translate(locale, "mcp_scope_docs_pin_hint"),
			Checked: true,
		},
		{
			Value: core.MCPTokenScopeDocsRead,
			Label: translate(locale, "mcp_scope_docs_read"),
			Hint:  translate(locale, "mcp_scope_docs_read_hint"),
		},
	}
}
