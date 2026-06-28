package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/netproxy"
)

type Adapter interface {
	Kind() core.ProviderKind
	DisplayName() string
	ListModels(context.Context) []core.ModelSpec
	Invoke(context.Context, core.RouteDecision, *core.GatewayRequest) (*core.GatewayResponse, error)
}

type UpstreamModel struct {
	ID          string
	DisplayName string
	OwnedBy     string
}

type ModelListingAdapter interface {
	Adapter
	FetchModels(context.Context, core.Account) ([]UpstreamModel, error)
}

type EmbeddingAdapter interface {
	Adapter
	Embed(context.Context, core.RouteDecision, *core.EmbeddingRequest) (*core.EmbeddingResponse, error)
}

type ResponsesAdapter interface {
	Adapter
	InvokeResponses(context.Context, core.RouteDecision, *core.ResponsesRequest) (*core.GatewayResponse, error)
}

type ResponsesStreamAdapter interface {
	Adapter
	OpenResponsesStream(context.Context, core.RouteDecision, *core.ResponsesRequest) (*StreamSession, error)
}

type ModerationAdapter interface {
	Adapter
	Moderate(context.Context, core.RouteDecision, *core.ModerationRequest) (*core.ModerationResponse, error)
}

type ImageGenerationAdapter interface {
	Adapter
	GenerateImage(context.Context, core.RouteDecision, *core.ImageGenerationRequest) (*core.ImageGenerationResponse, error)
}

type ImageGenerationStreamAdapter interface {
	Adapter
	OpenImageGenerationStream(context.Context, core.RouteDecision, *core.ImageGenerationRequest) (*StreamSession, error)
}

type ImageMultipartAdapter interface {
	Adapter
	ProcessImageMultipart(context.Context, core.RouteDecision, *core.ImageMultipartRequest) (*core.ImageMultipartResponse, error)
}

type ImageMultipartStreamAdapter interface {
	Adapter
	OpenImageMultipartStream(context.Context, core.RouteDecision, *core.ImageMultipartRequest) (*StreamSession, error)
}

type AudioSpeechAdapter interface {
	Adapter
	CreateSpeech(context.Context, core.RouteDecision, *core.AudioSpeechRequest) (*core.AudioSpeechResponse, error)
}

type AudioMultipartAdapter interface {
	Adapter
	ProcessAudioMultipart(context.Context, core.RouteDecision, *core.AudioMultipartRequest) (*core.AudioMultipartResponse, error)
}

type TokenCountAdapter interface {
	Adapter
	CountTokens(context.Context, core.RouteDecision, *core.TokenCountRequest) (*core.TokenCountResponse, error)
}

type RefreshingAdapter interface {
	Adapter
	Refresh(context.Context, core.Account) (core.Credential, error)
}

type QuotaFetchingAdapter interface {
	Adapter
	FetchQuota(context.Context, core.Account) (core.AccountQuotaSnapshot, error)
}

type Registry struct {
	adapters map[core.ProviderKind]Adapter
}

func NewRegistry(adapters ...Adapter) *Registry {
	registry := &Registry{adapters: make(map[core.ProviderKind]Adapter, len(adapters))}
	for _, adapter := range adapters {
		registry.adapters[adapter.Kind()] = adapter
	}
	return registry
}

func (r *Registry) Get(kind core.ProviderKind) (Adapter, bool) {
	adapter, ok := r.adapters[kind]
	return adapter, ok
}

func (r *Registry) ListModels(ctx context.Context) []core.ModelSpec {
	models := make([]core.ModelSpec, 0, 8)
	for _, adapter := range r.adapters {
		models = append(models, adapter.ListModels(ctx)...)
	}
	return models
}

type InvokeError struct {
	Code          string
	Temporary     bool
	Cooldown      time.Duration
	RetryAfter    time.Duration
	RetryAfterSet bool
	Err           error
}

type FailureScope int

const (
	FailureScopeRequest FailureScope = iota
	FailureScopeAccount
	FailureScopeQuota
	FailureScopeSharedUpstream
)

type FailurePolicy struct {
	Code                string
	Temporary           bool
	Cooldown            time.Duration
	Scope               FailureScope
	RetryFailover       bool
	RefreshCredential   bool
	AccountStatus       core.AccountStatus
	ApplyChatQuota      bool
	ApplyImageQuota     bool
	ResetWebSocket      bool
	PreviousFallback    bool
	SharedUpstreamScope string
}

var (
	httpClient              = &http.Client{Transport: newUpstreamTransport()}
	proxyClientMu           sync.RWMutex
	proxyClientsByURL       = map[string]*http.Client{}
	upstreamClientsByConfig = map[upstreamClientCacheKey]*http.Client{}
	proxyTestEndpoint       = "https://api.openai.com/v1/models"
)

