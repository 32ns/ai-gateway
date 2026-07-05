package web

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func (s *Server) handleOpenAICompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	bodyBytes, err := s.readProtocolBody(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req, raw, err := parseOpenAICompletionRequest(bodyBytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	protocolClient := protocolClientPointerFromContext(r.Context())
	sessionAffinityKey := openAIRequestSessionAffinityKey(r)
	metadata := applyOpenAICacheAffinityMetadata(req.Metadata, raw, req.Model, sessionAffinityKey)
	promptCacheKey := s.openAIPromptCacheKey(raw, req.Model, protocolClient, sessionAffinityKey)
	var streamIncludeUsage *bool
	if req.Stream {
		streamIncludeUsage = openAIStreamIncludeUsage(req.StreamOptions)
	}
	gatewayReq := &core.GatewayRequest{
		Model:               req.Model,
		Messages:            make([]core.Message, 0, len(req.Messages)),
		RawMessages:         raw["messages"],
		RawBody:             json.RawMessage(bodyBytes),
		Client:              protocolClient,
		ClientIP:            clientIP(r),
		Stream:              req.Stream,
		StreamIncludeUsage:  streamIncludeUsage,
		MaxTokens:           req.MaxTokens,
		MaxCompletionTokens: req.MaxCompletionTokens,
		ServiceTier:         strings.TrimSpace(req.ServiceTier),
		ReasoningEffort:     strings.TrimSpace(req.ReasoningEffort),
		PromptCacheKey:      promptCacheKey,
		Temperature:         req.Temperature,
		TopP:                req.TopP,
		Metadata:            metadata,
		Stop:                normalizeStops(req.Stop),
		Extra:               openAICompletionExtra(raw),
	}
	for _, message := range req.Messages {
		gatewayReq.Messages = append(gatewayReq.Messages, core.Message{Role: message.Role, Content: flattenOpenAIMessageContent(message.Content)})
	}

	if req.Stream {
		if err := s.streamOpenAICompletions(r.Context(), w, gatewayReq); err != nil {
			s.writeGatewayError(w, err)
		}
		return
	}

	resp, err := s.gateway.Execute(r.Context(), gatewayReq)
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	if openAIResponsesFailureFinishReason(resp.FinishReason) {
		writeProtocolGatewayError(w, http.StatusBadGateway)
		return
	}
	if len(resp.RawBody) > 0 {
		writeOpenAIRawCompletion(w, http.StatusOK, resp, gatewayReq)
		return
	}

	writeJSON(w, http.StatusOK, openAICompletionResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: resp.CreatedAt.Unix(),
		Model:   resp.Model,
		Choices: []openAIChoice{
			{
				Index: 0,
				Message: openAIMessage{
					Role:    "assistant",
					Content: resp.Content,
				},
				FinishReason: resp.FinishReason,
			},
		},
		Usage: resp.Usage,
	})
}

func (s *Server) handleOpenAIResponses(w http.ResponseWriter, r *http.Request) {
	if openAIResponsesWebSocketRequested(r) {
		s.handleOpenAIResponsesWebSocket(w, r)
		return
	}
	s.handleOpenAIResponsesRequest(w, r, false)
}

func (s *Server) handleOpenAIResponsesCompact(w http.ResponseWriter, r *http.Request) {
	s.handleOpenAIResponsesRequest(w, r, true)
}

