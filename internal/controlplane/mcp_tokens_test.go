package controlplane

import (
	"errors"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestMCPTokenLifecycle(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := service.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	rawToken, token, err := service.CreateMCPToken(admin, MCPTokenInput{
		Name:   "Docs Operator",
		Scopes: []string{core.MCPTokenScopeDocsPrivate, core.MCPTokenScopeDocsWrite},
	})
	if err != nil {
		t.Fatalf("CreateMCPToken returned error: %v", err)
	}
	if rawToken == "" || token.TokenHash == "" || token.TokenHash == rawToken {
		t.Fatalf("token material was not separated: raw=%q token=%#v", rawToken, token)
	}
	if !token.HasScope(core.MCPTokenScopeConnect) || !token.HasScope(core.MCPTokenScopeDocsWrite) {
		t.Fatalf("token scopes = %#v", token.Scopes)
	}
	auth, err := service.AuthorizeMCPToken(rawToken)
	if err != nil {
		t.Fatalf("AuthorizeMCPToken returned error: %v", err)
	}
	if auth.User.ID != admin.ID || !auth.Token.HasScope(core.MCPTokenScopeDocsPrivate) {
		t.Fatalf("authorization = %#v", auth)
	}
	stored, err := repo.GetMCPToken(token.ID)
	if err != nil {
		t.Fatalf("GetMCPToken returned error: %v", err)
	}
	if stored.LastUsedAt == nil {
		t.Fatalf("AuthorizeMCPToken should update LastUsedAt: %#v", stored)
	}
	updated, err := service.UpdateMCPToken(admin, token.ID, MCPTokenUpdateInput{
		Name:   "Docs Maintainer",
		Scopes: []string{core.MCPTokenScopeDocsRead},
	})
	if err != nil {
		t.Fatalf("UpdateMCPToken returned error: %v", err)
	}
	if updated.Name != "Docs Maintainer" || updated.HasScope(core.MCPTokenScopeDocsWrite) {
		t.Fatalf("updated token = %#v", updated)
	}
	if _, err := service.AuthorizeMCPToken(rawToken); err != nil {
		t.Fatalf("AuthorizeMCPToken after update returned error: %v", err)
	}
	deleted, err := service.DeleteMCPToken(admin, token.ID)
	if err != nil {
		t.Fatalf("DeleteMCPToken returned error: %v", err)
	}
	if deleted.ID != token.ID {
		t.Fatalf("deleted token = %#v", deleted)
	}
	if _, err := service.AuthorizeMCPToken(rawToken); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("AuthorizeMCPToken after delete error = %v, want ErrNotFound", err)
	}
}

func TestMCPTokenValidation(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := service.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user, err := service.CreateUser(UserInput{Username: "alice", Password: "alice-secret", Role: core.UserRoleUser, Enabled: true})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if _, _, err := service.CreateMCPToken(user, MCPTokenInput{Name: "Nope", Scopes: []string{core.MCPTokenScopeDocsRead}}); err == nil {
		t.Fatal("non-admin should not create MCP tokens")
	}
	if _, _, err := service.CreateMCPToken(admin, MCPTokenInput{Name: "Private", OwnerUserID: user.ID, Scopes: []string{core.MCPTokenScopeDocsPrivate}}); err == nil {
		t.Fatal("non-admin owner should not receive private document scope")
	}
	if _, _, err := service.CreateMCPToken(admin, MCPTokenInput{Name: "Mixed", Scopes: []string{core.MCPTokenScopeDocsRead, core.MCPTokenScopeDocsWrite}}); err == nil {
		t.Fatal("public read scope should not combine with document operation scopes")
	}
	expired := time.Now().UTC().Add(-time.Minute)
	rawToken, _, err := service.CreateMCPToken(admin, MCPTokenInput{Name: "Expired", Scopes: []string{core.MCPTokenScopeDocsRead}, ExpiresAt: &expired})
	if err != nil {
		t.Fatalf("CreateMCPToken expired returned error: %v", err)
	}
	if _, err := service.AuthorizeMCPToken(rawToken); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expired token authorization error = %v, want ErrNotFound", err)
	}
}