const maxUpstreamResponseBodyBytes = 64 << 20
const upstreamResponseHeaderTimeout = 60 * time.Second

type upstreamClientCacheKey struct {
	proxyURL              string
	responseHeaderTimeout time.Duration
}

type proxyContextKey struct{}
type upstreamAccountContextKey struct{}

type ProxyTestResult struct {
	TargetURL  string
	StatusCode int
	Duration   time.Duration
}

func WithProxyURL(ctx context.Context, proxyURL string) context.Context {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return ctx
	}
	return context.WithValue(ctx, proxyContextKey{}, proxyURL)
}

func proxyURLFromContext(ctx context.Context) string {
	value, _ := ctx.Value(proxyContextKey{}).(string)
	return value
}

func WithUpstreamAccount(ctx context.Context, account core.Account) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(account.ID) == "" && strings.TrimSpace(account.Credential.AccessToken) == "" {
		return ctx
	}
	return context.WithValue(ctx, upstreamAccountContextKey{}, upstreamAccountErrorContext(account))
}

func upstreamAccountFromContext(ctx context.Context) (core.Account, bool) {
	if ctx == nil {
		return core.Account{}, false
	}
	account, ok := ctx.Value(upstreamAccountContextKey{}).(core.Account)
	return account, ok
}

func upstreamAccountErrorContext(account core.Account) core.Account {
	metadata := map[string]string{}
	for _, key := range []string{
		"account_login_method",
		"api_key_quota_provider",
		"base_url",
		"endpoint",
	} {
		if value := strings.TrimSpace(account.Credential.Metadata[key]); value != "" {
			metadata[key] = value
		}
	}
	return core.Account{
		ID:       account.ID,
		Provider: account.Provider,
		Credential: core.Credential{
			Mode:     account.Credential.Mode,
			Metadata: metadata,
		},
	}
}

func (e *InvokeError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Err)
}

