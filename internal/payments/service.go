package payments

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

type CreateOrderInput struct {
	Order       core.PaymentOrder
	Settings    core.SystemPaymentSettings
	Description string
	ClientIP    string
	OpenID      string
}

type CreateOrderResult struct {
	Order   core.PaymentOrder
	Payload map[string]string
}

type QueryOrderInput struct {
	Order    core.PaymentOrder
	Settings core.SystemPaymentSettings
}

type CancelOrderInput struct {
	Order    core.PaymentOrder
	Settings core.SystemPaymentSettings
}

type Event struct {
	Provider            core.PaymentProvider
	OutTradeNo          string
	ProviderTradeNo     string
	Status              core.PaymentOrderStatus
	ProviderStatus      string
	AmountNanoUSD       int64
	ProviderAmountCents int64
	ProviderCurrency    string
	CodeURL             string
	PayURL              string
	PrepayID            string
	PaidAt              time.Time
	RawResponse         string
	RawBody             string
}

type QueryOrderResult = Event
type CancelOrderResult = Event
type Notification = Event

type Client struct {
	httpClient  *http.Client
	now         func() time.Time
	personalPay personalPayEngine
}

func (c *Client) PersonalPayConfigured() bool {
	return c != nil && c.personalPay != nil
}

var defaultHTTPClient = &http.Client{Timeout: 30 * time.Second}

func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = defaultHTTPClient
	}
	return &Client{httpClient: httpClient, now: time.Now}
}

func (c *Client) CreateOrder(ctx context.Context, input CreateOrderInput) (CreateOrderResult, error) {
	switch input.Order.Provider {
	case core.PaymentProviderWeChatPay:
		return c.createWeChatPayOrder(ctx, input)
	case core.PaymentProviderAlipay:
		return c.createAlipayOrder(ctx, input)
	case core.PaymentProviderPersonalPay:
		return c.createPersonalPayOrder(ctx, input)
	default:
		return CreateOrderResult{}, fmt.Errorf("unsupported payment provider %q", input.Order.Provider)
	}
}

func (c *Client) QueryOrder(ctx context.Context, input QueryOrderInput) (QueryOrderResult, error) {
	switch input.Order.Provider {
	case core.PaymentProviderWeChatPay:
		return c.queryWeChatPayOrder(ctx, input)
	case core.PaymentProviderAlipay:
		return c.queryAlipayOrder(ctx, input)
	case core.PaymentProviderPersonalPay:
		return c.queryPersonalPayOrder(ctx, input)
	default:
		return QueryOrderResult{}, fmt.Errorf("unsupported payment provider %q", input.Order.Provider)
	}
}

func (c *Client) CancelOrder(ctx context.Context, input CancelOrderInput) (CancelOrderResult, error) {
	switch input.Order.Provider {
	case core.PaymentProviderPersonalPay:
		return c.cancelPersonalPayOrder(ctx, input)
	default:
		return CancelOrderResult{}, fmt.Errorf("unsupported payment provider %q", input.Order.Provider)
	}
}

