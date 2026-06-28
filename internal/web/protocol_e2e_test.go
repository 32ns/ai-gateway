package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/32ns/ai-gateway/internal/accounts"
	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/failover"
	"github.com/32ns/ai-gateway/internal/gateway"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/routing"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestProtocolE2EChatCompletionsFailoverAndAudit(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_e2e_key")

	failUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("fail upstream path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer failUpstream.Close()

	okUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("ok upstream path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer ok-token" {
			t.Fatalf("authorization = %q, want Bearer ok-token", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if body["model"] != "gpt-4.1" {
			t.Fatalf("upstream model = %#v", body["model"])
		}
		cacheKey, _ := body["prompt_cache_key"].(string)
		if !strings.HasPrefix(cacheKey, "agpc_") {
			t.Fatalf("prompt_cache_key = %#v, want derived agpc_ key", body["prompt_cache_key"])
		}
		if strings.Contains(cacheKey, "sess_e2e") {
			t.Fatalf("prompt_cache_key should not expose session id: %q", cacheKey)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl_e2e",
			"model":   "gpt-4.1",
			"created": 1710000000,
			"choices": []map[string]any{
				{"message": map[string]any{"content": "e2e ok"}, "finish_reason": "stop"},
			},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5},
		})
	}))
	defer okUpstream.Close()

	for _, account := range []core.Account{
		{
			ID:       "acct_fail",
			Provider: core.ProviderOpenAI,
			Label:    "Failing",
			Status:   core.AccountStatusActive,
			Priority: 200,
			Weight:   100,
			Credential: core.Credential{
				AccessToken: "fail-token",
				Metadata:    map[string]string{"base_url": failUpstream.URL},
			},
		},
		{
			ID:       "acct_ok",
			Provider: core.ProviderOpenAI,
			Label:    "Working",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
			Credential: core.Credential{
				AccessToken: "ok-token",
				Metadata:    map[string]string{"base_url": okUpstream.URL},
			},
		},
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatal(err)
		}
	}

	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)
	server := NewServerWithOptions(control, gatewayService, ServerOptions{
		StatePath: "data/state.db",
		MasterKey: "cache-e2e-secret",
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer gw_e2e_key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session-id", "sess_e2e")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal gateway response: %v", err)
	}
	if response.ID != "chatcmpl_e2e" || response.Model != "gpt-4.1" || len(response.Choices) != 1 || response.Choices[0].Message.Content != "e2e ok" {
		t.Fatalf("response = %#v", response)
	}

	audits := repo.ListAudit(1)
	if len(audits) != 1 {
		t.Fatalf("len(audits) = %d, want 1", len(audits))
	}
	if audits[0].Status != "ok" || audits[0].AccountID != "acct_ok" || len(audits[0].Attempts) != 2 {
		t.Fatalf("audit = %#v", audits[0])
	}
	if audits[0].Attempts[0].AccountID != "acct_fail" || audits[0].Attempts[0].Status != "invoke_error" {
		t.Fatalf("first attempt = %#v", audits[0].Attempts[0])
	}
	if audits[0].Attempts[1].AccountID != "acct_ok" || audits[0].Attempts[1].Status != "ok" {
		t.Fatalf("second attempt = %#v", audits[0].Attempts[1])
	}
}

func TestProtocolE2EResponsesDerivesPromptCacheKey(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	mustSeedProtocolClient(t, control, "gw_responses_cache_key")

	upstreamPayloads := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("upstream path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer responses-token" {
			t.Fatalf("authorization = %q, want Bearer responses-token", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		upstreamPayloads <- body

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "resp_e2e_cache",
			"object":      "response",
			"model":       "gpt-4.1",
			"status":      "completed",
			"output_text": "responses ok",
			"usage":       map[string]any{"input_tokens": 4, "output_tokens": 2, "total_tokens": 6},
		})
	}))
	defer upstream.Close()

	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_responses_cache",
		Provider: core.ProviderOpenAI,
		Label:    "Responses Cache",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			AccessToken: "responses-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatal(err)
	}

	gatewayService := gateway.New(
		repo,
		routing.NewRouter(),
		failover.NewEngine(accounts.NewPool(repo), registry),
	)
	server := NewServerWithOptions(control, gatewayService, ServerOptions{
		StatePath: "data/state.db",
		MasterKey: "cache-e2e-secret",
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-4.1","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer gw_responses_cache_key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session-id", "sess_responses_e2e")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		Status     string `json:"status"`
		OutputText string `json:"output_text"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal gateway response: %v", err)
	}
	if response.ID != "resp_e2e_cache" || response.Model != "gpt-4.1" || response.Status != "completed" || response.OutputText != "responses ok" {
		t.Fatalf("response = %#v", response)
	}

	upstreamPayload := receivePayload(t, upstreamPayloads)
	cacheKey, _ := upstreamPayload["prompt_cache_key"].(string)
	if !strings.HasPrefix(cacheKey, "agpc_") {
		t.Fatalf("prompt_cache_key = %#v, want derived agpc_ key", upstreamPayload["prompt_cache_key"])
	}
	if strings.Contains(cacheKey, "sess_responses_e2e") {
		t.Fatalf("prompt_cache_key should not expose session id: %q", cacheKey)
	}

	binding, err := repo.GetOpenAIResponseBinding("resp_e2e_cache")
	if err != nil {
		t.Fatalf("GetOpenAIResponseBinding returned error: %v", err)
	}
	if binding.PromptCacheKey != cacheKey {
		t.Fatalf("binding prompt cache key = %q, want %q", binding.PromptCacheKey, cacheKey)
	}
}
