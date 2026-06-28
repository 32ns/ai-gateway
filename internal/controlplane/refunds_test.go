package controlplane

import (
	"errors"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestManualPaymentRefundCapsRefundByAvailableBalance(t *testing.T) {
	repo := storage.NewMemoryRepository()
	userID := "user_manual_refund"
	if err := repo.UpsertUser(core.User{ID: userID, Username: "manual-refund", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:                    "pay_manual_refund",
		OutTradeNo:            "out_manual_refund",
		UserID:                userID,
		Provider:              core.PaymentProviderAlipay,
		Channel:               core.PaymentChannelPage,
		AmountNanoUSD:         100 * core.NanoUSDPerUSD,
		Currency:              "USD",
		ProviderAmountCents:   71000,
		ProviderCurrency:      "CNY",
		ExchangeRateCNYPerUSD: "7.10",
		Status:                core.PaymentOrderPending,
		CreatedAt:             time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	if _, credited, err := repo.CompletePaymentOrder(order.OutTradeNo, "trade_manual_refund", order.AmountNanoUSD, time.Now().UTC()); err != nil || !credited {
		t.Fatalf("CompletePaymentOrder credited=%t err=%v", credited, err)
	}
	if err := repo.SetUserBalance(userID, 30*core.NanoUSDPerUSD); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if _, summary, err := service.CreateManualPaymentRefund(ManualPaymentRefundInput{
		OrderID:       order.ID,
		AmountNanoUSD: 40 * core.NanoUSDPerUSD,
		Reason:        "customer request",
	}); !errors.Is(err, ErrPaymentRefundAmountUnavailable) {
		t.Fatalf("CreateManualPaymentRefund err = %v summary=%#v, want ErrPaymentRefundAmountUnavailable", err, summary)
	}
	refund, summary, err := service.CreateManualPaymentRefund(ManualPaymentRefundInput{
		OrderID:       order.ID,
		AmountNanoUSD: 30 * core.NanoUSDPerUSD,
		Reason:        "customer request",
	})
	if err != nil {
		t.Fatalf("CreateManualPaymentRefund returned error: %v", err)
	}
	if refund.Status != core.PaymentRefundPending || refund.ProviderAmountCents != 21300 || refund.ProviderCurrency != "CNY" {
		t.Fatalf("refund = %#v", refund)
	}
	if summary.RemainingNanoUSD != 70*core.NanoUSDPerUSD || summary.AvailableRefundNanoUSD != 0 || summary.PendingUserRefundNanoUSD != 30*core.NanoUSDPerUSD {
		t.Fatalf("summary = %#v", summary)
	}

	completed, debited, err := service.CompleteManualPaymentRefund(ManualPaymentRefundCompleteInput{
		RefundID:  refund.ID,
		PayoutRef: "offline-transfer-1",
		Note:      "paid by admin",
	})
	if err != nil {
		t.Fatalf("CompleteManualPaymentRefund returned error: %v", err)
	}
	if !debited || completed.Status != core.PaymentRefundDone || completed.ManualPayoutRef != "offline-transfer-1" || completed.ManualPayoutAt == nil {
		t.Fatalf("completed=%#v debited=%t", completed, debited)
	}
	user, err := repo.GetUser(userID)
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != 0 {
		t.Fatalf("balance = %d, want 0", user.BalanceNanoUSD)
	}
	if _, _, err := service.CreateManualPaymentRefund(ManualPaymentRefundInput{
		OrderID:       order.ID,
		AmountNanoUSD: 61 * core.NanoUSDPerUSD,
	}); !errors.Is(err, ErrPaymentRefundAmountUnavailable) {
		t.Fatalf("over refund err = %v, want ErrPaymentRefundAmountUnavailable", err)
	}
}
