package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/payments"
	"github.com/32ns/ai-gateway/internal/storage"
)

var (
	ErrPaymentAmountBelowMinimum = errors.New("payment amount is below minimum recharge")
	ErrPaymentAmountAboveMaximum = errors.New("payment amount is above maximum recharge")
)

type PaymentOrderInput struct {
	UserID               string
	Provider             core.PaymentProvider
	Channel              core.PaymentChannel
	AmountNanoUSD        int64
	PaymentAmountNanoCNY int64
	Subject              string
	ClientIP             string
	OpenID               string
	NotifyBaseURL        string
}

type PaymentOrderResult struct {
	Order   core.PaymentOrder
	Payload map[string]string
}

type ManualPaymentConfirmInput struct {
	OrderID         string
	ProviderTradeNo string
}

type PaymentOrderFilter struct {
	UserID         string
	Provider       core.PaymentProvider
	Status         core.PaymentOrderStatus
	ExcludePending bool
	Page           int
	PageSize       int
}

type PaymentOrderPage struct {
	Orders   []core.PaymentOrder
	Filter   PaymentOrderFilter
	Total    int
	Page     int
	PageSize int
	HasPrev  bool
	PrevPage int
	HasNext  bool
	NextPage int
}

func (s *Service) CreatePaymentOrder(ctx context.Context, input PaymentOrderInput) (PaymentOrderResult, error) {
	userID := strings.TrimSpace(input.UserID)
	if userID == "" {
		return PaymentOrderResult{}, fmt.Errorf("user is required")
	}
	if _, err := s.repo.GetUser(userID); err != nil {
		return PaymentOrderResult{}, err
	}
	provider := core.PaymentProvider(strings.TrimSpace(string(input.Provider)))
	channel := normalizePaymentChannel(provider, input.Channel)
	if !validPaymentProvider(provider) {
		return PaymentOrderResult{}, fmt.Errorf("unsupported payment provider %q", provider)
	}
	if !validPaymentChannel(provider, channel) {
		return PaymentOrderResult{}, fmt.Errorf("unsupported payment channel %q for provider %q", channel, provider)
	}
	if provider == core.PaymentProviderWeChatPay && channel == core.PaymentChannelJSAPI && strings.TrimSpace(input.OpenID) == "" {
		return PaymentOrderResult{}, fmt.Errorf("openid is required for wechat jsapi payments")
	}
	settings := s.currentSystemSettings()
	paymentSettings, err := paymentSettingsForProvider(settings, provider, input.NotifyBaseURL)
	if err != nil {
		return PaymentOrderResult{}, err
	}
	amountNanoUSD, providerAmountCents, err := paymentOrderAmounts(input, paymentSettings.CNYPerUSD)
	if err != nil {
		return PaymentOrderResult{}, err
	}
	if err := validatePaymentRechargeAmount(amountNanoUSD, settings.Payment); err != nil {
		return PaymentOrderResult{}, err
	}
	now := time.Now().UTC()
	order := core.PaymentOrder{
		ID:                    fmt.Sprintf("pay_%d", now.UnixNano()),
		OutTradeNo:            fmt.Sprintf("pay_%d", now.UnixNano()),
		UserID:                userID,
		Provider:              provider,
		Channel:               channel,
		AmountNanoUSD:         amountNanoUSD,
		Currency:              "USD",
		ProviderAmountCents:   providerAmountCents,
		ProviderCurrency:      "CNY",
		ExchangeRateCNYPerUSD: strings.TrimSpace(paymentSettings.CNYPerUSD),
		Subject:               strings.TrimSpace(input.Subject),
		Status:                core.PaymentOrderPending,
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	if order.Subject == "" {
		order.Subject = "AI Gateway recharge"
	}
	result, err := s.payments.CreateOrder(ctx, payments.CreateOrderInput{
		Order:       order,
		Settings:    paymentSettings,
		Description: order.Subject,
		ClientIP:    input.ClientIP,
		OpenID:      input.OpenID,
	})
	if err != nil {
		return PaymentOrderResult{}, err
	}
	result.Order = finalizePaymentOrderResult(result.Order, order)
	if err := s.repo.CreatePaymentOrder(result.Order); err != nil {
		return PaymentOrderResult{}, err
	}
	return PaymentOrderResult{Order: result.Order, Payload: result.Payload}, nil
}

func finalizePaymentOrderResult(result, fallback core.PaymentOrder) core.PaymentOrder {
	result.ID = fallback.ID
	result.OutTradeNo = fallback.OutTradeNo
	result.UserID = fallback.UserID
	result.Provider = fallback.Provider
	result.Channel = fallback.Channel
	result.AmountNanoUSD = fallback.AmountNanoUSD
	result.Currency = fallback.Currency
	result.ProviderAmountCents = fallback.ProviderAmountCents
	result.ProviderCurrency = fallback.ProviderCurrency
	result.ExchangeRateCNYPerUSD = fallback.ExchangeRateCNYPerUSD
	result.Subject = fallback.Subject
	result.Status = fallback.Status
	result.CreatedAt = fallback.CreatedAt
	result.UpdatedAt = fallback.UpdatedAt
	return result
}

func paymentOrderAmounts(input PaymentOrderInput, cnyPerUSD string) (int64, int64, error) {
	hasCreditAmount := input.AmountNanoUSD > 0
	hasPaymentAmount := input.PaymentAmountNanoCNY > 0
	if hasCreditAmount && hasPaymentAmount {
		return 0, 0, fmt.Errorf("payment amount is ambiguous")
	}
	if hasPaymentAmount {
		providerAmountCents, err := cnyCentsForPaymentAmount(input.PaymentAmountNanoCNY)
		if err != nil {
			return 0, 0, err
		}
		amountNanoUSD, err := creditAmountForCNYCents(providerAmountCents, cnyPerUSD)
		if err != nil {
			return 0, 0, err
		}
		return amountNanoUSD, providerAmountCents, nil
	}
	if !hasCreditAmount {
		return 0, 0, fmt.Errorf("amount must be greater than zero")
	}
	if input.AmountNanoUSD%(core.NanoUSDPerUSD/100) != 0 {
		return 0, 0, fmt.Errorf("payment amount must be accurate to cents")
	}
	providerAmountCents, err := cnyCentsForCreditAmount(input.AmountNanoUSD, cnyPerUSD)
	if err != nil {
		return 0, 0, err
	}
	return input.AmountNanoUSD, providerAmountCents, nil
}

func validatePaymentRechargeAmount(amountNanoUSD int64, settings core.SystemPaymentSettings) error {
	if settings.MinRechargeNanoUSD > 0 && amountNanoUSD < settings.MinRechargeNanoUSD {
		return ErrPaymentAmountBelowMinimum
	}
	if settings.MaxRechargeNanoUSD > 0 && amountNanoUSD > settings.MaxRechargeNanoUSD {
		return ErrPaymentAmountAboveMaximum
	}
	return nil
}

func (s *Service) PaymentOrderPage(user core.User, filter PaymentOrderFilter) PaymentOrderPage {
	if filter.Page < 1 {
		filter.Page = 1
	}
	if filter.PageSize <= 0 {
		filter.PageSize = 25
	}
	if filter.PageSize > 100 {
		filter.PageSize = 100
	}
	userID := strings.TrimSpace(filter.UserID)
	if !user.IsAdmin() {
		filter.UserID = user.ID
		userID = user.ID
	} else {
		userID = s.resolveUsageLogUserFilter(userID)
	}
	filter.UserID = strings.TrimSpace(filter.UserID)
	filter.Provider = core.PaymentProvider(strings.TrimSpace(string(filter.Provider)))
	filter.Status = core.PaymentOrderStatus(strings.TrimSpace(string(filter.Status)))
	start := (filter.Page - 1) * filter.PageSize
	orders, total := s.repo.ListPaymentOrdersPage(storage.PaymentOrderQuery{
		UserID:         userID,
		Provider:       filter.Provider,
		Status:         filter.Status,
		ExcludePending: filter.ExcludePending,
		Offset:         start,
		Limit:          filter.PageSize,
	})
	page := PaymentOrderPage{
		Orders:   orders,
		Filter:   filter,
		Total:    total,
		Page:     filter.Page,
		PageSize: filter.PageSize,
	}
	if filter.Page > 1 && total > 0 {
		page.HasPrev = true
		page.PrevPage = filter.Page - 1
	}
	if start+len(orders) < total {
		page.HasNext = true
		page.NextPage = filter.Page + 1
	}
	return page
}

func (s *Service) GetPaymentOrderForUser(user core.User, id string) (core.PaymentOrder, error) {
	order, err := s.repo.GetPaymentOrder(strings.TrimSpace(id))
	if err != nil {
		return core.PaymentOrder{}, err
	}
	if !user.IsAdmin() && order.UserID != user.ID {
		return core.PaymentOrder{}, storage.ErrNotFound
	}
	return order, nil
}

func (s *Service) RefreshPaymentOrder(ctx context.Context, user core.User, id string) (core.PaymentOrder, bool, error) {
	order, err := s.GetPaymentOrderForUser(user, id)
	if err != nil {
		return core.PaymentOrder{}, false, err
	}
	if order.Status != core.PaymentOrderPending {
		return order, false, nil
	}
	settings := s.currentSystemSettings()
	result, err := s.payments.QueryOrder(ctx, payments.QueryOrderInput{Order: order, Settings: settings.Payment})
	if err != nil {
		return core.PaymentOrder{}, false, err
	}
	event := payments.Event(result)
	event.Provider = order.Provider
	event.OutTradeNo = order.OutTradeNo
	updated, credited, err := s.applyPaymentEventForOrder(order, event)
	if err != nil {
		return core.PaymentOrder{}, false, err
	}
	return updated, credited, nil
}

func (s *Service) CancelPaymentOrder(ctx context.Context, user core.User, id string) (core.PaymentOrder, bool, bool, error) {
	order, err := s.GetPaymentOrderForUser(user, id)
	if err != nil {
		return core.PaymentOrder{}, false, false, err
	}
	if order.Status != core.PaymentOrderPending {
		return order, false, false, nil
	}
	settings := s.currentSystemSettings()
	if result, err := s.payments.QueryOrder(ctx, payments.QueryOrderInput{Order: order, Settings: settings.Payment}); err == nil {
		event := payments.Event(result)
		event.Provider = order.Provider
		event.OutTradeNo = order.OutTradeNo
		updated, credited, applyErr := s.applyPaymentEventForOrder(order, event)
		if applyErr == nil && updated.Status != core.PaymentOrderPending {
			return updated, false, credited, nil
		}
		if applyErr == nil {
			order = updated
		}
	}
	if order.Provider == core.PaymentProviderPersonalPay && personalPayOrderExpired(order, settings.Payment.PersonalPay) {
		result, err := s.payments.CancelOrder(ctx, payments.CancelOrderInput{Order: order, Settings: settings.Payment})
		if err != nil {
			return order, false, false, nil
		}
		event := payments.Event(result)
		event.Provider = order.Provider
		event.OutTradeNo = order.OutTradeNo
		updated, credited, err := s.applyPaymentEventForOrder(order, event)
		return updated, false, credited, err
	}
	return order, false, false, nil
}

func personalPayOrderExpired(order core.PaymentOrder, settings core.PersonalPaySettings) bool {
	if order.CreatedAt.IsZero() {
		return false
	}
	expireAfter := personalPayOrderExpireAfterSec(order, settings)
	return !order.CreatedAt.Add(time.Duration(expireAfter) * time.Second).After(time.Now().UTC())
}

func personalPayOrderExpireAfterSec(order core.PaymentOrder, settings core.PersonalPaySettings) int {
	var request struct {
		ExpireAfterSec int64 `json:"expireAfterSec"`
	}
	if strings.TrimSpace(order.RawRequest) != "" && json.Unmarshal([]byte(order.RawRequest), &request) == nil && request.ExpireAfterSec > 0 {
		return int(request.ExpireAfterSec)
	}
	expireAfter := settings.ExpireAfterSec
	if expireAfter <= 0 {
		expireAfter = core.DefaultPersonalPayExpireAfterSec
	}
	return expireAfter
}

func (s *Service) ApplyPaymentEvent(event payments.Event) (core.PaymentOrder, bool, error) {
	if event.Provider == "" || strings.TrimSpace(event.OutTradeNo) == "" {
		return core.PaymentOrder{}, false, fmt.Errorf("payment event is incomplete")
	}
	existing, err := s.repo.GetPaymentOrderByOutTradeNo(event.OutTradeNo)
	if err != nil {
		return core.PaymentOrder{}, false, err
	}
	return s.applyPaymentEventForOrder(existing, event)
}

func (s *Service) ConfirmPaymentOrderPaidManually(input ManualPaymentConfirmInput) (core.PaymentOrder, bool, error) {
	orderID := strings.TrimSpace(input.OrderID)
	if orderID == "" {
		return core.PaymentOrder{}, false, fmt.Errorf("payment order is required")
	}
	order, err := s.repo.GetPaymentOrder(orderID)
	if err != nil {
		return core.PaymentOrder{}, false, err
	}
	if order.Status == core.PaymentOrderPaid {
		return order, false, nil
	}
	if order.Status != core.PaymentOrderClosed && order.Status != core.PaymentOrderFailed {
		return core.PaymentOrder{}, false, fmt.Errorf("payment order status cannot be manually confirmed")
	}
	providerTradeNo := strings.TrimSpace(input.ProviderTradeNo)
	if providerTradeNo == "" {
		providerTradeNo = strings.TrimSpace(order.ProviderTradeNo)
	}
	if providerTradeNo == "" {
		providerTradeNo = "manual_" + storageIDPart(order.ID)
	}
	return s.applyPaymentEventForOrder(order, payments.Event{
		Provider:            order.Provider,
		OutTradeNo:          order.OutTradeNo,
		Status:              core.PaymentOrderPaid,
		ProviderStatus:      "manual_confirmed",
		ProviderTradeNo:     providerTradeNo,
		AmountNanoUSD:       order.AmountNanoUSD,
		ProviderAmountCents: order.ProviderAmountCents,
		PaidAt:              time.Now().UTC(),
		RawResponse:         "manual admin confirmation",
	})
}

func (s *Service) PersonalPayRuntime(ctx context.Context) payments.PersonalPayRuntime {
	if s == nil || s.payments == nil {
		return payments.PersonalPayRuntime{}
	}
	return s.payments.PersonalPayRuntime(ctx)
}

func (s *Service) DeletePersonalPayDevice(ctx context.Context, deviceID string) error {
	if s == nil || s.payments == nil {
		return fmt.Errorf("personalpay runtime is not configured")
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return fmt.Errorf("personalpay device id is required")
	}
	return s.payments.DeletePersonalPayDevice(ctx, deviceID)
}

func (s *Service) ReleasePersonalPayAccountOrder(ctx context.Context, accountID string) (core.PaymentOrder, error) {
	if s == nil || s.payments == nil {
		return core.PaymentOrder{}, fmt.Errorf("personalpay runtime is not configured")
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return core.PaymentOrder{}, fmt.Errorf("personalpay account id is required")
	}
	runtime := s.payments.PersonalPayRuntime(ctx)
	var occupiedBy string
	for _, account := range runtime.Accounts {
		if account.ID == accountID {
			occupiedBy = strings.TrimSpace(account.OccupiedBy)
			break
		}
	}
	if occupiedBy == "" {
		for _, device := range runtime.Devices {
			for _, account := range device.Accounts {
				if account.ID == accountID {
					occupiedBy = strings.TrimSpace(account.OccupiedBy)
					break
				}
			}
			if occupiedBy != "" {
				break
			}
		}
	}
	if occupiedBy == "" {
		return core.PaymentOrder{}, fmt.Errorf("personalpay account is not occupied")
	}
	order, err := s.repo.GetPaymentOrderByOutTradeNo(occupiedBy)
	if err != nil {
		if byID, byIDErr := s.repo.GetPaymentOrder(occupiedBy); byIDErr == nil {
			order = byID
		} else {
			return core.PaymentOrder{}, err
		}
	}
	if order.Provider != core.PaymentProviderPersonalPay {
		return core.PaymentOrder{}, fmt.Errorf("occupied order is not a personalpay order")
	}
	if order.Status != core.PaymentOrderPending {
		return order, nil
	}
	settings := s.currentSystemSettings()
	result, err := s.payments.CancelOrder(ctx, payments.CancelOrderInput{Order: order, Settings: settings.Payment})
	if err != nil {
		return core.PaymentOrder{}, err
	}
	event := payments.Event(result)
	event.Provider = order.Provider
	event.OutTradeNo = order.OutTradeNo
	updated, _, err := s.applyPaymentEventForOrder(order, event)
	return updated, err
}

func (s *Service) CompletePaymentOrder(notification payments.Notification) (core.PaymentOrder, bool, error) {
	if notification.Status == "" && notification.AmountNanoUSD > 0 {
		notification.Status = core.PaymentOrderPaid
	}
	return s.ApplyPaymentEvent(notification)
}

func (s *Service) applyPaymentEventForOrder(existing core.PaymentOrder, event payments.Event) (core.PaymentOrder, bool, error) {
	if event.Provider == "" || strings.TrimSpace(event.OutTradeNo) == "" {
		return core.PaymentOrder{}, false, fmt.Errorf("payment event is incomplete")
	}
	if existing.Provider != event.Provider {
		return core.PaymentOrder{}, false, fmt.Errorf("payment provider does not match order")
	}
	update := core.PaymentOrderProviderUpdate{
		Status:          event.Status,
		ProviderStatus:  event.ProviderStatus,
		ProviderTradeNo: event.ProviderTradeNo,
		CodeURL:         event.CodeURL,
		PayURL:          event.PayURL,
		PrepayID:        event.PrepayID,
		RawResponse:     firstNonEmptyString(event.RawResponse, event.RawBody),
	}
	switch event.Status {
	case core.PaymentOrderPaid:
		if strings.TrimSpace(event.ProviderTradeNo) == "" {
			return core.PaymentOrder{}, false, fmt.Errorf("payment event is incomplete")
		}
		if event.ProviderAmountCents > 0 && existing.ProviderAmountCents > 0 && event.ProviderAmountCents != existing.ProviderAmountCents {
			return core.PaymentOrder{}, false, fmt.Errorf("payment amount or status does not match order")
		}
		paidAmount := event.AmountNanoUSD
		if event.ProviderAmountCents > 0 {
			paidAmount = existing.AmountNanoUSD
		}
		if paidAmount <= 0 {
			return core.PaymentOrder{}, false, fmt.Errorf("payment event is incomplete")
		}
		var credits []core.PaymentOrderBalanceCredit
		if existing.Status != core.PaymentOrderPaid {
			var err error
			credits, err = s.invitationRechargeRewardCredits(existing, paidAmount)
			if err != nil {
				return core.PaymentOrder{}, false, err
			}
		}
		order, credited, err := s.repo.CompletePaymentOrderWithCredits(existing.OutTradeNo, event.ProviderTradeNo, paidAmount, event.PaidAt, credits)
		if err != nil {
			if err == storage.ErrBillingRequestConflict {
				return core.PaymentOrder{}, false, fmt.Errorf("payment amount or status does not match order")
			}
			return core.PaymentOrder{}, false, err
		}
		return order, credited, nil
	case core.PaymentOrderClosed, core.PaymentOrderFailed:
		updated, err := s.repo.UpdatePaymentOrderProviderState(existing.ID, update)
		return updated, false, err
	default:
		update.Status = core.PaymentOrderPending
		updated, err := s.repo.UpdatePaymentOrderProviderState(existing.ID, update)
		return updated, false, err
	}
}

func (s *Service) invitationRechargeRewardCredits(order core.PaymentOrder, amountNanoUSD int64) ([]core.PaymentOrderBalanceCredit, error) {
	settings := s.currentSystemSettings()
	if !settings.Invitation.Enabled || settings.Invitation.InviterRechargeRewardBps <= 0 || amountNanoUSD <= 0 {
		return nil, nil
	}
	invitee, err := s.repo.GetUser(order.UserID)
	if err != nil {
		return nil, err
	}
	inviterID := strings.TrimSpace(invitee.InviterUserID)
	if inviterID == "" || inviterID == invitee.ID {
		return nil, nil
	}
	outTradeNo := strings.TrimSpace(order.OutTradeNo)
	if outTradeNo == "" {
		return nil, nil
	}
	inviter, err := s.repo.GetUser(inviterID)
	if err != nil {
		if err == storage.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	if !inviter.Enabled {
		return nil, nil
	}
	reward := invitationRechargeRewardAmount(amountNanoUSD, settings.Invitation.InviterRechargeRewardBps)
	if reward <= 0 {
		return nil, nil
	}
	return []core.PaymentOrderBalanceCredit{{
		UserID:        inviter.ID,
		Kind:          "manual_credit",
		LedgerID:      invitationRechargeRewardLedgerID(outTradeNo, inviter.ID),
		AmountNanoUSD: reward,
		Note:          invitationRechargeRewardNote(invitee.Username, outTradeNo, settings.Invitation.InviterRechargeRewardBps),
	}}, nil
}

func invitationRechargeRewardAmount(amountNanoUSD, rewardBps int64) int64 {
	if amountNanoUSD <= 0 || rewardBps <= 0 {
		return 0
	}
	if rewardBps > 10000 {
		rewardBps = 10000
	}
	numerator := new(big.Int).Mul(big.NewInt(amountNanoUSD), big.NewInt(rewardBps))
	quotient, remainder := new(big.Int).QuoRem(numerator, big.NewInt(10000), new(big.Int))
	remainder.Mul(remainder, big.NewInt(2))
	if remainder.Cmp(big.NewInt(10000)) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return 0
	}
	return quotient.Int64()
}

func invitationRechargeRewardNote(inviteeUsername, outTradeNo string, rewardBps int64) string {
	inviteeUsername = strings.TrimSpace(inviteeUsername)
	outTradeNo = strings.TrimSpace(outTradeNo)
	percent := formatRewardPercentBps(rewardBps)
	if inviteeUsername == "" {
		return fmt.Sprintf("invite recharge reward: %s%% payment %s", percent, outTradeNo)
	}
	return fmt.Sprintf("invite recharge reward: %s%% %s payment %s", percent, inviteeUsername, outTradeNo)
}

func invitationRechargeRewardLedgerID(outTradeNo, inviterID string) string {
	return fmt.Sprintf("ledger_invite_recharge_%s_%s", storageIDPart(outTradeNo), storageIDPart(inviterID))
}

func storageIDPart(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "empty"
	}
	replacer := strings.NewReplacer(" ", "_", "\t", "_", "\r", "_", "\n", "_", "/", "_", "\\", "_", ":", "_", "\x00", "_")
	return replacer.Replace(value)
}

