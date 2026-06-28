package storage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestMemoryRepositoryPaymentOrderCompletesOnce(t *testing.T) {
	repo := NewMemoryRepository()
	testPaymentOrderCompletesOnce(t, repo)
}

func TestSQLiteRepositoryPaymentOrderCompletesOnce(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	testPaymentOrderCompletesOnce(t, repo)
}

func TestMemoryRepositoryManualPaymentReplayAllowsRealProviderTrade(t *testing.T) {
	repo := NewMemoryRepository()
	testManualPaymentReplayAllowsRealProviderTrade(t, repo)
}

func TestSQLiteRepositoryManualPaymentReplayAllowsRealProviderTrade(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	testManualPaymentReplayAllowsRealProviderTrade(t, repo)
}

func TestMemoryRepositoryPaymentOrderCompletesWithCredits(t *testing.T) {
	repo := NewMemoryRepository()
	testPaymentOrderCompletesWithCredits(t, repo)
}

func TestSQLiteRepositoryPaymentOrderCompletesWithCredits(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	testPaymentOrderCompletesWithCredits(t, repo)
}

func testPaymentOrderCompletesOnce(t *testing.T, repo Repository) {
	t.Helper()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:            "pay_1",
		OutTradeNo:    "out_1",
		UserID:        "user_pay",
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: 123 * core.NanoUSDPerUSD / 100,
		Currency:      "CNY",
		Status:        core.PaymentOrderPending,
		CreatedAt:     time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}
	completed, credited, err := repo.CompletePaymentOrder("out_1", "trade_1", order.AmountNanoUSD, time.Now().UTC())
	if err != nil {
		t.Fatalf("CompletePaymentOrder returned error: %v", err)
	}
	if !credited || completed.Status != core.PaymentOrderPaid {
		t.Fatalf("completed=%#v credited=%t", completed, credited)
	}
	user, err := repo.GetUser("user_pay")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != order.AmountNanoUSD {
		t.Fatalf("balance = %d, want %d", user.BalanceNanoUSD, order.AmountNanoUSD)
	}
	if _, credited, err = repo.CompletePaymentOrder("out_1", "trade_1", order.AmountNanoUSD, time.Now().UTC()); err != nil || credited {
		t.Fatalf("duplicate complete credited=%t err=%v, want false nil", credited, err)
	}
	if _, _, err = repo.CompletePaymentOrder("out_1", "trade_other", order.AmountNanoUSD, time.Now().UTC()); !errors.Is(err, ErrBillingRequestConflict) {
		t.Fatalf("duplicate complete trade mismatch err=%v, want ErrBillingRequestConflict", err)
	}
	if _, _, err = repo.CompletePaymentOrder("out_1", "trade_1", order.AmountNanoUSD+(core.NanoUSDPerUSD/100), time.Now().UTC()); !errors.Is(err, ErrBillingRequestConflict) {
		t.Fatalf("duplicate complete amount mismatch err=%v, want ErrBillingRequestConflict", err)
	}
	user, err = repo.GetUser("user_pay")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != order.AmountNanoUSD {
		t.Fatalf("balance after duplicate = %d, want %d", user.BalanceNanoUSD, order.AmountNanoUSD)
	}
	if ledger := repo.ListBillingLedger("user_pay", 10); len(ledger) != 1 || ledger[0].Kind != "payment" {
		t.Fatalf("ledger = %#v, want one payment entry", ledger)
	}
	items, total := repo.ListPaymentOrdersPage(PaymentOrderQuery{UserID: "user_pay", Status: core.PaymentOrderPaid, Limit: 10})
	if total != 1 || len(items) != 1 || items[0].OutTradeNo != "out_1" {
		t.Fatalf("payment order page total=%d items=%#v", total, items)
	}
	items, total = repo.ListPaymentOrdersPage(PaymentOrderQuery{UserID: "pay", Status: core.PaymentOrderPaid, Limit: 10})
	if total != 1 || len(items) != 1 || items[0].OutTradeNo != "out_1" {
		t.Fatalf("payment order username page total=%d items=%#v", total, items)
	}
}

