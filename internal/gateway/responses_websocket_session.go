package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/failover"
	"github.com/32ns/ai-gateway/internal/providers"
)

type ResponsesWebSocketSession struct {
	service    *Service
	baseClient *core.APIClient
	baseErr    error

	mu       sync.Mutex
	upstream providers.ResponsesWebSocketSession
	decision core.RouteDecision
	attempts []core.AttemptRecord
	stateSeq uint64
}

const maxResponsesWebSocketRetries = 32

func (s *Service) NewResponsesWebSocketSession(metadata map[string]string, requestClient *core.APIClient) *ResponsesWebSocketSession {
	client, _, err := s.clientForRequest(metadata, requestClient)
	return &ResponsesWebSocketSession{
		service:    s,
		baseClient: client,
		baseErr:    err,
	}
}

func (s *ResponsesWebSocketSession) Close() error {
	if s == nil {
		return nil
	}
	upstream := s.detachUpstream(0, false)
	if upstream == nil {
		return nil
	}
	return upstream.Close()
}

func (s *ResponsesWebSocketSession) SendRaw(ctx context.Context, payload []byte) error {
	if s == nil {
		return nil
	}
	upstream, _, _, _ := s.snapshotUpstream()
	if upstream == nil {
		return nil
	}
	return upstream.SendRaw(ctx, payload)
}

