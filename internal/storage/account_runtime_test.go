package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestUpsertRuntimeAccountPreservesConfigAndCredential(t *testing.T) {
	cases := []struct {
		name string
		repo func(t *testing.T) Repository
	}{
		{
			name: "memory",
			repo: func(t *testing.T) Repository {
				t.Helper()
				return NewMemoryRepository()
			},
		},
		{
			name: "sqlite",
			repo: func(t *testing.T) Repository {
				t.Helper()
				repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
				if err != nil {
					t.Fatalf("NewSQLiteRepository returned error: %v", err)
				}
				t.Cleanup(func() { _ = repo.Close() })
				return repo
			},
		},
		{
			name: "cached",
			repo: func(t *testing.T) Repository {
				t.Helper()
				return NewCachedRepository(NewMemoryRepository())
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := tc.repo(t)
			runtimeRepo, ok := repo.(interface {
				UpsertRuntimeAccount(core.Account) error
			})
			if !ok {
				t.Fatalf("%s repo does not support UpsertRuntimeAccount", tc.name)
			}

			original := core.Account{
				ID:              "acct_runtime",
				Provider:        core.ProviderOpenAI,
				Label:           "Original Label",
				Group:           "Default",
				ProxyURL:        "http://proxy.example.com",
				Status:          core.AccountStatusActive,
				ControlDisabled: true,
				Backup:          true,
				Credential: core.Credential{
					Mode:         "oauth",
					AccessToken:  "access-original",
					RefreshToken: "refresh-original",
					Metadata: map[string]string{
						"token_source": "oauth",
					},
				},
			}
			if err := repo.UpsertAccount(original); err != nil {
				t.Fatalf("UpsertAccount returned error: %v", err)
			}

			lastUsed := time.Unix(1700000000, 0).UTC()
			cooldown := lastUsed.Add(time.Minute)
			runtimeUpdate := original
			runtimeUpdate.Label = "Changed Label"
			runtimeUpdate.ProxyURL = "http://changed.example.com"
			runtimeUpdate.ControlDisabled = false
			runtimeUpdate.Backup = false
			runtimeUpdate.Status = core.AccountStatusCooling
			runtimeUpdate.LastUsedAt = &lastUsed
			runtimeUpdate.CooldownUntil = &cooldown
			runtimeUpdate.ConsecutiveFails = 2
			runtimeUpdate.TotalFails = 7
			runtimeUpdate.Credential.AccessToken = "access-changed"
			runtimeUpdate.Credential.RefreshToken = "refresh-changed"
			runtimeUpdate.Credential.Metadata = map[string]string{
				"token_source":               "changed",
				core.AccountQuotaMetadataKey: `{"Used":1}`,
			}
			if err := runtimeRepo.UpsertRuntimeAccount(runtimeUpdate); err != nil {
				t.Fatalf("UpsertRuntimeAccount returned error: %v", err)
			}

			stored, err := repo.GetAccount(original.ID)
			if err != nil {
				t.Fatalf("GetAccount returned error: %v", err)
			}
			if stored.Label != original.Label || stored.ProxyURL != original.ProxyURL {
				t.Fatalf("config changed after runtime update: %#v", stored)
			}
			if stored.ControlDisabled != original.ControlDisabled || stored.Backup != original.Backup {
				t.Fatalf("account control config changed after runtime update: %#v", stored)
			}
			if stored.Credential.AccessToken != original.Credential.AccessToken ||
				stored.Credential.RefreshToken != original.Credential.RefreshToken ||
				stored.Credential.Metadata["token_source"] != "oauth" {
				t.Fatalf("credential changed after runtime update: %#v", stored.Credential)
			}
			if stored.Status != core.AccountStatusCooling ||
				stored.LastUsedAt == nil || !stored.LastUsedAt.Equal(lastUsed) ||
				stored.CooldownUntil == nil || !stored.CooldownUntil.Equal(cooldown) ||
				stored.ConsecutiveFails != 2 ||
				stored.TotalFails != 7 {
				t.Fatalf("runtime state was not updated: %#v", stored)
			}
			if got := stored.Credential.Metadata[core.AccountQuotaMetadataKey]; got != `{"Used":1}` {
				t.Fatalf("runtime metadata = %q, want quota snapshot", got)
			}
		})
	}
}
