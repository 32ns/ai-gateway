package controlplane

import (
	"strings"
	"testing"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestDocumentsPublishingAndSEO(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := service.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user, err := service.CreateUser(UserInput{Username: "alice", Password: "alice-secret", Role: core.UserRoleUser, Enabled: true})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if _, err := service.CreateDocument(user, DocumentInput{Slug: "denied", Title: "Denied", Body: "No"}); err == nil || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("CreateDocument as user err = %v, want admin error", err)
	}
	publishedDoc, err := service.CreateDocument(admin, DocumentInput{
		Slug:   "Getting Started",
		Title:  "Getting Started",
		Body:   "# Start",
		Pinned: true,
		Status: core.DocumentStatusPublished,
	})
	if err != nil {
		t.Fatalf("CreateDocument published returned error: %v", err)
	}
	if publishedDoc.Slug != "getting-started" {
		t.Fatalf("Slug = %q, want normalized getting-started", publishedDoc.Slug)
	}
	if !DocumentSEOIndexable(publishedDoc) || DocumentRobotsNoIndex(publishedDoc) {
		t.Fatalf("published document should be indexable: %#v", publishedDoc)
	}
	if !publishedDoc.Pinned {
		t.Fatalf("published document should be pinned: %#v", publishedDoc)
	}
	draftDoc, err := service.CreateDocument(admin, DocumentInput{
		Slug:   "draft-runbook",
		Title:  "Draft Runbook",
		Body:   "Internal",
		Status: core.DocumentStatusDraft,
	})
	if err != nil {
		t.Fatalf("CreateDocument draft returned error: %v", err)
	}
	if DocumentSEOIndexable(draftDoc) || !DocumentRobotsNoIndex(draftDoc) {
		t.Fatalf("draft document must be noindex: %#v", draftDoc)
	}
	if docs := service.ListVisibleDocuments(core.User{}); len(docs) != 1 || docs[0].ID != publishedDoc.ID {
		t.Fatalf("anonymous docs = %#v, want published doc only", docs)
	}
	if docs := service.ListVisibleDocuments(user); len(docs) != 1 || docs[0].ID != publishedDoc.ID {
		t.Fatalf("signed-in docs = %#v, want published doc only", docs)
	}
	if docs := service.ListDocuments(admin); len(docs) != 2 || !documentListContains(docs, draftDoc.ID) {
		t.Fatalf("admin docs = %#v, want published and draft docs", docs)
	}
	if docs := service.ListPublicDocumentsForSEO(); len(docs) != 1 || docs[0].ID != publishedDoc.ID {
		t.Fatalf("SEO docs = %#v, want published doc only", docs)
	}
}

func TestDocumentsRedirectsAndValidation(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := service.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	doc, err := service.CreateDocument(admin, DocumentInput{
		Slug:   "start-guide",
		Title:  "Start Guide",
		Body:   "Guide",
		Status: core.DocumentStatusPublished,
	})
	if err != nil {
		t.Fatalf("CreateDocument returned error: %v", err)
	}
	updated, err := service.UpdateDocument(admin, doc.ID, DocumentInput{
		Slug:   "start-guide-v2",
		Title:  "Start Guide",
		Body:   "Guide",
		Status: core.DocumentStatusPublished,
	})
	if err != nil {
		t.Fatalf("UpdateDocument returned error: %v", err)
	}
	if updated.Slug != "start-guide-v2" {
		t.Fatalf("updated slug = %q", updated.Slug)
	}
	redirect, err := service.GetDocumentRedirect("start-guide")
	if err != nil {
		t.Fatalf("GetDocumentRedirect returned error: %v", err)
	}
	if redirect.ToSlug != "start-guide-v2" || redirect.StatusCode != 301 {
		t.Fatalf("redirect = %#v", redirect)
	}
	if _, err := service.CreateDocument(admin, DocumentInput{Slug: "bad slug!", Title: "Bad", Body: "Bad"}); err == nil {
		t.Fatal("expected invalid slug to be rejected")
	}
	if _, err := service.CreateDocument(admin, DocumentInput{Slug: "bad-canonical", Title: "Bad Canonical", Body: "Bad", CanonicalURL: "javascript:alert(1)"}); err == nil {
		t.Fatal("expected invalid canonical URL to be rejected")
	}
	if _, err := service.CreateDocument(admin, DocumentInput{Slug: "bad-canonical-space", Title: "Bad Canonical", Body: "Bad", CanonicalURL: "https://example.com/bad url"}); err == nil {
		t.Fatal("expected canonical URL with whitespace to be rejected")
	}
}

func TestDocumentSlugGeneratedFromTitle(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := service.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	doc, err := service.CreateDocument(admin, DocumentInput{
		Title:  "Getting Started",
		Body:   "Guide",
		Status: core.DocumentStatusPublished,
	})
	if err != nil {
		t.Fatalf("CreateDocument returned error: %v", err)
	}
	if doc.Slug != "getting-started" {
		t.Fatalf("generated slug = %q, want getting-started", doc.Slug)
	}
	duplicate, err := service.CreateDocument(admin, DocumentInput{
		Title:  "Getting Started",
		Body:   "Second guide",
		Status: core.DocumentStatusPublished,
	})
	if err != nil {
		t.Fatalf("CreateDocument duplicate title returned error: %v", err)
	}
	if duplicate.Slug != "getting-started-2" {
		t.Fatalf("duplicate generated slug = %q, want getting-started-2", duplicate.Slug)
	}
	chineseDoc, err := service.CreateDocument(admin, DocumentInput{
		Title:  "中文文档",
		Body:   "Guide",
		Status: core.DocumentStatusDraft,
	})
	if err != nil {
		t.Fatalf("CreateDocument chinese title returned error: %v", err)
	}
	if !strings.HasPrefix(chineseDoc.Slug, "doc-") {
		t.Fatalf("chinese generated slug = %q, want doc- prefix", chineseDoc.Slug)
	}
	updated, err := service.UpdateDocument(admin, doc.ID, DocumentInput{
		Title:  "Renamed Guide",
		Body:   "Guide",
		Status: core.DocumentStatusPublished,
	})
	if err != nil {
		t.Fatalf("UpdateDocument without slug returned error: %v", err)
	}
	if updated.Slug != "getting-started" {
		t.Fatalf("update without slug changed slug to %q", updated.Slug)
	}
}

func documentListContains(documents []core.Document, id string) bool {
	for _, document := range documents {
		if document.ID == id {
			return true
		}
	}
	return false
}
