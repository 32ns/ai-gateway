package web

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
)

func (s *Server) requireConsoleUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := s.currentUserFromSession(r)
		if err != nil {
			s.writeConsoleAuthRequired(w, r)
			return
		}

		csrfToken, err := s.ensureConsoleCSRFToken(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if isStateChangingMethod(r.Method) {
			if isMultipartFormRequest(r) {
				defer cleanupMultipartForm(r)
			}
			submitted, err := readConsoleCSRFToken(w, r)
			if err != nil {
				s.handleConsoleCSRFError(w, r)
				return
			}
			if submitted != csrfToken {
				s.handleConsoleCSRFError(w, r)
				return
			}
		}

		ctx := withConsoleCSRFToken(r.Context(), csrfToken)
		ctx = withConsoleUser(ctx, user)
		ctx = withSiteMessageUnreadCount(ctx, s.control.SiteMessageUnreadCount(user))
		ctx = withSupportUnreadCount(ctx, s.control.SupportUnreadCount(user))
		if !user.ForcePasswordChange {
			ctx = withUnreadPopupSiteMessages(ctx, s.control.ListUnreadPopupSiteMessages(user, 5))
		}
		if user.ForcePasswordChange && !passwordChangeAllowedPath(r.URL.Path) {
			message := translate(resolveLocale(w, r), "password_change_required")
			if wantsJSON(r) {
				writeJSON(w, http.StatusForbidden, map[string]string{"status": "error", "message": message})
				return
			}
			http.Redirect(w, r, appendNoticeError("/profile/password", message), http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) requireAdminOnly(next http.Handler) http.Handler {
	return s.requireConsoleUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := currentUserFromContext(r.Context())
		if !ok || !user.IsAdmin() {
			s.writeAdminRequired(w, r)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (s *Server) requireImageLabAccess(next http.Handler) http.Handler {
	return s.requireConsoleUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := currentUserFromContext(r.Context())
		if !ok || !s.imageLabVisibleForUser(user) {
			s.writeImageLabDisabled(w, r)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (s *Server) currentUserFromSession(r *http.Request) (core.User, error) {
	cookie, err := r.Cookie(consoleSessionCookieName)
	if err != nil {
		return core.User{}, err
	}
	return s.control.UserBySessionToken(cookie.Value)
}

func (s *Server) writeConsoleAuthRequired(w http.ResponseWriter, r *http.Request) {
	message := translate(resolveLocale(w, r), "auth_required")
	if wantsJSON(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"status": "error", "message": message})
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		http.Redirect(w, r, loginRedirectWithNotice(loginNextPath(r), message), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, loginRedirectWithNotice(consoleCSRFReturnPath(r), message), http.StatusSeeOther)
}

func (s *Server) writeAdminRequired(w http.ResponseWriter, r *http.Request) {
	message := translate(resolveLocale(w, r), "admin_required")
	if wantsJSON(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"status": "error", "message": message})
		return
	}
	http.Redirect(w, r, appendNoticeError("/", message), http.StatusSeeOther)
}

func (s *Server) writeImageLabDisabled(w http.ResponseWriter, r *http.Request) {
	message := translate(resolveLocale(w, r), "image_lab_disabled")
	if wantsJSON(r) || strings.HasPrefix(r.URL.Path, "/images/api/") {
		writeJSON(w, http.StatusForbidden, imageLabErrorResponse{OK: false, Type: "forbidden", Message: message, Status: http.StatusForbidden})
		return
	}
	http.Error(w, message, http.StatusForbidden)
}

func passwordChangeAllowedPath(path string) bool {
	switch path {
	case "/profile/password", "/logout":
		return true
	default:
		return false
	}
}

func loginRedirectWithNotice(next, message string) string {
	values := url.Values{}
	values.Set("next", next)
	values.Set("notice_error", strings.TrimSpace(message))
	return "/login?" + values.Encode()
}

func (s *Server) handleConsoleCSRFError(w http.ResponseWriter, r *http.Request) {
	message := translate(resolveLocale(w, r), "csrf_invalid")
	if wantsJSON(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"status": "error", "message": message})
		return
	}
	redirectLocalSeeOther(w, r, appendNoticeError(consoleCSRFReturnPath(r), message))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/login" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if user, err := s.currentUserFromSession(r); err == nil && user.ID != "" {
			redirectLocalSeeOther(w, r, sanitizeNextPath(r.URL.Query().Get("next"), user))
			return
		}
		s.renderLoginPage(w, r, "", "", http.StatusOK)
	case http.MethodPost:
		if isMultipartFormRequest(r) {
			defer cleanupMultipartForm(r)
		}
		csrfToken, err := s.ensureConsoleCSRFToken(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		submitted, err := readConsoleCSRFToken(w, r)
		if err != nil {
			s.redirectLoginNotice(w, r, "csrf_invalid")
			return
		}
		if submitted != csrfToken {
			s.redirectLoginNotice(w, r, "csrf_invalid")
			return
		}
		if s.loginFailureRateLimited(r) {
			s.redirectLoginNotice(w, r, "login_rate_limited")
			return
		}
		user, err := s.control.AuthenticateUser(r.FormValue("username"), r.FormValue("password"))
		if err != nil {
			if !s.recordLoginFailure(r) {
				s.redirectLoginNotice(w, r, "login_rate_limited")
				return
			}
			s.redirectLoginNotice(w, r, "login_failed")
			return
		}
		s.clearLoginFailures(r)
		token, _, err := s.control.CreateUserSession(user.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.setConsoleSessionCookie(w, r, token)
		redirectLocalSeeOther(w, r, sanitizeNextPath(r.FormValue("next"), user))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/register" {
		http.NotFound(w, r)
		return
	}
	if !s.registrationAllowedForRequest(r) {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if user, err := s.currentUserFromSession(r); err == nil && user.ID != "" {
			http.Redirect(w, r, sanitizeNextPath("", user), http.StatusSeeOther)
			return
		}
		s.renderRegisterPage(w, r, "", http.StatusOK)
	case http.MethodPost:
		settings := s.controlCurrentSettings()
		locale := resolveLocale(w, r)
		if isMultipartFormRequest(r) {
			defer cleanupMultipartForm(r)
		}
		csrfToken, err := s.ensureConsoleCSRFToken(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		submitted, err := readConsoleCSRFToken(w, r)
		if err != nil {
			s.redirectRegisterError(w, r, translate(resolveLocale(w, r), "csrf_invalid"))
			return
		}
		if submitted != csrfToken {
			s.redirectRegisterError(w, r, translate(resolveLocale(w, r), "csrf_invalid"))
			return
		}
		if !s.allowRegistrationIPRequest(r, "register", settings.Registration.RegisterIPHourlyLimit) {
			s.renderRegisterPage(w, r, translate(locale, "registration_rate_limited"), http.StatusTooManyRequests)
			return
		}
		if r.FormValue("password") != r.FormValue("password_confirm") {
			s.redirectRegisterError(w, r, translate(resolveLocale(w, r), "register_password_mismatch"))
			return
		}
		if err := controlplane.ValidateUsername(r.FormValue("username")); err != nil {
			if errors.Is(err, controlplane.ErrInvalidUsernameCharacters) {
				s.redirectRegisterError(w, r, translate(locale, "username_allowed_characters_hint"))
				return
			}
			s.redirectRegisterError(w, r, err.Error())
			return
		}
		if err := validateRegisterUsernameLength(r.FormValue("username"), settings.Registration.UsernameMinLength); err != nil {
			s.redirectRegisterError(w, r, translatef(locale, "register_username_too_short", settings.Registration.UsernameMinLength))
			return
		}
		email := registrationEmailFromRequest(r)
		if err := s.control.ValidateRegistrationEmail(email); err != nil {
			s.redirectRegisterError(w, r, err.Error())
			return
		}
		if err := s.verifyRegistrationChallenge(r, settings); err != nil {
			s.redirectRegisterError(w, r, translate(locale, "captcha_verification_failed"))
			return
		}
		emailVerified := false
		if s.control.EmailVerificationRequiredForRegistration() {
			if err := s.control.VerifyEmailCode(controlplane.EmailVerificationPurposeRegister, email, r.FormValue("email_code")); err != nil {
				s.redirectRegisterError(w, r, err.Error())
				return
			}
			emailVerified = true
		}
		result, err := s.control.CreateInvitedUser(controlplane.UserInput{
			Username:                       r.FormValue("username"),
			Password:                       r.FormValue("password"),
			Role:                           core.UserRoleUser,
			Enabled:                        true,
			Email:                          email,
			EmailVerified:                  emailVerified,
			RegistrationIP:                 clientIP(r),
			RegistrationBrowserFingerprint: registrationBrowserFingerprint(r),
		}, r.FormValue("invite_code"))
		if err != nil {
			s.redirectRegisterError(w, r, err.Error())
			return
		}
		user, err := s.control.RecordUserLogin(result.User.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		token, _, err := s.control.CreateUserSession(user.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.setConsoleSessionCookie(w, r, token)
		redirectLocalSeeOther(w, r, sanitizeNextPath(r.FormValue("next"), user))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleRegisterEmailCodeSend(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/register/email-code/send" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.registrationAllowedForRequest(r) {
		http.NotFound(w, r)
		return
	}
	if isMultipartFormRequest(r) {
		defer cleanupMultipartForm(r)
	}
	csrfToken, err := s.ensureConsoleCSRFToken(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	submitted, err := readConsoleCSRFToken(w, r)
	if err != nil || submitted != csrfToken {
		writeJSON(w, http.StatusForbidden, map[string]string{"status": "error", "message": translate(resolveLocale(w, r), "csrf_invalid")})
		return
	}
	settings := s.controlCurrentSettings()
	if !s.allowRegistrationIPRequest(r, "register-email-code", settings.Registration.EmailCodeIPHourlyLimit) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"status": "error", "message": translate(resolveLocale(w, r), "email_code_rate_limited")})
		return
	}
	email := registrationEmailFromRequest(r)
	if err := s.control.ValidateRegistrationEmail(email); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": err.Error()})
		return
	}
	if err := s.control.SendEmailVerificationCode(r.Context(), controlplane.EmailVerificationInput{
		Purpose: controlplane.EmailVerificationPurposeRegister,
		Email:   email,
	}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "message": translate(resolveLocale(w, r), "email_code_sent")})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/logout" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if cookie, err := r.Cookie(consoleSessionCookieName); err == nil {
		_ = s.control.DeleteUserSessionToken(cookie.Value)
	}
	s.clearConsoleSessionCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handlePasswordPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/profile/password" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.renderPasswordPage(w, r, r.URL.Query().Get("saved") == "1", "", http.StatusOK)
	case http.MethodPost:
		user, ok := currentUserFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		returnTo := sanitizeModalReturn(r.FormValue("return_to"), modalPassword)
		nextPassword := r.FormValue("new_password")
		if nextPassword != r.FormValue("new_password_confirm") {
			message := translate(resolveLocale(w, r), "password_mismatch")
			if wantsJSON(r) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": message})
				return
			}
			s.redirectWithNoticeError(w, r, returnTo, errors.New(message))
			return
		}
		if err := s.control.ChangeUserPassword(user.ID, r.FormValue("current_password"), nextPassword); err != nil {
			message := err.Error()
			if errors.Is(err, controlplane.ErrInvalidCredentials) {
				message = translate(resolveLocale(w, r), "current_password_invalid")
			}
			if wantsJSON(r) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": message})
				return
			}
			s.redirectWithNoticeError(w, r, returnTo, errors.New(message))
			return
		}
		if wantsJSON(r) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "message": translate(resolveLocale(w, r), "password_saved")})
			return
		}
		if user.ForcePasswordChange && user.IsAdmin() {
			http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
			return
		}
		redirectLocalSeeOther(w, r, appendURLParam(returnTo, "saved", "1"))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) redirectRegisterError(w http.ResponseWriter, r *http.Request, message string) {
	values := url.Values{}
	if next := sanitizeNextPath(r.FormValue("next"), core.User{Role: core.UserRoleUser, Enabled: true}); next != "/" {
		values.Set("next", next)
	}
	if invite := strings.TrimSpace(r.FormValue("invite_code")); invite != "" {
		values.Set("invite", invite)
	}
	values.Set("notice_error", strings.TrimSpace(message))
	target := "/register"
	if encoded := values.Encode(); encoded != "" {
		target += "?" + encoded
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

const registrationIPRateLimitWindow = time.Hour

func (s *Server) allowRegistrationIPRequest(r *http.Request, scope string, limit int) bool {
	if s == nil {
		return true
	}
	return s.registrationLimiter.allow(scope, clientIP(r), limit, registrationIPRateLimitWindow)
}

func (s *Server) verifyRegistrationChallenge(r *http.Request, settings core.SystemSettings) error {
	if !settings.Registration.TurnstileEnabled {
		return nil
	}
	return verifyTurnstile(r.Context(), settings.Registration.TurnstileSecretKey, r.FormValue("cf-turnstile-response"), clientIP(r))
}

func (s *Server) renderPasswordPage(w http.ResponseWriter, r *http.Request, saved bool, message string, status int) {
	locale := resolveLocale(w, r)
	user, _ := currentUserFromContext(r.Context())
	settings := core.DefaultSystemSettings()
	if s != nil && s.control != nil {
		if loaded, err := s.control.GetSystemSettings(); err == nil {
			settings = loaded
		}
	}
	data := withCSRFData(map[string]any{
		"TitleKey":       "page_title_password",
		"ActiveNav":      "",
		"Locale":         locale,
		"Saved":          saved,
		"Error":          strings.TrimSpace(message),
		"ForceChange":    user.ForcePasswordChange,
		"NoticeError":    strings.TrimSpace(r.URL.Query().Get("notice_error")),
		"OAuthNotice":    passwordOAuthNotice(r, locale),
		"OAuthError":     strings.TrimSpace(r.URL.Query().Get("oauth_error")),
		"OAuthProviders": passwordOAuthProviders(user, settings),
	}, r)
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.render(w, "password.html", locale, data)
}

type passwordOAuthProviderView struct {
	Provider   string
	Label      string
	Configured bool
	Linked     bool
	Email      string
	Username   string
}

func passwordOAuthProviders(user core.User, settings core.SystemSettings) []passwordOAuthProviderView {
	providers := []passwordOAuthProviderView{
		{
			Provider:   "github",
			Label:      "GitHub",
			Configured: settings.OAuth.GitHubLoginEnabled && strings.TrimSpace(settings.OAuth.GitHubLoginClientID) != "" && strings.TrimSpace(settings.OAuth.GitHubLoginSecret) != "",
		},
		{
			Provider:   "google",
			Label:      "Google",
			Configured: settings.OAuth.GoogleLoginEnabled && strings.TrimSpace(settings.OAuth.GoogleLoginClientID) != "" && strings.TrimSpace(settings.OAuth.GoogleLoginSecret) != "",
		},
	}
	for i := range providers {
		for _, identity := range user.OAuthIdentities {
			if !strings.EqualFold(identity.Provider, providers[i].Provider) {
				continue
			}
			providers[i].Linked = true
			providers[i].Email = strings.TrimSpace(identity.Email)
			providers[i].Username = strings.TrimSpace(identity.Username)
			break
		}
	}
	return providers
}

func passwordOAuthNotice(r *http.Request, locale string) string {
	query := r.URL.Query()
	if provider := strings.TrimSpace(query.Get("oauth_linked")); provider != "" {
		return fmt.Sprintf(translate(locale, "oauth_bind_linked"), oauthProviderLabel(provider))
	}
	if provider := strings.TrimSpace(query.Get("oauth_unlinked")); provider != "" {
		return fmt.Sprintf(translate(locale, "oauth_bind_unlinked"), oauthProviderLabel(provider))
	}
	if provider := strings.TrimSpace(query.Get("oauth_merged")); provider != "" {
		return fmt.Sprintf(translate(locale, "oauth_bind_merged"), oauthProviderLabel(provider))
	}
	return ""
}

func oauthProviderLabel(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "github":
		return "GitHub"
	case "google":
		return "Google"
	default:
		return strings.TrimSpace(provider)
	}
}

func (s *Server) renderLoginPage(w http.ResponseWriter, r *http.Request, message string, username string, status int) {
	csrfToken, err := s.ensureConsoleCSRFToken(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	locale := resolveLocale(w, r)
	next := sanitizeNextPath(r.URL.Query().Get("next"), core.User{Role: core.UserRoleAdmin, Enabled: true})
	settings := core.DefaultSystemSettings()
	if s != nil && s.control != nil {
		if loaded, err := s.control.GetSystemSettings(); err == nil {
			settings = loaded
		}
	}
	w.WriteHeader(status)
	s.render(w, "login.html", locale, map[string]any{
		"TitleKey":                "page_title_login",
		"ActiveNav":               "",
		"Locale":                  locale,
		"CSRFToken":               csrfToken,
		"Next":                    next,
		"Message":                 message,
		"Username":                strings.TrimSpace(username),
		"NoticeError":             strings.TrimSpace(r.URL.Query().Get("notice_error")),
		"AllowPublicRegistration": s.publicRegistrationAllowed(),
		"GitHubLoginEnabled":      settings.OAuth.GitHubLoginEnabled && settings.OAuth.GitHubLoginClientID != "" && settings.OAuth.GitHubLoginSecret != "",
		"GoogleLoginEnabled":      settings.OAuth.GoogleLoginEnabled && settings.OAuth.GoogleLoginClientID != "" && settings.OAuth.GoogleLoginSecret != "",
	})
}

func (s *Server) publicRegistrationAllowed() bool {
	if s == nil {
		return false
	}
	if s.control == nil {
		return s.allowPublicRegistration
	}
	settings, err := s.control.GetSystemSettings()
	if err != nil {
		return s.allowPublicRegistration
	}
	if settings.Registration.RequireInvitationCode {
		return false
	}
	return s.allowPublicRegistration || settings.Runtime.AllowPublicRegistration
}

func (s *Server) registrationAllowedForRequest(r *http.Request) bool {
	if s == nil {
		return false
	}
	if s.control == nil {
		return s.allowPublicRegistration
	}
	settings, err := s.control.GetSystemSettings()
	if err != nil {
		return s.allowPublicRegistration
	}
	if !settings.Registration.RequireInvitationCode && (s.allowPublicRegistration || settings.Runtime.AllowPublicRegistration) {
		return true
	}
	if strings.TrimSpace(registerInviteCode(r)) == "" {
		return false
	}
	if !settings.Invitation.Enabled {
		return false
	}
	inviter, err := s.control.ResolveInvitationCode(registerInviteCode(r))
	return err == nil && inviter.Enabled
}

func (s *Server) renderRegisterPage(w http.ResponseWriter, r *http.Request, message string, status int) {
	csrfToken, err := s.ensureConsoleCSRFToken(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	locale := resolveLocale(w, r)
	next := sanitizeNextPath(r.URL.Query().Get("next"), core.User{Role: core.UserRoleUser, Enabled: true})
	emailVerificationRequired := false
	emailRequired := false
	registrationEmailDomains := []string(nil)
	settings := core.DefaultSystemSettings()
	if s != nil && s.control != nil {
		if loaded, err := s.control.GetSystemSettings(); err == nil {
			settings = loaded
		}
		emailVerificationRequired = settings.Email.RegistrationVerificationEnabled
		emailRequired = settings.Email.RegistrationVerificationEnabled || settings.Runtime.RegistrationEmailAllowlistEnabled
		if settings.Runtime.RegistrationEmailAllowlistEnabled {
			registrationEmailDomains = append(registrationEmailDomains, settings.Runtime.RegistrationEmailAllowlist...)
		}
	}
	email := registrationEmailFromRequest(r)
	emailLocal, emailDomain := splitRegistrationEmail(email, registrationEmailDomains)
	w.WriteHeader(status)
	s.render(w, "register.html", locale, map[string]any{
		"TitleKey":                      "page_title_register",
		"ActiveNav":                     "",
		"Locale":                        locale,
		"CSRFToken":                     csrfToken,
		"Next":                          next,
		"InviteCode":                    registerInviteCode(r),
		"Message":                       message,
		"Username":                      strings.TrimSpace(r.FormValue("username")),
		"Email":                         email,
		"EmailLocal":                    emailLocal,
		"EmailDomain":                   emailDomain,
		"EmailRequired":                 emailRequired,
		"EmailVerificationRequired":     emailVerificationRequired,
		"EmailSendCooldownSeconds":      settings.Email.SendCooldownSeconds,
		"RegistrationEmailDomains":      registrationEmailDomains,
		"RegistrationUsernameMinLength": settings.Registration.UsernameMinLength,
		"GitHubLoginEnabled":            settings.OAuth.GitHubLoginEnabled && strings.TrimSpace(settings.OAuth.GitHubLoginClientID) != "" && strings.TrimSpace(settings.OAuth.GitHubLoginSecret) != "",
		"GoogleLoginEnabled":            settings.OAuth.GoogleLoginEnabled && strings.TrimSpace(settings.OAuth.GoogleLoginClientID) != "" && strings.TrimSpace(settings.OAuth.GoogleLoginSecret) != "",
		"TurnstileEnabled":              settings.Registration.TurnstileEnabled && strings.TrimSpace(settings.Registration.TurnstileSiteKey) != "",
		"TurnstileSiteKey":              settings.Registration.TurnstileSiteKey,
		"NoticeError":                   strings.TrimSpace(r.URL.Query().Get("notice_error")),
	})
}

func registrationEmailFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	if email := strings.TrimSpace(r.FormValue("email")); email != "" {
		return email
	}
	local := strings.TrimSpace(r.FormValue("email_local"))
	domain := strings.TrimSpace(r.FormValue("email_domain"))
	if local == "" || domain == "" {
		return ""
	}
	if !strings.HasPrefix(domain, "@") {
		domain = "@" + domain
	}
	return local + domain
}

func splitRegistrationEmail(email string, domains []string) (string, string) {
	email = strings.TrimSpace(email)
	if email == "" {
		if len(domains) > 0 {
			return "", domains[0]
		}
		return "", ""
	}
	lowerEmail := strings.ToLower(email)
	for _, domain := range domains {
		normalized := strings.ToLower(strings.TrimSpace(domain))
		if normalized == "" {
			continue
		}
		if strings.HasSuffix(lowerEmail, normalized) && len(email) > len(normalized) {
			return email[:len(email)-len(normalized)], domain
		}
	}
	if at := strings.LastIndex(email, "@"); at > 0 {
		return email[:at], email[at:]
	}
	return email, ""
}

func validateRegisterUsernameLength(username string, minLength int) error {
	if minLength <= 1 {
		return nil
	}
	if len([]rune(strings.TrimSpace(username))) < minLength {
		return fmt.Errorf("username must be at least %d characters", minLength)
	}
	return nil
}

func registrationBrowserFingerprint(r *http.Request) string {
	if r == nil {
		return ""
	}
	if value := normalizeRegistrationFingerprint(r.FormValue("browser_fingerprint")); value != "" {
		return value
	}
	parts := []string{
		strings.TrimSpace(r.Header.Get("User-Agent")),
		strings.TrimSpace(r.Header.Get("Accept-Language")),
		strings.TrimSpace(r.Header.Get("Accept-Encoding")),
	}
	joined := strings.Join(parts, "\n")
	if strings.TrimSpace(joined) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(joined))
	return hex.EncodeToString(sum[:])
}

func normalizeRegistrationFingerprint(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 128 {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func registerInviteCode(r *http.Request) string {
	if r == nil {
		return ""
	}
	if value := strings.TrimSpace(r.FormValue("invite_code")); value != "" {
		return value
	}
	if r.URL == nil {
		return ""
	}
	return strings.TrimSpace(r.URL.Query().Get("invite"))
}

func (s *Server) setConsoleSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     consoleSessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		MaxAge:   int(defaultConsoleSessionMaxAge.Seconds()),
	})
}

