package failover

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/32ns/ai-gateway/internal/accounts"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
)

type Engine struct {
	pool              *accounts.Pool
	registry          *providers.Registry
	refreshMu         sync.Mutex
	refreshes         map[string]*refreshCall
	imageMu           sync.Mutex
	imageBusy         map[string]struct{}
	imageWake         chan struct{}
	upstreamMu        sync.Mutex
	upstreamBreakers  map[string]upstreamBreakerState
	routeBindingMu    sync.RWMutex
	routeBindings     map[routeBindingKey]string
	routeBindingOrder []routeBindingKey
}

type refreshCall struct {
	done    chan struct{}
	account core.Account
	err     error
}

type routeBindingKey struct {
	provider core.ProviderKind
	routeKey string
}

type upstreamBreakerState struct {
	failures      int
	windowStart   time.Time
	cooldownUntil time.Time
}

type InvocationResult struct {
	Response *core.GatewayResponse
	Attempts []core.AttemptRecord
}

type ResponsesResult struct {
	Response *core.GatewayResponse
	Attempts []core.AttemptRecord
}

type EmbeddingResult struct {
	Response *core.EmbeddingResponse
	Attempts []core.AttemptRecord
}

type ModerationResult struct {
	Response *core.ModerationResponse
	Attempts []core.AttemptRecord
}

type ImageGenerationResult struct {
	Response *core.ImageGenerationResponse
	Attempts []core.AttemptRecord
}

type ImageMultipartResult struct {
	Response *core.ImageMultipartResponse
	Attempts []core.AttemptRecord
}

type AudioSpeechResult struct {
	Response *core.AudioSpeechResponse
	Attempts []core.AttemptRecord
}

type AudioMultipartResult struct {
	Response *core.AudioMultipartResponse
	Attempts []core.AttemptRecord
}

type TokenCountResult struct {
	Response *core.TokenCountResponse
	Attempts []core.AttemptRecord
}

type StreamOpenResult struct {
	Session  *providers.StreamSession
	Decision core.RouteDecision
	Attempts []core.AttemptRecord
	Modality failureModality
}

type ResponsesWebSocketOpenResult struct {
	Session  providers.ResponsesWebSocketSession
	Decision core.RouteDecision
	Attempts []core.AttemptRecord
}

type attemptExecutor[T any] struct {
	unsupportedStatus string
	unsupportedCode   string
	unsupportedName   string
	modality          failureModality
	noAccount         func([]core.AttemptRecord, core.ProviderKind) []core.AttemptRecord
	selectCandidates  func(*Engine, core.ProviderKind, *core.APIClient) []core.Account
	ignoreBreakers    bool
	supported         func(providers.Adapter) bool
	invoke            func(context.Context, providers.Adapter, core.RouteDecision) (T, error)
}

type attemptOutcome[T any] struct {
	response T
	decision core.RouteDecision
}

type accountCandidatePhase struct {
	backups                  bool
	includeBreakerCandidates bool
}

var strictPrimaryBeforeBackupPhases = []accountCandidatePhase{
	{backups: false, includeBreakerCandidates: false},
	{backups: false, includeBreakerCandidates: true},
	{backups: true, includeBreakerCandidates: false},
	{backups: true, includeBreakerCandidates: true},
}

type imageAttemptExecutor[T any] struct {
	unsupportedStatus string
	unsupportedCode   string
	unsupportedName   string
	quotaUnits        int64
	supported         func(providers.Adapter) bool
	invoke            func(context.Context, providers.Adapter, core.RouteDecision) (T, error)
}

const maxRouteBindings = 8192
const maxUpstreamBreakerEntries = 4096
const sharedUpstreamFailureWindow = 2 * time.Minute
const sharedUpstreamBreakerThreshold = 1
const cacheAffinitySoftRetryBaseDelay = 900 * time.Millisecond
const cacheAffinitySoftRetryJitter = 700 * time.Millisecond
const cacheAffinitySoftRetryMaxRetries = 1

var cacheAffinitySoftRetryDelayFunc = cacheAffinitySoftRetryDelay

type failureModality int

const (
	failureModalityDefault failureModality = iota
	failureModalityImage
	failureModalityAccount
)

type ExecutionError struct {
	Attempts []core.AttemptRecord
}

type AttemptObserver interface {
	AttemptStarted(core.AttemptRecord)
	AttemptFinished(core.AttemptRecord)
}

type attemptObserverContextKey struct{}
type attemptTimeoutContextKey struct{}

func WithAttemptObserver(ctx context.Context, observer AttemptObserver) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, attemptObserverContextKey{}, observer)
}

func WithAttemptTimeout(ctx context.Context, timeout time.Duration) context.Context {
	if timeout <= 0 {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, attemptTimeoutContextKey{}, timeout)
}

func attemptObserverFromContext(ctx context.Context) AttemptObserver {
	observer, _ := ctx.Value(attemptObserverContextKey{}).(AttemptObserver)
	return observer
}

func attemptTimeoutFromContext(ctx context.Context) time.Duration {
	timeout, _ := ctx.Value(attemptTimeoutContextKey{}).(time.Duration)
	return timeout
}

func notifyAttemptStarted(ctx context.Context, attempt core.AttemptRecord) {
	if observer := attemptObserverFromContext(ctx); observer != nil {
		observer.AttemptStarted(attempt)
	}
}

func notifyAttemptFinished(ctx context.Context, attempt core.AttemptRecord) {
	if observer := attemptObserverFromContext(ctx); observer != nil {
		observer.AttemptFinished(attempt)
	}
}

func NotifyAttemptStarted(ctx context.Context, attempt core.AttemptRecord) {
	notifyAttemptStarted(ctx, attempt)
}

func NotifyAttemptFinished(ctx context.Context, attempt core.AttemptRecord) {
	notifyAttemptFinished(ctx, attempt)
}

func (e *ExecutionError) Error() string {
	if e == nil {
		return ""
	}
	return "all upstream attempts failed"
}

func (e *ExecutionError) Summary() string {
	if e == nil || len(e.Attempts) == 0 {
		return "all upstream attempts failed"
	}

	message := "all upstream attempts failed:"
	for _, attempt := range e.Attempts {
		label := string(attempt.Provider)
		if attempt.AccountLabel != "" {
			label += "/" + attempt.AccountLabel
		}
		message += " " + label + ":" + attempt.Status
		if attempt.ErrorCode != "" {
			message += "(" + attempt.ErrorCode + ")"
		}
		if attempt.ErrorMessage != "" {
			message += "=" + attempt.ErrorMessage
		}
		message += ";"
	}
	return message
}

func NewEngine(pool *accounts.Pool, registry *providers.Registry) *Engine {
	return &Engine{pool: pool, registry: registry}
}

type imageAccountLease struct {
	engine    *Engine
	accountID string
	once      sync.Once
}

func (l *imageAccountLease) release() {
	if l == nil || l.engine == nil || strings.TrimSpace(l.accountID) == "" {
		return
	}
	l.once.Do(func() {
		l.engine.releaseImageAccount(l.accountID)
	})
}

func (e *Engine) tryAcquireImageAccount(account core.Account) (*imageAccountLease, bool) {
	accountID := strings.TrimSpace(account.ID)
	if e == nil || accountID == "" {
		return nil, false
	}
	e.imageMu.Lock()
	defer e.imageMu.Unlock()
	if e.imageBusy == nil {
		e.imageBusy = make(map[string]struct{})
	}
	if _, busy := e.imageBusy[accountID]; busy {
		return nil, false
	}
	e.imageBusy[accountID] = struct{}{}
	return &imageAccountLease{engine: e, accountID: accountID}, true
}

func (e *Engine) releaseImageAccount(accountID string) {
	if e == nil || strings.TrimSpace(accountID) == "" {
		return
	}
	e.imageMu.Lock()
	if len(e.imageBusy) > 0 {
		delete(e.imageBusy, strings.TrimSpace(accountID))
	}
	wake := e.imageWake
	e.imageWake = nil
	e.imageMu.Unlock()
	if wake != nil {
		close(wake)
	}
}

func (e *Engine) waitForImageAccount(ctx context.Context, pending []string) error {
	if e == nil {
		return nil
	}
	e.imageMu.Lock()
	for _, accountID := range pending {
		accountID = strings.TrimSpace(accountID)
		if accountID == "" {
			continue
		}
		if _, busy := e.imageBusy[accountID]; !busy {
			e.imageMu.Unlock()
			return nil
		}
	}
	wake := e.imageWake
	if wake == nil {
		wake = make(chan struct{})
		e.imageWake = wake
	}
	e.imageMu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wake:
		return nil
	}
}