func (e *InvokeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func IsTemporary(err error) bool {
	var invokeErr *InvokeError
	if errors.As(err, &invokeErr) {
		return invokeErr.Temporary
	}
	return false
}

func ErrorCode(err error) string {
	var invokeErr *InvokeError
	if errors.As(err, &invokeErr) && invokeErr != nil {
		return strings.TrimSpace(invokeErr.Code)
	}
	return ""
}

func CooldownFor(err error) time.Duration {
	var invokeErr *InvokeError
	if errors.As(err, &invokeErr) && invokeErr.Cooldown > 0 {
		return invokeErr.Cooldown
	}
	return 30 * time.Second
}

func FailurePolicyFor(account core.Account, err error) FailurePolicy {
	var invokeErr *InvokeError
	policy := FailurePolicy{
		Code:          ErrorCode(err),
		Temporary:     IsTemporary(err),
		Cooldown:      CooldownFor(err),
		Scope:         FailureScopeRequest,
		AccountStatus: core.AccountStatusCooling,
	}
	if errors.As(err, &invokeErr) && invokeErr != nil {
		policy.Code = strings.TrimSpace(invokeErr.Code)
		policy.Temporary = invokeErr.Temporary
		if invokeErr.Cooldown > 0 {
			policy.Cooldown = invokeErr.Cooldown
		}
	}
	if policy.Cooldown <= 0 {
		policy.Cooldown = 30 * time.Second
	}

	switch policy.Code {
	case ErrorCodeMissingCredential,
		ErrorCodeCredentialExpired,
		ErrorCodeMissingRefreshCredential,
		ErrorCodeCredentialRefreshNotSupported,
		ErrorCodeUpstreamAuthError:
		policy.Scope = FailureScopeAccount
		policy.RetryFailover = true
		policy.RefreshCredential = policy.Code == ErrorCodeCredentialExpired || policy.Code == ErrorCodeUpstreamAuthError
		policy.PreviousFallback = true
		policy.AccountStatus = core.AccountStatusExpired
	case ErrorCodeUpstreamProviderBanned:
		policy.Scope = FailureScopeAccount
		policy.RetryFailover = true
		policy.PreviousFallback = true
		policy.AccountStatus = core.AccountStatusProviderBanned
	case ErrorCodeGatewayAPIKeyDisabled:
		policy.Scope = FailureScopeAccount
		policy.RetryFailover = true
		policy.PreviousFallback = true
		policy.ResetWebSocket = true
		policy.AccountStatus = core.AccountStatusCooling
	case ErrorCodeUpstreamRateLimited:
		policy.Scope = FailureScopeQuota
		policy.RetryFailover = true
		policy.PreviousFallback = true
		policy.AccountStatus = core.AccountStatusCooling
		policy.ApplyChatQuota = true
		policy.ApplyImageQuota = true
	case ErrorCodeUpstreamEmptyResponse:
		policy.RetryFailover = true
		policy.PreviousFallback = true
	case ErrorCodeUpstreamServerError,
		ErrorCodeUpstreamTransportError,
		ErrorCodeUpstreamReadError,
		ErrorCodeUpstreamTemporarilyUnavailable,
		ErrorCodeUpstreamInvalidStreamChunk:
		policy.Scope = FailureScopeSharedUpstream
		policy.RetryFailover = true
		policy.PreviousFallback = true
		policy.ResetWebSocket = true
		policy.SharedUpstreamScope = UpstreamFailureScope(account)
		if policy.Code == ErrorCodeUpstreamServerError {
			policy.PreviousFallback = sharedServerErrorCanDropResponseBinding(account, err)
		}
	case ErrorCodeImageBackendRequiresOAuth:
		policy.RetryFailover = true
	case ErrorCodeUpstreamForbidden:
		policy.Scope = FailureScopeAccount
		policy.RetryFailover = true
		policy.PreviousFallback = true
		policy.AccountStatus = core.AccountStatusCooling
	case ErrorCodeUpstreamRejected:
		if upstreamRejectedLooksAccountScoped(err) {
			policy.Scope = FailureScopeAccount
			policy.RetryFailover = true
			policy.PreviousFallback = true
			policy.AccountStatus = core.AccountStatusCooling
		}
	case ErrorCodeUpstreamNotFound:
		if upstreamNotFoundLooksModelScoped(err) {
			policy.RetryFailover = true
			policy.PreviousFallback = true
		}
	}
	return policy
}

func upstreamNotFoundLooksModelScoped(err error) bool {
	message := strings.ToLower(invokeErrorMessage(err))
	if message == "" || !strings.Contains(message, "model") {
		return false
	}
	for _, signal := range []string{
		"not supported",
		"unsupported",
		"not found",
		"does not exist",
		"doesn't exist",
		"not available",
		"no available",
		"not enabled",
	} {
		if strings.Contains(message, signal) {
			return true
		}
	}
	return false
}

func upstreamRejectedLooksAccountScoped(err error) bool {
	message := strings.ToLower(invokeErrorMessage(err))
	if message == "" {
		return false
	}
	for _, signal := range []string{
		"auth missing project_id",
		"missing project_id",
		"account or project",
		"disabled in this account",
		"disabled in this project",
		"service has been disabled",
		"project has been disabled",
		"account has been disabled",
		"permission_denied",
	} {
		if strings.Contains(message, signal) {
			return true
		}
	}
	if strings.Contains(message, "permission denied") &&
		(strings.Contains(message, "account") || strings.Contains(message, "project")) {
		return true
	}
	return false
}

func sharedServerErrorCanDropResponseBinding(account core.Account, err error) bool {
	message := strings.ToLower(invokeErrorMessage(err))
	if message == "" {
		return false
	}
	for _, signal := range []string{
		"html challenge",
		"challenge response",
		"challenge-platform",
		"cloudflare",
		"cf-ray",
		"captcha",
		"upstream authentication failed",
		"authentication failed",
		"auth failed",
		"access denied",
		"proxy",
		"tunnel",
		"bad gateway",
		"gateway timeout",
	} {
		if strings.Contains(message, signal) {
			return true
		}
	}
	return false
}

func ApplyCredentialRefreshFailureStatus(account core.Account, err error, now time.Time) core.Account {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if account.Status == core.AccountStatusBlocked {
		account.CooldownUntil = nil
		return account
	}
	if status, ok := CredentialRefreshTerminalStatus(err); ok {
		account.Status = status
		account.CooldownUntil = nil
		return account
	}
	if account.Status == core.AccountStatusExpired || account.Status == core.AccountStatusProviderBanned {
		account.CooldownUntil = nil
		return account
	}
	cooldown := CooldownFor(err)
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	until := now.Add(cooldown)
	account.Status = core.AccountStatusCooling
	account.CooldownUntil = &until
	return account
}

func CredentialRefreshTerminalStatus(err error) (core.AccountStatus, bool) {
	switch ErrorCode(err) {
	case ErrorCodeCredentialExpired, ErrorCodeMissingRefreshCredential, ErrorCodeCredentialRefreshNotSupported:
		return core.AccountStatusExpired, true
	case ErrorCodeUpstreamProviderBanned:
		return core.AccountStatusProviderBanned, true
	default:
		return "", false
	}
}

func CredentialNeedsRefresh(account core.Account) bool {
	if account.Credential.ExpiresAt == nil || account.Credential.ExpiresAt.IsZero() {
		return false
	}
	if !CredentialRefreshable(account) {
		return false
	}
	return time.Until(account.Credential.ExpiresAt.UTC()) <= 30*time.Second
}

func CredentialRefreshable(account core.Account) bool {
	tokenSource := strings.TrimSpace(account.Credential.Metadata["token_source"])
	mode := strings.TrimSpace(account.Credential.Mode)
	switch account.Provider {
	case core.ProviderOpenAI:
		return IsOpenAIOAuthTokenSource(tokenSource) || mode == OpenAIOAuthModeValue()
	case core.ProviderClaude:
		return tokenSource == ClaudeOAuthTokenSourceValue() || mode == ClaudeOAuthModeValue()
	default:
		return false
	}
}

func newJSONRequest(ctx context.Context, method, endpoint, token string, payload any, headers map[string]string) (*http.Request, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		if strings.TrimSpace(value) != "" {
			req.Header.Set(key, value)
		}
	}
	return req, nil
}