func (s *Server) handleOpenAIResponsesRequest(w http.ResponseWriter, r *http.Request, compact bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	bodyBytes, err := s.readProtocolBody(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req, raw, err := parseOpenAIResponseRequest(bodyBytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeProtocolError(w, http.StatusBadRequest, "model is required")
		return
	}
	if compact && req.Stream {
		writeProtocolError(w, http.StatusBadRequest, "stream is not supported for responses compaction")
		return
	}

	protocolClient := protocolClientPointerFromContext(r.Context())
	sessionAffinityKey := openAIRequestSessionAffinityKey(r)
	metadata := applyOpenAICacheAffinityMetadata(req.Metadata, raw, req.Model, sessionAffinityKey)
	promptCacheKey := s.openAIPromptCacheKey(raw, req.Model, protocolClient, sessionAffinityKey)
	responsesReq := &core.ResponsesRequest{
		Model:              req.Model,
		RawBody:            json.RawMessage(bodyBytes),
		Client:             protocolClient,
		ClientIP:           clientIP(r),
		Transport:          core.ResponsesTransportHTTP,
		Stream:             req.Stream,
		Compact:            compact,
		Generate:           req.Generate,
		MaxOutputTokens:    req.MaxOutputTokens,
		ServiceTier:        strings.TrimSpace(req.ServiceTier),
		PreviousResponseID: strings.TrimSpace(req.PreviousResponseID),
		PromptCacheKey:     promptCacheKey,
		Metadata:           metadata,
		Headers:            openAIResponsesHeadersFromRequest(r),
	}

	if req.Stream {
		responsesReq.Transport = core.ResponsesTransportSSE
		if err := s.streamOpenAIResponses(r.Context(), w, responsesReq); err != nil {
			s.writeGatewayError(w, err)
		}
		return
	}

	resp, err := s.gateway.ExecuteResponses(r.Context(), responsesReq)
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	if openAIResponsesFailureFinishReason(resp.FinishReason) {
		writeProtocolGatewayError(w, http.StatusBadGateway)
		return
	}
	if len(resp.RawBody) > 0 && resp.Provider == core.ProviderOpenAI {
		writeOpenAIRawResponse(w, http.StatusOK, resp, responsesReq)
		return
	}
	writeJSON(w, http.StatusOK, openAIResponseEnvelope(resp, responsesReq))
}

func (s *Server) handleOpenAIResponseResource(w http.ResponseWriter, r *http.Request) {
	if !openAIResponseResourceMethodAllowed(r) {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/responses/") || strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/responses/"), "/") == "" {
		writeProtocolError(w, http.StatusNotFound, "response not found")
		return
	}
	var body []byte
	if r.Method == http.MethodPost {
		var err error
		body, err = s.readProtocolBody(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	account, err := s.openAIProxyAccountForResponse(protocolClientPointerFromContext(r.Context()), openAIResponseResourceID(r.URL.Path))
	if err != nil {
		writeProtocolNotFound(w, err, "response not found")
		return
	}
	resp, err := (&providers.OpenAIAdapter{}).ProxyResponseResource(
		r.Context(),
		account,
		r.Method,
		r.URL.EscapedPath(),
		r.URL.RawQuery,
		body,
		r.Header.Get("Content-Type"),
	)
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	writeRawProxyResponse(w, resp, "application/json")
}

func (s *Server) handleOpenAIResponseInputTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := s.readProtocolBody(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	account, err := s.openAIProxyAccount(protocolClientPointerFromContext(r.Context()))
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	resp, err := (&providers.OpenAIAdapter{}).ProxyResponseResource(
		r.Context(),
		account,
		r.Method,
		r.URL.EscapedPath(),
		r.URL.RawQuery,
		body,
		r.Header.Get("Content-Type"),
	)
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	writeRawProxyResponse(w, resp, "application/json")
}

func writeRawProxyResponse(w http.ResponseWriter, resp *providers.RawProxyResponse, fallbackContentType string) {
	if resp == nil {
		writeProtocolGatewayError(w, http.StatusBadGateway)
		return
	}
	if resp.StatusCode >= http.StatusBadRequest {
		writeProtocolGatewayError(w, http.StatusBadGateway)
		return
	}
	contentType := safeRawProxyContentType(resp.ContentType, fallbackContentType)
	if contentTypeIsJSON(contentType) && rawBodyHasProtocolError(resp.Body) {
		writeProtocolGatewayError(w, http.StatusBadGateway)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(resp.Body)
}

func writeProtocolGatewayError(w http.ResponseWriter, status int) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"type":    gatewayProtocolErrorType,
			"message": gatewayProtocolErrorMessage,
		},
	})
}

