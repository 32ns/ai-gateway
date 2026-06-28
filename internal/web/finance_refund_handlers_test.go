package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestFinanceManualRefundWorkflow(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user := core.User{ID: "user_web_refund", Username: "web-refund", Enabled: true}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	order := core.PaymentOrder{
		ID:                    "pay_web_refund",
		OutTradeNo:            "out_web_refund",
		UserID:                user.ID,
		Provider:              core.PaymentProviderAlipay,
		Channel:               core.PaymentChannelPage,
		AmountNanoUSD:         5 * core.NanoUSDPerUSD,
		Currency:              "USD",
		ProviderAmountCents:   3500,
		ProviderCurrency:      "CNY",
		ExchangeRateCNYPerUSD: "7.00",
		Status:                core.PaymentOrderPending,
		CreatedAt:             time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}
	if _, credited, err := repo.CompletePaymentOrder(order.OutTradeNo, "trade_web_refund", order.AmountNanoUSD, time.Now().UTC()); err != nil || !credited {
		t.Fatalf("CompletePaymentOrder credited=%t err=%v", credited, err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	pageReq := httptest.NewRequest(http.MethodGet, "/admin/finance?tab=orders", nil)
	pageRec := httptest.NewRecorder()
	handler.ServeHTTP(pageRec, pageReq)
	if pageRec.Code != http.StatusOK {
		t.Fatalf("finance page status=%d body=%s", pageRec.Code, pageRec.Body.String())
	}
	for _, want := range []string{`data-group-settings-open="payment-refund-pay_web_refund"`, `class="settings-dialog finance-refund-dialog"`, `action="/admin/finance/refunds/create"`, "Refundable", "$5.00", "\u00a535.00"} {
		if !strings.Contains(pageRec.Body.String(), want) {
			t.Fatalf("finance page missing %q: %s", want, pageRec.Body.String())
		}
	}

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("order_id", order.ID)
	form.Set("amount_usd", "2.50")
	form.Set("reason", "manual refund")
	createReq := httptest.NewRequest(http.MethodPost, "/admin/finance/refunds/create", strings.NewReader(form.Encode()))
	createReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusSeeOther || createRec.Header().Get("Location") != "/admin/finance?tab=orders&notice=refund_created" {
		t.Fatalf("create status=%d location=%q body=%s", createRec.Code, createRec.Header().Get("Location"), createRec.Body.String())
	}
	refunds := repo.ListPaymentRefunds(order.ID)
	if len(refunds) != 1 || refunds[0].Status != core.PaymentRefundPending || refunds[0].ProviderAmountCents != 1750 {
		t.Fatalf("refunds after create = %#v", refunds)
	}

	completeForm := url.Values{}
	completeForm.Set("csrf_token", testConsoleCSRFToken)
	completeForm.Set("refund_id", refunds[0].ID)
	completeForm.Set("payout_ref", "offline-payout-1")
	completeForm.Set("note", "paid offline")
	completeReq := httptest.NewRequest(http.MethodPost, "/admin/finance/refunds/complete", strings.NewReader(completeForm.Encode()))
	completeReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	completeRec := httptest.NewRecorder()
	handler.ServeHTTP(completeRec, completeReq)
	if completeRec.Code != http.StatusSeeOther || completeRec.Header().Get("Location") != "/admin/finance?tab=orders&notice=refund_completed" {
		t.Fatalf("complete status=%d location=%q body=%s", completeRec.Code, completeRec.Header().Get("Location"), completeRec.Body.String())
	}
	refunds = repo.ListPaymentRefunds(order.ID)
	if len(refunds) != 1 || refunds[0].Status != core.PaymentRefundDone || refunds[0].ManualPayoutRef != "offline-payout-1" || refunds[0].ManualPayoutAt == nil {
		t.Fatalf("refunds after complete = %#v", refunds)
	}
	updated, err := repo.GetUser(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.BalanceNanoUSD != 250*core.NanoUSDPerUSD/100 {
		t.Fatalf("balance = %d, want %d", updated.BalanceNanoUSD, 250*core.NanoUSDPerUSD/100)
	}
}

func TestFinancePaymentRefundColumnFollowsPaymentStatus(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user := core.User{ID: "user_refund_status", Username: "refund-status", Enabled: true}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	now := time.Now().UTC()
	paidOrder := core.PaymentOrder{
		ID:                    "pay_refund_status_paid",
		OutTradeNo:            "out_refund_status_paid",
		UserID:                user.ID,
		Provider:              core.PaymentProviderAlipay,
		Channel:               core.PaymentChannelPage,
		AmountNanoUSD:         5 * core.NanoUSDPerUSD,
		Currency:              "USD",
		ProviderAmountCents:   3500,
		ProviderCurrency:      "CNY",
		ExchangeRateCNYPerUSD: "7.00",
		Status:                core.PaymentOrderPending,
		CreatedAt:             now,
	}
	if err := repo.CreatePaymentOrder(paidOrder); err != nil {
		t.Fatalf("CreatePaymentOrder paid returned error: %v", err)
	}
	if _, credited, err := repo.CompletePaymentOrder(paidOrder.OutTradeNo, "trade_refund_status_paid", paidOrder.AmountNanoUSD, now); err != nil || !credited {
		t.Fatalf("CompletePaymentOrder credited=%t err=%v", credited, err)
	}
	failedOrder := core.PaymentOrder{
		ID:                    "pay_refund_status_failed",
		OutTradeNo:            "out_refund_status_failed",
		UserID:                user.ID,
		Provider:              core.PaymentProviderAlipay,
		Channel:               core.PaymentChannelPage,
		AmountNanoUSD:         3 * core.NanoUSDPerUSD,
		Currency:              "USD",
		ProviderAmountCents:   2100,
		ProviderCurrency:      "CNY",
		ExchangeRateCNYPerUSD: "7.00",
		Status:                core.PaymentOrderFailed,
		CreatedAt:             now.Add(time.Second),
	}
	if err := repo.CreatePaymentOrder(failedOrder); err != nil {
		t.Fatalf("CreatePaymentOrder failed returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/admin/finance?tab=orders&order_status=", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("finance page status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`data-group-settings-open="payment-refund-pay_refund_status_paid"`, "Manual Refund", "failed", "Refundable</span><strong>$0.00</strong>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("finance page missing %q: %s", want, body)
		}
	}
	for _, unwanted := range []string{`data-group-settings-open="payment-refund-pay_refund_status_failed"`, `id="payment-refund-pay_refund_status_failed"`} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("finance page should not include %q: %s", unwanted, body)
		}
	}
}

func TestFinanceManualConfirmPaymentPaidCreditsUser(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user := core.User{ID: "user_manual_paid", Username: "manual-paid", Enabled: true}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	order := core.PaymentOrder{
		ID:                    "pay_manual_paid",
		OutTradeNo:            "out_manual_paid",
		UserID:                user.ID,
		Provider:              core.PaymentProviderPersonalPay,
		Channel:               core.PaymentChannelWeChat,
		AmountNanoUSD:         9 * core.NanoUSDPerUSD,
		Currency:              "USD",
		ProviderAmountCents:   900,
		ProviderCurrency:      "CNY",
		ExchangeRateCNYPerUSD: "1.00",
		Status:                core.PaymentOrderClosed,
		CreatedAt:             time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	pageReq := httptest.NewRequest(http.MethodGet, "/admin/finance?tab=orders&order_status=closed", nil)
	pageRec := httptest.NewRecorder()
	handler.ServeHTTP(pageRec, pageReq)
	if pageRec.Code != http.StatusOK {
		t.Fatalf("finance page status=%d body=%s", pageRec.Code, pageRec.Body.String())
	}
	for _, want := range []string{`action="/admin/finance/orders/confirm-paid"`, `data-confirm="Confirm this order was actually paid and credit the user balance?"`, "Mark Paid"} {
		if !strings.Contains(pageRec.Body.String(), want) {
			t.Fatalf("finance page missing %q: %s", want, pageRec.Body.String())
		}
	}

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("order_id", order.ID)
	confirmReq := httptest.NewRequest(http.MethodPost, "/admin/finance/orders/confirm-paid", strings.NewReader(form.Encode()))
	confirmReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	confirmRec := httptest.NewRecorder()
	handler.ServeHTTP(confirmRec, confirmReq)
	if confirmRec.Code != http.StatusSeeOther || confirmRec.Header().Get("Location") != "/admin/finance?tab=orders&order_status=&notice=payment_confirmed_paid" {
		t.Fatalf("confirm status=%d location=%q body=%s", confirmRec.Code, confirmRec.Header().Get("Location"), confirmRec.Body.String())
	}
	updatedOrder, err := repo.GetPaymentOrder(order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedOrder.Status != core.PaymentOrderPaid || updatedOrder.PaidAt == nil || updatedOrder.ProviderTradeNo != "manual_pay_manual_paid" {
		t.Fatalf("updated order = %#v", updatedOrder)
	}
	updatedUser, err := repo.GetUser(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedUser.BalanceNanoUSD != order.AmountNanoUSD {
		t.Fatalf("balance = %d, want %d", updatedUser.BalanceNanoUSD, order.AmountNanoUSD)
	}

	replayReq := httptest.NewRequest(http.MethodPost, "/admin/finance/orders/confirm-paid", strings.NewReader(form.Encode()))
	replayReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	replayRec := httptest.NewRecorder()
	handler.ServeHTTP(replayRec, replayReq)
	if replayRec.Code != http.StatusSeeOther {
		t.Fatalf("replay status=%d body=%s", replayRec.Code, replayRec.Body.String())
	}
	updatedUser, err = repo.GetUser(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedUser.BalanceNanoUSD != order.AmountNanoUSD {
		t.Fatalf("balance after replay = %d, want %d", updatedUser.BalanceNanoUSD, order.AmountNanoUSD)
	}
}