func testManualPaymentReplayAllowsRealProviderTrade(t *testing.T, repo Repository) {
	t.Helper()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:            "pay_manual_replay",
		OutTradeNo:    "out_manual_replay",
		UserID:        "user_pay",
		Provider:      core.PaymentProviderPersonalPay,
		Channel:       core.PaymentChannelWeChat,
		AmountNanoUSD: core.NanoUSDPerUSD,
		Currency:      "USD",
		Status:        core.PaymentOrderClosed,
		CreatedAt:     time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}
	if _, credited, err := repo.CompletePaymentOrder(order.OutTradeNo, "manual_pay_manual_replay", order.AmountNanoUSD, time.Now().UTC()); err != nil || !credited {
		t.Fatalf("manual complete credited=%t err=%v, want true nil", credited, err)
	}
	if _, credited, err := repo.CompletePaymentOrder(order.OutTradeNo, "real_trade_after_manual", order.AmountNanoUSD, time.Now().UTC()); err != nil || credited {
		t.Fatalf("manual replay with real trade credited=%t err=%v, want false nil", credited, err)
	}
	user, err := repo.GetUser("user_pay")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != order.AmountNanoUSD {
		t.Fatalf("balance after real replay = %d, want %d", user.BalanceNanoUSD, order.AmountNanoUSD)
	}
	if ledger := repo.ListBillingLedger("user_pay", 10); len(ledger) != 1 || ledger[0].Kind != "payment" {
		t.Fatalf("ledger = %#v, want one payment entry", ledger)
	}
}

