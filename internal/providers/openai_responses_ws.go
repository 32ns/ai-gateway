package providers

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/netproxy"
	"github.com/gorilla/websocket"
)

const (
	openAIResponsesWebSocketBeta  = "responses_websockets=2026-02-06"
	openAIResponsesWSReadLimit    = 16 << 20
	openAIResponsesWSDialTimeout  = 30 * time.Second
	openAIResponsesWSWriteTimeout = 30 * time.Second
	openAIResponsesCodexUserAgent = "codex_cli_rs/0.125.0 (Ubuntu 22.4.0; x86_64) xterm-256color"
)

var openAIResponsesPassthroughHeaderKeys = []string{
	"accept-language",
	"session-id",
	"thread-id",
	"x-client-request-id",
	"x-codex-installation-id",
	"x-codex-beta-features",
	"x-codex-parent-thread-id",
	"x-codex-turn-state",
	"x-codex-turn-metadata",
	"x-codex-window-id",
	"x-oai-attestation",
	"x-openai-internal-codex-responses-lite",
	"x-openai-memgen-request",
	"x-openai-subagent",
	"x-responsesapi-include-timing-metrics",
	"session_id",
	"conversation_id",
}

func (a *OpenAIAdapter) openOpenAIResponsesWebSocketStream(ctx context.Context, decision core.RouteDecision, req *core.ResponsesRequest) (*StreamSession, error) {
	return a.openResponsesWebSocketStream(ctx, decision, req, false)
}

func (a *OpenAIAdapter) openChatGPTCodexResponsesWebSocketStream(ctx context.Context, decision core.RouteDecision, req *core.ResponsesRequest) (*StreamSession, error) {
	return a.openResponsesWebSocketStream(ctx, decision, req, true)
}

func (a *OpenAIAdapter) openResponsesWebSocketStream(ctx context.Context, decision core.RouteDecision, req *core.ResponsesRequest, codex bool) (*StreamSession, error) {
	session, err := a.openResponsesWebSocketSession(ctx, decision, req, codex)
	if err != nil {
		return nil, err
	}
	streamSession, err := session.SendRequest(ctx, decision.Model, req)
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	streamSession.Stream = &singleUseResponsesWebSocketStream{
		Stream:  streamSession.Stream,
		session: session,
	}
	return streamSession, nil
}

func (a *OpenAIAdapter) OpenResponsesWebSocketSession(ctx context.Context, decision core.RouteDecision, req *core.ResponsesRequest) (ResponsesWebSocketSession, error) {
	return a.openResponsesWebSocketSession(ctx, decision, req, usesChatGPTCodexBackend(decision.Account))
}

func (a *OpenAIAdapter) openResponsesWebSocketSession(ctx context.Context, decision core.RouteDecision, req *core.ResponsesRequest, codex bool) (*openAIResponsesWebSocketSession, error) {
	ctx = WithUpstreamAccount(ctx, decision.Account)
	accessToken, err := accessTokenForAccount(decision.Account)
	if err != nil {
		return nil, err
	}
	endpoint := resolveEndpoint(decision.Account, openAIBaseURL, "/v1/responses")
	if codex {
		endpoint = resolveChatGPTCodexEndpoint(decision.Account, "/responses")
	}
	wsURL, err := websocketURL(endpoint)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}
	headers := openAIResponsesWebSocketHeaders(decision.Account, accessToken, req, codex)
	dialer, err := responsesWebSocketDialer(decision.Account.EffectiveProxyURL)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamTransportError,
			Temporary: true,
			Cooldown:  20 * time.Second,
			Err:       err,
		}
	}

	dialCtx, cancel := context.WithTimeout(ctx, openAIResponsesWSDialTimeout)
	defer cancel()
	conn, resp, err := dialer.DialContext(dialCtx, wsURL, headers)
	if err != nil {
		return nil, mapResponsesWebSocketDialError(ctx, err, resp)
	}
	conn.SetReadLimit(openAIResponsesWSReadLimit)

	return &openAIResponsesWebSocketSession{
		ctx:      ctx,
		conn:     conn,
		account:  decision.Account,
		provider: decision.Provider,
		codex:    codex,
	}, nil
}

type openAIResponsesWebSocketSession struct {
	ctx      context.Context
	conn     *websocket.Conn
	account  core.Account
	provider core.ProviderKind
	codex    bool
	writeMu  sync.Mutex
	closeMu  sync.Mutex
	closed   bool
}

