package web

import (
	"net/url"
	"strings"
)

const (
	modalPassword  = "password-modal"
	modalOAuthBind = "oauth-bind-modal"
)

func modalReturnPath(modalID string) string {
	modalID = strings.TrimSpace(modalID)
	if modalFallbackPath(modalID) == "" {
		return "/"
	}
	return "/?open_modal=" + url.QueryEscape(modalID)
}

func sanitizeModalReturn(raw, modalID string) string {
	fallback := modalFallbackPath(modalID)
	if fallback == "" {
		fallback = "/"
	}
	raw = strings.TrimSpace(raw)
	if raw == fallback {
		return raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.Path != "/" {
		return fallback
	}
	if parsed.Query().Get("open_modal") != modalID {
		return fallback
	}
	return modalReturnPath(modalID)
}

func modalFallbackPath(modalID string) string {
	switch strings.TrimSpace(modalID) {
	case modalPassword:
		return "/profile/password"
	case modalOAuthBind:
		return "/profile/oauth"
	default:
		return ""
	}
}

func appendURLParam(target, key, value string) string {
	parsed, err := url.Parse(strings.TrimSpace(target))
	if err != nil {
		return target
	}
	values := parsed.Query()
	values.Set(key, value)
	parsed.RawQuery = values.Encode()
	return parsed.String()
}
