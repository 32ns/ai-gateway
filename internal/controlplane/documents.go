package controlplane

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"
)

var documentSlugSegmentPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_-]*[a-z0-9])?$`)

type DocumentInput struct {
	Slug            string
	Title           string
	Body            string
	MetaTitle       string
	MetaDescription string
	CanonicalURL    string
	Pinned          bool
	NoIndex         bool
	Status          core.DocumentStatus
}

type documentPager interface {
	ListDocumentsPage(status core.DocumentStatus, seoOnly bool, offset, limit int) ([]core.Document, int)
}

type documentSearcher interface {
	SearchDocumentsPage(query string, status core.DocumentStatus, seoOnly bool, offset, limit int) ([]core.Document, int)
}

func (s *Service) CreateDocument(actor core.User, input DocumentInput) (core.Document, error) {
	if !actor.IsAdmin() {
		return core.Document{}, fmt.Errorf("admin role required")
	}
	document, err := s.documentFromInput(actor, core.Document{}, input, true)
	if err != nil {
		return core.Document{}, err
	}
	for range 3 {
		document.ID = generateDocumentID()
		if err := s.repo.CreateDocument(document); err != nil {
			if errors.Is(err, storage.ErrBillingRequestConflict) {
				continue
			}
			return core.Document{}, err
		}
		return document, nil
	}
	return core.Document{}, fmt.Errorf("document id conflict")
}

func (s *Service) UpdateDocument(actor core.User, id string, input DocumentInput) (core.Document, error) {
	if !actor.IsAdmin() {
		return core.Document{}, fmt.Errorf("admin role required")
	}
	existing, err := s.repo.GetDocument(strings.TrimSpace(id))
	if err != nil {
		return core.Document{}, err
	}
	previousSlug := existing.Slug
	document, err := s.documentFromInput(actor, existing, input, false)
	if err != nil {
		return core.Document{}, err
	}
	if err := s.repo.UpdateDocument(document); err != nil {
		return core.Document{}, err
	}
	if !strings.EqualFold(previousSlug, document.Slug) {
		_ = s.repo.UpsertDocumentRedirect(core.DocumentRedirect{
			FromSlug:   previousSlug,
			ToSlug:     document.Slug,
			StatusCode: 301,
			CreatedAt:  time.Now().UTC(),
		})
		_ = s.repo.DeleteDocumentRedirect(document.Slug)
	}
	return s.repo.GetDocument(document.ID)
}

func (s *Service) DeleteDocument(actor core.User, id string) (core.Document, error) {
	if !actor.IsAdmin() {
		return core.Document{}, fmt.Errorf("admin role required")
	}
	document, err := s.repo.GetDocument(strings.TrimSpace(id))
	if err != nil {
		return core.Document{}, err
	}
	if err := s.repo.DeleteDocument(document.ID); err != nil {
		return core.Document{}, err
	}
	return document, nil
}

func (s *Service) GetDocument(actor core.User, id string) (core.Document, error) {
	if !actor.IsAdmin() {
		return core.Document{}, fmt.Errorf("admin role required")
	}
	return s.repo.GetDocument(strings.TrimSpace(id))
}

func (s *Service) ListDocuments(actor core.User) []core.Document {
	if !actor.IsAdmin() {
		return nil
	}
	return s.repo.ListDocuments()
}

func (s *Service) ListDocumentsPage(actor core.User, status core.DocumentStatus, offset, limit int) ([]core.Document, int) {
	if !actor.IsAdmin() {
		return nil, 0
	}
	if !validDocumentStatusFilter(status) {
		return nil, 0
	}
	if pager, ok := s.repo.(documentPager); ok {
		return pager.ListDocumentsPage(status, false, offset, limit)
	}
	return documentPage(s.repo.ListDocuments(), status, false, offset, limit)
}

func (s *Service) SearchDocumentsPage(actor core.User, query string, status core.DocumentStatus, offset, limit int) ([]core.Document, int) {
	if !actor.IsAdmin() {
		return nil, 0
	}
	if !validDocumentStatusFilter(status) {
		return nil, 0
	}
	if searcher, ok := s.repo.(documentSearcher); ok {
		return searcher.SearchDocumentsPage(query, status, false, offset, limit)
	}
	return documentSearchPage(s.repo.ListDocuments(), query, status, false, offset, limit)
}

func (s *Service) ListVisibleDocuments(user core.User) []core.Document {
	documents := s.repo.ListDocuments()
	out := make([]core.Document, 0, len(documents))
	for _, document := range documents {
		if s.DocumentVisibleToUser(document, user) {
			out = append(out, document)
		}
	}
	return out
}

func (s *Service) ListPublicDocumentsForSEO() []core.Document {
	documents := s.repo.ListDocuments()
	out := make([]core.Document, 0, len(documents))
	for _, document := range documents {
		if DocumentSEOIndexable(document) {
			out = append(out, document)
		}
	}
	return out
}

func (s *Service) ListPublicDocumentsForSEOPage(status core.DocumentStatus, offset, limit int) ([]core.Document, int) {
	if status != "" && status != core.DocumentStatusPublished {
		return nil, 0
	}
	if pager, ok := s.repo.(documentPager); ok {
		return pager.ListDocumentsPage(core.DocumentStatusPublished, true, offset, limit)
	}
	return documentPage(s.repo.ListDocuments(), core.DocumentStatusPublished, true, offset, limit)
}

func (s *Service) SearchPublicDocumentsForSEOPage(query string, offset, limit int) ([]core.Document, int) {
	if searcher, ok := s.repo.(documentSearcher); ok {
		return searcher.SearchDocumentsPage(query, core.DocumentStatusPublished, true, offset, limit)
	}
	return documentSearchPage(s.repo.ListDocuments(), query, core.DocumentStatusPublished, true, offset, limit)
}

func (s *Service) GetDocumentForUser(slug string, user core.User) (core.Document, error) {
	document, err := s.repo.GetDocumentBySlug(slug)
	if err != nil {
		return core.Document{}, err
	}
	if !s.DocumentVisibleToUser(document, user) {
		return core.Document{}, fmt.Errorf("document is unavailable")
	}
	return document, nil
}

func (s *Service) GetDocumentRedirect(fromSlug string) (core.DocumentRedirect, error) {
	return s.repo.GetDocumentRedirect(fromSlug)
}

func (s *Service) DocumentVisibleToUser(document core.Document, user core.User) bool {
	if user.IsAdmin() {
		return true
	}
	return document.Status == core.DocumentStatusPublished
}

func DocumentSEOIndexable(document core.Document) bool {
	return document.Status == core.DocumentStatusPublished && !document.NoIndex
}

func DocumentRobotsNoIndex(document core.Document) bool {
	return !DocumentSEOIndexable(document)
}

func NormalizeDocumentSlug(value string) (string, error) {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	value = strings.Trim(value, "/")
	value = strings.ToLower(value)
	value = strings.Join(strings.Fields(value), "-")
	if value == "" {
		return "", fmt.Errorf("document slug is required")
	}
	if len(value) > 180 {
		return "", fmt.Errorf("document slug is too long")
	}
	if strings.Contains(value, "//") {
		return "", fmt.Errorf("document slug must not contain empty path segments")
	}
	segments := strings.Split(value, "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("document slug contains an invalid path segment")
		}
		if !documentSlugSegmentPattern.MatchString(segment) {
			return "", fmt.Errorf("document slug may only contain lowercase letters, numbers, hyphens, underscores, and slashes")
		}
	}
	return value, nil
}

func (s *Service) documentFromInput(actor core.User, existing core.Document, input DocumentInput, create bool) (core.Document, error) {
	title := strings.TrimSpace(input.Title)
	body := strings.TrimSpace(input.Body)
	if title == "" || body == "" {
		return core.Document{}, fmt.Errorf("document title and body are required")
	}
	slug, err := s.documentSlugFromInput(input, existing, create)
	if err != nil {
		return core.Document{}, err
	}
	status := normalizeDocumentStatus(input.Status)
	now := time.Now().UTC()
	document := existing
	if create {
		document.CreatedBy = actor.ID
		document.CreatedAt = now
	}
	document.Slug = slug
	document.Title = title
	document.Body = body
	document.MetaTitle = trimDocumentText(input.MetaTitle, 120)
	document.MetaDescription = trimDocumentText(input.MetaDescription, 240)
	document.CanonicalURL, err = normalizeDocumentAbsoluteURL("canonical URL", input.CanonicalURL)
	if err != nil {
		return core.Document{}, err
	}
	document.Pinned = input.Pinned
	document.NoIndex = input.NoIndex
	document.Status = status
	document.UpdatedBy = actor.ID
	document.UpdatedAt = now
	if document.CreatedAt.IsZero() {
		document.CreatedAt = now
	}
	switch status {
	case core.DocumentStatusPublished:
		if document.PublishedAt == nil || document.PublishedAt.IsZero() {
			publishedAt := now
			document.PublishedAt = &publishedAt
		}
	case core.DocumentStatusDraft:
		document.PublishedAt = nil
	}
	return document, nil
}

func (s *Service) documentSlugFromInput(input DocumentInput, existing core.Document, create bool) (string, error) {
	raw := strings.TrimSpace(input.Slug)
	if raw != "" {
		slug, err := NormalizeDocumentSlug(raw)
		if err != nil {
			return "", err
		}
		if err := s.ensureDocumentSlugAvailable(slug, existing.ID); err != nil {
			return "", err
		}
		return slug, nil
	}
	if !create && strings.TrimSpace(existing.Slug) != "" {
		return existing.Slug, nil
	}
	base := documentSlugCandidate(input.Title)
	if base == "" {
		base = strings.Replace(generateDocumentID(), "doc_", "doc-", 1)
	}
	return s.ensureGeneratedDocumentSlugAvailable(base, existing.ID)
}

func documentSlugCandidate(value string) string {
	var builder strings.Builder
	lastSeparator := true
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastSeparator = false
		case r == '-' || r == '_' || r == '/' || r == ' ' || r == '\t' || r == '\r' || r == '\n':
			if !lastSeparator && builder.Len() < 180 {
				builder.WriteByte('-')
				lastSeparator = true
			}
		default:
			if !lastSeparator && builder.Len() < 180 {
				builder.WriteByte('-')
				lastSeparator = true
			}
		}
		if builder.Len() >= 180 {
			break
		}
	}
	return strings.Trim(builder.String(), "-_")
}

func (s *Service) ensureGeneratedDocumentSlugAvailable(base, currentID string) (string, error) {
	base = strings.Trim(strings.TrimSpace(base), "-_")
	if base == "" {
		base = strings.Replace(generateDocumentID(), "doc_", "doc-", 1)
	}
	for suffix := 0; suffix < 100; suffix++ {
		slug := generatedDocumentSlug(base, suffix)
		available, err := s.documentSlugAvailable(slug, currentID)
		if err != nil {
			return "", err
		}
		if available {
			return slug, nil
		}
	}
	return strings.Replace(generateDocumentID(), "doc_", "doc-", 1), nil
}

func generatedDocumentSlug(base string, suffix int) string {
	if suffix <= 0 {
		return truncateGeneratedDocumentSlug(base, "")
	}
	value := fmt.Sprintf("-%d", suffix+1)
	return truncateGeneratedDocumentSlug(base, value) + value
}

func truncateGeneratedDocumentSlug(base, suffix string) string {
	maxLength := 180 - len(suffix)
	if maxLength < 1 {
		maxLength = 1
	}
	if len(base) > maxLength {
		base = base[:maxLength]
	}
	base = strings.Trim(base, "-_")
	if base == "" {
		return "doc"
	}
	return base
}

func (s *Service) ensureDocumentSlugAvailable(slug, currentID string) error {
	available, err := s.documentSlugAvailable(slug, currentID)
	if err != nil {
		return err
	}
	if !available {
		return fmt.Errorf("document slug %q already exists", slug)
	}
	return nil
}

func (s *Service) documentSlugAvailable(slug, currentID string) (bool, error) {
	existing, err := s.repo.GetDocumentBySlug(slug)
	if err == nil && existing.ID != strings.TrimSpace(currentID) {
		return false, nil
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return false, err
	}
	return true, nil
}

func normalizeDocumentStatus(status core.DocumentStatus) core.DocumentStatus {
	switch status {
	case core.DocumentStatusPublished, core.DocumentStatusArchived:
		return status
	default:
		return core.DocumentStatusDraft
	}
}

func validDocumentStatusFilter(status core.DocumentStatus) bool {
	switch status {
	case "", core.DocumentStatusPublished, core.DocumentStatusDraft, core.DocumentStatusArchived:
		return true
	default:
		return false
	}
}

func documentPage(documents []core.Document, status core.DocumentStatus, seoOnly bool, offset, limit int) ([]core.Document, int) {
	return documentFilterPage(documents, status, seoOnly, offset, limit, nil)
}

func documentSearchPage(documents []core.Document, query string, status core.DocumentStatus, seoOnly bool, offset, limit int) ([]core.Document, int) {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return documentPage(documents, status, seoOnly, offset, limit)
	}
	return documentFilterPage(documents, status, seoOnly, offset, limit, func(document core.Document) bool {
		haystack := strings.ToLower(strings.Join([]string{
			document.Title,
			document.Slug,
			document.MetaTitle,
			document.MetaDescription,
			document.Body,
		}, "\n"))
		for _, term := range terms {
			if !strings.Contains(haystack, term) {
				return false
			}
		}
		return true
	})
}

func documentFilterPage(documents []core.Document, status core.DocumentStatus, seoOnly bool, offset, limit int, match func(core.Document) bool) ([]core.Document, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 25
	}
	filtered := make([]core.Document, 0, len(documents))
	for _, document := range documents {
		if status != "" && document.Status != status {
			continue
		}
		if seoOnly && !DocumentSEOIndexable(document) {
			continue
		}
		if match != nil && !match(document) {
			continue
		}
		filtered = append(filtered, document)
	}
	total := len(filtered)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return append([]core.Document(nil), filtered[offset:end]...), total
}

func normalizeDocumentAbsoluteURL(field, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if strings.ContainsAny(value, " \t\r\n") {
		return "", fmt.Errorf("document %s must not contain whitespace", field)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("document %s must be an absolute http or https URL", field)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return value, nil
	default:
		return "", fmt.Errorf("document %s must use http or https", field)
	}
}

func trimDocumentText(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	if maxRunes <= 0 {
		return value
	}
	count := 0
	for index := range value {
		if count == maxRunes {
			return strings.TrimSpace(value[:index])
		}
		count++
	}
	return value
}

func generateDocumentID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("doc_%d", time.Now().UnixNano())
	}
	return "doc_" + hex.EncodeToString(buf)
}
