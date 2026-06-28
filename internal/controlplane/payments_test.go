package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/payments"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
	personalpay "personalpay/sdk-go"
)

type fakePaymentClient struct {
	lastCreateInput payments.CreateOrderInput
	lastQueryInput  payments.QueryOrderInput
	lastCancelInput payments.CancelOrderInput
	partialCreate   bool
	queryResult     payments.QueryOrderResult
	cancelResult    payments.CancelOrderResult
	personalRuntime payments.PersonalPayRuntime
	queryCalls      int
	cancelCalls     int
	deleteDeviceID  string
	deleteCalls     int
	deleteErr       error
}

func (f *fakePaymentClient) CreateOrder(_ context.Context, input payments.CreateOrderInput) (payments.CreateOrderResult, error) {
	f.lastCreateInput = input
	order := input.Order
	order.PayURL = "https://pay.example.test/order/" + order.OutTradeNo
	if f.partialCreate {
		return payments.CreateOrderResult{
			Order: core.PaymentOrder{
				ID:                    "provider_" + order.ID,
				OutTradeNo:            "provider_" + order.OutTradeNo,
				UserID:                "provider_user",
				Provider:              core.PaymentProviderWeChatPay,
				Channel:               core.PaymentChannelNative,
				AmountNanoUSD:         999 * core.NanoUSDPerUSD,
				ProviderAmountCents:   999,
				ExchangeRateCNYPerUSD: "999",
				PayURL:                order.PayURL,
				CreatedAt:             order.CreatedAt.Add(24 * time.Hour),
				UpdatedAt:             order.UpdatedAt.Add(24 * time.Hour),
			},
			Payload: map[string]string{"pay_url": order.PayURL},
		}, nil
	}
	return payments.CreateOrderResult{Order: order, Payload: map[string]string{"pay_url": order.PayURL}}, nil
}

func (f *fakePaymentClient) QueryOrder(_ context.Context, input payments.QueryOrderInput) (payments.QueryOrderResult, error) {
	f.lastQueryInput = input
	f.queryCalls++
	return f.queryResult, nil
}

func (f *fakePaymentClient) CancelOrder(_ context.Context, input payments.CancelOrderInput) (payments.CancelOrderResult, error) {
	f.lastCancelInput = input
	f.cancelCalls++
	return f.cancelResult, nil
}

func (f *fakePaymentClient) PersonalPayRuntime(context.Context) payments.PersonalPayRuntime {
	return f.personalRuntime
}

func (f *fakePaymentClient) DeletePersonalPayDevice(_ context.Context, deviceID string) error {
	f.deleteDeviceID = deviceID
	f.deleteCalls++
	return f.deleteErr
}

func TestCreateAndCompletePaymentOrderCreditsUser(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	settings := core.DefaultSystemSettings()
	settings.Runtime.PublicBaseURL = "https://gateway.example.com"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	paymentClient := &fakePaymentClient{}
	service.payments = paymentClient

	created, err := service.CreatePaymentOrder(context.Background(), PaymentOrderInput{
		UserID:        "user_pay",
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: 500 * core.NanoUSDPerUSD / 100,
		Subject:       "Top up",
	})
	if err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}
	if created.Order.PayURL == "" || created.Order.Status != core.PaymentOrderPending {
		t.Fatalf("created order = %#v", created.Order)
	}
	order, credited, err := service.CompletePaymentOrder(payments.Notification{
		Provider:        core.PaymentProviderAlipay,
		OutTradeNo:      created.Order.OutTradeNo,
		ProviderTradeNo: "trade_1",
		AmountNanoUSD:   created.Order.AmountNanoUSD,
		PaidAt:          time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CompletePaymentOrder returned error: %v", err)
	}
	if !credited || order.Status != core.PaymentOrderPaid {
		t.Fatalf("order=%#v credited=%t", order, credited)
	}
	user, err := repo.GetUser("user_pay")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != created.Order.AmountNanoUSD {
		t.Fatalf("balance = %d, want %d", user.BalanceNanoUSD, created.Order.AmountNanoUSD)
	}
}

