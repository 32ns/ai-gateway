package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestSendVerificationEmailCloudMail(t *testing.T) {
	var loginSeen bool
	var accountListSeen bool
	var sendSeen bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/login":
			loginSeen = true
			var payload struct {
				Email    string `json:"email"`
				Password string `json:"password"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode login payload: %v", err)
			}
			if payload.Email != "mail@example.com" || payload.Password != "secret" {
				t.Fatalf("login payload = %#v", payload)
			}
			_, _ = w.Write([]byte(`{"code":200,"message":"success","data":{"token":"token-123"}}`))
		case "/api/account/list":
			accountListSeen = true
			if got := r.Header.Get("Authorization"); got != "token-123" {
				t.Fatalf("Authorization = %q", got)
			}
			_, _ = w.Write([]byte(`{"code":200,"message":"success","data":[{"accountId":2,"email":"mail@example.com"}]}`))
		case "/api/email/send":
			sendSeen = true
			if got := r.Header.Get("Authorization"); got != "token-123" {
				t.Fatalf("Authorization = %q", got)
			}
			var payload struct {
				AccountID    int      `json:"accountId"`
				Name         string   `json:"name"`
				ReceiveEmail []string `json:"receiveEmail"`
				Subject      string   `json:"subject"`
				Text         string   `json:"text"`
				Content      string   `json:"content"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode send payload: %v", err)
			}
			if payload.AccountID != 2 || payload.Name != "Gateway" {
				t.Fatalf("send identity = %#v", payload)
			}
			if len(payload.ReceiveEmail) != 1 || payload.ReceiveEmail[0] != "alice@example.com" {
				t.Fatalf("receiveEmail = %#v", payload.ReceiveEmail)
			}
			if payload.Subject != "注册验证码 123456" || payload.Text != "alice@example.com 的验证码是 123456, 10 分钟内有效。" || payload.Content != "<p>alice@example.com 的验证码是 <strong>123456</strong></p>" {
				t.Fatalf("send content = %#v", payload)
			}
			_, _ = w.Write([]byte(`{"code":200,"message":"success","data":[{"emailId":1}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	settings := core.SystemEmailSettings{
		Provider:                    core.EmailProviderCloudMail,
		CloudMailBaseURL:            server.URL,
		CloudMailEmail:              "mail@example.com",
		CloudMailPassword:           "secret",
		CloudMailAccountID:          2,
		FromName:                    "Gateway",
		VerificationSubjectTemplate: "注册验证码 {{code}}",
		VerificationTextTemplate:    "{{email}} 的验证码是 {{code}}, {{minutes}} 分钟内有效。",
		VerificationHTMLTemplate:    "<p>{{email}} 的验证码是 <strong>{{code}}</strong></p>",
		CodeTTLSeconds:              600,
	}
	if err := sendVerificationEmail(context.Background(), settings, "alice@example.com", "123456"); err != nil {
		t.Fatalf("sendVerificationEmail returned error: %v", err)
	}
	if !loginSeen || !accountListSeen || !sendSeen {
		t.Fatalf("loginSeen=%v accountListSeen=%v sendSeen=%v", loginSeen, accountListSeen, sendSeen)
	}
}

func TestSendVerificationEmailCloudMailResolvesAccountIDByEmail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/login":
			_, _ = w.Write([]byte(`{"code":200,"data":{"token":"token-123"}}`))
		case "/api/account/list":
			_, _ = w.Write([]byte(`{"code":200,"data":[{"accountId":7,"email":"mail@example.com"}]}`))
		case "/api/email/send":
			var payload struct {
				AccountID int `json:"accountId"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode send payload: %v", err)
			}
			if payload.AccountID != 7 {
				t.Fatalf("accountId = %d, want resolved account id 7", payload.AccountID)
			}
			_, _ = w.Write([]byte(`{"code":200}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	settings := core.SystemEmailSettings{
		Provider:           core.EmailProviderCloudMail,
		CloudMailBaseURL:   server.URL,
		CloudMailEmail:     "mail@example.com",
		CloudMailPassword:  "secret",
		CloudMailAccountID: 1,
		CodeTTLSeconds:     600,
	}
	if err := sendVerificationEmail(context.Background(), settings, "alice@example.com", "123456"); err != nil {
		t.Fatalf("sendVerificationEmail returned error: %v", err)
	}
}

func TestValidateEmailVerificationSettingsChoosesProvider(t *testing.T) {
	if err := validateEmailVerificationSettings(core.SystemEmailSettings{
		Provider:           core.EmailProviderCloudMail,
		CloudMailBaseURL:   "https://mail.example.com",
		CloudMailEmail:     "mail@example.com",
		CloudMailPassword:  "secret",
		CloudMailAccountID: 2,
	}); err != nil {
		t.Fatalf("CloudMail settings validation returned error: %v", err)
	}
	if err := validateEmailVerificationSettings(core.SystemEmailSettings{
		Provider: core.EmailProviderSMTP,
	}); err == nil {
		t.Fatal("SMTP settings validation returned nil error for missing SMTP config")
	}
}
