package web

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
)

const profileOAuthStateCookieName = "ag_profile_oauth_state"
const profileOAuthMergeCookieName = "ag_profile_oauth_merge"

type profileOAuthMergeState struct {
	ID        string
	Provider  string
	Subject   string
	SourceID  string
	TargetID  string
	ExpiresAt time.Time
}

func (s *Server) handleProfileOAuthPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/profile/oauth" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	locale := resolveLocale(w, r)
	user, _ := currentUserFromContext(r.Context())
	settings := core.DefaultSystemSettings()
	if s != nil && s.control != nil {
		if loaded, err := s.control.GetSystemSettings(); err == nil {
			settings = loaded
		}
	}
	data := withCSRFData(map[string]any{
		"TitleKey":       "oauth_bind_title",
		"ActiveNav":      "",
		"Locale":         locale,
		"OAuthNotice":    passwordOAuthNotice(r, locale),
		"OAuthError":     strings.TrimSpace(r.URL.Query().Get("oauth_error")),
		"OAuthProviders": passwordOAuthProviders(user, settings),
	}, r)
	s.render(w, "profile_oauth.html", locale, data)
}

func (s *Server) handleProfileOAuth(w http.ResponseWriter, r *http.Request) {
	provider, action := parseProfileOAuthPath(r.URL.Path)
	if provider == "" {
		http.NotFound(w, r)
		return
	}
	switch {
	case action == "start" && r.Method == http.MethodGet:
		s.handleProfileOAuthStart(w, r, provider)
	case action == "callback" && r.Method == http.MethodGet:
		s.handleProfileOAuthCallback(w, r, provider)
	case action == "merge" && r.Method == http.MethodGet:
		s.handleProfileOAuthMergePage(w, r, provider)
	case action == "merge" && r.Method == http.MethodPost:
		s.handleProfileOAuthMergeConfirm(w, r, provider)
	case action == "unlink" && r.Method == http.MethodPost:
		s.handleProfileOAuthUnlink(w, r, provider)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func parseProfileOAuthPath(path string) (string, string) {
	rest := strings.Trim(strings.TrimPrefix(path, "/profile/oauth/"), "/")
	switch rest {
	case "github", "google", "linuxdo":
		return rest, "start"
	case "github/callback":
		return "github", "callback"
	case "google/callback":
		return "google", "callback"
	case "linuxdo/callback":
		return "linuxdo", "callback"
	case "github/merge":
		return "github", "merge"
	case "google/merge":
		return "google", "merge"
	case "linuxdo/merge":
		return "linuxdo", "merge"
	case "github/unlink":
		return "github", "unlink"
	case "google/unlink":
		return "google", "unlink"
	case "linuxdo/unlink":
		return "linuxdo", "unlink"
	default:
		return "", ""
	}
}

func (s *Server) handleProfileOAuthStart(w http.ResponseWriter, r *http.Request, provider string) {
	if _, ok := currentUserFromContext(r.Context()); !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	config, err := s.loginOAuthProvider(provider)
	if err != nil {
		s.redirectProfileOAuthError(w, r, err.Error())
		return
	}
	state, err := randomLoginOAuthState()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	returnTo := sanitizeModalReturn(r.URL.Query().Get("return_to"), modalOAuthBind)
	http.SetCookie(w, &http.Cookie{
		Name:     profileOAuthStateCookieName,
		Value:    provider + "|" + state + "|" + base64.RawURLEncoding.EncodeToString([]byte(returnTo)),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		MaxAge:   600,
	})
	values := url.Values{}
	values.Set("client_id", config.ClientID)
	values.Set("redirect_uri", s.profileOAuthRedirectURI(r, provider))
	values.Set("response_type", "code")
	values.Set("scope", config.Scope)
	values.Set("state", state)
	if provider == "google" {
		values.Set("access_type", "online")
		values.Set("prompt", "select_account")
	}
	http.Redirect(w, r, config.AuthURL+"?"+values.Encode(), http.StatusSeeOther)
}

func (s *Server) handleProfileOAuthCallback(w http.ResponseWriter, r *http.Request, provider string) {
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		var err error
		user, err = s.currentUserFromSession(r)
		if err != nil {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
	}
	config, err := s.loginOAuthProvider(provider)
	if err != nil {
		s.redirectProfileOAuthError(w, r, err.Error())
		return
	}
	state, returnTo, ok := profileOAuthStateFromCookie(r, provider)
	s.clearProfileOAuthStateCookie(w, r)
	if !ok || state == "" || state != strings.TrimSpace(r.URL.Query().Get("state")) {
		s.redirectProfileOAuthError(w, r, translate(resolveLocale(w, r), "oauth_state_invalid"))
		return
	}
	if errMessage := strings.TrimSpace(r.URL.Query().Get("error_description")); errMessage != "" {
		s.redirectProfileOAuthErrorTo(w, returnTo, errMessage)
		return
	}
	if errCode := strings.TrimSpace(r.URL.Query().Get("error")); errCode != "" {
		s.redirectProfileOAuthErrorTo(w, returnTo, errCode)
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		s.redirectProfileOAuthErrorTo(w, returnTo, translate(resolveLocale(w, r), "oauth_code_missing"))
		return
	}
	proxyURL := s.control.SystemProxyURL()
	token, err := exchangeLoginOAuthCode(r.Context(), config, proxyURL, s.profileOAuthRedirectURI(r, provider), code)
	if err != nil {
		s.redirectProfileOAuthErrorTo(w, returnTo, err.Error())
		return
	}
	profile, err := fetchLoginOAuthProfile(r.Context(), config, proxyURL, token)
	if err != nil {
		s.redirectProfileOAuthErrorTo(w, returnTo, err.Error())
		return
	}
	if owner, ok := s.control.OAuthIdentityOwner(profile.Provider, profile.Subject); ok && owner.ID != user.ID {
		state, err := s.createProfileOAuthMergeState(w, r, provider, profile.Subject, owner.ID, user.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		redirectLocalSeeOther(w, r, "/profile/oauth/"+url.PathEscape(provider)+"/merge?state="+url.QueryEscape(state.ID))
		return
	}
	if _, err := s.control.LinkOAuthIdentity(user.ID, controlplane.OAuthUserInput{
		Provider:      profile.Provider,
		Subject:       profile.Subject,
		Email:         profile.Email,
		EmailVerified: profile.EmailVerified,
		Username:      profile.Username,
	}); err != nil {
		s.redirectProfileOAuthErrorTo(w, returnTo, err.Error())
		return
	}
	redirectLocalSeeOther(w, r, appendURLParam(returnTo, "oauth_linked", provider))
}

func (s *Server) handleProfileOAuthMergePage(w http.ResponseWriter, r *http.Request, provider string) {
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	state, ok := s.profileOAuthMergeStateFromRequest(r, provider, user.ID)
	if !ok {
		s.redirectProfileOAuthError(w, r, translate(resolveLocale(w, r), "oauth_merge_state_invalid"))
		return
	}
	source, err := s.control.GetUser(state.SourceID)
	if err != nil {
		s.redirectProfileOAuthError(w, r, err.Error())
		return
	}
	target, err := s.control.GetUser(state.TargetID)
	if err != nil {
		s.redirectProfileOAuthError(w, r, err.Error())
		return
	}
	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":      "page_title_password",
		"ActiveNav":     "",
		"Locale":        locale,
		"Provider":      oauthProviderLabel(provider),
		"ProviderKey":   provider,
		"MergeState":    state.ID,
		"SourceUser":    source,
		"TargetUser":    target,
		"SourceBalance": formatUSDDisplay(source.BalanceNanoUSD),
		"TargetBalance": formatUSDDisplay(target.BalanceNanoUSD),
	}, r)
	s.render(w, "profile_oauth_merge.html", locale, data)
}

func (s *Server) handleProfileOAuthMergeConfirm(w http.ResponseWriter, r *http.Request, provider string) {
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	csrfToken, err := s.ensureConsoleCSRFToken(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	submitted, err := readConsoleCSRFToken(w, r)
	if err != nil || submitted != csrfToken {
		http.Error(w, translate(resolveLocale(w, r), "csrf_invalid"), http.StatusForbidden)
		return
	}
	state, ok := s.profileOAuthMergeStateFromRequest(r, provider, user.ID)
	if !ok || strings.TrimSpace(r.FormValue("merge_state")) != state.ID {
		s.redirectProfileOAuthError(w, r, translate(resolveLocale(w, r), "oauth_merge_state_invalid"))
		return
	}
	if _, _, err := s.control.MergeOAuthUser(user.ID, state.SourceID, state.Provider, state.Subject); err != nil {
		s.redirectProfileOAuthError(w, r, err.Error())
		return
	}
	s.consumeProfileOAuthMergeState(state.ID)
	s.clearProfileOAuthMergeCookie(w, r)
	http.Redirect(w, r, "/profile/oauth?oauth_merged="+url.QueryEscape(provider), http.StatusSeeOther)
}

func (s *Server) handleProfileOAuthUnlink(w http.ResponseWriter, r *http.Request, provider string) {
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	csrfToken, err := s.ensureConsoleCSRFToken(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	submitted, err := readConsoleCSRFToken(w, r)
	if err != nil || submitted != csrfToken {
		http.Error(w, translate(resolveLocale(w, r), "csrf_invalid"), http.StatusForbidden)
		return
	}
	if _, err := s.control.UnlinkOAuthIdentity(user.ID, provider); err != nil {
		s.redirectProfileOAuthError(w, r, err.Error())
		return
	}
	returnTo := sanitizeModalReturn(r.FormValue("return_to"), modalOAuthBind)
	redirectLocalSeeOther(w, r, appendURLParam(returnTo, "oauth_unlinked", provider))
}

func (s *Server) profileOAuthRedirectURI(r *http.Request, provider string) string {
	return s.loginOAuthRedirectURI(r, provider)
}

func profileOAuthStateFromCookie(r *http.Request, provider string) (string, string, bool) {
	cookie, err := r.Cookie(profileOAuthStateCookieName)
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(cookie.Value, "|", 3)
	if len(parts) != 3 || parts[0] != provider {
		return "", "", false
	}
	returnTo, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", "", false
	}
	return parts[1], sanitizeModalReturn(string(returnTo), modalOAuthBind), true
}

func profileOAuthStateMatchesRequest(r *http.Request, provider string) bool {
	state, _, ok := profileOAuthStateFromCookie(r, provider)
	return ok && state != "" && state == strings.TrimSpace(r.URL.Query().Get("state"))
}

func (s *Server) clearProfileOAuthStateCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     profileOAuthStateCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		MaxAge:   -1,
	})
}