func newRawRequest(ctx context.Context, method, endpoint, token string, body []byte, contentType string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(contentType) != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

func doJSON(req *http.Request, out any) (*http.Response, []byte, error) {
	client, err := httpClientForContext(req.Context())
	if err != nil {
		return nil, nil, &InvokeError{
			Code:      ErrorCodeUpstreamTransportError,
			Temporary: true,
			Cooldown:  20 * time.Second,
			Err:       err,
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, &InvokeError{
			Code:      ErrorCodeUpstreamTransportError,
			Temporary: true,
			Cooldown:  20 * time.Second,
			Err:       err,
		}
	}
	defer resp.Body.Close()

	payload, readErr := readLimitedUpstreamBody(resp.Body, maxUpstreamResponseBodyBytes)
	if readErr != nil {
		return resp, nil, &InvokeError{
			Code:      ErrorCodeUpstreamReadError,
			Temporary: true,
			Cooldown:  20 * time.Second,
			Err:       readErr,
		}
	}

	if resp.StatusCode >= 400 {
		return resp, payload, mapHTTPErrorForContext(req.Context(), resp.StatusCode, payload)
	}
	if out != nil {
		if err := json.Unmarshal(payload, out); err != nil {
			return resp, payload, &InvokeError{
				Code:      ErrorCodeUpstreamInvalidJSON,
				Temporary: true,
				Cooldown:  10 * time.Second,
				Err:       err,
			}
		}
	}
	return resp, payload, nil
}

func doDecodeJSON(req *http.Request, out any) (*http.Response, error) {
	client, err := httpClientForContext(req.Context())
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamTransportError,
			Temporary: true,
			Cooldown:  20 * time.Second,
			Err:       err,
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamTransportError,
			Temporary: true,
			Cooldown:  20 * time.Second,
			Err:       err,
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		payload, readErr := readLimitedUpstreamBody(resp.Body, maxUpstreamResponseBodyBytes)
		if readErr != nil {
			return resp, &InvokeError{
				Code:      ErrorCodeUpstreamReadError,
				Temporary: true,
				Cooldown:  20 * time.Second,
				Err:       readErr,
			}
		}
		return resp, mapHTTPErrorForContext(req.Context(), resp.StatusCode, payload)
	}
	if out == nil {
		return resp, nil
	}
	if err := decodeLimitedUpstreamJSON(resp.Body, out, maxUpstreamResponseBodyBytes); err != nil {
		return resp, &InvokeError{
			Code:      ErrorCodeUpstreamInvalidJSON,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}
	return resp, nil
}

func decodeLimitedUpstreamJSON(body io.Reader, out any, limit int64) error {
	limited := &io.LimitedReader{R: body, N: limit + 1}
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if limited.N <= 0 {
		return fmt.Errorf("upstream response body exceeds %d bytes", limit)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("upstream response body contains multiple JSON values")
		}
		return err
	}
	return nil
}

func readLimitedUpstreamBody(body io.Reader, limit int64) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > limit {
		return nil, fmt.Errorf("upstream response body exceeds %d bytes", limit)
	}
	return payload, nil
}

func httpClientForContext(ctx context.Context) (*http.Client, error) {
	proxyURL := proxyURLFromContext(ctx)
	responseHeaderTimeout := responseHeaderTimeoutForContext(ctx)
	if proxyURL == "" && responseHeaderTimeout == upstreamResponseHeaderTimeout {
		proxyClientMu.RLock()
		client := httpClient
		proxyClientMu.RUnlock()
		return client, nil
	}

	if responseHeaderTimeout == upstreamResponseHeaderTimeout {
		proxyClientMu.RLock()
		if client := proxyClientsByURL[proxyURL]; client != nil {
			proxyClientMu.RUnlock()
			return client, nil
		}
		proxyClientMu.RUnlock()

		proxyClientMu.Lock()
		defer proxyClientMu.Unlock()
		if client := proxyClientsByURL[proxyURL]; client != nil {
			return client, nil
		}
		client, err := newProxyHTTPClient(proxyURL)
		if err != nil {
			return nil, err
		}
		proxyClientsByURL[proxyURL] = client
		return client, nil
	}

	key := upstreamClientCacheKey{proxyURL: proxyURL, responseHeaderTimeout: responseHeaderTimeout}
	proxyClientMu.RLock()
	if client := upstreamClientsByConfig[key]; client != nil {
		proxyClientMu.RUnlock()
		return client, nil
	}
	proxyClientMu.RUnlock()

	proxyClientMu.Lock()
	defer proxyClientMu.Unlock()
	if client := upstreamClientsByConfig[key]; client != nil {
		return client, nil
	}
	client, err := netproxy.NewHTTPClient(proxyURL, 0, responseHeaderTimeout)
	if err != nil {
		return nil, err
	}
	upstreamClientsByConfig[key] = client
	return client, nil
}

