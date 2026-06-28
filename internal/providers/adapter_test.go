package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/gorilla/websocket"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func failurePolicyPenalizes(err error) bool {
	policy := FailurePolicyFor(core.Account{}, err)
	return policy.Scope == FailureScopeAccount || policy.Scope == FailureScopeQuota
}

func testHTTPJSONResponse(status int, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}, nil
}

func TestOpenAIAdapterInvokeUsesRealHTTPShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer openai-token" {
			t.Fatalf("authorization = %q", got)
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["model"] != "gpt-4.1" {
			t.Fatalf("model = %v", body["model"])
		}
		if body["max_tokens"] != float64(256) {
			t.Fatalf("max_tokens = %v", body["max_tokens"])
		}
		if body["top_p"] != 0.8 {
			t.Fatalf("top_p = %v", body["top_p"])
		}
		if body["stop"] != "END" {
			t.Fatalf("stop = %v", body["stop"])
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":           "chatcmpl_test",
			"model":        "gpt-4.1",
			"service_tier": "priority",
			"created":      1710000000,
			"choices": []map[string]any{
				{
					"message":       map[string]any{"content": "hello from openai"},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     10,
				"completion_tokens": 4,
				"total_tokens":      14,
			},
		})
	}))
	defer server.Close()

	adapter := &OpenAIAdapter{}
	resp, err := adapter.Invoke(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "openai-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "gpt-4.1",
	}, &core.GatewayRequest{
		Model:     "gpt-4.1",
		Messages:  []core.Message{{Role: "user", Content: "hi"}},
		MaxTokens: intPtr(256),
		TopP:      floatPtr(0.8),
		Stop:      []string{"END"},
	})
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
	if resp.Content != "hello from openai" {
		t.Fatalf("content = %q", resp.Content)
	}
	if resp.Usage.TotalTokens != 14 {
		t.Fatalf("total_tokens = %d", resp.Usage.TotalTokens)
	}
	if resp.ServiceTier != "priority" {
		t.Fatalf("service_tier = %q, want priority", resp.ServiceTier)
	}
	if !strings.Contains(string(resp.RawBody), `"hello from openai"`) {
		t.Fatalf("raw body should preserve upstream chat completion: %s", resp.RawBody)
	}
}

func TestOpenAIAdapterInvokeMapsGatewayAPIKeyDisabledAsTemporary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":    "API_KEY_DISABLED",
			"message": "API key is disabled",
		})
	}))
	defer server.Close()

	_, err := (&OpenAIAdapter{}).Invoke(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_sub2api",
			Label:    "Sub2API",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "sub2api-token",
				Metadata: map[string]string{
					"account_login_method":   "api_key",
					"api_key_quota_provider": "sub2api",
					"base_url":               server.URL,
				},
			},
		},
		Model: "gpt-5.4",
	}, &core.GatewayRequest{
		Model:    "gpt-5.4",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("err = %T %v, want InvokeError", err, err)
	}
	if invokeErr.Code != "gateway_api_key_disabled" || !invokeErr.Temporary {
		t.Fatalf("invoke error = %#v, want temporary gateway_api_key_disabled", invokeErr)
	}
	if FailurePolicyFor(core.Account{}, err).RefreshCredential {
		t.Fatal("gateway API key disabled should not trigger credential refresh")
	}
}

func TestOpenAIAdapterInvokeRejectsNewAPITopLevelErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"message":"no available channel","type":"new_api_error","code":"channel:no_available_key"}}`))
	}))
	defer server.Close()

	_, err := (&OpenAIAdapter{}).Invoke(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_new_api",
			Label:    "new-api",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "new-api-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "gpt-4.1",
	}, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("err = %T %v, want InvokeError", err, err)
	}
	if invokeErr.Code != "upstream_rejected" || !strings.Contains(invokeErr.Error(), "no available channel") {
		t.Fatalf("invoke error = %#v", invokeErr)
	}
}

func TestOpenAIAdapterInvokeMapsNewAPIChannelExhaustedAsTemporary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"message":"no available channel","type":"new_api_error","code":"channel:no_available_key"}}`))
	}))
	defer server.Close()

	_, err := (&OpenAIAdapter{}).Invoke(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_new_api",
			Label:    "new-api",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "new-api-token",
				Metadata: map[string]string{
					"base_url":               server.URL,
					"account_login_method":   "api_key",
					"api_key_quota_provider": "new-api",
				},
			},
		},
		Model: "gpt-4.1",
	}, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("err = %T %v, want InvokeError", err, err)
	}
	if invokeErr.Code != "upstream_temporarily_unavailable" || !invokeErr.Temporary {
		t.Fatalf("invoke error = %#v, want temporary upstream_temporarily_unavailable", invokeErr)
	}
	if failurePolicyPenalizes(err) {
		t.Fatal("new-api middle-layer exhaustion should not penalize the account")
	}
	if FailurePolicyFor(core.Account{}, err).Scope != FailureScopeSharedUpstream {
		t.Fatal("new-api middle-layer exhaustion should trigger failover")
	}
}

func TestOpenAIAdapterInvokeMapsNewAPIQuotaErrorBodyAsRateLimited(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota","code":"insufficient_quota"}}`))
	}))
	defer server.Close()

	_, err := (&OpenAIAdapter{}).Invoke(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_new_api",
			Label:    "new-api",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "new-api-token",
				Metadata: map[string]string{
					"base_url":               server.URL,
					"account_login_method":   "api_key",
					"api_key_quota_provider": "new-api",
				},
			},
		},
		Model: "gpt-4.1",
	}, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("err = %T %v, want InvokeError", err, err)
	}
	if invokeErr.Code != "upstream_rate_limited" || !invokeErr.Temporary {
		t.Fatalf("invoke error = %#v, want temporary upstream_rate_limited", invokeErr)
	}
	policy := FailurePolicyFor(core.Account{}, err)
	if policy.Scope != FailureScopeQuota || !policy.RetryFailover || !policy.ApplyChatQuota {
		t.Fatalf("policy = %#v, want quota failover", policy)
	}
}

func TestOpenAIAdapterPreservesRawChatCompletionWhenNeeded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_tool","model":"gpt-4.1","created":1710000000,"choices":[{"message":{"content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer server.Close()

	resp, err := (&OpenAIAdapter{}).Invoke(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "openai-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "gpt-4.1",
	}, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "use a tool"}},
	})
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
	if !strings.Contains(string(resp.RawBody), `"tool_calls"`) {
		t.Fatalf("raw body should preserve tool calls: %s", resp.RawBody)
	}
}

func TestOpenAIAdapterInvokeAcceptsResponsesBodyFromChatEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_gateway","object":"response","created_at":1710000000,"status":"completed","model":"gpt-5.4","service_tier":"priority","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"pong"}]}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10,"input_tokens_details":{"cached_tokens":2}}}`))
	}))
	defer server.Close()

	resp, err := (&OpenAIAdapter{}).Invoke(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "openai-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "gpt-5.4",
	}, &core.GatewayRequest{
		Model:    "gpt-5.4",
		Messages: []core.Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
	if resp.Content != "pong" {
		t.Fatalf("content = %q", resp.Content)
	}
	if resp.Usage.PromptTokens != 7 || resp.Usage.CachedPromptTokens != 2 || resp.Usage.CompletionTokens != 3 || resp.Usage.TotalTokens != 10 {
		t.Fatalf("usage = %#v", resp.Usage)
	}
	if resp.ServiceTier != "priority" {
		t.Fatalf("service_tier = %q, want priority", resp.ServiceTier)
	}
	if len(resp.RawBody) != 0 {
		t.Fatalf("chat fallback should not expose responses raw body: %s", resp.RawBody)
	}
}

func TestOpenAIUsagePayloadIncludesCacheCreationAndImageOutputTokens(t *testing.T) {
	chatUsage := usageFromOpenAIChatPayload(openAIUsagePayload{
		PromptTokens:             12,
		CompletionTokens:         5,
		TotalTokens:              20,
		CacheCreationInputTokens: 3,
		PromptTokensDetails:      openAITokenDetails{CachedTokens: 4},
		CompletionTokensDetails:  openAIOutputTokenDetails{ImageTokens: 2},
	})
	if chatUsage.PromptTokens != 12 || chatUsage.CachedPromptTokens != 4 || chatUsage.CacheCreationTokens != 3 || chatUsage.CompletionTokens != 5 || chatUsage.ImageOutputTokens != 2 || chatUsage.TotalTokens != 20 {
		t.Fatalf("chat usage = %#v", chatUsage)
	}

	responsesUsage := usageFromOpenAIResponsesPayload(openAIResponsesUsage{
		InputTokens:              30,
		OutputTokens:             9,
		TotalTokens:              44,
		CacheCreationInputTokens: 5,
		InputTokensDetails:       openAITokenDetails{CachedTokens: 7},
		OutputTokensDetails:      openAIOutputTokenDetails{ImageTokens: 4},
	})
	if responsesUsage.PromptTokens != 30 || responsesUsage.CachedPromptTokens != 7 || responsesUsage.CacheCreationTokens != 5 || responsesUsage.CompletionTokens != 9 || responsesUsage.ImageOutputTokens != 4 || responsesUsage.TotalTokens != 44 {
		t.Fatalf("responses usage = %#v", responsesUsage)
	}
}

func TestClaudeUsagePayloadTreatsCacheReadAsPromptTokens(t *testing.T) {
	usage := usageFromAnthropicPayload(anthropicUsagePayload{
		InputTokens:              100,
		OutputTokens:             25,
		CacheReadInputTokens:     40,
		CacheCreationInputTokens: 10,
		CacheCreation: anthropicCacheCreation{
			Ephemeral5mInputTokens: 6,
			Ephemeral1hInputTokens: 4,
		},
	})
	if usage.PromptTokens != 140 {
		t.Fatalf("prompt tokens = %d, want input + cache read", usage.PromptTokens)
	}
	if usage.CachedPromptTokens != 40 || usage.CacheCreationTokens != 10 || usage.CacheCreation5mTokens != 6 || usage.CacheCreation1hTokens != 4 || usage.CompletionTokens != 25 || usage.TotalTokens != 175 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestOpenAIAdapterReturnsRawFailedResponsesObject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_failed","object":"response","created_at":1710000000,"status":"failed","model":"gpt-4.1","error":{"type":"invalid_request_error","code":"bad_tool_output","message":"tool output was invalid"},"usage":{"input_tokens":3,"output_tokens":0,"total_tokens":3}}`))
	}))
	defer server.Close()

	resp, err := (&OpenAIAdapter{}).InvokeResponses(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "openai-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "gpt-4.1",
	}, &core.ResponsesRequest{
		Model:   "gpt-4.1",
		RawBody: json.RawMessage(`{"model":"gpt-4.1","input":"hello"}`),
	})
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
	if resp.FinishReason != "failed" || resp.Content != "tool output was invalid" {
		t.Fatalf("response = %#v", resp)
	}
	if !json.Valid(resp.RawBody) || !strings.Contains(string(resp.RawBody), `"status":"failed"`) {
		t.Fatalf("raw body = %s", string(resp.RawBody))
	}
}

func TestOpenAIAdapterInvokeResponsesRejectsNewAPITopLevelErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"message":"model access denied","type":"invalid_request_error","code":"model_not_found"}}`))
	}))
	defer server.Close()

	_, err := (&OpenAIAdapter{}).InvokeResponses(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_new_api",
			Label:    "new-api",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "new-api-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "gpt-4.1",
	}, &core.ResponsesRequest{
		Model:   "gpt-4.1",
		RawBody: json.RawMessage(`{"model":"gpt-4.1","input":"hello"}`),
	})
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("err = %T %v, want InvokeError", err, err)
	}
	if invokeErr.Code != "upstream_rejected" || !strings.Contains(invokeErr.Error(), "model access denied") {
		t.Fatalf("invoke error = %#v", invokeErr)
	}
}

func TestOpenAIAdapterInvokeResponsesCompactRejectsNewAPITopLevelErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses/compact" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"message":"compact unavailable","type":"new_api_error","code":"bad_response"}}`))
	}))
	defer server.Close()

	_, err := (&OpenAIAdapter{}).InvokeResponses(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_new_api",
			Label:    "new-api",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "new-api-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "gpt-4.1",
	}, &core.ResponsesRequest{
		Model:   "gpt-4.1",
		Compact: true,
		RawBody: json.RawMessage(`{"model":"gpt-4.1","input":"compact me"}`),
	})
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("err = %T %v, want InvokeError", err, err)
	}
	if invokeErr.Code != "upstream_rejected" || !strings.Contains(invokeErr.Error(), "compact unavailable") {
		t.Fatalf("invoke error = %#v", invokeErr)
	}
}

func TestOpenAIResponsesPayloadFromResponsesRequestPreservesRawFields(t *testing.T) {
	payload, err := openAIResponsesRawPayloadFromResponses("gpt-4.1", &core.ResponsesRequest{
		Model:              "gpt-4.1",
		Stream:             true,
		ServiceTier:        "fast",
		PreviousResponseID: "resp_prev",
		RawBody: json.RawMessage(`{
			"model":"gpt-4.1",
			"input":"hello",
			"tools":[{"type":"web_search_preview"}],
			"tool_choice":"auto",
			"reasoning":{"effort":"low"}
		}`),
	})
	if err != nil {
		t.Fatalf("openAIResponsesRawPayloadFromResponses returned error: %v", err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if body["previous_response_id"] != "resp_prev" || body["tool_choice"] != "auto" {
		t.Fatalf("payload = %#v", body)
	}
	if body["service_tier"] != "fast" {
		t.Fatalf("service_tier = %#v", body["service_tier"])
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", body["tools"])
	}
	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "low" {
		t.Fatalf("reasoning = %#v", body["reasoning"])
	}
}

func TestOpenAIResponsesPayloadFiltersInternalRouteMetadata(t *testing.T) {
	payload, err := openAIResponsesRawPayloadFromResponses("gpt-5.5", &core.ResponsesRequest{
		Model:          "gpt-5.5",
		PromptCacheKey: "public-cache",
		Metadata: map[string]string{
			"user":                   "visible",
			"route_affinity_key":     "session-id:sess_123",
			"route_affinity_model":   "gpt-5.5",
			"cache_affinity_key":     "private-cache",
			"prompt_cache_key":       "private-prompt-cache",
			"anthropic_version":      "2023-06-01",
			"anthropic_beta":         "tools-2024-04-04",
			"x-client-request-id":    "claude_req",
			"x-claude-code-agent-id": "agent_1",
		},
		RawBody: json.RawMessage(`{
			"model":"gpt-5.5",
			"input":"hello",
			"route_affinity_key":"raw-route",
			"cache_affinity_key":"raw-cache",
			"route_affinity_model":"raw-model",
			"metadata":{"raw":"metadata","route_affinity_key":"raw-route"}
		}`),
	})
	if err != nil {
		t.Fatalf("openAIResponsesRawPayloadFromResponses returned error: %v", err)
	}
	for _, key := range []string{"route_affinity_key", "cache_affinity_key", "route_affinity_model"} {
		if _, exists := payload[key]; exists {
			t.Fatalf("%s should not be forwarded: %#v", key, payload)
		}
	}
	if payload["prompt_cache_key"] != "public-cache" {
		t.Fatalf("prompt_cache_key = %#v", payload["prompt_cache_key"])
	}
	metadata, ok := payload["metadata"].(map[string]string)
	if !ok {
		t.Fatalf("metadata = %#v", payload["metadata"])
	}
	if len(metadata) != 1 || metadata["user"] != "visible" {
		t.Fatalf("metadata = %#v", metadata)
	}
	for _, key := range []string{"anthropic_version", "anthropic_beta", "x-client-request-id", "x-claude-code-agent-id"} {
		if _, exists := metadata[key]; exists {
			t.Fatalf("metadata.%s should be filtered: %#v", key, metadata)
		}
	}
}

func TestOpenAIChatCompletionPayloadFiltersInternalRouteMetadata(t *testing.T) {
	payload := openAIChatCompletionPayload("gpt-4.1", &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Metadata: map[string]string{
			"user":                 "visible",
			"route_affinity_key":   "session-id:sess_123",
			"route_affinity_model": "gpt-4.1",
			"cache_affinity_key":   "private-cache",
			"prompt_cache_key":     "private-prompt-cache",
		},
	}, false)

	if len(payload.Metadata) != 1 || payload.Metadata["user"] != "visible" {
		t.Fatalf("metadata = %#v", payload.Metadata)
	}
}

func TestOpenAIChatCompletionPayloadForSub2APIInjectsPromptCacheKeyFromRouteAffinity(t *testing.T) {
	payload := openAIChatCompletionPayloadForAccount("gpt-4.1", &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Metadata: map[string]string{"route_affinity_key": "session-id:sess_123"},
	}, false, sub2APITestAccount("https://sub2api.example"))

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if body["prompt_cache_key"] != "sess_123" {
		t.Fatalf("prompt_cache_key = %#v body=%#v", body["prompt_cache_key"], body)
	}
}

func TestOpenAIChatCompletionPayloadForSub2APIDoesNotUseClientIDAsPromptCacheKey(t *testing.T) {
	payload := openAIChatCompletionPayloadForAccount("gpt-4.1", &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Client:   &core.APIClient{ID: "client_pointer"},
		Metadata: map[string]string{"client_id": "client_metadata"},
	}, false, sub2APITestAccount("https://sub2api.example"))

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if _, exists := body["prompt_cache_key"]; exists {
		t.Fatalf("prompt_cache_key should not be derived from client id: %#v", body)
	}
}

func TestOpenAIChatCompletionPayloadDoesNotInjectInternalCacheAffinityForOfficialAccount(t *testing.T) {
	payload := openAIChatCompletionPayloadForAccount("gpt-4.1", &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Metadata: map[string]string{"cache_affinity_key": "private-cache", "route_affinity_key": "session-id:sess_123"},
	}, false, core.Account{Provider: core.ProviderOpenAI})

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if _, exists := body["prompt_cache_key"]; exists {
		t.Fatalf("prompt_cache_key should not be injected for official account: %#v", body)
	}
}

func TestOpenAIChatCompletionPayloadPreservesExplicitPromptCacheKeyForOfficialAccount(t *testing.T) {
	payload := openAIChatCompletionPayloadForAccount("gpt-4.1", &core.GatewayRequest{
		Model:          "gpt-4.1",
		Messages:       []core.Message{{Role: "user", Content: "hello"}},
		PromptCacheKey: "explicit-cache",
		Metadata:       map[string]string{"route_affinity_key": "session-id:sess_123"},
	}, false, core.Account{Provider: core.ProviderOpenAI})

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if body["prompt_cache_key"] != "explicit-cache" {
		t.Fatalf("prompt_cache_key = %#v body=%#v", body["prompt_cache_key"], body)
	}
}

func TestOpenAIResponsesPayloadForSub2APIUsesAnthropicCacheControlBeforeRouteAffinity(t *testing.T) {
	metadata := map[string]string{"route_affinity_key": "session-id:sess_123"}
	rawBody := json.RawMessage(`{
		"model":"gpt-5.5",
		"stream":true,
		"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]}]
	}`)
	account := sub2APITestAccount("https://sub2api.example")
	responsesReq, err := responsesRequestFromAnthropicGatewayRequest(account, &core.GatewayRequest{
		Model:        "gpt-5.5",
		RawBody:      rawBody,
		Stream:       true,
		UpstreamMode: "anthropic_messages",
		Metadata:     metadata,
	}, core.ResponsesTransportSSE)
	if err != nil {
		t.Fatalf("responsesRequestFromAnthropicGatewayRequest returned error: %v", err)
	}
	payload, err := openAIResponsesRawPayloadFromResponsesForAccount("gpt-5.5", responsesReq, account)
	if err != nil {
		t.Fatalf("openAIResponsesRawPayloadFromResponsesForAccount returned error: %v", err)
	}
	if got := payload["prompt_cache_key"]; got == nil {
		t.Fatalf("prompt_cache_key missing: %#v", payload)
	}
	key, _ := payload["prompt_cache_key"].(string)
	if !strings.HasPrefix(key, "anthropic-cache-") {
		t.Fatalf("prompt_cache_key = %q, want anthropic-cache-*; payload=%#v", key, payload)
	}
	if key == "sess_123" {
		t.Fatalf("prompt_cache_key should not fall back to route affinity: %#v", payload)
	}
}