func (s *openAIResponsesWebSocketSession) SendRequest(ctx context.Context, model string, req *core.ResponsesRequest) (*StreamSession, error) {
	if s == nil || s.conn == nil {
		return nil, io.EOF
	}
	payload, err := openAIResponsesWebSocketCreatePayload(model, req, s.account, s.codex)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamRequestBuildFailed,
			Temporary: true,
			Cooldown:  10 * time.Second,
			Err:       err,
		}
	}
	if err := s.writeText(ctx, body); err != nil {
		return nil, err
	}

	provider := s.provider
	if provider == "" {
		provider = core.ProviderOpenAI
	}
	meta := &core.GatewayResponse{
		ID:           fmt.Sprintf("resp_openai_ws_%d", time.Now().UnixNano()),
		Model:        model,
		Provider:     provider,
		AccountID:    s.account.ID,
		AccountLabel: s.account.Label,
		FinishReason: "stop",
		CreatedAt:    time.Now().UTC(),
	}
	stream := &openAIResponsesWebSocketStream{
		session:  s,
		response: meta,
		ctx:      ctx,
	}
	return &StreamSession{
		Response: meta,
		Stream:   stream,
	}, nil
}

func (s *openAIResponsesWebSocketSession) SendRaw(ctx context.Context, payload []byte) error {
	if len(strings.TrimSpace(string(payload))) == 0 {
		return nil
	}
	return s.writeText(ctx, payload)
}

func (s *openAIResponsesWebSocketSession) writeText(ctx context.Context, payload []byte) error {
	if s == nil || s.conn == nil {
		return io.EOF
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.isClosed() {
		return io.ErrClosedPipe
	}
	if openAIResponsesWSWriteTimeout > 0 {
		_ = s.conn.SetWriteDeadline(time.Now().Add(openAIResponsesWSWriteTimeout))
	}
	writeErr := s.conn.WriteMessage(websocket.TextMessage, payload)
	_ = s.conn.SetWriteDeadline(time.Time{})
	if writeErr != nil {
		_ = s.Close()
		return &InvokeError{
			Code:      ErrorCodeUpstreamTransportError,
			Temporary: true,
			Cooldown:  20 * time.Second,
			Err:       fmt.Errorf("write websocket message: %w", writeErr),
		}
	}
	return nil
}

func (s *openAIResponsesWebSocketSession) isClosed() bool {
	if s == nil {
		return true
	}
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	return s.closed
}

func (s *openAIResponsesWebSocketSession) Close() error {
	if s == nil || s.conn == nil {
		return nil
	}
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.conn.Close()
}

type openAIResponsesWebSocketStream struct {
	ctx          context.Context
	session      *openAIResponsesWebSocketSession
	response     *core.GatewayResponse
	emittedDelta bool
	doneSeen     bool
	terminalSent bool
}

func (s *openAIResponsesWebSocketStream) Next() (*core.StreamEvent, error) {
	if s == nil || s.session == nil || s.session.conn == nil {
		return nil, io.EOF
	}
	if s.terminalSent {
		return nil, io.EOF
	}
	for {
		messageType, data, err := s.session.conn.ReadMessage()
		if err != nil {
			if s.ctx != nil && s.ctx.Err() != nil {
				return nil, s.ctx.Err()
			}
			if !s.doneSeen {
				return nil, &InvokeError{
					Code:      ErrorCodeUpstreamReadError,
					Temporary: true,
					Cooldown:  20 * time.Second,
					Err:       err,
				}
			}
			return nil, io.EOF
		}
		if messageType != websocket.TextMessage {
			continue
		}
		trimmed := strings.TrimSpace(string(data))
		if trimmed == "" {
			continue
		}
		if trimmed == "[DONE]" {
			if !s.doneSeen {
				return nil, &InvokeError{
					Code:      ErrorCodeUpstreamReadError,
					Temporary: true,
					Cooldown:  20 * time.Second,
					Err:       fmt.Errorf("websocket stream ended before response.completed"),
				}
			}
			return nil, io.EOF
		}
		event, err := parseOpenAIResponsesStreamPayload(s.session.account, s.response, &s.emittedDelta, &s.doneSeen, "", data)
		if err != nil {
			return nil, err
		}
		if event == nil {
			continue
		}
		if event.Done {
			s.terminalSent = true
		}
		return event, nil
	}
}

func (s *openAIResponsesWebSocketStream) Close() error {
	return nil
}

type singleUseResponsesWebSocketStream struct {
	Stream
	session ResponsesWebSocketSession
}

func (s *singleUseResponsesWebSocketStream) Close() error {
	if s == nil {
		return nil
	}
	streamErr := error(nil)
	if s.Stream != nil {
		streamErr = s.Stream.Close()
	}
	if s.session == nil {
		return streamErr
	}
	return closeWithError(s.session, streamErr)
}

func openAIResponsesWebSocketCreatePayload(model string, req *core.ResponsesRequest, account core.Account, codex bool) (map[string]any, error) {
	var (
		payload map[string]any
		err     error
	)
	if codex {
		payload, err = chatGPTCodexRawResponsesPayloadFromResponses(model, req)
	} else {
		payload, err = openAIResponsesRawPayloadFromResponsesForAccount(model, req, account)
	}
	if err != nil {
		return nil, err
	}
	payload["type"] = "response.create"
	payload["model"] = model
	payload["stream"] = true
	return payload, nil
}

func openAIResponsesWebSocketHeaders(account core.Account, accessToken string, req *core.ResponsesRequest, codex bool) http.Header {
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+accessToken)
	headers.Set("OpenAI-Beta", openAIResponsesWebSocketBeta)
	for key, value := range openAIResponsesRequestHeadersForAccount(account, req, codex) {
		headers.Set(key, value)
	}
	return headers
}

