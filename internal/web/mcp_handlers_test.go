package web

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func TestMCPDocsReadRespectsTokenScopeAndIndexes(t *testing.T) {
	repo, control, admin, handler := newMCPDocsTestServer(t)
	if _, err := control.CreateDocument(admin, controlplane.DocumentInput{Title: "Public Guide", Body: "Visible content", Status: core.DocumentStatusPublished}); err != nil {
		t.Fatalf("CreateDocument public returned error: %v", err)
	}
	if _, err := control.CreateDocument(admin, controlplane.DocumentInput{Title: "Draft Runbook", Body: "Secret draft", Status: core.DocumentStatusDraft}); err != nil {
		t.Fatalf("CreateDocument draft returned error: %v", err)
	}
	if _, err := control.CreateDocument(admin, controlplane.DocumentInput{Title: "Hidden Guide", Body: "Hidden noindex", Status: core.DocumentStatusPublished, NoIndex: true}); err != nil {
		t.Fatalf("CreateDocument noindex returned error: %v", err)
	}
	readToken, _, err := control.CreateMCPToken(admin, controlplane.MCPTokenInput{Name: "Docs Reader", Scopes: []string{core.MCPTokenScopeDocsRead}})
	if err != nil {
		t.Fatalf("CreateMCPToken returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d body=%s", rec.Code, rec.Body.String())
	}

	tools := mcpRequestResult(t, handler, readToken, "tools/list", map[string]any{})
	if !strings.Contains(string(tools), `"docs.read"`) || strings.Contains(string(tools), `"docs.create_draft"`) {
		t.Fatalf("reader tools = %s", string(tools))
	}

	search := mcpToolPayload(t, handler, readToken, "docs.search", map[string]any{"query": "Guide"})
	searchBody := mcpMustJSON(t, search)
	if !strings.Contains(searchBody, "Public Guide") || strings.Contains(searchBody, "Hidden Guide") || strings.Contains(searchBody, "Draft Runbook") {
		t.Fatalf("reader search body = %s", searchBody)
	}

	read := mcpToolPayload(t, handler, readToken, "docs.read", map[string]any{"slug": "public-guide"})
	if got, _ := read["title"].(string); got != "Public Guide" {
		t.Fatalf("read title = %q payload=%#v", got, read)
	}
	response := mcpToolResponse(t, handler, readToken, "docs.read", map[string]any{"slug": "draft-runbook"})
	if response.Error == nil || response.Error.Code != -32004 {
		t.Fatalf("draft read response = %#v", response)
	}
	if events := repo.ListAudit(20); len(events) == 0 {
		t.Fatal("expected MCP calls to be audited")
	}
}

func TestMCPDocsOperatorCanDraftUpdatePublishAndPin(t *testing.T) {
	repo, control, admin, handler := newMCPDocsTestServer(t)
	operatorToken, _, err := control.CreateMCPToken(admin, controlplane.MCPTokenInput{
		Name: "Docs Operator",
		Scopes: []string{
			core.MCPTokenScopeDocsPrivate,
			core.MCPTokenScopeDocsWrite,
			core.MCPTokenScopeDocsPublish,
			core.MCPTokenScopeDocsArchive,
			core.MCPTokenScopeDocsPin,
		},
	})
	if err != nil {
		t.Fatalf("CreateMCPToken returned error: %v", err)
	}

	created := mcpToolPayload(t, handler, operatorToken, "docs.create_draft", map[string]any{
		"title":   "Ops Guide",
		"body":    "Initial body",
		"noindex": true,
	})
	if created["status"] != string(core.DocumentStatusDraft) || created["slug"] != "ops-guide" {
		t.Fatalf("created payload = %#v", created)
	}
	expected, _ := created["expected_updated_at"].(string)
	updated := mcpToolPayload(t, handler, operatorToken, "docs.update", map[string]any{
		"slug":                "ops-guide",
		"body":                "Updated body",
		"expected_updated_at": expected,
	})
	if !strings.Contains(mcpMustJSON(t, updated), "Updated body") {
		t.Fatalf("updated payload = %#v", updated)
	}
	conflict := mcpToolResponse(t, handler, operatorToken, "docs.publish", map[string]any{
		"slug":                "ops-guide",
		"expected_updated_at": "2000-01-01T00:00:00Z",
	})
	if conflict.Error == nil || conflict.Error.Code != -32009 {
		t.Fatalf("stale publish response = %#v", conflict)
	}
	expected, _ = updated["expected_updated_at"].(string)
	published := mcpToolPayload(t, handler, operatorToken, "docs.publish", map[string]any{
		"slug":                "ops-guide",
		"expected_updated_at": expected,
	})
	if published["status"] != string(core.DocumentStatusPublished) {
		t.Fatalf("published payload = %#v", published)
	}
	expected, _ = published["expected_updated_at"].(string)
	pinned := mcpToolPayload(t, handler, operatorToken, "docs.set_pinned", map[string]any{
		"slug":                "ops-guide",
		"pinned":              true,
		"expected_updated_at": expected,
	})
	if pinned["pinned"] != true {
		t.Fatalf("pinned payload = %#v", pinned)
	}
	documents := control.ListDocuments(admin)
	if len(documents) != 1 || documents[0].Status != core.DocumentStatusPublished || !documents[0].Pinned {
		t.Fatalf("documents = %#v", documents)
	}
	if events := repo.ListAudit(50); !mcpAuditContains(events, "docs.publish") || !mcpAuditContains(events, "docs.set_pinned") {
		t.Fatalf("audit events = %#v", events)
	}
}

