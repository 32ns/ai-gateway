package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/payments"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

type paymentHandlerFakeClient struct {
	queryResult payments.QueryOrderResult
	queryCalls  int
}

func (c *paymentHandlerFakeClient) CreateOrder(_ context.Context, input payments.CreateOrderInput) (payments.CreateOrderResult, error) {
	return payments.CreateOrderResult{Order: input.Order}, nil
}

func (c *paymentHandlerFakeClient) QueryOrder(_ context.Context, _ payments.QueryOrderInput) (payments.QueryOrderResult, error) {
	c.queryCalls++
	return c.queryResult, nil
}

func (c *paymentHandlerFakeClient) CancelOrder(_ context.Context, input payments.CancelOrderInput) (payments.CancelOrderResult, error) {
	return payments.CancelOrderResult{Provider: input.Order.Provider, OutTradeNo: input.Order.OutTradeNo, Status: core.PaymentOrderClosed}, nil
}

func (c *paymentHandlerFakeClient) PersonalPayRuntime(context.Context) payments.PersonalPayRuntime {
	return payments.PersonalPayRuntime{}
}

func (c *paymentHandlerFakeClient) DeletePersonalPayDevice(context.Context, string) error {
	return nil
}

func TestPaymentStatusRefreshQueryIsReadOnly(t *testing.T) {
	repo := storage.NewMemoryRepository()
	user := core.User{ID: "user_pay", Username: "pay", Role: core.UserRoleUser, Enabled: true}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatal(err)
	}
	order := core.PaymentOrder{
		ID:            "pay_readonly",
		OutTradeNo:    "out_readonly",
		UserID:        user.ID,
		Provider:      core.PaymentProviderWeChatPay,
		Channel:       core.PaymentChannelNative,
		AmountNanoUSD: core.NanoUSDPerUSD,
		Status:        core.PaymentOrderPending,
		CodeURL:       "weixin://wxpay/bizpayurl?pr=readonly",
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	if err := repo.CreatePaymentOrder(order); err != nil {
		t.Fatal(err)
	}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	fakePayments := &paymentHandlerFakeClient{queryResult: payments.QueryOrderResult{
		Provider:        order.Provider,
		OutTradeNo:      order.OutTradeNo,
		ProviderTradeNo: "trade_paid",
		Status:          core.PaymentOrderPaid,
		AmountNanoUSD:   order.AmountNanoUSD,
		PaidAt:          time.Now().UTC(),
	}}
	control.SetPaymentClient(fakePayments)
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/payments/status?id=pay_readonly&refresh=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if fakePayments.queryCalls != 0 {
		t.Fatalf("GET payment status triggered payment query %d time(s)", fakePayments.queryCalls)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode status payload: %v", err)
	}
	if payload["paid"] == true || payload["order_status"] != string(core.PaymentOrderPending) {
		t.Fatalf("payload = %#v, want pending unpaid status", payload)
	}
	stored, err := repo.GetPaymentOrder(order.ID)
	if err != nil {
		t.Fatalf("GetPaymentOrder returned error: %v", err)
	}
	if stored.Status != core.PaymentOrderPending || stored.ProviderTradeNo != "" {
		t.Fatalf("stored order = %#v, want unchanged pending order", stored)
	}
}
