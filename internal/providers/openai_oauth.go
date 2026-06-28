package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

const (
	openAIOAuthClientID              = "app_EMoamEEZ73f0CkXaXp7hrann"
	openAIDeviceAuthUserCodeEndpoint = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	openAIDeviceAuthTokenEndpoint    = "https://auth.openai.com/api/accounts/deviceauth/token"
	openAIOAuthTokenEndpoint         = "https://auth.openai.com/oauth/token"
	openAIDeviceVerificationURI      = "https://auth.openai.com/codex/device"
	openAIDeviceRedirectURI          = "https://auth.openai.com/deviceauth/callback"
	openAIOAuthUserAgent             = "ai-gateway-openai-oauth"
	openAIOAuthMode                  = "openai-oauth-device"
	openAITokenSourceDeviceCode      = "openai_device_code"
	openAITokenSourceCodexAuth       = "codex_auth_json"
	OpenAICodexAuthPathMetadataKey   = "codex_auth_path"
	OpenAIIDTokenMetadataKey         = "id_token"
)

var (
	ErrOpenAIOAuthPending = errors.New("openai oauth authorization pending")
	ErrOpenAIOAuthExpired = errors.New("openai oauth authorization expired")
)

type OpenAIDeviceCode struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	ExpiresIn       int64
	Interval        int64
}

type OpenAITokenSet struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresIn    int64
}

type openAIDeviceCodeResponse struct {
	DeviceAuthID string `json:"device_auth_id"`
	UserCode     string `json:"user_code"`
	ExpiresIn    any    `json:"expires_in"`
	ExpiresAt    string `json:"expires_at"`
	Interval     any    `json:"interval"`
}

type openAIDevicePollResponse struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeVerifier      string `json:"code_verifier"`
}

type openAITokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

type codexAuthDotJSON struct {
	AuthMode     string              `json:"auth_mode"`
	Tokens       *codexAuthTokenData `json:"tokens"`
	IDToken      string              `json:"id_token"`
	AccessToken  string              `json:"access_token"`
	RefreshToken string              `json:"refresh_token"`
	SessionToken string              `json:"session_token"`
	AccountID    string              `json:"account_id"`
	Email        string              `json:"email"`
	Expired      string              `json:"expired"`
	LastRefresh  *time.Time          `json:"last_refresh"`
	Raw          map[string]any      `json:"-"`
}

type codexAuthTokenData struct {
	IDToken      string  `json:"id_token"`
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	AccountID    *string `json:"account_id"`
}

func StartOpenAIDeviceFlow(ctx context.Context) (OpenAIDeviceCode, error) {
	req, err := newJSONRequest(ctx, http.MethodPost, openAIDeviceAuthUserCodeEndpoint, "", map[string]string{
		"client_id": openAIOAuthClientID,
	}, map[string]string{"User-Agent": openAIOAuthUserAgent})
	if err != nil {
		return OpenAIDeviceCode{}, err
	}

	resp, payload, err := doOAuthRaw(req)
	if err != nil {
		return OpenAIDeviceCode{}, mapOAuthHTTPError(resp, payload, err)
	}
	var upstream openAIDeviceCodeResponse
	if err := json.Unmarshal(payload, &upstream); err != nil {
		return OpenAIDeviceCode{}, mapOAuthHTTPError(resp, payload, err)
	}
	if strings.TrimSpace(upstream.DeviceAuthID) == "" || strings.TrimSpace(upstream.UserCode) == "" {
		if resp != nil && resp.StatusCode >= 400 {
			return OpenAIDeviceCode{}, mapOAuthHTTPError(resp, payload, fmt.Errorf("openai oauth returned status %d", resp.StatusCode))
		}
		return OpenAIDeviceCode{}, fmt.Errorf("openai oauth device response is missing required fields")
	}
	expiresIn := int64FromJSONValue(upstream.ExpiresIn)
	if expiresIn <= 0 && strings.TrimSpace(upstream.ExpiresAt) != "" {
		if expiresAt, err := time.Parse(time.RFC3339Nano, upstream.ExpiresAt); err == nil {
			expiresIn = int64(time.Until(expiresAt).Seconds())
		}
	}
	if expiresIn <= 0 {
		expiresIn = 900
	}
	interval := int64FromJSONValue(upstream.Interval)
	if interval <= 0 {
		interval = 5
	}
	return OpenAIDeviceCode{
		DeviceCode:      upstream.DeviceAuthID,
		UserCode:        upstream.UserCode,
		VerificationURI: openAIDeviceVerificationURI,
		ExpiresIn:       expiresIn,
		Interval:        interval,
	}, nil
}