func TestCompletePaymentOrderCreditsInviterRechargeReward(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_inviter", Username: "alice", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertUser(core.User{ID: "user_invitee", Username: "bob", Enabled: true, InviterUserID: "user_inviter"}); err != nil {
		t.Fatal(err)
	}
	settings := core.DefaultSystemSettings()
	settings.Runtime.PublicBaseURL = "https://gateway.example.com"
	settings.Invitation.Enabled = true
	settings.Invitation.InviterRechargeRewardBps = 500
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	service.payments = &fakePaymentClient{}

	created, err := service.CreatePaymentOrder(context.Background(), PaymentOrderInput{
		UserID:        "user_invitee",
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: 100 * core.NanoUSDPerUSD,
		Subject:       "Top up",
	})
	if err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}
	notification := payments.Notification{
		Provider:        core.PaymentProviderAlipay,
		OutTradeNo:      created.Order.OutTradeNo,
		ProviderTradeNo: "trade_invite_reward",
		AmountNanoUSD:   created.Order.AmountNanoUSD,
		PaidAt:          time.Now().UTC(),
	}
	if _, credited, err := service.CompletePaymentOrder(notification); err != nil || !credited {
		t.Fatalf("CompletePaymentOrder credited=%t err=%v", credited, err)
	}

	invitee, err := repo.GetUser("user_invitee")
	if err != nil {
		t.Fatal(err)
	}
	if invitee.BalanceNanoUSD != 100*core.NanoUSDPerUSD {
		t.Fatalf("invitee balance = %d", invitee.BalanceNanoUSD)
	}
	inviter, err := repo.GetUser("user_inviter")
	if err != nil {
		t.Fatal(err)
	}
	if inviter.BalanceNanoUSD != 5*core.NanoUSDPerUSD {
		t.Fatalf("inviter balance = %d, want %d", inviter.BalanceNanoUSD, 5*core.NanoUSDPerUSD)
	}
	ledger := repo.ListBillingLedger("user_inviter", 10)
	if len(ledger) != 1 || ledger[0].Kind != "manual_credit" || ledger[0].AmountNanoUSD != 5*core.NanoUSDPerUSD || !strings.Contains(ledger[0].Note, "invite recharge reward: 5% bob payment") {
		t.Fatalf("inviter ledger = %#v", ledger)
	}

	if _, credited, err := service.CompletePaymentOrder(notification); err != nil || credited {
		t.Fatalf("replay CompletePaymentOrder credited=%t err=%v", credited, err)
	}
	inviter, err = repo.GetUser("user_inviter")
	if err != nil {
		t.Fatal(err)
	}
	if inviter.BalanceNanoUSD != 5*core.NanoUSDPerUSD {
		t.Fatalf("inviter replay balance = %d, want %d", inviter.BalanceNanoUSD, 5*core.NanoUSDPerUSD)
	}
	if ledger := repo.ListBillingLedger("user_inviter", 10); len(ledger) != 1 {
		t.Fatalf("inviter replay ledger = %#v", ledger)
	}
}

func TestCreatePaymentOrderDefaultsNotifyURLFromPublicBaseURL(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	settings := core.DefaultSystemSettings()
	settings.Runtime.PublicBaseURL = "https://gateway.example.com/"
	settings.Payment.WeChatPay.Enabled = true
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	paymentClient := &fakePaymentClient{}
	service.payments = paymentClient

	if _, err := service.CreatePaymentOrder(context.Background(), PaymentOrderInput{
		UserID:        "user_pay",
		Provider:      core.PaymentProviderWeChatPay,
		Channel:       core.PaymentChannelNative,
		AmountNanoUSD: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}
	if got := paymentClient.lastCreateInput.Settings.WeChatPay.NotifyURL; got != "https://gateway.example.com/payments/notify/wechatpay" {
		t.Fatalf("wechat notify url = %q", got)
	}
}

func TestCreatePersonalPayOrderAppliesCNYRate(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	settings := core.DefaultSystemSettings()
	settings.Runtime.PublicBaseURL = "https://gateway.example.com"
	settings.Payment.CNYPerUSD = "7.20"
	settings.Payment.PersonalPay.Enabled = true
	settings.Payment.PersonalPay.AndroidToken = "android-token"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	service.payments = &fakePaymentClient{}

	created, err := service.CreatePaymentOrder(context.Background(), PaymentOrderInput{
		UserID:        "user_pay",
		Provider:      core.PaymentProviderPersonalPay,
		AmountNanoUSD: core.NanoUSDPerUSD,
	})
	if err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}
	if created.Order.ProviderAmountCents != 720 {
		t.Fatalf("provider amount cents = %d, want 720", created.Order.ProviderAmountCents)
	}
	if created.Order.ExchangeRateCNYPerUSD != "7.20" {
		t.Fatalf("exchange rate = %q, want 7.20", created.Order.ExchangeRateCNYPerUSD)
	}
	if created.Order.ProviderCurrency != "CNY" || created.Order.Currency != "USD" {
		t.Fatalf("order currencies = %#v", created.Order)
	}
}

func TestCreatePersonalPayOrderAppliesCNYPaymentAmountInput(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	settings := core.DefaultSystemSettings()
	settings.Payment.CNYPerUSD = "0.1"
	settings.Payment.PersonalPay.Enabled = true
	settings.Payment.PersonalPay.AndroidToken = "android-token"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	service.payments = &fakePaymentClient{}

	created, err := service.CreatePaymentOrder(context.Background(), PaymentOrderInput{
		UserID:               "user_pay",
		Provider:             core.PaymentProviderPersonalPay,
		PaymentAmountNanoCNY: core.NanoUSDPerUSD,
	})
	if err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}
	if created.Order.ProviderAmountCents != 100 {
		t.Fatalf("provider amount cents = %d, want 100", created.Order.ProviderAmountCents)
	}
	if created.Order.ExchangeRateCNYPerUSD != "0.1" {
		t.Fatalf("exchange rate = %q, want 0.1", created.Order.ExchangeRateCNYPerUSD)
	}
	if created.Order.AmountNanoUSD != 10*core.NanoUSDPerUSD {
		t.Fatalf("amount nano usd = %d, want %d", created.Order.AmountNanoUSD, 10*core.NanoUSDPerUSD)
	}
	if created.Order.ProviderCurrency != "CNY" || created.Order.Currency != "USD" {
		t.Fatalf("order currencies = %#v", created.Order)
	}
}

