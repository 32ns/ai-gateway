package providers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

const (
	claudeOAuthClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeOAuthAuthorizeURL = "https://claude.ai/oauth/authorize"
	claudeOAuthTokenURL     = "https://api.anthropic.com/v1/oauth/token"
	claudeOAuthScope        = "org:create_api_key user:profile user:inference"
	claudeOAuthGrantType    = "authorization_code"
	claudeOAuthTokenSource  = "claude_oauth"
	claudeOAuthMode         = "claude-oauth-pkce"
	claudeOAuthBetaHeader   = "oauth-2025-04-20"
)

var ErrClaudeOAuthStateMismatch = errors.New("claude oauth state mismatch")

type ClaudeOAuthStart struct {
	AuthorizationURL string
	State            string
	CodeVerifier     string
	ExpiresIn        int64
}

type ClaudeTokenSet struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	Scope        string
}

type claudeTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

func StartClaudeAuthorization(_ context.Context, redirectURI string) (ClaudeOAuthStart, error) {
	state, err := randomURLToken(24)
	if err != nil {
		return ClaudeOAuthStart{}, err
	}
	verifier, err := randomURLToken(48)
	if err != nil {
		return ClaudeOAuthStart{}, err
	}
	challenge := sha256.Sum256([]byte(verifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(challenge[:])

	return ClaudeOAuthStart{
		AuthorizationURL: fmt.Sprintf(
			"%s?code=true&client_id=%s&response_type=code&redirect_uri=%s&scope=%s&code_challenge=%s&code_challenge_method=S256&state=%s",
			claudeOAuthAuthorizeURL,
			claudeOAuthClientID,
			url.QueryEscape(redirectURI),
			url.QueryEscape(claudeOAuthScope),
			url.QueryEscape(codeChallenge),
			url.QueryEscape(state),
		),
		State:        state,
		CodeVerifier: verifier,
		ExpiresIn:    900,
	}, nil
}

func ExchangeClaudeAuthorizationCode(ctx context.Context, code, state, expectedState, redirectURI, codeVerifier string) (ClaudeTokenSet, error) {
	if strings.TrimSpace(code) == "" {
		return ClaudeTokenSet{}, fmt.Errorf("authorization code is required")
	}
	if strings.TrimSpace(expectedState) == "" || strings.TrimSpace(state) == "" || subtleTrimCompare(state, expectedState) != 1 {
		return ClaudeTokenSet{}, ErrClaudeOAuthStateMismatch
	}

	payload := map[string]string{
		"code":          strings.TrimSpace(code),
		"state":         strings.TrimSpace(state),
		"grant_type":    claudeOAuthGrantType,
		"client_id":     claudeOAuthClientID,
		"redirect_uri":  strings.TrimSpace(redirectURI),
		"code_verifier": strings.TrimSpace(codeVerifier),
	}
	return postClaudeTokenJSON(ctx, payload)
}

func RefreshClaudeToken(ctx context.Context, refreshToken string) (ClaudeTokenSet, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return ClaudeTokenSet{}, &InvokeError{
			Code:      ErrorCodeMissingRefreshCredential,
			Temporary: false,
			Err:       errors.New("refresh token is empty"),
		}
	}

	return postClaudeTokenJSON(ctx, map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     claudeOAuthClientID,
	})
}

func ExtractClaudeIdentity(tokens ClaudeTokenSet) (accountID, email string) {
	claims := parseJWTClaims(tokens.AccessToken)
	if len(claims) == 0 {
		return "", ""
	}
	accountID = claimString(claims, "sub")
	email = claimString(claims, "email")
	return accountID, email
}

func ClaudeTokenExpiry(expiresIn int64) *time.Time {
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	expiresAt := time.Now().UTC().Add(time.Duration(expiresIn) * time.Second)
	return &expiresAt
}

func ClaudeOAuthTokenSourceValue() string {
	return claudeOAuthTokenSource
}

func ClaudeOAuthModeValue() string {
	return claudeOAuthMode
}

func (a *ClaudeAdapter) Refresh(ctx context.Context, account core.Account) (core.Credential, error) {
	ctx = WithProxyURL(ctx, account.EffectiveProxyURL)
	if !CredentialRefreshable(account) {
		return account.Credential, nil
	}
	tokens, err := RefreshClaudeToken(ctx, account.Credential.RefreshToken)
	if err != nil {
		return account.Credential, err
	}
	if strings.TrimSpace(tokens.AccessToken) == "" {
		return account.Credential, &InvokeError{
			Code:      ErrorCodeUpstreamInvalidJSON,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       errors.New("claude oauth refresh response did not include access token"),
		}
	}

	credential := account.Credential
	credential.AccessToken = tokens.AccessToken
	if strings.TrimSpace(tokens.RefreshToken) != "" {
		credential.RefreshToken = tokens.RefreshToken
	}
	credential.ExpiresAt = ClaudeTokenExpiry(tokens.ExpiresIn)
	if credential.Metadata == nil {
		credential.Metadata = map[string]string{}
	}
	credential.Metadata["oauth_refreshed_at"] = time.Now().UTC().Format(time.RFC3339)
	credential.Metadata["token_source"] = claudeOAuthTokenSource
	return credential, nil
}

func claudeHeaders(account core.Account, token string) map[string]string {
	headers := map[string]string{
		"anthropic-version": "2023-06-01",
	}
	if strings.TrimSpace(account.Credential.Metadata["token_source"]) == claudeOAuthTokenSource || strings.TrimSpace(account.Credential.Mode) == claudeOAuthMode {
		headers["Authorization"] = "Bearer " + token
		headers["anthropic-beta"] = claudeOAuthBetaHeader
		return headers
	}
	headers["x-api-key"] = token
	return headers
}

func postClaudeTokenJSON(ctx context.Context, payload map[string]string) (ClaudeTokenSet, error) {
	req, err := newJSONRequest(ctx, http.MethodPost, claudeOAuthTokenURL, "", payload, map[string]string{
		"Accept": "application/json",
	})
	if err != nil {
		return ClaudeTokenSet{}, err
	}

	var upstream claudeTokenResponse
	resp, body, err := doOAuthJSON(req, &upstream)
	if err != nil {
		return ClaudeTokenSet{}, mapOAuthHTTPError(resp, body, err)
	}
	if strings.TrimSpace(upstream.AccessToken) == "" {
		return ClaudeTokenSet{}, fmt.Errorf("claude oauth token response is missing access token")
	}
	return ClaudeTokenSet(upstream), nil
}

func randomURLToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func subtleTrimCompare(left, right string) int {
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(left)), []byte(strings.TrimSpace(right)))
}
