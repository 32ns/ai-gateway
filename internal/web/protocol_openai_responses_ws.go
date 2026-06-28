package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/gorilla/websocket"
)

const openAIResponsesWebSocketReadLimit = 16 << 20

var openAIResponsesWebSocketUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func openAIResponsesWebSocketRequested(r *http.Request) bool {
	return websocket.IsWebSocketUpgrade(r)
}

func (s *Server) handleOpenAIResponsesWebSocket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	conn, err := openAIResponsesWebSocketUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(openAIResponsesWebSocketReadLimit)

	protocolClient := protocolClientPointerFromContext(r.Context())
	headers := openAIResponsesWebSocketHeadersFromRequest(r)
	gatewaySession := openAIResponsesWebSocketGatewaySession(nil)
	if s.gateway != nil {
		gatewaySession = s.gateway.NewResponsesWebSocketSession(nil, protocolClient)
		defer gatewaySession.Close()
	}
	sessionModel := ""
	readCh := make(chan openAIResponsesWebSocketClientMessage, 1)
	readerStop := make(chan struct{})
	defer close(readerStop)
	go readOpenAIResponsesWebSocketClient(conn, readCh, readerStop)

	var writeMu sync.Mutex
	writeText := func(payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(websocket.TextMessage, payload)
	}
	writeError := func(errorType, message string) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return writeOpenAIResponsesWebSocketError(conn, errorType, message)
	}
	writeSessionUpdated := func(sessionRaw json.RawMessage, model string) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return writeOpenAIResponsesWebSocketSessionUpdated(conn, sessionRaw, model)
	}
	writeCancelled := func(responseID string) error {
		body, err := json.Marshal(map[string]any{
			"type":        "response.cancelled",
			"response_id": strings.TrimSpace(responseID),
		})
		if err != nil {
			return err
		}
		return writeText(body)
	}

	var active *openAIResponsesWebSocketActiveTurn
	defer func() {
		if active != nil && active.cancel != nil {
			active.cancel()
		}
	}()

	for {
		doneCh := (<-chan error)(nil)
		if active != nil {
			doneCh = active.done
		}
		select {
		case <-r.Context().Done():
			if active != nil && active.cancel != nil {
				active.cancel()
			}
			return
		case result := <-doneCh:
			active = nil
			if result != nil {
				if errors.Is(result, context.Canceled) {
					continue
				}
				if isClientWebSocketClose(result) {
					return
				}
				return
			}
			continue
		case message := <-readCh:
			if message.err != nil {
				if active != nil && active.cancel != nil {
					active.cancel()
				}
				if isClientWebSocketClose(message.err) {
					return
				}
				_ = writeError("invalid_request_error", message.err.Error())
				return
			}
			if active != nil {
				select {
				case result := <-active.done:
					active = nil
					if result != nil {
						if errors.Is(result, context.Canceled) {
							break
						}
						if isClientWebSocketClose(result) {
							return
						}
						return
					}
				default:
				}
			}
			if message.messageType != websocket.TextMessage {
				_ = writeError("invalid_request_error", "unsupported websocket message type")
				return
			}
			if handled, err := openAIResponsesWebSocketResponseProcessed(message.payload); err != nil {
				_ = writeError("invalid_request_error", err.Error())
				return
			} else if handled {
				if gatewaySession != nil {
					_ = gatewaySession.SendRaw(r.Context(), message.payload)
				}
				continue
			}
			if handled, responseID, err := openAIResponsesWebSocketCancel(message.payload); err != nil {
				_ = writeError("invalid_request_error", err.Error())
				return
			} else if handled {
				if active == nil || active.cancel == nil {
					_ = writeError("invalid_request_error", "no active response to cancel")
					continue
				}
				if gatewaySession != nil {
					_ = gatewaySession.SendRaw(r.Context(), message.payload)
				}
				active.cancel()
				if gatewaySession != nil {
					_ = gatewaySession.Close()
				}
				if err := writeCancelled(responseID); err != nil {
					return
				}
				active.signalTerminal()
				continue
			}
			if handled, model, sessionRaw, err := openAIResponsesWebSocketSessionUpdate(message.payload); err != nil {
				_ = writeError("invalid_request_error", err.Error())
				return
			} else if handled {
				if strings.TrimSpace(model) != "" {
					sessionModel = strings.TrimSpace(model)
				}
				if err := writeSessionUpdated(sessionRaw, sessionModel); err != nil {
					return
				}
				continue
			}
			if active != nil {
				if !active.terminalSent() {
					_ = writeError("invalid_request_error", "response already in progress")
					continue
				}
				select {
				case result := <-active.done:
					active = nil
					if result != nil {
						if !errors.Is(result, context.Canceled) {
							if isClientWebSocketClose(result) {
								return
							}
							return
						}
					}
				case <-r.Context().Done():
					active.cancel()
					return
				}
			}
			req, err := s.openAIResponsesWebSocketRequest(message.payload, protocolClient, headers, sessionModel)
			if err != nil {
				_ = writeError("invalid_request_error", err.Error())
				return
			}
			if strings.TrimSpace(req.Model) != "" {
				sessionModel = strings.TrimSpace(req.Model)
			}
			turnCtx, cancel := context.WithCancel(r.Context())
			done := make(chan error, 1)
			activeTurn := &openAIResponsesWebSocketActiveTurn{cancel: cancel, done: done, terminal: make(chan struct{})}
			active = activeTurn
			go func(turn *openAIResponsesWebSocketActiveTurn) {
				defer cancel()
				done <- s.proxyOpenAIResponsesWebSocketTurn(turnCtx, writeText, writeError, turn.signalTerminal, gatewaySession, req)
			}(activeTurn)
		}
	}
}

