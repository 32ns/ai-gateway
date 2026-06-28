package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/accounts"
	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/failover"
	"github.com/32ns/ai-gateway/internal/gateway"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/routing"
	"github.com/32ns/ai-gateway/internal/storage"
	"github.com/gorilla/websocket"
)

type syntheticFailedResponsesAdapter struct{}

func (a *syntheticFailedResponsesAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *syntheticFailedResponsesAdapter) DisplayName() string { return "Synthetic Failed Responses" }

func (a *syntheticFailedResponsesAdapter) ListModels(context.Context) []core.ModelSpec {
	return []core.ModelSpec{{Name: "gpt-4.1", Provider: core.ProviderOpenAI}}
}

func (a *syntheticFailedResponsesAdapter) Invoke(context.Context, core.RouteDecision, *core.GatewayRequest) (*core.GatewayResponse, error) {
	return nil, errors.New("not used")
}

func (a *syntheticFailedResponsesAdapter) InvokeResponses(_ context.Context, decision core.RouteDecision, _ *core.ResponsesRequest) (*core.GatewayResponse, error) {
	return &core.GatewayResponse{
		ID:           "resp_synthetic_failed",
		Model:        decision.Model,
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Content:      "failed response",
		FinishReason: "failed",
		CreatedAt:    time.Now().UTC(),
		Usage:        core.Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4},
		RawBody:      []byte(`{"id":"resp_synthetic_failed","status":"failed","error":{"message":"upstream secret failed response"}}`),
	}, nil
}

func (a *syntheticFailedResponsesAdapter) OpenResponsesStream(_ context.Context, decision core.RouteDecision, _ *core.ResponsesRequest) (*providers.StreamSession, error) {
	return testWebStreamSession(decision,
		&core.StreamEvent{Delta: "partial"},
		&core.StreamEvent{
			FinishReason: "failed",
			Done:         true,
			Usage:        &core.Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4},
			RawEvent:     "response.failed",
			RawData:      []byte(`{"type":"response.failed","response":{"id":"resp_synthetic_failed","status":"failed","error":{"message":"upstream secret failed response"}}}`),
		},
	), nil
}

type syntheticFailedChatAdapter struct{}

func (a *syntheticFailedChatAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *syntheticFailedChatAdapter) DisplayName() string { return "Synthetic Failed Chat" }

func (a *syntheticFailedChatAdapter) ListModels(context.Context) []core.ModelSpec {
	return []core.ModelSpec{{Name: "gpt-4.1", Provider: core.ProviderOpenAI}}
}

func (a *syntheticFailedChatAdapter) Invoke(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
	return &core.GatewayResponse{
		ID:           "chat_synthetic_failed",
		Model:        decision.Model,
		Provider:     decision.Provider,
		AccountID:    decision.Account.ID,
		AccountLabel: decision.Account.Label,
		Content:      "upstream chat secret failed response",
		FinishReason: "failed",
		CreatedAt:    time.Now().UTC(),
		Usage:        core.Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4},
		RawBody:      []byte(`{"id":"chat_synthetic_failed","error":{"message":"upstream chat secret failed response"}}`),
	}, nil
}

func (a *syntheticFailedChatAdapter) OpenStream(_ context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*providers.StreamSession, error) {
	return testWebStreamSession(decision,
		&core.StreamEvent{
			FinishReason: "failed",
			Done:         true,
			Usage:        &core.Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4},
			RawEvent:     "response.failed",
			RawData:      []byte(`{"type":"response.failed","response":{"id":"chat_synthetic_failed","status":"failed","error":{"message":"upstream chat secret failed response"}}}`),
		},
	), nil
}