func PollOpenAIDeviceFlow(ctx context.Context, deviceAuthID, userCode string) (OpenAITokenSet, error) {
	req, err := newJSONRequest(ctx, http.MethodPost, openAIDeviceAuthTokenEndpoint, "", map[string]string{
		"device_auth_id": strings.TrimSpace(deviceAuthID),
		"user_code":      strings.TrimSpace(userCode),
	}, map[string]string{"User-Agent": openAIOAuthUserAgent})
	if err != nil {
		return OpenAITokenSet{}, err
	}

	var poll openAIDevicePollResponse
	resp, payload, err := doOAuthJSON(req, &poll)
	if err != nil {
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusForbidden, http.StatusNotFound:
				return OpenAITokenSet{}, ErrOpenAIOAuthPending
			case http.StatusGone:
				return OpenAITokenSet{}, ErrOpenAIOAuthExpired
			}
		}
		return OpenAITokenSet{}, mapOAuthHTTPError(resp, payload, err)
	}
	if strings.TrimSpace(poll.AuthorizationCode) == "" || strings.TrimSpace(poll.CodeVerifier) == "" {
		return OpenAITokenSet{}, fmt.Errorf("openai oauth poll response is missing token exchange fields")
	}
	return exchangeOpenAIAuthorizationCode(ctx, poll.AuthorizationCode, poll.CodeVerifier)
}

func RefreshOpenAIToken(ctx context.Context, refreshToken string) (OpenAITokenSet, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return OpenAITokenSet{}, &InvokeError{
			Code:      ErrorCodeMissingRefreshCredential,
			Temporary: false,
			Err:       errors.New("refresh token is empty"),
		}
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", openAIOAuthClientID)
	form.Set("scope", "openid profile email")
	return postOpenAITokenForm(ctx, form)
}

func OpenAITokenExpiry(expiresIn int64) *time.Time {
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	expiresAt := time.Now().UTC().Add(time.Duration(expiresIn) * time.Second)
	return &expiresAt
}

func OpenAIOAuthModeValue() string {
	return openAIOAuthMode
}

func OpenAIDeviceCodeTokenSourceValue() string {
	return openAITokenSourceDeviceCode
}

func OpenAICodexAuthTokenSourceValue() string {
	return openAITokenSourceCodexAuth
}

func IsOpenAIOAuthTokenSource(value string) bool {
	switch strings.TrimSpace(value) {
	case openAITokenSourceDeviceCode, openAITokenSourceCodexAuth:
		return true
	default:
		return false
	}
}

func ExtractOpenAIIdentity(tokens OpenAITokenSet) (accountID, email string) {
	for _, token := range []string{tokens.IDToken, tokens.AccessToken} {
		claims := parseJWTClaims(token)
		if len(claims) == 0 {
			continue
		}
		if email == "" {
			email = claimString(claims, "email")
		}
		if accountID == "" {
			accountID = claimString(claims, "chatgpt_account_id")
		}
		if accountID == "" {
			if nested, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
				accountID = firstNonEmpty(claimString(nested, "chatgpt_account_id"), claimString(nested, "user_id"))
			}
		}
		if accountID == "" {
			accountID = firstNonEmpty(claimString(claims, "user_id"), firstOrganizationID(claims))
		}
	}
	return strings.TrimSpace(accountID), strings.TrimSpace(email)
}

