package web

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
)

func (s *Server) handleFinancePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/finance" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	locale := resolveLocale(w, r)
	tab := normalizeFinanceTab(r.URL.Query().Get("tab"))
	if tab == "overview" && s.handleFinanceOverviewPartial(w, r, locale) {
		return
	}
	if deferredPartialRequested(r, "finance-page") {
		page := s.financePageForRequest(r, tab)
		data := withCSRFData(map[string]any{
			"TitleKey":                "page_title_finance",
			"ActiveNav":               "finance",
			"Locale":                  locale,
			"Finance":                 page,
			"FinanceTab":              tab,
			"FinanceOverviewDeferred": financeOverviewDeferredURLs(r, tab),
			"UserIdentityByID":        s.financeUserIdentities(page),
			"ReconcileReleaseOK":      strings.TrimSpace(r.URL.Query().Get("notice")) == "reconcile_release_ok",
			"FinanceNotice":           strings.TrimSpace(r.URL.Query().Get("notice")),
		}, r)
		s.renderFragment(w, "finance.html", "finance_page", locale, data)
		return
	}
	page := s.financePageForRequest(r, tab)
	data := withCSRFData(map[string]any{
		"TitleKey":                "page_title_finance",
		"ActiveNav":               "finance",
		"Locale":                  locale,
		"Finance":                 page,
		"FinanceTab":              tab,
		"FinanceOverviewDeferred": financeOverviewDeferredURLs(r, tab),
		"UserIdentityByID":        s.financeUserIdentities(page),
		"ReconcileReleaseOK":      strings.TrimSpace(r.URL.Query().Get("notice")) == "reconcile_release_ok",
		"FinanceNotice":           strings.TrimSpace(r.URL.Query().Get("notice")),
	}, r)
	s.render(w, "finance.html", locale, data)
}

func (s *Server) handleFinanceOverviewPartial(w http.ResponseWriter, r *http.Request, locale string) bool {
	type overviewPartial struct {
		name string
		page func() controlplane.FinancePage
	}
	partials := []overviewPartial{
		{
			name: "finance-overview-daily",
			page: func() controlplane.FinancePage {
				return controlplane.FinancePage{Daily: s.control.FinanceOverviewDailySummaries(14)}
			},
		},
		{
			name: "finance-overview-models",
			page: func() controlplane.FinancePage {
				return controlplane.FinancePage{Models: s.control.FinanceOverviewModelSummaries(10)}
			},
		},
		{
			name: "finance-overview-top-users",
			page: func() controlplane.FinancePage {
				return controlplane.FinancePage{Overview: s.control.FinanceOverviewTopUsersBySpend(5)}
			},
		},
		{
			name: "finance-overview-top-clients",
			page: func() controlplane.FinancePage {
				return controlplane.FinancePage{Overview: s.control.FinanceOverviewTopClientsBySpend(5)}
			},
		},
	}
	for _, partial := range partials {
		if !deferredPartialRequested(r, partial.name) {
			continue
		}
		page := partial.page()
		data := withCSRFData(map[string]any{
			"TitleKey":         "page_title_finance",
			"ActiveNav":        "finance",
			"Locale":           locale,
			"Finance":          page,
			"FinanceTab":       "overview",
			"UserIdentityByID": s.financeUserIdentities(page),
		}, r)
		s.renderFragment(w, "finance.html", "finance_page", locale, data)
		return true
	}
	return false
}

func financeOverviewDeferredURLs(r *http.Request, tab string) map[string]string {
	if normalizeFinanceTab(tab) != "overview" || r == nil || r.URL == nil {
		return nil
	}
	return map[string]string{
		"daily":       deferredPartialURL(r, "finance-overview-daily"),
		"models":      deferredPartialURL(r, "finance-overview-models"),
		"top_users":   deferredPartialURL(r, "finance-overview-top-users"),
		"top_clients": deferredPartialURL(r, "finance-overview-top-clients"),
	}
}