func responseHeaderTimeoutForContext(ctx context.Context) time.Duration {
	timeout := upstreamResponseHeaderTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining > timeout {
			timeout = roundDurationUp(remaining, time.Second)
		}
	}
	return timeout
}

func roundDurationUp(value, unit time.Duration) time.Duration {
	if value <= 0 || unit <= 0 {
		return value
	}
	return ((value + unit - 1) / unit) * unit
}

func ConfigureHTTPTransport() {
	ConfigureHTTPClient(&http.Client{Transport: newUpstreamTransport()})
}

func ConfigureHTTPClient(client *http.Client) {
	if client == nil {
		client = &http.Client{Transport: newUpstreamTransport()}
	}
	proxyClientMu.Lock()
	defer proxyClientMu.Unlock()
	httpClient = client
	proxyClientsByURL = map[string]*http.Client{}
	upstreamClientsByConfig = map[upstreamClientCacheKey]*http.Client{}
}

func newUpstreamTransport() *http.Transport {
	return netproxy.NewTransport(upstreamResponseHeaderTimeout)
}

func newProxyHTTPClient(proxyURL string) (*http.Client, error) {
	return netproxy.NewHTTPClient(proxyURL, 0, upstreamResponseHeaderTimeout)
}

func TestProxy(ctx context.Context, proxyURL string) (ProxyTestResult, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	result := ProxyTestResult{TargetURL: proxyTestEndpoint}
	if proxyURL == "" {
		return result, fmt.Errorf("proxy url is required")
	}
	client, err := newProxyHTTPClient(proxyURL)
	if err != nil {
		return result, err
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxyTestEndpoint, nil)
	if err != nil {
		return result, err
	}
	req.Header.Set("User-Agent", "ai-gateway-proxy-test")

	start := time.Now()
	resp, err := client.Do(req)
	result.Duration = time.Since(start)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	result.StatusCode = resp.StatusCode
	if resp.StatusCode == http.StatusProxyAuthRequired || resp.StatusCode >= 500 {
		return result, fmt.Errorf("proxy test returned HTTP %d", resp.StatusCode)
	}
	return result, nil
}

func mapHTTPError(status int, payload []byte) error {
	message := strings.TrimSpace(extractErrorMessage(payload))
	if message == "" {
		message = fmt.Sprintf("upstream returned status %d", status)
	}

	switch {
	case (status == http.StatusUnauthorized || status == http.StatusForbidden) && isProviderBannedMessage(message):
		return &InvokeError{Code: ErrorCodeUpstreamProviderBanned, Temporary: false, Err: errors.New(message)}
	case (status == http.StatusPaymentRequired || status == http.StatusForbidden || status == http.StatusTooManyRequests || status >= 500) && quotaFailureMessage(message):
		return &InvokeError{Code: ErrorCodeUpstreamRateLimited, Temporary: true, Cooldown: 45 * time.Second, Err: errors.New(message)}
	case status == http.StatusUnauthorized:
		return &InvokeError{Code: ErrorCodeUpstreamAuthError, Temporary: false, Cooldown: 2 * time.Minute, Err: errors.New(message)}
	case status == http.StatusForbidden:
		return &InvokeError{Code: ErrorCodeUpstreamForbidden, Temporary: false, Cooldown: 2 * time.Minute, Err: errors.New(message)}
	case status == http.StatusTooManyRequests:
		return &InvokeError{Code: ErrorCodeUpstreamRateLimited, Temporary: true, Cooldown: 45 * time.Second, Err: errors.New(message)}
	case status >= 500:
		return &InvokeError{Code: ErrorCodeUpstreamServerError, Temporary: true, Cooldown: 30 * time.Second, Err: errors.New(message)}
	case status == http.StatusNotFound:
		return &InvokeError{Code: ErrorCodeUpstreamNotFound, Temporary: false, Cooldown: 2 * time.Minute, Err: errors.New(message)}
	default:
		return &InvokeError{Code: ErrorCodeUpstreamRejected, Temporary: false, Cooldown: 2 * time.Minute, Err: errors.New(message)}
	}
}