func (e *Engine) filterBrokenUpstreamCandidates(candidates []core.Account) []core.Account {
	if e == nil || len(candidates) == 0 {
		return candidates
	}
	now := time.Now().UTC()
	filtered := make([]core.Account, 0, len(candidates))
	for _, account := range candidates {
		if e.upstreamBreakerActive(account, now) {
			continue
		}
		filtered = append(filtered, account)
	}
	if len(filtered) == len(candidates) {
		return candidates
	}
	return filtered
}

func (e *Engine) upstreamBreakerActive(account core.Account, now time.Time) bool {
	scope := providers.UpstreamFailureScope(account)
	if e == nil || scope == "" {
		return false
	}
	e.upstreamMu.Lock()
	active := false
	if state, ok := e.upstreamBreakers[scope]; ok {
		if state.cooldownUntil.After(now) {
			active = true
		} else {
			delete(e.upstreamBreakers, scope)
		}
	}
	e.upstreamMu.Unlock()
	return active
}

func (e *Engine) recordSharedUpstreamFailure(policy providers.FailurePolicy) {
	if e == nil || policy.Scope != providers.FailureScopeSharedUpstream {
		return
	}
	scope := strings.TrimSpace(policy.SharedUpstreamScope)
	if scope == "" {
		return
	}
	now := time.Now().UTC()
	cooldown := policy.Cooldown
	if cooldown <= 0 {
		cooldown = 20 * time.Second
	}
	e.upstreamMu.Lock()
	if e.upstreamBreakers == nil || len(e.upstreamBreakers) >= maxUpstreamBreakerEntries {
		e.upstreamBreakers = make(map[string]upstreamBreakerState)
	}
	state := e.upstreamBreakers[scope]
	if state.windowStart.IsZero() || now.Sub(state.windowStart) > sharedUpstreamFailureWindow {
		state.windowStart = now
		state.failures = 0
	}
	state.failures++
	if state.failures >= sharedUpstreamBreakerThreshold {
		state.cooldownUntil = now.Add(cooldown)
	}
	e.upstreamBreakers[scope] = state
	e.upstreamMu.Unlock()
}

func (e *Engine) recordUpstreamSuccess(account core.Account) {
	if e == nil {
		return
	}
	scope := providers.UpstreamFailureScope(account)
	if scope == "" {
		return
	}
	e.upstreamMu.Lock()
	if len(e.upstreamBreakers) > 0 {
		delete(e.upstreamBreakers, scope)
	}
	e.upstreamMu.Unlock()
}

func (e *Engine) nextImageAccount(ctx context.Context, provider core.ProviderKind, client *core.APIClient, attempted map[string]struct{}) (core.Account, *imageAccountLease, bool, error) {
	if e == nil || e.pool == nil {
		return core.Account{}, nil, false, nil
	}
retry:
	for {
		candidates := e.applyRouteBinding(provider, client, e.pool.ImageCandidates(provider, client))
		for _, phase := range strictPrimaryBeforeBackupPhases {
			if account, lease, pending := e.nextImageAccountFromCandidates(candidates, attempted, phase.backups, phase.includeBreakerCandidates); lease != nil {
				return account, lease, true, nil
			} else if len(pending) > 0 {
				if err := e.waitForImageAccount(ctx, pending); err != nil {
					return core.Account{}, nil, true, err
				}
				continue retry
			}
		}
		return core.Account{}, nil, false, nil
	}
}

func (e *Engine) nextImageAccountFromCandidates(candidates []core.Account, attempted map[string]struct{}, backups bool, includeBreakerCandidates bool) (core.Account, *imageAccountLease, []string) {
	pending := make([]string, 0, len(candidates))
	now := time.Now().UTC()
	for _, account := range candidates {
		if account.Backup != backups {
			continue
		}
		accountID := strings.TrimSpace(account.ID)
		if accountID == "" {
			continue
		}
		if _, ok := attempted[accountID]; ok {
			continue
		}
		if e.upstreamBreakerActive(account, now) != includeBreakerCandidates {
			continue
		}
		pending = append(pending, accountID)
		if lease, ok := e.tryAcquireImageAccount(account); ok {
			return account, lease, pending
		}
	}
	return core.Account{}, nil, pending
}

type imageLeaseStream struct {
	stream providers.Stream
	lease  *imageAccountLease
}

func (s *imageLeaseStream) Next() (*core.StreamEvent, error) {
	if s == nil || s.stream == nil {
		return nil, io.EOF
	}
	return s.stream.Next()
}

func (s *imageLeaseStream) Close() error {
	var err error
	if s != nil && s.stream != nil {
		err = s.stream.Close()
	}
	if s != nil && s.lease != nil {
		s.lease.release()
	}
	return err
}

func streamSessionWithImageLease(session *providers.StreamSession, lease *imageAccountLease) *providers.StreamSession {
	if session == nil || lease == nil {
		if lease != nil {
			lease.release()
		}
		return session
	}
	session.Stream = &imageLeaseStream{stream: session.Stream, lease: lease}
	return session
}

func (e *Engine) ImageGenerationCandidateCount(plan core.RoutePlan, client *core.APIClient) int {
	return e.imageCandidateCount(plan, client, func(adapter providers.Adapter) bool {
		_, ok := adapter.(providers.ImageGenerationAdapter)
		return ok
	})
}

func (e *Engine) ImageMultipartCandidateCount(plan core.RoutePlan, client *core.APIClient) int {
	return e.imageCandidateCount(plan, client, func(adapter providers.Adapter) bool {
		_, ok := adapter.(providers.ImageMultipartAdapter)
		return ok
	})
}

func (e *Engine) SupportsResponsesWebSocket(plan core.RoutePlan) bool {
	if e == nil || e.registry == nil {
		return false
	}
	for _, provider := range plan.Providers {
		adapter, ok := e.registry.Get(provider)
		if !ok {
			continue
		}
		if _, ok := adapter.(providers.ResponsesWebSocketAdapter); ok {
			return true
		}
	}
	return false
}

func (e *Engine) imageCandidateCount(plan core.RoutePlan, client *core.APIClient, supports func(providers.Adapter) bool) int {
	if e == nil || e.pool == nil || e.registry == nil {
		return 0
	}
	seen := make(map[string]struct{})
	for _, provider := range plan.Providers {
		adapter, ok := e.registry.Get(provider)
		if !ok || (supports != nil && !supports(adapter)) {
			continue
		}
		for _, account := range e.pool.ImageCandidates(provider, client) {
			accountID := strings.TrimSpace(account.ID)
			if accountID == "" {
				continue
			}
			seen[accountID] = struct{}{}
		}
	}
	return len(seen)
}

func (e *Engine) prepareAccount(ctx context.Context, adapter providers.Adapter, account core.Account) (core.Account, error) {
	if !providers.CredentialNeedsRefresh(account) {
		return account, nil
	}
	if current, ok := e.currentAccount(account.ID); ok {
		if strings.TrimSpace(account.EffectiveProxyURL) != "" {
			current.EffectiveProxyURL = account.EffectiveProxyURL
		}
		account = current
		if !providers.CredentialNeedsRefresh(account) {
			return account, nil
		}
	}

	refreshingAdapter, ok := adapter.(providers.RefreshingAdapter)
	if !ok {
		return account, &providers.InvokeError{
			Code:      providers.ErrorCodeCredentialExpired,
			Temporary: false,
			Err:       errors.New("account credential expired and provider does not support refresh"),
		}
	}

	return e.refreshAccount(ctx, refreshingAdapter, account)
}

func (e *Engine) currentAccount(accountID string) (core.Account, bool) {
	if e == nil || e.pool == nil || strings.TrimSpace(accountID) == "" {
		return core.Account{}, false
	}
	account, err := e.pool.GetAccount(accountID)
	return account, err == nil
}