func (s *Server) financeUserIdentities(page controlplane.FinancePage) map[string]core.User {
	ids := make(map[string]struct{})
	add := func(userID string) {
		userID = strings.TrimSpace(userID)
		if userID != "" {
			ids[userID] = struct{}{}
		}
	}
	for _, summary := range page.Overview.TopClientsBySpend {
		add(summary.Client.OwnerUserID)
	}
	for _, entry := range page.Overview.RecentLedger {
		add(entry.UserID)
	}
	for _, order := range page.Orders.Orders {
		add(order.UserID)
	}
	add(page.Orders.Filter.UserID)
	for _, row := range page.Usage.Rows {
		add(row.Request.UserID)
	}
	add(page.Usage.Filter.UserID)

	out := make(map[string]core.User, len(ids))
	for id := range ids {
		user, err := s.control.GetUser(id)
		if err != nil {
			continue
		}
		out[user.ID] = user
	}
	return out
}

func (s *Server) financePageForRequest(r *http.Request, tab string) controlplane.FinancePage {
	query := r.URL.Query()
	orderStatus := core.PaymentOrderStatus(strings.TrimSpace(query.Get("order_status")))
	if !query.Has("order_status") {
		orderStatus = core.PaymentOrderPaid
	}
	return s.control.FinancePageForTab(r.Context(), tab, controlplane.FinanceUserFilter{
		Page:     parsePositiveInt(query.Get("user_page"), 1),
		PageSize: 25,
	}, controlplane.PaymentOrderFilter{
		UserID:   strings.TrimSpace(query.Get("order_user_id")),
		Provider: core.PaymentProvider(strings.TrimSpace(query.Get("order_provider"))),
		Status:   orderStatus,
		Page:     parsePositiveInt(query.Get("order_page"), 1),
		PageSize: 10,
	}, controlplane.UsageLogFilter{
		UserID:    strings.TrimSpace(query.Get("usage_user_id")),
		ClientID:  strings.TrimSpace(query.Get("usage_client_id")),
		Model:     strings.TrimSpace(query.Get("usage_model")),
		Status:    core.BillingRequestStatus(strings.TrimSpace(query.Get("usage_status"))),
		StartedAt: parseOptionalDateTime(query.Get("usage_started_at")),
		EndedAt:   parseOptionalDateTime(query.Get("usage_ended_at")),
		Page:      parsePositiveInt(query.Get("usage_page"), 1),
		PageSize:  10,
	})
}

func (s *Server) handleFinanceActions(w http.ResponseWriter, r *http.Request) {
	switch strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/finance/"), "/") {
	case "adjust":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleFinanceAdjust(w, r)
	case "export":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleFinanceExport(w, r)
	case "reconcile/release":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleFinanceReleaseReservedBilling(w, r)
	case "orders/confirm-paid":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleFinancePaymentConfirmPaid(w, r)
	case "refunds/create":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleFinanceRefundCreate(w, r)
	case "refunds/complete":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleFinanceRefundComplete(w, r)
	case "refunds/fail":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleFinanceRefundFail(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleFinanceAdjust(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, "/admin/finance", err)
		return
	}
	userID := strings.TrimSpace(r.FormValue("user_id"))
	if userID == "" {
		s.redirectWithNoticeError(w, r, "/admin/finance", fmt.Errorf("user is required"))
		return
	}
	amount, err := parseNanoUSDFormValue(r, "amount_usd")
	if err != nil {
		s.redirectWithNoticeError(w, r, "/admin/finance", err)
		return
	}
	if amount <= 0 {
		s.redirectWithNoticeError(w, r, "/admin/finance", fmt.Errorf("amount must be greater than zero"))
		return
	}
	switch r.FormValue("direction") {
	case "credit":
	case "debit":
		amount = -amount
	default:
		s.redirectWithNoticeError(w, r, "/admin/finance", fmt.Errorf("direction is required"))
		return
	}

	user, previousBalance, err := s.control.AdjustUserBalance(userID, controlplane.UserBalanceAdjustment{
		AmountNanoUSD: amount,
		Reason:        r.FormValue("reason"),
	})
	if err != nil {
		s.recordAdminAudit(r, "error", "user.balance", "user", userID, "", err.Error())
		s.redirectWithNoticeError(w, r, "/admin/finance", err)
		return
	}
	s.recordAdminAudit(r, "ok", "user.balance", "user", user.ID, user.Username, fmt.Sprintf("previous=%s current=%s delta=%s reason=%s", core.FormatNanoUSD(previousBalance), core.FormatNanoUSD(user.BalanceNanoUSD), core.FormatNanoUSD(amount), strings.TrimSpace(r.FormValue("reason"))))
	s.publishBalanceUpdated(user.ID)
	s.publishFinanceChanged("balance_adjustment", user.ID)
	http.Redirect(w, r, "/admin/finance", http.StatusSeeOther)
}