func (c *Client) createWeChatPayOrder(ctx context.Context, input CreateOrderInput) (CreateOrderResult, error) {
	settings := input.Settings.WeChatPay
	if !settings.Enabled {
		return CreateOrderResult{}, errors.New("wechat pay is disabled")
	}
	privateKey, err := parseRSAPrivateKey(settings.MerchantPrivateKeyPEM)
	if err != nil {
		return CreateOrderResult{}, err
	}
	path, body := wechatPayCreateOrderRequest(input)
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return CreateOrderResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.mch.weixin.qq.com"+path, bytes.NewReader(bodyBytes))
	if err != nil {
		return CreateOrderResult{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	authorization, err := wechatPayAuthorization(req.Method, path, string(bodyBytes), settings.MchID, settings.MerchantSerialNo, privateKey, c.now())
	if err != nil {
		return CreateOrderResult{}, err
	}
	req.Header.Set("Authorization", authorization)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return CreateOrderResult{}, err
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return CreateOrderResult{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return CreateOrderResult{}, fmt.Errorf("wechat pay create order failed: status=%d body=%s", resp.StatusCode, string(respBytes))
	}
	if err := verifyWeChatPaySignature(resp.Header, respBytes, settings); err != nil {
		return CreateOrderResult{}, err
	}
	var payload map[string]string
	if err := json.Unmarshal(respBytes, &payload); err != nil {
		return CreateOrderResult{}, err
	}
	order := input.Order
	order.RawRequest = string(bodyBytes)
	order.RawResponse = string(respBytes)
	order.CodeURL = firstNonEmpty(payload["code_url"], payload["codeUrl"], payload["codeURL"])
	order.PrepayID = firstNonEmpty(payload["prepay_id"], payload["prepayId"], payload["prepayID"])
	order.PayURL = firstNonEmpty(payload["h5_url"], payload["h5Url"], payload["pay_url"], payload["payUrl"])
	return CreateOrderResult{Order: order, Payload: payload}, nil
}

func (c *Client) queryWeChatPayOrder(ctx context.Context, input QueryOrderInput) (QueryOrderResult, error) {
	settings := input.Settings.WeChatPay
	if !settings.Enabled {
		return QueryOrderResult{}, errors.New("wechat pay is disabled")
	}
	privateKey, err := parseRSAPrivateKey(settings.MerchantPrivateKeyPEM)
	if err != nil {
		return QueryOrderResult{}, err
	}
	canonicalURL := "/v3/pay/transactions/out-trade-no/" + url.PathEscape(input.Order.OutTradeNo) + "?mchid=" + url.QueryEscape(settings.MchID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.mch.weixin.qq.com"+canonicalURL, nil)
	if err != nil {
		return QueryOrderResult{}, err
	}
	req.Header.Set("Accept", "application/json")
	authorization, err := wechatPayAuthorization(req.Method, canonicalURL, "", settings.MchID, settings.MerchantSerialNo, privateKey, c.now())
	if err != nil {
		return QueryOrderResult{}, err
	}
	req.Header.Set("Authorization", authorization)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return QueryOrderResult{}, err
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return QueryOrderResult{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return QueryOrderResult{}, fmt.Errorf("wechat pay query order failed: status=%d body=%s", resp.StatusCode, string(respBytes))
	}
	if err := verifyWeChatPaySignature(resp.Header, respBytes, settings); err != nil {
		return QueryOrderResult{}, err
	}
	var tx struct {
		TransactionID string `json:"transaction_id"`
		TradeState    string `json:"trade_state"`
		SuccessTime   string `json:"success_time"`
		Amount        struct {
			PayerTotal int64 `json:"payer_total"`
			Total      int64 `json:"total"`
		} `json:"amount"`
	}
	if err := json.Unmarshal(respBytes, &tx); err != nil {
		return QueryOrderResult{}, err
	}
	status := core.PaymentOrderPending
	switch tx.TradeState {
	case "SUCCESS":
		status = core.PaymentOrderPaid
	case "CLOSED", "REVOKED":
		status = core.PaymentOrderClosed
	case "PAYERROR":
		status = core.PaymentOrderFailed
	}
	total := tx.Amount.PayerTotal
	if total <= 0 {
		total = tx.Amount.Total
	}
	if status == core.PaymentOrderPaid && total <= 0 {
		return QueryOrderResult{}, errors.New("wechat pay query returned paid order without amount")
	}
	paidAt, _ := time.Parse(time.RFC3339, tx.SuccessTime)
	return QueryOrderResult{
		ProviderTradeNo:     tx.TransactionID,
		Status:              status,
		AmountNanoUSD:       total * (core.NanoUSDPerUSD / 100),
		ProviderAmountCents: total,
		ProviderCurrency:    "CNY",
		PaidAt:              paidAt,
		RawResponse:         string(respBytes),
	}, nil
}

func wechatPayCreateOrderRequest(input CreateOrderInput) (string, map[string]any) {
	settings := input.Settings.WeChatPay
	amountFen := providerOrderCents(input.Order)
	body := map[string]any{
		"appid":        settings.AppID,
		"mchid":        settings.MchID,
		"description":  firstNonEmpty(input.Description, input.Order.Subject, "AI Gateway recharge"),
		"out_trade_no": input.Order.OutTradeNo,
		"notify_url":   settings.NotifyURL,
		"amount": map[string]any{
			"total":    amountFen,
			"currency": firstNonEmpty(input.Order.ProviderCurrency, "CNY"),
		},
	}
	switch input.Order.Channel {
	case core.PaymentChannelJSAPI:
		body["payer"] = map[string]any{"openid": strings.TrimSpace(input.OpenID)}
		return "/v3/pay/transactions/jsapi", body
	case core.PaymentChannelWAP:
		body["scene_info"] = map[string]any{
			"payer_client_ip": strings.TrimSpace(input.ClientIP),
			"h5_info":         map[string]any{"type": "Wap"},
		}
		return "/v3/pay/transactions/h5", body
	default:
		return "/v3/pay/transactions/native", body
	}
}

func wechatPayAuthorization(method, path, body, mchID, serialNo string, privateKey *rsa.PrivateKey, now time.Time) (string, error) {
	timestamp := strconv.FormatInt(now.Unix(), 10)
	nonce := randomNonce()
	message := strings.Join([]string{method, path, timestamp, nonce, body}, "\n") + "\n"
	digest := sha256.Sum256([]byte(message))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`WECHATPAY2-SHA256-RSA2048 mchid="%s",nonce_str="%s",signature="%s",timestamp="%s",serial_no="%s"`, mchID, nonce, base64.StdEncoding.EncodeToString(signature), timestamp, serialNo), nil
}

func (c *Client) createAlipayOrder(_ context.Context, input CreateOrderInput) (CreateOrderResult, error) {
	settings := input.Settings.Alipay
	if !settings.Enabled {
		return CreateOrderResult{}, errors.New("alipay is disabled")
	}
	if err := validateAlipaySignType(settings.SignType); err != nil {
		return CreateOrderResult{}, err
	}
	privateKey, err := parseRSAPrivateKey(settings.PrivateKeyPEM)
	if err != nil {
		return CreateOrderResult{}, err
	}
	method := "alipay.trade.precreate"
	if input.Order.Channel == core.PaymentChannelPage {
		method = "alipay.trade.page.pay"
	} else if input.Order.Channel == core.PaymentChannelWAP {
		method = "alipay.trade.wap.pay"
	}
	bizContent := map[string]any{
		"out_trade_no": input.Order.OutTradeNo,
		"total_amount": providerCentsDecimal(providerOrderCents(input.Order)),
		"subject":      firstNonEmpty(input.Description, input.Order.Subject, "AI Gateway recharge"),
	}
	if input.Order.Channel == core.PaymentChannelPage {
		bizContent["product_code"] = "FAST_INSTANT_TRADE_PAY"
	} else if input.Order.Channel == core.PaymentChannelWAP {
		bizContent["product_code"] = "QUICK_WAP_WAY"
	}
	bizContentBytes, err := json.Marshal(bizContent)
	if err != nil {
		return CreateOrderResult{}, err
	}
	values := url.Values{}
	values.Set("app_id", settings.AppID)
	values.Set("method", method)
	values.Set("format", "JSON")
	values.Set("charset", "utf-8")
	values.Set("sign_type", firstNonEmpty(settings.SignType, "RSA2"))
	values.Set("timestamp", c.now().Format("2006-01-02 15:04:05"))
	values.Set("version", "1.0")
	values.Set("notify_url", settings.NotifyURL)
	if settings.ReturnURL != "" {
		values.Set("return_url", settings.ReturnURL)
	}
	values.Set("biz_content", string(bizContentBytes))
	signature, err := signAlipayValues(values, privateKey)
	if err != nil {
		return CreateOrderResult{}, err
	}
	values.Set("sign", signature)
	order := input.Order
	order.RawRequest = values.Encode()
	order.PayURL = strings.TrimRight(firstNonEmpty(settings.GatewayURL, "https://openapi.alipay.com/gateway.do"), "?") + "?" + values.Encode()
	if method == "alipay.trade.precreate" {
		order.RawResponse = order.PayURL
	}
	return CreateOrderResult{Order: order, Payload: map[string]string{"pay_url": order.PayURL}}, nil
}

func (c *Client) queryAlipayOrder(ctx context.Context, input QueryOrderInput) (QueryOrderResult, error) {
	settings := input.Settings.Alipay
	if !settings.Enabled {
		return QueryOrderResult{}, errors.New("alipay is disabled")
	}
	if err := validateAlipaySignType(settings.SignType); err != nil {
		return QueryOrderResult{}, err
	}
	privateKey, err := parseRSAPrivateKey(settings.PrivateKeyPEM)
	if err != nil {
		return QueryOrderResult{}, err
	}
	bizContentBytes, err := json.Marshal(map[string]any{"out_trade_no": input.Order.OutTradeNo})
	if err != nil {
		return QueryOrderResult{}, err
	}
	values := url.Values{}
	values.Set("app_id", settings.AppID)
	values.Set("method", "alipay.trade.query")
	values.Set("format", "JSON")
	values.Set("charset", "utf-8")
	values.Set("sign_type", firstNonEmpty(settings.SignType, "RSA2"))
	values.Set("timestamp", c.now().Format("2006-01-02 15:04:05"))
	values.Set("version", "1.0")
	values.Set("biz_content", string(bizContentBytes))
	signature, err := signAlipayValues(values, privateKey)
	if err != nil {
		return QueryOrderResult{}, err
	}
	values.Set("sign", signature)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, firstNonEmpty(settings.GatewayURL, "https://openapi.alipay.com/gateway.do"), strings.NewReader(values.Encode()))
	if err != nil {
		return QueryOrderResult{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return QueryOrderResult{}, err
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return QueryOrderResult{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return QueryOrderResult{}, fmt.Errorf("alipay query order failed: status=%d body=%s", resp.StatusCode, string(respBytes))
	}
	var envelope struct {
		Response json.RawMessage `json:"alipay_trade_query_response"`
		Sign     string          `json:"sign"`
	}
	if err := json.Unmarshal(respBytes, &envelope); err != nil {
		return QueryOrderResult{}, err
	}
	if err := verifyAlipayResponseSignature(envelope.Response, envelope.Sign, settings.AlipayPublicKeyPEM); err != nil {
		return QueryOrderResult{}, err
	}
	var queryResponse struct {
		Code        string `json:"code"`
		SubMsg      string `json:"sub_msg"`
		TradeNo     string `json:"trade_no"`
		TradeStatus string `json:"trade_status"`
		TotalAmount string `json:"total_amount"`
		SendPayDate string `json:"send_pay_date"`
	}
	if err := json.Unmarshal(envelope.Response, &queryResponse); err != nil {
		return QueryOrderResult{}, err
	}
	if queryResponse.Code != "10000" {
		return QueryOrderResult{}, fmt.Errorf("alipay query order failed: %s", firstNonEmpty(queryResponse.SubMsg, queryResponse.Code))
	}
	status := core.PaymentOrderPending
	switch queryResponse.TradeStatus {
	case "TRADE_SUCCESS", "TRADE_FINISHED":
		status = core.PaymentOrderPaid
	case "TRADE_CLOSED":
		status = core.PaymentOrderClosed
	}
	amount, err := core.ParseNanoUSDDecimal(queryResponse.TotalAmount)
	if err != nil {
		return QueryOrderResult{}, err
	}
	providerAmountCents := nanoUSDToCents(amount)
	if status == core.PaymentOrderPaid && amount <= 0 {
		return QueryOrderResult{}, errors.New("alipay query returned paid order without amount")
	}
	paidAt, _ := time.ParseInLocation("2006-01-02 15:04:05", queryResponse.SendPayDate, time.Local)
	return QueryOrderResult{
		ProviderTradeNo:     queryResponse.TradeNo,
		Status:              status,
		AmountNanoUSD:       amount,
		ProviderAmountCents: providerAmountCents,
		ProviderCurrency:    "CNY",
		PaidAt:              paidAt,
		RawResponse:         string(respBytes),
	}, nil
}

func VerifyAlipayNotification(values url.Values, publicKeyPEM string) (Notification, error) {
	return VerifyAlipayNotificationWithSettings(values, core.AlipaySettings{AlipayPublicKeyPEM: publicKeyPEM})
}

func VerifyAlipayNotificationWithSettings(values url.Values, settings core.AlipaySettings) (Notification, error) {
	if err := validateAlipaySignType(values.Get("sign_type")); err != nil {
		return Notification{}, err
	}
	signature := values.Get("sign")
	if signature == "" {
		return Notification{}, errors.New("missing alipay signature")
	}
	if strings.TrimSpace(settings.AppID) != "" && values.Get("app_id") != strings.TrimSpace(settings.AppID) {
		return Notification{}, errors.New("alipay app id does not match")
	}
	publicKey, err := parseRSAPublicKey(settings.AlipayPublicKeyPEM)
	if err != nil {
		return Notification{}, err
	}
	signed := alipayCanonicalValues(values, true)
	digest := sha256.Sum256([]byte(signed))
	decoded, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return Notification{}, err
	}
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], decoded); err != nil {
		return Notification{}, err
	}
	if values.Get("trade_status") != "TRADE_SUCCESS" && values.Get("trade_status") != "TRADE_FINISHED" {
		return Notification{}, errors.New("alipay trade is not successful")
	}
	amount, err := core.ParseNanoUSDDecimal(values.Get("total_amount"))
	if err != nil {
		return Notification{}, err
	}
	if amount <= 0 {
		return Notification{}, errors.New("alipay notification amount is invalid")
	}
	paidAt, _ := time.ParseInLocation("2006-01-02 15:04:05", values.Get("gmt_payment"), time.Local)
	return Notification{
		Provider:            core.PaymentProviderAlipay,
		OutTradeNo:          values.Get("out_trade_no"),
		ProviderTradeNo:     values.Get("trade_no"),
		Status:              core.PaymentOrderPaid,
		AmountNanoUSD:       amount,
		ProviderAmountCents: nanoUSDToCents(amount),
		ProviderCurrency:    "CNY",
		PaidAt:              paidAt,
	}, nil
}

