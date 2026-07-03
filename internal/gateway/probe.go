package gateway

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/failover"
	"github.com/32ns/ai-gateway/internal/providers"
)

type MonitorProbeInput struct {
	TargetID     string
	AccountGroup string
	Model        string
	Prompt       string
	Timeout      time.Duration
}

type MonitorProbeResult struct {
	Status       core.MonitorStatus
	LatencyMS    int64
	Provider     core.ProviderKind
	AccountID    string
	AccountLabel string
	Attempts     []core.AttemptRecord
	ErrorCode    string
	ErrorMessage string
	CheckedAt    time.Time
}

func (s *Service) ProbeMonitorTarget(ctx context.Context, input MonitorProbeInput) (MonitorProbeResult, error) {
	started := time.Now()
	checkedAt := started.UTC()
	if ctx == nil {
		ctx = context.Background()
	}
	if input.Timeout > 0 {
		ctx = failover.WithAttemptTimeout(ctx, input.Timeout)
	}
	result := MonitorProbeResult{Status: core.MonitorStatusFailed, CheckedAt: checkedAt}
	if errors.Is(ctx.Err(), context.Canceled) {
		result.ErrorCode = "probe_canceled"
		result.ErrorMessage = ctx.Err().Error()
		return result, ctx.Err()
	}
	if s == nil {
		result.ErrorCode = "gateway_unavailable"
		result.ErrorMessage = "gateway service is unavailable"
		return result, errors.New(result.ErrorMessage)
	}
	modelID := strings.TrimSpace(input.Model)
	if modelID == "" {
		result.ErrorCode = "model_required"
		result.ErrorMessage = "model is required"
		return result, errors.New(result.ErrorMessage)
	}
	group := core.NormalizeAccountGroupName(input.AccountGroup)
	client := &core.APIClient{
		ID:           monitorClientID(input.TargetID),
		Name:         "Status Monitor",
		Enabled:      true,
		AccountGroup: group,
		RoutePolicy:  core.DefaultRoutePolicy(),
	}
	prompt := strings.TrimSpace(input.Prompt)
	if prompt == "" {
		prompt = "Reply with pong."
	}
	maxTokens := 8
	temperature := 0.0
	req := &core.GatewayRequest{
		Model: modelID,
		Messages: []core.Message{
			{Role: "system", Content: "You are a health check endpoint. Reply briefly."},
			{Role: "user", Content: prompt},
		},
		MaxTokens:   &maxTokens,
		Temperature: &temperature,
		Client:      client,
		Metadata: map[string]string{
			"client_id": client.ID,
			"purpose":   "status_monitor",
		},
	}
	_, routePolicy, err := s.clientForRequest(req.Metadata, req.Client)
	if err != nil {
		result.ErrorCode = "client_route_error"
		result.ErrorMessage = err.Error()
		return result, err
	}
	model, err := s.resolveManagedModel(modelID, client)
	if err != nil {
		result.ErrorCode = "model_unavailable"
		result.ErrorMessage = err.Error()
		return result, err
	}
	if core.NormalizeModelType(model.Type, model.ID) != core.ModelTypeText {
		result.ErrorCode = "model_not_text"
		result.ErrorMessage = "status monitor requires a text model"
		return result, &ModelUnavailableError{Model: modelID}
	}
	routePolicy = routePolicyForModelProvider(routePolicy, model.Provider)
	plan := s.router.BuildPlan(req, routePolicy)
	plan.Model = upstreamModelID(model)

	invocation, err := s.failover.Execute(ctx, plan, client, req)
	result.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		result.ErrorCode = "probe_failed"
		result.ErrorMessage = err.Error()
		var executionErr *failover.ExecutionError
		if errors.As(err, &executionErr) {
			result.Attempts = append([]core.AttemptRecord(nil), executionErr.Attempts...)
			result.ErrorMessage = executionErr.Summary()
			if last := lastFailedAttempt(executionErr.Attempts); last != nil {
				result.Provider = last.Provider
				result.AccountID = last.AccountID
				result.AccountLabel = last.AccountLabel
				if strings.TrimSpace(last.ErrorCode) != "" {
					result.ErrorCode = last.ErrorCode
				}
				if strings.TrimSpace(last.ErrorMessage) != "" {
					result.ErrorMessage = last.ErrorMessage
				}
				normalizeMonitorProbeError(&result)
			}
		}
		if ctxErr := ctx.Err(); errors.Is(ctxErr, context.Canceled) || errors.Is(ctxErr, context.DeadlineExceeded) {
			return result, ctxErr
		}
		return result, err
	}
	if invocation == nil || invocation.Response == nil {
		result.ErrorCode = "empty_response"
		result.ErrorMessage = "upstream returned no response"
		return result, errors.New(result.ErrorMessage)
	}
	resp := invocation.Response
	result.Provider = resp.Provider
	result.AccountID = resp.AccountID
	result.AccountLabel = resp.AccountLabel
	result.Attempts = append([]core.AttemptRecord(nil), invocation.Attempts...)
	if strings.TrimSpace(resp.Content) == "" {
		result.ErrorCode = "empty_response"
		result.ErrorMessage = "upstream returned an empty response"
		return result, errors.New(result.ErrorMessage)
	}
	result.Status = core.MonitorStatusOK
	if s.monitorSuccessfulAccountIsBackup(resp.AccountID) {
		result.Status = core.MonitorStatusDegraded
	}
	return result, nil
}

func normalizeMonitorProbeError(result *MonitorProbeResult) {
	if result == nil {
		return
	}
	if result.ErrorCode == providers.ErrorCodeUpstreamEmptyResponse {
		result.ErrorCode = "empty_response"
		if strings.TrimSpace(result.ErrorMessage) == "" || strings.Contains(result.ErrorMessage, providers.ErrorCodeUpstreamEmptyResponse) {
			result.ErrorMessage = "upstream returned an empty response"
		}
	}
}

func monitorClientID(targetID string) string {
	targetID = strings.TrimSpace(targetID)
	if targetID == "" {
		return "monitor"
	}
	return "monitor:" + targetID
}

func (s *Service) monitorSuccessfulAccountIsBackup(accountID string) bool {
	accountID = strings.TrimSpace(accountID)
	if s == nil || s.repo == nil || accountID == "" {
		return false
	}
	account, err := s.repo.GetAccount(accountID)
	return err == nil && account.Backup
}

func lastFailedAttempt(attempts []core.AttemptRecord) *core.AttemptRecord {
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].Status != "" && attempts[i].Status != "ok" && attempts[i].Status != "running" {
			return &attempts[i]
		}
	}
	if len(attempts) == 0 {
		return nil
	}
	return &attempts[len(attempts)-1]
}