func openAIIdentitySource(credential core.Credential) string {
	accountID := strings.TrimSpace(credential.Metadata["oauth_account_id"])
	if accountID == "" {
		return ""
	}
	for _, token := range []string{credential.AccessToken, credential.Metadata[OpenAIIDTokenMetadataKey]} {
		if openAIClaimedUserID(token, accountID) {
			return "user_id"
		}
	}
	return ""
}

func openAIClaimedUserID(token, accountID string) bool {
	token = strings.TrimSpace(token)
	accountID = strings.TrimSpace(accountID)
	if token == "" || accountID == "" {
		return false
	}
	claims := parseJWTClaims(token)
	if len(claims) == 0 {
		return false
	}
	if claimString(claims, "user_id") == accountID {
		return true
	}
	if nested, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if claimString(nested, "user_id") == accountID {
			return true
		}
	}
	return false
}

func ExtractOpenAITokenExpiry(tokens OpenAITokenSet) *time.Time {
	for _, token := range []string{tokens.AccessToken, tokens.IDToken} {
		expiresAt := parseJWTExpiry(token)
		if expiresAt != nil {
			return expiresAt
		}
	}
	return nil
}

func (a *OpenAIAdapter) Refresh(ctx context.Context, account core.Account) (core.Credential, error) {
	ctx = WithProxyURL(ctx, account.EffectiveProxyURL)
	credential := account.Credential
	if !CredentialRefreshable(account) {
		return credential, nil
	}
	forceRefresh := strings.TrimSpace(credential.Metadata["force_oauth_refresh"]) == "true"
	originalAccessToken := strings.TrimSpace(credential.AccessToken)
	if synced, ok, err := syncCodexAuthCredentialFromFile(account); err == nil && ok {
		credential = synced
		account.Credential = synced
		syncedAccessToken := strings.TrimSpace(synced.AccessToken)
		if (!forceRefresh || (syncedAccessToken != "" && syncedAccessToken != originalAccessToken)) && !CredentialNeedsRefresh(account) {
			return synced, nil
		}
	}

	attemptedRefreshToken := strings.TrimSpace(credential.RefreshToken)
	tokens, err := RefreshOpenAIToken(ctx, attemptedRefreshToken)
	if err != nil && isCodexImportedCredential(credential) {
		if synced, ok, syncErr := syncCodexAuthCredentialFromFile(account); syncErr == nil && ok {
			account.Credential = synced
			if !CredentialNeedsRefresh(account) {
				return synced, nil
			}
			if latestRefreshToken := strings.TrimSpace(synced.RefreshToken); latestRefreshToken != "" && latestRefreshToken != attemptedRefreshToken {
				credential = synced
				tokens, err = RefreshOpenAIToken(ctx, latestRefreshToken)
			}
		}
	}
	if err != nil {
		if credential.Metadata != nil {
			delete(credential.Metadata, "force_oauth_refresh")
		}
		return credential, err
	}
	if strings.TrimSpace(tokens.AccessToken) == "" {
		return account.Credential, &InvokeError{
			Code:      ErrorCodeUpstreamInvalidJSON,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       errors.New("openai oauth refresh response did not include access token"),
		}
	}

	credential.AccessToken = tokens.AccessToken
	if strings.TrimSpace(tokens.RefreshToken) != "" {
		credential.RefreshToken = tokens.RefreshToken
	}
	if expiresAt := ExtractOpenAITokenExpiry(tokens); expiresAt != nil {
		credential.ExpiresAt = expiresAt
	} else {
		credential.ExpiresAt = OpenAITokenExpiry(tokens.ExpiresIn)
	}
	if credential.Metadata == nil {
		credential.Metadata = map[string]string{}
	}
	delete(credential.Metadata, "force_oauth_refresh")
	if strings.TrimSpace(tokens.IDToken) != "" {
		credential.Metadata[OpenAIIDTokenMetadataKey] = strings.TrimSpace(tokens.IDToken)
	}
	credential.Metadata["oauth_refreshed_at"] = time.Now().UTC().Format(time.RFC3339)
	accountID, email := ExtractOpenAIIdentity(tokens)
	if strings.TrimSpace(credential.Metadata["oauth_account_id"]) == "" && strings.TrimSpace(accountID) != "" {
		credential.Metadata["oauth_account_id"] = strings.TrimSpace(accountID)
	}
	if strings.TrimSpace(credential.Metadata["email"]) == "" && strings.TrimSpace(email) != "" {
		credential.Metadata["email"] = strings.TrimSpace(email)
	}
	if err := persistCodexAuthTokens(credential, tokens); err != nil {
		credential.Metadata["codex_auth_sync_error"] = err.Error()
	}
	return credential, nil
}

