package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestApplyOpenAICacheAffinityMetadataUsesSessionFallback(t *testing.T) {
	raw := map[string]json.RawMessage{}
	metadata := applyOpenAICacheAffinityMetadata(nil, raw, "gpt-5.5", "session-id:sess_123")

	if metadata["route_affinity_key"] != "session-id:sess_123" {
		t.Fatalf("route_affinity_key = %q", metadata["route_affinity_key"])
	}
	if metadata["prompt_cache_key"] != "" {
		t.Fatalf("prompt_cache_key = %q, want empty for session fallback", metadata["prompt_cache_key"])
	}
	if metadata["cache_affinity_key"] != "" {
		t.Fatalf("cache_affinity_key = %q, want empty for session fallback", metadata["cache_affinity_key"])
	}
	if metadata["route_affinity_model"] != "gpt-5.5" {
		t.Fatalf("route_affinity_model = %q", metadata["route_affinity_model"])
	}
}

func TestOpenAIPromptCacheKeyDerivesOpaqueSessionKey(t *testing.T) {
	server := NewServerWithOptions(nil, nil, ServerOptions{MasterKey: "cache-secret"})
	raw := map[string]json.RawMessage{}
	client := &core.APIClient{ID: "client_a"}

	key := server.openAIPromptCacheKey(raw, "gpt-5.5", client, "session-id:sess_123")
	repeated := server.openAIPromptCacheKey(raw, "gpt-5.5", client, "session-id:sess_123")
	otherClient := server.openAIPromptCacheKey(raw, "gpt-5.5", &core.APIClient{ID: "client_b"}, "session-id:sess_123")
	otherModel := server.openAIPromptCacheKey(raw, "gpt-4.1", client, "session-id:sess_123")

	if !strings.HasPrefix(key, "agpc_") {
		t.Fatalf("prompt cache key = %q, want agpc_ prefix", key)
	}
	if key != repeated {
		t.Fatalf("derived key changed: %q vs %q", key, repeated)
	}
	if strings.Contains(key, "sess_123") {
		t.Fatalf("derived key exposes session id: %q", key)
	}
	if otherClient == key || otherModel == key {
		t.Fatalf("derived key should vary by client/model: key=%q otherClient=%q otherModel=%q", key, otherClient, otherModel)
	}
}

func TestOpenAIPromptCacheKeyRequiresMasterKey(t *testing.T) {
	server := NewServer(nil, nil, "")
	raw := map[string]json.RawMessage{}

	if got := server.openAIPromptCacheKey(raw, "gpt-5.5", &core.APIClient{ID: "client_a"}, "session-id:sess_123"); got != "" {
		t.Fatalf("prompt cache key = %q, want empty without master key", got)
	}
}

func TestOpenAIPromptCacheKeyPrefersExplicitKey(t *testing.T) {
	server := NewServerWithOptions(nil, nil, ServerOptions{MasterKey: "cache-secret"})
	raw := map[string]json.RawMessage{"prompt_cache_key": json.RawMessage(`"explicit-cache"`)}

	if got := server.openAIPromptCacheKey(raw, "gpt-5.5", &core.APIClient{ID: "client_a"}, "session-id:sess_123"); got != "explicit-cache" {
		t.Fatalf("prompt cache key = %q, want explicit-cache", got)
	}
}

func TestOpenAIPromptCacheKeyUsesPreviousResponseBinding(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServerWithOptions(control, nil, ServerOptions{MasterKey: "cache-secret"})
	client := &core.APIClient{ID: "client_a"}
	if err := repo.UpsertClient(*client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{
		ResponseID:     "resp_prev",
		AccountID:      "acct_1",
		ClientID:       client.ID,
		PromptCacheKey: "agpc_binding",
	}); err != nil {
		t.Fatal(err)
	}
	raw := map[string]json.RawMessage{"previous_response_id": json.RawMessage(`"resp_prev"`)}

	if got := server.openAIPromptCacheKey(raw, "gpt-5.5", client, ""); got != "agpc_binding" {
		t.Fatalf("prompt cache key = %q, want binding key", got)
	}
}

