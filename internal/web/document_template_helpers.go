package web

import (
	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
)

func documentStatusText(locale string, status core.DocumentStatus) string {
	switch status {
	case core.DocumentStatusPublished:
		return translate(locale, "document_status_published")
	case core.DocumentStatusArchived:
		return translate(locale, "document_status_archived")
	default:
		return translate(locale, "document_status_draft")
	}
}

func documentStatusClass(status core.DocumentStatus) string {
	switch status {
	case core.DocumentStatusPublished:
		return "tone-good"
	case core.DocumentStatusArchived:
		return "tone-muted"
	default:
		return "tone-warn"
	}
}

func documentIndexingText(locale string, document core.Document) string {
	if controlplane.DocumentSEOIndexable(document) {
		return translate(locale, "document_indexable")
	}
	return translate(locale, "document_noindex")
}

func documentIndexingClass(document core.Document) string {
	if controlplane.DocumentSEOIndexable(document) {
		return "tone-good"
	}
	return "tone-muted"
}