func (s *Server) handleFinancePaymentConfirmPaid(w http.ResponseWriter, r *http.Request) {
	target := "/admin/finance?tab=orders&order_status="
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, target, err)
		return
	}
	orderID := strings.TrimSpace(r.FormValue("order_id"))
	order, credited, err := s.control.ConfirmPaymentOrderPaidManually(controlplane.ManualPaymentConfirmInput{
		OrderID:         orderID,
		ProviderTradeNo: r.FormValue("provider_trade_no"),
	})
	if err != nil {
		s.recordAdminAudit(r, "error", "payment.order.confirm_paid", "payment_order", orderID, "", err.Error())
		s.redirectWithNoticeError(w, r, target, err)
		return
	}
	s.recordAdminAudit(r, "ok", "payment.order.confirm_paid", "payment_order", order.ID, order.OutTradeNo, fmt.Sprintf("credited=%t amount=%s provider_trade_no=%s", credited, core.FormatNanoUSD(order.AmountNanoUSD), strings.TrimSpace(order.ProviderTradeNo)))
	s.publishPaymentUpdated(core.User{ID: order.UserID}, order)
	http.Redirect(w, r, target+"&notice=payment_confirmed_paid", http.StatusSeeOther)
}

func (s *Server) handleFinanceRefundCreate(w http.ResponseWriter, r *http.Request) {
	target := "/admin/finance?tab=orders"
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, target, err)
		return
	}
	orderID := strings.TrimSpace(r.FormValue("order_id"))
	amount, err := parseNanoUSDFormValue(r, "amount_usd")
	if err != nil {
		s.redirectWithNoticeError(w, r, target, err)
		return
	}
	refund, summary, err := s.control.CreateManualPaymentRefund(controlplane.ManualPaymentRefundInput{
		OrderID:       orderID,
		AmountNanoUSD: amount,
		Reason:        r.FormValue("reason"),
	})
	if err != nil {
		s.recordAdminAudit(r, "error", "payment.refund.create", "payment_order", orderID, "", err.Error())
		s.redirectWithNoticeError(w, r, target, err)
		return
	}
	s.recordAdminAudit(r, "ok", "payment.refund.create", "payment_refund", refund.ID, refund.OutTradeNo, fmt.Sprintf("amount=%s payout=%s remaining=%s reason=%s", core.FormatNanoUSD(refund.AmountNanoUSD), formatCNYCentsDisplay(refund.ProviderAmountCents), core.FormatNanoUSD(summary.RemainingNanoUSD), strings.TrimSpace(refund.Reason)))
	s.publishFinanceChanged("payment_refund_pending", refund.ID)
	http.Redirect(w, r, target+"&notice=refund_created", http.StatusSeeOther)
}

func (s *Server) handleFinanceRefundComplete(w http.ResponseWriter, r *http.Request) {
	target := "/admin/finance?tab=orders"
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, target, err)
		return
	}
	refundID := strings.TrimSpace(r.FormValue("refund_id"))
	refund, debited, err := s.control.CompleteManualPaymentRefund(controlplane.ManualPaymentRefundCompleteInput{
		RefundID:  refundID,
		PayoutRef: r.FormValue("payout_ref"),
		Note:      r.FormValue("note"),
	})
	if err != nil {
		s.recordAdminAudit(r, "error", "payment.refund.complete", "payment_refund", refundID, "", err.Error())
		s.redirectWithNoticeError(w, r, target, err)
		return
	}
	s.recordAdminAudit(r, "ok", "payment.refund.complete", "payment_refund", refund.ID, refund.OutTradeNo, fmt.Sprintf("amount=%s payout=%s ref=%s debited=%t", core.FormatNanoUSD(refund.AmountNanoUSD), formatCNYCentsDisplay(refund.ProviderAmountCents), strings.TrimSpace(refund.ManualPayoutRef), debited))
	if debited {
		s.publishBalanceUpdated(refund.UserID)
	}
	s.publishFinanceChanged("payment_refund_done", refund.ID)
	http.Redirect(w, r, target+"&notice=refund_completed", http.StatusSeeOther)
}