func formatRewardPercentBps(bps int64) string {
	if bps < 0 {
		bps = 0
	}
	whole := bps / 100
	fraction := bps % 100
	if fraction == 0 {
		return fmt.Sprintf("%d", whole)
	}
	return fmt.Sprintf("%d.%s", whole, strings.TrimRight(fmt.Sprintf("%02d", fraction), "0"))
}

func normalizePaymentChannel(provider core.PaymentProvider, channel core.PaymentChannel) core.PaymentChannel {
	channel = core.PaymentChannel(strings.TrimSpace(string(channel)))
	if channel != "" {
		return channel
	}
	if provider == core.PaymentProviderPersonalPay {
		return core.PaymentChannelWeChat
	}
	if provider == core.PaymentProviderAlipay {
		return core.PaymentChannelPage
	}
	return core.PaymentChannelNative
}

func validPaymentProvider(provider core.PaymentProvider) bool {
	return provider == core.PaymentProviderWeChatPay || provider == core.PaymentProviderAlipay || provider == core.PaymentProviderPersonalPay
}

func validPaymentChannel(provider core.PaymentProvider, channel core.PaymentChannel) bool {
	switch provider {
	case core.PaymentProviderWeChatPay:
		return channel == core.PaymentChannelNative || channel == core.PaymentChannelJSAPI || channel == core.PaymentChannelWAP
	case core.PaymentProviderAlipay:
		return channel == core.PaymentChannelPage || channel == core.PaymentChannelWAP
	case core.PaymentProviderPersonalPay:
		return channel == core.PaymentChannelWeChat
	default:
		return false
	}
}