type openAIResponsesWebSocketClientMessage struct {
	messageType int
	payload     []byte
	err         error
}

type openAIResponsesWebSocketActiveTurn struct {
	cancel       context.CancelFunc
	done         chan error
	terminal     chan struct{}
	terminalOnce sync.Once
}

func (t *openAIResponsesWebSocketActiveTurn) signalTerminal() {
	if t == nil || t.terminal == nil {
		return
	}
	t.terminalOnce.Do(func() {
		close(t.terminal)
	})
}

func (t *openAIResponsesWebSocketActiveTurn) terminalSent() bool {
	if t == nil || t.terminal == nil {
		return false
	}
	select {
	case <-t.terminal:
		return true
	default:
		return false
	}
}

type openAIResponsesWebSocketGatewaySession interface {
	Execute(context.Context, *core.ResponsesRequest, func(*core.GatewayResponse, *core.StreamEvent) error) error
	SendRaw(context.Context, []byte) error
	Close() error
}

func readOpenAIResponsesWebSocketClient(conn *websocket.Conn, out chan<- openAIResponsesWebSocketClientMessage, stop <-chan struct{}) {
	for {
		messageType, payload, err := conn.ReadMessage()
		message := openAIResponsesWebSocketClientMessage{messageType: messageType, payload: payload, err: err}
		select {
		case out <- message:
		case <-stop:
			return
		}
		if err != nil {
			return
		}
	}
}

