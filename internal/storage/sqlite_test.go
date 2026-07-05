package storage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestSQLiteRepositoryEnablesForeignKeysOnPooledConnections(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	ctx := context.Background()
	conn, err := repo.db.Conn(ctx)
	if err != nil {
		t.Fatalf("hold first sqlite connection: %v", err)
	}
	defer conn.Close()
	var heldForeignKeys int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&heldForeignKeys); err != nil {
		t.Fatalf("query held connection foreign_keys: %v", err)
	}
	if heldForeignKeys != 1 {
		t.Fatalf("held connection foreign_keys = %d, want 1", heldForeignKeys)
	}

	_, err = repo.db.Exec(`INSERT INTO user_balances(user_id, balance_nano_usd, updated_at_ns) VALUES(?, ?, ?)`, "missing_user", 1, time.Now().UnixNano())
	if err == nil {
		t.Fatal("expected pooled sqlite connection to enforce foreign key constraints")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("orphan insert err = %v, want foreign key error", err)
	}
}

func TestSQLiteRepositoryMigratesBillingPlanGroupQuotaPriceRatio(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	db, err := sql.Open("sqlite", sqliteOpenDSN(statePath))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE billing_plan_groups (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			sale_disabled INTEGER NOT NULL DEFAULT 0,
			sort_order INTEGER NOT NULL DEFAULT 0,
			created_at_ns INTEGER NOT NULL DEFAULT 0,
			updated_at_ns INTEGER NOT NULL DEFAULT 0
		)
	`); err != nil {
		_ = db.Close()
		t.Fatalf("create old billing_plan_groups table: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO billing_plan_groups(id, name, sale_disabled, sort_order, created_at_ns, updated_at_ns)
		VALUES(?, ?, ?, ?, ?, ?)
	`, "group_old_cards", "Old Cards", 0, 7, int64(100), int64(200)); err != nil {
		_ = db.Close()
		t.Fatalf("insert old billing plan group: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close old sqlite: %v", err)
	}

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository after old schema returned error: %v", err)
	}
	group, err := repo.GetBillingPlanGroup("group_old_cards")
	if err != nil {
		_ = repo.Close()
		t.Fatalf("GetBillingPlanGroup returned error: %v", err)
	}
	if group.QuotaPriceRatio != core.DefaultBillingPlanGroupQuotaPriceRatio {
		_ = repo.Close()
		t.Fatalf("migrated quota price ratio = %q, want %q", group.QuotaPriceRatio, core.DefaultBillingPlanGroupQuotaPriceRatio)
	}
	group.QuotaPriceRatio = "1:0.8"
	if err := repo.UpsertBillingPlanGroup(group); err != nil {
		_ = repo.Close()
		t.Fatalf("UpsertBillingPlanGroup returned error: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("close migrated sqlite: %v", err)
	}

	reopened, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository after migrated schema returned error: %v", err)
	}
	defer reopened.Close()
	reloaded, err := reopened.GetBillingPlanGroup("group_old_cards")
	if err != nil {
		t.Fatalf("GetBillingPlanGroup after reopen returned error: %v", err)
	}
	if reloaded.QuotaPriceRatio != "1:0.8" {
		t.Fatalf("reloaded quota price ratio = %q, want %q", reloaded.QuotaPriceRatio, "1:0.8")
	}
}

