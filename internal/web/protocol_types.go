package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

type openAICompletionRequest struct {
	Model               string                      `json:"model"`
	Messages            []openAIMessage             `json:"messages"`
	Stream              bool                        `json:"stream"`
	StreamOptions       *openAIStreamOptionsRequest `json:"stream_options,omitempty"`
	MaxTokens           *int                        `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                        `json:"max_completion_tokens,omitempty"`
	ServiceTier         string                      `json:"service_tier,omitempty"`
	ReasoningEffort     string                      `json:"reasoning_effort,omitempty"`
	PromptCacheKey      string                      `json:"prompt_cache_key,omitempty"`
	Temperature         *float64                    `json:"temperature,omitempty"`
	TopP                *float64                    `json:"top_p,omitempty"`
	Stop                any                         `json:"stop,omitempty"`
	Metadata            map[string]string           `json:"metadata,omitempty"`
}

type openAIStreamOptionsRequest struct {
	IncludeUsage *bool `json:"include_usage,omitempty"`
}

func parseOpenAICompletionRequest(body []byte) (openAICompletionRequest, map[string]json.RawMessage, error) {
	raw, err := parseRawObject(body)
	if err != nil {
		return openAICompletionRequest{}, nil, err
	}
	var req openAICompletionRequest
	if err := decodeRawField(raw, "model", &req.Model); err != nil {
		return openAICompletionRequest{}, nil, err
	}
	if err := decodeRawField(raw, "messages", &req.Messages); err != nil {
		return openAICompletionRequest{}, nil, err
	}
	if err := decodeRawField(raw, "stream", &req.Stream); err != nil {
		return openAICompletionRequest{}, nil, err
	}
	if err := decodeRawField(raw, "stream_options", &req.StreamOptions); err != nil {
		return openAICompletionRequest{}, nil, err
	}
	if err := decodeRawField(raw, "max_tokens", &req.MaxTokens); err != nil {
		return openAICompletionRequest{}, nil, err
	}
	if err := decodeRawField(raw, "max_completion_tokens", &req.MaxCompletionTokens); err != nil {
		return openAICompletionRequest{}, nil, err
	}
	if err := decodeRawField(raw, "service_tier", &req.ServiceTier); err != nil {
		return openAICompletionRequest{}, nil, err
	}
	if err := decodeRawField(raw, "reasoning_effort", &req.ReasoningEffort); err != nil {
		return openAICompletionRequest{}, nil, err
	}
	if err := decodeRawField(raw, "prompt_cache_key", &req.PromptCacheKey); err != nil {
		return openAICompletionRequest{}, nil, err
	}
	if err := decodeRawField(raw, "temperature", &req.Temperature); err != nil {
		return openAICompletionRequest{}, nil, err
	}
	if err := decodeRawField(raw, "top_p", &req.TopP); err != nil {
		return openAICompletionRequest{}, nil, err
	}
	if err := decodeRawField(raw, "stop", &req.Stop); err != nil {
		return openAICompletionRequest{}, nil, err
	}
	if err := decodeRawField(raw, "metadata", &req.Metadata); err != nil {
		return openAICompletionRequest{}, nil, err
	}
	return req, raw, nil
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type openAIChoice struct {
	Index        int           `json:"index"`
	Message      openAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type openAICompletionResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   core.Usage     `json:"usage"`
}

type openAIStreamChunkResponse struct {
	ID      string                `json:"id"`
	Object  string                `json:"object"`
	Created int64                 `json:"created"`
	Model   string                `json:"model"`
	Choices [1]openAIStreamChoice `json:"choices"`
}

type openAIStreamUsageResponse struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Created int64                `json:"created"`
	Model   string               `json:"model"`
	Choices []openAIStreamChoice `json:"choices"`
	Usage   *core.Usage          `json:"usage,omitempty"`
}

type openAIStreamChoice struct {
	Index        int               `json:"index"`
	Delta        openAIStreamDelta `json:"delta"`
	FinishReason string            `json:"finish_reason,omitempty"`
}

type openAIStreamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

var emptyOpenAIStreamChoices = []openAIStreamChoice{}

func openAIStreamRoleChunk(resp *core.GatewayResponse) openAIStreamChunkResponse {
	return openAIStreamChunk(resp, openAIStreamDelta{Role: "assistant"}, "")
}

func openAIStreamDeltaChunk(resp *core.GatewayResponse, delta string) openAIStreamChunkResponse {
	return openAIStreamChunk(resp, openAIStreamDelta{Content: delta}, "")
}

func openAIStreamFinishChunk(resp *core.GatewayResponse, finishReason string) openAIStreamChunkResponse {
	return openAIStreamChunk(resp, openAIStreamDelta{}, finishReason)
}

func openAIStreamChunk(resp *core.GatewayResponse, delta openAIStreamDelta, finishReason string) openAIStreamChunkResponse {
	chunk := openAIStreamChunkResponse{
		Object: "chat.completion.chunk",
		Choices: [1]openAIStreamChoice{
			{
				Index:        0,
				Delta:        delta,
				FinishReason: finishReason,
			},
		},
	}
	if resp != nil {
		chunk.ID = resp.ID
		chunk.Created = resp.CreatedAt.Unix()
		chunk.Model = resp.Model
	}
	return chunk
}

func openAIStreamUsageChunk(resp *core.GatewayResponse, usage *core.Usage) openAIStreamUsageResponse {
	chunk := openAIStreamUsageResponse{
		Object:  "chat.completion.chunk",
		Choices: emptyOpenAIStreamChoices,
		Usage:   usage,
	}
	if resp != nil {
		chunk.ID = resp.ID
		chunk.Created = resp.CreatedAt.Unix()
		chunk.Model = resp.Model
	}
	return chunk
}

type openAIResponseRequest struct {
	Model              string            `json:"model"`
	Input              any               `json:"input"`
	Instructions       string            `json:"instructions,omitempty"`
	Stream             bool              `json:"stream"`
	Generate           *bool             `json:"generate,omitempty"`
	MaxOutputTokens    *int              `json:"max_output_tokens,omitempty"`
	ServiceTier        string            `json:"service_tier,omitempty"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
	PromptCacheKey     string            `json:"prompt_cache_key,omitempty"`
	Temperature        *float64          `json:"temperature,omitempty"`
	TopP               *float64          `json:"top_p,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
}

func parseOpenAIResponseRequest(body []byte) (openAIResponseRequest, map[string]json.RawMessage, error) {
	raw, err := parseRawObject(body)
	if err != nil {
		return openAIResponseRequest{}, nil, err
	}
	var req openAIResponseRequest
	if err := decodeRawField(raw, "model", &req.Model); err != nil {
		return openAIResponseRequest{}, nil, err
	}
	if err := decodeRawField(raw, "input", &req.Input); err != nil {
		return openAIResponseRequest{}, nil, err
	}
	if err := decodeRawField(raw, "instructions", &req.Instructions); err != nil {
		return openAIResponseRequest{}, nil, err
	}
	if err := decodeRawField(raw, "stream", &req.Stream); err != nil {
		return openAIResponseRequest{}, nil, err
	}
	if err := decodeRawField(raw, "generate", &req.Generate); err != nil {
		return openAIResponseRequest{}, nil, err
	}
	if err := decodeRawField(raw, "max_output_tokens", &req.MaxOutputTokens); err != nil {
		return openAIResponseRequest{}, nil, err
	}
	if err := decodeRawField(raw, "service_tier", &req.ServiceTier); err != nil {
		return openAIResponseRequest{}, nil, err
	}
	if err := decodeRawField(raw, "previous_response_id", &req.PreviousResponseID); err != nil {
		return openAIResponseRequest{}, nil, err
	}
	if err := decodeRawField(raw, "prompt_cache_key", &req.PromptCacheKey); err != nil {
		return openAIResponseRequest{}, nil, err
	}
	if err := decodeRawField(raw, "temperature", &req.Temperature); err != nil {
		return openAIResponseRequest{}, nil, err
	}
	if err := decodeRawField(raw, "top_p", &req.TopP); err != nil {
		return openAIResponseRequest{}, nil, err
	}
	if err := decodeRawField(raw, "metadata", &req.Metadata); err != nil {
		return openAIResponseRequest{}, nil, err
	}
	return req, raw, nil
}

type openAIEmbeddingRequest struct {
	Model          string            `json:"model"`
	Input          any               `json:"input"`
	EncodingFormat string            `json:"encoding_format,omitempty"`
	Dimensions     *int              `json:"dimensions,omitempty"`
	User           string            `json:"user,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

type openAIEmbeddingResponse struct {
	Object string                 `json:"object"`
	Data   []core.EmbeddingObject `json:"data"`
	Model  string                 `json:"model"`
	Usage  core.Usage             `json:"usage"`
}

type openAIModerationRequest struct {
	Model string `json:"model"`
	Input any    `json:"input"`
}

type openAIImageGenerationRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

func parseOpenAIImageGenerationRequest(body []byte) (openAIImageGenerationRequest, map[string]json.RawMessage, error) {
	raw, err := parseRawObject(body)
	if err != nil {
		return openAIImageGenerationRequest{}, nil, err
	}
	var req openAIImageGenerationRequest
	if err := decodeRawField(raw, "model", &req.Model); err != nil {
		return openAIImageGenerationRequest{}, nil, err
	}
	if err := decodeRawField(raw, "prompt", &req.Prompt); err != nil {
		return openAIImageGenerationRequest{}, nil, err
	}
	return req, raw, nil
}

type openAIAudioSpeechRequest struct {
	Model string          `json:"model"`
	Input string          `json:"input"`
	Voice json.RawMessage `json:"voice"`
}

func parseOpenAIAudioSpeechRequest(body []byte) (openAIAudioSpeechRequest, map[string]json.RawMessage, error) {
	raw, err := parseRawObject(body)
	if err != nil {
		return openAIAudioSpeechRequest{}, nil, err
	}
	var req openAIAudioSpeechRequest
	if err := decodeRawField(raw, "model", &req.Model); err != nil {
		return openAIAudioSpeechRequest{}, nil, err
	}
	if err := decodeRawField(raw, "input", &req.Input); err != nil {
		return openAIAudioSpeechRequest{}, nil, err
	}
	if err := decodeRawField(raw, "voice", &req.Voice); err != nil {
		return openAIAudioSpeechRequest{}, nil, err
	}
	return req, raw, nil
}

func openAIAudioVoicePresent(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text) != ""
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err == nil {
		return len(object) > 0
	}
	return true
}

