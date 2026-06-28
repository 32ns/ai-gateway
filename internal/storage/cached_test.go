package storage

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestCachedRepositoryIndexesClientKeys(t *testing.T) {
	base := NewMemoryRepository()
	if err := base.UpsertClient(core.APIClient{
		ID:      "client_a",
		Name:    "Client A",
		APIKey:  "old_key",
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	repo := NewCachedRepository(base)

	if _, err := repo.FindClientByAPIKey("old_key"); err != nil {
		t.Fatalf("FindClientByAPIKey returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:      "client_a",
		Name:    "Client A",
		APIKey:  "new_key",
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.FindClientByAPIKey("old_key"); err == nil {
		t.Fatal("old api key should not resolve after cache refresh")
	}
	if client, err := repo.FindClientByAPIKey("new_key"); err != nil || client.ID != "client_a" {
		t.Fatalf("new api key lookup = %#v, %v", client, err)
	}
}

func TestCachedRepositoryLoadsClientSummariesWithoutFullClientList(t *testing.T) {
	base := &clientSummaryRepository{MemoryRepository: NewMemoryRepository()}
	if err := base.UpsertClient(core.APIClient{
		ID:                "client_summary",
		Name:              "Summary Client",
		APIKey:            "gw_summary",
		OwnerUserID:       "user_summary",
		Enabled:           true,
		SpendLimitNanoUSD: 123,
		RoutePolicy:       core.DefaultRoutePolicy(),
		AccountGroup:      "summary",
	}); err != nil {
		t.Fatal(err)
	}

	repo := NewCachedRepository(base)
	clients := repo.ListClients()
	if len(clients) != 1 {
		t.Fatalf("len(clients) = %d, want 1", len(clients))
	}
	if clients[0].APIKey != "" {
		t.Fatalf("summary API key = %q, want empty", clients[0].APIKey)
	}
	if clients[0].AccountGroup != "summary" || clients[0].SpendLimitNanoUSD != 123 {
		t.Fatalf("summary client = %#v", clients[0])
	}

	client, err := repo.GetClient("client_summary")
	if err != nil {
		t.Fatalf("GetClient returned error: %v", err)
	}
	if client.APIKey != "gw_summary" {
		t.Fatalf("hydrated API key = %q", client.APIKey)
	}
	client, err = repo.FindClientByAPIKey("gw_summary")
	if err != nil {
		t.Fatalf("FindClientByAPIKey returned error: %v", err)
	}
	if client.ID != "client_summary" {
		t.Fatalf("hydrated client ID = %q", client.ID)
	}
	if clients := repo.ListClients(); len(clients) != 1 || clients[0].APIKey != "" {
		t.Fatalf("ListClients after hydrate = %#v, want summary without key", clients)
	}
}

func TestCachedRepositoryStartsWithoutFullUserOrClientLists(t *testing.T) {
	base := &cachedLazyLoadRepository{MemoryRepository: NewMemoryRepository()}
	admin := core.User{
		ID:           "user_admin",
		Username:     "Admin",
		PasswordHash: "admin-password-hash",
		Role:         core.UserRoleAdmin,
		Enabled:      true,
	}
	invited := core.User{
		ID:            "user_invited",
		Username:      "Invited",
		PasswordHash:  "invited-password-hash",
		Enabled:       true,
		InviterUserID: admin.ID,
		OAuthIdentities: []core.UserOAuthIdentity{{
			Provider: "github",
			Subject:  "oauth-subject",
		}},
	}
	if err := base.UpsertUser(admin); err != nil {
		t.Fatal(err)
	}
	if err := base.UpsertUser(invited); err != nil {
		t.Fatal(err)
	}
	if err := base.UpsertClient(core.APIClient{
		ID:          "client_owned",
		Name:        "Owned Client",
		APIKey:      "gw_owned",
		OwnerUserID: admin.ID,
		Enabled:     true,
	}); err != nil {
		t.Fatal(err)
	}

	repo := NewCachedRepository(base)
	if user, err := repo.GetUser(admin.ID); err != nil || user.ID != admin.ID {
		t.Fatalf("GetUser = %#v, %v", user, err)
	}
	if user, err := repo.FindUserByUsername("invited"); err != nil || user.ID != invited.ID {
		t.Fatalf("FindUserByUsername = %#v, %v", user, err)
	}
	if user, err := repo.FindUserByOAuthIdentity("GitHub", "oauth-subject"); err != nil || user.ID != invited.ID {
		t.Fatalf("FindUserByOAuthIdentity = %#v, %v", user, err)
	}
	if user, err := repo.FindUserByInvitationSignature(core.UserInvitationSignature(invited)); err != nil || user.ID != invited.ID {
		t.Fatalf("FindUserByInvitationSignature = %#v, %v", user, err)
	}
	if got := repo.CountUsersByInviter(admin.ID); got != 1 {
		t.Fatalf("CountUsersByInviter = %d, want 1", got)
	}
	if users := repo.ListUsersByInviter(admin.ID); len(users) != 1 || users[0].ID != invited.ID {
		t.Fatalf("ListUsersByInviter = %#v", users)
	}
	if got := repo.CountEnabledAdminsExcluding(nil); got != 1 {
		t.Fatalf("CountEnabledAdminsExcluding(nil) = %d, want 1", got)
	}
	if got := repo.CountEnabledAdminsExcluding([]string{admin.ID}); got != 0 {
		t.Fatalf("CountEnabledAdminsExcluding(admin) = %d, want 0", got)
	}
	if client, err := repo.GetClient("client_owned"); err != nil || client.APIKey != "gw_owned" {
		t.Fatalf("GetClient = %#v, %v", client, err)
	}
	if client, err := repo.FindClientByAPIKey("gw_owned"); err != nil || client.ID != "client_owned" {
		t.Fatalf("FindClientByAPIKey = %#v, %v", client, err)
	}
	if clients := repo.ListClientsByOwner(admin.ID); len(clients) != 1 || clients[0].ID != "client_owned" || clients[0].APIKey != "" {
		t.Fatalf("ListClientsByOwner = %#v", clients)
	}
	if clients, total := repo.ListClientSummariesPage(0, 1); total != 1 || len(clients) != 1 || clients[0].APIKey != "" {
		t.Fatalf("ListClientSummariesPage = total %d clients %#v", total, clients)
	}
	if clients := repo.ListClients(); len(clients) != 1 || clients[0].APIKey != "" {
		t.Fatalf("ListClients = %#v", clients)
	}
}

type clientSummaryRepository struct {
	*MemoryRepository
}

func (r *clientSummaryRepository) ListClients() []core.APIClient {
	panic("cached repository startup should use ListClientSummaries")
}

func (r *clientSummaryRepository) ListClientSummaries() []core.APIClient {
	clients := r.MemoryRepository.ListClients()
	for i := range clients {
		clients[i].APIKey = ""
	}
	return clients
}

type cachedLazyLoadRepository struct {
	*MemoryRepository
}

func (r *cachedLazyLoadRepository) ListUsers() []core.User {
	panic("cached repository should not require full user list")
}

func (r *cachedLazyLoadRepository) ListClients() []core.APIClient {
	panic("cached repository should not require full client list")
}

func (r *cachedLazyLoadRepository) ListClientSummaries() []core.APIClient {
	return cloneClientSummaries(r.MemoryRepository.ListClients())
}

func (r *cachedLazyLoadRepository) ListClientSummariesPage(offset, limit int) ([]core.APIClient, int) {
	return clientSummaryPage(r.MemoryRepository.ListClients(), offset, limit)
}

func TestCachedRepositoryMaintainsUserAndOwnerIndexes(t *testing.T) {
	base := NewMemoryRepository()
	admin := core.User{ID: "user_admin", Username: "admin", Role: core.UserRoleAdmin, Enabled: true}
	invited := core.User{ID: "user_invited", Username: "invited", Enabled: true, InviterUserID: "user_a"}
	if err := base.UpsertUser(admin); err != nil {
		t.Fatal(err)
	}
	if err := base.UpsertUser(invited); err != nil {
		t.Fatal(err)
	}
	if err := base.UpsertClient(core.APIClient{ID: "client_owner", Name: "Owner", APIKey: "gw_owner", OwnerUserID: "user_a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	repo := NewCachedRepository(base)

	if got := repo.CountUsersByInviter("user_a"); got != 1 {
		t.Fatalf("CountUsersByInviter(user_a) = %d, want 1", got)
	}
	if users := repo.ListUsersByInviter("user_a"); len(users) != 1 || users[0].ID != invited.ID {
		t.Fatalf("ListUsersByInviter(user_a) = %#v", users)
	}
	if got := repo.CountEnabledAdminsExcluding(nil); got != 1 {
		t.Fatalf("CountEnabledAdminsExcluding(nil) = %d, want 1", got)
	}
	if clients := repo.ListClientsByOwner("user_a"); len(clients) != 1 || clients[0].ID != "client_owner" {
		t.Fatalf("ListClientsByOwner(user_a) = %#v", clients)
	}

	invited.InviterUserID = "user_b"
	if err := repo.UpsertUser(invited); err != nil {
		t.Fatal(err)
	}
	if got := repo.CountUsersByInviter("user_a"); got != 0 {
		t.Fatalf("old inviter count = %d, want 0", got)
	}
	if got := repo.CountUsersByInviter("user_b"); got != 1 {
		t.Fatalf("new inviter count = %d, want 1", got)
	}
	admin.Enabled = false
	if err := repo.UpsertUser(admin); err != nil {
		t.Fatal(err)
	}
	if got := repo.CountEnabledAdminsExcluding(nil); got != 0 {
		t.Fatalf("enabled admin count = %d, want 0", got)
	}

	client := core.APIClient{ID: "client_owner", Name: "Owner", APIKey: "gw_owner", OwnerUserID: "user_b", Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatal(err)
	}
	if clients := repo.ListClientsByOwner("user_a"); len(clients) != 0 {
		t.Fatalf("old owner clients = %#v, want empty", clients)
	}
	if clients := repo.ListClientsByOwner("user_b"); len(clients) != 1 || clients[0].ID != "client_owner" {
		t.Fatalf("new owner clients = %#v", clients)
	}
	if err := repo.DeleteClient("client_owner"); err != nil {
		t.Fatal(err)
	}
	if clients := repo.ListClientsByOwner("user_b"); len(clients) != 0 {
		t.Fatalf("deleted owner clients = %#v, want empty", clients)
	}
}

func TestCachedRepositoryRevisionsAreScoped(t *testing.T) {
	base := NewMemoryRepository()
	repo := NewCachedRepository(base)

	initialConfigRev := repo.ConfigRevision()
	initialAccountRev := repo.AccountRevision()
	initialUserRev := repo.UserRevision()
	initialModelRev := repo.ModelRevision()
	initialClientRev := repo.ClientRevision()
	if initialConfigRev == 0 || initialAccountRev == 0 || initialUserRev == 0 || initialModelRev == 0 || initialClientRev == 0 {
		t.Fatalf("initial revisions must be non-zero: config=%d account=%d user=%d model=%d client=%d", initialConfigRev, initialAccountRev, initialUserRev, initialModelRev, initialClientRev)
	}

	if err := repo.UpsertAccount(core.Account{ID: "acct_a", Provider: core.ProviderOpenAI, Status: core.AccountStatusActive}); err != nil {
		t.Fatal(err)
	}
	if repo.AccountRevision() == initialAccountRev {
		t.Fatal("account revision should change after account update")
	}
	if repo.ModelRevision() != initialModelRev {
		t.Fatal("model revision should not change after account update")
	}
	if repo.ClientRevision() != initialClientRev {
		t.Fatal("client revision should not change after account update")
	}
	if repo.UserRevision() != initialUserRev {
		t.Fatal("user revision should not change after account update")
	}
	accountRev := repo.AccountRevision()
	if err := repo.UpsertModel(core.ModelConfig{ID: "model_a", Provider: core.ProviderOpenAI, DisplayName: "Model A", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if repo.ModelRevision() == initialModelRev {
		t.Fatal("model revision should change after model update")
	}
	if repo.AccountRevision() != accountRev {
		t.Fatal("account revision should not change after model update")
	}
	modelRev := repo.ModelRevision()
	if err := repo.UpsertClient(core.APIClient{ID: "client_a", Name: "Client A", APIKey: "key_a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if repo.ClientRevision() == initialClientRev {
		t.Fatal("client revision should change after client update")
	}
	if repo.ModelRevision() != modelRev {
		t.Fatal("model revision should not change after client update")
	}

	if err := repo.UpsertUser(core.User{ID: "user_a", Username: "User A", Enabled: true, BalanceNanoUSD: 1000}); err != nil {
		t.Fatal(err)
	}
	if repo.UserRevision() == initialUserRev {
		t.Fatal("user revision should change after user update")
	}
	userRev := repo.UserRevision()
	if err := repo.SetUserBalance("user_a", 2000); err != nil {
		t.Fatal(err)
	}
	if repo.UserRevision() == userRev {
		t.Fatal("user revision should change after balance set")
	}
	userRev = repo.UserRevision()
	if _, _, err := repo.AdjustUserBalance("user_a", 1000, "top up"); err != nil {
		t.Fatal(err)
	}
	if repo.UserRevision() == userRev {
		t.Fatal("user revision should change after balance adjustment")
	}

	accountRev = repo.AccountRevision()
	settings := core.DefaultSystemSettings()
	settings.Network.SystemProxyURL = "http://127.0.0.1:7890"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	if repo.AccountRevision() == accountRev {
		t.Fatal("account revision should change after system proxy update")
	}
}

func TestCachedRepositoryDeletesClientCacheAndPreservesBilling(t *testing.T) {
	base := NewMemoryRepository()
	if err := base.UpsertUser(core.User{ID: "user_history", Username: "history", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := base.UpsertClient(core.APIClient{ID: "client_history", Name: "History", APIKey: "gw_history", OwnerUserID: "user_history", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := base.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_history",
		ClientID:        "client_history",
		UserID:          "user_history",
		ReservedNanoUSD: 100,
		Fingerprint:     "history",
	}); err != nil {
		t.Fatal(err)
	}
	repo := NewCachedRepository(base)

	if err := repo.DeleteClient("client_history"); err != nil {
		t.Fatalf("DeleteClient returned error: %v", err)
	}
	if _, err := repo.GetClient("client_history"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cached client err = %v, want ErrNotFound", err)
	}
	if _, err := repo.FindClientByAPIKey("gw_history"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cached api key err = %v, want ErrNotFound", err)
	}
	requests, total := repo.ListBillingRequestsPage(BillingRequestQuery{ClientID: "client_history", Limit: 10})
	if total != 1 || len(requests) != 1 || requests[0].RequestID != "req_history" {
		t.Fatalf("billing requests after client delete = total %d items %#v, want preserved req_history", total, requests)
	}
}

func TestCachedRepositoryDeleteUserClearsOwnedClientCache(t *testing.T) {
	base := NewMemoryRepository()
	if err := base.UpsertUser(core.User{ID: "user_owner", Username: "owner", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := base.UpsertClient(core.APIClient{ID: "client_owned", Name: "Owned", APIKey: "gw_owned", OwnerUserID: "user_owner", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	repo := NewCachedRepository(base)

	if _, err := repo.GetClient("client_owned"); err != nil {
		t.Fatalf("GetClient returned error: %v", err)
	}
	if _, err := repo.FindClientByAPIKey("gw_owned"); err != nil {
		t.Fatalf("FindClientByAPIKey returned error: %v", err)
	}
	if err := repo.DeleteUser("user_owner"); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	if _, err := repo.GetClient("client_owned"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetClient after user delete err = %v, want ErrNotFound", err)
	}
	if _, err := repo.FindClientByAPIKey("gw_owned"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindClientByAPIKey after user delete err = %v, want ErrNotFound", err)
	}
}

func TestCachedRepositoryUpsertUserAllowsExistingNegativeBalance(t *testing.T) {
	base := NewMemoryRepository()
	if err := base.UpsertUser(core.User{ID: "user_balance_cache", Username: "balance-cache", Enabled: true, BalanceNanoUSD: 6000}); err != nil {
		t.Fatal(err)
	}
	if err := base.UpsertClient(core.APIClient{ID: "client_balance_cache", Name: "Balance Cache", APIKey: "gw_balance_cache", OwnerUserID: "user_balance_cache", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := base.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_balance_cache",
		ClientID:        "client_balance_cache",
		UserID:          "user_balance_cache",
		ReservedNanoUSD: 5000,
		Fingerprint:     "balance-cache",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := base.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_balance_cache",
		ClientID:      "client_balance_cache",
		ActualNanoUSD: 7000,
	}); err != nil {
		t.Fatal(err)
	}
	repo := NewCachedRepository(base)

	user, err := repo.GetUser("user_balance_cache")
	if err != nil {
		t.Fatal(err)
	}
	user.Username = "balance-cache-renamed"
	if err := repo.UpsertUser(user); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	stored, err := repo.GetUser("user_balance_cache")
	if err != nil {
		t.Fatal(err)
	}
	if stored.BalanceNanoUSD != -1000 {
		t.Fatalf("balance = %d, want -1000", stored.BalanceNanoUSD)
	}
	if stored.Username != "balance-cache-renamed" {
		t.Fatalf("username = %q", stored.Username)
	}
}

func TestCachedRepositoryMergeUsersReloadsClientCache(t *testing.T) {
	base := NewMemoryRepository()
	source := core.User{ID: "user_source", Username: "source", Enabled: true, BalanceNanoUSD: 1000}
	target := core.User{ID: "user_target", Username: "target", Enabled: true, BalanceNanoUSD: 2000}
	if err := base.UpsertUser(source); err != nil {
		t.Fatal(err)
	}
	if err := base.UpsertUser(target); err != nil {
		t.Fatal(err)
	}
	if err := base.UpsertClient(core.APIClient{ID: "client_source", Name: "Source", APIKey: "gw_source", OwnerUserID: source.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	repo := NewCachedRepository(base)

	initialUserRev := repo.UserRevision()
	initialClientRev := repo.ClientRevision()
	source.Enabled = false
	source.BalanceNanoUSD = 0
	target.BalanceNanoUSD = 3000
	if err := repo.MergeUsers(source, target); err != nil {
		t.Fatal(err)
	}
	if repo.UserRevision() == initialUserRev || repo.ClientRevision() == initialClientRev {
		t.Fatalf("merge should refresh user and client revisions: user %d->%d client %d->%d", initialUserRev, repo.UserRevision(), initialClientRev, repo.ClientRevision())
	}
	client, err := repo.GetClient("client_source")
	if err != nil {
		t.Fatal(err)
	}
	if client.OwnerUserID != target.ID {
		t.Fatalf("cached client owner = %q, want %q", client.OwnerUserID, target.ID)
	}
	client, err = repo.FindClientByAPIKey("gw_source")
	if err != nil {
		t.Fatal(err)
	}
	if client.OwnerUserID != target.ID {
		t.Fatalf("cached api key owner = %q, want %q", client.OwnerUserID, target.ID)
	}
}

func TestAsyncAuditRepositoryDropsRequestBodiesFromHotPath(t *testing.T) {
	base := NewMemoryRepository()
	repo := NewAsyncAuditRepository(base, 8)
	largeMessage := strings.Repeat("x", maxAuditSummaryMessageRunes+16)
	if err := repo.AppendAudit(core.AuditEvent{
		ID:          "audit_a",
		Kind:        core.AuditKindGateway,
		Status:      "ok",
		Message:     largeMessage,
		RequestBody: `{"large":"request"}`,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	audits := base.ListAudit(1)
	if len(audits) != 1 {
		t.Fatalf("audit count = %d, want 1", len(audits))
	}
	if audits[0].RequestBody != "" {
		t.Fatalf("request body = %q, want empty", audits[0].RequestBody)
	}
	if len([]rune(audits[0].Message)) >= len([]rune(largeMessage)) || !strings.Contains(audits[0].Message, "[truncated]") {
		t.Fatalf("message was not truncated: len=%d value=%q", len([]rune(audits[0].Message)), audits[0].Message)
	}
}

func TestAsyncAuditRepositoryAppendsAdminAuditAfterClose(t *testing.T) {
	base := NewMemoryRepository()
	repo := NewAsyncAuditRepository(base, 1)
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	if err := repo.AppendAudit(core.AuditEvent{
		ID:        "audit_admin_after_close",
		Kind:      core.AuditKindAdmin,
		Action:    "settings.update",
		Status:    "ok",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	audits := base.ListAudit(1)
	if len(audits) != 1 {
		t.Fatalf("audit count = %d, want 1", len(audits))
	}
	if audits[0].ID != "audit_admin_after_close" {
		t.Fatalf("audit ID = %q", audits[0].ID)
	}
}
