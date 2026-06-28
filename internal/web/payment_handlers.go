package web

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/payments"
	qrcode "github.com/skip2/go-qrcode"
)

const paymentNotifyBodyLimit = 1 << 20

type paymentCreateRequest struct {
	Provider  string `json:"provider"`
	Channel   string `json:"channel"`
	EPayType  string `json:"epay_type"`
	AmountUSD string `json:"amount_usd"`
	Subject   string `json:"subject"`
	OpenID    string `json:"openid"`
}

func (s *Server) handlePaymentsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/payments" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	filter := controlplane.PaymentOrderFilter{
		UserID:   r.URL.Query().Get("user_id"),
		Provider: core.PaymentProvider(r.URL.Query().Get("provider")),
		Status:   core.PaymentOrderStatus(r.URL.Query().Get("status")),
		Page:     parsePositiveInt(r.URL.Query().Get("page"), 1),
		PageSize: 25,
	}
	page := s.control.PaymentOrderPage(user, filter)
	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":      "page_title_payments",
		"ActiveNav":     "payments",
		"Locale":        locale,
		"Payments":      page,
		"PaymentReturn": strings.TrimSpace(r.URL.Query().Get("notice")) == "payment_return",
	}, r)
	s.render(w, "payments.html", locale, data)
}

func (s *Server) handlePaymentCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req paymentCreateRequest
	if err := decodeStrictJSONBody(w, r, 1<<20, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": translate(resolveLocale(w, r), "payment_create_invalid")})
		return
	}
	amount, err := core.ParseNanoUSDDecimal(req.AmountUSD)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": translate(resolveLocale(w, r), "payment_amount_invalid")})
		return
	}
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	settings := s.controlCurrentSettings()
	input := controlplane.PaymentOrderInput{
		UserID:        user.ID,
		Provider:      core.PaymentProvider(strings.TrimSpace(req.Provider)),
		Channel:       core.PaymentChannel(strings.TrimSpace(req.Channel)),
		Subject:       req.Subject,
		ClientIP:      clientIP(r),
		OpenID:        req.OpenID,
		NotifyBaseURL: requestBaseURL(r),
	}
	if settings.Payment.RechargeInputMode == core.RechargeInputModePaymentCNY {
		input.PaymentAmountNanoCNY = amount
	} else {
		input.AmountNanoUSD = amount
	}
	result, err := s.control.CreatePaymentOrder(r.Context(), input)
	if err != nil {
		s.recordAdminAudit(r, "error", "payment.create", "payment_order", "", string(req.Provider), err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": paymentCreateErrorMessage(resolveLocale(w, r), core.PaymentProvider(strings.TrimSpace(req.Provider)), err)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"order":   result.Order,
		"payload": result.Payload,
		"mode":    settings.Payment.RechargeInputMode,
	})
	s.publishPaymentUpdated(user, result.Order)
}

func paymentCreateErrorMessage(locale string, provider core.PaymentProvider, err error) string {
	if errors.Is(err, controlplane.ErrPaymentAmountBelowMinimum) {
		return translate(locale, "payment_amount_below_minimum")
	}
	if errors.Is(err, controlplane.ErrPaymentAmountAboveMaximum) {
		return translate(locale, "payment_amount_above_maximum")
	}
	if provider == core.PaymentProviderPersonalPay && err != nil {
		switch strings.TrimSpace(err.Error()) {
		case "personalpay is disabled":
			return translate(locale, "payment_personalpay_disabled")
		case "personalpay android token is required":
			return translate(locale, "payment_personalpay_token_required")
		case "personalpay sdk is not configured":
			return translate(locale, "payment_personalpay_not_configured")
		case "personalpay amount is invalid":
			return translate(locale, "payment_amount_invalid")
		case "personalpay: no available account":
			return translate(locale, "payment_personalpay_no_account")
		case "personalpay: account is busy":
			return translate(locale, "payment_personalpay_account_busy")
		case "personalpay: invalid payment channel":
			return translate(locale, "payment_personalpay_invalid_channel")
		default:
			message := strings.TrimSpace(err.Error())
			if message != "" {
				return message
			}
		}
	}
	return translate(locale, "payment_create_failed")
}

func (s *Server) handlePaymentCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := decodeStrictJSONBody(w, r, 1<<20, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": translate(resolveLocale(w, r), "payment_create_invalid")})
			return
		}
	} else {
		req.ID = r.FormValue("id")
	}
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	order, deleted, credited, err := s.control.CancelPaymentOrder(r.Context(), user, req.ID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": translate(resolveLocale(w, r), "payment_order_not_found")})
		return
	}
	if credited {
		s.recordSystemPaymentAudit(r, order)
	}
	s.publishPaymentUpdated(user, order)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       "ok",
		"deleted":      deleted,
		"order_status": order.Status,
	})
}