func (s *Server) handleFinanceRefundFail(w http.ResponseWriter, r *http.Request) {
	target := "/admin/finance?tab=orders"
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, target, err)
		return
	}
	refundID := strings.TrimSpace(r.FormValue("refund_id"))
	refund, err := s.control.FailManualPaymentRefund(refundID, r.FormValue("note"))
	if err != nil {
		s.recordAdminAudit(r, "error", "payment.refund.fail", "payment_refund", refundID, "", err.Error())
		s.redirectWithNoticeError(w, r, target, err)
		return
	}
	s.recordAdminAudit(r, "ok", "payment.refund.fail", "payment_refund", refund.ID, refund.OutTradeNo, fmt.Sprintf("amount=%s note=%s", core.FormatNanoUSD(refund.AmountNanoUSD), strings.TrimSpace(refund.RawResponse)))
	s.publishFinanceChanged("payment_refund_failed", refund.ID)
	http.Redirect(w, r, target+"&notice=refund_failed", http.StatusSeeOther)
}

func (s *Server) handleFinanceReleaseReservedBilling(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, "/admin/finance?tab=reconcile", err)
		return
	}
	requestID := strings.TrimSpace(r.FormValue("request_id"))
	clientID := strings.TrimSpace(r.FormValue("client_id"))
	request, err := s.control.ReleaseReservedBillingRequest(requestID, clientID, "released from finance reconciliation")
	if err != nil {
		s.recordAdminAudit(r, "error", "billing.release_reserved", "billing_request", requestID, clientID, err.Error())
		s.redirectWithNoticeError(w, r, "/admin/finance?tab=reconcile", err)
		return
	}
	s.recordAdminAudit(r, "ok", "billing.release_reserved", "billing_request", request.RequestID, request.ClientID, fmt.Sprintf("user=%s amount=%s model=%s", s.userDisplayName(request.UserID), core.FormatNanoUSD(0), request.Model))
	s.publishBalanceUpdated(request.UserID)
	s.publishFinanceChanged("reserved_billing_release", request.RequestID)
	http.Redirect(w, r, "/admin/finance?tab=reconcile&notice=reconcile_release_ok", http.StatusSeeOther)
}

func (s *Server) handleFinanceExport(w http.ResponseWriter, r *http.Request) {
	filename := "finance-users-" + time.Now().Format("20060102-150405") + ".csv"
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.WriteHeader(http.StatusOK)

	writer := csv.NewWriter(w)
	if err := writer.Write([]string{
		"user_id",
		"username",
		"role",
		"enabled",
		"balance_usd",
		"recharge_usd",
		"reward_usd",
		"spend_usd",
		"refund_usd",
		"client_count",
		"client_spend_usd",
		"last_payment_at",
		"last_spend_at",
	}); err != nil {
		return
	}
	err := s.control.ForEachFinanceUserSummaryForExport(func(summary controlplane.FinanceUserSummary) error {
		return writer.Write([]string{
			summary.User.ID,
			summary.User.Username,
			string(summary.User.Role),
			fmt.Sprintf("%t", summary.User.Enabled),
			core.FormatNanoUSD(summary.BalanceNanoUSD),
			core.FormatNanoUSD(summary.RechargeNanoUSD),
			core.FormatNanoUSD(summary.RewardNanoUSD),
			core.FormatNanoUSD(summary.SpendNanoUSD),
			core.FormatNanoUSD(summary.RefundNanoUSD),
			fmt.Sprintf("%d", summary.ClientCount),
			core.FormatNanoUSD(summary.ClientSpendNanoUSD),
			financeCSVTime(summary.LastPaymentAt),
			financeCSVTime(summary.LastSpendAt),
		})
	})
	writer.Flush()
	if err != nil || writer.Error() != nil {
		return
	}
}

func financeCSVTime(value *time.Time) string {
	if value == nil || value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func normalizeFinanceTab(raw string) string {
	tab := strings.TrimSpace(raw)
	switch tab {
	case "":
		return "orders"
	case "overview", "users", "ledger", "orders", "usage", "tokens", "reconcile":
		return tab
	default:
		return "overview"
	}
}