func (s *Server) createProfileOAuthMergeState(w http.ResponseWriter, r *http.Request, provider, subject, sourceID, targetID string) (profileOAuthMergeState, error) {
	stateID, err := randomLoginOAuthState()
	if err != nil {
		return profileOAuthMergeState{}, err
	}
	state := profileOAuthMergeState{
		ID:        stateID,
		Provider:  provider,
		Subject:   strings.TrimSpace(subject),
		SourceID:  strings.TrimSpace(sourceID),
		TargetID:  strings.TrimSpace(targetID),
		ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
	}
	s.oauthMergeMu.Lock()
	if s.oauthMergeStates == nil {
		s.oauthMergeStates = make(map[string]profileOAuthMergeState)
	}
	s.pruneProfileOAuthMergeStatesLocked(time.Now().UTC())
	s.oauthMergeStates[state.ID] = state
	s.oauthMergeMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     profileOAuthMergeCookieName,
		Value:    state.ID,
		Path:     "/profile/oauth/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		MaxAge:   600,
	})
	return state, nil
}

func (s *Server) profileOAuthMergeStateFromRequest(r *http.Request, provider, targetID string) (profileOAuthMergeState, bool) {
	cookie, err := r.Cookie(profileOAuthMergeCookieName)
	if err != nil {
		return profileOAuthMergeState{}, false
	}
	stateID := strings.TrimSpace(cookie.Value)
	if stateID == "" || stateID != strings.TrimSpace(r.URL.Query().Get("state")) && r.Method == http.MethodGet {
		return profileOAuthMergeState{}, false
	}
	s.oauthMergeMu.Lock()
	defer s.oauthMergeMu.Unlock()
	s.pruneProfileOAuthMergeStatesLocked(time.Now().UTC())
	state, ok := s.oauthMergeStates[stateID]
	if !ok || state.Provider != provider || state.TargetID != targetID {
		return profileOAuthMergeState{}, false
	}
	return state, true
}

