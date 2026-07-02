package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/failover"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

type ResponsesBindingError struct {
	ResponseID string
	Err        error
}

func (e *ResponsesBindingError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.ResponseID) == "" {
		return "previous response not found"
	}
	return fmt.Sprintf("previous response %q not found", strings.TrimSpace(e.ResponseID))
}

func (e *ResponsesBindingError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (s *Service) ExecuteResponses(ctx context.Context, req *core.ResponsesRequest) (*core.GatewayResponse, error) {
	requestID := s.newRequestID()
	requestedModel := strings.TrimSpace(req.Model)
	exposedModel := req.Model
	client, routePolicy, err := s.clientForRequest(req.Metadata, req.Client)
	if err != nil {
		return nil, err
	}
	if err := s.applyResponsesAccountBinding(req, client); err != nil {
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
	plan := s.router.BuildResponsesPlan(req, routePolicy)
	plan.Model = upstreamModel

	billing, err := s.reserveResponsesBilling(requestID, client, model, plan.Model, req)
	if err != nil {
		return nil, err
	}

	started := time.Now()
	result, activeReq, err := s.executeResponsesWithPreviousFallback(billingAttemptContext(ctx, billing), plan, client, req)
	if err != nil {
		s.releaseBilling(billing, err.Error())
		message := err.Error()
		attempts := []core.AttemptRecord(nil)
		var executionErr *failover.ExecutionError
		if errors.As(err, &executionErr) {
			message = executionErr.Summary()
			attempts = executionErr.Attempts
		}
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
		return nil, err
	}
	resp := result.Response
	s.refreshAccountQuotaAfterUse(resp.AccountID)
	resp.Model = firstNonEmptyModel(requestedModel, exposedModel)

	if err := s.settleGatewayBilling(billing, resp); err != nil {
		return nil, err
	}
	if shouldRememberResponsesBinding(activeReq, resp) {
		s.rememberResponsesBinding(activeReq, resp, client)
	}

	_ = s.repo.AppendAudit(core.AuditEvent{
		ID:         "audit_" + requestID,
		Kind:       core.AuditKindGateway,
		RequestID:  requestID,
		ClientID:   auditClientID(req.Metadata, client),
		ClientName: auditClientName(req.Metadata, client),
		Provider:   resp.Provider,
		AccountID:  resp.AccountID,
		Model:      resp.Model,
		Status:     "ok",
		Message:    resp.Content,
		Attempts:   result.Attempts,
		DurationMS: time.Since(started).Milliseconds(),
		CreatedAt:  time.Now().UTC(),
	})

	return resp, nil
}

func (s *Service) ExecuteResponsesStream(ctx context.Context, req *core.ResponsesRequest, emit func(*core.GatewayResponse, *core.StreamEvent) error) error {
	return s.executeResponsesStream(ctx, req, emit, false)
}

type responsesStreamConsumeResult struct {
	Response     core.GatewayResponse
	Committed    bool
	TerminalSeen bool
	StreamErr    error
	UpstreamErr  error
}

func consumeResponsesStream(requestStarted time.Time, requestedModel, exposedModel string, session *providers.StreamSession, emit func(*core.GatewayResponse, *core.StreamEvent) error) responsesStreamConsumeResult {
	if session == nil || session.Response == nil || session.Stream == nil {
		err := &providers.InvokeError{
			Code:      providers.ErrorCodeUpstreamReadError,
			Temporary: true,
			Cooldown:  20 * time.Second,
			Err:       errors.New("responses stream session is not initialized"),
		}
		return responsesStreamConsumeResult{StreamErr: err, UpstreamErr: err}
	}
	session.Response.Model = firstNonEmptyModel(requestedModel, exposedModel)
	content := limitedStringBuilder{limit: maxStreamCollectedContentRunes}
	finishReason := session.Response.FinishReason
	usage := session.Response.Usage
	var streamErr error
	var upstreamErr error
	terminalSeen := false
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
		if event.Done || event.FinishReason != "" {
			terminalSeen = true
		}
		if !committed {
			if failureErr := streamEventPreOutputError(event); failureErr != nil {
				streamErr = failureErr
				upstreamErr = failureErr
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

	if streamErr == nil && !terminalSeen {
		streamErr = &providers.InvokeError{
			Code:      providers.ErrorCodeUpstreamReadError,
			Temporary: true,
			Cooldown:  20 * time.Second,
			Err:       fmt.Errorf("responses stream closed before terminal event"),
		}
		upstreamErr = streamErr
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
	return responsesStreamConsumeResult{
		Response:     finalResp,
		Committed:    committed,
		TerminalSeen: terminalSeen,
		StreamErr:    streamErr,
		UpstreamErr:  upstreamErr,
	}
}

func (s *Service) executeResponsesStream(ctx context.Context, req *core.ResponsesRequest, emit func(*core.GatewayResponse, *core.StreamEvent) error, forceSSEUpstream bool) error {
	requestID := s.newRequestID()
	requestStarted := time.Now()
	requestedModel := strings.TrimSpace(req.Model)
	exposedModel := req.Model
	client, routePolicy, err := s.clientForRequest(req.Metadata, req.Client)
	if err != nil {
		return err
	}
	if err := s.applyResponsesAccountBinding(req, client); err != nil {
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
	plan := s.router.BuildResponsesPlan(req, routePolicy)
	plan.Model = upstreamModel
	upstreamReq := s.responsesStreamUpstreamRequest(req, forceSSEUpstream)

	billing, err := s.reserveResponsesBilling(requestID, client, model, plan.Model, req)
	if err != nil {
		return err
	}

	started := time.Now()
	attemptCtx := billingAttemptContext(ctx, billing)
	allAttempts := []core.AttemptRecord(nil)
	var streamResult *failover.StreamOpenResult
	activeReq := upstreamReq
	var finalResp core.GatewayResponse
	var streamErr error
	sameAccountStreamRetries := 0
	var retryReq *core.ResponsesRequest
	for streamRetries := 0; ; streamRetries++ {
		openReq := activeReq
		if retryReq != nil {
			openReq = retryReq
		}
		var err error
		streamResult, activeReq, err = s.openResponsesStreamWithPreviousFallback(attemptCtx, plan, client, openReq)
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
			return err
		}

		consume := consumeResponsesStream(requestStarted, requestedModel, exposedModel, streamResult.Session, emit)
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
				retryReq = responsesRequestWithStrictAccount(activeReq, streamResult.Decision.Account.ID)
				continue
			}
			retryReq = nil
			sameAccountStreamRetries = 0
			s.failover.RecordStreamFailure(streamResult, consume.UpstreamErr)
			recordBillingStreamFailure(billing, failureAttempt)
			if !consume.Committed && shouldRetryResponsesWithoutPreviousResponseAfterPreOutputFailure(activeReq, streamResult.Decision.Account, failureAttempt, consume.UpstreamErr) {
				allAttempts = currentAttempts
				activeReq = responsesRequestWithoutPreviousResponse(activeReq)
				retryReq = nil
				sameAccountStreamRetries = 0
				continue
			}
			if shouldRetryStreamBeforeOutput(consume.UpstreamErr, consume.Committed, streamRetries) {
				allAttempts = currentAttempts
				retryReq = nil
				sameAccountStreamRetries = 0
				activeReq = responsesRequestWithoutStrictAccount(activeReq)
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

	if err := s.settleGatewayBilling(billing, &finalResp); err != nil {
		return err
	}
	if shouldRememberResponsesBinding(activeReq, &finalResp) {
		s.rememberResponsesBinding(activeReq, &finalResp, client)
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
		Message:    finalResp.Content,
		Attempts:   streamResult.Attempts,
		DurationMS: time.Since(started).Milliseconds(),
		CreatedAt:  time.Now().UTC(),
	})

	return nil
}

func (s *Service) reserveResponsesBilling(requestID string, client *core.APIClient, model core.ModelConfig, routedModel string, req *core.ResponsesRequest) (*billingHold, error) {
	if req == nil {
		return nil, nil
	}
	if responsesGenerateDisabled(req) {
		return nil, nil
	}
	serviceTier := responsesRequestServiceTier(req)
	fastMode := gatewayServiceTierIsFast(serviceTier)
	fastModelID := fastBillingModelID(model, routedModel)
	fastBps := fastBillingMultiplierBps(serviceTier, fastModelID)
	promptTokens := estimateResponsesPromptTokens(req)
	completionTokens := estimateResponsesCompletionTokens(req)
	fingerprint := fmt.Sprintf("responses:%s:%d:%d", strings.TrimSpace(model.ID), promptTokens, completionTokens)
	cacheDiagnostics := billingCacheDiagnosticsForResponsesRequest(req)
	if normalizeModelBillingMode(model.BillingMode) == core.ModelBillingModeRequest {
		return s.reserveRequestBilling(requestID, client, model, core.BillingModalityText, fingerprint, fastBps, fastMode, fastModelID, "", cacheDiagnostics)
	}
	return s.reserveBilling(requestID, client, model, promptTokens, completionTokens, fingerprint, fastBps, fastMode, fastModelID, cacheDiagnostics)
}

func (s *Service) executeResponsesWithPreviousFallback(ctx context.Context, plan core.RoutePlan, client *core.APIClient, req *core.ResponsesRequest) (*failover.ResponsesResult, *core.ResponsesRequest, error) {
	result, err := s.failover.ExecuteResponses(ctx, plan, client, req)
	if err == nil || !shouldRetryResponsesWithoutPreviousResponse(req, err) {
		return result, req, err
	}
	initialAttempts := executionAttempts(err)
	retryReq := responsesRequestWithoutPreviousResponse(req)
	retryResult, retryErr := s.failover.ExecuteResponses(ctx, plan, client, retryReq)
	if retryErr != nil {
		return nil, retryReq, mergeExecutionAttempts(initialAttempts, retryErr)
	}
	retryResult.Attempts = append(initialAttempts, retryResult.Attempts...)
	return retryResult, retryReq, nil
}

func (s *Service) openResponsesStreamWithPreviousFallback(ctx context.Context, plan core.RoutePlan, client *core.APIClient, req *core.ResponsesRequest) (*failover.StreamOpenResult, *core.ResponsesRequest, error) {
	result, err := s.failover.OpenResponsesStream(ctx, plan, client, req)
	if err == nil || !shouldRetryResponsesWithoutPreviousResponse(req, err) {
		return result, req, err
	}
	initialAttempts := executionAttempts(err)
	retryReq := responsesRequestWithoutPreviousResponse(req)
	retryResult, retryErr := s.failover.OpenResponsesStream(ctx, plan, client, retryReq)
	if retryErr != nil {
		return nil, retryReq, mergeExecutionAttempts(initialAttempts, retryErr)
	}
	retryResult.Attempts = append(initialAttempts, retryResult.Attempts...)
	return retryResult, retryReq, nil
}

func shouldRetryResponsesWithoutPreviousResponse(req *core.ResponsesRequest, err error) bool {
	if req == nil || err == nil || req.Compact {
		return false
	}
	if !req.StrictAccountAffinity || strings.TrimSpace(req.PreferredAccountID) == "" || strings.TrimSpace(req.PreviousResponseID) == "" {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	attempts := executionAttempts(err)
	if len(attempts) == 0 {
		return false
	}
	attempt, ok := lastRelevantResponsesBoundAttempt(strings.TrimSpace(req.PreferredAccountID), attempts)
	if !ok {
		return false
	}
	return responsesBoundAttemptAllowsPreviousFallback(strings.TrimSpace(req.PreferredAccountID), attempt)
}

func lastRelevantResponsesBoundAttempt(boundAccountID string, attempts []core.AttemptRecord) (core.AttemptRecord, bool) {
	for i := len(attempts) - 1; i >= 0; i-- {
		attempt := attempts[i]
		if strings.TrimSpace(attempt.ErrorCode) == "bound_account_unavailable" {
			return attempt, true
		}
		accountID := strings.TrimSpace(attempt.AccountID)
		if accountID == "" {
			continue
		}
		if boundAccountID == "" || strings.EqualFold(accountID, boundAccountID) {
			return attempt, true
		}
	}
	return core.AttemptRecord{}, false
}

func responsesBoundAttemptAllowsPreviousFallback(boundAccountID string, attempt core.AttemptRecord) bool {
	code := strings.TrimSpace(attempt.ErrorCode)
	if code == "bound_account_unavailable" {
		return true
	}
	if boundAccountID != "" && strings.TrimSpace(attempt.AccountID) != "" && !strings.EqualFold(strings.TrimSpace(attempt.AccountID), boundAccountID) {
		return false
	}
	account := core.Account{ID: strings.TrimSpace(attempt.AccountID)}
	if code == providers.ErrorCodeUpstreamServerError {
		account.Provider = attempt.Provider
	}
	err := &providers.InvokeError{
		Code:      code,
		Temporary: attempt.Temporary,
		Err:       errors.New(strings.TrimSpace(attempt.ErrorMessage)),
	}
	return providers.FailurePolicyFor(account, err).PreviousFallback
}

func shouldRetryResponsesWithoutPreviousResponseAfterPreOutputFailure(req *core.ResponsesRequest, account core.Account, attempt core.AttemptRecord, err error) bool {
	if req == nil || err == nil {
		return false
	}
	policy := providers.FailurePolicyFor(account, err)
	if policy.Scope == providers.FailureScopeSharedUpstream && strings.TrimSpace(policy.SharedUpstreamScope) != "" {
		return false
	}
	return shouldRetryResponsesWithoutPreviousResponse(req, &failover.ExecutionError{Attempts: []core.AttemptRecord{attempt}})
}

func responsesRequestWithoutPreviousResponse(req *core.ResponsesRequest) *core.ResponsesRequest {
	if req == nil {
		return nil
	}
	clone := *req
	clone.PreviousResponseID = ""
	clone.PreferredAccountID = ""
	clone.StrictAccountAffinity = false
	clone.ExcludedAccountIDs = appendResponseExcludedAccountID(req.ExcludedAccountIDs, req.PreferredAccountID)
	if req.RawBody != nil {
		clone.RawBody = removeRawJSONField(req.RawBody, "previous_response_id")
	}
	if req.Metadata != nil {
		clone.Metadata = cloneStringMap(req.Metadata)
	}
	if req.Headers != nil {
		clone.Headers = cloneStringMap(req.Headers)
	}
	return &clone
}

func responsesRequestWithStrictAccount(req *core.ResponsesRequest, accountID string) *core.ResponsesRequest {
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
	if req.Headers != nil {
		clone.Headers = cloneStringMap(req.Headers)
	}
	return &clone
}

func responsesRequestWithoutStrictAccount(req *core.ResponsesRequest) *core.ResponsesRequest {
	if req == nil || (!req.StrictAccountAffinity && strings.TrimSpace(req.PreferredAccountID) == "") {
		return req
	}
	clone := *req
	clone.PreferredAccountID = ""
	clone.StrictAccountAffinity = false
	if req.Metadata != nil {
		clone.Metadata = cloneStringMap(req.Metadata)
	}
	if req.Headers != nil {
		clone.Headers = cloneStringMap(req.Headers)
	}
	return &clone
}

func appendResponseExcludedAccountID(existing []string, accountID string) []string {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return append([]string(nil), existing...)
	}
	excluded := append([]string(nil), existing...)
	for _, id := range excluded {
		if strings.EqualFold(strings.TrimSpace(id), accountID) {
			return excluded
		}
	}
	return append(excluded, accountID)
}

func removeRawJSONField(raw json.RawMessage, key string) json.RawMessage {
	if len(raw) == 0 || strings.TrimSpace(key) == "" {
		return raw
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	delete(payload, key)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	return json.RawMessage(encoded)
}

func executionAttempts(err error) []core.AttemptRecord {
	var executionErr *failover.ExecutionError
	if !errors.As(err, &executionErr) || executionErr == nil || len(executionErr.Attempts) == 0 {
		return nil
	}
	return append([]core.AttemptRecord(nil), executionErr.Attempts...)
}

func mergeExecutionAttempts(initial []core.AttemptRecord, err error) error {
	if len(initial) == 0 || err == nil {
		return err
	}
	attempts := append([]core.AttemptRecord(nil), initial...)
	if retryAttempts := executionAttempts(err); len(retryAttempts) > 0 {
		attempts = append(attempts, retryAttempts...)
		return &failover.ExecutionError{Attempts: attempts}
	}
	return err
}

func (s *Service) responsesStreamUpstreamRequest(req *core.ResponsesRequest, forceSSE bool) *core.ResponsesRequest {
	if req == nil || req.Transport != core.ResponsesTransportWebSocket {
		return req
	}
	settings := s.currentSystemSettings()
	if !forceSSE && core.ResponsesWebSocketUpstreamEnabled(settings.Runtime) {
		return req
	}
	upstreamReq := *req
	upstreamReq.Transport = core.ResponsesTransportSSE
	upstreamReq.Stream = true
	return &upstreamReq
}

func responsesGenerateDisabled(req *core.ResponsesRequest) bool {
	if req == nil {
		return false
	}
	if req.Generate != nil {
		return !*req.Generate
	}
	if len(req.RawBody) == 0 {
		return false
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(req.RawBody, &payload); err != nil {
		return false
	}
	raw := payload["generate"]
	if len(raw) == 0 {
		return false
	}
	var generate bool
	if err := json.Unmarshal(raw, &generate); err != nil {
		return false
	}
	return !generate
}

func responsesRequestServiceTier(req *core.ResponsesRequest) string {
	if req == nil {
		return ""
	}
	if serviceTier := strings.TrimSpace(req.ServiceTier); serviceTier != "" {
		return serviceTier
	}
	if len(req.RawBody) == 0 {
		return ""
	}
	return rawJSONTextField(req.RawBody, "service_tier")
}

func estimateResponsesPromptTokens(req *core.ResponsesRequest) int {
	if req == nil {
		return 0
	}
	if len(req.RawBody) > 0 {
		return estimateTextTokens(string(req.RawBody))
	}
	return 0
}

func estimateResponsesCompletionTokens(req *core.ResponsesRequest) int {
	if req == nil {
		return 0
	}
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		return *req.MaxOutputTokens
	}
	return 1024
}

func rawJSONTextField(raw []byte, key string) string {
	if len(raw) == 0 || strings.TrimSpace(key) == "" {
		return ""
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	value := payload[key]
	if len(value) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return ""
	}
	return strings.TrimSpace(text)
}

func (s *Service) applyResponsesAccountBinding(req *core.ResponsesRequest, client *core.APIClient) error {
	if req == nil {
		return nil
	}
	if req.Compact {
		return nil
	}
	previousResponseID := strings.TrimSpace(req.PreviousResponseID)
	if previousResponseID == "" {
		previousResponseID = rawJSONTextField(req.RawBody, "previous_response_id")
	}
	if previousResponseID == "" {
		return nil
	}
	req.PreviousResponseID = previousResponseID
	binding, err := s.repo.GetOpenAIResponseBinding(previousResponseID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return &ResponsesBindingError{ResponseID: previousResponseID, Err: err}
		}
		return err
	}
	if strings.TrimSpace(binding.ClientID) != "" {
		if client == nil || !strings.EqualFold(strings.TrimSpace(client.ID), strings.TrimSpace(binding.ClientID)) {
			return &ResponsesBindingError{ResponseID: previousResponseID, Err: storage.ErrNotFound}
		}
	}
	req.PreferredAccountID = strings.TrimSpace(binding.AccountID)
	req.StrictAccountAffinity = req.PreferredAccountID != ""
	if strings.TrimSpace(req.PromptCacheKey) == "" && strings.TrimSpace(binding.PromptCacheKey) != "" {
		req.PromptCacheKey = strings.TrimSpace(binding.PromptCacheKey)
	}
	return nil
}

func (s *Service) rememberResponsesBinding(req *core.ResponsesRequest, resp *core.GatewayResponse, client *core.APIClient) {
	if s == nil || s.repo == nil || resp == nil {
		return
	}
	if !rememberableResponsesBindingResponse(resp) {
		return
	}
	responseID := strings.TrimSpace(resp.ID)
	accountID := strings.TrimSpace(resp.AccountID)
	if responseID == "" || accountID == "" {
		return
	}
	clientID := ""
	if client != nil {
		clientID = strings.TrimSpace(client.ID)
	}
	_ = s.repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{
		ResponseID:     responseID,
		AccountID:      accountID,
		ClientID:       clientID,
		PromptCacheKey: responsesPromptCacheKey(req),
	})
}

func responsesPromptCacheKey(req *core.ResponsesRequest) string {
	if req == nil {
		return ""
	}
	if value := strings.TrimSpace(req.PromptCacheKey); value != "" {
		return value
	}
	if req.Metadata != nil {
		if value := strings.TrimSpace(req.Metadata["prompt_cache_key"]); value != "" {
			return value
		}
		if value := strings.TrimSpace(req.Metadata["cache_affinity_key"]); value != "" {
			return value
		}
	}
	return ""
}

func shouldRememberResponsesBinding(req *core.ResponsesRequest, resp *core.GatewayResponse) bool {
	if req != nil && req.Compact {
		return false
	}
	return rememberableResponsesBindingResponse(resp)
}

func rememberableResponsesBindingResponse(resp *core.GatewayResponse) bool {
	if resp == nil || resp.Provider != core.ProviderOpenAI {
		return false
	}
	responseID := strings.TrimSpace(resp.ID)
	if responseID == "" || strings.HasPrefix(responseID, "resp_openai_") {
		return false
	}
	return true
}
