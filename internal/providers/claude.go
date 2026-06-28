package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

const claudeBaseURL = "https://api.anthropic.com"

type ClaudeAdapter struct{}

type anthropicMessagesRequest struct {
	Model         string                 `json:"model"`
	Messages      []anthropicMessageItem `json:"messages"`
	System        string                 `json:"system,omitempty"`
	Stream        bool                   `json:"stream"`
	MaxTokens     int                    `json:"max_tokens"`
	Temperature   *float64               `json:"temperature,omitempty"`
	TopP          *float64               `json:"top_p,omitempty"`
	StopSequences []string               `json:"stop_sequences,omitempty"`
	Metadata      map[string]string      `json:"metadata,omitempty"`
}

type anthropicMessageItem struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicMessagesResponse struct {
	ID         string                  `json:"id"`
	Model      string                  `json:"model"`
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      anthropicUsagePayload   `json:"usage"`
}

type anthropicUsagePayload struct {
	InputTokens              int                    `json:"input_tokens"`
	OutputTokens             int                    `json:"output_tokens"`
	CacheReadInputTokens     int                    `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int                    `json:"cache_creation_input_tokens"`
	CacheCreation            anthropicCacheCreation `json:"cache_creation"`
}

type anthropicCacheCreation struct {
	Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
}

type anthropicStreamMessageStart struct {
	Message struct {
		ID    string                `json:"id"`
		Model string                `json:"model"`
		Usage anthropicUsagePayload `json:"usage"`
	} `json:"message"`
}

type anthropicStreamContentBlockStart struct {
	ContentBlock anthropicContentBlock `json:"content_block"`
}

type anthropicStreamContentDelta struct {
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
}

type anthropicStreamMessageDelta struct {
	Delta struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage anthropicUsagePayload `json:"usage"`
}

type anthropicStreamErrorPayload struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
}

func usageFromAnthropicPayload(payload anthropicUsagePayload) core.Usage {
	cacheCreationTokens := payload.CacheCreationInputTokens
	if cacheCreationTokens <= 0 {
		cacheCreationTokens = payload.CacheCreation.Ephemeral5mInputTokens + payload.CacheCreation.Ephemeral1hInputTokens
	}
	promptTokens := payload.InputTokens + payload.CacheReadInputTokens
	total := promptTokens + cacheCreationTokens + payload.OutputTokens
	return core.Usage{
		PromptTokens:          promptTokens,
		CachedPromptTokens:    payload.CacheReadInputTokens,
		CacheCreationTokens:   cacheCreationTokens,
		CacheCreation5mTokens: payload.CacheCreation.Ephemeral5mInputTokens,
		CacheCreation1hTokens: payload.CacheCreation.Ephemeral1hInputTokens,
		CompletionTokens:      payload.OutputTokens,
		TotalTokens:           total,
	}
}

func anthropicUsageEmpty(payload anthropicUsagePayload) bool {
	return payload.InputTokens == 0 &&
		payload.OutputTokens == 0 &&
		payload.CacheReadInputTokens == 0 &&
		payload.CacheCreationInputTokens == 0 &&
		payload.CacheCreation.Ephemeral5mInputTokens == 0 &&
		payload.CacheCreation.Ephemeral1hInputTokens == 0
}

func anthropicContentBlockStartsOutput(block anthropicContentBlock) bool {
	switch strings.TrimSpace(block.Type) {
	case "tool_use", "server_tool_use", "thinking", "redacted_thinking":
		return true
	default:
		return false
	}
}

func anthropicDeltaStartsOutput(deltaType, partialJSON string) bool {
	switch strings.TrimSpace(deltaType) {
	case "input_json_delta":
		return partialJSON != ""
	case "thinking_delta", "signature_delta":
		return true
	default:
		return false
	}
}

func mergeUsage(dst *core.Usage, src core.Usage) {
	if dst == nil {
		return
	}
	if src.PromptTokens > 0 {
		dst.PromptTokens = src.PromptTokens
	}
	if src.CachedPromptTokens > 0 {
		dst.CachedPromptTokens = src.CachedPromptTokens
	}
	if src.CacheCreationTokens > 0 {
		dst.CacheCreationTokens = src.CacheCreationTokens
	}
	if src.CacheCreation5mTokens > 0 {
		dst.CacheCreation5mTokens = src.CacheCreation5mTokens
	}
	if src.CacheCreation1hTokens > 0 {
		dst.CacheCreation1hTokens = src.CacheCreation1hTokens
	}
	if src.CompletionTokens > 0 {
		dst.CompletionTokens = src.CompletionTokens
	}
	if src.ImageOutputTokens > 0 {
		dst.ImageOutputTokens = src.ImageOutputTokens
	}
	dst.TotalTokens = dst.PromptTokens + dst.CacheCreationTokens + dst.CompletionTokens
	if src.TotalTokens > dst.TotalTokens {
		dst.TotalTokens = src.TotalTokens
	}
}

func (a *ClaudeAdapter) Kind() core.ProviderKind {
	return core.ProviderClaude
}

func (a *ClaudeAdapter) DisplayName() string {
	return "Claude"
}

func (a *ClaudeAdapter) ListModels(context.Context) []core.ModelSpec {
	return nil
}

type anthropicModelsResponse struct {
	Data []anthropicModelObject `json:"data"`
}

type anthropicModelObject struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

func (a *ClaudeAdapter) FetchModels(ctx context.Context, account core.Account) ([]UpstreamModel, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, account.EffectiveProxyURL), account)
	accessToken, err := accessTokenForAccount(account)
	if err != nil {
		return nil, err
	}

	endpoint := resolveEndpoint(account, claudeBaseURL, "/v1/models")
	req, err := newJSONRequest(ctx, "GET", endpoint, "", nil, claudeHeaders(account, accessToken))
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}

	var upstream anthropicModelsResponse
	if _, err := doDecodeJSON(req, &upstream); err != nil {
		return nil, err
	}

	models := make([]UpstreamModel, 0, len(upstream.Data))
	for _, item := range upstream.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		models = append(models, UpstreamModel{
			ID:          id,
			DisplayName: strings.TrimSpace(item.DisplayName),
			OwnedBy:     string(core.ProviderClaude),
		})
	}
	return models, nil
}

func (a *ClaudeAdapter) Invoke(ctx context.Context, decision core.RouteDecision, req *core.GatewayRequest) (*core.GatewayResponse, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, decision.Account.EffectiveProxyURL), decision.Account)
	accessToken, err := accessTokenForAccount(decision.Account)
	if err != nil {
		return nil, err
	}

	endpoint := resolveEndpoint(decision.Account, claudeBaseURL, "/v1/messages")
	payload, err := anthropicMessagesPayload(decision.Model, req, false)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}

	headers := claudeRequestHeaders(decision.Account, accessToken, req)
	httpReq, err := newJSONRequest(ctx, "POST", endpoint, "", payload, headers)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}

	var upstream anthropicMessagesResponse
	var rawBody []byte
	if req != nil && req.UpstreamMode == "anthropic_messages" {
		_, rawBody, err = doJSON(httpReq, &upstream)
		if err != nil {
			return nil, err
		}
	} else {
		if _, err := doDecodeJSON(httpReq, &upstream); err != nil {
			return nil, err
		}
	}

	resp := &core.GatewayResponse{
		ID:           upstream.ID,
		Model:        firstNonEmpty(upstream.Model, decision.Model),
		Provider:     core.ProviderClaude,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Content:      flattenAnthropicBlocks(upstream.Content),
		FinishReason: firstNonEmpty(upstream.StopReason, "end_turn"),
		CreatedAt:    time.Now().UTC(),
		Usage:        usageFromAnthropicPayload(upstream.Usage),
	}
	if req != nil && req.UpstreamMode == "anthropic_messages" {
		resp.RawBody = rawBody
	}
	return resp, nil
}

func (a *ClaudeAdapter) OpenStream(ctx context.Context, decision core.RouteDecision, req *core.GatewayRequest) (*StreamSession, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, decision.Account.EffectiveProxyURL), decision.Account)
	accessToken, err := accessTokenForAccount(decision.Account)
	if err != nil {
		return nil, err
	}

	meta := &core.GatewayResponse{
		ID:           fmt.Sprintf("resp_claude_stream_%d", time.Now().UnixNano()),
		Model:        decision.Model,
		Provider:     core.ProviderClaude,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		CreatedAt:    time.Now().UTC(),
	}

	endpoint := resolveEndpoint(decision.Account, claudeBaseURL, "/v1/messages")
	payload, err := anthropicMessagesPayload(decision.Model, req, true)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}

	headers := claudeRequestHeaders(decision.Account, accessToken, req)
	httpReq, err := newJSONRequest(ctx, "POST", endpoint, "", payload, headers)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}

	httpResp, err := doStream(httpReq)
	if err != nil {
		return nil, err
	}

	return &StreamSession{
		Response: meta,
		Stream: &claudeStream{
			body:      httpResp.Body,
			reader:    newSSEReader(httpResp.Body),
			response:  meta,
			rawEvents: req != nil && req.UpstreamMode == "anthropic_messages",
		},
	}, nil
}

func (a *ClaudeAdapter) CountTokens(ctx context.Context, decision core.RouteDecision, req *core.TokenCountRequest) (*core.TokenCountResponse, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, decision.Account.EffectiveProxyURL), decision.Account)
	accessToken, err := accessTokenForAccount(decision.Account)
	if err != nil {
		return nil, err
	}

	payload, err := anthropicTokenCountPayload(decision.Model, req)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}
	endpoint := resolveEndpoint(decision.Account, claudeBaseURL, "/v1/messages/count_tokens")
	httpReq, err := newJSONRequest(ctx, "POST", endpoint, "", payload, claudeTokenCountHeaders(decision.Account, accessToken, req))
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}

	_, rawBody, err := doJSON(httpReq, nil)
	if err != nil {
		return nil, err
	}
	return &core.TokenCountResponse{
		Model:        decision.Model,
		Provider:     core.ProviderClaude,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Body:         rawBody,
	}, nil
}

func anthropicMessagesPayload(model string, req *core.GatewayRequest, stream bool) (any, error) {
	if req != nil && req.UpstreamMode == "anthropic_messages" && len(req.RawBody) > 0 {
		return anthropicRawMessagesPayload(model, req, stream)
	}
	return anthropicNormalizedMessagesPayload(model, req, stream), nil
}

func anthropicTokenCountPayload(model string, req *core.TokenCountRequest) (map[string]any, error) {
	payload := map[string]any{}
	if req != nil && len(req.RawBody) > 0 {
		if err := json.Unmarshal(req.RawBody, &payload); err != nil {
			return nil, err
		}
	}
	payload["model"] = strings.TrimSpace(model)
	return payload, nil
}

func claudeTokenCountHeaders(account core.Account, token string, req *core.TokenCountRequest) map[string]string {
	headers := claudeHeaders(account, token)
	if req == nil || len(req.Metadata) == 0 {
		return headers
	}
	applyClaudeProtocolHeaders(headers, req.Metadata)
	return headers
}

func claudeRequestHeaders(account core.Account, token string, req *core.GatewayRequest) map[string]string {
	headers := claudeHeaders(account, token)
	if req == nil || len(req.Metadata) == 0 {
		return headers
	}
	applyClaudeProtocolHeaders(headers, req.Metadata)
	return headers
}

func applyClaudeProtocolHeaders(headers map[string]string, metadata map[string]string) {
	if len(metadata) == 0 {
		return
	}
	if version := strings.TrimSpace(metadata["anthropic_version"]); version != "" {
		headers["anthropic-version"] = version
	}
	if beta := strings.TrimSpace(metadata["anthropic_beta"]); beta != "" {
		headers["anthropic-beta"] = beta
	}
	if requestID := strings.TrimSpace(metadata["x-client-request-id"]); requestID != "" {
		headers["x-client-request-id"] = requestID
	}
	for key, value := range metadata {
		normalized := strings.ToLower(strings.TrimSpace(key))
		if !strings.HasPrefix(normalized, "x-claude-code-") {
			continue
		}
		if value = strings.TrimSpace(value); value != "" {
			headers[normalized] = value
		}
	}
}

func anthropicRawMessagesPayload(model string, req *core.GatewayRequest, stream bool) (map[string]any, error) {
	payload := map[string]any{}
	if err := json.Unmarshal(req.RawBody, &payload); err != nil {
		return nil, err
	}
	payload["model"] = strings.TrimSpace(model)
	payload["stream"] = stream
	if !positiveJSONNumber(payload["max_tokens"]) {
		payload["max_tokens"] = firstPositiveInt(req.MaxTokens, 1024)
	}
	return payload, nil
}

func positiveJSONNumber(value any) bool {
	switch typed := value.(type) {
	case float64:
		return typed > 0
	case int:
		return typed > 0
	case int64:
		return typed > 0
	case json.Number:
		parsed, err := typed.Int64()
		return err == nil && parsed > 0
	default:
		return false
	}
}

func anthropicNormalizedMessagesPayload(model string, req *core.GatewayRequest, stream bool) anthropicMessagesRequest {
	if req == nil {
		return anthropicMessagesRequest{
			Model:     model,
			Stream:    stream,
			MaxTokens: 4096,
		}
	}
	payload := anthropicMessagesRequest{
		Model:         model,
		Messages:      make([]anthropicMessageItem, 0, len(req.Messages)),
		Stream:        stream,
		MaxTokens:     firstPositiveInt(req.MaxTokens, 4096),
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: req.Stop,
		Metadata:      req.Metadata,
	}
	for _, message := range req.Messages {
		role := anthropicRole(message.Role)
		if role == "system" {
			if payload.System == "" {
				payload.System = message.Content
			} else {
				payload.System += "\n" + message.Content
			}
			continue
		}
		payload.Messages = append(payload.Messages, anthropicMessageItem{
			Role: role,
			Content: []anthropicContentBlock{
				{Type: "text", Text: message.Content},
			},
		})
	}
	return payload
}

func flattenAnthropicBlocks(blocks []anthropicContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func anthropicRole(role string) string {
	switch strings.TrimSpace(role) {
	case "assistant":
		return "assistant"
	case "system", "developer":
		return "system"
	default:
		return "user"
	}
}

func firstPositiveInt(value *int, fallback int) int {
	if value != nil && *value > 0 {
		return *value
	}
	return fallback
}

type claudeStream struct {
	body      io.Closer
	reader    *sseReader
	response  *core.GatewayResponse
	rawEvents bool
}

func (s *claudeStream) Next() (*core.StreamEvent, error) {
	for {
		event, err := s.reader.Next()
		if err != nil {
			return nil, err
		}
		if event == nil {
			continue
		}

		switch event.Event {
		case "error":
			return nil, anthropicStreamError(event.Data)
		case "message_start":
			var payload anthropicStreamMessageStart
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return nil, &InvokeError{
					Code:      ErrorCodeUpstreamInvalidStreamChunk,
					Temporary: true,
					Cooldown:  10 * time.Second,
					Err:       err,
				}
			}
			if payload.Message.ID != "" {
				s.response.ID = payload.Message.ID
			}
			if payload.Message.Model != "" {
				s.response.Model = payload.Message.Model
			}
			if !anthropicUsageEmpty(payload.Message.Usage) {
				mergeUsage(&s.response.Usage, usageFromAnthropicPayload(payload.Message.Usage))
			}
			if s.rawEvents {
				return &core.StreamEvent{RawEvent: event.Event, RawData: event.Data}, nil
			}
		case "content_block_start":
			if s.rawEvents {
				firstOutput := false
				var payload anthropicStreamContentBlockStart
				if err := json.Unmarshal(event.Data, &payload); err == nil {
					firstOutput = anthropicContentBlockStartsOutput(payload.ContentBlock)
				}
				return &core.StreamEvent{FirstOutput: firstOutput, RawEvent: event.Event, RawData: event.Data}, nil
			}
		case "content_block_delta":
			var payload anthropicStreamContentDelta
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return nil, &InvokeError{
					Code:      ErrorCodeUpstreamInvalidStreamChunk,
					Temporary: true,
					Cooldown:  10 * time.Second,
					Err:       err,
				}
			}
			if payload.Delta.Type == "text_delta" && payload.Delta.Text != "" {
				streamEvent := &core.StreamEvent{Delta: payload.Delta.Text}
				if s.rawEvents {
					streamEvent.RawEvent = event.Event
					streamEvent.RawData = event.Data
				}
				return streamEvent, nil
			}
			if s.rawEvents {
				return &core.StreamEvent{FirstOutput: anthropicDeltaStartsOutput(payload.Delta.Type, payload.Delta.PartialJSON), RawEvent: event.Event, RawData: event.Data}, nil
			}
		case "message_delta":
			var payload anthropicStreamMessageDelta
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return nil, &InvokeError{
					Code:      ErrorCodeUpstreamInvalidStreamChunk,
					Temporary: true,
					Cooldown:  10 * time.Second,
					Err:       err,
				}
			}
			if payload.Delta.StopReason == "" && anthropicUsageEmpty(payload.Usage) {
				continue
			}
			if !anthropicUsageEmpty(payload.Usage) {
				mergeUsage(&s.response.Usage, usageFromAnthropicPayload(payload.Usage))
			}
			streamEvent := &core.StreamEvent{
				FinishReason: payload.Delta.StopReason,
				Usage:        &s.response.Usage,
				Done:         payload.Delta.StopReason != "",
			}
			if s.rawEvents {
				streamEvent.RawEvent = event.Event
				streamEvent.RawData = event.Data
			}
			return streamEvent, nil
		case "message_stop":
			if s.rawEvents {
				return &core.StreamEvent{RawEvent: event.Event, RawData: event.Data, Done: true}, nil
			}
			return nil, io.EOF
		default:
			if s.rawEvents {
				return &core.StreamEvent{RawEvent: event.Event, RawData: event.Data}, nil
			}
		}
	}
}

func (s *claudeStream) Close() error {
	return closeWithError(s.body, nil)
}

func anthropicStreamError(data []byte) error {
	var payload anthropicStreamErrorPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return &InvokeError{
			Code:      ErrorCodeUpstreamInvalidStreamChunk,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}
	message := strings.TrimSpace(payload.Error.Message)
	if message == "" {
		message = strings.TrimSpace(payload.Message)
	}
	if message == "" {
		message = "anthropic stream error"
	}
	errorType := strings.ToLower(strings.TrimSpace(payload.Error.Type))
	messageLower := strings.ToLower(message)
	code := ErrorCodeUpstreamRejected
	temporary := false
	cooldown := time.Duration(0)
	if strings.Contains(errorType, "overload") ||
		strings.Contains(errorType, "server") ||
		strings.Contains(errorType, "temporar") ||
		strings.Contains(messageLower, "overload") ||
		strings.Contains(messageLower, "temporar") {
		code = ErrorCodeUpstreamTemporarilyUnavailable
		temporary = true
		cooldown = 30 * time.Second
	} else if strings.Contains(errorType, "rate_limit") || strings.Contains(messageLower, "rate limit") {
		code = ErrorCodeUpstreamRateLimited
		temporary = true
		cooldown = 45 * time.Second
	}
	return &InvokeError{
		Code:      code,
		Temporary: temporary,
		Cooldown:  cooldown,
		Err:       fmt.Errorf("%s", message),
	}
}