func (s *ResponsesWebSocketSession) Execute(ctx context.Context, req *core.ResponsesRequest, emit func(*core.GatewayResponse, *core.StreamEvent) error) error {
	if s == nil || s.service == nil {
		return fmt.Errorf("responses websocket session is not initialized")
	}
	service := s.service
	if req == nil {
		return fmt.Errorf("responses request is required")
	}
	if s.baseErr != nil {
		return s.baseErr
	}
	client, routePolicy, err := service.clientForRequest(req.Metadata, s.baseClient)
	if err != nil {
		return err
	}
	req.Client = client
	if !core.ResponsesWebSocketUpstreamEnabled(service.currentSystemSettings().Runtime) {
		s.resetUpstream()
		return service.executeResponsesStream(ctx, req, emit, true)
	}
	requestID := service.newRequestID()
	requestStarted := time.Now()
	requestedModel := strings.TrimSpace(req.Model)
	exposedModel := req.Model
	if err := service.applyResponsesAccountBinding(req, client); err != nil {
		return err
	}
	model, err := service.resolveManagedModel(exposedModel, client)
	if err != nil {
		return err
	}
	upstreamModel := upstreamModelID(model)
	routePolicy = routePolicyForModelProvider(routePolicy, model.Provider)
	plan := service.router.BuildResponsesPlan(req, routePolicy)
	plan.Model = upstreamModel
	if !service.failover.SupportsResponsesWebSocket(plan) {
		s.resetUpstream()
		return service.executeResponsesStream(ctx, req, emit, true)
	}

	release, err := service.reserveUserRequestSlot(ctx, client, req.ClientIP)
	if err != nil {
		return err
	}
	defer release()

	billing, err := service.reserveResponsesBilling(requestID, client, model, plan.Model, req)
	if err != nil {
		return err
	}

	started := time.Now()
	attemptCtx := billingAttemptContext(ctx, billing)
	allAttempts := []core.AttemptRecord(nil)
	var attempts []core.AttemptRecord
	var streamDecision core.RouteDecision
	var streamStateSeq uint64
	var finalResp core.GatewayResponse
	var streamErr error
	activeReq := req
	sameAccountStreamRetries := 0
	for streamRetries := 0; ; streamRetries++ {
		streamSession, openAttempts, decision, stateSeq, err := s.openOrSend(attemptCtx, plan, client, activeReq)
		attempts = appendStreamOpenAttempts(append([]core.AttemptRecord(nil), allAttempts...), openAttempts)
		streamDecision = decision
		streamStateSeq = stateSeq
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) &&
				shouldRetryResponsesWithoutPreviousResponse(activeReq, &failover.ExecutionError{Attempts: openAttempts}) {
				allAttempts = attempts
				retryReq := responsesRequestWithoutPreviousResponse(activeReq)
				activeReq = retryReq
				req = retryReq
				continue
			}
			service.releaseBilling(billing, err.Error())
			message := err.Error()
			var executionErr *failover.ExecutionError
			if errors.As(err, &executionErr) {
				attempts = appendStreamOpenAttempts(append([]core.AttemptRecord(nil), allAttempts...), executionErr.Attempts)
				message = (&failover.ExecutionError{Attempts: attempts}).Summary()
			}
			_ = service.repo.AppendAudit(core.AuditEvent{
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

		consume := consumeResponsesWebSocketStream(requestStarted, requestedModel, exposedModel, streamSession, emit)
		finalResp = consume.Response
		streamErr = consume.StreamErr
		if consume.UpstreamErr != nil {
			s.resetUpstreamIf(streamStateSeq)
			failureAttempt := streamFailureAttempt(streamDecision.Provider, streamDecision.Account.ID, streamDecision.Account.Label, consume.UpstreamErr)
			attempts = append(attempts, failureAttempt)
			if sameAccountStreamRetries < maxPreOutputSameAccountStreamRetries &&
				shouldRetrySameAccountStreamBeforeOutput(client, consume.UpstreamErr, consume.Committed) {
				allAttempts = attempts
				sameAccountStreamRetries++
				activeReq = responsesRequestWithStrictAccount(activeReq, streamDecision.Account.ID)
				continue
			}
			sameAccountStreamRetries = 0
			service.failover.RecordResponsesWebSocketFailure(streamDecision, consume.UpstreamErr)
			recordBillingStreamFailure(billing, failureAttempt)
			if !consume.Committed && shouldRetryResponsesWithoutPreviousResponseAfterPreOutputFailure(activeReq, streamDecision.Account, failureAttempt, consume.UpstreamErr) {
				allAttempts = attempts
				retryReq := responsesRequestWithoutPreviousResponse(activeReq)
				activeReq = retryReq
				sameAccountStreamRetries = 0
				continue
			}
			if shouldRetryStreamBeforeOutput(consume.UpstreamErr, consume.Committed, streamRetries) {
				allAttempts = attempts
				sameAccountStreamRetries = 0
				activeReq = responsesRequestExcludingAccount(responsesRequestWithoutStrictAccount(activeReq), failureAttempt.AccountID)
				continue
			}
		}
		break
	}
	service.refreshAccountQuotaAfterUse(finalResp.AccountID)

	if streamErr != nil {
		service.releaseBilling(billing, streamErr.Error())
		_ = service.repo.AppendAudit(core.AuditEvent{
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
			Attempts:   attempts,
			DurationMS: time.Since(started).Milliseconds(),
			CreatedAt:  time.Now().UTC(),
		})
		return streamErr
	}

	if err := service.settleGatewayBilling(billing, &finalResp); err != nil {
		return err
	}
	if shouldRememberResponsesBinding(activeReq, &finalResp) {
		service.rememberResponsesBinding(activeReq, &finalResp, client)
	}

	_ = service.repo.AppendAudit(core.AuditEvent{
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
		Attempts:   attempts,
		DurationMS: time.Since(started).Milliseconds(),
		CreatedAt:  time.Now().UTC(),
	})

	return nil
}

type responsesWebSocketStreamConsumeResult struct {
	Response     core.GatewayResponse
	Committed    bool
	TerminalSeen bool
	StreamErr    error
	UpstreamErr  error
}

func consumeResponsesWebSocketStream(requestStarted time.Time, requestedModel, exposedModel string, session *providers.StreamSession, emit func(*core.GatewayResponse, *core.StreamEvent) error) responsesWebSocketStreamConsumeResult {
	consume := consumeResponsesStream(requestStarted, requestedModel, exposedModel, session, emit)
	if consume.StreamErr == nil && !consume.TerminalSeen {
		err := &providers.InvokeError{
			Code:      providers.ErrorCodeUpstreamReadError,
			Temporary: true,
			Cooldown:  20 * time.Second,
			Err:       fmt.Errorf("responses websocket stream closed before terminal event"),
		}
		consume.StreamErr = err
		consume.UpstreamErr = err
	}
	return responsesWebSocketStreamConsumeResult{
		Response:     consume.Response,
		Committed:    consume.Committed,
		TerminalSeen: consume.TerminalSeen,
		StreamErr:    consume.StreamErr,
		UpstreamErr:  consume.UpstreamErr,
	}
}

func (s *ResponsesWebSocketSession) openOrSend(ctx context.Context, plan core.RoutePlan, client *core.APIClient, req *core.ResponsesRequest) (*providers.StreamSession, []core.AttemptRecord, core.RouteDecision, uint64, error) {
	return s.openOrSendWithRetryBudget(ctx, plan, client, req, maxResponsesWebSocketRetries)
}

func (s *ResponsesWebSocketSession) openOrSendWithRetryBudget(ctx context.Context, plan core.RoutePlan, client *core.APIClient, req *core.ResponsesRequest, retriesLeft int) (*providers.StreamSession, []core.AttemptRecord, core.RouteDecision, uint64, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, nil, core.RouteDecision{}, 0, err
		}
	}
	upstream, decision, cachedAttempts, stateSeq := s.snapshotUpstream()
	if upstream == nil {
		result, err := s.service.failover.OpenResponsesWebSocketSession(ctx, plan, client, req)
		if err != nil {
			return nil, nil, core.RouteDecision{}, 0, err
		}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				_ = result.Session.Close()
				return nil, result.Attempts, result.Decision, 0, err
			}
		}
		storedSeq, stored := s.storeUpstreamIfUnchanged(stateSeq, result.Session, result.Decision, result.Attempts)
		if !stored {
			_ = result.Session.Close()
			return nil, result.Attempts, result.Decision, 0, io.ErrClosedPipe
		}
		streamSession, err := result.Session.SendRequest(ctx, result.Decision.Model, req)
		if err != nil {
			err = normalizeResponsesWebSocketSendError(err)
			s.service.failover.RecordResponsesWebSocketFailure(result.Decision, err)
			attempt := core.AttemptRecord{
				Provider:     result.Decision.Provider,
				AccountID:    result.Decision.Account.ID,
				AccountLabel: result.Decision.Account.Label,
				Status:       "invoke_error",
				ErrorMessage: err.Error(),
				Temporary:    providers.IsTemporary(err),
			}
			if code := providers.ErrorCode(err); code != "" {
				attempt.ErrorCode = code
			}
			failover.NotifyAttemptFinished(ctx, attempt)
			attempts := append([]core.AttemptRecord(nil), result.Attempts...)
			attempts = append(attempts, attempt)
			s.resetUpstreamIf(storedSeq)
			if shouldRetryResponsesWithoutPreviousResponseAfterPreOutputFailure(req, result.Decision.Account, attempt, err) {
				retryReq := responsesRequestWithoutPreviousResponse(req)
				retrySession, retryAttempts, retryDecision, retryStateSeq, retryErr := s.openOrSendWithRetryBudget(ctx, plan, client, retryReq, retriesLeft-1)
				if retryErr == nil {
					return retrySession, append(attempts, retryAttempts...), retryDecision, retryStateSeq, nil
				}
				return nil, append(attempts, retryAttempts...), retryDecision, retryStateSeq, retryErr
			}
			if shouldRetryResponsesWebSocketSend(err) && retriesLeft > 0 {
				retryReq := responsesWebSocketSendRetryRequest(req, result.Decision.Account.ID)
				retrySession, retryAttempts, retryDecision, retryStateSeq, retryErr := s.openOrSendWithRetryBudget(ctx, plan, client, retryReq, retriesLeft-1)
				if retryErr == nil {
					return retrySession, append(attempts, retryAttempts...), retryDecision, retryStateSeq, nil
				}
				return nil, append(attempts, retryAttempts...), retryDecision, retryStateSeq, retryErr
			}
			return nil, attempts, result.Decision, storedSeq, err
		}
		return streamSession, result.Attempts, result.Decision, storedSeq, nil
	}

	if strings.TrimSpace(decision.Model) != strings.TrimSpace(plan.Model) {
		s.resetUpstreamIf(stateSeq)
		return s.openOrSendWithRetryBudget(ctx, plan, client, req, retriesLeft)
	}
	if req.StrictAccountAffinity && strings.TrimSpace(req.PreferredAccountID) != "" &&
		!strings.EqualFold(strings.TrimSpace(req.PreferredAccountID), strings.TrimSpace(decision.Account.ID)) {
		s.resetUpstreamIf(stateSeq)
		return s.openOrSendWithRetryBudget(ctx, plan, client, req, retriesLeft)
	}

	running := core.AttemptRecord{
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Status:       "running",
	}
	failover.NotifyAttemptStarted(ctx, running)
	streamSession, err := upstream.SendRequest(ctx, decision.Model, req)
	if err != nil {
		err = normalizeResponsesWebSocketSendError(err)
		s.service.failover.RecordResponsesWebSocketFailure(decision, err)
		attempt := core.AttemptRecord{
			Provider:     decision.Provider,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			Status:       "invoke_error",
			ErrorMessage: err.Error(),
			Temporary:    providers.IsTemporary(err),
		}
		if code := providers.ErrorCode(err); code != "" {
			attempt.ErrorCode = code
		}
		failover.NotifyAttemptFinished(ctx, attempt)
		attempts := append([]core.AttemptRecord(nil), cachedAttempts...)
		attempts = append(attempts, attempt)
		s.resetUpstreamIf(stateSeq)
		if shouldRetryResponsesWithoutPreviousResponseAfterPreOutputFailure(req, decision.Account, attempt, err) {
			retryReq := responsesRequestWithoutPreviousResponse(req)
			retrySession, retryAttempts, retryDecision, retryStateSeq, retryErr := s.openOrSendWithRetryBudget(ctx, plan, client, retryReq, retriesLeft-1)
			if retryErr == nil {
				return retrySession, append(attempts, retryAttempts...), retryDecision, retryStateSeq, nil
			}
			return nil, append(attempts, retryAttempts...), retryDecision, retryStateSeq, retryErr
		}
		if shouldRetryResponsesWebSocketSend(err) && retriesLeft > 0 {
			retryReq := responsesWebSocketSendRetryRequest(req, decision.Account.ID)
			retrySession, retryAttempts, retryDecision, retryStateSeq, retryErr := s.openOrSendWithRetryBudget(ctx, plan, client, retryReq, retriesLeft-1)
			if retryErr == nil {
				return retrySession, append(attempts, retryAttempts...), retryDecision, retryStateSeq, nil
			}
			return nil, append(attempts, retryAttempts...), retryDecision, retryStateSeq, retryErr
		}
		return nil, attempts, decision, stateSeq, err
	}
	attempts := []core.AttemptRecord{{
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Status:       "ok",
	}}
	failover.NotifyAttemptFinished(ctx, attempts[0])
	return streamSession, attempts, decision, stateSeq, nil
}