func (e *Engine) refreshAccount(ctx context.Context, adapter providers.RefreshingAdapter, account core.Account) (core.Account, error) {
	if e == nil || strings.TrimSpace(account.ID) == "" {
		return refreshAccount(ctx, e, adapter, account)
	}

	e.refreshMu.Lock()
	if e.refreshes == nil {
		e.refreshes = make(map[string]*refreshCall)
	}
	if call := e.refreshes[account.ID]; call != nil {
		e.refreshMu.Unlock()
		select {
		case <-call.done:
			return call.account, call.err
		case <-ctx.Done():
			return account, ctx.Err()
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	e.refreshes[account.ID] = call
	e.refreshMu.Unlock()

	if current, ok := e.currentAccount(account.ID); ok {
		if strings.TrimSpace(account.EffectiveProxyURL) != "" {
			current.EffectiveProxyURL = account.EffectiveProxyURL
		}
		forceRefresh := strings.TrimSpace(account.Credential.Metadata["force_oauth_refresh"]) == "true"
		accessTokenChanged := strings.TrimSpace(current.Credential.AccessToken) != "" && current.Credential.AccessToken != account.Credential.AccessToken
		refreshTokenChanged := strings.TrimSpace(current.Credential.RefreshToken) != "" && current.Credential.RefreshToken != account.Credential.RefreshToken
		if accessTokenChanged && !providers.CredentialNeedsRefresh(current) {
			call.account = current
			e.refreshMu.Lock()
			delete(e.refreshes, account.ID)
			close(call.done)
			e.refreshMu.Unlock()
			return call.account, nil
		}
		if accessTokenChanged || refreshTokenChanged {
			if forceRefresh {
				if current.Credential.Metadata == nil {
					current.Credential.Metadata = map[string]string{}
				}
				current.Credential.Metadata["force_oauth_refresh"] = "true"
				current.Credential.ExpiresAt = account.Credential.ExpiresAt
			}
			account = current
		}
	}

	call.account, call.err = refreshAccount(ctx, e, adapter, account)
	e.refreshMu.Lock()
	delete(e.refreshes, account.ID)
	close(call.done)
	e.refreshMu.Unlock()
	return call.account, call.err
}

func refreshAccount(ctx context.Context, engine *Engine, adapter providers.RefreshingAdapter, account core.Account) (core.Account, error) {
	if engine != nil && engine.pool != nil {
		_ = engine.pool.MarkRefreshing(account)
	}
	credential, err := adapter.Refresh(ctx, account)
	if err != nil {
		return account, err
	}

	if engine == nil || engine.pool == nil {
		account.Credential = credential
		return account, nil
	}
	refreshed, err := engine.pool.MarkCredentialRefreshed(account, credential)
	if err != nil {
		return account, &providers.InvokeError{
			Code:      providers.ErrorCodeCredentialStateUpdateFailed,
			Temporary: true,
			Cooldown:  5 * time.Second,
			Err:       err,
		}
	}
	if strings.TrimSpace(account.EffectiveProxyURL) != "" {
		refreshed.EffectiveProxyURL = account.EffectiveProxyURL
	}
	return refreshed, nil
}

func appendNoAdapterAttempt(attempts []core.AttemptRecord, provider core.ProviderKind) []core.AttemptRecord {
	return append(attempts, core.AttemptRecord{
		Provider:     provider,
		Status:       AttemptStatusNoAdapter,
		ErrorCode:    ErrorCodeNoAdapter,
		ErrorMessage: fmt.Sprintf("provider %q is not registered", provider),
	})
}

func appendNoAccountAttempt(attempts []core.AttemptRecord, provider core.ProviderKind) []core.AttemptRecord {
	return append(attempts, core.AttemptRecord{
		Provider:     provider,
		Status:       AttemptStatusNoAccount,
		ErrorCode:    ErrorCodeNoAccount,
		ErrorMessage: fmt.Sprintf("provider %q has no eligible accounts", provider),
	})
}

func appendBoundAccountNotFoundAttempt(attempts []core.AttemptRecord, provider core.ProviderKind, accountID string) []core.AttemptRecord {
	return append(attempts, core.AttemptRecord{
		Provider:     provider,
		AccountID:    strings.TrimSpace(accountID),
		Status:       AttemptStatusBoundAccountUnavailable,
		ErrorCode:    ErrorCodeBoundAccountUnavailable,
		ErrorMessage: fmt.Sprintf("bound account %q is not eligible for provider %q", strings.TrimSpace(accountID), provider),
	})
}

func appendUnsupportedAttempt(attempts []core.AttemptRecord, provider core.ProviderKind, status, code, capability string) []core.AttemptRecord {
	return append(attempts, core.AttemptRecord{
		Provider:     provider,
		Status:       status,
		ErrorCode:    code,
		ErrorMessage: fmt.Sprintf("provider %q does not support %s", provider, capability),
	})
}

func invokeErrorAttempt(provider core.ProviderKind, account core.Account, status string, err error) core.AttemptRecord {
	attempt := core.AttemptRecord{
		Provider:     provider,
		AccountID:    account.ID,
		AccountLabel: account.Label,
		Status:       status,
		ErrorMessage: err.Error(),
		Temporary:    providers.IsTemporary(err),
	}
	if code := providers.ErrorCode(err); code != "" {
		attempt.ErrorCode = code
	}
	return attempt
}

func runningAttempt(provider core.ProviderKind, account core.Account) core.AttemptRecord {
	return core.AttemptRecord{
		Provider:     provider,
		AccountID:    account.ID,
		AccountLabel: account.Label,
		Status:       "running",
	}
}

func defaultCandidates(e *Engine, provider core.ProviderKind, client *core.APIClient) []core.Account {
	if e == nil || e.pool == nil {
		return nil
	}
	return e.applyRouteBinding(provider, client, e.pool.Candidates(provider, client))
}

func responsesCandidates(req *core.ResponsesRequest) func(*Engine, core.ProviderKind, *core.APIClient) []core.Account {
	return func(e *Engine, provider core.ProviderKind, client *core.APIClient) []core.Account {
		return e.responsesCandidates(provider, client, req)
	}
}

func gatewayRequestCandidates(req *core.GatewayRequest) func(*Engine, core.ProviderKind, *core.APIClient) []core.Account {
	return func(e *Engine, provider core.ProviderKind, client *core.APIClient) []core.Account {
		return e.gatewayRequestCandidates(provider, client, req)
	}
}

func gatewayRequestNoAccount(req *core.GatewayRequest) func([]core.AttemptRecord, core.ProviderKind) []core.AttemptRecord {
	return func(attempts []core.AttemptRecord, provider core.ProviderKind) []core.AttemptRecord {
		if req != nil && req.StrictAccountAffinity && strings.TrimSpace(req.PreferredAccountID) != "" {
			return appendBoundAccountNotFoundAttempt(attempts, provider, req.PreferredAccountID)
		}
		return appendNoAccountAttempt(attempts, provider)
	}
}

func gatewayRequestIgnoreBreakers(req *core.GatewayRequest) bool {
	return req != nil && req.StrictAccountAffinity && strings.TrimSpace(req.PreferredAccountID) != ""
}

func responsesIgnoreBreakers(req *core.ResponsesRequest) bool {
	return req != nil && req.StrictAccountAffinity && strings.TrimSpace(req.PreferredAccountID) != ""
}

func responsesNoAccount(req *core.ResponsesRequest) func([]core.AttemptRecord, core.ProviderKind) []core.AttemptRecord {
	return func(attempts []core.AttemptRecord, provider core.ProviderKind) []core.AttemptRecord {
		if req != nil && req.StrictAccountAffinity && strings.TrimSpace(req.PreferredAccountID) != "" {
			return appendBoundAccountNotFoundAttempt(attempts, provider, req.PreferredAccountID)
		}
		return appendNoAccountAttempt(attempts, provider)
	}
}

func (e *Engine) responsesCandidates(provider core.ProviderKind, client *core.APIClient, req *core.ResponsesRequest) []core.Account {
	if req == nil || !req.StrictAccountAffinity || strings.TrimSpace(req.PreferredAccountID) == "" {
		return filterExcludedResponseAccounts(defaultCandidates(e, provider, client), req)
	}
	account, ok := e.boundAccountCandidate(provider, client, req.PreferredAccountID)
	if !ok {
		return nil
	}
	return filterExcludedResponseAccounts([]core.Account{account}, req)
}

func (e *Engine) gatewayRequestCandidates(provider core.ProviderKind, client *core.APIClient, req *core.GatewayRequest) []core.Account {
	if req == nil || !req.StrictAccountAffinity || strings.TrimSpace(req.PreferredAccountID) == "" {
		return filterExcludedGatewayAccounts(defaultCandidates(e, provider, client), req)
	}
	account, ok := e.boundAccountCandidate(provider, client, req.PreferredAccountID)
	if !ok {
		return nil
	}
	return filterExcludedGatewayAccounts([]core.Account{account}, req)
}

func (e *Engine) applyRouteBinding(provider core.ProviderKind, client *core.APIClient, candidates []core.Account) []core.Account {
	if len(candidates) < 2 {
		return candidates
	}
	key, ok := routeBindingKeyForClient(provider, client)
	if !ok {
		return candidates
	}
	accountID, ok := e.boundRouteAccountID(key)
	if !ok || strings.TrimSpace(accountID) == "" || candidates[0].ID == accountID {
		return candidates
	}
	for i := 1; i < len(candidates); i++ {
		if candidates[i].ID != accountID {
			continue
		}
		if candidates[i].Backup && hasPrimaryAccountCandidate(candidates) {
			return candidates
		}
		out := append([]core.Account(nil), candidates...)
		bound := out[i]
		copy(out[1:i+1], out[0:i])
		out[0] = bound
		return out
	}
	return candidates
}

func hasPrimaryAccountCandidate(candidates []core.Account) bool {
	for _, candidate := range candidates {
		if !candidate.Backup {
			return true
		}
	}
	return false
}

func (e *Engine) bindRouteAccount(provider core.ProviderKind, client *core.APIClient, accountID string) {
	if e == nil {
		return
	}
	key, ok := routeBindingKeyForClient(provider, client)
	if !ok {
		return
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return
	}
	e.routeBindingMu.Lock()
	if e.routeBindings == nil {
		e.routeBindings = make(map[routeBindingKey]string)
	}
	if _, exists := e.routeBindings[key]; !exists {
		e.routeBindingOrder = append(e.routeBindingOrder, key)
	}
	e.routeBindings[key] = accountID
	for len(e.routeBindingOrder) > maxRouteBindings {
		oldest := e.routeBindingOrder[0]
		e.routeBindingOrder[0] = routeBindingKey{}
		e.routeBindingOrder = e.routeBindingOrder[1:]
		delete(e.routeBindings, oldest)
	}
	e.routeBindingMu.Unlock()
}

func (e *Engine) bindSuccessfulRouteAccount(provider core.ProviderKind, client *core.APIClient, accountID string) {
	e.bindRouteAccount(provider, client, accountID)
}

func (e *Engine) boundRouteAccountID(key routeBindingKey) (string, bool) {
	if e == nil {
		return "", false
	}
	e.routeBindingMu.RLock()
	accountID, ok := e.routeBindings[key]
	e.routeBindingMu.RUnlock()
	return accountID, ok
}

func routeBindingKeyForClient(provider core.ProviderKind, client *core.APIClient) (routeBindingKey, bool) {
	if client == nil {
		return routeBindingKey{}, false
	}
	routeKey := strings.TrimSpace(client.RouteAffinityKey)
	if routeKey == "" {
		return routeBindingKey{}, false
	}
	return routeBindingKey{provider: provider, routeKey: routeKey}, true
}

func filterExcludedResponseAccounts(candidates []core.Account, req *core.ResponsesRequest) []core.Account {
	if len(candidates) == 0 || req == nil || len(req.ExcludedAccountIDs) == 0 {
		return candidates
	}
	excluded := make(map[string]struct{}, len(req.ExcludedAccountIDs))
	for _, id := range req.ExcludedAccountIDs {
		if id = strings.TrimSpace(id); id != "" {
			excluded[id] = struct{}{}
		}
	}
	if len(excluded) == 0 {
		return candidates
	}
	filtered := make([]core.Account, 0, len(candidates))
	for _, candidate := range candidates {
		if _, ok := excluded[strings.TrimSpace(candidate.ID)]; ok {
			continue
		}
		filtered = append(filtered, candidate)
	}
	return filtered
}

func filterExcludedGatewayAccounts(candidates []core.Account, req *core.GatewayRequest) []core.Account {
	if len(candidates) == 0 || req == nil || len(req.ExcludedAccountIDs) == 0 {
		return candidates
	}
	excluded := make(map[string]struct{}, len(req.ExcludedAccountIDs))
	for _, id := range req.ExcludedAccountIDs {
		if id = strings.TrimSpace(id); id != "" {
			excluded[id] = struct{}{}
		}
	}
	if len(excluded) == 0 {
		return candidates
	}
	filtered := make([]core.Account, 0, len(candidates))
	for _, candidate := range candidates {
		if _, ok := excluded[strings.TrimSpace(candidate.ID)]; ok {
			continue
		}
		filtered = append(filtered, candidate)
	}
	return filtered
}

func (e *Engine) boundAccountCandidate(provider core.ProviderKind, client *core.APIClient, accountID string) (core.Account, bool) {
	if e == nil || e.pool == nil {
		return core.Account{}, false
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return core.Account{}, false
	}
	for _, account := range e.pool.Candidates(provider, client) {
		if strings.TrimSpace(account.ID) == accountID {
			return account, true
		}
	}
	account, err := e.pool.GetAccount(accountID)
	if err != nil {
		return core.Account{}, false
	}
	if account.Provider != provider {
		return core.Account{}, false
	}
	if client != nil && !strings.EqualFold(core.NormalizeAccountGroupName(account.Group), core.NormalizeAccountGroupName(client.AccountGroup)) {
		return core.Account{}, false
	}
	if !core.AccountAvailableForRouting(account, time.Now().UTC()) {
		return core.Account{}, false
	}
	return account, true
}

func contextDone(ctx context.Context) bool {
	return ctx != nil && ctx.Err() != nil
}

func shouldRetryFailover(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if contextDone(ctx) {
		return false
	}
	return providers.FailurePolicyFor(core.Account{}, err).RetryFailover
}

func shouldTryNextAccount(ctx context.Context, err error) bool {
	return shouldRetryFailover(ctx, err)
}

func shouldContinueFailover(ctx context.Context, err error, modality failureModality) bool {
	if modality == failureModalityImage && shouldStopImageFailover(err) {
		return false
	}
	return shouldTryNextAccount(ctx, err)
}

func shouldSoftRetrySameAccount(ctx context.Context, client *core.APIClient, err error, modality failureModality) bool {
	if client == nil || strings.TrimSpace(client.RouteAffinityKey) == "" {
		return false
	}
	if contextDone(ctx) {
		return false
	}
	if modality == failureModalityImage && shouldStopImageFailover(err) {
		return false
	}
	switch providers.ErrorCode(err) {
	case providers.ErrorCodeUpstreamTransportError,
		providers.ErrorCodeUpstreamReadError,
		providers.ErrorCodeUpstreamTemporarilyUnavailable,
		providers.ErrorCodeUpstreamServerError,
		providers.ErrorCodeUpstreamInvalidStreamChunk:
		return shouldRetryFailover(ctx, err)
	default:
		return false
	}
}

func cacheAffinitySoftRetryDelay(retryIndex int) time.Duration {
	if retryIndex < 1 {
		retryIndex = 1
	}
	multiplier := time.Duration(retryIndex)
	return cacheAffinitySoftRetryBaseDelay*multiplier + randomDuration(cacheAffinitySoftRetryJitter*multiplier)
}

func randomDuration(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return time.Duration(time.Now().UnixNano() % int64(max))
	}
	return time.Duration(binary.BigEndian.Uint64(buf[:]) % uint64(max))
}

func waitBeforeSoftRetry(ctx context.Context, retryIndex int) error {
	delay := cacheAffinitySoftRetryDelayFunc(retryIndex)
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func executeAcrossAccounts[T any](e *Engine, ctx context.Context, plan core.RoutePlan, client *core.APIClient, exec attemptExecutor[T]) (attemptOutcome[T], []core.AttemptRecord, error) {
	attempts := make([]core.AttemptRecord, 0, len(plan.Providers))
	if exec.selectCandidates == nil {
		exec.selectCandidates = defaultCandidates
	}
	if exec.noAccount == nil {
		exec.noAccount = appendNoAccountAttempt
	}
	for _, provider := range plan.Providers {
		adapter, ok := e.registry.Get(provider)
		if !ok {
			attempts = appendNoAdapterAttempt(attempts, provider)
			continue
		}
		if exec.supported != nil && !exec.supported(adapter) {
			attempts = appendUnsupportedAttempt(attempts, provider, exec.unsupportedStatus, exec.unsupportedCode, exec.unsupportedName)
			continue
		}
		candidates := exec.selectCandidates(e, provider, client)
		if len(candidates) == 0 {
			attempts = exec.noAccount(attempts, provider)
			continue
		}
		attempted := make(map[string]struct{}, len(candidates))
		for _, phase := range strictPrimaryBeforeBackupPhases {
			if exec.ignoreBreakers && phase.includeBreakerCandidates {
				continue
			}
			for _, account := range candidates {
				if account.Backup != phase.backups {
					continue
				}
				accountID := strings.TrimSpace(account.ID)
				if accountID != "" {
					if _, ok := attempted[accountID]; ok {
						continue
					}
				}
				if !exec.ignoreBreakers {
					breakerActive := e.upstreamBreakerActive(account, time.Now().UTC())
					if breakerActive != phase.includeBreakerCandidates {
						continue
					}
				}
				if accountID != "" {
					attempted[accountID] = struct{}{}
				}
				outcome, nextAttempts, ok, err := executeAccountAttempt(e, ctx, plan, client, provider, adapter, account, attempts, exec)
				attempts = nextAttempts
				if ok {
					e.bindSuccessfulRouteAccount(provider, client, outcome.decision.Account.ID)
				}
				if ok || err != nil {
					return outcome, attempts, err
				}
			}
		}
	}
	var zero attemptOutcome[T]
	return zero, attempts, &ExecutionError{Attempts: attempts}
}

func executeAccountAttempt[T any](e *Engine, ctx context.Context, plan core.RoutePlan, client *core.APIClient, provider core.ProviderKind, adapter providers.Adapter, account core.Account, attempts []core.AttemptRecord, exec attemptExecutor[T]) (attemptOutcome[T], []core.AttemptRecord, bool, error) {
	var zero attemptOutcome[T]
	notifyAttemptStarted(ctx, runningAttempt(provider, account))
	attemptCtx := ctx
	var cancel context.CancelFunc
	if timeout := attemptTimeoutFromContext(ctx); timeout > 0 {
		attemptCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	account, err := e.prepareAccount(attemptCtx, adapter, account)
	if err != nil {
		err = accountAttemptTimeoutError(ctx, attemptCtx, err)
		e.markFailureForModality(account, err, failureModalityAccount)
		attempt := invokeErrorAttempt(provider, account, "refresh_error", err)
		notifyAttemptFinished(ctx, attempt)
		attempts = append(attempts, attempt)
		if !shouldContinueFailover(ctx, err, failureModalityAccount) {
			return zero, attempts, false, &ExecutionError{Attempts: attempts}
		}
		return zero, attempts, false, nil
	}

	decision := core.RouteDecision{
		Provider: provider,
		Account:  account,
		Model:    plan.Model,
		Reason:   plan.Reason,
	}
	resp, finalDecision, invokeAttempts, errorStatus, err := invokeWithRefresh(e, attemptCtx, adapter, decision, exec.invoke)
	attempts = append(attempts, invokeAttempts...)
	if err == nil {
		success := core.AttemptRecord{Provider: provider, AccountID: finalDecision.Account.ID, AccountLabel: finalDecision.Account.Label, Status: "ok"}
		notifyAttemptFinished(ctx, success)
		attempts = append(attempts, success)
		e.recordUpstreamSuccess(finalDecision.Account)
		_ = e.pool.MarkSuccess(finalDecision.Account)
		return attemptOutcome[T]{response: resp, decision: finalDecision}, attempts, true, nil
	}

	failureModality := exec.modality
	if errorStatus == "refresh_error" {
		failureModality = failureModalityAccount
	}
	err = accountAttemptTimeoutError(ctx, attemptCtx, err)
	attempt := invokeErrorAttempt(provider, finalDecision.Account, errorStatus, err)
	notifyAttemptFinished(ctx, attempt)
	attempts = append(attempts, attempt)
	softRetryDecision := finalDecision
	softRetryErr := err
	for retryIndex := 1; retryIndex <= cacheAffinitySoftRetryMaxRetries && !contextDone(attemptCtx) && shouldSoftRetrySameAccount(ctx, client, softRetryErr, failureModality); retryIndex++ {
		if waitErr := waitBeforeSoftRetry(ctx, retryIndex); waitErr != nil {
			return zero, attempts, false, waitErr
		}
		retryResp, retryDecision, retryAttempts, retryErrorStatus, retryErr := invokeWithRefresh(e, attemptCtx, adapter, softRetryDecision, exec.invoke)
		attempts = append(attempts, retryAttempts...)
		if retryErr == nil {
			success := core.AttemptRecord{Provider: provider, AccountID: retryDecision.Account.ID, AccountLabel: retryDecision.Account.Label, Status: "ok"}
			notifyAttemptFinished(ctx, success)
			attempts = append(attempts, success)
			e.recordUpstreamSuccess(retryDecision.Account)
			_ = e.pool.MarkSuccess(retryDecision.Account)
			return attemptOutcome[T]{response: retryResp, decision: retryDecision}, attempts, true, nil
		}
		retryErr = accountAttemptTimeoutError(ctx, attemptCtx, retryErr)
		if retryErrorStatus == "refresh_error" {
			failureModality = failureModalityAccount
		}
		softRetryDecision = retryDecision
		softRetryErr = retryErr
		attempt = invokeErrorAttempt(provider, retryDecision.Account, retryErrorStatus, retryErr)
		notifyAttemptFinished(ctx, attempt)
		attempts = append(attempts, attempt)
	}
	finalDecision = softRetryDecision
	err = softRetryErr
	e.markFailureForModality(finalDecision.Account, err, failureModality)
	if !shouldContinueFailover(ctx, err, failureModality) {
		return zero, attempts, false, &ExecutionError{Attempts: attempts}
	}
	return zero, attempts, false, nil
}

func accountAttemptTimeoutError(parentCtx context.Context, attemptCtx context.Context, err error) error {
	if err == nil || attemptCtx == nil {
		return err
	}
	if !errors.Is(attemptCtx.Err(), context.DeadlineExceeded) || contextDone(parentCtx) {
		return err
	}
	if providers.ErrorCode(err) != "" {
		return err
	}
	return &providers.InvokeError{
		Code:      providers.ErrorCodeUpstreamTransportError,
		Temporary: true,
		Cooldown:  30 * time.Second,
		Err:       err,
	}
}

func invokeWithRefresh[T any](e *Engine, ctx context.Context, adapter providers.Adapter, decision core.RouteDecision, invoke func(context.Context, providers.Adapter, core.RouteDecision) (T, error)) (T, core.RouteDecision, []core.AttemptRecord, string, error) {
	resp, err := invoke(ctx, adapter, decision)
	if err == nil {
		return resp, decision, nil, "", nil
	}
	if !providers.FailurePolicyFor(decision.Account, err).RefreshCredential {
		var zero T
		return zero, decision, nil, "invoke_error", err
	}
	refreshed, refreshErr := e.refreshAfterInvokeAuthError(ctx, adapter, decision.Account, err)
	if refreshErr != nil {
		var zero T
		return zero, decision, nil, "refresh_error", refreshErr
	}
	if strings.TrimSpace(refreshed.ID) == "" {
		var zero T
		return zero, decision, nil, "invoke_error", err
	}
	failed := invokeErrorAttempt(decision.Provider, decision.Account, "invoke_error", err)
	notifyAttemptFinished(ctx, failed)
	decision.Account = refreshed
	notifyAttemptStarted(ctx, runningAttempt(decision.Provider, refreshed))
	resp, err = invoke(ctx, adapter, decision)
	return resp, decision, []core.AttemptRecord{failed}, "invoke_error", err
}

func executeAcrossImageAccounts[T any](e *Engine, ctx context.Context, plan core.RoutePlan, client *core.APIClient, exec imageAttemptExecutor[T]) (attemptOutcome[T], []core.AttemptRecord, *imageAccountLease, error) {
	attempts := make([]core.AttemptRecord, 0, len(plan.Providers))
	for _, provider := range plan.Providers {
		adapter, ok := e.registry.Get(provider)
		if !ok {
			attempts = appendNoAdapterAttempt(attempts, provider)
			continue
		}
		if exec.supported != nil && !exec.supported(adapter) {
			attempts = appendUnsupportedAttempt(attempts, provider, exec.unsupportedStatus, exec.unsupportedCode, exec.unsupportedName)
			continue
		}

		attempted := make(map[string]struct{})
		for {
			account, lease, ok, err := e.nextImageAccount(ctx, provider, client, attempted)
			if err != nil {
				var zero attemptOutcome[T]
				return zero, attempts, nil, err
			}
			if !ok {
				if len(attempted) == 0 {
					attempts = appendNoAccountAttempt(attempts, provider)
				}
				break
			}
			outcome, nextAttempts, keepLease, retry, err := executeImageAccountAttempt(e, ctx, plan, client, provider, adapter, account, lease, attempts, exec)
			attempted[strings.TrimSpace(account.ID)] = struct{}{}
			attempts = nextAttempts
			if err != nil {
				return outcome, attempts, keepLease, err
			}
			if keepLease != nil {
				e.bindSuccessfulRouteAccount(provider, client, outcome.decision.Account.ID)
				return outcome, attempts, keepLease, nil
			}
			if !retry {
				break
			}
		}
	}
	var zero attemptOutcome[T]
	return zero, attempts, nil, &ExecutionError{Attempts: attempts}
}

func executeImageAccountAttempt[T any](e *Engine, ctx context.Context, plan core.RoutePlan, client *core.APIClient, provider core.ProviderKind, adapter providers.Adapter, account core.Account, lease *imageAccountLease, attempts []core.AttemptRecord, exec imageAttemptExecutor[T]) (attemptOutcome[T], []core.AttemptRecord, *imageAccountLease, bool, error) {
	var zero attemptOutcome[T]
	notifyAttemptStarted(ctx, runningAttempt(provider, account))
	account, err := e.prepareAccount(ctx, adapter, account)
	if err != nil {
		e.markFailureForModality(account, err, failureModalityAccount)
		attempt := invokeErrorAttempt(provider, account, "refresh_error", err)
		notifyAttemptFinished(ctx, attempt)
		attempts = append(attempts, attempt)
		lease.release()
		return zero, attempts, nil, shouldContinueFailover(ctx, err, failureModalityAccount), nil
	}

	decision := core.RouteDecision{
		Provider: provider,
		Account:  account,
		Model:    plan.Model,
		Reason:   plan.Reason,
	}
	resp, finalDecision, invokeAttempts, errorStatus, err := invokeWithRefresh(e, ctx, adapter, decision, exec.invoke)
	attempts = append(attempts, invokeAttempts...)
	if err == nil {
		success := core.AttemptRecord{Provider: provider, AccountID: finalDecision.Account.ID, AccountLabel: finalDecision.Account.Label, Status: "ok"}
		notifyAttemptFinished(ctx, success)
		attempts = append(attempts, success)
		e.recordUpstreamSuccess(finalDecision.Account)
		_ = e.pool.MarkSuccess(finalDecision.Account)
		_ = e.pool.MarkImageQuotaUsed(finalDecision.Account, exec.quotaUnits)
		return attemptOutcome[T]{response: resp, decision: finalDecision}, attempts, lease, false, nil
	}

	failureModality := failureModalityImage
	if errorStatus == "refresh_error" {
		failureModality = failureModalityAccount
	}
	attempt := invokeErrorAttempt(provider, finalDecision.Account, errorStatus, err)
	notifyAttemptFinished(ctx, attempt)
	attempts = append(attempts, attempt)
	softRetryDecision := finalDecision
	softRetryErr := err
	for retryIndex := 1; retryIndex <= cacheAffinitySoftRetryMaxRetries && shouldSoftRetrySameAccount(ctx, client, softRetryErr, failureModality); retryIndex++ {
		if waitErr := waitBeforeSoftRetry(ctx, retryIndex); waitErr != nil {
			lease.release()
			return zero, attempts, nil, false, waitErr
		}
		retryResp, retryDecision, retryAttempts, retryErrorStatus, retryErr := invokeWithRefresh(e, ctx, adapter, softRetryDecision, exec.invoke)
		attempts = append(attempts, retryAttempts...)
		if retryErr == nil {
			success := core.AttemptRecord{Provider: provider, AccountID: retryDecision.Account.ID, AccountLabel: retryDecision.Account.Label, Status: "ok"}
			notifyAttemptFinished(ctx, success)
			attempts = append(attempts, success)
			e.recordUpstreamSuccess(retryDecision.Account)
			_ = e.pool.MarkSuccess(retryDecision.Account)
			_ = e.pool.MarkImageQuotaUsed(retryDecision.Account, exec.quotaUnits)
			return attemptOutcome[T]{response: retryResp, decision: retryDecision}, attempts, lease, false, nil
		}
		if retryErrorStatus == "refresh_error" {
			failureModality = failureModalityAccount
		}
		softRetryDecision = retryDecision
		softRetryErr = retryErr
		attempt = invokeErrorAttempt(provider, retryDecision.Account, retryErrorStatus, retryErr)
		notifyAttemptFinished(ctx, attempt)
		attempts = append(attempts, attempt)
	}
	finalDecision = softRetryDecision
	err = softRetryErr
	e.markFailureForModality(finalDecision.Account, err, failureModality)
	lease.release()
	if !shouldContinueFailover(ctx, err, failureModality) {
		return zero, attempts, nil, false, &ExecutionError{Attempts: attempts}
	}
	return zero, attempts, nil, true, nil
}

func (e *Engine) markFailure(account core.Account, err error) {
	e.markFailureForModality(account, err, failureModalityDefault)
}

func (e *Engine) markImageFailure(account core.Account, err error) {
	e.markFailureForModality(account, err, failureModalityImage)
}

func (e *Engine) markAccountFailure(account core.Account, err error) {
	e.markFailureForModality(account, err, failureModalityAccount)
}

func (e *Engine) markFailureForModality(account core.Account, err error, modality failureModality) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	policy := providers.FailurePolicyFor(account, err)
	if policy.Scope == providers.FailureScopeSharedUpstream {
		if strings.TrimSpace(policy.SharedUpstreamScope) != "" {
			e.recordSharedUpstreamFailure(policy)
			return
		}
		return
	}
	if policy.Scope != providers.FailureScopeAccount && policy.Scope != providers.FailureScopeQuota {
		return
	}
	if policy.Scope == providers.FailureScopeQuota {
		if modality == failureModalityImage {
			_ = e.pool.MarkImageQuotaLimited(account, policy.Cooldown)
		} else if modality == failureModalityAccount {
			_ = e.pool.MarkFailure(account, policy.AccountStatus, policy.Cooldown)
		} else if policy.ApplyChatQuota {
			_ = e.pool.MarkChatQuotaLimited(account, policy.Cooldown)
		}
		return
	}
	status := policy.AccountStatus
	if (status == core.AccountStatusBlocked || status == core.AccountStatusExpired) && strings.TrimSpace(account.Credential.RefreshToken) != "" {
		expiredAt := time.Now().UTC().Add(-1 * time.Second)
		account.Credential.ExpiresAt = &expiredAt
		status = core.AccountStatusCooling
	}
	_ = e.pool.MarkFailure(account, status, policy.Cooldown)
}

func (e *Engine) Execute(ctx context.Context, plan core.RoutePlan, client *core.APIClient, req *core.GatewayRequest) (*InvocationResult, error) {
	outcome, attempts, err := executeAcrossAccounts[*core.GatewayResponse](e, ctx, plan, client, attemptExecutor[*core.GatewayResponse]{
		selectCandidates: gatewayRequestCandidates(req),
		noAccount:        gatewayRequestNoAccount(req),
		ignoreBreakers:   gatewayRequestIgnoreBreakers(req),
		invoke: func(ctx context.Context, adapter providers.Adapter, decision core.RouteDecision) (*core.GatewayResponse, error) {
			resp, err := adapter.Invoke(ctx, decision, req)
			if err != nil {
				return nil, err
			}
			if monitorProbeRequiresContent(req, client) && (resp == nil || strings.TrimSpace(resp.Content) == "") {
				return nil, &providers.InvokeError{
					Code:      providers.ErrorCodeUpstreamEmptyResponse,
					Temporary: true,
					Cooldown:  10 * time.Second,
					Err:       fmt.Errorf("upstream returned an empty response"),
				}
			}
			return resp, nil
		},
	})
	if err != nil {
		return nil, err
	}
	return &InvocationResult{Response: outcome.response, Attempts: attempts}, nil
}

func monitorProbeRequiresContent(req *core.GatewayRequest, client *core.APIClient) bool {
	if req == nil || len(req.Metadata) == 0 {
		return false
	}
	if client == nil || !strings.EqualFold(strings.TrimSpace(client.Name), "Status Monitor") {
		return false
	}
	clientID := strings.TrimSpace(client.ID)
	if clientID != "monitor" && !strings.HasPrefix(clientID, "monitor:") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(req.Metadata["purpose"]), "status_monitor")
}

func (e *Engine) ExecuteResponses(ctx context.Context, plan core.RoutePlan, client *core.APIClient, req *core.ResponsesRequest) (*ResponsesResult, error) {
	outcome, attempts, err := executeAcrossAccounts[*core.GatewayResponse](e, ctx, plan, client, attemptExecutor[*core.GatewayResponse]{
		unsupportedStatus: AttemptStatusResponsesUnsupported,
		unsupportedCode:   ErrorCodeResponsesNotSupported,
		unsupportedName:   "responses",
		selectCandidates:  responsesCandidates(req),
		noAccount:         responsesNoAccount(req),
		ignoreBreakers:    responsesIgnoreBreakers(req),
		supported: func(adapter providers.Adapter) bool {
			_, ok := adapter.(providers.ResponsesAdapter)
			return ok
		},
		invoke: func(ctx context.Context, adapter providers.Adapter, decision core.RouteDecision) (*core.GatewayResponse, error) {
			return adapter.(providers.ResponsesAdapter).InvokeResponses(ctx, decision, req)
		},
	})
	if err != nil {
		return nil, err
	}
	return &ResponsesResult{Response: outcome.response, Attempts: attempts}, nil
}

func (e *Engine) refreshAfterInvokeAuthError(ctx context.Context, adapter providers.Adapter, account core.Account, invokeErr error) (core.Account, error) {
	if !providers.FailurePolicyFor(account, invokeErr).RefreshCredential {
		return core.Account{}, nil
	}
	if !providers.CredentialRefreshable(account) {
		return core.Account{}, nil
	}
	refreshingAdapter, ok := adapter.(providers.RefreshingAdapter)
	if !ok || strings.TrimSpace(account.Credential.RefreshToken) == "" {
		return core.Account{}, nil
	}
	if account.Credential.Metadata == nil {
		account.Credential.Metadata = map[string]string{}
	}
	account.Credential.Metadata["force_oauth_refresh"] = "true"
	expiredAt := time.Now().UTC().Add(-1 * time.Second)
	account.Credential.ExpiresAt = &expiredAt
	return e.refreshAccount(ctx, refreshingAdapter, account)
}

func (e *Engine) ExecuteEmbedding(ctx context.Context, plan core.RoutePlan, client *core.APIClient, req *core.EmbeddingRequest) (*EmbeddingResult, error) {
	outcome, attempts, err := executeAcrossAccounts[*core.EmbeddingResponse](e, ctx, plan, client, attemptExecutor[*core.EmbeddingResponse]{
		unsupportedStatus: AttemptStatusEmbeddingUnsupported,
		unsupportedCode:   ErrorCodeEmbeddingsNotSupported,
		unsupportedName:   "embeddings",
		supported: func(adapter providers.Adapter) bool {
			_, ok := adapter.(providers.EmbeddingAdapter)
			return ok
		},
		invoke: func(ctx context.Context, adapter providers.Adapter, decision core.RouteDecision) (*core.EmbeddingResponse, error) {
			return adapter.(providers.EmbeddingAdapter).Embed(ctx, decision, req)
		},
	})
	if err != nil {
		return nil, err
	}
	return &EmbeddingResult{Response: outcome.response, Attempts: attempts}, nil
}

func (e *Engine) ExecuteModeration(ctx context.Context, plan core.RoutePlan, client *core.APIClient, req *core.ModerationRequest) (*ModerationResult, error) {
	outcome, attempts, err := executeAcrossAccounts[*core.ModerationResponse](e, ctx, plan, client, attemptExecutor[*core.ModerationResponse]{
		unsupportedStatus: AttemptStatusModerationUnsupported,
		unsupportedCode:   ErrorCodeModerationsNotSupported,
		unsupportedName:   "moderations",
		supported: func(adapter providers.Adapter) bool {
			_, ok := adapter.(providers.ModerationAdapter)
			return ok
		},
		invoke: func(ctx context.Context, adapter providers.Adapter, decision core.RouteDecision) (*core.ModerationResponse, error) {
			return adapter.(providers.ModerationAdapter).Moderate(ctx, decision, req)
		},
	})
	if err != nil {
		return nil, err
	}
	return &ModerationResult{Response: outcome.response, Attempts: attempts}, nil
}

func (e *Engine) ExecuteImageGeneration(ctx context.Context, plan core.RoutePlan, client *core.APIClient, req *core.ImageGenerationRequest) (*ImageGenerationResult, error) {
	outcome, attempts, lease, err := executeAcrossImageAccounts[*core.ImageGenerationResponse](e, ctx, plan, client, imageAttemptExecutor[*core.ImageGenerationResponse]{
		unsupportedStatus: AttemptStatusImageGenerationUnsupported,
		unsupportedCode:   ErrorCodeImageGenerationNotSupported,
		unsupportedName:   "image generation",
		quotaUnits:        imageGenerationQuotaUnits(req),
		supported: func(adapter providers.Adapter) bool {
			_, ok := adapter.(providers.ImageGenerationAdapter)
			return ok
		},
		invoke: func(ctx context.Context, adapter providers.Adapter, decision core.RouteDecision) (*core.ImageGenerationResponse, error) {
			return adapter.(providers.ImageGenerationAdapter).GenerateImage(ctx, decision, req)
		},
	})
	if lease != nil {
		lease.release()
	}
	if err != nil {
		return nil, err
	}
	return &ImageGenerationResult{Response: outcome.response, Attempts: attempts}, nil
}

func (e *Engine) OpenImageGenerationStream(ctx context.Context, plan core.RoutePlan, client *core.APIClient, req *core.ImageGenerationRequest) (*StreamOpenResult, error) {
	outcome, attempts, lease, err := executeAcrossImageAccounts[*providers.StreamSession](e, ctx, plan, client, imageAttemptExecutor[*providers.StreamSession]{
		unsupportedStatus: AttemptStatusImageGenerationStreamUnsupported,
		unsupportedCode:   ErrorCodeImageGenerationStreamingNotSupported,
		unsupportedName:   "image generation streaming",
		quotaUnits:        imageGenerationQuotaUnits(req),
		supported: func(adapter providers.Adapter) bool {
			_, ok := adapter.(providers.ImageGenerationStreamAdapter)
			return ok
		},
		invoke: func(ctx context.Context, adapter providers.Adapter, decision core.RouteDecision) (*providers.StreamSession, error) {
			return adapter.(providers.ImageGenerationStreamAdapter).OpenImageGenerationStream(ctx, decision, req)
		},
	})
	if err != nil {
		return nil, err
	}
	return &StreamOpenResult{Session: streamSessionWithImageLease(outcome.response, lease), Decision: outcome.decision, Attempts: attempts, Modality: failureModalityImage}, nil
}

func (e *Engine) ExecuteImageMultipart(ctx context.Context, plan core.RoutePlan, client *core.APIClient, req *core.ImageMultipartRequest) (*ImageMultipartResult, error) {
	outcome, attempts, lease, err := executeAcrossImageAccounts[*core.ImageMultipartResponse](e, ctx, plan, client, imageAttemptExecutor[*core.ImageMultipartResponse]{
		unsupportedStatus: AttemptStatusImageMultipartUnsupported,
		unsupportedCode:   ErrorCodeImageMultipartNotSupported,
		unsupportedName:   "multipart images",
		quotaUnits:        imageMultipartQuotaUnits(req),
		supported: func(adapter providers.Adapter) bool {
			_, ok := adapter.(providers.ImageMultipartAdapter)
			return ok
		},
		invoke: func(ctx context.Context, adapter providers.Adapter, decision core.RouteDecision) (*core.ImageMultipartResponse, error) {
			return adapter.(providers.ImageMultipartAdapter).ProcessImageMultipart(ctx, decision, req)
		},
	})
	if lease != nil {
		lease.release()
	}
	if err != nil {
		return nil, err
	}
	return &ImageMultipartResult{Response: outcome.response, Attempts: attempts}, nil
}

func (e *Engine) OpenImageMultipartStream(ctx context.Context, plan core.RoutePlan, client *core.APIClient, req *core.ImageMultipartRequest) (*StreamOpenResult, error) {
	outcome, attempts, lease, err := executeAcrossImageAccounts[*providers.StreamSession](e, ctx, plan, client, imageAttemptExecutor[*providers.StreamSession]{
		unsupportedStatus: AttemptStatusImageMultipartStreamUnsupported,
		unsupportedCode:   ErrorCodeImageMultipartStreamingNotSupported,
		unsupportedName:   "image multipart streaming",
		quotaUnits:        imageMultipartQuotaUnits(req),
		supported: func(adapter providers.Adapter) bool {
			_, ok := adapter.(providers.ImageMultipartStreamAdapter)
			return ok
		},
		invoke: func(ctx context.Context, adapter providers.Adapter, decision core.RouteDecision) (*providers.StreamSession, error) {
			return adapter.(providers.ImageMultipartStreamAdapter).OpenImageMultipartStream(ctx, decision, req)
		},
	})
	if err != nil {
		return nil, err
	}
	return &StreamOpenResult{Session: streamSessionWithImageLease(outcome.response, lease), Decision: outcome.decision, Attempts: attempts, Modality: failureModalityImage}, nil
}

func shouldStopImageFailover(err error) bool {
	switch providers.ErrorCode(err) {
	case providers.ErrorCodeImageGenerationRejected,
		providers.ErrorCodeImageGenerationFailed,
		providers.ErrorCodeImageGenerationNotStarted,
		providers.ErrorCodeImagePollTimeout,
		providers.ErrorCodeImageModelUnsupported,
		providers.ErrorCodeImageEndpointUnsupported:
		return true
	default:
		return false
	}
}

func imageGenerationQuotaUnits(req *core.ImageGenerationRequest) int64 {
	if req == nil || len(req.Extra) == 0 {
		return 1
	}
	return positiveQuotaUnitsFromJSON(req.Extra["n"])
}

func imageMultipartQuotaUnits(req *core.ImageMultipartRequest) int64 {
	if req == nil {
		return 1
	}
	if value, ok := req.FormFields["n"]; ok {
		return positiveQuotaUnitsFromString(value)
	}
	mediaType, params, err := mime.ParseMediaType(req.ContentType)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return 1
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return 1
	}
	reader := multipart.NewReader(bytes.NewReader(req.Body), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return 1
		}
		if err != nil {
			return 1
		}
		if part.FormName() != "n" {
			_ = part.Close()
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, 64))
		_ = part.Close()
		if err != nil {
			return 1
		}
		return positiveQuotaUnitsFromString(string(data))
	}
}