func TestOpenAIAdapterForwardsSessionHeadersToSub2APIChat(t *testing.T) {
	var firstUserID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("session_id") != "sess_123" || r.Header.Get("conversation_id") != "sess_123" {
			t.Fatalf("session headers = session_id %q conversation_id %q", r.Header.Get("session_id"), r.Header.Get("conversation_id"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["prompt_cache_key"] != "sess_123" {
			t.Fatalf("prompt_cache_key = %#v body=%#v", body["prompt_cache_key"], body)
		}
		metadata, _ := body["metadata"].(map[string]any)
		if _, exists := metadata["route_affinity_key"]; exists {
			t.Fatalf("metadata leaked route_affinity_key: %#v", metadata)
		}
		userID, _ := metadata["user_id"].(string)
		if !strings.HasPrefix(userID, "user_") || !strings.Contains(userID, "_account__session_") {
			t.Fatalf("metadata.user_id = %#v", userID)
		}
		if firstUserID == "" {
			firstUserID = userID
		} else if userID != firstUserID {
			t.Fatalf("metadata.user_id changed across same session: %q != %q", userID, firstUserID)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl_sub2api",
			"model":   "gpt-4.1",
			"choices": []map[string]any{{"message": map[string]any{"content": "ok"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	defer server.Close()

	adapter := &OpenAIAdapter{}
	resp, err := adapter.Invoke(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Model:    "gpt-4.1",
		Account:  sub2APITestAccount(server.URL),
	}, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Metadata: map[string]string{"route_affinity_key": "session-id:sess_123"},
	})
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
	if resp == nil || resp.Content != "ok" {
		t.Fatalf("resp = %#v", resp)
	}
	resp, err = adapter.Invoke(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Model:    "gpt-4.1",
		Account:  sub2APITestAccount(server.URL),
	}, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello again"}},
		Metadata: map[string]string{"route_affinity_key": "session-id:sess_123"},
	})
	if err != nil {
		t.Fatalf("second Invoke returned error: %v", err)
	}
	if resp == nil || resp.Content != "ok" {
		t.Fatalf("second resp = %#v", resp)
	}
}

func TestOpenAIAdapterForwardsSessionHeadersToSub2APIResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("session_id") != "sess_456" || r.Header.Get("conversation_id") != "sess_456" {
			t.Fatalf("session headers = session_id %q conversation_id %q", r.Header.Get("session_id"), r.Header.Get("conversation_id"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["prompt_cache_key"] != "sess_456" {
			t.Fatalf("prompt_cache_key = %#v body=%#v", body["prompt_cache_key"], body)
		}
		metadata, _ := body["metadata"].(map[string]any)
		userID, _ := metadata["user_id"].(string)
		if !strings.HasPrefix(userID, "user_") || !strings.Contains(userID, "_account__session_") {
			t.Fatalf("metadata.user_id = %#v body=%#v", userID, body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "resp_sub2api",
			"object":      "response",
			"model":       "gpt-4.1",
			"status":      "completed",
			"output_text": "ok",
			"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
		})
	}))
	defer server.Close()

	adapter := &OpenAIAdapter{}
	resp, err := adapter.InvokeResponses(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Model:    "gpt-4.1",
		Account:  sub2APITestAccount(server.URL),
	}, &core.ResponsesRequest{
		Model:    "gpt-4.1",
		RawBody:  json.RawMessage(`{"model":"gpt-4.1","input":"hello"}`),
		Metadata: map[string]string{"route_affinity_key": "thread-id:sess_456"},
	})
	if err != nil {
		t.Fatalf("InvokeResponses returned error: %v", err)
	}
	if resp == nil || resp.Content != "ok" {
		t.Fatalf("resp = %#v", resp)
	}
}

func TestOpenAIAdapterForwardsCodexHeadersToResponsesHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s, want /v1/responses", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer openai-token" {
			t.Fatalf("authorization = %q, want Bearer openai-token", got)
		}
		for key, want := range map[string]string{
			"session-id":                             "sess_http",
			"x-codex-window-id":                      "window-http",
			"x-openai-subagent":                      "delegate-1",
			"x-openai-internal-codex-responses-lite": "true",
			"User-Agent":                             "codex_cli_rs/0.999.0",
			"originator":                             "codex_cli_rs",
			"Accept-Language":                        "zh-CN",
			"x-client-request-id":                    "req_http",
			"Authorization":                          "Bearer openai-token",
		} {
			if got := r.Header.Get(key); got != want {
				t.Fatalf("%s = %q, want %q", key, got, want)
			}
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["input"] != "hello" {
			t.Fatalf("input = %#v", body["input"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "resp_http_headers",
			"object":      "response",
			"model":       "gpt-4.1",
			"status":      "completed",
			"output_text": "ok",
			"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
		})
	}))
	defer server.Close()

	resp, err := (&OpenAIAdapter{}).InvokeResponses(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Model:    "gpt-4.1",
		Account: core.Account{
			ID:       "acct_openai_http",
			Label:    "OpenAI HTTP",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "openai-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
	}, &core.ResponsesRequest{
		Model:   "gpt-4.1",
		RawBody: json.RawMessage(`{"model":"gpt-4.1","input":"hello"}`),
		Headers: map[string]string{
			"session-id":                             "sess_http",
			"x-codex-window-id":                      "window-http",
			"x-openai-subagent":                      "delegate-1",
			"x-openai-internal-codex-responses-lite": "true",
			"user-agent":                             "codex_cli_rs/0.999.0",
			"originator":                             "codex_cli_rs",
			"accept-language":                        "zh-CN",
			"x-client-request-id":                    "req_http",
			"authorization":                          "Bearer client-token",
		},
	})
	if err != nil {
		t.Fatalf("InvokeResponses returned error: %v", err)
	}
	if resp == nil || resp.Content != "ok" {
		t.Fatalf("resp = %#v", resp)
	}
}

func TestOpenAIAdapterForwardsSessionToSub2APIChatStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("session_id") != "sess_stream" || r.Header.Get("conversation_id") != "sess_stream" {
			t.Fatalf("session headers = session_id %q conversation_id %q", r.Header.Get("session_id"), r.Header.Get("conversation_id"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["prompt_cache_key"] != "sess_stream" {
			t.Fatalf("prompt_cache_key = %#v body=%#v", body["prompt_cache_key"], body)
		}
		metadata, _ := body["metadata"].(map[string]any)
		if userID, _ := metadata["user_id"].(string); !strings.HasPrefix(userID, "user_") {
			t.Fatalf("metadata.user_id = %#v body=%#v", userID, body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	session, err := (&OpenAIAdapter{}).OpenStream(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Model:    "gpt-4.1",
		Account:  sub2APITestAccount(server.URL),
	}, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Stream:   true,
		Metadata: map[string]string{"route_affinity_key": "session-id:sess_stream"},
	})
	if err != nil {
		t.Fatalf("OpenStream returned error: %v", err)
	}
	defer session.Stream.Close()
	if event, err := session.Stream.Next(); err != nil || event.Delta != "ok" {
		t.Fatalf("event = %#v err=%v", event, err)
	}
}

func TestOpenAIResponsesCompactPayloadAddsSub2APIMetadataUserID(t *testing.T) {
	payload, err := openAIResponsesCompactPayloadFromResponsesForAccount("gpt-4.1", &core.ResponsesRequest{
		Model:    "gpt-4.1",
		RawBody:  json.RawMessage(`{"model":"gpt-4.1","input":"hello"}`),
		Metadata: map[string]string{"route_affinity_key": "session-id:sess_compact"},
	}, sub2APITestAccount("https://sub2api.example"))
	if err != nil {
		t.Fatalf("openAIResponsesCompactPayloadFromResponsesForAccount returned error: %v", err)
	}
	metadata, _ := payload["metadata"].(map[string]string)
	if userID := metadata["user_id"]; !strings.HasPrefix(userID, "user_") {
		t.Fatalf("metadata.user_id = %#v payload=%#v", userID, payload)
	}
}

func TestOpenAISub2APISessionHeadersOnlyForSub2APIAccounts(t *testing.T) {
	headers := openAISub2APISessionHeaders(core.Account{
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:     "manual-token",
			Metadata: map[string]string{"base_url": "https://api.openai.com", "api_key_quota_provider": "sub2api", "account_login_method": "api_key"},
		},
	}, map[string]string{"route_affinity_key": "session-id:sess_789"})
	if len(headers) != 0 {
		t.Fatalf("headers = %#v, want none for official OpenAI base URL", headers)
	}
	metadata := openAIUserMetadataForAccount(sub2APITestAccount("https://sub2api.example"), map[string]string{"route_affinity_key": "session-id:sess_789"})
	if metadata["user_id"] == "" {
		t.Fatalf("metadata.user_id not generated: %#v", metadata)
	}
	officialMetadata := openAIUserMetadataForAccount(core.Account{
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:     "manual-token",
			Metadata: map[string]string{"base_url": "https://api.openai.com", "api_key_quota_provider": "sub2api", "account_login_method": "api_key"},
		},
	}, map[string]string{"route_affinity_key": "session-id:sess_789"})
	if len(officialMetadata) != 0 {
		t.Fatalf("official metadata = %#v, want none", officialMetadata)
	}
}

func sub2APITestAccount(baseURL string) core.Account {
	return core.Account{
		ID:       "acct_sub2api",
		Label:    "Sub2API",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "sub2api-token",
			Metadata: map[string]string{
				"base_url":               baseURL,
				"api_key_quota_provider": "sub2api",
				"account_login_method":   "api_key",
			},
		},
	}
}

func TestOpenAIResponsesPayloadFromResponsesRequestPreservesStructuredInput(t *testing.T) {
	payload, err := openAIResponsesRawPayloadFromResponses("gpt-4.1", &core.ResponsesRequest{
		Model:   "gpt-4.1",
		RawBody: json.RawMessage(`{"input":[{"type":"function_call_output","call_id":"call_1","output":"42"}]}`),
	})
	if err != nil {
		t.Fatalf("openAIResponsesRawPayloadFromResponses returned error: %v", err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	input, _ := body["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %#v", body["input"])
	}
	item, _ := input[0].(map[string]any)
	if item["type"] != "function_call_output" || item["call_id"] != "call_1" {
		t.Fatalf("input item = %#v", item)
	}
}

func TestChatGPTCodexResponsesPayloadNormalizesStringInputToList(t *testing.T) {
	payload, err := chatGPTCodexRawResponsesPayloadFromResponses("gpt-4.1", &core.ResponsesRequest{
		Model:   "gpt-4.1",
		RawBody: json.RawMessage(`{"model":"gpt-4.1","input":"hello"}`),
	})
	if err != nil {
		t.Fatalf("chatGPTCodexRawResponsesPayloadFromResponses returned error: %v", err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	input, _ := body["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %#v", body["input"])
	}
	item, _ := input[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "user" {
		t.Fatalf("input item = %#v", item)
	}
	content, _ := item["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %#v", item["content"])
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "input_text" || block["text"] != "hello" {
		t.Fatalf("content block = %#v", block)
	}
}

func TestOpenAIResponsesRawPayloadOverlaysServiceTier(t *testing.T) {
	payload, err := openAIResponsesRawPayloadFromResponses("gpt-5.5", &core.ResponsesRequest{
		Model:       "gpt-5.5",
		ServiceTier: "fast",
		RawBody:     json.RawMessage(`{"model":"gpt-5.5","input":"hello","service_tier":"default"}`),
	})
	if err != nil {
		t.Fatalf("openAIResponsesRawPayloadFromResponses returned error: %v", err)
	}
	if payload["service_tier"] != "fast" {
		t.Fatalf("service_tier = %#v, want fast", payload["service_tier"])
	}
	if payload["model"] != "gpt-5.5" {
		t.Fatalf("model = %#v", payload["model"])
	}
}

func TestOpenAIResponsesCompactPayloadSkipsNullFields(t *testing.T) {
	payload, err := openAIResponsesCompactPayloadFromResponses("gpt-5.5", &core.ResponsesRequest{
		Model: "gpt-5.5",
		RawBody: json.RawMessage(`{
			"model":"gpt-5.5",
			"input":"compact me",
			"tools":null,
			"parallel_tool_calls":null,
			"reasoning":null,
			"text":null
		}`),
	})
	if err != nil {
		t.Fatalf("openAIResponsesCompactPayloadFromResponses returned error: %v", err)
	}
	if _, ok := payload["input"].([]any); !ok {
		t.Fatalf("input = %#v", payload["input"])
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 0 {
		t.Fatalf("tools = %#v", payload["tools"])
	}
	if payload["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %#v", payload["parallel_tool_calls"])
	}
	for _, field := range []string{"reasoning", "text"} {
		if _, exists := payload[field]; exists {
			t.Fatalf("%s should be omitted when null: %#v", field, payload)
		}
	}
}

func TestOpenAIAdapterPreservesChatCompletionExtraFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		messages, ok := body["messages"].([]any)
		if !ok || len(messages) != 1 {
			t.Fatalf("messages = %#v", body["messages"])
		}
		firstMessage, _ := messages[0].(map[string]any)
		content, ok := firstMessage["content"].([]any)
		if !ok || len(content) != 2 {
			t.Fatalf("message content = %#v", firstMessage["content"])
		}
		if _, ok := body["tools"].([]any); !ok {
			t.Fatalf("tools = %#v", body["tools"])
		}
		if body["tool_choice"] != "auto" {
			t.Fatalf("tool_choice = %#v", body["tool_choice"])
		}
		responseFormat, _ := body["response_format"].(map[string]any)
		if responseFormat["type"] != "json_object" {
			t.Fatalf("response_format = %#v", body["response_format"])
		}
		if body["max_completion_tokens"] != float64(64) {
			t.Fatalf("max_completion_tokens = %#v", body["max_completion_tokens"])
		}
		if body["service_tier"] != "fast" {
			t.Fatalf("service_tier = %#v", body["service_tier"])
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl_test",
			"model":   "gpt-4.1",
			"created": 1710000000,
			"choices": []map[string]any{
				{"message": map[string]any{"content": "{}"}, "finish_reason": "stop"},
			},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5},
		})
	}))
	defer server.Close()

	adapter := &OpenAIAdapter{}
	_, err := adapter.Invoke(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "openai-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "gpt-4.1",
	}, &core.GatewayRequest{
		Model:       "gpt-4.1",
		Messages:    []core.Message{{Role: "user", Content: "describe"}},
		RawMessages: json.RawMessage(`[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]`),
		ServiceTier: "fast",
		Extra: map[string]json.RawMessage{
			"tools":                 json.RawMessage(`[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]`),
			"tool_choice":           json.RawMessage(`"auto"`),
			"response_format":       json.RawMessage(`{"type":"json_object"}`),
			"max_completion_tokens": json.RawMessage(`64`),
		},
	})
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
}

func TestOpenAIAdapterAlignsReasoningModelRequestShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if _, exists := body["max_tokens"]; exists {
			t.Fatalf("max_tokens should be omitted for gpt-5 request: %#v", body)
		}
		if body["max_completion_tokens"] != float64(128) {
			t.Fatalf("max_completion_tokens = %#v", body["max_completion_tokens"])
		}
		if _, exists := body["temperature"]; exists {
			t.Fatalf("temperature should be omitted for gpt-5 request: %#v", body)
		}
		if _, exists := body["top_p"]; exists {
			t.Fatalf("top_p should be omitted for gpt-5 request: %#v", body)
		}
		if body["reasoning_effort"] != "high" {
			t.Fatalf("reasoning_effort = %#v", body["reasoning_effort"])
		}
		messages, ok := body["messages"].([]any)
		if !ok || len(messages) == 0 {
			t.Fatalf("messages = %#v", body["messages"])
		}
		first, _ := messages[0].(map[string]any)
		if first["role"] != "developer" {
			t.Fatalf("first role = %#v, want developer", first["role"])
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl_reasoning",
			"model":   "gpt-5.4",
			"created": 1710000000,
			"choices": []map[string]any{
				{"message": map[string]any{"content": "ok"}, "finish_reason": "stop"},
			},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5},
		})
	}))
	defer server.Close()

	maxTokens := 128
	temperature := 0.7
	topP := 0.9
	_, err := (&OpenAIAdapter{}).Invoke(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "openai-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "gpt-5.4",
	}, &core.GatewayRequest{
		Model:           "gpt-5.4",
		RawMessages:     json.RawMessage(`[{"role":"system","content":"be precise"},{"role":"user","content":"hi"}]`),
		MaxTokens:       &maxTokens,
		Temperature:     &temperature,
		TopP:            &topP,
		ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
}

func TestOpenAIAdapterEmbedUsesRealHTTPShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer openai-token" {
			t.Fatalf("authorization = %q", got)
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["model"] != "text-embedding-3-large" {
			t.Fatalf("model = %v", body["model"])
		}
		if body["encoding_format"] != "float" {
			t.Fatalf("encoding_format = %v", body["encoding_format"])
		}
		if body["dimensions"] != float64(3) {
			t.Fatalf("dimensions = %v", body["dimensions"])
		}
		if body["user"] != "user-123" {
			t.Fatalf("user = %v", body["user"])
		}

		input, ok := body["input"].([]any)
		if !ok || len(input) != 2 || input[0] != "alpha" || input[1] != "beta" {
			t.Fatalf("input = %#v", body["input"])
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"model":  "text-embedding-3-large",
			"data": []map[string]any{
				{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2, 0.3}},
				{"object": "embedding", "index": 1, "embedding": []float64{0.4, 0.5, 0.6}},
			},
			"usage": map[string]any{
				"prompt_tokens": 8,
				"total_tokens":  8,
			},
		})
	}))
	defer server.Close()

	adapter := &OpenAIAdapter{}
	resp, err := adapter.Embed(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "openai-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "text-embedding-3-large",
	}, &core.EmbeddingRequest{
		Model:          "text-embedding-3-large",
		Input:          []string{"alpha", "beta"},
		EncodingFormat: "float",
		Dimensions:     intPtr(3),
		User:           "user-123",
	})
	if err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}
	if len(resp.RawBody) == 0 {
		t.Fatalf("RawBody should preserve upstream embeddings response")
	}
	if !strings.Contains(string(resp.RawBody), `"embedding":[0.1,0.2,0.3]`) {
		t.Fatalf("RawBody = %s", resp.RawBody)
	}
	if resp.Usage.TotalTokens != 8 {
		t.Fatalf("total_tokens = %d", resp.Usage.TotalTokens)
	}
}

func TestOpenAIAdapterFetchModelsUsesOpenAIAPIForAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer openai-token" {
			t.Fatalf("authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "gpt-api", "owned_by": "openai"},
			},
		})
	}))
	defer server.Close()

	models, err := (&OpenAIAdapter{}).FetchModels(context.Background(), core.Account{
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": server.URL},
		},
	})
	if err != nil {
		t.Fatalf("FetchModels returned error: %v", err)
	}
	if len(models) != 1 || models[0].ID != "gpt-api" || models[0].OwnedBy != "openai" {
		t.Fatalf("models = %#v", models)
	}
}

func TestOpenAIAdapterFetchModelsUsesChatGPTCodexBackendForOAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("client_version"); got == "" {
			t.Fatalf("client_version query missing: %s", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-token" {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct_chatgpt" {
			t.Fatalf("ChatGPT-Account-ID = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"slug": "gpt-codex", "display_name": "GPT Codex"},
			},
		})
	}))
	defer server.Close()

	models, err := (&OpenAIAdapter{}).FetchModels(context.Background(), core.Account{
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			AccessToken: "oauth-token",
			Metadata: map[string]string{
				"token_source":     OpenAIDeviceCodeTokenSourceValue(),
				"oauth_account_id": "acct_chatgpt",
				"codex_base_url":   server.URL,
			},
		},
	})
	if err != nil {
		t.Fatalf("FetchModels returned error: %v", err)
	}
	if len(models) != 1 || models[0].ID != "gpt-codex" || models[0].DisplayName != "GPT Codex" || models[0].OwnedBy != "openai" {
		t.Fatalf("models = %#v", models)
	}
}

func TestOpenAIAdapterFetchModelsUsesCodexForFreeCodexAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("client_version"); got == "" {
			t.Fatalf("client_version query missing: %s", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-token" {
			t.Fatalf("authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"slug": "gpt-5-codex", "display_name": "GPT-5 Codex", "owned_by": "openai"},
			},
		})
	}))
	defer server.Close()

	models, err := (&OpenAIAdapter{}).FetchModels(context.Background(), core.Account{
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			AccessToken: "oauth-token",
			Metadata: map[string]string{
				"token_source":   OpenAICodexAuthTokenSourceValue(),
				"account_type":   "free",
				"codex_base_url": server.URL,
			},
		},
	})
	if err != nil {
		t.Fatalf("FetchModels returned error: %v", err)
	}
	if len(models) != 1 || models[0].ID != "gpt-5-codex" || models[0].DisplayName != "GPT-5 Codex" {
		t.Fatalf("models = %#v", models)
	}
}