func TestSQLiteRepositoryPersistsState(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "test-master-key")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	account := core.Account{
		ID:       "acct_test",
		Provider: core.ProviderOpenAI,
		Label:    "Test Account",
		Remark:   "persistent note",
		Status:   core.AccountStatusActive,
		Priority: 50,
		Weight:   50,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "token-value",
			Metadata: map[string]string{
				"base_url": "https://example.com",
			},
		},
	}
	if err := repo.UpsertAccount(account); err != nil {
		t.Fatalf("UpsertAccount returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:                "client_test",
		Name:              "Client Test",
		APIKey:            "gw_client_key",
		Enabled:           true,
		SpendLimitNanoUSD: 10 * core.NanoUSDPerUSD,
		RouteAffinityKey:  "runtime-only",
		RoutePolicy: core.RoutePolicy{
			DefaultProvider:   core.ProviderClaude,
			FallbackProviders: []core.ProviderKind{core.ProviderOpenAI},
			Rules:             core.DefaultRoutePolicy().Rules,
		},
	}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if err := repo.AppendAudit(core.AuditEvent{
		ID:          "audit_test",
		Kind:        core.AuditKindGateway,
		Status:      "ok",
		RequestBody: `{"model":"gpt-5.4","input":"large request"}`,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("AppendAudit returned error: %v", err)
	}
	fullAudits := repo.ListAudit(1)
	if len(fullAudits) != 1 {
		t.Fatalf("ListAudit returned %d items, want 1", len(fullAudits))
	}
	if !strings.Contains(fullAudits[0].RequestBody, "large request") {
		t.Fatalf("ListAudit should return full request body, got %q", fullAudits[0].RequestBody)
	}
	auditSummaries := repo.ListAuditSummaries(1)
	if len(auditSummaries) != 1 {
		t.Fatalf("ListAuditSummaries returned %d items, want 1", len(auditSummaries))
	}
	if auditSummaries[0].RequestBody != "" {
		t.Fatalf("ListAuditSummaries should return summary without request body, got %q", auditSummaries[0].RequestBody)
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if strings.Contains(string(raw), "token-value") {
		t.Fatal("sqlite database contains plaintext access token")
	}
	if strings.Contains(string(raw), "gw_client_key") {
		t.Fatal("sqlite database contains plaintext client api key")
	}

	reloaded, err := NewSQLiteRepository(statePath, "test-master-key")
	if err != nil {
		t.Fatalf("reloading repository returned error: %v", err)
	}
	t.Cleanup(func() { _ = reloaded.Close() })

	saved, err := reloaded.GetAccount("acct_test")
	if err != nil {
		t.Fatalf("GetAccount returned error: %v", err)
	}
	if saved.Credential.AccessToken != "token-value" {
		t.Fatalf("saved access token = %q, want %q", saved.Credential.AccessToken, "token-value")
	}
	if saved.Remark != "persistent note" {
		t.Fatalf("saved remark = %q, want %q", saved.Remark, "persistent note")
	}
	client, err := reloaded.GetClient("client_test")
	if err != nil {
		t.Fatalf("GetClient returned error: %v", err)
	}
	if client.APIKey != "gw_client_key" {
		t.Fatalf("saved api key = %q, want %q", client.APIKey, "gw_client_key")
	}
	if client.RouteAffinityKey != "" {
		t.Fatalf("route affinity key persisted: %q", client.RouteAffinityKey)
	}
	if len(reloaded.ListAudit(1)) != 1 {
		t.Fatalf("expected persisted audit row")
	}
}

func TestSQLiteRepositoryMergeUsersMovesOwnedData(t *testing.T) {
	tempDir := t.TempDir()
	repo, err := NewSQLiteRepository(filepath.Join(tempDir, "state.db"), "test-master-key")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	target := core.User{ID: "user_target", Username: "target", Enabled: true, BalanceNanoUSD: 1000}
	source := core.User{
		ID:             "user_source",
		Username:       "source",
		Enabled:        true,
		BalanceNanoUSD: core.NanoUSDPerUSD/100 + 5000,
		OAuthIdentities: []core.UserOAuthIdentity{{
			Provider: "google",
			Subject:  "google-subject",
			LinkedAt: time.Now().UTC(),
		}},
	}
	if err := repo.UpsertUser(target); err != nil {
		t.Fatalf("UpsertUser target returned error: %v", err)
	}
	if err := repo.UpsertUser(source); err != nil {
		t.Fatalf("UpsertUser source returned error: %v", err)
	}
	if err := repo.UpsertUser(core.User{ID: "user_invitee", Username: "invitee", Enabled: true, InviterUserID: source.ID}); err != nil {
		t.Fatalf("UpsertUser invitee returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_source", Name: "Source", APIKey: "gw_source", OwnerUserID: source.ID, Enabled: true}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_source",
		ClientID:        "client_source",
		UserID:          source.ID,
		Provider:        core.ProviderOpenAI,
		Model:           "gpt-5.4",
		ReservedNanoUSD: 1000,
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if err := repo.CreatePaymentOrder(core.PaymentOrder{
		ID:              "pay_source",
		OutTradeNo:      "trade_source",
		UserID:          source.ID,
		Provider:        core.PaymentProviderAlipay,
		Channel:         core.PaymentChannelPage,
		AmountNanoUSD:   core.NanoUSDPerUSD / 100,
		Subject:         "Recharge",
		Status:          core.PaymentOrderPaid,
		ProviderTradeNo: "provider_trade_source",
	}); err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}
	if err := repo.CreatePaymentRefund(core.PaymentRefund{
		ID:            "refund_source",
		OrderID:       "pay_source",
		OutTradeNo:    "trade_source",
		UserID:        source.ID,
		Provider:      core.PaymentProviderAlipay,
		AmountNanoUSD: core.NanoUSDPerUSD / 100,
		Status:        core.PaymentRefundPending,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreatePaymentRefund returned error: %v", err)
	}
	if err := repo.CreateSiteMessage(core.SiteMessage{
		ID:            "msg_source",
		Title:         "Notice",
		Body:          "Body",
		Enabled:       true,
		TargetUserIDs: []string{source.ID},
	}); err != nil {
		t.Fatalf("CreateSiteMessage returned error: %v", err)
	}
	if err := repo.MarkSiteMessageRead("msg_source", source.ID, time.Now().UTC()); err != nil {
		t.Fatalf("MarkSiteMessageRead returned error: %v", err)
	}

	source, err = repo.GetUser(source.ID)
	if err != nil {
		t.Fatalf("GetUser source returned error: %v", err)
	}
	target, err = repo.GetUser(target.ID)
	if err != nil {
		t.Fatalf("GetUser target returned error: %v", err)
	}
	target.BalanceNanoUSD += source.BalanceNanoUSD
	target.OAuthIdentities = source.OAuthIdentities
	source.Enabled = false
	source.BalanceNanoUSD = 0
	source.OAuthIdentities = nil
	if err := repo.MergeUsers(source, target); err != nil {
		t.Fatalf("MergeUsers returned error: %v", err)
	}

	merged, err := repo.GetUser(target.ID)
	if err != nil {
		t.Fatalf("GetUser merged returned error: %v", err)
	}
	if merged.BalanceNanoUSD != core.NanoUSDPerUSD/100+6000 || len(merged.OAuthIdentities) != 1 {
		t.Fatalf("merged user = %#v", merged)
	}
	disabled, err := repo.GetUser(source.ID)
	if err != nil {
		t.Fatalf("GetUser disabled returned error: %v", err)
	}
	if disabled.Enabled || disabled.BalanceNanoUSD != 0 || len(disabled.OAuthIdentities) != 0 {
		t.Fatalf("disabled source = %#v", disabled)
	}
	client, err := repo.GetClient("client_source")
	if err != nil {
		t.Fatalf("GetClient returned error: %v", err)
	}
	if client.OwnerUserID != target.ID {
		t.Fatalf("client owner = %q, want %q", client.OwnerUserID, target.ID)
	}
	invitee, err := repo.GetUser("user_invitee")
	if err != nil {
		t.Fatalf("GetUser invitee returned error: %v", err)
	}
	if invitee.InviterUserID != target.ID {
		t.Fatalf("invitee InviterUserID = %q, want %q", invitee.InviterUserID, target.ID)
	}
	if _, total := repo.ListBillingRequestsPage(BillingRequestQuery{UserID: target.ID, Limit: 10}); total != 0 {
		t.Fatalf("target billing total = %d, want 0", total)
	}
	if _, total := repo.ListBillingRequestsPage(BillingRequestQuery{UserID: source.ID, Limit: 10}); total != 1 {
		t.Fatalf("source billing total = %d, want 1", total)
	}
	if ledger := repo.ListBillingLedger(target.ID, 10); len(ledger) == 0 || ledger[0].Kind != "account_merge" || ledger[0].AmountNanoUSD != core.NanoUSDPerUSD/100+5000 || ledger[0].BalanceAfterNanoUSD != core.NanoUSDPerUSD/100+6000 {
		t.Fatalf("target ledger should include account merge entry, got %#v", ledger)
	}
	if orders, total := repo.ListPaymentOrdersPage(PaymentOrderQuery{UserID: target.ID, Limit: 10}); total != 0 || len(orders) != 0 {
		t.Fatalf("target payment orders = %#v total=%d, want empty", orders, total)
	}
	if orders, total := repo.ListPaymentOrdersPage(PaymentOrderQuery{UserID: source.ID, Limit: 10}); total != 1 || len(orders) != 1 || orders[0].UserID != source.ID {
		t.Fatalf("source payment orders = %#v total=%d, want preserved source order", orders, total)
	}
	refunds := repo.ListPaymentRefunds("pay_source")
	if len(refunds) != 1 || refunds[0].UserID != source.ID {
		t.Fatalf("source payment refunds = %#v, want preserved source refund", refunds)
	}
	deliveries := repo.ListSiteMessageDeliveries(target.ID, false)
	if len(deliveries) != 1 || !deliveries[0].Read || len(deliveries[0].Message.TargetUserIDs) != 1 || deliveries[0].Message.TargetUserIDs[0] != target.ID {
		t.Fatalf("target message deliveries = %#v", deliveries)
	}
}

func TestSQLiteRepositoryBillingReserveSettleAndLedger(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_billing", Username: "billing", Enabled: true, BalanceNanoUSD: 10000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_billing", Name: "Billing", APIKey: "gw_billing", OwnerUserID: "user_billing", Enabled: true, SpendLimitNanoUSD: 9000}); err != nil {
		t.Fatal(err)
	}

	reservation, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:                 "req_billing",
		ClientID:                  "client_billing",
		ClientName:                "Billing",
		UserID:                    "user_billing",
		Provider:                  core.ProviderOpenAI,
		Model:                     "priced-model",
		FastMode:                  true,
		EstimatedPromptTokens:     1,
		EstimatedCompletionTokens: 2,
		InputPriceNanoUSDPer1M:    core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M:   core.NanoUSDPerUSD,
		ReservedNanoUSD:           6000,
		Fingerprint:               "fp",
		CacheDiagnostics: core.BillingCacheDiagnostics{
			RequestShape:          "responses_sse",
			PromptCacheKeySource:  "metadata.route_affinity_key",
			PromptCacheKeyHash:    "keyhash",
			SessionAffinityHash:   "sessionhash",
			PromptCacheKeyPresent: true,
		},
	})
	if err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if reservation.Status != core.BillingRequestReserved {
		t.Fatalf("status = %q", reservation.Status)
	}
	if !reservation.FastMode {
		t.Fatal("reservation FastMode = false, want true")
	}
	user, err := repo.GetUser("user_billing")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != 10000 {
		t.Fatalf("balance after reserve = %d, want 10000", user.BalanceNanoUSD)
	}

	settlement, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:                    "req_billing",
		ClientID:                     "client_billing",
		AccountID:                    "acct_billing",
		AccountGroup:                 "Premium",
		AccountGroupMultiplierBps:    25000,
		Provider:                     core.ProviderOpenAI,
		Model:                        "priced-model",
		Usage:                        core.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		InputPriceNanoUSDPer1M:       2 * core.NanoUSDPerUSD,
		CachedInputPriceNanoUSDPer1M: core.NanoUSDPerUSD / 2,
		OutputPriceNanoUSDPer1M:      3 * core.NanoUSDPerUSD,
		ActualNanoUSD:                5000,
	})
	if err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	if settlement.DeltaNanoUSD != 5000 {
		t.Fatalf("delta = %d, want 5000", settlement.DeltaNanoUSD)
	}
	if !settlement.Request.FastMode {
		t.Fatal("settlement FastMode = false, want true")
	}
	user, err = repo.GetUser("user_billing")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != 5000 {
		t.Fatalf("balance after settle = %d, want 5000", user.BalanceNanoUSD)
	}
	spend, err := repo.GetClientSpend("client_billing")
	if err != nil {
		t.Fatal(err)
	}
	if spend.SpendUsedNanoUSD != 5000 {
		t.Fatalf("spend used = %d, want 5000", spend.SpendUsedNanoUSD)
	}
	actualSpend, err := repo.GetClientActualSpend("client_billing")
	if err != nil {
		t.Fatal(err)
	}
	if actualSpend.SpendUsedNanoUSD != 5000 {
		t.Fatalf("actual spend used = %d, want 5000", actualSpend.SpendUsedNanoUSD)
	}
	requests, total := repo.ListBillingRequestsPage(BillingRequestQuery{ClientID: "client_billing", Limit: 1})
	if total != 1 || len(requests) != 1 {
		t.Fatalf("billing requests = total %d items %#v, want one", total, requests)
	}
	if !requests[0].FastMode {
		t.Fatal("listed FastMode = false, want true")
	}
	if requests[0].ClientName != "Billing" || requests[0].AccountGroup != "Premium" || requests[0].AccountGroupMultiplierBps != 25000 {
		t.Fatalf("listed snapshots = client %q group %q multiplier %d", requests[0].ClientName, requests[0].AccountGroup, requests[0].AccountGroupMultiplierBps)
	}
	if requests[0].InputPriceNanoUSDPer1M != 2*core.NanoUSDPerUSD ||
		requests[0].CachedInputPriceNanoUSDPer1M != core.NanoUSDPerUSD/2 ||
		requests[0].OutputPriceNanoUSDPer1M != 3*core.NanoUSDPerUSD {
		t.Fatalf("listed settlement prices = input %d cached %d output %d", requests[0].InputPriceNanoUSDPer1M, requests[0].CachedInputPriceNanoUSDPer1M, requests[0].OutputPriceNanoUSDPer1M)
	}
	if requests[0].CacheDiagnostics.RequestShape != "responses_sse" ||
		requests[0].CacheDiagnostics.PromptCacheKeySource != "metadata.route_affinity_key" ||
		requests[0].CacheDiagnostics.PromptCacheKeyHash != "keyhash" ||
		requests[0].CacheDiagnostics.SessionAffinityHash != "sessionhash" {
		t.Fatalf("cache diagnostics = %#v", requests[0].CacheDiagnostics)
	}
	ledger := repo.ListBillingLedger("user_billing", 10)
	if len(ledger) != 1 {
		t.Fatalf("ledger len = %d, want 1", len(ledger))
	}

	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_pending",
		ClientID:        "client_billing",
		UserID:          "user_billing",
		ReservedNanoUSD: 1000,
		Fingerprint:     "pending",
	}); err != nil {
		t.Fatalf("ReserveBilling pending returned error: %v", err)
	}
	spend, err = repo.GetClientSpend("client_billing")
	if err != nil {
		t.Fatal(err)
	}
	if spend.SpendUsedNanoUSD != 5000 {
		t.Fatalf("limit spend with pending = %d, want 5000", spend.SpendUsedNanoUSD)
	}
	actualSpend, err = repo.GetClientActualSpend("client_billing")
	if err != nil {
		t.Fatal(err)
	}
	if actualSpend.SpendUsedNanoUSD != 5000 {
		t.Fatalf("actual spend with pending = %d, want 5000", actualSpend.SpendUsedNanoUSD)
	}

	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_pending",
		ClientID:      "client_billing",
		ActualNanoUSD: 4000,
	}); err != nil {
		t.Fatalf("SettleBilling pending returned error: %v", err)
	}

	_, err = repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_limit",
		ClientID:        "client_billing",
		UserID:          "user_billing",
		ReservedNanoUSD: 4000,
		Fingerprint:     "limit",
	})
	if !errors.Is(err, ErrClientSpendLimitExceeded) {
		t.Fatalf("ReserveBilling limit err = %v", err)
	}

	_, err = repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_missing_client",
		ClientID:        "client_missing",
		UserID:          "user_billing",
		ReservedNanoUSD: 1,
		Fingerprint:     "missing",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ReserveBilling missing client err = %v", err)
	}
}

