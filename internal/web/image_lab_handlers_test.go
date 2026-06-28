package web

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
)

func TestImageLabBootstrapScopesClientsToCurrentUser(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	alice, bob := mustCreateImageLabUsers(t, control)
	if err := repo.UpsertClient(core.APIClient{ID: "client_alice", Name: "Alice Key", APIKey: "gw_alice", OwnerUserID: alice.ID, Enabled: true, AccountGroup: core.DefaultAccountGroupName}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_bob", Name: "Bob Key", APIKey: "gw_bob", OwnerUserID: bob.ID, Enabled: true, AccountGroup: core.DefaultAccountGroupName}); err != nil {
		t.Fatal(err)
	}
	if _, err := control.CreateModel(controlplane.ModelInput{ID: "gpt-5.4", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{core.DefaultAccountGroupName}}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, alice, server.Handler())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/images/api/bootstrap", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		Clients []struct {
			ID string `json:"id"`
		} `json:"clients"`
		DefaultClientID string                         `json:"default_client_id"`
		ModelsByClient  map[string][]imageLabModelView `json:"models_by_client"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal bootstrap: %v", err)
	}
	if len(payload.Clients) != 1 || payload.Clients[0].ID != "client_alice" || payload.DefaultClientID != "client_alice" {
		t.Fatalf("clients payload = %#v", payload)
	}
	if _, ok := payload.ModelsByClient["client_bob"]; ok {
		t.Fatalf("bootstrap leaked bob client models: %#v", payload.ModelsByClient)
	}
}

func TestImageLabAttemptStatusMessageHandlesTemporaryUnavailable(t *testing.T) {
	status, message, ok := imageLabAttemptStatusMessage(core.AttemptRecord{
		ErrorCode:    "upstream_temporarily_unavailable",
		ErrorMessage: "upstream_temporarily_unavailable: no available channel",
	})
	if !ok {
		t.Fatal("expected handled message")
	}
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", status, http.StatusBadGateway)
	}
	if message != gatewayProtocolErrorMessage {
		t.Fatalf("message = %q", message)
	}
	if strings.Contains(message, "upstream_temporarily_unavailable") || strings.Contains(message, "no available channel") || strings.Contains(message, "上游") {
		t.Fatalf("message leaked upstream detail: %q", message)
	}
}

func TestImageLabExecutionErrorStatusMessageHidesUpstreamDetails(t *testing.T) {
	err := &failover.ExecutionError{Attempts: []core.AttemptRecord{{
		AccountID:    "acct_secret",
		AccountLabel: "Secret Account",
		ErrorCode:    "unexpected_provider_error",
		ErrorMessage: "provider exploded with credential detail",
	}}}
	status, message := imageLabExecutionErrorStatusMessage(err)
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", status, http.StatusBadGateway)
	}
	if message != gatewayProtocolErrorMessage {
		t.Fatalf("message = %q, want %q", message, gatewayProtocolErrorMessage)
	}
	for _, leaked := range []string{"acct_secret", "Secret Account", "unexpected_provider_error", "credential detail"} {
		if strings.Contains(message, leaked) {
			t.Fatalf("message leaked %q: %q", leaked, message)
		}
	}
}

func TestImageLabAttemptMessagePriorityHandlesTemporaryUnavailable(t *testing.T) {
	priority := imageLabAttemptMessagePriority(core.AttemptRecord{ErrorCode: "upstream_temporarily_unavailable"})
	if priority != 60 {
		t.Fatalf("priority = %d, want 60", priority)
	}
}

func TestImageLabClientLookupByIDAvoidsFullOwnerClientList(t *testing.T) {
	base := storage.NewMemoryRepository()
	repo := &imageLabFullOwnerListPanicRepository{MemoryRepository: base}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	alice, bob := mustCreateImageLabUsers(t, control)
	if err := base.UpsertClient(core.APIClient{ID: "client_alice", Name: "Alice Key", APIKey: "gw_alice", OwnerUserID: alice.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := base.UpsertClient(core.APIClient{ID: "client_bob", Name: "Bob Key", APIKey: "gw_bob", OwnerUserID: bob.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(control, nil, "data/state.db")

	client, lookupErr := server.imageLabClientForUser(alice, "client_alice")
	if lookupErr != nil {
		t.Fatalf("imageLabClientForUser returned error: %#v", lookupErr)
	}
	if client.ID != "client_alice" || client.APIKey != "gw_alice" {
		t.Fatalf("client = %#v", client)
	}
	if _, lookupErr := server.imageLabClientForUser(alice, "client_bob"); lookupErr == nil || lookupErr.status != http.StatusForbidden {
		t.Fatalf("foreign client lookup error = %#v, want forbidden", lookupErr)
	}
}

func TestImageLabBootstrapUsesImageModels(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := control.CreateUser(controlplane.UserInput{Username: "alice", Password: "alice-secret", Role: core.UserRoleUser, Enabled: true})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_text", Name: "Text Key", APIKey: "gw_text", OwnerUserID: user.ID, Enabled: true, AccountGroup: "text"}); err != nil {
		t.Fatal(err)
	}
	if _, err := control.CreateAccountGroup("text"); err != nil {
		t.Fatal(err)
	}
	if _, err := control.CreateModel(controlplane.ModelInput{ID: "gpt-5.4", Provider: core.ProviderOpenAI, Type: core.ModelTypeText, Enabled: true, VisibleGroups: []string{"text"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := control.CreateModel(controlplane.ModelInput{ID: "gpt-image-2", Provider: core.ProviderOpenAI, Type: core.ModelTypeImage, Enabled: true, VisibleGroups: []string{"text"}}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/images/api/bootstrap", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		DefaultClientID string                         `json:"default_client_id"`
		DefaultModel    string                         `json:"default_model"`
		ModelsByClient  map[string][]imageLabModelView `json:"models_by_client"`
		ActiveTasks     json.RawMessage                `json:"active_tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal bootstrap: %v", err)
	}
	if payload.DefaultClientID != "client_text" || payload.DefaultModel != "gpt-image-2" {
		t.Fatalf("defaults = client %q model %q, want client_text/gpt-image-2", payload.DefaultClientID, payload.DefaultModel)
	}
	models := payload.ModelsByClient["client_text"]
	if len(models) != 1 || models[0].ID != "gpt-image-2" {
		t.Fatalf("models_by_client = %#v, want only image model", payload.ModelsByClient)
	}
	if strings.TrimSpace(string(payload.ActiveTasks)) != "[]" {
		t.Fatalf("bootstrap active_tasks = %s, want empty list", payload.ActiveTasks)
	}
}