func TestIsOpenAIChatGPTFreeAccount(t *testing.T) {
	quotaMetadata := func(snapshot core.AccountQuotaSnapshot) map[string]string {
		t.Helper()
		raw, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		return map[string]string{
			core.AccountQuotaMetadataKey: string(raw),
		}
	}

	if !IsOpenAIChatGPTFreeAccount(core.Account{
		Provider:   core.ProviderOpenAI,
		Credential: core.Credential{Metadata: quotaMetadata(core.AccountQuotaSnapshot{Source: openAIUsageDefaultSource, Plan: "free"})},
	}) {
		t.Fatal("expected refreshed free openai account to be detected")
	}
	if IsOpenAIChatGPTFreeAccount(core.Account{
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Metadata: map[string]string{
				"token_source": OpenAICodexAuthTokenSourceValue(),
				"account_type": "free",
			},
		},
	}) {
		t.Fatal("expected unrefreshed codex auth account to not be assumed free")
	}
	if IsOpenAIChatGPTFreeAccount(core.Account{
		Provider:   core.ProviderOpenAI,
		Credential: core.Credential{Metadata: quotaMetadata(core.AccountQuotaSnapshot{Source: openAIUsageDefaultSource, Plan: "plus"})},
	}) {
		t.Fatal("expected paid plan to not be detected as free")
	}
	if IsOpenAIChatGPTFreeAccount(core.Account{
		Provider:   core.ProviderClaude,
		Credential: core.Credential{Metadata: quotaMetadata(core.AccountQuotaSnapshot{Source: openAIUsageDefaultSource, Plan: "free"})},
	}) {
		t.Fatal("expected non-openai account to not be detected as free")
	}
}

func TestOpenAIAdapterInvokeUsesChatGPTCodexResponsesForOAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("client_version"); got == "" {
			t.Fatalf("client_version query missing: %s", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-token" {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct_chatgpt" {
			t.Fatalf("ChatGPT-Account-ID = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Fatalf("accept = %q", got)
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["model"] != "gpt-5.4" {
			t.Fatalf("model = %v", body["model"])
		}
		if body["stream"] != true {
			t.Fatalf("stream = %v", body["stream"])
		}
		if _, exists := body["client_metadata"]; exists {
			t.Fatalf("codex chat request should not synthesize client_metadata: %#v", body)
		}
		if body["instructions"] == "" {
			t.Fatalf("instructions missing")
		}
		input, ok := body["input"].([]any)
		if !ok || len(input) != 1 {
			t.Fatalf("input = %#v", body["input"])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello codex\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_codex\",\"model\":\"gpt-5.4\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n")
	}))
	defer server.Close()

	resp, err := (&OpenAIAdapter{}).Invoke(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI OAuth",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "oauth-token",
				Metadata: map[string]string{
					"token_source":     OpenAIDeviceCodeTokenSourceValue(),
					"oauth_account_id": "acct_chatgpt",
					"codex_base_url":   server.URL,
				},
			},
		},
		Model: "gpt-5.4",
	}, &core.GatewayRequest{
		Model:    "gpt-5.4",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
	if resp.ID != "resp_codex" || resp.Content != "hello codex" {
		t.Fatalf("response = %#v", resp)
	}
	if resp.Usage.TotalTokens != 10 {
		t.Fatalf("usage = %#v", resp.Usage)
	}
}

func TestOpenAIAdapterInvokeUsesChatGPTCodexResponsesForTokenLogin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("client_version"); got == "" {
			t.Fatalf("client_version query missing: %s", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-login-token" {
			t.Fatalf("authorization = %q", got)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello token\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_token_login\",\"model\":\"gpt-5.4\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n")
	}))
	defer server.Close()

	resp, err := (&OpenAIAdapter{}).Invoke(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI Token Login",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "token-login-token",
				Metadata: map[string]string{
					"account_login_method": "token",
					"codex_base_url":       server.URL,
				},
			},
		},
		Model: "gpt-5.4",
	}, &core.GatewayRequest{
		Model:    "gpt-5.4",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
	if resp.ID != "resp_token_login" || resp.Content != "hello token" {
		t.Fatalf("response = %#v", resp)
	}
}

func TestOpenAIAdapterFreeCodexAuthChatUsesCodexBackend(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("client_version"); got == "" {
			t.Fatalf("client_version query missing: %s", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-token" {
			t.Fatalf("authorization = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["model"] != "gpt-5-codex" {
			t.Fatalf("model = %#v", body["model"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello free codex\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_free_codex\",\"model\":\"gpt-5-codex\",\"usage\":{\"input_tokens\":5,\"output_tokens\":4,\"total_tokens\":9}}}\n\n")
	}))
	defer server.Close()

	resp, err := (&OpenAIAdapter{}).Invoke(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI Free",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "oauth-token",
				Metadata: map[string]string{
					"token_source":   OpenAICodexAuthTokenSourceValue(),
					"account_type":   "free",
					"codex_base_url": server.URL,
				},
			},
		},
		Model: "gpt-5-codex",
	}, &core.GatewayRequest{
		Model:    "gpt-5-codex",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
	if resp.ID != "resp_free_codex" || resp.Content != "hello free codex" || resp.Usage.TotalTokens != 9 {
		t.Fatalf("response = %#v", resp)
	}
}

func TestOpenAIAdapterPreservesRawResponsesPayloadForCodexOAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-token" {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct_chatgpt" {
			t.Fatalf("ChatGPT-Account-ID = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "codex-cli-test/1.0" {
			t.Fatalf("User-Agent = %q", got)
		}
		if got := r.Header.Get("originator"); got != "codex_cli_rs" {
			t.Fatalf("originator = %q", got)
		}
		if got := r.Header.Get("x-codex-window-id"); got != "window-http-codex" {
			t.Fatalf("x-codex-window-id = %q", got)
		}
		if got := r.Header.Get("x-codex-turn-metadata"); got != `{"turn_id":"turn-http"}` {
			t.Fatalf("x-codex-turn-metadata = %q", got)
		}
		if got := r.Header.Get("x-openai-internal-codex-responses-lite"); got != "true" {
			t.Fatalf("x-openai-internal-codex-responses-lite = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["model"] != "gpt-5.5" {
			t.Fatalf("model = %#v", body["model"])
		}
		if body["stream"] != true {
			t.Fatalf("stream = %#v", body["stream"])
		}
		if body["store"] != false {
			t.Fatalf("store = %#v", body["store"])
		}
		if _, exists := body["max_output_tokens"]; exists {
			t.Fatalf("max_output_tokens should be removed for codex backend: %#v", body)
		}
		if _, exists := body["temperature"]; exists {
			t.Fatalf("temperature should be removed for codex backend: %#v", body)
		}
		if _, exists := body["top_p"]; exists {
			t.Fatalf("top_p should be removed for codex backend: %#v", body)
		}
		if _, exists := body["metadata"]; exists {
			t.Fatalf("metadata should be removed for codex backend: %#v", body)
		}
		clientMetadata, ok := body["client_metadata"].(map[string]any)
		if !ok {
			t.Fatalf("client_metadata = %#v", body["client_metadata"])
		}
		if clientMetadata["x"] != "y" {
			t.Fatalf("client_metadata.x = %#v", clientMetadata["x"])
		}
		if _, ok := body["tools"].([]any); !ok {
			t.Fatalf("tools = %#v", body["tools"])
		}
		if body["tool_choice"] != "auto" {
			t.Fatalf("tool_choice = %#v", body["tool_choice"])
		}
		include, ok := body["include"].([]any)
		if !ok || len(include) != 1 || include[0] != "reasoning.encrypted_content" {
			t.Fatalf("include = %#v", body["include"])
		}
		if body["prompt_cache_key"] != "cache-key" {
			t.Fatalf("prompt_cache_key = %#v", body["prompt_cache_key"])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_codex\",\"model\":\"gpt-5.5\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n")
	}))
	defer server.Close()

	_, err := (&OpenAIAdapter{}).InvokeResponses(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI OAuth",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "oauth-token",
				Metadata: map[string]string{
					"token_source":     OpenAIDeviceCodeTokenSourceValue(),
					"oauth_account_id": "acct_chatgpt",
					"codex_base_url":   server.URL,
				},
			},
		},
		Model: "gpt-5.5",
	}, &core.ResponsesRequest{
		Model:  "gpt-5.5",
		Stream: false,
		Headers: map[string]string{
			"user-agent":                             "codex-cli-test/1.0",
			"originator":                             "codex_cli_rs",
			"x-codex-window-id":                      "window-http-codex",
			"x-codex-turn-metadata":                  `{"turn_id":"turn-http"}`,
			"x-openai-internal-codex-responses-lite": "true",
			"authorization":                          "Bearer client-token",
		},
		RawBody: json.RawMessage(`{
			"model":"gpt-5.5",
			"stream":false,
			"input":[{"role":"user","content":[{"type":"input_text","text":"list files"}]}],
			"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],
			"tool_choice":"auto",
			"include":["reasoning.encrypted_content"],
			"prompt_cache_key":"cache-key",
			"max_output_tokens":1024,
			"temperature":0.2,
			"top_p":0.9,
			"metadata":{"client":"codex"},
			"client_metadata":{"x":"y"}
		}`),
	})
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
}

func TestOpenAIAdapterCodexCompactUsesCompactEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses/compact" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("client_version"); got == "" {
			t.Fatalf("client_version query missing: %s", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-token" {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct_chatgpt" {
			t.Fatalf("ChatGPT-Account-ID = %q", got)
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["model"] != "gpt-5.5" {
			t.Fatalf("model = %#v", body["model"])
		}
		if body["instructions"] != "compact instructions" {
			t.Fatalf("instructions = %#v", body["instructions"])
		}
		if _, exists := body["previous_response_id"]; exists {
			t.Fatalf("previous_response_id should not be sent to compact endpoint: %#v", body)
		}
		for _, forbidden := range []string{"stream", "store", "generate", "max_output_tokens", "temperature", "top_p", "metadata", "client_metadata"} {
			if _, exists := body[forbidden]; exists {
				t.Fatalf("%s should not be sent to compact endpoint: %#v", forbidden, body)
			}
		}
		if _, ok := body["input"].([]any); !ok {
			t.Fatalf("input = %#v", body["input"])
		}
		if _, ok := body["tools"].([]any); !ok {
			t.Fatalf("tools = %#v", body["tools"])
		}
		if body["parallel_tool_calls"] != false {
			t.Fatalf("parallel_tool_calls = %#v", body["parallel_tool_calls"])
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_compact","object":"response.compaction","model":"gpt-5.5","status":"completed","usage":{"input_tokens":11,"output_tokens":2,"total_tokens":13}}`))
	}))
	defer server.Close()

	resp, err := (&OpenAIAdapter{}).InvokeResponses(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI OAuth",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "oauth-token",
				Metadata: map[string]string{
					"token_source":     OpenAIDeviceCodeTokenSourceValue(),
					"oauth_account_id": "acct_chatgpt",
					"codex_base_url":   server.URL,
				},
			},
		},
		Model: "gpt-5.5",
	}, &core.ResponsesRequest{
		Model:   "gpt-5.5",
		Compact: true,
		RawBody: json.RawMessage(`{
			"model":"gpt-5.5",
			"input":"compact me",
			"instructions":"compact instructions",
			"tools":[],
			"parallel_tool_calls":false,
			"previous_response_id":"resp_prev",
			"stream":true,
			"store":false,
			"generate":false,
			"max_output_tokens":1024,
			"temperature":0.2,
			"top_p":0.9,
			"metadata":{"client":"codex"},
			"client_metadata":{"x":"y"}
		}`),
	})
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
	if resp.ID != "resp_compact" || resp.Usage.TotalTokens != 13 {
		t.Fatalf("response = %#v", resp)
	}
	if !strings.Contains(string(resp.RawBody), `"response.compaction"`) {
		t.Fatalf("raw body = %s", resp.RawBody)
	}
}

func TestOpenAIAdapterCodexNonStreamPreservesCompletedResponseRawBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.output_item.done\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"shell\",\"arguments\":\"{\\\"cmd\\\":\\\"dir\\\"}\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_tool\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"output\":[{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"shell\",\"arguments\":\"{\\\"cmd\\\":\\\"dir\\\"}\"}],\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n")
	}))
	defer server.Close()

	resp, err := (&OpenAIAdapter{}).InvokeResponses(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI OAuth",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "oauth-token",
				Metadata: map[string]string{
					"token_source":     OpenAIDeviceCodeTokenSourceValue(),
					"oauth_account_id": "acct_chatgpt",
					"codex_base_url":   server.URL,
				},
			},
		},
		Model: "gpt-5.5",
	}, &core.ResponsesRequest{
		Model:   "gpt-5.5",
		RawBody: json.RawMessage(`{"model":"gpt-5.5","input":"list files","tools":[{"type":"function","name":"shell"}]}`),
	})
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
	if !strings.Contains(string(resp.RawBody), `"type":"function_call"`) {
		t.Fatalf("raw body missing function call: %s", resp.RawBody)
	}
	if !strings.Contains(string(resp.RawBody), `"call_id":"call_1"`) {
		t.Fatalf("raw body missing call id: %s", resp.RawBody)
	}
}

func TestChatGPTCodexPromptCacheKeyRequiresExplicitAffinity(t *testing.T) {
	req := &core.GatewayRequest{
		Messages:    []core.Message{{Role: "user", Content: "hello"}},
		Client:      &core.APIClient{ID: "client_pointer"},
		ServiceTier: "fast",
	}
	payload := buildChatGPTCodexResponsesRequest(core.RouteDecision{Model: "gpt-5.5"}, req)
	if payload.PromptCacheKey != "" {
		t.Fatalf("PromptCacheKey = %q, want empty without explicit affinity", payload.PromptCacheKey)
	}
	if payload.ServiceTier != "fast" {
		t.Fatalf("ServiceTier = %q, want fast", payload.ServiceTier)
	}

	req.Metadata = map[string]string{
		"request_id": "req_unique",
		"client_id":  "client_stable",
	}
	payload = buildChatGPTCodexResponsesRequest(core.RouteDecision{Model: "gpt-5.5"}, req)
	if payload.PromptCacheKey != "" {
		t.Fatalf("PromptCacheKey = %q, want empty for client metadata only", payload.PromptCacheKey)
	}

	req.Metadata["cache_affinity_key"] = "affinity_stable"
	payload = buildChatGPTCodexResponsesRequest(core.RouteDecision{Model: "gpt-5.5"}, req)
	if payload.PromptCacheKey != "affinity_stable" {
		t.Fatalf("PromptCacheKey = %q, want cache affinity key", payload.PromptCacheKey)
	}

	req.Metadata["prompt_cache_key"] = "prompt_stable"
	payload = buildChatGPTCodexResponsesRequest(core.RouteDecision{Model: "gpt-5.5"}, req)
	if payload.PromptCacheKey != "prompt_stable" {
		t.Fatalf("PromptCacheKey = %q, want prompt cache key", payload.PromptCacheKey)
	}

	req.PromptCacheKey = "request_stable"
	payload = buildChatGPTCodexResponsesRequest(core.RouteDecision{Model: "gpt-5.5"}, req)
	if payload.PromptCacheKey != "request_stable" {
		t.Fatalf("PromptCacheKey = %q, want request prompt cache key", payload.PromptCacheKey)
	}
}

