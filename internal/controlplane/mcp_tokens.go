package controlplane

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"
)

type MCPTokenInput struct {
	Name        string
	OwnerUserID string
	Scopes      []string
	ExpiresAt   *time.Time
}

type MCPTokenUpdateInput struct {
	Name         string
	Scopes       []string
	ExpiresAt    *time.Time
	UpdateExpiry bool
}

type MCPAuthorization struct {
	Token core.MCPToken
	User  core.User
}

func (a MCPAuthorization) HasScope(scope string) bool {
	return a.Token.HasScope(scope)
}

func (s *Service) CreateMCPToken(actor core.User, input MCPTokenInput) (string, core.MCPToken, error) {
	if !actor.IsAdmin() {
		return "", core.MCPToken{}, fmt.Errorf("admin role required")
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return "", core.MCPToken{}, fmt.Errorf("mcp token name is required")
	}
	ownerID := strings.TrimSpace(input.OwnerUserID)
	if ownerID == "" {
		ownerID = actor.ID
	}
	owner, err := s.repo.GetUser(ownerID)
	if err != nil {
		return "", core.MCPToken{}, err
	}
	if !owner.Enabled {
		return "", core.MCPToken{}, fmt.Errorf("mcp token owner is disabled")
	}
	scopes, err := normalizeMCPTokenScopes(input.Scopes)
	if err != nil {
		return "", core.MCPToken{}, err
	}
	if mcpScopesRequireAdmin(scopes) && !owner.IsAdmin() {
		return "", core.MCPToken{}, fmt.Errorf("mcp token owner must be an admin for requested scopes")
	}
	now := time.Now().UTC()
	for range 3 {
		rawToken, err := randomHex(32)
		if err != nil {
			return "", core.MCPToken{}, err
		}
		rawToken = "mcp_" + rawToken
		id, err := randomHex(8)
		if err != nil {
			return "", core.MCPToken{}, err
		}
		token := core.MCPToken{
			ID:          "mcp_" + id,
			Name:        name,
			TokenHash:   sessionTokenHash(rawToken),
			OwnerUserID: owner.ID,
			Scopes:      scopes,
			Enabled:     true,
			ExpiresAt:   cloneTimePtr(input.ExpiresAt),
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := s.repo.UpsertMCPToken(token); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "already") || strings.Contains(strings.ToLower(err.Error()), "unique") {
				continue
			}
			return "", core.MCPToken{}, err
		}
		return rawToken, token, nil
	}
	return "", core.MCPToken{}, fmt.Errorf("mcp token id conflict")
}

func (s *Service) ListMCPTokens(actor core.User) []core.MCPToken {
	if !actor.IsAdmin() {
		return nil
	}
	return s.repo.ListMCPTokens("")
}

func (s *Service) UpdateMCPToken(actor core.User, id string, input MCPTokenUpdateInput) (core.MCPToken, error) {
	if !actor.IsAdmin() {
		return core.MCPToken{}, fmt.Errorf("admin role required")
	}
	token, err := s.repo.GetMCPToken(strings.TrimSpace(id))
	if err != nil {
		return core.MCPToken{}, err
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return core.MCPToken{}, fmt.Errorf("mcp token name is required")
	}
	owner, err := s.repo.GetUser(token.OwnerUserID)
	if err != nil {
		return core.MCPToken{}, err
	}
	if !owner.Enabled {
		return core.MCPToken{}, fmt.Errorf("mcp token owner is disabled")
	}
	scopes, err := normalizeMCPTokenScopes(input.Scopes)
	if err != nil {
		return core.MCPToken{}, err
	}
	if mcpScopesRequireAdmin(scopes) && !owner.IsAdmin() {
		return core.MCPToken{}, fmt.Errorf("mcp token owner must be an admin for requested scopes")
	}
	token.Name = name
	token.Scopes = scopes
	if input.UpdateExpiry {
		token.ExpiresAt = cloneTimePtr(input.ExpiresAt)
	}
	if err := s.repo.UpsertMCPToken(token); err != nil {
		return core.MCPToken{}, err
	}
	return s.repo.GetMCPToken(token.ID)
}

func (s *Service) DeleteMCPToken(actor core.User, id string) (core.MCPToken, error) {
	if !actor.IsAdmin() {
		return core.MCPToken{}, fmt.Errorf("admin role required")
	}
	token, err := s.repo.GetMCPToken(strings.TrimSpace(id))
	if err != nil {
		return core.MCPToken{}, err
	}
	if err := s.repo.DeleteMCPToken(token.ID); err != nil {
		return core.MCPToken{}, err
	}
	return token, nil
}

func (s *Service) RevokeMCPToken(actor core.User, id string) (core.MCPToken, error) {
	if !actor.IsAdmin() {
		return core.MCPToken{}, fmt.Errorf("admin role required")
	}
	token, err := s.repo.GetMCPToken(strings.TrimSpace(id))
	if err != nil {
		return core.MCPToken{}, err
	}
	now := time.Now().UTC()
	token.Enabled = false
	token.RevokedAt = &now
	if err := s.repo.UpsertMCPToken(token); err != nil {
		return core.MCPToken{}, err
	}
	return s.repo.GetMCPToken(token.ID)
}