func TestMCPDocsListUsesPagedDocuments(t *testing.T) {
	base := storage.NewMemoryRepository()
	repo := &mcpDocumentFullListPanicRepository{MemoryRepository: base}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	for _, input := range []controlplane.DocumentInput{
		{Title: "Pinned Public", Body: "Visible", Status: core.DocumentStatusPublished, Pinned: true},
		{Title: "Draft Private", Body: "Draft", Status: core.DocumentStatusDraft},
		{Title: "Hidden Public", Body: "Hidden", Status: core.DocumentStatusPublished, NoIndex: true},
	} {
		if _, err := control.CreateDocument(admin, input); err != nil {
			t.Fatalf("CreateDocument(%s) returned error: %v", input.Title, err)
		}
	}
	privateToken, _, err := control.CreateMCPToken(admin, controlplane.MCPTokenInput{Name: "Private", Scopes: []string{core.MCPTokenScopeDocsPrivate}})
	if err != nil {
		t.Fatalf("CreateMCPToken private returned error: %v", err)
	}
	readToken, _, err := control.CreateMCPToken(admin, controlplane.MCPTokenInput{Name: "Read", Scopes: []string{core.MCPTokenScopeDocsRead}})
	if err != nil {
		t.Fatalf("CreateMCPToken read returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	privateList := mcpToolPayload(t, handler, privateToken, "docs.list", map[string]any{"status": "draft", "limit": 1})
	privateBody := mcpMustJSON(t, privateList)
	if !strings.Contains(privateBody, "Draft Private") || strings.Contains(privateBody, "Pinned Public") {
		t.Fatalf("private docs.list body = %s", privateBody)
	}
	publicList := mcpToolPayload(t, handler, readToken, "docs.list", map[string]any{"limit": 10})
	publicBody := mcpMustJSON(t, publicList)
	if !strings.Contains(publicBody, "Pinned Public") || strings.Contains(publicBody, "Hidden Public") || strings.Contains(publicBody, "Draft Private") {
		t.Fatalf("public docs.list body = %s", publicBody)
	}
	if repo.pageCalls == 0 {
		t.Fatal("docs.list did not use ListDocumentsPage")
	}
}

func TestMCPDocsSearchUsesPagedSearch(t *testing.T) {
	base := storage.NewMemoryRepository()
	repo := &mcpDocumentFullListPanicRepository{MemoryRepository: base}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	for _, input := range []controlplane.DocumentInput{
		{Title: "Pinned Public", Body: "visible release notes", Status: core.DocumentStatusPublished, Pinned: true},
		{Title: "Draft Private", Body: "internal release notes", Status: core.DocumentStatusDraft},
		{Title: "Hidden Public", Body: "hidden release notes", Status: core.DocumentStatusPublished, NoIndex: true},
	} {
		if _, err := control.CreateDocument(admin, input); err != nil {
			t.Fatalf("CreateDocument(%s) returned error: %v", input.Title, err)
		}
	}
	privateToken, _, err := control.CreateMCPToken(admin, controlplane.MCPTokenInput{Name: "Private", Scopes: []string{core.MCPTokenScopeDocsPrivate}})
	if err != nil {
		t.Fatalf("CreateMCPToken private returned error: %v", err)
	}
	readToken, _, err := control.CreateMCPToken(admin, controlplane.MCPTokenInput{Name: "Read", Scopes: []string{core.MCPTokenScopeDocsRead}})
	if err != nil {
		t.Fatalf("CreateMCPToken read returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	privateSearch := mcpToolPayload(t, handler, privateToken, "docs.search", map[string]any{"query": "draft release", "limit": 10})
	privateBody := mcpMustJSON(t, privateSearch)
	if !strings.Contains(privateBody, "Draft Private") || strings.Contains(privateBody, "Pinned Public") {
		t.Fatalf("private docs.search body = %s", privateBody)
	}
	publicSearch := mcpToolPayload(t, handler, readToken, "docs.search", map[string]any{"query": "release", "limit": 10})
	publicBody := mcpMustJSON(t, publicSearch)
	if !strings.Contains(publicBody, "Pinned Public") || strings.Contains(publicBody, "Hidden Public") || strings.Contains(publicBody, "Draft Private") {
		t.Fatalf("public docs.search body = %s", publicBody)
	}
	if repo.searchCalls == 0 {
		t.Fatal("docs.search did not use SearchDocumentsPage")
	}
}

func TestMCPResourcesUsePagedDocuments(t *testing.T) {
	base := storage.NewMemoryRepository()
	repo := &mcpDocumentFullListPanicRepository{MemoryRepository: base}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	for i := 0; i < 21; i++ {
		input := controlplane.DocumentInput{
			Slug:   fmt.Sprintf("public-page-%02d", i),
			Title:  fmt.Sprintf("Public Page %02d", i),
			Body:   "visible",
			Status: core.DocumentStatusPublished,
		}
		if _, err := control.CreateDocument(admin, input); err != nil {
			t.Fatalf("CreateDocument(%s) returned error: %v", input.Slug, err)
		}
	}
	if _, err := control.CreateDocument(admin, controlplane.DocumentInput{Slug: "hidden-public", Title: "Hidden Public", Body: "hidden", Status: core.DocumentStatusPublished, NoIndex: true}); err != nil {
		t.Fatalf("CreateDocument hidden returned error: %v", err)
	}
	if _, err := control.CreateDocument(admin, controlplane.DocumentInput{Slug: "draft-private", Title: "Draft Private", Body: "draft", Status: core.DocumentStatusDraft}); err != nil {
		t.Fatalf("CreateDocument draft returned error: %v", err)
	}
	token, _, err := control.CreateMCPToken(admin, controlplane.MCPTokenInput{Name: "Read", Scopes: []string{core.MCPTokenScopeDocsRead}})
	if err != nil {
		t.Fatalf("CreateMCPToken returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	var list struct {
		Resources []struct {
			URI string `json:"uri"`
		} `json:"resources"`
		NextCursor string `json:"nextCursor"`
	}
	if err := json.Unmarshal(mcpRequestResult(t, handler, token, "resources/list", nil), &list); err != nil {
		t.Fatalf("resources/list unmarshal error: %v", err)
	}
	if len(list.Resources) != 21 || list.Resources[0].URI != mcpDocsResourceURI+"index" || list.NextCursor != "20" {
		t.Fatalf("resources/list page 1 = %#v", list)
	}
	nextCursor := list.NextCursor
	list = struct {
		Resources []struct {
			URI string `json:"uri"`
		} `json:"resources"`
		NextCursor string `json:"nextCursor"`
	}{}
	if err := json.Unmarshal(mcpRequestResult(t, handler, token, "resources/list", map[string]any{"cursor": nextCursor}), &list); err != nil {
		t.Fatalf("resources/list page 2 unmarshal error: %v", err)
	}
	if len(list.Resources) != 1 || list.Resources[0].URI == mcpDocsResourceURI+"index" || list.NextCursor != "" {
		t.Fatalf("resources/list page 2 = %#v", list)
	}

	var read struct {
		Contents []struct {
			Text string `json:"text"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(mcpRequestResult(t, handler, token, "resources/read", map[string]any{"uri": mcpDocsResourceURI + "index"}), &read); err != nil {
		t.Fatalf("resources/read index unmarshal error: %v", err)
	}
	if len(read.Contents) != 1 {
		t.Fatalf("resources/read index contents = %#v", read.Contents)
	}
	var index struct {
		Documents       []map[string]any `json:"documents"`
		Total           int              `json:"total"`
		NextCursor      string           `json:"next_cursor"`
		NextResourceURI string           `json:"next_resource_uri"`
	}
	if err := json.Unmarshal([]byte(read.Contents[0].Text), &index); err != nil {
		t.Fatalf("resources/read index text unmarshal error: %v text=%s", err, read.Contents[0].Text)
	}
	if len(index.Documents) != 20 || index.Total != 21 || index.NextCursor != "20" || index.NextResourceURI == "" {
		t.Fatalf("index page 1 = %#v", index)
	}
	if err := json.Unmarshal(mcpRequestResult(t, handler, token, "resources/read", map[string]any{"uri": index.NextResourceURI}), &read); err != nil {
		t.Fatalf("resources/read next index unmarshal error: %v", err)
	}
	if len(read.Contents) != 1 {
		t.Fatalf("resources/read next index contents = %#v", read.Contents)
	}
	index = struct {
		Documents       []map[string]any `json:"documents"`
		Total           int              `json:"total"`
		NextCursor      string           `json:"next_cursor"`
		NextResourceURI string           `json:"next_resource_uri"`
	}{}
	if err := json.Unmarshal([]byte(read.Contents[0].Text), &index); err != nil {
		t.Fatalf("resources/read next index text unmarshal error: %v text=%s", err, read.Contents[0].Text)
	}
	if len(index.Documents) != 1 || index.Total != 21 || index.NextCursor != "" || index.NextResourceURI != "" {
		t.Fatalf("index page 2 = %#v", index)
	}
	if repo.pageCalls == 0 {
		t.Fatal("resources handlers did not use ListDocumentsPage")
	}
}

func TestMCPBatchRejectsTooManyRequests(t *testing.T) {
	_, control, admin, handler := newMCPDocsTestServer(t)
	token, _, err := control.CreateMCPToken(admin, controlplane.MCPTokenInput{Name: "Batch", Scopes: []string{core.MCPTokenScopeDocsRead}})
	if err != nil {
		t.Fatalf("CreateMCPToken returned error: %v", err)
	}
	requests := make([]map[string]any, 0, maxMCPBatchRequests+1)
	for i := 0; i < maxMCPBatchRequests+1; i++ {
		requests = append(requests, map[string]any{
			"jsonrpc": "2.0",
			"id":      i + 1,
			"method":  "ping",
		})
	}
	body, err := json.Marshal(requests)
	if err != nil {
		t.Fatalf("batch marshal error: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("batch status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "mcp batch request limit exceeded") {
		t.Fatalf("batch error body = %s", rec.Body.String())
	}
}

func TestAdminMCPTokensPageCreatesUpdatesAndDeletesToken(t *testing.T) {
	_, control, admin, handler := newMCPDocsTestServer(t)
	adminHandler := authenticatedAdminHandler(t, control, handler)

	rec := httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/mcp-tokens", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Mcp Token") || strings.Contains(rec.Body.String(), "MCP JSON Template") {
		t.Fatalf("MCP tokens page status=%d body=%s", rec.Code, rec.Body.String())
	}

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("name", "Codex Docs Operator")
	form.Set("expires_days", "30")
	form.Add("scopes", core.MCPTokenScopeDocsRead)
	req := httptest.NewRequest(http.MethodPost, "/admin/mcp-tokens", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "mcp_") {
		t.Fatalf("create MCP token status=%d body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "mcpServers") || !strings.Contains(body, "ag-toos") || !strings.Contains(body, "/mcp") || !strings.Contains(body, "Authorization") {
		t.Fatalf("create MCP token page missing client config JSON: %s", body)
	}
	tokens := control.ListMCPTokens(admin)
	if len(tokens) != 1 || tokens[0].Name != "Codex Docs Operator" || !tokens[0].Enabled {
		t.Fatalf("tokens = %#v", tokens)
	}

	updateForm := url.Values{}
	updateForm.Set("csrf_token", testConsoleCSRFToken)
	updateForm.Set("name", "Codex Docs Maintainer")
	updateForm.Add("scopes", core.MCPTokenScopeDocsPrivate)
	req = httptest.NewRequest(http.MethodPost, "/admin/mcp-tokens/"+tokens[0].ID+"/update", strings.NewReader(updateForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("update MCP token status=%d body=%s", rec.Code, rec.Body.String())
	}
	tokens = control.ListMCPTokens(admin)
	if len(tokens) != 1 || tokens[0].Name != "Codex Docs Maintainer" || !tokens[0].HasScope(core.MCPTokenScopeDocsPrivate) {
		t.Fatalf("tokens after update = %#v", tokens)
	}

	deleteForm := url.Values{}
	deleteForm.Set("csrf_token", testConsoleCSRFToken)
	req = httptest.NewRequest(http.MethodPost, "/admin/mcp-tokens/"+tokens[0].ID+"/delete", strings.NewReader(deleteForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete MCP token status=%d body=%s", rec.Code, rec.Body.String())
	}
	tokens = control.ListMCPTokens(admin)
	if len(tokens) != 0 {
		t.Fatalf("tokens after delete = %#v", tokens)
	}
}

func newMCPDocsTestServer(t *testing.T) (*storage.MemoryRepository, *controlplane.Service, core.User, http.Handler) {
	t.Helper()
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	settings, err := control.GetSystemSettings()
	if err != nil {
		t.Fatalf("GetSystemSettings returned error: %v", err)
	}
	settings.Runtime.PublicBaseURL = "https://docs.example.com"
	if _, err := control.UpdateSystemSettings(settings); err != nil {
		t.Fatalf("UpdateSystemSettings returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	return repo, control, admin, server.Handler()
}

func mcpToolPayload(t *testing.T, handler http.Handler, token, name string, args map[string]any) map[string]any {
	t.Helper()
	response := mcpToolResponse(t, handler, token, name, args)
	if response.Error != nil {
		t.Fatalf("%s returned MCP error: %#v", name, response.Error)
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatalf("tool result unmarshal error: %v body=%s", err, string(response.Result))
	}
	if len(result.Content) != 1 || result.Content[0].Type != "text" {
		t.Fatalf("tool result content = %#v", result.Content)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
		t.Fatalf("tool payload unmarshal error: %v text=%s", err, result.Content[0].Text)
	}
	return payload
}

func mcpToolResponse(t *testing.T, handler http.Handler, token, name string, args map[string]any) mcpTestResponse {
	t.Helper()
	return mcpRequest(t, handler, token, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
}

func mcpRequestResult(t *testing.T, handler http.Handler, token, method string, params map[string]any) json.RawMessage {
	t.Helper()
	response := mcpRequest(t, handler, token, method, params)
	if response.Error != nil {
		t.Fatalf("%s returned MCP error: %#v", method, response.Error)
	}
	return response.Result
}

func mcpRequest(t *testing.T, handler http.Handler, token, method string, params map[string]any) mcpTestResponse {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		t.Fatalf("request marshal error: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s status = %d body=%s", method, rec.Code, rec.Body.String())
	}
	var response mcpTestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("response unmarshal error: %v body=%s", err, rec.Body.String())
	}
	return response
}

type mcpTestResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *mcpRPCError    `json:"error"`
}

func mcpMustJSON(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json marshal error: %v", err)
	}
	return string(body)
}

func mcpAuditContains(events []core.AuditEvent, action string) bool {
	for _, event := range events {
		if event.Action == action {
			return true
		}
	}
	return false
}

type mcpDocumentFullListPanicRepository struct {
	*storage.MemoryRepository
	pageCalls   int
	searchCalls int
}

func (r *mcpDocumentFullListPanicRepository) ListDocuments() []core.Document {
	panic("MCP docs handlers should use paged document APIs instead of full ListDocuments")
}

func (r *mcpDocumentFullListPanicRepository) ListDocumentsPage(status core.DocumentStatus, seoOnly bool, offset, limit int) ([]core.Document, int) {
	r.pageCalls++
	return r.MemoryRepository.ListDocumentsPage(status, seoOnly, offset, limit)
}

func (r *mcpDocumentFullListPanicRepository) SearchDocumentsPage(query string, status core.DocumentStatus, seoOnly bool, offset, limit int) ([]core.Document, int) {
	r.searchCalls++
	return r.MemoryRepository.SearchDocumentsPage(query, status, seoOnly, offset, limit)
}
