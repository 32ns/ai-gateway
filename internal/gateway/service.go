package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/failover"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/routing"
	"github.com/32ns/ai-gateway/internal/storage"
)

type Service struct {
	repo              storage.Repository
	router            *routing.Router
	failover          *failover.Engine
	requestSeq        atomic.Uint64
	modelsMu          sync.Mutex
	models            atomicModelCache
	groupRoutesMu     sync.Mutex
	groupRoutes       atomicGroupRoutePolicyCache
	quotaRegistry     *providers.Registry
	quotaMu           sync.Mutex
	quotaRefresh      map[string]time.Time
	userConcurrencyMu sync.Mutex
	userConcurrency   map[string]int
	planConcurrency   map[string]int
	userRateMu        sync.Mutex
	userRateNext      map[string]time.Time
	userRateCleanupAt time.Time
	billingEvents     func(BillingEvent)
}

type BillingEvent struct {
	Reason    string
	UserID    string
	RequestID string
	ClientID  string
}

type gatewayModelCache struct {
	revision uint64
	models   map[string]core.ModelConfig
}

type atomicModelCache struct {
	value atomic.Value
}

type groupRoutePolicyCache struct {
	revision uint64
	byGroup  map[string]core.RoutePolicy
}

type atomicGroupRoutePolicyCache struct {
	value atomic.Value
}

const maxStreamCollectedContentRunes = 4096
const accountQuotaRefreshMinInterval = 30 * time.Second
const maxPreOutputStreamRetries = 32
const maxPreOutputSameAccountStreamRetries = 1

var ErrModelUnavailable = errors.New("model unavailable")
var ErrUserConcurrentRequestLimitExceeded = errors.New("user concurrent request limit exceeded")
var ErrPlanConcurrentRequestLimitExceeded = errors.New("plan concurrent request limit exceeded")

func streamEventRecordsFirstOutput(event *core.StreamEvent) bool {
	if event == nil || streamEventIsResponsesCompletionSignal(event.RawEvent) {
		return false
	}
	return event.FirstOutput || event.Delta != ""
}

func streamEventCommitsClientOutput(event *core.StreamEvent) bool {
	if event == nil || event.Started {
		return false
	}
	if streamEventRecordsFirstOutput(event) {
		return true
	}
	if len(event.RawData) == 0 {
		return false
	}
	if strings.HasPrefix(strings.TrimSpace(event.RawEvent), "response.") {
		return false
	}
	if streamEventIsResponsesCompletionSignal(event.RawEvent) {
		return false
	}
	return true
}

func streamEventIsResponsesCompletionSignal(rawEvent string) bool {
	rawEvent = strings.TrimSpace(rawEvent)
	if rawEvent == "" {
		return false
	}
	switch rawEvent {
	case "response.completed",
		"response.done",
		"response.failed",
		"response.incomplete",
		"response.cancelled",
		"response.canceled":
		return true
	default:
		return strings.HasPrefix(rawEvent, "response.") && strings.HasSuffix(rawEvent, ".done")
	}
}

func streamEventIsResponsesFailureSignal(rawEvent string) bool {
	rawEvent = strings.TrimSpace(rawEvent)
	switch rawEvent {
	case "response.failed":
		return true
	default:
		return false
	}
}

func streamEventPreOutputError(event *core.StreamEvent) error {
	if event == nil || len(event.RawData) == 0 {
		return nil
	}
	if err := streamEventFailureError(event); err != nil {
		return err
	}
	raw := strings.TrimSpace(string(event.RawData))
	if raw == "" {
		return nil
	}
	if !streamEventRecordsFirstOutput(event) && streamRawDataLooksLikeHTMLChallenge(event.RawData) {
		return &providers.InvokeError{
			Code:      providers.ErrorCodeUpstreamServerError,
			Temporary: true,
			Cooldown:  30 * time.Second,
			Err:       fmt.Errorf("upstream returned HTML challenge response"),
		}
	}
	if strings.HasPrefix(strings.TrimSpace(event.RawEvent), "response.") {
		return nil
	}
	if !streamEventRecordsFirstOutput(event) {
		if err := streamEventTopLevelError(event.RawData); err != nil {
			return err
		}
	}
	if json.Valid(event.RawData) {
		return nil
	}
	return &providers.InvokeError{
		Code:      providers.ErrorCodeUpstreamInvalidStreamChunk,
		Temporary: true,
		Cooldown:  10 * time.Second,
		Err:       fmt.Errorf("upstream returned invalid stream chunk before output"),
	}
}