func isCodexImportedCredential(credential core.Credential) bool {
	if strings.TrimSpace(credential.Metadata["token_source"]) != openAITokenSourceCodexAuth {
		return false
	}
	return strings.TrimSpace(credential.Metadata[OpenAICodexAuthPathMetadataKey]) != ""
}

func syncCodexAuthCredentialFromFile(account core.Account) (core.Credential, bool, error) {
	credential := account.Credential
	if !isCodexImportedCredential(credential) {
		return credential, false, nil
	}
	authPath := strings.TrimSpace(credential.Metadata[OpenAICodexAuthPathMetadataKey])
	auth, err := readCodexAuthFile(authPath)
	if err != nil {
		return credential, false, err
	}
	if auth.Tokens == nil {
		return credential, false, nil
	}
	tokens := OpenAITokenSet{
		AccessToken:  strings.TrimSpace(auth.Tokens.AccessToken),
		RefreshToken: strings.TrimSpace(auth.Tokens.RefreshToken),
		IDToken:      strings.TrimSpace(auth.Tokens.IDToken),
	}
	if tokens.AccessToken == "" {
		return credential, false, nil
	}
	accountID, email := ExtractOpenAIIdentity(tokens)
	if accountID == "" && auth.Tokens.AccountID != nil {
		accountID = strings.TrimSpace(*auth.Tokens.AccountID)
	}
	expectedAccountID := strings.TrimSpace(credential.Metadata["oauth_account_id"])
	if expectedAccountID != "" && accountID != "" && expectedAccountID != accountID {
		return credential, false, fmt.Errorf("codex auth account id %q does not match account %q", accountID, expectedAccountID)
	}

	credential.AccessToken = tokens.AccessToken
	if tokens.RefreshToken != "" {
		credential.RefreshToken = tokens.RefreshToken
	}
	if sessionToken := strings.TrimSpace(auth.SessionToken); sessionToken != "" {
		credential.SessionToken = sessionToken
	}
	if expiresAt := ExtractOpenAITokenExpiry(tokens); expiresAt != nil {
		credential.ExpiresAt = expiresAt
	} else if expiresAt := parseCodexAuthExpired(auth.Expired); expiresAt != nil {
		credential.ExpiresAt = expiresAt
	}
	if credential.Metadata == nil {
		credential.Metadata = map[string]string{}
	}
	if accountID != "" {
		credential.Metadata["oauth_account_id"] = accountID
	}
	if email != "" {
		credential.Metadata["email"] = email
	}
	if tokens.IDToken != "" {
		credential.Metadata[OpenAIIDTokenMetadataKey] = tokens.IDToken
	}
	credential.Metadata["codex_auth_synced_at"] = time.Now().UTC().Format(time.RFC3339)
	delete(credential.Metadata, "codex_auth_sync_error")
	return credential, true, nil
}

func readCodexAuthFile(path string) (codexAuthDotJSON, error) {
	payload, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return codexAuthDotJSON{}, err
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return codexAuthDotJSON{}, err
	}
	var auth codexAuthDotJSON
	if err := json.Unmarshal(payload, &auth); err != nil {
		return codexAuthDotJSON{}, err
	}
	auth.Raw = raw
	auth.normalize()
	return auth, nil
}

