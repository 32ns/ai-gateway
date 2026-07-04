package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

type getUserCountingRepository struct {
	*storage.MemoryRepository
	getUserCalls atomic.Uint64
}

func (r *getUserCountingRepository) GetUser(id string) (core.User, error) {
	r.getUserCalls.Add(1)
	return r.MemoryRepository.GetUser(id)
}

type negativeBalanceLoginRepository struct {
	*storage.MemoryRepository
	upserts atomic.Uint64
	updates atomic.Uint64
	touches atomic.Uint64
}

func (r *negativeBalanceLoginRepository) UpsertUser(user core.User) error {
	r.upserts.Add(1)
	return r.MemoryRepository.UpsertUser(user)
}

func (r *negativeBalanceLoginRepository) UpdateUserMetadata(user core.User) error {
	r.updates.Add(1)
	return r.MemoryRepository.UpdateUserMetadata(user)
}

func (r *negativeBalanceLoginRepository) TouchUserLastUsedAt(userID string, usedAt time.Time) error {
	r.touches.Add(1)
	return r.MemoryRepository.TouchUserLastUsedAt(userID, usedAt)
}

func (r *negativeBalanceLoginRepository) upsertCalls() uint64 {
	return r.upserts.Load()
}

func (r *negativeBalanceLoginRepository) updateCalls() uint64 {
	return r.updates.Load()
}

func (r *negativeBalanceLoginRepository) touchCalls() uint64 {
	return r.touches.Load()
}

func makeUserBalanceNegative(t *testing.T, repo storage.Repository, userID string) {
	t.Helper()
	clientID := userID + "_client"
	if err := repo.UpsertClient(core.APIClient{ID: clientID, Name: clientID, APIKey: "gw_" + clientID, OwnerUserID: userID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       userID + "_request",
		ClientID:        clientID,
		UserID:          userID,
		ReservedNanoUSD: 500,
		Fingerprint:     userID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     userID + "_request",
		ClientID:      clientID,
		ActualNanoUSD: 1500,
	}); err != nil {
		t.Fatal(err)
	}
}

func hashPasswordOrFail(t *testing.T, password string) string {
	t.Helper()
	hash, err := hashPassword(password)
	if err != nil {
		t.Fatalf("hashPassword returned error: %v", err)
	}
	return hash
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return data
}

func TestUpdateAccountPreservesTokenWhenBlank(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_blue", Name: "Blue"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_green", Name: "Green"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_edit",
		Provider: core.ProviderOpenAI,
		Label:    "Original",
		Remark:   "old note",
		Group:    "Blue",
		ProxyURL: "http://127.0.0.1:7890",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   50,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "secret-token",
			Metadata: map[string]string{
				"base_url": "https://old.example.com",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	expiresAt := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	if err := service.UpdateAccount("acct_edit", "Updated", "new note", "Green", "socks5://127.0.0.1:1080", "", "", "", "https://new.example.com", &expiresAt, 90, 75, core.AccountStatusBlocked, false, true); err != nil {
		t.Fatalf("update account: %v", err)
	}

	account, err := repo.GetAccount("acct_edit")
	if err != nil {
		t.Fatal(err)
	}
	if account.Label != "Updated" {
		t.Fatalf("label = %q, want %q", account.Label, "Updated")
	}
	if account.Remark != "new note" {
		t.Fatalf("remark = %q, want %q", account.Remark, "new note")
	}
	if account.Group != "Green" {
		t.Fatalf("group = %q, want %q", account.Group, "Green")
	}
	if account.ProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("proxy_url = %q", account.ProxyURL)
	}
	if account.Credential.AccessToken != "secret-token" {
		t.Fatalf("token = %q, want %q", account.Credential.AccessToken, "secret-token")
	}
	if account.Credential.Metadata["base_url"] != "https://new.example.com" {
		t.Fatalf("base_url = %q, want %q", account.Credential.Metadata["base_url"], "https://new.example.com")
	}
	if account.Credential.ExpiresAt == nil || !account.Credential.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("expires_at = %#v, want %v", account.Credential.ExpiresAt, expiresAt)
	}
	if account.Status != core.AccountStatusBlocked {
		t.Fatalf("status = %q, want %q", account.Status, core.AccountStatusBlocked)
	}
	if !account.Backup {
		t.Fatal("backup = false, want true")
	}
}

func TestCompleteManualConnectStoresBackupFlag(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	if err := service.CompleteManualConnect(ManualConnectInput{
		Provider:    core.ProviderOpenAI,
		Label:       "Backup Account",
		Group:       core.DefaultAccountGroupName,
		AccessToken: "token",
		Backup:      true,
		Priority:    100,
		Weight:      100,
	}); err != nil {
		t.Fatalf("complete manual connect: %v", err)
	}

	accounts := repo.ListAccounts()
	if len(accounts) != 1 {
		t.Fatalf("account count = %d, want 1", len(accounts))
	}
	if !accounts[0].Backup {
		t.Fatal("backup = false, want true")
	}
}

func TestUsageLogPageScopesNonAdminToOwnRequests(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_a", Username: "a", Role: core.UserRoleUser, Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertUser(core.User{ID: "user_b", Username: "b", Role: core.UserRoleUser, Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_a", Name: "A", APIKey: "gw_a", OwnerUserID: "user_a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_b", Name: "B", APIKey: "gw_b", OwnerUserID: "user_b", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	seedUsageLogBillingRequest(t, repo, "req_a", "client_a", "user_a")
	seedUsageLogBillingRequest(t, repo, "req_b", "client_b", "user_b")

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	page := service.UsageLogPage(context.Background(), core.User{ID: "user_a", Role: core.UserRoleUser}, UsageLogFilter{
		UserID:   "user_b",
		ClientID: "client_b",
		PageSize: 25,
	})
	if page.Total != 0 || len(page.Rows) != 0 {
		t.Fatalf("non-admin saw other user's rows: total=%d rows=%#v", page.Total, page.Rows)
	}

	page = service.UsageLogPage(context.Background(), core.User{ID: "admin", Role: core.UserRoleAdmin}, UsageLogFilter{
		UserID:   "user_b",
		PageSize: 25,
	})
	if page.Total != 1 || len(page.Rows) != 1 || page.Rows[0].Request.RequestID != "req_b" {
		t.Fatalf("admin filtered rows = total=%d rows=%#v", page.Total, page.Rows)
	}

	page = service.UsageLogPage(context.Background(), core.User{ID: "admin", Role: core.UserRoleAdmin}, UsageLogFilter{
		UserID:   "b",
		PageSize: 25,
	})
	if page.Total != 1 || len(page.Rows) != 1 || page.Rows[0].Request.RequestID != "req_b" {
		t.Fatalf("admin username filtered rows = total=%d rows=%#v", page.Total, page.Rows)
	}
}

func TestUsageLogPageFiltersByClientName(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_a", Username: "a", Role: core.UserRoleUser, Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_a", Name: "key", APIKey: "gw_a", OwnerUserID: "user_a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	seedUsageLogBillingRequest(t, repo, "req_a_1", "client_a", "user_a")
	time.Sleep(time.Millisecond)
	seedUsageLogBillingRequest(t, repo, "req_a_2", "client_a", "user_a")

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	page := service.UsageLogPage(context.Background(), core.User{ID: "admin", Role: core.UserRoleAdmin}, UsageLogFilter{
		ClientID: "key",
		PageSize: 25,
	})
	if page.Total != 2 || len(page.Rows) != 2 || page.Rows[0].Request.RequestID != "req_a_2" || page.Rows[1].Request.RequestID != "req_a_1" {
		t.Fatalf("admin client-name filtered rows = total=%d rows=%#v", page.Total, page.Rows)
	}
}

func TestUsageLogPageShowsAccountGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_group", Username: "group", Role: core.UserRoleUser, Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_group", Name: "Group Client", APIKey: "gw_group", OwnerUserID: "user_group", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_group", Provider: core.ProviderOpenAI, Label: "Group Account", Group: "Plus", Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_group",
		ClientID:        "client_group",
		ClientName:      "Group Client",
		UserID:          "user_group",
		AccountID:       "acct_group",
		Model:           "gpt-4.1",
		ReservedNanoUSD: 100,
		Fingerprint:     "req_group",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:                 "req_group",
		ClientID:                  "client_group",
		AccountID:                 "acct_group",
		AccountGroup:              "Plus",
		AccountGroupMultiplierBps: 25000,
		Model:                     "gpt-4.1",
		Usage:                     core.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		ActualNanoUSD:             120,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_group", Provider: core.ProviderOpenAI, Label: "Group Account", Group: "Changed", Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	page := service.UsageLogPage(context.Background(), core.User{ID: "user_group", Role: core.UserRoleUser}, UsageLogFilter{PageSize: 25})
	if page.Total != 1 || len(page.Rows) != 1 {
		t.Fatalf("rows = total=%d rows=%#v", page.Total, page.Rows)
	}
	if page.Rows[0].AccountGroup != "Plus" {
		t.Fatalf("account group = %q, want Plus", page.Rows[0].AccountGroup)
	}
	if page.Rows[0].ClientName != "Group Client" {
		t.Fatalf("client name = %q, want snapshot Group Client", page.Rows[0].ClientName)
	}
	if page.Rows[0].Request.AccountGroupMultiplierBps != 25000 {
		t.Fatalf("account group multiplier = %d, want 25000", page.Rows[0].Request.AccountGroupMultiplierBps)
	}
}

func TestUsageLogPageShowsAccountLabelForAdmin(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_account_log", Username: "account-log", Role: core.UserRoleUser, Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_account_log", Name: "Account Log Client", APIKey: "gw_account_log", OwnerUserID: "user_account_log", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_account_log", Provider: core.ProviderOpenAI, Label: "OpenAI user@example.com", Group: "Default", Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}
	seedUsageLogBillingRequestWithAccount(t, repo, "req_account_log", "client_account_log", "user_account_log", "acct_account_log")

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	page := service.UsageLogPage(context.Background(), core.User{ID: "admin", Role: core.UserRoleAdmin}, UsageLogFilter{PageSize: 25})
	if page.Total != 1 || len(page.Rows) != 1 {
		t.Fatalf("rows = total=%d rows=%#v", page.Total, page.Rows)
	}
	if page.Rows[0].AccountLabel != "OpenAI user@example.com" {
		t.Fatalf("account label = %q, want OpenAI user@example.com", page.Rows[0].AccountLabel)
	}
	if page.Rows[0].Request.AccountID != "acct_account_log" {
		t.Fatalf("account id = %q, want acct_account_log", page.Rows[0].Request.AccountID)
	}

	userPage := service.UsageLogPage(context.Background(), core.User{ID: "user_account_log", Role: core.UserRoleUser}, UsageLogFilter{PageSize: 25})
	if len(userPage.Rows) != 1 {
		t.Fatalf("user rows = %#v", userPage.Rows)
	}
	if userPage.Rows[0].AccountLabel != "" {
		t.Fatalf("regular user account label = %q, want empty", userPage.Rows[0].AccountLabel)
	}
}

func TestUsageLogPageKeepsDefaultAccountGroupSnapshot(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_default_group", Username: "default_group", Role: core.UserRoleUser, Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_default_group", Name: "Default Client", APIKey: "gw_default_group", OwnerUserID: "user_default_group", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_current_plus", Provider: core.ProviderOpenAI, Label: "Plus Account", Group: "Plus", Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_default_group",
		ClientID:        "client_default_group",
		ClientName:      "Default Client",
		UserID:          "user_default_group",
		AccountID:       "acct_current_plus",
		Model:           "gpt-4.1",
		ReservedNanoUSD: 100,
		Fingerprint:     "req_default_group",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:                 "req_default_group",
		ClientID:                  "client_default_group",
		AccountID:                 "acct_current_plus",
		AccountGroup:              core.DefaultAccountGroupName,
		AccountGroupMultiplierBps: core.AccountGroupDefaultMultiplierBps,
		Model:                     "gpt-4.1",
		Usage:                     core.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		ActualNanoUSD:             120,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	page := service.UsageLogPage(context.Background(), core.User{ID: "user_default_group", Role: core.UserRoleUser}, UsageLogFilter{PageSize: 25})
	if page.Total != 1 || len(page.Rows) != 1 {
		t.Fatalf("rows = total=%d rows=%#v", page.Total, page.Rows)
	}
	if page.Rows[0].AccountGroup != core.DefaultAccountGroupName {
		t.Fatalf("account group = %q, want default snapshot", page.Rows[0].AccountGroup)
	}
}

func TestUpdateUserPreservesBalance(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_edit", Username: "old", Role: core.UserRoleUser, Enabled: true, BalanceNanoUSD: 5000}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := service.UpdateUser("user_edit", UserInput{
		Username:       "new",
		Role:           core.UserRoleAdmin,
		Enabled:        true,
		BalanceNanoUSD: 1,
	})
	if err != nil {
		t.Fatalf("UpdateUser returned error: %v", err)
	}
	if user.Username != "new" || user.Role != core.UserRoleAdmin {
		t.Fatalf("user = %#v", user)
	}
	if user.BalanceNanoUSD != 5000 {
		t.Fatalf("balance = %d, want 5000", user.BalanceNanoUSD)
	}
}

func TestAuthenticateUserAllowsNegativeBalanceUser(t *testing.T) {
	repo := &negativeBalanceLoginRepository{MemoryRepository: storage.NewMemoryRepository()}
	if err := repo.UpsertUser(core.User{ID: "user_negative_login", Username: "negative-login", Role: core.UserRoleUser, Enabled: true, PasswordHash: hashPasswordOrFail(t, "secret"), BalanceNanoUSD: 1000}); err != nil {
		t.Fatal(err)
	}
	makeUserBalanceNegative(t, repo, "user_negative_login")
	upsertsBefore := repo.upsertCalls()

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := service.AuthenticateUser("negative-login", "secret")
	if err != nil {
		t.Fatalf("AuthenticateUser returned error: %v", err)
	}
	if user.BalanceNanoUSD != -500 {
		t.Fatalf("balance = %d, want -500", user.BalanceNanoUSD)
	}
	if repo.upsertCalls() != upsertsBefore {
		t.Fatalf("AuthenticateUser should not call UpsertUser, got %d new calls", repo.upsertCalls()-upsertsBefore)
	}
	if repo.touchCalls() != 1 {
		t.Fatalf("TouchUserLastUsedAt calls = %d, want 1", repo.touchCalls())
	}
	if user.LastLoginAt == nil {
		t.Fatal("LastLoginAt should be set")
	}
}