func TestOpenAIResponsesWebSocketUsesGatewayBillingAndBinding(t *testing.T) {
	upstreamHeaders := make(chan http.Header, 1)
	upstreamPayloads := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %s, want /v1/responses", r.URL.Path)
		}
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade upstream websocket: %v", err)
			return
		}
		defer conn.Close()
		upstreamHeaders <- r.Header.Clone()
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read upstream websocket payload: %v", err)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Errorf("decode upstream payload: %v", err)
			return
		}
		upstreamPayloads <- body
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"hello gateway ws"}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_ws_gateway","model":"gpt-4.1","status":"completed","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_ws", Username: "ws", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:           "client_ws",
		Name:         "WS Client",
		APIKey:       "gw_ws",
		OwnerUserID:  "user_ws",
		Enabled:      true,
		RoutePolicy:  core.DefaultRoutePolicy(),
		AccountGroup: core.DefaultAccountGroupName,
	}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "openai_ws",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI WS",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatalf("UpsertAccount returned error: %v", err)
	}

	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	if _, err := control.CreateModel(controlplane.ModelInput{
		ID:                      "gpt-4.1",
		Provider:                core.ProviderOpenAI,
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatalf("CreateModel returned error: %v", err)
	}
	gatewayService := gateway.New(repo, routing.NewRouter(), failover.NewEngine(accounts.NewPool(repo), registry))
	server := httptest.NewServer(NewServerWithOptions(control, gatewayService, ServerOptions{
		StatePath: "data/state.db",
		MasterKey: "cache-ws-secret",
	}).Handler())
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"Authorization": []string{"Bearer gw_ws"},
		"User-Agent":    []string{"codex_cli_rs/0.125.0 test"},
		"originator":    []string{"codex_cli_rs"},
	})
	if err != nil {
		t.Fatalf("dial gateway websocket: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-4.1","stream":false,"input":"hello","prompt_cache_key":"cache-ws"}`)); err != nil {
		t.Fatalf("write gateway websocket: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var sawDelta bool
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read gateway websocket: %v", err)
		}
		var event map[string]any
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatalf("decode gateway websocket event %s: %v", payload, err)
		}
		switch event["type"] {
		case "response.output_text.delta":
			if event["delta"] == "hello gateway ws" {
				sawDelta = true
			}
		case "response.completed":
			if !sawDelta {
				t.Fatal("response completed before expected delta")
			}
			goto done
		case "error":
			t.Fatalf("gateway websocket error event: %s", payload)
		}
	}

done:
	headers := receiveHeader(t, upstreamHeaders)
	if got := headers.Get("Authorization"); got != "Bearer openai-token" {
		t.Fatalf("upstream authorization = %q", got)
	}
	if got := headers.Get("OpenAI-Beta"); got != "responses_websockets=2026-02-06" {
		t.Fatalf("OpenAI-Beta = %q", got)
	}
	upstreamPayload := receivePayload(t, upstreamPayloads)
	if upstreamPayload["type"] != "response.create" || upstreamPayload["stream"] != true {
		t.Fatalf("upstream payload = %#v", upstreamPayload)
	}
	if _, exists := upstreamPayload["generate"]; exists {
		t.Fatalf("generate should be absent for normal requests: %#v", upstreamPayload)
	}

	request := waitForSettledBillingRequest(t, repo, "client_ws")
	if request.PromptTokens != 5 || request.CompletionTokens != 2 || request.ActualNanoUSD <= 0 {
		t.Fatalf("billing request = %#v", request)
	}
	binding := waitForOpenAIResponseBinding(t, repo, "resp_ws_gateway")
	if binding.AccountID != "openai_ws" || binding.ClientID != "client_ws" {
		t.Fatalf("binding = %#v", binding)
	}
}

func TestOpenAIResponsesWebSocketReusesUpstreamConnectionAcrossTurns(t *testing.T) {
	upstreamPayloads := make(chan map[string]any, 2)
	var upstreamConnections atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %s, want /v1/responses", r.URL.Path)
		}
		upstreamConnections.Add(1)
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade upstream websocket: %v", err)
			return
		}
		defer conn.Close()
		for index := 1; index <= 2; index++ {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				t.Errorf("read upstream websocket payload %d: %v", index, err)
				return
			}
			var body map[string]any
			if err := json.Unmarshal(payload, &body); err != nil {
				t.Errorf("decode upstream payload %d: %v", index, err)
				return
			}
			upstreamPayloads <- body
			if index == 1 {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"first"}`))
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_ws_reuse_1","model":"gpt-4.1","status":"completed","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`))
				continue
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"second"}`))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_ws_reuse_2","model":"gpt-4.1","status":"completed","usage":{"input_tokens":6,"output_tokens":3,"total_tokens":9}}}`))
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_ws_reuse", Username: "ws-reuse", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:           "client_ws_reuse",
		Name:         "WS Reuse Client",
		APIKey:       "gw_ws_reuse",
		OwnerUserID:  "user_ws_reuse",
		Enabled:      true,
		RoutePolicy:  core.DefaultRoutePolicy(),
		AccountGroup: core.DefaultAccountGroupName,
	}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "openai_ws_reuse",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI WS Reuse",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatalf("UpsertAccount returned error: %v", err)
	}

	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	if _, err := control.CreateModel(controlplane.ModelInput{
		ID:                      "gpt-4.1",
		Provider:                core.ProviderOpenAI,
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatalf("CreateModel returned error: %v", err)
	}
	gatewayService := gateway.New(repo, routing.NewRouter(), failover.NewEngine(accounts.NewPool(repo), registry))
	server := httptest.NewServer(NewServerWithOptions(control, gatewayService, ServerOptions{
		StatePath: "data/state.db",
		MasterKey: "cache-warmup-secret",
	}).Handler())
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"Authorization": []string{"Bearer gw_ws_reuse"},
		"User-Agent":    []string{"codex_cli_rs/0.125.0 test"},
		"originator":    []string{"codex_cli_rs"},
	})
	if err != nil {
		t.Fatalf("dial gateway websocket: %v", err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-4.1","input":"first","prompt_cache_key":"reuse-cache"}`)); err != nil {
		t.Fatalf("write first response.create: %v", err)
	}
	readWebSocketUntilEvent(t, conn, "response.completed")
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-4.1","input":"second","prompt_cache_key":"reuse-cache"}`)); err != nil {
		t.Fatalf("write second response.create: %v", err)
	}
	readWebSocketUntilEvent(t, conn, "response.completed")

	firstPayload := receivePayload(t, upstreamPayloads)
	secondPayload := receivePayload(t, upstreamPayloads)
	if firstPayload["input"] != "first" || secondPayload["input"] != "second" {
		t.Fatalf("upstream payloads = %#v / %#v", firstPayload, secondPayload)
	}
	if got := upstreamConnections.Load(); got != 1 {
		t.Fatalf("upstream websocket connections = %d, want 1 reused connection", got)
	}
	requests := waitForSettledBillingRequestCount(t, repo, "client_ws_reuse", 2)
	if requests[0].ActualNanoUSD <= 0 || requests[1].ActualNanoUSD <= 0 {
		t.Fatalf("billing requests = %#v", requests)
	}
}