func openAIAudioVoiceString(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	return ""
}

func parseRawObject(body []byte) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	return raw, nil
}

func decodeRawField(raw map[string]json.RawMessage, name string, dst any) error {
	value := raw[name]
	if len(value) == 0 {
		return nil
	}
	if err := json.Unmarshal(value, dst); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type anthropicMessagesRequest struct {
	Model         string             `json:"model"`
	System        any                `json:"system"`
	Messages      []anthropicMessage `json:"messages"`
	Stream        bool               `json:"stream"`
	MaxTokens     *int               `json:"max_tokens,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	Stop          any                `json:"stop,omitempty"`
	StopSequences any                `json:"stop_sequences,omitempty"`
	Metadata      map[string]string  `json:"metadata,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

type anthropicResponse struct {
	ID         string             `json:"id"`
	Type       string             `json:"type"`
	Role       string             `json:"role"`
	Model      string             `json:"model"`
	Content    []anthropicContent `json:"content"`
	StopReason string             `json:"stop_reason"`
	Usage      anthropicUsage     `json:"usage"`
}

func normalizeStops(values ...any) []string {
	var out []string

	appendStop := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range out {
			if existing == value {
				return
			}
		}
		if out == nil {
			out = make([]string, 0, 4)
		}
		out = append(out, value)
	}

	for _, value := range values {
		switch item := value.(type) {
		case string:
			appendStop(item)
		case []string:
			for _, entry := range item {
				appendStop(entry)
			}
		case []any:
			for _, entry := range item {
				if text, ok := entry.(string); ok {
					appendStop(text)
				}
			}
		}
	}

	return out
}

func normalizeEmbeddingInput(input any) ([]string, error) {
	switch value := input.(type) {
	case string:
		return []string{value}, nil
	case []string:
		if len(value) == 0 {
			return nil, fmt.Errorf("input is required")
		}
		return append([]string(nil), value...), nil
	case []any:
		if len(value) == 0 {
			return nil, fmt.Errorf("input is required")
		}
		if embeddingTokenArray(value) {
			return nil, nil
		}
		out := make([]string, 0, len(value))
		for _, item := range value {
			text, ok := item.(string)
			if !ok {
				if embeddingTokenInputItem(item) {
					return nil, nil
				}
				return nil, fmt.Errorf("input must be a string, token array, array of strings, or array of token arrays")
			}
			out = append(out, text)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("input must be a string, token array, array of strings, or array of token arrays")
	}
}

func embeddingTokenInputItem(value any) bool {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return false
	}
	return embeddingTokenArray(items)
}

func embeddingTokenArray(items []any) bool {
	for _, item := range items {
		if _, ok := item.(float64); !ok {
			return false
		}
	}
	return true
}

func openAICompletionExtra(raw map[string]json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var extra map[string]json.RawMessage
	for key, value := range raw {
		if openAICompletionKnownField(key) || len(value) == 0 {
			continue
		}
		if extra == nil {
			extra = make(map[string]json.RawMessage, len(raw))
		}
		extra[key] = append(json.RawMessage(nil), value...)
	}
	return extra
}

func openAICompletionKnownField(key string) bool {
	switch key {
	case "model",
		"messages",
		"stream",
		"service_tier",
		"max_tokens",
		"max_completion_tokens",
		"reasoning_effort",
		"prompt_cache_key",
		"temperature",
		"top_p",
		"stop",
		"metadata",
		"stream_options",
		"route_affinity_key",
		"cache_affinity_key",
		"route_affinity_model":
		return true
	default:
		return false
	}
}

func rawStringField(raw map[string]json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	value := raw[key]
	if len(value) == 0 {
		return ""
	}
	var out string
	if err := json.Unmarshal(value, &out); err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (s *Server) openAIPromptCacheKey(raw map[string]json.RawMessage, model string, client *core.APIClient, sessionAffinityKey string) string {
	if value := rawStringField(raw, "prompt_cache_key"); value != "" {
		return value
	}
	if value := rawStringField(raw, "cache_affinity_key"); value != "" {
		return value
	}
	if value := s.openAIPromptCacheKeyFromPreviousResponse(raw, client); value != "" {
		return value
	}
	if value := s.deriveOpenAIPromptCacheKey(model, client, sessionAffinityKey); value != "" {
		return value
	}
	return ""
}

func (s *Server) openAIPromptCacheKeyFromPreviousResponse(raw map[string]json.RawMessage, client *core.APIClient) string {
	if s == nil || s.control == nil {
		return ""
	}
	previousResponseID := rawStringField(raw, "previous_response_id")
	if previousResponseID == "" {
		return ""
	}
	binding, err := s.control.GetOpenAIResponseBinding(previousResponseID)
	if err != nil {
		return ""
	}
	if strings.TrimSpace(binding.ClientID) != "" {
		if client == nil || !strings.EqualFold(strings.TrimSpace(client.ID), strings.TrimSpace(binding.ClientID)) {
			return ""
		}
	}
	if value := strings.TrimSpace(binding.PromptCacheKey); value != "" {
		return value
	}
	return s.deriveOpenAIPromptCacheKey("", client, "previous_response_id:"+previousResponseID)
}

func (s *Server) deriveOpenAIPromptCacheKey(model string, client *core.APIClient, sessionAffinityKey string) string {
	sessionAffinityKey = strings.TrimSpace(sessionAffinityKey)
	if sessionAffinityKey == "" {
		return ""
	}
	clientID := ""
	if client != nil {
		clientID = strings.TrimSpace(client.ID)
	}
	parts := []string{
		"ai-gateway/openai-prompt-cache/v1",
		clientID,
		strings.TrimSpace(model),
		sessionAffinityKey,
	}
	secret := strings.TrimSpace(s.masterKey)
	if secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strings.Join(parts, "\x00")))
	return "agpc_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:18])
}