func safeRawProxyContentType(contentType, fallbackContentType string) string {
	fallbackContentType = strings.TrimSpace(fallbackContentType)
	if fallbackContentType == "" {
		fallbackContentType = "application/octet-stream"
	}
	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		return fallbackContentType
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return fallbackContentType
	}
	switch strings.ToLower(mediaType) {
	case "application/json", "application/problem+json", "text/event-stream":
		return contentType
	default:
		return fallbackContentType
	}
}

func openAIResponseResourceMethodAllowed(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodDelete:
		return true
	case http.MethodPost:
		return strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/cancel")
	default:
		return false
	}
}

func openAIResponseResourceID(path string) string {
	suffix := strings.Trim(strings.TrimPrefix(path, "/v1/responses/"), "/")
	if suffix == "" {
		return ""
	}
	responseID, _, _ := strings.Cut(suffix, "/")
	return strings.TrimSpace(responseID)
}

func (s *Server) openAIProxyAccountForResponse(client *core.APIClient, responseID string) (core.Account, error) {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" || s == nil || s.control == nil {
		return core.Account{}, storage.ErrNotFound
	}
	binding, err := s.control.GetOpenAIResponseBinding(responseID)
	if err != nil {
		return core.Account{}, err
	}
	if strings.TrimSpace(binding.ClientID) != "" {
		if client == nil || !strings.EqualFold(strings.TrimSpace(client.ID), strings.TrimSpace(binding.ClientID)) {
			return core.Account{}, storage.ErrNotFound
		}
	}
	account, err := s.control.GetAccount(binding.AccountID)
	if err != nil {
		return core.Account{}, err
	}
	if account.Provider != core.ProviderOpenAI {
		return core.Account{}, storage.ErrNotFound
	}
	if !s.openAIProxyBoundAccountAccessible(client, account) {
		return core.Account{}, storage.ErrNotFound
	}
	account.EffectiveProxyURL = s.effectiveAccountProxyURL(account)
	return account, nil
}

func (s *Server) openAIProxyBoundAccountAccessible(client *core.APIClient, account core.Account) bool {
	return s.openAIProxyClientCanUseAccount(client, account)
}

func (s *Server) openAIProxyAccount(client *core.APIClient) (core.Account, error) {
	var selected *core.Account
	now := time.Now().UTC()
	for _, account := range s.control.ListAccounts() {
		if account.Provider != core.ProviderOpenAI {
			continue
		}
		if openAIProxyAccountUnavailable(account, now) {
			continue
		}
		if !s.openAIProxyClientCanUseAccount(client, account) {
			continue
		}
		candidate := account
		candidate.EffectiveProxyURL = s.effectiveAccountProxyURL(candidate)
		if selected == nil || openAIProxyAccountLess(candidate, *selected) {
			selected = &candidate
		}
	}
	if selected == nil {
		return core.Account{}, fmt.Errorf("no available openai account for responses resource proxy")
	}
	return *selected, nil
}

func (s *Server) openAIProxyClientCanUseAccount(client *core.APIClient, account core.Account) bool {
	if client == nil {
		return true
	}
	clientGroup := normalizeAccountGroupForWeb(client.AccountGroup)
	accountGroup := normalizeAccountGroupForWeb(account.Group)
	return strings.EqualFold(accountGroup, clientGroup)
}

func openAIProxyAccountUnavailable(account core.Account, now time.Time) bool {
	return !core.AccountAvailableForRouting(account, now)
}

func openAIProxyAccountLess(a, b core.Account) bool {
	if a.Backup != b.Backup {
		return !a.Backup
	}
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	if a.ConsecutiveFails != b.ConsecutiveFails {
		return a.ConsecutiveFails < b.ConsecutiveFails
	}
	if a.LastUsedAt == nil && b.LastUsedAt != nil {
		return true
	}
	if a.LastUsedAt != nil && b.LastUsedAt == nil {
		return false
	}
	if a.LastUsedAt != nil && b.LastUsedAt != nil {
		if diff := a.LastUsedAt.Compare(*b.LastUsedAt); diff != 0 {
			return diff < 0
		}
	}
	if a.Weight != b.Weight {
		return a.Weight > b.Weight
	}
	return a.ID < b.ID
}