func TestOpenAIResponsesWebSocketCanForceUpstreamSSE(t *testing.T) {
	upstreamHeaders := make(chan http.Header, 1)
	upstreamPayloads := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %s, want /v1/responses", r.URL.Path)
		}
		if websocket.IsWebSocketUpgrade(r) {
			t.Error("upstream received websocket upgrade while upstream websocket is disabled")
			http.Error(w, "websocket disabled", http.StatusBadRequest)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		upstreamHeaders <- r.Header.Clone()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream payload: %v", err)
			return
		}
		upstreamPayloads <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.output_text.delta\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello gateway sse\"}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_sse_gateway\",\"model\":\"gpt-4.1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"total_tokens\":7}}}\n\n"))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_ws_sse", Username: "ws-sse", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:           "client_ws_sse",
		Name:         "WS SSE Client",
		APIKey:       "gw_ws_sse",
		OwnerUserID:  "user_ws_sse",
		Enabled:      true,
		RoutePolicy:  core.DefaultRoutePolicy(),
		AccountGroup: core.DefaultAccountGroupName,
	}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "openai_ws_sse",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI WS SSE",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatalf("UpsertAccount returned error: %v", err)
	}
	upstreamWebSocketEnabled := false
	settings := core.DefaultSystemSettings()
	settings.Runtime.ResponsesWebSocketUpstreamEnabled = &upstreamWebSocketEnabled
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatalf("UpsertSystemSettings returned error: %v", err)
	}

	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	if _, err := control.CreateModel(controlplane.ModelInput{
		ID:                      "gpt-4.1",
		Provider:                core.ProviderOpenAI,
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatalf("CreateModel returned error: %v", err)
	}
	gatewayService := gateway.New(repo, routing.NewRouter(), failover.NewEngine(accounts.NewPool(repo), registry))
	server := httptest.NewServer(NewServerWithOptions(control, gatewayService, ServerOptions{
		StatePath: "data/state.db",
		MasterKey: "cache-warmup-secret",
	}).Handler())
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"Authorization": []string{"Bearer gw_ws_sse"},
		"User-Agent":    []string{"codex_cli_rs/0.125.0 test"},
		"originator":    []string{"codex_cli_rs"},
	})
	if err != nil {
		t.Fatalf("dial gateway websocket: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-4.1","stream":false,"input":"hello","prompt_cache_key":"cache-ws-sse"}`)); err != nil {
		t.Fatalf("write gateway websocket: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var sawDelta bool
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read gateway websocket: %v", err)
		}
		var event map[string]any
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatalf("decode gateway websocket event %s: %v", payload, err)
		}
		switch event["type"] {
		case "response.output_text.delta":
			if event["delta"] == "hello gateway sse" {
				sawDelta = true
			}
		case "response.completed":
			if !sawDelta {
				t.Fatal("response completed before expected SSE delta")
			}
			goto done
		case "error":
			t.Fatalf("gateway websocket error event: %s", payload)
		}
	}

done:
	headers := receiveHeader(t, upstreamHeaders)
	if got := headers.Get("Authorization"); got != "Bearer openai-token" {
		t.Fatalf("upstream authorization = %q", got)
	}
	if got := headers.Get("OpenAI-Beta"); got != "" {
		t.Fatalf("OpenAI-Beta = %q, want empty for SSE upstream", got)
	}
	if got := headers.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %q, want text/event-stream", got)
	}
	upstreamPayload := receivePayload(t, upstreamPayloads)
	if _, exists := upstreamPayload["type"]; exists {
		t.Fatalf("SSE upstream payload should not include websocket event type: %#v", upstreamPayload)
	}
	if upstreamPayload["stream"] != true || upstreamPayload["model"] != "gpt-4.1" {
		t.Fatalf("upstream payload = %#v", upstreamPayload)
	}
	if upstreamPayload["prompt_cache_key"] != "cache-ws-sse" {
		t.Fatalf("prompt_cache_key = %#v", upstreamPayload["prompt_cache_key"])
	}

	request := waitForSettledBillingRequest(t, repo, "client_ws_sse")
	if request.PromptTokens != 5 || request.CompletionTokens != 2 || request.ActualNanoUSD <= 0 {
		t.Fatalf("billing request = %#v", request)
	}
	binding := waitForOpenAIResponseBinding(t, repo, "resp_sse_gateway")
	if binding.AccountID != "openai_ws_sse" || binding.ClientID != "client_ws_sse" {
		t.Fatalf("binding = %#v", binding)
	}
}

func TestOpenAIResponsesWebSocketCodexWarmupSkipsBillingAndKeepsBinding(t *testing.T) {
	upstreamHeaders := make(chan http.Header, 2)
	upstreamPayloads := make(chan map[string]any, 2)
	var upstreamRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %s, want /v1/responses", r.URL.Path)
		}
		requestIndex := upstreamRequests.Add(1)
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade upstream websocket: %v", err)
			return
		}
		defer conn.Close()
		upstreamHeaders <- r.Header.Clone()
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read upstream websocket payload: %v", err)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Errorf("decode upstream payload: %v", err)
			return
		}
		upstreamPayloads <- body
		if requestIndex == 1 {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_ws_warmup","model":"gpt-4.1"}}`))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`))
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"real response"}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_ws_after_warmup","model":"gpt-4.1","status":"completed","usage":{"input_tokens":6,"output_tokens":3,"total_tokens":9}}}`))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_ws_warmup", Username: "ws-warmup", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:           "client_ws_warmup",
		Name:         "WS Warmup Client",
		APIKey:       "gw_ws_warmup",
		OwnerUserID:  "user_ws_warmup",
		Enabled:      true,
		RoutePolicy:  core.DefaultRoutePolicy(),
		AccountGroup: core.DefaultAccountGroupName,
	}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "openai_ws_warmup",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI WS Warmup",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-warmup-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatalf("UpsertAccount returned error: %v", err)
	}

	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	if _, err := control.CreateModel(controlplane.ModelInput{
		ID:                      "gpt-4.1",
		Provider:                core.ProviderOpenAI,
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatalf("CreateModel returned error: %v", err)
	}
	gatewayService := gateway.New(repo, routing.NewRouter(), failover.NewEngine(accounts.NewPool(repo), registry))
	server := httptest.NewServer(NewServerWithOptions(control, gatewayService, ServerOptions{
		StatePath: "data/state.db",
		MasterKey: "cache-warmup-secret",
	}).Handler())
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"Authorization":                         []string{"Bearer gw_ws_warmup"},
		"User-Agent":                            []string{"codex_cli_rs/0.125.0 test"},
		"originator":                            []string{"codex_cli_rs"},
		"Accept-Language":                       []string{"zh-CN"},
		"session-id":                            []string{"sess-warmup"},
		"thread-id":                             []string{"thread-warmup"},
		"x-client-request-id":                   []string{"request-warmup"},
		"x-codex-installation-id":               []string{"install-warmup"},
		"x-codex-turn-state":                    []string{"turn-state-warmup"},
		"x-codex-turn-metadata":                 []string{"turn-meta-warmup"},
		"x-codex-window-id":                     []string{"window-warmup"},
		"x-oai-attestation":                     []string{"attestation-warmup"},
		"x-responsesapi-include-timing-metrics": []string{"true"},
	})
	if err != nil {
		t.Fatalf("dial gateway websocket: %v", err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-4.1","stream":true,"generate":false,"input":"warmup","client_metadata":{"x-codex-turn-metadata":"body-meta"}}`)); err != nil {
		t.Fatalf("write warmup response.create: %v", err)
	}
	readWebSocketUntilEvent(t, conn, "response.done", "response.completed")

	warmupHeaders := receiveHeader(t, upstreamHeaders)
	if got := warmupHeaders.Get("Authorization"); got != "Bearer openai-warmup-token" {
		t.Fatalf("warmup upstream authorization = %q", got)
	}
	if got := warmupHeaders.Get("OpenAI-Beta"); got != "responses_websockets=2026-02-06" {
		t.Fatalf("OpenAI-Beta = %q", got)
	}
	for key, want := range map[string]string{
		"session-id":                            "sess-warmup",
		"thread-id":                             "thread-warmup",
		"x-client-request-id":                   "request-warmup",
		"x-codex-installation-id":               "install-warmup",
		"x-codex-turn-state":                    "turn-state-warmup",
		"x-codex-turn-metadata":                 "turn-meta-warmup",
		"x-codex-window-id":                     "window-warmup",
		"x-oai-attestation":                     "attestation-warmup",
		"x-responsesapi-include-timing-metrics": "true",
	} {
		if got := warmupHeaders.Get(key); got != want {
			t.Fatalf("warmup header %s = %q, want %q", key, got, want)
		}
	}
	warmupPayload := receivePayload(t, upstreamPayloads)
	if warmupPayload["type"] != "response.create" || warmupPayload["stream"] != true || warmupPayload["generate"] != false {
		t.Fatalf("warmup payload = %#v", warmupPayload)
	}
	warmupPromptCacheKey, _ := warmupPayload["prompt_cache_key"].(string)
	if !strings.HasPrefix(warmupPromptCacheKey, "agpc_") {
		t.Fatalf("warmup prompt_cache_key = %#v, want derived agpc_ key", warmupPayload["prompt_cache_key"])
	}
	if strings.Contains(warmupPromptCacheKey, "sess-warmup") {
		t.Fatalf("warmup prompt_cache_key should not expose session id: %q", warmupPromptCacheKey)
	}
	clientMetadata, ok := warmupPayload["client_metadata"].(map[string]any)
	if !ok || clientMetadata["x-codex-turn-metadata"] != "body-meta" {
		t.Fatalf("warmup client_metadata = %#v", warmupPayload["client_metadata"])
	}

	binding := waitForOpenAIResponseBinding(t, repo, "resp_ws_warmup")
	if binding.AccountID != "openai_ws_warmup" || binding.ClientID != "client_ws_warmup" {
		t.Fatalf("warmup binding = %#v", binding)
	}
	if binding.PromptCacheKey != warmupPromptCacheKey {
		t.Fatalf("warmup binding prompt cache key = %q, want %q", binding.PromptCacheKey, warmupPromptCacheKey)
	}
	assertNoBillingRequests(t, repo, "client_ws_warmup")

	if err := repo.UpsertAccount(core.Account{
		ID:       "openai_ws_other",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI WS Other",
		Status:   core.AccountStatusActive,
		Priority: 200,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "other-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatalf("UpsertAccount other returned error: %v", err)
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.processed","response_id":"resp_ws_warmup"}`)); err != nil {
		t.Fatalf("write response.processed: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-4.1","previous_response_id":"resp_ws_warmup","input":"real after warmup"}`)); err != nil {
		t.Fatalf("write real response.create: %v", err)
	}
	readWebSocketUntilEvent(t, conn, "response.completed")

	followupHeaders := receiveHeader(t, upstreamHeaders)
	if got := followupHeaders.Get("Authorization"); got != "Bearer openai-warmup-token" {
		t.Fatalf("followup upstream authorization = %q, want bound warmup account", got)
	}
	followupPayload := receivePayload(t, upstreamPayloads)
	if followupPayload["previous_response_id"] != "resp_ws_warmup" {
		t.Fatalf("followup previous_response_id = %#v payload=%#v", followupPayload["previous_response_id"], followupPayload)
	}
	if followupPayload["prompt_cache_key"] != warmupPromptCacheKey {
		t.Fatalf("followup prompt_cache_key = %#v, want %q", followupPayload["prompt_cache_key"], warmupPromptCacheKey)
	}
	if _, exists := followupPayload["generate"]; exists {
		t.Fatalf("followup generate should be absent: %#v", followupPayload)
	}

	request := waitForSettledBillingRequest(t, repo, "client_ws_warmup")
	if request.PromptTokens != 6 || request.CompletionTokens != 3 || request.ActualNanoUSD <= 0 {
		t.Fatalf("followup billing request = %#v", request)
	}
}

func TestOpenAIResponsesWebSocketSessionUpdateModelFallback(t *testing.T) {
	upstreamPayloads := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade upstream websocket: %v", err)
			return
		}
		defer conn.Close()
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read upstream websocket payload: %v", err)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Errorf("decode upstream payload: %v", err)
			return
		}
		upstreamPayloads <- body
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_ws_session_update","model":"gpt-4.1","status":"completed","usage":{"input_tokens":4,"output_tokens":1,"total_tokens":5}}}`))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_ws_session", Username: "ws-session", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:           "client_ws_session",
		Name:         "WS Session Client",
		APIKey:       "gw_ws_session",
		OwnerUserID:  "user_ws_session",
		Enabled:      true,
		RoutePolicy:  core.DefaultRoutePolicy(),
		AccountGroup: core.DefaultAccountGroupName,
	}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "openai_ws_session",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI WS Session",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatalf("UpsertAccount returned error: %v", err)
	}

	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	if _, err := control.CreateModel(controlplane.ModelInput{
		ID:                      "gpt-4.1",
		Provider:                core.ProviderOpenAI,
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatalf("CreateModel returned error: %v", err)
	}
	gatewayService := gateway.New(repo, routing.NewRouter(), failover.NewEngine(accounts.NewPool(repo), registry))
	server := httptest.NewServer(NewServer(control, gatewayService, "data/state.db").Handler())
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": []string{"Bearer gw_ws_session"}})
	if err != nil {
		t.Fatalf("dial gateway websocket: %v", err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.update","session":{"model":"gpt-4.1"}}`)); err != nil {
		t.Fatalf("write session.update: %v", err)
	}
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read session.updated: %v", err)
	}
	var sessionEvent map[string]any
	if err := json.Unmarshal(payload, &sessionEvent); err != nil {
		t.Fatalf("decode session.updated %s: %v", payload, err)
	}
	if sessionEvent["type"] != "session.updated" {
		t.Fatalf("session update event = %#v", sessionEvent)
	}
	select {
	case upstreamPayload := <-upstreamPayloads:
		t.Fatalf("session.update should not open upstream turn, got %#v", upstreamPayload)
	default:
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","input":"hello without model"}`)); err != nil {
		t.Fatalf("write response.create: %v", err)
	}
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read gateway websocket: %v", err)
		}
		var event map[string]any
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatalf("decode gateway websocket event %s: %v", payload, err)
		}
		switch event["type"] {
		case "response.completed":
			upstreamPayload := receivePayload(t, upstreamPayloads)
			if upstreamPayload["model"] != "gpt-4.1" {
				t.Fatalf("upstream model = %#v payload=%#v", upstreamPayload["model"], upstreamPayload)
			}
			request := waitForSettledBillingRequest(t, repo, "client_ws_session")
			if request.PromptTokens != 4 || request.CompletionTokens != 1 || request.ActualNanoUSD <= 0 {
				t.Fatalf("billing request = %#v", request)
			}
			return
		case "error":
			t.Fatalf("gateway websocket error event: %s", payload)
		}
	}
}