func mapHTTPErrorForContext(ctx context.Context, status int, payload []byte) error {
	if account, ok := upstreamAccountFromContext(ctx); ok {
		return mapHTTPErrorForAccount(account, status, payload)
	}
	return mapHTTPError(status, payload)
}

func mapHTTPErrorForAccount(account core.Account, status int, payload []byte) error {
	if IsGatewayAPIKeyAccount(account) {
		if err := mapGatewayAPIKeyError(account, status, payload); err != nil {
			return err
		}
	}
	return mapHTTPError(status, payload)
}

func apiKeyDisabledError(message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "gateway API key is disabled"
	}
	return &InvokeError{
		Code:      ErrorCodeGatewayAPIKeyDisabled,
		Temporary: true,
		Cooldown:  2 * time.Minute,
		Err:       errors.New(message),
	}
}

func IsGatewayAPIKeyAccount(account core.Account) bool {
	if strings.TrimSpace(account.Credential.Mode) != "manual-token" {
		return false
	}
	if !gatewayProviderSupportsAPIKey(account) {
		return false
	}
	baseURL := strings.TrimSpace(firstNonEmptyMetadata(account.Credential.Metadata, "base_url", "endpoint"))
	if baseURL == "" || isOfficialProviderBaseURL(account.Provider, baseURL) {
		return false
	}
	return gatewayQuotaProvider(account) != ""
}

func IsSub2APIGatewayAPIKeyAccount(account core.Account) bool {
	if !IsGatewayAPIKeyAccount(account) {
		return false
	}
	switch gatewayQuotaProvider(account) {
	case "sub2api", "gateway":
		return true
	default:
		return false
	}
}

func gatewayProviderSupportsAPIKey(account core.Account) bool {
	switch account.Provider {
	case core.ProviderOpenAI:
		return openAIAccountLoginMethod(account) == "api_key"
	case core.ProviderClaude:
		return apiKeyQuotaAccountLoginMethod(account) == "api_key"
	default:
		return true
	}
}

func gatewayQuotaProvider(account core.Account) string {
	provider := strings.ToLower(strings.TrimSpace(account.Credential.Metadata["api_key_quota_provider"]))
	switch provider {
	case "new-api", "sub2api", "gateway":
		return provider
	default:
		return ""
	}
}

func isOfficialProviderBaseURL(provider core.ProviderKind, baseURL string) bool {
	switch provider {
	case core.ProviderOpenAI:
		return isOfficialOpenAIAPIBaseURL(baseURL)
	case core.ProviderClaude:
		return sameUpstreamBaseURL(baseURL, claudeBaseURL, "/v1")
	default:
		return false
	}
}

func mapGatewayAPIKeyError(account core.Account, status int, payload []byte) error {
	if isGatewayAPIKeyDisabledPayload(payload) {
		return apiKeyDisabledError(strings.TrimSpace(extractErrorMessage(payload)))
	}
	if err := gatewayMiddleLayerError(status, payload); err != nil {
		return err
	}
	return nil
}