func (s *Server) consumeProfileOAuthMergeState(stateID string) {
	s.oauthMergeMu.Lock()
	defer s.oauthMergeMu.Unlock()
	delete(s.oauthMergeStates, strings.TrimSpace(stateID))
}

func (s *Server) pruneProfileOAuthMergeStatesLocked(now time.Time) {
	for id, state := range s.oauthMergeStates {
		if !state.ExpiresAt.After(now) {
			delete(s.oauthMergeStates, id)
		}
	}
}

func (s *Server) clearProfileOAuthMergeCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     profileOAuthMergeCookieName,
		Value:    "",
		Path:     "/profile/oauth/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		MaxAge:   -1,
	})
}

func (s *Server) redirectProfileOAuthError(w http.ResponseWriter, r *http.Request, message string) {
	returnTo := sanitizeModalReturn(r.FormValue("return_to"), modalOAuthBind)
	if returnTo == "/profile/oauth" {
		returnTo = sanitizeModalReturn(r.URL.Query().Get("return_to"), modalOAuthBind)
	}
	s.redirectProfileOAuthErrorTo(w, returnTo, message)
}

func (s *Server) redirectProfileOAuthErrorTo(w http.ResponseWriter, returnTo, message string) {
	w.Header().Set("Location", appendURLParam(returnTo, "oauth_error", strings.TrimSpace(message)))
	w.WriteHeader(http.StatusSeeOther)
}