func (s *Server) clearConsoleSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     consoleSessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		MaxAge:   -1,
	})
}

func loginNextPath(r *http.Request) string {
	if r == nil || r.URL == nil {
		return "/"
	}
	next := r.URL.RequestURI()
	if next == "" || strings.HasPrefix(next, "/login") || strings.HasPrefix(next, "/register") {
		return "/"
	}
	return next
}

func consoleCSRFReturnPath(r *http.Request) string {
	if r == nil {
		return "/"
	}
	if referer := strings.TrimSpace(r.Header.Get("Referer")); referer != "" {
		if parsed, err := url.Parse(referer); err == nil && parsed.Path != "" {
			if parsed.IsAbs() && !strings.EqualFold(parsed.Host, r.Host) {
				return "/"
			}
			return parsed.RequestURI()
		}
	}
	if r.URL == nil {
		return "/"
	}
	path := r.URL.EscapedPath()
	switch {
	case path == "/admin/account-groups" || strings.HasPrefix(path, "/admin/account-groups/"):
		return "/admin/accounts"
	case path == "/admin/accounts/connect":
		return connectErrorTarget(core.ProviderKind(r.FormValue("provider")))
	case strings.HasPrefix(path, "/admin/accounts/"):
		return accountsGroupHref(accountGroupReturnKey(r))
	case path == "/admin/users/new" || strings.HasPrefix(path, "/admin/users/"):
		return "/admin/users"
	case path == "/admin/models/new" || strings.HasPrefix(path, "/admin/models/"):
		return "/admin/models"
	case path == "/admin/docs" || strings.HasPrefix(path, "/admin/docs/"):
		return "/admin/docs"
	case path == "/clients/new" || strings.HasPrefix(path, "/clients/"):
		return "/clients"
	case path == "/docs" || strings.HasPrefix(path, "/docs/"):
		return path
	case path == "/messages" || strings.HasPrefix(path, "/messages/"):
		return "/messages"
	case path == "/payments/refresh":
		return "/payments"
	case path == "/profile/password":
		return "/profile/password"
	case path == "/admin/settings":
		return "/admin/settings"
	default:
		return "/"
	}
}

func sanitizeNextPath(raw string, user core.User) string {
	fallback := "/"
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, `\`) {
		return fallback
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.IsAbs() || parsed.Host != "" {
		return fallback
	}
	if strings.HasPrefix(parsed.Path, "/login") || strings.HasPrefix(parsed.Path, "/logout") || strings.HasPrefix(parsed.Path, "/register") {
		return fallback
	}
	if user.ID != "" && !user.IsAdmin() {
		if parsed.Path == "/" || parsed.Path == "/clients" || strings.HasPrefix(parsed.Path, "/clients/") || parsed.Path == "/logs" || parsed.Path == "/plans" || parsed.Path == "/docs" || strings.HasPrefix(parsed.Path, "/docs/") {
			return raw
		}
		return fallback
	}
	return raw
}