func TestCreatePaymentOrderEnforcesRechargeLimits(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	settings := core.DefaultSystemSettings()
	settings.Payment.CNYPerUSD = "1"
	settings.Payment.MinRechargeNanoUSD = 2 * core.NanoUSDPerUSD
	settings.Payment.MaxRechargeNanoUSD = 5 * core.NanoUSDPerUSD
	settings.Payment.PersonalPay.Enabled = true
	settings.Payment.PersonalPay.AndroidToken = "android-token"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	service.payments = &fakePaymentClient{}

	_, err := service.CreatePaymentOrder(context.Background(), PaymentOrderInput{
		UserID:        "user_pay",
		Provider:      core.PaymentProviderPersonalPay,
		AmountNanoUSD: core.NanoUSDPerUSD,
	})
	if !errors.Is(err, ErrPaymentAmountBelowMinimum) {
		t.Fatalf("low amount error = %v, want ErrPaymentAmountBelowMinimum", err)
	}
	_, err = service.CreatePaymentOrder(context.Background(), PaymentOrderInput{
		UserID:        "user_pay",
		Provider:      core.PaymentProviderPersonalPay,
		AmountNanoUSD: 6 * core.NanoUSDPerUSD,
	})
	if !errors.Is(err, ErrPaymentAmountAboveMaximum) {
		t.Fatalf("high amount error = %v, want ErrPaymentAmountAboveMaximum", err)
	}
	if _, err = service.CreatePaymentOrder(context.Background(), PaymentOrderInput{
		UserID:        "user_pay",
		Provider:      core.PaymentProviderPersonalPay,
		AmountNanoUSD: 3 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatalf("CreatePaymentOrder in range returned error: %v", err)
	}

	settings.Payment.CNYPerUSD = "0.5"
	settings.Payment.RechargeInputMode = core.RechargeInputModePaymentCNY
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	_, err = service.CreatePaymentOrder(context.Background(), PaymentOrderInput{
		UserID:               "user_pay",
		Provider:             core.PaymentProviderPersonalPay,
		PaymentAmountNanoCNY: core.NanoUSDPerUSD / 2,
	})
	if !errors.Is(err, ErrPaymentAmountBelowMinimum) {
		t.Fatalf("low CNY payment error = %v, want ErrPaymentAmountBelowMinimum", err)
	}
}

func TestCreatePaymentOrderKeepsLocalBillingFieldsFromPartialProviderOrder(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	settings := core.DefaultSystemSettings()
	settings.Payment.CNYPerUSD = "7.20"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	service.payments = &fakePaymentClient{partialCreate: true}

	created, err := service.CreatePaymentOrder(context.Background(), PaymentOrderInput{
		UserID:        "user_pay",
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: core.NanoUSDPerUSD,
		NotifyBaseURL: "https://gateway.example.com",
	})
	if err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}
	if created.Order.UserID != "user_pay" || created.Order.Provider != core.PaymentProviderAlipay || created.Order.Channel != core.PaymentChannelPage {
		t.Fatalf("order identity fields were not preserved: %#v", created.Order)
	}
	if created.Order.AmountNanoUSD != core.NanoUSDPerUSD || created.Order.ProviderAmountCents != 720 {
		t.Fatalf("order amounts = amount %d provider cents %d, want $1 and 720 cents", created.Order.AmountNanoUSD, created.Order.ProviderAmountCents)
	}
	if created.Order.ExchangeRateCNYPerUSD != "7.20" {
		t.Fatalf("exchange rate = %q, want 7.20", created.Order.ExchangeRateCNYPerUSD)
	}
	if created.Order.CreatedAt.After(created.Order.UpdatedAt) || created.Order.UpdatedAt.Sub(created.Order.CreatedAt) > time.Second {
		t.Fatalf("order timestamps should stay local: created=%s updated=%s", created.Order.CreatedAt, created.Order.UpdatedAt)
	}
	if created.Order.Currency != "USD" || created.Order.ProviderCurrency != "CNY" || created.Order.PayURL == "" {
		t.Fatalf("order currencies/pay url = %#v", created.Order)
	}
}

