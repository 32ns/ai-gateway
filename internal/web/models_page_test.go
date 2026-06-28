package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestModelsPageUsesProviderTabs(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{}))
	for _, model := range []core.ModelConfig{
		{ID: "gpt-alpha", Provider: core.ProviderOpenAI, Enabled: true, Source: core.ModelSourceManual},
		{ID: "claude-sonnet-4-6", Provider: core.ProviderClaude, Enabled: true, Source: core.ModelSourceManual},
		{ID: "gpt-disabled", Provider: core.ProviderOpenAI, Enabled: false, Source: core.ModelSourceManual},
	} {
		if err := repo.UpsertModel(model); err != nil {
			t.Fatal(err)
		}
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/admin/models", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`class="group-bar model-provider-bar"`,
		`href="/admin/models?provider=openai"`,
		`href="/admin/models?provider=claude"`,
		`class="group-tab active"`,
		`>OpenAI</a>`,
		`>Claude</a>`,
		`data-group-settings-open="model-billing-1"`,
		`data-group-settings-open="model-billing-2"`,
		`id="model-billing-1"`,
		`id="model-billing-2"`,
		`name="current_provider" value="openai"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("models page missing %q: %s", want, body)
		}
	}
	if !strings.Contains(body, "gpt-alpha") || !strings.Contains(body, "gpt-disabled") {
		t.Fatalf("OpenAI tab missing OpenAI models: %s", body)
	}
	if strings.Contains(body, "claude-sonnet-4-6") {
		t.Fatalf("OpenAI tab should not render Claude model rows: %s", body)
	}
	if strings.Contains(body, "price_cache_write_short") || strings.Contains(body, "cache_write_price_usd_per_1m") {
		t.Fatalf("OpenAI models page should not render cache write pricing fields: %s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/models?provider=claude", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = rec.Body.String()
	for _, want := range []string{
		`href="/admin/models?provider=openai"`,
		`href="/admin/models?provider=claude"`,
		`claude-sonnet-4-6`,
		`data-group-settings-open="model-billing-0"`,
		`id="model-billing-0"`,
		`name="current_provider" value="claude"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Claude tab missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "gpt-alpha") || strings.Contains(body, "gpt-disabled") {
		t.Fatalf("Claude tab should not render OpenAI model rows: %s", body)
	}
	if strings.Contains(body, "price_cache_write_short") || strings.Contains(body, "cache_write_price_usd_per_1m") {
		t.Fatalf("Claude models page should not render cache write pricing fields: %s", body)
	}
}

func TestModelActionsRedirectToProviderTab(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{}))
	if err := repo.UpsertModel(core.ModelConfig{
		ID:       "claude-sonnet-4-6",
		Provider: core.ProviderClaude,
		Enabled:  true,
		Source:   core.ModelSourceManual,
	}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("current_provider", string(core.ProviderClaude))
	req := httptest.NewRequest(http.MethodPost, "/admin/models/claude-sonnet-4-6/toggle", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); location != "/admin/models?provider=claude" {
		t.Fatalf("Location = %q, want Claude provider tab", location)
	}
}

func TestUserModelsPageGroupsPricesByProvider(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}, &providers.ClaudeAdapter{}))
	for _, model := range []core.ModelConfig{
		{ID: "claude-public", Provider: core.ProviderClaude, Enabled: true, VisibleGroups: []string{core.DefaultAccountGroupName}, InputPriceNanoUSDPer1M: core.NanoUSDPerUSD},
		{ID: "gpt-public", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{core.DefaultAccountGroupName}, InputPriceNanoUSDPer1M: core.NanoUSDPerUSD},
	} {
		if err := repo.UpsertModel(model); err != nil {
			t.Fatal(err)
		}
	}

	server := NewServer(control, nil, "data/state.db")
	req := httptest.NewRequest(http.MethodGet, "/models", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`class="user-model-provider-section"`,
		`id="user-model-provider-openai"`,
		`id="user-model-provider-claude"`,
		`>OpenAI</h3>`,
		`>Claude</h3>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("models page missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "price_cache_write_short") || strings.Contains(body, "cache_write_price_usd_per_1m") {
		t.Fatalf("user models page should not render cache write pricing fields: %s", body)
	}
	openAISection := strings.Index(body, `id="user-model-provider-openai"`)
	openAIModel := strings.Index(body, `data-copy-value="gpt-public"`)
	claudeSection := strings.Index(body, `id="user-model-provider-claude"`)
	claudeModel := strings.Index(body, `data-copy-value="claude-public"`)
	if openAISection < 0 || openAIModel < 0 || claudeSection < 0 || claudeModel < 0 {
		t.Fatalf("models page missing provider or model markers: %s", body)
	}
	if !(openAISection < openAIModel && openAIModel < claudeSection && claudeSection < claudeModel) {
		t.Fatalf("models page did not group rows by provider: %s", body)
	}
}