func TestSQLiteRepositoryPersistsDetailedBillingUsageAndPrices(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.db")
	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_detailed_billing", Username: "detailed", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_detailed_billing", Name: "Detailed", APIKey: "gw_detailed", OwnerUserID: "user_detailed_billing", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:                     "req_detailed_billing",
		ClientID:                      "client_detailed_billing",
		UserID:                        "user_detailed_billing",
		Model:                         "detailed-model",
		InputPriceNanoUSDPer1M:        1,
		CachedInputPriceNanoUSDPer1M:  2,
		CacheWritePriceNanoUSDPer1M:   3,
		CacheWrite5mPriceNanoUSDPer1M: 4,
		CacheWrite1hPriceNanoUSDPer1M: 5,
		OutputPriceNanoUSDPer1M:       6,
		ImageOutputPriceNanoUSDPer1M:  7,
		Fingerprint:                   "req_detailed_billing",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID: "req_detailed_billing",
		ClientID:  "client_detailed_billing",
		Model:     "detailed-model",
		Usage: core.Usage{
			PromptTokens:          100,
			CachedPromptTokens:    25,
			CacheCreationTokens:   12,
			CacheCreation5mTokens: 7,
			CacheCreation1hTokens: 5,
			CompletionTokens:      30,
			ImageOutputTokens:     9,
			TotalTokens:           142,
		},
		InputPriceNanoUSDPer1M:        11,
		CachedInputPriceNanoUSDPer1M:  12,
		CacheWritePriceNanoUSDPer1M:   13,
		CacheWrite5mPriceNanoUSDPer1M: 14,
		CacheWrite1hPriceNanoUSDPer1M: 15,
		OutputPriceNanoUSDPer1M:       16,
		ImageOutputPriceNanoUSDPer1M:  17,
		ActualNanoUSD:                 1234,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}

	requests := repo.ListBillingRequests(BillingRequestQuery{ClientID: "client_detailed_billing", Limit: 10})
	if len(requests) != 1 {
		t.Fatalf("requests = %#v, want one", requests)
	}
	got := requests[0]
	if got.PromptTokens != 100 || got.CachedPromptTokens != 25 || got.CacheCreationTokens != 12 || got.CacheCreation5mTokens != 7 || got.CacheCreation1hTokens != 5 || got.CompletionTokens != 30 || got.ImageOutputTokens != 9 || got.TotalTokens != 142 {
		t.Fatalf("usage fields = %#v", got)
	}
	if got.InputPriceNanoUSDPer1M != 11 || got.CachedInputPriceNanoUSDPer1M != 12 || got.CacheWritePriceNanoUSDPer1M != 13 || got.CacheWrite5mPriceNanoUSDPer1M != 14 || got.CacheWrite1hPriceNanoUSDPer1M != 15 || got.OutputPriceNanoUSDPer1M != 16 || got.ImageOutputPriceNanoUSDPer1M != 17 {
		t.Fatalf("price fields = %#v", got)
	}
}

func TestSQLiteRepositorySettleBillingCanMarkFastMode(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_fast_settle", Username: "fast_settle", Enabled: true, BalanceNanoUSD: 10000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_fast_settle", Name: "Fast Settle", APIKey: "gw_fast_settle", OwnerUserID: "user_fast_settle", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_fast_settle",
		ClientID:        "client_fast_settle",
		UserID:          "user_fast_settle",
		ReservedNanoUSD: 1000,
		Fingerprint:     "fp",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}

	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_fast_settle",
		ClientID:      "client_fast_settle",
		FastMode:      true,
		ActualNanoUSD: 1000,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}

	requests, total := repo.ListBillingRequestsPage(BillingRequestQuery{ClientID: "client_fast_settle", Limit: 1})
	if total != 1 || len(requests) != 1 {
		t.Fatalf("billing requests = total %d items %#v, want one", total, requests)
	}
	if !requests[0].FastMode {
		t.Fatal("listed FastMode = false, want true")
	}
}

func TestSQLiteRepositoryListBillingRequestsPageFiltersByClientName(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_a", Username: "a", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_a", Name: "key", APIKey: "gw_a", OwnerUserID: "user_a", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	seedBillingRequest(t, repo, "req_a_1", "client_a", "user_a", "gpt-4.1", 1100)
	time.Sleep(time.Millisecond)
	seedBillingRequest(t, repo, "req_a_2", "client_a", "user_a", "gpt-4.1", 1200)

	requests, total := repo.ListBillingRequestsPage(BillingRequestQuery{ClientID: "key", Limit: 10})
	if total != 2 || len(requests) != 2 || requests[0].RequestID != "req_a_2" || requests[1].RequestID != "req_a_1" {
		t.Fatalf("client name filtered requests = total %d items %#v", total, requests)
	}
}

func TestSQLiteRepositoryListBillingRequestsPageDoesNotMatchSubstringClientID(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_a", Username: "a", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_xxx", Name: "key", APIKey: "gw_xxx", OwnerUserID: "user_a", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	seedBillingRequest(t, repo, "req_xxx", "client_xxx", "user_a", "gpt-4.1", 1100)

	requests, total := repo.ListBillingRequestsPage(BillingRequestQuery{ClientID: "xxx", Limit: 10})
	if total != 0 || len(requests) != 0 {
		t.Fatalf("substring client id filter = total %d items %#v, want none", total, requests)
	}

	requests, total = repo.ListBillingRequestsPage(BillingRequestQuery{ClientID: "client_xxx", Limit: 10})
	if total != 1 || len(requests) != 1 || requests[0].RequestID != "req_xxx" {
		t.Fatalf("exact client id filter = total %d items %#v", total, requests)
	}
}

func TestSQLiteRepositoryBillingRequiresClientOwner(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_a", Username: "a", Enabled: true, BalanceNanoUSD: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertUser(core.User{ID: "user_b", Username: "b", Enabled: true, BalanceNanoUSD: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_a", Name: "A", APIKey: "gw_a", OwnerUserID: "user_a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	_, err = repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_owner",
		ClientID:        "client_a",
		UserID:          "user_b",
		ReservedNanoUSD: 1,
		Fingerprint:     "owner",
	})
	if !errors.Is(err, ErrBillingClientOwnerMismatch) {
		t.Fatalf("ReserveBilling owner mismatch err = %v", err)
	}
}