func TestApplyPersonalPayEventUpdatesQRCodeAndCreditsUser(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:                  "pay_personalpay_event",
		OutTradeNo:          "out_personalpay_event",
		UserID:              "user_pay",
		Provider:            core.PaymentProviderPersonalPay,
		Channel:             core.PaymentChannelWeChat,
		AmountNanoUSD:       2 * core.NanoUSDPerUSD,
		Currency:            "USD",
		ProviderAmountCents: 1440,
		ProviderCurrency:    "CNY",
		Status:              core.PaymentOrderPending,
		CreatedAt:           time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	updated, credited, err := service.ApplyPaymentEvent(payments.Event{
		Provider:            core.PaymentProviderPersonalPay,
		OutTradeNo:          order.OutTradeNo,
		Status:              core.PaymentOrderPending,
		ProviderStatus:      "qr_ready",
		ProviderTradeNo:     "pp_qr_1",
		ProviderAmountCents: 1440,
		ProviderCurrency:    "CNY",
		CodeURL:             "weixin://wxpay/bizpayurl?pr=personalpay",
	})
	if err != nil || credited {
		t.Fatalf("ApplyPaymentEvent qr updated=%#v credited=%t err=%v", updated, credited, err)
	}
	if updated.CodeURL == "" || updated.ProviderStatus != "qr_ready" || updated.Status != core.PaymentOrderPending {
		t.Fatalf("qr update not persisted: %#v", updated)
	}

	paid, credited, err := service.ApplyPaymentEvent(payments.Event{
		Provider:            core.PaymentProviderPersonalPay,
		OutTradeNo:          order.OutTradeNo,
		Status:              core.PaymentOrderPaid,
		ProviderStatus:      "paid",
		ProviderTradeNo:     "pp_trade_1",
		ProviderAmountCents: 1440,
		ProviderCurrency:    "CNY",
		PaidAt:              time.Now().UTC(),
	})
	if err != nil || !credited || paid.Status != core.PaymentOrderPaid {
		t.Fatalf("ApplyPaymentEvent paid=%#v credited=%t err=%v", paid, credited, err)
	}
	user, err := repo.GetUser("user_pay")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != order.AmountNanoUSD {
		t.Fatalf("balance = %d, want %d", user.BalanceNanoUSD, order.AmountNanoUSD)
	}
}

func TestApplyPersonalPayPaidEventCreditsClosedOrder(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:                  "pay_personalpay_closed_paid",
		OutTradeNo:          "out_personalpay_closed_paid",
		UserID:              "user_pay",
		Provider:            core.PaymentProviderPersonalPay,
		Channel:             core.PaymentChannelWeChat,
		AmountNanoUSD:       2 * core.NanoUSDPerUSD,
		Currency:            "USD",
		ProviderAmountCents: 1440,
		ProviderCurrency:    "CNY",
		Status:              core.PaymentOrderClosed,
		ProviderStatus:      "canceled",
		CreatedAt:           time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	paid, credited, err := service.ApplyPaymentEvent(payments.Event{
		Provider:            core.PaymentProviderPersonalPay,
		OutTradeNo:          order.OutTradeNo,
		Status:              core.PaymentOrderPaid,
		ProviderStatus:      "paid",
		ProviderTradeNo:     "pp_trade_late_success",
		ProviderAmountCents: 1440,
		ProviderCurrency:    "CNY",
		PaidAt:              time.Now().UTC(),
	})
	if err != nil || !credited || paid.Status != core.PaymentOrderPaid {
		t.Fatalf("ApplyPaymentEvent paid=%#v credited=%t err=%v", paid, credited, err)
	}
	user, err := repo.GetUser("user_pay")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != order.AmountNanoUSD {
		t.Fatalf("balance = %d, want %d", user.BalanceNanoUSD, order.AmountNanoUSD)
	}
}

func TestConfirmPaymentOrderPaidManuallyCreditsClosedOrder(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:                  "pay_manual_confirm",
		OutTradeNo:          "out_manual_confirm",
		UserID:              "user_pay",
		Provider:            core.PaymentProviderPersonalPay,
		Channel:             core.PaymentChannelWeChat,
		AmountNanoUSD:       2 * core.NanoUSDPerUSD,
		Currency:            "USD",
		ProviderAmountCents: 200,
		ProviderCurrency:    "CNY",
		Status:              core.PaymentOrderClosed,
		CreatedAt:           time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	paid, credited, err := service.ConfirmPaymentOrderPaidManually(ManualPaymentConfirmInput{OrderID: order.ID})
	if err != nil || !credited || paid.Status != core.PaymentOrderPaid || paid.ProviderTradeNo != "manual_pay_manual_confirm" {
		t.Fatalf("ConfirmPaymentOrderPaidManually paid=%#v credited=%t err=%v", paid, credited, err)
	}
	user, err := repo.GetUser("user_pay")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != order.AmountNanoUSD {
		t.Fatalf("balance = %d, want %d", user.BalanceNanoUSD, order.AmountNanoUSD)
	}
	if replay, credited, err := service.ConfirmPaymentOrderPaidManually(ManualPaymentConfirmInput{OrderID: order.ID}); err != nil || credited || replay.Status != core.PaymentOrderPaid {
		t.Fatalf("replay paid=%#v credited=%t err=%v", replay, credited, err)
	}
	user, err = repo.GetUser("user_pay")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != order.AmountNanoUSD {
		t.Fatalf("replay balance = %d, want %d", user.BalanceNanoUSD, order.AmountNanoUSD)
	}
	callback, credited, err := service.ApplyPaymentEvent(payments.Event{
		Provider:            order.Provider,
		OutTradeNo:          order.OutTradeNo,
		Status:              core.PaymentOrderPaid,
		ProviderStatus:      "paid",
		ProviderTradeNo:     "real_provider_trade_after_manual_confirm",
		ProviderAmountCents: order.ProviderAmountCents,
		PaidAt:              time.Now().UTC(),
	})
	if err != nil || credited || callback.Status != core.PaymentOrderPaid {
		t.Fatalf("callback after manual confirm order=%#v credited=%t err=%v", callback, credited, err)
	}
	user, err = repo.GetUser("user_pay")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != order.AmountNanoUSD {
		t.Fatalf("callback balance = %d, want %d", user.BalanceNanoUSD, order.AmountNanoUSD)
	}
}