func TestOpenAIResponsesWebSocketCancelReleasesAndClosesUpstream(t *testing.T) {
	upstreamPayloads := make(chan map[string]any, 1)
	upstreamClosed := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade upstream websocket: %v", err)
			return
		}
		defer conn.Close()
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read upstream websocket payload: %v", err)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Errorf("decode upstream payload: %v", err)
			return
		}
		upstreamPayloads <- body
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"partial"}`))
		_, _, _ = conn.ReadMessage()
		upstreamClosed <- struct{}{}
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_ws_cancel", Username: "ws-cancel", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:           "client_ws_cancel",
		Name:         "WS Cancel Client",
		APIKey:       "gw_ws_cancel",
		OwnerUserID:  "user_ws_cancel",
		Enabled:      true,
		RoutePolicy:  core.DefaultRoutePolicy(),
		AccountGroup: core.DefaultAccountGroupName,
	}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "openai_ws_cancel",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI WS Cancel",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "openai-token",
			Metadata:    map[string]string{"base_url": upstream.URL},
		},
	}); err != nil {
		t.Fatalf("UpsertAccount returned error: %v", err)
	}

	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	if _, err := control.CreateModel(controlplane.ModelInput{
		ID:                      "gpt-4.1",
		Provider:                core.ProviderOpenAI,
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatalf("CreateModel returned error: %v", err)
	}
	gatewayService := gateway.New(repo, routing.NewRouter(), failover.NewEngine(accounts.NewPool(repo), registry))
	server := httptest.NewServer(NewServer(control, gatewayService, "data/state.db").Handler())
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": []string{"Bearer gw_ws_cancel"}})
	if err != nil {
		t.Fatalf("dial gateway websocket: %v", err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-4.1","input":"cancel me"}`)); err != nil {
		t.Fatalf("write response.create: %v", err)
	}

	var sawDelta bool
	for !sawDelta {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read gateway websocket: %v", err)
		}
		var event map[string]any
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatalf("decode gateway websocket event %s: %v", payload, err)
		}
		switch event["type"] {
		case "response.output_text.delta":
			sawDelta = true
		case "error":
			t.Fatalf("gateway websocket error event before cancel: %s", payload)
		}
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancel","response_id":"resp_cancel_client"}`)); err != nil {
		t.Fatalf("write response.cancel: %v", err)
	}
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read cancel event: %v", err)
	}
	var cancelEvent map[string]any
	if err := json.Unmarshal(payload, &cancelEvent); err != nil {
		t.Fatalf("decode cancel event %s: %v", payload, err)
	}
	if cancelEvent["type"] != "response.cancelled" {
		t.Fatalf("cancel event = %#v", cancelEvent)
	}
	upstreamPayload := receivePayload(t, upstreamPayloads)
	if upstreamPayload["model"] != "gpt-4.1" {
		t.Fatalf("upstream payload = %#v", upstreamPayload)
	}
	select {
	case <-upstreamClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream websocket close after cancel")
	}
	waitForReleasedBillingRequest(t, repo, "client_ws_cancel")
	if _, err := repo.GetOpenAIResponseBinding("resp_cancel_client"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cancelled response binding err = %v, want ErrNotFound", err)
	}
}