func TestImageLabBootstrapPrefersGPTImage2DefaultModel(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := control.CreateUser(controlplane.UserInput{Username: "alice", Password: "alice-secret", Role: core.UserRoleUser, Enabled: true})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_image", Name: "Image Key", APIKey: "gw_image", OwnerUserID: user.ID, Enabled: true, AccountGroup: core.DefaultAccountGroupName}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"codex-gpt-image-2", "dall-e-3", "gpt-image-2"} {
		if _, err := control.CreateModel(controlplane.ModelInput{ID: id, Provider: core.ProviderOpenAI, Type: core.ModelTypeImage, Enabled: true, VisibleGroups: []string{core.DefaultAccountGroupName}}); err != nil {
			t.Fatalf("CreateModel(%s) returned error: %v", id, err)
		}
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/images/api/bootstrap", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		DefaultModel string `json:"default_model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal bootstrap: %v", err)
	}
	if payload.DefaultModel != "gpt-image-2" {
		t.Fatalf("default_model = %q, want gpt-image-2", payload.DefaultModel)
	}
}

func TestImageLabPageIsBackgroundImageTaskUI(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "image-user",
		Password: "image-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/images", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"OpenAI 兼容图片接口", "刷新页面后可继续查看当前进度", "参考图（可选）", "张数", "正在加载后台任务"} {
		if !strings.Contains(body, want) {
			t.Fatalf("image lab page missing %q: %s", want, body)
		}
	}
	for _, forbidden := range []string{"Responses API", "image_generation", "本地历史只保存在当前浏览器中", "服务器仅保留近期后台任务状态"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("image lab page still contains %q: %s", forbidden, body)
		}
	}
	for _, forbidden := range []string{`data-image-lab-concurrency`, `name="concurrency"`, `name="timeout_sec"`, `data-image-lab-mode=`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("image lab page still contains %q: %s", forbidden, body)
		}
	}
}