func TestConfirmPaymentOrderPaidManuallyRejectsPendingOrder(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:            "pay_manual_pending",
		OutTradeNo:    "out_manual_pending",
		UserID:        "user_pay",
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: core.NanoUSDPerUSD,
		Status:        core.PaymentOrderPending,
		CreatedAt:     time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	if _, credited, err := service.ConfirmPaymentOrderPaidManually(ManualPaymentConfirmInput{OrderID: order.ID}); err == nil || credited || !strings.Contains(err.Error(), "cannot be manually confirmed") {
		t.Fatalf("ConfirmPaymentOrderPaidManually credited=%t err=%v, want pending rejection", credited, err)
	}
}

func TestApplyPersonalPayEventRejectsWrongProviderAmount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:                  "pay_personalpay_wrong_amount",
		OutTradeNo:          "out_personalpay_wrong_amount",
		UserID:              "user_pay",
		Provider:            core.PaymentProviderPersonalPay,
		Channel:             core.PaymentChannelWeChat,
		AmountNanoUSD:       core.NanoUSDPerUSD,
		Currency:            "USD",
		ProviderAmountCents: 720,
		ProviderCurrency:    "CNY",
		Status:              core.PaymentOrderPending,
		CreatedAt:           time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	if _, credited, err := service.ApplyPaymentEvent(payments.Event{
		Provider:            core.PaymentProviderPersonalPay,
		OutTradeNo:          order.OutTradeNo,
		Status:              core.PaymentOrderPaid,
		ProviderStatus:      "paid",
		ProviderTradeNo:     "pp_trade_wrong",
		ProviderAmountCents: 719,
		ProviderCurrency:    "CNY",
		PaidAt:              time.Now().UTC(),
	}); err == nil || !strings.Contains(err.Error(), "amount") || credited {
		t.Fatalf("ApplyPaymentEvent err=%v credited=%t, want amount mismatch", err, credited)
	}
}

func TestCreateAlipayOrderDefaultsReturnURLFromRequestBaseURL(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	paymentClient := &fakePaymentClient{}
	service.payments = paymentClient

	if _, err := service.CreatePaymentOrder(context.Background(), PaymentOrderInput{
		UserID:        "user_pay",
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: core.NanoUSDPerUSD,
		NotifyBaseURL: "https://request.example.com",
	}); err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}
	if got := paymentClient.lastCreateInput.Settings.Alipay.NotifyURL; got != "https://request.example.com/payments/notify/alipay" {
		t.Fatalf("alipay notify url = %q", got)
	}
	if got := paymentClient.lastCreateInput.Settings.Alipay.ReturnURL; got != "https://request.example.com/payments/return/alipay" {
		t.Fatalf("alipay return url = %q", got)
	}
}

func TestCreatePaymentOrderRequiresNotifyURL(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	service.payments = &fakePaymentClient{}

	if _, err := service.CreatePaymentOrder(context.Background(), PaymentOrderInput{
		UserID:        "user_pay",
		Provider:      core.PaymentProviderWeChatPay,
		Channel:       core.PaymentChannelNative,
		AmountNanoUSD: core.NanoUSDPerUSD,
	}); err == nil || !strings.Contains(err.Error(), "notify url is required") {
		t.Fatalf("CreatePaymentOrder err = %v, want notify url required", err)
	}
}

func TestCreatePaymentOrderUsesRequestBaseURLForNotifyURL(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	paymentClient := &fakePaymentClient{}
	service.payments = paymentClient

	if _, err := service.CreatePaymentOrder(context.Background(), PaymentOrderInput{
		UserID:        "user_pay",
		Provider:      core.PaymentProviderWeChatPay,
		Channel:       core.PaymentChannelNative,
		AmountNanoUSD: core.NanoUSDPerUSD,
		NotifyBaseURL: "https://request.example.com",
	}); err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}
	if got := paymentClient.lastCreateInput.Settings.WeChatPay.NotifyURL; got != "https://request.example.com/payments/notify/wechatpay" {
		t.Fatalf("wechat notify url = %q", got)
	}
}

