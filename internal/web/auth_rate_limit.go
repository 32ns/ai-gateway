package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

const (
	loginFailureRateLimitWindow = 15 * time.Minute
	loginFailureIPLimit         = 30
	loginFailureUserLimit       = 10
)

func (s *Server) loginFailureRateLimited(r *http.Request) bool {
	if s == nil || s.loginLimiter == nil {
		return false
	}
	if s.loginLimiter.blocked("login-failure-ip", clientIP(r), loginFailureIPLimit, loginFailureRateLimitWindow) {
		return true
	}
	username := loginRateLimitUsername(r.FormValue("username"))
	if username == "" {
		return false
	}
	return s.loginLimiter.blocked("login-failure-user", loginFailureUserKey(r, username), loginFailureUserLimit, loginFailureRateLimitWindow)
}

func (s *Server) recordLoginFailure(r *http.Request) bool {
	if s == nil || s.loginLimiter == nil {
		return true
	}
	allowedIP := s.loginLimiter.allow("login-failure-ip", clientIP(r), loginFailureIPLimit, loginFailureRateLimitWindow)
	username := loginRateLimitUsername(r.FormValue("username"))
	if username == "" {
		return allowedIP
	}
	allowedUser := s.loginLimiter.allow("login-failure-user", loginFailureUserKey(r, username), loginFailureUserLimit, loginFailureRateLimitWindow)
	return allowedIP && allowedUser
}

func (s *Server) clearLoginFailures(r *http.Request) {
	if s == nil || s.loginLimiter == nil {
		return
	}
	username := loginRateLimitUsername(r.FormValue("username"))
	if username == "" {
		return
	}
	s.loginLimiter.reset("login-failure-user", loginFailureUserKey(r, username))
}

func (s *Server) redirectLoginNotice(w http.ResponseWriter, r *http.Request, messageKey string) {
	next := sanitizeNextPath(r.FormValue("next"), core.User{Role: core.UserRoleAdmin, Enabled: true})
	http.Redirect(w, r, loginRedirectWithNotice(next, translate(resolveLocale(w, r), messageKey)), http.StatusSeeOther)
}

func loginFailureUserKey(r *http.Request, username string) string {
	return clientIP(r) + "\x00" + loginRateLimitUsername(username)
}

func loginRateLimitUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}
