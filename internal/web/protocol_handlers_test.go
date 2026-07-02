package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/failover"
	"github.com/32ns/ai-gateway/internal/providers"
)

func TestWriteRawProxyResponseFallsBackForUnsafeContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	writeRawProxyResponse(rec, &providers.RawProxyResponse{
		StatusCode:  http.StatusOK,
		ContentType: "text/html; charset=utf-8",
		Body:        []byte("<script>alert(1)</script>"),
	}, "application/json")

	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Body.String(); got != "<script>alert(1)</script>" {
		t.Fatalf("body = %q", got)
	}
}

func TestWriteRawProxyResponseKeepsJSONContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	writeRawProxyResponse(rec, &providers.RawProxyResponse{
		StatusCode:  http.StatusOK,
		ContentType: "application/json; charset=utf-8",
		Body:        []byte(`{"ok":true}`),
	}, "application/json")

	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
}

func TestWriteRawProxyResponseHidesUpstreamErrorBody(t *testing.T) {
	rec := httptest.NewRecorder()
	writeRawProxyResponse(rec, &providers.RawProxyResponse{
		StatusCode:  http.StatusBadRequest,
		ContentType: "application/json",
		Body:        []byte(`{"error":{"message":"upstream secret detail","code":"upstream_bad_request"}}`),
	}, "application/json")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.Error.Type != gatewayProtocolErrorType || payload.Error.Message != gatewayProtocolErrorMessage {
		t.Fatalf("payload = %#v", payload)
	}
	for _, leaked := range []string{"upstream secret detail", "upstream_bad_request"} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Fatalf("response leaked upstream detail %q in %s", leaked, rec.Body.String())
		}
	}
}

func TestWriteRawProxyResponseHidesTopLevelErrorBodyEvenOnOK(t *testing.T) {
	rec := httptest.NewRecorder()
	writeRawProxyResponse(rec, &providers.RawProxyResponse{
		StatusCode:  http.StatusOK,
		ContentType: "application/json",
		Body:        []byte(`{"error":{"message":"upstream ok-status secret","code":"server_error"}}`),
	}, "application/json")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	for _, leaked := range []string{"upstream ok-status secret", "server_error"} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Fatalf("response leaked %q in %s", leaked, rec.Body.String())
		}
	}
}

func TestOpenAIResponsesFailureFinishReasonIsGatewayError(t *testing.T) {
	rec := httptest.NewRecorder()
	resp := &core.GatewayResponse{
		ID:           "resp_failed_secret",
		Model:        "gpt-4.1",
		Provider:     core.ProviderOpenAI,
		FinishReason: "failed",
		Content:      "tool output was invalid",
		RawBody:      []byte(`{"status":"failed","error":{"type":"server_error","message":"upstream secret detail"}}`),
	}
	if openAIResponsesFailureFinishReason(resp.FinishReason) {
		writeProtocolGatewayError(rec, http.StatusBadGateway)
	} else {
		writeOpenAIRawResponse(rec, http.StatusOK, resp, &core.ResponsesRequest{})
	}

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.Error.Type != gatewayProtocolErrorType || payload.Error.Message != gatewayProtocolErrorMessage {
		t.Fatalf("payload = %#v", payload)
	}
	for _, leaked := range []string{"resp_failed_secret", "tool output was invalid", "upstream secret detail", `"status":"failed"`} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Fatalf("response leaked %q in %s", leaked, rec.Body.String())
		}
	}
}

func TestWriteGatewayErrorExposesUpstreamRejectedMessage(t *testing.T) {
	rec := httptest.NewRecorder()
	err := &failover.ExecutionError{Attempts: []core.AttemptRecord{
		{
			Provider:     core.ProviderOpenAI,
			AccountID:    "acct_secret",
			AccountLabel: "secret account",
			Status:       "invoke_error",
			ErrorCode:    providers.ErrorCodeUpstreamRejected,
			ErrorMessage: "upstream_rejected: Your input exceeds the context window of this model. Please adjust your input and try again.",
		},
	}}

	(&Server{}).writeGatewayError(rec, err)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.Error.Type != "invalid_request_error" || payload.Error.Code != providers.ErrorCodeUpstreamRejected {
		t.Fatalf("payload = %#v", payload)
	}
	if !strings.Contains(payload.Error.Message, "context window") {
		t.Fatalf("message = %q, want upstream rejection detail", payload.Error.Message)
	}
	for _, leaked := range []string{"acct_secret", "secret account"} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Fatalf("response leaked %q in %s", leaked, rec.Body.String())
		}
	}
}