func applyOpenAICacheAffinityMetadata(metadata map[string]string, raw map[string]json.RawMessage, model string, sessionAffinityKey string) map[string]string {
	cacheKey := rawStringField(raw, "prompt_cache_key")
	hasExplicitPromptCacheKey := cacheKey != ""
	if cacheKey == "" {
		cacheKey = rawStringField(raw, "cache_affinity_key")
	}
	hasExplicitCacheAffinityKey := cacheKey != ""
	if cacheKey == "" {
		cacheKey = rawStringField(raw, "route_affinity_key")
	}
	hasExplicitRouteAffinityKey := cacheKey != ""
	sessionAffinityKey = strings.TrimSpace(sessionAffinityKey)
	if cacheKey == "" {
		cacheKey = sessionAffinityKey
	}
	if cacheKey == "" {
		return metadata
	}
	if metadata == nil {
		metadata = map[string]string{}
	}
	if hasExplicitPromptCacheKey || hasExplicitCacheAffinityKey {
		metadata["prompt_cache_key"] = cacheKey
		metadata["cache_affinity_key"] = cacheKey
	} else if hasExplicitRouteAffinityKey || sessionAffinityKey != "" {
		metadata["route_affinity_key"] = cacheKey
	}
	if model = strings.TrimSpace(model); model != "" {
		metadata["route_affinity_model"] = model
	}
	return metadata
}