func gatewayMiddleLayerError(status int, payload []byte) error {
	message := strings.TrimSpace(extractErrorMessage(payload))
	if message == "" {
		message = fmt.Sprintf("upstream returned status %d", status)
	}
	if gatewayPayloadLooksQuotaFailure(payload) {
		return &InvokeError{Code: ErrorCodeUpstreamRateLimited, Temporary: true, Cooldown: 45 * time.Second, Err: errors.New(message)}
	}
	if status >= 500 {
		return &InvokeError{Code: ErrorCodeUpstreamTemporarilyUnavailable, Temporary: true, Cooldown: 30 * time.Second, Err: errors.New(message)}
	}
	if gatewayPayloadLooksMiddleLayerFailure(payload) {
		return &InvokeError{Code: ErrorCodeUpstreamTemporarilyUnavailable, Temporary: true, Cooldown: 30 * time.Second, Err: errors.New(message)}
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound {
		return nil
	}
	if status == http.StatusTooManyRequests {
		return &InvokeError{Code: ErrorCodeUpstreamRateLimited, Temporary: true, Cooldown: 45 * time.Second, Err: errors.New(message)}
	}
	return nil
}

func gatewayPayloadLooksMiddleLayerFailure(payload []byte) bool {
	if len(bytes.TrimSpace(payload)) == 0 {
		return false
	}
	if looksLikeHTMLPayload(payload) {
		return true
	}
	code := strings.ToLower(strings.TrimSpace(extractErrorCode(payload)))
	message := strings.ToLower(strings.TrimSpace(extractErrorMessage(payload)))
	if strings.Contains(code, "no_available") ||
		strings.Contains(code, "no_available_key") ||
		strings.Contains(code, "no_available_channel") ||
		strings.Contains(code, "bad_response") ||
		strings.Contains(code, "timeout") ||
		strings.Contains(code, "overloaded") ||
		strings.Contains(code, "temporarily_unavailable") {
		return true
	}
	if code == "new_api_error" {
		for _, signal := range gatewayMiddleLayerSignals() {
			if strings.Contains(message, signal) {
				return true
			}
		}
	}
	for _, signal := range gatewayMiddleLayerSignals() {
		if strings.Contains(message, signal) {
			return true
		}
	}
	return false
}

func gatewayPayloadLooksQuotaFailure(payload []byte) bool {
	if len(bytes.TrimSpace(payload)) == 0 {
		return false
	}
	code := strings.ToLower(strings.TrimSpace(extractErrorCode(payload)))
	message := strings.ToLower(strings.TrimSpace(extractErrorMessage(payload)))
	for _, signal := range []string{
		"insufficient_quota",
		"quota_exceeded",
		"quota_exhausted",
		"rate_limit_exceeded",
		"billing_hard_limit_reached",
	} {
		if strings.Contains(code, signal) {
			return true
		}
	}
	for _, signal := range []string{
		"you exceeded your current quota",
		"insufficient quota",
		"quota exceeded",
		"quota exhausted",
		"daily limit exhausted",
		"monthly limit exhausted",
		"credit exhausted",
		"credits exhausted",
		"balance not enough",
		"not enough balance",
		"out of quota",
		"billing hard limit",
		"rate limit reached",
		"too many requests",
	} {
		if strings.Contains(message, signal) {
			return true
		}
	}
	if quotaFailureMessage(message) {
		return true
	}
	return false
}

func quotaFailureMessage(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	if message == "" {
		return false
	}
	for _, signal := range []string{
		"insufficient balance",
		"insufficient account balance",
		"account balance insufficient",
		"account balance is insufficient",
		"balance insufficient",
	} {
		if strings.Contains(message, signal) {
			return true
		}
	}
	return false
}

func gatewayMiddleLayerSignals() []string {
	return []string{
		"no available channel",
		"no available key",
		"no available accounts",
		"no available compatible accounts",
		"all available accounts exhausted",
		"channel:no_available_key",
		"bad response",
		"bad_response",
		"database unavailable",
		"usage unavailable",
		"temporarily unavailable",
		"temporary unavailable",
		"service unavailable",
		"upstream service temporarily unavailable",
		"upstream service overloaded",
		"upstream request failed",
		"upstream rate limit exceeded",
		"too many pending requests",
		"concurrency limit exceeded",
		"gateway timeout",
		"bad gateway",
		"cloudflare",
		"challenge response",
		"html challenge",
	}
}

func UpstreamFailureScope(account core.Account) string {
	parts := []string{strings.TrimSpace(string(account.Provider))}
	if proxy := strings.TrimSpace(account.EffectiveProxyURL); proxy != "" {
		parts = append(parts, "proxy="+proxy)
	} else {
		if upstream := upstreamEndpointScope(account); upstream != "" {
			parts = append(parts, "upstream="+upstream)
		} else {
			return strings.Join(parts, "|")
		}
	}
	return strings.Join(parts, "|")
}

func upstreamEndpointScope(account core.Account) string {
	switch account.Provider {
	case core.ProviderOpenAI:
		if usesChatGPTCodexBackend(account) {
			base := strings.TrimSpace(account.Credential.Metadata["codex_base_url"])
			if base == "" || sameUpstreamBaseURL(base, chatGPTCodexBaseURL, "") {
				return normalizeUpstreamBaseURL(chatGPTCodexBaseURL)
			}
			return normalizeUpstreamBaseURL(base)
		}
		base := firstNonEmptyMetadata(account.Credential.Metadata, "base_url", "endpoint")
		if base == "" || sameUpstreamBaseURL(base, openAIBaseURL, "/v1") {
			return normalizeUpstreamBaseURL(openAIBaseURL)
		}
		return normalizeUpstreamBaseURL(base)
	case core.ProviderClaude:
		base := firstNonEmptyMetadata(account.Credential.Metadata, "base_url", "endpoint")
		if base == "" || sameUpstreamBaseURL(base, claudeBaseURL, "/v1") {
			return normalizeUpstreamBaseURL(claudeBaseURL)
		}
		return normalizeUpstreamBaseURL(base)
	default:
		if base := firstNonEmptyMetadata(account.Credential.Metadata, "base_url", "endpoint"); base != "" {
			return normalizeUpstreamBaseURL(base)
		}
		return ""
	}
}

func firstNonEmptyMetadata(metadata map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(metadata[key]); value != "" {
			return value
		}
	}
	return ""
}

func normalizeUpstreamBaseURL(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimRight(base, "/")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.EscapedPath(), "/")
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func sameUpstreamBaseURL(a, b, defaultVersionPath string) bool {
	a = normalizeUpstreamBaseURLForCompare(a, defaultVersionPath)
	b = normalizeUpstreamBaseURLForCompare(b, defaultVersionPath)
	return a != "" && a == b
}

