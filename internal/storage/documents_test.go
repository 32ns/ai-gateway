package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestMemoryRepositoryDocuments(t *testing.T) {
	repo := NewMemoryRepository()
	testRepositoryDocuments(t, repo)
}

func TestSQLiteRepositoryDocuments(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	testRepositoryDocuments(t, repo)
}

func TestMemoryRepositoryDocumentsPage(t *testing.T) {
	repo := NewMemoryRepository()
	testRepositoryDocumentsPage(t, repo)
}

func TestSQLiteRepositoryDocumentsPage(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	testRepositoryDocumentsPage(t, repo)
}

func TestMemoryRepositoryDocumentsSearchPage(t *testing.T) {
	repo := NewMemoryRepository()
	testRepositoryDocumentsSearchPage(t, repo)
}

func TestSQLiteRepositoryDocumentsSearchPage(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	testRepositoryDocumentsSearchPage(t, repo)
}

func testRepositoryDocumentsSearchPage(t *testing.T, repo Repository) {
	t.Helper()
	searcher, ok := repo.(documentSearcher)
	if !ok {
		t.Fatal("repository does not implement documentSearcher")
	}
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	publishedAt := base
	documents := []core.Document{
		{ID: "doc_alpha", Slug: "guides/alpha", Title: "Alpha Guide", Body: "shared alpha body", MetaDescription: "install target", Status: core.DocumentStatusPublished, PublishedAt: &publishedAt, CreatedAt: base, UpdatedAt: base.Add(3 * time.Minute)},
		{ID: "doc_beta", Slug: "guides/beta", Title: "Beta Guide", Body: "shared beta body", Status: core.DocumentStatusPublished, PublishedAt: &publishedAt, CreatedAt: base, UpdatedAt: base.Add(2 * time.Minute)},
		{ID: "doc_hidden", Slug: "guides/hidden", Title: "Hidden Alpha", Body: "shared alpha body", Status: core.DocumentStatusPublished, NoIndex: true, PublishedAt: &publishedAt, CreatedAt: base, UpdatedAt: base.Add(time.Minute)},
		{ID: "doc_draft", Slug: "guides/draft", Title: "Draft Alpha", Body: "shared alpha body", Status: core.DocumentStatusDraft, CreatedAt: base, UpdatedAt: base.Add(4 * time.Minute)},
	}
	for _, document := range documents {
		if err := repo.CreateDocument(document); err != nil {
			t.Fatalf("CreateDocument(%s) returned error: %v", document.ID, err)
		}
	}

	page, total := searcher.SearchDocumentsPage("alpha shared", "", false, 0, 2)
	if total != 3 {
		t.Fatalf("all search total = %d, want 3", total)
	}
	if len(page) != 2 || page[0].ID != "doc_alpha" || page[1].ID != "doc_hidden" {
		t.Fatalf("all search page = %#v", page)
	}

	page, total = searcher.SearchDocumentsPage("alpha", core.DocumentStatusPublished, true, 0, 10)
	if total != 1 || len(page) != 1 || page[0].ID != "doc_alpha" {
		t.Fatalf("seo search total=%d page=%#v", total, page)
	}

	page, total = searcher.SearchDocumentsPage("install target", core.DocumentStatusPublished, true, 0, 10)
	if total != 1 || len(page) != 1 || page[0].ID != "doc_alpha" {
		t.Fatalf("metadata search total=%d page=%#v", total, page)
	}
}