func paymentSettingsForProvider(settings core.SystemSettings, provider core.PaymentProvider, fallbackBaseURL string) (core.SystemPaymentSettings, error) {
	paymentSettings := settings.Payment
	switch provider {
	case core.PaymentProviderWeChatPay:
		if strings.TrimSpace(paymentSettings.WeChatPay.NotifyURL) == "" {
			notifyURL, err := defaultPaymentNotifyURL(firstNonEmptyString(settings.Runtime.PublicBaseURL, fallbackBaseURL), provider)
			if err != nil {
				return core.SystemPaymentSettings{}, fmt.Errorf("wechat pay notify url is required: configure WeChat Notify URL or Public Base URL")
			}
			paymentSettings.WeChatPay.NotifyURL = notifyURL
		}
	case core.PaymentProviderAlipay:
		if strings.TrimSpace(paymentSettings.Alipay.NotifyURL) == "" {
			notifyURL, err := defaultPaymentNotifyURL(firstNonEmptyString(settings.Runtime.PublicBaseURL, fallbackBaseURL), provider)
			if err != nil {
				return core.SystemPaymentSettings{}, fmt.Errorf("alipay notify url is required: configure Alipay Notify URL or Public Base URL")
			}
			paymentSettings.Alipay.NotifyURL = notifyURL
		}
		if strings.TrimSpace(paymentSettings.Alipay.ReturnURL) == "" {
			returnURL, err := defaultPaymentReturnURL(firstNonEmptyString(settings.Runtime.PublicBaseURL, fallbackBaseURL), provider)
			if err != nil {
				return core.SystemPaymentSettings{}, fmt.Errorf("alipay return url is required: configure Alipay Return URL or Public Base URL")
			}
			paymentSettings.Alipay.ReturnURL = returnURL
		}
	}
	return paymentSettings, nil
}

