package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestMemoryRepositoryMCPTokens(t *testing.T) {
	repo := NewMemoryRepository()
	testRepositoryMCPTokens(t, repo)
}

func TestSQLiteRepositoryMCPTokens(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	testRepositoryMCPTokens(t, repo)
}

func testRepositoryMCPTokens(t *testing.T, repo Repository) {
	t.Helper()
	now := time.Now().UTC()
	user := core.User{ID: "user_mcp", Username: "mcp-admin", Role: core.UserRoleAdmin, Enabled: true, CreatedAt: now, UpdatedAt: now}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	expiresAt := now.Add(time.Hour)
	token := core.MCPToken{
		ID:          "mcp_token",
		Name:        "Docs Operator",
		TokenHash:   "hash_one",
		OwnerUserID: user.ID,
		Scopes:      []string{core.MCPTokenScopeConnect, core.MCPTokenScopeDocsRead},
		Enabled:     true,
		ExpiresAt:   &expiresAt,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := repo.UpsertMCPToken(token); err != nil {
		t.Fatalf("UpsertMCPToken returned error: %v", err)
	}
	byHash, err := repo.GetMCPTokenByHash("hash_one")
	if err != nil {
		t.Fatalf("GetMCPTokenByHash returned error: %v", err)
	}
	if byHash.ID != token.ID || !byHash.HasScope(core.MCPTokenScopeDocsRead) || byHash.ExpiresAt == nil {
		t.Fatalf("stored token = %#v", byHash)
	}
	byHash.Scopes[0] = "mutated"
	again, err := repo.GetMCPToken(token.ID)
	if err != nil {
		t.Fatalf("GetMCPToken returned error: %v", err)
	}
	if again.Scopes[0] == "mutated" {
		t.Fatalf("token scopes were not cloned: %#v", again)
	}
	lastUsed := now.Add(time.Minute)
	token.LastUsedAt = &lastUsed
	token.Enabled = false
	if err := repo.UpsertMCPToken(token); err != nil {
		t.Fatalf("UpsertMCPToken update returned error: %v", err)
	}
	tokens := repo.ListMCPTokens(user.ID)
	if len(tokens) != 1 || tokens[0].ID != token.ID || tokens[0].Enabled || tokens[0].LastUsedAt == nil {
		t.Fatalf("ListMCPTokens = %#v", tokens)
	}
	if err := repo.DeleteMCPToken(token.ID); err != nil {
		t.Fatalf("DeleteMCPToken returned error: %v", err)
	}
	if _, err := repo.GetMCPToken(token.ID); err == nil {
		t.Fatal("GetMCPToken after delete returned nil error")
	}
	if _, err := repo.GetMCPTokenByHash(token.TokenHash); err == nil {
		t.Fatal("GetMCPTokenByHash after delete returned nil error")
	}
}