func TestDoJSONUsesHTTPProxyFromContext(t *testing.T) {
	proxyHit := make(chan *http.Request, 1)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHit <- r
		if r.URL.Scheme != "http" {
			t.Fatalf("proxy request scheme = %q", r.URL.Scheme)
		}
		if r.URL.Host != "upstream.example" {
			t.Fatalf("proxy request host = %q", r.URL.Host)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer proxyServer.Close()

	ctx := WithProxyURL(context.Background(), proxyServer.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.example/v1/proxy-test", nil)
	if err != nil {
		t.Fatal(err)
	}

	var body map[string]bool
	if _, _, err := doJSON(req, &body); err != nil {
		t.Fatalf("doJSON returned error: %v", err)
	}
	if !body["ok"] {
		t.Fatalf("body = %#v", body)
	}
	select {
	case request := <-proxyHit:
		if request.URL.Path != "/v1/proxy-test" {
			t.Fatalf("proxy path = %q", request.URL.Path)
		}
	default:
		t.Fatal("expected proxy server to receive request")
	}
}

func TestReadLimitedUpstreamBodyRejectsOversizedPayload(t *testing.T) {
	payload, err := readLimitedUpstreamBody(strings.NewReader("12345"), 4)
	if err == nil {
		t.Fatalf("expected oversized body error, got payload %q", string(payload))
	}
	if !strings.Contains(err.Error(), "exceeds 4 bytes") {
		t.Fatalf("error = %v", err)
	}
}

func TestRewriteMultipartModelPreservesFilePartHeaders(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="image"; filename="input.png"`)
	header.Set("Content-Type", "image/png")
	header.Set("X-Part-Meta", "keep")
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("PNG")); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("model", "gpt-image-1"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	rewritten, contentType, err := rewriteMultipartModel(body.Bytes(), writer.FormDataContentType(), "gpt-image-upstream")
	if err != nil {
		t.Fatalf("rewriteMultipartModel returned error: %v", err)
	}
	reader := multipart.NewReader(bytes.NewReader(rewritten), strings.TrimPrefix(contentType, "multipart/form-data; boundary="))
	seenFile := false
	seenModel := false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch part.FormName() {
		case "image":
			seenFile = true
			if got := part.Header.Get("Content-Type"); got != "image/png" {
				t.Fatalf("content-type = %q, want image/png", got)
			}
			if got := part.Header.Get("X-Part-Meta"); got != "keep" {
				t.Fatalf("x-part-meta = %q, want keep", got)
			}
		case "model":
			seenModel = true
			value, err := io.ReadAll(part)
			if err != nil {
				t.Fatal(err)
			}
			if string(value) != "gpt-image-upstream" {
				t.Fatalf("model = %q, want gpt-image-upstream", string(value))
			}
		}
	}
	if !seenFile || !seenModel {
		t.Fatalf("seenFile=%v seenModel=%v", seenFile, seenModel)
	}
}

func TestTestProxyUsesConfiguredHTTPProxy(t *testing.T) {
	proxyHit := make(chan *http.Request, 1)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHit <- r
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer proxyServer.Close()

	previous := proxyTestEndpoint
	proxyTestEndpoint = "http://upstream.example/proxy-test"
	t.Cleanup(func() { proxyTestEndpoint = previous })

	result, err := TestProxy(context.Background(), proxyServer.URL)
	if err != nil {
		t.Fatalf("TestProxy returned error: %v", err)
	}
	if result.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", result.StatusCode)
	}
	select {
	case request := <-proxyHit:
		if request.URL.Host != "upstream.example" || request.URL.Path != "/proxy-test" {
			t.Fatalf("proxy request URL = %s", request.URL.String())
		}
	default:
		t.Fatal("expected proxy server to receive request")
	}
}

func TestNewProxyHTTPClientSupportsSOCKS5(t *testing.T) {
	client, err := newProxyHTTPClient("socks5://user:pass@127.0.0.1:1080")
	if err != nil {
		t.Fatalf("newProxyHTTPClient returned error: %v", err)
	}
	if client.Transport == nil {
		t.Fatal("expected proxy transport")
	}
}

func TestResponseHeaderTimeoutForContextUsesLongDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 420*time.Second)
	defer cancel()

	timeout := responseHeaderTimeoutForContext(ctx)
	if timeout < 419*time.Second || timeout > 420*time.Second {
		t.Fatalf("timeout = %v, want about 420s", timeout)
	}
}

func TestResolveEndpointDoesNotDuplicateV1BasePath(t *testing.T) {
	tests := []struct {
		name   string
		base   string
		suffix string
		want   string
	}{
		{
			name:   "root base",
			base:   "https://allcode.cc",
			suffix: "/v1/images/generations",
			want:   "https://allcode.cc/v1/images/generations",
		},
		{
			name:   "versioned base",
			base:   "https://allcode.cc/v1",
			suffix: "/v1/images/generations",
			want:   "https://allcode.cc/v1/images/generations",
		},
		{
			name:   "versioned subpath base",
			base:   "https://allcode.cc/api/v1",
			suffix: "/v1/chat/completions",
			want:   "https://allcode.cc/api/v1/chat/completions",
		},
		{
			name:   "full endpoint base",
			base:   "https://allcode.cc/v1/images/generations",
			suffix: "/v1/images/generations",
			want:   "https://allcode.cc/v1/images/generations",
		},
		{
			name:   "versioned base with query",
			base:   "https://allcode.cc/v1?line=main",
			suffix: "/v1/models",
			want:   "https://allcode.cc/v1/models?line=main",
		},
		{
			name:   "full endpoint base with query",
			base:   "https://allcode.cc/v1/models?line=main",
			suffix: "/v1/models",
			want:   "https://allcode.cc/v1/models?line=main",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveEndpoint(core.Account{
				Credential: core.Credential{
					Metadata: map[string]string{"base_url": tt.base},
				},
			}, "https://api.openai.com", tt.suffix)
			if got != tt.want {
				t.Fatalf("endpoint = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOpenAIRefreshSyncsFreshCodexAuthJSON(t *testing.T) {
	authPath := t.TempDir() + "/auth.json"
	freshAccessToken := testJWT(t, time.Now().UTC().Add(time.Hour), map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_codex",
		},
		"email": "user@example.com",
	})
	if err := os.WriteFile(authPath, []byte(`{
  "auth_mode": "chatgpt",
  "tokens": {
    "access_token": "`+freshAccessToken+`",
    "refresh_token": "fresh-refresh-token",
    "id_token": "`+freshAccessToken+`",
    "account_id": "acct_codex"
  }
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	expiredAt := time.Now().UTC().Add(-time.Hour)
	adapter := &OpenAIAdapter{}
	credential, err := adapter.Refresh(context.Background(), core.Account{
		ID:       "acct_openai",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:         OpenAIOAuthModeValue(),
			AccessToken:  "expired-access-token",
			RefreshToken: "stale-refresh-token",
			ExpiresAt:    &expiredAt,
			Metadata: map[string]string{
				"token_source":                 OpenAICodexAuthTokenSourceValue(),
				"oauth_account_id":             "acct_codex",
				OpenAICodexAuthPathMetadataKey: authPath,
			},
		},
	})
	if err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if credential.AccessToken != freshAccessToken {
		t.Fatalf("access token = %q", credential.AccessToken)
	}
	if credential.RefreshToken != "fresh-refresh-token" {
		t.Fatalf("refresh token = %q", credential.RefreshToken)
	}
	if credential.ExpiresAt == nil || !credential.ExpiresAt.After(time.Now().UTC()) {
		t.Fatalf("expires_at = %#v", credential.ExpiresAt)
	}
	if credential.Metadata["codex_auth_synced_at"] == "" {
		t.Fatalf("expected sync timestamp, metadata = %#v", credential.Metadata)
	}
}

func TestOpenAIRefreshSyncsFlatCLIProxyCodexAuthJSON(t *testing.T) {
	authPath := t.TempDir() + "/codex-user@example.com.json"
	freshAccessToken := testJWT(t, time.Now().UTC().Add(time.Hour), map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_flat",
		},
		"email": "flat@example.com",
	})
	if err := os.WriteFile(authPath, []byte(`{
  "type": "codex",
  "email": "flat@example.com",
  "account_id": "acct_flat",
  "access_token": "`+freshAccessToken+`",
  "refresh_token": "fresh-refresh-token",
  "id_token": "`+freshAccessToken+`"
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	expiredAt := time.Now().UTC().Add(-time.Hour)
	adapter := &OpenAIAdapter{}
	credential, err := adapter.Refresh(context.Background(), core.Account{
		ID:       "acct_openai",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:         OpenAIOAuthModeValue(),
			AccessToken:  "expired-access-token",
			RefreshToken: "stale-refresh-token",
			ExpiresAt:    &expiredAt,
			Metadata: map[string]string{
				"token_source":                 OpenAICodexAuthTokenSourceValue(),
				"oauth_account_id":             "acct_flat",
				OpenAICodexAuthPathMetadataKey: authPath,
			},
		},
	})
	if err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if credential.AccessToken != freshAccessToken || credential.RefreshToken != "fresh-refresh-token" {
		t.Fatalf("credential = %#v", credential)
	}
	if credential.Metadata["email"] != "flat@example.com" || credential.Metadata["oauth_account_id"] != "acct_flat" {
		t.Fatalf("metadata = %#v", credential.Metadata)
	}
	raw, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if _, ok := stored["tokens"]; ok {
		t.Fatalf("flat CLI Proxy auth file should stay flat after sync: %s", string(raw))
	}
	if stored["access_token"] != freshAccessToken || stored["refresh_token"] != "fresh-refresh-token" || stored["account_id"] != "acct_flat" {
		t.Fatalf("stored flat auth = %#v", stored)
	}
}

func TestOpenAIForceRefreshUsesFreshSyncedCodexAuthJSON(t *testing.T) {
	authPath := t.TempDir() + "/codex-user@example.com.json"
	freshAccessToken := testJWT(t, time.Now().UTC().Add(time.Hour), map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_flat",
		},
		"email": "flat@example.com",
	})
	if err := os.WriteFile(authPath, []byte(`{
  "type": "codex",
  "email": "flat@example.com",
  "account_id": "acct_flat",
  "access_token": "`+freshAccessToken+`",
  "refresh_token": "already-used-refresh-token",
  "id_token": "`+freshAccessToken+`"
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	expiredAt := time.Now().UTC().Add(-time.Hour)
	adapter := &OpenAIAdapter{}
	credential, err := adapter.Refresh(context.Background(), core.Account{
		ID:       "acct_openai",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:         OpenAIOAuthModeValue(),
			AccessToken:  "stale-access-token",
			RefreshToken: "stale-refresh-token",
			ExpiresAt:    &expiredAt,
			Metadata: map[string]string{
				"force_oauth_refresh":          "true",
				"token_source":                 OpenAICodexAuthTokenSourceValue(),
				"oauth_account_id":             "acct_flat",
				OpenAICodexAuthPathMetadataKey: authPath,
			},
		},
	})
	if err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if credential.AccessToken != freshAccessToken || credential.RefreshToken != "already-used-refresh-token" {
		t.Fatalf("credential = %#v", credential)
	}
}

func TestExtractOpenAITokenExpiryPrefersAccessToken(t *testing.T) {
	accessToken := testJWT(t, time.Now().UTC().Add(10*24*time.Hour), map[string]any{})
	idToken := testJWT(t, time.Now().UTC().Add(-time.Hour), map[string]any{})

	expiresAt := ExtractOpenAITokenExpiry(OpenAITokenSet{
		AccessToken: accessToken,
		IDToken:     idToken,
	})
	if expiresAt == nil || time.Until(expiresAt.UTC()) < 9*24*time.Hour {
		t.Fatalf("expires_at = %#v, want access token expiry", expiresAt)
	}
}

func TestAccessTokenForAccountRejectsBlankCredential(t *testing.T) {
	_, err := accessTokenForAccount(core.Account{})
	if err == nil {
		t.Fatal("expected missing credential error")
	}
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected InvokeError, got %T", err)
	}
	if invokeErr.Code != "missing_credential" {
		t.Fatalf("code = %q", invokeErr.Code)
	}
}

func TestFailurePolicyForUpstreamRejectedUsesCooldown(t *testing.T) {
	err := &InvokeError{
		Code:      "upstream_rejected",
		Temporary: false,
		Err:       fmt.Errorf("unsupported parameter"),
	}
	if status := FailurePolicyFor(core.Account{}, err).AccountStatus; status != core.AccountStatusCooling {
		t.Fatalf("status = %q, want %q", status, core.AccountStatusCooling)
	}
}

func TestFailurePolicyForAccountScopedUpstreamRejectedRetriesFailover(t *testing.T) {
	err := &InvokeError{
		Code:      "upstream_rejected",
		Temporary: false,
		Err:       fmt.Errorf("antigravity auth missing project_id: PERMISSION_DENIED: This service has been disabled in this account"),
	}
	policy := FailurePolicyFor(core.Account{}, err)
	if policy.Scope != FailureScopeAccount || !policy.RetryFailover {
		t.Fatalf("policy = %#v, want account-scoped failover", policy)
	}
	if policy.AccountStatus != core.AccountStatusCooling {
		t.Fatalf("account status = %q, want %q", policy.AccountStatus, core.AccountStatusCooling)
	}
}

func TestFailurePolicyForRequestScopedUpstreamRejectedDoesNotRetryFailover(t *testing.T) {
	err := &InvokeError{
		Code:      "upstream_rejected",
		Temporary: false,
		Err:       fmt.Errorf("unsupported parameter"),
	}
	policy := FailurePolicyFor(core.Account{}, err)
	if policy.Scope != FailureScopeRequest || policy.RetryFailover {
		t.Fatalf("policy = %#v, want request-scoped terminal rejection", policy)
	}
}

func TestFailurePolicyForModelNotFoundRetriesWithoutPenalizingAccount(t *testing.T) {
	err := &InvokeError{
		Code:      ErrorCodeUpstreamNotFound,
		Temporary: false,
		Err:       fmt.Errorf("Model 'gpt-5.5-openai-compact' is not supported by any configured account in this group"),
	}
	policy := FailurePolicyFor(core.Account{}, err)
	if policy.Scope != FailureScopeRequest || !policy.RetryFailover || !policy.PreviousFallback {
		t.Fatalf("policy = %#v, want request-scoped failover", policy)
	}
	if failurePolicyPenalizes(err) {
		t.Fatalf("model-scoped not found should not penalize account")
	}
}

func TestFailurePolicyForPlainNotFoundDoesNotRetryFailover(t *testing.T) {
	err := &InvokeError{
		Code:      ErrorCodeUpstreamNotFound,
		Temporary: false,
		Err:       fmt.Errorf("upstream route not found"),
	}
	policy := FailurePolicyFor(core.Account{}, err)
	if policy.Scope != FailureScopeRequest || policy.RetryFailover {
		t.Fatalf("policy = %#v, want terminal request-scoped not found", policy)
	}
}

func TestFailurePolicyScopesErrors(t *testing.T) {
	cases := map[string]bool{
		"missing_credential":               true,
		"credential_expired":               true,
		"upstream_provider_banned":         true,
		"upstream_auth_error":              true,
		"gateway_api_key_disabled":         true,
		"upstream_rate_limited":            true,
		"upstream_empty_response":          false,
		"upstream_server_error":            false,
		"upstream_transport_error":         false,
		"upstream_read_error":              false,
		"upstream_temporarily_unavailable": false,
		"upstream_invalid_stream_chunk":    false,
		"upstream_rejected":                false,
		"upstream_forbidden":               true,
		"upstream_not_found":               false,
		"upstream_request_build_failed":    false,
		"upstream_invalid_json":            false,
		"unexpected_provider_error":        false,
	}
	for code, want := range cases {
		t.Run(code, func(t *testing.T) {
			err := &InvokeError{
				Code:      code,
				Temporary: code == "upstream_transport_error" || code == "upstream_temporarily_unavailable",
				Err:       fmt.Errorf("request failed"),
			}
			if got := failurePolicyPenalizes(err); got != want {
				t.Fatalf("penalizes = %v, want %v", got, want)
			}
			if shared := FailurePolicyFor(core.Account{}, err).Scope == FailureScopeSharedUpstream; shared != (code == "upstream_server_error" || code == "upstream_transport_error" || code == "upstream_read_error" || code == "upstream_temporarily_unavailable" || code == "upstream_invalid_stream_chunk") {
				t.Fatalf("shared upstream = %v", shared)
			}
		})
	}
	if failurePolicyPenalizes(fmt.Errorf("plain error")) {
		t.Fatal("plain errors should not penalize accounts")
	}
}

func TestFailurePolicyForUpstreamEmptyResponseRetriesWithoutSharedBreaker(t *testing.T) {
	err := &InvokeError{
		Code:      ErrorCodeUpstreamEmptyResponse,
		Temporary: true,
		Cooldown:  10 * time.Second,
		Err:       errors.New("upstream returned an empty response"),
	}

	policy := FailurePolicyFor(core.Account{}, err)
	if !policy.RetryFailover || !policy.PreviousFallback {
		t.Fatalf("policy = %#v, want retryable previous fallback", policy)
	}
	if policy.Scope != FailureScopeRequest {
		t.Fatalf("scope = %v, want request-scoped empty response", policy.Scope)
	}
	if failurePolicyPenalizes(err) {
		t.Fatal("empty upstream responses should not penalize the account")
	}
}

func TestUpstreamFailureScopeUsesOfficialDirectUpstreams(t *testing.T) {
	cases := []struct {
		account core.Account
		want    string
	}{
		{
			account: core.Account{
				ID:       "openai_default",
				Provider: core.ProviderOpenAI,
			},
			want: "openai|upstream=https://api.openai.com",
		},
		{
			account: core.Account{
				ID:       "openai_v1",
				Provider: core.ProviderOpenAI,
				Credential: core.Credential{Metadata: map[string]string{
					"base_url": "https://api.openai.com/v1/",
				}},
			},
			want: "openai|upstream=https://api.openai.com",
		},
		{
			account: core.Account{
				ID:       "claude_default",
				Provider: core.ProviderClaude,
			},
			want: "claude|upstream=https://api.anthropic.com",
		},
		{
			account: core.Account{
				ID:       "claude_v1",
				Provider: core.ProviderClaude,
				Credential: core.Credential{Metadata: map[string]string{
					"base_url": "https://api.anthropic.com/v1",
				}},
			},
			want: "claude|upstream=https://api.anthropic.com",
		},
		{
			account: core.Account{
				ID:       "codex_default",
				Provider: core.ProviderOpenAI,
				Credential: core.Credential{Metadata: map[string]string{
					"account_login_method": "token",
					"codex_base_url":       "https://chatgpt.com/backend-api/codex/",
				}},
			},
			want: "openai|upstream=https://chatgpt.com/backend-api/codex",
		},
	}
	for _, tt := range cases {
		t.Run(tt.account.ID, func(t *testing.T) {
			if scope := UpstreamFailureScope(tt.account); scope != tt.want {
				t.Fatalf("scope = %q, want %q", scope, tt.want)
			}
		})
	}
}

func TestUpstreamFailureScopeUsesProxyOrCustomEndpoint(t *testing.T) {
	cases := []struct {
		name    string
		account core.Account
		want    string
	}{
		{
			name: "proxy wins over official base",
			account: core.Account{
				Provider:          core.ProviderOpenAI,
				EffectiveProxyURL: "http://proxy.example:8080",
				Credential: core.Credential{Metadata: map[string]string{
					"base_url": "https://api.openai.com/v1",
				}},
			},
			want: "openai|proxy=http://proxy.example:8080",
		},
		{
			name: "custom openai endpoint",
			account: core.Account{
				Provider: core.ProviderOpenAI,
				Credential: core.Credential{Metadata: map[string]string{
					"endpoint": "https://gateway.example.com/v1?ignored=true",
				}},
			},
			want: "openai|upstream=https://gateway.example.com/v1",
		},
		{
			name: "custom claude base url",
			account: core.Account{
				Provider: core.ProviderClaude,
				Credential: core.Credential{Metadata: map[string]string{
					"base_url": "https://claude-gateway.example.com/",
				}},
			},
			want: "claude|upstream=https://claude-gateway.example.com",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := UpstreamFailureScope(tt.account); got != tt.want {
				t.Fatalf("scope = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFailurePolicyForRequestScopedErrorsUsesCooldown(t *testing.T) {
	cases := []string{
		"upstream_not_found",
		"upstream_forbidden",
		"upstream_invalid_json",
		"unexpected_provider_error",
	}
	for _, code := range cases {
		t.Run(code, func(t *testing.T) {
			err := &InvokeError{
				Code:      code,
				Temporary: false,
				Err:       fmt.Errorf("request failed"),
			}
			if status := FailurePolicyFor(core.Account{}, err).AccountStatus; status != core.AccountStatusCooling {
				t.Fatalf("status = %q, want %q", status, core.AccountStatusCooling)
			}
		})
	}
}

func TestGatewayAPIKeyDisabledMapsToTemporaryCooldown(t *testing.T) {
	account := core.Account{
		ID:       "acct_sub2api",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode: "manual-token",
			Metadata: map[string]string{
				"account_login_method":   "api_key",
				"api_key_quota_provider": "sub2api",
				"base_url":               "https://gpt.qinnaonao.com",
			},
		},
	}
	err := mapHTTPErrorForAccount(account, http.StatusUnauthorized, []byte(`{"code":"API_KEY_DISABLED","message":"API key is disabled"}`))
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("err = %T %v, want InvokeError", err, err)
	}
	if invokeErr.Code != "gateway_api_key_disabled" || !invokeErr.Temporary {
		t.Fatalf("invoke error = %#v, want temporary gateway_api_key_disabled", invokeErr)
	}
	if status := FailurePolicyFor(core.Account{}, err).AccountStatus; status != core.AccountStatusCooling {
		t.Fatalf("status = %q, want cooling", status)
	}
	if !failurePolicyPenalizes(err) {
		t.Fatal("gateway API key disabled should cool the account")
	}
	if FailurePolicyFor(core.Account{}, err).RefreshCredential {
		t.Fatal("gateway API key disabled should not trigger OAuth/API credential refresh")
	}
}

func TestGatewayAPIKeyDisabledMapsToTemporaryCooldownForNewAPI(t *testing.T) {
	account := core.Account{
		ID:       "acct_new_api",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode: "manual-token",
			Metadata: map[string]string{
				"account_login_method":   "api_key",
				"api_key_quota_provider": "new-api",
				"base_url":               "https://gpt.qinnaonao.com",
			},
		},
	}
	err := mapHTTPErrorForAccount(account, http.StatusUnauthorized, []byte(`{"error":{"type":"API_KEY_DISABLED","message":"API key is disabled"}}`))
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("err = %T %v, want InvokeError", err, err)
	}
	if invokeErr.Code != "gateway_api_key_disabled" || !invokeErr.Temporary {
		t.Fatalf("invoke error = %#v, want temporary gateway_api_key_disabled", invokeErr)
	}
	if status := FailurePolicyFor(core.Account{}, err).AccountStatus; status != core.AccountStatusCooling {
		t.Fatalf("status = %q, want cooling", status)
	}
}

func TestGatewayAPIKeyDisabledMapsForbiddenToTemporaryCooldown(t *testing.T) {
	account := core.Account{
		ID:       "acct_sub2api",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode: "manual-token",
			Metadata: map[string]string{
				"account_login_method":   "api_key",
				"api_key_quota_provider": "gateway",
				"base_url":               "https://gpt.qinnaonao.com",
			},
		},
	}
	err := mapHTTPErrorForAccount(account, http.StatusForbidden, []byte(`{"code":"API_KEY_DISABLED","message":"API key is disabled"}`))
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("err = %T %v, want InvokeError", err, err)
	}
	if invokeErr.Code != "gateway_api_key_disabled" || !invokeErr.Temporary {
		t.Fatalf("invoke error = %#v, want temporary gateway_api_key_disabled", invokeErr)
	}
	if status := FailurePolicyFor(core.Account{}, err).AccountStatus; status != core.AccountStatusCooling {
		t.Fatalf("status = %q, want cooling", status)
	}
	if !failurePolicyPenalizes(err) {
		t.Fatal("gateway API key disabled should cool the account")
	}
}

func TestGatewayAPIKeyDisabledMapsOpenAIErrorTypeToTemporaryCooldown(t *testing.T) {
	account := core.Account{
		ID:       "acct_sub2api",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode: "manual-token",
			Metadata: map[string]string{
				"account_login_method":   "api_key",
				"api_key_quota_provider": "sub2api",
				"base_url":               "https://gpt.qinnaonao.com",
			},
		},
	}
	err := mapHTTPErrorForAccount(account, http.StatusUnauthorized, []byte(`{"error":{"type":"API_KEY_DISABLED","message":"API key is disabled"}}`))
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("err = %T %v, want InvokeError", err, err)
	}
	if invokeErr.Code != "gateway_api_key_disabled" || !invokeErr.Temporary {
		t.Fatalf("invoke error = %#v, want temporary gateway_api_key_disabled", invokeErr)
	}
	if FailurePolicyFor(core.Account{}, err).RefreshCredential {
		t.Fatal("gateway API key disabled should not trigger credential refresh")
	}
}

func TestGatewayAPIKeyDisabledDoesNotMaskOfficialOpenAIAuth(t *testing.T) {
	account := core.Account{
		ID:       "acct_official",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode: "manual-token",
			Metadata: map[string]string{
				"account_login_method": "api_key",
				"base_url":             "https://api.openai.com/v1",
			},
		},
	}
	err := mapHTTPErrorForAccount(account, http.StatusUnauthorized, []byte(`{"code":"API_KEY_DISABLED","message":"API key is disabled"}`))
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("err = %T %v, want InvokeError", err, err)
	}
	if invokeErr.Code != "upstream_auth_error" || invokeErr.Temporary {
		t.Fatalf("invoke error = %#v, want non-temporary upstream_auth_error", invokeErr)
	}
	if status := FailurePolicyFor(core.Account{}, err).AccountStatus; status != core.AccountStatusExpired {
		t.Fatalf("status = %q, want expired", status)
	}
}

func TestFailurePolicyForUnknownErrorsUsesCooldown(t *testing.T) {
	err := fmt.Errorf("unexpected provider error")
	if status := FailurePolicyFor(core.Account{}, err).AccountStatus; status != core.AccountStatusCooling {
		t.Fatalf("status = %q, want %q", status, core.AccountStatusCooling)
	}
}

