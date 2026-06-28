package storage

import (
	"path/filepath"
	"testing"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestUsageLogRetentionCanDisable(t *testing.T) {
	repos := map[string]Repository{
		"memory": NewMemoryRepository(),
	}
	sqliteRepo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = sqliteRepo.Close() })
	repos["sqlite"] = sqliteRepo

	for name, repo := range repos {
		t.Run(name, func(t *testing.T) {
			if err := repo.UpsertUser(core.User{ID: "user_logs_disabled", Username: "logs-disabled", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
				t.Fatal(err)
			}
			if err := repo.UpsertClient(core.APIClient{ID: "client_logs_disabled", Name: "Logs Disabled", APIKey: "gw_logs_disabled", OwnerUserID: "user_logs_disabled", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			seedBillingRequest(t, repo, "req_disabled_settled", "client_logs_disabled", "user_logs_disabled", "gpt-4.1", 1000)
			seedZeroCostBillingRequest(t, repo, "req_disabled_zero", "client_logs_disabled", "user_logs_disabled", "gpt-4.1")
			if _, err := repo.ReserveBilling(core.BillingReservationInput{
				RequestID:       "req_disabled_reserved",
				ClientID:        "client_logs_disabled",
				UserID:          "user_logs_disabled",
				Model:           "gpt-4.1",
				ReservedNanoUSD: 100,
				Fingerprint:     "req_disabled_reserved",
			}); err != nil {
				t.Fatal(err)
			}
			if err := repo.ConfigureUsageLogRetention(0); err != nil {
				t.Fatalf("ConfigureUsageLogRetention returned error: %v", err)
			}

			items, total := repo.ListBillingRequestsPage(BillingRequestQuery{UserID: "user_logs_disabled", Limit: 10})
			if total != 1 || len(items) != 1 || items[0].RequestID != "req_disabled_reserved" {
				t.Fatalf("usage logs = total %d items %#v, want only active reserved request", total, items)
			}
		})
	}
}
