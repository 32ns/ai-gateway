package payments

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	personalpay "personalpay/sdk-go"
)

type personalPayEngine interface {
	CreateOrder(context.Context, personalpay.CreateOrderRequest) (personalpay.Order, error)
	GetOrder(context.Context, string) (personalpay.Order, error)
	CancelOrder(context.Context, string) (personalpay.Order, error)
	ListAccounts(context.Context) []personalpay.Account
	ListDevices(context.Context) []personalpay.Device
	DeleteDevice(context.Context, string) error
}

var (
	personalPayQRCodeWaitTimeout = 12 * time.Second
	personalPayQRCodePollEvery   = 300 * time.Millisecond
)

func (c *Client) SetPersonalPayEngine(engine personalPayEngine) *Client {
	c.personalPay = engine
	return c
}

func (c *Client) PersonalPayRuntime(ctx context.Context) PersonalPayRuntime {
	if c.personalPay == nil {
		return PersonalPayRuntime{}
	}
	accounts := c.personalPay.ListAccounts(ctx)
	devices := c.personalPay.ListDevices(ctx)
	return PersonalPayRuntime{
		Devices:  devices,
		Accounts: accounts,
		Summary:  summarizePersonalPayRuntime(devices, accounts),
	}
}

func (c *Client) DeletePersonalPayDevice(ctx context.Context, deviceID string) error {
	if c.personalPay == nil {
		return errors.New("personalpay sdk is not configured")
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return errors.New("personalpay device id is required")
	}
	return c.personalPay.DeleteDevice(ctx, deviceID)
}

func (c *Client) createPersonalPayOrder(ctx context.Context, input CreateOrderInput) (CreateOrderResult, error) {
	settings := input.Settings.PersonalPay
	if !settings.Enabled {
		return CreateOrderResult{}, errors.New("personalpay is disabled")
	}
	if strings.TrimSpace(settings.AndroidToken) == "" {
		return CreateOrderResult{}, errors.New("personalpay android token is required")
	}
	if c.personalPay == nil {
		return CreateOrderResult{}, errors.New("personalpay sdk is not configured")
	}
	amountCents := providerOrderCents(input.Order)
	if amountCents <= 0 {
		return CreateOrderResult{}, errors.New("personalpay amount is invalid")
	}

	request := personalpay.CreateOrderRequest{
		MerchantOrderID: input.Order.OutTradeNo,
		Channel:         personalPayChannel(input.Order.Channel),
		AmountCents:     amountCents,
		Memo:            input.Order.OutTradeNo,
		Mode:            personalpay.QRModeTemporary,
		ExpireAfterSec:  int64(settings.ExpireAfterSec),
		Metadata: map[string]string{
			"aiGatewayOrderId": input.Order.ID,
			"userId":           input.Order.UserID,
		},
	}
	orderPayload, err := c.personalPay.CreateOrder(ctx, request)
	if err != nil {
		return CreateOrderResult{}, err
	}
	orderPayload = c.waitPersonalPayQRCode(ctx, request.MerchantOrderID, orderPayload)

	requestBytes, _ := json.Marshal(request)
	responseBytes, _ := json.Marshal(orderPayload)
	order := input.Order
	order.RawRequest = string(requestBytes)
	order.RawResponse = string(responseBytes)
	order.ProviderStatus = personalPayProviderStatus(orderPayload)
	order.ProviderTradeNo = firstNonEmpty(orderPayload.ProviderTradeKey, orderPayload.ID)
	order.CodeURL = orderPayload.QRURL
	order.ProviderAmountCents = amountCents
	order.ProviderCurrency = "CNY"
	return CreateOrderResult{
		Order: order,
		Payload: map[string]string{
			"provider_status": order.ProviderStatus,
			"code_url":        order.CodeURL,
		},
	}, nil
}

func (c *Client) waitPersonalPayQRCode(ctx context.Context, orderID string, current personalpay.Order) personalpay.Order {
	if c == nil || c.personalPay == nil || strings.TrimSpace(current.QRURL) != "" {
		return current
	}
	timeout := personalPayQRCodeWaitTimeout
	if timeout <= 0 {
		return current
	}
	interval := personalPayQRCodePollEvery
	if interval <= 0 {
		interval = 300 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return current
		case <-timer.C:
			latest, err := c.personalPay.GetOrder(ctx, orderID)
			if err == nil {
				current = latest
				if strings.TrimSpace(latest.QRURL) != "" || isPersonalPayTerminal(latest.Status) {
					return latest
				}
			}
			timer.Reset(interval)
		}
	}
}