func normalizeUpstreamBaseURLForCompare(base string, defaultVersionPath string) string {
	normalized := normalizeUpstreamBaseURL(base)
	if normalized == "" || strings.TrimSpace(defaultVersionPath) == "" {
		return normalized
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return normalized
	}
	versionPath := "/" + strings.Trim(strings.TrimSpace(defaultVersionPath), "/")
	if parsed.EscapedPath() == versionPath {
		parsed.Path = ""
	}
	return parsed.String()
}

func invokeErrorMessage(err error) string {
	var invokeErr *InvokeError
	if errors.As(err, &invokeErr) && invokeErr != nil && invokeErr.Err != nil {
		return strings.TrimSpace(invokeErr.Err.Error())
	}
	if err == nil {
		return ""
	}
	return strings.TrimSpace(err.Error())
}

func isGatewayAPIKeyDisabledPayload(payload []byte) bool {
	code := strings.TrimSpace(extractErrorCode(payload))
	if strings.EqualFold(code, "API_KEY_DISABLED") {
		return true
	}
	message := strings.ToLower(strings.TrimSpace(extractErrorMessage(payload)))
	return strings.Contains(message, "api key is disabled")
}

func isProviderBannedMessage(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	if normalized == "" {
		return false
	}
	accountSignals := []string{
		"account",
		"workspace",
		"organization",
		"org",
		"user",
	}
	banSignals := []string{
		"banned",
		"ban",
		"suspended",
		"suspension",
		"terminated",
		"termination",
		"deactivated",
		"disabled",
		"closed",
	}
	hasAccountSignal := false
	for _, signal := range accountSignals {
		if strings.Contains(normalized, signal) {
			hasAccountSignal = true
			break
		}
	}
	if !hasAccountSignal {
		return false
	}
	for _, signal := range banSignals {
		if strings.Contains(normalized, signal) {
			return true
		}
	}
	return false
}

func extractErrorMessage(payload []byte) string {
	if looksLikeHTMLPayload(payload) {
		return "upstream returned HTML challenge response"
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return string(payload)
	}
	if text := nestedString(body, "error", "message"); text != "" {
		return text
	}
	if text := nestedString(body, "message"); text != "" {
		return text
	}
	if text := nestedString(body, "error"); text != "" {
		return text
	}
	return string(payload)
}

func extractErrorCode(payload []byte) string {
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return ""
	}
	if text := nestedString(body, "error", "code"); text != "" {
		return text
	}
	if text := nestedString(body, "error", "type"); text != "" {
		return text
	}
	if text := nestedString(body, "code"); text != "" {
		return text
	}
	if text := nestedString(body, "type"); text != "" {
		return text
	}
	return ""
}

func looksLikeHTMLPayload(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return false
	}
	lowered := strings.ToLower(string(trimmed))
	return bytes.HasPrefix(trimmed, []byte("<")) ||
		strings.Contains(lowered, "<html") ||
		strings.Contains(lowered, "cloudflare") ||
		strings.Contains(lowered, "challenge-platform")
}

func nestedString(input map[string]any, path ...string) string {
	var current any = input
	for _, key := range path {
		asMap, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = asMap[key]
	}
	value, _ := current.(string)
	return value
}

func resolveEndpoint(account core.Account, defaultBase, suffix string) string {
	base := strings.TrimSpace(account.Credential.Metadata["base_url"])
	if base == "" {
		base = strings.TrimSpace(account.Credential.Metadata["endpoint"])
	}
	if base == "" {
		base = defaultBase
	}
	return appendEndpointSuffix(base, suffix)
}

func appendEndpointSuffix(base, suffix string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	suffix = normalizeEndpointSuffix(suffix)
	if suffix == "" {
		return base
	}
	if strings.ContainsAny(base, "?#") && strings.Contains(base, "://") {
		if parsed, err := url.Parse(base); err == nil {
			parsed.Path = appendEndpointPath(parsed.Path, suffix)
			return parsed.String()
		}
	}
	return appendEndpointPath(base, suffix)
}

func normalizeEndpointSuffix(suffix string) string {
	suffix = strings.TrimSpace(suffix)
	if suffix == "" {
		return ""
	}
	return "/" + strings.TrimLeft(suffix, "/")
}

func appendEndpointPath(base, suffix string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, suffix) {
		return base
	}
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(suffix, "/v1/") {
		return base + strings.TrimPrefix(suffix, "/v1")
	}
	return base + suffix
}

func accessTokenForAccount(account core.Account) (string, error) {
	token := strings.TrimSpace(account.Credential.AccessToken)
	if token != "" {
		return token, nil
	}
	return "", &InvokeError{
		Code:      ErrorCodeMissingCredential,
		Temporary: false,
		Err:       errors.New("account credential is empty"),
	}
}