func (auth *codexAuthDotJSON) normalize() {
	if auth == nil || auth.Tokens != nil {
		return
	}
	if strings.TrimSpace(auth.AccessToken) == "" && strings.TrimSpace(auth.RefreshToken) == "" {
		return
	}
	accountID := strings.TrimSpace(auth.AccountID)
	auth.Tokens = &codexAuthTokenData{
		IDToken:      strings.TrimSpace(auth.IDToken),
		AccessToken:  strings.TrimSpace(auth.AccessToken),
		RefreshToken: strings.TrimSpace(auth.RefreshToken),
	}
	if accountID != "" {
		auth.Tokens.AccountID = &accountID
	}
}

func parseCodexAuthExpired(value string) *time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			utc := parsed.UTC()
			return &utc
		}
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			utc := parsed.UTC()
			return &utc
		}
	}
	return nil
}

func persistCodexAuthTokens(credential core.Credential, tokens OpenAITokenSet) error {
	if !isCodexImportedCredential(credential) {
		return nil
	}
	authPath := strings.TrimSpace(credential.Metadata[OpenAICodexAuthPathMetadataKey])
	auth, err := readCodexAuthFile(authPath)
	if err != nil {
		return err
	}
	raw := auth.Raw
	if raw == nil {
		raw = map[string]any{}
	}
	tokenMap, _ := raw["tokens"].(map[string]any)
	flatFormat := tokenMap == nil && (raw["access_token"] != nil || raw["refresh_token"] != nil || raw["id_token"] != nil)
	if strings.TrimSpace(tokens.IDToken) != "" {
		setCodexAuthTokenValue(raw, tokenMap, flatFormat, "id_token", strings.TrimSpace(tokens.IDToken))
	} else if idToken := strings.TrimSpace(credential.Metadata[OpenAIIDTokenMetadataKey]); idToken != "" {
		setCodexAuthTokenValue(raw, tokenMap, flatFormat, "id_token", idToken)
	}
	if strings.TrimSpace(tokens.AccessToken) != "" {
		setCodexAuthTokenValue(raw, tokenMap, flatFormat, "access_token", strings.TrimSpace(tokens.AccessToken))
	}
	if strings.TrimSpace(tokens.RefreshToken) != "" {
		setCodexAuthTokenValue(raw, tokenMap, flatFormat, "refresh_token", strings.TrimSpace(tokens.RefreshToken))
	}
	if accountID := strings.TrimSpace(credential.Metadata["oauth_account_id"]); accountID != "" {
		setCodexAuthTokenValue(raw, tokenMap, flatFormat, "account_id", accountID)
	}
	raw["last_refresh"] = time.Now().UTC()
	payload, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(authPath, append(payload, '\n'), 0o600)
}