func openAIResponsesRequestHeaders(account core.Account, req *core.ResponsesRequest) map[string]string {
	return openAIResponsesRequestHeadersForAccount(account, req, false)
}

func chatGPTCodexResponsesRequestHeaders(account core.Account, req *core.ResponsesRequest) map[string]string {
	return openAIResponsesRequestHeadersForAccount(account, req, true)
}

func openAIResponsesRequestHeadersForAccount(account core.Account, req *core.ResponsesRequest, codex bool) map[string]string {
	headers := cloneStringHeaders(openAISub2APISessionHeaders(account, requestMetadata(req)))
	if codex {
		for key, value := range chatGPTCodexHeaders(account) {
			if strings.TrimSpace(value) != "" {
				headers[key] = value
			}
		}
	}
	for _, key := range openAIResponsesPassthroughHeaderKeys {
		if value := responsesRequestHeader(req, key); value != "" {
			headers[key] = value
		}
	}
	if userAgent := responsesRequestHeader(req, "user-agent"); userAgent != "" {
		headers["User-Agent"] = userAgent
	}
	if originator := responsesRequestHeader(req, "originator"); originator != "" {
		headers["originator"] = originator
	}
	if codex {
		if strings.TrimSpace(headers["User-Agent"]) == "" {
			headers["User-Agent"] = openAIResponsesCodexUserAgent
		}
		if strings.TrimSpace(headers["originator"]) == "" {
			headers["originator"] = "codex_cli_rs"
		}
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}

func cloneStringHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return map[string]string{}
	}
	cloned := make(map[string]string, len(headers))
	for key, value := range headers {
		if strings.TrimSpace(value) != "" {
			cloned[key] = value
		}
	}
	return cloned
}

func requestMetadata(req *core.ResponsesRequest) map[string]string {
	if req == nil {
		return nil
	}
	return req.Metadata
}

func responsesRequestHeader(req *core.ResponsesRequest, key string) string {
	if req == nil || len(req.Headers) == 0 {
		return ""
	}
	for candidate, value := range req.Headers {
		if strings.EqualFold(strings.TrimSpace(candidate), key) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func responsesWebSocketDialer(proxyURL string) (*websocket.Dialer, error) {
	transport := netproxy.NewTransport(0)
	if proxyURL = strings.TrimSpace(proxyURL); proxyURL != "" {
		if err := netproxy.ConfigureTransportProxy(transport, proxyURL); err != nil {
			return nil, err
		}
	}
	return &websocket.Dialer{
		Proxy:            transport.Proxy,
		NetDialContext:   transport.DialContext,
		TLSClientConfig:  responsesWebSocketTLSConfig(transport.TLSClientConfig),
		HandshakeTimeout: openAIResponsesWSDialTimeout,
	}, nil
}

func responsesWebSocketTLSConfig(base *tls.Config) *tls.Config {
	config := &tls.Config{}
	if base != nil {
		config = base.Clone()
	}
	config.NextProtos = []string{"http/1.1"}
	return config
}

func websocketURL(endpoint string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", err
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	case "wss", "ws":
	default:
		return "", fmt.Errorf("unsupported websocket scheme %q", parsed.Scheme)
	}
	return parsed.String(), nil
}

func mapResponsesWebSocketDialError(ctx context.Context, err error, resp *http.Response) error {
	if resp != nil && resp.StatusCode > 0 {
		var payload []byte
		if resp.Body != nil {
			payload, _ = readLimitedUpstreamBody(resp.Body, 1<<20)
			_ = resp.Body.Close()
		}
		return mapHTTPErrorForContext(ctx, resp.StatusCode, payload)
	}
	return &InvokeError{
		Code:      ErrorCodeUpstreamTransportError,
		Temporary: true,
		Cooldown:  20 * time.Second,
		Err:       err,
	}
}