func TestRefreshPaymentOrderDoesNotRequireNotifyURL(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	paymentClient := &fakePaymentClient{}
	service.payments = paymentClient

	created, err := service.CreatePaymentOrder(context.Background(), PaymentOrderInput{
		UserID:        "user_pay",
		Provider:      core.PaymentProviderWeChatPay,
		Channel:       core.PaymentChannelNative,
		AmountNanoUSD: core.NanoUSDPerUSD,
		NotifyBaseURL: "https://request.example.com",
	})
	if err != nil {
		t.Fatalf("CreatePaymentOrder returned error: %v", err)
	}
	if _, _, err := service.RefreshPaymentOrder(context.Background(), core.User{ID: "user_pay", Role: core.UserRoleUser}, created.Order.ID); err != nil {
		t.Fatalf("RefreshPaymentOrder returned error: %v", err)
	}
	if paymentClient.lastQueryInput.Order.ID != created.Order.ID {
		t.Fatalf("query order = %#v", paymentClient.lastQueryInput.Order)
	}
}

func TestRefreshPaymentOrderSkipsNonPendingOrder(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetUserBalance("user_pay", core.NanoUSDPerUSD); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:            "pay_closed",
		OutTradeNo:    "out_closed",
		UserID:        "user_pay",
		Provider:      core.PaymentProviderWeChatPay,
		Channel:       core.PaymentChannelNative,
		AmountNanoUSD: core.NanoUSDPerUSD,
		Status:        core.PaymentOrderClosed,
		CreatedAt:     time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	paymentClient := &fakePaymentClient{}
	service.payments = paymentClient

	refreshed, credited, err := service.RefreshPaymentOrder(context.Background(), core.User{ID: "user_pay", Role: core.UserRoleUser}, order.ID)
	if err != nil {
		t.Fatalf("RefreshPaymentOrder returned error: %v", err)
	}
	if credited || refreshed.Status != core.PaymentOrderClosed {
		t.Fatalf("refreshed=%#v credited=%t", refreshed, credited)
	}
	if paymentClient.queryCalls != 0 {
		t.Fatalf("payment query calls = %d, want 0", paymentClient.queryCalls)
	}
}