func (s *Server) proxyOpenAIResponsesWebSocketTurn(ctx context.Context, sendText func([]byte) error, sendError func(string, string) error, signalTerminal func(), gatewaySession openAIResponsesWebSocketGatewaySession, req *core.ResponsesRequest) error {
	var (
		clientClosed bool
		started      bool
		responseID   string
		model        string
		content      = limitedTextBuilder{limit: maxStreamResponseContentRunes}
	)

	writeText := func(payload []byte) error {
		if clientClosed {
			return nil
		}
		if err := sendText(payload); err != nil {
			clientClosed = true
			return nil
		}
		return nil
	}
	writeJSONEvent := func(payload any) error {
		body, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		return writeText(body)
	}

	stream := func(emit func(*core.GatewayResponse, *core.StreamEvent) error) error {
		if gatewaySession != nil {
			return gatewaySession.Execute(ctx, req, emit)
		}
		return s.gateway.ExecuteResponsesStream(ctx, req, emit)
	}
	err := stream(func(resp *core.GatewayResponse, event *core.StreamEvent) error {
		if resp != nil {
			if strings.TrimSpace(resp.ID) != "" {
				responseID = resp.ID
			}
			if strings.TrimSpace(resp.Model) != "" {
				model = resp.Model
			}
		}
		if event == nil {
			return nil
		}
		if event.FinishReason != "" && openAIResponsesFailureFinishReason(event.FinishReason) {
			_ = sendError(gatewayProtocolErrorType, gatewayProtocolErrorMessage)
			signalTerminal()
			return nil
		}
		if len(event.RawData) > 0 && resp != nil && resp.Provider == core.ProviderOpenAI {
			started = true
			if err := writeText(event.RawData); err != nil {
				return err
			}
			if event.Done || event.FinishReason != "" {
				signalTerminal()
			}
			return nil
		}
		if !started {
			if resp == nil {
				return nil
			}
			responseID = resp.ID
			model = resp.Model
			if err := writeJSONEvent(map[string]any{
				"type":        "response.created",
				"response_id": responseID,
				"response": map[string]any{
					"id":         responseID,
					"object":     "response",
					"created_at": resp.CreatedAt.Unix(),
					"model":      model,
					"status":     "in_progress",
				},
			}); err != nil {
				return err
			}
			started = true
		}
		if event.Delta != "" {
			content.WriteString(event.Delta)
			if err := writeJSONEvent(map[string]any{
				"type":          "response.output_text.delta",
				"response_id":   responseID,
				"output_index":  0,
				"content_index": 0,
				"delta":         event.Delta,
			}); err != nil {
				return err
			}
		}
		if event.FinishReason != "" && resp != nil {
			eventName := openAIResponsesTerminalEventName(event.FinishReason)
			if err := writeJSONEvent(map[string]any{
				"type":        eventName,
				"response_id": responseID,
				"response":    openAIResponseStreamPayloadWithStatus(responseID, firstNonEmptyString(model, resp.Model), resp.CreatedAt, openAIResponsesTerminalStatus(event.FinishReason), content.String(), event.Usage),
			}); err != nil {
				return err
			}
			signalTerminal()
			return nil
		}
		return nil
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	if clientClosed {
		return nil
	}
	_ = sendError(gatewayProtocolErrorType, gatewayProtocolErrorMessage)
	return err
}

func (s *Server) openAIResponsesWebSocketRequest(payload []byte, client *core.APIClient, headers map[string]string, fallbackModel string) (*core.ResponsesRequest, error) {
	normalized, _, err := normalizeOpenAIResponsesWebSocketPayload(payload)
	if err != nil {
		return nil, err
	}
	req, rawFields, err := parseOpenAIResponseRequest(normalized)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Model) == "" {
		req.Model = strings.TrimSpace(fallbackModel)
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("model is required")
	}
	sessionAffinityKey := openAIHeadersSessionAffinityKey(headers)
	metadata := applyOpenAICacheAffinityMetadata(req.Metadata, rawFields, req.Model, sessionAffinityKey)
	promptCacheKey := s.openAIPromptCacheKey(rawFields, req.Model, client, sessionAffinityKey)
	return &core.ResponsesRequest{
		Model:              req.Model,
		RawBody:            json.RawMessage(normalized),
		Client:             client,
		Transport:          core.ResponsesTransportWebSocket,
		Stream:             true,
		Generate:           req.Generate,
		MaxOutputTokens:    req.MaxOutputTokens,
		ServiceTier:        strings.TrimSpace(req.ServiceTier),
		PreviousResponseID: strings.TrimSpace(req.PreviousResponseID),
		PromptCacheKey:     promptCacheKey,
		Metadata:           metadata,
		Headers:            headers,
	}, nil
}

func openAIResponsesWebSocketResponseProcessed(payload []byte) (bool, error) {
	raw, err := parseRawObject(payload)
	if err != nil {
		return false, err
	}
	return rawStringField(raw, "type") == "response.processed", nil
}

func openAIResponsesWebSocketSessionUpdate(payload []byte) (bool, string, json.RawMessage, error) {
	raw, err := parseRawObject(payload)
	if err != nil {
		return false, "", nil, err
	}
	if rawStringField(raw, "type") != "session.update" {
		return false, "", nil, nil
	}
	sessionRaw := raw["session"]
	if len(sessionRaw) == 0 {
		return true, "", json.RawMessage(`{}`), nil
	}
	var session map[string]json.RawMessage
	if err := json.Unmarshal(sessionRaw, &session); err != nil {
		return false, "", nil, err
	}
	return true, rawStringField(session, "model"), sessionRaw, nil
}

func openAIResponsesWebSocketCancel(payload []byte) (bool, string, error) {
	raw, err := parseRawObject(payload)
	if err != nil {
		return false, "", err
	}
	if rawStringField(raw, "type") != "response.cancel" {
		return false, "", nil
	}
	responseID := rawStringField(raw, "response_id")
	if responseID == "" {
		responseID = rawStringField(raw, "id")
	}
	return true, responseID, nil
}

func writeOpenAIResponsesWebSocketSessionUpdated(conn *websocket.Conn, sessionRaw json.RawMessage, model string) error {
	session := map[string]any{}
	if len(sessionRaw) > 0 {
		_ = json.Unmarshal(sessionRaw, &session)
	}
	if strings.TrimSpace(model) != "" {
		session["model"] = strings.TrimSpace(model)
	}
	body, err := json.Marshal(map[string]any{
		"type":    "session.updated",
		"session": session,
	})
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err = conn.WriteMessage(websocket.TextMessage, body)
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

func normalizeOpenAIResponsesWebSocketPayload(payload []byte) ([]byte, map[string]json.RawMessage, error) {
	raw, err := parseRawObject(payload)
	if err != nil {
		return nil, nil, err
	}
	eventType := rawStringField(raw, "type")
	if eventType != "" && eventType != "response.create" {
		return nil, nil, fmt.Errorf("unsupported websocket event type %q", eventType)
	}
	body := make(map[string]json.RawMessage, len(raw)+1)
	for key, value := range raw {
		key = strings.TrimSpace(key)
		if key == "" || key == "type" {
			continue
		}
		body[key] = value
	}
	body["stream"] = json.RawMessage(`true`)
	normalized, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	return normalized, body, nil
}

func openAIResponsesWebSocketHeadersFromRequest(r *http.Request) map[string]string {
	return openAIResponsesHeadersFromRequest(r)
}

func writeOpenAIResponsesWebSocketError(conn *websocket.Conn, errorType string, message string) error {
	if conn == nil {
		return nil
	}
	errorType = strings.TrimSpace(errorType)
	if errorType == "" {
		errorType = gatewayProtocolErrorType
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = gatewayProtocolErrorMessage
	}
	body, err := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errorType,
			"message": message,
		},
	})
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err = conn.WriteMessage(websocket.TextMessage, body)
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

func isClientWebSocketClose(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, io.EOF) ||
		websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived, websocket.CloseAbnormalClosure)
}
