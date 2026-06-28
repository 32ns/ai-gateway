package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func (r *SQLiteRepository) CreateDocument(document core.Document) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	document.ID = strings.TrimSpace(document.ID)
	document.Slug = normalizeDocumentSlugForStorage(document.Slug)
	document.Title = strings.TrimSpace(document.Title)
	document.Body = strings.TrimSpace(document.Body)
	if document.ID == "" || document.Slug == "" || document.Title == "" || document.Body == "" {
		return fmt.Errorf("document id, slug, title, and body are required")
	}
	now := time.Now().UTC()
	if document.CreatedAt.IsZero() {
		document.CreatedAt = now
	}
	if document.UpdatedAt.IsZero() {
		document.UpdatedAt = document.CreatedAt
	}
	payload, err := json.Marshal(cloneDocument(document))
	if err != nil {
		return err
	}
	_, err = r.db.Exec(
		`INSERT INTO documents(id, slug_key, status, pinned, noindex, search_text, visibility, category_key, sort_order, published_at_ns, updated_at_ns, payload)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		document.ID,
		documentSlugKey(document.Slug),
		string(document.Status),
		boolInt(document.Pinned),
		boolInt(document.NoIndex),
		documentSearchText(document),
		"public",
		"",
		0,
		sqliteTimeNS(valueTime(document.PublishedAt)),
		sqliteTimeNS(document.UpdatedAt),
		string(payload),
	)
	return err
}

func (r *SQLiteRepository) UpdateDocument(document core.Document) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, err := r.getDocumentLocked(strings.TrimSpace(document.ID))
	if err != nil {
		return err
	}
	document.Slug = normalizeDocumentSlugForStorage(document.Slug)
	document.Title = strings.TrimSpace(document.Title)
	document.Body = strings.TrimSpace(document.Body)
	if document.Slug == "" || document.Title == "" || document.Body == "" {
		return fmt.Errorf("document slug, title, and body are required")
	}
	document.ID = existing.ID
	document.CreatedBy = existing.CreatedBy
	document.CreatedAt = existing.CreatedAt
	document.UpdatedAt = time.Now().UTC()
	payload, err := json.Marshal(cloneDocument(document))
	if err != nil {
		return err
	}
	_, err = r.db.Exec(
		`UPDATE documents
		SET slug_key = ?, status = ?, pinned = ?, noindex = ?, search_text = ?, visibility = ?, category_key = ?, sort_order = ?, published_at_ns = ?, updated_at_ns = ?, payload = ?
		WHERE id = ?`,
		documentSlugKey(document.Slug),
		string(document.Status),
		boolInt(document.Pinned),
		boolInt(document.NoIndex),
		documentSearchText(document),
		"public",
		"",
		0,
		sqliteTimeNS(valueTime(document.PublishedAt)),
		sqliteTimeNS(document.UpdatedAt),
		string(payload),
		document.ID,
	)
	return err
}

func (r *SQLiteRepository) GetDocument(id string) (core.Document, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.getDocumentLocked(strings.TrimSpace(id))
}

func (r *SQLiteRepository) GetDocumentBySlug(slug string) (core.Document, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var payload string
	err := r.db.QueryRow(`SELECT payload FROM documents WHERE slug_key = ?`, documentSlugKey(slug)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Document{}, ErrNotFound
	}
	if err != nil {
		return core.Document{}, err
	}
	return decodeDocumentPayload(payload)
}

func (r *SQLiteRepository) DeleteDocument(id string) error {
	return r.deleteByID("documents", id)
}

func (r *SQLiteRepository) ListDocuments() []core.Document {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`SELECT payload FROM documents ORDER BY status ASC, sort_order ASC, updated_at_ns DESC, slug_key ASC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]core.Document, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		document, err := decodeDocumentPayload(payload)
		if err != nil {
			continue
		}
		out = append(out, document)
	}
	return sortDocuments(out)
}