func TestWriteGatewayErrorHidesExecutionServerError(t *testing.T) {
	rec := httptest.NewRecorder()
	err := &failover.ExecutionError{Attempts: []core.AttemptRecord{
		{
			Provider:     core.ProviderOpenAI,
			AccountLabel: "secret account",
			Status:       "invoke_error",
			ErrorCode:    providers.ErrorCodeUpstreamServerError,
			ErrorMessage: "upstream_server_error: temporary upstream outage",
		},
	}}

	(&Server{}).writeGatewayError(rec, err)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if !strings.Contains(rec.Body.String(), gatewayProtocolErrorMessage) {
		t.Fatalf("body missing gateway wrapper: %s", rec.Body.String())
	}
	for _, leaked := range []string{"temporary upstream outage", "secret account"} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Fatalf("response leaked %q in %s", leaked, rec.Body.String())
		}
	}
}

func TestOpenAIRawCompletionHidesTopLevelErrorBody(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOpenAIRawCompletion(rec, http.StatusOK, &core.GatewayResponse{
		RawBody: []byte(`{"error":{"type":"server_error","message":"upstream secret detail"}}`),
	}, &core.GatewayRequest{})

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if !strings.Contains(rec.Body.String(), gatewayProtocolErrorMessage) {
		t.Fatalf("body missing gateway message: %s", rec.Body.String())
	}
	for _, leaked := range []string{"upstream secret detail", "server_error"} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Fatalf("response leaked %q in %s", leaked, rec.Body.String())
		}
	}
}

func TestAnthropicRawResponseHidesTopLevelErrorBody(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAnthropicRawResponse(rec, http.StatusOK, &core.GatewayResponse{
		RawBody: []byte(`{"type":"error","error":{"type":"api_error","message":"upstream secret detail"}}`),
	})

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if !strings.Contains(rec.Body.String(), gatewayProtocolErrorMessage) {
		t.Fatalf("body missing gateway message: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "upstream secret detail") {
		t.Fatalf("response leaked upstream detail in %s", rec.Body.String())
	}
}

func TestRawBodyProtocolErrorAllowsOpenAIResponseObjects(t *testing.T) {
	body := []byte(`{"object":"response","status":"failed","error":{"message":"upstream detail"}}`)
	if rawBodyHasProtocolError(body) {
		t.Fatalf("response object should be handled by finish reason guard, not raw top-level guard")
	}
}

func TestContentTypeIsJSON(t *testing.T) {
	for _, value := range []string{"application/json", "application/json; charset=utf-8", "application/problem+json"} {
		if !contentTypeIsJSON(value) {
			t.Fatalf("contentTypeIsJSON(%q) = false", value)
		}
	}
	if contentTypeIsJSON("audio/mpeg") {
		t.Fatal("contentTypeIsJSON(audio/mpeg) = true")
	}
}

func TestAnthropicUsageFromCoreUsesAnthropicInputSemantics(t *testing.T) {
	usage := anthropicUsageFromCore(core.Usage{
		PromptTokens:        140,
		CachedPromptTokens:  40,
		CacheCreationTokens: 10,
		CompletionTokens:    25,
	})
	if usage.InputTokens != 100 || usage.CacheReadInputTokens != 40 || usage.CacheCreationInputTokens != 10 || usage.OutputTokens != 25 {
		t.Fatalf("usage = %#v", usage)
	}

	payload := anthropicUsageMapFromCore(core.Usage{
		PromptTokens:        20,
		CachedPromptTokens:  30,
		CacheCreationTokens: 5,
		CompletionTokens:    7,
	})
	if payload["input_tokens"] != 0 || payload["cache_read_input_tokens"] != 30 || payload["cache_creation_input_tokens"] != 5 || payload["output_tokens"] != 7 {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestOpenAIResponseStreamPayloadIncludesDetailedUsage(t *testing.T) {
	payload := openAIResponseStreamPayloadWithStatus("resp", "gpt-test", time.Unix(1710000000, 0), "completed", "ok", &core.Usage{
		PromptTokens:        12,
		CachedPromptTokens:  3,
		CacheCreationTokens: 4,
		CompletionTokens:    6,
		ImageOutputTokens:   2,
		TotalTokens:         22,
	})
	usage, ok := payload["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage payload = %#v", payload["usage"])
	}
	if usage["cache_creation_input_tokens"] != 4 {
		t.Fatalf("cache_creation_input_tokens = %#v", usage["cache_creation_input_tokens"])
	}
	inputDetails, ok := usage["input_tokens_details"].(map[string]any)
	if !ok || inputDetails["cached_tokens"] != 3 {
		t.Fatalf("input details = %#v", usage["input_tokens_details"])
	}
	outputDetails, ok := usage["output_tokens_details"].(map[string]any)
	if !ok || outputDetails["image_tokens"] != 2 {
		t.Fatalf("output details = %#v", usage["output_tokens_details"])
	}
}