func TestSQLiteRepositoryAdjustUserBalanceWritesLedger(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_adjust", Username: "adjust", Enabled: true, BalanceNanoUSD: 5000}); err != nil {
		t.Fatal(err)
	}
	previous, next, err := repo.AdjustUserBalance("user_adjust", 2000, "top up")
	if err != nil {
		t.Fatalf("credit returned error: %v", err)
	}
	if previous != 5000 || next != 7000 {
		t.Fatalf("credit previous=%d next=%d, want 5000/7000", previous, next)
	}
	previous, next, err = repo.AdjustUserBalance("user_adjust", -1500, "correction")
	if err != nil {
		t.Fatalf("debit returned error: %v", err)
	}
	if previous != 7000 || next != 5500 {
		t.Fatalf("debit previous=%d next=%d, want 7000/5500", previous, next)
	}
	if _, _, err := repo.AdjustUserBalance("user_adjust", -6000, "too much"); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("negative balance err = %v, want ErrInsufficientBalance", err)
	}
	user, err := repo.GetUser("user_adjust")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != 5500 {
		t.Fatalf("balance = %d, want 5500", user.BalanceNanoUSD)
	}
	ledger := repo.ListBillingLedger("user_adjust", 10)
	if len(ledger) != 2 {
		t.Fatalf("ledger len = %d, want 2: %#v", len(ledger), ledger)
	}
	if ledger[0].Kind != "manual_debit" || ledger[0].AmountNanoUSD != -1500 || ledger[0].BalanceAfterNanoUSD != 5500 || ledger[0].Note != "correction" {
		t.Fatalf("debit ledger = %#v", ledger[0])
	}
	if ledger[1].Kind != "manual_credit" || ledger[1].AmountNanoUSD != 2000 || ledger[1].BalanceAfterNanoUSD != 7000 || ledger[1].Note != "top up" {
		t.Fatalf("credit ledger = %#v", ledger[1])
	}
}

func TestSQLiteRepositoryRejectsBalanceOverflow(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_negative", Username: "negative", Enabled: true, BalanceNanoUSD: -1}); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("upsert negative balance err = %v, want ErrInsufficientBalance", err)
	}
	if err := repo.UpsertUser(core.User{ID: "user_overflow", Username: "overflow", Enabled: true, BalanceNanoUSD: maxInt64}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetUserBalance("user_overflow", -1); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("set negative balance err = %v, want ErrInsufficientBalance", err)
	}
	if _, _, err := repo.AdjustUserBalance("user_overflow", 1, "too much"); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("adjust overflow err = %v, want ErrAmountOverflow", err)
	}
	user, err := repo.GetUser("user_overflow")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != maxInt64 {
		t.Fatalf("balance changed after overflow: %d", user.BalanceNanoUSD)
	}
}

func TestSQLiteRepositoryUpsertUserAllowsExistingNegativeBalance(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_negative_upsert", Username: "negative-upsert", Enabled: true, BalanceNanoUSD: 6000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_negative_upsert", Name: "Negative Upsert", APIKey: "gw_negative_upsert", OwnerUserID: "user_negative_upsert", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_negative_upsert",
		ClientID:        "client_negative_upsert",
		UserID:          "user_negative_upsert",
		ReservedNanoUSD: 5000,
		Fingerprint:     "negative-upsert",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_negative_upsert",
		ClientID:      "client_negative_upsert",
		ActualNanoUSD: 7000,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	user, err := repo.GetUser("user_negative_upsert")
	if err != nil {
		t.Fatal(err)
	}
	user.Username = "negative-upsert-renamed"
	if err := repo.UpsertUser(user); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	user, err = repo.GetUser("user_negative_upsert")
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != "negative-upsert-renamed" {
		t.Fatalf("username = %q", user.Username)
	}
	if user.BalanceNanoUSD != -1000 {
		t.Fatalf("balance = %d, want -1000", user.BalanceNanoUSD)
	}
}

func TestSQLiteRepositoryRejectsSpendOverflow(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_overflow", Username: "overflow", Enabled: true, BalanceNanoUSD: maxInt64}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_negative", Name: "Negative", APIKey: "gw_negative", OwnerUserID: "user_overflow", Enabled: true, SpendLimitNanoUSD: -1}); !errors.Is(err, ErrClientSpendLimitExceeded) {
		t.Fatalf("upsert negative spend limit err = %v, want ErrClientSpendLimitExceeded", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_overflow", Name: "Overflow", APIKey: "gw_overflow", OwnerUserID: "user_overflow", Enabled: true, SpendLimitNanoUSD: maxInt64}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(`UPDATE client_spend SET spend_used_nano_usd = ? WHERE client_id = ?`, maxInt64-5, "client_overflow"); err != nil {
		t.Fatal(err)
	}

	_, err = repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_overflow",
		ClientID:        "client_overflow",
		UserID:          "user_overflow",
		ReservedNanoUSD: 10,
		Fingerprint:     "overflow",
	})
	if err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	_, err = repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_overflow",
		ClientID:      "client_overflow",
		ActualNanoUSD: 10,
	})
	if !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("settle overflow err = %v, want ErrAmountOverflow", err)
	}
	user, err := repo.GetUser("user_overflow")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != maxInt64 {
		t.Fatalf("balance changed after overflow: %d", user.BalanceNanoUSD)
	}
}

func TestSQLiteRepositoryTrimsUsageLogsAfterOneDay(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_logs", Username: "logs", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_logs", Name: "Logs", APIKey: "gw_logs", OwnerUserID: "user_logs", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	oldAt := time.Now().UTC().Add(-25 * time.Hour)
	seedBillingRequest(t, repo, "req_old", "client_logs", "user_logs", "gpt-4.1", 1100)
	oldNS := oldAt.UnixNano()
	if _, err := repo.db.Exec(`UPDATE billing_requests SET created_at_ns = ?, settled_at_ns = ? WHERE request_id = ?`, oldNS, oldNS, "req_old"); err != nil {
		t.Fatalf("backdate billing request: %v", err)
	}
	seedZeroCostBillingRequest(t, repo, "req_old_zero", "client_logs", "user_logs", "gpt-4.1")
	if _, err := repo.db.Exec(`UPDATE billing_requests SET created_at_ns = ?, settled_at_ns = ? WHERE request_id = ?`, oldNS, oldNS, "req_old_zero"); err != nil {
		t.Fatalf("backdate zero-cost billing request: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_old_reserved",
		ClientID:        "client_logs",
		UserID:          "user_logs",
		Model:           "gpt-4.1",
		ReservedNanoUSD: 100,
		Fingerprint:     "req_old_reserved",
	}); err != nil {
		t.Fatal(err)
	}
	oldReservedNS := oldAt.Add(time.Second).UnixNano()
	if _, err := repo.db.Exec(`UPDATE billing_requests SET created_at_ns = ? WHERE request_id = ?`, oldReservedNS, "req_old_reserved"); err != nil {
		t.Fatalf("backdate reserved billing request: %v", err)
	}

	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_new",
		ClientID:        "client_logs",
		UserID:          "user_logs",
		Model:           "gpt-4.1",
		ReservedNanoUSD: 100,
		Fingerprint:     "req_new",
	}); err != nil {
		t.Fatal(err)
	}
	repo.usageTrimAt = time.Time{}

	items, total := repo.ListBillingRequestsPage(BillingRequestQuery{UserID: "user_logs", Limit: 10})
	if total != 2 || len(items) != 2 || items[0].RequestID != "req_new" || items[1].RequestID != "req_old_reserved" {
		t.Fatalf("usage logs = total %d items %#v, want newest request plus pending request", total, items)
	}
	var oldFinancialRemaining int
	if err := repo.db.QueryRow(`SELECT COUNT(*) FROM billing_requests WHERE request_id = ?`, "req_old").Scan(&oldFinancialRemaining); err != nil {
		t.Fatal(err)
	}
	if oldFinancialRemaining != 0 {
		t.Fatalf("expired financial usage log rows remaining = %d, want 0", oldFinancialRemaining)
	}
	var oldZeroRemaining int
	if err := repo.db.QueryRow(`SELECT COUNT(*) FROM billing_requests WHERE request_id = ?`, "req_old_zero").Scan(&oldZeroRemaining); err != nil {
		t.Fatal(err)
	}
	if oldZeroRemaining != 0 {
		t.Fatalf("expired zero-cost usage log rows remaining = %d, want 0", oldZeroRemaining)
	}
	if len(repo.ListBillingLedger("user_logs", 10)) == 0 {
		t.Fatal("billing ledger should not be trimmed with usage logs")
	}
	actual, err := repo.GetClientActualSpend("client_logs")
	if err != nil {
		t.Fatal(err)
	}
	if actual.SpendUsedNanoUSD != 1100 {
		t.Fatalf("actual spend = %d, want 1100", actual.SpendUsedNanoUSD)
	}
}

func TestSQLiteRepositoryListBillingRequestsTrimsExpiredUsageLogs(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_logs", Username: "logs", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_logs", Name: "Logs", APIKey: "gw_logs", OwnerUserID: "user_logs", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	seedZeroCostBillingRequest(t, repo, "req_old", "client_logs", "user_logs", "gpt-4.1")
	oldNS := time.Now().UTC().Add(-25 * time.Hour).UnixNano()
	if _, err := repo.db.Exec(`UPDATE billing_requests SET created_at_ns = ?, settled_at_ns = ? WHERE request_id = ?`, oldNS, oldNS, "req_old"); err != nil {
		t.Fatal(err)
	}
	repo.usageTrimAt = time.Time{}

	items, total := repo.ListBillingRequestsPage(BillingRequestQuery{UserID: "user_logs", Limit: 10})
	if total != 0 || len(items) != 0 {
		t.Fatalf("expired usage logs = total %d items %#v, want none", total, items)
	}
	var remaining int
	if err := repo.db.QueryRow(`SELECT COUNT(*) FROM billing_requests WHERE request_id = ?`, "req_old").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("expired usage log rows remaining = %d, want 0", remaining)
	}
}