func TestFailurePolicyForCredentialErrorsStillRemovesAccount(t *testing.T) {
	cases := map[string]core.AccountStatus{
		"missing_credential":         core.AccountStatusExpired,
		"credential_expired":         core.AccountStatusExpired,
		"upstream_provider_banned":   core.AccountStatusProviderBanned,
		"upstream_auth_error":        core.AccountStatusExpired,
		"missing_refresh_credential": core.AccountStatusExpired,
	}
	for code, want := range cases {
		t.Run(code, func(t *testing.T) {
			err := &InvokeError{
				Code:      code,
				Temporary: false,
				Err:       fmt.Errorf("credential failed"),
			}
			if status := FailurePolicyFor(core.Account{}, err).AccountStatus; status != want {
				t.Fatalf("status = %q, want %q", status, want)
			}
		})
	}
}

func TestMapOAuthHTTPErrorInvalidGrantExpiresCredential(t *testing.T) {
	err := mapOAuthHTTPError(
		&http.Response{StatusCode: http.StatusBadRequest},
		[]byte(`{"error":"invalid_grant"}`),
		errors.New("oauth request failed"),
	)
	var invokeErr *InvokeError
	if !errors.As(err, &invokeErr) {
		t.Fatalf("err = %T %v, want InvokeError", err, err)
	}
	if invokeErr.Code != "credential_expired" || invokeErr.Temporary {
		t.Fatalf("invoke error = %#v, want non-temporary credential_expired", invokeErr)
	}
	if !failurePolicyPenalizes(err) {
		t.Fatal("invalid_grant should be account-scoped")
	}
	if !FailurePolicyFor(core.Account{}, err).RefreshCredential {
		t.Fatal("invalid_grant should keep failover eligible for the next account")
	}
}

func TestCredentialNeedsRefreshIgnoresAPIKeyExpiry(t *testing.T) {
	expiresAt := time.Now().UTC().Add(-time.Minute)
	if CredentialNeedsRefresh(core.Account{
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:      "manual-token",
			ExpiresAt: &expiresAt,
			Metadata: map[string]string{
				"account_login_method": "api_key",
			},
		},
	}) {
		t.Fatal("OpenAI API keys should not enter OAuth refresh flow")
	}
	if !CredentialNeedsRefresh(core.Account{
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:      OpenAIOAuthModeValue(),
			ExpiresAt: &expiresAt,
			Metadata: map[string]string{
				"token_source": OpenAIDeviceCodeTokenSourceValue(),
			},
		},
	}) {
		t.Fatal("OpenAI OAuth credentials should refresh near expiry")
	}
}

func TestClaudeAdapterInvokeUsesRealHTTPShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "claude-token" {
			t.Fatalf("x-api-key = %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got == "" {
			t.Fatal("anthropic-version header missing")
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["model"] != "claude-sonnet-4-0" {
			t.Fatalf("model = %v", body["model"])
		}
		if body["max_tokens"] != float64(512) {
			t.Fatalf("max_tokens = %v", body["max_tokens"])
		}
		if body["top_p"] != 0.75 {
			t.Fatalf("top_p = %v", body["top_p"])
		}
		stops, ok := body["stop_sequences"].([]any)
		if !ok || len(stops) != 2 || stops[0] != "END" || stops[1] != "DONE" {
			t.Fatalf("stop_sequences = %#v", body["stop_sequences"])
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "msg_test",
			"model": "claude-sonnet-4-0",
			"content": []map[string]any{
				{"type": "text", "text": "hello from claude"},
			},
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens":            12,
				"output_tokens":           5,
				"cache_read_input_tokens": 4,
			},
		})
	}))
	defer server.Close()

	adapter := &ClaudeAdapter{}
	resp, err := adapter.Invoke(context.Background(), core.RouteDecision{
		Provider: core.ProviderClaude,
		Account: core.Account{
			ID:       "acct_claude",
			Label:    "Claude",
			Provider: core.ProviderClaude,
			Credential: core.Credential{
				AccessToken: "claude-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "claude-sonnet-4-0",
	}, &core.GatewayRequest{
		Model:     "claude-sonnet-4-0",
		MaxTokens: intPtr(512),
		TopP:      floatPtr(0.75),
		Stop:      []string{"END", "DONE"},
		Messages: []core.Message{
			{Role: "system", Content: "system"},
			{Role: "user", Content: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
	if resp.Content != "hello from claude" {
		t.Fatalf("content = %q", resp.Content)
	}
	if resp.Usage.TotalTokens != 21 {
		t.Fatalf("total_tokens = %d", resp.Usage.TotalTokens)
	}
	if resp.Usage.PromptTokens != 16 {
		t.Fatalf("prompt_tokens = %d", resp.Usage.PromptTokens)
	}
	if resp.Usage.CachedPromptTokens != 4 {
		t.Fatalf("cached_prompt_tokens = %d", resp.Usage.CachedPromptTokens)
	}
}

func TestClaudeAdapterInvokeUsesBearerForOAuthCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer claude-oauth-token" {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.Header.Get("x-api-key"); got != "" {
			t.Fatalf("x-api-key = %q, want empty", got)
		}
		if got := r.Header.Get("anthropic-beta"); got != claudeOAuthBetaHeader {
			t.Fatalf("anthropic-beta = %q", got)
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "msg_test",
			"model":       "claude-sonnet-4-0",
			"content":     []map[string]any{{"type": "text", "text": "hello from claude oauth"}},
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens":  8,
				"output_tokens": 3,
			},
		})
	}))
	defer server.Close()

	adapter := &ClaudeAdapter{}
	resp, err := adapter.Invoke(context.Background(), core.RouteDecision{
		Provider: core.ProviderClaude,
		Account: core.Account{
			ID:       "acct_claude_oauth",
			Label:    "Claude OAuth",
			Provider: core.ProviderClaude,
			Credential: core.Credential{
				Mode:        claudeOAuthMode,
				AccessToken: "claude-oauth-token",
				Metadata: map[string]string{
					"base_url":     server.URL,
					"token_source": claudeOAuthTokenSource,
				},
			},
		},
		Model: "claude-sonnet-4-0",
	}, &core.GatewayRequest{
		Messages: []core.Message{
			{Role: "user", Content: "hello"},
		},
	})
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
	if resp.Content != "hello from claude oauth" {
		t.Fatalf("content = %q", resp.Content)
	}
}

func TestOpenAIAdapterOpenStreamUsesRealHTTPShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer openai-token" {
			t.Fatalf("authorization = %q", got)
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["stream"] != true {
			t.Fatalf("stream = %v", body["stream"])
		}
		streamOptions, ok := body["stream_options"].(map[string]any)
		if !ok || streamOptions["include_usage"] != true {
			t.Fatalf("stream_options = %#v", body["stream_options"])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"service_tier\":\"priority\",\"created\":1710000000,\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	adapter := &OpenAIAdapter{}
	session, err := adapter.OpenStream(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "openai-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "gpt-4.1",
	}, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("OpenStream returned error: %v", err)
	}

	var text, finish string
	var usage core.Usage
	for {
		event, err := session.Stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next returned error: %v", err)
		}
		text += event.Delta
		if event.FinishReason != "" {
			finish = event.FinishReason
		}
		if event.Usage != nil {
			usage = *event.Usage
		}
	}
	if err := session.Stream.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	if text != "hello world" {
		t.Fatalf("text = %q", text)
	}
	if finish != "stop" {
		t.Fatalf("finish = %q", finish)
	}
	if usage.TotalTokens != 6 {
		t.Fatalf("usage total = %d", usage.TotalTokens)
	}
	if session.Response.ID != "chatcmpl_stream" {
		t.Fatalf("response id = %q", session.Response.ID)
	}
	if session.Response.ServiceTier != "priority" {
		t.Fatalf("service_tier = %q, want priority", session.Response.ServiceTier)
	}
}

func TestOpenAIAdapterOpenStreamRejectsNewAPIStreamErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"error\":{\"message\":\"stream channel failed\",\"type\":\"new_api_error\",\"code\":\"bad_response\"}}\n\n")
	}))
	defer server.Close()

	session, err := (&OpenAIAdapter{}).OpenStream(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_new_api",
			Label:    "new-api",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "new-api-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "gpt-4.1",
	}, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("OpenStream returned error: %v", err)
	}
	defer session.Stream.Close()

	if _, err := session.Stream.Next(); ErrorCode(err) != "upstream_rejected" || !strings.Contains(err.Error(), "stream channel failed") {
		t.Fatalf("Next err = %v, want upstream_rejected stream channel failed", err)
	}
}

func TestLooksLikeHTMLStreamChunkAvoidsPlainCloudflareMention(t *testing.T) {
	if looksLikeHTMLStreamChunk([]byte(`{"choices":[{"delta":{"content":"Cloudflare Workers endpoint"}}]}`)) {
		t.Fatal("plain Cloudflare mention in JSON stream chunk should not look like HTML challenge")
	}
	for _, payload := range [][]byte{
		[]byte(`<body><div>Cloudflare challenge-platform</div></body>`),
		[]byte(`Cloudflare challenge cf-ray`),
	} {
		if !looksLikeHTMLStreamChunk(payload) {
			t.Fatalf("payload %q should look like HTML challenge", payload)
		}
	}
}

func TestOpenAIStreamAllowsCloudflareChallengeTextInValidJSON(t *testing.T) {
	stream := &openAIStream{
		body:     io.NopCloser(strings.NewReader("")),
		reader:   newSSEReader(strings.NewReader("data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[{\"delta\":{\"content\":\"Cloudflare challenge cf-ray\"}}]}\n\n")),
		response: &core.GatewayResponse{},
	}
	defer stream.Close()

	event, err := stream.Next()
	if err != nil {
		t.Fatalf("Next returned error: %v", err)
	}
	if event.Delta != "Cloudflare challenge cf-ray" {
		t.Fatalf("delta = %q", event.Delta)
	}
}

func TestOpenAIAdapterOpenStreamUsesChatGPTCodexForTokenLogin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %s, want /responses", r.URL.Path)
		}
		if got := r.URL.Query().Get("client_version"); got == "" {
			t.Fatalf("client_version query missing: %s", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-login-token" {
			t.Fatalf("authorization = %q", got)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello token stream\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_token_stream\",\"model\":\"gpt-5.4\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n")
	}))
	defer server.Close()

	session, err := (&OpenAIAdapter{}).OpenStream(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI Token Login",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "token-login-token",
				Metadata: map[string]string{
					"account_login_method": "token",
					"codex_base_url":       server.URL,
				},
			},
		},
		Model: "gpt-5.4",
	}, &core.GatewayRequest{
		Model:    "gpt-5.4",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("OpenStream returned error: %v", err)
	}
	defer session.Stream.Close()

	event, err := session.Stream.Next()
	if err != nil {
		t.Fatalf("first Next returned error: %v", err)
	}
	if event.Delta != "hello token stream" {
		t.Fatalf("delta = %q", event.Delta)
	}
	event, err = session.Stream.Next()
	if err != nil {
		t.Fatalf("second Next returned error: %v", err)
	}
	if event.RawEvent != "response.completed" || !event.Done {
		t.Fatalf("terminal event = %#v", event)
	}
}

func TestOpenAIAdapterOpenResponsesStreamPreservesTerminalEvents(t *testing.T) {
	for _, tc := range []struct {
		eventType string
		finish    string
	}{
		{eventType: "response.failed", finish: "failed"},
		{eventType: "response.incomplete", finish: "incomplete"},
		{eventType: "response.cancelled", finish: "cancelled"},
	} {
		t.Run(tc.finish, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/responses" {
					t.Fatalf("path = %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "event: %s\n", tc.eventType)
				_, _ = fmt.Fprintf(w, "data: {\"type\":%q,\"response\":{\"id\":\"resp_terminal\",\"model\":\"gpt-4.1\",\"service_tier\":\"priority\",\"status\":%q,\"usage\":{\"input_tokens\":3,\"output_tokens\":0,\"total_tokens\":3},\"error\":{\"message\":\"tool output was invalid\"}}}\n\n", tc.eventType, tc.finish)
			}))
			defer server.Close()

			session, err := (&OpenAIAdapter{}).OpenResponsesStream(context.Background(), core.RouteDecision{
				Provider: core.ProviderOpenAI,
				Account: core.Account{
					ID:       "acct_openai",
					Label:    "OpenAI",
					Provider: core.ProviderOpenAI,
					Credential: core.Credential{
						AccessToken: "openai-token",
						Metadata:    map[string]string{"base_url": server.URL},
					},
				},
				Model: "gpt-4.1",
			}, &core.ResponsesRequest{
				Model:     "gpt-4.1",
				Transport: core.ResponsesTransportSSE,
				Stream:    true,
				RawBody:   json.RawMessage(`{"model":"gpt-4.1","stream":true,"input":"hello"}`),
			})
			if err != nil {
				t.Fatalf("OpenStream returned error: %v", err)
			}
			defer session.Stream.Close()

			event, err := session.Stream.Next()
			if err != nil {
				t.Fatalf("Next returned error: %v", err)
			}
			if event.FinishReason != tc.finish || event.RawEvent != tc.eventType || !event.Done {
				t.Fatalf("event = %#v", event)
			}
			if event.Usage == nil || event.Usage.TotalTokens != 3 {
				t.Fatalf("usage = %#v", event.Usage)
			}
			if session.Response.ServiceTier != "priority" {
				t.Fatalf("service_tier = %q, want priority", session.Response.ServiceTier)
			}
			if !strings.Contains(string(event.RawData), `"status":"`+tc.finish+`"`) {
				t.Fatalf("raw data = %s", event.RawData)
			}
			if _, err := session.Stream.Next(); err != io.EOF {
				t.Fatalf("next err = %v, want EOF", err)
			}
		})
	}
}

func TestOpenAIAdapterOpenResponsesStreamRejectsNewAPIStreamErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: error\n")
		_, _ = fmt.Fprint(w, "data: {\"error\":{\"message\":\"responses stream failed\",\"type\":\"new_api_error\",\"code\":\"bad_response\"}}\n\n")
	}))
	defer server.Close()

	session, err := (&OpenAIAdapter{}).OpenResponsesStream(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_new_api",
			Label:    "new-api",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "new-api-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "gpt-4.1",
	}, &core.ResponsesRequest{
		Model:     "gpt-4.1",
		Transport: core.ResponsesTransportSSE,
		Stream:    true,
		RawBody:   json.RawMessage(`{"model":"gpt-4.1","stream":true,"input":"hello"}`),
	})
	if err != nil {
		t.Fatalf("OpenResponsesStream returned error: %v", err)
	}
	defer session.Stream.Close()

	if _, err := session.Stream.Next(); ErrorCode(err) != "upstream_rejected" || !strings.Contains(err.Error(), "responses stream failed") {
		t.Fatalf("Next err = %v, want upstream_rejected responses stream failed", err)
	}
}

func TestOpenAIAdapterOpenResponsesStreamRejectsEarlyDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	session, err := (&OpenAIAdapter{}).OpenResponsesStream(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "openai-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "gpt-4.1",
	}, &core.ResponsesRequest{
		Model:     "gpt-4.1",
		Transport: core.ResponsesTransportSSE,
		Stream:    true,
		RawBody:   json.RawMessage(`{"model":"gpt-4.1","stream":true,"input":"hello"}`),
	})
	if err != nil {
		t.Fatalf("OpenResponsesStream returned error: %v", err)
	}
	defer session.Stream.Close()

	if _, err := session.Stream.Next(); ErrorCode(err) != "upstream_read_error" {
		t.Fatalf("Next err = %v, want upstream_read_error", err)
	}
}

func TestOpenAIAdapterOpenResponsesWebSocketStreamUsesResponsesWebSocket(t *testing.T) {
	var receivedHeader http.Header
	var receivedPayload map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s, want /v1/responses", r.URL.Path)
		}
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade upstream websocket: %v", err)
		}
		defer conn.Close()
		receivedHeader = r.Header.Clone()
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read upstream payload: %v", err)
		}
		if messageType != websocket.TextMessage {
			t.Fatalf("message type = %d, want text", messageType)
		}
		if err := json.Unmarshal(payload, &receivedPayload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"hello ws"}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_ws_provider","model":"gpt-4.1","status":"completed","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`))
	}))
	defer upstream.Close()

	session, err := (&OpenAIAdapter{}).OpenResponsesStream(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "openai-token",
				Metadata:    map[string]string{"base_url": upstream.URL},
			},
		},
		Model: "gpt-4.1",
	}, &core.ResponsesRequest{
		Model:     "gpt-4.1",
		RawBody:   json.RawMessage(`{"type":"response.create","model":"client-model","stream":false,"generate":false,"input":"hello"}`),
		Transport: core.ResponsesTransportWebSocket,
		Stream:    true,
	})
	if err != nil {
		t.Fatalf("OpenResponsesStream returned error: %v", err)
	}
	defer session.Stream.Close()

	first, err := session.Stream.Next()
	if err != nil {
		t.Fatalf("first Next returned error: %v", err)
	}
	if first.Delta != "hello ws" || first.RawEvent != "response.output_text.delta" {
		t.Fatalf("first event = %#v", first)
	}
	second, err := session.Stream.Next()
	if err != nil {
		t.Fatalf("second Next returned error: %v", err)
	}
	if !second.Done || second.Usage == nil || second.Usage.TotalTokens != 7 {
		t.Fatalf("terminal event = %#v", second)
	}
	if _, err := session.Stream.Next(); err != io.EOF {
		t.Fatalf("third Next err = %v, want EOF", err)
	}
	if got := receivedHeader.Get("Authorization"); got != "Bearer openai-token" {
		t.Fatalf("authorization = %q", got)
	}
	if got := receivedHeader.Get("OpenAI-Beta"); got != openAIResponsesWebSocketBeta {
		t.Fatalf("OpenAI-Beta = %q, want %q", got, openAIResponsesWebSocketBeta)
	}
	if got := receivedHeader.Get("ChatGPT-Account-ID"); got != "" {
		t.Fatalf("ChatGPT-Account-ID should not be sent to official API websocket, got %q", got)
	}
	if receivedPayload["type"] != "response.create" {
		t.Fatalf("type = %v", receivedPayload["type"])
	}
	if receivedPayload["model"] != "gpt-4.1" {
		t.Fatalf("model = %v", receivedPayload["model"])
	}
	if receivedPayload["stream"] != true {
		t.Fatalf("stream = %v", receivedPayload["stream"])
	}
	if receivedPayload["generate"] != false {
		t.Fatalf("generate = %#v, want false", receivedPayload["generate"])
	}
	if session.Response.ID != "resp_ws_provider" || session.Response.Usage.TotalTokens != 7 {
		t.Fatalf("session response = %#v", session.Response)
	}
}

func TestOpenAIAdapterOpenResponsesWebSocketStreamRejectsNewAPIErrorBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade upstream websocket: %v", err)
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatalf("read upstream payload: %v", err)
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"error":{"message":"websocket responses failed","type":"new_api_error","code":"bad_response"}}`))
	}))
	defer upstream.Close()

	session, err := (&OpenAIAdapter{}).OpenResponsesStream(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_new_api",
			Label:    "new-api",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "new-api-token",
				Metadata:    map[string]string{"base_url": upstream.URL},
			},
		},
		Model: "gpt-4.1",
	}, &core.ResponsesRequest{
		Model:     "gpt-4.1",
		RawBody:   json.RawMessage(`{"type":"response.create","model":"client-model","stream":false,"input":"hello"}`),
		Transport: core.ResponsesTransportWebSocket,
		Stream:    true,
	})
	if err != nil {
		t.Fatalf("OpenResponsesStream returned error: %v", err)
	}
	defer session.Stream.Close()

	if _, err := session.Stream.Next(); ErrorCode(err) != "upstream_rejected" || !strings.Contains(err.Error(), "websocket responses failed") {
		t.Fatalf("Next err = %v, want upstream_rejected websocket responses failed", err)
	}
}

func TestOpenAIAdapterOpenResponsesWebSocketStreamMarksReasoningAsFirstOutput(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade upstream websocket: %v", err)
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatalf("read upstream payload: %v", err)
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.reasoning_summary_text.delta","summary_index":0,"delta":"thinking"}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_ws_reasoning","model":"gpt-4.1","status":"completed","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`))
	}))
	defer upstream.Close()

	session, err := (&OpenAIAdapter{}).OpenResponsesStream(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "openai-token",
				Metadata:    map[string]string{"base_url": upstream.URL},
			},
		},
		Model: "gpt-4.1",
	}, &core.ResponsesRequest{
		Model:     "gpt-4.1",
		RawBody:   json.RawMessage(`{"type":"response.create","model":"client-model","stream":false,"input":"hello"}`),
		Transport: core.ResponsesTransportWebSocket,
		Stream:    true,
	})
	if err != nil {
		t.Fatalf("OpenResponsesStream returned error: %v", err)
	}
	defer session.Stream.Close()

	first, err := session.Stream.Next()
	if err != nil {
		t.Fatalf("first Next returned error: %v", err)
	}
	if first.RawEvent != "response.reasoning_summary_text.delta" || !first.FirstOutput || first.Delta != "" {
		t.Fatalf("first event = %#v", first)
	}
	if !strings.Contains(string(first.RawData), `"delta":"thinking"`) {
		t.Fatalf("raw data = %s", first.RawData)
	}
	second, err := session.Stream.Next()
	if err != nil {
		t.Fatalf("second Next returned error: %v", err)
	}
	if !second.Done || second.FirstOutput {
		t.Fatalf("terminal event = %#v", second)
	}
}