func cnyCentsForCreditAmount(amountNanoUSD int64, cnyPerUSD string) (int64, error) {
	if amountNanoUSD <= 0 {
		return 0, fmt.Errorf("amount must be greater than zero")
	}
	rateNano, err := core.ParseNanoUSDDecimal(strings.TrimSpace(cnyPerUSD))
	if err != nil || rateNano <= 0 {
		return 0, fmt.Errorf("cny per usd exchange rate is invalid")
	}
	numerator := big.NewInt(amountNanoUSD)
	numerator.Mul(numerator, big.NewInt(rateNano))
	numerator.Mul(numerator, big.NewInt(100))
	denominator := big.NewInt(core.NanoUSDPerUSD)
	denominator.Mul(denominator, big.NewInt(core.NanoUSDPerUSD))
	quotient, remainder := new(big.Int).QuoRem(numerator, denominator, new(big.Int))
	remainder.Mul(remainder, big.NewInt(2))
	if remainder.Cmp(denominator) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() || quotient.Int64() <= 0 {
		return 0, fmt.Errorf("payment amount is invalid")
	}
	return quotient.Int64(), nil
}

func cnyCentsForPaymentAmount(amountNanoCNY int64) (int64, error) {
	if amountNanoCNY <= 0 {
		return 0, fmt.Errorf("amount must be greater than zero")
	}
	if amountNanoCNY%(core.NanoUSDPerUSD/100) != 0 {
		return 0, fmt.Errorf("payment amount must be accurate to cents")
	}
	cents := amountNanoCNY / (core.NanoUSDPerUSD / 100)
	if cents <= 0 {
		return 0, fmt.Errorf("payment amount is invalid")
	}
	return cents, nil
}