func TestOpenAIPromptCacheKeyDerivesFromPreviousResponseWhenBindingEmpty(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServerWithOptions(control, nil, ServerOptions{MasterKey: "cache-secret"})
	client := &core.APIClient{ID: "client_a"}
	if err := repo.UpsertClient(*client); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{
		ResponseID: "resp_prev",
		AccountID:  "acct_1",
		ClientID:   client.ID,
	}); err != nil {
		t.Fatal(err)
	}
	raw := map[string]json.RawMessage{"previous_response_id": json.RawMessage(`"resp_prev"`)}

	got := server.openAIPromptCacheKey(raw, "gpt-5.5", client, "")
	if got == "" || !strings.HasPrefix(got, "agpc_") {
		t.Fatalf("prompt cache key = %q, want derived agpc_ key", got)
	}
}

func TestApplyOpenAICacheAffinityMetadataPrefersExplicitPromptCacheKey(t *testing.T) {
	raw := map[string]json.RawMessage{
		"prompt_cache_key": json.RawMessage(`"explicit-cache"`),
	}
	metadata := applyOpenAICacheAffinityMetadata(nil, raw, "gpt-5.5", "session-id:sess_123")

	if metadata["cache_affinity_key"] != "explicit-cache" {
		t.Fatalf("cache_affinity_key = %q", metadata["cache_affinity_key"])
	}
	if metadata["prompt_cache_key"] != "explicit-cache" {
		t.Fatalf("prompt_cache_key = %q", metadata["prompt_cache_key"])
	}
}

func TestOpenAIRequestSessionAffinityKeyUsesCodexSessionHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("session-id", "sess_abc")
	req.Header.Set("thread-id", "thread_def")

	if got := openAIRequestSessionAffinityKey(req); got != "session-id:sess_abc" {
		t.Fatalf("session affinity key = %q", got)
	}
}

func TestRouteAffinityMetadataForModelUsesSessionKey(t *testing.T) {
	metadata := routeAffinityMetadataForModel("gpt-image-2", "session-id:sess_123")

	if metadata["route_affinity_key"] != "session-id:sess_123" {
		t.Fatalf("route_affinity_key = %q", metadata["route_affinity_key"])
	}
	if metadata["route_affinity_model"] != "gpt-image-2" {
		t.Fatalf("route_affinity_model = %q", metadata["route_affinity_model"])
	}
}

func TestRouteAffinityMetadataForModelIgnoresEmptySessionKey(t *testing.T) {
	if metadata := routeAffinityMetadataForModel("gpt-image-2", ""); metadata != nil {
		t.Fatalf("metadata = %#v, want nil", metadata)
	}
}

func TestOpenAIRequestSessionAffinityKeyIgnoresInstallationScopedHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("x-codex-installation-id", "install_abc")
	req.Header.Set("x-codex-window-id", "window_def")

	if got := openAIRequestSessionAffinityKey(req); got != "" {
		t.Fatalf("session affinity key = %q, want empty", got)
	}
}

func TestOpenAIResponseEnvelopeFiltersInternalRouteMetadata(t *testing.T) {
	envelope := openAIResponseEnvelope(&core.GatewayResponse{Model: "gpt-4.1"}, &core.ResponsesRequest{
		Metadata: map[string]string{
			"user":                 "visible",
			"route_affinity_key":   "session-id:sess_123",
			"route_affinity_model": "gpt-4.1",
			"cache_affinity_key":   "private-cache",
			"prompt_cache_key":     "private-prompt-cache",
		},
	})

	metadata, ok := envelope["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata = %#v", envelope["metadata"])
	}
	if len(metadata) != 1 || metadata["user"] != "visible" {
		t.Fatalf("metadata = %#v", metadata)
	}
}