func TestSQLiteRepositoryUsageLogRetentionTrimsFundingAllocations(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_logs", Username: "logs", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_logs", Name: "Logs", APIKey: "gw_logs", OwnerUserID: "user_logs", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	createdAts := map[string]time.Time{
		"req_alloc_old": now.Add(-25 * time.Hour),
		"req_alloc_new": now.Add(-time.Hour),
	}
	for _, requestID := range []string{"req_alloc_old", "req_alloc_new"} {
		seedZeroCostBillingRequest(t, repo, requestID, "client_logs", "user_logs", "gpt-4.1")
		createdNS := createdAts[requestID].UnixNano()
		if _, err := repo.db.Exec(`UPDATE billing_requests SET created_at_ns = ?, settled_at_ns = ? WHERE request_id = ?`, createdNS, createdNS, requestID); err != nil {
			t.Fatalf("backdate billing request %s: %v", requestID, err)
		}
		if _, err := repo.db.Exec(`
			INSERT INTO billing_funding_allocations(
				id, request_id, client_id, user_id, source, entitlement_id,
				actual_nano_usd, created_at_ns, updated_at_ns
			) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, billingAllocationID(requestID, "client_logs", core.BillingFundingSourceCash, ""), requestID, "client_logs", "user_logs", core.BillingFundingSourceCash, "", 1, createdNS, createdNS); err != nil {
			t.Fatalf("insert funding allocation %s: %v", requestID, err)
		}
	}

	if err := repo.ConfigureUsageLogRetention(1); err != nil {
		t.Fatalf("ConfigureUsageLogRetention returned error: %v", err)
	}

	var remaining int
	if err := repo.db.QueryRow(`SELECT COUNT(*) FROM billing_funding_allocations WHERE request_id = ?`, "req_alloc_old").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("trimmed usage log funding allocations remaining = %d, want 0", remaining)
	}
	if err := repo.db.QueryRow(`SELECT COUNT(*) FROM billing_funding_allocations WHERE request_id = ?`, "req_alloc_new").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("retained usage log funding allocations = %d, want 1", remaining)
	}
}

func TestSQLiteRepositoryBillingLedgerRetention(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	now := time.Now().UTC()
	for _, entry := range []struct {
		id        string
		createdAt time.Time
	}{
		{id: "ledger_new", createdAt: now.Add(-2 * 24 * time.Hour)},
		{id: "ledger_old", createdAt: now.Add(-4 * 24 * time.Hour)},
	} {
		if _, err := repo.db.Exec(
			`INSERT INTO billing_ledger(id, user_id, client_id, request_id, kind, amount_nano_usd, balance_after_nano_usd, note, created_at_ns)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			entry.id,
			"user_ledger",
			"",
			"",
			"manual_credit",
			1000,
			1000,
			"",
			entry.createdAt.UnixNano(),
		); err != nil {
			t.Fatalf("insert billing ledger %s: %v", entry.id, err)
		}
	}

	if err := repo.ConfigureBillingLedgerRetention(1); err != nil {
		t.Fatalf("ConfigureBillingLedgerRetention returned error: %v", err)
	}

	ledger := repo.ListBillingLedger("user_ledger", 10)
	if len(ledger) != 1 || ledger[0].ID != "ledger_new" {
		t.Fatalf("ledger = %#v, want only non-expired entry", ledger)
	}
}

func TestSQLiteRepositorySettleBillingRecordsClientSpendOverLimit(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_billing", Username: "billing", Enabled: true, BalanceNanoUSD: 10000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_billing", Name: "Billing", APIKey: "gw_billing", OwnerUserID: "user_billing", Enabled: true, SpendLimitNanoUSD: 6000}); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_billing",
		ClientID:        "client_billing",
		UserID:          "user_billing",
		ReservedNanoUSD: 5000,
		Fingerprint:     "fp",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}

	if _, err = repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_billing",
		ClientID:      "client_billing",
		ActualNanoUSD: 7000,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	spend, err := repo.GetClientSpend("client_billing")
	if err != nil {
		t.Fatal(err)
	}
	if spend.SpendUsedNanoUSD != 7000 {
		t.Fatalf("spend = %d, want 7000", spend.SpendUsedNanoUSD)
	}
}

