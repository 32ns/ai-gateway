package payments

import (
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
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	personalpay "personalpay/sdk-go"
)

func TestSummarizePersonalPayRuntimeCountsOnlyOnlineDevices(t *testing.T) {
	summary := summarizePersonalPayRuntime([]personalpay.Device{
		{ID: "android-online", Online: true},
		{ID: "android-offline", Online: false},
	}, []personalpay.Account{
		{ID: "wechat-1", Channel: personalpay.ChannelWeChat, Status: personalpay.AccountIdle},
		{ID: "wechat-2", Channel: personalpay.ChannelWeChat, Status: personalpay.AccountOffline},
	})

	if summary.DeviceCount != 1 || summary.AccountCount != 1 || summary.IdleAccountCount != 1 || summary.OfflineCount != 1 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestCreatePersonalPayOrderWaitsForQRCode(t *testing.T) {
	previousTimeout := personalPayQRCodeWaitTimeout
	previousEvery := personalPayQRCodePollEvery
	personalPayQRCodeWaitTimeout = time.Second
	personalPayQRCodePollEvery = 10 * time.Millisecond
	defer func() {
		personalPayQRCodeWaitTimeout = previousTimeout
		personalPayQRCodePollEvery = previousEvery
	}()

	engine := &fakePersonalPayEngine{
		created: personalpay.Order{
			ID:              "pay_1",
			MerchantOrderID: "pay_1",
			Status:          personalpay.OrderQRRequested,
			AmountCents:     15,
		},
		latest: personalpay.Order{
			ID:              "pay_1",
			MerchantOrderID: "pay_1",
			Status:          personalpay.OrderQRReady,
			PaymentStatus:   personalpay.OrderQRReady,
			AmountCents:     15,
			QRURL:           "weixin://wxpay/bizpayurl?pr=ready",
		},
	}
	client := NewClient(nil).SetPersonalPayEngine(engine)

	result, err := client.createPersonalPayOrder(context.Background(), CreateOrderInput{
		Order: core.PaymentOrder{
			ID:                  "pay_1",
			OutTradeNo:          "pay_1",
			Provider:            core.PaymentProviderPersonalPay,
			Channel:             core.PaymentChannelWeChat,
			ProviderAmountCents: 15,
			AmountNanoUSD:       150 * (core.NanoUSDPerUSD / 100),
		},
		Settings: core.SystemPaymentSettings{
			PersonalPay: core.PersonalPaySettings{Enabled: true, AndroidToken: "android-token"},
		},
	})
	if err != nil {
		t.Fatalf("createPersonalPayOrder returned error: %v", err)
	}
	if result.Order.CodeURL != engine.latest.QRURL || result.Payload["code_url"] != engine.latest.QRURL {
		t.Fatalf("code url not returned: order=%#v payload=%#v", result.Order, result.Payload)
	}
	if engine.getOrderCalls == 0 {
		t.Fatal("expected create to poll for async QR result")
	}
}

func TestPersonalPayOrderEventTreatsLatePaidAsPaid(t *testing.T) {
	paidAt := time.Now().UTC()
	event := PersonalPayOrderEvent(personalpay.Order{
		ID:               "pp_order_late_paid",
		MerchantOrderID:  "out_late_paid",
		ProviderTradeKey: "pp_trade_late_paid",
		Status:           personalpay.OrderLatePaid,
		PaymentStatus:    personalpay.OrderLatePaid,
		AmountCents:      9900,
		PaidAt:           &paidAt,
	}, "{}")

	if event.Status != core.PaymentOrderPaid || event.ProviderTradeNo != "pp_trade_late_paid" || event.ProviderAmountCents != 9900 || event.PaidAt.IsZero() {
		t.Fatalf("event = %#v, want paid late payment event", event)
	}
}

func TestVerifyAlipayNotification(t *testing.T) {
	privateKey := testRSAKey(t)
	values := url.Values{}
	values.Set("app_id", "app_1")
	values.Set("out_trade_no", "pay_1")
	values.Set("trade_no", "trade_1")
	values.Set("trade_status", "TRADE_SUCCESS")
	values.Set("total_amount", "12.34")
	values.Set("gmt_payment", "2026-05-03 12:00:00")
	values.Set("sign_type", "RSA2")
	sign, err := signAlipayValues(values, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	values.Set("sign", sign)

	notification, err := VerifyAlipayNotification(values, publicKeyPEM(t, &privateKey.PublicKey))
	if err != nil {
		t.Fatalf("VerifyAlipayNotification returned error: %v", err)
	}
	if notification.Provider != core.PaymentProviderAlipay || notification.OutTradeNo != "pay_1" || notification.ProviderTradeNo != "trade_1" {
		t.Fatalf("notification = %#v", notification)
	}
	if notification.AmountNanoUSD != 12*core.NanoUSDPerUSD+34*(core.NanoUSDPerUSD/100) {
		t.Fatalf("amount = %d", notification.AmountNanoUSD)
	}
}

func TestVerifyAlipayNotificationRejectsMismatchedAppID(t *testing.T) {
	privateKey := testRSAKey(t)
	values := url.Values{}
	values.Set("app_id", "app_1")
	values.Set("out_trade_no", "pay_1")
	values.Set("trade_no", "trade_1")
	values.Set("trade_status", "TRADE_SUCCESS")
	values.Set("total_amount", "12.34")
	values.Set("sign_type", "RSA2")
	sign, err := signAlipayValues(values, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	values.Set("sign", sign)

	if _, err := VerifyAlipayNotificationWithSettings(values, core.AlipaySettings{
		AppID:              "other_app",
		AlipayPublicKeyPEM: publicKeyPEM(t, &privateKey.PublicKey),
	}); err == nil {
		t.Fatal("expected mismatched alipay app id to be rejected")
	}
}

func TestVerifyAlipayNotificationRejectsUnsupportedSignType(t *testing.T) {
	values := url.Values{}
	values.Set("sign_type", "RSA")
	values.Set("sign", "invalid")

	if _, err := VerifyAlipayNotification(values, "invalid"); err == nil {
		t.Fatal("expected unsupported alipay sign type to be rejected")
	}
}

func TestVerifyAlipayNotificationRejectsZeroAmount(t *testing.T) {
	privateKey := testRSAKey(t)
	values := url.Values{}
	values.Set("app_id", "app_1")
	values.Set("out_trade_no", "pay_1")
	values.Set("trade_no", "trade_1")
	values.Set("trade_status", "TRADE_SUCCESS")
	values.Set("total_amount", "0")
	values.Set("sign_type", "RSA2")
	sign, err := signAlipayValues(values, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	values.Set("sign", sign)

	if _, err := VerifyAlipayNotification(values, publicKeyPEM(t, &privateKey.PublicKey)); err == nil {
		t.Fatal("expected zero amount notification to be rejected")
	}
}

func TestCreateAlipayOrderSetsProductCode(t *testing.T) {
	privateKey := testRSAKey(t)
	client := NewClient(nil)
	client.now = func() time.Time {
		return time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	}

	tests := []struct {
		name        string
		channel     core.PaymentChannel
		productCode string
	}{
		{name: "page", channel: core.PaymentChannelPage, productCode: "FAST_INSTANT_TRADE_PAY"},
		{name: "wap", channel: core.PaymentChannelWAP, productCode: "QUICK_WAP_WAY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := client.createAlipayOrder(context.TODO(), CreateOrderInput{
				Order: core.PaymentOrder{
					ID:            "pay_1",
					OutTradeNo:    "pay_1",
					Provider:      core.PaymentProviderAlipay,
					Channel:       tt.channel,
					AmountNanoUSD: 12*core.NanoUSDPerUSD + 34*(core.NanoUSDPerUSD/100),
					Currency:      "CNY",
					Subject:       "Recharge",
				},
				Settings: core.SystemPaymentSettings{Alipay: core.AlipaySettings{
					Enabled:       true,
					AppID:         "app_1",
					PrivateKeyPEM: privateKeyPEM(t, privateKey),
					GatewayURL:    "https://openapi.alipay.com/gateway.do",
					NotifyURL:     "https://gateway.example.com/payments/notify/alipay",
					SignType:      "RSA2",
				}},
			})
			if err != nil {
				t.Fatalf("createAlipayOrder returned error: %v", err)
			}
			payURL, err := url.Parse(result.Order.PayURL)
			if err != nil {
				t.Fatalf("parse pay url: %v", err)
			}
			var bizContent map[string]string
			if err := json.Unmarshal([]byte(payURL.Query().Get("biz_content")), &bizContent); err != nil {
				t.Fatalf("parse biz_content: %v", err)
			}
			if bizContent["product_code"] != tt.productCode {
				t.Fatalf("product_code = %q, want %q", bizContent["product_code"], tt.productCode)
			}
			if payURL.Query().Get("sign") == "" {
				t.Fatal("missing alipay sign")
			}
		})
	}
}

func TestAlipayCanonicalValuesIncludesSignTypeAndSkipsEmptyValues(t *testing.T) {
	values := url.Values{}
	values.Set("b", "2")
	values.Set("a", "1")
	values.Set("empty", "")
	values.Set("sign_type", "RSA2")
	values.Set("sign", "signature")

	got := alipayCanonicalValues(values, true)
	want := "a=1&b=2&sign_type=RSA2"
	if got != want {
		t.Fatalf("canonical values = %q, want %q", got, want)
	}
}

type fakePersonalPayEngine struct {
	created       personalpay.Order
	latest        personalpay.Order
	getOrderCalls int
}

func (f *fakePersonalPayEngine) CreateOrder(context.Context, personalpay.CreateOrderRequest) (personalpay.Order, error) {
	return f.created, nil
}

func (f *fakePersonalPayEngine) GetOrder(context.Context, string) (personalpay.Order, error) {
	f.getOrderCalls++
	return f.latest, nil
}

func (f *fakePersonalPayEngine) CancelOrder(context.Context, string) (personalpay.Order, error) {
	return personalpay.Order{}, nil
}

func (f *fakePersonalPayEngine) ListAccounts(context.Context) []personalpay.Account {
	return nil
}

func (f *fakePersonalPayEngine) ListDevices(context.Context) []personalpay.Device {
	return nil
}

func (f *fakePersonalPayEngine) DeleteDevice(context.Context, string) error {
	return nil
}

func TestVerifyWeChatPayNotification(t *testing.T) {
	privateKey := testRSAKey(t)
	apiKey := "0123456789abcdef0123456789abcdef"
	resource := map[string]any{
		"appid":          "wx_app",
		"mchid":          "mch_1",
		"out_trade_no":   "pay_1",
		"transaction_id": "wx_trade_1",
		"trade_state":    "SUCCESS",
		"success_time":   "2026-05-03T12:00:00+08:00",
		"amount": map[string]any{
			"payer_total": 1234,
			"total":       1234,
		},
	}
	plain, err := json.Marshal(resource)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext := encryptWechatResource(t, apiKey, "transaction", "nonce1234567", plain)
	body, err := json.Marshal(map[string]any{
		"resource": map[string]string{
			"algorithm":       "AEAD_AES_256_GCM",
			"associated_data": "transaction",
			"nonce":           "nonce1234567",
			"ciphertext":      ciphertext,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	timestamp := "1777777777"
	nonce := "notify_nonce"
	message := timestamp + "\n" + nonce + "\n" + string(body) + "\n"
	digest := sha256.Sum256([]byte(message))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{}
	headers.Set("Wechatpay-Timestamp", timestamp)
	headers.Set("Wechatpay-Nonce", nonce)
	headers.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(signature))
	headers.Set("Wechatpay-Serial", "PUB_KEY_ID_1")

	notification, err := VerifyWeChatPayNotification(headers, body, core.WeChatPaySettings{
		APIV3Key:              apiKey,
		AppID:                 "wx_app",
		MchID:                 "mch_1",
		WeChatPayPublicKeyID:  "PUB_KEY_ID_1",
		WeChatPayPublicKeyPEM: publicKeyPEM(t, &privateKey.PublicKey),
	})
	if err != nil {
		t.Fatalf("VerifyWeChatPayNotification returned error: %v", err)
	}
	if notification.Provider != core.PaymentProviderWeChatPay || notification.OutTradeNo != "pay_1" || notification.ProviderTradeNo != "wx_trade_1" {
		t.Fatalf("notification = %#v", notification)
	}
	if notification.AmountNanoUSD != 12*core.NanoUSDPerUSD+34*(core.NanoUSDPerUSD/100) {
		t.Fatalf("amount = %d", notification.AmountNanoUSD)
	}
	if notification.PaidAt.IsZero() || notification.PaidAt.Location() == time.UTC {
		t.Fatalf("paid at = %v", notification.PaidAt)
	}
}

func TestVerifyWeChatPayNotificationRequiresPublicKey(t *testing.T) {
	if _, err := VerifyWeChatPayNotification(http.Header{}, []byte(`{}`), core.WeChatPaySettings{}); err == nil || !strings.Contains(err.Error(), "public key") {
		t.Fatalf("VerifyWeChatPayNotification err = %v, want public key requirement", err)
	}
}

func TestVerifyWeChatPayNotificationRejectsMismatchedMerchant(t *testing.T) {
	privateKey := testRSAKey(t)
	apiKey := "0123456789abcdef0123456789abcdef"
	resource := map[string]any{
		"appid":          "wx_app",
		"mchid":          "mch_1",
		"out_trade_no":   "pay_1",
		"transaction_id": "wx_trade_1",
		"trade_state":    "SUCCESS",
		"success_time":   "2026-05-03T12:00:00+08:00",
		"amount":         map[string]any{"payer_total": 1234},
	}
	plain, err := json.Marshal(resource)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"resource": map[string]string{
			"associated_data": "transaction",
			"nonce":           "nonce1234567",
			"ciphertext":      encryptWechatResource(t, apiKey, "transaction", "nonce1234567", plain),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	timestamp := "1777777777"
	nonce := "notify_nonce"
	digest := sha256.Sum256([]byte(timestamp + "\n" + nonce + "\n" + string(body) + "\n"))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{}
	headers.Set("Wechatpay-Timestamp", timestamp)
	headers.Set("Wechatpay-Nonce", nonce)
	headers.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(signature))

	if _, err := VerifyWeChatPayNotification(headers, body, core.WeChatPaySettings{
		APIV3Key:              apiKey,
		AppID:                 "wx_app",
		MchID:                 "other_mch",
		WeChatPayPublicKeyPEM: publicKeyPEM(t, &privateKey.PublicKey),
	}); err == nil {
		t.Fatal("expected mismatched wechat merchant id to be rejected")
	}
}

func TestVerifyWeChatPayNotificationRejectsMismatchedSerial(t *testing.T) {
	privateKey := testRSAKey(t)
	apiKey := "0123456789abcdef0123456789abcdef"
	resource := map[string]any{
		"out_trade_no":   "pay_1",
		"transaction_id": "wx_trade_1",
		"trade_state":    "SUCCESS",
		"success_time":   "2026-05-03T12:00:00+08:00",
		"amount":         map[string]any{"payer_total": 1234},
	}
	plain, err := json.Marshal(resource)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"resource": map[string]string{
			"associated_data": "transaction",
			"nonce":           "nonce1234567",
			"ciphertext":      encryptWechatResource(t, apiKey, "transaction", "nonce1234567", plain),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	timestamp := "1777777777"
	nonce := "notify_nonce"
	digest := sha256.Sum256([]byte(timestamp + "\n" + nonce + "\n" + string(body) + "\n"))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{}
	headers.Set("Wechatpay-Timestamp", timestamp)
	headers.Set("Wechatpay-Nonce", nonce)
	headers.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(signature))
	headers.Set("Wechatpay-Serial", "OTHER")

	if _, err := VerifyWeChatPayNotification(headers, body, core.WeChatPaySettings{
		APIV3Key:              apiKey,
		WeChatPayPublicKeyID:  "PUB_KEY_ID_1",
		WeChatPayPublicKeyPEM: publicKeyPEM(t, &privateKey.PublicKey),
	}); err == nil {
		t.Fatal("expected mismatched wechat serial to be rejected")
	}
}

func TestVerifyWeChatPayNotificationRejectsUnsupportedAlgorithm(t *testing.T) {
	privateKey := testRSAKey(t)
	apiKey := "0123456789abcdef0123456789abcdef"
	plain, err := json.Marshal(map[string]any{
		"appid":          "wx_app",
		"mchid":          "mch_1",
		"out_trade_no":   "pay_1",
		"transaction_id": "wx_trade_1",
		"trade_state":    "SUCCESS",
		"amount":         map[string]any{"payer_total": 1234},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"resource": map[string]string{
			"algorithm":       "UNKNOWN",
			"associated_data": "transaction",
			"nonce":           "nonce1234567",
			"ciphertext":      encryptWechatResource(t, apiKey, "transaction", "nonce1234567", plain),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	timestamp := "1777777777"
	nonce := "notify_nonce"
	digest := sha256.Sum256([]byte(timestamp + "\n" + nonce + "\n" + string(body) + "\n"))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{}
	headers.Set("Wechatpay-Timestamp", timestamp)
	headers.Set("Wechatpay-Nonce", nonce)
	headers.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(signature))

	if _, err := VerifyWeChatPayNotification(headers, body, core.WeChatPaySettings{
		APIV3Key:              apiKey,
		WeChatPayPublicKeyPEM: publicKeyPEM(t, &privateKey.PublicKey),
	}); err == nil {
		t.Fatal("expected unsupported wechat pay resource algorithm to be rejected")
	}
}

func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func publicKeyPEM(t *testing.T, publicKey *rsa.PublicKey) string {
	t.Helper()
	encoded, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: encoded}))
}

func privateKeyPEM(t *testing.T, privateKey *rsa.PrivateKey) string {
	t.Helper()
	encoded := x509.MarshalPKCS1PrivateKey(privateKey)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: encoded}))
}

func encryptWechatResource(t *testing.T, apiKey, associatedData, nonce string, plain []byte) string {
	t.Helper()
	block, err := aes.NewCipher([]byte(apiKey))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(aead.Seal(nil, []byte(nonce), plain, []byte(associatedData)))
}
