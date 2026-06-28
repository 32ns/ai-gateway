package storage

import (
	"path/filepath"
	"testing"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestRepositoriesNormalizeBlankAccountAndClientGroups(t *testing.T) {
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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := tc.repo(t)
			if err := repo.UpsertAccount(core.Account{
				ID:       "acct_blank_group",
				Provider: core.ProviderOpenAI,
				Label:    "Blank Group",
				Group:    " \t ",
				Status:   core.AccountStatusActive,
			}); err != nil {
				t.Fatalf("UpsertAccount returned error: %v", err)
			}
			account, err := repo.GetAccount("acct_blank_group")
			if err != nil {
				t.Fatalf("GetAccount returned error: %v", err)
			}
			if account.Group != core.DefaultAccountGroupName {
				t.Fatalf("account group = %q, want %q", account.Group, core.DefaultAccountGroupName)
			}

			if err := repo.UpsertClient(core.APIClient{
				ID:           "client_blank_group",
				Name:         "Blank Group Key",
				APIKey:       "gw_blank_group",
				Enabled:      true,
				AccountGroup: " \t ",
			}); err != nil {
				t.Fatalf("UpsertClient returned error: %v", err)
			}
			client, err := repo.GetClient("client_blank_group")
			if err != nil {
				t.Fatalf("GetClient returned error: %v", err)
			}
			if client.AccountGroup != core.DefaultAccountGroupName {
				t.Fatalf("client account group = %q, want %q", client.AccountGroup, core.DefaultAccountGroupName)
			}
		})
	}
}