func normalizeResponsesWebSocketSendError(err error) error {
	if err == nil {
		return nil
	}
	if providers.ErrorCode(err) != "" {
		return err
	}
	if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, io.EOF) {
		return &providers.InvokeError{
			Code:      providers.ErrorCodeUpstreamReadError,
			Temporary: true,
			Cooldown:  20 * time.Second,
			Err:       fmt.Errorf("responses websocket send failed: %w", err),
		}
	}
	return err
}

func responsesWebSocketSendRetryRequest(req *core.ResponsesRequest, accountID string) *core.ResponsesRequest {
	if req != nil && req.StrictAccountAffinity &&
		strings.EqualFold(strings.TrimSpace(req.PreferredAccountID), strings.TrimSpace(accountID)) {
		return req
	}
	return responsesRequestExcludingAccount(req, accountID)
}

func shouldRetryResponsesWebSocketSend(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, io.EOF) {
		return true
	}
	return providers.FailurePolicyFor(core.Account{}, err).ResetWebSocket
}

func (s *ResponsesWebSocketSession) resetUpstream() {
	if s == nil {
		return
	}
	if upstream := s.detachUpstream(0, false); upstream != nil {
		_ = upstream.Close()
	}
}

func (s *ResponsesWebSocketSession) resetUpstreamIf(stateSeq uint64) {
	if s == nil || stateSeq == 0 {
		return
	}
	if upstream := s.detachUpstream(stateSeq, true); upstream != nil {
		_ = upstream.Close()
	}
}

func (s *ResponsesWebSocketSession) snapshotUpstream() (providers.ResponsesWebSocketSession, core.RouteDecision, []core.AttemptRecord, uint64) {
	if s == nil {
		return nil, core.RouteDecision{}, nil, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upstream, s.decision, append([]core.AttemptRecord(nil), s.attempts...), s.stateSeq
}

func (s *ResponsesWebSocketSession) storeUpstreamIfUnchanged(stateSeq uint64, upstream providers.ResponsesWebSocketSession, decision core.RouteDecision, attempts []core.AttemptRecord) (uint64, bool) {
	if s == nil || upstream == nil {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stateSeq != stateSeq {
		return 0, false
	}
	s.upstream = upstream
	s.decision = decision
	s.attempts = append([]core.AttemptRecord(nil), attempts...)
	s.stateSeq++
	return s.stateSeq, true
}

func (s *ResponsesWebSocketSession) detachUpstream(stateSeq uint64, requireMatch bool) providers.ResponsesWebSocketSession {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if requireMatch && s.stateSeq != stateSeq {
		return nil
	}
	upstream := s.upstream
	s.upstream = nil
	s.decision = core.RouteDecision{}
	s.attempts = nil
	s.stateSeq++
	return upstream
}