func positiveQuotaUnitsFromJSON(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 1
	}
	var number int64
	if err := json.Unmarshal(raw, &number); err == nil {
		return positiveQuotaUnits(number)
	}
	var floatNumber float64
	if err := json.Unmarshal(raw, &floatNumber); err == nil {
		return positiveQuotaUnits(int64(floatNumber))
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return positiveQuotaUnitsFromString(text)
	}
	return 1
}

func positiveQuotaUnitsFromString(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 1
	}
	if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
		return positiveQuotaUnits(parsed)
	}
	if parsed, err := strconv.ParseFloat(value, 64); err == nil {
		return positiveQuotaUnits(int64(parsed))
	}
	return 1
}

func positiveQuotaUnits(value int64) int64 {
	if value <= 0 {
		return 1
	}
	return value
}

func (e *Engine) ExecuteAudioSpeech(ctx context.Context, plan core.RoutePlan, client *core.APIClient, req *core.AudioSpeechRequest) (*AudioSpeechResult, error) {
	outcome, attempts, err := executeAcrossAccounts[*core.AudioSpeechResponse](e, ctx, plan, client, attemptExecutor[*core.AudioSpeechResponse]{
		unsupportedStatus: AttemptStatusAudioSpeechUnsupported,
		unsupportedCode:   ErrorCodeAudioSpeechNotSupported,
		unsupportedName:   "audio speech",
		supported: func(adapter providers.Adapter) bool {
			_, ok := adapter.(providers.AudioSpeechAdapter)
			return ok
		},
		invoke: func(ctx context.Context, adapter providers.Adapter, decision core.RouteDecision) (*core.AudioSpeechResponse, error) {
			return adapter.(providers.AudioSpeechAdapter).CreateSpeech(ctx, decision, req)
		},
	})
	if err != nil {
		return nil, err
	}
	return &AudioSpeechResult{Response: outcome.response, Attempts: attempts}, nil
}