func testRepositoryDocumentsPage(t *testing.T, repo Repository) {
	t.Helper()
	pager, ok := repo.(documentPager)
	if !ok {
		t.Fatal("repository does not implement documentPager")
	}
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	publishedAt := base
	documents := []core.Document{
		{ID: "doc_published", Slug: "published", Title: "Published", Body: "Body", Status: core.DocumentStatusPublished, PublishedAt: &publishedAt, CreatedAt: base, UpdatedAt: base.Add(2 * time.Minute)},
		{ID: "doc_pinned", Slug: "pinned", Title: "Pinned", Body: "Body", Status: core.DocumentStatusPublished, Pinned: true, PublishedAt: &publishedAt, CreatedAt: base, UpdatedAt: base.Add(time.Minute)},
		{ID: "doc_noindex", Slug: "noindex", Title: "Noindex", Body: "Body", Status: core.DocumentStatusPublished, NoIndex: true, PublishedAt: &publishedAt, CreatedAt: base, UpdatedAt: base.Add(4 * time.Minute)},
		{ID: "doc_draft", Slug: "draft", Title: "Draft", Body: "Body", Status: core.DocumentStatusDraft, CreatedAt: base, UpdatedAt: base.Add(3 * time.Minute)},
		{ID: "doc_archived", Slug: "archived", Title: "Archived", Body: "Body", Status: core.DocumentStatusArchived, CreatedAt: base, UpdatedAt: base.Add(5 * time.Minute)},
	}
	for _, document := range documents {
		if err := repo.CreateDocument(document); err != nil {
			t.Fatalf("CreateDocument(%s) returned error: %v", document.ID, err)
		}
	}

	page, total := pager.ListDocumentsPage("", false, 0, 3)
	if total != 5 {
		t.Fatalf("all total = %d, want 5", total)
	}
	if len(page) != 3 || page[0].ID != "doc_pinned" || page[1].ID != "doc_noindex" || page[2].ID != "doc_published" {
		t.Fatalf("all page = %#v", page)
	}

	page, total = pager.ListDocumentsPage(core.DocumentStatusPublished, true, 0, 10)
	if total != 2 {
		t.Fatalf("seo total = %d, want 2", total)
	}
	if len(page) != 2 || page[0].ID != "doc_pinned" || page[1].ID != "doc_published" {
		t.Fatalf("seo page = %#v", page)
	}

	page, total = pager.ListDocumentsPage(core.DocumentStatusDraft, false, 0, 10)
	if total != 1 || len(page) != 1 || page[0].ID != "doc_draft" {
		t.Fatalf("draft page total=%d items=%#v", total, page)
	}
}

func testRepositoryDocuments(t *testing.T, repo Repository) {
	t.Helper()
	now := time.Now().UTC()
	publishedAt := now
	document := core.Document{
		ID:          "doc_one",
		Slug:        "guides/start",
		Title:       "Start",
		Body:        "# Start",
		Status:      core.DocumentStatusPublished,
		CreatedBy:   "admin",
		PublishedAt: &publishedAt,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := repo.CreateDocument(document); err != nil {
		t.Fatalf("CreateDocument returned error: %v", err)
	}
	if _, err := repo.GetDocumentBySlug("guides/start"); err != nil {
		t.Fatalf("GetDocumentBySlug returned error: %v", err)
	}
	if err := repo.UpsertDocumentRedirect(core.DocumentRedirect{FromSlug: "old/start", ToSlug: "guides/start", StatusCode: 301, CreatedAt: now}); err != nil {
		t.Fatalf("UpsertDocumentRedirect returned error: %v", err)
	}
	redirect, err := repo.GetDocumentRedirect("old/start")
	if err != nil {
		t.Fatalf("GetDocumentRedirect returned error: %v", err)
	}
	if redirect.ToSlug != "guides/start" || redirect.StatusCode != 301 {
		t.Fatalf("redirect = %#v", redirect)
	}
	document.Title = "Start Updated"
	document.Slug = "guides/start-here"
	if err := repo.UpdateDocument(document); err != nil {
		t.Fatalf("UpdateDocument returned error: %v", err)
	}
	updated, err := repo.GetDocument(document.ID)
	if err != nil {
		t.Fatalf("GetDocument returned error: %v", err)
	}
	if updated.Title != "Start Updated" || updated.Slug != "guides/start-here" || updated.CreatedAt.IsZero() {
		t.Fatalf("updated document = %#v", updated)
	}
	pinnedDocument := core.Document{
		ID:          "doc_pinned",
		Slug:        "guides/pinned",
		Title:       "Pinned",
		Body:        "# Pinned",
		Pinned:      true,
		Status:      core.DocumentStatusPublished,
		PublishedAt: &publishedAt,
		CreatedAt:   now.Add(-time.Hour),
		UpdatedAt:   now.Add(-time.Hour),
	}
	if err := repo.CreateDocument(pinnedDocument); err != nil {
		t.Fatalf("CreateDocument pinned returned error: %v", err)
	}
	documents := repo.ListDocuments()
	if len(documents) != 2 || documents[0].ID != pinnedDocument.ID || !documents[0].Pinned {
		t.Fatalf("ListDocuments = %#v", documents)
	}
	if err := repo.DeleteDocument(document.ID); err != nil {
		t.Fatalf("DeleteDocument returned error: %v", err)
	}
	if err := repo.DeleteDocument(pinnedDocument.ID); err != nil {
		t.Fatalf("DeleteDocument pinned returned error: %v", err)
	}
	if _, err := repo.GetDocument(document.ID); err == nil {
		t.Fatal("expected deleted document to be missing")
	}
}