func TestParseOpenAIResponsesStreamPayloadMarksToolCallItemAddedAsFirstOutput(t *testing.T) {
	raw := []byte(`{"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"shell","arguments":""}}`)
	event, err := parseOpenAIResponsesStreamPayload(core.Account{}, &core.GatewayResponse{}, nil, nil, "", raw)
	if err != nil {
		t.Fatalf("parseOpenAIResponsesStreamPayload returned error: %v", err)
	}
	if event == nil || event.RawEvent != "response.output_item.added" || !event.FirstOutput || event.Delta != "" {
		t.Fatalf("event = %#v", event)
	}

	raw = []byte(`{"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"dir\"}"}}`)
	event, err = parseOpenAIResponsesStreamPayload(core.Account{}, &core.GatewayResponse{}, nil, nil, "", raw)
	if err != nil {
		t.Fatalf("parseOpenAIResponsesStreamPayload returned error: %v", err)
	}
	if event == nil || event.RawEvent != "response.output_item.done" || !event.FirstOutput || event.Delta != "" {
		t.Fatalf("output_item.done event = %#v, want first output", event)
	}

	raw = []byte(`{"type":"response.function_call_arguments.delta","delta":"{\"cmd\""}`)
	event, err = parseOpenAIResponsesStreamPayload(core.Account{}, &core.GatewayResponse{}, nil, nil, "", raw)
	if err != nil {
		t.Fatalf("parseOpenAIResponsesStreamPayload returned error: %v", err)
	}
	if event == nil || event.RawEvent != "response.function_call_arguments.delta" || !event.FirstOutput || event.Delta != "" {
		t.Fatalf("function_call_arguments.delta event = %#v", event)
	}
}

func TestParseOpenAIResponsesStreamPayloadMarksTextItemDoneAsFirstOutput(t *testing.T) {
	emittedDelta := false
	raw := []byte(`{"type":"response.output_item.done","item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}}`)
	event, err := parseOpenAIResponsesStreamPayload(core.Account{}, &core.GatewayResponse{}, &emittedDelta, nil, "", raw)
	if err != nil {
		t.Fatalf("parseOpenAIResponsesStreamPayload returned error: %v", err)
	}
	if event == nil || event.RawEvent != "response.output_item.done" || event.Delta != "hello" || !event.FirstOutput {
		t.Fatalf("output_item.done text event = %#v, want first output delta", event)
	}
	if !emittedDelta {
		t.Fatal("emittedDelta = false, want true")
	}
}

func TestOpenAIAdapterOpenResponsesWebSocketStreamUsesChatGPTCodexBackend(t *testing.T) {
	var receivedHeader http.Header
	var receivedPayload map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %s, want /responses", r.URL.Path)
		}
		if got := r.URL.Query().Get("client_version"); got == "" {
			t.Fatalf("client_version query missing: %s", r.URL.RawQuery)
		}
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade upstream websocket: %v", err)
		}
		defer conn.Close()
		receivedHeader = r.Header.Clone()
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read upstream payload: %v", err)
		}
		if messageType != websocket.TextMessage {
			t.Fatalf("message type = %d, want text", messageType)
		}
		if err := json.Unmarshal(payload, &receivedPayload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_codex_ws_provider","model":"gpt-5.5"}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`))
	}))
	defer upstream.Close()

	generate := false
	session, err := (&OpenAIAdapter{}).OpenResponsesStream(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI OAuth",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "oauth-token",
				Metadata: map[string]string{
					"token_source":     OpenAIDeviceCodeTokenSourceValue(),
					"oauth_account_id": "acct_chatgpt",
					"codex_base_url":   upstream.URL,
				},
			},
		},
		Model: "gpt-5.5",
	}, &core.ResponsesRequest{
		Model:     "gpt-5.5",
		Transport: core.ResponsesTransportWebSocket,
		Stream:    true,
		Generate:  &generate,
		Headers: map[string]string{
			"x-codex-turn-metadata": `{"turn_id":"turn-1"}`,
			"x-codex-window-id":     "window-header",
		},
		RawBody: json.RawMessage(`{
			"type":"response.create",
			"model":"client-model",
			"stream":false,
			"generate":false,
			"input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}],
			"client_metadata":{
				"x-codex-turn-metadata":"body-meta",
				"x-codex-window-id":"window-body"
			}
		}`),
	})
	if err != nil {
		t.Fatalf("OpenResponsesStream returned error: %v", err)
	}
	defer session.Stream.Close()

	first, err := session.Stream.Next()
	if err != nil {
		t.Fatalf("first Next returned error: %v", err)
	}
	if first.RawEvent != "response.created" || first.Done {
		t.Fatalf("first event = %#v", first)
	}
	second, err := session.Stream.Next()
	if err != nil {
		t.Fatalf("second Next returned error: %v", err)
	}
	if !second.Done || second.Usage == nil || second.Usage.TotalTokens != 7 || second.RawEvent != "response.done" {
		t.Fatalf("terminal event = %#v", second)
	}
	if _, err := session.Stream.Next(); err != io.EOF {
		t.Fatalf("third Next err = %v, want EOF", err)
	}
	if got := receivedHeader.Get("Authorization"); got != "Bearer oauth-token" {
		t.Fatalf("authorization = %q", got)
	}
	if got := receivedHeader.Get("ChatGPT-Account-ID"); got != "acct_chatgpt" {
		t.Fatalf("ChatGPT-Account-ID = %q", got)
	}
	if got := receivedHeader.Get("OpenAI-Beta"); got != openAIResponsesWebSocketBeta {
		t.Fatalf("OpenAI-Beta = %q, want %q", got, openAIResponsesWebSocketBeta)
	}
	if got := receivedHeader.Get("x-codex-turn-metadata"); got != `{"turn_id":"turn-1"}` {
		t.Fatalf("x-codex-turn-metadata = %q", got)
	}
	if got := receivedHeader.Get("x-codex-window-id"); got != "window-header" {
		t.Fatalf("x-codex-window-id = %q", got)
	}
	if receivedPayload["type"] != "response.create" {
		t.Fatalf("type = %#v", receivedPayload["type"])
	}
	if receivedPayload["model"] != "gpt-5.5" {
		t.Fatalf("model = %#v", receivedPayload["model"])
	}
	if receivedPayload["stream"] != true {
		t.Fatalf("stream = %#v", receivedPayload["stream"])
	}
	if receivedPayload["generate"] != false {
		t.Fatalf("generate = %#v, want false", receivedPayload["generate"])
	}
	if receivedPayload["store"] != false {
		t.Fatalf("store = %#v", receivedPayload["store"])
	}
	clientMetadata, ok := receivedPayload["client_metadata"].(map[string]any)
	if !ok {
		t.Fatalf("client_metadata = %#v", receivedPayload["client_metadata"])
	}
	if clientMetadata["x-codex-turn-metadata"] != "body-meta" {
		t.Fatalf("client_metadata x-codex-turn-metadata = %#v", clientMetadata["x-codex-turn-metadata"])
	}
	if clientMetadata["x-codex-window-id"] != "window-body" {
		t.Fatalf("client_metadata x-codex-window-id = %#v", clientMetadata["x-codex-window-id"])
	}
	if session.Response.ID != "resp_codex_ws_provider" || session.Response.Usage.TotalTokens != 7 {
		t.Fatalf("session response = %#v", session.Response)
	}
}

func TestOpenAIAdapterOpenResponsesWebSocketStreamRejectsCloseBeforeTerminalEvent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade upstream websocket: %v", err)
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatalf("read upstream payload: %v", err)
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"partial"}`))
	}))
	defer upstream.Close()

	session, err := (&OpenAIAdapter{}).OpenResponsesStream(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "openai-token",
				Metadata:    map[string]string{"base_url": upstream.URL},
			},
		},
		Model: "gpt-4.1",
	}, &core.ResponsesRequest{
		Model:     "gpt-4.1",
		RawBody:   json.RawMessage(`{"type":"response.create","model":"client-model","stream":false,"input":"hello"}`),
		Transport: core.ResponsesTransportWebSocket,
		Stream:    true,
	})
	if err != nil {
		t.Fatalf("OpenResponsesStream returned error: %v", err)
	}
	defer session.Stream.Close()

	event, err := session.Stream.Next()
	if err != nil {
		t.Fatalf("first Next returned error: %v", err)
	}
	if event.Delta != "partial" {
		t.Fatalf("delta = %q", event.Delta)
	}
	if _, err := session.Stream.Next(); ErrorCode(err) != "upstream_read_error" {
		t.Fatalf("second Next err = %v, want upstream_read_error", err)
	}
}

func TestClaudeAdapterNormalizesUnsupportedRoles(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if system, _ := body["system"].(string); system != "system prompt\ndeveloper prompt" {
			t.Fatalf("system = %q", system)
		}
		messages, ok := body["messages"].([]any)
		if !ok || len(messages) != 2 {
			t.Fatalf("messages = %#v", body["messages"])
		}
		first, _ := messages[0].(map[string]any)
		second, _ := messages[1].(map[string]any)
		if first["role"] != "assistant" {
			t.Fatalf("first role = %#v", first["role"])
		}
		if second["role"] != "user" {
			t.Fatalf("second role = %#v", second["role"])
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"id":          "msg_roles",
			"model":       "claude-sonnet-4-0",
			"content":     []map[string]any{{"type": "text", "text": "ok"}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 8, "output_tokens": 3},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	adapter := &ClaudeAdapter{}
	_, err := adapter.Invoke(context.Background(), core.RouteDecision{
		Provider: core.ProviderClaude,
		Account: core.Account{
			ID:       "acct_claude",
			Label:    "Claude",
			Provider: core.ProviderClaude,
			Credential: core.Credential{
				AccessToken: "claude-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "claude-sonnet-4-0",
	}, &core.GatewayRequest{
		Messages: []core.Message{
			{Role: "system", Content: "system prompt"},
			{Role: "developer", Content: "developer prompt"},
			{Role: "assistant", Content: "previous answer"},
			{Role: "tool", Content: "tool output"},
		},
	})
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
}

func TestOpenAIAdapterOpenStreamPreservesToolCallChunks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	adapter := &OpenAIAdapter{}
	session, err := adapter.OpenStream(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "openai-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "gpt-4.1",
	}, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "use tool"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("OpenStream returned error: %v", err)
	}
	defer session.Stream.Close()

	event, err := session.Stream.Next()
	if err != nil {
		t.Fatalf("Next returned error: %v", err)
	}
	if !strings.Contains(string(event.RawData), `"tool_calls"`) {
		t.Fatalf("raw data = %s", event.RawData)
	}
	if !event.FirstOutput {
		t.Fatalf("tool call chunk should mark first output: %#v", event)
	}
	event, err = session.Stream.Next()
	if err != nil {
		t.Fatalf("Next returned finish error: %v", err)
	}
	if event.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q", event.FinishReason)
	}
}

func TestOpenAIAdapterOpenStreamMarksTerminalToolCallChunkAsFirstOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl_stream\",\"model\":\"gpt-4.1\",\"created\":1710000000,\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	session, err := (&OpenAIAdapter{}).OpenStream(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "openai-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "gpt-4.1",
	}, &core.GatewayRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{{Role: "user", Content: "use tool"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("OpenStream returned error: %v", err)
	}
	defer session.Stream.Close()

	event, err := session.Stream.Next()
	if err != nil {
		t.Fatalf("Next returned error: %v", err)
	}
	if event.FinishReason != "tool_calls" || !event.Done || !event.FirstOutput {
		t.Fatalf("terminal tool call event = %#v", event)
	}
	if !strings.Contains(string(event.RawData), `"tool_calls"`) {
		t.Fatalf("raw data = %s", event.RawData)
	}
	if _, err := session.Stream.Next(); err != io.EOF {
		t.Fatalf("next err = %v, want EOF", err)
	}
}

func TestClaudeAdapterOpenStreamUsesRealHTTPShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "claude-token" {
			t.Fatalf("x-api-key = %q", got)
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["stream"] != true {
			t.Fatalf("stream = %v", body["stream"])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"message\":{\"id\":\"msg_stream\",\"model\":\"claude-sonnet-4-0\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"hello \"}}\n\n")
		_, _ = fmt.Fprint(w, "event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"claude\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":12,\"output_tokens\":5,\"cache_read_input_tokens\":4}}\n\n")
		_, _ = fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	adapter := &ClaudeAdapter{}
	session, err := adapter.OpenStream(context.Background(), core.RouteDecision{
		Provider: core.ProviderClaude,
		Account: core.Account{
			ID:       "acct_claude",
			Label:    "Claude",
			Provider: core.ProviderClaude,
			Credential: core.Credential{
				AccessToken: "claude-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "claude-sonnet-4-0",
	}, &core.GatewayRequest{
		Model:    "claude-sonnet-4-0",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("OpenStream returned error: %v", err)
	}

	var (
		text   string
		finish string
		usage  core.Usage
	)
	for {
		event, err := session.Stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next returned error: %v", err)
		}
		text += event.Delta
		if event.FinishReason != "" {
			finish = event.FinishReason
		}
		if event.Usage != nil {
			usage = *event.Usage
		}
	}
	if err := session.Stream.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	if text != "hello claude" {
		t.Fatalf("text = %q", text)
	}
	if finish != "end_turn" {
		t.Fatalf("finish = %q", finish)
	}
	if usage.TotalTokens != 21 {
		t.Fatalf("usage total = %d", usage.TotalTokens)
	}
	if usage.PromptTokens != 16 {
		t.Fatalf("prompt tokens = %d", usage.PromptTokens)
	}
	if usage.CachedPromptTokens != 4 {
		t.Fatalf("cached prompt tokens = %d", usage.CachedPromptTokens)
	}
	if session.Response.ID != "msg_stream" {
		t.Fatalf("response id = %q", session.Response.ID)
	}
}

func TestClaudeAdapterRawStreamMarksToolUseAsFirstOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"message\":{\"id\":\"msg_tool\",\"model\":\"claude-sonnet-4-0\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"lookup\",\"input\":{}}}\n\n")
		_, _ = fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"input_tokens\":12,\"output_tokens\":5}}\n\n")
		_, _ = fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	adapter := &ClaudeAdapter{}
	session, err := adapter.OpenStream(context.Background(), core.RouteDecision{
		Provider: core.ProviderClaude,
		Account: core.Account{
			ID:       "acct_claude",
			Label:    "Claude",
			Provider: core.ProviderClaude,
			Credential: core.Credential{
				AccessToken: "claude-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "claude-sonnet-4-0",
	}, &core.GatewayRequest{
		Model:        "claude-sonnet-4-0",
		RawBody:      json.RawMessage(`{"model":"claude-sonnet-4-0","max_tokens":64,"messages":[{"role":"user","content":"use tool"}]}`),
		Stream:       true,
		UpstreamMode: "anthropic_messages",
	})
	if err != nil {
		t.Fatalf("OpenStream returned error: %v", err)
	}
	defer session.Stream.Close()

	first, err := session.Stream.Next()
	if err != nil {
		t.Fatalf("first Next returned error: %v", err)
	}
	if first.FirstOutput {
		t.Fatalf("message_start should not mark first output: %#v", first)
	}
	second, err := session.Stream.Next()
	if err != nil {
		t.Fatalf("second Next returned error: %v", err)
	}
	if second.RawEvent != "content_block_start" || !second.FirstOutput {
		t.Fatalf("content_block_start event = %#v, want first output", second)
	}
	third, err := session.Stream.Next()
	if err != nil {
		t.Fatalf("third Next returned error: %v", err)
	}
	if third.RawEvent != "content_block_delta" || !third.FirstOutput {
		t.Fatalf("content_block_delta event = %#v, want first output", third)
	}
}

func TestClaudeAdapterRawStreamRejectsErrorEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"anthropic overloaded\"}}\n\n")
	}))
	defer server.Close()

	session, err := (&ClaudeAdapter{}).OpenStream(context.Background(), core.RouteDecision{
		Provider: core.ProviderClaude,
		Account: core.Account{
			ID:       "acct_claude",
			Label:    "Claude",
			Provider: core.ProviderClaude,
			Credential: core.Credential{
				AccessToken: "claude-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "claude-sonnet-4-0",
	}, &core.GatewayRequest{
		Model:        "claude-sonnet-4-0",
		RawBody:      json.RawMessage(`{"model":"claude-sonnet-4-0","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`),
		Stream:       true,
		UpstreamMode: "anthropic_messages",
	})
	if err != nil {
		t.Fatalf("OpenStream returned error: %v", err)
	}
	defer session.Stream.Close()

	if event, err := session.Stream.Next(); ErrorCode(err) != "upstream_temporarily_unavailable" || !strings.Contains(err.Error(), "anthropic overloaded") || event != nil {
		t.Fatalf("Next event=%#v err=%v, want upstream_temporarily_unavailable anthropic overloaded", event, err)
	}
}

func TestOpenAIAdapterImageStreamRejectsTopLevelErrorEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			t.Fatalf("path = %q, want /v1/images/generations", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"error\":{\"message\":\"invalid image request\",\"type\":\"invalid_request_error\",\"code\":\"bad_request\"}}\n\n")
	}))
	defer server.Close()

	session, err := (&OpenAIAdapter{}).OpenImageGenerationStream(context.Background(), core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_openai",
			Label:    "OpenAI",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				AccessToken: "openai-token",
				Metadata:    map[string]string{"base_url": server.URL},
			},
		},
		Model: "gpt-image-2",
	}, &core.ImageGenerationRequest{Model: "gpt-image-2", Prompt: "test"})
	if err != nil {
		t.Fatalf("OpenImageGenerationStream returned error: %v", err)
	}
	defer session.Stream.Close()

	if event, err := session.Stream.Next(); ErrorCode(err) != "upstream_rejected" || event != nil {
		t.Fatalf("Next event=%#v err=%v, want upstream_rejected error", event, err)
	}
}

func TestOpenAIAdapterImageGenerationFiltersInternalRouteFields(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			t.Fatalf("path = %q, want /v1/images/generations", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"created": 1710000000,
			"data": []map[string]string{
				{"url": "https://example.com/image.png"},
			},
		})
	}))
	defer server.Close()

	adapter := &OpenAIAdapter{}
	decision := core.RouteDecision{
		Provider: core.ProviderOpenAI,
		Account: core.Account{
			ID:       "acct_api",
			Label:    "API Key",
			Provider: core.ProviderOpenAI,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "api-token",
				Metadata: map[string]string{
					"base_url": server.URL,
				},
			},
		},
		Model: "gpt-image-1",
	}

	_, err := adapter.GenerateImage(context.Background(), decision, &core.ImageGenerationRequest{
		Model:  "gpt-image-1",
		Prompt: "test",
		Extra: map[string]json.RawMessage{
			"size":                 json.RawMessage(`"1024x1024"`),
			"route_affinity_key":   json.RawMessage(`"session-id:sess_123"`),
			"cache_affinity_key":   json.RawMessage(`"private-cache"`),
			"route_affinity_model": json.RawMessage(`"gpt-image-1"`),
			"prompt_cache_key":     json.RawMessage(`"private-prompt-cache"`),
		},
	})
	if err != nil {
		t.Fatalf("GenerateImage returned error: %v", err)
	}
	for _, key := range []string{"route_affinity_key", "cache_affinity_key", "route_affinity_model", "prompt_cache_key"} {
		if _, exists := gotBody[key]; exists {
			t.Fatalf("%s leaked in image payload: %#v", key, gotBody)
		}
	}
	if gotBody["size"] != "1024x1024" {
		t.Fatalf("size = %#v", gotBody["size"])
	}
	if gotBody["model"] != "gpt-image-1" || gotBody["prompt"] != "test" {
		t.Fatalf("body = %#v", gotBody)
	}
}

func TestMapHTTPErrorClassifiesStatusCodes(t *testing.T) {
	err := mapHTTPError(http.StatusTooManyRequests, []byte(`{"error":{"message":"slow down"}}`))
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected InvokeError, got %T", err)
	}
	if invokeErr.Code != "upstream_rate_limited" {
		t.Fatalf("code = %q", invokeErr.Code)
	}
	if !invokeErr.Temporary {
		t.Fatal("expected temporary error")
	}
}

