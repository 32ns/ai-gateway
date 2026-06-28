package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

const openAIImageStreamHeartbeatInterval = 20 * time.Second

func (s *Server) streamOpenAICompletions(ctx context.Context, w http.ResponseWriter, req *core.GatewayRequest) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming is not supported")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	introSent := false
	headerSent := false
	flushHeader := func() {
		if headerSent {
			return
		}
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		headerSent = true
	}
	writeChunk := func(payload any) error {
		flushHeader()
		if err := writeSSEJSON(w, "", payload); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	err := s.gateway.ExecuteStream(ctx, req, func(resp *core.GatewayResponse, event *core.StreamEvent) error {
		if event != nil && event.Started {
			flushHeader()
			return nil
		}
		if !introSent {
			if err := writeChunk(openAIStreamRoleChunk(resp)); err != nil {
				return err
			}
			introSent = true
		}
		if event.Delta != "" {
			if err := writeChunk(openAIStreamDeltaChunk(resp, event.Delta)); err != nil {
				return err
			}
		}
		if event.FinishReason != "" && openAIResponsesFailureFinishReason(event.FinishReason) {
			_ = writeChunk(map[string]any{
				"error": map[string]any{
					"type":    gatewayProtocolErrorType,
					"message": gatewayProtocolErrorMessage,
				},
			})
			return nil
		}
		if len(event.RawData) > 0 {
			if err := writeSSEData(w, event.RawData); err != nil {
				return err
			}
			flusher.Flush()
		}
		if event.FinishReason != "" {
			if err := writeChunk(openAIStreamFinishChunk(resp, event.FinishReason)); err != nil {
				return err
			}
		}
		if event.Usage != nil && openAIShouldSendStreamUsage(req) {
			if err := writeChunk(openAIStreamUsageChunk(resp, event.Usage)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if headerSent {
			_ = writeChunk(map[string]any{
				"error": map[string]any{
					"type":    gatewayProtocolErrorType,
					"message": gatewayProtocolErrorMessage,
				},
			})
			return nil
		}
		return err
	}
	if introSent {
		if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
			return err
		}
		flusher.Flush()
	}
	return nil
}

func (s *Server) streamOpenAIResponses(ctx context.Context, w http.ResponseWriter, req *core.ResponsesRequest) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming is not supported")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	started := false
	var (
		responseID string
		model      string
		content    = limitedTextBuilder{limit: maxStreamResponseContentRunes}
	)

	writeEvent := func(name string, payload any) error {
		if err := writeSSEJSON(w, name, payload); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	writeRawEvent := func(name string, data []byte) error {
		if len(data) == 0 {
			return nil
		}
		if err := writeSSERawEvent(w, strings.TrimSpace(name), data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	err := s.gateway.ExecuteResponsesStream(ctx, req, func(resp *core.GatewayResponse, event *core.StreamEvent) error {
		if resp != nil {
			if strings.TrimSpace(resp.ID) != "" {
				responseID = resp.ID
			}
			if strings.TrimSpace(resp.Model) != "" {
				model = resp.Model
			}
		}
		if event.FinishReason != "" && openAIResponsesFailureFinishReason(event.FinishReason) {
			started = true
			_ = writeEvent("response.failed", map[string]any{
				"type":        "response.failed",
				"response_id": responseID,
				"error": map[string]any{
					"type":    gatewayProtocolErrorType,
					"message": gatewayProtocolErrorMessage,
				},
			})
			return nil
		}
		if len(event.RawData) > 0 && resp != nil && resp.Provider == core.ProviderOpenAI {
			started = true
			return writeRawEvent(event.RawEvent, event.RawData)
		}
		if !started {
			responseID = resp.ID
			model = resp.Model
			if err := writeEvent("response.created", map[string]any{
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
			if err := writeEvent("response.output_text.delta", map[string]any{
				"type":          "response.output_text.delta",
				"response_id":   responseID,
				"output_index":  0,
				"content_index": 0,
				"delta":         event.Delta,
			}); err != nil {
				return err
			}
		}
		if event.FinishReason != "" {
			eventName := openAIResponsesTerminalEventName(event.FinishReason)
			payload := openAIResponseStreamPayloadWithStatus(responseID, firstNonEmptyString(model, resp.Model), resp.CreatedAt, openAIResponsesTerminalStatus(event.FinishReason), content.String(), event.Usage)
			if err := writeEvent(eventName, map[string]any{
				"type":        eventName,
				"response_id": responseID,
				"response":    payload,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if started {
			_ = writeEvent("response.failed", map[string]any{
				"type":        "response.failed",
				"response_id": responseID,
				"error": map[string]any{
					"type":    gatewayProtocolErrorType,
					"message": gatewayProtocolErrorMessage,
				},
			})
			return nil
		}
		return err
	}
	return nil
}

func (s *Server) streamOpenAIImageGeneration(ctx context.Context, w http.ResponseWriter, req *core.ImageGenerationRequest) error {
	return s.streamOpenAIImageEvents(ctx, w, func(emit func(*core.GatewayResponse, *core.StreamEvent) error) error {
		return s.gateway.StreamImageGeneration(ctx, req, emit)
	})
}

func (s *Server) streamOpenAIImageMultipart(ctx context.Context, w http.ResponseWriter, req *core.ImageMultipartRequest) error {
	return s.streamOpenAIImageEvents(ctx, w, func(emit func(*core.GatewayResponse, *core.StreamEvent) error) error {
		return s.gateway.StreamImageMultipart(ctx, req, emit)
	})
}

func (s *Server) streamOpenAIImageEvents(ctx context.Context, w http.ResponseWriter, stream func(func(*core.GatewayResponse, *core.StreamEvent) error) error) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming is not supported")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	writeEvent := func(eventName string, payload any) error {
		if err := writeSSEJSON(w, eventName, payload); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	writeRawEvent := func(eventName string, data []byte) error {
		if len(data) == 0 {
			return nil
		}
		if err := writeSSERawEvent(w, strings.TrimSpace(eventName), data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	startedAt := time.Now().UTC()
	if err := writeEvent("image_generation.started", map[string]any{
		"object":        "image.generation.chunk",
		"type":          "image_generation.started",
		"created":       startedAt.Unix(),
		"progress_text": "image generation request accepted",
		"data":          []any{},
	}); err != nil {
		return err
	}

	type imageStreamItem struct {
		event *core.StreamEvent
		err   error
		done  bool
	}
	events := make(chan imageStreamItem, 8)
	go func() {
		err := stream(func(_ *core.GatewayResponse, event *core.StreamEvent) error {
			if event == nil || len(event.RawData) == 0 {
				return nil
			}
			select {
			case events <- imageStreamItem{event: event}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		select {
		case events <- imageStreamItem{err: err, done: true}:
		case <-ctx.Done():
		}
		close(events)
	}()

	heartbeat := time.NewTicker(openAIImageStreamHeartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case item, ok := <-events:
			if !ok {
				return nil
			}
			if item.err != nil {
				_ = writeEvent("error", map[string]any{
					"type":    gatewayProtocolErrorType,
					"message": gatewayProtocolErrorMessage,
				})
				return nil
			}
			if item.done || item.event == nil {
				return nil
			}
			if err := writeRawEvent(item.event.RawEvent, item.event.RawData); err != nil {
				return err
			}
			if item.event.Done {
				return nil
			}
		case now := <-heartbeat.C:
			if err := writeEvent("image_generation.progress", map[string]any{
				"object":        "image.generation.chunk",
				"type":          "image_generation.progress",
				"created":       now.Unix(),
				"progress_text": fmt.Sprintf("image generation is still running, elapsed %ds", int(time.Since(startedAt).Seconds())),
				"data":          []any{},
			}); err != nil {
				return err
			}
		}
	}
}

func (s *Server) streamAnthropicMessages(ctx context.Context, w http.ResponseWriter, req *core.GatewayRequest) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming is not supported")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	started := false
	contentStopped := false
	rawPassthrough := false
	wroteSSE := false
	writeEvent := func(event string, payload any) error {
		if err := writeSSEJSON(w, event, payload); err != nil {
			return err
		}
		wroteSSE = true
		flusher.Flush()
		return nil
	}
	if err := writeEvent("ping", map[string]any{"type": "ping"}); err != nil {
		return err
	}

	err := s.gateway.ExecuteStream(ctx, req, func(resp *core.GatewayResponse, event *core.StreamEvent) error {
		if event.FinishReason != "" && openAIResponsesFailureFinishReason(event.FinishReason) {
			_ = writeEvent("error", map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    gatewayProtocolErrorType,
					"message": gatewayProtocolErrorMessage,
				},
			})
			return nil
		}
		if req != nil && req.UpstreamMode == "anthropic_messages" && resp != nil && resp.Provider == core.ProviderClaude && event.RawEvent != "" {
			started = true
			rawPassthrough = true
			if err := writeSSERawEvent(w, event.RawEvent, event.RawData); err != nil {
				return err
			}
			wroteSSE = true
			flusher.Flush()
			return nil
		}
		if !started {
			if err := writeEvent("message_start", map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id":            resp.ID,
					"type":          "message",
					"role":          "assistant",
					"content":       []any{},
					"model":         resp.Model,
					"stop_reason":   nil,
					"stop_sequence": nil,
					"usage":         anthropicUsageMapFromCore(core.Usage{}),
				},
			}); err != nil {
				return err
			}
			if err := writeEvent("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": 0,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			}); err != nil {
				return err
			}
			started = true
		}
		if event.Delta != "" {
			if err := writeEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{
					"type": "text_delta",
					"text": event.Delta,
				},
			}); err != nil {
				return err
			}
		}
		if event.FinishReason != "" {
			if !contentStopped {
				if err := writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}); err != nil {
					return err
				}
				contentStopped = true
			}
			usage := core.Usage{}
			if event.Usage != nil {
				usage = *event.Usage
			}
			if err := writeEvent("message_delta", map[string]any{
				"type": "message_delta",
				"delta": map[string]any{
					"stop_reason": anthropicStopReason(event.FinishReason),
				},
				"usage": anthropicUsageMapFromCore(usage),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if wroteSSE {
			_ = writeEvent("error", map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    gatewayProtocolErrorType,
					"message": gatewayProtocolErrorMessage,
				},
			})
			return nil
		}
		return err
	}
	if rawPassthrough {
		return nil
	}
	if started {
		if !contentStopped {
			if err := writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}); err != nil {
				return err
			}
		}
		if err := writeEvent("message_stop", map[string]any{"type": "message_stop"}); err != nil {
			return err
		}
	}
	return nil
}