func TestOpenAIResponsesSyntheticTerminalEventsHideFailureDetails(t *testing.T) {
	t.Run("http", func(t *testing.T) {
		server, repo := newSyntheticFailedResponsesServer(t)
		defer server.Close()

		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4.1","input":"hello"}`))
		req.Header.Set("Authorization", "Bearer gw_synthetic")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		server.Config.Handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		var payload struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if payload.Error.Type != gatewayProtocolErrorType || payload.Error.Message != gatewayProtocolErrorMessage {
			t.Fatalf("payload = %#v", payload)
		}
		for _, leaked := range []string{`"status":"failed"`, "failed response", "resp_synthetic_failed"} {
			if strings.Contains(rec.Body.String(), leaked) {
				t.Fatalf("response leaked %q in %s", leaked, rec.Body.String())
			}
		}
		request := waitForSettledBillingRequest(t, repo, "client_synthetic")
		if request.PromptTokens != 3 || request.CompletionTokens != 1 || request.ActualNanoUSD <= 0 {
			t.Fatalf("billing request = %#v", request)
		}
	})

	t.Run("sse", func(t *testing.T) {
		server, repo := newSyntheticFailedResponsesServer(t)
		defer server.Close()

		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4.1","stream":true,"input":"hello"}`))
		req.Header.Set("Authorization", "Bearer gw_synthetic")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		server.Config.Handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, want := range []string{"event: response.failed", gatewayProtocolErrorType, gatewayProtocolErrorMessage} {
			if !strings.Contains(body, want) {
				t.Fatalf("body missing %q: %s", want, body)
			}
		}
		if strings.Contains(body, "event: response.completed") || strings.Contains(body, `"status":"completed"`) {
			t.Fatalf("body should not synthesize completed terminal event: %s", body)
		}
		for _, leaked := range []string{`"status":"failed"`, "failed response", "resp_synthetic_failed"} {
			if strings.Contains(body, leaked) {
				t.Fatalf("stream leaked %q in %s", leaked, body)
			}
		}
		request := waitForSettledBillingRequest(t, repo, "client_synthetic")
		if request.PromptTokens != 3 || request.CompletionTokens != 1 || request.ActualNanoUSD <= 0 {
			t.Fatalf("billing request = %#v", request)
		}
	})

	t.Run("websocket", func(t *testing.T) {
		server, repo := newSyntheticFailedResponsesServer(t)
		defer server.Close()

		wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
			"Authorization": []string{"Bearer gw_synthetic"},
		})
		if err != nil {
			t.Fatalf("dial gateway websocket: %v", err)
		}
		defer conn.Close()
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-4.1","input":"hello"}`)); err != nil {
			t.Fatalf("write gateway websocket: %v", err)
		}

		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("read gateway websocket: %v", err)
			}
			var event map[string]any
			if err := json.Unmarshal(payload, &event); err != nil {
				t.Fatalf("decode gateway websocket event %s: %v", payload, err)
			}
			switch event["type"] {
			case "error":
				errorPayload, ok := event["error"].(map[string]any)
				if !ok || errorPayload["type"] != gatewayProtocolErrorType || errorPayload["message"] != gatewayProtocolErrorMessage {
					t.Fatalf("gateway websocket error event = %#v", event)
				}
				body := string(payload)
				for _, leaked := range []string{`"status":"failed"`, "failed response", "resp_synthetic_failed"} {
					if strings.Contains(body, leaked) {
						t.Fatalf("websocket leaked %q in %s", leaked, body)
					}
				}
				request := waitForSettledBillingRequest(t, repo, "client_synthetic")
				if request.PromptTokens != 3 || request.CompletionTokens != 1 || request.ActualNanoUSD <= 0 {
					t.Fatalf("billing request = %#v", request)
				}
				return
			case "response.failed":
				t.Fatalf("unexpected raw failed event: %s", payload)
			case "response.completed":
				t.Fatalf("unexpected completed event: %s", payload)
			}
		}
	})
}