func routeAffinityMetadataForModel(model string, sessionAffinityKey string) map[string]string {
	sessionAffinityKey = strings.TrimSpace(sessionAffinityKey)
	if sessionAffinityKey == "" {
		return nil
	}
	metadata := map[string]string{"route_affinity_key": sessionAffinityKey}
	if model = strings.TrimSpace(model); model != "" {
		metadata["route_affinity_model"] = model
	}
	return metadata
}

func openAIRequestSessionAffinityKey(r *http.Request) string {
	if r == nil {
		return ""
	}
	headers := map[string]string{}
	for _, key := range openAISessionAffinityHeaderKeys() {
		if value := strings.TrimSpace(r.Header.Get(key)); value != "" {
			headers[key] = value
		}
	}
	return openAIHeadersSessionAffinityKey(headers)
}

func openAIHeadersSessionAffinityKey(headers map[string]string) string {
	for _, key := range openAISessionAffinityHeaderKeys() {
		if value := strings.TrimSpace(headers[key]); value != "" {
			return key + ":" + value
		}
	}
	return ""
}

func openAISessionAffinityHeaderKeys() []string {
	return []string{
		"session-id",
		"session_id",
		"thread-id",
		"conversation_id",
		"x-codex-parent-thread-id",
	}
}

func openAIVisibleMetadata(metadata map[string]string) map[string]any {
	if len(metadata) == 0 {
		return map[string]any{}
	}
	out := map[string]any{}
	for key, value := range metadata {
		key = strings.TrimSpace(key)
		if key == "" || openAIInternalMetadataKey(key) {
			continue
		}
		out[key] = value
	}
	return out
}

func openAIInternalMetadataKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "route_affinity_key",
		"route_affinity_model",
		"cache_affinity_key",
		"prompt_cache_key":
		return true
	default:
		return false
	}
}

func openAIStreamIncludeUsage(options *openAIStreamOptionsRequest) *bool {
	if options == nil || options.IncludeUsage == nil {
		return nil
	}
	value := *options.IncludeUsage
	return &value
}

func openAIShouldSendStreamUsage(req *core.GatewayRequest) bool {
	if req != nil && req.StreamIncludeUsage != nil {
		return *req.StreamIncludeUsage
	}
	return true
}

func writeOpenAIRawCompletion(w http.ResponseWriter, status int, resp *core.GatewayResponse, req *core.GatewayRequest) {
	_ = req
	if rawBodyHasProtocolError(resp.RawBody) {
		writeProtocolGatewayError(w, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(resp.RawBody)
}

func writeOpenAIRawResponse(w http.ResponseWriter, status int, resp *core.GatewayResponse, req *core.ResponsesRequest) {
	_ = req
	if rawBodyHasProtocolError(resp.RawBody) {
		writeProtocolGatewayError(w, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(resp.RawBody)
}

func writeAnthropicRawResponse(w http.ResponseWriter, status int, resp *core.GatewayResponse) {
	if rawBodyHasProtocolError(resp.RawBody) {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    "api_error",
				"message": gatewayProtocolErrorMessage,
			},
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(resp.RawBody)
}

func rawBodyHasProtocolError(body []byte) bool {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	if _, ok := payload["error"]; !ok {
		return false
	}
	if object := strings.TrimSpace(stringField(payload, "object")); object == "response" || object == "response.compaction" {
		return false
	}
	return true
}

func openAIResponseEnvelope(resp *core.GatewayResponse, req *core.ResponsesRequest) map[string]any {
	outputText := ""
	if resp != nil {
		outputText = resp.Content
	}
	responseID := ""
	createdAt := time.Now()
	model := ""
	usage := core.Usage{}
	metadata := map[string]any{}
	if req != nil {
		metadata = openAIVisibleMetadata(req.Metadata)
	}
	if resp != nil {
		responseID = resp.ID
		createdAt = resp.CreatedAt
		model = resp.Model
		usage = resp.Usage
	}
	status := "completed"
	if resp != nil {
		status = openAIResponsesTerminalStatus(resp.FinishReason)
	}
	return map[string]any{
		"id":          responseID,
		"object":      "response",
		"created_at":  createdAt.Unix(),
		"status":      status,
		"model":       model,
		"output":      openAIResponseOutput(responseID, outputText),
		"output_text": outputText,
		"usage": map[string]any{
			"input_tokens":                usage.PromptTokens,
			"output_tokens":               usage.CompletionTokens,
			"total_tokens":                usage.TotalTokens,
			"cache_creation_input_tokens": usage.CacheCreationTokens,
			"input_tokens_details":        map[string]any{"cached_tokens": usage.CachedPromptTokens},
			"output_tokens_details":       map[string]any{"image_tokens": usage.ImageOutputTokens},
		},
		"metadata": metadata,
	}
}

func openAIResponsesTerminalEventName(finishReason string) string {
	switch strings.ToLower(strings.TrimSpace(finishReason)) {
	case "failed":
		return "response.failed"
	case "incomplete":
		return "response.incomplete"
	case "cancelled", "canceled":
		return "response.cancelled"
	default:
		return "response.completed"
	}
}

func openAIResponsesTerminalStatus(finishReason string) string {
	switch strings.ToLower(strings.TrimSpace(finishReason)) {
	case "failed":
		return "failed"
	case "incomplete":
		return "incomplete"
	case "cancelled", "canceled":
		return "cancelled"
	default:
		return "completed"
	}
}

func openAIResponsesFailureFinishReason(finishReason string) bool {
	switch strings.ToLower(strings.TrimSpace(finishReason)) {
	case "failed", "incomplete", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func openAIResponseStreamPayloadWithStatus(responseID, model string, createdAt time.Time, status string, outputText string, usage *core.Usage) map[string]any {
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	status = strings.TrimSpace(status)
	if status == "" {
		status = "completed"
	}
	tokens := core.Usage{}
	if usage != nil {
		tokens = *usage
	}
	return map[string]any{
		"id":          responseID,
		"object":      "response",
		"created_at":  createdAt.Unix(),
		"status":      status,
		"model":       model,
		"output":      openAIResponseOutput(responseID, outputText),
		"output_text": outputText,
		"usage": map[string]any{
			"input_tokens":                tokens.PromptTokens,
			"output_tokens":               tokens.CompletionTokens,
			"total_tokens":                tokens.TotalTokens,
			"cache_creation_input_tokens": tokens.CacheCreationTokens,
			"input_tokens_details":        map[string]any{"cached_tokens": tokens.CachedPromptTokens},
			"output_tokens_details":       map[string]any{"image_tokens": tokens.ImageOutputTokens},
		},
	}
}

func anthropicUsageFromCore(usage core.Usage) anthropicUsage {
	inputTokens := usage.PromptTokens - usage.CachedPromptTokens
	if inputTokens < 0 {
		inputTokens = 0
	}
	return anthropicUsage{
		InputTokens:              inputTokens,
		OutputTokens:             usage.CompletionTokens,
		CacheReadInputTokens:     usage.CachedPromptTokens,
		CacheCreationInputTokens: usage.CacheCreationTokens,
	}
}

func anthropicUsageMapFromCore(usage core.Usage) map[string]any {
	out := map[string]any{
		"input_tokens":  anthropicUsageFromCore(usage).InputTokens,
		"output_tokens": usage.CompletionTokens,
	}
	if usage.CachedPromptTokens > 0 {
		out["cache_read_input_tokens"] = usage.CachedPromptTokens
	}
	if usage.CacheCreationTokens > 0 {
		out["cache_creation_input_tokens"] = usage.CacheCreationTokens
	}
	return out
}

func openAIResponseOutput(responseID, text string) []map[string]any {
	return []map[string]any{
		{
			"id":   responseID + "_msg",
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{
				{
					"type":        "output_text",
					"text":        text,
					"annotations": []any{},
				},
			},
		},
	}
}

func anthropicResponseFromOpenAIResponsesRaw(resp *core.GatewayResponse) (map[string]any, bool) {
	var raw map[string]any
	if err := json.Unmarshal(resp.RawBody, &raw); err != nil {
		return nil, false
	}
	if object := stringField(raw, "object"); object != "" && object != "response" {
		return nil, false
	}
	content := make([]map[string]any, 0, 2)
	stopReason := anthropicStopReason(resp.FinishReason)
	for _, itemValue := range rawArray(raw["output"]) {
		item, ok := itemValue.(map[string]any)
		if !ok {
			continue
		}
		itemType := stringField(item, "type")
		switch itemType {
		case "message":
			for _, contentValue := range rawArray(item["content"]) {
				block, ok := contentValue.(map[string]any)
				if !ok {
					continue
				}
				if text := stringField(block, "text"); text != "" {
					content = append(content, map[string]any{"type": "text", "text": text})
				}
			}
		case "function_call":
			if toolUse := anthropicToolUseFromOpenAIResponseFunctionCall(item); toolUse != nil {
				content = append(content, toolUse)
				stopReason = "tool_use"
			}
		}
	}
	if len(content) == 0 && strings.TrimSpace(resp.Content) != "" {
		content = append(content, map[string]any{"type": "text", "text": resp.Content})
	}
	usage := rawMap(raw["usage"])
	return map[string]any{
		"id":          resp.ID,
		"type":        "message",
		"role":        "assistant",
		"model":       resp.Model,
		"content":     content,
		"stop_reason": stopReason,
		"usage": map[string]any{
			"input_tokens":  intField(usage, "input_tokens"),
			"output_tokens": intField(usage, "output_tokens"),
		},
	}, true
}

func anthropicToolUseFromOpenAIResponseFunctionCall(item map[string]any) map[string]any {
	callID := firstNonEmptyString(stringField(item, "call_id"), stringField(item, "id"))
	name := stringField(item, "name")
	if callID == "" || name == "" {
		return nil
	}
	input := any(map[string]any{})
	if arguments := stringField(item, "arguments"); arguments != "" {
		var decoded any
		if err := json.Unmarshal([]byte(arguments), &decoded); err == nil {
			input = decoded
		} else {
			input = map[string]any{"arguments": arguments}
		}
	}
	return map[string]any{"type": "tool_use", "id": callID, "name": name, "input": input}
}

func rawMap(value any) map[string]any {
	out, _ := value.(map[string]any)
	return out
}

func rawArray(value any) []any {
	out, _ := value.([]any)
	return out
}

func stringField(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	text, _ := values[key].(string)
	return strings.TrimSpace(text)
}

func intField(values map[string]any, key string) int {
	if values == nil {
		return 0
	}
	switch value := values[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case int64:
		return int(value)
	case json.Number:
		parsed, err := value.Int64()
		if err == nil {
			return int(parsed)
		}
	}
	return 0
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