func verifyAlipayResponseSignature(content []byte, signature, publicKeyPEM string) error {
	if strings.TrimSpace(publicKeyPEM) == "" {
		return nil
	}
	if len(content) == 0 {
		return errors.New("missing alipay response content")
	}
	if strings.TrimSpace(signature) == "" {
		return errors.New("missing alipay response signature")
	}
	publicKey, err := parseRSAPublicKey(publicKeyPEM)
	if err != nil {
		return err
	}
	decoded, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(content)
	return rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], decoded)
}

func VerifyWeChatPayNotification(headers http.Header, body []byte, settings core.WeChatPaySettings) (Notification, error) {
	if err := verifyWeChatPaySignature(headers, body, settings); err != nil {
		return Notification{}, err
	}
	var envelope struct {
		Resource struct {
			Algorithm      string `json:"algorithm"`
			Ciphertext     string `json:"ciphertext"`
			Nonce          string `json:"nonce"`
			AssociatedData string `json:"associated_data"`
		} `json:"resource"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Notification{}, err
	}
	if envelope.Resource.Algorithm != "AEAD_AES_256_GCM" {
		return Notification{}, fmt.Errorf("unsupported wechat pay resource algorithm %q", envelope.Resource.Algorithm)
	}
	plain, err := decryptWeChatPayResource(settings.APIV3Key, envelope.Resource.AssociatedData, envelope.Resource.Nonce, envelope.Resource.Ciphertext)
	if err != nil {
		return Notification{}, err
	}
	var tx struct {
		AppID         string `json:"appid"`
		MchID         string `json:"mchid"`
		OutTradeNo    string `json:"out_trade_no"`
		TransactionID string `json:"transaction_id"`
		TradeState    string `json:"trade_state"`
		SuccessTime   string `json:"success_time"`
		Amount        struct {
			PayerTotal int64 `json:"payer_total"`
			Total      int64 `json:"total"`
		} `json:"amount"`
	}
	if err := json.Unmarshal(plain, &tx); err != nil {
		return Notification{}, err
	}
	if strings.TrimSpace(settings.AppID) != "" && tx.AppID != strings.TrimSpace(settings.AppID) {
		return Notification{}, errors.New("wechat pay app id does not match")
	}
	if strings.TrimSpace(settings.MchID) != "" && tx.MchID != strings.TrimSpace(settings.MchID) {
		return Notification{}, errors.New("wechat pay merchant id does not match")
	}
	if tx.TradeState != "SUCCESS" {
		return Notification{}, errors.New("wechat pay trade is not successful")
	}
	paidAt, _ := time.Parse(time.RFC3339, tx.SuccessTime)
	total := tx.Amount.PayerTotal
	if total <= 0 {
		total = tx.Amount.Total
	}
	if total <= 0 {
		return Notification{}, errors.New("wechat pay notification amount is invalid")
	}
	return Notification{
		Provider:            core.PaymentProviderWeChatPay,
		OutTradeNo:          tx.OutTradeNo,
		ProviderTradeNo:     tx.TransactionID,
		Status:              core.PaymentOrderPaid,
		AmountNanoUSD:       total * (core.NanoUSDPerUSD / 100),
		ProviderAmountCents: total,
		ProviderCurrency:    "CNY",
		PaidAt:              paidAt,
	}, nil
}

func verifyWeChatPaySignature(headers http.Header, body []byte, settings core.WeChatPaySettings) error {
	if strings.TrimSpace(settings.WeChatPayPublicKeyPEM) == "" {
		return errors.New("wechat pay public key is required")
	}
	signature := headers.Get("Wechatpay-Signature")
	timestamp := headers.Get("Wechatpay-Timestamp")
	nonce := headers.Get("Wechatpay-Nonce")
	if signature == "" || timestamp == "" || nonce == "" {
		return errors.New("missing wechat pay signature headers")
	}
	if settings.WeChatPayPublicKeyID != "" {
		serial := strings.TrimSpace(headers.Get("Wechatpay-Serial"))
		if serial == "" || serial != settings.WeChatPayPublicKeyID {
			return errors.New("wechat pay public key id does not match")
		}
	}
	publicKey, err := parseRSAPublicKey(settings.WeChatPayPublicKeyPEM)
	if err != nil {
		return err
	}
	message := timestamp + "\n" + nonce + "\n" + string(body) + "\n"
	digest := sha256.Sum256([]byte(message))
	decodedSig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return err
	}
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], decodedSig); err != nil {
		return err
	}
	return nil
}

func decryptWeChatPayResource(apiV3Key, associatedData, nonce, ciphertext string) ([]byte, error) {
	block, err := aes.NewCipher([]byte(strings.TrimSpace(apiV3Key)))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	cipherBytes, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, []byte(nonce), cipherBytes, []byte(associatedData))
}

func signAlipayValues(values url.Values, privateKey *rsa.PrivateKey) (string, error) {
	digest := sha256.Sum256([]byte(alipayCanonicalValues(values, true)))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(signature), nil
}

func validateAlipaySignType(signType string) error {
	if strings.TrimSpace(signType) == "" || strings.EqualFold(strings.TrimSpace(signType), "RSA2") {
		return nil
	}
	return fmt.Errorf("unsupported alipay sign type %q", signType)
}

func alipayCanonicalValues(values url.Values, omitSign bool) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		if omitSign && key == "sign" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value := values.Get(key)
		if value == "" {
			continue
		}
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, "&")
}

func parseRSAPrivateKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemText)))
	if block == nil {
		return nil, errors.New("invalid rsa private key pem")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not rsa")
	}
	return rsaKey, nil
}

func parseRSAPublicKey(pemText string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemText)))
	if block == nil {
		return nil, errors.New("invalid rsa public key pem")
	}
	if key, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PublicKey); ok {
			return rsaKey, nil
		}
	}
	key, err := x509.ParsePKCS1PublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	return key, nil
}

func nanoUSDToCents(nanoUSD int64) int64 {
	if nanoUSD <= 0 {
		return 0
	}
	return nanoUSD / (core.NanoUSDPerUSD / 100)
}

func providerOrderCents(order core.PaymentOrder) int64 {
	if order.ProviderAmountCents > 0 {
		return order.ProviderAmountCents
	}
	return nanoUSDToCents(order.AmountNanoUSD)
}

func providerCentsDecimal(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

func randomNonce() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return base64.RawURLEncoding.EncodeToString(buf[:])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