func TestImageLabUserConsoleToggleHidesUsersButAllowsAdmins(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "image-user",
		Password: "image-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	disabled := false
	settings := core.DefaultSystemSettings()
	settings.Image.UserConsoleEnabled = &disabled
	if _, err := control.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")

	userHandler := authenticatedUserHandler(t, control, user, server.Handler())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	userHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("user dashboard status = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `href="/images"`) {
		t.Fatalf("user dashboard should hide image lab link while disabled: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/images", nil)
	userHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("disabled user image page status = %d, want %d body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/images/api/bootstrap", nil)
	userHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"type":"forbidden"`) {
		t.Fatalf("disabled user image api status=%d body=%s", rec.Code, rec.Body.String())
	}

	adminHandler := authenticatedAdminHandler(t, control, server.Handler())
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/settings", nil)
	adminHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin settings status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `href="/images"`) {
		t.Fatalf("admin navigation should keep image lab link while disabled: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/images", nil)
	adminHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin image page status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestImageLabGenerateCreatesBackgroundTaskUsingImageGenerationsEndpoint(t *testing.T) {
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			t.Fatalf("path = %s, want /v1/images/generations", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"aW1hZ2U=","revised_prompt":"a quiet control room"}]}`))
	}))
	defer upstream.Close()

	control, gatewayService, user := newImageLabGatewayFixture(t, upstream.URL)
	statePath := filepath.Join(t.TempDir(), "state.db")
	server := NewServer(control, gatewayService, statePath)
	handler := authenticatedUserHandler(t, control, user, server.Handler())

	body := `{"client_id":"client_image","prompt":"a quiet control room","ratio":"1:1","resolution":"standard","model":"gpt-image-2","count":1}`
	req := httptest.NewRequest(http.MethodPost, "/images/api/generate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var created imageLabTaskSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created task: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("created task missing id: %#v", created)
	}
	if created.ID == "" || created.Status != "running" || created.Count != 1 {
		t.Fatalf("created task = %#v", created)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/images/api/jobs", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("jobs list status = %d body=%s", rec.Code, rec.Body.String())
	}
	var listed struct {
		ActiveTasks []imageLabTaskSnapshot `json:"active_tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode jobs list: %v", err)
	}
	if len(listed.ActiveTasks) != 1 || listed.ActiveTasks[0].ID != created.ID {
		t.Fatalf("jobs list = %#v, want created task %q", listed.ActiveTasks, created.ID)
	}

	var snapshot imageLabTaskSnapshot
	for attempt := 0; attempt < 50; attempt++ {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/images/api/jobs/"+created.ID, nil)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("job status = %d body=%s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
			t.Fatalf("decode task: %v", err)
		}
		if snapshot.Status != "running" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if snapshot.Status != "completed" {
		t.Fatalf("snapshot status = %q body=%#v", snapshot.Status, snapshot)
	}
	if len(snapshot.Results) != 1 || snapshot.Results[0] == nil || !snapshot.Results[0].OK || snapshot.Results[0].Image != "data:image/png;base64,aW1hZ2U=" || snapshot.Results[0].MIME != "image/png" {
		t.Fatalf("snapshot results = %#v", snapshot.Results)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(statePath), "image-lab")); !os.IsNotExist(err) {
		t.Fatalf("image-lab storage dir exists or stat err=%v", err)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/images/api/jobs/"+created.ID, nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete task status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/images/api/jobs", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("jobs after delete status=%d body=%s", rec.Code, rec.Body.String())
	}
	var dismissedList struct {
		ActiveTasks []imageLabTaskSnapshot `json:"active_tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dismissedList); err != nil {
		t.Fatalf("decode dismissed list: %v", err)
	}
	if len(dismissedList.ActiveTasks) != 0 {
		t.Fatalf("dismissed active tasks = %#v, want none", dismissedList.ActiveTasks)
	}
	restarted := NewServer(control, nil, statePath)
	restartedHandler := authenticatedUserHandler(t, control, user, restarted.Handler())
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/images/api/jobs", nil)
	restartedHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("restarted jobs status = %d body=%s", rec.Code, rec.Body.String())
	}
	var restored struct {
		ActiveTasks []imageLabTaskSnapshot `json:"active_tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &restored); err != nil {
		t.Fatalf("decode restored jobs: %v", err)
	}
	if len(restored.ActiveTasks) != 0 {
		t.Fatalf("restored dismissed tasks = %#v, want none", restored.ActiveTasks)
	}
	if upstreamBody["model"] != "gpt-image-2" || upstreamBody["prompt"] != "a quiet control room" || upstreamBody["response_format"] != "b64_json" || upstreamBody["size"] != "1024x1024" {
		t.Fatalf("upstream body = %#v", upstreamBody)
	}
	if upstreamBody["n"] != float64(1) {
		t.Fatalf("upstream n = %#v, want 1", upstreamBody["n"])
	}
}

func TestImageLabGenerateRunsMultipleImagesAcrossAvailableAccounts(t *testing.T) {
	var mu sync.Mutex
	active := 0
	maxActive := 0
	tokens := make(map[string]int)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			t.Fatalf("path = %s, want /v1/images/generations", r.URL.Path)
		}
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		tokens[r.Header.Get("Authorization")]++
		mu.Unlock()
		time.Sleep(150 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"aW1hZ2U="}]}`))
	}))
	defer upstream.Close()

	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	user, err := control.CreateUser(controlplane.UserInput{Username: "image-user", Password: "image-secret", Role: core.UserRoleUser, Enabled: true})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_image", Name: "Image Key", APIKey: "gw_image", OwnerUserID: user.ID, Enabled: true, AccountGroup: core.DefaultAccountGroupName}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if _, err := control.CreateModel(controlplane.ModelInput{ID: "gpt-image-2", Provider: core.ProviderOpenAI, Type: core.ModelTypeImage, Enabled: true, VisibleGroups: []string{core.DefaultAccountGroupName}}); err != nil {
		t.Fatalf("CreateModel returned error: %v", err)
	}
	for _, account := range []core.Account{
		imageLabAccountWithQuota(t, core.Account{
			ID:       "acct_high",
			Provider: core.ProviderOpenAI,
			Label:    "High",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
			Credential: core.Credential{
				AccessToken: "token-high",
				Metadata:    map[string]string{"base_url": upstream.URL},
			},
		}, 9),
		imageLabAccountWithQuota(t, core.Account{
			ID:       "acct_low",
			Provider: core.ProviderOpenAI,
			Label:    "Low",
			Status:   core.AccountStatusActive,
			Priority: 100,
			Weight:   100,
			Credential: core.Credential{
				AccessToken: "token-low",
				Metadata:    map[string]string{"base_url": upstream.URL},
			},
		}, 4),
	} {
		if err := repo.UpsertAccount(account); err != nil {
			t.Fatalf("UpsertAccount returned error: %v", err)
		}
	}
	gatewayService := gateway.New(repo, routing.NewRouter(), failover.NewEngine(accounts.NewPool(repo), registry))
	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())

	body := `{"client_id":"client_image","prompt":"a quiet control room","ratio":"1:1","resolution":"standard","model":"gpt-image-2","count":2}`
	req := httptest.NewRequest(http.MethodPost, "/images/api/generate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var created imageLabTaskSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created task: %v", err)
	}

	var snapshot imageLabTaskSnapshot
	for attempt := 0; attempt < 100; attempt++ {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/images/api/jobs/"+created.ID, nil)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("job status = %d body=%s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
			t.Fatalf("decode task: %v", err)
		}
		if snapshot.Status != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if snapshot.Status != "completed" {
		t.Fatalf("snapshot status = %q body=%#v", snapshot.Status, snapshot)
	}
	if len(snapshot.Results) != 2 || snapshot.Results[0] == nil || snapshot.Results[1] == nil || !snapshot.Results[0].OK || !snapshot.Results[1].OK {
		t.Fatalf("snapshot results = %#v", snapshot.Results)
	}
	mu.Lock()
	defer mu.Unlock()
	if maxActive < 2 {
		t.Fatalf("max upstream concurrency = %d, want requests to run across available accounts", maxActive)
	}
	if tokens["Bearer token-high"] != 1 || tokens["Bearer token-low"] != 1 {
		t.Fatalf("authorization tokens = %#v, want one request per account", tokens)
	}
}

