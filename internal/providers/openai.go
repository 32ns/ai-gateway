package providers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"sort"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

const (
	openAIBaseURL                   = "https://api.openai.com"
	chatGPTCodexBaseURL             = "https://chatgpt.com/backend-api/codex"
	chatGPTCodexDefaultInstructions = "You are a helpful assistant."
)

type OpenAIAdapter struct{}

var (
	openAISystemRoleNeedle = []byte(`"system"`)
)

type openAIChatCompletionRequest struct {
	Model               string                     `json:"model"`
	Messages            any                        `json:"messages"`
	MaxTokens           *int                       `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                       `json:"max_completion_tokens,omitempty"`
	ServiceTier         string                     `json:"service_tier,omitempty"`
	ReasoningEffort     string                     `json:"reasoning_effort,omitempty"`
	PromptCacheKey      string                     `json:"prompt_cache_key,omitempty"`
	Temperature         *float64                   `json:"temperature,omitempty"`
	TopP                *float64                   `json:"top_p,omitempty"`
	Stop                any                        `json:"stop,omitempty"`
	Stream              bool                       `json:"stream"`
	StreamOptions       *openAIStreamOptions       `json:"stream_options,omitempty"`
	Metadata            map[string]string          `json:"metadata,omitempty"`
	Extra               map[string]json.RawMessage `json:"-"`
}

type openAIChatCompletionRequestNoExtra struct {
	Model               string               `json:"model"`
	Messages            any                  `json:"messages,omitempty"`
	MaxTokens           *int                 `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                 `json:"max_completion_tokens,omitempty"`
	ServiceTier         string               `json:"service_tier,omitempty"`
	ReasoningEffort     string               `json:"reasoning_effort,omitempty"`
	PromptCacheKey      string               `json:"prompt_cache_key,omitempty"`
	Temperature         *float64             `json:"temperature,omitempty"`
	TopP                *float64             `json:"top_p,omitempty"`
	Stop                any                  `json:"stop,omitempty"`
	Stream              bool                 `json:"stream"`
	StreamOptions       *openAIStreamOptions `json:"stream_options,omitempty"`
	Metadata            map[string]string    `json:"metadata,omitempty"`
}

type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (r openAIChatCompletionRequest) MarshalJSON() ([]byte, error) {
	r.ServiceTier = strings.TrimSpace(r.ServiceTier)
	r.ReasoningEffort = strings.TrimSpace(r.ReasoningEffort)
	r.PromptCacheKey = strings.TrimSpace(r.PromptCacheKey)
	if len(r.Extra) == 0 {
		return json.Marshal(openAIChatCompletionRequestNoExtra{
			Model:               r.Model,
			Messages:            r.Messages,
			MaxTokens:           r.MaxTokens,
			MaxCompletionTokens: r.MaxCompletionTokens,
			ServiceTier:         r.ServiceTier,
			ReasoningEffort:     r.ReasoningEffort,
			PromptCacheKey:      r.PromptCacheKey,
			Temperature:         r.Temperature,
			TopP:                r.TopP,
			Stop:                r.Stop,
			Stream:              r.Stream,
			StreamOptions:       r.StreamOptions,
			Metadata:            r.Metadata,
		})
	}
	body := make(map[string]any, len(r.Extra)+9)
	for key, value := range r.Extra {
		if strings.TrimSpace(key) != "" && len(value) > 0 {
			body[key] = value
		}
	}
	body["model"] = r.Model
	if r.Messages != nil {
		body["messages"] = r.Messages
	}
	if r.MaxTokens != nil {
		body["max_tokens"] = r.MaxTokens
	}
	if r.MaxCompletionTokens != nil {
		body["max_completion_tokens"] = r.MaxCompletionTokens
	}
	if r.ServiceTier != "" {
		body["service_tier"] = r.ServiceTier
	}
	if r.ReasoningEffort != "" {
		body["reasoning_effort"] = r.ReasoningEffort
	}
	if r.PromptCacheKey != "" {
		body["prompt_cache_key"] = r.PromptCacheKey
	}
	if r.Temperature != nil {
		body["temperature"] = r.Temperature
	}
	if r.TopP != nil {
		body["top_p"] = r.TopP
	}
	if r.Stop != nil {
		body["stop"] = r.Stop
	}
	body["stream"] = r.Stream
	if r.StreamOptions != nil {
		body["stream_options"] = r.StreamOptions
	}
	if len(r.Metadata) > 0 {
		body["metadata"] = r.Metadata
	}
	return json.Marshal(body)
}

type openAIChatCompletionResponse struct {
	ID          string                `json:"id"`
	Model       string                `json:"model"`
	ServiceTier string                `json:"service_tier"`
	Created     int64                 `json:"created"`
	Choices     []openAIChoicePayload `json:"choices"`
	Usage       openAIUsagePayload    `json:"usage"`
}

type openAIChoicePayload struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}

type openAIStreamChoicePayload struct {
	Delta struct {
		Role      string          `json:"role"`
		Content   string          `json:"content"`
		ToolCalls json.RawMessage `json:"tool_calls"`
	} `json:"delta"`
	FinishReason string `json:"finish_reason"`
}

type openAIChatCompletionChunk struct {
	ID          string                      `json:"id"`
	Model       string                      `json:"model"`
	ServiceTier string                      `json:"service_tier"`
	Created     int64                       `json:"created"`
	Choices     []openAIStreamChoicePayload `json:"choices"`
	Usage       *openAIUsagePayload         `json:"usage"`
}

type openAIResponsesRequest struct {
	Model             string                `json:"model"`
	Instructions      string                `json:"instructions,omitempty"`
	Input             []openAIResponsesItem `json:"input"`
	Tools             []any                 `json:"tools"`
	ToolChoice        string                `json:"tool_choice"`
	ParallelToolCalls bool                  `json:"parallel_tool_calls"`
	Reasoning         any                   `json:"reasoning"`
	Store             bool                  `json:"store"`
	Stream            bool                  `json:"stream"`
	Include           []string              `json:"include"`
	PromptCacheKey    string                `json:"prompt_cache_key,omitempty"`
	ServiceTier       string                `json:"service_tier,omitempty"`
}

type openAIResponsesItem struct {
	Type    string                   `json:"type"`
	Role    string                   `json:"role,omitempty"`
	ID      string                   `json:"id,omitempty"`
	Content []openAIResponsesContent `json:"content,omitempty"`
	Summary []openAIResponsesContent `json:"summary,omitempty"`
}

type openAIResponsesContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type openAIResponsesStreamPayload struct {
	Type     string                       `json:"type"`
	Delta    string                       `json:"delta"`
	Item     *openAIResponsesItem         `json:"item"`
	Response *openAIResponsesResponseBody `json:"response"`
}

type openAIResponsesResponseBody struct {
	ID          string                `json:"id"`
	Model       string                `json:"model"`
	ServiceTier string                `json:"service_tier"`
	CreatedAt   int64                 `json:"created_at"`
	Output      []openAIResponsesItem `json:"output"`
	OutputText  string                `json:"output_text"`
	Error       *openAIErrorObject    `json:"error"`
	Usage       openAIResponsesUsage  `json:"usage"`
	Status      string                `json:"status"`
}

type openAIErrorObject struct {
	Message string `json:"message"`
	Code    string `json:"code"`
	Type    string `json:"type"`
}

type openAIResponsesUsage struct {
	InputTokens              int                      `json:"input_tokens"`
	OutputTokens             int                      `json:"output_tokens"`
	TotalTokens              int                      `json:"total_tokens"`
	CacheCreationInputTokens int                      `json:"cache_creation_input_tokens"`
	InputTokensDetails       openAITokenDetails       `json:"input_tokens_details"`
	OutputTokensDetails      openAIOutputTokenDetails `json:"output_tokens_details"`
}

type openAIUsagePayload struct {
	PromptTokens             int                      `json:"prompt_tokens"`
	CompletionTokens         int                      `json:"completion_tokens"`
	TotalTokens              int                      `json:"total_tokens"`
	CacheCreationInputTokens int                      `json:"cache_creation_input_tokens"`
	PromptTokensDetails      openAITokenDetails       `json:"prompt_tokens_details"`
	CompletionTokensDetails  openAIOutputTokenDetails `json:"completion_tokens_details"`
}

type openAITokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type openAIOutputTokenDetails struct {
	ImageTokens int `json:"image_tokens"`
}

func usageFromOpenAIChatPayload(payload openAIUsagePayload) core.Usage {
	total := payload.TotalTokens
	if total <= 0 {
		total = payload.PromptTokens + payload.CompletionTokens
	}
	return core.Usage{
		PromptTokens:        payload.PromptTokens,
		CachedPromptTokens:  payload.PromptTokensDetails.CachedTokens,
		CacheCreationTokens: payload.CacheCreationInputTokens,
		CompletionTokens:    payload.CompletionTokens,
		ImageOutputTokens:   payload.CompletionTokensDetails.ImageTokens,
		TotalTokens:         total,
	}
}

func usageFromOpenAIResponsesPayload(payload openAIResponsesUsage) core.Usage {
	total := payload.TotalTokens
	if total <= 0 {
		total = payload.InputTokens + payload.OutputTokens
	}
	return core.Usage{
		PromptTokens:        payload.InputTokens,
		CachedPromptTokens:  payload.InputTokensDetails.CachedTokens,
		CacheCreationTokens: payload.CacheCreationInputTokens,
		CompletionTokens:    payload.OutputTokens,
		ImageOutputTokens:   payload.OutputTokensDetails.ImageTokens,
		TotalTokens:         total,
	}
}

type openAIEmbeddingRequest struct {
	Model          string `json:"model"`
	Input          any    `json:"input"`
	EncodingFormat string `json:"encoding_format,omitempty"`
	Dimensions     *int   `json:"dimensions,omitempty"`
	User           string `json:"user,omitempty"`
}

type openAIModerationRequest struct {
	Model string `json:"model"`
	Input any    `json:"input"`
}

type openAIImageGenerationRequest struct {
	Model  string                     `json:"model"`
	Prompt string                     `json:"prompt"`
	Extra  map[string]json.RawMessage `json:"-"`
}

type openAIImageGenerationRequestNoExtra struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

func (r openAIImageGenerationRequest) MarshalJSON() ([]byte, error) {
	if len(r.Extra) == 0 {
		return json.Marshal(openAIImageGenerationRequestNoExtra{
			Model:  r.Model,
			Prompt: r.Prompt,
		})
	}
	body := make(map[string]any, len(r.Extra)+2)
	for key, value := range r.Extra {
		if strings.TrimSpace(key) != "" && len(value) > 0 {
			body[key] = value
		}
	}
	body["model"] = r.Model
	body["prompt"] = r.Prompt
	return json.Marshal(body)
}

type openAIAudioSpeechRequest struct {
	Model string                     `json:"model"`
	Input string                     `json:"input"`
	Voice string                     `json:"voice"`
	Extra map[string]json.RawMessage `json:"-"`
}

type openAIAudioSpeechRequestNoExtra struct {
	Model string `json:"model"`
	Input string `json:"input"`
	Voice string `json:"voice"`
}