func (s *Server) handlePaymentQRCode(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/payments/qr" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	order, err := s.control.GetPaymentOrderForUser(user, r.URL.Query().Get("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.TrimSpace(order.CodeURL) == "" {
		http.Error(w, "payment order has no code url", http.StatusBadRequest)
		return
	}
	if order.Status != core.PaymentOrderPending {
		http.Error(w, "payment order is not pending", http.StatusBadRequest)
		return
	}
	png, err := qrcode.Encode(order.CodeURL, qrcode.Medium, 240)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

func (s *Server) handlePaymentStatus(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/payments/status" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	order, err := s.control.GetPaymentOrderForUser(user, r.URL.Query().Get("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"status": "error", "message": translate(resolveLocale(w, r), "payment_order_not_found")})
		return
	}
	writeJSON(w, http.StatusOK, s.paymentStatusPayload(user, order))
}

func (s *Server) paymentStatusPayload(user core.User, order core.PaymentOrder) map[string]any {
	currentUser, err := s.control.GetUser(user.ID)
	if err != nil {
		currentUser = user
	}
	hasCodeURL := strings.TrimSpace(order.CodeURL) != ""
	codeVersion := ""
	if hasCodeURL {
		codeVersion = paymentCodeVersion(order.CodeURL)
	}
	return map[string]any{
		"status":          "ok",
		"order_status":    order.Status,
		"provider":        order.Provider,
		"provider_status": order.ProviderStatus,
		"has_code_url":    hasCodeURL,
		"code_version":    codeVersion,
		"paid":            order.Status == core.PaymentOrderPaid,
		"balance":         core.FormatNanoUSD(currentUser.BalanceNanoUSD),
	}
}

func paymentCodeVersion(codeURL string) string {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(strings.TrimSpace(codeURL)))
	return fmt.Sprintf("%x", hash.Sum64())
}

func (s *Server) handlePaymentRefresh(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/payments/refresh" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := decodeStrictJSONBody(w, r, 1<<20, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": translate(resolveLocale(w, r), "payment_create_invalid")})
			return
		}
	} else {
		req.ID = r.FormValue("id")
	}
	order, credited, err := s.control.RefreshPaymentOrder(r.Context(), user, req.ID)
	if err != nil {
		if wantsJSON(r) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": err.Error()})
			return
		}
		s.redirectWithNoticeError(w, r, "/payments", err)
		return
	}
	if credited {
		s.recordSystemPaymentAudit(r, order)
	}
	s.publishPaymentUpdated(user, order)
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, s.paymentStatusPayload(user, order))
		return
	}
	http.Redirect(w, r, "/payments", http.StatusSeeOther)
}

func (s *Server) handlePaymentReturn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	provider := core.PaymentProvider(strings.Trim(strings.TrimPrefix(r.URL.Path, "/payments/return/"), "/"))
	if !validPaymentReturnProvider(provider) {
		http.NotFound(w, r)
		return
	}
	orderID := strings.TrimSpace(r.URL.Query().Get("out_trade_no"))
	if orderID == "" {
		orderID = strings.TrimSpace(r.URL.Query().Get("outTradeNo"))
	}
	target := "/payments?notice=payment_return"
	if orderID != "" {
		target += "&order_id=" + url.QueryEscape(orderID)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) handlePaymentNotify(w http.ResponseWriter, r *http.Request) {
	provider := strings.Trim(strings.TrimPrefix(r.URL.Path, "/payments/notify/"), "/")
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	settings := s.controlCurrentSettings()
	var notification payments.Notification
	var err error
	switch core.PaymentProvider(provider) {
	case core.PaymentProviderAlipay:
		r.Body = http.MaxBytesReader(w, r.Body, paymentNotifyBodyLimit)
		if err = r.ParseForm(); err == nil {
			notification, err = payments.VerifyAlipayNotificationWithSettings(r.PostForm, settings.Payment.Alipay)
		}
	case core.PaymentProviderWeChatPay:
		var body []byte
		r.Body = http.MaxBytesReader(w, r.Body, paymentNotifyBodyLimit)
		body, err = io.ReadAll(r.Body)
		if err == nil {
			notification, err = payments.VerifyWeChatPayNotification(r.Header, body, settings.Payment.WeChatPay)
		}
	default:
		err = http.ErrNotSupported
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	order, credited, err := s.control.ApplyPaymentEvent(notification)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if credited {
		s.recordSystemPaymentAudit(r, order)
	}
	s.publishPaymentUpdated(core.User{ID: order.UserID}, order)
	if notification.Provider == core.PaymentProviderWeChatPay {
		writeJSON(w, http.StatusOK, map[string]string{"code": "SUCCESS", "message": "OK"})
		return
	}
	_, _ = w.Write([]byte("success"))
}

func validPaymentReturnProvider(provider core.PaymentProvider) bool {
	switch provider {
	case core.PaymentProviderWeChatPay, core.PaymentProviderAlipay:
		return true
	default:
		return false
	}
}

func (s *Server) controlCurrentSettings() core.SystemSettings {
	if s == nil || s.control == nil {
		return core.DefaultSystemSettings()
	}
	settings, err := s.control.GetSystemSettings()
	if err != nil {
		return core.DefaultSystemSettings()
	}
	return settings
}

func (s *Server) recordSystemPaymentAudit(r *http.Request, order core.PaymentOrder) {
	s.recordAdminAudit(r, "ok", "user.balance", "user", order.UserID, s.userDisplayName(order.UserID), "payment="+order.OutTradeNo+" provider="+string(order.Provider)+" amount="+core.FormatNanoUSD(order.AmountNanoUSD))
}