func creditAmountForCNYCents(cnyCents int64, cnyPerUSD string) (int64, error) {
	if cnyCents <= 0 {
		return 0, fmt.Errorf("amount must be greater than zero")
	}
	rateNano, err := core.ParseNanoUSDDecimal(strings.TrimSpace(cnyPerUSD))
	if err != nil || rateNano <= 0 {
		return 0, fmt.Errorf("cny per usd exchange rate is invalid")
	}
	numerator := big.NewInt(cnyCents)
	numerator.Mul(numerator, big.NewInt(core.NanoUSDPerUSD))
	denominator := big.NewInt(rateNano)
	quotient, remainder := new(big.Int).QuoRem(numerator, denominator, new(big.Int))
	remainder.Mul(remainder, big.NewInt(2))
	if remainder.Cmp(denominator) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() || quotient.Int64() <= 0 {
		return 0, fmt.Errorf("payment amount is invalid")
	}
	usdCents := quotient.Int64()
	if usdCents > (1<<63-1)/(core.NanoUSDPerUSD/100) {
		return 0, fmt.Errorf("payment amount is invalid")
	}
	return usdCents * (core.NanoUSDPerUSD / 100), nil
}

func defaultPaymentNotifyURL(publicBaseURL string, provider core.PaymentProvider) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(publicBaseURL), "/")
	if base == "" {
		return "", fmt.Errorf("public base url is empty")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("public base url is invalid")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("public base url scheme must be http or https")
	}
	return base + "/payments/notify/" + string(provider), nil
}

func defaultPaymentReturnURL(publicBaseURL string, provider core.PaymentProvider) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(publicBaseURL), "/")
	if base == "" {
		return "", fmt.Errorf("public base url is empty")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("public base url is invalid")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("public base url scheme must be http or https")
	}
	return base + "/payments/return/" + string(provider), nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