func (r openAIAudioSpeechRequest) MarshalJSON() ([]byte, error) {
	if len(r.Extra) == 0 {
		return json.Marshal(openAIAudioSpeechRequestNoExtra{
			Model: r.Model,
			Input: r.Input,
			Voice: r.Voice,
		})
	}
	body := make(map[string]any, len(r.Extra)+3)
	for key, value := range r.Extra {
		if strings.TrimSpace(key) != "" && len(value) > 0 {
			body[key] = value
		}
	}
	body["model"] = r.Model
	body["input"] = r.Input
	body["voice"] = r.Voice
	return json.Marshal(body)
}

type openAIEmbeddingUsageResponse struct {
	Model string             `json:"model"`
	Usage openAIUsagePayload `json:"usage"`
}

func (a *OpenAIAdapter) Kind() core.ProviderKind {
	return core.ProviderOpenAI
}

func (a *OpenAIAdapter) DisplayName() string {
	return "OpenAI"
}

func (a *OpenAIAdapter) ListModels(context.Context) []core.ModelSpec {
	return nil
}

type openAIModelsResponse struct {
	Data []openAIModelObject `json:"data"`
}

type openAIModelObject struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by"`
}

type chatGPTCodexModelsResponse struct {
	Models []chatGPTCodexModelObject `json:"models"`
}

