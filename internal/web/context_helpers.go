package web

import (
	"context"
	"net/http"
	"strings"

	"github.com/32ns/ai-gateway/internal/core"
)

func withConsoleCSRFToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, consoleCSRFContextKey{}, token)
}

func withConsoleUser(ctx context.Context, user core.User) context.Context {
	return context.WithValue(ctx, consoleUserContextKey{}, user)
}

func withSiteMessageUnreadCount(ctx context.Context, count int) context.Context {
	return context.WithValue(ctx, siteMessageUnreadCountContextKey{}, count)
}

func withSupportUnreadCount(ctx context.Context, count int) context.Context {
	return context.WithValue(ctx, supportUnreadCountContextKey{}, count)
}

func withUnreadPopupSiteMessages(ctx context.Context, deliveries []core.SiteMessageDelivery) context.Context {
	return context.WithValue(ctx, siteMessagePopupDeliveriesContextKey{}, deliveries)
}

func consoleUsernameFromContext(ctx context.Context) string {
	user, ok := currentUserFromContext(ctx)
	if !ok {
		return ""
	}
	return strings.TrimSpace(user.Username)
}

func currentUserFromContext(ctx context.Context) (core.User, bool) {
	user, ok := ctx.Value(consoleUserContextKey{}).(core.User)
	return user, ok && user.ID != ""
}

func consoleCSRFTokenFromContext(ctx context.Context) string {
	token, _ := ctx.Value(consoleCSRFContextKey{}).(string)
	return token
}

func withCSRFData(data map[string]any, r *http.Request) map[string]any {
	data["CSRFToken"] = consoleCSRFTokenFromContext(r.Context())
	data["NoticeError"] = strings.TrimSpace(r.URL.Query().Get("notice_error"))
	if !profilePagePath(r.URL.Path) {
		locale := localeEN
		if value, ok := data["Locale"].(string); ok && strings.TrimSpace(value) != "" {
			locale = value
		} else {
			locale = detectLocaleFromHeaders(r.Header.Get("Accept-Language"))
		}
		if passwordSavedGlobalNotice(r) {
			data["GlobalNotice"] = translate(locale, "password_saved")
		} else if notice := passwordOAuthNotice(r, locale); notice != "" {
			data["GlobalNotice"] = notice
		}
		data["GlobalOAuthError"] = strings.TrimSpace(r.URL.Query().Get("oauth_error"))
	}
	if user, ok := currentUserFromContext(r.Context()); ok {
		data["CurrentUser"] = user
	}
	if count, ok := r.Context().Value(siteMessageUnreadCountContextKey{}).(int); ok {
		data["UnreadSiteMessages"] = count
	}
	if count, ok := r.Context().Value(supportUnreadCountContextKey{}).(int); ok {
		data["UnreadSupportMessages"] = count
	}
	if deliveries, ok := r.Context().Value(siteMessagePopupDeliveriesContextKey{}).([]core.SiteMessageDelivery); ok {
		data["UnreadPopupSiteMessages"] = deliveries
	}
	return data
}

func profilePagePath(path string) bool {
	return path == "/profile/password" || path == "/profile/oauth"
}

func passwordSavedGlobalNotice(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	query := r.URL.Query()
	return r.URL.Path == "/" && query.Get("saved") == "1" && query.Get("open_modal") == modalPassword
}

func maskToken(account core.Account) string {
	return maskSecret(account.Credential.AccessToken)
}

func withProtocolClient(ctx context.Context, client *core.APIClient) context.Context {
	return context.WithValue(ctx, protocolClientContextKey{}, client)
}

func protocolClientPointerFromContext(ctx context.Context) *core.APIClient {
	client, _ := ctx.Value(protocolClientContextKey{}).(*core.APIClient)
	return client
}

func maskSecret(token string) string {
	if len(token) <= 8 {
		if token == "" {
			return "empty"
		}
		return "****"
	}
	return token[:4] + "..." + token[len(token)-4:]
}