func (e *Engine) ExecuteAudioMultipart(ctx context.Context, plan core.RoutePlan, client *core.APIClient, req *core.AudioMultipartRequest) (*AudioMultipartResult, error) {
	outcome, attempts, err := executeAcrossAccounts[*core.AudioMultipartResponse](e, ctx, plan, client, attemptExecutor[*core.AudioMultipartResponse]{
		unsupportedStatus: AttemptStatusAudioMultipartUnsupported,
		unsupportedCode:   ErrorCodeAudioMultipartNotSupported,
		unsupportedName:   "multipart audio",
		supported: func(adapter providers.Adapter) bool {
			_, ok := adapter.(providers.AudioMultipartAdapter)
			return ok
		},
		invoke: func(ctx context.Context, adapter providers.Adapter, decision core.RouteDecision) (*core.AudioMultipartResponse, error) {
			return adapter.(providers.AudioMultipartAdapter).ProcessAudioMultipart(ctx, decision, req)
		},
	})
	if err != nil {
		return nil, err
	}
	return &AudioMultipartResult{Response: outcome.response, Attempts: attempts}, nil
}

func (e *Engine) ExecuteTokenCount(ctx context.Context, plan core.RoutePlan, client *core.APIClient, req *core.TokenCountRequest) (*TokenCountResult, error) {
	outcome, attempts, err := executeAcrossAccounts[*core.TokenCountResponse](e, ctx, plan, client, attemptExecutor[*core.TokenCountResponse]{
		unsupportedStatus: AttemptStatusTokenCountUnsupported,
		unsupportedCode:   ErrorCodeTokenCountNotSupported,
		unsupportedName:   "token counting",
		supported: func(adapter providers.Adapter) bool {
			_, ok := adapter.(providers.TokenCountAdapter)
			return ok
		},
		invoke: func(ctx context.Context, adapter providers.Adapter, decision core.RouteDecision) (*core.TokenCountResponse, error) {
			return adapter.(providers.TokenCountAdapter).CountTokens(ctx, decision, req)
		},
	})
	if err != nil {
		return nil, err
	}
	return &TokenCountResult{Response: outcome.response, Attempts: attempts}, nil
}