func (r *SQLiteRepository) ListDocumentsPage(status core.DocumentStatus, seoOnly bool, offset, limit int) ([]core.Document, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 25
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	where, args := documentPageWhere(status, seoOnly)
	total := 0
	_ = r.db.QueryRow(`SELECT COUNT(*) FROM documents `+where, args...).Scan(&total)
	queryArgs := append(append([]any(nil), args...), limit, offset)
	rows, err := r.db.Query(`
		SELECT payload
		FROM documents
		`+where+`
		ORDER BY CASE status
			WHEN 'published' THEN 0
			WHEN 'draft' THEN 1
			WHEN 'archived' THEN 2
			ELSE 3
		END ASC, pinned DESC, updated_at_ns DESC, slug_key ASC
		LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		return nil, total
	}
	defer rows.Close()
	out := make([]core.Document, 0, limit)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		document, err := decodeDocumentPayload(payload)
		if err != nil {
			continue
		}
		out = append(out, document)
	}
	return out, total
}

func (r *SQLiteRepository) SearchDocumentsPage(query string, status core.DocumentStatus, seoOnly bool, offset, limit int) ([]core.Document, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 25
	}
	terms := documentSearchTerms(query)
	if len(terms) == 0 {
		return r.ListDocumentsPage(status, seoOnly, offset, limit)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	where, args := documentSearchWhere(status, seoOnly, terms, 0)
	total := 0
	_ = r.db.QueryRow(`SELECT COUNT(*) FROM documents `+where, args...).Scan(&total)
	queryArgs := append(append([]any(nil), args...), limit, offset)
	rows, err := r.db.Query(`
		SELECT payload
		FROM documents
		`+where+`
		ORDER BY CASE status
			WHEN 'published' THEN 0
			WHEN 'draft' THEN 1
			WHEN 'archived' THEN 2
			ELSE 3
		END ASC, pinned DESC, updated_at_ns DESC, slug_key ASC
		LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		return nil, total
	}
	defer rows.Close()
	out := make([]core.Document, 0, limit)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		document, err := decodeDocumentPayload(payload)
		if err != nil {
			continue
		}
		out = append(out, document)
	}
	return out, total
}

func (r *SQLiteRepository) GetDocumentRedirect(fromSlug string) (core.DocumentRedirect, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var redirect core.DocumentRedirect
	var createdAtNS int64
	err := r.db.QueryRow(`SELECT from_slug, to_slug, status_code, created_at_ns FROM document_redirects WHERE from_slug_key = ?`, documentSlugKey(fromSlug)).
		Scan(&redirect.FromSlug, &redirect.ToSlug, &redirect.StatusCode, &createdAtNS)
	if errors.Is(err, sql.ErrNoRows) {
		return core.DocumentRedirect{}, ErrNotFound
	}
	if err != nil {
		return core.DocumentRedirect{}, err
	}
	redirect.CreatedAt = timeFromNS(createdAtNS)
	return redirect, nil
}

func (r *SQLiteRepository) UpsertDocumentRedirect(redirect core.DocumentRedirect) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	redirect.FromSlug = normalizeDocumentSlugForStorage(redirect.FromSlug)
	redirect.ToSlug = normalizeDocumentSlugForStorage(redirect.ToSlug)
	if redirect.FromSlug == "" || redirect.ToSlug == "" || documentSlugKey(redirect.FromSlug) == documentSlugKey(redirect.ToSlug) {
		return fmt.Errorf("document redirect source and target slugs are required")
	}
	if redirect.StatusCode == 0 {
		redirect.StatusCode = 301
	}
	if redirect.CreatedAt.IsZero() {
		redirect.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.Exec(
		`INSERT INTO document_redirects(from_slug_key, from_slug, to_slug, status_code, created_at_ns)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(from_slug_key) DO UPDATE SET to_slug = excluded.to_slug, status_code = excluded.status_code, created_at_ns = excluded.created_at_ns`,
		documentSlugKey(redirect.FromSlug),
		redirect.FromSlug,
		redirect.ToSlug,
		redirect.StatusCode,
		sqliteTimeNS(redirect.CreatedAt),
	)
	return err
}

func (r *SQLiteRepository) DeleteDocumentRedirect(fromSlug string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	result, err := r.db.Exec(`DELETE FROM document_redirects WHERE from_slug_key = ?`, documentSlugKey(fromSlug))
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *SQLiteRepository) getDocumentLocked(id string) (core.Document, error) {
	if id == "" {
		return core.Document{}, ErrNotFound
	}
	var payload string
	err := r.db.QueryRow(`SELECT payload FROM documents WHERE id = ?`, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Document{}, ErrNotFound
	}
	if err != nil {
		return core.Document{}, err
	}
	return decodeDocumentPayload(payload)
}

func decodeDocumentPayload(payload string) (core.Document, error) {
	var document core.Document
	if err := json.Unmarshal([]byte(payload), &document); err != nil {
		return core.Document{}, err
	}
	return cloneDocument(document), nil
}