func streamRawDataLooksLikeHTMLChallenge(data []byte) bool {
	raw := strings.TrimSpace(string(data))
	if raw == "" {
		return false
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err == nil {
		return streamJSONValueLooksLikeHTMLChallenge(decoded)
	}
	return streamTextLooksLikeHTMLChallenge(raw)
}

func streamJSONValueLooksLikeHTMLChallenge(value any) bool {
	switch typed := value.(type) {
	case string:
		return streamTextLooksLikeHTMLSource(typed)
	case []any:
		for _, item := range typed {
			if streamJSONValueLooksLikeHTMLChallenge(item) {
				return true
			}
		}
	case map[string]any:
		for _, item := range typed {
			if streamJSONValueLooksLikeHTMLChallenge(item) {
				return true
			}
		}
	}
	return false
}

func streamTextLooksLikeHTMLChallenge(text string) bool {
	if streamTextLooksLikeHTMLSource(text) {
		return true
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	return strings.Contains(lower, "cloudflare") &&
		(strings.Contains(lower, "challenge") ||
			strings.Contains(lower, "captcha") ||
			strings.Contains(lower, "cf-ray") ||
			strings.Contains(lower, "cf-chl")) ||
		strings.Contains(lower, "challenge-platform") ||
		strings.Contains(lower, "cf-chl") ||
		(strings.Contains(lower, "just a moment") &&
			(strings.Contains(lower, "cloudflare") || strings.Contains(lower, "<")))
}

func streamTextLooksLikeHTMLSource(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	return strings.HasPrefix(lower, "<") ||
		strings.Contains(lower, "<!doctype") ||
		strings.Contains(lower, "<html") ||
		strings.Contains(lower, "<head") ||
		strings.Contains(lower, "<body") ||
		strings.Contains(lower, "<script") ||
		strings.Contains(lower, "<div") ||
		strings.Contains(lower, "<title") ||
		strings.Contains(lower, "</html") ||
		strings.Contains(lower, "</head") ||
		strings.Contains(lower, "</body")
}

type responsesStreamFailurePayload struct {
	Type     string                       `json:"type"`
	Error    *responsesStreamFailureError `json:"error"`
	Response *struct {
		Status string                       `json:"status"`
		Error  *responsesStreamFailureError `json:"error"`
	} `json:"response"`
}

type streamTopLevelErrorPayload struct {
	Error any    `json:"error"`
	Code  string `json:"code"`
	Type  string `json:"type"`
}

func streamEventTopLevelError(rawData []byte) error {
	var payload streamTopLevelErrorPayload
	if err := json.Unmarshal(rawData, &payload); err != nil {
		return nil
	}
	if payload.Error == nil {
		return nil
	}
	code, message := streamTopLevelErrorCodeMessage(payload)
	if message == "" {
		message = "upstream returned stream error before output"
	}
	codeLower := strings.ToLower(strings.TrimSpace(code))
	messageLower := strings.ToLower(strings.TrimSpace(message))
	switch {
	case streamTopLevelErrorLooksGatewayKeyDisabled(codeLower, messageLower):
		return &providers.InvokeError{Code: providers.ErrorCodeGatewayAPIKeyDisabled, Temporary: true, Cooldown: 2 * time.Minute, Err: errors.New(message)}
	case streamTopLevelErrorLooksProviderBanned(codeLower, messageLower):
		return &providers.InvokeError{Code: providers.ErrorCodeUpstreamProviderBanned, Temporary: false, Err: errors.New(message)}
	case streamTopLevelErrorLooksAuth(codeLower, messageLower):
		return &providers.InvokeError{Code: providers.ErrorCodeUpstreamAuthError, Temporary: false, Cooldown: 2 * time.Minute, Err: errors.New(message)}
	case responsesStreamFailureLooksQuota(codeLower, messageLower):
		return &providers.InvokeError{Code: providers.ErrorCodeUpstreamRateLimited, Temporary: true, Cooldown: 45 * time.Second, Err: errors.New(message)}
	case responsesStreamFailureLooksTemporary(codeLower, messageLower):
		return &providers.InvokeError{Code: providers.ErrorCodeUpstreamTemporarilyUnavailable, Temporary: true, Cooldown: 30 * time.Second, Err: errors.New(message)}
	default:
		return &providers.InvokeError{Code: providers.ErrorCodeUpstreamRejected, Temporary: false, Err: errors.New(message)}
	}
}

func streamTopLevelErrorCodeMessage(payload streamTopLevelErrorPayload) (string, string) {
	code := strings.TrimSpace(firstNonEmptyModel(payload.Code, payload.Type))
	message := ""
	switch errValue := payload.Error.(type) {
	case string:
		message = strings.TrimSpace(errValue)
	case map[string]any:
		for _, key := range []string{"code", "type"} {
			if text, _ := errValue[key].(string); strings.TrimSpace(text) != "" {
				if code == "" {
					code = strings.TrimSpace(text)
				}
			}
		}
		for _, key := range []string{"message", "detail", "error"} {
			if text, _ := errValue[key].(string); strings.TrimSpace(text) != "" {
				message = strings.TrimSpace(text)
				break
			}
		}
	}
	return code, strings.TrimSpace(message)
}

func streamTopLevelErrorLooksGatewayKeyDisabled(code, message string) bool {
	return strings.Contains(code, "api_key_disabled") ||
		strings.Contains(code, "key_disabled") ||
		strings.Contains(message, "api key is disabled") ||
		strings.Contains(message, "key is disabled")
}

func streamTopLevelErrorLooksProviderBanned(code, message string) bool {
	for _, signal := range []string{"banned", "suspended", "deactivated", "disabled for your project"} {
		if strings.Contains(code, signal) || strings.Contains(message, signal) {
			return true
		}
	}
	return false
}

func streamTopLevelErrorLooksAuth(code, message string) bool {
	for _, signal := range []string{"authentication", "unauthorized", "invalid_api_key", "invalid api key", "auth_error"} {
		if strings.Contains(code, signal) || strings.Contains(message, signal) {
			return true
		}
	}
	return false
}

type responsesStreamFailureError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
	Type    string `json:"type"`
}

func streamEventFailureError(event *core.StreamEvent) error {
	if event == nil || !streamEventIsResponsesFailureSignal(event.RawEvent) {
		return nil
	}
	code := responsesStreamFailureCode(event)
	if code == "" {
		return nil
	}
	reason := strings.TrimSpace(event.FinishReason)
	if reason == "" {
		reason = strings.TrimPrefix(strings.TrimSpace(event.RawEvent), "response.")
	}
	if reason == "canceled" {
		reason = "cancelled"
	}
	if reason == "" {
		reason = "failed"
	}
	return &providers.InvokeError{
		Code:      code,
		Temporary: true,
		Cooldown:  20 * time.Second,
		Err:       fmt.Errorf("responses stream ended with %s before output", reason),
	}
}

func responsesStreamFailureCode(event *core.StreamEvent) string {
	if event == nil || len(event.RawData) == 0 {
		return ""
	}
	var payload responsesStreamFailurePayload
	if err := json.Unmarshal(event.RawData, &payload); err != nil {
		return ""
	}
	errorPayload := payload.Error
	if errorPayload == nil && payload.Response != nil {
		errorPayload = payload.Response.Error
	}
	if errorPayload == nil {
		return ""
	}
	code := strings.ToLower(strings.TrimSpace(firstNonEmptyModel(errorPayload.Code, errorPayload.Type)))
	message := strings.ToLower(strings.TrimSpace(errorPayload.Message))
	switch {
	case responsesStreamFailureLooksQuota(code, message):
		return providers.ErrorCodeUpstreamRateLimited
	case responsesStreamFailureLooksTemporary(code, message):
		return providers.ErrorCodeUpstreamTemporarilyUnavailable
	default:
		return ""
	}
}

func responsesStreamFailureLooksQuota(code, message string) bool {
	for _, signal := range []string{
		"rate_limit",
		"quota",
		"insufficient_quota",
		"limit_exceeded",
		"too_many_requests",
		"billing_hard_limit",
	} {
		if strings.Contains(code, signal) || strings.Contains(message, signal) {
			return true
		}
	}
	return false
}

func responsesStreamFailureLooksTemporary(code, message string) bool {
	for _, signal := range []string{
		"server_error",
		"internal_error",
		"temporarily_unavailable",
		"service_unavailable",
		"overloaded",
		"timeout",
		"timed out",
		"bad_response",
		"bad gateway",
		"gateway timeout",
		"no_available_channel",
		"no_available_key",
		"no_available_accounts",
		"no available channel",
		"no available key",
		"no available accounts",
		"all available accounts exhausted",
		"channel:no_available_key",
	} {
		if strings.Contains(code, signal) || strings.Contains(message, signal) {
			return true
		}
	}
	return false
}

func shouldRetryStreamBeforeOutput(err error, committed bool, attempts int) bool {
	if err == nil || committed || attempts >= maxPreOutputStreamRetries {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch providers.ErrorCode(err) {
	case providers.ErrorCodeUpstreamTransportError,
		providers.ErrorCodeUpstreamReadError,
		providers.ErrorCodeUpstreamTemporarilyUnavailable,
		providers.ErrorCodeUpstreamServerError,
		providers.ErrorCodeUpstreamInvalidStreamChunk,
		providers.ErrorCodeUpstreamRateLimited,
		providers.ErrorCodeUpstreamAuthError,
		providers.ErrorCodeGatewayAPIKeyDisabled,
		providers.ErrorCodeUpstreamProviderBanned:
	default:
		return false
	}
	return providers.FailurePolicyFor(core.Account{}, err).RetryFailover
}

func shouldRetrySameAccountStreamBeforeOutput(client *core.APIClient, err error, committed bool) bool {
	if client == nil || strings.TrimSpace(client.RouteAffinityKey) == "" {
		return false
	}
	if err == nil || committed {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch providers.ErrorCode(err) {
	case providers.ErrorCodeUpstreamTransportError,
		providers.ErrorCodeUpstreamReadError,
		providers.ErrorCodeUpstreamTemporarilyUnavailable,
		providers.ErrorCodeUpstreamServerError,
		providers.ErrorCodeUpstreamInvalidStreamChunk:
	default:
		return false
	}
	return providers.FailurePolicyFor(core.Account{}, err).RetryFailover
}

func appendStreamOpenAttempts(out []core.AttemptRecord, attempts []core.AttemptRecord) []core.AttemptRecord {
	if len(attempts) == 0 {
		return out
	}
	return append(out, attempts...)
}

func gatewayRequestWithStrictAccount(req *core.GatewayRequest, accountID string) *core.GatewayRequest {
	accountID = strings.TrimSpace(accountID)
	if req == nil || accountID == "" {
		return req
	}
	clone := *req
	clone.PreferredAccountID = accountID
	clone.StrictAccountAffinity = true
	if req.Metadata != nil {
		clone.Metadata = cloneStringMap(req.Metadata)
	}
	return &clone
}

func gatewayRequestExcludingAccount(req *core.GatewayRequest, accountID string) *core.GatewayRequest {
	accountID = strings.TrimSpace(accountID)
	if req == nil || accountID == "" {
		return req
	}
	clone := *req
	clone.PreferredAccountID = ""
	clone.StrictAccountAffinity = false
	clone.ExcludedAccountIDs = appendResponseExcludedAccountID(req.ExcludedAccountIDs, accountID)
	if req.Metadata != nil {
		clone.Metadata = cloneStringMap(req.Metadata)
	}
	if req.Extra != nil {
		clone.Extra = cloneRawJSONMap(req.Extra)
	}
	return &clone
}

func cloneRawJSONMap(values map[string]json.RawMessage) map[string]json.RawMessage {
	if len(values) == 0 {
		return nil
	}
	clone := make(map[string]json.RawMessage, len(values))
	for key, value := range values {
		if value != nil {
			clone[key] = append(json.RawMessage(nil), value...)
		} else {
			clone[key] = nil
		}
	}
	return clone
}

func streamFailureAttempt(provider core.ProviderKind, accountID, accountLabel string, err error) core.AttemptRecord {
	attempt := core.AttemptRecord{
		Provider:     provider,
		AccountID:    strings.TrimSpace(accountID),
		AccountLabel: strings.TrimSpace(accountLabel),
		Status:       "stream_error",
		ErrorMessage: err.Error(),
		Temporary:    providers.IsTemporary(err),
	}
	if code := providers.ErrorCode(err); code != "" {
		attempt.ErrorCode = code
	}
	return attempt
}

func streamFailureAttemptFromResult(result *failover.StreamOpenResult, err error) core.AttemptRecord {
	if result != nil && strings.TrimSpace(result.Decision.Account.ID) != "" {
		account := result.Decision.Account
		return streamFailureAttempt(result.Decision.Provider, account.ID, account.Label, err)
	}
	if result != nil {
		for i := len(result.Attempts) - 1; i >= 0; i-- {
			attempt := result.Attempts[i]
			if attempt.Status == "ok" && strings.TrimSpace(attempt.AccountID) != "" {
				return streamFailureAttempt(attempt.Provider, attempt.AccountID, attempt.AccountLabel, err)
			}
		}
	}
	return streamFailureAttempt("", "", "", err)
}

func recordBillingStreamFailure(hold *billingHold, attempt core.AttemptRecord) {
	if hold == nil || strings.TrimSpace(attempt.AccountID) == "" {
		return
	}
	hold.AttemptFinished(attempt)
}

func cloneStreamEvent(event *core.StreamEvent) core.StreamEvent {
	if event == nil {
		return core.StreamEvent{}
	}
	clone := *event
	if len(event.RawData) > 0 {
		clone.RawData = append([]byte(nil), event.RawData...)
	}
	if event.Usage != nil {
		usage := *event.Usage
		clone.Usage = &usage
	}
	return clone
}

type gatewayStreamConsumeResult struct {
	Response    core.GatewayResponse
	Committed   bool
	StreamErr   error
	UpstreamErr error
}

func consumeGatewayStream(requestStarted time.Time, requestedModel, exposedModel string, session *providers.StreamSession, emit func(*core.GatewayResponse, *core.StreamEvent) error) gatewayStreamConsumeResult {
	if session == nil || session.Response == nil || session.Stream == nil {
		err := &providers.InvokeError{
			Code:      providers.ErrorCodeUpstreamReadError,
			Temporary: true,
			Cooldown:  20 * time.Second,
			Err:       errors.New("stream session is not initialized"),
		}
		return gatewayStreamConsumeResult{StreamErr: err, UpstreamErr: err}
	}
	session.Response.Model = firstNonEmptyModel(requestedModel, exposedModel)
	content := limitedStringBuilder{limit: maxStreamCollectedContentRunes}
	finishReason := session.Response.FinishReason
	usage := session.Response.Usage
	var streamErr error
	var upstreamErr error
	var firstTokenMS int64
	committed := false
	pending := make([]core.StreamEvent, 0, 2)
	flushPending := func() error {
		if emit == nil || len(pending) == 0 {
			pending = nil
			return nil
		}
		for i := range pending {
			if err := emit(session.Response, &pending[i]); err != nil {
				return err
			}
		}
		pending = nil
		return nil
	}

streamLoop:
	for {
		event, err := session.Stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			streamErr = err
			upstreamErr = err
			break
		}
		if event == nil {
			continue
		}
		if firstTokenMS == 0 && streamEventRecordsFirstOutput(event) {
			firstTokenMS = elapsedFirstTokenMS(requestStarted)
		}
		if event.Delta != "" {
			content.WriteString(event.Delta)
		}
		if event.FinishReason != "" {
			finishReason = event.FinishReason
		}
		if event.Usage != nil {
			usage = *event.Usage
		}
		if !committed {
			if preOutputErr := streamEventPreOutputError(event); preOutputErr != nil {
				streamErr = preOutputErr
				upstreamErr = preOutputErr
				break
			}
		}
		eventCommits := streamEventCommitsClientOutput(event) || event.Done || event.FinishReason != ""
		if eventCommits {
			committed = true
			if err := flushPending(); err != nil {
				streamErr = err
				break streamLoop
			}
		} else if !committed {
			if emit != nil {
				pending = append(pending, cloneStreamEvent(event))
			}
			continue
		}
		if emit != nil {
			if err := emit(session.Response, event); err != nil {
				streamErr = err
				break
			}
		}
	}

	if closeErr := session.Stream.Close(); streamErr == nil && closeErr != nil {
		streamErr = closeErr
		upstreamErr = closeErr
	}
	if streamErr == nil {
		streamErr = flushPending()
	}

	finalResp := *session.Response
	finalResp.Model = firstNonEmptyModel(requestedModel, exposedModel)
	finalResp.Content = content.String()
	finalResp.FinishReason = finishReason
	finalResp.Usage = usage
	finalResp.FirstTokenMS = firstTokenMS
	return gatewayStreamConsumeResult{
		Response:    finalResp,
		Committed:   committed,
		StreamErr:   streamErr,
		UpstreamErr: upstreamErr,
	}
}

type imageStreamConsumeResult struct {
	Response    core.GatewayResponse
	Committed   bool
	StreamErr   error
	UpstreamErr error
}

func consumeImageStream(requestedModel, exposedModel string, session *providers.StreamSession, emit func(*core.GatewayResponse, *core.StreamEvent) error) imageStreamConsumeResult {
	if session == nil || session.Response == nil || session.Stream == nil {
		err := &providers.InvokeError{
			Code:      providers.ErrorCodeUpstreamReadError,
			Temporary: true,
			Cooldown:  20 * time.Second,
			Err:       errors.New("stream session is not initialized"),
		}
		return imageStreamConsumeResult{StreamErr: err, UpstreamErr: err}
	}
	finalResp := streamImageGatewayResponse(session, requestedModel, exposedModel)
	var streamErr error
	var upstreamErr error
	committed := false
	for {
		event, err := session.Stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			streamErr = err
			upstreamErr = err
			break
		}
		if event == nil {
			continue
		}
		if event.FinishReason != "" {
			finalResp.FinishReason = event.FinishReason
		}
		if event.Usage != nil {
			finalResp.Usage = *event.Usage
		}
		if !committed {
			if preOutputErr := streamEventPreOutputError(event); preOutputErr != nil {
				streamErr = preOutputErr
				upstreamErr = preOutputErr
				break
			}
		}
		if streamEventCommitsClientOutput(event) {
			committed = true
		}
		if event.Done || event.FinishReason != "" {
			committed = true
		}
		if emit != nil {
			if err := emit(&finalResp, event); err != nil {
				streamErr = err
				break
			}
		}
	}
	if closeErr := session.Stream.Close(); streamErr == nil && closeErr != nil {
		streamErr = closeErr
		upstreamErr = closeErr
	}
	return imageStreamConsumeResult{
		Response:    finalResp,
		Committed:   committed,
		StreamErr:   streamErr,
		UpstreamErr: upstreamErr,
	}
}

type ModelUnavailableError struct {
	Model string
}

func (e *ModelUnavailableError) Error() string {
	if e == nil || strings.TrimSpace(e.Model) == "" {
		return ErrModelUnavailable.Error()
	}
	return fmt.Sprintf("model %q is not enabled", e.Model)
}

func (e *ModelUnavailableError) Unwrap() error {
	return ErrModelUnavailable
}

type BillingError struct {
	StatusCode int
	Code       string
	Message    string
	PublicURL  string
	Err        error
}

func (e *BillingError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "billing failed"
}

func (e *BillingError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type ConcurrencyLimitError struct {
	StatusCode int
	Code       string
	Message    string
	Scope      string
	UserKey    string
	Limit      int
	Active     int
	Err        error
}

func (e *ConcurrencyLimitError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "too many concurrent requests"
}

func (e *ConcurrencyLimitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type AccessError struct {
	StatusCode int
	Code       string
	Message    string
	Err        error
}

func (e *AccessError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "access denied"
}

func (e *AccessError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func New(repo storage.Repository, router *routing.Router, engine *failover.Engine) *Service {
	return &Service{
		repo:            repo,
		router:          router,
		failover:        engine,
		userConcurrency: make(map[string]int),
		planConcurrency: make(map[string]int),
		userRateNext:    make(map[string]time.Time),
	}
}

func (s *Service) newRequestID() string {
	now := time.Now().UTC()
	return fmt.Sprintf("req_%d_%d", now.UnixNano(), s.requestSeq.Add(1))
}

func (s *Service) WithQuotaRegistry(registry *providers.Registry) *Service {
	s.quotaRegistry = registry
	return s
}

func (s *Service) WithBillingEvents(fn func(BillingEvent)) *Service {
	if s != nil {
		s.billingEvents = fn
	}
	return s
}

func (s *Service) currentSystemSettings() core.SystemSettings {
	if s == nil || s.repo == nil {
		return core.DefaultSystemSettings()
	}
	settings, err := storage.LoadStartupSystemSettings(s.repo)
	if err != nil {
		return core.DefaultSystemSettings()
	}
	return core.NormalizeSystemSettings(settings)
}

func (s *Service) ImageCandidateCount(client *core.APIClient, modelID string, multipart bool) int {
	if s == nil || s.router == nil || s.failover == nil {
		return 0
	}
	client, routePolicy, err := s.clientForRequest(nil, client)
	if err != nil {
		return 0
	}
	model, err := s.resolveManagedModel(modelID, client)
	if err != nil {
		return 0
	}
	upstreamModel := upstreamModelID(model)
	routePolicy = routePolicyForModelProvider(routePolicy, model.Provider)
	if multipart {
		req := &core.ImageMultipartRequest{Model: modelID}
		plan := s.router.BuildImageMultipartPlan(req, routePolicy)
		plan.Model = upstreamModel
		return s.failover.ImageMultipartCandidateCount(plan, client)
	}
	req := &core.ImageGenerationRequest{Model: modelID}
	plan := s.router.BuildImageGenerationPlan(req, routePolicy)
	plan.Model = upstreamModel
	return s.failover.ImageGenerationCandidateCount(plan, client)
}

func (s *Service) userConcurrentRequestLimit(client *core.APIClient) int {
	limit := s.currentSystemSettings().Runtime.UserConcurrentRequestLimit
	if s == nil || s.repo == nil || client == nil {
		return limit
	}
	ownerID := strings.TrimSpace(client.OwnerUserID)
	if ownerID == "" {
		return limit
	}
	user, err := s.repo.GetUser(ownerID)
	if err != nil || user.ConcurrentRequestLimitOverride == nil {
		return limit
	}
	return *user.ConcurrentRequestLimitOverride
}

func (s *Service) reserveUserConcurrentRequestSlot(client *core.APIClient) (func(), error) {
	releaseUser, err := s.reserveUserConcurrencySlot(client)
	if err != nil {
		return func() {}, err
	}
	releasePlan, err := s.reservePlanConcurrencySlot(client)
	if err != nil {
		releaseUser()
		return func() {}, err
	}
	return func() {
		releasePlan()
		releaseUser()
	}, nil
}

func (s *Service) reserveUserRequestSlot(ctx context.Context, client *core.APIClient) (func(), error) {
	release, err := s.reserveUserConcurrentRequestSlot(client)
	if err != nil {
		return func() {}, err
	}
	if err := s.waitUserRequestRate(ctx, client); err != nil {
		release()
		return func() {}, err
	}
	return release, nil
}

func (s *Service) reserveUserConcurrencySlot(client *core.APIClient) (func(), error) {
	limit := s.userConcurrentRequestLimit(client)
	if limit <= 0 {
		return func() {}, nil
	}
	key := userConcurrencyKey(client)
	if key == "" {
		return func() {}, nil
	}

	s.userConcurrencyMu.Lock()
	defer s.userConcurrencyMu.Unlock()
	if s.userConcurrency == nil {
		s.userConcurrency = make(map[string]int)
	}
	active := s.userConcurrency[key]
	if active >= limit {
		return func() {}, &ConcurrencyLimitError{
			StatusCode: http.StatusTooManyRequests,
			Code:       ErrorCodeRateLimitExceeded,
			Message:    "too many concurrent requests for this user",
			Scope:      "user",
			UserKey:    key,
			Limit:      limit,
			Active:     active,
			Err:        ErrUserConcurrentRequestLimitExceeded,
		}
	}
	s.userConcurrency[key] = active + 1
	released := false
	return func() {
		if released {
			return
		}
		released = true
		s.userConcurrencyMu.Lock()
		defer s.userConcurrencyMu.Unlock()
		active := s.userConcurrency[key]
		switch {
		case active <= 1:
			delete(s.userConcurrency, key)
		default:
			s.userConcurrency[key] = active - 1
		}
	}, nil
}

func (s *Service) planConcurrentRequestLimit(client *core.APIClient) int {
	if clientBillingSource(client) != core.ClientBillingSourcePlan {
		return 0
	}
	return s.currentSystemSettings().Runtime.PlanConcurrentRequestLimit
}

func (s *Service) reservePlanConcurrencySlot(client *core.APIClient) (func(), error) {
	limit := s.planConcurrentRequestLimit(client)
	if limit <= 0 {
		return func() {}, nil
	}
	key := userConcurrencyKey(client)
	if key == "" {
		return func() {}, nil
	}

	s.userConcurrencyMu.Lock()
	defer s.userConcurrencyMu.Unlock()
	if s.planConcurrency == nil {
		s.planConcurrency = make(map[string]int)
	}
	active := s.planConcurrency[key]
	if active >= limit {
		return func() {}, &ConcurrencyLimitError{
			StatusCode: http.StatusTooManyRequests,
			Code:       ErrorCodeRateLimitExceeded,
			Message:    "too many concurrent requests for this plan",
			Scope:      "plan",
			UserKey:    key,
			Limit:      limit,
			Active:     active,
			Err:        ErrPlanConcurrentRequestLimitExceeded,
		}
	}
	s.planConcurrency[key] = active + 1
	released := false
	return func() {
		if released {
			return
		}
		released = true
		s.userConcurrencyMu.Lock()
		defer s.userConcurrencyMu.Unlock()
		active := s.planConcurrency[key]
		switch {
		case active <= 1:
			delete(s.planConcurrency, key)
		default:
			s.planConcurrency[key] = active - 1
		}
	}, nil
}

func (s *Service) userRequestRateLimitPerMinute(client *core.APIClient) int {
	limit := s.currentSystemSettings().Runtime.UserRequestRateLimitPerMinute
	if s == nil || s.repo == nil || client == nil {
		if limit < 0 {
			return 0
		}
		return limit
	}
	ownerID := strings.TrimSpace(client.OwnerUserID)
	if ownerID == "" {
		if limit < 0 {
			return 0
		}
		return limit
	}
	user, err := s.repo.GetUser(ownerID)
	if err != nil || user.RequestRateLimitPerMinuteOverride == nil {
		if limit < 0 {
			return 0
		}
		return limit
	}
	limit = *user.RequestRateLimitPerMinuteOverride
	if limit < 0 {
		return 0
	}
	return limit
}

func (s *Service) waitUserRequestRate(ctx context.Context, client *core.APIClient) error {
	if s == nil {
		return nil
	}
	limit := s.userRequestRateLimitPerMinute(client)
	if limit <= 0 {
		return nil
	}
	key := userConcurrencyKey(client)
	if key == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	interval := time.Minute / time.Duration(limit)
	if interval <= 0 {
		interval = time.Nanosecond
	}

	now := time.Now()
	s.userRateMu.Lock()
	if s.userRateNext == nil {
		s.userRateNext = make(map[string]time.Time)
	}
	s.cleanupUserRequestRateLocked(now)
	scheduled := s.userRateNext[key]
	if scheduled.Before(now) {
		scheduled = now
	}
	next := scheduled.Add(interval)
	s.userRateNext[key] = next
	s.userRateMu.Unlock()

	if !scheduled.After(now) {
		return nil
	}
	timer := time.NewTimer(time.Until(scheduled))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		s.rollbackUserRequestRate(key, scheduled, next)
		return ctx.Err()
	}
}

func (s *Service) rollbackUserRequestRate(key string, scheduled, next time.Time) {
	if s == nil || strings.TrimSpace(key) == "" {
		return
	}
	s.userRateMu.Lock()
	defer s.userRateMu.Unlock()
	if s.userRateNext == nil {
		return
	}
	current := s.userRateNext[key]
	if !current.Equal(next) {
		return
	}
	if scheduled.After(time.Now()) {
		s.userRateNext[key] = scheduled
		return
	}
	delete(s.userRateNext, key)
}

func (s *Service) cleanupUserRequestRateLocked(now time.Time) {
	if s == nil || s.userRateNext == nil {
		return
	}
	if !s.userRateCleanupAt.IsZero() && now.Sub(s.userRateCleanupAt) < time.Minute {
		return
	}
	s.userRateCleanupAt = now
	for key, next := range s.userRateNext {
		if !next.After(now) {
			delete(s.userRateNext, key)
		}
	}
}

func userConcurrencyKey(client *core.APIClient) string {
	if client == nil {
		return ""
	}
	if ownerID := strings.TrimSpace(client.OwnerUserID); ownerID != "" {
		return ownerID
	}
	return strings.TrimSpace(client.ID)
}

func (s *Service) Execute(ctx context.Context, req *core.GatewayRequest) (*core.GatewayResponse, error) {
	requestID := s.newRequestID()
	requestedModel := strings.TrimSpace(req.Model)
	exposedModel := req.Model
	client, routePolicy, err := s.clientForRequest(req.Metadata, req.Client)
	if err != nil {
		return nil, err
	}
	release, err := s.reserveUserRequestSlot(ctx, client)
	if err != nil {
		return nil, err
	}
	defer release()

	model, err := s.resolveManagedModel(exposedModel, client)
	if err != nil {
		return nil, err
	}
	upstreamModel := upstreamModelID(model)
	routePolicy = routePolicyForModelProvider(routePolicy, model.Provider)
	plan := s.router.BuildPlan(req, routePolicy)
	plan.Model = upstreamModel

	billing, err := s.reserveGatewayBilling(requestID, client, model, plan.Model, req)
	if err != nil {
		return nil, err
	}

	started := time.Now()
	result, err := s.failover.Execute(billingAttemptContext(ctx, billing), plan, client, req)
	if err != nil {
		s.releaseBilling(billing, err.Error())
		message := err.Error()
		attempts := []core.AttemptRecord(nil)
		var executionErr *failover.ExecutionError
		if errors.As(err, &executionErr) {
			message = executionErr.Summary()
			attempts = executionErr.Attempts
		}
		requestBody := ""
		_ = s.repo.AppendAudit(core.AuditEvent{
			ID:          "audit_" + requestID,
			Kind:        core.AuditKindGateway,
			RequestID:   requestID,
			ClientID:    auditClientID(req.Metadata, client),
			ClientName:  auditClientName(req.Metadata, client),
			Model:       firstNonEmptyModel(requestedModel, exposedModel),
			Status:      "error",
			Message:     message,
			RequestBody: requestBody,
			Attempts:    attempts,
			DurationMS:  time.Since(started).Milliseconds(),
			CreatedAt:   time.Now().UTC(),
		})
		return nil, err
	}
	resp := result.Response
	s.refreshAccountQuotaAfterUse(resp.AccountID)
	requestBody := ""
	resp.Model = firstNonEmptyModel(requestedModel, exposedModel)

	if err := s.settleGatewayBilling(billing, resp); err != nil {
		return nil, err
	}

	_ = s.repo.AppendAudit(core.AuditEvent{
		ID:          "audit_" + requestID,
		Kind:        core.AuditKindGateway,
		RequestID:   requestID,
		ClientID:    auditClientID(req.Metadata, client),
		ClientName:  auditClientName(req.Metadata, client),
		Provider:    resp.Provider,
		AccountID:   resp.AccountID,
		Model:       resp.Model,
		Status:      "ok",
		Message:     resp.Content,
		RequestBody: requestBody,
		Attempts:    result.Attempts,
		DurationMS:  time.Since(started).Milliseconds(),
		CreatedAt:   time.Now().UTC(),
	})

	return resp, nil
}

func (s *Service) Embed(ctx context.Context, req *core.EmbeddingRequest) (*core.EmbeddingResponse, error) {
	requestID := s.newRequestID()
	requestedModel := strings.TrimSpace(req.Model)
	exposedModel := req.Model
	client, routePolicy, err := s.clientForRequest(req.Metadata, req.Client)
	if err != nil {
		return nil, err
	}
	release, err := s.reserveUserRequestSlot(ctx, client)
	if err != nil {
		return nil, err
	}
	defer release()

	model, err := s.resolveManagedModel(exposedModel, client)
	if err != nil {
		return nil, err
	}
	upstreamModel := upstreamModelID(model)
	routePolicy = routePolicyForModelProvider(routePolicy, model.Provider)
	plan := s.router.BuildEmbeddingPlan(req, routePolicy)
	plan.Model = upstreamModel

	billing, err := s.reserveEmbeddingBilling(requestID, client, model, req)
	if err != nil {
		return nil, err
	}

	started := time.Now()
	result, err := s.failover.ExecuteEmbedding(billingAttemptContext(ctx, billing), plan, client, req)
	if err != nil {
		s.releaseBilling(billing, err.Error())
		message := err.Error()
		attempts := []core.AttemptRecord(nil)
		var executionErr *failover.ExecutionError
		if errors.As(err, &executionErr) {
			message = executionErr.Summary()
			attempts = executionErr.Attempts
		}
		requestBody := ""
		_ = s.repo.AppendAudit(core.AuditEvent{
			ID:          "audit_" + requestID,
			Kind:        core.AuditKindGateway,
			RequestID:   requestID,
			ClientID:    auditClientID(req.Metadata, client),
			ClientName:  auditClientName(req.Metadata, client),
			Model:       firstNonEmptyModel(requestedModel, exposedModel),
			Status:      "error",
			Message:     message,
			RequestBody: requestBody,
			Attempts:    attempts,
			DurationMS:  time.Since(started).Milliseconds(),
			CreatedAt:   time.Now().UTC(),
		})
		return nil, err
	}
	resp := result.Response
	s.refreshAccountQuotaAfterUse(resp.AccountID)
	requestBody := ""
	resp.Model = firstNonEmptyModel(requestedModel, exposedModel)

	if err := s.settleEmbeddingBilling(billing, resp); err != nil {
		return nil, err
	}

	_ = s.repo.AppendAudit(core.AuditEvent{
		ID:          "audit_" + requestID,
		Kind:        core.AuditKindGateway,
		RequestID:   requestID,
		ClientID:    auditClientID(req.Metadata, client),
		ClientName:  auditClientName(req.Metadata, client),
		Provider:    resp.Provider,
		AccountID:   resp.AccountID,
		Model:       resp.Model,
		Status:      "ok",
		Message:     fmt.Sprintf("embeddings=%d", len(resp.Data)),
		RequestBody: requestBody,
		Attempts:    result.Attempts,
		DurationMS:  time.Since(started).Milliseconds(),
		CreatedAt:   time.Now().UTC(),
	})

	return resp, nil
}

func (s *Service) Moderate(ctx context.Context, req *core.ModerationRequest) (*core.ModerationResponse, error) {
	requestID := s.newRequestID()
	requestedModel := strings.TrimSpace(req.Model)
	exposedModel := req.Model
	client, routePolicy, err := s.clientForRequest(req.Metadata, req.Client)
	if err != nil {
		return nil, err
	}
	release, err := s.reserveUserRequestSlot(ctx, client)
	if err != nil {
		return nil, err
	}
	defer release()

	model, err := s.resolveManagedModel(exposedModel, client)
	if err != nil {
		return nil, err
	}
	upstreamModel := upstreamModelID(model)
	routePolicy = routePolicyForModelProvider(routePolicy, model.Provider)
	plan := s.router.BuildModerationPlan(req, routePolicy)
	plan.Model = upstreamModel

	billing, err := s.reserveRequestBilling(requestID, client, model, core.BillingModalityText, "moderation:"+strings.TrimSpace(model.ID), 10000, false, "")
	if err != nil {
		return nil, err
	}

	started := time.Now()
	result, err := s.failover.ExecuteModeration(billingAttemptContext(ctx, billing), plan, client, req)
	if err != nil {
		s.releaseBilling(billing, err.Error())
		message := err.Error()
		attempts := []core.AttemptRecord(nil)
		var executionErr *failover.ExecutionError
		if errors.As(err, &executionErr) {
			message = executionErr.Summary()
			attempts = executionErr.Attempts
		}
		requestBody := ""
		_ = s.repo.AppendAudit(core.AuditEvent{
			ID:          "audit_" + requestID,
			Kind:        core.AuditKindGateway,
			RequestID:   requestID,
			ClientID:    auditClientID(req.Metadata, client),
			ClientName:  auditClientName(req.Metadata, client),
			Model:       firstNonEmptyModel(requestedModel, exposedModel),
			Status:      "error",
			Message:     message,
			RequestBody: requestBody,
			Attempts:    attempts,
			DurationMS:  time.Since(started).Milliseconds(),
			CreatedAt:   time.Now().UTC(),
		})
		return nil, err
	}
	resp := result.Response
	requestBody := ""
	resp.Model = firstNonEmptyModel(requestedModel, exposedModel)

	if err := s.settleFixedBilling(billing, resp.AccountID, resp.Provider, resp.Model); err != nil {
		return nil, err
	}

	_ = s.repo.AppendAudit(core.AuditEvent{
		ID:          "audit_" + requestID,
		Kind:        core.AuditKindGateway,
		RequestID:   requestID,
		ClientID:    auditClientID(req.Metadata, client),
		ClientName:  auditClientName(req.Metadata, client),
		Provider:    resp.Provider,
		AccountID:   resp.AccountID,
		Model:       resp.Model,
		Status:      "ok",
		Message:     "moderation completed",
		RequestBody: requestBody,
		Attempts:    result.Attempts,
		DurationMS:  time.Since(started).Milliseconds(),
		CreatedAt:   time.Now().UTC(),
	})

	return resp, nil
}

func (s *Service) GenerateImage(ctx context.Context, req *core.ImageGenerationRequest) (*core.ImageGenerationResponse, error) {
	requestID := s.newRequestID()
	requestedModel := strings.TrimSpace(req.Model)
	exposedModel := req.Model
	client, routePolicy, err := s.clientForRequest(req.Metadata, req.Client)
	if err != nil {
		return nil, err
	}
	release, err := s.reserveUserRequestSlot(ctx, client)
	if err != nil {
		return nil, err
	}
	defer release()

	model, err := s.resolveManagedModel(exposedModel, client)
	if err != nil {
		return nil, err
	}
	upstreamModel := upstreamModelID(model)
	routePolicy = routePolicyForModelProvider(routePolicy, model.Provider)
	plan := s.router.BuildImageGenerationPlan(req, routePolicy)
	plan.Model = upstreamModel

	billing, err := s.reserveRequestBilling(requestID, client, model, core.BillingModalityImage, "image_generation:"+strings.TrimSpace(model.ID), 10000, false, "")
	if err != nil {
		return nil, err
	}

	started := time.Now()
	result, err := s.failover.ExecuteImageGeneration(billingAttemptContext(ctx, billing), plan, client, req)
	if err != nil {
		s.releaseBilling(billing, err.Error())
		message := err.Error()
		attempts := []core.AttemptRecord(nil)
		var executionErr *failover.ExecutionError
		if errors.As(err, &executionErr) {
			message = executionErr.Summary()
			attempts = executionErr.Attempts
		}
		requestBody := ""
		_ = s.repo.AppendAudit(core.AuditEvent{
			ID:          "audit_" + requestID,
			Kind:        core.AuditKindGateway,
			RequestID:   requestID,
			ClientID:    auditClientID(req.Metadata, client),
			ClientName:  auditClientName(req.Metadata, client),
			Model:       firstNonEmptyModel(requestedModel, exposedModel),
			Status:      "error",
			Message:     message,
			RequestBody: requestBody,
			Attempts:    attempts,
			DurationMS:  time.Since(started).Milliseconds(),
			CreatedAt:   time.Now().UTC(),
		})
		return nil, err
	}
	resp := result.Response
	resp.RequestID = requestID
	requestBody := ""
	resp.Model = firstNonEmptyModel(requestedModel, exposedModel)

	if err := s.settleFixedBilling(billing, resp.AccountID, resp.Provider, resp.Model); err != nil {
		return nil, err
	}

	_ = s.repo.AppendAudit(core.AuditEvent{
		ID:          "audit_" + requestID,
		Kind:        core.AuditKindGateway,
		RequestID:   requestID,
		ClientID:    auditClientID(req.Metadata, client),
		ClientName:  auditClientName(req.Metadata, client),
		Provider:    resp.Provider,
		AccountID:   resp.AccountID,
		Model:       resp.Model,
		Status:      "ok",
		Message:     "image generation completed",
		RequestBody: requestBody,
		Attempts:    result.Attempts,
		DurationMS:  time.Since(started).Milliseconds(),
		CreatedAt:   time.Now().UTC(),
	})

	return resp, nil
}

func (s *Service) StreamImageGeneration(ctx context.Context, req *core.ImageGenerationRequest, emit func(*core.GatewayResponse, *core.StreamEvent) error) error {
	requestID := s.newRequestID()
	requestedModel := strings.TrimSpace(req.Model)
	exposedModel := req.Model
	client, routePolicy, err := s.clientForRequest(req.Metadata, req.Client)
	if err != nil {
		return err
	}
	release, err := s.reserveUserRequestSlot(ctx, client)
	if err != nil {
		return err
	}
	defer release()

	model, err := s.resolveManagedModel(exposedModel, client)
	if err != nil {
		return err
	}
	upstreamModel := upstreamModelID(model)
	routePolicy = routePolicyForModelProvider(routePolicy, model.Provider)
	plan := s.router.BuildImageGenerationPlan(req, routePolicy)
	plan.Model = upstreamModel

	billing, err := s.reserveRequestBilling(requestID, client, model, core.BillingModalityImage, "image_generation_stream:"+strings.TrimSpace(model.ID), 10000, false, "")
	if err != nil {
		return err
	}

	started := time.Now()
	attemptCtx := billingAttemptContext(ctx, billing)
	allAttempts := []core.AttemptRecord(nil)
	var streamResult *failover.StreamOpenResult
	var finalResp core.GatewayResponse
	var streamErr error
	for streamRetries := 0; ; streamRetries++ {
		var err error
		streamResult, err = s.failover.OpenImageGenerationStream(attemptCtx, plan, client, req)
		if err != nil {
			message := err.Error()
			attempts := append([]core.AttemptRecord(nil), allAttempts...)
			var executionErr *failover.ExecutionError
			if errors.As(err, &executionErr) {
				attempts = appendStreamOpenAttempts(attempts, executionErr.Attempts)
				message = (&failover.ExecutionError{Attempts: attempts}).Summary()
			}
			s.releaseBilling(billing, message)
			_ = s.repo.AppendAudit(core.AuditEvent{
				ID:         "audit_" + requestID,
				Kind:       core.AuditKindGateway,
				RequestID:  requestID,
				ClientID:   auditClientID(req.Metadata, client),
				ClientName: auditClientName(req.Metadata, client),
				Model:      firstNonEmptyModel(requestedModel, exposedModel),
				Status:     "error",
				Message:    message,
				Attempts:   attempts,
				DurationMS: time.Since(started).Milliseconds(),
				CreatedAt:  time.Now().UTC(),
			})
			return gatewayDisplayError(err)
		}
		consume := consumeImageStream(requestedModel, exposedModel, streamResult.Session, emit)
		finalResp = consume.Response
		currentAttempts := appendStreamOpenAttempts(append([]core.AttemptRecord(nil), allAttempts...), streamResult.Attempts)
		streamErr = consume.StreamErr
		if consume.UpstreamErr != nil {
			s.failover.RecordStreamFailure(streamResult, consume.UpstreamErr)
			failureAttempt := streamFailureAttemptFromResult(streamResult, consume.UpstreamErr)
			recordBillingStreamFailure(billing, failureAttempt)
			currentAttempts = append(currentAttempts, failureAttempt)
			if shouldRetryStreamBeforeOutput(consume.UpstreamErr, consume.Committed, streamRetries) {
				allAttempts = currentAttempts
				continue
			}
		}
		streamResult.Attempts = currentAttempts
		break
	}
	s.refreshAccountQuotaAfterUse(finalResp.AccountID)

	if streamErr != nil {
		s.releaseBilling(billing, streamErr.Error())
		_ = s.repo.AppendAudit(core.AuditEvent{
			ID:         "audit_" + requestID,
			Kind:       core.AuditKindGateway,
			RequestID:  requestID,
			ClientID:   auditClientID(req.Metadata, client),
			ClientName: auditClientName(req.Metadata, client),
			Provider:   finalResp.Provider,
			AccountID:  finalResp.AccountID,
			Model:      finalResp.Model,
			Status:     "error",
			Message:    streamErr.Error(),
			Attempts:   streamResult.Attempts,
			DurationMS: time.Since(started).Milliseconds(),
			CreatedAt:  time.Now().UTC(),
		})
		return streamErr
	}

	if err := s.settleFixedBilling(billing, finalResp.AccountID, finalResp.Provider, finalResp.Model); err != nil {
		return err
	}
	_ = s.repo.AppendAudit(core.AuditEvent{
		ID:         "audit_" + requestID,
		Kind:       core.AuditKindGateway,
		RequestID:  requestID,
		ClientID:   auditClientID(req.Metadata, client),
		ClientName: auditClientName(req.Metadata, client),
		Provider:   finalResp.Provider,
		AccountID:  finalResp.AccountID,
		Model:      finalResp.Model,
		Status:     "ok",
		Message:    "image generation stream completed",
		Attempts:   streamResult.Attempts,
		DurationMS: time.Since(started).Milliseconds(),
		CreatedAt:  time.Now().UTC(),
	})
	return nil
}

func (s *Service) ProcessImageMultipart(ctx context.Context, req *core.ImageMultipartRequest) (*core.ImageMultipartResponse, error) {
	requestID := s.newRequestID()
	requestedModel := strings.TrimSpace(req.Model)
	exposedModel := req.Model
	client, routePolicy, err := s.clientForRequest(req.Metadata, req.Client)
	if err != nil {
		return nil, err
	}
	release, err := s.reserveUserRequestSlot(ctx, client)
	if err != nil {
		return nil, err
	}
	defer release()

	model, err := s.resolveManagedModel(exposedModel, client)
	if err != nil {
		return nil, err
	}
	upstreamModel := upstreamModelID(model)
	routePolicy = routePolicyForModelProvider(routePolicy, model.Provider)
	plan := s.router.BuildImageMultipartPlan(req, routePolicy)
	plan.Model = upstreamModel

	billing, err := s.reserveRequestBilling(requestID, client, model, core.BillingModalityImage, "image_multipart:"+strings.TrimSpace(model.ID)+":"+strings.TrimSpace(req.Endpoint), 10000, false, "")
	if err != nil {
		return nil, err
	}

	started := time.Now()
	result, err := s.failover.ExecuteImageMultipart(billingAttemptContext(ctx, billing), plan, client, req)
	if err != nil {
		s.releaseBilling(billing, err.Error())
		message := err.Error()
		attempts := []core.AttemptRecord(nil)
		var executionErr *failover.ExecutionError
		if errors.As(err, &executionErr) {
			message = executionErr.Summary()
			attempts = executionErr.Attempts
		}
		requestBody := ""
		_ = s.repo.AppendAudit(core.AuditEvent{
			ID:          "audit_" + requestID,
			Kind:        core.AuditKindGateway,
			RequestID:   requestID,
			ClientID:    auditClientID(req.Metadata, client),
			ClientName:  auditClientName(req.Metadata, client),
			Model:       firstNonEmptyModel(requestedModel, exposedModel),
			Status:      "error",
			Message:     message,
			RequestBody: requestBody,
			Attempts:    attempts,
			DurationMS:  time.Since(started).Milliseconds(),
			CreatedAt:   time.Now().UTC(),
		})
		return nil, err
	}
	resp := result.Response
	resp.RequestID = requestID
	requestBody := ""
	resp.Model = firstNonEmptyModel(requestedModel, exposedModel)

	if err := s.settleFixedBilling(billing, resp.AccountID, resp.Provider, resp.Model); err != nil {
		return nil, err
	}

	_ = s.repo.AppendAudit(core.AuditEvent{
		ID:          "audit_" + requestID,
		Kind:        core.AuditKindGateway,
		RequestID:   requestID,
		ClientID:    auditClientID(req.Metadata, client),
		ClientName:  auditClientName(req.Metadata, client),
		Provider:    resp.Provider,
		AccountID:   resp.AccountID,
		Model:       resp.Model,
		Status:      "ok",
		Message:     fmt.Sprintf("image multipart bytes=%d", len(resp.Body)),
		RequestBody: requestBody,
		Attempts:    result.Attempts,
		DurationMS:  time.Since(started).Milliseconds(),
		CreatedAt:   time.Now().UTC(),
	})

	return resp, nil
}

func (s *Service) StreamImageMultipart(ctx context.Context, req *core.ImageMultipartRequest, emit func(*core.GatewayResponse, *core.StreamEvent) error) error {
	requestID := s.newRequestID()
	requestedModel := strings.TrimSpace(req.Model)
	exposedModel := req.Model
	client, routePolicy, err := s.clientForRequest(req.Metadata, req.Client)
	if err != nil {
		return err
	}
	release, err := s.reserveUserRequestSlot(ctx, client)
	if err != nil {
		return err
	}
	defer release()

	model, err := s.resolveManagedModel(exposedModel, client)
	if err != nil {
		return err
	}
	upstreamModel := upstreamModelID(model)
	routePolicy = routePolicyForModelProvider(routePolicy, model.Provider)
	plan := s.router.BuildImageMultipartPlan(req, routePolicy)
	plan.Model = upstreamModel

	billing, err := s.reserveRequestBilling(requestID, client, model, core.BillingModalityImage, "image_multipart_stream:"+strings.TrimSpace(model.ID)+":"+strings.TrimSpace(req.Endpoint), 10000, false, "")
	if err != nil {
		return err
	}

	started := time.Now()
	attemptCtx := billingAttemptContext(ctx, billing)
	allAttempts := []core.AttemptRecord(nil)
	var streamResult *failover.StreamOpenResult
	var finalResp core.GatewayResponse
	var streamErr error
	for streamRetries := 0; ; streamRetries++ {
		var err error
		streamResult, err = s.failover.OpenImageMultipartStream(attemptCtx, plan, client, req)
		if err != nil {
			message := err.Error()
			attempts := append([]core.AttemptRecord(nil), allAttempts...)
			var executionErr *failover.ExecutionError
			if errors.As(err, &executionErr) {
				attempts = appendStreamOpenAttempts(attempts, executionErr.Attempts)
				message = (&failover.ExecutionError{Attempts: attempts}).Summary()
			}
			s.releaseBilling(billing, message)
			_ = s.repo.AppendAudit(core.AuditEvent{
				ID:         "audit_" + requestID,
				Kind:       core.AuditKindGateway,
				RequestID:  requestID,
				ClientID:   auditClientID(req.Metadata, client),
				ClientName: auditClientName(req.Metadata, client),
				Model:      firstNonEmptyModel(requestedModel, exposedModel),
				Status:     "error",
				Message:    message,
				Attempts:   attempts,
				DurationMS: time.Since(started).Milliseconds(),
				CreatedAt:  time.Now().UTC(),
			})
			return gatewayDisplayError(err)
		}
		consume := consumeImageStream(requestedModel, exposedModel, streamResult.Session, emit)
		finalResp = consume.Response
		currentAttempts := appendStreamOpenAttempts(append([]core.AttemptRecord(nil), allAttempts...), streamResult.Attempts)
		streamErr = consume.StreamErr
		if consume.UpstreamErr != nil {
			s.failover.RecordStreamFailure(streamResult, consume.UpstreamErr)
			failureAttempt := streamFailureAttemptFromResult(streamResult, consume.UpstreamErr)
			recordBillingStreamFailure(billing, failureAttempt)
			currentAttempts = append(currentAttempts, failureAttempt)
			if shouldRetryStreamBeforeOutput(consume.UpstreamErr, consume.Committed, streamRetries) {
				allAttempts = currentAttempts
				continue
			}
		}
		streamResult.Attempts = currentAttempts
		break
	}
	s.refreshAccountQuotaAfterUse(finalResp.AccountID)

	if streamErr != nil {
		s.releaseBilling(billing, streamErr.Error())
		_ = s.repo.AppendAudit(core.AuditEvent{
			ID:         "audit_" + requestID,
			Kind:       core.AuditKindGateway,
			RequestID:  requestID,
			ClientID:   auditClientID(req.Metadata, client),
			ClientName: auditClientName(req.Metadata, client),
			Provider:   finalResp.Provider,
			AccountID:  finalResp.AccountID,
			Model:      finalResp.Model,
			Status:     "error",
			Message:    streamErr.Error(),
			Attempts:   streamResult.Attempts,
			DurationMS: time.Since(started).Milliseconds(),
			CreatedAt:  time.Now().UTC(),
		})
		return streamErr
	}

	if err := s.settleFixedBilling(billing, finalResp.AccountID, finalResp.Provider, finalResp.Model); err != nil {
		return err
	}
	_ = s.repo.AppendAudit(core.AuditEvent{
		ID:         "audit_" + requestID,
		Kind:       core.AuditKindGateway,
		RequestID:  requestID,
		ClientID:   auditClientID(req.Metadata, client),
		ClientName: auditClientName(req.Metadata, client),
		Provider:   finalResp.Provider,
		AccountID:  finalResp.AccountID,
		Model:      finalResp.Model,
		Status:     "ok",
		Message:    "image multipart stream completed",
		Attempts:   streamResult.Attempts,
		DurationMS: time.Since(started).Milliseconds(),
		CreatedAt:  time.Now().UTC(),
	})
	return nil
}

func (s *Service) CreateSpeech(ctx context.Context, req *core.AudioSpeechRequest) (*core.AudioSpeechResponse, error) {
	requestID := s.newRequestID()
	requestedModel := strings.TrimSpace(req.Model)
	exposedModel := req.Model
	client, routePolicy, err := s.clientForRequest(req.Metadata, req.Client)
	if err != nil {
		return nil, err
	}
	release, err := s.reserveUserRequestSlot(ctx, client)
	if err != nil {
		return nil, err
	}
	defer release()

	model, err := s.resolveManagedModel(exposedModel, client)
	if err != nil {
		return nil, err
	}
	upstreamModel := upstreamModelID(model)
	routePolicy = routePolicyForModelProvider(routePolicy, model.Provider)
	plan := s.router.BuildAudioSpeechPlan(req, routePolicy)
	plan.Model = upstreamModel

	billing, err := s.reserveRequestBilling(requestID, client, model, core.BillingModalityAudio, "audio_speech:"+strings.TrimSpace(model.ID), 10000, false, "")
	if err != nil {
		return nil, err
	}

	started := time.Now()
	result, err := s.failover.ExecuteAudioSpeech(billingAttemptContext(ctx, billing), plan, client, req)
	if err != nil {
		s.releaseBilling(billing, err.Error())
		message := err.Error()
		attempts := []core.AttemptRecord(nil)
		var executionErr *failover.ExecutionError
		if errors.As(err, &executionErr) {
			message = executionErr.Summary()
			attempts = executionErr.Attempts
		}
		requestBody := ""
		_ = s.repo.AppendAudit(core.AuditEvent{
			ID:          "audit_" + requestID,
			Kind:        core.AuditKindGateway,
			RequestID:   requestID,
			ClientID:    auditClientID(req.Metadata, client),
			ClientName:  auditClientName(req.Metadata, client),
			Model:       firstNonEmptyModel(requestedModel, exposedModel),
			Status:      "error",
			Message:     message,
			RequestBody: requestBody,
			Attempts:    attempts,
			DurationMS:  time.Since(started).Milliseconds(),
			CreatedAt:   time.Now().UTC(),
		})
		return nil, err
	}
	resp := result.Response
	requestBody := ""
	resp.Model = firstNonEmptyModel(requestedModel, exposedModel)

	if err := s.settleFixedBilling(billing, resp.AccountID, resp.Provider, resp.Model); err != nil {
		return nil, err
	}

	_ = s.repo.AppendAudit(core.AuditEvent{
		ID:          "audit_" + requestID,
		Kind:        core.AuditKindGateway,
		RequestID:   requestID,
		ClientID:    auditClientID(req.Metadata, client),
		ClientName:  auditClientName(req.Metadata, client),
		Provider:    resp.Provider,
		AccountID:   resp.AccountID,
		Model:       resp.Model,
		Status:      "ok",
		Message:     fmt.Sprintf("audio speech bytes=%d", len(resp.Body)),
		RequestBody: requestBody,
		Attempts:    result.Attempts,
		DurationMS:  time.Since(started).Milliseconds(),
		CreatedAt:   time.Now().UTC(),
	})

	return resp, nil
}

func (s *Service) ProcessAudioMultipart(ctx context.Context, req *core.AudioMultipartRequest) (*core.AudioMultipartResponse, error) {
	requestID := s.newRequestID()
	requestedModel := strings.TrimSpace(req.Model)
	exposedModel := req.Model
	client, routePolicy, err := s.clientForRequest(req.Metadata, req.Client)
	if err != nil {
		return nil, err
	}
	release, err := s.reserveUserRequestSlot(ctx, client)
	if err != nil {
		return nil, err
	}
	defer release()

	model, err := s.resolveManagedModel(exposedModel, client)
	if err != nil {
		return nil, err
	}
	upstreamModel := upstreamModelID(model)
	routePolicy = routePolicyForModelProvider(routePolicy, model.Provider)
	plan := s.router.BuildAudioMultipartPlan(req, routePolicy)
	plan.Model = upstreamModel

	billing, err := s.reserveRequestBilling(requestID, client, model, core.BillingModalityAudio, "audio_multipart:"+strings.TrimSpace(model.ID)+":"+strings.TrimSpace(req.Endpoint), 10000, false, "")
	if err != nil {
		return nil, err
	}

	started := time.Now()
	result, err := s.failover.ExecuteAudioMultipart(billingAttemptContext(ctx, billing), plan, client, req)
	if err != nil {
		s.releaseBilling(billing, err.Error())
		message := err.Error()
		attempts := []core.AttemptRecord(nil)
		var executionErr *failover.ExecutionError
		if errors.As(err, &executionErr) {
			message = executionErr.Summary()
			attempts = executionErr.Attempts
		}
		requestBody := ""
		_ = s.repo.AppendAudit(core.AuditEvent{
			ID:          "audit_" + requestID,
			Kind:        core.AuditKindGateway,
			RequestID:   requestID,
			ClientID:    auditClientID(req.Metadata, client),
			ClientName:  auditClientName(req.Metadata, client),
			Model:       firstNonEmptyModel(requestedModel, exposedModel),
			Status:      "error",
			Message:     message,
			RequestBody: requestBody,
			Attempts:    attempts,
			DurationMS:  time.Since(started).Milliseconds(),
			CreatedAt:   time.Now().UTC(),
		})
		return nil, err
	}
	resp := result.Response
	requestBody := ""
	resp.Model = firstNonEmptyModel(requestedModel, exposedModel)

	if err := s.settleFixedBilling(billing, resp.AccountID, resp.Provider, resp.Model); err != nil {
		return nil, err
	}

	_ = s.repo.AppendAudit(core.AuditEvent{
		ID:          "audit_" + requestID,
		Kind:        core.AuditKindGateway,
		RequestID:   requestID,
		ClientID:    auditClientID(req.Metadata, client),
		ClientName:  auditClientName(req.Metadata, client),
		Provider:    resp.Provider,
		AccountID:   resp.AccountID,
		Model:       resp.Model,
		Status:      "ok",
		Message:     fmt.Sprintf("audio multipart bytes=%d", len(resp.Body)),
		RequestBody: requestBody,
		Attempts:    result.Attempts,
		DurationMS:  time.Since(started).Milliseconds(),
		CreatedAt:   time.Now().UTC(),
	})

	return resp, nil
}

func (s *Service) CountTokens(ctx context.Context, req *core.TokenCountRequest) (*core.TokenCountResponse, error) {
	requestID := s.newRequestID()
	requestedModel := strings.TrimSpace(req.Model)
	exposedModel := req.Model
	client, routePolicy, err := s.clientForRequest(req.Metadata, req.Client)
	if err != nil {
		return nil, err
	}
	release, err := s.reserveUserRequestSlot(ctx, client)
	if err != nil {
		return nil, err
	}
	defer release()

	model, err := s.resolveManagedModel(exposedModel, client)
	if err != nil {
		return nil, err
	}
	upstreamModel := upstreamModelID(model)
	routePolicy = routePolicyForModelProvider(routePolicy, model.Provider)
	plan := s.router.BuildTokenCountPlan(req, routePolicy)
	plan.Model = upstreamModel

	billing, err := s.reserveRequestBilling(requestID, client, model, core.BillingModalityText, "token_count:"+strings.TrimSpace(model.ID), 10000, false, "")
	if err != nil {
		return nil, err
	}

	started := time.Now()
	result, err := s.failover.ExecuteTokenCount(billingAttemptContext(ctx, billing), plan, client, req)
	if err != nil {
		s.releaseBilling(billing, err.Error())
		message := err.Error()
		attempts := []core.AttemptRecord(nil)
		var executionErr *failover.ExecutionError
		if errors.As(err, &executionErr) {
			message = executionErr.Summary()
			attempts = executionErr.Attempts
		}
		requestBody := ""
		_ = s.repo.AppendAudit(core.AuditEvent{
			ID:          "audit_" + requestID,
			Kind:        core.AuditKindGateway,
			RequestID:   requestID,
			ClientID:    auditClientID(req.Metadata, client),
			ClientName:  auditClientName(req.Metadata, client),
			Model:       firstNonEmptyModel(requestedModel, exposedModel),
			Status:      "error",
			Message:     message,
			RequestBody: requestBody,
			Attempts:    attempts,
			DurationMS:  time.Since(started).Milliseconds(),
			CreatedAt:   time.Now().UTC(),
		})
		return nil, err
	}
	resp := result.Response
	requestBody := ""
	resp.Model = firstNonEmptyModel(requestedModel, exposedModel)

	if err := s.settleFixedBilling(billing, resp.AccountID, resp.Provider, resp.Model); err != nil {
		return nil, err
	}

	_ = s.repo.AppendAudit(core.AuditEvent{
		ID:          "audit_" + requestID,
		Kind:        core.AuditKindGateway,
		RequestID:   requestID,
		ClientID:    auditClientID(req.Metadata, client),
		ClientName:  auditClientName(req.Metadata, client),
		Provider:    resp.Provider,
		AccountID:   resp.AccountID,
		Model:       resp.Model,
		Status:      "ok",
		Message:     "token count completed",
		RequestBody: requestBody,
		Attempts:    result.Attempts,
		DurationMS:  time.Since(started).Milliseconds(),
		CreatedAt:   time.Now().UTC(),
	})

	return resp, nil
}

func (s *Service) ExecuteStream(ctx context.Context, req *core.GatewayRequest, emit func(*core.GatewayResponse, *core.StreamEvent) error) error {
	requestID := s.newRequestID()
	requestStarted := time.Now()
	requestedModel := strings.TrimSpace(req.Model)
	exposedModel := req.Model
	client, routePolicy, err := s.clientForRequest(req.Metadata, req.Client)
	if err != nil {
		return err
	}
	release, err := s.reserveUserRequestSlot(ctx, client)
	if err != nil {
		return err
	}
	defer release()

	model, err := s.resolveManagedModel(exposedModel, client)
	if err != nil {
		return err
	}
	upstreamModel := upstreamModelID(model)
	routePolicy = routePolicyForModelProvider(routePolicy, model.Provider)
	plan := s.router.BuildPlan(req, routePolicy)
	plan.Model = upstreamModel

	billing, err := s.reserveGatewayBilling(requestID, client, model, plan.Model, req)
	if err != nil {
		return err
	}

	started := time.Now()
	requestBody := ""
	attemptCtx := billingAttemptContext(ctx, billing)
	allAttempts := []core.AttemptRecord(nil)
	var streamResult *failover.StreamOpenResult
	var finalResp core.GatewayResponse
	var streamErr error
	sameAccountStreamRetries := 0
	var retryReq *core.GatewayRequest
	for streamRetries := 0; ; streamRetries++ {
		var err error
		openReq := req
		if retryReq != nil {
			openReq = retryReq
		}
		streamResult, err = s.failover.OpenStream(attemptCtx, plan, client, openReq)
		if err != nil {
			message := err.Error()
			attempts := append([]core.AttemptRecord(nil), allAttempts...)
			var executionErr *failover.ExecutionError
			if errors.As(err, &executionErr) {
				attempts = appendStreamOpenAttempts(attempts, executionErr.Attempts)
				message = (&failover.ExecutionError{Attempts: attempts}).Summary()
			}
			s.releaseBilling(billing, message)
			_ = s.repo.AppendAudit(core.AuditEvent{
				ID:          "audit_" + requestID,
				Kind:        core.AuditKindGateway,
				RequestID:   requestID,
				ClientID:    auditClientID(req.Metadata, client),
				ClientName:  auditClientName(req.Metadata, client),
				Model:       firstNonEmptyModel(requestedModel, exposedModel),
				Status:      "error",
				Message:     message,
				RequestBody: requestBody,
				Attempts:    attempts,
				DurationMS:  time.Since(started).Milliseconds(),
				CreatedAt:   time.Now().UTC(),
			})
			return err
		}

		if emit != nil && streamResult != nil && streamResult.Session != nil && streamResult.Session.Response != nil &&
			(req == nil || strings.TrimSpace(req.UpstreamMode) != "anthropic_messages") {
			if err := emit(streamResult.Session.Response, &core.StreamEvent{Started: true}); err != nil {
				streamResult.Attempts = appendStreamOpenAttempts(append([]core.AttemptRecord(nil), allAttempts...), streamResult.Attempts)
				if streamResult.Session.Stream != nil {
					_ = streamResult.Session.Stream.Close()
				}
				streamErr = err
				break
			}
		}
		consume := consumeGatewayStream(requestStarted, requestedModel, exposedModel, streamResult.Session, emit)
		finalResp = consume.Response
		currentAttempts := appendStreamOpenAttempts(append([]core.AttemptRecord(nil), allAttempts...), streamResult.Attempts)
		streamErr = consume.StreamErr
		if consume.UpstreamErr != nil {
			failureAttempt := streamFailureAttemptFromResult(streamResult, consume.UpstreamErr)
			currentAttempts = append(currentAttempts, failureAttempt)
			if sameAccountStreamRetries < maxPreOutputSameAccountStreamRetries &&
				shouldRetrySameAccountStreamBeforeOutput(client, consume.UpstreamErr, consume.Committed) {
				allAttempts = currentAttempts
				sameAccountStreamRetries++
				retryReq = gatewayRequestWithStrictAccount(openReq, streamResult.Decision.Account.ID)
				continue
			}
			s.failover.RecordStreamFailure(streamResult, consume.UpstreamErr)
			recordBillingStreamFailure(billing, failureAttempt)
			if shouldRetryStreamBeforeOutput(consume.UpstreamErr, consume.Committed, streamRetries) {
				allAttempts = currentAttempts
				retryReq = gatewayRequestExcludingAccount(openReq, failureAttempt.AccountID)
				sameAccountStreamRetries = 0
				continue
			}
		}
		streamResult.Attempts = currentAttempts
		break
	}
	s.refreshAccountQuotaAfterUse(finalResp.AccountID)

	if streamErr != nil {
		s.releaseBilling(billing, streamErr.Error())
		_ = s.repo.AppendAudit(core.AuditEvent{
			ID:          "audit_" + requestID,
			Kind:        core.AuditKindGateway,
			RequestID:   requestID,
			ClientID:    auditClientID(req.Metadata, client),
			ClientName:  auditClientName(req.Metadata, client),
			Provider:    finalResp.Provider,
			AccountID:   finalResp.AccountID,
			Model:       finalResp.Model,
			Status:      "error",
			Message:     streamErr.Error(),
			RequestBody: requestBody,
			Attempts:    streamResult.Attempts,
			DurationMS:  time.Since(started).Milliseconds(),
			CreatedAt:   time.Now().UTC(),
		})
		return streamErr
	}

	if err := s.settleGatewayBilling(billing, &finalResp); err != nil {
		return err
	}

	_ = s.repo.AppendAudit(core.AuditEvent{
		ID:          "audit_" + requestID,
		Kind:        core.AuditKindGateway,
		RequestID:   requestID,
		ClientID:    auditClientID(req.Metadata, client),
		ClientName:  auditClientName(req.Metadata, client),
		Provider:    finalResp.Provider,
		AccountID:   finalResp.AccountID,
		Model:       finalResp.Model,
		Status:      "ok",
		Message:     finalResp.Content,
		RequestBody: requestBody,
		Attempts:    streamResult.Attempts,
		DurationMS:  time.Since(started).Milliseconds(),
		CreatedAt:   time.Now().UTC(),
	})

	return nil
}

type configRevisionProvider interface {
	ConfigRevision() uint64
}

type modelRevisionProvider interface {
	ModelRevision() uint64
}

type accountRevisionProvider interface {
	AccountRevision() uint64
}

func (c *atomicModelCache) load() (gatewayModelCache, bool) {
	if c == nil {
		return gatewayModelCache{}, false
	}
	value := c.value.Load()
	if value == nil {
		return gatewayModelCache{}, false
	}
	cache, ok := value.(gatewayModelCache)
	return cache, ok
}

func (c *atomicModelCache) store(cache gatewayModelCache) {
	c.value.Store(cache)
}

func (c *atomicGroupRoutePolicyCache) load() (groupRoutePolicyCache, bool) {
	value := c.value.Load()
	if value == nil {
		return groupRoutePolicyCache{}, false
	}
	cache, ok := value.(groupRoutePolicyCache)
	return cache, ok
}

func (c *atomicGroupRoutePolicyCache) store(cache groupRoutePolicyCache) {
	c.value.Store(cache)
}

type limitedStringBuilder struct {
	builder   strings.Builder
	limit     int
	count     int
	truncated bool
}

func (b *limitedStringBuilder) WriteString(value string) {
	if value == "" || b.limit <= 0 || b.truncated {
		return
	}
	for _, r := range value {
		if b.count >= b.limit {
			b.truncated = true
			return
		}
		b.builder.WriteRune(r)
		b.count++
	}
}

func (b *limitedStringBuilder) String() string {
	if b.truncated {
		return b.builder.String() + "...[truncated]"
	}
	return b.builder.String()
}

func (s *Service) resolveManagedModel(modelID string, client *core.APIClient) (core.ModelConfig, error) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return core.ModelConfig{}, &ModelUnavailableError{Model: modelID}
	}
	if cache, ok := s.modelCache(); ok {
		model, ok := cache.models[modelID]
		if ok && model.Enabled && s.modelVisibleToClient(model, client) {
			return model, nil
		}
		return core.ModelConfig{}, &ModelUnavailableError{Model: modelID}
	}
	model, err := s.repo.GetModel(modelID)
	if err == nil && model.Enabled && s.modelVisibleToClient(model, client) {
		return model, nil
	}
	return core.ModelConfig{}, &ModelUnavailableError{Model: modelID}
}

func (s *Service) modelVisibleToClient(model core.ModelConfig, client *core.APIClient) bool {
	if len(model.VisibleGroups) == 0 {
		return false
	}
	groups := s.modelVisibilityGroupsForClient(client)
	for _, group := range model.VisibleGroups {
		for _, candidate := range groups {
			if strings.EqualFold(gatewayAccountGroupName(group), candidate) {
				return true
			}
		}
	}
	return false
}

func (s *Service) modelVisibilityGroupsForClient(client *core.APIClient) []string {
	if client == nil {
		return []string{core.DefaultAccountGroupName}
	}
	return uniqueGatewayModelGroups([]string{clientAccountGroup(client)})
}

func uniqueGatewayModelGroups(groups []string) []string {
	out := make([]string, 0, len(groups))
	seen := map[string]bool{}
	for _, group := range groups {
		group = gatewayAccountGroupName(group)
		key := strings.ToLower(group)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, group)
	}
	return out
}