func (e *Engine) OpenStream(ctx context.Context, plan core.RoutePlan, client *core.APIClient, req *core.GatewayRequest) (*StreamOpenResult, error) {
	outcome, attempts, err := executeAcrossAccounts[*providers.StreamSession](e, ctx, plan, client, attemptExecutor[*providers.StreamSession]{
		unsupportedStatus: AttemptStatusStreamUnsupported,
		unsupportedCode:   ErrorCodeStreamingNotSupported,
		unsupportedName:   "streaming",
		selectCandidates:  gatewayRequestCandidates(req),
		noAccount:         gatewayRequestNoAccount(req),
		ignoreBreakers:    gatewayRequestIgnoreBreakers(req),
		supported: func(adapter providers.Adapter) bool {
			_, ok := adapter.(providers.StreamingAdapter)
			return ok
		},
		invoke: func(ctx context.Context, adapter providers.Adapter, decision core.RouteDecision) (*providers.StreamSession, error) {
			return adapter.(providers.StreamingAdapter).OpenStream(ctx, decision, req)
		},
	})
	if err != nil {
		return nil, err
	}
	return &StreamOpenResult{Session: outcome.response, Decision: outcome.decision, Attempts: attempts}, nil
}

func (e *Engine) OpenResponsesStream(ctx context.Context, plan core.RoutePlan, client *core.APIClient, req *core.ResponsesRequest) (*StreamOpenResult, error) {
	outcome, attempts, err := executeAcrossAccounts[*providers.StreamSession](e, ctx, plan, client, attemptExecutor[*providers.StreamSession]{
		unsupportedStatus: AttemptStatusResponsesStreamUnsupported,
		unsupportedCode:   ErrorCodeResponsesStreamingNotSupported,
		unsupportedName:   "responses streaming",
		selectCandidates:  responsesCandidates(req),
		noAccount:         responsesNoAccount(req),
		ignoreBreakers:    responsesIgnoreBreakers(req),
		supported: func(adapter providers.Adapter) bool {
			_, ok := adapter.(providers.ResponsesStreamAdapter)
			return ok
		},
		invoke: func(ctx context.Context, adapter providers.Adapter, decision core.RouteDecision) (*providers.StreamSession, error) {
			return adapter.(providers.ResponsesStreamAdapter).OpenResponsesStream(ctx, decision, req)
		},
	})
	if err != nil {
		return nil, err
	}
	return &StreamOpenResult{Session: outcome.response, Decision: outcome.decision, Attempts: attempts}, nil
}