func TestUpdateUserKeepsNegativeBalance(t *testing.T) {
	repo := &negativeBalanceLoginRepository{MemoryRepository: storage.NewMemoryRepository()}
	if err := repo.UpsertUser(core.User{ID: "user_negative_update", Username: "negative-update", Role: core.UserRoleUser, Enabled: true, PasswordHash: hashPasswordOrFail(t, "secret"), BalanceNanoUSD: 1000}); err != nil {
		t.Fatal(err)
	}
	makeUserBalanceNegative(t, repo, "user_negative_update")
	updatesBefore := repo.updateCalls()

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	updated, err := service.UpdateUser("user_negative_update", UserInput{
		Username: "negative-update",
		Role:     core.UserRoleAdmin,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("UpdateUser returned error: %v", err)
	}
	if updated.BalanceNanoUSD != -500 {
		t.Fatalf("balance = %d, want -500", updated.BalanceNanoUSD)
	}
	if repo.updateCalls() != updatesBefore+1 {
		t.Fatalf("UpdateUser metadata updates = %d, want %d", repo.updateCalls(), updatesBefore+1)
	}
}

func TestChangeUserPasswordKeepsNegativeBalance(t *testing.T) {
	repo := &negativeBalanceLoginRepository{MemoryRepository: storage.NewMemoryRepository()}
	if err := repo.UpsertUser(core.User{ID: "user_negative_password", Username: "negative-password", Role: core.UserRoleUser, Enabled: true, PasswordHash: hashPasswordOrFail(t, "old-secret"), BalanceNanoUSD: 1000}); err != nil {
		t.Fatal(err)
	}
	makeUserBalanceNegative(t, repo, "user_negative_password")
	updatesBefore := repo.updateCalls()

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if err := service.ChangeUserPassword("user_negative_password", "old-secret", "new-secret"); err != nil {
		t.Fatalf("ChangeUserPassword returned error: %v", err)
	}
	if repo.updateCalls() != updatesBefore+1 {
		t.Fatalf("ChangeUserPassword metadata updates = %d, want %d", repo.updateCalls(), updatesBefore+1)
	}
	user, err := service.AuthenticateUser("negative-password", "new-secret")
	if err != nil {
		t.Fatalf("AuthenticateUser with new password returned error: %v", err)
	}
	if user.BalanceNanoUSD != -500 {
		t.Fatalf("balance = %d, want -500", user.BalanceNanoUSD)
	}
}

func TestOAuthLinkAndUnlinkKeepNegativeBalance(t *testing.T) {
	repo := &negativeBalanceLoginRepository{MemoryRepository: storage.NewMemoryRepository()}
	if err := repo.UpsertUser(core.User{ID: "user_negative_oauth", Username: "negative-oauth", Role: core.UserRoleUser, Enabled: true, PasswordHash: hashPasswordOrFail(t, "secret"), BalanceNanoUSD: 1000}); err != nil {
		t.Fatal(err)
	}
	makeUserBalanceNegative(t, repo, "user_negative_oauth")

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	linked, err := service.LinkOAuthIdentity("user_negative_oauth", OAuthUserInput{
		Provider: "github",
		Subject:  "negative-oauth-subject",
		Email:    "negative@example.com",
		Username: "negative-oauth",
	})
	if err != nil {
		t.Fatalf("LinkOAuthIdentity returned error: %v", err)
	}
	if linked.BalanceNanoUSD != -500 {
		t.Fatalf("linked balance = %d, want -500", linked.BalanceNanoUSD)
	}
	if _, ok := service.OAuthIdentityOwner("github", "negative-oauth-subject"); !ok {
		t.Fatal("linked oauth identity should resolve")
	}

	unlinked, err := service.UnlinkOAuthIdentity("user_negative_oauth", "github")
	if err != nil {
		t.Fatalf("UnlinkOAuthIdentity returned error: %v", err)
	}
	if unlinked.BalanceNanoUSD != -500 {
		t.Fatalf("unlinked balance = %d, want -500", unlinked.BalanceNanoUSD)
	}
	if _, ok := service.OAuthIdentityOwner("github", "negative-oauth-subject"); ok {
		t.Fatal("unlinked oauth identity should not resolve")
	}
}

func TestUpdateUserStoresConcurrentRequestLimitOverride(t *testing.T) {
	repo := storage.NewMemoryRepository()
	initialLimit := 2
	if err := repo.UpsertUser(core.User{
		ID:                             "user_concurrency",
		Username:                       "limited",
		Role:                           core.UserRoleUser,
		Enabled:                        true,
		ConcurrentRequestLimitOverride: &initialLimit,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	limit := 5
	user, err := service.UpdateUser("user_concurrency", UserInput{
		Username:                       "limited",
		Role:                           core.UserRoleUser,
		Enabled:                        true,
		ConcurrentRequestLimitOverride: &limit,
	})
	if err != nil {
		t.Fatalf("UpdateUser returned error: %v", err)
	}
	if user.ConcurrentRequestLimitOverride == nil || *user.ConcurrentRequestLimitOverride != 5 {
		t.Fatalf("ConcurrentRequestLimitOverride = %#v, want 5", user.ConcurrentRequestLimitOverride)
	}

	unlimited := 0
	user, err = service.UpdateUser("user_concurrency", UserInput{
		Username:                       "limited",
		Role:                           core.UserRoleUser,
		Enabled:                        true,
		ConcurrentRequestLimitOverride: &unlimited,
	})
	if err != nil {
		t.Fatalf("UpdateUser unlimited returned error: %v", err)
	}
	if user.ConcurrentRequestLimitOverride == nil || *user.ConcurrentRequestLimitOverride != 0 {
		t.Fatalf("ConcurrentRequestLimitOverride = %#v, want 0", user.ConcurrentRequestLimitOverride)
	}

	user, err = service.UpdateUser("user_concurrency", UserInput{
		Username: "limited",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("UpdateUser inherit returned error: %v", err)
	}
	if user.ConcurrentRequestLimitOverride != nil {
		t.Fatalf("ConcurrentRequestLimitOverride = %#v, want nil", user.ConcurrentRequestLimitOverride)
	}
}

func TestUpdateUserStoresRequestRateLimitOverride(t *testing.T) {
	repo := storage.NewMemoryRepository()
	initialLimit := 30
	if err := repo.UpsertUser(core.User{
		ID:                                "user_rate",
		Username:                          "rate",
		Role:                              core.UserRoleUser,
		Enabled:                           true,
		RequestRateLimitPerMinuteOverride: &initialLimit,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	limit := 60
	user, err := service.UpdateUser("user_rate", UserInput{
		Username:                          "rate",
		Role:                              core.UserRoleUser,
		Enabled:                           true,
		RequestRateLimitPerMinuteOverride: &limit,
	})
	if err != nil {
		t.Fatalf("UpdateUser returned error: %v", err)
	}
	if user.RequestRateLimitPerMinuteOverride == nil || *user.RequestRateLimitPerMinuteOverride != 60 {
		t.Fatalf("RequestRateLimitPerMinuteOverride = %#v, want 60", user.RequestRateLimitPerMinuteOverride)
	}

	unlimited := 0
	user, err = service.UpdateUser("user_rate", UserInput{
		Username:                          "rate",
		Role:                              core.UserRoleUser,
		Enabled:                           true,
		RequestRateLimitPerMinuteOverride: &unlimited,
	})
	if err != nil {
		t.Fatalf("UpdateUser unlimited returned error: %v", err)
	}
	if user.RequestRateLimitPerMinuteOverride == nil || *user.RequestRateLimitPerMinuteOverride != 0 {
		t.Fatalf("RequestRateLimitPerMinuteOverride = %#v, want 0", user.RequestRateLimitPerMinuteOverride)
	}

	user, err = service.UpdateUser("user_rate", UserInput{
		Username: "rate",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("UpdateUser inherit returned error: %v", err)
	}
	if user.RequestRateLimitPerMinuteOverride != nil {
		t.Fatalf("RequestRateLimitPerMinuteOverride = %#v, want nil", user.RequestRateLimitPerMinuteOverride)
	}
}

func TestAdjustUserBalanceCreditsAndDebits(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_balance", Username: "balance", Role: core.UserRoleUser, Enabled: true, BalanceNanoUSD: 5000}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, previous, err := service.AdjustUserBalance("user_balance", UserBalanceAdjustment{AmountNanoUSD: 2500, Reason: "top up"})
	if err != nil {
		t.Fatalf("credit returned error: %v", err)
	}
	if previous != 5000 || user.BalanceNanoUSD != 7500 {
		t.Fatalf("credit previous=%d user=%#v", previous, user)
	}
	user, previous, err = service.AdjustUserBalance("user_balance", UserBalanceAdjustment{AmountNanoUSD: -1500, Reason: "correction"})
	if err != nil {
		t.Fatalf("debit returned error: %v", err)
	}
	if previous != 7500 || user.BalanceNanoUSD != 6000 {
		t.Fatalf("debit previous=%d user=%#v", previous, user)
	}
	if _, _, err := service.AdjustUserBalance("user_balance", UserBalanceAdjustment{AmountNanoUSD: -7000}); err == nil {
		t.Fatal("expected negative balance adjustment to fail")
	}
	ledger := service.ListBillingLedger("user_balance", 10)
	if len(ledger) != 2 {
		t.Fatalf("ledger len = %d, want 2: %#v", len(ledger), ledger)
	}
	if ledger[0].Kind != "manual_debit" || ledger[0].AmountNanoUSD != -1500 || ledger[0].BalanceAfterNanoUSD != 6000 || ledger[0].Note != "correction" {
		t.Fatalf("debit ledger = %#v", ledger[0])
	}
	if ledger[1].Kind != "manual_credit" || ledger[1].AmountNanoUSD != 2500 || ledger[1].BalanceAfterNanoUSD != 7500 || ledger[1].Note != "top up" {
		t.Fatalf("credit ledger = %#v", ledger[1])
	}
}

func TestCreateInvitedUserCreditsInviteeReward(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	inviter, err := service.CreateUser(UserInput{
		Username: "alice",
		Password: "alice-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser(inviter) returned error: %v", err)
	}
	settings := core.DefaultSystemSettings()
	settings.Invitation.Enabled = true
	settings.Invitation.InviterRechargeRewardBps = 500
	settings.Invitation.InviteeRewardNanoUSD = 2 * core.NanoUSDPerUSD
	if _, err := service.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	code := service.InvitationCodeForUser(inviter)
	if code == "" {
		t.Fatal("expected invitation code")
	}

	result, err := service.CreateInvitedUser(UserInput{
		Username: "bob",
		Password: "bob-secret",
		Role:     core.UserRoleAdmin,
		Enabled:  true,
	}, code)
	if err != nil {
		t.Fatalf("CreateInvitedUser returned error: %v", err)
	}
	if result.Inviter == nil || result.Inviter.ID != inviter.ID {
		t.Fatalf("inviter = %#v, want %s", result.Inviter, inviter.ID)
	}
	if result.User.Role != core.UserRoleUser {
		t.Fatalf("invited role = %s, want user", result.User.Role)
	}
	if result.User.InviterUserID != inviter.ID {
		t.Fatalf("InviterUserID = %q, want %q", result.User.InviterUserID, inviter.ID)
	}
	if result.User.BalanceNanoUSD != 2*core.NanoUSDPerUSD {
		t.Fatalf("invitee balance = %d", result.User.BalanceNanoUSD)
	}
	storedInviter, err := service.GetUser(inviter.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedInviter.BalanceNanoUSD != 0 {
		t.Fatalf("inviter balance = %d, want no registration reward", storedInviter.BalanceNanoUSD)
	}
	if _, err := service.ResolveInvitationCode(code + "x"); err == nil {
		t.Fatal("expected tampered invitation code to fail")
	}
}

func TestCreateInvitedUserRequiresInviteWhenConfigured(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	inviter, err := service.CreateUser(UserInput{
		Username: "inviter",
		Password: "inviter-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser(inviter) returned error: %v", err)
	}
	settings := core.DefaultSystemSettings()
	settings.Invitation.Enabled = true
	settings.Registration.RequireInvitationCode = true
	if _, err := service.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}

	if _, err := service.CreateInvitedUser(UserInput{Username: "missing", Password: "missing-secret"}, ""); err == nil {
		t.Fatal("CreateInvitedUser without invite returned nil error")
	}
	code := service.InvitationCodeForUser(inviter)
	if _, err := service.CreateInvitedUser(UserInput{Username: "invited", Password: "invited-secret"}, code); err != nil {
		t.Fatalf("CreateInvitedUser with invite returned error: %v", err)
	}
}

func TestCreateInvitedUserValidatesUsernameBeforeMinimum(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	settings := core.DefaultSystemSettings()
	settings.Registration.UsernameMinLength = 6
	if _, err := service.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}

	_, err := service.CreateInvitedUser(UserInput{
		Username: "@",
		Password: "invalid-secret",
	}, "")
	if !errors.Is(err, ErrInvalidUsernameCharacters) {
		t.Fatalf("CreateInvitedUser error = %v, want ErrInvalidUsernameCharacters", err)
	}
}

func TestCreateInvitedUserCreditsNewUserReward(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	settings := core.DefaultSystemSettings()
	settings.Registration.NewUserRewardEnabled = true
	settings.Registration.NewUserRewardNanoUSD = 1500000000
	if _, err := service.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}

	result, err := service.CreateInvitedUser(UserInput{
		Username: "rewarded",
		Password: "rewarded-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	}, "")
	if err != nil {
		t.Fatalf("CreateInvitedUser returned error: %v", err)
	}
	if result.Inviter != nil {
		t.Fatalf("Inviter = %#v, want nil", result.Inviter)
	}
	if result.User.BalanceNanoUSD != 1500000000 {
		t.Fatalf("new user balance = %d", result.User.BalanceNanoUSD)
	}
}

func TestCreateUserStoresInviterUserID(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	user, err := service.CreateUser(UserInput{
		Username:      "invited-user",
		Password:      "invited-secret",
		Role:          core.UserRoleUser,
		Enabled:       true,
		InviterUserID: "user_inviter",
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if user.InviterUserID != "user_inviter" {
		t.Fatalf("InviterUserID = %q, want user_inviter", user.InviterUserID)
	}
}

func TestCreateUserStoresVerifiedEmail(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	user, err := service.CreateUser(UserInput{
		Username:      "email-user",
		Password:      "email-secret",
		Role:          core.UserRoleUser,
		Enabled:       true,
		Email:         "  Alice@Example.COM ",
		EmailVerified: true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if user.Email != "alice@example.com" || !user.EmailVerified {
		t.Fatalf("email fields = %q verified=%v", user.Email, user.EmailVerified)
	}
}

func TestCreateUserValidatesUsernameCharacters(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	if _, err := service.CreateUser(UserInput{
		Username: "User_name-1.2",
		Password: "valid-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	}); err != nil {
		t.Fatalf("CreateUser with allowed username returned error: %v", err)
	}

	for _, username := range []string{"bad@name", "bad name", "中文"} {
		if _, err := service.CreateUser(UserInput{
			Username: username,
			Password: "invalid-secret",
			Role:     core.UserRoleUser,
			Enabled:  true,
		}); !errors.Is(err, ErrInvalidUsernameCharacters) {
			t.Fatalf("CreateUser(%q) error = %v, want ErrInvalidUsernameCharacters", username, err)
		}
	}
}

func TestVerifyEmailCodeMarksCodeUsedAndRejectsReuse(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	now := time.Now().UTC()
	record := core.EmailVerificationCode{
		ID:          "email_code_test",
		Purpose:     EmailVerificationPurposeRegister,
		Email:       "alice@example.com",
		MaxAttempts: 3,
		ExpiresAt:   now.Add(time.Minute),
		CreatedAt:   now,
	}
	record.CodeHash = emailVerificationCodeHash(record.Purpose, record.Email, "123456", record.ID)
	if err := repo.CreateEmailVerificationCode(record); err != nil {
		t.Fatalf("CreateEmailVerificationCode returned error: %v", err)
	}

	if err := service.VerifyEmailCode(EmailVerificationPurposeRegister, "Alice@Example.COM", "123456"); err != nil {
		t.Fatalf("VerifyEmailCode returned error: %v", err)
	}
	stored, err := repo.LatestEmailVerificationCode(EmailVerificationPurposeRegister, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if stored.UsedAt == nil || stored.Attempts != 1 {
		t.Fatalf("stored code = %#v", stored)
	}
	if err := service.VerifyEmailCode(EmailVerificationPurposeRegister, "alice@example.com", "123456"); err == nil {
		t.Fatal("VerifyEmailCode reuse returned nil error")
	}
}

func TestAuthenticateOAuthUserAutoCreatesAndRewardsUser(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	settings := core.DefaultSystemSettings()
	settings.OAuth.LoginAutoCreateUser = true
	settings.Registration.NewUserRewardEnabled = true
	settings.Registration.NewUserRewardNanoUSD = 2500000000
	if _, err := service.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}

	user, created, err := service.AuthenticateOAuthUser(OAuthUserInput{
		Provider:                       "github",
		Subject:                        "12345",
		Email:                          "alice@example.com",
		EmailVerified:                  true,
		Username:                       "Alice",
		RegistrationIP:                 "203.0.113.55",
		RegistrationBrowserFingerprint: "oauth-fingerprint-alice",
	})
	if err != nil {
		t.Fatalf("AuthenticateOAuthUser returned error: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	if user.Username != "github_alice" || user.Role != core.UserRoleUser || !user.Enabled {
		t.Fatalf("user = %#v", user)
	}
	if user.Email != "alice@example.com" || !user.EmailVerified {
		t.Fatalf("email fields = %q verified=%v", user.Email, user.EmailVerified)
	}
	if user.RegistrationIP != "203.0.113.55" || user.RegistrationBrowserFingerprint != "oauth-fingerprint-alice" {
		t.Fatalf("registration metadata = ip %q fingerprint %q", user.RegistrationIP, user.RegistrationBrowserFingerprint)
	}
	if user.BalanceNanoUSD != 2500000000 {
		t.Fatalf("BalanceNanoUSD = %d", user.BalanceNanoUSD)
	}
	if len(user.OAuthIdentities) != 1 || user.OAuthIdentities[0].Provider != "github" || user.OAuthIdentities[0].Subject != "12345" {
		t.Fatalf("OAuthIdentities = %#v", user.OAuthIdentities)
	}

	again, created, err := service.AuthenticateOAuthUser(OAuthUserInput{Provider: "github", Subject: "12345"})
	if err != nil {
		t.Fatalf("AuthenticateOAuthUser again returned error: %v", err)
	}
	if created {
		t.Fatal("created again = true, want false")
	}
	if again.ID != user.ID {
		t.Fatalf("again user ID = %q, want %q", again.ID, user.ID)
	}
}

func TestAuthenticateOAuthUserAutoCreateBypassesRegistrationEmailAllowlist(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	settings := core.DefaultSystemSettings()
	settings.OAuth.LoginAutoCreateUser = true
	settings.Runtime.RegistrationEmailAllowlistEnabled = true
	settings.Runtime.RegistrationEmailAllowlist = []string{"@allowed.com"}
	if _, err := service.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}

	user, created, err := service.AuthenticateOAuthUser(OAuthUserInput{
		Provider:      "github",
		Subject:       "oauth-subject",
		Email:         "ThirdParty-ID",
		EmailVerified: true,
		Username:      "thirdparty",
	})
	if err != nil {
		t.Fatalf("AuthenticateOAuthUser returned error: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	if user.Email != "thirdparty-id" || !user.EmailVerified {
		t.Fatalf("email fields = %q verified=%v", user.Email, user.EmailVerified)
	}
	if len(user.OAuthIdentities) != 1 || user.OAuthIdentities[0].Email != "ThirdParty-ID" {
		t.Fatalf("OAuthIdentities = %#v", user.OAuthIdentities)
	}
}

func TestAuthenticateOAuthUserGeneratesAvailableUsername(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	settings := core.DefaultSystemSettings()
	settings.OAuth.LoginAutoCreateUser = true
	if _, err := service.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	for _, username := range []string{"github_alice", "github_alice_12345678"} {
		if _, err := service.CreateUser(UserInput{
			Username: username,
			Password: "existing-secret",
			Role:     core.UserRoleUser,
			Enabled:  true,
		}); err != nil {
			t.Fatalf("CreateUser(%s) returned error: %v", username, err)
		}
	}

	user, created, err := service.AuthenticateOAuthUser(OAuthUserInput{
		Provider: "github",
		Subject:  "1234567890",
		Username: "alice",
	})
	if err != nil {
		t.Fatalf("AuthenticateOAuthUser returned error: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	if user.Username != "github_alice_123456782" {
		t.Fatalf("Username = %q", user.Username)
	}
}

func TestAuthenticateOAuthUserGeneratesValidUsernameFromSubject(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	settings := core.DefaultSystemSettings()
	settings.OAuth.LoginAutoCreateUser = true
	if _, err := service.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}

	user, created, err := service.AuthenticateOAuthUser(OAuthUserInput{
		Provider: "github",
		Subject:  "bad@id",
	})
	if err != nil {
		t.Fatalf("AuthenticateOAuthUser returned error: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	if user.Username != "github_badid" {
		t.Fatalf("Username = %q, want github_badid", user.Username)
	}
}

func TestAuthenticateOAuthUserAutoCreateRespectsRegistrationSecurity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings func(*core.SystemSettings)
	}{
		{
			name: "require invite",
			settings: func(settings *core.SystemSettings) {
				settings.Invitation.Enabled = true
				settings.Registration.RequireInvitationCode = true
			},
		},
		{
			name: "turnstile",
			settings: func(settings *core.SystemSettings) {
				settings.Registration.TurnstileEnabled = true
				settings.Registration.TurnstileSiteKey = "site"
				settings.Registration.TurnstileSecretKey = "secret"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := storage.NewMemoryRepository()
			service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
			settings := core.DefaultSystemSettings()
			settings.OAuth.LoginAutoCreateUser = true
			tc.settings(&settings)
			if _, err := service.UpdateSystemSettings(settings); err != nil {
				t.Fatalf("UpdateSystemSettings returned error: %v", err)
			}

			_, created, err := service.AuthenticateOAuthUser(OAuthUserInput{
				Provider: "github",
				Subject:  "oauth-subject",
				Email:    "alice@example.com",
				Username: "alice",
			})
			if err == nil || !strings.Contains(err.Error(), "registration security") {
				t.Fatalf("AuthenticateOAuthUser err = %v, want registration security error", err)
			}
			if created {
				t.Fatal("created = true, want false")
			}
			if _, err := repo.FindUserByUsername("github_alice"); err == nil {
				t.Fatal("oauth auto-create created a user despite registration security settings")
			}
		})
	}
}

func TestLinkOAuthIdentityToExistingUser(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := service.CreateUser(UserInput{
		Username: "alice",
		Password: "secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}

	linked, err := service.LinkOAuthIdentity(user.ID, OAuthUserInput{
		Provider: "github",
		Subject:  "12345",
		Email:    "alice@example.com",
		Username: "alice-gh",
	})
	if err != nil {
		t.Fatalf("LinkOAuthIdentity returned error: %v", err)
	}
	if len(linked.OAuthIdentities) != 1 {
		t.Fatalf("OAuthIdentities = %#v", linked.OAuthIdentities)
	}
	identity := linked.OAuthIdentities[0]
	if identity.Provider != "github" || identity.Subject != "12345" || identity.Email != "alice@example.com" || identity.Username != "alice-gh" || identity.LinkedAt.IsZero() {
		t.Fatalf("identity = %#v", identity)
	}

	loggedIn, created, err := service.AuthenticateOAuthUser(OAuthUserInput{Provider: "github", Subject: "12345"})
	if err != nil {
		t.Fatalf("AuthenticateOAuthUser returned error: %v", err)
	}
	if created || loggedIn.ID != user.ID {
		t.Fatalf("oauth login returned user=%q created=%v, want existing %q", loggedIn.ID, created, user.ID)
	}
}

func TestLinkOAuthIdentitySupportsLinuxDO(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := service.CreateUser(UserInput{
		Username: "alice",
		Password: "secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}

	linked, err := service.LinkOAuthIdentity(user.ID, OAuthUserInput{
		Provider: "linuxdo",
		Subject:  "linuxdo-subject",
		Email:    "alice@example.com",
		Username: "alice-linuxdo",
	})
	if err != nil {
		t.Fatalf("LinkOAuthIdentity returned error: %v", err)
	}
	if len(linked.OAuthIdentities) != 1 {
		t.Fatalf("OAuthIdentities = %#v", linked.OAuthIdentities)
	}
	identity := linked.OAuthIdentities[0]
	if identity.Provider != "linuxdo" || identity.Subject != "linuxdo-subject" || identity.Email != "alice@example.com" || identity.Username != "alice-linuxdo" {
		t.Fatalf("identity = %#v", identity)
	}

	loggedIn, created, err := service.AuthenticateOAuthUser(OAuthUserInput{Provider: "linuxdo", Subject: "linuxdo-subject"})
	if err != nil {
		t.Fatalf("AuthenticateOAuthUser returned error: %v", err)
	}
	if created || loggedIn.ID != user.ID {
		t.Fatalf("oauth login returned user=%q created=%v, want existing %q", loggedIn.ID, created, user.ID)
	}

	unlinked, err := service.UnlinkOAuthIdentity(user.ID, "linuxdo")
	if err != nil {
		t.Fatalf("UnlinkOAuthIdentity returned error: %v", err)
	}
	if len(unlinked.OAuthIdentities) != 0 {
		t.Fatalf("OAuthIdentities after unlink = %#v", unlinked.OAuthIdentities)
	}
}

func TestLinkOAuthIdentityRejectsIdentityLinkedToAnotherUser(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	alice, err := service.CreateUser(UserInput{Username: "alice", Password: "secret", Role: core.UserRoleUser, Enabled: true})
	if err != nil {
		t.Fatalf("CreateUser alice returned error: %v", err)
	}
	bob, err := service.CreateUser(UserInput{Username: "bob", Password: "secret", Role: core.UserRoleUser, Enabled: true})
	if err != nil {
		t.Fatalf("CreateUser bob returned error: %v", err)
	}
	if _, err := service.LinkOAuthIdentity(alice.ID, OAuthUserInput{Provider: "google", Subject: "subject-1"}); err != nil {
		t.Fatalf("LinkOAuthIdentity alice returned error: %v", err)
	}
	if _, err := service.LinkOAuthIdentity(bob.ID, OAuthUserInput{Provider: "google", Subject: "subject-1"}); err == nil || !strings.Contains(err.Error(), "already linked") {
		t.Fatalf("LinkOAuthIdentity bob err = %v, want already linked", err)
	}
}

func TestUnlinkOAuthIdentityRemovesProvider(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := service.CreateUser(UserInput{Username: "alice", Password: "secret", Role: core.UserRoleUser, Enabled: true})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if _, err := service.LinkOAuthIdentity(user.ID, OAuthUserInput{Provider: "github", Subject: "12345"}); err != nil {
		t.Fatalf("LinkOAuthIdentity github returned error: %v", err)
	}
	if _, err := service.LinkOAuthIdentity(user.ID, OAuthUserInput{Provider: "google", Subject: "67890"}); err != nil {
		t.Fatalf("LinkOAuthIdentity google returned error: %v", err)
	}

	updated, err := service.UnlinkOAuthIdentity(user.ID, "github")
	if err != nil {
		t.Fatalf("UnlinkOAuthIdentity returned error: %v", err)
	}
	if len(updated.OAuthIdentities) != 1 || updated.OAuthIdentities[0].Provider != "google" {
		t.Fatalf("OAuthIdentities after unlink = %#v", updated.OAuthIdentities)
	}
}

func TestMergeOAuthUserMovesIdentityBalanceAndClients(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	target, err := service.CreateUser(UserInput{
		Username:       "alice",
		Password:       "secret",
		Role:           core.UserRoleUser,
		Enabled:        true,
		BalanceNanoUSD: 1000,
	})
	if err != nil {
		t.Fatalf("CreateUser target returned error: %v", err)
	}
	source, err := service.CreateUser(UserInput{
		Username:       "google_alice",
		Password:       "secret",
		Role:           core.UserRoleUser,
		Enabled:        true,
		BalanceNanoUSD: 2500,
		Email:          "alice@example.com",
		EmailVerified:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser source returned error: %v", err)
	}
	if _, err := service.LinkOAuthIdentity(source.ID, OAuthUserInput{Provider: "google", Subject: "google-subject", Email: "alice@example.com"}); err != nil {
		t.Fatalf("LinkOAuthIdentity returned error: %v", err)
	}
	invitee, err := service.CreateUser(UserInput{
		Username:      "invitee",
		Password:      "secret",
		Role:          core.UserRoleUser,
		Enabled:       true,
		InviterUserID: source.ID,
	})
	if err != nil {
		t.Fatalf("CreateUser invitee returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_source", Name: "Source", APIKey: "gw_source", OwnerUserID: source.ID, Enabled: true}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}

	merged, disabled, err := service.MergeOAuthUser(target.ID, source.ID, "google", "google-subject")
	if err != nil {
		t.Fatalf("MergeOAuthUser returned error: %v", err)
	}
	if merged.ID != target.ID || merged.BalanceNanoUSD != 3500 || merged.Email != "alice@example.com" || !merged.EmailVerified {
		t.Fatalf("merged user = %#v", merged)
	}
	if len(merged.OAuthIdentities) != 1 || merged.OAuthIdentities[0].Provider != "google" || merged.OAuthIdentities[0].Subject != "google-subject" {
		t.Fatalf("merged OAuthIdentities = %#v", merged.OAuthIdentities)
	}
	if disabled.ID != source.ID || disabled.Enabled || disabled.BalanceNanoUSD != 0 || len(disabled.OAuthIdentities) != 0 {
		t.Fatalf("disabled source = %#v", disabled)
	}
	client, err := repo.GetClient("client_source")
	if err != nil {
		t.Fatalf("GetClient returned error: %v", err)
	}
	if client.OwnerUserID != target.ID {
		t.Fatalf("client OwnerUserID = %q, want %q", client.OwnerUserID, target.ID)
	}
	updatedInvitee, err := service.GetUser(invitee.ID)
	if err != nil {
		t.Fatalf("GetUser invitee returned error: %v", err)
	}
	if updatedInvitee.InviterUserID != target.ID {
		t.Fatalf("invitee InviterUserID = %q, want %q", updatedInvitee.InviterUserID, target.ID)
	}
	ledger := service.ListBillingLedger(target.ID, 10)
	if len(ledger) == 0 || ledger[0].Kind != "account_merge" || ledger[0].AmountNanoUSD != 2500 || ledger[0].BalanceAfterNanoUSD != 3500 {
		t.Fatalf("merge ledger = %#v", ledger)
	}
	loggedIn, created, err := service.AuthenticateOAuthUser(OAuthUserInput{Provider: "google", Subject: "google-subject"})
	if err != nil {
		t.Fatalf("AuthenticateOAuthUser returned error: %v", err)
	}
	if created || loggedIn.ID != target.ID {
		t.Fatalf("oauth login returned user=%q created=%v, want target %q", loggedIn.ID, created, target.ID)
	}
}

func TestDeleteUserPreservesBillingHistoryAndDeletesOwnedClients(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_history", Username: "history", Role: core.UserRoleUser, Enabled: true, BalanceNanoUSD: 5000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_history", Name: "History", APIKey: "gw_history", OwnerUserID: "user_history", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_clean", Name: "Clean", APIKey: "gw_clean", OwnerUserID: "user_history", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	seedUsageLogBillingRequest(t, repo, "req_history", "client_history", "user_history")

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	deleted, err := service.DeleteUser("user_history")
	if err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	if deleted.ID != "user_history" {
		t.Fatalf("deleted user = %#v", deleted)
	}
	if _, err := repo.GetUser("user_history"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetUser err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetClient("client_history"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetClient client_history err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetClient("client_clean"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetClient client_clean err = %v, want ErrNotFound", err)
	}
	if items, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{UserID: "user_history", Limit: 10}); total != 1 || len(items) != 1 {
		t.Fatalf("billing requests after user delete = total %d items %#v, want preserved", total, items)
	}
}

func TestDeleteUserWithOptionsDeletesInvitedUsers(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, user := range []core.User{
		{ID: "user_inviter", Username: "inviter", Role: core.UserRoleUser, Enabled: true},
		{ID: "user_invited_a", Username: "invited-a", Role: core.UserRoleUser, Enabled: true, InviterUserID: "user_inviter"},
		{ID: "user_invited_b", Username: "invited-b", Role: core.UserRoleUser, Enabled: true, InviterUserID: "user_inviter"},
		{ID: "user_unrelated", Username: "unrelated", Role: core.UserRoleUser, Enabled: true},
	} {
		if err := repo.UpsertUser(user); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_invited", Name: "Invited", APIKey: "gw_invited", OwnerUserID: "user_invited_a", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	result, err := service.DeleteUserWithOptions("user_inviter", DeleteUserOptions{DeleteInvitedUsers: true})
	if err != nil {
		t.Fatalf("DeleteUserWithOptions returned error: %v", err)
	}
	if result.User.ID != "user_inviter" || len(result.InvitedUsers) != 2 {
		t.Fatalf("delete result = %#v", result)
	}
	for _, id := range []string{"user_inviter", "user_invited_a", "user_invited_b"} {
		if _, err := repo.GetUser(id); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("GetUser(%s) err = %v, want ErrNotFound", id, err)
		}
	}
	if _, err := repo.GetClient("client_invited"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetClient client_invited err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetUser("user_unrelated"); err != nil {
		t.Fatalf("unrelated user should remain: %v", err)
	}
}

func TestDeleteUserAllowsUnpaidPaymentOrderAndDeletesOwnedClients(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_unpaid_order", Username: "unpaid_order", Role: core.UserRoleUser, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_clean", Name: "Clean", APIKey: "gw_clean", OwnerUserID: "user_unpaid_order", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreatePaymentOrder(core.PaymentOrder{
		ID:            "pay_unpaid",
		OutTradeNo:    "out_unpaid",
		UserID:        "user_unpaid_order",
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: core.NanoUSDPerUSD / 100,
		Status:        core.PaymentOrderPending,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	deleted, err := service.DeleteUser("user_unpaid_order")
	if err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	if deleted.ID != "user_unpaid_order" {
		t.Fatalf("deleted user = %#v", deleted)
	}
	if _, err := repo.GetUser("user_unpaid_order"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetUser err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetClient("client_clean"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetClient err = %v, want ErrNotFound", err)
	}
	if order, err := repo.GetPaymentOrder("pay_unpaid"); err != nil || order.UserID != "user_unpaid_order" {
		t.Fatalf("GetPaymentOrder = %#v err=%v, want preserved static order", order, err)
	}
}

func TestDetectAccountStatusPingsSelectedAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:               "acct_test",
		Provider:         core.ProviderOpenAI,
		Label:            "Test Account",
		Group:            "Plus",
		Status:           core.AccountStatusCooling,
		CooldownUntil:    ptrTime(time.Now().UTC().Add(5 * time.Minute)),
		ConsecutiveFails: 2,
		TotalFails:       5,
		Credential: core.Credential{
			AccessToken: "stale-token",
			ExpiresAt:   ptrTime(time.Now().UTC().Add(time.Second)),
			Metadata: map[string]string{
				"token_source": providers.OpenAIDeviceCodeTokenSourceValue(),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	adapter := &testQuotaAdapter{}
	service := New(repo, providers.NewRegistry(adapter))
	account, err := service.DetectAccountStatus(context.Background(), "acct_test")
	if err != nil {
		t.Fatalf("DetectAccountStatus returned error: %v", err)
	}
	if account.ID != "acct_test" {
		t.Fatalf("account id = %q, want acct_test", account.ID)
	}
	if account.Status != core.AccountStatusActive {
		t.Fatalf("status = %q, want active", account.Status)
	}
	if account.Credential.AccessToken != "fresh-token" {
		t.Fatalf("access token = %q, want fresh-token", account.Credential.AccessToken)
	}
	if strings.TrimSpace(account.Credential.Metadata["oauth_refreshed_at"]) == "" {
		t.Fatalf("metadata = %#v, want oauth refresh marker", account.Credential.Metadata)
	}
	if account.CooldownUntil != nil {
		t.Fatalf("cooldown = %#v, want nil", account.CooldownUntil)
	}
	if account.ConsecutiveFails != 0 {
		t.Fatalf("consecutive fails = %d, want 0", account.ConsecutiveFails)
	}
	if account.TotalFails != 5 {
		t.Fatalf("total fails = %d, want 5", account.TotalFails)
	}
}

func TestDetectAccountStatusReactivatesBlockedAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_test",
		Provider: core.ProviderOpenAI,
		Label:    "Blocked Account",
		Group:    "Plus",
		Status:   core.AccountStatusBlocked,
		Credential: core.Credential{
			AccessToken: "api-key-token",
			Metadata: map[string]string{
				"token_source": providers.OpenAIDeviceCodeTokenSourceValue(),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&testQuotaAdapter{}))
	account, err := service.DetectAccountStatus(context.Background(), "acct_test")
	if err != nil {
		t.Fatalf("DetectAccountStatus returned error: %v", err)
	}
	if account.Status != core.AccountStatusActive {
		t.Fatalf("status = %q, want active", account.Status)
	}
	saved, err := repo.GetAccount("acct_test")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != core.AccountStatusActive {
		t.Fatalf("saved status = %q, want active", saved.Status)
	}
}

func TestDetectAccountStatusUsesUnifiedOpenAIModelForTokenLogin(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_test",
		Provider: core.ProviderOpenAI,
		Label:    "Test Account",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			AccessToken: "api-key-token",
			Metadata: map[string]string{
				"account_login_method": "token",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	adapter := &testAccountInvokeAdapter{}
	service := New(repo, providers.NewRegistry(adapter))
	if _, err := service.DetectAccountStatus(context.Background(), "acct_test"); err != nil {
		t.Fatalf("DetectAccountStatus returned error: %v", err)
	}
	if adapter.upstreamMode != "" {
		t.Fatalf("upstream mode = %q, want empty", adapter.upstreamMode)
	}
	if adapter.model != "gpt-5.5" {
		t.Fatalf("model = %q, want gpt-5.5", adapter.model)
	}
	if adapter.message != "Reply with exactly this word: pong" {
		t.Fatalf("message = %q, want token detection prompt", adapter.message)
	}
}

func TestDetectAccountStatusUsesChatCompletionsForOpenAIAPIKeyLogin(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_test",
		Provider: core.ProviderOpenAI,
		Label:    "Test Account",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			AccessToken: "api-key-token",
			Mode:        "manual-token",
			Metadata: map[string]string{
				"account_login_method": "api_key",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	adapter := &testAccountInvokeAdapter{}
	service := New(repo, providers.NewRegistry(adapter))
	if _, err := service.DetectAccountStatus(context.Background(), "acct_test"); err != nil {
		t.Fatalf("DetectAccountStatus returned error: %v", err)
	}
	if adapter.upstreamMode != "" {
		t.Fatalf("upstream mode = %q, want chat completions", adapter.upstreamMode)
	}
	if adapter.model != "gpt-5.5" {
		t.Fatalf("model = %q, want gpt-5.5", adapter.model)
	}
	if adapter.message != "hi" {
		t.Fatalf("message = %q, want API-key detection prompt", adapter.message)
	}
}

func TestDetectAccountStatusUsesRealMessageForOpenAIAPIKeyLogin(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:              "acct_test",
		Provider:        core.ProviderOpenAI,
		Label:           "Test Account",
		Status:          core.AccountStatusExpired,
		ControlDisabled: true,
		Credential: core.Credential{
			AccessToken: "api-key-token",
			Mode:        "manual-token",
			Metadata: map[string]string{
				"account_login_method":   "api_key",
				"api_key_quota_provider": "sub2api",
				"base_url":               "https://gateway.example.test",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	adapter := &testAccountInvokeAdapter{}
	service := New(repo, providers.NewRegistry(adapter))
	account, err := service.DetectAccountStatus(context.Background(), "acct_test")
	if err != nil {
		t.Fatalf("DetectAccountStatus returned error: %v", err)
	}
	if adapter.upstreamMode != "" {
		t.Fatalf("upstream mode = %q, want chat completions", adapter.upstreamMode)
	}
	if adapter.model != "gpt-5.5" {
		t.Fatalf("model = %q, want gpt-5.5", adapter.model)
	}
	if adapter.message != "hi" {
		t.Fatalf("message = %q, want real detection prompt for API-key login", adapter.message)
	}
	if account.Status != core.AccountStatusActive {
		t.Fatalf("status = %q, want active", account.Status)
	}
	if !account.ControlDisabled {
		t.Fatal("control-disabled state should be preserved")
	}
}

func TestDetectAccountStatusUsesRealMessageEvenWithQuotaMetadata(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_sub2api_disabled",
		Provider: core.ProviderOpenAI,
		Label:    "Sub2API Disabled",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			AccessToken: "api-key-token",
			Mode:        "manual-token",
			Metadata: map[string]string{
				"account_login_method":                  "api_key",
				"api_key_quota_provider":                "sub2api",
				"base_url":                              "https://gateway.example.test",
				core.AccountQuotaErrorStatusMetadataKey: string(core.AccountStatusExpired),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	adapter := &testAccountInvokeAdapter{}
	service := New(repo, providers.NewRegistry(adapter))
	account, err := service.DetectAccountStatus(context.Background(), "acct_sub2api_disabled")
	if err != nil {
		t.Fatalf("DetectAccountStatus returned error: %v", err)
	}
	if adapter.upstreamMode != "" {
		t.Fatalf("upstream mode = %q, want chat completions", adapter.upstreamMode)
	}
	if adapter.model != "gpt-5.5" {
		t.Fatalf("model = %q, want gpt-5.5", adapter.model)
	}
	if adapter.message != "hi" {
		t.Fatalf("message = %q, want real detection prompt for API-key login", adapter.message)
	}
	if account.Status != core.AccountStatusActive {
		t.Fatalf("returned status = %q, want active", account.Status)
	}
	saved, err := repo.GetAccount("acct_sub2api_disabled")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != core.AccountStatusActive {
		t.Fatalf("saved status = %q, want active", saved.Status)
	}
}

func TestDetectAccountStatusAPIKeyIgnoresConfiguredTextModel(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertModel(core.ModelConfig{
		ID:       "gpt-5.5",
		Provider: core.ProviderOpenAI,
		Type:     core.ModelTypeText,
		Enabled:  true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_test",
		Provider: core.ProviderOpenAI,
		Label:    "Test Account",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			AccessToken: "api-key-token",
			Metadata: map[string]string{
				"account_login_method": "api_key",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	adapter := &testAccountInvokeAdapter{}
	service := New(repo, providers.NewRegistry(adapter))
	if _, err := service.DetectAccountStatus(context.Background(), "acct_test"); err != nil {
		t.Fatalf("DetectAccountStatus returned error: %v", err)
	}
	if adapter.model != "gpt-5.5" {
		t.Fatalf("model = %q, want gpt-5.5", adapter.model)
	}
}

func TestDetectAccountStatusTokenLoginUsesUnifiedOpenAIModel(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertModel(core.ModelConfig{
		ID:       "gpt-5.3-codex",
		Provider: core.ProviderOpenAI,
		Type:     core.ModelTypeText,
		Enabled:  true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_test",
		Provider: core.ProviderOpenAI,
		Label:    "Test Account",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			AccessToken: "api-key-token",
			Metadata: map[string]string{
				"account_login_method": "token",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	adapter := &testAccountInvokeAdapter{}
	service := New(repo, providers.NewRegistry(adapter))
	if _, err := service.DetectAccountStatus(context.Background(), "acct_test"); err != nil {
		t.Fatalf("DetectAccountStatus returned error: %v", err)
	}
	if adapter.model != "gpt-5.5" {
		t.Fatalf("model = %q, want gpt-5.5", adapter.model)
	}
}

func TestDetectAccountStatusUsesClaudeDefaultDetectionModel(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_claude",
		Provider: core.ProviderClaude,
		Label:    "Claude Account",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			AccessToken: "claude-token",
		},
	}); err != nil {
		t.Fatal(err)
	}

	adapter := &testClaudeAccountInvokeAdapter{}
	service := New(repo, providers.NewRegistry(adapter))
	if _, err := service.DetectAccountStatus(context.Background(), "acct_claude"); err != nil {
		t.Fatalf("DetectAccountStatus returned error: %v", err)
	}
	if adapter.model != "claude-sonnet-4-6" {
		t.Fatalf("model = %q, want claude-sonnet-4-6", adapter.model)
	}
	if adapter.message != "Reply with exactly this word: pong" {
		t.Fatalf("message = %q, want Claude detection prompt", adapter.message)
	}
	if len(adapter.metadata) != 0 {
		t.Fatalf("metadata = %#v, want empty for Claude detection", adapter.metadata)
	}
}

func TestDetectAccountStatusDoesNotPersistPingFailuresAsTerminal(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:               "acct_test",
		Provider:         core.ProviderOpenAI,
		Label:            "Test Account",
		Group:            "Plus",
		Status:           core.AccountStatusActive,
		ConsecutiveFails: 3,
		TotalFails:       9,
		Credential: core.Credential{
			AccessToken: "api-key-token",
			Metadata: map[string]string{
				"token_source": "api_key",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&testQuotaAuthFailAdapter{}))
	account, err := service.DetectAccountStatus(context.Background(), "acct_test")
	if err == nil {
		t.Fatal("expected ping detection error")
	}
	if !strings.Contains(err.Error(), "refresh token") {
		t.Fatalf("error = %q, want ping failure hint", err.Error())
	}
	if account.Status != core.AccountStatusActive {
		t.Fatalf("status = %q, want active", account.Status)
	}
	if account.CooldownUntil != nil {
		t.Fatalf("cooldown = %#v, want nil", account.CooldownUntil)
	}
	if account.ConsecutiveFails != 3 {
		t.Fatalf("consecutive fails = %d, want 3", account.ConsecutiveFails)
	}
	if account.TotalFails != 9 {
		t.Fatalf("total fails = %d, want 9", account.TotalFails)
	}
}

func TestDetectAccountStatusRejectsEmptyDetectionStream(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_test",
		Provider: core.ProviderOpenAI,
		Label:    "Test Account",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			AccessToken: "api-key-token",
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&testEmptyStreamAdapter{}))
	account, err := service.DetectAccountStatus(context.Background(), "acct_test")
	if err == nil {
		t.Fatal("expected empty detection stream error")
	}
	if !strings.Contains(err.Error(), "detection stream ended") {
		t.Fatalf("error = %q, want empty stream hint", err.Error())
	}
	if account.Status != core.AccountStatusActive {
		t.Fatalf("status = %q, want active", account.Status)
	}
}

func TestDetectAccountStatusRequiresOpenAIResponsesCompleted(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_test",
		Provider: core.ProviderOpenAI,
		Label:    "Test Account",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			AccessToken: "api-key-token",
			Mode:        providers.OpenAIOAuthModeValue(),
			Metadata: map[string]string{
				"token_source": providers.OpenAIDeviceCodeTokenSourceValue(),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&testOpenAIDeltaOnlyAdapter{}))
	account, err := service.DetectAccountStatus(context.Background(), "acct_test")
	if err == nil {
		t.Fatal("expected stream completion error")
	}
	if !strings.Contains(err.Error(), "detection stream ended") {
		t.Fatalf("error = %q, want completion hint", err.Error())
	}
	if account.Status != core.AccountStatusActive {
		t.Fatalf("status = %q, want active", account.Status)
	}
}

func TestDetectAccountStatusRejectsOpenAIResponsesFailed(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_test",
		Provider: core.ProviderOpenAI,
		Label:    "Test Account",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			AccessToken: "api-key-token",
			Mode:        providers.OpenAIOAuthModeValue(),
			Metadata: map[string]string{
				"token_source": providers.OpenAIDeviceCodeTokenSourceValue(),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&testOpenAIFailedAdapter{}))
	account, err := service.DetectAccountStatus(context.Background(), "acct_test")
	if err == nil {
		t.Fatal("expected failed stream error")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Fatalf("error = %q, want failed hint", err.Error())
	}
	if account.Status != core.AccountStatusActive {
		t.Fatalf("status = %q, want active", account.Status)
	}
}

func TestDetectAccountStatusPreservesTerminalStatusOnTransientFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status core.AccountStatus
	}{
		{name: "expired", status: core.AccountStatusExpired},
		{name: "provider_banned", status: core.AccountStatusProviderBanned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := storage.NewMemoryRepository()
			if err := repo.UpsertAccount(core.Account{
				ID:               "acct_test",
				Provider:         core.ProviderOpenAI,
				Label:            "Test Account",
				Group:            "Plus",
				Status:           tc.status,
				ConsecutiveFails: 4,
				TotalFails:       11,
				Credential: core.Credential{
					AccessToken: "api-key-token",
					Metadata: map[string]string{
						"token_source": "api_key",
					},
				},
			}); err != nil {
				t.Fatal(err)
			}

			service := New(repo, providers.NewRegistry(&testQuotaFailAdapter{}))
			account, err := service.DetectAccountStatus(context.Background(), "acct_test")
			if err == nil {
				t.Fatal("expected ping detection error")
			}
			if account.Status != tc.status {
				t.Fatalf("status = %q, want %q", account.Status, tc.status)
			}
			if account.CooldownUntil != nil {
				t.Fatalf("cooldown = %#v, want nil", account.CooldownUntil)
			}
			if account.ConsecutiveFails != 4 {
				t.Fatalf("consecutive fails = %d, want 4", account.ConsecutiveFails)
			}
			if account.TotalFails != 11 {
				t.Fatalf("total fails = %d, want 11", account.TotalFails)
			}
		})
	}
}

func TestCreateModelStoresAndInfersType(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	custom, err := service.CreateModel(ModelInput{
		ID:       "internal-renderer",
		Provider: core.ProviderOpenAI,
		Type:     core.ModelTypeVideo,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateModel returned error: %v", err)
	}
	if custom.Type != core.ModelTypeVideo {
		t.Fatalf("custom type = %q, want %q", custom.Type, core.ModelTypeVideo)
	}

	inferred, err := service.CreateModel(ModelInput{
		ID:       "gpt-image-2",
		Provider: core.ProviderOpenAI,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateModel inferred returned error: %v", err)
	}
	if inferred.Type != core.ModelTypeImage {
		t.Fatalf("inferred type = %q, want %q", inferred.Type, core.ModelTypeImage)
	}
}

func TestAdminClientAccessIsScopedToOwnedClients(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertClient(core.APIClient{ID: "client_admin", Name: "Admin Key", APIKey: "gw_admin", OwnerUserID: "admin_user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_other", Name: "Other Key", APIKey: "gw_other", OwnerUserID: "other_user", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin := core.User{ID: "admin_user", Role: core.UserRoleAdmin, Enabled: true}
	clients := service.ClientsForUser(admin)
	if len(clients) != 1 || clients[0].ID != "client_admin" {
		t.Fatalf("admin clients = %#v, want only owned client", clients)
	}

	err := service.ToggleClientForUser(admin, "client_other")
	if err == nil {
		t.Fatal("expected admin foreign client access to be denied")
	}
	var accessErr *AccessError
	if !errors.As(err, &accessErr) || accessErr.StatusCode != httpStatusForbidden {
		t.Fatalf("expected forbidden AccessError, got %T %[1]v", err)
	}
}

func seedUsageLogBillingRequest(t *testing.T, repo storage.Repository, requestID, clientID, userID string) {
	t.Helper()
	seedUsageLogBillingRequestWithAccount(t, repo, requestID, clientID, userID, "")
}

func seedUsageLogBillingRequestWithAccount(t *testing.T, repo storage.Repository, requestID, clientID, userID, accountID string) {
	t.Helper()
	client, err := repo.GetClient(clientID)
	if err != nil {
		t.Fatalf("GetClient(%s): %v", clientID, err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       requestID,
		ClientID:        clientID,
		ClientName:      client.Name,
		UserID:          userID,
		AccountID:       accountID,
		Model:           "gpt-4.1",
		ReservedNanoUSD: 100,
		Fingerprint:     requestID,
	}); err != nil {
		t.Fatalf("ReserveBilling(%s): %v", requestID, err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     requestID,
		ClientID:      clientID,
		AccountID:     accountID,
		Model:         "gpt-4.1",
		Usage:         core.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		ActualNanoUSD: 120,
	}); err != nil {
		t.Fatalf("SettleBilling(%s): %v", requestID, err)
	}
}

func seedZeroCostUsageLogBillingRequest(t *testing.T, repo storage.Repository, requestID, clientID, userID string) {
	t.Helper()
	client, err := repo.GetClient(clientID)
	if err != nil {
		t.Fatalf("GetClient(%s): %v", clientID, err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:   requestID,
		ClientID:    clientID,
		ClientName:  client.Name,
		UserID:      userID,
		Model:       "gpt-4.1",
		Fingerprint: requestID,
	}); err != nil {
		t.Fatalf("ReserveBilling(%s): %v", requestID, err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     requestID,
		ClientID:      clientID,
		Model:         "gpt-4.1",
		ActualNanoUSD: 0,
	}); err != nil {
		t.Fatalf("SettleBilling(%s): %v", requestID, err)
	}
}

func TestUpdateAccountGroupProxy(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_core", Name: "Core"}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	group, err := service.UpdateAccountGroupProxy("group_core", "http://127.0.0.1:7890")
	if err != nil {
		t.Fatalf("update group proxy: %v", err)
	}
	if group.ProxyURL != "http://127.0.0.1:7890" {
		t.Fatalf("proxy_url = %q", group.ProxyURL)
	}

	if _, err := service.UpdateAccountGroupProxy("group_core", "ftp://127.0.0.1:21"); err == nil {
		t.Fatal("expected unsupported proxy scheme error")
	}
}

func TestDashboardBuildsAccountGroups(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_alpha", Name: "Alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_beta", Name: "Beta"}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{ID: "acct_1", Provider: core.ProviderOpenAI, Label: "Default", Group: core.DefaultAccountGroupName, Status: core.AccountStatusActive},
		{ID: "acct_5", Provider: core.ProviderOpenAI, Label: "Alpha Blocked", Group: "Alpha", Status: core.AccountStatusBlocked},
		accountWithQuotaSnapshot(t, core.Account{ID: "acct_6", Provider: core.ProviderOpenAI, Label: "Alpha Time Limit", Group: "Alpha", Status: core.AccountStatusActive}, core.AccountQuotaSnapshot{Primary: &core.AccountQuotaWindow{UsedPercent: 100}}),
		{ID: "acct_2", Provider: core.ProviderOpenAI, Label: "Alpha A", Group: "Alpha", Status: core.AccountStatusActive},
		accountWithQuotaSnapshot(t, core.Account{ID: "acct_7", Provider: core.ProviderOpenAI, Label: "Alpha Weekly Limit", Group: "Alpha", Status: core.AccountStatusActive}, core.AccountQuotaSnapshot{Secondary: &core.AccountQuotaWindow{UsedPercent: 100}}),
		{ID: "acct_3", Provider: core.ProviderClaude, Label: "Beta A", Group: "Beta", Status: core.AccountStatusActive},
		{ID: "acct_4", Provider: core.ProviderOpenAI, Label: "Alpha B", Group: "Alpha", Status: core.AccountStatusActive},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	dashboard := service.Dashboard(context.Background())
	if len(dashboard.AccountGroups) != 3 {
		t.Fatalf("group count = %d, want 3", len(dashboard.AccountGroups))
	}
	if dashboard.AccountGroups[0].Label != core.DefaultAccountGroupName || len(dashboard.AccountGroups[0].Accounts) != 1 {
		t.Fatalf("first group = %#v", dashboard.AccountGroups[0])
	}
	if dashboard.AccountGroups[1].Label != "Alpha" || len(dashboard.AccountGroups[1].Accounts) != 5 {
		t.Fatalf("second group = %#v", dashboard.AccountGroups[1])
	}
	gotAlphaOrder := []string{}
	for _, account := range dashboard.AccountGroups[1].Accounts {
		gotAlphaOrder = append(gotAlphaOrder, account.ID)
	}
	wantAlphaOrder := []string{"acct_2", "acct_4", "acct_6", "acct_7", "acct_5"}
	if !slices.Equal(gotAlphaOrder, wantAlphaOrder) {
		t.Fatalf("alpha account order = %#v, want %#v", gotAlphaOrder, wantAlphaOrder)
	}
	if dashboard.AccountGroups[2].Label != "Beta" || len(dashboard.AccountGroups[2].Accounts) != 1 {
		t.Fatalf("third group = %#v", dashboard.AccountGroups[2])
	}
}

func TestDashboardBuildsAccountPools(t *testing.T) {
	repo := storage.NewMemoryRepository()
	future := time.Now().UTC().Add(time.Hour)
	if err := repo.UpsertAccount(core.Account{ID: "normal", Provider: core.ProviderOpenAI, Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "cooling", Provider: core.ProviderOpenAI, Status: core.AccountStatusCooling, CooldownUntil: &future}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "abnormal", Provider: core.ProviderOpenAI, Status: core.AccountStatusBlocked}); err != nil {
		t.Fatal(err)
	}

	dashboard := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{})).Dashboard(context.Background())
	if len(dashboard.AccountPools.Normal) != 1 || dashboard.AccountPools.Normal[0].ID != "normal" {
		t.Fatalf("normal pool = %#v", dashboard.AccountPools.Normal)
	}
	if len(dashboard.AccountPools.Cooling) != 1 || dashboard.AccountPools.Cooling[0].ID != "cooling" {
		t.Fatalf("cooling pool = %#v", dashboard.AccountPools.Cooling)
	}
	if len(dashboard.AccountPools.Abnormal) != 1 || dashboard.AccountPools.Abnormal[0].ID != "abnormal" {
		t.Fatalf("abnormal pool = %#v", dashboard.AccountPools.Abnormal)
	}
}

func accountWithQuotaSnapshot(t *testing.T, account core.Account, snapshot core.AccountQuotaSnapshot) core.Account {
	t.Helper()
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if account.Credential.Metadata == nil {
		account.Credential.Metadata = map[string]string{}
	}
	account.Credential.Metadata[core.AccountQuotaMetadataKey] = string(raw)
	return account
}

func TestApplyBillingMultiplierSaturatesOverflow(t *testing.T) {
	const wantMaxInt64 = int64(^uint64(0) >> 1)
	if got := applyBillingMultiplier(wantMaxInt64, 10000); got != wantMaxInt64 {
		t.Fatalf("multiplied price = %d, want max int64", got)
	}
	if got := applyBillingMultiplier(wantMaxInt64/2+1, 20000); got != wantMaxInt64 {
		t.Fatalf("overflow multiplied price = %d, want max int64", got)
	}
}

func TestDashboardAlwaysIncludesDefaultAccountGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_plus", Provider: core.ProviderOpenAI, Label: "Plus A", Group: "Plus", Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	dashboard := service.Dashboard(context.Background())
	if len(dashboard.AccountGroups) != 2 {
		t.Fatalf("group count = %d, want 2", len(dashboard.AccountGroups))
	}
	if dashboard.AccountGroups[0].Label != core.DefaultAccountGroupName || len(dashboard.AccountGroups[0].Accounts) != 0 {
		t.Fatalf("default group = %#v", dashboard.AccountGroups[0])
	}
	if dashboard.AccountGroups[0].ID != "default" || dashboard.AccountGroups[0].BillingMultiplierBps != 10000 {
		t.Fatalf("default settings = %#v", dashboard.AccountGroups[0])
	}
	if dashboard.AccountGroups[1].Label != "Plus" || len(dashboard.AccountGroups[1].Accounts) != 1 {
		t.Fatalf("plus group = %#v", dashboard.AccountGroups[1])
	}
}

func TestDashboardUsesAuditSummaries(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.AppendAudit(core.AuditEvent{
		ID:          "audit_large",
		Kind:        core.AuditKindGateway,
		ClientID:    "client_test",
		ClientName:  "Client Test",
		Status:      "ok",
		RequestBody: strings.Repeat("large request ", 128),
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	dashboard := service.Dashboard(context.Background())
	if len(dashboard.Audit) != 1 || dashboard.Audit[0].RequestBody != "" {
		t.Fatalf("dashboard audit should use summaries: %#v", dashboard.Audit)
	}
	if len(dashboard.GatewayAudit) != 1 || dashboard.GatewayAudit[0].RequestBody != "" {
		t.Fatalf("gateway audit should use summaries: %#v", dashboard.GatewayAudit)
	}
}

type dashboardFullListPanicRepository struct {
	*storage.MemoryRepository
}

func (r *dashboardFullListPanicRepository) ListUsers() []core.User {
	panic("Dashboard should use ListUsersPage instead of full ListUsers")
}

func (r *dashboardFullListPanicRepository) ListClients() []core.APIClient {
	panic("Dashboard should use ListClientSummariesPage instead of full ListClients")
}

func (r *dashboardFullListPanicRepository) ListUsersPage(query storage.UserListQuery) ([]storage.UserListItem, int, int) {
	return []storage.UserListItem{{
		User: core.User{ID: "user_preview", Username: "preview"},
	}}, 42, 42
}

func (r *dashboardFullListPanicRepository) ListClientSummariesPage(offset, limit int) ([]core.APIClient, int) {
	return []core.APIClient{
		{ID: "client_a", Name: "Client A", APIKey: "secret", RouteAffinityKey: "affinity", Enabled: true, AccountGroup: "Plus"},
	}, 13
}

func TestDashboardUsesPagedEntitySummaries(t *testing.T) {
	repo := &dashboardFullListPanicRepository{MemoryRepository: storage.NewMemoryRepository()}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	dashboard := service.Dashboard(context.Background())
	if dashboard.TotalUsers != 42 || dashboard.TotalClients != 13 {
		t.Fatalf("dashboard counts = users %d clients %d, want 42 and 13", dashboard.TotalUsers, dashboard.TotalClients)
	}
	if len(dashboard.Clients) != 1 || dashboard.Clients[0].ID != "client_a" {
		t.Fatalf("dashboard clients = %#v", dashboard.Clients)
	}
	if dashboard.Clients[0].APIKey != "" || dashboard.Clients[0].RouteAffinityKey != "" {
		t.Fatalf("dashboard client summary leaked secrets: %#v", dashboard.Clients[0])
	}
}

func TestAccountDashboardLoadsOnlySelectedVisibleUsers(t *testing.T) {
	base := storage.NewMemoryRepository()
	if err := base.UpsertUser(core.User{ID: "user_visible_group", Username: "visible", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := base.UpsertUser(core.User{ID: "user_unselected_group", Username: "unselected", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	hide := false
	if err := base.UpsertAccountGroup(core.AccountGroup{
		ID:                 "group_private",
		Name:               "Private",
		ShowInClientEditor: &hide,
		VisibleUserIDs:     []string{"user_visible_group"},
	}); err != nil {
		t.Fatal(err)
	}
	repo := &dashboardFullListPanicRepository{MemoryRepository: base}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	dashboard := service.AccountDashboard(context.Background())
	if len(dashboard.Users) != 1 || dashboard.Users[0].ID != "user_visible_group" {
		t.Fatalf("dashboard users = %#v, want selected visible user only", dashboard.Users)
	}
}

type dashboardForUserFullOwnerListPanicRepository struct {
	*storage.MemoryRepository
}

func (r *dashboardForUserFullOwnerListPanicRepository) ListClientsByOwner(ownerUserID string) []core.APIClient {
	panic("DashboardForUser should use ListClientSummariesByOwnerPage instead of full ListClientsByOwner")
}

func (r *dashboardForUserFullOwnerListPanicRepository) ListClientSummariesByOwnerPage(ownerUserID string, offset, limit int) ([]core.APIClient, int) {
	clients := r.MemoryRepository.ListClientsByOwner(ownerUserID)
	total := len(clients)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	if limit <= 0 {
		limit = dashboardClientPreviewLimit
	}
	end := offset + limit
	if end > total {
		end = total
	}
	out := make([]core.APIClient, 0, end-offset)
	for _, client := range clients[offset:end] {
		out = append(out, core.APIClient{
			ID:           client.ID,
			Name:         client.Name,
			OwnerUserID:  client.OwnerUserID,
			Enabled:      client.Enabled,
			AccountGroup: client.AccountGroup,
			LastUsedAt:   client.LastUsedAt,
			CreatedAt:    client.CreatedAt,
			UpdatedAt:    client.UpdatedAt,
		})
	}
	return out, total
}

func TestDashboardForUserUsesOwnedClientPage(t *testing.T) {
	base := storage.NewMemoryRepository()
	user := core.User{ID: "user_dashboard", Username: "dashboard", Role: core.UserRoleUser, Enabled: true}
	if err := base.UpsertUser(user); err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		if err := base.UpsertClient(core.APIClient{
			ID:           fmt.Sprintf("client_dashboard_%02d", i),
			Name:         fmt.Sprintf("Dashboard %02d", i),
			APIKey:       fmt.Sprintf("gw_dashboard_%02d", i),
			OwnerUserID:  user.ID,
			Enabled:      true,
			AccountGroup: "Plus",
		}); err != nil {
			t.Fatalf("UpsertClient(%d) returned error: %v", i, err)
		}
	}
	repo := &dashboardForUserFullOwnerListPanicRepository{MemoryRepository: base}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	dashboard := service.DashboardForUser(context.Background(), user)
	if dashboard.TotalClients != 8 {
		t.Fatalf("TotalClients = %d, want 8", dashboard.TotalClients)
	}
	if len(dashboard.Clients) != dashboardClientPreviewLimit {
		t.Fatalf("dashboard clients = %d, want %d", len(dashboard.Clients), dashboardClientPreviewLimit)
	}
	for _, client := range dashboard.Clients {
		if client.APIKey != "" || client.RouteAffinityKey != "" {
			t.Fatalf("dashboard client summary leaked secrets: %#v", client)
		}
		if client.OwnerUserID != user.ID {
			t.Fatalf("dashboard client owner = %q, want %q", client.OwnerUserID, user.ID)
		}
	}
}

func TestCreateAndDeleteAccountGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_1",
		Provider: core.ProviderOpenAI,
		Label:    "A",
		Group:    "Shared",
		Status:   core.AccountStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	group, err := service.CreateAccountGroup("Shared")
	if err != nil {
		t.Fatalf("CreateAccountGroup returned error: %v", err)
	}
	if group.Name != "Shared" {
		t.Fatalf("group name = %q", group.Name)
	}
	if group.BillingMultiplierBps != 10000 {
		t.Fatalf("billing multiplier = %d, want 10000", group.BillingMultiplierBps)
	}

	deleted, err := service.DeleteAccountGroup(group.ID)
	if err != nil {
		t.Fatalf("DeleteAccountGroup returned error: %v", err)
	}
	if deleted.Name != "Shared" {
		t.Fatalf("deleted group = %#v", deleted)
	}

	account, err := repo.GetAccount("acct_1")
	if err != nil {
		t.Fatal(err)
	}
	if account.Group != core.DefaultAccountGroupName {
		t.Fatalf("account group = %q, want Default", account.Group)
	}
}

func TestCreateAccountGroupStoresNormalizedType(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{}))

	group, err := service.CreateAccountGroup("Claude", "anthropic")
	if err != nil {
		t.Fatalf("CreateAccountGroup returned error: %v", err)
	}
	if group.Type != core.AccountGroupTypeClaude {
		t.Fatalf("group type = %q, want %q", group.Type, core.AccountGroupTypeClaude)
	}
}

func TestUpdateAccountGroupNameRenamesReferences(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	group, err := service.CreateAccountGroup("Shared")
	if err != nil {
		t.Fatalf("CreateAccountGroup returned error: %v", err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_shared", Provider: core.ProviderOpenAI, Label: "Shared", Group: "Shared", Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_shared", Name: "Shared Client", APIKey: "gw_shared", Enabled: true, AccountGroup: "Shared"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{ID: "gpt-5", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{"Shared", "shared", "Default"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateSiteMessage(core.SiteMessage{ID: "msg_shared", Title: "Shared", Body: "Shared body", CreatedBy: "admin", Enabled: true, TargetAccountGroups: []string{"Shared", "Default"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertMonitorTarget(core.MonitorTarget{ID: "mon_shared", AccountGroup: "Shared", Model: "gpt-5", Name: "Shared / gpt-5", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	renamed, err := service.UpdateAccountGroupName(group.ID, "Premium")
	if err != nil {
		t.Fatalf("UpdateAccountGroupName returned error: %v", err)
	}
	if renamed.Name != "Premium" {
		t.Fatalf("renamed group name = %q, want Premium", renamed.Name)
	}
	account, err := repo.GetAccount("acct_shared")
	if err != nil {
		t.Fatal(err)
	}
	if account.Group != "Premium" {
		t.Fatalf("account group = %q, want Premium", account.Group)
	}
	client, err := repo.GetClient("client_shared")
	if err != nil {
		t.Fatal(err)
	}
	if client.AccountGroup != "Premium" || client.APIKey != "gw_shared" {
		t.Fatalf("client after rename = %#v", client)
	}
	model, err := repo.GetModel("gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(model.VisibleGroups, []string{"Premium", "Default"}) {
		t.Fatalf("model visible groups = %#v", model.VisibleGroups)
	}
	message, err := repo.GetSiteMessage("msg_shared")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(message.TargetAccountGroups, []string{"Premium", "Default"}) {
		t.Fatalf("message target groups = %#v", message.TargetAccountGroups)
	}
	target, err := repo.GetMonitorTarget("mon_shared")
	if err != nil {
		t.Fatal(err)
	}
	if target.AccountGroup != "Premium" || target.Name != "Premium / gpt-5" {
		t.Fatalf("monitor target after rename = %#v", target)
	}
}

func TestUpdateAccountGroupProfileUpdatesFieldsAndRenamesReferences(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{}))

	group, err := service.CreateAccountGroup("Shared", core.AccountGroupTypeMixed, "old note")
	if err != nil {
		t.Fatalf("CreateAccountGroup returned error: %v", err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_profile", Provider: core.ProviderOpenAI, Label: "Profile", Group: "Shared", Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_profile", Name: "Profile Client", APIKey: "gw_profile", Enabled: true, AccountGroup: "Shared"}); err != nil {
		t.Fatal(err)
	}

	updated, err := service.UpdateAccountGroupProfile(group.ID, "Premium", core.AccountGroupTypeOpenAI, "  new   profile note  ")
	if err != nil {
		t.Fatalf("UpdateAccountGroupProfile returned error: %v", err)
	}
	if updated.Name != "Premium" || updated.Type != core.AccountGroupTypeOpenAI || updated.Remark != "new profile note" {
		t.Fatalf("updated group = %#v", updated)
	}
	account, err := repo.GetAccount("acct_profile")
	if err != nil {
		t.Fatal(err)
	}
	if account.Group != "Premium" {
		t.Fatalf("account group = %q, want Premium", account.Group)
	}
	client, err := repo.GetClient("client_profile")
	if err != nil {
		t.Fatal(err)
	}
	if client.AccountGroup != "Premium" {
		t.Fatalf("client group = %q, want Premium", client.AccountGroup)
	}
}

func TestUpdateAccountGroupNameRejectsDefaultAndDuplicates(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	shared, err := service.CreateAccountGroup("Shared")
	if err != nil {
		t.Fatalf("CreateAccountGroup(Shared) returned error: %v", err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_premium", Name: "Premium"}); err != nil {
		t.Fatalf("UpsertAccountGroup(Premium) returned error: %v", err)
	}
	if _, err := service.UpdateAccountGroupName(shared.ID, "premium"); err == nil {
		t.Fatal("UpdateAccountGroupName duplicate returned nil error")
	}
	if _, err := service.UpdateAccountGroupName(core.DefaultAccountGroupID, "Renamed"); err == nil {
		t.Fatal("UpdateAccountGroupName default returned nil error")
	}
	if _, err := service.UpdateAccountGroupName(shared.ID, core.DefaultAccountGroupName); err == nil {
		t.Fatal("UpdateAccountGroupName reserved returned nil error")
	}
}

func TestCompleteManualConnectRejectsProviderMismatchedGroupType(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_claude", Name: "Claude", Type: core.AccountGroupTypeClaude}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{}))

	err := service.CompleteManualConnect(ManualConnectInput{
		Provider:    core.ProviderOpenAI,
		Label:       "OpenAI Key",
		Group:       "Claude",
		AccessToken: "sk-test",
		Priority:    100,
		Weight:      100,
	})
	if err == nil {
		t.Fatal("CompleteManualConnect returned nil error, want provider mismatch")
	}
	if !strings.Contains(err.Error(), "does not allow") {
		t.Fatalf("error = %v, want does not allow", err)
	}
	if accounts := repo.ListAccounts(); len(accounts) != 0 {
		t.Fatalf("accounts = %#v, want none", accounts)
	}
}

func TestUpdateAccountGroupTypeRejectsExistingMismatchedAccounts(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_shared", Name: "Shared", Type: core.AccountGroupTypeMixed}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_openai", Provider: core.ProviderOpenAI, Label: "OpenAI", Group: "Shared", Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{}))

	if _, err := service.UpdateAccountGroupType("group_shared", core.AccountGroupTypeClaude); err == nil {
		t.Fatal("UpdateAccountGroupType returned nil error, want provider mismatch")
	}
	groups := repo.ListAccountGroups()
	if len(groups) != 1 {
		t.Fatalf("groups = %#v, want one", groups)
	}
	if groups[0].Type != core.AccountGroupTypeMixed {
		t.Fatalf("group type = %q, want unchanged mixed", groups[0].Type)
	}
}

func TestUpdateAccountGroupRemarkNormalizesRemark(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	group, err := service.CreateAccountGroup("Remarked", core.AccountGroupTypeMixed, "  Primary   pool\nfor paid users  ")
	if err != nil {
		t.Fatalf("CreateAccountGroup returned error: %v", err)
	}
	if group.Remark != "Primary pool for paid users" {
		t.Fatalf("created remark = %q, want normalized text", group.Remark)
	}

	longRemark := strings.Repeat("界", 121)
	updated, err := service.UpdateAccountGroupRemark(group.ID, longRemark)
	if err != nil {
		t.Fatalf("UpdateAccountGroupRemark returned error: %v", err)
	}
	if got := len([]rune(updated.Remark)); got != 120 {
		t.Fatalf("remark length = %d, want 120", got)
	}
	if updated.Remark != strings.Repeat("界", 120) {
		t.Fatalf("remark = %q, want truncated rune text", updated.Remark)
	}

	saved := repo.ListAccountGroups()
	if len(saved) != 1 || saved[0].Remark != strings.Repeat("界", 120) {
		t.Fatalf("saved groups = %#v, want truncated remark", saved)
	}
}

func TestCreateAccountGroupRejectsReservedNames(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	for _, name := range []string{core.DefaultAccountGroupName} {
		if _, err := service.CreateAccountGroup(name); err == nil {
			t.Fatalf("CreateAccountGroup(%q) returned nil error", name)
		}
	}
}

func TestDeleteAccountGroupMovesBoundClientsAndNotifiesOwners(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	owner, err := service.CreateUser(UserInput{
		Username: "owner",
		Password: "owner-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	group, err := service.CreateAccountGroup("Shared")
	if err != nil {
		t.Fatalf("CreateAccountGroup returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:           "client_shared",
		Name:         "Shared Client",
		APIKey:       "gw_shared",
		OwnerUserID:  owner.ID,
		Enabled:      true,
		AccountGroup: "Shared",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := service.DeleteAccountGroup(group.ID); err != nil {
		t.Fatalf("DeleteAccountGroup returned error: %v", err)
	}
	client, err := repo.GetClient("client_shared")
	if err != nil {
		t.Fatal(err)
	}
	if client.AccountGroup != core.DefaultAccountGroupName {
		t.Fatalf("client account group = %q, want default", client.AccountGroup)
	}
	deliveries := service.ListSiteMessages(owner)
	if len(deliveries) != 1 {
		t.Fatalf("owner messages = %#v, want one notice", deliveries)
	}
	if !strings.Contains(deliveries[0].Message.Title, "账号组") || !strings.Contains(deliveries[0].Message.Body, "Shared") || !strings.Contains(deliveries[0].Message.Body, "Shared Client") {
		t.Fatalf("notice = %#v", deliveries[0].Message)
	}
	noticeID := deliveries[0].Message.ID
	if got := service.SiteMessageUnreadCount(owner); got != 1 {
		t.Fatalf("owner unread = %d, want 1", got)
	}
	admin, _, err := service.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	if deliveries := service.ListSiteMessages(admin); siteMessageDeliveryContains(deliveries, noticeID) {
		t.Fatalf("admin inbox should not include account group deletion notice targeted to owner: %#v", deliveries)
	}
}

func TestDeleteAccountGroupPreservesClientAPIKeyFromSummaryCache(t *testing.T) {
	base := storage.NewMemoryRepository()
	repo := storage.NewCachedRepository(base)
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	group, err := service.CreateAccountGroup("Shared")
	if err != nil {
		t.Fatalf("CreateAccountGroup returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:           "client_shared_key",
		Name:         "Shared Key",
		APIKey:       "gw_shared_key",
		Enabled:      true,
		AccountGroup: "Shared",
	}); err != nil {
		t.Fatal(err)
	}
	if clients := repo.ListClients(); len(clients) != 1 || clients[0].APIKey != "" {
		t.Fatalf("cached client list should expose summaries without API keys: %#v", clients)
	}

	if _, err := service.DeleteAccountGroup(group.ID); err != nil {
		t.Fatalf("DeleteAccountGroup returned error: %v", err)
	}
	client, err := base.GetClient("client_shared_key")
	if err != nil {
		t.Fatal(err)
	}
	if client.APIKey != "gw_shared_key" {
		t.Fatalf("api key = %q, want preserved key", client.APIKey)
	}
	if client.AccountGroup != core.DefaultAccountGroupName {
		t.Fatalf("client account group = %q, want default", client.AccountGroup)
	}
}

func TestStartConnectPrefillsOpenAIOAuthImport(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	service.openAIImports = map[string]oauthConnectImport{
		"oauth_test": {
			Label:               "OpenAI user@example.com",
			AccessToken:         "access-token",
			RefreshToken:        "refresh-token",
			CredentialExpiresAt: &expiresAt,
			Backup:              true,
			Priority:            100,
			Weight:              100,
			CredentialMode:      providers.OpenAIOAuthModeValue(),
			TokenSource:         providers.OpenAIDeviceCodeTokenSourceValue(),
			OAuthAccountID:      "acct_123",
			OAuthEmail:          "user@example.com",
			ExpiresAt:           time.Now().UTC().Add(10 * time.Minute),
		},
	}

	start, err := service.StartConnect(core.ProviderOpenAI, "oauth_test")
	if err != nil {
		t.Fatalf("StartConnect returned error: %v", err)
	}
	if start.ProviderLabel != "OpenAI" {
		t.Fatalf("provider label = %q", start.ProviderLabel)
	}
	if start.AccessToken != "access-token" || start.RefreshToken != "refresh-token" {
		t.Fatalf("prefill tokens were not loaded: %#v", start)
	}
	if start.CredentialMode != providers.OpenAIOAuthModeValue() || start.TokenSource != providers.OpenAIDeviceCodeTokenSourceValue() {
		t.Fatalf("prefill source = %q / %q", start.CredentialMode, start.TokenSource)
	}
	if start.ExpiresAtText != expiresAt.Format(time.RFC3339) {
		t.Fatalf("expires at = %q", start.ExpiresAtText)
	}
	if !start.Backup {
		t.Fatal("backup = false, want true")
	}
}

func TestUpdateAccountOAuthCredentialUpdatesExistingAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	cooldownUntil := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	oldExpiresAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := repo.UpsertAccount(core.Account{
		ID:               "acct_oauth",
		Provider:         core.ProviderOpenAI,
		Label:            "Existing OAuth",
		Group:            "Plus",
		ProxyURL:         "http://127.0.0.1:7890",
		Status:           core.AccountStatusExpired,
		Priority:         80,
		Weight:           60,
		Tags:             []string{"oauth", "chatgpt"},
		CooldownUntil:    &cooldownUntil,
		ConsecutiveFails: 4,
		TotalFails:       7,
		Credential: core.Credential{
			Mode:         providers.OpenAIOAuthModeValue(),
			AccessToken:  "old-access",
			RefreshToken: "old-refresh",
			ExpiresAt:    &oldExpiresAt,
			Metadata: map[string]string{
				"token_source":                          providers.OpenAIDeviceCodeTokenSourceValue(),
				"oauth_account_id":                      "old-account",
				"email":                                 "old@example.com",
				"account_login_method":                  "token",
				"account_type":                          "official",
				"api_key_quota_provider":                "gateway",
				core.AccountQuotaErrorMetadataKey:       "credential_expired: refresh token was already used",
				core.AccountQuotaErrorAtMetadataKey:     "2026-05-18T16:17:13Z",
				core.AccountQuotaErrorCodeMetadataKey:   "credential_expired",
				core.AccountQuotaErrorStatusMetadataKey: string(core.AccountStatusExpired),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	newExpiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	updated, err := service.updateAccountOAuthCredential("acct_oauth", core.ProviderOpenAI, oauthConnectImport{
		AccessToken:         "new-access",
		RefreshToken:        "new-refresh",
		CredentialExpiresAt: &newExpiresAt,
		CredentialMode:      providers.OpenAIOAuthModeValue(),
		TokenSource:         providers.OpenAIDeviceCodeTokenSourceValue(),
		OAuthAccountID:      "new-account",
		OAuthEmail:          "new@example.com",
	})
	if err != nil {
		t.Fatalf("updateAccountOAuthCredential returned error: %v", err)
	}
	if updated.ID != "acct_oauth" || updated.Label != "Existing OAuth" || updated.Group != "Plus" {
		t.Fatalf("updated account identity changed: %#v", updated)
	}
	if updated.Credential.AccessToken != "new-access" || updated.Credential.RefreshToken != "new-refresh" {
		t.Fatalf("tokens were not updated: %#v", updated.Credential)
	}
	if updated.Credential.ExpiresAt == nil || !updated.Credential.ExpiresAt.Equal(newExpiresAt) {
		t.Fatalf("expires_at = %#v, want %v", updated.Credential.ExpiresAt, newExpiresAt)
	}
	if updated.Status != core.AccountStatusActive || updated.CooldownUntil != nil || updated.ConsecutiveFails != 0 {
		t.Fatalf("runtime state was not recovered: status=%s cooldown=%#v fails=%d", updated.Status, updated.CooldownUntil, updated.ConsecutiveFails)
	}
	if updated.TotalFails != 7 {
		t.Fatalf("total fails = %d, want preserved 7", updated.TotalFails)
	}
	metadata := updated.Credential.Metadata
	if metadata["token_source"] != providers.OpenAIDeviceCodeTokenSourceValue() ||
		metadata["oauth_account_id"] != "new-account" ||
		metadata["email"] != "new@example.com" ||
		metadata["account_login_method"] != "token" ||
		metadata["account_type"] != "official" {
		t.Fatalf("metadata = %#v", metadata)
	}
	if metadata["api_key_quota_provider"] != "" ||
		metadata[core.AccountQuotaErrorMetadataKey] != "" ||
		metadata[core.AccountQuotaErrorAtMetadataKey] != "" ||
		metadata[core.AccountQuotaErrorCodeMetadataKey] != "" ||
		metadata[core.AccountQuotaErrorStatusMetadataKey] != "" {
		t.Fatalf("stale metadata was not cleared: %#v", metadata)
	}
}

func TestStartConnectFiltersHiddenAccountGroups(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_visible", Name: "Visible"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_hidden", Name: "Hidden", ShowInClientEditor: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	start, err := service.StartConnect(core.ProviderOpenAI, "")
	if err != nil {
		t.Fatalf("StartConnect returned error: %v", err)
	}
	if len(start.AvailableGroups) != 2 {
		t.Fatalf("available groups = %#v", start.AvailableGroups)
	}
	for _, group := range start.AvailableGroups {
		if group.Name == "Hidden" {
			t.Fatalf("hidden group leaked into start connect groups: %#v", start.AvailableGroups)
		}
	}
}

func TestStartConnectFiltersAccountGroupsByProviderType(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: core.DefaultAccountGroupID, Name: core.DefaultAccountGroupName, Type: core.AccountGroupTypeOpenAI}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_claude", Name: "Claude", Type: core.AccountGroupTypeClaude}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_mixed", Name: "Mixed", Type: core.AccountGroupTypeMixed}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{}))

	start, err := service.StartConnect(core.ProviderClaude, "", core.DefaultAccountGroupName)
	if err != nil {
		t.Fatalf("StartConnect returned error: %v", err)
	}
	if start.Group == core.DefaultAccountGroupName {
		t.Fatalf("start group = %q, want provider-compatible group", start.Group)
	}
	got := map[string]bool{}
	for _, group := range start.AvailableGroups {
		got[group.Name] = true
	}
	if got[core.DefaultAccountGroupName] || !got["Claude"] || !got["Mixed"] {
		t.Fatalf("available groups = %#v, want Claude and Mixed only", got)
	}
}

func TestStartConnectDisablesOpenAIAPIKeyQuotaByDefault(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	start, err := service.StartConnect(core.ProviderOpenAI, "")
	if err != nil {
		t.Fatalf("StartConnect returned error: %v", err)
	}
	if start.APIKeyQuotaProvider != "" {
		t.Fatalf("api key quota provider = %q, want empty", start.APIKeyQuotaProvider)
	}
}

func TestCompleteManualConnectLeavesOpenAIAPIKeyQuotaDisabledByDefault(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	if err := service.CompleteManualConnect(ManualConnectInput{
		Provider:    core.ProviderOpenAI,
		Label:       "OpenAI API Key",
		Group:       core.DefaultAccountGroupName,
		AccessToken: "sk-test",
		Priority:    100,
		Weight:      100,
	}); err != nil {
		t.Fatalf("CompleteManualConnect returned error: %v", err)
	}
	accounts := repo.ListAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	if got := accounts[0].Credential.Metadata["api_key_quota_provider"]; got != "" {
		t.Fatalf("api_key_quota_provider = %q, want empty", got)
	}
}

func TestCompleteManualConnectStoresClaudeAPIKeyQuotaProvider(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.ClaudeAdapter{}))

	if err := service.CompleteManualConnect(ManualConnectInput{
		Provider:            core.ProviderClaude,
		Label:               "Claude API Key",
		Group:               core.DefaultAccountGroupName,
		AccessToken:         "claude-key",
		BaseURL:             "https://claude-gateway.example",
		APIKeyQuotaProvider: "sub2api",
		Priority:            100,
		Weight:              100,
	}); err != nil {
		t.Fatalf("CompleteManualConnect returned error: %v", err)
	}
	accounts := repo.ListAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	metadata := accounts[0].Credential.Metadata
	if metadata["api_key_quota_provider"] != "sub2api" || metadata["account_login_method"] != "api_key" {
		t.Fatalf("metadata = %#v", metadata)
	}
	if !service.SupportsQuotaRefresh(accounts[0]) {
		t.Fatal("Claude API key quota refresh should be supported once quota provider and base URL are configured")
	}
}

func TestAccountPoolImportExportPreservesBackupFlag(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_backup_export",
		Provider: core.ProviderOpenAI,
		Label:    "Backup Export",
		Group:    core.DefaultAccountGroupName,
		Status:   core.AccountStatusActive,
		Backup:   true,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "token",
		},
	}); err != nil {
		t.Fatal(err)
	}

	exported := service.ExportAccountPool(AccountPoolExportInput{AllGroups: true})
	if len(exported.Accounts) != 1 || !exported.Accounts[0].Backup {
		t.Fatalf("exported accounts = %#v, want backup flag preserved", exported.Accounts)
	}
	payload, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}

	importRepo := storage.NewMemoryRepository()
	importService := New(importRepo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	result, err := importService.ImportAccountPoolPayload(payload, AccountPoolImportOptions{})
	if err != nil {
		t.Fatalf("ImportAccountPoolPayload returned error: %v", err)
	}
	if result.Imported != 1 || result.Failed != 0 {
		t.Fatalf("import result = %#v", result)
	}
	imported, err := importRepo.GetAccount("acct_backup_export")
	if err != nil {
		t.Fatal(err)
	}
	if !imported.Backup {
		t.Fatalf("imported account = %#v, want backup flag preserved", imported)
	}
}

func TestImportCodexOpenAIAuthCreatesConnectImport(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	root := t.TempDir()
	authPath := filepath.Join(root, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{
  "auth_mode": "chatgpt",
  "tokens": {
    "access_token": "header.payload.signature",
    "refresh_token": "refresh-token",
    "id_token": "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJleHAiOjQxMDI0NDQ4MDAsImVtYWlsIjoidXNlckBleGFtcGxlLmNvbSIsImh0dHBzOi8vYXBpLm9wZW5haS5jb20vYXV0aCI6eyJjaGF0Z3B0X2FjY291bnRfaWQiOiJvcmdfY29kZXgifX0.c2ln",
    "account_id": "org_codex"
  }
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := service.ImportCodexOpenAIAuthPayload(authPath, mustReadFile(t, authPath))
	if err != nil {
		t.Fatalf("ImportCodexOpenAIAuthPayload returned error: %v", err)
	}
	if result.Label != "OpenAI user@example.com" {
		t.Fatalf("label = %q", result.Label)
	}

	start, err := service.StartConnect(core.ProviderOpenAI, result.ImportID)
	if err != nil {
		t.Fatalf("StartConnect returned error: %v", err)
	}
	if start.TokenSource != providers.OpenAICodexAuthTokenSourceValue() {
		t.Fatalf("token source = %q", start.TokenSource)
	}
	if start.CredentialMode != providers.OpenAIOAuthModeValue() {
		t.Fatalf("credential mode = %q", start.CredentialMode)
	}
	if start.RefreshToken != "refresh-token" || start.OAuthAccountID != "org_codex" || start.OAuthEmail != "user@example.com" {
		t.Fatalf("start = %#v", start)
	}
	if start.CodexAuthPath != authPath {
		t.Fatalf("codex auth path = %q, want %q", start.CodexAuthPath, authPath)
	}
	if start.ExpiresAtText == "" {
		t.Fatal("expected expires_at to be derived from JWT exp")
	}
	credentialExpiresAt, err := time.Parse(time.RFC3339, start.ExpiresAtText)
	if err != nil {
		t.Fatalf("parse start expires_at: %v", err)
	}

	if err := service.CompleteManualConnect(ManualConnectInput{
		Provider:       core.ProviderOpenAI,
		Label:          start.Label,
		Group:          start.Group,
		AccessToken:    start.AccessToken,
		RefreshToken:   start.RefreshToken,
		ExpiresAt:      &credentialExpiresAt,
		Priority:       start.Priority,
		Weight:         start.Weight,
		CredentialMode: start.CredentialMode,
		TokenSource:    start.TokenSource,
		OAuthAccountID: start.OAuthAccountID,
		OAuthEmail:     start.OAuthEmail,
		CodexAuthPath:  start.CodexAuthPath,
	}); err != nil {
		t.Fatalf("CompleteManualConnect returned error: %v", err)
	}
	accounts := repo.ListAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	if got := accounts[0].Credential.Metadata[providers.OpenAICodexAuthPathMetadataKey]; got != authPath {
		t.Fatalf("stored codex auth path = %q, want %q", got, authPath)
	}
}

func TestImportCodexOpenAIAuthSupportsAPIKeyMode(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	root := t.TempDir()
	authPath := filepath.Join(root, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{
  "auth_mode": "api_key",
  "OPENAI_API_KEY": "sk-codex-api-key"
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := service.ImportCodexOpenAIAuthPayload(authPath, mustReadFile(t, authPath))
	if err != nil {
		t.Fatalf("ImportCodexOpenAIAuthPayload returned error: %v", err)
	}
	if result.Label != "OpenAI API key from Codex" {
		t.Fatalf("label = %q", result.Label)
	}

	start, err := service.StartConnect(core.ProviderOpenAI, result.ImportID)
	if err != nil {
		t.Fatalf("StartConnect returned error: %v", err)
	}
	if start.TokenSource != "" {
		t.Fatalf("token source = %q, want empty", start.TokenSource)
	}
	if start.CredentialMode != "manual-token" {
		t.Fatalf("credential mode = %q", start.CredentialMode)
	}
	if start.AccessToken != "sk-codex-api-key" {
		t.Fatalf("access token = %q", start.AccessToken)
	}
	if start.CodexAuthPath != authPath {
		t.Fatalf("codex auth path = %q, want %q", start.CodexAuthPath, authPath)
	}
}

func TestImportCodexOpenAIAuthSupportsFlatExport(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	root := t.TempDir()
	authPath := filepath.Join(root, "codex-user@example.com.json")
	if err := os.WriteFile(authPath, []byte(`{
  "type": "codex",
  "email": "flat@example.com",
  "account_id": "acct_flat",
  "access_token": "header.payload.signature",
  "refresh_token": "refresh-token",
  "id_token": "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJleHAiOjQxMDI0NDQ4MDAsImVtYWlsIjoiZmxhdEBleGFtcGxlLmNvbSJ9.c2ln"
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := service.ImportCodexOpenAIAuthPayload(authPath, mustReadFile(t, authPath))
	if err != nil {
		t.Fatalf("ImportCodexOpenAIAuthPayload returned error: %v", err)
	}
	start, err := service.StartConnect(core.ProviderOpenAI, result.ImportID)
	if err != nil {
		t.Fatalf("StartConnect returned error: %v", err)
	}
	if start.OAuthEmail != "flat@example.com" || start.OAuthAccountID != "acct_flat" || start.RefreshToken != "refresh-token" {
		t.Fatalf("start = %#v", start)
	}
	if err := service.CompleteManualConnect(ManualConnectInput{
		Provider:       core.ProviderOpenAI,
		Label:          start.Label,
		Group:          start.Group,
		AccessToken:    start.AccessToken,
		RefreshToken:   start.RefreshToken,
		CredentialMode: start.CredentialMode,
		TokenSource:    start.TokenSource,
		OAuthAccountID: start.OAuthAccountID,
		OAuthEmail:     start.OAuthEmail,
		CodexAuthPath:  start.CodexAuthPath,
	}); err != nil {
		t.Fatalf("CompleteManualConnect returned error: %v", err)
	}
	account := repo.ListAccounts()[0]
	if account.Credential.Metadata["account_login_method"] != "token" || account.Credential.Metadata["account_type"] != "free" || account.Credential.Metadata["oauth_account_id"] != "acct_flat" {
		t.Fatalf("metadata = %#v", account.Credential.Metadata)
	}
}

func TestImportCodexOpenAIAuthSupportsFlatExportWithoutRefreshToken(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	root := t.TempDir()
	authPath := filepath.Join(root, "codex-session-only@example.com.json")
	if err := os.WriteFile(authPath, []byte(`{
  "type": "codex",
  "email": "session-only@example.com",
  "account_id": "acct_session_only",
  "access_token": "header.payload.signature",
  "refresh_token": "",
  "session_token": "session-token",
  "expired": "2030-01-02T03:04:05Z",
  "id_token": "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJlbWFpbCI6InNlc3Npb24tb25seUBleGFtcGxlLmNvbSJ9.c2ln"
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := service.ImportCodexOpenAIAuthPayload(authPath, mustReadFile(t, authPath))
	if err != nil {
		t.Fatalf("ImportCodexOpenAIAuthPayload returned error: %v", err)
	}
	start, err := service.StartConnect(core.ProviderOpenAI, result.ImportID)
	if err != nil {
		t.Fatalf("StartConnect returned error: %v", err)
	}
	if start.OAuthEmail != "session-only@example.com" || start.OAuthAccountID != "acct_session_only" {
		t.Fatalf("start identity = %#v", start)
	}
	if start.RefreshToken != "" || start.SessionToken != "session-token" {
		t.Fatalf("start tokens = %#v", start)
	}
	if start.ExpiresAtText != "2030-01-02T03:04:05Z" {
		t.Fatalf("expires at = %q", start.ExpiresAtText)
	}
	expiresAt, err := time.Parse(time.RFC3339, start.ExpiresAtText)
	if err != nil {
		t.Fatalf("parse expires at: %v", err)
	}
	if err := service.CompleteManualConnect(ManualConnectInput{
		Provider:       core.ProviderOpenAI,
		Label:          start.Label,
		Group:          start.Group,
		AccessToken:    start.AccessToken,
		RefreshToken:   start.RefreshToken,
		SessionToken:   start.SessionToken,
		ExpiresAt:      &expiresAt,
		CredentialMode: start.CredentialMode,
		TokenSource:    start.TokenSource,
		OAuthAccountID: start.OAuthAccountID,
		OAuthEmail:     start.OAuthEmail,
		CodexAuthPath:  start.CodexAuthPath,
	}); err != nil {
		t.Fatalf("CompleteManualConnect returned error: %v", err)
	}
	account := repo.ListAccounts()[0]
	if account.Credential.RefreshToken != "" || account.Credential.SessionToken != "session-token" {
		t.Fatalf("stored credential = %#v", account.Credential)
	}
	if account.Credential.ExpiresAt == nil || !account.Credential.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("stored expires at = %#v", account.Credential.ExpiresAt)
	}
}

func TestImportCodexOpenAIAuthUploadsDedupesByEmailForTeamAccounts(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if err := service.SeedDefaults(); err != nil {
		t.Fatal(err)
	}

	payload := []byte(`[
{
  "type": "codex",
  "email": "member-a@example.com",
  "account_id": "acct_team",
  "access_token": "header.payload.signature",
  "refresh_token": "refresh-a",
  "id_token": "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJleHAiOjQxMDI0NDQ4MDB9.c2ln"
},
{
  "type": "codex",
  "email": "member-b@example.com",
  "account_id": "acct_team",
  "access_token": "header.payload.signature",
  "refresh_token": "refresh-b",
  "id_token": "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJleHAiOjQxMDI0NDQ4MDB9.c2ln"
}
]`)
	result, err := service.ImportCodexOpenAIAuthUploads([]CodexOpenAIAuthUpload{{Name: "team.json", Payload: payload}}, core.DefaultAccountGroupName)
	if err != nil {
		t.Fatalf("ImportCodexOpenAIAuthUploads returned error: %v", err)
	}
	if result.Imported != 2 || result.Skipped != 0 || result.Failed != 0 {
		t.Fatalf("result = imported:%d skipped:%d failed:%d items:%#v", result.Imported, result.Skipped, result.Failed, result.Items)
	}

	accounts := repo.ListAccounts()
	if len(accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(accounts))
	}
	seenEmails := map[string]bool{}
	for _, account := range accounts {
		if got := account.Credential.Metadata["oauth_account_id"]; got != "acct_team" {
			t.Fatalf("account %s oauth_account_id = %q, want acct_team", account.ID, got)
		}
		seenEmails[account.Credential.Metadata["email"]] = true
	}
	if !seenEmails["member-a@example.com"] || !seenEmails["member-b@example.com"] {
		t.Fatalf("seen emails = %#v", seenEmails)
	}

	duplicate := []byte(`{
  "type": "codex",
  "email": "member-a@example.com",
  "account_id": "acct_other",
  "access_token": "header.payload.signature",
  "refresh_token": "refresh-duplicate",
  "id_token": "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJleHAiOjQxMDI0NDQ4MDB9.c2ln"
}`)
	result, err = service.ImportCodexOpenAIAuthUploads([]CodexOpenAIAuthUpload{{Name: "duplicate.json", Payload: duplicate}}, core.DefaultAccountGroupName)
	if err != nil {
		t.Fatalf("ImportCodexOpenAIAuthUploads duplicate returned error: %v", err)
	}
	if result.Imported != 0 || result.Skipped != 1 || result.Failed != 0 {
		t.Fatalf("duplicate result = imported:%d skipped:%d failed:%d items:%#v", result.Imported, result.Skipped, result.Failed, result.Items)
	}
}

func TestOAuthImportLabel(t *testing.T) {
	tests := []struct {
		name      string
		provider  core.ProviderKind
		email     string
		accountID string
		want      string
	}{
		{
			name:     "openai email",
			provider: core.ProviderOpenAI,
			email:    "user@example.com",
			want:     "OpenAI user@example.com",
		},
		{
			name:      "claude account id fallback",
			provider:  core.ProviderClaude,
			accountID: "user_123456789",
			want:      "Claude OAuth user_123",
		},
		{
			name:     "provider fallback",
			provider: core.ProviderClaude,
			want:     "Claude OAuth",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := oauthImportLabel(tt.provider, tt.email, tt.accountID)
			if got != tt.want {
				t.Fatalf("oauthImportLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStartConnectPrefillsClaudeOAuthImport(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.ClaudeAdapter{}))

	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	service.claudeImports = map[string]oauthConnectImport{
		"oauth_test": {
			Label:               "Claude user@example.com",
			AccessToken:         "claude-access-token",
			RefreshToken:        "claude-refresh-token",
			CredentialExpiresAt: &expiresAt,
			Priority:            100,
			Weight:              100,
			CredentialMode:      providers.ClaudeOAuthModeValue(),
			TokenSource:         providers.ClaudeOAuthTokenSourceValue(),
			OAuthAccountID:      "user_123",
			OAuthEmail:          "user@example.com",
			ExpiresAt:           time.Now().UTC().Add(10 * time.Minute),
		},
	}

	start, err := service.StartConnect(core.ProviderClaude, "oauth_test")
	if err != nil {
		t.Fatalf("StartConnect returned error: %v", err)
	}
	if start.ProviderLabel != "Claude" {
		t.Fatalf("provider label = %q", start.ProviderLabel)
	}
	if start.AccessToken != "claude-access-token" || start.RefreshToken != "claude-refresh-token" {
		t.Fatalf("prefill tokens were not loaded: %#v", start)
	}
	if start.CredentialMode != providers.ClaudeOAuthModeValue() || start.TokenSource != providers.ClaudeOAuthTokenSourceValue() {
		t.Fatalf("prefill source = %q / %q", start.CredentialMode, start.TokenSource)
	}
	if start.ExpiresAtText != expiresAt.Format(time.RFC3339) {
		t.Fatalf("expires at = %q", start.ExpiresAtText)
	}
}

func TestDeleteAccountKeepsClientGroupBinding(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{ID: "acct_a", Provider: core.ProviderOpenAI, Label: "A", Group: "Plus", Status: core.AccountStatusActive},
		{ID: "acct_b", Provider: core.ProviderOpenAI, Label: "B", Group: "Plus", Status: core.AccountStatusActive},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:           "client_a",
		Name:         "Client A",
		APIKey:       "gw_key",
		Enabled:      true,
		AccountGroup: "Plus",
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	deleted, err := service.DeleteAccount("acct_a")
	if err != nil {
		t.Fatalf("DeleteAccount returned error: %v", err)
	}
	if deleted.ID != "acct_a" {
		t.Fatalf("deleted id = %q", deleted.ID)
	}
	if _, err := repo.GetAccount("acct_a"); err == nil {
		t.Fatal("expected account to be deleted")
	}

	client, err := repo.GetClient("client_a")
	if err != nil {
		t.Fatal(err)
	}
	if client.AccountGroup != "Plus" {
		t.Fatalf("account group = %q", client.AccountGroup)
	}
}

func TestDashboardAndHealthReportAggregateProviderHealth(t *testing.T) {
	repo := storage.NewMemoryRepository()
	now := time.Now().UTC().Truncate(time.Second)
	openAIExpiresAt := now.Add(10 * time.Minute)
	openAILastUsed := now.Add(-2 * time.Minute)
	claudeLastUsed := now.Add(-1 * time.Minute)
	cooldownUntil := now.Add(5 * time.Minute)

	for _, account := range []core.Account{
		{
			ID:       "acct_openai_active",
			Provider: core.ProviderOpenAI,
			Label:    "OpenAI Active",
			Status:   core.AccountStatusActive,
			Credential: core.Credential{
				AccessToken: "token-openai-active",
				ExpiresAt:   &openAIExpiresAt,
			},
			LastUsedAt: &openAILastUsed,
		},
		{
			ID:            "acct_openai_cooling",
			Provider:      core.ProviderOpenAI,
			Label:         "OpenAI Cooling",
			Status:        core.AccountStatusCooling,
			CooldownUntil: &cooldownUntil,
		},
		{
			ID:               "acct_claude_blocked",
			Provider:         core.ProviderClaude,
			Label:            "Claude Blocked",
			Status:           core.AccountStatusBlocked,
			LastUsedAt:       &claudeLastUsed,
			ConsecutiveFails: 2,
			TotalFails:       4,
		},
		{
			ID:       "acct_claude_provider_banned",
			Provider: core.ProviderClaude,
			Label:    "Claude Provider Banned",
			Status:   core.AccountStatusProviderBanned,
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{}))
	dashboard := service.Dashboard(context.Background())

	if dashboard.Stats.TotalAccounts != 4 {
		t.Fatalf("total accounts = %d, want 4", dashboard.Stats.TotalAccounts)
	}
	if dashboard.Stats.AvailableAccounts != 1 {
		t.Fatalf("available accounts = %d, want 1", dashboard.Stats.AvailableAccounts)
	}
	if dashboard.Stats.CoolingCount != 1 {
		t.Fatalf("cooling count = %d, want 1", dashboard.Stats.CoolingCount)
	}
	if dashboard.Stats.FailureCount != 2 {
		t.Fatalf("failure count = %d, want 2", dashboard.Stats.FailureCount)
	}
	if dashboard.Stats.ExpiringSoonCount != 1 {
		t.Fatalf("expiring soon count = %d, want 1", dashboard.Stats.ExpiringSoonCount)
	}
	if dashboard.Stats.HealthyProviderCount != 1 {
		t.Fatalf("healthy providers = %d, want 1", dashboard.Stats.HealthyProviderCount)
	}
	if dashboard.Stats.DegradedProviderCount != 1 {
		t.Fatalf("degraded providers = %d, want 1", dashboard.Stats.DegradedProviderCount)
	}
	if dashboard.Stats.Status != "degraded" {
		t.Fatalf("status = %q, want degraded", dashboard.Stats.Status)
	}
	if dashboard.Stats.Reason != "one or more providers have no available accounts" {
		t.Fatalf("reason = %q", dashboard.Stats.Reason)
	}
	if len(dashboard.Providers) != 2 {
		t.Fatalf("provider summaries = %d, want 2", len(dashboard.Providers))
	}

	summaries := map[core.ProviderKind]ProviderSummary{}
	for _, summary := range dashboard.Providers {
		summaries[summary.Kind] = summary
	}

	openAISummary := summaries[core.ProviderOpenAI]
	if openAISummary.TotalAccounts != 2 || openAISummary.AvailableAccounts != 1 || openAISummary.ActiveAccounts != 1 || openAISummary.CoolingAccounts != 1 {
		t.Fatalf("unexpected openai summary: %#v", openAISummary)
	}
	if openAISummary.LastUsedAt == nil || !openAISummary.LastUsedAt.Equal(openAILastUsed) {
		t.Fatalf("unexpected openai last used: %#v", openAISummary.LastUsedAt)
	}

	claudeSummary := summaries[core.ProviderClaude]
	if claudeSummary.TotalAccounts != 2 || claudeSummary.AvailableAccounts != 0 || claudeSummary.BlockedAccounts != 1 || claudeSummary.ProviderBannedAccounts != 1 {
		t.Fatalf("unexpected claude summary: %#v", claudeSummary)
	}
	if claudeSummary.LastUsedAt == nil || !claudeSummary.LastUsedAt.Equal(claudeLastUsed) {
		t.Fatalf("unexpected claude last used: %#v", claudeSummary.LastUsedAt)
	}

	report := service.HealthReport(context.Background())
	if report.Status != dashboard.Stats.Status || report.Reason != dashboard.Stats.Reason {
		t.Fatalf("unexpected report: %#v", report)
	}
	if report.GeneratedAt.IsZero() {
		t.Fatal("expected generated timestamp")
	}
	if len(report.Providers) != len(dashboard.Providers) {
		t.Fatalf("provider count = %d, want %d", len(report.Providers), len(dashboard.Providers))
	}
}

func TestDashboardListAccountsAndHealthReportNormalizeExpiredCooldown(t *testing.T) {
	repo := storage.NewMemoryRepository()
	expiredCooldown := time.Now().UTC().Add(-time.Minute)
	if err := repo.UpsertAccount(core.Account{
		ID:               "acct_expired_cooldown",
		Provider:         core.ProviderOpenAI,
		Label:            "Expired Cooldown",
		Status:           core.AccountStatusCooling,
		CooldownUntil:    &expiredCooldown,
		ConsecutiveFails: 1,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	accounts := service.ListAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	if accounts[0].Status != core.AccountStatusActive || accounts[0].CooldownUntil != nil {
		t.Fatalf("normalized account = %#v, want active without cooldown", accounts[0])
	}

	dashboard := service.Dashboard(context.Background())
	if dashboard.Accounts[0].Status != core.AccountStatusActive || dashboard.Accounts[0].CooldownUntil != nil {
		t.Fatalf("dashboard account = %#v, want active without cooldown", dashboard.Accounts[0])
	}
	if dashboard.Stats.AvailableAccounts != 1 || dashboard.Stats.CoolingCount != 0 || dashboard.Stats.Status != "ok" {
		t.Fatalf("dashboard stats = %#v, want one available active account", dashboard.Stats)
	}
	if len(dashboard.Providers) != 1 || dashboard.Providers[0].AvailableAccounts != 1 || dashboard.Providers[0].CoolingAccounts != 0 {
		t.Fatalf("provider summary = %#v, want available active account", dashboard.Providers)
	}

	report := service.HealthReport(context.Background())
	if report.AvailableAccounts != 1 || report.Status != "ok" {
		t.Fatalf("health report = %#v, want ok with one available account", report)
	}
}

func TestDashboardTreatsQuotaLimitAsUnavailableRuntimeStatus(t *testing.T) {
	repo := storage.NewMemoryRepository()
	reset := time.Now().UTC().Add(time.Hour)
	if err := repo.UpsertAccount(accountWithQuotaSnapshot(t, core.Account{
		ID:       "acct_time_limit",
		Provider: core.ProviderOpenAI,
		Label:    "Time Limit",
		Status:   core.AccountStatusActive,
	}, core.AccountQuotaSnapshot{Primary: &core.AccountQuotaWindow{UsedPercent: 100, ResetsAt: &reset}})); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(accountWithQuotaSnapshot(t, core.Account{
		ID:       "acct_week_limit",
		Provider: core.ProviderOpenAI,
		Label:    "Week Limit",
		Status:   core.AccountStatusActive,
	}, core.AccountQuotaSnapshot{Secondary: &core.AccountQuotaWindow{UsedPercent: 100, ResetsAt: &reset}})); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	dashboard := service.Dashboard(context.Background())
	if dashboard.Stats.AvailableAccounts != 0 || dashboard.Stats.Status != "error" {
		t.Fatalf("dashboard stats = %#v, want no available accounts", dashboard.Stats)
	}
	if len(dashboard.Providers) != 1 || dashboard.Providers[0].AvailableAccounts != 0 {
		t.Fatalf("provider summary = %#v, want provider unavailable", dashboard.Providers)
	}
	if got := AccountFilterStatus(dashboard.Accounts[0]); got != core.AccountRuntimeStatusTimeLimit {
		t.Fatalf("first filter status = %q, want time limit", got)
	}
	if got := AccountFilterStatus(dashboard.Accounts[1]); got != core.AccountRuntimeStatusWeekLimit {
		t.Fatalf("second filter status = %q, want week limit", got)
	}
}

func TestDashboardTreatsScopedRateLimitAsCooling(t *testing.T) {
	repo := storage.NewMemoryRepository()
	reset := time.Now().UTC().Add(time.Hour)
	if err := repo.UpsertAccount(accountWithQuotaSnapshot(t, core.Account{
		ID:       "acct_runtime_limited",
		Provider: core.ProviderOpenAI,
		Label:    "Runtime Limited",
		Status:   core.AccountStatusActive,
	}, core.AccountQuotaSnapshot{
		Additional: map[string]core.AccountQuotaSnapshot{
			core.AccountQuotaRuntimeChatLimitID: {
				Source:      core.AccountQuotaRuntimeSource,
				LimitID:     core.AccountQuotaRuntimeChatLimitID,
				Primary:     &core.AccountQuotaWindow{Name: "rate_limit", UsedPercent: 100, ResetsAt: &reset},
				ReachedType: "primary_window",
			},
		},
	})); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	dashboard := service.Dashboard(context.Background())
	if dashboard.Stats.AvailableAccounts != 0 || dashboard.Stats.CoolingCount != 1 || dashboard.Stats.Status != "error" {
		t.Fatalf("dashboard stats = %#v, want one cooling unavailable account", dashboard.Stats)
	}
	if len(dashboard.Providers) != 1 ||
		dashboard.Providers[0].AvailableAccounts != 0 ||
		dashboard.Providers[0].ActiveAccounts != 0 ||
		dashboard.Providers[0].CoolingAccounts != 1 {
		t.Fatalf("provider summary = %#v, want one cooling unavailable account", dashboard.Providers)
	}
	if got := AccountFilterStatus(dashboard.Accounts[0]); got != "cooling" {
		t.Fatalf("filter status = %q, want cooling", got)
	}
}

func TestDashboardKeepsTextAvailabilityForScopedImageRateLimit(t *testing.T) {
	repo := storage.NewMemoryRepository()
	reset := time.Now().UTC().Add(time.Hour)
	if err := repo.UpsertAccount(accountWithQuotaSnapshot(t, core.Account{
		ID:       "acct_runtime_image_limited",
		Provider: core.ProviderOpenAI,
		Label:    "Runtime Image Limited",
		Status:   core.AccountStatusActive,
	}, core.AccountQuotaSnapshot{
		Additional: map[string]core.AccountQuotaSnapshot{
			core.AccountQuotaRuntimeImageLimitID: {
				Source:  core.AccountQuotaRuntimeSource,
				LimitID: core.AccountQuotaRuntimeImageLimitID,
				Image:   &core.AccountImageQuota{Remaining: 0, ResetsAt: &reset},
			},
		},
	})); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	dashboard := service.Dashboard(context.Background())
	if dashboard.Stats.AvailableAccounts != 1 || dashboard.Stats.CoolingCount != 1 || dashboard.Stats.Status != "ok" {
		t.Fatalf("dashboard stats = %#v, want text-available image-cooling account", dashboard.Stats)
	}
	if len(dashboard.Providers) != 1 ||
		dashboard.Providers[0].AvailableAccounts != 1 ||
		dashboard.Providers[0].ActiveAccounts != 0 ||
		dashboard.Providers[0].CoolingAccounts != 1 {
		t.Fatalf("provider summary = %#v, want text-available image-cooling account", dashboard.Providers)
	}
	if got := AccountFilterStatus(dashboard.Accounts[0]); got != "cooling" {
		t.Fatalf("filter status = %q, want cooling", got)
	}
}

func TestControlDisabledAccountKeepsRuntimeStatusAndIsUnavailable(t *testing.T) {
	repo := storage.NewMemoryRepository()
	reset := time.Now().UTC().Add(time.Hour)
	if err := repo.UpsertAccount(accountWithQuotaSnapshot(t, core.Account{
		ID:              "acct_disabled_limited",
		Provider:        core.ProviderOpenAI,
		Label:           "Disabled Limited",
		Status:          core.AccountStatusActive,
		ControlDisabled: true,
	}, core.AccountQuotaSnapshot{Primary: &core.AccountQuotaWindow{UsedPercent: 100, ResetsAt: &reset}})); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	dashboard := service.Dashboard(context.Background())
	if dashboard.Stats.AvailableAccounts != 0 || dashboard.Stats.Status != "error" {
		t.Fatalf("dashboard stats = %#v, want unavailable account", dashboard.Stats)
	}
	if got := AccountRuntimeStatus(dashboard.Accounts[0]); got != core.AccountRuntimeStatusTimeLimit {
		t.Fatalf("runtime status = %q, want time limit", got)
	}
	if got := AccountFilterStatus(dashboard.Accounts[0]); got != core.AccountRuntimeStatusTimeLimit {
		t.Fatalf("filter status = %q, want time limit", got)
	}
	if !dashboard.Accounts[0].ControlDisabled {
		t.Fatalf("account = %#v, want control disabled preserved", dashboard.Accounts[0])
	}
}

func TestListAccountsNormalizesStaleRefreshing(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_stale_refreshing",
		Provider: core.ProviderOpenAI,
		Label:    "Stale Refreshing",
		Status:   core.AccountStatusRefreshing,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	accounts := service.ListAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	if accounts[0].Status != core.AccountStatusActive {
		t.Fatalf("status = %q, want active", accounts[0].Status)
	}
	if got := AccountRuntimeStatus(accounts[0]); got != string(core.AccountStatusActive) {
		t.Fatalf("runtime status = %q, want active", got)
	}
}

func TestDashboardIgnoresUnconfiguredProvidersInHealthCounts(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_openai_only",
		Provider: core.ProviderOpenAI,
		Label:    "Only OpenAI",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			AccessToken: "token-openai",
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{}))
	dashboard := service.Dashboard(context.Background())

	if len(dashboard.Providers) != 1 {
		t.Fatalf("provider summaries = %d, want 1", len(dashboard.Providers))
	}
	if dashboard.Providers[0].Kind != core.ProviderOpenAI {
		t.Fatalf("provider = %q, want %q", dashboard.Providers[0].Kind, core.ProviderOpenAI)
	}
	if dashboard.Stats.DegradedProviderCount != 0 {
		t.Fatalf("degraded providers = %d, want 0", dashboard.Stats.DegradedProviderCount)
	}
	if dashboard.Stats.Status != "ok" {
		t.Fatalf("status = %q, want ok", dashboard.Stats.Status)
	}
}

func TestDashboardWithNoAccountsUsesSetupStatus(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{}))

	dashboard := service.Dashboard(context.Background())
	if dashboard.Stats.TotalAccounts != 0 || dashboard.Stats.AvailableAccounts != 0 {
		t.Fatalf("unexpected counts: %#v", dashboard.Stats)
	}
	if dashboard.Stats.Status != "setup" {
		t.Fatalf("status = %q, want setup", dashboard.Stats.Status)
	}
	if dashboard.Stats.Reason != "no accounts configured" {
		t.Fatalf("reason = %q", dashboard.Stats.Reason)
	}

	report := service.HealthReport(context.Background())
	if report.Status != "setup" || report.Reason != "no accounts configured" {
		t.Fatalf("unexpected report: %#v", report)
	}
}

func TestUpdateSystemSettingsPersistsAndAppliesRuntimeValues(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{}))

	settings := core.DefaultSystemSettings()
	settings.Network.SystemProxyURL = " http://127.0.0.1:7890 "
	settings.Image.Backend = core.ImageBackendOfficial
	settings.OAuth.ClaudeEnabled = false
	settings.Runtime.UserConcurrentRequestLimit = 2
	settings.Runtime.PlanConcurrentRequestLimit = 4
	settings.Retention.AuditLimit = 1

	stored, err := service.UpdateSystemSettings(settings)
	if err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	if stored.Network.SystemProxyURL != "http://127.0.0.1:7890" {
		t.Fatalf("SystemProxyURL = %q", stored.Network.SystemProxyURL)
	}
	if service.SystemProxyURL() != "http://127.0.0.1:7890" {
		t.Fatalf("system proxy = %q", service.SystemProxyURL())
	}
	if stored.Image.Backend != core.ImageBackendOfficial {
		t.Fatalf("image settings = %#v", stored.Image)
	}
	if service.OAuthEnabled(core.ProviderClaude) {
		t.Fatalf("Claude OAuth should be disabled")
	}
	if stored.Runtime.UserConcurrentRequestLimit != 2 {
		t.Fatalf("UserConcurrentRequestLimit = %d, want 2", stored.Runtime.UserConcurrentRequestLimit)
	}
	if stored.Runtime.PlanConcurrentRequestLimit != 4 {
		t.Fatalf("PlanConcurrentRequestLimit = %d, want 4", stored.Runtime.PlanConcurrentRequestLimit)
	}

	if err := repo.AppendAudit(core.AuditEvent{ID: "audit_1", Kind: core.AuditKindAdmin, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("AppendAudit returned error: %v", err)
	}
	if err := repo.AppendAudit(core.AuditEvent{ID: "audit_2", Kind: core.AuditKindAdmin, CreatedAt: time.Now().UTC().Add(time.Millisecond)}); err != nil {
		t.Fatalf("AppendAudit returned error: %v", err)
	}
	audits := repo.ListAudit(10)
	if len(audits) != 1 || audits[0].ID != "audit_2" {
		t.Fatalf("audit retention = %#v, want only audit_2", audits)
	}
}

func TestUpdateSystemSettingsValidatesImageBackend(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	settings := core.DefaultSystemSettings()
	settings.Image.Backend = "unsupported"
	if _, err := service.UpdateSystemSettings(settings); err == nil || !strings.Contains(err.Error(), "unsupported image backend") {
		t.Fatalf("UpdateSystemSettings err = %v, want unsupported image backend error", err)
	}
}

func TestUpdateSystemSettingsValidatesEnabledPaymentSecrets(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	settings := core.DefaultSystemSettings()
	settings.Payment.MinRechargeNanoUSD = 5 * core.NanoUSDPerUSD
	settings.Payment.MaxRechargeNanoUSD = 4 * core.NanoUSDPerUSD
	if _, err := service.UpdateSystemSettings(settings); err == nil || !strings.Contains(err.Error(), "maximum recharge") {
		t.Fatalf("UpdateSystemSettings err = %v, want invalid recharge range", err)
	}

	settings = core.DefaultSystemSettings()
	settings.Payment.PersonalPay.Enabled = true
	if _, err := service.UpdateSystemSettings(settings); err == nil || !strings.Contains(err.Error(), "personalpay android token") {
		t.Fatalf("UpdateSystemSettings err = %v, want missing PersonalPay android token", err)
	}

	settings = core.DefaultSystemSettings()
	settings.Backup.AndroidAutoEnabled = true
	settings.Payment.PersonalPay.AndroidToken = "android-token"
	if _, err := service.UpdateSystemSettings(settings); err == nil || !strings.Contains(err.Error(), "PersonalPay to be enabled") {
		t.Fatalf("UpdateSystemSettings err = %v, want PersonalPay enabled error", err)
	}

	settings.Payment.PersonalPay.Enabled = true
	if _, err := service.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error for enabled PersonalPay backup: %v", err)
	}

	settings = core.DefaultSystemSettings()
	settings.Payment.WeChatPay.Enabled = true
	settings.Payment.WeChatPay.AppID = "wx-app"
	settings.Payment.WeChatPay.MchID = "merchant"
	settings.Payment.WeChatPay.APIV3Key = "0123456789abcdef0123456789abcdef"
	settings.Payment.WeChatPay.MerchantSerialNo = "serial"
	settings.Payment.WeChatPay.MerchantPrivateKeyPEM = "private-key"

	if _, err := service.UpdateSystemSettings(settings); err == nil || !strings.Contains(err.Error(), "wechat pay public key") {
		t.Fatalf("UpdateSystemSettings err = %v, want missing WeChat public key", err)
	}

	settings.Payment.WeChatPay.WeChatPayPublicKeyPEM = "public-key"
	if _, err := service.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error after required payment settings were filled: %v", err)
	}
}

func TestUpdateSystemSettingsValidatesRegistrationSecurity(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	settings := core.DefaultSystemSettings()
	settings.Registration.RequireInvitationCode = true
	if _, err := service.UpdateSystemSettings(settings); err == nil || !strings.Contains(err.Error(), "invitation registration") {
		t.Fatalf("UpdateSystemSettings err = %v, want invitation requirement error", err)
	}

	settings = core.DefaultSystemSettings()
	settings.Registration.TurnstileEnabled = true
	settings.Registration.TurnstileSiteKey = "site"
	if _, err := service.UpdateSystemSettings(settings); err == nil || !strings.Contains(err.Error(), "turnstile secret key") {
		t.Fatalf("UpdateSystemSettings err = %v, want Turnstile secret error", err)
	}
}

func TestEffectiveProxyUsesSystemProxyFallbackAndOverrides(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	settings := core.DefaultSystemSettings()
	settings.Network.SystemProxyURL = "http://127.0.0.1:18080"
	if _, err := service.UpdateSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_core", Name: "Core", ProxyURL: "http://127.0.0.1:7890"}); err != nil {
		t.Fatal(err)
	}

	if got := service.effectiveProxyURLForAccount(core.Account{}); got != "http://127.0.0.1:18080" {
		t.Fatalf("system fallback proxy = %q", got)
	}
	if got := service.effectiveProxyURLForAccount(core.Account{Group: "Core"}); got != "http://127.0.0.1:7890" {
		t.Fatalf("group proxy = %q", got)
	}
	if got := service.effectiveProxyURLForAccount(core.Account{Group: "Core", ProxyURL: "socks5://127.0.0.1:1080"}); got != "socks5://127.0.0.1:1080" {
		t.Fatalf("account proxy = %q", got)
	}
}

func TestApplySystemSettingsBootstrapsAuditLimitFallback(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	if err := service.ApplySystemSettings(17); err != nil {
		t.Fatalf("ApplySystemSettings returned error: %v", err)
	}
	settings, err := service.GetSystemSettings()
	if err != nil {
		t.Fatalf("GetSystemSettings returned error: %v", err)
	}
	if settings.Retention.AuditLimit != 17 {
		t.Fatalf("AuditLimit = %d, want 17", settings.Retention.AuditLimit)
	}
	if settings.UpdatedAt.IsZero() {
		t.Fatalf("UpdatedAt was not set")
	}
}

func TestUpdateSystemSettingsAppliesUsageLogRetentionDisable(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if err := repo.UpsertUser(core.User{ID: "user_usage_retention", Username: "usage", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_usage_retention", Name: "Usage", APIKey: "gw_usage_retention", OwnerUserID: "user_usage_retention", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	seedZeroCostUsageLogBillingRequest(t, repo, "req_usage_retention_disabled", "client_usage_retention", "user_usage_retention")

	settings := core.DefaultSystemSettings()
	settings.Retention.UsageLogMaxAgeDays = 0
	if _, err := service.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}

	items, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{UserID: "user_usage_retention", Limit: 10})
	if total != 0 || len(items) != 0 {
		t.Fatalf("usage logs total=%d rows=%#v, want disabled", total, items)
	}
}

func TestApplySystemSettingsBootstrapsPublicBaseURLFallback(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	if err := service.ApplySystemSettings(17, "https://example.com/"); err != nil {
		t.Fatalf("ApplySystemSettings returned error: %v", err)
	}
	settings, err := service.GetSystemSettings()
	if err != nil {
		t.Fatalf("GetSystemSettings returned error: %v", err)
	}
	if settings.Runtime.PublicBaseURL != "https://example.com" {
		t.Fatalf("PublicBaseURL = %q", settings.Runtime.PublicBaseURL)
	}
}

func TestApplySystemSettingsSyncsConfiguredPublicBaseURL(t *testing.T) {
	repo := storage.NewMemoryRepository()
	existing := core.DefaultSystemSettings()
	existing.Runtime.PublicBaseURL = "https://existing.example.com"
	if err := repo.UpsertSystemSettings(existing); err != nil {
		t.Fatalf("UpsertSystemSettings returned error: %v", err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	if err := service.ApplySystemSettings(17, "https://example.com"); err != nil {
		t.Fatalf("ApplySystemSettings returned error: %v", err)
	}
	settings, err := service.GetSystemSettings()
	if err != nil {
		t.Fatalf("GetSystemSettings returned error: %v", err)
	}
	if settings.Runtime.PublicBaseURL != "https://example.com" {
		t.Fatalf("PublicBaseURL = %q", settings.Runtime.PublicBaseURL)
	}
}

type auditDropCountRepository struct {
	*storage.MemoryRepository
	droppedAudit uint64
}

func (r auditDropCountRepository) DroppedAuditEvents() uint64 {
	return r.droppedAudit
}

func TestHealthReportExposesDroppedAsyncEvents(t *testing.T) {
	repo := auditDropCountRepository{MemoryRepository: storage.NewMemoryRepository(), droppedAudit: 7}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	report := service.HealthReport(context.Background())
	if report.DroppedAuditEvents != 7 {
		t.Fatalf("dropped audit events = %d, want 7", report.DroppedAuditEvents)
	}
	dashboard := service.Dashboard(context.Background())
	if dashboard.Stats.DroppedAuditEvents != 7 {
		t.Fatalf("dashboard dropped audit events = %d, want 7", dashboard.Stats.DroppedAuditEvents)
	}
}

func TestEnsureProtocolClientCreatesDefaultClient(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	client, created, err := service.EnsureProtocolClient("gw_seed_key")
	if err != nil {
		t.Fatalf("EnsureProtocolClient returned error: %v", err)
	}
	if !created {
		t.Fatal("expected client to be created")
	}
	if client.APIKey != "gw_seed_key" {
		t.Fatalf("api key = %q", client.APIKey)
	}

	clients := repo.ListClients()
	if len(clients) != 1 {
		t.Fatalf("len(clients) = %d, want 1", len(clients))
	}
	if clients[0].APIKey != "gw_seed_key" {
		t.Fatalf("api key = %q", clients[0].APIKey)
	}
}

func TestEnsureProtocolClientGeneratesKeyWhenSeedBlank(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	client, created, err := service.EnsureProtocolClient("")
	if err != nil {
		t.Fatalf("EnsureProtocolClient returned error: %v", err)
	}
	if !created {
		t.Fatal("expected client to be created")
	}
	if !strings.HasPrefix(client.APIKey, "sk-") {
		t.Fatalf("api key = %q", client.APIKey)
	}
}

func TestEnsureProtocolClientUsesPagedClientExistence(t *testing.T) {
	repo := &ensureProtocolFullClientListPanicRepository{MemoryRepository: storage.NewMemoryRepository()}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	client, created, err := service.EnsureProtocolClient("gw_seed_key")
	if err != nil {
		t.Fatalf("EnsureProtocolClient returned error: %v", err)
	}
	if created {
		t.Fatal("expected existing client to skip seeding")
	}
	if client.ID != "" {
		t.Fatalf("client = %#v, want empty client when skipped", client)
	}
}

type ensureProtocolFullClientListPanicRepository struct {
	*storage.MemoryRepository
}

func (r *ensureProtocolFullClientListPanicRepository) ListClients() []core.APIClient {
	panic("EnsureProtocolClient should use ListClientSummariesPage instead of full ListClients")
}

func (r *ensureProtocolFullClientListPanicRepository) ListClientSummariesPage(offset, limit int) ([]core.APIClient, int) {
	return []core.APIClient{{ID: "client_existing", Name: "Existing", Enabled: true}}, 1
}

func TestAuthorizeProtocolKeyReturnsEnabledClient(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertClient(core.APIClient{
		ID:      "client_a",
		Name:    "Client A",
		APIKey:  "gw_key",
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	client, err := service.AuthorizeProtocolKey("gw_key")
	if err != nil {
		t.Fatalf("AuthorizeProtocolKey returned error: %v", err)
	}
	if client.ID != "client_a" {
		t.Fatalf("client id = %q", client.ID)
	}
}

func TestAuthorizeProtocolKeyRecordsOwnerLastUsedAt(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := service.CreateUser(UserInput{
		Username: "alice",
		Password: "alice-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if user.LastLoginAt != nil {
		t.Fatalf("new user LastLoginAt = %#v, want nil", user.LastLoginAt)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:          "client_alice",
		Name:        "Alice Key",
		APIKey:      "gw_alice",
		OwnerUserID: user.ID,
		Enabled:     true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AuthorizeProtocolKey("bad_key"); err == nil {
		t.Fatal("expected invalid key to be rejected")
	}
	unchanged, err := repo.GetUser(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.LastLoginAt != nil {
		t.Fatalf("invalid key updated LastLoginAt: %#v", unchanged.LastLoginAt)
	}
	profileUpdatedAt := unchanged.UpdatedAt
	if _, err := service.AuthorizeProtocolKey("gw_alice"); err != nil {
		t.Fatalf("AuthorizeProtocolKey returned error: %v", err)
	}
	updated, err := repo.GetUser(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastLoginAt == nil {
		t.Fatal("AuthorizeProtocolKey should update owner LastLoginAt")
	}
	if !updated.UpdatedAt.Equal(profileUpdatedAt) {
		t.Fatal("API key use should not masquerade as a profile UpdatedAt change")
	}
}

func TestAuthorizeProtocolKeyRejectsDisabledClient(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertClient(core.APIClient{
		ID:      "client_a",
		Name:    "Client A",
		APIKey:  "gw_key",
		Enabled: false,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	_, err := service.AuthorizeProtocolKey("gw_key")
	if err == nil {
		t.Fatal("expected quota error")
	}
	accessErr, ok := err.(*AccessError)
	if !ok {
		t.Fatalf("expected AccessError, got %T", err)
	}
	if accessErr.StatusCode != httpStatusForbidden {
		t.Fatalf("status code = %d", accessErr.StatusCode)
	}
}

func TestUserSessionAuthenticatesUser(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	admin, created, err := service.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	if !created {
		t.Fatal("expected admin user to be created")
	}
	authenticated, err := service.AuthenticateUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("AuthenticateUser returned error: %v", err)
	}
	if authenticated.ID != admin.ID {
		t.Fatalf("authenticated id = %q, want %q", authenticated.ID, admin.ID)
	}
	token, _, err := service.CreateUserSession(authenticated.ID)
	if err != nil {
		t.Fatalf("CreateUserSession returned error: %v", err)
	}
	sessionUser, err := service.UserBySessionToken(token)
	if err != nil {
		t.Fatalf("UserBySessionToken returned error: %v", err)
	}
	if sessionUser.Username != "admin" {
		t.Fatalf("session username = %q", sessionUser.Username)
	}
}

func TestEnsureAdminUserForcesDefaultPasswordChange(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	admin, created, err := service.EnsureAdminUser("", "")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	if !created || !admin.ForcePasswordChange {
		t.Fatalf("admin created=%v ForcePasswordChange=%v, want created forced", created, admin.ForcePasswordChange)
	}
	if admin.Username != "root" {
		t.Fatalf("admin username = %q, want root", admin.Username)
	}
	if err := service.ChangeUserPassword(admin.ID, "toor", "admin-secret"); err != nil {
		t.Fatalf("ChangeUserPassword returned error: %v", err)
	}
	updated, err := service.GetUser(admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ForcePasswordChange {
		t.Fatal("ForcePasswordChange should be cleared after password change")
	}
}

func TestChangeUserPasswordRequiresCurrentPassword(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	user, err := service.CreateUser(UserInput{
		Username: "alice",
		Password: "old-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if err := service.ChangeUserPassword(user.ID, "wrong-secret", "new-secret"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("ChangeUserPassword wrong current err = %v, want invalid credentials", err)
	}
	if _, err := service.AuthenticateUser("alice", "old-secret"); err != nil {
		t.Fatalf("old password should still authenticate: %v", err)
	}
	if err := service.ChangeUserPassword(user.ID, "old-secret", "new-secret"); err != nil {
		t.Fatalf("ChangeUserPassword returned error: %v", err)
	}
	if _, err := service.AuthenticateUser("alice", "old-secret"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old password err = %v, want invalid credentials", err)
	}
	if _, err := service.AuthenticateUser("alice", "new-secret"); err != nil {
		t.Fatalf("new password should authenticate: %v", err)
	}
}

func TestDisabledClientOwnerCannotAuthorizeProtocolKey(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	user, err := service.CreateUser(UserInput{
		Username: "alice",
		Password: "alice-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	client := core.APIClient{
		ID:          "client_alice",
		Name:        "Alice Key",
		APIKey:      "gw_alice",
		OwnerUserID: user.ID,
		Enabled:     true,
		RoutePolicy: core.DefaultRoutePolicy(),
	}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AuthorizeProtocolKey("gw_alice"); err != nil {
		t.Fatalf("AuthorizeProtocolKey returned error while user enabled: %v", err)
	}
	if _, err := service.UpdateUser(user.ID, UserInput{
		Username: user.Username,
		Role:     core.UserRoleUser,
		Enabled:  false,
	}); err != nil {
		t.Fatalf("UpdateUser returned error: %v", err)
	}
	_, err = service.AuthorizeProtocolKey("gw_alice")
	if err == nil {
		t.Fatal("expected disabled owner to block client")
	}
	accessErr, ok := err.(*AccessError)
	if !ok {
		t.Fatalf("expected AccessError, got %T", err)
	}
	if accessErr.StatusCode != httpStatusForbidden {
		t.Fatalf("status code = %d, want %d", accessErr.StatusCode, httpStatusForbidden)
	}
}

func TestAuthorizeProtocolKeyCachesClientOwnerStatus(t *testing.T) {
	base := &getUserCountingRepository{MemoryRepository: storage.NewMemoryRepository()}
	if err := base.UpsertUser(core.User{ID: "user_cached", Username: "cached", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := base.UpsertClient(core.APIClient{
		ID:          "client_cached",
		Name:        "Cached Key",
		APIKey:      "gw_cached",
		OwnerUserID: "user_cached",
		Enabled:     true,
		RoutePolicy: core.DefaultRoutePolicy(),
	}); err != nil {
		t.Fatal(err)
	}
	repo := storage.NewCachedRepository(base)
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	if _, err := service.AuthorizeProtocolKey("gw_cached"); err != nil {
		t.Fatalf("AuthorizeProtocolKey returned error: %v", err)
	}
	if _, err := service.AuthorizeProtocolKey("gw_cached"); err != nil {
		t.Fatalf("second AuthorizeProtocolKey returned error: %v", err)
	}
	if calls := base.getUserCalls.Load(); calls != 1 {
		t.Fatalf("auth hot path called GetUser %d times, want 1 initial indexed lookup", calls)
	}

	if err := repo.UpsertUser(core.User{ID: "user_cached", Username: "cached", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	_, err := service.AuthorizeProtocolKey("gw_cached")
	if err == nil {
		t.Fatal("expected disabled owner to block client")
	}
	accessErr, ok := err.(*AccessError)
	if !ok {
		t.Fatalf("expected AccessError, got %T", err)
	}
	if accessErr.StatusCode != httpStatusForbidden {
		t.Fatalf("status code = %d, want %d", accessErr.StatusCode, httpStatusForbidden)
	}
	if calls := base.getUserCalls.Load(); calls != 1 {
		t.Fatalf("auth after user revision called GetUser %d times, want 1", calls)
	}
}

func TestAuthorizeProtocolKeyDoesNotListAllClients(t *testing.T) {
	base := storage.NewMemoryRepository()
	if err := base.UpsertClient(core.APIClient{
		ID:          "client_no_full_list",
		Name:        "No Full List",
		APIKey:      "gw_no_full_list",
		OwnerUserID: "",
		Enabled:     true,
		RoutePolicy: core.DefaultRoutePolicy(),
	}); err != nil {
		t.Fatal(err)
	}
	repo := &authFullClientListPanicRepository{CachedRepository: storage.NewCachedRepository(base)}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	client, err := service.AuthorizeProtocolKey("gw_no_full_list")
	if err != nil {
		t.Fatalf("AuthorizeProtocolKey returned error: %v", err)
	}
	if client.ID != "client_no_full_list" {
		t.Fatalf("client ID = %q", client.ID)
	}
}

type authFullClientListPanicRepository struct {
	*storage.CachedRepository
}

func (r *authFullClientListPanicRepository) ListClients() []core.APIClient {
	panic("AuthorizeProtocolKey should not build auth cache from ListClients")
}

func TestCreateClientStoresRoutePolicy(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_claude",
		Provider: core.ProviderClaude,
		Label:    "Claude",
		Group:    "Plus",
		Status:   core.AccountStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	client, err := service.CreateClientInAccountGroupForUser(core.User{ID: "admin", Role: core.UserRoleAdmin, Enabled: true}, "Client A", 0, true, "Plus", core.ProviderClaude, []core.ProviderKind{core.ProviderOpenAI})
	if err != nil {
		t.Fatalf("CreateClientInAccountGroupForUser returned error: %v", err)
	}
	if client.RoutePolicy.DefaultProvider != core.ProviderClaude {
		t.Fatalf("default provider = %q", client.RoutePolicy.DefaultProvider)
	}
	if len(client.RoutePolicy.FallbackProviders) != 1 || client.RoutePolicy.FallbackProviders[0] != core.ProviderOpenAI {
		t.Fatalf("fallback providers = %#v", client.RoutePolicy.FallbackProviders)
	}
}

func TestDeleteClientRemovesClient(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertClient(core.APIClient{
		ID:      "client_a",
		Name:    "Client A",
		APIKey:  "gw_key",
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	deleted, err := service.DeleteClient("client_a")
	if err != nil {
		t.Fatalf("DeleteClient returned error: %v", err)
	}
	if deleted.ID != "client_a" {
		t.Fatalf("deleted id = %q", deleted.ID)
	}
	if _, err := repo.GetClient("client_a"); err == nil {
		t.Fatal("expected client to be deleted")
	}
}

func TestClientEditorInfersSelectedAccountGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_premium", Name: "Premium"}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []core.Account{
		{ID: "acct_a", Provider: core.ProviderOpenAI, Label: "A", Group: "Premium", Status: core.AccountStatusActive},
		{ID: "acct_b", Provider: core.ProviderOpenAI, Label: "B", Group: "Premium", Status: core.AccountStatusActive},
		{ID: "acct_c", Provider: core.ProviderClaude, Label: "C", Group: "Other", Status: core.AccountStatusActive},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	client, err := service.CreateClientInAccountGroupForUser(
		core.User{ID: "admin", Role: core.UserRoleAdmin, Enabled: true},
		"Client A",
		0,
		true,
		"Premium",
		core.ProviderOpenAI,
		nil,
	)
	if err != nil {
		t.Fatalf("CreateClientInAccountGroupForUser returned error: %v", err)
	}

	editor, err := service.GetClientEditor(client.ID)
	if err != nil {
		t.Fatalf("GetClientEditor returned error: %v", err)
	}
	if editor.SelectedAccountGroup != "Premium" {
		t.Fatalf("selected group = %q, want Premium", editor.SelectedAccountGroup)
	}
}

func TestClientEditorUsesClientOwnerPlanStatus(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_plan_owner", Username: "plan-owner", Enabled: true, BalanceNanoUSD: 20 * core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "owner_plan",
		Name:               "Owner Plan",
		Enabled:            true,
		PriceNanoUSD:       core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 10 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_plan_owner", PlanID: "owner_plan"}); err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:          "client_plan_owner",
		Name:        "Plan Owner Key",
		APIKey:      "gw_plan_owner",
		OwnerUserID: "user_plan_owner",
		Enabled:     true,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	editor, err := service.GetClientEditor("client_plan_owner")
	if err != nil {
		t.Fatalf("GetClientEditor returned error: %v", err)
	}
	if !editor.HasActivePlan {
		t.Fatal("editor.HasActivePlan = false, want true for the client owner")
	}
}

func TestNewClientEditorForUserDefaultsToPlanBillingWhenSubscribed(t *testing.T) {
	repo := storage.NewMemoryRepository()
	user := core.User{ID: "user_plan_create_default", Username: "plan-create-default", Enabled: true, BalanceNanoUSD: 20 * core.NanoUSDPerUSD}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "create_default_plan",
		Name:               "Create Default Plan",
		Enabled:            true,
		PriceNanoUSD:       core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 10 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: user.ID, PlanID: "create_default_plan"}); err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	editor := service.NewClientEditorForUser(user)
	if !editor.HasActivePlan {
		t.Fatal("editor.HasActivePlan = false, want true")
	}
	if editor.Client.BillingSource != core.ClientBillingSourcePlan || editor.SelectedBillingSource != core.ClientBillingSourcePlan {
		t.Fatalf("billing source = client %q selected %q, want plan", editor.Client.BillingSource, editor.SelectedBillingSource)
	}
}

func TestClientPlanBillingRejectsDisabledAccountGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	user := core.User{ID: "user_plan_group_disabled", Username: "plan-group-disabled", Enabled: true, BalanceNanoUSD: 20 * core.NanoUSDPerUSD}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "disabled_group_plan",
		Name:               "Disabled Group Plan",
		Enabled:            true,
		PriceNanoUSD:       core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 10 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: user.ID, PlanID: "disabled_group_plan"}); err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}
	planBillingEnabled := false
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_cash_only", Name: "Cash Only", PlanBillingEnabled: &planBillingEnabled}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_cash_only", Provider: core.ProviderOpenAI, Label: "Cash Only", Group: "Cash Only", Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	if _, err := service.CreateClientInBillingSourceForUser(user, "Plan Client", 0, true, "Cash Only", core.ClientBillingSourcePlan, core.ProviderOpenAI, nil); err == nil {
		t.Fatal("CreateClientInBillingSourceForUser returned nil error, want plan billing disabled")
	}
	client, err := service.CreateClientInBillingSourceForUser(user, "Cash Client", 0, true, "Cash Only", core.ClientBillingSourceCash, core.ProviderOpenAI, nil)
	if err != nil {
		t.Fatalf("CreateClientInBillingSourceForUser cash returned error: %v", err)
	}
	if err := service.UpdateClientBillingSourceForUser(user, client.ID, client.Name, 0, true, "Cash Only", core.ClientBillingSourcePlan, core.ProviderOpenAI, nil); err == nil {
		t.Fatal("UpdateClientBillingSourceForUser returned nil error, want plan billing disabled")
	}
}

func TestNewClientEditorIncludesDefaultAccountGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	editor := service.NewClientEditor()
	if len(editor.AvailableAccountGroups) < 2 {
		t.Fatalf("available groups = %#v", editor.AvailableAccountGroups)
	}
	if editor.AvailableAccountGroups[0].ID != defaultAccountGroupID || editor.AvailableAccountGroups[0].Name != core.DefaultAccountGroupName {
		t.Fatalf("default group missing or not first: %#v", editor.AvailableAccountGroups)
	}
	if editor.AvailableAccountGroups[0].BillingMultiplierBps != 10000 {
		t.Fatalf("default multiplier = %d", editor.AvailableAccountGroups[0].BillingMultiplierBps)
	}
	if editor.SelectedAccountGroup != core.DefaultAccountGroupName {
		t.Fatalf("selected group = %q, want default", editor.SelectedAccountGroup)
	}
}

func TestDefaultClientModelListUsesOnlyDefaultGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	hide := false
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_hidden", Name: "Hidden", ShowInClientEditor: &hide}); err != nil {
		t.Fatal(err)
	}
	for _, model := range []core.ModelConfig{
		{ID: "default-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{core.DefaultAccountGroupName}},
		{ID: "plus-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{"Plus"}},
		{ID: "hidden-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{"Hidden"}},
	} {
		if err := repo.UpsertModel(model); err != nil {
			t.Fatal(err)
		}
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	models := service.ListModelsForClient(context.Background(), &core.APIClient{ID: "client_default"})
	got := map[string]bool{}
	for _, model := range models {
		got[model.Name] = true
	}
	if !got["default-model"] || got["plus-model"] || got["hidden-model"] {
		t.Fatalf("models = %#v, want Default group models only", got)
	}
}

func TestHiddenAccountGroupModelListRequiresVisibleUser(t *testing.T) {
	repo := storage.NewMemoryRepository()
	hide := false
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_hidden", Name: "Hidden", ShowInClientEditor: &hide, VisibleUserIDs: []string{"user_visible_models"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{ID: "hidden-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{"Hidden"}}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	deniedModels := service.ListModelsForClient(context.Background(), &core.APIClient{ID: "client_denied_models", OwnerUserID: "user_denied_models", AccountGroup: "Hidden"})
	if len(deniedModels) != 0 {
		t.Fatalf("denied models = %#v, want none", deniedModels)
	}

	visibleModels := service.ListModelsForClient(context.Background(), &core.APIClient{ID: "client_visible_models", OwnerUserID: "user_visible_models", AccountGroup: "Hidden"})
	if len(visibleModels) != 1 || visibleModels[0].Name != "hidden-model" {
		t.Fatalf("visible models = %#v, want hidden-model", visibleModels)
	}
}

func TestSyncProviderModelsDefaultsVisibleGroupsByProviderGroupType(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, group := range []core.AccountGroup{
		{ID: core.DefaultAccountGroupID, Name: core.DefaultAccountGroupName, Type: core.AccountGroupTypeMixed},
		{ID: "group_openai", Name: "OpenAI", Type: core.AccountGroupTypeOpenAI},
		{ID: "group_claude", Name: "Claude", Type: core.AccountGroupTypeClaude},
		{ID: "group_shared", Name: "Shared", Type: core.AccountGroupTypeMixed},
	} {
		if err := repo.UpsertAccountGroup(group); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_claude",
		Provider: core.ProviderClaude,
		Label:    "Claude",
		Group:    "Claude",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			AccessToken: "claude-token",
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&testProviderModelListingAdapter{
		kind:    core.ProviderClaude,
		label:   "Claude",
		modelID: "claude-test",
		ownedBy: "anthropic",
	}))

	result, err := service.SyncProviderModels(context.Background(), core.ProviderClaude)
	if err != nil {
		t.Fatalf("SyncProviderModels returned error: %v", err)
	}
	if result.Imported != 1 {
		t.Fatalf("sync result = %#v, want one import", result)
	}
	model, err := repo.GetModel("claude-test")
	if err != nil {
		t.Fatalf("GetModel returned error: %v", err)
	}
	if !slices.Equal(model.VisibleGroups, []string{"Claude"}) {
		t.Fatalf("visible groups = %#v, want Claude only", model.VisibleGroups)
	}
}

func TestClientEditorKeepsCurrentHiddenAccountGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_visible", Name: "Visible"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_hidden", Name: "Hidden", ShowInClientEditor: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:           "client_hidden",
		Name:         "Client A",
		APIKey:       "gw_hidden",
		Enabled:      true,
		RoutePolicy:  core.DefaultRoutePolicy(),
		AccountGroup: "Hidden",
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	editor, err := service.GetClientEditor("client_hidden")
	if err != nil {
		t.Fatalf("GetClientEditor returned error: %v", err)
	}
	foundHidden := false
	for _, group := range editor.AvailableAccountGroups {
		if group.Name == "Hidden" {
			foundHidden = true
			break
		}
	}
	if !foundHidden {
		t.Fatalf("current hidden group missing from editor options: %#v", editor.AvailableAccountGroups)
	}
}

func TestUpdateAccountGroupVisibility(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	group, err := service.UpdateAccountGroupVisibility("group_plus", false)
	if err != nil {
		t.Fatalf("UpdateAccountGroupVisibility returned error: %v", err)
	}
	if group.ShowInClientEditor == nil || *group.ShowInClientEditor {
		t.Fatalf("group visibility = %#v, want false", group.ShowInClientEditor)
	}

	editor := service.NewClientEditor()
	for _, option := range editor.AvailableAccountGroups {
		if option.Name == "Plus" {
			t.Fatalf("hidden group leaked into new client editor: %#v", editor.AvailableAccountGroups)
		}
	}

	group, err = service.UpdateAccountGroupVisibility("group_plus", true)
	if err != nil {
		t.Fatalf("UpdateAccountGroupVisibility returned error: %v", err)
	}
	if group.ShowInClientEditor == nil || !*group.ShowInClientEditor {
		t.Fatalf("group visibility = %#v, want true", group.ShowInClientEditor)
	}
}

func TestCreateClientRejectsHiddenAccountGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_hidden", Name: "Hidden", ShowInClientEditor: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_hidden", Provider: core.ProviderOpenAI, Label: "Hidden", Group: "Hidden", Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	_, err := service.CreateClientInAccountGroupForUser(
		core.User{ID: "admin", Role: core.UserRoleAdmin, Enabled: true},
		"Client A",
		0,
		true,
		"Hidden",
		core.ProviderOpenAI,
		nil,
	)
	if err == nil {
		t.Fatal("expected hidden group validation error")
	}
}

func TestAccountGroupVisibilityAllowsSpecificUsers(t *testing.T) {
	repo := storage.NewMemoryRepository()
	visibleUser := core.User{ID: "user_visible_group", Username: "visible", Role: core.UserRoleUser, Enabled: true}
	otherUser := core.User{ID: "user_other_group", Username: "other", Role: core.UserRoleUser, Enabled: true}
	for _, user := range []core.User{visibleUser, otherUser} {
		if err := repo.UpsertUser(user); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_private", Name: "Private", ShowInClientEditor: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_private", Provider: core.ProviderOpenAI, Label: "Private", Group: "Private", Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	group, err := service.UpdateAccountGroupVisibilitySettings("group_private", false, []string{visibleUser.Username})
	if err != nil {
		t.Fatalf("UpdateAccountGroupVisibilitySettings returned error: %v", err)
	}
	if len(group.VisibleUserIDs) != 1 || group.VisibleUserIDs[0] != visibleUser.ID {
		t.Fatalf("visible users = %#v, want %s", group.VisibleUserIDs, visibleUser.ID)
	}
	visibleEditor := service.NewClientEditorForUser(visibleUser)
	if !clientEditorHasGroup(visibleEditor, "Private") {
		t.Fatalf("private group missing for visible user: %#v", visibleEditor.AvailableAccountGroups)
	}
	otherEditor := service.NewClientEditorForUser(otherUser)
	if clientEditorHasGroup(otherEditor, "Private") {
		t.Fatalf("private group leaked to other user: %#v", otherEditor.AvailableAccountGroups)
	}
	if _, err := service.CreateClientInAccountGroupForUser(visibleUser, "Visible Client", 0, true, "Private", core.ProviderOpenAI, nil); err != nil {
		t.Fatalf("visible user CreateClientInAccountGroupForUser returned error: %v", err)
	}
	if _, err := service.CreateClientInAccountGroupForUser(otherUser, "Other Client", 0, true, "Private", core.ProviderOpenAI, nil); err == nil {
		t.Fatal("other user should not create client in private group")
	}
}

func TestHiddenAccountGroupExistingClientRequiresVisibleUser(t *testing.T) {
	repo := storage.NewMemoryRepository()
	visibleUser := core.User{ID: "user_visible_existing_group", Username: "visible-existing", Role: core.UserRoleUser, Enabled: true}
	otherUser := core.User{ID: "user_other_existing_group", Username: "other-existing", Role: core.UserRoleUser, Enabled: true}
	for _, user := range []core.User{visibleUser, otherUser} {
		if err := repo.UpsertUser(user); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_private", Name: "Private", ShowInClientEditor: boolPtr(false), VisibleUserIDs: []string{visibleUser.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{ID: "acct_private", Provider: core.ProviderOpenAI, Label: "Private", Group: "Private", Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_other_private", Name: "Other Private", APIKey: "gw_other_private", OwnerUserID: otherUser.ID, Enabled: true, AccountGroup: "Private"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_visible_private", Name: "Visible Private", APIKey: "gw_visible_private", OwnerUserID: visibleUser.ID, Enabled: true, AccountGroup: "Private"}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	otherEditor, err := service.GetClientEditorForUser("client_other_private", otherUser)
	if err != nil {
		t.Fatalf("GetClientEditorForUser returned error: %v", err)
	}
	if clientEditorHasGroup(otherEditor, "Private") {
		t.Fatalf("private group leaked to non-visible current owner: %#v", otherEditor.AvailableAccountGroups)
	}
	if err := service.UpdateClientBillingSourceForUser(otherUser, "client_other_private", "Other Private", 0, true, "Private", core.ClientBillingSourceCash, core.ProviderOpenAI, nil); err == nil {
		t.Fatal("non-visible owner should not keep hidden account group")
	}

	visibleEditor, err := service.GetClientEditorForUser("client_visible_private", visibleUser)
	if err != nil {
		t.Fatalf("GetClientEditorForUser returned error: %v", err)
	}
	if !clientEditorHasGroup(visibleEditor, "Private") {
		t.Fatalf("private group missing for visible current owner: %#v", visibleEditor.AvailableAccountGroups)
	}
	if err := service.UpdateClientBillingSourceForUser(visibleUser, "client_visible_private", "Visible Private", 0, true, "Private", core.ClientBillingSourceCash, core.ProviderOpenAI, nil); err != nil {
		t.Fatalf("visible owner should keep hidden account group: %v", err)
	}
}

func clientEditorHasGroup(editor ClientEditor, name string) bool {
	for _, group := range editor.AvailableAccountGroups {
		if strings.EqualFold(group.Name, name) {
			return true
		}
	}
	return false
}

func TestCreateClientRejectsUnknownAccountGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	_, err := service.CreateClientInAccountGroupForUser(
		core.User{ID: "admin", Role: core.UserRoleAdmin, Enabled: true},
		"Client A",
		0,
		true,
		"Missing",
		core.ProviderOpenAI,
		nil,
	)
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestAuditPageFiltersAndPaginates(t *testing.T) {
	repo := storage.NewMemoryRepository()
	now := time.Now().UTC()
	events := []core.AuditEvent{
		{ID: "1", Kind: core.AuditKindAdmin, Actor: "admin", Action: "user.update", ResourceID: "user_admin", ResourceName: "admin", Status: "ok", CreatedAt: now},
		{ID: "2", Kind: core.AuditKindGateway, ClientID: "client_a", ClientName: "Client A", Provider: core.ProviderOpenAI, Model: "gpt-4.1", Status: "ok", CreatedAt: now.Add(-time.Second)},
		{ID: "3", Kind: core.AuditKindAdmin, Actor: "admin", Action: "client.update", ResourceID: "client_a", Status: "error", CreatedAt: now.Add(-2 * time.Second)},
		{ID: "4", Kind: core.AuditKindAdmin, Actor: "ops", Action: "account.update", ResourceID: "acct_a", Status: "ok", CreatedAt: now.Add(-3 * time.Second)},
	}
	for _, event := range events {
		if err := repo.AppendAudit(event); err != nil {
			t.Fatal(err)
		}
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	page := service.AuditPage(context.Background(), AuditFilter{
		Kind:     core.AuditKindAdmin,
		Status:   "ok",
		Actor:    "admin",
		Resource: "admin",
		Page:     1,
		PageSize: 10,
	})
	if page.Total != 1 {
		t.Fatalf("total = %d, want 1", page.Total)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "1" {
		t.Fatalf("items = %#v", page.Items)
	}

	page = service.AuditPage(context.Background(), AuditFilter{
		Kind:     core.AuditKindAdmin,
		Page:     2,
		PageSize: 2,
	})
	if !page.HasPrev {
		t.Fatal("expected previous page")
	}
	if page.HasNext {
		t.Fatal("did not expect next page")
	}
	if len(page.Items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(page.Items))
	}
}

func TestRecoverAccountReactivatesCoolingAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	cooldownUntil := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	if err := repo.UpsertAccount(core.Account{
		ID:               "acct_recover",
		Provider:         core.ProviderOpenAI,
		Label:            "Recover Me",
		Status:           core.AccountStatusCooling,
		CooldownUntil:    &cooldownUntil,
		ConsecutiveFails: 3,
		TotalFails:       7,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	account, err := service.RecoverAccount("acct_recover")
	if err != nil {
		t.Fatalf("recover account: %v", err)
	}

	if account.Status != core.AccountStatusActive {
		t.Fatalf("status = %q, want %q", account.Status, core.AccountStatusActive)
	}
	if account.CooldownUntil != nil {
		t.Fatalf("cooldown = %#v, want nil", account.CooldownUntil)
	}
	if account.ConsecutiveFails != 0 {
		t.Fatalf("consecutive fails = %d, want 0", account.ConsecutiveFails)
	}
	if account.TotalFails != 7 {
		t.Fatalf("total fails = %d, want 7", account.TotalFails)
	}
}

func TestRecoverAccountClearsQuotaRefreshFailureMetadata(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:              "acct_recover_metadata",
		Label:           "Recover Metadata",
		Status:          core.AccountStatusExpired,
		ControlDisabled: true,
		Credential: core.Credential{
			Metadata: map[string]string{
				core.AccountQuotaErrorMetadataKey:       "credential_expired: refresh token was already used",
				core.AccountQuotaErrorAtMetadataKey:     "2026-05-18T16:17:13Z",
				core.AccountQuotaErrorCodeMetadataKey:   "credential_expired",
				core.AccountQuotaErrorStatusMetadataKey: string(core.AccountStatusExpired),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	account, err := service.RecoverAccount("acct_recover_metadata")
	if err != nil {
		t.Fatalf("recover account: %v", err)
	}
	if account.Status != core.AccountStatusActive {
		t.Fatalf("status = %q, want %q", account.Status, core.AccountStatusActive)
	}
	if !account.ControlDisabled {
		t.Fatal("recover should preserve control disabled state")
	}
	if account.Credential.Metadata[core.AccountQuotaErrorMetadataKey] != "" ||
		account.Credential.Metadata[core.AccountQuotaErrorAtMetadataKey] != "" ||
		account.Credential.Metadata[core.AccountQuotaErrorCodeMetadataKey] != "" ||
		account.Credential.Metadata[core.AccountQuotaErrorStatusMetadataKey] != "" {
		t.Fatalf("quota refresh failure metadata was not cleared: %#v", account.Credential.Metadata)
	}
}

func TestRecoverAccountClearsScopedRateLimitSnapshot(t *testing.T) {
	repo := storage.NewMemoryRepository()
	reset := time.Now().UTC().Add(time.Hour)
	if err := repo.UpsertAccount(accountWithQuotaSnapshot(t, core.Account{
		ID:       "acct_recover_rate_limit",
		Provider: core.ProviderOpenAI,
		Label:    "Recover Rate Limit",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			Metadata: map[string]string{
				"account_login_method": "api_key",
				"base_url":             "https://hk.getelucid.com",
			},
		},
	}, core.AccountQuotaSnapshot{
		Additional: map[string]core.AccountQuotaSnapshot{
			core.AccountQuotaRuntimeChatLimitID: {
				Source:      core.AccountQuotaRuntimeSource,
				LimitID:     core.AccountQuotaRuntimeChatLimitID,
				Primary:     &core.AccountQuotaWindow{Name: "rate_limit", UsedPercent: 100, ResetsAt: &reset},
				ReachedType: "primary_window",
			},
		},
	})); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	before, err := repo.GetAccount("acct_recover_rate_limit")
	if err != nil {
		t.Fatal(err)
	}
	if got := AccountFilterStatus(before); got != "cooling" {
		t.Fatalf("filter status before recover = %q, want cooling", got)
	}

	account, err := service.RecoverAccount("acct_recover_rate_limit")
	if err != nil {
		t.Fatalf("recover account: %v", err)
	}
	if quota := core.ReadAccountQuota(account); quota != nil {
		t.Fatalf("quota snapshot = %#v, want cleared", quota)
	}
	if got := AccountFilterStatus(account); got != "normal" {
		t.Fatalf("filter status after recover = %q, want normal", got)
	}
}

func TestRecoverAccountDropsProviderImageSnapshot(t *testing.T) {
	repo := storage.NewMemoryRepository()
	reset := time.Now().UTC().Add(time.Hour)
	if err := repo.UpsertAccount(accountWithQuotaSnapshot(t, core.Account{
		ID:       "acct_recover_provider_quota",
		Provider: core.ProviderOpenAI,
		Label:    "Recover Provider Quota",
		Status:   core.AccountStatusActive,
	}, core.AccountQuotaSnapshot{
		Source:    "openai_chatgpt_usage",
		Plan:      "plus",
		Secondary: &core.AccountQuotaWindow{Name: "secondary", UsedPercent: 50, ResetsAt: &reset},
	})); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	account, err := service.RecoverAccount("acct_recover_provider_quota")
	if err != nil {
		t.Fatalf("recover account: %v", err)
	}
	quota := core.ReadAccountQuota(account)
	if quota == nil || quota.Source != "openai_chatgpt_usage" || quota.Secondary == nil || quota.Image != nil {
		t.Fatalf("provider quota snapshot = %#v, want image dropped", quota)
	}
}

func TestRecoverAccountPreservesControlDisabledState(t *testing.T) {
	repo := storage.NewMemoryRepository()
	cooldownUntil := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	if err := repo.UpsertAccount(core.Account{
		ID:               "acct_disabled",
		Provider:         core.ProviderOpenAI,
		Label:            "Disabled",
		Status:           core.AccountStatusExpired,
		ControlDisabled:  true,
		CooldownUntil:    &cooldownUntil,
		ConsecutiveFails: 2,
		TotalFails:       5,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	account, err := service.RecoverAccount("acct_disabled")
	if err != nil {
		t.Fatalf("recover disabled account: %v", err)
	}

	if account.Status != core.AccountStatusActive {
		t.Fatalf("status = %q, want %q", account.Status, core.AccountStatusActive)
	}
	if !account.ControlDisabled {
		t.Fatalf("control disabled = false, want true")
	}
	if account.CooldownUntil != nil {
		t.Fatalf("cooldown = %#v, want nil", account.CooldownUntil)
	}
	if account.ConsecutiveFails != 0 {
		t.Fatalf("consecutive fails = %d, want 0", account.ConsecutiveFails)
	}
	if account.TotalFails != 5 {
		t.Fatalf("total fails = %d, want 5", account.TotalFails)
	}
}

func TestUpdateAccountClearsQuotaRefreshFailureMetadataOnExplicitStatusChange(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:     "acct_edit_status",
		Label:  "Editable",
		Group:  core.DefaultAccountGroupName,
		Status: core.AccountStatusExpired,
		Credential: core.Credential{
			Metadata: map[string]string{
				core.AccountQuotaErrorMetadataKey:       "credential_expired: refresh token was already used",
				core.AccountQuotaErrorAtMetadataKey:     "2026-05-18T16:17:13Z",
				core.AccountQuotaErrorCodeMetadataKey:   "credential_expired",
				core.AccountQuotaErrorStatusMetadataKey: string(core.AccountStatusExpired),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if err := service.UpdateAccount("acct_edit_status", "Editable", "", core.DefaultAccountGroupName, "", "", "", "", "", nil, 0, 0, core.AccountStatusActive, false, false); err != nil {
		t.Fatalf("update account: %v", err)
	}

	account, err := repo.GetAccount("acct_edit_status")
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != core.AccountStatusActive {
		t.Fatalf("status = %q, want %q", account.Status, core.AccountStatusActive)
	}
	if account.Credential.Metadata[core.AccountQuotaErrorMetadataKey] != "" ||
		account.Credential.Metadata[core.AccountQuotaErrorAtMetadataKey] != "" ||
		account.Credential.Metadata[core.AccountQuotaErrorCodeMetadataKey] != "" ||
		account.Credential.Metadata[core.AccountQuotaErrorStatusMetadataKey] != "" {
		t.Fatalf("quota refresh failure metadata was not cleared: %#v", account.Credential.Metadata)
	}
}

func TestRefreshAccountQuotaPersistsSnapshotAndRefreshedCredential(t *testing.T) {
	repo := storage.NewMemoryRepository()
	expiresAt := time.Now().UTC().Add(10 * time.Second)
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_oauth",
		Provider: core.ProviderOpenAI,
		Label:    "OAuth Account",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			Mode:         "openai-oauth-device",
			AccessToken:  "stale-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    &expiresAt,
			Metadata: map[string]string{
				"token_source": "openai_device_code",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&testQuotaAdapter{}))
	account, snapshot, err := service.RefreshAccountQuota(context.Background(), "acct_oauth")
	if err != nil {
		t.Fatalf("RefreshAccountQuota returned error: %v", err)
	}
	if account.Credential.AccessToken != "fresh-token" {
		t.Fatalf("access token = %q", account.Credential.AccessToken)
	}
	if snapshot.Plan != "pro" {
		t.Fatalf("plan = %q", snapshot.Plan)
	}

	saved, err := repo.GetAccount("acct_oauth")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Credential.Metadata["oauth_refreshed_at"] == "" {
		t.Fatalf("metadata = %#v", saved.Credential.Metadata)
	}
	parsed := ReadAccountQuota(saved)
	if parsed == nil || parsed.Plan != "pro" {
		t.Fatalf("parsed quota = %#v", parsed)
	}
	if saved.Credential.Metadata[core.AccountQuotaErrorMetadataKey] != "" {
		t.Fatalf("unexpected quota error metadata: %#v", saved.Credential.Metadata)
	}
}

func TestSupportsQuotaRefreshSkipsOpenAIAPIKeyByDefault(t *testing.T) {
	service := New(storage.NewMemoryRepository(), providers.NewRegistry(&providers.OpenAIAdapter{}))

	account := core.Account{
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "sk-test",
			Metadata: map[string]string{
				"account_login_method": "api_key",
			},
		},
	}
	if service.SupportsQuotaRefresh(account) {
		t.Fatal("OpenAI API key quota refresh should be disabled without an explicit quota provider")
	}

	account.Credential.Metadata["api_key_quota_provider"] = "gateway"
	if service.SupportsQuotaRefresh(account) {
		t.Fatal("gateway quota refresh should require a base URL")
	}

	account.Credential.Metadata["base_url"] = "https://gateway.example"
	if !service.SupportsQuotaRefresh(account) {
		t.Fatal("gateway quota refresh should be supported once a base URL is configured")
	}
}

func TestRefreshAccountQuotaSkipsControlDisabledAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:              "acct_disabled_quota",
		Provider:        core.ProviderOpenAI,
		Label:           "Disabled Quota",
		Status:          core.AccountStatusActive,
		ControlDisabled: true,
		Credential: core.Credential{
			AccessToken: "token",
			Metadata: map[string]string{
				"token_source": providers.OpenAIDeviceCodeTokenSourceValue(),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&testQuotaAdapter{}))
	if service.SupportsQuotaRefresh(core.Account{
		Provider:        core.ProviderOpenAI,
		ControlDisabled: true,
		Credential: core.Credential{Metadata: map[string]string{
			"token_source": providers.OpenAIDeviceCodeTokenSourceValue(),
		}},
	}) {
		t.Fatal("control-disabled account should not support quota refresh")
	}
	_, _, err := service.RefreshAccountQuota(context.Background(), "acct_disabled_quota")
	if err == nil || !strings.Contains(err.Error(), "does not support quota refresh") {
		t.Fatalf("RefreshAccountQuota err = %v, want unsupported", err)
	}
	account, err := repo.GetAccount("acct_disabled_quota")
	if err != nil {
		t.Fatal(err)
	}
	if !account.ControlDisabled || account.Status != core.AccountStatusActive {
		t.Fatalf("account = %#v, want still control disabled active runtime state", account)
	}
}

func TestRefreshAccountQuotaPersistsExpiredCooldownNormalization(t *testing.T) {
	repo := storage.NewMemoryRepository()
	expiredCooldown := time.Now().UTC().Add(-time.Minute)
	if err := repo.UpsertAccount(core.Account{
		ID:               "acct_expired_cooldown_quota",
		Provider:         core.ProviderOpenAI,
		Label:            "Expired Cooldown",
		Status:           core.AccountStatusCooling,
		CooldownUntil:    &expiredCooldown,
		ConsecutiveFails: 1,
		Credential: core.Credential{
			Mode:        "openai-oauth-device",
			AccessToken: "fresh-token",
			Metadata: map[string]string{
				"token_source": "openai_device_code",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&testQuotaAdapter{}))
	account, snapshot, err := service.RefreshAccountQuota(context.Background(), "acct_expired_cooldown_quota")
	if err != nil {
		t.Fatalf("RefreshAccountQuota returned error: %v", err)
	}
	if snapshot.Plan != "pro" {
		t.Fatalf("snapshot = %#v, want pro plan", snapshot)
	}
	if account.Status != core.AccountStatusActive || account.CooldownUntil != nil {
		t.Fatalf("returned account = %#v, want active without cooldown", account)
	}

	saved, err := repo.GetAccount("acct_expired_cooldown_quota")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != core.AccountStatusActive || saved.CooldownUntil != nil {
		t.Fatalf("saved account = %#v, want active without cooldown", saved)
	}
}

func TestRefreshAccountQuotaReactivatesCoolingAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	futureCooldown := time.Now().UTC().Add(time.Minute)
	if err := repo.UpsertAccount(core.Account{
		ID:               "acct_cooling_quota",
		Provider:         core.ProviderOpenAI,
		Label:            "Cooling Account",
		Status:           core.AccountStatusCooling,
		CooldownUntil:    &futureCooldown,
		ConsecutiveFails: 2,
		Credential: core.Credential{
			Mode:        "openai-oauth-device",
			AccessToken: "fresh-token",
			Metadata: map[string]string{
				"token_source": "openai_device_code",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&testQuotaAdapter{}))
	account, snapshot, err := service.RefreshAccountQuota(context.Background(), "acct_cooling_quota")
	if err != nil {
		t.Fatalf("RefreshAccountQuota returned error: %v", err)
	}
	if snapshot.Plan != "pro" {
		t.Fatalf("snapshot = %#v, want pro plan", snapshot)
	}
	if account.Status != core.AccountStatusActive || account.CooldownUntil != nil {
		t.Fatalf("returned account = %#v, want active without cooldown", account)
	}

	saved, err := repo.GetAccount("acct_cooling_quota")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != core.AccountStatusActive || saved.CooldownUntil != nil {
		t.Fatalf("saved account = %#v, want active without cooldown", saved)
	}
}

func TestRefreshAccountQuotaReactivatesBlockedAccount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:               "acct_blocked_quota",
		Provider:         core.ProviderOpenAI,
		Label:            "Blocked Account",
		Status:           core.AccountStatusBlocked,
		ConsecutiveFails: 2,
		Credential: core.Credential{
			Mode:        "openai-oauth-device",
			AccessToken: "fresh-token",
			Metadata: map[string]string{
				"token_source":                        "openai_device_code",
				core.AccountQuotaErrorMetadataKey:     "previous transient failure",
				core.AccountQuotaErrorAtMetadataKey:   time.Now().UTC().Format(time.RFC3339),
				core.AccountQuotaErrorCodeMetadataKey: providers.ErrorCodeUpstreamTransportError,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&testQuotaAdapter{}))
	account, snapshot, err := service.RefreshAccountQuota(context.Background(), "acct_blocked_quota")
	if err != nil {
		t.Fatalf("RefreshAccountQuota returned error: %v", err)
	}
	if snapshot.Plan != "pro" {
		t.Fatalf("snapshot = %#v, want pro plan", snapshot)
	}
	if account.Status != core.AccountStatusActive || account.CooldownUntil != nil || account.ConsecutiveFails != 0 {
		t.Fatalf("returned account = %#v, want active without failure state", account)
	}
	if message, _ := ReadAccountQuotaError(account); message != "" {
		t.Fatalf("quota error = %q, want cleared", message)
	}
}

func TestRefreshAccountQuotaPersistsRefreshError(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_oauth_error",
		Provider: core.ProviderOpenAI,
		Label:    "OAuth Account",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			Mode:        "openai-oauth-device",
			AccessToken: "oauth-token",
			Metadata: map[string]string{
				"token_source": "openai_device_code",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&testQuotaFailAdapter{}))
	_, _, err := service.RefreshAccountQuota(context.Background(), "acct_oauth_error")
	if err == nil {
		t.Fatal("expected refresh error")
	}

	saved, err := repo.GetAccount("acct_oauth_error")
	if err != nil {
		t.Fatal(err)
	}
	message, at := ReadAccountQuotaError(saved)
	if !strings.Contains(message, "quota backend unavailable") {
		t.Fatalf("quota error = %q", message)
	}
	if at == nil {
		t.Fatalf("expected quota error timestamp, metadata = %#v", saved.Credential.Metadata)
	}
	if saved.Status != core.AccountStatusActive {
		t.Fatalf("status = %q, want active for transient refresh error", saved.Status)
	}
}

func TestRefreshAccountQuotaDoesNotMarkQuotaFetchAuthErrorExpired(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_oauth_fetch_error",
		Provider: core.ProviderOpenAI,
		Label:    "OAuth Account",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			Mode:        "openai-oauth-device",
			AccessToken: "oauth-token",
			Metadata: map[string]string{
				"token_source":                          "openai_device_code",
				core.AccountQuotaErrorStatusMetadataKey: string(core.AccountStatusExpired),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&testQuotaAuthFailAdapter{}))
	_, _, err := service.RefreshAccountQuota(context.Background(), "acct_oauth_fetch_error")
	if err == nil {
		t.Fatal("expected refresh error")
	}

	saved, err := repo.GetAccount("acct_oauth_fetch_error")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != core.AccountStatusActive {
		t.Fatalf("status = %q, want %q", saved.Status, core.AccountStatusActive)
	}
	if saved.Credential.Metadata[core.AccountQuotaErrorCodeMetadataKey] != "credential_expired" {
		t.Fatalf("metadata = %#v, want credential_expired code", saved.Credential.Metadata)
	}
	if saved.Credential.Metadata[core.AccountQuotaErrorStatusMetadataKey] != "" {
		t.Fatalf("metadata = %#v, want no quota error status", saved.Credential.Metadata)
	}
	if got := AccountRuntimeStatus(saved); got != string(core.AccountStatusActive) {
		t.Fatalf("runtime status = %q, want active", got)
	}
	if got := AccountFilterStatus(saved); got != "normal" {
		t.Fatalf("filter status = %q, want normal", got)
	}
}

func TestCredentialRefreshFailurePreservesExistingTerminalStatus(t *testing.T) {
	future := time.Now().UTC().Add(time.Hour)
	for _, status := range []core.AccountStatus{core.AccountStatusExpired, core.AccountStatusProviderBanned} {
		account := providers.ApplyCredentialRefreshFailureStatus(core.Account{
			Status:        status,
			CooldownUntil: &future,
		}, &providers.InvokeError{
			Code:      "upstream_transport_error",
			Temporary: true,
			Err:       errors.New("temporary network error"),
		}, time.Now().UTC())
		if account.Status != status {
			t.Fatalf("status = %q, want %q", account.Status, status)
		}
		if account.CooldownUntil != nil {
			t.Fatalf("cooldown = %#v, want nil", account.CooldownUntil)
		}
	}
}

func TestRefreshAccountQuotaMarksCredentialRefreshErrorExpired(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_oauth_expired",
		Provider: core.ProviderOpenAI,
		Label:    "OAuth Account",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			Mode:        "openai-oauth-device",
			AccessToken: "oauth-token",
			ExpiresAt:   ptrTime(time.Now().UTC().Add(-time.Minute)),
			Metadata: map[string]string{
				"token_source": "openai_device_code",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&testRefreshAuthFailAdapter{}))
	_, _, err := service.RefreshAccountQuota(context.Background(), "acct_oauth_expired")
	if err == nil {
		t.Fatal("expected refresh error")
	}

	saved, err := repo.GetAccount("acct_oauth_expired")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != core.AccountStatusExpired {
		t.Fatalf("status = %q, want %q", saved.Status, core.AccountStatusExpired)
	}
	if saved.Credential.Metadata[core.AccountQuotaErrorCodeMetadataKey] != "credential_expired" ||
		saved.Credential.Metadata[core.AccountQuotaErrorStatusMetadataKey] != string(core.AccountStatusExpired) {
		t.Fatalf("metadata = %#v, want credential_expired terminal quota metadata", saved.Credential.Metadata)
	}
	if got := AccountRuntimeStatus(saved); got != string(core.AccountStatusExpired) {
		t.Fatalf("runtime status = %q, want expired", got)
	}
	if got := AccountFilterStatus(saved); got != "exception" {
		t.Fatalf("filter status = %q, want exception", got)
	}
}

func TestRefreshAccountQuotaSuccessClearsQuotaRefreshFailureStatus(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_oauth_recovered",
		Provider: core.ProviderOpenAI,
		Label:    "OAuth Account",
		Status:   core.AccountStatusExpired,
		Credential: core.Credential{
			Mode:        "openai-oauth-device",
			AccessToken: "stale-token",
			ExpiresAt:   ptrTime(time.Now().UTC().Add(-time.Minute)),
			Metadata: map[string]string{
				"token_source":                          "openai_device_code",
				core.AccountQuotaErrorMetadataKey:       "credential_expired: refresh token was already used",
				core.AccountQuotaErrorCodeMetadataKey:   "credential_expired",
				core.AccountQuotaErrorStatusMetadataKey: string(core.AccountStatusExpired),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&testQuotaAdapter{}))
	account, _, err := service.RefreshAccountQuota(context.Background(), "acct_oauth_recovered")
	if err != nil {
		t.Fatalf("RefreshAccountQuota returned error: %v", err)
	}
	if account.Status != core.AccountStatusActive {
		t.Fatalf("status = %q, want active", account.Status)
	}
	if account.Credential.Metadata[core.AccountQuotaErrorMetadataKey] != "" ||
		account.Credential.Metadata[core.AccountQuotaErrorCodeMetadataKey] != "" ||
		account.Credential.Metadata[core.AccountQuotaErrorStatusMetadataKey] != "" {
		t.Fatalf("quota refresh error metadata was not cleared: %#v", account.Credential.Metadata)
	}
}

func TestApplyAccountBatchDisableDeduplicatesAndPreservesRuntimeState(t *testing.T) {
	repo := storage.NewMemoryRepository()
	cooldownUntil := time.Now().UTC().Add(10 * time.Minute)
	for _, account := range []core.Account{
		{ID: "acct_a", Provider: core.ProviderOpenAI, Label: "A", Status: core.AccountStatusActive},
		{ID: "acct_b", Provider: core.ProviderOpenAI, Label: "B", Status: core.AccountStatusCooling, CooldownUntil: &cooldownUntil},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	result, err := service.ApplyAccountBatch(context.Background(), AccountBatchInput{
		Action:     AccountBatchActionDisable,
		AccountIDs: []string{"acct_b", "acct_a", "acct_a", ""},
	})
	if err != nil {
		t.Fatalf("ApplyAccountBatch returned error: %v", err)
	}
	if result.Total != 2 || result.Succeeded != 2 || result.Failed != 0 || result.Skipped != 0 {
		t.Fatalf("batch result = %#v", result)
	}
	for _, id := range []string{"acct_a", "acct_b"} {
		account, err := repo.GetAccount(id)
		if err != nil {
			t.Fatal(err)
		}
		if !account.ControlDisabled {
			t.Fatalf("%s account = %#v, want control disabled", id, account)
		}
		if id == "acct_b" && (account.Status != core.AccountStatusCooling || account.CooldownUntil == nil) {
			t.Fatalf("%s account = %#v, want runtime cooldown preserved", id, account)
		}
	}
}

func TestApplyAccountBatchEnablePreservesRuntimeState(t *testing.T) {
	repo := storage.NewMemoryRepository()
	cooldownUntil := time.Now().UTC().Add(10 * time.Minute)
	if err := repo.UpsertAccount(core.Account{
		ID:               "acct_enable",
		Provider:         core.ProviderOpenAI,
		Label:            "Enable",
		Status:           core.AccountStatusCooling,
		ControlDisabled:  true,
		CooldownUntil:    &cooldownUntil,
		ConsecutiveFails: 3,
		TotalFails:       9,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	result, err := service.ApplyAccountBatch(context.Background(), AccountBatchInput{
		Action:     AccountBatchActionEnable,
		AccountIDs: []string{"acct_enable"},
	})
	if err != nil {
		t.Fatalf("ApplyAccountBatch returned error: %v", err)
	}
	if result.Succeeded != 1 || result.Failed != 0 || result.Skipped != 0 {
		t.Fatalf("batch result = %#v", result)
	}
	account, err := repo.GetAccount("acct_enable")
	if err != nil {
		t.Fatal(err)
	}
	if account.ControlDisabled {
		t.Fatalf("account = %#v, want control enabled", account)
	}
	if account.Status != core.AccountStatusCooling || account.CooldownUntil == nil || account.ConsecutiveFails != 3 {
		t.Fatalf("account = %#v, want runtime cooldown and failures preserved", account)
	}
	if account.TotalFails != 9 {
		t.Fatalf("total failures = %d, want preserved total", account.TotalFails)
	}
}

func TestApplyAccountBatchDeleteDeletesSelected(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{ID: "acct_a", Provider: core.ProviderOpenAI, Label: "A", Status: core.AccountStatusActive},
		{ID: "acct_b", Provider: core.ProviderOpenAI, Label: "B", Status: core.AccountStatusActive},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	result, err := service.ApplyAccountBatch(context.Background(), AccountBatchInput{
		Action:     AccountBatchActionDelete,
		AccountIDs: []string{"acct_a", "acct_b"},
	})
	if err != nil {
		t.Fatalf("ApplyAccountBatch returned error: %v", err)
	}
	if result.Succeeded != 2 || result.Failed != 0 || result.Skipped != 0 {
		t.Fatalf("batch result = %#v", result)
	}
	for _, id := range []string{"acct_a", "acct_b"} {
		if _, err := repo.GetAccount(id); err == nil {
			t.Fatalf("expected %s to be deleted", id)
		}
	}
}

func TestApplyAccountBatchMoveGroupPreservesAccountState(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, group := range []core.AccountGroup{
		{ID: "group_blue", Name: "Blue"},
		{ID: "group_green", Name: "Green"},
	} {
		if err := repo.UpsertAccountGroup(group); err != nil {
			t.Fatal(err)
		}
	}
	cooldownUntil := time.Now().UTC().Add(10 * time.Minute)
	for _, account := range []core.Account{
		{
			ID:              "acct_a",
			Provider:        core.ProviderOpenAI,
			Label:           "A",
			Group:           "Blue",
			Status:          core.AccountStatusCooling,
			ControlDisabled: true,
			CooldownUntil:   &cooldownUntil,
			TotalFails:      7,
		},
		{
			ID:       "acct_b",
			Provider: core.ProviderClaude,
			Label:    "B",
			Group:    "Blue",
			Status:   core.AccountStatusExpired,
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{}))
	result, err := service.ApplyAccountBatch(context.Background(), AccountBatchInput{
		Action:      AccountBatchActionMoveGroup,
		AccountIDs:  []string{"acct_a", "acct_b", "acct_a"},
		TargetGroup: "green",
	})
	if err != nil {
		t.Fatalf("ApplyAccountBatch returned error: %v", err)
	}
	if result.Total != 2 || result.Succeeded != 2 || result.Failed != 0 || result.Skipped != 0 || result.TargetGroup != "Green" {
		t.Fatalf("batch result = %#v", result)
	}
	accountA, err := repo.GetAccount("acct_a")
	if err != nil {
		t.Fatal(err)
	}
	if accountA.Group != "Green" || !accountA.ControlDisabled || accountA.Status != core.AccountStatusCooling || accountA.CooldownUntil == nil || accountA.TotalFails != 7 {
		t.Fatalf("acct_a = %#v, want group moved and state preserved", accountA)
	}
	accountB, err := repo.GetAccount("acct_b")
	if err != nil {
		t.Fatal(err)
	}
	if accountB.Group != "Green" || accountB.Status != core.AccountStatusExpired {
		t.Fatalf("acct_b = %#v, want group moved and runtime status preserved", accountB)
	}
	if _, err := service.ApplyAccountBatch(context.Background(), AccountBatchInput{
		Action:      AccountBatchActionMoveGroup,
		AccountIDs:  []string{"acct_a"},
		TargetGroup: "Missing",
	}); err == nil {
		t.Fatal("expected missing target group to be rejected")
	}
}

func TestApplyAccountBatchRefreshQuotaSkipsUnsupportedAccounts(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_oauth",
		Provider: core.ProviderOpenAI,
		Label:    "OAuth",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			AccessToken: "fresh-token",
			Metadata: map[string]string{
				"token_source": "openai_device_code",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_claude",
		Provider: core.ProviderClaude,
		Label:    "Claude",
		Status:   core.AccountStatusActive,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&testQuotaAdapter{}))
	result, err := service.ApplyAccountBatch(context.Background(), AccountBatchInput{
		Action:     AccountBatchActionRefreshQuota,
		AccountIDs: []string{"acct_oauth", "acct_claude"},
	})
	if err != nil {
		t.Fatalf("ApplyAccountBatch returned error: %v", err)
	}
	if result.Succeeded != 1 || result.Skipped != 1 || result.Failed != 0 {
		t.Fatalf("batch result = %#v", result)
	}
	account, err := repo.GetAccount("acct_oauth")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := ReadAccountQuota(account); snapshot == nil || snapshot.Plan != "pro" {
		t.Fatalf("quota snapshot = %#v", snapshot)
	}
}

func TestApplyAccountBatchTestReportsPerAccountFailures(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{
			ID:       "acct_ok",
			Provider: core.ProviderOpenAI,
			Label:    "OK",
			Group:    "Plus",
			Status:   core.AccountStatusActive,
			Credential: core.Credential{
				AccessToken: "stale-token",
				ExpiresAt:   ptrTime(time.Now().UTC().Add(time.Second)),
				Metadata: map[string]string{
					"token_source": providers.OpenAIDeviceCodeTokenSourceValue(),
				},
			},
		},
		{
			ID:       "acct_fail",
			Provider: core.ProviderOpenAI,
			Label:    "Fail",
			Group:    "Hidden",
			Status:   core.AccountStatusActive,
			Credential: core.Credential{
				AccessToken: "token",
				Metadata: map[string]string{
					"token_source": "api_key",
				},
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	adapter := &testQuotaAdapter{}
	service := New(repo, providers.NewRegistry(adapter))
	result, err := service.ApplyAccountBatch(context.Background(), AccountBatchInput{
		Action:     AccountBatchActionTest,
		AccountIDs: []string{"acct_ok", "acct_fail"},
	})
	if err != nil {
		t.Fatalf("ApplyAccountBatch returned error: %v", err)
	}
	if result.Succeeded != 1 || result.Failed != 1 || result.Skipped != 0 {
		t.Fatalf("batch result = %#v", result)
	}
	if result.Items[0].Message == "" || !strings.Contains(result.Items[1].Message, "ping backend unavailable") {
		t.Fatalf("batch items = %#v", result.Items)
	}
	failed, err := repo.GetAccount("acct_fail")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != core.AccountStatusActive || failed.CooldownUntil != nil {
		t.Fatalf("failed account status = %s cooldown=%#v, want active without cooldown", failed.Status, failed.CooldownUntil)
	}
	if got := AccountFilterStatus(failed); got != "normal" {
		t.Fatalf("failed account filter status = %q, want normal", got)
	}
	ok, err := repo.GetAccount("acct_ok")
	if err != nil {
		t.Fatal(err)
	}
	if got := AccountFilterStatus(ok); got != "normal" {
		t.Fatalf("ok account filter status = %q, want normal", got)
	}
}

func TestSyncProviderModelsTriesNextAccountWhenFirstLacksScope(t *testing.T) {
	repo := storage.NewMemoryRepository()
	for _, account := range []core.Account{
		{
			ID:       "acct_bad",
			Provider: core.ProviderOpenAI,
			Label:    "No Scope",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
			Credential: core.Credential{
				AccessToken: "bad-token",
			},
		},
		{
			ID:       "acct_good",
			Provider: core.ProviderOpenAI,
			Label:    "Model Reader",
			Status:   core.AccountStatusActive,
			Priority: 90,
			Weight:   100,
			Credential: core.Credential{
				AccessToken: "good-token",
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	service := New(repo, providers.NewRegistry(&testModelListingAdapter{}))
	result, err := service.SyncProviderModels(context.Background(), core.ProviderOpenAI)
	if err != nil {
		t.Fatalf("SyncProviderModels returned error: %v", err)
	}
	if result.Imported != 1 || result.Updated != 0 || result.Skipped != 0 {
		t.Fatalf("sync result = %#v", result)
	}
	model, err := repo.GetModel("gpt-test")
	if err != nil {
		t.Fatalf("GetModel returned error: %v", err)
	}
	if model.Enabled {
		t.Fatal("synced model should be disabled until explicitly exposed")
	}
}

type testQuotaAdapter struct{}

func (a *testQuotaAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *testQuotaAdapter) DisplayName() string { return "OpenAI" }

func (a *testQuotaAdapter) ListModels(context.Context) []core.ModelSpec { return nil }

func (a *testQuotaAdapter) Invoke(_ context.Context, decision core.RouteDecision, req *core.GatewayRequest) (*core.GatewayResponse, error) {
	return testDetectionInvoke(decision, req)
}

func (a *testQuotaAdapter) OpenStream(_ context.Context, decision core.RouteDecision, req *core.GatewayRequest) (*providers.StreamSession, error) {
	if decision.Account.ID == "acct_fail" {
		return nil, errors.New("ping backend unavailable")
	}
	if decision.Account.Credential.AccessToken == "stale-token" {
		return nil, errors.New("ping used stale access token")
	}
	if strings.TrimSpace(decision.Model) == "" {
		return nil, errors.New("missing detection model")
	}
	if req == nil || len(req.Messages) == 0 || strings.TrimSpace(req.Messages[0].Content) == "" {
		return nil, errors.New("missing detection prompt")
	}
	return testOpenAICompletedStreamSession(decision), nil
}

func (a *testQuotaAdapter) FetchModels(_ context.Context, account core.Account) ([]providers.UpstreamModel, error) {
	if account.ID == "acct_fail" {
		return nil, errors.New("ping backend unavailable")
	}
	if account.Credential.AccessToken == "stale-token" {
		return nil, errors.New("ping used stale access token")
	}
	return []providers.UpstreamModel{{ID: "gpt-test", OwnedBy: string(account.Provider)}}, nil
}

func (a *testQuotaAdapter) Refresh(_ context.Context, account core.Account) (core.Credential, error) {
	credential := account.Credential
	credential.AccessToken = "fresh-token"
	if credential.Metadata == nil {
		credential.Metadata = map[string]string{}
	}
	credential.Metadata["oauth_refreshed_at"] = time.Now().UTC().Format(time.RFC3339)
	return credential, nil
}

func (a *testQuotaAdapter) FetchQuota(_ context.Context, account core.Account) (core.AccountQuotaSnapshot, error) {
	if account.Credential.AccessToken != "fresh-token" {
		return core.AccountQuotaSnapshot{}, os.ErrInvalid
	}
	refreshedAt := time.Now().UTC()
	return core.AccountQuotaSnapshot{
		Source:      "test",
		Plan:        "pro",
		RefreshedAt: &refreshedAt,
	}, nil
}

type testAccountInvokeAdapter struct {
	model        string
	message      string
	upstreamMode string
	metadata     map[string]string
}

func (a *testAccountInvokeAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *testAccountInvokeAdapter) DisplayName() string { return "OpenAI" }

func (a *testAccountInvokeAdapter) ListModels(context.Context) []core.ModelSpec { return nil }

func (a *testAccountInvokeAdapter) Invoke(_ context.Context, decision core.RouteDecision, req *core.GatewayRequest) (*core.GatewayResponse, error) {
	return testDetectionInvoke(decision, req)
}

func (a *testAccountInvokeAdapter) OpenStream(_ context.Context, decision core.RouteDecision, req *core.GatewayRequest) (*providers.StreamSession, error) {
	a.model = decision.Model
	if req != nil {
		a.upstreamMode = req.UpstreamMode
		a.metadata = req.Metadata
	}
	if req != nil && len(req.Messages) > 0 {
		a.message = req.Messages[0].Content
	}
	return testOpenAICompletedStreamSession(decision), nil
}

type testClaudeAccountInvokeAdapter struct {
	testAccountInvokeAdapter
}

func (a *testClaudeAccountInvokeAdapter) Kind() core.ProviderKind { return core.ProviderClaude }

func (a *testClaudeAccountInvokeAdapter) DisplayName() string { return "Claude" }

type testOpenAIDeltaOnlyAdapter struct {
	testAccountInvokeAdapter
}

func (a *testOpenAIDeltaOnlyAdapter) OpenStream(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*providers.StreamSession, error) {
	return testStreamSession(decision, &core.StreamEvent{
		Delta:    "pong",
		RawEvent: "response.output_text.delta",
		RawData:  []byte(`{"type":"response.output_text.delta","delta":"pong"}`),
	}), nil
}

type testOpenAIFailedAdapter struct {
	testAccountInvokeAdapter
}

func (a *testOpenAIFailedAdapter) OpenStream(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*providers.StreamSession, error) {
	return testStreamSession(decision, &core.StreamEvent{
		FinishReason: "failed",
		Done:         true,
		RawEvent:     "response.failed",
		RawData:      []byte(`{"type":"response.failed","response":{"status":"failed"}}`),
	}), nil
}

type testModelListingAdapter struct{}

func (a *testModelListingAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *testModelListingAdapter) DisplayName() string { return "OpenAI" }

func (a *testModelListingAdapter) ListModels(context.Context) []core.ModelSpec { return nil }

func (a *testModelListingAdapter) Invoke(context.Context, core.RouteDecision, *core.GatewayRequest) (*core.GatewayResponse, error) {
	return nil, nil
}

func (a *testModelListingAdapter) FetchModels(_ context.Context, account core.Account) ([]providers.UpstreamModel, error) {
	if account.Credential.AccessToken == "bad-token" {
		return nil, errors.New("upstream_auth_error: Missing scopes: api.model.read")
	}
	return []providers.UpstreamModel{{ID: "gpt-test", OwnedBy: "openai"}}, nil
}

type testProviderModelListingAdapter struct {
	kind    core.ProviderKind
	label   string
	modelID string
	ownedBy string
}

func (a *testProviderModelListingAdapter) Kind() core.ProviderKind { return a.kind }

func (a *testProviderModelListingAdapter) DisplayName() string { return a.label }

func (a *testProviderModelListingAdapter) ListModels(context.Context) []core.ModelSpec { return nil }

func (a *testProviderModelListingAdapter) Invoke(context.Context, core.RouteDecision, *core.GatewayRequest) (*core.GatewayResponse, error) {
	return nil, nil
}

func (a *testProviderModelListingAdapter) FetchModels(context.Context, core.Account) ([]providers.UpstreamModel, error) {
	return []providers.UpstreamModel{{ID: a.modelID, OwnedBy: a.ownedBy}}, nil
}

type testEmptyStreamAdapter struct {
	testAccountInvokeAdapter
}

func (a *testEmptyStreamAdapter) OpenStream(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*providers.StreamSession, error) {
	return testStreamSession(decision), nil
}

type testQuotaFailAdapter struct{}

func (a *testQuotaFailAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *testQuotaFailAdapter) DisplayName() string { return "OpenAI" }

func (a *testQuotaFailAdapter) ListModels(context.Context) []core.ModelSpec { return nil }

func (a *testQuotaFailAdapter) Invoke(context.Context, core.RouteDecision, *core.GatewayRequest) (*core.GatewayResponse, error) {
	return nil, errors.New("ping backend unavailable")
}

func (a *testQuotaFailAdapter) OpenStream(context.Context, core.RouteDecision, *core.GatewayRequest) (*providers.StreamSession, error) {
	return nil, errors.New("ping backend unavailable")
}

func (a *testQuotaFailAdapter) FetchModels(context.Context, core.Account) ([]providers.UpstreamModel, error) {
	return nil, errors.New("ping backend unavailable")
}

func (a *testQuotaFailAdapter) FetchQuota(context.Context, core.Account) (core.AccountQuotaSnapshot, error) {
	return core.AccountQuotaSnapshot{}, errors.New("quota backend unavailable")
}

type testQuotaAuthFailAdapter struct{}

func (a *testQuotaAuthFailAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *testQuotaAuthFailAdapter) DisplayName() string { return "OpenAI" }

func (a *testQuotaAuthFailAdapter) ListModels(context.Context) []core.ModelSpec { return nil }

func (a *testQuotaAuthFailAdapter) Invoke(context.Context, core.RouteDecision, *core.GatewayRequest) (*core.GatewayResponse, error) {
	return nil, &providers.InvokeError{
		Code:      "credential_expired",
		Temporary: false,
		Err:       errors.New("refresh token was already used"),
	}
}

func (a *testQuotaAuthFailAdapter) OpenStream(context.Context, core.RouteDecision, *core.GatewayRequest) (*providers.StreamSession, error) {
	return nil, &providers.InvokeError{
		Code:      "credential_expired",
		Temporary: false,
		Err:       errors.New("refresh token was already used"),
	}
}

func (a *testQuotaAuthFailAdapter) FetchModels(context.Context, core.Account) ([]providers.UpstreamModel, error) {
	return nil, &providers.InvokeError{
		Code:      "credential_expired",
		Temporary: false,
		Err:       errors.New("refresh token was already used"),
	}
}

func (a *testQuotaAuthFailAdapter) FetchQuota(context.Context, core.Account) (core.AccountQuotaSnapshot, error) {
	return core.AccountQuotaSnapshot{}, &providers.InvokeError{
		Code:      "credential_expired",
		Temporary: false,
		Err:       errors.New("refresh token was already used"),
	}
}

type testRefreshAuthFailAdapter struct {
	testQuotaAdapter
}

func (a *testRefreshAuthFailAdapter) Refresh(context.Context, core.Account) (core.Credential, error) {
	return core.Credential{}, &providers.InvokeError{
		Code:      "credential_expired",
		Temporary: false,
		Err:       errors.New("refresh token was already used"),
	}
}

func testDetectionInvoke(decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
	return &core.GatewayResponse{
		ID:           "resp_test",
		Model:        decision.Model,
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Content:      "pong",
		CreatedAt:    time.Now().UTC(),
	}, nil
}

func testStreamSession(decision core.RouteDecision, events ...*core.StreamEvent) *providers.StreamSession {
	return &providers.StreamSession{
		Response: &core.GatewayResponse{
			ID:           "resp_stream_test",
			Model:        decision.Model,
			Provider:     decision.Provider,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			CreatedAt:    time.Now().UTC(),
		},
		Stream: &testStream{events: events},
	}
}

func testOpenAICompletedStreamSession(decision core.RouteDecision) *providers.StreamSession {
	return testStreamSession(decision,
		&core.StreamEvent{
			Delta:    "pong",
			RawEvent: "response.output_text.delta",
			RawData:  []byte(`{"type":"response.output_text.delta","delta":"pong"}`),
		},
		&core.StreamEvent{
			FinishReason: "stop",
			Done:         true,
			RawEvent:     "response.completed",
			RawData:      []byte(`{"type":"response.completed","response":{"status":"completed"}}`),
		},
	)
}

type testStream struct {
	events []*core.StreamEvent
	index  int
}

func (s *testStream) Next() (*core.StreamEvent, error) {
	if s.index >= len(s.events) {
		return nil, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *testStream) Close() error {
	return nil
}

func ptrTime(value time.Time) *time.Time {
	return &value
}
