package postgresrepo

import (
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

type PaymentOrderQuery struct {
	UserID         string
	Provider       core.PaymentProvider
	Status         core.PaymentOrderStatus
	ExcludePending bool
	Offset         int
	Limit          int
}

func normalizePaymentOrderQuery(query PaymentOrderQuery) PaymentOrderQuery {
	query.UserID = strings.TrimSpace(query.UserID)
	query.Provider = core.PaymentProvider(strings.TrimSpace(string(query.Provider)))
	query.Status = core.PaymentOrderStatus(strings.TrimSpace(string(query.Status)))
	if query.Offset < 0 {
		query.Offset = 0
	}
	if query.Limit <= 0 {
		query.Limit = 25
	}
	if query.Limit > 100 {
		query.Limit = 100
	}
	return query
}

func expirePaymentOrderIfNeeded(order core.PaymentOrder, now time.Time) core.PaymentOrder {
	if order.Status != core.PaymentOrderPending || order.CreatedAt.IsZero() {
		return order
	}
	if !order.CreatedAt.Add(core.DefaultPaymentOrderPendingTTL).After(now) {
		order.Status = core.PaymentOrderClosed
		order.UpdatedAt = now
	}
	return order
}

func paidPaymentReplayMatches(order core.PaymentOrder, providerTradeNo string, paidAmountNanoUSD int64) bool {
	if paidAmountNanoUSD != order.AmountNanoUSD {
		return false
	}
	providerTradeNo = strings.TrimSpace(providerTradeNo)
	existingProviderTradeNo := strings.TrimSpace(order.ProviderTradeNo)
	if existingProviderTradeNo == "" || providerTradeNo == "" {
		return true
	}
	if strings.HasPrefix(existingProviderTradeNo, "manual_") {
		return true
	}
	return existingProviderTradeNo == providerTradeNo
}