func (e *Engine) OpenResponsesWebSocketSession(ctx context.Context, plan core.RoutePlan, client *core.APIClient, req *core.ResponsesRequest) (*ResponsesWebSocketOpenResult, error) {
	outcome, attempts, err := executeAcrossAccounts[providers.ResponsesWebSocketSession](e, ctx, plan, client, attemptExecutor[providers.ResponsesWebSocketSession]{
		unsupportedStatus: AttemptStatusResponsesWebSocketUnsupported,
		unsupportedCode:   ErrorCodeResponsesWebSocketNotSupported,
		unsupportedName:   "responses websocket",
		selectCandidates:  responsesCandidates(req),
		noAccount:         responsesNoAccount(req),
		ignoreBreakers:    responsesIgnoreBreakers(req),
		supported: func(adapter providers.Adapter) bool {
			_, ok := adapter.(providers.ResponsesWebSocketAdapter)
			return ok
		},
		invoke: func(ctx context.Context, adapter providers.Adapter, decision core.RouteDecision) (providers.ResponsesWebSocketSession, error) {
			return adapter.(providers.ResponsesWebSocketAdapter).OpenResponsesWebSocketSession(ctx, decision, req)
		},
	})
	if err != nil {
		return nil, err
	}
	return &ResponsesWebSocketOpenResult{Session: outcome.response, Decision: outcome.decision, Attempts: attempts}, nil
}