type chatGPTCodexModelObject struct {
	Slug        string `json:"slug"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	OwnedBy     string `json:"owned_by"`
}

func (a *OpenAIAdapter) FetchModels(ctx context.Context, account core.Account) ([]UpstreamModel, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, account.EffectiveProxyURL), account)
	accessToken, err := accessTokenForAccount(account)
	if err != nil {
		return nil, err
	}
	if usesChatGPTCodexBackend(account) {
		return a.fetchChatGPTCodexModels(ctx, account, accessToken)
	}
	return a.fetchOpenAIAPIModels(ctx, account, accessToken)
}

func (a *OpenAIAdapter) fetchOpenAIAPIModels(ctx context.Context, account core.Account, accessToken string) ([]UpstreamModel, error) {
	endpoint := resolveEndpoint(account, openAIBaseURL, "/v1/models")
	req, err := newJSONRequest(ctx, "GET", endpoint, accessToken, nil, nil)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}

	var upstream openAIModelsResponse
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
			ID:      id,
			OwnedBy: strings.TrimSpace(item.OwnedBy),
		})
	}
	return models, nil
}

func (a *OpenAIAdapter) fetchChatGPTCodexModels(ctx context.Context, account core.Account, accessToken string) ([]UpstreamModel, error) {
	endpoint := resolveChatGPTCodexEndpoint(account, "/models")
	req, err := newJSONRequest(ctx, "GET", endpoint, accessToken, nil, chatGPTCodexHeaders(account))
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}

	var upstream chatGPTCodexModelsResponse
	if _, err := doDecodeJSON(req, &upstream); err != nil {
		return nil, err
	}

	models := make([]UpstreamModel, 0, len(upstream.Models))
	for _, item := range upstream.Models {
		id := strings.TrimSpace(item.Slug)
		if id == "" {
			id = strings.TrimSpace(item.ID)
		}
		if id == "" {
			continue
		}
		ownedBy := strings.TrimSpace(item.OwnedBy)
		if ownedBy == "" {
			ownedBy = string(core.ProviderOpenAI)
		}
		models = append(models, UpstreamModel{
			ID:          id,
			DisplayName: strings.TrimSpace(item.DisplayName),
			OwnedBy:     ownedBy,
		})
	}
	return models, nil
}

func chatGPTCodexHeaders(account core.Account) map[string]string {
	if accountID := strings.TrimSpace(account.Credential.Metadata["oauth_account_id"]); accountID != "" {
		return map[string]string{"ChatGPT-Account-ID": accountID}
	}
	return nil
}

func resolveChatGPTCodexEndpoint(account core.Account, suffix string) string {
	base := strings.TrimSpace(account.Credential.Metadata["codex_base_url"])
	if base == "" {
		base = chatGPTCodexBaseURL
	}
	base = strings.TrimRight(base, "/")
	endpoint := base + suffix
	separator := "?"
	if strings.Contains(endpoint, "?") {
		separator = "&"
	}
	return endpoint + separator + "client_version=0.0.0"
}

func (a *OpenAIAdapter) Invoke(ctx context.Context, decision core.RouteDecision, req *core.GatewayRequest) (*core.GatewayResponse, error) {
	if req != nil && req.UpstreamMode == "anthropic_messages" && openAIAnthropicBridgeUsesResponses(decision.Model) {
		responsesReq, err := responsesRequestFromAnthropicGatewayRequest(decision.Account, req, core.ResponsesTransportHTTP)
		if err != nil {
			return nil, &InvokeError{
				Code:      ErrorCodeUpstreamRequestBuildFailed,
				Temporary: true,
				Cooldown:  10 * time.Second,
				Err:       err,
			}
		}
		return a.InvokeResponses(ctx, decision, responsesReq)
	}
	if usesChatGPTCodexBackend(decision.Account) {
		return a.invokeChatGPTCodex(ctx, decision, req)
	}

	ctx = WithUpstreamAccount(WithProxyURL(ctx, decision.Account.EffectiveProxyURL), decision.Account)
	accessToken, err := accessTokenForAccount(decision.Account)
	if err != nil {
		return nil, err
	}

	endpoint := resolveEndpoint(decision.Account, openAIBaseURL, "/v1/chat/completions")
	payload := openAIChatCompletionPayloadForAccount(decision.Model, req, false, decision.Account)

	httpReq, err := newJSONRequest(ctx, "POST", endpoint, accessToken, payload, openAISub2APISessionHeaders(decision.Account, req.Metadata))
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}

	var upstream openAIChatCompletionResponse
	_, rawBody, err := doJSON(httpReq, &upstream)
	if err != nil {
		return nil, err
	}
	if err := openAITopLevelErrorBody(decision.Account, rawBody); err != nil {
		return nil, err
	}
	if len(upstream.Choices) == 0 {
		if responses, ok := parseOpenAIResponsesBody(rawBody); ok {
			return gatewayResponseFromOpenAIResponsesBody(responses, decision, nil), nil
		}
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamEmptyResponse,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       fmt.Errorf("upstream returned no choices"),
		}
	}

	createdAt := time.Now().UTC()
	if upstream.Created > 0 {
		createdAt = time.Unix(upstream.Created, 0).UTC()
	}

	return &core.GatewayResponse{
		ID:           upstream.ID,
		Model:        firstNonEmpty(upstream.Model, decision.Model),
		Provider:     core.ProviderOpenAI,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		ServiceTier:  strings.TrimSpace(upstream.ServiceTier),
		Content:      upstream.Choices[0].Message.Content,
		FinishReason: firstNonEmpty(upstream.Choices[0].FinishReason, "stop"),
		CreatedAt:    createdAt,
		Usage:        usageFromOpenAIChatPayload(upstream.Usage),
		RawBody:      rawBody,
	}, nil
}

func (a *OpenAIAdapter) InvokeResponses(ctx context.Context, decision core.RouteDecision, req *core.ResponsesRequest) (*core.GatewayResponse, error) {
	if req != nil && req.Compact {
		if usesChatGPTCodexBackend(decision.Account) {
			return a.invokeChatGPTCodexCompactResponses(ctx, decision, req)
		}
		return a.invokeOpenAIResponsesCompactResponses(ctx, decision, req)
	}
	if usesChatGPTCodexBackend(decision.Account) {
		return a.invokeChatGPTCodexResponses(ctx, decision, req)
	}
	return a.invokeOpenAIResponsesWithPathResponses(ctx, decision, req, "/v1/responses")
}

func parseOpenAIResponsesBody(rawBody []byte) (openAIResponsesResponseBody, bool) {
	var probe struct {
		Object string `json:"object"`
	}
	if err := json.Unmarshal(rawBody, &probe); err != nil || strings.TrimSpace(probe.Object) != "response" {
		return openAIResponsesResponseBody{}, false
	}
	var response openAIResponsesResponseBody
	if err := json.Unmarshal(rawBody, &response); err != nil {
		return openAIResponsesResponseBody{}, false
	}
	return response, true
}

func gatewayResponseFromOpenAIResponsesBody(upstream openAIResponsesResponseBody, decision core.RouteDecision, rawBody []byte) *core.GatewayResponse {
	createdAt := time.Now().UTC()
	if upstream.CreatedAt > 0 {
		createdAt = time.Unix(upstream.CreatedAt, 0).UTC()
	}
	content := strings.TrimSpace(upstream.OutputText)
	if content == "" {
		content = outputTextFromResponsesItems(upstream.Output)
	}
	if content == "" && upstream.Error != nil {
		content = strings.TrimSpace(upstream.Error.Message)
	}
	return &core.GatewayResponse{
		ID:           upstream.ID,
		Model:        firstNonEmpty(upstream.Model, decision.Model),
		Provider:     core.ProviderOpenAI,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		ServiceTier:  strings.TrimSpace(upstream.ServiceTier),
		Content:      content,
		FinishReason: firstNonEmpty(upstream.Status, "completed"),
		CreatedAt:    createdAt,
		Usage:        usageFromOpenAIResponsesPayload(upstream.Usage),
		RawBody:      rawBody,
	}
}

func (a *OpenAIAdapter) invokeOpenAIResponsesWithPathResponses(ctx context.Context, decision core.RouteDecision, req *core.ResponsesRequest, path string) (*core.GatewayResponse, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, decision.Account.EffectiveProxyURL), decision.Account)
	accessToken, err := accessTokenForAccount(decision.Account)
	if err != nil {
		return nil, err
	}

	payload, err := openAIResponsesRawPayloadFromResponsesForAccount(decision.Model, req, decision.Account)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}
	httpReq, err := newJSONRequest(ctx, "POST", resolveEndpoint(decision.Account, openAIBaseURL, path), accessToken, payload, openAIResponsesRequestHeaders(decision.Account, req))
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}

	var upstream openAIResponsesResponseBody
	_, rawBody, err := doJSON(httpReq, &upstream)
	if err != nil {
		return nil, err
	}
	if err := openAITopLevelErrorBody(decision.Account, rawBody); err != nil {
		return nil, err
	}

	return gatewayResponseFromOpenAIResponsesBody(upstream, decision, rawBody), nil
}

func (a *OpenAIAdapter) invokeOpenAIResponsesCompactResponses(ctx context.Context, decision core.RouteDecision, req *core.ResponsesRequest) (*core.GatewayResponse, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, decision.Account.EffectiveProxyURL), decision.Account)
	accessToken, err := accessTokenForAccount(decision.Account)
	if err != nil {
		return nil, err
	}

	payload, err := openAIResponsesCompactPayloadFromResponsesForAccount(decision.Model, req, decision.Account)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}
	httpReq, err := newJSONRequest(ctx, "POST", resolveEndpoint(decision.Account, openAIBaseURL, "/v1/responses/compact"), accessToken, payload, openAIResponsesRequestHeaders(decision.Account, req))
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}

	var upstream openAIResponsesResponseBody
	_, rawBody, err := doJSON(httpReq, &upstream)
	if err != nil {
		return nil, err
	}
	if err := openAITopLevelErrorBody(decision.Account, rawBody); err != nil {
		return nil, err
	}

	return gatewayResponseFromOpenAIResponsesBody(upstream, decision, rawBody), nil
}

func openAITopLevelErrorBody(account core.Account, body []byte) error {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	if _, exists := payload["error"]; !exists {
		return nil
	}
	if object, _ := payload["object"].(string); strings.TrimSpace(object) == "response" || strings.TrimSpace(object) == "response.compaction" {
		return nil
	}
	return mapHTTPErrorForAccount(account, http.StatusBadRequest, body)
}

type RawProxyResponse struct {
	StatusCode  int
	ContentType string
	Body        []byte
}

func (a *OpenAIAdapter) ProxyResponseResource(ctx context.Context, account core.Account, method, path, rawQuery string, body []byte, contentType string) (*RawProxyResponse, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, account.EffectiveProxyURL), account)
	accessToken, err := accessTokenForAccount(account)
	if err != nil {
		return nil, err
	}
	endpoint := resolveEndpoint(account, openAIBaseURL, path)
	if strings.TrimSpace(rawQuery) != "" {
		separator := "?"
		if strings.Contains(endpoint, "?") {
			separator = "&"
		}
		endpoint += separator + rawQuery
	}
	httpReq, err := newRawRequest(ctx, method, endpoint, accessToken, body, contentType)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}
	client, err := httpClientForContext(ctx)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamTransportError,
			Temporary: true,
			Cooldown:  20 * time.Second,
			Err:       err,
		}
	}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamTransportError,
			Temporary: true,
			Cooldown:  20 * time.Second,
			Err:       err,
		}
	}
	defer httpResp.Body.Close()
	payload, err := readLimitedUpstreamBody(httpResp.Body, maxUpstreamResponseBodyBytes)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamReadError,
			Temporary: true,
			Cooldown:  20 * time.Second,
			Err:       err,
		}
	}
	return &RawProxyResponse{
		StatusCode:  httpResp.StatusCode,
		ContentType: httpResp.Header.Get("Content-Type"),
		Body:        payload,
	}, nil
}

func (a *OpenAIAdapter) invokeChatGPTCodexCompactResponses(ctx context.Context, decision core.RouteDecision, req *core.ResponsesRequest) (*core.GatewayResponse, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, decision.Account.EffectiveProxyURL), decision.Account)
	accessToken, err := accessTokenForAccount(decision.Account)
	if err != nil {
		return nil, err
	}

	payload, err := openAIResponsesCompactPayloadFromResponses(decision.Model, req)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}
	httpReq, err := newJSONRequest(ctx, "POST", resolveChatGPTCodexEndpoint(decision.Account, "/responses/compact"), accessToken, payload, chatGPTCodexResponsesRequestHeaders(decision.Account, req))
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}

	var upstream openAIResponsesResponseBody
	_, rawBody, err := doJSON(httpReq, &upstream)
	if err != nil {
		return nil, err
	}

	return gatewayResponseFromOpenAIResponsesBody(upstream, decision, rawBody), nil
}

func (a *OpenAIAdapter) invokeChatGPTCodex(ctx context.Context, decision core.RouteDecision, req *core.GatewayRequest) (*core.GatewayResponse, error) {
	session, err := a.openChatGPTCodexStream(ctx, decision, req)
	if err != nil {
		return nil, err
	}
	defer session.Stream.Close()

	var content strings.Builder
	var rawCompletedResponse []byte
	for {
		event, err := session.Stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if event == nil {
			continue
		}
		if len(event.RawData) > 0 && event.RawEvent == "response.completed" {
			rawCompletedResponse = completedResponseRawBody(event.RawData)
		}
		content.WriteString(event.Delta)
		if event.FinishReason != "" {
			session.Response.FinishReason = event.FinishReason
		}
		if event.Usage != nil {
			session.Response.Usage = *event.Usage
		}
	}
	session.Response.Content = content.String()
	if session.Response.FinishReason == "" {
		session.Response.FinishReason = "stop"
	}
	if len(rawCompletedResponse) > 0 {
		session.Response.RawBody = rawCompletedResponse
	}
	return session.Response, nil
}

func (a *OpenAIAdapter) invokeChatGPTCodexResponses(ctx context.Context, decision core.RouteDecision, req *core.ResponsesRequest) (*core.GatewayResponse, error) {
	session, err := a.openChatGPTCodexResponsesStream(ctx, decision, req)
	if err != nil {
		return nil, err
	}
	defer session.Stream.Close()

	var content strings.Builder
	var rawCompletedResponse []byte
	for {
		event, err := session.Stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if event == nil {
			continue
		}
		if len(event.RawData) > 0 && event.RawEvent == "response.completed" {
			rawCompletedResponse = completedResponseRawBody(event.RawData)
		}
		content.WriteString(event.Delta)
		if event.FinishReason != "" {
			session.Response.FinishReason = event.FinishReason
		}
		if event.Usage != nil {
			session.Response.Usage = *event.Usage
		}
	}
	session.Response.Content = content.String()
	if session.Response.FinishReason == "" {
		session.Response.FinishReason = "stop"
	}
	if len(rawCompletedResponse) > 0 {
		session.Response.RawBody = rawCompletedResponse
	}
	return session.Response, nil
}

func (a *OpenAIAdapter) Embed(ctx context.Context, decision core.RouteDecision, req *core.EmbeddingRequest) (*core.EmbeddingResponse, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, decision.Account.EffectiveProxyURL), decision.Account)
	accessToken, err := accessTokenForAccount(decision.Account)
	if err != nil {
		return nil, err
	}

	payload := openAIEmbeddingRequest{
		Model:      decision.Model,
		Input:      embeddingInputPayload(req.Input),
		Dimensions: req.Dimensions,
		User:       req.User,
	}
	if len(req.RawBody) > 0 {
		rawPayload := map[string]any{}
		if err := json.Unmarshal(req.RawBody, &rawPayload); err != nil {
			return nil, &InvokeError{
				Code:      ErrorCodeUpstreamRequestBuildFailed,
				Temporary: true,
				Cooldown:  10 * time.Second,
				Err:       err,
			}
		}
		rawPayload["model"] = decision.Model
		payloadAny := rawPayload
		endpoint := resolveEndpoint(decision.Account, openAIBaseURL, "/v1/embeddings")
		httpReq, err := newJSONRequest(ctx, "POST", endpoint, accessToken, payloadAny, nil)
		if err != nil {
			return nil, &InvokeError{
				Code:      ErrorCodeUpstreamRequestBuildFailed,
				Temporary: true,
				Cooldown:  10 * time.Second,
				Err:       err,
			}
		}
		var upstream openAIEmbeddingUsageResponse
		_, rawBody, err := doJSON(httpReq, &upstream)
		if err != nil {
			return nil, err
		}
		return openAIEmbeddingRawResponseToCore(upstream, rawBody, decision), nil
	}
	if strings.TrimSpace(req.EncodingFormat) != "" {
		payload.EncodingFormat = req.EncodingFormat
	}

	endpoint := resolveEndpoint(decision.Account, openAIBaseURL, "/v1/embeddings")
	httpReq, err := newJSONRequest(ctx, "POST", endpoint, accessToken, payload, nil)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}

	var upstream openAIEmbeddingUsageResponse
	_, rawBody, err := doJSON(httpReq, &upstream)
	if err != nil {
		return nil, err
	}
	return openAIEmbeddingRawResponseToCore(upstream, rawBody, decision), nil
}

func openAIEmbeddingRawResponseToCore(upstream openAIEmbeddingUsageResponse, rawBody []byte, decision core.RouteDecision) *core.EmbeddingResponse {
	return &core.EmbeddingResponse{
		Model:        firstNonEmpty(upstream.Model, decision.Model),
		Provider:     core.ProviderOpenAI,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Usage:        usageFromOpenAIChatPayload(upstream.Usage),
		RawBody:      rawBody,
	}
}

func (a *OpenAIAdapter) Moderate(ctx context.Context, decision core.RouteDecision, req *core.ModerationRequest) (*core.ModerationResponse, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, decision.Account.EffectiveProxyURL), decision.Account)
	accessToken, err := accessTokenForAccount(decision.Account)
	if err != nil {
		return nil, err
	}

	payload := openAIModerationRequest{
		Model: decision.Model,
		Input: req.Input,
	}
	endpoint := resolveEndpoint(decision.Account, openAIBaseURL, "/v1/moderations")
	httpReq, err := newJSONRequest(ctx, "POST", endpoint, accessToken, payload, nil)
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
	return &core.ModerationResponse{
		Model:        decision.Model,
		Provider:     core.ProviderOpenAI,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Body:         rawBody,
	}, nil
}

func (a *OpenAIAdapter) GenerateImage(ctx context.Context, decision core.RouteDecision, req *core.ImageGenerationRequest) (*core.ImageGenerationResponse, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, decision.Account.EffectiveProxyURL), decision.Account)
	accessToken, err := accessTokenForAccount(decision.Account)
	if err != nil {
		return nil, err
	}

	payload := openAIImageGenerationRequest{
		Model:  decision.Model,
		Prompt: req.Prompt,
		Extra:  cloneRawMessageMapWithout(req.Extra, "model", "prompt", "route_affinity_key", "cache_affinity_key", "route_affinity_model", "prompt_cache_key"),
	}
	endpoint := resolveEndpoint(decision.Account, openAIBaseURL, "/v1/images/generations")
	httpReq, err := newJSONRequest(ctx, "POST", endpoint, accessToken, payload, nil)
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
	return &core.ImageGenerationResponse{
		Model:        decision.Model,
		Provider:     core.ProviderOpenAI,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Body:         rawBody,
	}, nil
}

func (a *OpenAIAdapter) OpenImageGenerationStream(ctx context.Context, decision core.RouteDecision, req *core.ImageGenerationRequest) (*StreamSession, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, decision.Account.EffectiveProxyURL), decision.Account)
	accessToken, err := accessTokenForAccount(decision.Account)
	if err != nil {
		return nil, err
	}

	payload := openAIImageGenerationRequest{
		Model:  decision.Model,
		Prompt: req.Prompt,
		Extra:  cloneRawMessageMapWithout(req.Extra, "model", "prompt", "route_affinity_key", "cache_affinity_key", "route_affinity_model", "prompt_cache_key"),
	}
	endpoint := resolveEndpoint(decision.Account, openAIBaseURL, "/v1/images/generations")
	httpReq, err := newJSONRequest(ctx, "POST", endpoint, accessToken, payload, nil)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	httpResp, err := doStream(httpReq)
	if err != nil {
		return nil, err
	}
	response := &core.GatewayResponse{
		ID:           fmt.Sprintf("image_openai_stream_%d", time.Now().UnixNano()),
		Model:        decision.Model,
		Provider:     core.ProviderOpenAI,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		CreatedAt:    time.Now().UTC(),
		FinishReason: "stream",
	}
	return &StreamSession{
		Response: response,
		Stream:   &openAIImageStream{body: httpResp.Body, reader: newSSEReader(httpResp.Body), account: decision.Account},
	}, nil
}

func (a *OpenAIAdapter) CreateSpeech(ctx context.Context, decision core.RouteDecision, req *core.AudioSpeechRequest) (*core.AudioSpeechResponse, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, decision.Account.EffectiveProxyURL), decision.Account)
	accessToken, err := accessTokenForAccount(decision.Account)
	if err != nil {
		return nil, err
	}

	if len(req.RawBody) > 0 {
		rawPayload := map[string]any{}
		if err := json.Unmarshal(req.RawBody, &rawPayload); err != nil {
			return nil, &InvokeError{
				Code:      ErrorCodeUpstreamRequestBuildFailed,
				Temporary: true,
				Cooldown:  10 * time.Second,
				Err:       err,
			}
		}
		rawPayload["model"] = decision.Model
		endpoint := resolveEndpoint(decision.Account, openAIBaseURL, "/v1/audio/speech")
		httpReq, err := newJSONRequest(ctx, "POST", endpoint, accessToken, rawPayload, nil)
		if err != nil {
			return nil, &InvokeError{
				Code:      ErrorCodeUpstreamRequestBuildFailed,
				Temporary: true,
				Cooldown:  10 * time.Second,
				Err:       err,
			}
		}
		httpResp, rawBody, err := doJSON(httpReq, nil)
		if err != nil {
			return nil, err
		}
		contentType := ""
		if httpResp != nil {
			contentType = strings.TrimSpace(httpResp.Header.Get("Content-Type"))
		}
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		return &core.AudioSpeechResponse{
			Model:        decision.Model,
			Provider:     core.ProviderOpenAI,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			Body:         rawBody,
			ContentType:  contentType,
		}, nil
	}

	payload := openAIAudioSpeechRequest{
		Model: decision.Model,
		Input: req.Input,
		Voice: req.Voice,
		Extra: cloneRawMessageMapWithout(req.Extra, "model", "input", "voice"),
	}
	endpoint := resolveEndpoint(decision.Account, openAIBaseURL, "/v1/audio/speech")
	httpReq, err := newJSONRequest(ctx, "POST", endpoint, accessToken, payload, nil)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}

	httpResp, rawBody, err := doJSON(httpReq, nil)
	if err != nil {
		return nil, err
	}
	contentType := ""
	if httpResp != nil {
		contentType = strings.TrimSpace(httpResp.Header.Get("Content-Type"))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return &core.AudioSpeechResponse{
		Model:        decision.Model,
		Provider:     core.ProviderOpenAI,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Body:         rawBody,
		ContentType:  contentType,
	}, nil
}

func (a *OpenAIAdapter) ProcessAudioMultipart(ctx context.Context, decision core.RouteDecision, req *core.AudioMultipartRequest) (*core.AudioMultipartResponse, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, decision.Account.EffectiveProxyURL), decision.Account)
	accessToken, err := accessTokenForAccount(decision.Account)
	if err != nil {
		return nil, err
	}

	body := req.Body
	contentType := req.ContentType
	if strings.TrimSpace(req.Model) != strings.TrimSpace(decision.Model) {
		var err error
		body, contentType, err = rewriteMultipartModel(req.Body, req.ContentType, decision.Model)
		if err != nil {
			return nil, &InvokeError{
				Code:      ErrorCodeUpstreamRequestBuildFailed,
				Temporary: true,
				Cooldown:  10 * time.Second,
				Err:       err,
			}
		}
	}

	endpoint := resolveEndpoint(decision.Account, openAIBaseURL, req.Endpoint)
	httpReq, err := newRawRequest(ctx, "POST", endpoint, accessToken, body, contentType)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}

	httpResp, rawBody, err := doJSON(httpReq, nil)
	if err != nil {
		return nil, err
	}
	respContentType := ""
	if httpResp != nil {
		respContentType = strings.TrimSpace(httpResp.Header.Get("Content-Type"))
	}
	if respContentType == "" {
		respContentType = "application/json"
	}
	return &core.AudioMultipartResponse{
		Model:        decision.Model,
		Provider:     core.ProviderOpenAI,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Body:         rawBody,
		ContentType:  respContentType,
	}, nil
}

func (a *OpenAIAdapter) ProcessImageMultipart(ctx context.Context, decision core.RouteDecision, req *core.ImageMultipartRequest) (*core.ImageMultipartResponse, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, decision.Account.EffectiveProxyURL), decision.Account)
	accessToken, err := accessTokenForAccount(decision.Account)
	if err != nil {
		return nil, err
	}

	body := req.Body
	contentType := req.ContentType
	if strings.TrimSpace(req.Model) != strings.TrimSpace(decision.Model) || !imageMultipartHasFormField(req, "model") {
		var err error
		body, contentType, err = rewriteMultipartModel(req.Body, req.ContentType, decision.Model)
		if err != nil {
			return nil, &InvokeError{
				Code:      ErrorCodeUpstreamRequestBuildFailed,
				Temporary: true,
				Cooldown:  10 * time.Second,
				Err:       err,
			}
		}
	}

	endpoint := resolveEndpoint(decision.Account, openAIBaseURL, req.Endpoint)
	httpReq, err := newRawRequest(ctx, "POST", endpoint, accessToken, body, contentType)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}

	httpResp, rawBody, err := doJSON(httpReq, nil)
	if err != nil {
		return nil, err
	}
	respContentType := ""
	if httpResp != nil {
		respContentType = strings.TrimSpace(httpResp.Header.Get("Content-Type"))
	}
	if respContentType == "" {
		respContentType = "application/json"
	}
	return &core.ImageMultipartResponse{
		Model:        decision.Model,
		Provider:     core.ProviderOpenAI,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Body:         rawBody,
		ContentType:  respContentType,
	}, nil
}

func (a *OpenAIAdapter) OpenImageMultipartStream(ctx context.Context, decision core.RouteDecision, req *core.ImageMultipartRequest) (*StreamSession, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, decision.Account.EffectiveProxyURL), decision.Account)
	accessToken, err := accessTokenForAccount(decision.Account)
	if err != nil {
		return nil, err
	}

	body := req.Body
	contentType := req.ContentType
	if strings.TrimSpace(req.Model) != strings.TrimSpace(decision.Model) || !imageMultipartHasFormField(req, "model") {
		var err error
		body, contentType, err = rewriteMultipartModel(req.Body, req.ContentType, decision.Model)
		if err != nil {
			return nil, &InvokeError{
				Code:      ErrorCodeUpstreamRequestBuildFailed,
				Temporary: true,
				Cooldown:  10 * time.Second,
				Err:       err,
			}
		}
	}

	endpoint := resolveEndpoint(decision.Account, openAIBaseURL, req.Endpoint)
	httpReq, err := newRawRequest(ctx, "POST", endpoint, accessToken, body, contentType)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	httpResp, err := doStream(httpReq)
	if err != nil {
		return nil, err
	}
	response := &core.GatewayResponse{
		ID:           fmt.Sprintf("image_openai_stream_%d", time.Now().UnixNano()),
		Model:        decision.Model,
		Provider:     core.ProviderOpenAI,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		CreatedAt:    time.Now().UTC(),
		FinishReason: "stream",
	}
	return &StreamSession{
		Response: response,
		Stream:   &openAIImageStream{body: httpResp.Body, reader: newSSEReader(httpResp.Body), account: decision.Account},
	}, nil
}

func rewriteMultipartModel(body []byte, contentType, model string) ([]byte, string, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, "", err
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return nil, "", fmt.Errorf("content type must be multipart/form-data")
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return nil, "", fmt.Errorf("multipart boundary is required")
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	modelWritten := false

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = writer.Close()
			return nil, "", err
		}
		formName := part.FormName()
		if formName == "" {
			continue
		}
		if formName == "model" {
			field, err := writer.CreateFormField("model")
			if err != nil {
				_ = writer.Close()
				return nil, "", err
			}
			if _, err := field.Write([]byte(model)); err != nil {
				_ = writer.Close()
				return nil, "", err
			}
			modelWritten = true
			continue
		}
		var target io.Writer
		if filename := part.FileName(); filename != "" {
			target, err = writer.CreatePart(cloneMultipartPartHeader(part.Header))
		} else {
			target, err = writer.CreateFormField(formName)
		}
		if err != nil {
			_ = writer.Close()
			return nil, "", err
		}
		if _, err := io.Copy(target, part); err != nil {
			_ = writer.Close()
			return nil, "", err
		}
	}
	if !modelWritten {
		field, err := writer.CreateFormField("model")
		if err != nil {
			_ = writer.Close()
			return nil, "", err
		}
		if _, err := field.Write([]byte(model)); err != nil {
			_ = writer.Close()
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return out.Bytes(), writer.FormDataContentType(), nil
}

func multipartHasFormField(body []byte, contentType, name string) bool {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return false
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return false
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return false
		}
		if err != nil {
			return false
		}
		if part.FormName() == name {
			return true
		}
	}
}

func imageMultipartHasFormField(req *core.ImageMultipartRequest, name string) bool {
	if req == nil {
		return false
	}
	if _, ok := req.FormFields[name]; ok {
		return true
	}
	return multipartHasFormField(req.Body, req.ContentType, name)
}

func cloneMultipartPartHeader(header textproto.MIMEHeader) textproto.MIMEHeader {
	out := make(textproto.MIMEHeader, len(header))
	for key, values := range header {
		out[key] = append([]string(nil), values...)
	}
	return out
}

func (a *OpenAIAdapter) OpenStream(ctx context.Context, decision core.RouteDecision, req *core.GatewayRequest) (*StreamSession, error) {
	if req != nil && req.UpstreamMode == "anthropic_messages" && openAIAnthropicBridgeUsesResponses(decision.Model) {
		responsesReq, err := responsesRequestFromAnthropicGatewayRequest(decision.Account, req, core.ResponsesTransportSSE)
		if err != nil {
			return nil, &InvokeError{
				Code:      ErrorCodeUpstreamRequestBuildFailed,
				Temporary: true,
				Cooldown:  10 * time.Second,
				Err:       err,
			}
		}
		return a.OpenResponsesStream(ctx, decision, responsesReq)
	}
	if usesChatGPTCodexBackend(decision.Account) {
		return a.openChatGPTCodexStream(ctx, decision, req)
	}

	ctx = WithUpstreamAccount(WithProxyURL(ctx, decision.Account.EffectiveProxyURL), decision.Account)
	accessToken, err := accessTokenForAccount(decision.Account)
	if err != nil {
		return nil, err
	}

	meta := &core.GatewayResponse{
		ID:           fmt.Sprintf("resp_openai_stream_%d", time.Now().UnixNano()),
		Model:        decision.Model,
		Provider:     core.ProviderOpenAI,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		CreatedAt:    time.Now().UTC(),
	}

	endpoint := resolveEndpoint(decision.Account, openAIBaseURL, "/v1/chat/completions")
	payload := openAIChatCompletionPayloadForAccount(decision.Model, req, true, decision.Account)

	httpReq, err := newJSONRequest(ctx, "POST", endpoint, accessToken, payload, openAISub2APISessionHeaders(decision.Account, req.Metadata))
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
		Stream: &openAIStream{
			body:     httpResp.Body,
			reader:   newSSEReader(httpResp.Body),
			response: meta,
			account:  decision.Account,
		},
	}, nil
}

func (a *OpenAIAdapter) OpenResponsesStream(ctx context.Context, decision core.RouteDecision, req *core.ResponsesRequest) (*StreamSession, error) {
	if req != nil && req.Transport == core.ResponsesTransportWebSocket {
		if usesChatGPTCodexBackend(decision.Account) {
			return a.openChatGPTCodexResponsesWebSocketStream(ctx, decision, req)
		}
		return a.openOpenAIResponsesWebSocketStream(ctx, decision, req)
	}
	if usesChatGPTCodexBackend(decision.Account) {
		return a.openChatGPTCodexResponsesStream(ctx, decision, req)
	}
	return a.openOpenAIResponsesStreamResponses(ctx, decision, req)
}

func (a *OpenAIAdapter) openOpenAIResponsesStreamResponses(ctx context.Context, decision core.RouteDecision, req *core.ResponsesRequest) (*StreamSession, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, decision.Account.EffectiveProxyURL), decision.Account)
	accessToken, err := accessTokenForAccount(decision.Account)
	if err != nil {
		return nil, err
	}
	meta := &core.GatewayResponse{
		ID:           fmt.Sprintf("resp_openai_response_stream_%d", time.Now().UnixNano()),
		Model:        decision.Model,
		Provider:     core.ProviderOpenAI,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		FinishReason: "stop",
		CreatedAt:    time.Now().UTC(),
	}

	payload, err := openAIResponsesRawPayloadFromResponsesForAccount(decision.Model, req, decision.Account)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}
	httpReq, err := newJSONRequest(ctx, "POST", resolveEndpoint(decision.Account, openAIBaseURL, "/v1/responses"), accessToken, payload, openAIResponsesRequestHeaders(decision.Account, req))
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	httpResp, err := doStream(httpReq)
	if err != nil {
		return nil, err
	}
	return &StreamSession{
		Response: meta,
		Stream: &openAIResponsesStream{
			body:     httpResp.Body,
			reader:   newSSEReader(httpResp.Body),
			response: meta,
			account:  decision.Account,
		},
	}, nil
}

func (a *OpenAIAdapter) openChatGPTCodexStream(ctx context.Context, decision core.RouteDecision, req *core.GatewayRequest) (*StreamSession, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, decision.Account.EffectiveProxyURL), decision.Account)
	accessToken, err := accessTokenForAccount(decision.Account)
	if err != nil {
		return nil, err
	}

	meta := &core.GatewayResponse{
		ID:           fmt.Sprintf("resp_openai_codex_%d", time.Now().UnixNano()),
		Model:        decision.Model,
		Provider:     core.ProviderOpenAI,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		FinishReason: "stop",
		CreatedAt:    time.Now().UTC(),
	}

	payload, err := buildChatGPTCodexRequestPayload(decision, req)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}
	httpReq, err := newJSONRequest(ctx, "POST", resolveChatGPTCodexEndpoint(decision.Account, "/responses"), accessToken, payload, chatGPTCodexHeaders(decision.Account))
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	httpResp, err := doStream(httpReq)
	if err != nil {
		return nil, err
	}

	return &StreamSession{
		Response: meta,
		Stream: &openAIResponsesStream{
			body:     httpResp.Body,
			reader:   newSSEReader(httpResp.Body),
			response: meta,
			account:  decision.Account,
		},
	}, nil
}

func (a *OpenAIAdapter) openChatGPTCodexResponsesStream(ctx context.Context, decision core.RouteDecision, req *core.ResponsesRequest) (*StreamSession, error) {
	ctx = WithUpstreamAccount(WithProxyURL(ctx, decision.Account.EffectiveProxyURL), decision.Account)
	accessToken, err := accessTokenForAccount(decision.Account)
	if err != nil {
		return nil, err
	}

	meta := &core.GatewayResponse{
		ID:           fmt.Sprintf("resp_openai_codex_%d", time.Now().UnixNano()),
		Model:        decision.Model,
		Provider:     core.ProviderOpenAI,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		FinishReason: "stop",
		CreatedAt:    time.Now().UTC(),
	}

	payload, err := chatGPTCodexRawResponsesPayloadFromResponses(decision.Model, req)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}
	httpReq, err := newJSONRequest(ctx, "POST", resolveChatGPTCodexEndpoint(decision.Account, "/responses"), accessToken, payload, chatGPTCodexResponsesRequestHeaders(decision.Account, req))
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	httpResp, err := doStream(httpReq)
	if err != nil {
		return nil, err
	}

	return &StreamSession{
		Response: meta,
		Stream: &openAIResponsesStream{
			body:     httpResp.Body,
			reader:   newSSEReader(httpResp.Body),
			response: meta,
			account:  decision.Account,
		},
	}, nil
}

func usesChatGPTCodexBackend(account core.Account) bool {
	if IsOpenAIOAuthTokenSource(account.Credential.Metadata["token_source"]) {
		return true
	}
	return strings.TrimSpace(account.Credential.Metadata["account_login_method"]) == "token"
}

func IsOpenAIChatGPTFreeAccount(account core.Account) bool {
	if account.Provider != core.ProviderOpenAI {
		return false
	}
	quota := core.ReadAccountQuota(account)
	if quota == nil {
		return false
	}
	source := strings.TrimSpace(quota.Source)
	return strings.EqualFold(source, openAIUsageDefaultSource) && isOpenAIChatGPTFreePlan(quota.Plan)
}

func buildChatGPTCodexRequestPayload(decision core.RouteDecision, req *core.GatewayRequest) (any, error) {
	return buildChatGPTCodexResponsesRequest(decision, req), nil
}

func buildChatGPTCodexResponsesRequest(decision core.RouteDecision, req *core.GatewayRequest) openAIResponsesRequest {
	instructions, input := chatGPTCodexInput(req.Messages)
	return openAIResponsesRequest{
		Model:             decision.Model,
		Instructions:      instructions,
		Input:             input,
		Tools:             []any{},
		ToolChoice:        "auto",
		ParallelToolCalls: false,
		Reasoning:         nil,
		Store:             false,
		Stream:            true,
		Include:           []string{},
		PromptCacheKey:    chatGPTCodexPromptCacheKey(req),
		ServiceTier:       strings.TrimSpace(req.ServiceTier),
	}
}

func chatGPTCodexPromptCacheKey(req *core.GatewayRequest) string {
	if req == nil {
		return ""
	}
	if value := strings.TrimSpace(req.PromptCacheKey); value != "" {
		return value
	}
	if metadata := req.Metadata; len(metadata) > 0 {
		if value := strings.TrimSpace(metadata["prompt_cache_key"]); value != "" {
			return value
		}
		if value := strings.TrimSpace(metadata["cache_affinity_key"]); value != "" {
			return value
		}
	}
	return ""
}

func chatGPTCodexInput(messages []core.Message) (string, []openAIResponsesItem) {
	var instructions []string
	input := make([]openAIResponsesItem, 0, len(messages))
	for _, message := range messages {
		content := strings.TrimSpace(message.Content)
		if content == "" {
			continue
		}
		role := strings.TrimSpace(message.Role)
		if role == "" {
			role = "user"
		}
		if role == "system" || role == "developer" {
			instructions = append(instructions, content)
			continue
		}
		contentType := "input_text"
		if role == "assistant" {
			contentType = "output_text"
		}
		input = append(input, openAIResponsesItem{
			Type: "message",
			Role: role,
			Content: []openAIResponsesContent{
				{Type: contentType, Text: content},
			},
		})
	}
	instructionText := strings.Join(instructions, "\n\n")
	if strings.TrimSpace(instructionText) == "" {
		instructionText = chatGPTCodexDefaultInstructions
	}
	return instructionText, input
}

func firstNonEmpty(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func stopPayload(stop []string) any {
	switch len(stop) {
	case 0:
		return nil
	case 1:
		return stop[0]
	default:
		return stop
	}
}

func embeddingInputPayload(input []string) any {
	switch len(input) {
	case 0:
		return []string{}
	case 1:
		return input[0]
	default:
		return input
	}
}

func openAIChatCompletionPayload(model string, req *core.GatewayRequest, stream bool) openAIChatCompletionRequest {
	return openAIChatCompletionPayloadForAccount(model, req, stream, core.Account{})
}

func openAIChatCompletionPayloadForAccount(model string, req *core.GatewayRequest, stream bool, account core.Account) openAIChatCompletionRequest {
	promptCacheKey := ""
	var metadata map[string]string
	var (
		extra               map[string]json.RawMessage
		maxTokens           *int
		maxCompletionTokens *int
		serviceTier         string
		reasoningEffort     string
		temperature         *float64
		topP                *float64
		stop                []string
	)
	if req != nil {
		promptCacheKey = req.PromptCacheKey
		metadata = req.Metadata
		extra = req.Extra
		maxTokens = req.MaxTokens
		maxCompletionTokens = req.MaxCompletionTokens
		serviceTier = req.ServiceTier
		reasoningEffort = req.ReasoningEffort
		temperature = req.Temperature
		topP = req.TopP
		stop = req.Stop
	}
	payload := openAIChatCompletionRequest{
		Model:               model,
		Messages:            openAIMessagePayload(req, model),
		Stop:                stopPayload(stop),
		Stream:              stream,
		Metadata:            openAIUserMetadataForAccount(account, metadata),
		Extra:               extra,
		MaxTokens:           maxTokens,
		MaxCompletionTokens: maxCompletionTokens,
		ServiceTier:         serviceTier,
		ReasoningEffort:     reasoningEffort,
		PromptCacheKey:      openAIStablePromptCacheKeyForAccount(account, promptCacheKey, metadata),
		Temperature:         temperature,
		TopP:                topP,
	}
	if stream {
		payload.StreamOptions = openAIStreamOptionsForRequest(req)
	}
	if openAIUsesReasoningTokenField(model) {
		if payload.MaxCompletionTokens == nil && payload.MaxTokens != nil {
			payload.MaxCompletionTokens = payload.MaxTokens
			payload.MaxTokens = nil
		}
		if strings.HasPrefix(model, "o") {
			payload.Temperature = nil
		}
		if strings.HasPrefix(model, "gpt-5") {
			payload.Temperature = nil
			payload.TopP = nil
			payload.Extra = cloneRawMessageMapWithout(payload.Extra, "logprobs", "top_logprobs")
		}
	}
	return payload
}

func openAIMessagePayload(req *core.GatewayRequest, model string) any {
	if req != nil && len(req.RawMessages) > 0 {
		return openAIAdjustedRawMessages(req.RawMessages, model)
	}
	messages := make([]openAIChatMessage, 0, len(req.Messages))
	for index, message := range req.Messages {
		role := message.Role
		if index == 0 && openAIShouldUseDeveloperRole(model) && strings.TrimSpace(role) == "system" {
			role = "developer"
		}
		messages = append(messages, openAIChatMessage{
			Role:    role,
			Content: message.Content,
		})
	}
	return messages
}

func openAIAdjustedRawMessages(raw json.RawMessage, model string) json.RawMessage {
	if !openAIShouldUseDeveloperRole(model) {
		return raw
	}
	if !bytes.Contains(raw, openAISystemRoleNeedle) {
		return raw
	}
	var messages []map[string]any
	if err := json.Unmarshal(raw, &messages); err != nil || len(messages) == 0 {
		return raw
	}
	if role, _ := messages[0]["role"].(string); strings.TrimSpace(role) == "system" {
		messages[0]["role"] = "developer"
		adjusted, err := json.Marshal(messages)
		if err == nil {
			return adjusted
		}
	}
	return raw
}

func openAIUsesReasoningTokenField(model string) bool {
	return strings.HasPrefix(model, "o") || strings.HasPrefix(model, "gpt-5")
}

func openAIShouldUseDeveloperRole(model string) bool {
	if strings.HasPrefix(model, "o1-mini") || strings.HasPrefix(model, "o1-preview") {
		return false
	}
	return openAIUsesReasoningTokenField(model)
}

func cloneRawMessageMapWithout(in map[string]json.RawMessage, keys ...string) map[string]json.RawMessage {
	if len(in) == 0 {
		return in
	}
	hasSkippedKey := false
	for key := range in {
		if rawMessageKeyIn(key, keys) {
			hasSkippedKey = true
			break
		}
	}
	if !hasSkippedKey {
		return in
	}
	out := make(map[string]json.RawMessage, len(in))
	for key, value := range in {
		if rawMessageKeyIn(key, keys) {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func rawMessageKeyIn(key string, values []string) bool {
	for _, value := range values {
		if key == value {
			return true
		}
	}
	return false
}

func openAIStreamOptionsForRequest(req *core.GatewayRequest) *openAIStreamOptions {
	return &openAIStreamOptions{IncludeUsage: true}
}

type openAIStream struct {
	body     io.Closer
	reader   *sseReader
	response *core.GatewayResponse
	account  core.Account
}

type openAIImageStream struct {
	body    io.Closer
	reader  *sseReader
	account core.Account
}

func (s *openAIImageStream) Next() (*core.StreamEvent, error) {
	for {
		event, err := s.reader.Next()
		if err != nil {
			return nil, err
		}
		if event == nil {
			continue
		}
		trimmedData := bytes.TrimSpace(event.Data)
		if len(trimmedData) == 0 {
			continue
		}
		if bytes.Equal(trimmedData, sseDoneData) {
			return nil, io.EOF
		}
		if err := openAITopLevelErrorBody(s.account, event.Data); err != nil {
			return nil, err
		}
		done := strings.Contains(strings.ToLower(strings.TrimSpace(event.Event)), "completed")
		finishReason := ""
		if done {
			finishReason = "stop"
		}
		return &core.StreamEvent{
			FinishReason: finishReason,
			Done:         done,
			RawEvent:     event.Event,
			RawData:      event.Data,
		}, nil
	}
}

func (s *openAIImageStream) Close() error {
	return closeWithError(s.body, nil)
}

func (s *openAIStream) Next() (*core.StreamEvent, error) {
	for {
		event, err := s.reader.Next()
		if err != nil {
			return nil, err
		}
		if event == nil {
			continue
		}
		trimmedData := bytes.TrimSpace(event.Data)
		if len(trimmedData) == 0 {
			continue
		}
		if bytes.Equal(trimmedData, sseDoneData) {
			return nil, io.EOF
		}
		if err := openAITopLevelErrorBody(s.account, event.Data); err != nil {
			return nil, err
		}

		var chunk openAIChatCompletionChunk
		if err := json.Unmarshal(event.Data, &chunk); err != nil {
			if looksLikeHTMLStreamChunk(event.Data) {
				return nil, &InvokeError{
					Code:      ErrorCodeUpstreamServerError,
					Temporary: true,
					Cooldown:  30 * time.Second,
					Err:       fmt.Errorf("upstream returned HTML challenge response"),
				}
			}
			return nil, &InvokeError{
				Code:      ErrorCodeUpstreamInvalidStreamChunk,
				Temporary: true,
				Cooldown:  10 * time.Second,
				Err:       err,
			}
		}
		if chunk.ID != "" {
			s.response.ID = chunk.ID
		}
		if chunk.Model != "" {
			s.response.Model = chunk.Model
		}
		if serviceTier := strings.TrimSpace(chunk.ServiceTier); serviceTier != "" {
			s.response.ServiceTier = serviceTier
		}
		if chunk.Created > 0 {
			s.response.CreatedAt = time.Unix(chunk.Created, 0).UTC()
		}
		if len(chunk.Choices) == 0 {
			if chunk.Usage != nil {
				usage := usageFromOpenAIChatPayload(*chunk.Usage)
				s.response.Usage = usage
				return &core.StreamEvent{Usage: &usage}, nil
			}
			continue
		}

		choice := chunk.Choices[0]
		firstOutput := len(choice.Delta.ToolCalls) > 0
		if choice.Delta.Content == "" && choice.FinishReason == "" {
			if firstOutput {
				return &core.StreamEvent{FirstOutput: true, RawData: event.Data}, nil
			}
			continue
		}
		return &core.StreamEvent{
			Delta:        choice.Delta.Content,
			FirstOutput:  firstOutput,
			RawData:      choiceRawData(firstOutput, event.Data),
			FinishReason: choice.FinishReason,
			Done:         choice.FinishReason != "",
		}, nil
	}
}

func (s *openAIStream) Close() error {
	return closeWithError(s.body, nil)
}

func looksLikeHTMLStreamChunk(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return false
	}
	lower := strings.ToLower(string(trimmed))
	if bytes.HasPrefix(trimmed, []byte("<")) ||
		strings.Contains(lower, "<!doctype") ||
		strings.Contains(lower, "<html") ||
		strings.Contains(lower, "<head") ||
		strings.Contains(lower, "<body") ||
		strings.Contains(lower, "<title") ||
		strings.Contains(lower, "</html") {
		return true
	}
	return strings.Contains(lower, "cloudflare") &&
		(strings.Contains(lower, "challenge") ||
			strings.Contains(lower, "captcha") ||
			strings.Contains(lower, "cf-ray") ||
			strings.Contains(lower, "cf-chl")) ||
		strings.Contains(lower, "challenge-platform") ||
		strings.Contains(lower, "cf-chl")
}

func choiceRawData(include bool, data []byte) []byte {
	if !include || len(data) == 0 {
		return nil
	}
	return data
}

type openAIResponsesStream struct {
	body         io.Closer
	reader       *sseReader
	response     *core.GatewayResponse
	account      core.Account
	emittedDelta bool
	doneSeen     bool
}

func (s *openAIResponsesStream) Next() (*core.StreamEvent, error) {
	for {
		event, err := s.reader.Next()
		if err != nil {
			if err == io.EOF && !s.doneSeen {
				return nil, &InvokeError{
					Code:      ErrorCodeUpstreamReadError,
					Temporary: true,
					Cooldown:  20 * time.Second,
					Err:       fmt.Errorf("stream closed before response.completed"),
				}
			}
			return nil, err
		}
		if event == nil {
			continue
		}
		trimmedData := bytes.TrimSpace(event.Data)
		if len(trimmedData) == 0 {
			continue
		}
		if bytes.Equal(trimmedData, sseDoneData) {
			if !s.doneSeen {
				return nil, &InvokeError{
					Code:      ErrorCodeUpstreamReadError,
					Temporary: true,
					Cooldown:  20 * time.Second,
					Err:       fmt.Errorf("stream sent [DONE] before response.completed"),
				}
			}
			return nil, io.EOF
		}

		parsed, err := parseOpenAIResponsesStreamPayload(s.account, s.response, &s.emittedDelta, &s.doneSeen, event.Event, event.Data)
		if err != nil {
			return nil, err
		}
		if parsed == nil {
			continue
		}
		return parsed, nil
	}
}

func parseOpenAIResponsesStreamPayload(account core.Account, response *core.GatewayResponse, emittedDelta *bool, doneSeen *bool, eventName string, rawData []byte) (*core.StreamEvent, error) {
	if err := openAITopLevelErrorBody(account, rawData); err != nil {
		return nil, err
	}
	var payload openAIResponsesStreamPayload
	if err := json.Unmarshal(rawData, &payload); err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamInvalidStreamChunk,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}
	if payload.Type == "" {
		payload.Type = eventName
	}
	rawEvent := payload.Type
	if rawEvent == "" {
		rawEvent = eventName
	}

	switch payload.Type {
	case "response.created":
		applyOpenAIResponsesCompleted(response, payload.Response)
		return &core.StreamEvent{RawEvent: rawEvent, RawData: rawData}, nil
	case "response.output_text.delta":
		if payload.Delta == "" {
			return nil, nil
		}
		if emittedDelta != nil {
			*emittedDelta = true
		}
		return &core.StreamEvent{Delta: payload.Delta, FirstOutput: true, RawEvent: rawEvent, RawData: rawData}, nil
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return &core.StreamEvent{FirstOutput: payload.Delta != "", RawEvent: rawEvent, RawData: rawData}, nil
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		return &core.StreamEvent{FirstOutput: payload.Delta != "", RawEvent: rawEvent, RawData: rawData}, nil
	case "response.output_item.added":
		firstOutput := payload.Item != nil && openAIResponsesItemRecordsFirstOutput(*payload.Item)
		return &core.StreamEvent{FirstOutput: firstOutput, RawEvent: rawEvent, RawData: rawData}, nil
	case "response.output_item.done":
		if payload.Item == nil {
			return &core.StreamEvent{RawEvent: rawEvent, RawData: rawData}, nil
		}
		text := outputTextFromResponsesItem(*payload.Item)
		if text == "" || (emittedDelta != nil && *emittedDelta) {
			firstOutput := text == "" && openAIResponsesItemRecordsFirstOutput(*payload.Item)
			return &core.StreamEvent{FirstOutput: firstOutput, RawEvent: rawEvent, RawData: rawData}, nil
		}
		if emittedDelta != nil {
			*emittedDelta = true
		}
		return &core.StreamEvent{Delta: text, FirstOutput: true, RawEvent: rawEvent, RawData: rawData}, nil
	case "response.completed", "response.done":
		if doneSeen != nil {
			*doneSeen = true
		}
		usage := applyOpenAIResponsesCompleted(response, payload.Response)
		return &core.StreamEvent{FinishReason: "stop", Usage: usage, Done: true, RawEvent: rawEvent, RawData: rawData}, nil
	case "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		if doneSeen != nil {
			*doneSeen = true
		}
		usage := applyOpenAIResponsesCompleted(response, payload.Response)
		finishReason := strings.TrimPrefix(payload.Type, "response.")
		if finishReason == "canceled" {
			finishReason = "cancelled"
		}
		return &core.StreamEvent{FinishReason: finishReason, Usage: usage, Done: true, RawEvent: rawEvent, RawData: rawData}, nil
	default:
		return &core.StreamEvent{RawEvent: rawEvent, RawData: rawData}, nil
	}
}

func openAIResponsesItemRecordsFirstOutput(item openAIResponsesItem) bool {
	switch item.Type {
	case "message":
		role := strings.TrimSpace(item.Role)
		if role != "" && role != "assistant" {
			return false
		}
		return outputTextFromResponsesItem(item) != ""
	case "reasoning":
		return openAIResponsesContentHasText(item.Summary) || openAIResponsesContentHasText(item.Content)
	case "local_shell_call",
		"function_call",
		"custom_tool_call",
		"tool_search_call",
		"web_search_call",
		"image_generation_call",
		"compaction",
		"context_compaction":
		return true
	default:
		return false
	}
}

func openAIResponsesContentHasText(contents []openAIResponsesContent) bool {
	for _, content := range contents {
		if content.Text != "" {
			return true
		}
	}
	return false
}

func applyOpenAIResponsesCompleted(response *core.GatewayResponse, resp *openAIResponsesResponseBody) *core.Usage {
	if resp == nil {
		return nil
	}
	if response != nil && resp.ID != "" {
		response.ID = resp.ID
	}
	if response != nil && resp.Model != "" {
		response.Model = resp.Model
	}
	if response != nil {
		if serviceTier := strings.TrimSpace(resp.ServiceTier); serviceTier != "" {
			response.ServiceTier = serviceTier
		}
	}
	usage := usageFromOpenAIResponsesPayload(resp.Usage)
	if response != nil {
		response.Usage = usage
	}
	return &usage
}

func (s *openAIResponsesStream) Close() error {
	return closeWithError(s.body, nil)
}

func outputTextFromResponsesItem(item openAIResponsesItem) string {
	var output strings.Builder
	for _, content := range item.Content {
		if content.Type == "output_text" || content.Type == "text" {
			output.WriteString(content.Text)
		}
	}
	return output.String()
}

func outputTextFromResponsesItems(items []openAIResponsesItem) string {
	var output strings.Builder
	for _, item := range items {
		output.WriteString(outputTextFromResponsesItem(item))
	}
	return output.String()
}

func completedResponseRawBody(rawData []byte) []byte {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rawData, &payload); err != nil {
		return nil
	}
	response := payload["response"]
	if len(response) == 0 {
		return nil
	}
	if !json.Valid(response) {
		return nil
	}
	return response
}

func openAIResponsesRawPayloadFromResponses(model string, req *core.ResponsesRequest) (map[string]any, error) {
	return openAIResponsesRawPayloadFromResponsesForAccount(model, req, core.Account{})
}

func openAIResponsesRawPayloadFromResponsesForAccount(model string, req *core.ResponsesRequest, account core.Account) (map[string]any, error) {
	payload := map[string]any{}
	if req != nil && len(req.RawBody) > 0 {
		if err := json.Unmarshal(req.RawBody, &payload); err != nil {
			return nil, err
		}
	}
	delete(payload, "type")
	delete(payload, "route_affinity_key")
	delete(payload, "cache_affinity_key")
	delete(payload, "route_affinity_model")
	payload["model"] = model
	if req == nil {
		return payload, nil
	}
	delete(payload, "metadata")
	payload["stream"] = req.Stream
	if req.Generate != nil {
		payload["generate"] = *req.Generate
	}
	if req.MaxOutputTokens != nil {
		payload["max_output_tokens"] = *req.MaxOutputTokens
	}
	if serviceTier := strings.TrimSpace(req.ServiceTier); serviceTier != "" {
		payload["service_tier"] = serviceTier
	}
	if promptCacheKey := openAIStablePromptCacheKeyForAccount(account, req.PromptCacheKey, req.Metadata); promptCacheKey != "" {
		payload["prompt_cache_key"] = promptCacheKey
	}
	if metadata := openAIUserMetadataForAccount(account, req.Metadata); len(metadata) > 0 {
		payload["metadata"] = metadata
	}
	if previousResponseID := strings.TrimSpace(req.PreviousResponseID); previousResponseID != "" {
		payload["previous_response_id"] = previousResponseID
	}
	return payload, nil
}

func openAIUserMetadata(metadata map[string]string) map[string]string {
	return openAIUserMetadataForAccount(core.Account{}, metadata)
}

func openAIUserMetadataForAccount(account core.Account, metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		if userID := openAISub2APIMetadataUserID(account, metadata); userID != "" {
			return map[string]string{"user_id": userID}
		}
		return nil
	}
	out := make(map[string]string, len(metadata))
	for key, value := range metadata {
		key = strings.TrimSpace(key)
		if key == "" || openAIInternalMetadataKey(key) {
			continue
		}
		out[key] = value
	}
	if userID := openAISub2APIMetadataUserID(account, metadata); userID != "" {
		out["user_id"] = userID
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func openAIInternalMetadataKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "route_affinity_key",
		"route_affinity_model",
		"cache_affinity_key",
		"prompt_cache_key",
		"anthropic_version",
		"anthropic_beta",
		"x-client-request-id":
		return true
	default:
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(key)), "x-claude-code-")
	}
}

func openAISub2APISessionHeaders(account core.Account, metadata map[string]string) map[string]string {
	if !IsSub2APIGatewayAPIKeyAccount(account) {
		return nil
	}
	sessionID := openAISub2APISessionID(metadata)
	if sessionID == "" {
		return nil
	}
	return map[string]string{
		"session_id":      sessionID,
		"conversation_id": sessionID,
	}
}

func openAISub2APISessionID(metadata map[string]string) string {
	for _, key := range []string{"route_affinity_key", "cache_affinity_key", "prompt_cache_key"} {
		if value := strings.TrimSpace(metadata[key]); value != "" {
			return openAIRouteAffinityValue(value)
		}
	}
	return ""
}

func openAIRouteAffinityValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if idx := strings.Index(value, ":"); idx >= 0 {
		value = strings.TrimSpace(value[idx+1:])
	}
	return value
}

func openAISub2APIMetadataUserID(account core.Account, metadata map[string]string) string {
	if !IsSub2APIGatewayAPIKeyAccount(account) {
		return ""
	}
	sessionID := openAISub2APISessionID(metadata)
	if sessionID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("ai-gateway/sub2api/session\x00" + sessionID))
	deviceID := hex.EncodeToString(sum[:])
	return "user_" + deviceID + "_account__session_" + openAISub2APISessionUUID(sessionID)
}

func openAISub2APISessionUUID(sessionID string) string {
	sum := sha256.Sum256([]byte("ai-gateway/sub2api/session-uuid\x00" + sessionID))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3]),
		uint16(b[4])<<8|uint16(b[5]),
		uint16(b[6])<<8|uint16(b[7]),
		uint16(b[8])<<8|uint16(b[9]),
		uint64(b[10])<<40|uint64(b[11])<<32|uint64(b[12])<<24|uint64(b[13])<<16|uint64(b[14])<<8|uint64(b[15]),
	)
}

func openAIResponsesCompactPayloadFromResponses(model string, req *core.ResponsesRequest) (map[string]any, error) {
	return openAIResponsesCompactPayloadFromResponsesForAccount(model, req, core.Account{})
}

func openAIResponsesCompactPayloadFromResponsesForAccount(model string, req *core.ResponsesRequest, account core.Account) (map[string]any, error) {
	source := map[string]any{}
	if req != nil && len(req.RawBody) > 0 {
		if err := json.Unmarshal(req.RawBody, &source); err != nil {
			return nil, err
		}
	}
	source["model"] = model
	if req != nil {
		if serviceTier := strings.TrimSpace(req.ServiceTier); serviceTier != "" {
			source["service_tier"] = serviceTier
		}
		if promptCacheKey := openAIStablePromptCacheKeyForAccount(account, req.PromptCacheKey, req.Metadata); promptCacheKey != "" {
			source["prompt_cache_key"] = promptCacheKey
		}
	}
	if err := normalizeOpenAIResponsesRawInput(source); err != nil {
		return nil, err
	}

	payload := map[string]any{"model": model}
	copyNonNilJSONField(payload, source, "input", "input")
	if instructions, _ := source["instructions"].(string); strings.TrimSpace(instructions) != "" {
		payload["instructions"] = instructions
	}
	if tools, exists := source["tools"]; exists && tools != nil {
		payload["tools"] = tools
	} else {
		payload["tools"] = []any{}
	}
	if parallelToolCalls, exists := source["parallel_tool_calls"].(bool); exists {
		payload["parallel_tool_calls"] = parallelToolCalls
	} else {
		payload["parallel_tool_calls"] = false
	}
	copyNonNilJSONField(payload, source, "reasoning", "reasoning")
	if serviceTier, _ := source["service_tier"].(string); strings.TrimSpace(serviceTier) != "" {
		payload["service_tier"] = serviceTier
	}
	if promptCacheKey, _ := source["prompt_cache_key"].(string); strings.TrimSpace(promptCacheKey) != "" {
		payload["prompt_cache_key"] = promptCacheKey
	}
	if req != nil {
		if metadata := openAIUserMetadataForAccount(account, req.Metadata); len(metadata) > 0 {
			payload["metadata"] = metadata
		}
	}
	copyNonNilJSONField(payload, source, "text", "text")
	return payload, nil
}

func chatGPTCodexRawResponsesPayloadFromResponses(model string, req *core.ResponsesRequest) (map[string]any, error) {
	payload, err := openAIResponsesRawPayloadFromResponses(model, req)
	if err != nil {
		return nil, err
	}
	if err := normalizeOpenAIResponsesRawInput(payload); err != nil {
		return nil, err
	}
	if _, exists := payload["instructions"]; !exists {
		payload["instructions"] = ""
	}
	payload["model"] = model
	payload["stream"] = true
	payload["store"] = false
	delete(payload, "max_output_tokens")
	delete(payload, "temperature")
	delete(payload, "top_p")
	delete(payload, "metadata")
	return payload, nil
}

func openAIStablePromptCacheKeyForAccount(account core.Account, explicit string, metadata map[string]string) string {
	if value := openAIExplicitPromptCacheKeyForAccount(account, explicit, metadata); value != "" {
		return value
	}
	if !IsSub2APIGatewayAPIKeyAccount(account) {
		return ""
	}
	return openAISub2APIPromptCacheFallbackKey(metadata)
}

func openAIExplicitPromptCacheKeyForAccount(account core.Account, explicit string, metadata map[string]string) string {
	if value := strings.TrimSpace(explicit); value != "" {
		return value
	}
	if !IsSub2APIGatewayAPIKeyAccount(account) {
		return ""
	}
	if metadata != nil {
		if value := strings.TrimSpace(metadata["prompt_cache_key"]); value != "" {
			return value
		}
		if value := strings.TrimSpace(metadata["cache_affinity_key"]); value != "" {
			return value
		}
	}
	return ""
}

func openAISub2APIPromptCacheFallbackKey(metadata map[string]string) string {
	if metadata != nil {
		if value := strings.TrimSpace(metadata["route_affinity_key"]); value != "" {
			return openAIRouteAffinityValue(value)
		}
	}
	return ""
}

func openAIAnthropicBridgeUsesResponses(model string) bool {
	model = strings.TrimSpace(model)
	return strings.HasPrefix(model, "gpt-5")
}

func responsesRequestFromAnthropicGatewayRequest(account core.Account, req *core.GatewayRequest, transport core.ResponsesTransport) (*core.ResponsesRequest, error) {
	if req == nil {
		return nil, nil
	}
	promptCacheKey := openAIStablePromptCacheKeyForAccount(account, req.PromptCacheKey, req.Metadata)
	responsesReq := &core.ResponsesRequest{
		Model:           req.Model,
		Client:          req.Client,
		Transport:       transport,
		Stream:          transport != core.ResponsesTransportHTTP,
		MaxOutputTokens: req.MaxTokens,
		ServiceTier:     strings.TrimSpace(req.ServiceTier),
		PromptCacheKey:  promptCacheKey,
		Metadata:        req.Metadata,
	}
	if req.UpstreamMode == "anthropic_messages" && len(req.RawBody) > 0 {
		rawBody, err := anthropicMessagesRawToOpenAIResponsesRaw(req)
		if err != nil {
			return nil, err
		}
		responsesReq.RawBody = rawBody
		if IsSub2APIGatewayAPIKeyAccount(account) {
			explicitPromptCacheKey := openAIExplicitPromptCacheKeyForAccount(account, req.PromptCacheKey, req.Metadata)
			anthropicPromptCacheKey := anthropicPromptCacheKeyFromRaw(req.RawBody)
			responsesReq.PromptCacheKey = firstNonEmptyString(
				firstNonEmptyString(explicitPromptCacheKey, anthropicPromptCacheKey),
				openAISub2APIPromptCacheFallbackKey(req.Metadata),
			)
		}
		return responsesReq, nil
	}
	payload := openAIResponsesPayloadFromGatewayRequest(req)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	responsesReq.RawBody = json.RawMessage(body)
	return responsesReq, nil
}

func anthropicMessagesRawToOpenAIResponsesRaw(req *core.GatewayRequest) (json.RawMessage, error) {
	var source map[string]any
	if err := json.Unmarshal(req.RawBody, &source); err != nil {
		return nil, err
	}
	out := map[string]any{
		"model":  req.Model,
		"stream": req.Stream,
	}
	copyJSONField(out, source, "temperature", "temperature")
	copyJSONField(out, source, "top_p", "top_p")
	copyJSONField(out, source, "metadata", "metadata")
	copyJSONField(out, source, "stop_sequences", "stop")
	copyJSONField(out, source, "stop", "stop")
	copyJSONField(out, source, "max_tokens", "max_output_tokens")
	if instructions := anthropicSystemToInstructions(source["system"]); strings.TrimSpace(instructions) != "" {
		out["instructions"] = instructions
	}
	if input := anthropicMessagesToOpenAIResponsesInput(source["messages"]); len(input) > 0 {
		out["input"] = input
	}
	if tools := anthropicToolsToOpenAIResponsesTools(source["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	if toolChoice := anthropicToolChoiceToOpenAIResponses(source["tool_choice"]); toolChoice != nil {
		out["tool_choice"] = toolChoice
	}
	if promptCacheKey := anthropicPromptCacheKeyFromSource(source); promptCacheKey != "" {
		out["prompt_cache_key"] = promptCacheKey
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(body), nil
}

func anthropicPromptCacheKeyFromRaw(raw json.RawMessage) string {
	var source map[string]any
	if err := json.Unmarshal(raw, &source); err != nil {
		return ""
	}
	return anthropicPromptCacheKeyFromSource(source)
}

func anthropicPromptCacheKeyFromSource(source map[string]any) string {
	if len(source) == 0 {
		return ""
	}
	var parts []string
	appendCacheText := func(prefix string, value any) {
		text := strings.TrimSpace(anthropicCacheControlText(value))
		if text != "" {
			parts = append(parts, prefix+":"+text)
		}
	}
	appendCacheText("system", source["system"])
	appendCacheText("messages", source["messages"])
	appendCacheText("tools", source["tools"])
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte("ai-gateway/anthropic-cache\x00" + strings.Join(parts, "\n")))
	return "anthropic-cache-" + hex.EncodeToString(sum[:16])
}

func anthropicCacheControlText(value any) string {
	var parts []string
	var walk func(any)
	walk = func(item any) {
		switch typed := item.(type) {
		case []any:
			for _, entry := range typed {
				walk(entry)
			}
		case map[string]any:
			if cacheControlIsEphemeral(typed["cache_control"]) {
				if text, _ := typed["text"].(string); strings.TrimSpace(text) != "" {
					parts = append(parts, strings.TrimSpace(text))
				}
				if name, _ := typed["name"].(string); strings.TrimSpace(name) != "" {
					parts = append(parts, strings.TrimSpace(name))
				}
			}
			for key, nested := range typed {
				if key == "cache_control" {
					continue
				}
				walk(nested)
			}
		}
	}
	walk(value)
	return strings.Join(parts, "\n")
}

func cacheControlIsEphemeral(value any) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	typ, _ := object["type"].(string)
	return strings.TrimSpace(typ) == "ephemeral"
}

func copyJSONField(dst map[string]any, src map[string]any, from, to string) {
	if value, ok := src[from]; ok {
		dst[to] = value
	}
}

func copyNonNilJSONField(dst map[string]any, src map[string]any, from, to string) {
	if value, ok := src[from]; ok && value != nil {
		dst[to] = value
	}
}

func anthropicSystemToInstructions(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if block, ok := item.(map[string]any); ok {
				if text, _ := block["text"].(string); strings.TrimSpace(text) != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n\n")
	default:
		return ""
	}
}

func anthropicMessagesToOpenAIResponsesInput(value any) []any {
	items, _ := value.([]any)
	out := make([]any, 0, len(items))
	for _, item := range items {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		role = strings.TrimSpace(role)
		if role == "" {
			role = "user"
		}
		content := message["content"]
		switch blocks := content.(type) {
		case string:
			if strings.TrimSpace(blocks) != "" {
				out = append(out, openAIResponsesMessageItem(role, []any{openAIResponsesTextContentForRole(role, blocks)}))
			}
		case []any:
			messageContent := make([]any, 0, len(blocks))
			for _, blockValue := range blocks {
				block, ok := blockValue.(map[string]any)
				if !ok {
					continue
				}
				blockType, _ := block["type"].(string)
				switch blockType {
				case "text":
					if text, _ := block["text"].(string); strings.TrimSpace(text) != "" {
						messageContent = append(messageContent, openAIResponsesTextContentForRole(role, text))
					}
				case "image":
					if image := anthropicImageBlockToOpenAIContent(block); image != nil {
						messageContent = append(messageContent, image)
					}
				case "tool_result":
					if toolResult := anthropicToolResultToOpenAIResponsesItem(block); toolResult != nil {
						out = append(out, toolResult)
					}
				case "tool_use":
					if toolCall := anthropicToolUseToOpenAIResponsesItem(block); toolCall != nil {
						out = append(out, toolCall)
					}
				}
			}
			if len(messageContent) > 0 {
				out = append(out, openAIResponsesMessageItem(role, messageContent))
			}
		}
	}
	return out
}

func openAIResponsesMessageItem(role string, content []any) map[string]any {
	if role == "system" || role == "developer" {
		role = "user"
	}
	return map[string]any{"type": "message", "role": role, "content": content}
}

func openAIResponsesTextContentForRole(role, text string) map[string]any {
	contentType := "input_text"
	if role == "assistant" {
		contentType = "output_text"
	}
	return map[string]any{"type": contentType, "text": text}
}

func anthropicImageBlockToOpenAIContent(block map[string]any) map[string]any {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return nil
	}
	sourceType, _ := source["type"].(string)
	mediaType, _ := source["media_type"].(string)
	data, _ := source["data"].(string)
	if sourceType != "base64" || strings.TrimSpace(data) == "" {
		return nil
	}
	if strings.TrimSpace(mediaType) == "" {
		mediaType = "application/octet-stream"
	}
	return map[string]any{"type": "input_image", "image_url": "data:" + mediaType + ";base64," + data}
}

func anthropicToolResultToOpenAIResponsesItem(block map[string]any) map[string]any {
	callID, _ := block["tool_use_id"].(string)
	if strings.TrimSpace(callID) == "" {
		return nil
	}
	return map[string]any{
		"type":    "function_call_output",
		"call_id": callID,
		"output":  anthropicToolResultContentToText(block["content"]),
	}
}

func anthropicToolResultContentToText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if block, ok := item.(map[string]any); ok {
				if text, _ := block["text"].(string); strings.TrimSpace(text) != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		raw, _ := json.Marshal(value)
		return string(raw)
	}
}

func anthropicToolUseToOpenAIResponsesItem(block map[string]any) map[string]any {
	callID, _ := block["id"].(string)
	name, _ := block["name"].(string)
	if strings.TrimSpace(callID) == "" || strings.TrimSpace(name) == "" {
		return nil
	}
	arguments, _ := json.Marshal(block["input"])
	return map[string]any{
		"type":      "function_call",
		"id":        callID,
		"call_id":   callID,
		"name":      name,
		"arguments": string(arguments),
	}
}

func anthropicToolsToOpenAIResponsesTools(value any) []any {
	items, _ := value.([]any)
	out := make([]any, 0, len(items))
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tool["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		converted := map[string]any{"type": "function", "name": name}
		copyJSONField(converted, tool, "description", "description")
		copyJSONField(converted, tool, "input_schema", "parameters")
		out = append(out, converted)
	}
	return out
}

func anthropicToolChoiceToOpenAIResponses(value any) any {
	switch typed := value.(type) {
	case string:
		if typed == "auto" || typed == "none" || typed == "required" {
			return typed
		}
	case map[string]any:
		choiceType, _ := typed["type"].(string)
		switch choiceType {
		case "auto", "none":
			return choiceType
		case "any":
			return "required"
		case "tool":
			if name, _ := typed["name"].(string); strings.TrimSpace(name) != "" {
				return map[string]any{"type": "function", "name": name}
			}
		}
	}
	return nil
}

func normalizeOpenAIResponsesRawInput(payload map[string]any) error {
	if payload == nil {
		return nil
	}
	input, ok := payload["input"]
	if !ok {
		return nil
	}
	switch value := input.(type) {
	case nil:
		return nil
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			delete(payload, "input")
			return nil
		}
		payload["input"] = []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{
						"type": "input_text",
						"text": trimmed,
					},
				},
			},
		}
	case []any:
		if len(value) == 0 {
			delete(payload, "input")
			return nil
		}
	case map[string]any:
		payload["input"] = []any{value}
	default:
		return fmt.Errorf("input must be a string or list")
	}
	return nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func openAIResponsesPayloadFromGatewayRequest(req *core.GatewayRequest) map[string]any {
	payload := map[string]any{
		"stream": req.Stream,
	}
	for key, value := range req.Extra {
		if strings.TrimSpace(key) != "" && len(value) > 0 {
			payload[key] = value
		}
	}
	if req.MaxTokens != nil {
		payload["max_output_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = *req.TopP
	}
	if serviceTier := strings.TrimSpace(req.ServiceTier); serviceTier != "" {
		payload["service_tier"] = serviceTier
	}
	if len(req.Metadata) > 0 {
		payload["metadata"] = req.Metadata
	}
	instructions, input := openAIResponsesInputFromGatewayMessages(req.Messages)
	if strings.TrimSpace(instructions) != "" {
		payload["instructions"] = instructions
	}
	if len(input) > 0 {
		payload["input"] = input
	}
	return payload
}

func openAIResponsesInputFromGatewayMessages(messages []core.Message) (string, []openAIResponsesItem) {
	var instructions []string
	input := make([]openAIResponsesItem, 0, len(messages))
	for _, message := range messages {
		content := strings.TrimSpace(message.Content)
		if content == "" {
			continue
		}
		role := strings.TrimSpace(message.Role)
		if role == "" {
			role = "user"
		}
		if role == "system" || role == "developer" {
			instructions = append(instructions, content)
			continue
		}
		contentType := "input_text"
		if role == "assistant" {
			contentType = "output_text"
		}
		input = append(input, openAIResponsesItem{
			Type: "message",
			Role: role,
			Content: []openAIResponsesContent{
				{Type: contentType, Text: content},
			},
		})
	}
	return strings.Join(instructions, "\n\n"), input
}
