package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestSQLiteRepositoryListUsersPageFiltersSortsAndSummarizes(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	users := []core.User{
		{ID: "user_old", Username: "old-spender", Role: core.UserRoleUser, Enabled: true, BalanceNanoUSD: 1000, CreatedAt: base.Add(-2 * time.Hour), UpdatedAt: base.Add(-2 * time.Hour)},
		{ID: "user_mid", Username: "mid-invited", Role: core.UserRoleUser, Enabled: true, InviterUserID: "user_old", CreatedAt: base.Add(-time.Hour), UpdatedAt: base.Add(-time.Hour)},
		{ID: "user_new", Username: "new-disabled", Role: core.UserRoleAdmin, Enabled: false, CreatedAt: base, UpdatedAt: base},
	}
	for _, user := range users {
		if err := repo.UpsertUser(user); err != nil {
			t.Fatalf("UpsertUser(%s) returned error: %v", user.ID, err)
		}
	}

	const clientID = "client_user_page_spend"
	if err := repo.UpsertClient(core.APIClient{ID: clientID, Name: "User Page Spend", APIKey: "gw_user_page_spend", OwnerUserID: "user_old", Enabled: true}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	spend := int64(123)
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_user_page_spend",
		ClientID:        clientID,
		ClientName:      "User Page Spend",
		UserID:          "user_old",
		Model:           "gpt-4.1",
		ReservedNanoUSD: spend,
		Fingerprint:     "req_user_page_spend",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_user_page_spend",
		ClientID:      clientID,
		Model:         "gpt-4.1",
		ActualNanoUSD: spend,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}

	page, filtered, total := repo.ListUsersPage(UserListQuery{Sort: "created_at", Direction: "desc", Offset: 0, Limit: 2})
	if total != 3 || filtered != 3 {
		t.Fatalf("counts filtered=%d total=%d, want 3/3", filtered, total)
	}
	if got := userListItemIDs(page); len(got) != 2 || got[0] != "user_new" || got[1] != "user_mid" {
		t.Fatalf("created desc page ids = %#v, want user_new,user_mid", got)
	}

	page, filtered, total = repo.ListUsersPage(UserListQuery{Sort: "spend", Direction: "desc", Offset: 0, Limit: 1})
	if total != 3 || filtered != 3 || len(page) != 1 || page[0].User.ID != "user_old" || page[0].SpendNanoUSD != spend || page[0].InviteCount != 1 {
		t.Fatalf("spend page=%#v filtered=%d total=%d", page, filtered, total)
	}

	page, filtered, total = repo.ListUsersPage(UserListQuery{Inviter: "old-spender", Sort: "created_at", Direction: "desc", Offset: 0, Limit: 10})
	if total != 3 || filtered != 1 || len(page) != 1 || page[0].User.ID != "user_mid" {
		t.Fatalf("inviter filtered page=%#v filtered=%d total=%d", page, filtered, total)
	}

	page, filtered, total = repo.ListUsersPage(UserListQuery{Status: "disabled", Sort: "created_at", Direction: "desc", Offset: 0, Limit: 10})
	if total != 3 || filtered != 1 || len(page) != 1 || page[0].User.ID != "user_new" {
		t.Fatalf("disabled filtered page=%#v filtered=%d total=%d", page, filtered, total)
	}
}

func TestSQLiteRepositoryUserActualSpendTotalUsesCurrentClientOwner(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	oldOwner := core.User{ID: "user_old_owner", Username: "old-owner", Role: core.UserRoleUser, Enabled: true, BalanceNanoUSD: 1000}
	newOwner := core.User{ID: "user_new_owner", Username: "new-owner", Role: core.UserRoleUser, Enabled: true}
	for _, user := range []core.User{oldOwner, newOwner} {
		if err := repo.UpsertUser(user); err != nil {
			t.Fatalf("UpsertUser(%s) returned error: %v", user.ID, err)
		}
	}

	client := core.APIClient{ID: "client_owner_transfer", Name: "Owner Transfer", APIKey: "gw_owner_transfer", OwnerUserID: oldOwner.ID, Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatalf("UpsertClient(old owner) returned error: %v", err)
	}
	const spend = int64(321)
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_owner_transfer",
		ClientID:        client.ID,
		ClientName:      client.Name,
		UserID:          oldOwner.ID,
		Model:           "gpt-4.1",
		ReservedNanoUSD: spend,
		Fingerprint:     "req_owner_transfer",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_owner_transfer",
		ClientID:      client.ID,
		Model:         "gpt-4.1",
		ActualNanoUSD: spend,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}

	client.OwnerUserID = newOwner.ID
	if err := repo.UpsertClient(client); err != nil {
		t.Fatalf("UpsertClient(new owner) returned error: %v", err)
	}

	if got := repo.UserActualSpendTotal(newOwner.ID); got != spend {
		t.Fatalf("new owner spend = %d, want %d", got, spend)
	}
	if got := repo.UserActualSpendTotal(oldOwner.ID); got != 0 {
		t.Fatalf("old owner spend = %d, want 0", got)
	}
}

func userListItemIDs(items []UserListItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.User.ID)
	}
	return out
}