func (e *Engine) RecordStreamFailure(result *StreamOpenResult, err error) {
	if e == nil || e.pool == nil || result == nil || err == nil {
		return
	}
	if strings.TrimSpace(result.Decision.Account.ID) != "" {
		account := result.Decision.Account
		if current, getErr := e.pool.GetAccount(account.ID); getErr == nil {
			if strings.TrimSpace(account.EffectiveProxyURL) != "" {
				current.EffectiveProxyURL = account.EffectiveProxyURL
			}
			account = current
		}
		e.markFailureForModality(account, err, result.Modality)
		return
	}
	for i := len(result.Attempts) - 1; i >= 0; i-- {
		attempt := result.Attempts[i]
		if attempt.Status != "ok" || attempt.AccountID == "" {
			continue
		}
		account, getErr := e.pool.GetAccount(attempt.AccountID)
		if getErr != nil {
			return
		}
		e.markFailureForModality(account, err, result.Modality)
		return
	}
}

func (e *Engine) RecordResponsesWebSocketFailure(decision core.RouteDecision, err error) {
	if e == nil || e.pool == nil || err == nil || strings.TrimSpace(decision.Account.ID) == "" {
		return
	}
	account := decision.Account
	account, getErr := e.pool.GetAccount(decision.Account.ID)
	if getErr == nil {
		if strings.TrimSpace(decision.Account.EffectiveProxyURL) != "" {
			account.EffectiveProxyURL = decision.Account.EffectiveProxyURL
		}
	} else {
		account = decision.Account
	}
	e.markFailureForModality(account, err, failureModalityDefault)
}
