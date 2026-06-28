package controlplane

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"
)

var (
	ErrPaymentRefundOrderNotPaid        = errors.New("payment order is not paid")
	ErrPaymentRefundAmountExceedsRemain = errors.New("payment refund amount exceeds remaining refundable amount")
	ErrPaymentRefundAmountUnavailable   = errors.New("payment refund amount exceeds available refundable balance")
)

type ManualPaymentRefundInput struct {
	OrderID       string
	AmountNanoUSD int64
	Reason        string
}

type ManualPaymentRefundCompleteInput struct {
	RefundID  string
	PayoutRef string
	Note      string
}

type PaymentOrderRefundSummary struct {
	Refunds                      []core.PaymentRefund
	RemainingNanoUSD             int64
	RemainingProviderAmountCents int64
	UserBalanceNanoUSD           int64
	PendingUserRefundNanoUSD     int64
	AvailableRefundNanoUSD       int64
	AvailableProviderAmountCents int64
	RefundedNanoUSD              int64
	RefundedProviderAmountCents  int64
	PendingNanoUSD               int64
	PendingProviderAmountCents   int64
	CompletedNanoUSD             int64
	CompletedProviderAmountCents int64
}

func (s *Service) PaymentOrderRefundSummaries(orders []core.PaymentOrder) map[string]PaymentOrderRefundSummary {
	out := make(map[string]PaymentOrderRefundSummary, len(orders))
	for _, order := range orders {
		orderID := strings.TrimSpace(order.ID)
		if orderID == "" {
			continue
		}
		out[orderID] = s.paymentOrderRefundSummary(order)
	}
	return out
}

func (s *Service) CreateManualPaymentRefund(input ManualPaymentRefundInput) (core.PaymentRefund, PaymentOrderRefundSummary, error) {
	orderID := strings.TrimSpace(input.OrderID)
	if orderID == "" {
		return core.PaymentRefund{}, PaymentOrderRefundSummary{}, fmt.Errorf("payment order is required")
	}
	order, err := s.repo.GetPaymentOrder(orderID)
	if err != nil {
		return core.PaymentRefund{}, PaymentOrderRefundSummary{}, err
	}
	if order.Status != core.PaymentOrderPaid {
		return core.PaymentRefund{}, PaymentOrderRefundSummary{}, ErrPaymentRefundOrderNotPaid
	}
	summary := s.paymentOrderRefundSummary(order)
	amount := input.AmountNanoUSD
	if amount <= 0 {
		amount = summary.AvailableRefundNanoUSD
	}
	if amount <= 0 {
		return core.PaymentRefund{}, summary, fmt.Errorf("refund amount must be greater than zero")
	}
	if amount > summary.RemainingNanoUSD {
		return core.PaymentRefund{}, summary, ErrPaymentRefundAmountExceedsRemain
	}
	if amount > summary.AvailableRefundNanoUSD {
		return core.PaymentRefund{}, summary, ErrPaymentRefundAmountUnavailable
	}

	now := time.Now().UTC()
	refund := core.PaymentRefund{
		ID:                    fmt.Sprintf("refund_%d", now.UnixNano()),
		OrderID:               order.ID,
		OutTradeNo:            order.OutTradeNo,
		UserID:                order.UserID,
		Provider:              order.Provider,
		AmountNanoUSD:         amount,
		ProviderAmountCents:   providerRefundAmountCents(order, summary, amount),
		ProviderCurrency:      paymentProviderCurrency(order),
		ExchangeRateCNYPerUSD: strings.TrimSpace(order.ExchangeRateCNYPerUSD),
		Status:                core.PaymentRefundPending,
		Reason:                strings.TrimSpace(input.Reason),
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	if err := s.repo.CreatePaymentRefund(refund); err != nil {
		if errors.Is(err, storage.ErrInsufficientBalance) {
			return core.PaymentRefund{}, summary, ErrPaymentRefundAmountUnavailable
		}
		return core.PaymentRefund{}, summary, err
	}
	return refund, s.paymentOrderRefundSummary(order), nil
}

func (s *Service) CompleteManualPaymentRefund(input ManualPaymentRefundCompleteInput) (core.PaymentRefund, bool, error) {
	refundID := strings.TrimSpace(input.RefundID)
	if refundID == "" {
		return core.PaymentRefund{}, false, fmt.Errorf("payment refund is required")
	}
	pending, err := s.findPaymentRefund(refundID)
	if err != nil {
		return core.PaymentRefund{}, false, err
	}
	if pending.Status == core.PaymentRefundPending {
		user, err := s.repo.GetUser(pending.UserID)
		if err != nil {
			return core.PaymentRefund{}, false, err
		}
		if user.BalanceNanoUSD < pending.AmountNanoUSD {
			return core.PaymentRefund{}, false, ErrPaymentRefundAmountUnavailable
		}
	}
	note := strings.TrimSpace(input.Note)
	if note == "" {
		note = "manual payout confirmed"
	}
	refund, debited, err := s.repo.CompletePaymentRefund(refundID, strings.TrimSpace(input.PayoutRef), note)
	return refund, debited, err
}

func (s *Service) FailManualPaymentRefund(refundID, note string) (core.PaymentRefund, error) {
	refundID = strings.TrimSpace(refundID)
	if refundID == "" {
		return core.PaymentRefund{}, fmt.Errorf("payment refund is required")
	}
	note = strings.TrimSpace(note)
	if note == "" {
		note = "manual payout cancelled"
	}
	return s.repo.FailPaymentRefund(refundID, note)
}

func (s *Service) paymentOrderRefundSummary(order core.PaymentOrder) PaymentOrderRefundSummary {
	refunds := s.repo.ListPaymentRefunds(strings.TrimSpace(order.ID))
	user, _ := s.repo.GetUser(order.UserID)
	pendingUserRefunds := pendingPaymentRefundNanoUSDForUser(s.repo.ListPaymentRefunds(""), order.UserID)
	return paymentOrderRefundSummary(order, refunds, user.BalanceNanoUSD, pendingUserRefunds)
}

func paymentOrderRefundSummary(order core.PaymentOrder, refunds []core.PaymentRefund, userBalanceNanoUSD, pendingUserRefundNanoUSD int64) PaymentOrderRefundSummary {
	summary := PaymentOrderRefundSummary{
		Refunds:                  refunds,
		UserBalanceNanoUSD:       userBalanceNanoUSD,
		PendingUserRefundNanoUSD: pendingUserRefundNanoUSD,
	}
	for _, refund := range refunds {
		if refund.Status != core.PaymentRefundPending && refund.Status != core.PaymentRefundDone {
			continue
		}
		amount := refund.AmountNanoUSD
		providerAmount := refundProviderAmountCents(order, refund)
		summary.RefundedNanoUSD = addSignedNanoUSDSaturating(summary.RefundedNanoUSD, amount)
		summary.RefundedProviderAmountCents += providerAmount
		if refund.Status == core.PaymentRefundPending {
			summary.PendingNanoUSD = addSignedNanoUSDSaturating(summary.PendingNanoUSD, amount)
			summary.PendingProviderAmountCents += providerAmount
		} else {
			summary.CompletedNanoUSD = addSignedNanoUSDSaturating(summary.CompletedNanoUSD, amount)
			summary.CompletedProviderAmountCents += providerAmount
		}
	}
	summary.RemainingNanoUSD = order.AmountNanoUSD - summary.RefundedNanoUSD
	if summary.RemainingNanoUSD < 0 {
		summary.RemainingNanoUSD = 0
	}
	summary.RemainingProviderAmountCents = order.ProviderAmountCents - summary.RefundedProviderAmountCents
	if summary.RemainingProviderAmountCents < 0 {
		summary.RemainingProviderAmountCents = 0
	}
	availableBalance := userBalanceNanoUSD - pendingUserRefundNanoUSD
	if availableBalance < 0 {
		availableBalance = 0
	}
	summary.AvailableRefundNanoUSD = minInt64(summary.RemainingNanoUSD, availableBalance)
	summary.AvailableProviderAmountCents = providerRefundAmountCents(order, summary, summary.AvailableRefundNanoUSD)
	return summary
}

func pendingPaymentRefundNanoUSDForUser(refunds []core.PaymentRefund, userID string) int64 {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return 0
	}
	total := int64(0)
	for _, refund := range refunds {
		if refund.Status != core.PaymentRefundPending || strings.TrimSpace(refund.UserID) != userID {
			continue
		}
		total = addSignedNanoUSDSaturating(total, refund.AmountNanoUSD)
	}
	return total
}

