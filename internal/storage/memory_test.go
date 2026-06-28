package storage

import (
	"strings"
	"testing"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestMemoryRepositoryUpsertUserRejectsDuplicateUsername(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_a", Username: "Lin1207", Enabled: true}); err != nil {
		t.Fatalf("UpsertUser user_a returned error: %v", err)
	}
	if err := repo.UpsertUser(core.User{ID: "user_b", Username: " lin1207 ", Enabled: true}); err == nil {
		t.Fatalf("UpsertUser duplicate username returned nil")
	} else if !strings.Contains(err.Error(), "username already exists") {
		t.Fatalf("UpsertUser duplicate username error = %v", err)
	}
	if err := repo.UpsertUser(core.User{ID: "user_a", Username: "lin1207", Enabled: false}); err != nil {
		t.Fatalf("UpsertUser same user rename case returned error: %v", err)
	}
}
