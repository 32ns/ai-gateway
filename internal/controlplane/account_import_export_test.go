package controlplane

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestAccountPoolExportAndImportRoundTrip(t *testing.T) {
	sourceRepo := storage.NewMemoryRepository()
	if err := sourceRepo.UpsertAccountGroup(core.AccountGroup{
		ID:                   "group_plus",
		Name:                 "Plus",
		ProxyURL:             "http://127.0.0.1:7890",
		BillingMultiplierBps: 15000,
	}); err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	if err := sourceRepo.UpsertAccount(core.Account{
		ID:              "acct_export",
		Provider:        core.ProviderOpenAI,
		Label:           "Exported",
		Group:           "Plus",
		ProxyURL:        "socks5://127.0.0.1:1080",
		Status:          core.AccountStatusActive,
		Priority:        80,
		Weight:          60,
		ControlDisabled: true,
		Credential: core.Credential{
			Mode:         "manual-token",
			AccessToken:  "secret-access",
			RefreshToken: "secret-refresh",
			ExpiresAt:    &expiresAt,
			Metadata: map[string]string{
				"base_url":             "https://upstream.example.com",
				"account_login_method": "api_key",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	source := New(sourceRepo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	exported := source.ExportAccountPool(AccountPoolExportInput{AllGroups: true})
	payload, err := json.Marshal(exported)
	if err != nil {
		t.Fatalf("Marshal export: %v", err)
	}

	targetRepo := storage.NewMemoryRepository()
	target := New(targetRepo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	result, err := target.ImportAccountPoolPayload(payload, AccountPoolImportOptions{})
	if err != nil {
		t.Fatalf("ImportAccountPoolPayload returned error: %v", err)
	}
	if result.Imported != 1 || result.Failed != 0 || result.Skipped != 0 {
		t.Fatalf("result = %#v", result)
	}

	account, err := targetRepo.GetAccount("acct_export")
	if err != nil {
		t.Fatalf("GetAccount returned error: %v", err)
	}
	if account.Credential.AccessToken != "secret-access" || account.Credential.RefreshToken != "secret-refresh" {
		t.Fatalf("credentials not preserved: %#v", account.Credential)
	}
	if account.Group != "Plus" || account.ProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("account routing fields = %#v", account)
	}
	if !account.ControlDisabled || account.Priority != 80 || account.Weight != 60 {
		t.Fatalf("account control fields = %#v", account)
	}
	if account.Credential.Metadata["base_url"] != "https://upstream.example.com" {
		t.Fatalf("metadata = %#v", account.Credential.Metadata)
	}
	group, ok := testAccountGroupByName(targetRepo.ListAccountGroups(), "Plus")
	if !ok {
		t.Fatalf("imported group Plus not found")
	}
	if group.ProxyURL != "http://127.0.0.1:7890" || group.BillingMultiplierBps != 15000 {
		t.Fatalf("group = %#v", group)
	}
}

func TestAccountPoolExportSelectedAccounts(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_a",
		Provider: core.ProviderOpenAI,
		Label:    "A",
		Group:    "Plus",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "token-a",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_b",
		Provider: core.ProviderOpenAI,
		Label:    "B",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "token-b",
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	exported := service.ExportAccountPool(AccountPoolExportInput{AccountIDs: []string{"acct_a"}})
	if len(exported.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(exported.Accounts))
	}
	if exported.Accounts[0].ID != "acct_a" {
		t.Fatalf("exported account = %q, want acct_a", exported.Accounts[0].ID)
	}
	if len(exported.AccountGroups) != 1 {
		t.Fatalf("exported groups = %#v, want Plus only", exported.AccountGroups)
	}
	if exported.AccountGroups[0].Name != "Plus" {
		t.Fatalf("exported groups = %#v, want Plus", exported.AccountGroups)
	}
}

func TestImportAccountPoolSkipsExistingAndCanOverrideGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_existing",
		Provider: core.ProviderOpenAI,
		Label:    "Existing",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "existing-token",
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	payload, err := json.Marshal([]core.Account{
		{
			ID:       "acct_existing",
			Provider: core.ProviderOpenAI,
			Label:    "Duplicate",
			Status:   core.AccountStatusActive,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "duplicate-token",
			},
		},
		{
			ID:       "acct_new",
			Provider: core.ProviderOpenAI,
			Label:    "New",
			Group:    "Original",
			Status:   core.AccountStatusActive,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "new-token",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	overrideGroup := "Imported"
	result, err := service.ImportAccountPoolPayload(payload, AccountPoolImportOptions{GroupOverride: &overrideGroup})
	if err != nil {
		t.Fatalf("ImportAccountPoolPayload returned error: %v", err)
	}
	if result.Imported != 1 || result.Skipped != 1 || result.Failed != 0 {
		t.Fatalf("result = %#v", result)
	}
	existing, err := repo.GetAccount("acct_existing")
	if err != nil {
		t.Fatal(err)
	}
	if existing.Credential.AccessToken != "existing-token" {
		t.Fatalf("existing account was overwritten: %#v", existing)
	}
	imported, err := repo.GetAccount("acct_new")
	if err != nil {
		t.Fatal(err)
	}
	if imported.Group != "Imported" {
		t.Fatalf("group = %q, want Imported", imported.Group)
	}
	if _, ok := testAccountGroupByName(repo.ListAccountGroups(), "Imported"); !ok {
		t.Fatalf("import group was not created")
	}
}

func TestImportAccountPoolSkipsFileGroupsWhenOverridingGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	payload, err := json.Marshal(AccountPoolExport{
		Version: AccountPoolExportVersion,
		AccountGroups: []core.AccountGroup{{
			ID:                   "group_source",
			Name:                 "Source",
			ProxyURL:             "http://127.0.0.1:7890",
			BillingMultiplierBps: 14000,
		}},
		Accounts: []core.Account{{
			ID:       "acct_group_override",
			Provider: core.ProviderOpenAI,
			Label:    "Override",
			Group:    "Source",
			Status:   core.AccountStatusActive,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "override-token",
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	overrideGroup := "Imported"
	result, err := service.ImportAccountPoolPayload(payload, AccountPoolImportOptions{GroupOverride: &overrideGroup})
	if err != nil {
		t.Fatalf("ImportAccountPoolPayload returned error: %v", err)
	}
	if result.GroupsImported != 0 {
		t.Fatalf("GroupsImported = %d, want 0", result.GroupsImported)
	}
	if _, ok := testAccountGroupByName(repo.ListAccountGroups(), "Source"); ok {
		t.Fatalf("file group Source should not have been imported")
	}
	if _, ok := testAccountGroupByName(repo.ListAccountGroups(), "Imported"); !ok {
		t.Fatalf("override group Imported was not created")
	}
	account, err := repo.GetAccount("acct_group_override")
	if err != nil {
		t.Fatalf("GetAccount returned error: %v", err)
	}
	if account.Group != "Imported" {
		t.Fatalf("account group = %q, want Imported", account.Group)
	}
}

func testAccountGroupByName(groups []core.AccountGroup, name string) (core.AccountGroup, bool) {
	for _, group := range groups {
		if group.Name == name {
			return group, true
		}
	}
	return core.AccountGroup{}, false
}