func TestOpenAIChatSyntheticTerminalEventsHideFailureDetails(t *testing.T) {
	t.Run("http", func(t *testing.T) {
		server, repo := newSyntheticFailedChatServer(t)
		defer server.Close()

		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hello"}]}`))
		req.Header.Set("Authorization", "Bearer gw_synthetic_chat")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		server.Config.Handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		var payload struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if payload.Error.Type != gatewayProtocolErrorType || payload.Error.Message != gatewayProtocolErrorMessage {
			t.Fatalf("payload = %#v", payload)
		}
		for _, leaked := range []string{"chat_synthetic_failed", "upstream chat secret failed response"} {
			if strings.Contains(rec.Body.String(), leaked) {
				t.Fatalf("response leaked %q in %s", leaked, rec.Body.String())
			}
		}
		request := waitForSettledBillingRequest(t, repo, "client_synthetic_chat")
		if request.PromptTokens != 3 || request.CompletionTokens != 1 || request.ActualNanoUSD <= 0 {
			t.Fatalf("billing request = %#v", request)
		}
	})

	t.Run("sse", func(t *testing.T) {
		server, repo := newSyntheticFailedChatServer(t)
		defer server.Close()

		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4.1","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
		req.Header.Set("Authorization", "Bearer gw_synthetic_chat")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		server.Config.Handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, want := range []string{gatewayProtocolErrorType, gatewayProtocolErrorMessage, "data: [DONE]"} {
			if !strings.Contains(body, want) {
				t.Fatalf("body missing %q: %s", want, body)
			}
		}
		for _, leaked := range []string{"response.failed", `"status":"failed"`, "chat_synthetic_failed", "upstream chat secret failed response"} {
			if strings.Contains(body, leaked) {
				t.Fatalf("stream leaked %q in %s", leaked, body)
			}
		}
		request := waitForSettledBillingRequest(t, repo, "client_synthetic_chat")
		if request.PromptTokens != 3 || request.CompletionTokens != 1 || request.ActualNanoUSD <= 0 {
			t.Fatalf("billing request = %#v", request)
		}
	})
}

