package web

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestStreamOpenAIImageEventsSendsStartedBeforeResult(t *testing.T) {
	server := &Server{}
	rec := httptest.NewRecorder()
	err := server.streamOpenAIImageEvents(context.Background(), rec, func(emit func(*core.GatewayResponse, *core.StreamEvent) error) error {
		return emit(nil, &core.StreamEvent{
			Done:     true,
			RawEvent: "image_generation.completed",
			RawData:  []byte(`{"object":"image.generation.result","data":[{"url":"https://example.com/image.png"}]}`),
		})
	})
	if err != nil {
		t.Fatalf("streamOpenAIImageEvents returned error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: image_generation.started") {
		t.Fatalf("started event missing: %s", body)
	}
	if !strings.Contains(body, "event: image_generation.completed") {
		t.Fatalf("completed event missing: %s", body)
	}
	if strings.Index(body, "image_generation.started") > strings.Index(body, "image_generation.completed") {
		t.Fatalf("started event should be emitted before completed event: %s", body)
	}
}

func TestStreamOpenAIImageEventsHandlesCleanEndWithoutDoneEvent(t *testing.T) {
	server := &Server{}
	rec := httptest.NewRecorder()
	err := server.streamOpenAIImageEvents(context.Background(), rec, func(emit func(*core.GatewayResponse, *core.StreamEvent) error) error {
		return emit(nil, &core.StreamEvent{
			RawEvent: "image_generation.partial_image",
			RawData:  []byte(`{"object":"image.generation.chunk","data":[]}`),
		})
	})
	if err != nil {
		t.Fatalf("streamOpenAIImageEvents returned error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: image_generation.started") {
		t.Fatalf("started event missing: %s", body)
	}
	if !strings.Contains(body, "event: image_generation.partial_image") {
		t.Fatalf("partial event missing: %s", body)
	}
}

func TestWriteSSEDataPrefixesEachLine(t *testing.T) {
	var buf bytes.Buffer
	if err := writeSSEData(&buf, []byte("first\nsecond")); err != nil {
		t.Fatalf("writeSSEData returned error: %v", err)
	}
	if got, want := buf.String(), "data: first\ndata: second\n\n"; got != want {
		t.Fatalf("SSE data = %q, want %q", got, want)
	}
}