func TestCancelPaymentOrderKeepsOwnedPendingOrder(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertUser(core.User{ID: "other", Username: "other", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:            "pay_cancel",
		OutTradeNo:    "out_cancel",
		UserID:        "user_pay",
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: core.NanoUSDPerUSD,
		Status:        core.PaymentOrderPending,
		CreatedAt:     time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	service.payments = &fakePaymentClient{}
	if _, deleted, credited, err := service.CancelPaymentOrder(context.Background(), core.User{ID: "other", Role: core.UserRoleUser}, order.ID); !errors.Is(err, storage.ErrNotFound) || deleted || credited {
		t.Fatalf("foreign cancel deleted=%t err=%v, want ErrNotFound", deleted, err)
	}
	canceled, deleted, credited, err := service.CancelPaymentOrder(context.Background(), core.User{ID: "user_pay", Role: core.UserRoleUser}, order.ID)
	if err != nil || deleted || credited || canceled.Status != core.PaymentOrderPending {
		t.Fatalf("CancelPaymentOrder order=%#v deleted=%t credited=%t err=%v, want pending false false nil", canceled, deleted, credited, err)
	}
	if existing, err := repo.GetPaymentOrder(order.ID); err != nil || existing.Status != core.PaymentOrderPending {
		t.Fatalf("GetPaymentOrder existing=%#v err=%v, want pending order", existing, err)
	}
}

func TestCancelPaymentOrderDoesNotCancelFreshPersonalPayOrder(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	settings := core.DefaultSystemSettings()
	settings.Payment.CNYPerUSD = "1"
	settings.Payment.PersonalPay.Enabled = true
	settings.Payment.PersonalPay.AndroidToken = "android-token"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:                  "pay_personalpay_cancel",
		OutTradeNo:          "out_personalpay_cancel",
		UserID:              "user_pay",
		Provider:            core.PaymentProviderPersonalPay,
		Channel:             core.PaymentChannelWeChat,
		AmountNanoUSD:       core.NanoUSDPerUSD,
		ProviderAmountCents: 100,
		ProviderCurrency:    "CNY",
		Status:              core.PaymentOrderPending,
		CreatedAt:           time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	paymentClient := &fakePaymentClient{queryResult: payments.QueryOrderResult{
		Provider:            core.PaymentProviderPersonalPay,
		OutTradeNo:          order.OutTradeNo,
		ProviderTradeNo:     "pp_trade_1",
		Status:              core.PaymentOrderPending,
		ProviderStatus:      "paying",
		ProviderAmountCents: 100,
		ProviderCurrency:    "CNY",
	}}
	service.payments = paymentClient

	canceled, deleted, credited, err := service.CancelPaymentOrder(context.Background(), core.User{ID: "user_pay", Role: core.UserRoleUser}, order.ID)
	if err != nil || deleted || credited {
		t.Fatalf("CancelPaymentOrder deleted=%t credited=%t err=%v", deleted, credited, err)
	}
	if canceled.Status != core.PaymentOrderPending || canceled.ProviderStatus != "paying" || paymentClient.cancelCalls != 0 || paymentClient.queryCalls != 1 {
		t.Fatalf("canceled=%#v cancelCalls=%d queryCalls=%d", canceled, paymentClient.cancelCalls, paymentClient.queryCalls)
	}
}

func TestCancelPaymentOrderReleasesExpiredPersonalPayOrder(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	settings := core.DefaultSystemSettings()
	settings.Payment.CNYPerUSD = "1"
	settings.Payment.PersonalPay.Enabled = true
	settings.Payment.PersonalPay.AndroidToken = "android-token"
	settings.Payment.PersonalPay.ExpireAfterSec = 2
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:                  "pay_personalpay_expired_cancel",
		OutTradeNo:          "out_personalpay_expired_cancel",
		UserID:              "user_pay",
		Provider:            core.PaymentProviderPersonalPay,
		Channel:             core.PaymentChannelWeChat,
		AmountNanoUSD:       core.NanoUSDPerUSD,
		ProviderAmountCents: 100,
		ProviderCurrency:    "CNY",
		Status:              core.PaymentOrderPending,
		CreatedAt:           time.Now().UTC().Add(-3 * time.Second),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	paymentClient := &fakePaymentClient{
		queryResult: payments.QueryOrderResult{
			Provider:            core.PaymentProviderPersonalPay,
			OutTradeNo:          order.OutTradeNo,
			Status:              core.PaymentOrderPending,
			ProviderStatus:      "paying",
			ProviderAmountCents: 100,
			ProviderCurrency:    "CNY",
		},
		cancelResult: payments.CancelOrderResult{
			Provider:            core.PaymentProviderPersonalPay,
			OutTradeNo:          order.OutTradeNo,
			ProviderTradeNo:     "pp_expired_cancel",
			Status:              core.PaymentOrderClosed,
			ProviderStatus:      "canceled",
			ProviderAmountCents: 100,
			ProviderCurrency:    "CNY",
		},
	}
	service.payments = paymentClient

	canceled, deleted, credited, err := service.CancelPaymentOrder(context.Background(), core.User{ID: "user_pay", Role: core.UserRoleUser}, order.ID)
	if err != nil || deleted || credited {
		t.Fatalf("CancelPaymentOrder deleted=%t credited=%t err=%v", deleted, credited, err)
	}
	if canceled.Status != core.PaymentOrderClosed || canceled.ProviderStatus != "canceled" || paymentClient.cancelCalls != 1 || paymentClient.queryCalls != 1 {
		t.Fatalf("canceled=%#v cancelCalls=%d queryCalls=%d", canceled, paymentClient.cancelCalls, paymentClient.queryCalls)
	}
}

func TestCancelPaymentOrderUsesPersonalPayOrderRequestExpiry(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	settings := core.DefaultSystemSettings()
	settings.Payment.CNYPerUSD = "1"
	settings.Payment.PersonalPay.Enabled = true
	settings.Payment.PersonalPay.AndroidToken = "android-token"
	settings.Payment.PersonalPay.ExpireAfterSec = 1
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:                  "pay_personalpay_request_expiry",
		OutTradeNo:          "out_personalpay_request_expiry",
		UserID:              "user_pay",
		Provider:            core.PaymentProviderPersonalPay,
		Channel:             core.PaymentChannelWeChat,
		AmountNanoUSD:       core.NanoUSDPerUSD,
		ProviderAmountCents: 100,
		ProviderCurrency:    "CNY",
		Status:              core.PaymentOrderPending,
		CreatedAt:           time.Now().UTC().Add(-2 * time.Second),
		RawRequest:          `{"expireAfterSec":5}`,
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	paymentClient := &fakePaymentClient{queryResult: payments.QueryOrderResult{
		Provider:            core.PaymentProviderPersonalPay,
		OutTradeNo:          order.OutTradeNo,
		Status:              core.PaymentOrderPending,
		ProviderStatus:      "paying",
		ProviderAmountCents: 100,
		ProviderCurrency:    "CNY",
	}}
	service.payments = paymentClient

	updated, deleted, credited, err := service.CancelPaymentOrder(context.Background(), core.User{ID: "user_pay", Role: core.UserRoleUser}, order.ID)
	if err != nil || deleted || credited {
		t.Fatalf("CancelPaymentOrder deleted=%t credited=%t err=%v", deleted, credited, err)
	}
	if updated.Status != core.PaymentOrderPending || paymentClient.cancelCalls != 0 || paymentClient.queryCalls != 1 {
		t.Fatalf("updated=%#v cancelCalls=%d queryCalls=%d", updated, paymentClient.cancelCalls, paymentClient.queryCalls)
	}
}

func TestReleasePersonalPayAccountOrderCancelsOccupiedOrder(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	settings := core.DefaultSystemSettings()
	settings.Payment.CNYPerUSD = "1"
	settings.Payment.PersonalPay.Enabled = true
	settings.Payment.PersonalPay.AndroidToken = "android-token"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:                  "pay_release",
		OutTradeNo:          "out_release",
		UserID:              "user_pay",
		Provider:            core.PaymentProviderPersonalPay,
		Channel:             core.PaymentChannelWeChat,
		AmountNanoUSD:       core.NanoUSDPerUSD,
		ProviderAmountCents: 100,
		ProviderCurrency:    "CNY",
		Status:              core.PaymentOrderPending,
		CreatedAt:           time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	paymentClient := &fakePaymentClient{
		personalRuntime: payments.PersonalPayRuntime{
			Accounts: []personalpay.Account{{
				ID:         "account-1",
				Channel:    personalpay.ChannelWeChat,
				Status:     personalpay.AccountOccupied,
				OccupiedBy: order.OutTradeNo,
			}},
		},
		cancelResult: payments.CancelOrderResult{
			Provider:            core.PaymentProviderPersonalPay,
			OutTradeNo:          order.OutTradeNo,
			ProviderTradeNo:     "pp_release",
			Status:              core.PaymentOrderClosed,
			ProviderStatus:      "canceled",
			ProviderAmountCents: 100,
			ProviderCurrency:    "CNY",
		},
	}
	service.payments = paymentClient

	released, err := service.ReleasePersonalPayAccountOrder(context.Background(), "account-1")
	if err != nil {
		t.Fatalf("ReleasePersonalPayAccountOrder returned error: %v", err)
	}
	if released.Status != core.PaymentOrderClosed || released.ProviderStatus != "canceled" {
		t.Fatalf("released order = %#v", released)
	}
	if paymentClient.cancelCalls != 1 || paymentClient.lastCancelInput.Order.OutTradeNo != order.OutTradeNo {
		t.Fatalf("cancelCalls=%d lastCancel=%#v", paymentClient.cancelCalls, paymentClient.lastCancelInput)
	}
}

func TestCancelPaymentOrderCompletesPaidProviderOrder(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:            "pay_cancel_paid",
		OutTradeNo:    "out_cancel_paid",
		UserID:        "user_pay",
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: core.NanoUSDPerUSD,
		Status:        core.PaymentOrderPending,
		CreatedAt:     time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	service.payments = &fakePaymentClient{queryResult: payments.QueryOrderResult{
		Status:          core.PaymentOrderPaid,
		ProviderTradeNo: "trade_cancel_paid",
		AmountNanoUSD:   order.AmountNanoUSD,
		PaidAt:          time.Now().UTC(),
	}}

	completed, deleted, credited, err := service.CancelPaymentOrder(context.Background(), core.User{ID: "user_pay", Role: core.UserRoleUser}, order.ID)
	if err != nil || deleted || !credited || completed.Status != core.PaymentOrderPaid {
		t.Fatalf("CancelPaymentOrder completed=%#v deleted=%t credited=%t err=%v", completed, deleted, credited, err)
	}
	user, err := repo.GetUser("user_pay")
	if err != nil {
		t.Fatal(err)
	}
	if user.BalanceNanoUSD != order.AmountNanoUSD {
		t.Fatalf("balance = %d, want %d", user.BalanceNanoUSD, order.AmountNanoUSD)
	}
}

func TestCreatePaymentOrderRejectsSubCentAmount(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	service.payments = &fakePaymentClient{}

	if _, err := service.CreatePaymentOrder(context.Background(), PaymentOrderInput{
		UserID:        "user_pay",
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelPage,
		AmountNanoUSD: core.NanoUSDPerUSD/100 + 1,
	}); err == nil {
		t.Fatal("expected sub-cent payment amount to be rejected")
	}
}

func TestCreatePaymentOrderRejectsProviderChannelMismatch(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_pay", Username: "pay", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	service.payments = &fakePaymentClient{}

	if _, err := service.CreatePaymentOrder(context.Background(), PaymentOrderInput{
		UserID:        "user_pay",
		Provider:      core.PaymentProviderAlipay,
		Channel:       core.PaymentChannelNative,
		AmountNanoUSD: core.NanoUSDPerUSD,
	}); err == nil {
		t.Fatal("expected alipay native channel to be rejected")
	}
}

func TestCompletePaymentOrderRejectsIncompleteNotification(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))

	if _, _, err := service.CompletePaymentOrder(payments.Notification{
		Provider:      core.PaymentProviderAlipay,
		OutTradeNo:    "out_1",
		AmountNanoUSD: core.NanoUSDPerUSD,
	}); err == nil {
		t.Fatal("expected missing provider trade number to be rejected")
	}
}