func newSyntheticFailedResponsesServer(t *testing.T) (*httptest.Server, *storage.MemoryRepository) {
	t.Helper()
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_synthetic", Username: "synthetic", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:           "client_synthetic",
		Name:         "Synthetic Client",
		APIKey:       "gw_synthetic",
		OwnerUserID:  "user_synthetic",
		Enabled:      true,
		RoutePolicy:  core.DefaultRoutePolicy(),
		AccountGroup: core.DefaultAccountGroupName,
	}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "openai_synthetic",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI Synthetic",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "synthetic-token",
		},
	}); err != nil {
		t.Fatalf("UpsertAccount returned error: %v", err)
	}
	registry := providers.NewRegistry(&syntheticFailedResponsesAdapter{})
	control := controlplane.New(repo, registry)
	if _, err := control.CreateModel(controlplane.ModelInput{
		ID:                      "gpt-4.1",
		Provider:                core.ProviderOpenAI,
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatalf("CreateModel returned error: %v", err)
	}
	gatewayService := gateway.New(repo, routing.NewRouter(), failover.NewEngine(accounts.NewPool(repo), registry))
	return httptest.NewServer(NewServer(control, gatewayService, "data/state.db").Handler()), repo
}

func newSyntheticFailedChatServer(t *testing.T) (*httptest.Server, *storage.MemoryRepository) {
	t.Helper()
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertUser(core.User{ID: "user_synthetic_chat", Username: "synthetic-chat", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{
		ID:           "client_synthetic_chat",
		Name:         "Synthetic Chat Client",
		APIKey:       "gw_synthetic_chat",
		OwnerUserID:  "user_synthetic_chat",
		Enabled:      true,
		RoutePolicy:  core.DefaultRoutePolicy(),
		AccountGroup: core.DefaultAccountGroupName,
	}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "openai_synthetic_chat",
		Provider: core.ProviderOpenAI,
		Label:    "OpenAI Synthetic Chat",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "synthetic-chat-token",
		},
	}); err != nil {
		t.Fatalf("UpsertAccount returned error: %v", err)
	}
	registry := providers.NewRegistry(&syntheticFailedChatAdapter{})
	control := controlplane.New(repo, registry)
	if _, err := control.CreateModel(controlplane.ModelInput{
		ID:                      "gpt-4.1",
		Provider:                core.ProviderOpenAI,
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		InputPriceNanoUSDPer1M:  core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatalf("CreateModel returned error: %v", err)
	}
	gatewayService := gateway.New(repo, routing.NewRouter(), failover.NewEngine(accounts.NewPool(repo), registry))
	return httptest.NewServer(NewServer(control, gatewayService, "data/state.db").Handler()), repo
}