func TestGatewayAPIKeyMiddleLayerRateLimitMapsToSharedFailure(t *testing.T) {
	account := core.Account{
		ID:       "acct_new_api",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "new-api-token",
			Metadata: map[string]string{
				"account_login_method":   "api_key",
				"api_key_quota_provider": "new-api",
				"base_url":               "https://new-api.example.test",
			},
		},
	}
	err := mapHTTPErrorForAccount(account, http.StatusTooManyRequests, []byte(`{"error":{"message":"no available channel","code":"channel:no_available_key"}}`))
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected InvokeError, got %T", err)
	}
	if invokeErr.Code != "upstream_temporarily_unavailable" {
		t.Fatalf("code = %q, want upstream_temporarily_unavailable", invokeErr.Code)
	}
	policy := FailurePolicyFor(account, err)
	if policy.Scope != FailureScopeSharedUpstream || !policy.RetryFailover || failurePolicyPenalizes(err) {
		t.Fatalf("policy = %#v, want shared failover without account penalty", policy)
	}
}

func TestGatewayAPIKeyOuterRateLimitMapsToQuotaFailure(t *testing.T) {
	account := core.Account{
		ID:       "acct_new_api",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "new-api-token",
			Metadata: map[string]string{
				"account_login_method":   "api_key",
				"api_key_quota_provider": "new-api",
				"base_url":               "https://new-api.example.test",
			},
		},
	}
	err := mapHTTPErrorForAccount(account, http.StatusTooManyRequests, []byte(`{"error":{"message":"api key daily limit exhausted"}}`))
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected InvokeError, got %T", err)
	}
	if invokeErr.Code != "upstream_rate_limited" {
		t.Fatalf("code = %q, want upstream_rate_limited", invokeErr.Code)
	}
	policy := FailurePolicyFor(account, err)
	if policy.Scope != FailureScopeQuota || !policy.ApplyChatQuota || !policy.RetryFailover {
		t.Fatalf("policy = %#v, want quota-scoped failover", policy)
	}
}

func TestGatewayAPIKeyBadRequestQuotaPayloadMapsToQuotaFailure(t *testing.T) {
	account := core.Account{
		ID:       "acct_new_api",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "new-api-token",
			Metadata: map[string]string{
				"account_login_method":   "api_key",
				"api_key_quota_provider": "new-api",
				"base_url":               "https://new-api.example.test",
			},
		},
	}
	err := mapHTTPErrorForAccount(account, http.StatusBadRequest, []byte(`{"error":{"message":"quota exhausted","code":"insufficient_quota"}}`))
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected InvokeError, got %T", err)
	}
	if invokeErr.Code != "upstream_rate_limited" {
		t.Fatalf("code = %q, want upstream_rate_limited", invokeErr.Code)
	}
	if policy := FailurePolicyFor(account, err); policy.Scope != FailureScopeQuota || !policy.RetryFailover || !policy.ApplyChatQuota {
		t.Fatalf("policy = %#v, want quota-scoped failover", policy)
	}
}

func TestGatewayAPIKeyForbiddenHTMLChallengeMapsToSharedFailure(t *testing.T) {
	account := core.Account{
		ID:       "acct_new_api",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "new-api-token",
			Metadata: map[string]string{
				"account_login_method":   "api_key",
				"api_key_quota_provider": "new-api",
				"base_url":               "https://new-api.example.test",
			},
		},
	}
	err := mapHTTPErrorForAccount(account, http.StatusForbidden, []byte(`<!doctype html><html><body>Cloudflare challenge</body></html>`))
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected InvokeError, got %T", err)
	}
	if invokeErr.Code != "upstream_temporarily_unavailable" {
		t.Fatalf("code = %q, want upstream_temporarily_unavailable", invokeErr.Code)
	}
	if policy := FailurePolicyFor(account, err); policy.Scope != FailureScopeSharedUpstream || !policy.RetryFailover {
		t.Fatalf("policy = %#v, want shared upstream failover", policy)
	}
}

func TestGatewayAPIKeyUnauthorizedNoAvailableChannelMapsToSharedFailure(t *testing.T) {
	account := core.Account{
		ID:       "acct_new_api",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "new-api-token",
			Metadata: map[string]string{
				"account_login_method":   "api_key",
				"api_key_quota_provider": "new-api",
				"base_url":               "https://new-api.example.test",
			},
		},
	}
	err := mapHTTPErrorForAccount(account, http.StatusUnauthorized, []byte(`{"error":{"message":"no available channel","code":"channel:no_available_key"}}`))
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected InvokeError, got %T", err)
	}
	if invokeErr.Code != "upstream_temporarily_unavailable" {
		t.Fatalf("code = %q, want upstream_temporarily_unavailable", invokeErr.Code)
	}
	if policy := FailurePolicyFor(account, err); policy.Scope != FailureScopeSharedUpstream || !policy.RetryFailover {
		t.Fatalf("policy = %#v, want shared upstream failover", policy)
	}
}

func TestForbiddenDoesNotBlockAccount(t *testing.T) {
	err := mapHTTPError(http.StatusForbidden, []byte(`{"error":{"message":"model access denied"}}`))
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected InvokeError, got %T", err)
	}
	if invokeErr.Code != "upstream_forbidden" {
		t.Fatalf("code = %q", invokeErr.Code)
	}
	if status := FailurePolicyFor(core.Account{}, err).AccountStatus; status != core.AccountStatusCooling {
		t.Fatalf("status = %q, want %q", status, core.AccountStatusCooling)
	}
	if !FailurePolicyFor(core.Account{}, err).RetryFailover {
		t.Fatal("forbidden errors should fail over to the next account")
	}
}

func TestForbiddenInsufficientBalanceMapsToQuotaFailover(t *testing.T) {
	for _, message := range []string{"insufficient balance", "Insufficient account balance"} {
		t.Run(message, func(t *testing.T) {
			err := mapHTTPError(http.StatusForbidden, []byte(fmt.Sprintf(`{"error":{"message":%q}}`, message)))
			invokeErr, ok := err.(*InvokeError)
			if !ok {
				t.Fatalf("expected InvokeError, got %T", err)
			}
			if invokeErr.Code != "upstream_rate_limited" {
				t.Fatalf("code = %q, want upstream_rate_limited", invokeErr.Code)
			}
			policy := FailurePolicyFor(core.Account{}, err)
			if policy.Scope != FailureScopeQuota || !policy.RetryFailover || !policy.ApplyChatQuota {
				t.Fatalf("policy = %#v, want quota failover", policy)
			}
		})
	}
}

func TestGatewayAPIKeyBadGatewayInsufficientBalanceMapsToQuotaFailover(t *testing.T) {
	account := core.Account{
		ID:       "acct_sub2api",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "sub2api-token",
			Metadata: map[string]string{
				"account_login_method":   "api_key",
				"api_key_quota_provider": "sub2api",
				"base_url":               "https://sub2api.example.test",
			},
		},
	}
	err := mapHTTPErrorForAccount(account, http.StatusBadGateway, []byte(`{"error":{"message":"Insufficient account balance"}}`))
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected InvokeError, got %T", err)
	}
	if invokeErr.Code != "upstream_rate_limited" {
		t.Fatalf("code = %q, want upstream_rate_limited", invokeErr.Code)
	}
	policy := FailurePolicyFor(account, err)
	if policy.Scope != FailureScopeQuota || !policy.RetryFailover || !policy.ApplyChatQuota {
		t.Fatalf("policy = %#v, want quota failover", policy)
	}
}

func TestProviderBannedHTTPErrorUsesProviderBannedStatus(t *testing.T) {
	err := mapHTTPError(http.StatusForbidden, []byte(`{"error":{"message":"Your account has been suspended."}}`))
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected InvokeError, got %T", err)
	}
	if invokeErr.Code != "upstream_provider_banned" {
		t.Fatalf("code = %q", invokeErr.Code)
	}
	if invokeErr.Temporary {
		t.Fatal("provider-banned errors should not be temporary")
	}
	if status := FailurePolicyFor(core.Account{}, err).AccountStatus; status != core.AccountStatusProviderBanned {
		t.Fatalf("status = %q, want %q", status, core.AccountStatusProviderBanned)
	}
	if !failurePolicyPenalizes(err) {
		t.Fatal("provider-banned errors should penalize the account")
	}
}

func TestProviderBannedDetectionRequiresAccountContext(t *testing.T) {
	err := mapHTTPError(http.StatusForbidden, []byte(`{"error":{"message":"This model is disabled for your project."}}`))
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected InvokeError, got %T", err)
	}
	if invokeErr.Code != "upstream_forbidden" {
		t.Fatalf("code = %q", invokeErr.Code)
	}
}

func TestMapHTTPErrorSummarizesHTMLChallengePayload(t *testing.T) {
	err := mapHTTPError(http.StatusForbidden, []byte(`<!doctype html><html><head><title>challenge</title></head><body>Cloudflare</body></html>`))
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected InvokeError, got %T", err)
	}
	if invokeErr.Code != "upstream_forbidden" {
		t.Fatalf("code = %q", invokeErr.Code)
	}
	if strings.Contains(strings.ToLower(invokeErr.Error()), "<html") {
		t.Fatalf("error should be summarized, got %q", invokeErr.Error())
	}
}

func TestOpenAIQuotaEndpointDefaultsToCodexWhamUsage(t *testing.T) {
	previous := openAIQuotaEndpoint
	openAIQuotaEndpoint = ""
	t.Cleanup(func() { openAIQuotaEndpoint = previous })

	if got := openAIQuotaEndpointURL(); got != "https://chatgpt.com/backend-api/wham/usage" {
		t.Fatalf("default endpoint = %q", got)
	}
}

func TestOpenAIAdapterFetchQuotaMapsHTMLChallengeUsagePayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wham/usage" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><html><body>Cloudflare challenge</body></html>`))
	}))
	defer server.Close()

	previous := openAIQuotaEndpoint
	openAIQuotaEndpoint = server.URL + "/wham/usage"
	t.Cleanup(func() { openAIQuotaEndpoint = previous })

	_, err := (&OpenAIAdapter{}).FetchQuota(context.Background(), core.Account{
		ID:       "acct_openai_oauth",
		Label:    "OpenAI OAuth",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			AccessToken: "oauth-access-token",
			Metadata: map[string]string{
				"token_source": OpenAIDeviceCodeTokenSourceValue(),
			},
		},
	})
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("error = %T %v, want InvokeError", err, err)
	}
	if invokeErr.Code != "upstream_forbidden" || strings.Contains(strings.ToLower(invokeErr.Error()), "<html") {
		t.Fatalf("error = %#v", invokeErr)
	}
}

func TestOpenAIAdapterFetchQuotaUsesChatGPTUsageEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-access-token" {
			t.Fatalf("authorization = %q", got)
		}
		switch r.URL.Path {
		case "/wham/usage":
			if got := r.Header.Get("ChatGPT-Account-Id"); got != "org_123" {
				t.Fatalf("account id header = %q", got)
			}
			if got := r.Header.Get("User-Agent"); got != openAIOAuthUserAgent {
				t.Fatalf("user-agent = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"plan_type": "pro",
				"rate_limit": map[string]any{
					"primary_window": map[string]any{
						"used_percent":         42.0,
						"limit_window_seconds": 18000,
						"reset_at":             1710000000,
					},
					"secondary_window": map[string]any{
						"used_percent":         12.0,
						"limit_window_seconds": 604800,
						"reset_at":             1710600000,
					},
				},
				"credits": map[string]any{
					"has_credits": true,
					"unlimited":   false,
					"balance":     18.5,
				},
				"rate_limit_reached_type": map[string]any{
					"type": "primary_window",
				},
			})
		default:
			t.Fatalf("path = %s", r.URL.Path)
		}
	}))
	defer server.Close()

	previous := openAIQuotaEndpoint
	openAIQuotaEndpoint = server.URL + "/wham/usage"
	t.Cleanup(func() { openAIQuotaEndpoint = previous })

	adapter := &OpenAIAdapter{}
	snapshot, err := adapter.FetchQuota(context.Background(), core.Account{
		ID:       "acct_openai_oauth",
		Label:    "OpenAI OAuth",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			AccessToken: "oauth-access-token",
			Metadata: map[string]string{
				"token_source":         "openai_device_code",
				"oauth_account_id":     "org_123",
				"chatgpt_web_base_url": server.URL,
			},
		},
	})
	if err != nil {
		t.Fatalf("FetchQuota returned error: %v", err)
	}
	if snapshot.Plan != "pro" {
		t.Fatalf("plan = %q", snapshot.Plan)
	}
	if snapshot.Primary == nil || snapshot.Primary.WindowMinutes != 300 {
		t.Fatalf("primary = %#v", snapshot.Primary)
	}
	if snapshot.Primary.UsedPercent != 42 {
		t.Fatalf("primary used percent = %v, want 42", snapshot.Primary.UsedPercent)
	}
	if snapshot.Secondary == nil || snapshot.Secondary.WindowMinutes != 10080 {
		t.Fatalf("secondary = %#v", snapshot.Secondary)
	}
	if snapshot.Credits == nil || snapshot.Credits.Balance == nil || *snapshot.Credits.Balance != 18.5 {
		t.Fatalf("credits = %#v", snapshot.Credits)
	}
	if snapshot.ReachedType != "primary_window" {
		t.Fatalf("reached type = %q", snapshot.ReachedType)
	}
	if snapshot.RefreshedAt == nil {
		t.Fatal("expected refreshed at timestamp")
	}
}

func TestOpenAIAdapterFetchQuotaUsesCodexWhamUsageEndpointAndAdditionalLimits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-access-token" {
			t.Fatalf("authorization = %q", got)
		}
		switch r.URL.Path {
		case "/wham/usage":
			if got := r.Header.Get("ChatGPT-Account-Id"); got != "org_123" {
				t.Fatalf("account id header = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"plan_type": "pro",
				"rate_limit": map[string]any{
					"primary_window": map[string]any{
						"used_percent":         42.0,
						"limit_window_seconds": 3600,
						"reset_at":             1735689720,
					},
					"secondary_window": map[string]any{
						"used_percent":         5.0,
						"limit_window_seconds": 86400,
						"reset_at":             1735693200,
					},
				},
				"additional_rate_limits": []map[string]any{
					{
						"limit_name":      "codex_other",
						"metered_feature": "codex_other",
						"rate_limit": map[string]any{
							"primary_window": map[string]any{
								"used_percent":         88.0,
								"limit_window_seconds": 1800,
								"reset_at":             1735693200,
							},
						},
					},
				},
			})
		default:
			t.Fatalf("path = %s", r.URL.Path)
		}
	}))
	defer server.Close()

	previous := openAIQuotaEndpoint
	openAIQuotaEndpoint = server.URL + "/wham/usage"
	t.Cleanup(func() { openAIQuotaEndpoint = previous })

	adapter := &OpenAIAdapter{}
	snapshot, err := adapter.FetchQuota(context.Background(), core.Account{
		ID:       "acct_openai_oauth",
		Label:    "OpenAI OAuth",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			AccessToken: "oauth-access-token",
			Metadata: map[string]string{
				"token_source":         OpenAIDeviceCodeTokenSourceValue(),
				"oauth_account_id":     "org_123",
				"chatgpt_web_base_url": server.URL,
			},
		},
	})
	if err != nil {
		t.Fatalf("FetchQuota returned error: %v", err)
	}
	if snapshot.LimitID != "codex" {
		t.Fatalf("limit id = %q, want codex", snapshot.LimitID)
	}
	if snapshot.Primary == nil || snapshot.Primary.UsedPercent != 42 || snapshot.Primary.WindowMinutes != 60 {
		t.Fatalf("primary = %#v", snapshot.Primary)
	}
	if snapshot.Secondary == nil || snapshot.Secondary.UsedPercent != 5 || snapshot.Secondary.WindowMinutes != 1440 {
		t.Fatalf("secondary = %#v", snapshot.Secondary)
	}
	additional := snapshot.Additional["codex_other"]
	if additional.LimitID != "codex_other" || additional.Primary == nil || additional.Primary.UsedPercent != 88 || additional.Primary.WindowMinutes != 30 {
		t.Fatalf("additional = %#v", additional)
	}
}

func TestOpenAIAdapterFetchQuotaUsesCodexWhamUsageForCodexAuthAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/wham/usage":
			if got := r.Header.Get("Authorization"); got != "Bearer oauth-access-token" {
				t.Fatalf("authorization = %q", got)
			}
			if got := r.Header.Get("ChatGPT-Account-Id"); got != "org_123" {
				t.Fatalf("account id header = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"plan_type": "plus",
				"rate_limit": map[string]any{
					"primary_window": map[string]any{
						"used_percent":         31.0,
						"limit_window_seconds": 10800,
					},
					"secondary_window": map[string]any{
						"used_percent":         7.0,
						"limit_window_seconds": 604800,
					},
				},
			})
		default:
			t.Fatalf("path = %s", r.URL.Path)
		}
	}))
	defer server.Close()

	previous := openAIQuotaEndpoint
	openAIQuotaEndpoint = server.URL + "/wham/usage"
	t.Cleanup(func() { openAIQuotaEndpoint = previous })

	adapter := &OpenAIAdapter{}
	snapshot, err := adapter.FetchQuota(context.Background(), core.Account{
		ID:       "acct_openai_codex",
		Label:    "OpenAI Codex",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			AccessToken: "oauth-access-token",
			Metadata: map[string]string{
				"token_source":         OpenAICodexAuthTokenSourceValue(),
				"account_type":         "free",
				"oauth_account_id":     "org_123",
				"chatgpt_web_base_url": server.URL,
			},
		},
	})
	if err != nil {
		t.Fatalf("FetchQuota returned error: %v", err)
	}
	if snapshot.Source != openAIUsageDefaultSource || snapshot.Plan != "plus" {
		t.Fatalf("snapshot source/plan = %q/%q", snapshot.Source, snapshot.Plan)
	}
	if snapshot.Primary == nil || snapshot.Primary.UsedPercent != 31 || snapshot.Primary.WindowMinutes != 180 {
		t.Fatalf("primary = %#v", snapshot.Primary)
	}
	if snapshot.Secondary == nil || snapshot.Secondary.UsedPercent != 7 || snapshot.Secondary.WindowMinutes != 10080 {
		t.Fatalf("secondary = %#v", snapshot.Secondary)
	}
}

func TestOpenAIAdapterFetchQuotaUsesCodexWhamUsageForFreeCodexAuthAccounts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/wham/usage":
			if got := r.Header.Get("Authorization"); got != "Bearer oauth-access-token" {
				t.Fatalf("authorization = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"plan_type": "free",
			})
		default:
			t.Fatalf("path = %s", r.URL.Path)
		}
	}))
	defer server.Close()

	previous := openAIQuotaEndpoint
	openAIQuotaEndpoint = server.URL + "/wham/usage"
	t.Cleanup(func() { openAIQuotaEndpoint = previous })

	adapter := &OpenAIAdapter{}
	snapshot, err := adapter.FetchQuota(context.Background(), core.Account{
		ID:       "acct_openai_free",
		Label:    "OpenAI Free",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			AccessToken: "oauth-access-token",
			Metadata: map[string]string{
				"token_source":         OpenAICodexAuthTokenSourceValue(),
				"account_type":         "free",
				"chatgpt_web_base_url": server.URL,
			},
		},
	})
	if err != nil {
		t.Fatalf("FetchQuota returned error: %v", err)
	}
	if snapshot.Plan != "free" {
		t.Fatalf("plan = %q", snapshot.Plan)
	}
	if snapshot.Source != openAIUsageDefaultSource {
		t.Fatalf("source = %q", snapshot.Source)
	}
	if snapshot.RefreshedAt == nil {
		t.Fatal("expected refreshed at timestamp")
	}
}

func TestOpenAIAdapterFetchQuotaTreatsFreeAccountsWithoutImageQuotaAsExhausted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/wham/usage":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"plan_type": "free",
			})
		default:
			t.Fatalf("path = %s", r.URL.Path)
		}
	}))
	defer server.Close()

	previous := openAIQuotaEndpoint
	openAIQuotaEndpoint = server.URL + "/wham/usage"
	t.Cleanup(func() { openAIQuotaEndpoint = previous })

	adapter := &OpenAIAdapter{}
	_, err := adapter.FetchQuota(context.Background(), core.Account{
		ID:       "acct_openai_free",
		Label:    "OpenAI Free",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			AccessToken: "oauth-access-token",
			Metadata: map[string]string{
				"token_source":         OpenAICodexAuthTokenSourceValue(),
				"account_type":         "free",
				"chatgpt_web_base_url": server.URL,
			},
		},
	})
	if err != nil {
		t.Fatalf("FetchQuota returned error: %v", err)
	}
}