func isPersonalPayTerminal(status personalpay.OrderStatus) bool {
	return status == personalpay.OrderPaid ||
		status == personalpay.OrderCanceled ||
		status == personalpay.OrderExpired ||
		status == personalpay.OrderFailed ||
		status == personalpay.OrderLatePaid
}

func (c *Client) queryPersonalPayOrder(ctx context.Context, input QueryOrderInput) (QueryOrderResult, error) {
	settings := input.Settings.PersonalPay
	if !settings.Enabled {
		return QueryOrderResult{}, errors.New("personalpay is disabled")
	}
	if c.personalPay == nil {
		return QueryOrderResult{}, errors.New("personalpay sdk is not configured")
	}
	orderPayload, err := c.personalPay.GetOrder(ctx, input.Order.OutTradeNo)
	if err != nil {
		return QueryOrderResult{}, err
	}
	return PersonalPayOrderEvent(orderPayload, marshalJSON(orderPayload)), nil
}

func (c *Client) cancelPersonalPayOrder(ctx context.Context, input CancelOrderInput) (CancelOrderResult, error) {
	settings := input.Settings.PersonalPay
	if !settings.Enabled {
		return CancelOrderResult{}, errors.New("personalpay is disabled")
	}
	if c.personalPay == nil {
		return CancelOrderResult{}, errors.New("personalpay sdk is not configured")
	}
	orderPayload, err := c.personalPay.CancelOrder(ctx, input.Order.OutTradeNo)
	if err != nil {
		return CancelOrderResult{}, err
	}
	return PersonalPayOrderEvent(orderPayload, marshalJSON(orderPayload)), nil
}

func PersonalPayNotificationEvent(notification personalpay.OrderNotification) Event {
	event := PersonalPayOrderEvent(notification.Order, marshalJSON(notification))
	if event.ProviderStatus == "" {
		event.ProviderStatus = firstNonEmpty(string(notification.Event.Status), notification.Event.Type)
	}
	event.RawBody = event.RawResponse
	return event
}

func PersonalPayOrderEvent(order personalpay.Order, raw string) QueryOrderResult {
	status := core.PaymentOrderPending
	switch order.Status {
	case personalpay.OrderPaid, personalpay.OrderLatePaid:
		status = core.PaymentOrderPaid
	case personalpay.OrderCanceled, personalpay.OrderExpired:
		status = core.PaymentOrderClosed
	case personalpay.OrderFailed:
		status = core.PaymentOrderFailed
	}
	paidAt := time.Time{}
	if order.PaidAt != nil {
		paidAt = order.PaidAt.UTC()
	}
	return QueryOrderResult{
		Provider:            core.PaymentProviderPersonalPay,
		OutTradeNo:          firstNonEmpty(order.MerchantOrderID, order.ID),
		ProviderTradeNo:     firstNonEmpty(order.ProviderTradeKey, order.ID),
		Status:              status,
		ProviderStatus:      personalPayProviderStatus(order),
		ProviderAmountCents: order.AmountCents,
		ProviderCurrency:    "CNY",
		CodeURL:             order.QRURL,
		PaidAt:              paidAt,
		RawResponse:         raw,
	}
}

func personalPayProviderStatus(order personalpay.Order) string {
	return firstNonEmpty(string(order.PaymentStatus), string(order.Status))
}

func personalPayChannel(channel core.PaymentChannel) personalpay.PaymentChannel {
	switch channel {
	case core.PaymentChannelAlipay:
		return personalpay.ChannelAlipay
	default:
		return personalpay.ChannelWeChat
	}
}

func marshalJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

type PersonalPayRuntime struct {
	Devices  []personalpay.Device
	Accounts []personalpay.Account
	Summary  PersonalPayRuntimeSummary
}

type PersonalPayRuntimeSummary struct {
	DeviceCount        int
	AccountCount       int
	WeChatAccountCount int
	AlipayAccountCount int
	IdleAccountCount   int
	OccupiedCount      int
	OfflineCount       int
}

func summarizePersonalPayRuntime(devices []personalpay.Device, accounts []personalpay.Account) PersonalPayRuntimeSummary {
	var summary PersonalPayRuntimeSummary
	for _, device := range devices {
		if device.Online {
			summary.DeviceCount++
		}
	}
	for _, account := range accounts {
		if account.Status == personalpay.AccountOffline {
			summary.OfflineCount++
			continue
		}
		summary.AccountCount++
		switch account.Channel {
		case personalpay.ChannelWeChat:
			summary.WeChatAccountCount++
		case personalpay.ChannelAlipay:
			summary.AlipayAccountCount++
		}
		switch account.Status {
		case personalpay.AccountIdle:
			summary.IdleAccountCount++
		case personalpay.AccountOccupied:
			summary.OccupiedCount++
		}
	}
	return summary
}