func (s *Service) modelCache() (gatewayModelCache, bool) {
	revision := modelRevision(s.repo)
	if revision == 0 {
		return gatewayModelCache{}, false
	}
	if cached, ok := s.models.load(); ok && cached.revision == revision {
		return cached, true
	}

	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()
	if cached, ok := s.models.load(); ok && cached.revision == revision {
		return cached, true
	}
	models := s.repo.ListModels()
	byID := make(map[string]core.ModelConfig, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	cache := gatewayModelCache{
		revision: revision,
		models:   byID,
	}
	s.models.store(cache)
	return cache, true
}

func modelRevision(repo storage.Repository) uint64 {
	if revisioned, ok := repo.(modelRevisionProvider); ok {
		return revisioned.ModelRevision()
	}
	if revisioned, ok := repo.(configRevisionProvider); ok {
		return revisioned.ConfigRevision()
	}
	return 0
}

func upstreamModelID(model core.ModelConfig) string {
	upstreamID := strings.TrimSpace(model.UpstreamID)
	if upstreamID != "" {
		return upstreamID
	}
	return strings.TrimSpace(model.ID)
}

func firstNonEmptyModel(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

type gatewayDisplayErr struct {
	message string
	err     error
}

func (e *gatewayDisplayErr) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func (e *gatewayDisplayErr) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func gatewayDisplayError(err error) error {
	var executionErr *failover.ExecutionError
	if errors.As(err, &executionErr) {
		return &gatewayDisplayErr{message: executionErr.Summary(), err: err}
	}
	return err
}

func streamImageGatewayResponse(session *providers.StreamSession, requestedModel, exposedModel string) core.GatewayResponse {
	resp := core.GatewayResponse{
		Model:        firstNonEmptyModel(requestedModel, exposedModel),
		CreatedAt:    time.Now().UTC(),
		FinishReason: "stream",
	}
	if session != nil && session.Response != nil {
		resp = *session.Response
		resp.Model = firstNonEmptyModel(requestedModel, exposedModel, resp.Model)
		if resp.CreatedAt.IsZero() {
			resp.CreatedAt = time.Now().UTC()
		}
		if strings.TrimSpace(resp.FinishReason) == "" {
			resp.FinishReason = "stream"
		}
	}
	return resp
}

func routePolicyForModelProvider(policy core.RoutePolicy, provider core.ProviderKind) core.RoutePolicy {
	if provider == "" {
		return policy
	}
	providers := make([]core.ProviderKind, 0, 1+len(policy.FallbackProviders))
	providers = appendUniqueProviderKind(providers, provider)
	providers = appendUniqueProviderKind(providers, policy.DefaultProvider)
	for _, fallback := range policy.FallbackProviders {
		providers = appendUniqueProviderKind(providers, fallback)
	}
	return core.RoutePolicy{DefaultProvider: providers[0], FallbackProviders: providers[1:], Rules: policy.Rules}
}

func (s *Service) clientForMetadata(metadata map[string]string) (*core.APIClient, core.RoutePolicy, error) {
	if len(metadata) == 0 {
		return nil, core.DefaultRoutePolicy(), nil
	}

	clientID := strings.TrimSpace(metadata["client_id"])
	if clientID == "" {
		return nil, core.DefaultRoutePolicy(), nil
	}

	client, err := s.repo.GetClient(clientID)
	if err != nil {
		return nil, core.DefaultRoutePolicy(), nil
	}

	return s.clientForRoute(metadata, &client)
}

func (s *Service) clientForRequest(metadata map[string]string, requestClient *core.APIClient) (*core.APIClient, core.RoutePolicy, error) {
	if requestClient == nil {
		return s.clientForMetadata(metadata)
	}
	return s.clientForRoute(metadata, requestClient)
}

func (s *Service) clientForRoute(metadata map[string]string, client *core.APIClient) (*core.APIClient, core.RoutePolicy, error) {
	if client == nil {
		return nil, core.DefaultRoutePolicy(), nil
	}
	if err := s.ensureClientAccountGroupUsable(client); err != nil {
		return nil, core.DefaultRoutePolicy(), err
	}
	routePolicy := s.routePolicyForClient(client)
	if routePolicy.DefaultProvider == "" {
		routePolicy = core.DefaultRoutePolicy()
	}
	affinityKey := routeAffinityKey(metadata, client.ID)
	if affinityKey == "" || affinityKey == strings.TrimSpace(client.ID) {
		return client, routePolicy, nil
	}
	routedClient := *client
	routedClient.RouteAffinityKey = affinityKey
	routedClient.CacheAffinityRoute = explicitCacheAffinityRoute(metadata)
	return &routedClient, routePolicy, nil
}

func (s *Service) ensureClientAccountGroupUsable(client *core.APIClient) error {
	if s == nil || s.repo == nil || client == nil {
		return nil
	}
	groupName := clientAccountGroup(client)
	if strings.EqualFold(groupName, core.DefaultAccountGroupName) {
		return nil
	}
	ownerID := strings.TrimSpace(client.OwnerUserID)
	if ownerID == "" {
		return nil
	}
	for _, group := range s.repo.ListAccountGroups() {
		if !strings.EqualFold(gatewayAccountGroupName(group.Name), groupName) {
			continue
		}
		if core.AccountGroupVisibleInClientEditorForUser(group, ownerID) {
			return nil
		}
		return &AccessError{
			StatusCode: http.StatusForbidden,
			Code:       ErrorCodeAccountGroupForbidden,
			Message:    fmt.Sprintf("account group %q is not available to this API key owner", groupName),
		}
	}
	return nil
}

func (s *Service) routePolicyForClient(client *core.APIClient) core.RoutePolicy {
	if client == nil {
		return core.DefaultRoutePolicy()
	}
	groupName := clientAccountGroup(client)
	policies, ok := s.groupRoutePolicies()
	if !ok {
		return client.RoutePolicy
	}
	if policy, ok := policies[strings.ToLower(groupName)]; ok && policy.DefaultProvider != "" {
		return policy
	}
	return client.RoutePolicy
}

func (s *Service) groupRoutePolicies() (map[string]core.RoutePolicy, bool) {
	revision := accountRevision(s.repo)
	if revision == 0 {
		return s.buildGroupRoutePolicies(), true
	}
	if cached, ok := s.groupRoutes.load(); ok && cached.revision == revision {
		return cached.byGroup, true
	}

	s.groupRoutesMu.Lock()
	defer s.groupRoutesMu.Unlock()
	if cached, ok := s.groupRoutes.load(); ok && cached.revision == revision {
		return cached.byGroup, true
	}
	cache := groupRoutePolicyCache{
		revision: revision,
		byGroup:  s.buildGroupRoutePolicies(),
	}
	s.groupRoutes.store(cache)
	return cache.byGroup, true
}

func (s *Service) buildGroupRoutePolicies() map[string]core.RoutePolicy {
	byGroup := make(map[string]core.RoutePolicy)
	now := time.Now().UTC()
	for _, account := range s.repo.ListAccounts() {
		account = core.NormalizeAccountRuntimeState(account, now)
		groupName := strings.ToLower(gatewayAccountGroupName(account.Group))
		if groupName == "" || !accountAvailableForRoute(account, now) {
			continue
		}
		byGroup[groupName] = appendRoutePolicyProvider(byGroup[groupName], account.Provider)
	}
	return byGroup
}

func appendRoutePolicyProvider(policy core.RoutePolicy, provider core.ProviderKind) core.RoutePolicy {
	providers := make([]core.ProviderKind, 0, 1+len(policy.FallbackProviders))
	providers = appendUniqueProviderKind(providers, policy.DefaultProvider)
	for _, fallback := range policy.FallbackProviders {
		providers = appendUniqueProviderKind(providers, fallback)
	}
	if containsRouteProvider(providers, provider) {
		return policy
	}
	providers = append(providers, provider)
	return core.RoutePolicy{DefaultProvider: providers[0], FallbackProviders: providers[1:]}
}

func accountAvailableForRoute(account core.Account, now time.Time) bool {
	return core.AccountAvailableForRouting(account, now)
}

func containsRouteProvider(providers []core.ProviderKind, target core.ProviderKind) bool {
	for _, provider := range providers {
		if provider == target {
			return true
		}
	}
	return false
}

func appendUniqueProviderKind(providers []core.ProviderKind, provider core.ProviderKind) []core.ProviderKind {
	if provider == "" || containsRouteProvider(providers, provider) {
		return providers
	}
	return append(providers, provider)
}

func gatewayAccountGroupName(group string) string {
	return core.NormalizeAccountGroupName(group)
}

func accountRevision(repo storage.Repository) uint64 {
	if revisioned, ok := repo.(accountRevisionProvider); ok {
		return revisioned.AccountRevision()
	}
	return 0
}

func auditClientID(metadata map[string]string, client *core.APIClient) string {
	if len(metadata) > 0 {
		if id := strings.TrimSpace(metadata["client_id"]); id != "" {
			return id
		}
	}
	if client == nil {
		return ""
	}
	return strings.TrimSpace(client.ID)
}

func auditClientName(metadata map[string]string, client *core.APIClient) string {
	if len(metadata) > 0 {
		if name := strings.TrimSpace(metadata["client_name"]); name != "" {
			return name
		}
	}
	if client == nil {
		return ""
	}
	return strings.TrimSpace(client.Name)
}

func routeAffinityKey(metadata map[string]string, clientID string) string {
	cacheKey := strings.TrimSpace(metadata["prompt_cache_key"])
	if cacheKey == "" {
		cacheKey = strings.TrimSpace(metadata["cache_affinity_key"])
	}
	if cacheKey == "" {
		cacheKey = strings.TrimSpace(metadata["route_affinity_key"])
	}
	clientID = strings.TrimSpace(clientID)
	model := strings.TrimSpace(metadata["route_affinity_model"])
	if cacheKey == "" {
		return clientID
	}
	if clientID != "" && model != "" {
		return clientID + "\x00" + model + "\x00" + cacheKey
	}
	if clientID != "" {
		return clientID + "\x00" + cacheKey
	}
	if model != "" {
		return model + "\x00" + cacheKey
	}
	return cacheKey
}

func explicitCacheAffinityRoute(metadata map[string]string) bool {
	if len(metadata) == 0 {
		return false
	}
	if strings.TrimSpace(metadata["prompt_cache_key"]) != "" {
		return true
	}
	if strings.TrimSpace(metadata["cache_affinity_key"]) != "" {
		return true
	}
	return strings.TrimSpace(metadata["route_affinity_key"]) != ""
}
