package providers

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

type StreamingAdapter interface {
	Adapter
	OpenStream(context.Context, core.RouteDecision, *core.GatewayRequest) (*StreamSession, error)
}

type Stream interface {
	Next() (*core.StreamEvent, error)
	Close() error
}

type StreamSession struct {
	Response *core.GatewayResponse
	Stream   Stream
}

type ResponsesWebSocketAdapter interface {
	Adapter
	OpenResponsesWebSocketSession(context.Context, core.RouteDecision, *core.ResponsesRequest) (ResponsesWebSocketSession, error)
}

type ResponsesWebSocketSession interface {
	SendRequest(context.Context, string, *core.ResponsesRequest) (*StreamSession, error)
	SendRaw(context.Context, []byte) error
	Close() error
}

type sseEvent struct {
	Event string
	Data  []byte
}

type sseReader struct {
	scanner *bufio.Scanner
}

var (
	sseEventPrefix = []byte("event:")
	sseDataPrefix  = []byte("data:")
	sseDoneData    = []byte("[DONE]")
)

func doStream(req *http.Request) (*http.Response, error) {
	client, err := httpClientForContext(req.Context())
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamTransportError,
			Temporary: true,
			Cooldown:  20 * time.Second,
			Err:       err,
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &InvokeError{
			Code:      ErrorCodeUpstreamTransportError,
			Temporary: true,
			Cooldown:  20 * time.Second,
			Err:       err,
		}
	}

	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if readErr != nil {
			return nil, &InvokeError{
				Code:      ErrorCodeUpstreamReadError,
				Temporary: true,
				Cooldown:  20 * time.Second,
				Err:       readErr,
			}
		}
		return nil, mapHTTPErrorForContext(req.Context(), resp.StatusCode, payload)
	}
	return resp, nil
}

func newSSEReader(reader io.Reader) *sseReader {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), maxUpstreamResponseBodyBytes)
	return &sseReader{scanner: scanner}
}

func (r *sseReader) Next() (*sseEvent, error) {
	if r == nil || r.scanner == nil {
		return nil, io.EOF
	}

	var (
		eventName string
		data      bytes.Buffer
		hasData   bool
	)

	for r.scanner.Scan() {
		line := bytes.TrimRight(r.scanner.Bytes(), "\r")
		if len(line) == 0 {
			if eventName == "" && !hasData {
				continue
			}
			return &sseEvent{
				Event: eventName,
				Data:  data.Bytes(),
			}, nil
		}
		switch {
		case bytes.HasPrefix(line, sseEventPrefix):
			eventName = string(bytes.TrimSpace(bytes.TrimPrefix(line, sseEventPrefix)))
		case bytes.HasPrefix(line, sseDataPrefix):
			if hasData {
				data.WriteByte('\n')
			}
			_, _ = data.Write(bytes.TrimSpace(bytes.TrimPrefix(line, sseDataPrefix)))
			hasData = true
		}
	}

	if err := r.scanner.Err(); err != nil {
		return nil, err
	}
	if eventName != "" || hasData {
		return &sseEvent{
			Event: eventName,
			Data:  data.Bytes(),
		}, nil
	}
	return nil, io.EOF
}

type sliceStream struct {
	events []*core.StreamEvent
	index  int
}

func newSliceStream(events ...*core.StreamEvent) Stream {
	return &sliceStream{events: events}
}

func (s *sliceStream) Next() (*core.StreamEvent, error) {
	if s.index >= len(s.events) {
		return nil, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *sliceStream) Close() error {
	return nil
}

func closeWithError(closer io.Closer, baseErr error) error {
	if closer == nil {
		return baseErr
	}
	closeErr := closer.Close()
	if baseErr == nil {
		return closeErr
	}
	if closeErr == nil {
		return baseErr
	}
	return errors.Join(baseErr, closeErr)
}