func (s *Service) findPaymentRefund(refundID string) (core.PaymentRefund, error) {
	refundID = strings.TrimSpace(refundID)
	for _, refund := range s.repo.ListPaymentRefunds("") {
		if strings.TrimSpace(refund.ID) == refundID {
			return refund, nil
		}
	}
	return core.PaymentRefund{}, storage.ErrNotFound
}

func providerRefundAmountCents(order core.PaymentOrder, summary PaymentOrderRefundSummary, amountNanoUSD int64) int64 {
	if amountNanoUSD <= 0 || order.ProviderAmountCents <= 0 || order.AmountNanoUSD <= 0 {
		return 0
	}
	if amountNanoUSD >= summary.RemainingNanoUSD && summary.RemainingProviderAmountCents > 0 {
		return summary.RemainingProviderAmountCents
	}
	amount := proportionalProviderAmountCents(order, amountNanoUSD)
	if summary.RemainingProviderAmountCents > 0 && amount > summary.RemainingProviderAmountCents {
		return summary.RemainingProviderAmountCents
	}
	return amount
}

func refundProviderAmountCents(order core.PaymentOrder, refund core.PaymentRefund) int64 {
	if refund.ProviderAmountCents > 0 {
		return refund.ProviderAmountCents
	}
	return proportionalProviderAmountCents(order, refund.AmountNanoUSD)
}

func proportionalProviderAmountCents(order core.PaymentOrder, amountNanoUSD int64) int64 {
	if amountNanoUSD <= 0 || order.ProviderAmountCents <= 0 || order.AmountNanoUSD <= 0 {
		return 0
	}
	if amountNanoUSD >= order.AmountNanoUSD {
		return order.ProviderAmountCents
	}
	numerator := big.NewInt(amountNanoUSD)
	numerator.Mul(numerator, big.NewInt(order.ProviderAmountCents))
	value, ok := roundedBigIntQuotient(numerator, big.NewInt(order.AmountNanoUSD))
	if !ok || value < 0 {
		return 0
	}
	return value
}

func roundedBigIntQuotient(numerator, denominator *big.Int) (int64, bool) {
	if numerator == nil || denominator == nil || denominator.Sign() <= 0 {
		return 0, false
	}
	quotient, remainder := new(big.Int).QuoRem(new(big.Int).Set(numerator), denominator, new(big.Int))
	remainder.Mul(remainder, big.NewInt(2))
	if remainder.Cmp(denominator) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return 0, false
	}
	return quotient.Int64(), true
}

func paymentProviderCurrency(order core.PaymentOrder) string {
	currency := strings.TrimSpace(order.ProviderCurrency)
	if currency == "" {
		currency = "CNY"
	}
	return currency
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