func receiveHeader(t *testing.T, ch <-chan http.Header) http.Header {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream headers")
		return nil
	}
}

func receivePayload(t *testing.T, ch <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream payload")
		return nil
	}
}

func readWebSocketUntilEvent(t *testing.T, conn *websocket.Conn, eventTypes ...string) map[string]any {
	t.Helper()
	wanted := map[string]bool{}
	for _, eventType := range eventTypes {
		if eventType = strings.TrimSpace(eventType); eventType != "" {
			wanted[eventType] = true
		}
	}
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read gateway websocket: %v", err)
		}
		var event map[string]any
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatalf("decode gateway websocket event %s: %v", payload, err)
		}
		eventType, _ := event["type"].(string)
		if eventType == "error" {
			t.Fatalf("gateway websocket error event: %s", payload)
		}
		if wanted[eventType] {
			return event
		}
	}
}

func assertNoBillingRequests(t *testing.T, repo *storage.MemoryRepository, clientID string) {
	t.Helper()
	requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: clientID, Limit: 10})
	if total != 0 || len(requests) != 0 {
		t.Fatalf("billing requests total=%d rows=%#v, want none", total, requests)
	}
}

func waitForSettledBillingRequest(t *testing.T, repo *storage.MemoryRepository, clientID string) core.BillingReservation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: clientID, Limit: 10})
		if total == 1 && len(requests) == 1 && requests[0].Status == core.BillingRequestSettled {
			return requests[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("billing requests total=%d rows=%#v, want one settled request", total, requests)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForSettledBillingRequestCount(t *testing.T, repo *storage.MemoryRepository, clientID string, count int) []core.BillingReservation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: clientID, Limit: count})
		if total == count && len(requests) == count {
			allSettled := true
			for _, request := range requests {
				if request.Status != core.BillingRequestSettled {
					allSettled = false
					break
				}
			}
			if allSettled {
				return requests
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d settled billing requests for %s; total=%d rows=%#v", count, clientID, total, requests)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForReleasedBillingRequest(t *testing.T, repo *storage.MemoryRepository, clientID string) core.BillingReservation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		requests, total := repo.ListBillingRequestsPage(storage.BillingRequestQuery{ClientID: clientID, Limit: 10})
		if total == 1 && len(requests) == 1 && requests[0].Status == core.BillingRequestReleased {
			return requests[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("billing requests total=%d rows=%#v, want one released request", total, requests)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForOpenAIResponseBinding(t *testing.T, repo *storage.MemoryRepository, responseID string) core.OpenAIResponseBinding {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		binding, err := repo.GetOpenAIResponseBinding(responseID)
		if err == nil {
			return binding
		}
		if !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("GetOpenAIResponseBinding returned error: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for response binding %q", responseID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