func TestImageLabGenerateBackgroundPassesReferenceImagesToImageEditsEndpoint(t *testing.T) {
	type upstreamCapture struct {
		fields         map[string][]string
		imageFileCount int
		fileCount      int
	}
	captures := make(chan upstreamCapture, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/edits" {
			t.Fatalf("path = %s, want /v1/images/edits", r.URL.Path)
		}
		if err := r.ParseMultipartForm(20 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		capture := upstreamCapture{
			fields:         cloneStringValues(r.MultipartForm.Value),
			imageFileCount: len(r.MultipartForm.File["image"]),
		}
		for _, files := range r.MultipartForm.File {
			capture.fileCount += len(files)
		}
		select {
		case captures <- capture:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"aW1hZ2U="}]}`))
	}))
	defer upstream.Close()

	control, gatewayService, user := newImageLabGatewayFixture(t, upstream.URL)
	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())

	body := `{"client_id":"client_image","prompt":"redraw this","ratio":"auto","resolution":"auto","model":"gpt-image-2","count":1,"input_images":[{"name":"input.jpg","type":"image/jpg","data_url":"data:image/jpg;base64,aW1hZ2U=","size":5}]}`
	req := httptest.NewRequest(http.MethodPost, "/images/api/generate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var created imageLabTaskSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created task: %v", err)
	}
	var capture upstreamCapture
	select {
	case capture = <-captures:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("background task did not reach upstream")
	}
	if capture.fields["model"][0] != "gpt-image-2" || capture.fields["prompt"][0] != "redraw this" || capture.fields["response_format"][0] != "b64_json" || capture.fields["n"][0] != "1" {
		t.Fatalf("upstream fields = %#v", capture.fields)
	}
	if _, ok := capture.fields["size"]; ok {
		t.Fatalf("auto size should omit size field: %#v", capture.fields)
	}
	if capture.fileCount != 1 {
		t.Fatalf("upstream file count = %d, want 1", capture.fileCount)
	}
	if capture.imageFileCount != 1 {
		t.Fatalf("upstream image file count = %d, want 1", capture.imageFileCount)
	}
}

func cloneStringValues(values map[string][]string) map[string][]string {
	clone := make(map[string][]string, len(values))
	for key, item := range values {
		clone[key] = append([]string(nil), item...)
	}
	return clone
}

func TestImageLabGenerateRejectsServerTaskOnlyFields(t *testing.T) {
	control, gatewayService, user := newImageLabGatewayFixture(t, "https://upstream.example")
	server := NewServer(control, gatewayService, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())

	body := `{"client_id":"client_image","prompt":"test","ratio":"1:1","resolution":"standard","model":"gpt-image-2","concurrency":2}`
	req := httptest.NewRequest(http.MethodPost, "/images/api/generate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "请求格式无效") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestImageLabProxyRejectsPrivateHost(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := control.CreateUser(controlplane.UserInput{Username: "image-user", Password: "image-secret", Role: core.UserRoleUser, Enabled: true})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/images/api/proxy?url=http%3A%2F%2F127.0.0.1%2Fout.png", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestImageLabProxyRejectsSVG(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := control.CreateUser(controlplane.UserInput{Username: "image-user", Password: "image-secret", Role: core.UserRoleUser, Enabled: true})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	previousClient := imageLabProxyHTTPClient
	imageLabProxyHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Type", "image/svg+xml")
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)),
			Request:    req,
		}, nil
	})}
	defer func() {
		imageLabProxyHTTPClient = previousClient
	}()

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/images/api/proxy?url=https%3A%2F%2Fcdn.example.com%2Fout.svg", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
}

func TestImageLabProxyRejectsPrivateRedirect(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := control.CreateUser(controlplane.UserInput{Username: "image-user", Password: "image-secret", Role: core.UserRoleUser, Enabled: true})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	requests := 0
	previousClient := imageLabProxyHTTPClient
	imageLabProxyHTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests++
			if requests > 1 {
				t.Fatalf("proxy followed private redirect to %s", req.URL.String())
			}
			header := make(http.Header)
			header.Set("Location", "http://127.0.0.1/out.png")
			return &http.Response{
				StatusCode: http.StatusFound,
				Status:     "302 Found",
				Header:     header,
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		}),
		CheckRedirect: newImageLabProxyHTTPClient().CheckRedirect,
	}
	defer func() {
		imageLabProxyHTTPClient = previousClient
	}()

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/images/api/proxy?url=https%3A%2F%2Fcdn.example.com%2Fout.png", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestImageLabProxyBlockedIPRanges(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "100.64.0.1", "::1", "fc00::1", "fe80::1"}
	for _, value := range blocked {
		if !imageLabProxyBlockedIP(net.ParseIP(value)) {
			t.Fatalf("imageLabProxyBlockedIP(%q) = false, want true", value)
		}
	}
	if imageLabProxyBlockedIP(net.ParseIP("8.8.8.8")) {
		t.Fatalf("imageLabProxyBlockedIP(8.8.8.8) = true, want false")
	}
}

func imageLabAccountWithQuota(t *testing.T, account core.Account, remaining int64) core.Account {
	t.Helper()
	raw, err := json.Marshal(core.AccountQuotaSnapshot{
		Image: &core.AccountImageQuota{Remaining: remaining},
	})
	if err != nil {
		t.Fatal(err)
	}
	if account.Credential.Metadata == nil {
		account.Credential.Metadata = map[string]string{}
	}
	account.Credential.Metadata[core.AccountQuotaMetadataKey] = string(raw)
	return account
}

func newImageLabGatewayFixture(t *testing.T, upstreamURL string) (*controlplane.Service, *gateway.Service, core.User) {
	t.Helper()
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&providers.OpenAIAdapter{})
	control := controlplane.New(repo, registry)
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "image-user",
		Password: "image-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_image", Name: "Image Key", APIKey: "gw_image", OwnerUserID: user.ID, Enabled: true, AccountGroup: core.DefaultAccountGroupName}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if _, err := control.CreateModel(controlplane.ModelInput{ID: "gpt-image-2", Provider: core.ProviderOpenAI, Type: core.ModelTypeImage, Enabled: true, VisibleGroups: []string{core.DefaultAccountGroupName}}); err != nil {
		t.Fatalf("CreateModel returned error: %v", err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_image",
		Provider: core.ProviderOpenAI,
		Label:    "Image Account",
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			AccessToken: "upstream-token",
			Metadata:    map[string]string{"base_url": upstreamURL},
		},
	}); err != nil {
		t.Fatalf("UpsertAccount returned error: %v", err)
	}
	gatewayService := gateway.New(repo, routing.NewRouter(), failover.NewEngine(accounts.NewPool(repo), registry))
	return control, gatewayService, user
}

func mustCreateImageLabUsers(t *testing.T, control *controlplane.Service) (core.User, core.User) {
	t.Helper()
	alice, err := control.CreateUser(controlplane.UserInput{Username: "alice", Password: "alice-secret", Role: core.UserRoleUser, Enabled: true})
	if err != nil {
		t.Fatalf("CreateUser(alice) returned error: %v", err)
	}
	bob, err := control.CreateUser(controlplane.UserInput{Username: "bob", Password: "bob-secret", Role: core.UserRoleUser, Enabled: true})
	if err != nil {
		t.Fatalf("CreateUser(bob) returned error: %v", err)
	}
	return alice, bob
}

type imageLabFullOwnerListPanicRepository struct {
	*storage.MemoryRepository
}

func (r *imageLabFullOwnerListPanicRepository) ListClientsByOwner(ownerUserID string) []core.APIClient {
	panic("image lab client lookup by id should use GetClient instead of full ListClientsByOwner")
}