func (s *Server) effectiveAccountProxyURL(account core.Account) string {
	if proxyURL := strings.TrimSpace(account.ProxyURL); proxyURL != "" {
		return proxyURL
	}
	groupName := strings.TrimSpace(account.Group)
	if groupName != "" {
		for _, group := range s.control.ListAccountGroups() {
			if strings.EqualFold(strings.TrimSpace(group.Name), groupName) {
				if proxyURL := strings.TrimSpace(group.ProxyURL); proxyURL != "" {
					return proxyURL
				}
				break
			}
		}
	}
	return strings.TrimSpace(s.control.SystemProxyURL())
}

func (s *Server) handleOpenAIEmbeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bodyBytes, err := s.readProtocolBody(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	raw, err := parseRawObject(bodyBytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req openAIEmbeddingRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	input, err := normalizeEmbeddingInput(req.Input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	encodingFormat := strings.TrimSpace(req.EncodingFormat)
	if encodingFormat == "" {
		encodingFormat = "float"
	}
	if encodingFormat != "float" && encodingFormat != "base64" {
		http.Error(w, `encoding_format must be "float" or "base64"`, http.StatusBadRequest)
		return
	}

	protocolClient := protocolClientPointerFromContext(r.Context())
	gatewayReq := &core.EmbeddingRequest{
		Model:          req.Model,
		Input:          input,
		RawInput:       raw["input"],
		RawBody:        json.RawMessage(bodyBytes),
		EncodingFormat: encodingFormat,
		Dimensions:     req.Dimensions,
		User:           strings.TrimSpace(req.User),
		Client:         protocolClient,
		ClientIP:       clientIP(r),
		Metadata:       req.Metadata,
	}

	resp, err := s.gateway.Embed(r.Context(), gatewayReq)
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	if len(resp.RawBody) > 0 {
		if rawBodyHasProtocolError(resp.RawBody) {
			writeProtocolGatewayError(w, http.StatusBadGateway)
			return
		}
		writeOpenAIRawJSONBody(w, http.StatusOK, resp.RawBody)
		return
	}

	writeJSON(w, http.StatusOK, openAIEmbeddingResponse{
		Object: "list",
		Data:   resp.Data,
		Model:  resp.Model,
		Usage:  resp.Usage,
	})
}

func (s *Server) handleOpenAIModerations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	bodyBytes, err := s.readProtocolBody(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req openAIModerationRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		req.Model = defaultOpenAIModerationModel
	}
	if req.Input == nil {
		writeProtocolError(w, http.StatusBadRequest, "input is required")
		return
	}

	protocolClient := protocolClientPointerFromContext(r.Context())
	resp, err := s.gateway.Moderate(r.Context(), &core.ModerationRequest{
		Model:    req.Model,
		Input:    req.Input,
		Client:   protocolClient,
		ClientIP: clientIP(r),
		RawBody:  json.RawMessage(bodyBytes),
	})
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	payload := map[string]any{}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		writeProtocolGatewayError(w, http.StatusBadGateway)
		return
	}
	if rawPayloadHasProtocolError(payload) {
		writeProtocolGatewayError(w, http.StatusBadGateway)
		return
	}
	payload["model"] = resp.Model
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleOpenAIImageGenerations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	bodyBytes, err := s.readProtocolBody(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req, raw, err := parseOpenAIImageGenerationRequest(bodyBytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		req.Model = defaultOpenAIImageGenerationModel(raw)
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeProtocolError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	if message := validateOpenAIImageGenerationRequest(req.Model, raw); message != "" {
		writeProtocolError(w, http.StatusBadRequest, message)
		return
	}
	if rawJSONBool(raw, "stream") {
		protocolClient := protocolClientPointerFromContext(r.Context())
		metadata := applyOpenAICacheAffinityMetadata(nil, raw, req.Model, openAIRequestSessionAffinityKey(r))
		if err := s.streamOpenAIImageGeneration(r.Context(), w, &core.ImageGenerationRequest{
			Model:    req.Model,
			Prompt:   req.Prompt,
			Client:   protocolClient,
			ClientIP: clientIP(r),
			Metadata: metadata,
			Extra:    raw,
			RawBody:  json.RawMessage(bodyBytes),
		}); err != nil {
			s.writeGatewayError(w, err)
		}
		return
	}

	protocolClient := protocolClientPointerFromContext(r.Context())
	metadata := applyOpenAICacheAffinityMetadata(nil, raw, req.Model, openAIRequestSessionAffinityKey(r))
	resp, err := s.gateway.GenerateImage(r.Context(), &core.ImageGenerationRequest{
		Model:    req.Model,
		Prompt:   req.Prompt,
		Client:   protocolClient,
		ClientIP: clientIP(r),
		Metadata: metadata,
		Extra:    raw,
		RawBody:  json.RawMessage(bodyBytes),
	})
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	payload := map[string]any{}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		writeProtocolGatewayError(w, http.StatusBadGateway)
		return
	}
	if rawPayloadHasProtocolError(payload) {
		writeProtocolGatewayError(w, http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleOpenAIAudioSpeech(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	bodyBytes, err := s.readProtocolBody(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req, raw, err := parseOpenAIAudioSpeechRequest(bodyBytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeProtocolError(w, http.StatusBadRequest, "model is required")
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		writeProtocolError(w, http.StatusBadRequest, "input is required")
		return
	}
	if !openAIAudioVoicePresent(req.Voice) {
		writeProtocolError(w, http.StatusBadRequest, "voice is required")
		return
	}

	protocolClient := protocolClientPointerFromContext(r.Context())
	resp, err := s.gateway.CreateSpeech(r.Context(), &core.AudioSpeechRequest{
		Model:    req.Model,
		Input:    req.Input,
		Voice:    openAIAudioVoiceString(req.Voice),
		Client:   protocolClient,
		ClientIP: clientIP(r),
		Extra:    raw,
		RawBody:  json.RawMessage(bodyBytes),
	})
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	contentType := strings.TrimSpace(resp.ContentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if contentTypeIsJSON(contentType) && rawBodyHasProtocolError(resp.Body) {
		writeProtocolGatewayError(w, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp.Body)
}

func (s *Server) handleOpenAIAudioMultipart(endpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		bodyBytes, err := s.readProtocolBody(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
		fields, err := multipartFormFieldValues(bodyBytes, contentType, "model")
		if err != nil {
			writeProtocolError(w, http.StatusBadRequest, err.Error())
			return
		}
		model := fields["model"]
		if strings.TrimSpace(model) == "" {
			writeProtocolError(w, http.StatusBadRequest, "model is required")
			return
		}

		protocolClient := protocolClientPointerFromContext(r.Context())
		resp, err := s.gateway.ProcessAudioMultipart(r.Context(), &core.AudioMultipartRequest{
			Model:       model,
			Endpoint:    endpoint,
			ContentType: contentType,
			Body:        bodyBytes,
			Client:      protocolClient,
			ClientIP:    clientIP(r),
			FormFields:  fields,
		})
		if err != nil {
			s.writeGatewayError(w, err)
			return
		}
		respContentType := strings.TrimSpace(resp.ContentType)
		if respContentType == "" {
			respContentType = "application/json"
		}
		if contentTypeIsJSON(respContentType) && rawBodyHasProtocolError(resp.Body) {
			writeProtocolGatewayError(w, http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", respContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(resp.Body)
	}
}

func (s *Server) handleOpenAIImageMultipart(endpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		bodyBytes, err := s.readProtocolBody(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
		fields, err := multipartFormFieldValues(bodyBytes, contentType, "model", "background", "input_fidelity", "stream", "n")
		if err != nil {
			writeProtocolError(w, http.StatusBadRequest, err.Error())
			return
		}
		model := fields["model"]
		if strings.TrimSpace(model) == "" {
			model = defaultOpenAIImageMultipartModel(endpoint)
		}
		if message := validateOpenAIImageMultipartRequest(model, fields); message != "" {
			writeProtocolError(w, http.StatusBadRequest, message)
			return
		}
		if multipartFieldBool(fields, "stream") {
			protocolClient := protocolClientPointerFromContext(r.Context())
			metadata := routeAffinityMetadataForModel(model, openAIRequestSessionAffinityKey(r))
			if err := s.streamOpenAIImageMultipart(r.Context(), w, &core.ImageMultipartRequest{
				Model:       model,
				Endpoint:    endpoint,
				ContentType: contentType,
				Body:        bodyBytes,
				Client:      protocolClient,
				ClientIP:    clientIP(r),
				Metadata:    metadata,
				FormFields:  fields,
			}); err != nil {
				s.writeGatewayError(w, err)
			}
			return
		}

		protocolClient := protocolClientPointerFromContext(r.Context())
		metadata := routeAffinityMetadataForModel(model, openAIRequestSessionAffinityKey(r))
		resp, err := s.gateway.ProcessImageMultipart(r.Context(), &core.ImageMultipartRequest{
			Model:       model,
			Endpoint:    endpoint,
			ContentType: contentType,
			Body:        bodyBytes,
			Client:      protocolClient,
			ClientIP:    clientIP(r),
			Metadata:    metadata,
			FormFields:  fields,
		})
		if err != nil {
			s.writeGatewayError(w, err)
			return
		}
		respContentType := strings.TrimSpace(resp.ContentType)
		if respContentType == "" {
			respContentType = "application/json"
		}
		if contentTypeIsJSON(respContentType) && rawBodyHasProtocolError(resp.Body) {
			writeProtocolGatewayError(w, http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", respContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(resp.Body)
	}
}

const (
	defaultOpenAIModerationModel    = "omni-moderation-latest"
	defaultOpenAIImageGenerationGPT = "gpt-image-2"
	defaultOpenAIImageEditModel     = defaultOpenAIImageGenerationGPT
)

func defaultOpenAIImageGenerationModel(raw map[string]json.RawMessage) string {
	return defaultOpenAIImageGenerationGPT
}

func defaultOpenAIImageMultipartModel(endpoint string) string {
	switch endpoint {
	case "/v1/images/edits":
		return defaultOpenAIImageEditModel
	default:
		return defaultOpenAIImageGenerationGPT
	}
}

func validateOpenAIImageGenerationRequest(model string, raw map[string]json.RawMessage) string {
	if isGPTImage2Model(model) && strings.EqualFold(rawJSONString(raw, "background"), "transparent") {
		return strings.TrimSpace(model) + " does not support transparent background"
	}
	return ""
}

func validateOpenAIImageMultipartRequest(model string, fields map[string]string) string {
	if isGPTImage2Model(model) {
		if strings.EqualFold(fields["background"], "transparent") {
			return strings.TrimSpace(model) + " does not support transparent background"
		}
		if _, ok := fields["input_fidelity"]; ok {
			return strings.TrimSpace(model) + " does not support input_fidelity"
		}
	}
	return ""
}

func isGPTImage2Model(model string) bool {
	normalized := strings.ToLower(strings.TrimSpace(model))
	return normalized == defaultOpenAIImageGenerationGPT || strings.HasPrefix(normalized, defaultOpenAIImageGenerationGPT+"-")
}

func rawJSONBool(raw map[string]json.RawMessage, key string) bool {
	if len(raw) == 0 {
		return false
	}
	value := raw[key]
	if len(value) == 0 {
		return false
	}
	var flag bool
	if err := json.Unmarshal(value, &flag); err != nil {
		return false
	}
	return flag
}

func multipartFieldBool(fields map[string]string, key string) bool {
	value := strings.ToLower(strings.TrimSpace(fields[key]))
	return value == "true" || value == "1"
}

func rawJSONString(raw map[string]json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	value := raw[key]
	if len(value) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return ""
	}
	return strings.TrimSpace(text)
}

func (s *Server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	bodyBytes, err := s.readProtocolBody(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req struct {
		Model    string             `json:"model"`
		Messages []anthropicMessage `json:"messages"`
	}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeAnthropicError(w, http.StatusBadRequest, "model is required")
		return
	}
	if len(req.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "messages is required")
		return
	}
	protocolClient := protocolClientPointerFromContext(r.Context())
	resp, err := s.gateway.CountTokens(r.Context(), &core.TokenCountRequest{
		Model:    req.Model,
		Client:   protocolClient,
		ClientIP: clientIP(r),
		Metadata: anthropicProtocolMetadataForClient(r, nil, protocolClient),
		RawBody:  json.RawMessage(bodyBytes),
	})
	if err != nil {
		s.writeAnthropicGatewayError(w, err)
		return
	}
	if rawBodyHasProtocolError(resp.Body) {
		s.writeAnthropicGatewayError(w, fmt.Errorf("%s", gatewayProtocolErrorMessage))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp.Body)
}

func writeAnthropicError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    "invalid_request_error",
			"message": message,
		},
	})
}

func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	bodyBytes, err := s.readProtocolBody(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req anthropicMessagesRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeAnthropicError(w, http.StatusBadRequest, "model is required")
		return
	}
	if req.MaxTokens == nil || *req.MaxTokens <= 0 {
		defaultMaxTokens := 1024
		req.MaxTokens = &defaultMaxTokens
	}
	if len(req.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "messages is required")
		return
	}

	protocolClient := protocolClientPointerFromContext(r.Context())
	gatewayReq := &core.GatewayRequest{
		Model:        req.Model,
		Messages:     make([]core.Message, 0, len(req.Messages)+1),
		RawBody:      json.RawMessage(bodyBytes),
		Client:       protocolClient,
		ClientIP:     clientIP(r),
		UpstreamMode: "anthropic_messages",
		Stream:       req.Stream,
		MaxTokens:    req.MaxTokens,
		Temperature:  req.Temperature,
		TopP:         req.TopP,
		Metadata:     anthropicProtocolMetadataForClient(r, req.Metadata, protocolClient),
		Stop:         normalizeStops(req.StopSequences, req.Stop),
	}
	if system := flattenAnthropicContent(req.System); strings.TrimSpace(system) != "" {
		gatewayReq.Messages = append(gatewayReq.Messages, core.Message{Role: "system", Content: system})
	}
	for _, message := range req.Messages {
		gatewayReq.Messages = append(gatewayReq.Messages, core.Message{Role: message.Role, Content: flattenAnthropicContent(message.Content)})
	}

	if req.Stream {
		if err := s.streamAnthropicMessages(r.Context(), w, gatewayReq); err != nil {
			s.writeAnthropicGatewayError(w, err)
		}
		return
	}

	resp, err := s.gateway.Execute(r.Context(), gatewayReq)
	if err != nil {
		s.writeAnthropicGatewayError(w, err)
		return
	}
	if openAIResponsesFailureFinishReason(resp.FinishReason) {
		s.writeAnthropicGatewayError(w, fmt.Errorf("%s", gatewayProtocolErrorMessage))
		return
	}
	if len(resp.RawBody) > 0 && resp.Provider == core.ProviderClaude {
		writeAnthropicRawResponse(w, http.StatusOK, resp)
		return
	}
	if len(resp.RawBody) > 0 && resp.Provider == core.ProviderOpenAI {
		if payload, ok := anthropicResponseFromOpenAIResponsesRaw(resp); ok {
			writeJSON(w, http.StatusOK, payload)
			return
		}
	}

	writeJSON(w, http.StatusOK, anthropicResponse{
		ID:    resp.ID,
		Type:  "message",
		Role:  "assistant",
		Model: resp.Model,
		Content: []anthropicContent{
			{Type: "text", Text: resp.Content},
		},
		StopReason: anthropicStopReason(resp.FinishReason),
		Usage:      anthropicUsageFromCore(resp.Usage),
	})
}

func writeOpenAIRawJSONBody(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func rawPayloadHasProtocolError(payload map[string]any) bool {
	if len(payload) == 0 {
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

func contentTypeIsJSON(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		mediaType = strings.TrimSpace(contentType)
	}
	mediaType = strings.ToLower(mediaType)
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}