func testPaymentOrderCompletesWithCredits(t *testing.T, repo Repository) {
	t.Helper()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertUser(core.User{ID: "user_inviter", Username: "inviter", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:            "pay_credit",
		OutTradeNo:    "out_credit",
		UserID:        "user_pay",
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: 10 * core.NanoUSDPerUSD,
		Currency:      "CNY",
		Status:        core.PaymentOrderPending,
		CreatedAt:     time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}
	credits := []core.PaymentOrderBalanceCredit{{
		UserID:        "user_inviter",
		Kind:          "manual_credit",
		LedgerID:      "ledger_invite_out_credit_user_inviter",
		AmountNanoUSD: core.NanoUSDPerUSD,
		Note:          "invite recharge reward",
	}}
	completed, credited, err := repo.CompletePaymentOrderWithCredits(order.OutTradeNo, "trade_credit", order.AmountNanoUSD, time.Now().UTC(), credits)
	if err != nil {
		t.Fatalf("CompletePaymentOrderWithCredits returned error: %v", err)
	}
	if !credited || completed.Status != core.PaymentOrderPaid {
		t.Fatalf("completed=%#v credited=%t", completed, credited)
	}
	payer, err := repo.GetUser("user_pay")
	if err != nil {
		t.Fatal(err)
	}
	if payer.BalanceNanoUSD != order.AmountNanoUSD {
		t.Fatalf("payer balance = %d, want %d", payer.BalanceNanoUSD, order.AmountNanoUSD)
	}
	inviter, err := repo.GetUser("user_inviter")
	if err != nil {
		t.Fatal(err)
	}
	if inviter.BalanceNanoUSD != core.NanoUSDPerUSD {
		t.Fatalf("inviter balance = %d, want %d", inviter.BalanceNanoUSD, core.NanoUSDPerUSD)
	}
	inviterLedger := repo.ListBillingLedger("user_inviter", 10)
	if len(inviterLedger) != 1 || inviterLedger[0].ID != credits[0].LedgerID || inviterLedger[0].Kind != "manual_credit" || inviterLedger[0].AmountNanoUSD != core.NanoUSDPerUSD {
		t.Fatalf("inviter ledger = %#v", inviterLedger)
	}
	if _, credited, err = repo.CompletePaymentOrderWithCredits(order.OutTradeNo, "trade_credit", order.AmountNanoUSD, time.Now().UTC(), credits); err != nil || credited {
		t.Fatalf("duplicate CompletePaymentOrderWithCredits credited=%t err=%v", credited, err)
	}
	inviter, err = repo.GetUser("user_inviter")
	if err != nil {
		t.Fatal(err)
	}
	if inviter.BalanceNanoUSD != core.NanoUSDPerUSD {
		t.Fatalf("inviter duplicate balance = %d, want %d", inviter.BalanceNanoUSD, core.NanoUSDPerUSD)
	}
}

func TestMemoryRepositoryPaymentOrderRejectsAmountMismatch(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{ID: "pay_1", OutTradeNo: "out_1", UserID: "user_pay", Provider: core.PaymentProviderWeChatPay, AmountNanoUSD: core.NanoUSDPerUSD}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.CompletePaymentOrder("out_1", "trade_1", order.AmountNanoUSD-(core.NanoUSDPerUSD/100), time.Now().UTC()); !errors.Is(err, ErrBillingRequestConflict) {
		t.Fatalf("amount mismatch err = %v, want ErrBillingRequestConflict", err)
	}
}

func TestMemoryRepositoryPaymentOrderExpiresPendingOrders(t *testing.T) {
	repo := NewMemoryRepository()
	testPaymentOrderExpiresPendingOrders(t, repo)
}

func TestSQLiteRepositoryPaymentOrderExpiresPendingOrders(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	testPaymentOrderExpiresPendingOrders(t, repo)
}

func TestMemoryRepositoryDeletesPendingPaymentOrder(t *testing.T) {
	repo := NewMemoryRepository()
	testDeletePendingPaymentOrder(t, repo)
}

func TestSQLiteRepositoryDeletesPendingPaymentOrder(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	testDeletePendingPaymentOrder(t, repo)
}

func testDeletePendingPaymentOrder(t *testing.T, repo Repository) {
	t.Helper()
	if err := repo.UpsertUser(core.User{ID: "user_cancel", Username: "cancel", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	pending := core.PaymentOrder{
		ID:            "pay_cancel",
		OutTradeNo:    "out_cancel",
		UserID:        "user_cancel",
		Provider:      core.PaymentProviderAlipay,
		AmountNanoUSD: core.NanoUSDPerUSD,
		Status:        core.PaymentOrderPending,
		CreatedAt:     time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(pending); err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}
	deletedOrder, deleted, err := repo.DeletePendingPaymentOrder(pending.ID)
	if err != nil || !deleted || deletedOrder.ID != pending.ID {
		t.Fatalf("DeletePendingPaymentOrder order=%#v deleted=%t err=%v", deletedOrder, deleted, err)
	}
	if _, err := repo.GetPaymentOrder(pending.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetPaymentOrder err=%v, want ErrNotFound", err)
	}
	expiredPending := pending
	expiredPending.ID = "pay_cancel_expired"
	expiredPending.OutTradeNo = "out_cancel_expired"
	expiredPending.CreatedAt = time.Now().UTC().Add(-core.DefaultPaymentOrderPendingTTL - time.Minute)
	if err := repo.CreatePaymentOrder(expiredPending); err != nil {
		t.Fatalf("Create expired pending payment order returned error: %v", err)
	}
	expiredOrder, deleted, err := repo.DeletePendingPaymentOrder(expiredPending.ID)
	if err != nil || !deleted || expiredOrder.ID != expiredPending.ID {
		t.Fatalf("Delete expired pending order=%#v deleted=%t err=%v", expiredOrder, deleted, err)
	}
	if _, err := repo.GetPaymentOrder(expiredPending.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get expired pending order err=%v, want ErrNotFound", err)
	}
	paid := pending
	paid.ID = "pay_paid"
	paid.OutTradeNo = "out_paid"
	if err := repo.CreatePaymentOrder(paid); err != nil {
		t.Fatalf("CreatePaymentOrder paid returned error: %v", err)
	}
	if _, _, err := repo.CompletePaymentOrder(paid.OutTradeNo, "trade_paid", paid.AmountNanoUSD, time.Now().UTC()); err != nil {
		t.Fatalf("CompletePaymentOrder returned error: %v", err)
	}
	order, deleted, err := repo.DeletePendingPaymentOrder(paid.ID)
	if err != nil || deleted || order.Status != core.PaymentOrderPaid {
		t.Fatalf("Delete paid order=%#v deleted=%t err=%v", order, deleted, err)
	}
}

func TestMemoryRepositoryPaymentRefundCompletesOnce(t *testing.T) {
	repo := NewMemoryRepository()
	testPaymentRefundCompletesOnce(t, repo)
}

func TestSQLiteRepositoryPaymentRefundCompletesOnce(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	testPaymentRefundCompletesOnce(t, repo)
}

func TestMemoryRepositoryPaymentRefundRejectsInsufficientBalance(t *testing.T) {
	repo := NewMemoryRepository()
	testPaymentRefundRejectsInsufficientBalance(t, repo)
}

func TestSQLiteRepositoryPaymentRefundRejectsInsufficientBalance(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	testPaymentRefundRejectsInsufficientBalance(t, repo)
}

func TestMemoryRepositoryPaymentRefundRejectsNonPendingCreate(t *testing.T) {
	repo := NewMemoryRepository()
	testPaymentRefundRejectsNonPendingCreate(t, repo)
}

func TestSQLiteRepositoryPaymentRefundRejectsNonPendingCreate(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	testPaymentRefundRejectsNonPendingCreate(t, repo)
}

func testPaymentRefundCompletesOnce(t *testing.T, repo Repository) {
	t.Helper()
	if err := repo.UpsertUser(core.User{ID: "user_refund", Username: "refund", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:            "pay_refund",
		OutTradeNo:    "out_refund",
		UserID:        "user_refund",
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: core.NanoUSDPerUSD,
		Status:        core.PaymentOrderPending,
		CreatedAt:     time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	if _, credited, err := repo.CompletePaymentOrder(order.OutTradeNo, "trade_refund", order.AmountNanoUSD, time.Now().UTC()); err != nil || !credited {
		t.Fatalf("CompletePaymentOrder credited=%t err=%v", credited, err)
	}
	refund := core.PaymentRefund{
		ID:            "refund_1",
		OrderID:       order.ID,
		OutTradeNo:    order.OutTradeNo,
		UserID:        order.UserID,
		Provider:      order.Provider,
		AmountNanoUSD: 25 * core.NanoUSDPerUSD / 100,
		Status:        core.PaymentRefundPending,
		CreatedAt:     time.Now().UTC(),
	}
	if err := repo.CreatePaymentRefund(refund); err != nil {
		t.Fatalf("CreatePaymentRefund returned error: %v", err)
	}
	completed, debited, err := repo.CompletePaymentRefund(refund.ID, "provider_refund_1", "raw")
	if err != nil {
		t.Fatalf("CompletePaymentRefund returned error: %v", err)
	}
	if !debited || completed.Status != core.PaymentRefundDone || completed.ProviderRefundNo != "provider_refund_1" {
		t.Fatalf("completed=%#v debited=%t", completed, debited)
	}
	user, err := repo.GetUser(order.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if want := 75 * core.NanoUSDPerUSD / 100; user.BalanceNanoUSD != want {
		t.Fatalf("balance = %d, want %d", user.BalanceNanoUSD, want)
	}
	if _, debited, err = repo.CompletePaymentRefund(refund.ID, "provider_refund_1", "raw"); err != nil || debited {
		t.Fatalf("duplicate refund debited=%t err=%v, want false nil", debited, err)
	}
	failedRefund := core.PaymentRefund{
		ID:            "refund_failed",
		OrderID:       order.ID,
		OutTradeNo:    order.OutTradeNo,
		UserID:        order.UserID,
		Provider:      order.Provider,
		AmountNanoUSD: 75 * core.NanoUSDPerUSD / 100,
		Status:        core.PaymentRefundPending,
		CreatedAt:     time.Now().UTC(),
	}
	if err := repo.CreatePaymentRefund(failedRefund); err != nil {
		t.Fatalf("CreatePaymentRefund failed refund returned error: %v", err)
	}
	if failed, err := repo.FailPaymentRefund(failedRefund.ID, "provider error"); err != nil || failed.Status != core.PaymentRefundFailed {
		t.Fatalf("FailPaymentRefund refund=%#v err=%v", failed, err)
	}
	retryRefund := failedRefund
	retryRefund.ID = "refund_retry"
	if err := repo.CreatePaymentRefund(retryRefund); err != nil {
		t.Fatalf("CreatePaymentRefund retry returned error: %v", err)
	}
	refunds := repo.ListPaymentRefunds(order.ID)
	if len(refunds) != 3 {
		t.Fatalf("refunds = %#v", refunds)
	}
	ledger := repo.ListBillingLedger(order.UserID, 10)
	if len(ledger) < 2 || ledger[0].Kind != "payment_refund" {
		t.Fatalf("ledger = %#v", ledger)
	}
}

func testPaymentRefundRejectsNonPendingCreate(t *testing.T, repo Repository) {
	t.Helper()
	if err := repo.UpsertUser(core.User{ID: "user_refund_status", Username: "refund-status", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:            "pay_refund_status",
		OutTradeNo:    "out_refund_status",
		UserID:        "user_refund_status",
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: core.NanoUSDPerUSD,
		Status:        core.PaymentOrderPending,
		CreatedAt:     time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	if _, credited, err := repo.CompletePaymentOrder(order.OutTradeNo, "trade_refund_status", order.AmountNanoUSD, time.Now().UTC()); err != nil || !credited {
		t.Fatalf("CompletePaymentOrder credited=%t err=%v", credited, err)
	}
	refund := core.PaymentRefund{
		ID:            "refund_status",
		OrderID:       order.ID,
		OutTradeNo:    order.OutTradeNo,
		UserID:        order.UserID,
		Provider:      order.Provider,
		AmountNanoUSD: order.AmountNanoUSD,
		Status:        core.PaymentRefundDone,
		CreatedAt:     time.Now().UTC(),
	}
	if err := repo.CreatePaymentRefund(refund); !errors.Is(err, ErrBillingRequestConflict) {
		t.Fatalf("CreatePaymentRefund err = %v, want ErrBillingRequestConflict", err)
	}
	if refunds := repo.ListPaymentRefunds(order.ID); len(refunds) != 0 {
		t.Fatalf("refunds = %#v, want none", refunds)
	}
}

func testPaymentRefundRejectsInsufficientBalance(t *testing.T, repo Repository) {
	t.Helper()
	userID := "user_refund_negative"
	if err := repo.UpsertUser(core.User{ID: userID, Username: "refund-negative", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:            "pay_refund_negative",
		OutTradeNo:    "out_refund_negative",
		UserID:        userID,
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: core.NanoUSDPerUSD,
		Status:        core.PaymentOrderPending,
		CreatedAt:     time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	if _, credited, err := repo.CompletePaymentOrder(order.OutTradeNo, "trade_refund_negative", order.AmountNanoUSD, time.Now().UTC()); err != nil || !credited {
		t.Fatalf("CompletePaymentOrder credited=%t err=%v", credited, err)
	}
	if err := repo.SetUserBalance(userID, 0); err != nil {
		t.Fatal(err)
	}
	refund := core.PaymentRefund{
		ID:            "refund_negative",
		OrderID:       order.ID,
		OutTradeNo:    order.OutTradeNo,
		UserID:        order.UserID,
		Provider:      order.Provider,
		AmountNanoUSD: order.AmountNanoUSD,
		Status:        core.PaymentRefundPending,
		CreatedAt:     time.Now().UTC(),
	}
	if err := repo.CreatePaymentRefund(refund); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("CreatePaymentRefund err = %v, want ErrInsufficientBalance", err)
	}
}

func testPaymentOrderExpiresPendingOrders(t *testing.T, repo Repository) {
	t.Helper()
	if err := repo.UpsertUser(core.User{ID: "user_expire", Username: "expire", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:            "pay_expire",
		OutTradeNo:    "out_expire",
		UserID:        "user_expire",
		Provider:      core.PaymentProviderWeChatPay,
		AmountNanoUSD: core.NanoUSDPerUSD,
		Status:        core.PaymentOrderPending,
		CreatedAt:     time.Now().UTC().Add(-core.DefaultPaymentOrderPendingTTL - time.Minute),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	expired, err := repo.GetPaymentOrder(order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if expired.Status != core.PaymentOrderClosed {
		t.Fatalf("expired status = %s, want %s", expired.Status, core.PaymentOrderClosed)
	}
	items, total := repo.ListPaymentOrdersPage(PaymentOrderQuery{UserID: "user_expire", Status: core.PaymentOrderClosed, Limit: 10})
	if total != 1 || len(items) != 1 || items[0].ID != order.ID {
		t.Fatalf("closed payment page total=%d items=%#v", total, items)
	}
	completed, credited, err := repo.CompletePaymentOrder(order.OutTradeNo, "trade_expire", order.AmountNanoUSD, time.Now().UTC())
	if err != nil || !credited || completed.Status != core.PaymentOrderPaid {
		t.Fatalf("complete expired order=%#v credited=%t err=%v, want paid true nil", completed, credited, err)
	}
	user, err := repo.GetUser("user_expire")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != order.AmountNanoUSD {
		t.Fatalf("balance = %d, want %d", user.BalanceNanoUSD, order.AmountNanoUSD)
	}
	if _, credited, err := repo.CompletePaymentOrder(order.OutTradeNo, "trade_expire", order.AmountNanoUSD, time.Now().UTC()); err != nil || credited {
		t.Fatalf("replay expired complete credited=%t err=%v, want false nil", credited, err)
	}
}