func TestOpenAIAdapterFetchQuotaAcceptsStringCreditBalance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "pro",
			"credits": map[string]any{
				"has_credits": true,
				"unlimited":   false,
				"balance":     "18.5",
			},
		})
	}))
	defer server.Close()

	previous := openAIQuotaEndpoint
	openAIQuotaEndpoint = server.URL + "/wham/usage"
	t.Cleanup(func() { openAIQuotaEndpoint = previous })

	adapter := &OpenAIAdapter{}
	snapshot, err := adapter.FetchQuota(context.Background(), core.Account{
		ID:       "acct_openai_oauth",
		Label:    "OpenAI OAuth",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			AccessToken: "oauth-access-token",
			Metadata: map[string]string{
				"token_source":         OpenAIDeviceCodeTokenSourceValue(),
				"chatgpt_web_base_url": server.URL,
			},
		},
	})
	if err != nil {
		t.Fatalf("FetchQuota returned error: %v", err)
	}
	if snapshot.Credits == nil || snapshot.Credits.Balance == nil || *snapshot.Credits.Balance != 18.5 {
		t.Fatalf("credits = %#v", snapshot.Credits)
	}
}

func TestOpenAIAdapterFetchQuotaKeepsUsageWhenImageQuotaUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/wham/usage":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"plan_type": "pro",
				"credits": map[string]any{
					"has_credits": true,
					"balance":     18.5,
				},
			})
		default:
			t.Fatalf("path = %s", r.URL.Path)
		}
	}))
	defer server.Close()

	previous := openAIQuotaEndpoint
	openAIQuotaEndpoint = server.URL + "/wham/usage"
	t.Cleanup(func() { openAIQuotaEndpoint = previous })

	adapter := &OpenAIAdapter{}
	snapshot, err := adapter.FetchQuota(context.Background(), core.Account{
		ID:       "acct_openai_oauth",
		Label:    "OpenAI OAuth",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			AccessToken: "oauth-access-token",
			Metadata: map[string]string{
				"token_source":         OpenAIDeviceCodeTokenSourceValue(),
				"chatgpt_web_base_url": server.URL,
			},
		},
	})
	if err != nil {
		t.Fatalf("FetchQuota returned error: %v", err)
	}
	if snapshot.Plan != "pro" || snapshot.Credits == nil || snapshot.Credits.Balance == nil || *snapshot.Credits.Balance != 18.5 {
		t.Fatalf("usage snapshot = %#v", snapshot)
	}
}

func TestOpenAIAdapterFetchQuotaUsesChatGPTEndpointForManualTokenLogin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer manual-token" {
			t.Fatalf("authorization = %q", got)
		}
		switch r.URL.Path {
		case "/wham/usage":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"plan_type": "plus",
			})
		default:
			t.Fatalf("path = %s", r.URL.Path)
		}
	}))
	defer server.Close()

	previous := openAIQuotaEndpoint
	openAIQuotaEndpoint = server.URL + "/wham/usage"
	t.Cleanup(func() { openAIQuotaEndpoint = previous })

	adapter := &OpenAIAdapter{}
	snapshot, err := adapter.FetchQuota(context.Background(), core.Account{
		ID:       "acct_openai_token",
		Label:    "OpenAI Token",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "manual-token",
			Metadata: map[string]string{
				"account_login_method": "token",
				"chatgpt_web_base_url": server.URL,
			},
		},
	})
	if err != nil {
		t.Fatalf("FetchQuota returned error: %v", err)
	}
	if snapshot.Source != "openai_chatgpt_usage" || snapshot.Plan != "plus" {
		t.Fatalf("snapshot source/plan = %q/%q", snapshot.Source, snapshot.Plan)
	}
}

func TestOpenAIAdapterFetchQuotaRejectsUnconfiguredAPIKeyQuota(t *testing.T) {
	_, err := (&OpenAIAdapter{}).FetchQuota(context.Background(), core.Account{
		ID:       "acct_openai_key",
		Label:    "OpenAI Key",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "sk-test",
			Metadata: map[string]string{
				"account_login_method": "api_key",
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("FetchQuota error = %v, want not configured", err)
	}
}

func TestOpenAIAdapterFetchQuotaRejectsOfficialAPIKeyBilling(t *testing.T) {
	_, err := (&OpenAIAdapter{}).FetchQuota(context.Background(), core.Account{
		ID:       "acct_openai_key",
		Label:    "OpenAI Key",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "sk-test",
			Metadata: map[string]string{
				"base_url":               "https://api.openai.com/v1",
				"account_login_method":   "api_key",
				"api_key_quota_provider": "new-api",
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "do not expose dashboard billing quota") {
		t.Fatalf("FetchQuota error = %v, want official API rejection", err)
	}
}

func TestOpenAIAPIKeyQuotaBillingForbiddenIsNotCredentialExpired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"billing scope denied"}}`, http.StatusForbidden)
	}))
	defer server.Close()

	_, err := (&OpenAIAdapter{}).FetchQuota(context.Background(), core.Account{
		ID:       "acct_openai_key",
		Label:    "OpenAI Key",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "sk-test",
			Metadata: map[string]string{
				"base_url":               server.URL,
				"account_login_method":   "api_key",
				"api_key_quota_provider": "new-api",
			},
		},
	})
	invokeErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected InvokeError, got %T %v", err, err)
	}
	if invokeErr.Code != "upstream_forbidden" {
		t.Fatalf("code = %q, want upstream_forbidden", invokeErr.Code)
	}
	if FailurePolicyFor(core.Account{}, err).AccountStatus != core.AccountStatusCooling {
		t.Fatalf("status = %q, want cooldown", FailurePolicyFor(core.Account{}, err).AccountStatus)
	}
}

func TestOpenAIAdapterFetchQuotaUsesAPIKeyBillingEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/dashboard/billing/subscription":
			if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
				t.Fatalf("authorization = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"hard_limit_usd":     120.0,
				"soft_limit_usd":     100.0,
				"has_payment_method": true,
			})
		case "/v1/dashboard/billing/usage":
			if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
				t.Fatalf("authorization = %q", got)
			}
			if r.URL.Query().Get("start_date") == "" || r.URL.Query().Get("end_date") == "" {
				t.Fatalf("usage query missing dates: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_usage": 3456.0,
			})
		default:
			t.Fatalf("path = %s", r.URL.Path)
		}
	}))
	defer server.Close()

	adapter := &OpenAIAdapter{}
	snapshot, err := adapter.FetchQuota(context.Background(), core.Account{
		ID:       "acct_openai_key",
		Label:    "OpenAI Key",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "sk-test",
			Metadata: map[string]string{
				"base_url":               server.URL,
				"account_type":           "api_key",
				"account_login_method":   "api_key",
				"api_key_quota_provider": "new-api",
			},
		},
	})
	if err != nil {
		t.Fatalf("FetchQuota returned error: %v", err)
	}
	if snapshot.Source != "new_api_key_billing" || snapshot.Plan != "api_key" {
		t.Fatalf("snapshot source/plan = %q/%q", snapshot.Source, snapshot.Plan)
	}
	if snapshot.Credits == nil || snapshot.Credits.Balance == nil || math.Abs(*snapshot.Credits.Balance-85.44) > 0.0001 {
		t.Fatalf("credits = %#v", snapshot.Credits)
	}
	if snapshot.Primary == nil || math.Abs(snapshot.Primary.UsedPercent-28.8) > 0.0001 {
		t.Fatalf("primary = %#v", snapshot.Primary)
	}
	if snapshot.RefreshedAt == nil {
		t.Fatal("expected refreshed at timestamp")
	}
}

func TestOpenAIAdapterFetchQuotaRejectsNewAPISubscriptionErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/dashboard/billing/subscription" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"type": "new_api_error", "message": "database unavailable"},
		})
	}))
	defer server.Close()

	_, err := (&OpenAIAdapter{}).FetchQuota(context.Background(), core.Account{
		ID:       "acct_new_api_key",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "sk-test",
			Metadata: map[string]string{
				"base_url":               server.URL,
				"account_login_method":   "api_key",
				"api_key_quota_provider": "new-api",
			},
		},
	})
	invokeErr, ok := err.(*InvokeError)
	if !ok || invokeErr.Code != "upstream_rejected" || !strings.Contains(invokeErr.Error(), "database unavailable") {
		t.Fatalf("FetchQuota err = %#v, want upstream_rejected with new-api message", err)
	}
}

func TestOpenAIAdapterFetchQuotaRejectsNewAPIUsageErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/dashboard/billing/subscription":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"hard_limit_usd":     10.0,
				"soft_limit_usd":     10.0,
				"has_payment_method": true,
			})
		case "/v1/dashboard/billing/usage":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"type": "new_api_error", "message": "usage unavailable"},
			})
		default:
			t.Fatalf("path = %s", r.URL.Path)
		}
	}))
	defer server.Close()

	_, err := (&OpenAIAdapter{}).FetchQuota(context.Background(), core.Account{
		ID:       "acct_new_api_key",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "sk-test",
			Metadata: map[string]string{
				"base_url":               server.URL,
				"account_login_method":   "api_key",
				"api_key_quota_provider": "new-api",
			},
		},
	})
	invokeErr, ok := err.(*InvokeError)
	if !ok || invokeErr.Code != "upstream_rejected" || !strings.Contains(invokeErr.Error(), "usage unavailable") {
		t.Fatalf("FetchQuota err = %#v, want upstream_rejected with new-api message", err)
	}
}

func TestOpenAIAdapterFetchQuotaTreatsNewAPIUnlimitedSentinelAsUnlimited(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/dashboard/billing/subscription":
			if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
				t.Fatalf("authorization = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"hard_limit_usd":     100000000.0,
				"soft_limit_usd":     100000000.0,
				"has_payment_method": true,
			})
		case "/v1/dashboard/billing/usage":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_usage": 5854.0,
			})
		default:
			t.Fatalf("path = %s", r.URL.Path)
		}
	}))
	defer server.Close()

	adapter := &OpenAIAdapter{}
	snapshot, err := adapter.FetchQuota(context.Background(), core.Account{
		ID:       "acct_openai_key",
		Label:    "OpenAI Key",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "sk-test",
			Metadata: map[string]string{
				"base_url":               server.URL,
				"account_type":           "api_key",
				"account_login_method":   "api_key",
				"api_key_quota_provider": "new-api",
			},
		},
	})
	if err != nil {
		t.Fatalf("FetchQuota returned error: %v", err)
	}
	if snapshot.Source != "new_api_key_billing" {
		t.Fatalf("snapshot source = %q", snapshot.Source)
	}
	if snapshot.Credits == nil || !snapshot.Credits.Unlimited || snapshot.Credits.Balance != nil {
		t.Fatalf("credits = %#v", snapshot.Credits)
	}
	if snapshot.RefreshedAt == nil {
		t.Fatal("expected refreshed at timestamp")
	}
}

func TestOpenAIAdapterFetchQuotaUsesSub2APIUsageEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sub2api-token" {
			t.Fatalf("authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mode":    "quota_limited",
			"isValid": true,
			"status":  "active",
			"quota": map[string]any{
				"limit":     100,
				"used":      33.75,
				"remaining": 66.25,
				"unit":      "USD",
			},
			"remaining": 66.25,
			"unit":      "USD",
		})
	}))
	defer server.Close()

	adapter := &OpenAIAdapter{}
	snapshot, err := adapter.FetchQuota(context.Background(), core.Account{
		ID:       "acct_sub2api_key",
		Label:    "Sub2API Key",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "sub2api-token",
			Metadata: map[string]string{
				"base_url":               server.URL,
				"account_login_method":   "api_key",
				"api_key_quota_provider": "sub2api",
			},
		},
	})
	if err != nil {
		t.Fatalf("FetchQuota returned error: %v", err)
	}
	if snapshot.Source != "sub2api_usage" || snapshot.Credits == nil || snapshot.Credits.Balance == nil || *snapshot.Credits.Balance != 66.25 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.Primary == nil || snapshot.Primary.Name != "billing" || snapshot.Primary.UsedPercent != 33.75 {
		t.Fatalf("primary quota = %#v", snapshot.Primary)
	}
}

func TestOpenAIAdapterFetchQuotaRejectsSub2APIInvalidKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"isValid": false,
			"status":  "invalid",
			"balance": 10,
		})
	}))
	defer server.Close()

	_, err := (&OpenAIAdapter{}).FetchQuota(context.Background(), core.Account{
		ID:       "acct_sub2api_key",
		Label:    "Sub2API Key",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "sub2api-token",
			Metadata: map[string]string{
				"base_url":               server.URL,
				"account_login_method":   "api_key",
				"api_key_quota_provider": "sub2api",
			},
		},
	})
	if invokeErr, ok := err.(*InvokeError); !ok || invokeErr.Code != "upstream_auth_error" || invokeErr.Temporary {
		t.Fatalf("FetchQuota err = %#v, want non-temporary upstream_auth_error", err)
	}
}

func TestOpenAIAdapterFetchQuotaUsesSub2APIUsageBalanceMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sub2api-token" {
			t.Fatalf("authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mode":      "unrestricted",
			"isValid":   true,
			"planName":  "wallet",
			"remaining": "12.50",
			"balance":   "12.50",
			"unit":      "USD",
		})
	}))
	defer server.Close()

	adapter := &OpenAIAdapter{}
	snapshot, err := adapter.FetchQuota(context.Background(), core.Account{
		ID:       "acct_sub2api_key",
		Label:    "Sub2API Key",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "sub2api-token",
			Metadata: map[string]string{
				"base_url":               server.URL + "/v1",
				"account_login_method":   "api_key",
				"api_key_quota_provider": "sub2api",
			},
		},
	})
	if err != nil {
		t.Fatalf("FetchQuota returned error: %v", err)
	}
	if snapshot.Source != "sub2api_usage" || snapshot.Plan != "wallet" {
		t.Fatalf("snapshot source/plan = %q/%q", snapshot.Source, snapshot.Plan)
	}
	if snapshot.Credits == nil || snapshot.Credits.Balance == nil || *snapshot.Credits.Balance != 12.50 {
		t.Fatalf("credits = %#v", snapshot.Credits)
	}
}

func TestOpenAIAdapterFetchQuotaUsesGatewayQuotaEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ag/v1/account/quota" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer gw-client-key" {
			t.Fatalf("authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"remaining_nano_usd": 7250000000,
			"balance_nano_usd":   9000000000,
		})
	}))
	defer server.Close()

	adapter := &OpenAIAdapter{}
	snapshot, err := adapter.FetchQuota(context.Background(), core.Account{
		ID:       "acct_gateway_key",
		Label:    "Gateway Key",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "gw-client-key",
			Metadata: map[string]string{
				"base_url":               server.URL,
				"account_login_method":   "api_key",
				"api_key_quota_provider": "gateway",
			},
		},
	})
	if err != nil {
		t.Fatalf("FetchQuota returned error: %v", err)
	}
	if snapshot.Source != "ai_gateway_quota" || snapshot.Plan != "api_key" {
		t.Fatalf("snapshot source/plan = %q/%q", snapshot.Source, snapshot.Plan)
	}
	if snapshot.Credits == nil || snapshot.Credits.Balance == nil || *snapshot.Credits.Balance != 7.25 {
		t.Fatalf("credits = %#v", snapshot.Credits)
	}
	if snapshot.RefreshedAt == nil {
		t.Fatal("expected refreshed at timestamp")
	}
}

func TestOpenAIAdapterFetchGatewayQuotaStripsOpenAIBasePath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ag/v1/account/quota" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"remaining_nano_usd": 5500000000,
		})
	}))
	defer server.Close()

	snapshot, err := (&OpenAIAdapter{}).FetchQuota(context.Background(), core.Account{
		ID:       "acct_gateway_key",
		Label:    "Gateway Key",
		Provider: core.ProviderOpenAI,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "gw-client-key",
			Metadata: map[string]string{
				"base_url":               server.URL + "/v1",
				"account_login_method":   "api_key",
				"api_key_quota_provider": "gateway",
			},
		},
	})
	if err != nil {
		t.Fatalf("FetchQuota returned error: %v", err)
	}
	if snapshot.Credits == nil || snapshot.Credits.Balance == nil || *snapshot.Credits.Balance != 5.5 {
		t.Fatalf("credits = %#v", snapshot.Credits)
	}
}

func TestClaudeAdapterFetchQuotaUsesSub2APIUsageEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer claude-sub2api-token" {
			t.Fatalf("authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mode":      "unrestricted",
			"isValid":   true,
			"planName":  "claude-wallet",
			"remaining": 42.5,
			"unit":      "USD",
		})
	}))
	defer server.Close()

	snapshot, err := (&ClaudeAdapter{}).FetchQuota(context.Background(), core.Account{
		ID:       "acct_claude_sub2api",
		Label:    "Claude Sub2API Key",
		Provider: core.ProviderClaude,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "claude-sub2api-token",
			Metadata: map[string]string{
				"base_url":               server.URL,
				"account_login_method":   "api_key",
				"api_key_quota_provider": "sub2api",
			},
		},
	})
	if err != nil {
		t.Fatalf("FetchQuota returned error: %v", err)
	}
	if snapshot.Source != "sub2api_usage" || snapshot.Plan != "claude-wallet" {
		t.Fatalf("snapshot source/plan = %q/%q", snapshot.Source, snapshot.Plan)
	}
	if snapshot.Credits == nil || snapshot.Credits.Balance == nil || *snapshot.Credits.Balance != 42.5 {
		t.Fatalf("credits = %#v", snapshot.Credits)
	}
}

func TestClaudeAdapterFetchQuotaUsesGatewayQuotaEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ag/v1/account/quota" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer claude-gateway-token" {
			t.Fatalf("authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"remaining_nano_usd": 3250000000,
			"balance_nano_usd":   4000000000,
		})
	}))
	defer server.Close()

	snapshot, err := (&ClaudeAdapter{}).FetchQuota(context.Background(), core.Account{
		ID:       "acct_claude_gateway",
		Label:    "Claude Gateway Key",
		Provider: core.ProviderClaude,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "claude-gateway-token",
			Metadata: map[string]string{
				"base_url":               server.URL,
				"account_login_method":   "api_key",
				"api_key_quota_provider": "gateway",
			},
		},
	})
	if err != nil {
		t.Fatalf("FetchQuota returned error: %v", err)
	}
	if snapshot.Source != "ai_gateway_quota" || snapshot.Credits == nil || snapshot.Credits.Balance == nil || *snapshot.Credits.Balance != 3.25 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestClaudeAdapterFetchQuotaUsesOAuthUsageEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer claude-oauth-token" {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.Header.Get("anthropic-beta"); got != claudeOAuthBetaHeader {
			t.Fatalf("anthropic-beta = %q", got)
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"plan_type": "max",
			"five_hour": map[string]any{
				"utilization": 0.4,
				"resets_at":   "2026-04-27T10:00:00Z",
			},
			"seven_day": map[string]any{
				"utilization": 0.25,
				"resets_at":   "2026-05-01T10:00:00Z",
			},
			"extra_usage": map[string]any{
				"is_enabled":    true,
				"monthly_limit": 50.0,
				"used_credits":  12.5,
			},
		})
	}))
	defer server.Close()

	previous := claudeQuotaEndpoint
	claudeQuotaEndpoint = server.URL + "/api/oauth/usage"
	t.Cleanup(func() { claudeQuotaEndpoint = previous })

	adapter := &ClaudeAdapter{}
	snapshot, err := adapter.FetchQuota(context.Background(), core.Account{
		ID:       "acct_claude_oauth",
		Label:    "Claude OAuth",
		Provider: core.ProviderClaude,
		Credential: core.Credential{
			Mode:        claudeOAuthMode,
			AccessToken: "claude-oauth-token",
			Metadata: map[string]string{
				"token_source": claudeOAuthTokenSource,
			},
		},
	})
	if err != nil {
		t.Fatalf("FetchQuota returned error: %v", err)
	}
	if snapshot.Plan != "max" {
		t.Fatalf("plan = %q", snapshot.Plan)
	}
	if snapshot.Primary == nil || snapshot.Primary.Name != claudeTierFiveHour {
		t.Fatalf("primary = %#v", snapshot.Primary)
	}
	if snapshot.Primary.UsedPercent != 40 {
		t.Fatalf("primary used percent = %v, want 40", snapshot.Primary.UsedPercent)
	}
	if snapshot.Secondary == nil || snapshot.Secondary.Name != claudeTierSevenDay {
		t.Fatalf("secondary = %#v", snapshot.Secondary)
	}
	if snapshot.Secondary.UsedPercent != 25 {
		t.Fatalf("secondary used percent = %v, want 25", snapshot.Secondary.UsedPercent)
	}
	if snapshot.Credits == nil || snapshot.Credits.Balance == nil || *snapshot.Credits.Balance != 37.5 {
		t.Fatalf("credits = %#v", snapshot.Credits)
	}
	if snapshot.RefreshedAt == nil {
		t.Fatal("expected refreshed at timestamp")
	}
}

func intPtr(value int) *int {
	return &value
}

func floatPtr(value float64) *float64 {
	return &value
}

func testJWT(t *testing.T, expiresAt time.Time, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	claims["exp"] = expiresAt.Unix()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}