func TestSQLiteRepositoryListBillingUsageSpendByClientExcludesReserved(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_usage_summary", Username: "usage_summary", Enabled: true, BalanceNanoUSD: 10000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_usage_summary", Name: "Usage Summary", APIKey: "gw_usage_summary", OwnerUserID: "user_usage_summary", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_usage_settled",
		ClientID:        "client_usage_summary",
		UserID:          "user_usage_summary",
		ReservedNanoUSD: 5000,
		Fingerprint:     "usage-settled",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_usage_settled",
		ClientID:      "client_usage_summary",
		ActualNanoUSD: 7000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_usage_reserved",
		ClientID:        "client_usage_summary",
		UserID:          "user_usage_summary",
		ReservedNanoUSD: 1000,
		Fingerprint:     "usage-reserved",
	}); err != nil {
		t.Fatal(err)
	}

	summaries := repo.ListBillingUsageSpendByClient()
	if len(summaries) != 1 || summaries[0].UserID != "user_usage_summary" || summaries[0].ClientID != "client_usage_summary" || summaries[0].SpendNanoUSD != 7000 {
		t.Fatalf("usage spend summaries = %#v, want settled actual spend only", summaries)
	}
	if _, err := repo.db.Exec(`
		INSERT INTO billing_requests(id, request_id, client_id, user_id, provider, model, status, actual_nano_usd, fingerprint, created_at_ns)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, "billing_req_usage_historical", "req_usage_historical", "client_usage_summary", "user_usage_summary", "", "historical-model", string(core.BillingRequestSettled), int64(1100), "usage-historical", sqliteTimeNS(time.Now().UTC())); err != nil {
		t.Fatalf("insert historical billing request: %v", err)
	}
	summaries = repo.ListBillingUsageSpendByClient()
	if len(summaries) != 1 || summaries[0].UserID != "user_usage_summary" || summaries[0].ClientID != "client_usage_summary" || summaries[0].SpendNanoUSD != 8100 {
		t.Fatalf("usage spend summaries with historical request = %#v, want settled request spend", summaries)
	}
	if err := repo.UpsertUser(core.User{ID: "user_usage_other", Username: "usage_other", Enabled: true, BalanceNanoUSD: 10000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_usage_other", Name: "Usage Other", APIKey: "gw_usage_other", OwnerUserID: "user_usage_other", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_usage_other",
		ClientID:        "client_usage_other",
		UserID:          "user_usage_other",
		ReservedNanoUSD: 2000,
		Fingerprint:     "usage-other",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_usage_other",
		ClientID:      "client_usage_other",
		ActualNanoUSD: 3000,
	}); err != nil {
		t.Fatal(err)
	}

	summaries = repo.ListBillingUsageSpendByClientForUser("user_usage_summary")
	if len(summaries) != 1 || summaries[0].UserID != "user_usage_summary" || summaries[0].ClientID != "client_usage_summary" || summaries[0].SpendNanoUSD != 8100 {
		t.Fatalf("filtered usage spend summaries = %#v, want only user_usage_summary", summaries)
	}
}

func TestSQLiteRepositoryBillingUsageSpendNanoUSDUsesLedgerWindow(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_usage_window", Username: "usage_window", Enabled: true, BalanceNanoUSD: 20000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_usage_window", Name: "Usage Window", APIKey: "gw_usage_window", OwnerUserID: "user_usage_window", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	start := time.Now().UTC().Add(-time.Second)
	if _, _, err := repo.AdjustUserBalance("user_usage_window", 1234, "manual credit"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_usage_window_settled",
		ClientID:        "client_usage_window",
		UserID:          "user_usage_window",
		ReservedNanoUSD: 5000,
		Fingerprint:     "usage-window-settled",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_usage_window_settled",
		ClientID:      "client_usage_window",
		ActualNanoUSD: 7000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_usage_window_reserved",
		ClientID:        "client_usage_window",
		UserID:          "user_usage_window",
		ReservedNanoUSD: 3000,
		Fingerprint:     "usage-window-reserved",
	}); err != nil {
		t.Fatal(err)
	}
	end := time.Now().UTC().Add(time.Second)

	if got := repo.BillingUsageSpendNanoUSD(start, end); got != 7000 {
		t.Fatalf("usage spend = %d, want settled actual only", got)
	}
	if got := repo.BillingUsageSpendNanoUSD(end, time.Time{}); got != 0 {
		t.Fatalf("future usage spend = %d, want 0", got)
	}
}

func TestSQLiteRepositoryListBillingUsageSpendByDayUsesLedgerWindow(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_usage_day", Username: "usage_day", Enabled: true, BalanceNanoUSD: 20000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_usage_day", Name: "Usage Day", APIKey: "gw_usage_day", OwnerUserID: "user_usage_day", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if _, _, err := repo.AdjustUserBalance("user_usage_day", 1234, "manual credit"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_usage_day_settled",
		ClientID:        "client_usage_day",
		UserID:          "user_usage_day",
		ReservedNanoUSD: 5000,
		Fingerprint:     "usage-day-settled",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_usage_day_settled",
		ClientID:      "client_usage_day",
		ActualNanoUSD: 7000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_usage_day_reserved",
		ClientID:        "client_usage_day",
		UserID:          "user_usage_day",
		ReservedNanoUSD: 3000,
		Fingerprint:     "usage-day-reserved",
	}); err != nil {
		t.Fatal(err)
	}

	summaries := repo.ListBillingUsageSpendByDay(startOfDay, 1)
	if len(summaries) != 1 || summaries[0].Date != startOfDay.Format("2006-01-02") || summaries[0].SpendNanoUSD != 7000 {
		t.Fatalf("daily usage spend summaries = %#v, want one day with settled actual only", summaries)
	}
}

func TestSQLiteRepositoryListBillingUsageSpendByHourForUser(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_usage_hour", Username: "usage_hour", Enabled: true, BalanceNanoUSD: 20000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_usage_hour", Name: "Usage Hour", APIKey: "gw_usage_hour", OwnerUserID: "user_usage_hour", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_usage_hour_settled",
		ClientID:        "client_usage_hour",
		UserID:          "user_usage_hour",
		ReservedNanoUSD: 5000,
		Fingerprint:     "usage-hour-settled",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_usage_hour_settled",
		ClientID:      "client_usage_hour",
		ActualNanoUSD: 7000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_usage_hour_reserved",
		ClientID:        "client_usage_hour",
		UserID:          "user_usage_hour",
		ReservedNanoUSD: 3000,
		Fingerprint:     "usage-hour-reserved",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_usage_hour_released",
		ClientID:        "client_usage_hour",
		UserID:          "user_usage_hour",
		ReservedNanoUSD: 1000,
		Fingerprint:     "usage-hour-released",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReleaseBilling(core.BillingReleaseInput{
		RequestID: "req_usage_hour_released",
		ClientID:  "client_usage_hour",
		Reason:    "test",
	}); err != nil {
		t.Fatal(err)
	}

	summaries := repo.ListBillingUsageSpendByHourForUser("user_usage_hour", startOfDay, startOfDay.AddDate(0, 0, 1))
	if len(summaries) != 1 || summaries[0].Hour != now.Local().Hour() || summaries[0].SpendNanoUSD != 7000 {
		t.Fatalf("hourly usage spend summaries = %#v, want current hour with settled actual only", summaries)
	}
}

func TestSQLiteRepositorySettleBillingRecordsNegativeBalance(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_billing", Username: "billing", Enabled: true, BalanceNanoUSD: 6000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_billing", Name: "Billing", APIKey: "gw_billing", OwnerUserID: "user_billing", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_billing",
		ClientID:        "client_billing",
		UserID:          "user_billing",
		ReservedNanoUSD: 5000,
		Fingerprint:     "fp",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}

	if _, err = repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_billing",
		ClientID:      "client_billing",
		ActualNanoUSD: 7000,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	user, err := repo.GetUser("user_billing")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != -1000 {
		t.Fatalf("balance = %d, want -1000", user.BalanceNanoUSD)
	}
}

func TestSQLiteRepositoryDeleteUserPreservesBillingHistory(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_history", Username: "history", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_history", Name: "History", APIKey: "gw_history", OwnerUserID: "user_history", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	seedBillingRequest(t, repo, "req_history", "client_history", "user_history", "gpt-4.1", 1100)

	if err := repo.DeleteUser("user_history"); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	if _, err := repo.GetUser("user_history"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUser err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetClient("client_history"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetClient err = %v, want ErrNotFound", err)
	}
	if items, total := repo.ListBillingRequestsPage(BillingRequestQuery{UserID: "user_history", Limit: 10}); total != 1 || len(items) != 1 {
		t.Fatalf("billing requests after user delete = total %d items %#v, want preserved", total, items)
	}
	if ledger := repo.ListBillingLedger("user_history", 10); len(ledger) == 0 {
		t.Fatal("billing ledger should remain after user delete")
	}
}

func TestSQLiteRepositoryDeleteUserRemovesOwnedClients(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_delete", Username: "delete", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_delete", Name: "Delete", APIKey: "gw_delete", OwnerUserID: "user_delete", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{ResponseID: "resp_delete", AccountID: "account_delete", ClientID: "client_delete", PromptCacheKey: "agpc_delete"}); err != nil {
		t.Fatal(err)
	}

	if err := repo.DeleteUser("user_delete"); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	if _, err := repo.GetClient("client_delete"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetClient err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetOpenAIResponseBinding("resp_delete"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetOpenAIResponseBinding err = %v, want ErrNotFound", err)
	}
}

func TestSQLiteRepositoryOpenAIResponseBindingPromptCacheKeyRoundTrip(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{
		ResponseID:     "resp_cache",
		AccountID:      "account_cache",
		ClientID:       "client_cache",
		PromptCacheKey: "agpc_cache",
	}); err != nil {
		t.Fatal(err)
	}

	binding, err := repo.GetOpenAIResponseBinding("resp_cache")
	if err != nil {
		t.Fatalf("GetOpenAIResponseBinding returned error: %v", err)
	}
	if binding.PromptCacheKey != "agpc_cache" {
		t.Fatalf("PromptCacheKey = %q, want agpc_cache", binding.PromptCacheKey)
	}
}

func TestSQLiteRepositoryFindUserByEmailFallsBackToPayloadAndRejectsDuplicates(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_email_a", Username: "email-a", Email: "Alice@Example.com", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(`UPDATE users SET email_key = '' WHERE id = ?`, "user_email_a"); err != nil {
		t.Fatalf("clear email_key: %v", err)
	}
	user, err := repo.FindUserByEmail("alice@example.com")
	if err != nil {
		t.Fatalf("FindUserByEmail returned error: %v", err)
	}
	if user.ID != "user_email_a" {
		t.Fatalf("FindUserByEmail ID = %q, want user_email_a", user.ID)
	}

	if err := repo.UpsertUser(core.User{ID: "user_email_b", Username: "email-b", Email: "ALICE@example.com", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.FindUserByEmail("alice@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindUserByEmail duplicate err = %v, want ErrNotFound", err)
	}
}

func TestSQLiteRepositoryPasswordResetTokenRoundTripAndSessionCleanup(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	now := time.Now().UTC()
	if err := repo.UpsertUser(core.User{ID: "user_reset", Username: "reset", Email: "reset@example.com", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertUser(core.User{ID: "user_other", Username: "other", Email: "other@example.com", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertUserSession(core.UserSession{TokenHash: "session_reset", UserID: "user_reset", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertUserSession(core.UserSession{TokenHash: "session_other", UserID: "user_other", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	token := core.PasswordResetToken{
		ID:        "reset_token",
		UserID:    "user_reset",
		Email:     "Reset@Example.com",
		TokenHash: "reset_hash",
		ExpiresAt: now.Add(time.Hour),
		CreatedAt: now,
	}
	if err := repo.CreatePasswordResetToken(token); err != nil {
		t.Fatalf("CreatePasswordResetToken returned error: %v", err)
	}
	loaded, err := repo.GetPasswordResetTokenByHash("reset_hash")
	if err != nil {
		t.Fatalf("GetPasswordResetTokenByHash returned error: %v", err)
	}
	if loaded.ID != token.ID || loaded.Email != "reset@example.com" {
		t.Fatalf("loaded reset token = %#v", loaded)
	}
	latest, err := repo.LatestPasswordResetToken("RESET@example.com")
	if err != nil {
		t.Fatalf("LatestPasswordResetToken returned error: %v", err)
	}
	if latest.ID != token.ID {
		t.Fatalf("LatestPasswordResetToken ID = %q, want %q", latest.ID, token.ID)
	}
	if count := repo.CountPasswordResetTokensSince("reset@example.com", now.Add(-time.Minute)); count != 1 {
		t.Fatalf("CountPasswordResetTokensSince = %d, want 1", count)
	}

	usedAt := now.Add(time.Minute)
	loaded.UsedAt = &usedAt
	if err := repo.UpdatePasswordResetToken(loaded); err != nil {
		t.Fatalf("UpdatePasswordResetToken returned error: %v", err)
	}
	updated, err := repo.GetPasswordResetTokenByHash("reset_hash")
	if err != nil {
		t.Fatalf("GetPasswordResetTokenByHash after update returned error: %v", err)
	}
	if updated.UsedAt == nil || !updated.UsedAt.Equal(usedAt) {
		t.Fatalf("Updated UsedAt = %v, want %v", updated.UsedAt, usedAt)
	}

	if err := repo.DeleteUserSessionsByUser("user_reset"); err != nil {
		t.Fatalf("DeleteUserSessionsByUser returned error: %v", err)
	}
	if _, err := repo.GetUserSession("session_reset"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUserSession session_reset err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetUserSession("session_other"); err != nil {
		t.Fatalf("GetUserSession session_other returned error: %v", err)
	}
	if err := repo.DeletePasswordResetToken(token.ID); err != nil {
		t.Fatalf("DeletePasswordResetToken returned error: %v", err)
	}
	if _, err := repo.GetPasswordResetTokenByHash("reset_hash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetPasswordResetTokenByHash after delete err = %v, want ErrNotFound", err)
	}
}

func TestSQLiteRepositoryDeleteClientRemovesResponseBindings(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertClient(core.APIClient{ID: "client_delete", Name: "Delete", APIKey: "gw_delete", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{ResponseID: "resp_delete", AccountID: "account_delete", ClientID: "client_delete", PromptCacheKey: "agpc_delete"}); err != nil {
		t.Fatal(err)
	}

	if err := repo.DeleteClient("client_delete"); err != nil {
		t.Fatalf("DeleteClient returned error: %v", err)
	}
	if _, err := repo.GetOpenAIResponseBinding("resp_delete"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetOpenAIResponseBinding err = %v, want ErrNotFound", err)
	}
}

func TestSQLiteRepositoryDeleteUserPreservesHistoryAndRemovesOwnedData(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_force", Username: "force", Email: "force@example.com", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_force", Name: "Force", APIKey: "gw_force", OwnerUserID: "user_force", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{ResponseID: "resp_force", AccountID: "account_force", ClientID: "client_force", PromptCacheKey: "agpc_force"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateEmailVerificationCode(core.EmailVerificationCode{ID: "code_force", Purpose: "register", Email: "force@example.com", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	seedBillingRequest(t, repo, "req_force", "client_force", "user_force", "gpt-4.1", 1100)

	if err := repo.DeleteUser("user_force"); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	if _, err := repo.GetUser("user_force"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUser err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetClient("client_force"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetClient err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetOpenAIResponseBinding("resp_force"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetOpenAIResponseBinding err = %v, want ErrNotFound", err)
	}
	if items, total := repo.ListBillingRequestsPage(BillingRequestQuery{UserID: "user_force", Limit: 10}); total != 1 || len(items) != 1 {
		t.Fatalf("billing requests after user delete = total %d items %#v, want preserved", total, items)
	}
	if ledger := repo.ListBillingLedger("user_force", 10); len(ledger) == 0 {
		t.Fatalf("billing ledger after user delete = %#v, want preserved", ledger)
	}
	if _, err := repo.LatestEmailVerificationCode("register", "force@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestEmailVerificationCode err = %v, want ErrNotFound", err)
	}
}

func TestSQLiteRepositoryDeleteUserAllowsUnpaidPaymentOrder(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_unpaid_order", Username: "unpaid_order", Enabled: true}); err != nil {
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
	if err := repo.CreateSiteMessage(core.SiteMessage{
		ID:            "msg_unpaid_user",
		Title:         "Notice",
		Body:          "Body",
		Enabled:       true,
		TargetUserIDs: []string{"user_unpaid_order"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkSiteMessageRead("msg_unpaid_user", "user_unpaid_order", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	if err := repo.DeleteUser("user_unpaid_order"); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	if _, err := repo.GetUser("user_unpaid_order"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUser err = %v, want ErrNotFound", err)
	}
	if order, err := repo.GetPaymentOrder("pay_unpaid"); err != nil || order.UserID != "user_unpaid_order" {
		t.Fatalf("GetPaymentOrder = %#v err=%v, want preserved static order", order, err)
	}
	messages := repo.ListSiteMessages()
	if len(messages) != 1 || len(messages[0].TargetUserIDs) != 0 {
		t.Fatalf("site message targets after delete = %#v", messages)
	}
	var readCount int
	if err := repo.db.QueryRow(`SELECT COUNT(*) FROM site_message_reads WHERE user_id = ?`, "user_unpaid_order").Scan(&readCount); err != nil {
		t.Fatal(err)
	}
	if readCount != 0 {
		t.Fatalf("site message reads after delete = %d, want 0", readCount)
	}
}

func TestSQLiteRepositoryDeleteUserPreservesPaidPaymentHistory(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_payment_history", Username: "payment_history", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreatePaymentOrder(core.PaymentOrder{
		ID:            "pay_history",
		OutTradeNo:    "out_history",
		UserID:        "user_payment_history",
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: core.NanoUSDPerUSD / 100,
		Status:        core.PaymentOrderPending,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.CompletePaymentOrder("out_history", "trade_history", core.NanoUSDPerUSD/100, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	if err := repo.DeleteUser("user_payment_history"); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	if _, err := repo.GetUser("user_payment_history"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUser err = %v, want ErrNotFound", err)
	}
	if order, err := repo.GetPaymentOrder("pay_history"); err != nil || order.Status != core.PaymentOrderPaid || order.UserID != "user_payment_history" {
		t.Fatalf("payment order after delete = %#v err=%v, want preserved paid order", order, err)
	}
}

func TestSQLiteRepositoryDeleteUserPreservesOwnedClientBillingHistory(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_owner_history", Username: "owner_history", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_owner_history", Name: "Owner History", APIKey: "gw_owner_history", OwnerUserID: "user_owner_history", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	seedBillingRequest(t, repo, "req_owner_history", "client_owner_history", "user_owner_history", "gpt-4.1", 1100)
	if err := repo.SetUserBalance("user_owner_history", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(`DELETE FROM billing_ledger WHERE user_id = ?`, "user_owner_history"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(`UPDATE billing_requests SET user_id = ? WHERE user_id = ?`, "archived_user", "user_owner_history"); err != nil {
		t.Fatal(err)
	}

	if err := repo.DeleteUser("user_owner_history"); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	if _, err := repo.GetUser("user_owner_history"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUser err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetClient("client_owner_history"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetClient err = %v, want ErrNotFound", err)
	}
	if items, total := repo.ListBillingRequestsPage(BillingRequestQuery{ClientID: "client_owner_history", Limit: 10}); total != 1 || len(items) != 1 {
		t.Fatalf("billing requests after user delete = total %d items %#v, want preserved", total, items)
	}
}

func TestSQLiteRepositoryDeleteClientPreservesBillingHistory(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_history", Username: "history", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_history", Name: "History", APIKey: "gw_history", OwnerUserID: "user_history", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	seedBillingRequest(t, repo, "req_history", "client_history", "user_history", "gpt-4.1", 1100)

	if err := repo.DeleteClient("client_history"); err != nil {
		t.Fatalf("DeleteClient returned error: %v", err)
	}
	if _, err := repo.GetClient("client_history"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetClient err = %v, want ErrNotFound", err)
	}
	requests, total := repo.ListBillingRequestsPage(BillingRequestQuery{ClientID: "client_history", Limit: 10})
	if total != 1 || len(requests) != 1 || requests[0].RequestID != "req_history" || requests[0].ClientID != "client_history" {
		t.Fatalf("billing requests after client delete = total %d items %#v, want preserved req_history", total, requests)
	}
	if ledger := repo.ListBillingLedger("user_history", 10); len(ledger) == 0 {
		t.Fatalf("billing ledger should remain after client delete")
	}
}

func TestSQLiteRepositoryDeleteClientReleasesReservedBilling(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	const startingBalance int64 = 100000
	if err := repo.UpsertUser(core.User{ID: "user_reserved_client_delete", Username: "reserved", Enabled: true, BalanceNanoUSD: startingBalance}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_reserved_delete", Name: "Reserved Delete", APIKey: "gw_reserved_delete", OwnerUserID: "user_reserved_client_delete", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_reserved_delete",
		ClientID:        "client_reserved_delete",
		UserID:          "user_reserved_client_delete",
		Model:           "gpt-4.1",
		ReservedNanoUSD: 5000,
		Fingerprint:     "reserved-delete",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteClient("client_reserved_delete"); err != nil {
		t.Fatalf("DeleteClient returned error: %v", err)
	}
	user, err := repo.GetUser("user_reserved_client_delete")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != startingBalance {
		t.Fatalf("balance after client delete = %d, want %d", user.BalanceNanoUSD, startingBalance)
	}
	requests, total := repo.ListBillingRequestsPage(BillingRequestQuery{ClientID: "client_reserved_delete", Limit: 10})
	if total != 1 || len(requests) != 1 || requests[0].Status != core.BillingRequestReleased {
		t.Fatalf("billing requests after client delete = total %d items %#v, want released", total, requests)
	}
}

func TestSQLiteRepositoryDeleteUserReleasesReservedBilling(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_reserved_delete", Username: "reserved", Enabled: true, BalanceNanoUSD: 100000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_owned_reserved_delete", Name: "Owned Reserved Delete", APIKey: "gw_owned_reserved_delete", OwnerUserID: "user_reserved_delete", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_owned_reserved_delete",
		ClientID:        "client_owned_reserved_delete",
		UserID:          "user_reserved_delete",
		Model:           "gpt-4.1",
		ReservedNanoUSD: 5000,
		Fingerprint:     "owned-reserved-delete",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteUser("user_reserved_delete"); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	requests, total := repo.ListBillingRequestsPage(BillingRequestQuery{ClientID: "client_owned_reserved_delete", Limit: 10})
	if total != 1 || len(requests) != 1 || requests[0].Status != core.BillingRequestReleased {
		t.Fatalf("billing requests after user delete = total %d items %#v, want released", total, requests)
	}
	if _, err := repo.GetClient("client_owned_reserved_delete"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetClient err = %v, want ErrNotFound", err)
	}
}

func TestSQLiteRepositoryDeleteClientPreservesZeroCostUsageLog(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertUser(core.User{ID: "user_zero_cost", Username: "zero_cost", Enabled: true, BalanceNanoUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_zero_cost", Name: "Zero Cost", APIKey: "gw_zero_cost", OwnerUserID: "user_zero_cost", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:   "req_zero_cost",
		ClientID:    "client_zero_cost",
		UserID:      "user_zero_cost",
		Model:       "gpt-4.1",
		Fingerprint: "zero-cost",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_zero_cost",
		ClientID:      "client_zero_cost",
		Model:         "gpt-4.1",
		ActualNanoUSD: 0,
	}); err != nil {
		t.Fatal(err)
	}

	if err := repo.DeleteClient("client_zero_cost"); err != nil {
		t.Fatalf("DeleteClient returned error: %v", err)
	}
	if _, total := repo.ListBillingRequestsPage(BillingRequestQuery{ClientID: "client_zero_cost", Limit: 10}); total != 1 {
		t.Fatalf("zero-cost billing requests after delete = %d, want 1", total)
	}
}

func TestSQLiteRepositoryFindsClientByAPIKeyWithHashIndex(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.UpsertClient(core.APIClient{ID: "client_a", Name: "A", APIKey: "old_key", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	client, err := repo.FindClientByAPIKey("old_key")
	if err != nil || client.ID != "client_a" {
		t.Fatalf("FindClientByAPIKey(old) = %#v, %v", client, err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_a", Name: "A", APIKey: "new_key", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.FindClientByAPIKey("old_key"); err == nil {
		t.Fatal("old key should not resolve after update")
	}
	client, err = repo.FindClientByAPIKey("new_key")
	if err != nil || client.ID != "client_a" {
		t.Fatalf("FindClientByAPIKey(new) = %#v, %v", client, err)
	}

	var hash string
	if err := repo.db.QueryRow(`SELECT api_key_hash FROM clients WHERE id = ?`, "client_a").Scan(&hash); err != nil {
		t.Fatalf("select api_key_hash: %v", err)
	}
	if hash == "" || hash != clientAPIKeyHash("new_key") || strings.Contains(hash, "new_key") {
		t.Fatalf("api key hash = %q", hash)
	}
}

func TestSQLiteRepositoryListsClientSummariesByOwnerPage(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	for _, client := range []core.APIClient{
		{ID: "client_alpha", Name: "Alpha", APIKey: "gw_alpha", OwnerUserID: "user_owner", Enabled: true},
		{ID: "client_beta", Name: "Beta", APIKey: "gw_beta", OwnerUserID: "user_owner", Enabled: true},
		{ID: "client_gamma", Name: "Gamma", APIKey: "gw_gamma", OwnerUserID: "user_owner", Enabled: false},
		{ID: "client_other", Name: "Other", APIKey: "gw_other", OwnerUserID: "user_other", Enabled: true},
	} {
		if err := repo.UpsertClient(client); err != nil {
			t.Fatalf("UpsertClient(%s) returned error: %v", client.ID, err)
		}
	}

	clients, total := repo.ListClientSummariesByOwnerPage("user_owner", 1, 2)
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(clients) != 2 || clients[0].ID != "client_beta" || clients[1].ID != "client_gamma" {
		t.Fatalf("clients = %#v", clients)
	}
	for _, client := range clients {
		if client.APIKey != "" || client.RouteAffinityKey != "" {
			t.Fatalf("summary leaked secret fields: %#v", client)
		}
		if client.OwnerUserID != "user_owner" {
			t.Fatalf("summary owner = %q, want user_owner", client.OwnerUserID)
		}
	}
}

func TestSQLiteRepositoryStoresAuditSummaryOnlyWhenDifferent(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if err := repo.AppendAudit(core.AuditEvent{
		ID:        "audit_short",
		Kind:      core.AuditKindAdmin,
		Status:    "ok",
		Message:   "short",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("AppendAudit(short) returned error: %v", err)
	}
	if err := repo.AppendAudit(core.AuditEvent{
		ID:          "audit_large",
		Kind:        core.AuditKindGateway,
		Status:      "ok",
		Message:     "large",
		RequestBody: `{"input":"large request"}`,
		CreatedAt:   time.Now().UTC().Add(time.Second),
	}); err != nil {
		t.Fatalf("AppendAudit(large) returned error: %v", err)
	}

	var shortSummary string
	if err := repo.db.QueryRow(`SELECT summary_payload FROM audit WHERE event_id = ?`, "audit_short").Scan(&shortSummary); err != nil {
		t.Fatalf("select short summary: %v", err)
	}
	if shortSummary != "" {
		t.Fatalf("short summary payload = %q, want empty fallback", shortSummary)
	}

	var largePayload string
	var largeSummary string
	if err := repo.db.QueryRow(`SELECT payload, summary_payload FROM audit WHERE event_id = ?`, "audit_large").Scan(&largePayload, &largeSummary); err != nil {
		t.Fatalf("select large summary: %v", err)
	}
	if largeSummary == "" {
		t.Fatal("large summary payload should be stored")
	}
	if len(largeSummary) >= len(largePayload) {
		t.Fatalf("summary len = %d, payload len = %d; want smaller summary", len(largeSummary), len(largePayload))
	}

	summaries := repo.ListAuditSummaries(10)
	if len(summaries) != 2 {
		t.Fatalf("ListAuditSummaries returned %d items, want 2", len(summaries))
	}
	for _, summary := range summaries {
		if summary.ID == "audit_large" && summary.RequestBody != "" {
			t.Fatalf("large summary request body = %q, want empty", summary.RequestBody)
		}
	}
}

func TestSQLiteRepositoryFiltersAuditWithTermIndex(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	now := time.Now().UTC()
	events := []core.AuditEvent{
		{
			ID:           "audit_user",
			Kind:         core.AuditKindAdmin,
			Actor:        "Admin User",
			Action:       "user.update",
			ResourceType: "user",
			ResourceID:   "user_admin",
			ResourceName: "admin",
			Status:       "ok",
			CreatedAt:    now,
		},
		{
			ID:         "audit_gateway",
			Kind:       core.AuditKindGateway,
			ClientID:   "client_gateway",
			ClientName: "Gateway Client",
			Model:      "gpt-4.1",
			Status:     "error",
			CreatedAt:  now.Add(time.Second),
		},
	}
	for _, event := range events {
		if err := repo.AppendAudit(event); err != nil {
			t.Fatalf("AppendAudit(%s) returned error: %v", event.ID, err)
		}
	}

	var termCount int
	if err := repo.db.QueryRow(`SELECT COUNT(*) FROM audit_terms`).Scan(&termCount); err != nil {
		t.Fatalf("count audit_terms: %v", err)
	}
	if termCount == 0 {
		t.Fatal("expected audit_terms to be populated")
	}

	items, total := repo.ListAuditSummariesPage(AuditQuery{
		Kind:     core.AuditKindAdmin,
		Actor:    "admin user",
		Resource: "admin",
		Limit:    10,
	})
	if total != 1 || len(items) != 1 || items[0].ID != "audit_user" {
		t.Fatalf("filtered audit total=%d items=%#v", total, items)
	}

	items, total = repo.ListAuditSummariesPage(AuditQuery{
		Resource: "gateway client",
		Limit:    10,
	})
	if total != 1 || len(items) != 1 || items[0].ID != "audit_gateway" {
		t.Fatalf("resource filtered audit total=%d items=%#v", total, items)
	}

	if err := repo.ConfigureAuditLimit(1); err != nil {
		t.Fatalf("ConfigureAuditLimit returned error: %v", err)
	}
	items, total = repo.ListAuditSummariesPage(AuditQuery{
		Resource: "prompt envelope",
		Limit:    10,
	})
	if total != 0 || len(items) != 0 {
		t.Fatalf("trimmed audit should not be searchable, total=%d items=%#v", total, items)
	}
}

func TestSQLiteRepositoryRejectsEncryptedStateWithoutKey(t *testing.T) {
	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.db")

	repo, err := NewSQLiteRepository(statePath, "test-master-key")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_test",
		Provider: core.ProviderOpenAI,
		Label:    "Encrypted Account",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "token-value",
		},
	}); err != nil {
		t.Fatalf("UpsertAccount returned error: %v", err)
	}

	_, err = NewSQLiteRepository(statePath, "")
	if err == nil {
		t.Fatal("expected error when loading encrypted sqlite state without key")
	}
	if !strings.Contains(err.Error(), "master_key") {
		t.Fatalf("error = %q, want missing key hint", err.Error())
	}
}