func (s *Service) AuthorizeMCPToken(rawToken string) (MCPAuthorization, error) {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return MCPAuthorization{}, storage.ErrNotFound
	}
	token, err := s.repo.GetMCPTokenByHash(sessionTokenHash(rawToken))
	if err != nil {
		return MCPAuthorization{}, err
	}
	now := time.Now().UTC()
	if !token.Enabled || token.RevokedAt != nil {
		return MCPAuthorization{}, storage.ErrNotFound
	}
	if token.ExpiresAt != nil && !token.ExpiresAt.After(now) {
		return MCPAuthorization{}, storage.ErrNotFound
	}
	if !token.HasScope(core.MCPTokenScopeConnect) {
		return MCPAuthorization{}, fmt.Errorf("mcp token is missing connect scope")
	}
	user, err := s.repo.GetUser(token.OwnerUserID)
	if err != nil {
		return MCPAuthorization{}, err
	}
	if !user.Enabled {
		return MCPAuthorization{}, storage.ErrNotFound
	}
	token.LastUsedAt = &now
	if err := s.repo.UpsertMCPToken(token); err != nil {
		return MCPAuthorization{}, err
	}
	return MCPAuthorization{Token: token, User: user}, nil
}

func (s *Service) AppendMCPAudit(auth MCPAuthorization, action, resourceType, resourceID, resourceName, status, message, requestBody string) error {
	actor := strings.TrimSpace(auth.Token.Name)
	if actor == "" {
		actor = strings.TrimSpace(auth.Token.ID)
	}
	if actor != "" {
		actor = "mcp:" + actor
	}
	return s.repo.AppendAudit(core.AuditEvent{
		ID:           fmt.Sprintf("audit_mcp_%d", time.Now().UnixNano()),
		Kind:         core.AuditKindAdmin,
		Actor:        actor,
		Action:       strings.TrimSpace(action),
		ResourceType: strings.TrimSpace(resourceType),
		ResourceID:   strings.TrimSpace(resourceID),
		ResourceName: strings.TrimSpace(resourceName),
		ClientID:     strings.TrimSpace(auth.Token.ID),
		ClientName:   strings.TrimSpace(auth.Token.Name),
		Status:       strings.TrimSpace(status),
		Message:      strings.TrimSpace(message),
		RequestBody:  strings.TrimSpace(requestBody),
		CreatedAt:    time.Now().UTC(),
	})
}

func RequireMCPScope(auth MCPAuthorization, scope string) error {
	if !auth.HasScope(scope) {
		return fmt.Errorf("mcp token missing required scope: %s", scope)
	}
	return nil
}

func RequireMCPAdminScope(auth MCPAuthorization, scope string) error {
	if err := RequireMCPScope(auth, scope); err != nil {
		return err
	}
	if !auth.User.IsAdmin() {
		return fmt.Errorf("admin role required")
	}
	return nil
}

func normalizeMCPTokenScopes(scopes []string) ([]string, error) {
	seen := map[string]struct{}{core.MCPTokenScopeConnect: {}}
	out := []string{core.MCPTokenScopeConnect}
	for _, scope := range scopes {
		scope = strings.ToLower(strings.TrimSpace(scope))
		if scope == "" {
			continue
		}
		if !validMCPTokenScope(scope) {
			return nil, fmt.Errorf("unsupported mcp token scope: %s", scope)
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		out = append(out, scope)
	}
	if _, ok := seen[core.MCPTokenScopeDocsRead]; ok {
		for _, scope := range out {
			if scope != core.MCPTokenScopeConnect && scope != core.MCPTokenScopeDocsRead {
				return nil, fmt.Errorf("public document read scope cannot be combined with document operation scopes")
			}
		}
	}
	slices.Sort(out[1:])
	return out, nil
}

func validMCPTokenScope(scope string) bool {
	switch scope {
	case core.MCPTokenScopeConnect,
		core.MCPTokenScopeDocsRead,
		core.MCPTokenScopeDocsPrivate,
		core.MCPTokenScopeDocsWrite,
		core.MCPTokenScopeDocsPublish,
		core.MCPTokenScopeDocsArchive,
		core.MCPTokenScopeDocsPin:
		return true
	default:
		return false
	}
}

func mcpScopesRequireAdmin(scopes []string) bool {
	for _, scope := range scopes {
		switch scope {
		case core.MCPTokenScopeDocsPrivate,
			core.MCPTokenScopeDocsWrite,
			core.MCPTokenScopeDocsPublish,
			core.MCPTokenScopeDocsArchive,
			core.MCPTokenScopeDocsPin:
			return true
		}
	}
	return false
}
