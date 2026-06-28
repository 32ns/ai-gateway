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

func TestDocumentPublicSEOEndpoints(t *testing.T) {
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
	adminHandler := authenticatedAdminHandler(t, control, server.Handler())

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("title", "Getting Started")
	form.Set("body", "# Install\n\nUse `gw` safely.")
	form.Set("status", "published")
	form.Set("pinned", "on")

	rec := httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/docs/new", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("new document page status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`action="/admin/docs/new"`, `name="body"`, `name="pinned"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("new document page missing %q: %s", want, body)
		}
	}
	for _, hiddenField := range []string{`name="slug"`, `name="meta_title"`, `name="meta_description"`, `name="canonical_url"`} {
		if strings.Contains(body, hiddenField) {
			t.Fatalf("new document page should auto-generate %q: %s", hiddenField, body)
		}
	}
	if strings.Contains(body, `id="document-create"`) || strings.Contains(body, `data-group-settings-open="document-edit`) {
		t.Fatalf("new document page should not render document modals: %s", body)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/docs", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("removed create route status = %d, want %d body=%s", rec.Code, http.StatusMethodNotAllowed, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/docs/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create document status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if events := repo.ListAudit(10); len(events) == 0 || events[0].Action != "document.create" || events[0].Actor != admin.Username {
		t.Fatalf("audit events = %#v", events)
	}
	documents := control.ListDocuments(admin)
	if len(documents) != 1 {
		t.Fatalf("documents = %#v, want one created document", documents)
	}
	documentID := documents[0].ID
	if documents[0].Slug != "getting-started" {
		t.Fatalf("auto-generated slug = %q, want getting-started", documents[0].Slug)
	}
	if !documents[0].Pinned {
		t.Fatalf("created document should be pinned: %#v", documents[0])
	}

	handler := server.Handler()
	rec = httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/docs", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Getting Started") || !strings.Contains(rec.Body.String(), "Pinned") {
		t.Fatalf("admin docs status=%d body=%s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	for _, want := range []string{`href="/admin/docs/new"`, `href="/admin/docs/` + documentID + `/edit"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin docs list missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, `id="document-create"`) || strings.Contains(body, `data-group-settings-open="document-edit`) || strings.Contains(body, `/update"`) {
		t.Fatalf("admin docs list should not render modal edit/create controls: %s", body)
	}

	rec = httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/docs/"+documentID+"/edit", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("edit document page status = %d body=%s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	for _, want := range []string{`action="/admin/docs/` + documentID + `/edit"`, "Getting Started", `/docs/getting-started`, `name="pinned" checked`} {
		if !strings.Contains(body, want) {
			t.Fatalf("edit document page missing %q: %s", want, body)
		}
	}
	for _, hiddenField := range []string{`name="slug"`, `name="meta_title"`, `name="meta_description"`, `name="canonical_url"`} {
		if strings.Contains(body, hiddenField) {
			t.Fatalf("edit document page should auto-generate %q: %s", hiddenField, body)
		}
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/docs/"+documentID+"/edit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("edit document status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if events := repo.ListAudit(10); len(events) == 0 || events[0].Action != "document.update" {
		t.Fatalf("audit events after edit = %#v", events)
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/docs/"+documentID+"/update", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("removed update route status = %d, want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Getting Started") || !strings.Contains(rec.Body.String(), "Pinned") {
		t.Fatalf("docs index status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `class="docs-row-pin"`) {
		t.Fatalf("docs index should render pinned marker on the row edge: %s", rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Fatalf("docs index Cache-Control = %q", got)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs/Getting%20Started", nil))
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/docs/getting-started" {
		t.Fatalf("canonical slug redirect code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs/getting-started", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("doc status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Fatalf("document Cache-Control = %q", got)
	}
	body = rec.Body.String()
	for _, want := range []string{
		`<title>Getting Started</title>`,
		`<meta name="robots" content="index,follow">`,
		`<link rel="canonical" href="https://docs.example.com/docs/getting-started">`,
		`<script type="application/ld+json">`,
		"<h1>Install</h1>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("document page missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, `href="/docs/getting-started.md"`) {
		t.Fatalf("document page should not render markdown button: %s", body)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs/getting-started.md", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "# Getting Started") || !strings.Contains(rec.Header().Get("Content-Type"), "text/markdown") {
		t.Fatalf("markdown response code=%d content-type=%q body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}

	for _, path := range []string{"/sitemap.xml", "/sitemap-docs.xml", "/llms.txt", "/robots.txt"} {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "https://docs.example.com/docs/getting-started") && path != "/robots.txt" {
			t.Fatalf("%s missing document URL: %s", path, rec.Body.String())
		}
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/robots.txt", nil))
	if !strings.Contains(rec.Body.String(), "IndexNow: https://docs.example.com/"+indexNowKey+".txt") {
		t.Fatalf("robots.txt missing IndexNow key URL: %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/"+indexNowKey+".txt", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != indexNowKey {
		t.Fatalf("IndexNow key response code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestAdminDraftPreviewCanPublishDocument(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	draftDoc, err := control.CreateDocument(admin, controlplane.DocumentInput{
		Slug:   "draft-runbook",
		Title:  "Draft Runbook",
		Body:   "Internal",
		Status: core.DocumentStatusDraft,
	})
	if err != nil {
		t.Fatalf("CreateDocument returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()
	adminHandler := authenticatedUserHandler(t, control, admin, handler)

	rec := httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs/draft-runbook", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `/admin/docs/`+draftDoc.ID+`/publish`) || !strings.Contains(body, `>Publish</button>`) {
		t.Fatalf("admin draft preview should show publish action; code=%d body=%s", rec.Code, body)
	}
	if strings.Contains(body, `href="/docs/draft-runbook.md"`) {
		t.Fatalf("draft preview should not render markdown button: %s", body)
	}

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	req := httptest.NewRequest(http.MethodPost, "/admin/docs/"+draftDoc.ID+"/publish", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/docs/draft-runbook" {
		t.Fatalf("publish response code=%d location=%q body=%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	published, err := control.GetDocumentForUser("draft-runbook", core.User{})
	if err != nil {
		t.Fatalf("published document should be public: %v", err)
	}
	if published.Status != core.DocumentStatusPublished {
		t.Fatalf("published status = %q", published.Status)
	}
}

func TestAdminDocumentActionsAvoidFullDocumentList(t *testing.T) {
	base := storage.NewMemoryRepository()
	admin := core.User{ID: "admin", Username: "admin", Role: core.UserRoleAdmin, Enabled: true}
	if err := base.UpsertUser(admin); err != nil {
		t.Fatal(err)
	}
	repo := &documentFullListPanicRepository{MemoryRepository: base}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	document, err := control.CreateDocument(admin, controlplane.DocumentInput{
		Slug:   "draft-runbook",
		Title:  "Draft Runbook",
		Body:   "Internal",
		Status: core.DocumentStatusDraft,
	})
	if err != nil {
		t.Fatalf("CreateDocument returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, admin, server.Handler())

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/docs/"+document.ID+"/edit", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("edit page status = %d body=%s", rec.Code, rec.Body.String())
	}

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("title", "Updated Runbook")
	form.Set("body", "Updated body")
	form.Set("status", string(core.DocumentStatusDraft))
	req := httptest.NewRequest(http.MethodPost, "/admin/docs/"+document.ID+"/edit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("edit submit status = %d body=%s", rec.Code, rec.Body.String())
	}

	publishForm := url.Values{}
	publishForm.Set("csrf_token", testConsoleCSRFToken)
	req = httptest.NewRequest(http.MethodPost, "/admin/docs/"+document.ID+"/publish", strings.NewReader(publishForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/docs/draft-runbook" {
		t.Fatalf("publish response code=%d location=%q body=%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
}

func TestDraftDocumentsAreNoindexAndExcludedFromAIIndexes(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	draftDoc, err := control.CreateDocument(admin, controlplane.DocumentInput{
		Slug:   "draft-runbook",
		Title:  "Draft Runbook",
		Body:   "Internal",
		Status: core.DocumentStatusDraft,
	})
	if err != nil {
		t.Fatalf("CreateDocument returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := server.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs/draft-runbook", nil))
	if rec.Code != http.StatusNotFound || rec.Header().Get("X-Robots-Tag") != "noindex" {
		t.Fatalf("draft doc response code=%d x-robots=%q", rec.Code, rec.Header().Get("X-Robots-Tag"))
	}

	adminHandler := authenticatedUserHandler(t, control, admin, handler)
	rec = httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs/draft-runbook", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("X-Robots-Tag") != "noindex" || !strings.Contains(rec.Body.String(), `content="noindex,nofollow"`) {
		t.Fatalf("admin draft doc response code=%d x-robots=%q body=%s", rec.Code, rec.Header().Get("X-Robots-Tag"), rec.Body.String())
	}

	if _, err := control.UpdateDocument(admin, draftDoc.ID, controlplane.DocumentInput{
		Slug:   "draft-runbook-v2",
		Title:  "Draft Runbook",
		Body:   "Internal",
		Status: core.DocumentStatusDraft,
	}); err != nil {
		t.Fatalf("UpdateDocument returned error: %v", err)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs/draft-runbook", nil))
	if rec.Code != http.StatusNotFound || rec.Header().Get("Location") != "" {
		t.Fatalf("anonymous draft redirect leak code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}

	for _, path := range []string{"/sitemap.xml", "/llms.txt", "/docs/draft-runbook.md"} {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if strings.Contains(rec.Body.String(), "draft-runbook") {
			t.Fatalf("%s should exclude draft doc: %s", path, rec.Body.String())
		}
	}
}

type documentFullListPanicRepository struct {
	*storage.MemoryRepository
}

func (r *documentFullListPanicRepository) ListDocuments() []core.Document {
	panic("document edit and publish actions should use GetDocument instead of full ListDocuments")
}