func writeFileAtomic(path string, payload []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	cleanup := true
	defer func() {
		_ = temp.Close()
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()

	if err := temp.Chmod(perm); err != nil {
		return err
	}
	if _, err := temp.Write(payload); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	cleanup = false
	return os.Rename(tempPath, path)
}

func setCodexAuthTokenValue(raw map[string]any, tokenMap map[string]any, flatFormat bool, key, value string) {
	if flatFormat {
		raw[key] = value
		return
	}
	if tokenMap == nil {
		tokenMap = map[string]any{}
		raw["tokens"] = tokenMap
	}
	tokenMap[key] = value
}

func exchangeOpenAIAuthorizationCode(ctx context.Context, code, verifier string) (OpenAITokenSet, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", strings.TrimSpace(code))
	form.Set("redirect_uri", openAIDeviceRedirectURI)
	form.Set("client_id", openAIOAuthClientID)
	form.Set("code_verifier", strings.TrimSpace(verifier))
	return postOpenAITokenForm(ctx, form)
}

func postOpenAITokenForm(ctx context.Context, form url.Values) (OpenAITokenSet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAIOAuthTokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return OpenAITokenSet{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", openAIOAuthUserAgent)

	var upstream openAITokenResponse
	resp, payload, err := doOAuthJSON(req, &upstream)
	if err != nil {
		return OpenAITokenSet{}, mapOAuthHTTPError(resp, payload, err)
	}
	if strings.TrimSpace(upstream.AccessToken) == "" {
		return OpenAITokenSet{}, fmt.Errorf("openai oauth token response is missing access token")
	}
	return OpenAITokenSet(upstream), nil
}

func doOAuthJSON(req *http.Request, out any) (*http.Response, []byte, error) {
	resp, payload, err := doOAuthRaw(req)
	if err != nil {
		return resp, payload, err
	}
	if resp.StatusCode >= 400 {
		return resp, payload, fmt.Errorf("openai oauth returned status %d", resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(payload, out); err != nil {
			return resp, payload, err
		}
	}
	return resp, payload, nil
}

func doOAuthRaw(req *http.Request) (*http.Response, []byte, error) {
	client, err := httpClientForContext(req.Context())
	if err != nil {
		return nil, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return resp, nil, readErr
	}
	return resp, payload, nil
}

func int64FromJSONValue(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	case string:
		var parsed int64
		_, _ = fmt.Sscanf(strings.TrimSpace(typed), "%d", &parsed)
		return parsed
	default:
		return 0
	}
}

func mapOAuthHTTPError(resp *http.Response, payload []byte, err error) error {
	if err == nil {
		err = errors.New("openai oauth request failed")
	}
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	message := strings.TrimSpace(extractErrorMessage(payload))
	if message == "" {
		message = err.Error()
	}
	switch {
	case status == http.StatusBadRequest && isOAuthCredentialExpiredMessage(message):
		return &InvokeError{Code: ErrorCodeCredentialExpired, Temporary: false, Cooldown: 2 * time.Minute, Err: errors.New(message)}
	case status == http.StatusUnauthorized:
		return &InvokeError{Code: ErrorCodeCredentialExpired, Temporary: false, Cooldown: 2 * time.Minute, Err: errors.New(message)}
	case status == http.StatusForbidden && quotaFailureMessage(message):
		return &InvokeError{Code: ErrorCodeUpstreamRateLimited, Temporary: true, Cooldown: 45 * time.Second, Err: errors.New(message)}
	case status == http.StatusForbidden:
		return &InvokeError{Code: ErrorCodeUpstreamForbidden, Temporary: false, Cooldown: 2 * time.Minute, Err: errors.New(message)}
	case status == http.StatusTooManyRequests:
		return &InvokeError{Code: ErrorCodeUpstreamRateLimited, Temporary: true, Cooldown: 45 * time.Second, Err: errors.New(message)}
	case status >= 500 || status == 0:
		return &InvokeError{Code: ErrorCodeUpstreamTransportError, Temporary: true, Cooldown: 20 * time.Second, Err: errors.New(message)}
	default:
		return fmt.Errorf("%s", message)
	}
}

func isOAuthCredentialExpiredMessage(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	if normalized == "" {
		return false
	}
	for _, signal := range []string{
		"invalid_grant",
		"invalid grant",
		"invalid refresh token",
		"refresh token expired",
		"expired refresh token",
	} {
		if strings.Contains(normalized, signal) {
			return true
		}
	}
	return false
}

func parseJWTClaims(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	claims := map[string]any{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	return claims
}

func claimString(claims map[string]any, key string) string {
	value, _ := claims[key].(string)
	return strings.TrimSpace(value)
}

func firstOrganizationID(claims map[string]any) string {
	raw, ok := claims["organizations"].([]any)
	if !ok || len(raw) == 0 {
		return ""
	}
	first, ok := raw[0].(map[string]any)
	if !ok {
		return ""
	}
	return claimString(first, "id")
}

func parseJWTExpiry(token string) *time.Time {
	claims := parseJWTClaims(token)
	if len(claims) == 0 {
		return nil
	}
	value, ok := claims["exp"]
	if !ok {
		return nil
	}
	exp := int64FromJSONValue(value)
	if exp <= 0 {
		return nil
	}
	expiresAt := time.Unix(exp, 0).UTC()
	return &expiresAt
}
