package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/netproxy"
)

const loginOAuthStateCookieName = "ag_login_oauth_state"

type loginOAuthProviderConfig struct {
	Provider     string
	ClientID     string
	ClientSecret string
	AuthURL      string
	TokenURL     string
	UserURL      string
	EmailURL     string
	Scope        string
}

type loginOAuthProfile struct {
	Provider      string
	Subject       string
	Email         string
	EmailVerified bool
	Username      string
}

func (s *Server) handleLoginOAuth(w http.ResponseWriter, r *http.Request) {
	provider, callback := parseLoginOAuthPath(r.URL.Path)
	if provider == "" {
		http.NotFound(w, r)
		return
	}
	switch {
	case !callback && r.Method == http.MethodGet:
		s.handleLoginOAuthStart(w, r, provider)
	case callback && r.Method == http.MethodGet:
		if profileOAuthStateMatchesRequest(r, provider) {
			s.handleProfileOAuthCallback(w, r, provider)
			return
		}
		s.handleLoginOAuthCallback(w, r, provider)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func parseLoginOAuthPath(path string) (string, bool) {
	rest := strings.Trim(strings.TrimPrefix(path, "/login/oauth/"), "/")
	switch rest {
	case "github", "google":
		return rest, false
	case "github/callback":
		return "github", true
	case "google/callback":
		return "google", true
	default:
		return "", false
	}
}

func (s *Server) handleLoginOAuthStart(w http.ResponseWriter, r *http.Request, provider string) {
	if user, err := s.currentUserFromSession(r); err == nil && user.ID != "" {
		next := sanitizeNextPath(r.URL.Query().Get("next"), user)
		redirectLocalSeeOther(w, r, next)
		return
	}
	config, err := s.loginOAuthProvider(provider)
	if err != nil {
		s.redirectLoginOAuthError(w, r, err.Error())
		return
	}
	state, err := randomLoginOAuthState()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	next := sanitizeNextPath(r.URL.Query().Get("next"), core.User{Role: core.UserRoleUser, Enabled: true})
	http.SetCookie(w, &http.Cookie{
		Name:     loginOAuthStateCookieName,
		Value:    provider + "|" + state + "|" + base64.RawURLEncoding.EncodeToString([]byte(next)),
		Path:     "/login/oauth/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		MaxAge:   600,
	})
	values := url.Values{}
	values.Set("client_id", config.ClientID)
	values.Set("redirect_uri", s.loginOAuthRedirectURI(r, provider))
	values.Set("response_type", "code")
	values.Set("scope", config.Scope)
	values.Set("state", state)
	if provider == "google" {
		values.Set("access_type", "online")
		values.Set("prompt", "select_account")
	}
	http.Redirect(w, r, config.AuthURL+"?"+values.Encode(), http.StatusSeeOther)
}

func (s *Server) handleLoginOAuthCallback(w http.ResponseWriter, r *http.Request, provider string) {
	config, err := s.loginOAuthProvider(provider)
	if err != nil {
		s.redirectLoginOAuthError(w, r, err.Error())
		return
	}
	state, next, ok := loginOAuthStateFromCookie(r, provider)
	s.clearLoginOAuthStateCookie(w, r)
	if !ok || state == "" || state != strings.TrimSpace(r.URL.Query().Get("state")) {
		s.redirectLoginOAuthError(w, r, translate(resolveLocale(w, r), "oauth_state_invalid"))
		return
	}
	if errMessage := strings.TrimSpace(r.URL.Query().Get("error_description")); errMessage != "" {
		s.redirectLoginOAuthError(w, r, errMessage)
		return
	}
	if errCode := strings.TrimSpace(r.URL.Query().Get("error")); errCode != "" {
		s.redirectLoginOAuthError(w, r, errCode)
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		s.redirectLoginOAuthError(w, r, translate(resolveLocale(w, r), "oauth_code_missing"))
		return
	}
	proxyURL := s.control.SystemProxyURL()
	token, err := exchangeLoginOAuthCode(r.Context(), config, proxyURL, s.loginOAuthRedirectURI(r, provider), code)
	if err != nil {
		s.redirectLoginOAuthError(w, r, err.Error())
		return
	}
	profile, err := fetchLoginOAuthProfile(r.Context(), config, proxyURL, token)
	if err != nil {
		s.redirectLoginOAuthError(w, r, err.Error())
		return
	}
	user, _, err := s.control.AuthenticateOAuthUser(loginOAuthUserInputFromRequest(profile, r))
	if err != nil {
		s.redirectLoginOAuthError(w, r, err.Error())
		return
	}
	sessionToken, _, err := s.control.CreateUserSession(user.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.setConsoleSessionCookie(w, r, sessionToken)
	redirectLocalSeeOther(w, r, sanitizeNextPath(next, user))
}

func (s *Server) loginOAuthProvider(provider string) (loginOAuthProviderConfig, error) {
	settings, err := s.control.GetSystemSettings()
	if err != nil {
		return loginOAuthProviderConfig{}, err
	}
	switch provider {
	case "github":
		if !settings.OAuth.GitHubLoginEnabled || settings.OAuth.GitHubLoginClientID == "" || settings.OAuth.GitHubLoginSecret == "" {
			return loginOAuthProviderConfig{}, fmt.Errorf("github login is not configured")
		}
		return loginOAuthProviderConfig{
			Provider:     "github",
			ClientID:     settings.OAuth.GitHubLoginClientID,
			ClientSecret: settings.OAuth.GitHubLoginSecret,
			AuthURL:      "https://github.com/login/oauth/authorize",
			TokenURL:     "https://github.com/login/oauth/access_token",
			UserURL:      "https://api.github.com/user",
			EmailURL:     "https://api.github.com/user/emails",
			Scope:        "read:user user:email",
		}, nil
	case "google":
		if !settings.OAuth.GoogleLoginEnabled || settings.OAuth.GoogleLoginClientID == "" || settings.OAuth.GoogleLoginSecret == "" {
			return loginOAuthProviderConfig{}, fmt.Errorf("google login is not configured")
		}
		return loginOAuthProviderConfig{
			Provider:     "google",
			ClientID:     settings.OAuth.GoogleLoginClientID,
			ClientSecret: settings.OAuth.GoogleLoginSecret,
			AuthURL:      "https://accounts.google.com/o/oauth2/v2/auth",
			TokenURL:     "https://oauth2.googleapis.com/token",
			UserURL:      "https://openidconnect.googleapis.com/v1/userinfo",
			Scope:        "openid email profile",
		}, nil
	default:
		return loginOAuthProviderConfig{}, fmt.Errorf("oauth provider is not supported")
	}
}

func loginOAuthUserInputFromRequest(profile loginOAuthProfile, r *http.Request) controlplane.OAuthUserInput {
	return controlplane.OAuthUserInput{
		Provider:                       profile.Provider,
		Subject:                        profile.Subject,
		Email:                          profile.Email,
		EmailVerified:                  profile.EmailVerified,
		Username:                       profile.Username,
		RegistrationIP:                 clientIP(r),
		RegistrationBrowserFingerprint: registrationBrowserFingerprint(r),
	}
}

func (s *Server) loginOAuthRedirectURI(r *http.Request, provider string) string {
	baseURL := strings.TrimRight(strings.TrimSpace(s.control.PublicBaseURL()), "/")
	if baseURL == "" {
		baseURL = requestOrigin(r)
	}
	return baseURL + "/login/oauth/" + provider + "/callback"
}

func exchangeLoginOAuthCode(ctx context.Context, config loginOAuthProviderConfig, proxyURL, redirectURI, code string) (string, error) {
	form := url.Values{}
	form.Set("client_id", config.ClientID)
	form.Set("client_secret", config.ClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("grant_type", "authorization_code")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client, err := loginOAuthHTTPClient(proxyURL)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("oauth token exchange failed with status %d", resp.StatusCode)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	if payload.Error != "" {
		if payload.Description != "" {
			return "", errors.New(payload.Description)
		}
		return "", errors.New(payload.Error)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return "", fmt.Errorf("oauth token response did not include an access token")
	}
	return payload.AccessToken, nil
}

func fetchLoginOAuthProfile(ctx context.Context, config loginOAuthProviderConfig, proxyURL, token string) (loginOAuthProfile, error) {
	switch config.Provider {
	case "github":
		return fetchGitHubLoginProfile(ctx, config, proxyURL, token)
	case "google":
		return fetchGoogleLoginProfile(ctx, config, proxyURL, token)
	default:
		return loginOAuthProfile{}, fmt.Errorf("oauth provider is not supported")
	}
}

func fetchGitHubLoginProfile(ctx context.Context, config loginOAuthProviderConfig, proxyURL, token string) (loginOAuthProfile, error) {
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Email string `json:"email"`
	}
	if err := getLoginOAuthJSON(ctx, config.UserURL, proxyURL, token, &user); err != nil {
		return loginOAuthProfile{}, err
	}
	email := strings.TrimSpace(user.Email)
	if email == "" {
		email = fetchGitHubPrimaryEmail(ctx, config.EmailURL, proxyURL, token)
	}
	if user.ID == 0 {
		return loginOAuthProfile{}, fmt.Errorf("github profile did not include a user id")
	}
	return loginOAuthProfile{
		Provider:      "github",
		Subject:       fmt.Sprintf("%d", user.ID),
		Email:         email,
		EmailVerified: email != "",
		Username:      user.Login,
	}, nil
}

func fetchGitHubPrimaryEmail(ctx context.Context, emailURL, proxyURL, token string) string {
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := getLoginOAuthJSON(ctx, emailURL, proxyURL, token, &emails); err != nil {
		return ""
	}
	for _, item := range emails {
		if item.Primary && item.Verified && strings.TrimSpace(item.Email) != "" {
			return strings.TrimSpace(item.Email)
		}
	}
	return ""
}

func fetchGoogleLoginProfile(ctx context.Context, config loginOAuthProviderConfig, proxyURL, token string) (loginOAuthProfile, error) {
	var user struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := getLoginOAuthJSON(ctx, config.UserURL, proxyURL, token, &user); err != nil {
		return loginOAuthProfile{}, err
	}
	if strings.TrimSpace(user.Sub) == "" {
		return loginOAuthProfile{}, fmt.Errorf("google profile did not include a subject")
	}
	return loginOAuthProfile{
		Provider:      "google",
		Subject:       user.Sub,
		Email:         user.Email,
		EmailVerified: user.EmailVerified,
		Username:      user.Name,
	}, nil
}

func getLoginOAuthJSON(ctx context.Context, target, proxyURL, token string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	client, err := loginOAuthHTTPClient(proxyURL)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("oauth profile request failed with status %d", resp.StatusCode)
	}
	return json.Unmarshal(body, dst)
}

func loginOAuthHTTPClient(proxyURL string) (*http.Client, error) {
	return netproxy.NewHTTPClient(proxyURL, 15*time.Second, 15*time.Second)
}

func randomLoginOAuthState() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func loginOAuthStateFromCookie(r *http.Request, provider string) (string, string, bool) {
	cookie, err := r.Cookie(loginOAuthStateCookieName)
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(cookie.Value, "|", 3)
	if len(parts) != 3 || parts[0] != provider {
		return "", "", false
	}
	next, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", "", false
	}
	return parts[1], string(next), true
}

func (s *Server) clearLoginOAuthStateCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     loginOAuthStateCookieName,
		Value:    "",
		Path:     "/login/oauth/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		MaxAge:   -1,
	})
}

func (s *Server) redirectLoginOAuthError(w http.ResponseWriter, r *http.Request, message string) {
	http.Redirect(w, r, "/login?notice_error="+url.QueryEscape(strings.TrimSpace(message)), http.StatusSeeOther)
}
