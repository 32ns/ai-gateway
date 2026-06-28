package web

import (
	"testing"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestUserIdentityHelperAvoidsFullUserListFallback(t *testing.T) {
	base := storage.NewMemoryRepository()
	if err := base.UpsertUser(core.User{ID: "user_template_identity", Username: "template-identity", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	repo := &templateIdentityFullListPanicRepository{MemoryRepository: base}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")

	userIdentity, ok := renderFuncMap(server, "en", nil)["userIdentity"].(func(string) userIdentityView)
	if !ok {
		t.Fatal("userIdentity helper has unexpected type")
	}
	got := userIdentity("user_template_identity")
	if !got.Known || got.ID != "user_template_identity" || got.Username != "template-identity" {
		t.Fatalf("userIdentity existing user = %#v", got)
	}
	missing := userIdentity("missing_user")
	if missing.Known || missing.ID != "missing_user" {
		t.Fatalf("userIdentity missing user = %#v, want unknown view with ID", missing)
	}
}

type templateIdentityFullListPanicRepository struct {
	*storage.MemoryRepository
}

func (r *templateIdentityFullListPanicRepository) ListUsers() []core.User {
	panic("userIdentity helper should use GetUser instead of full ListUsers")
}
