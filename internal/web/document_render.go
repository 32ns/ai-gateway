package web

import (
	"bytes"
	"encoding/json"
	"html/template"
	"regexp"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	goldmarkHTML "github.com/yuin/goldmark/renderer/html"
)

var (
	documentMarkdown = goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithRendererOptions(goldmarkHTML.WithUnsafe()),
	)
	documentHTMLPolicy       = bluemonday.UGCPolicy()
	documentWindowsPathRegex = regexp.MustCompile(`(?i)\b[A-Z]:\\[^\s]*`)
	siteMessageMarkdown      = goldmark.New(
		goldmark.WithExtensions(extension.GFM),
	)
	siteMessageHTMLPolicy = newSiteMessageHTMLPolicy()
)

func renderDocumentMarkdown(body string) template.HTML {
	var rendered bytes.Buffer
	body = escapeDocumentWindowsPaths(body)
	if err := documentMarkdown.Convert([]byte(body), &rendered); err != nil {
		return template.HTML(template.HTMLEscapeString(body))
	}
	return template.HTML(documentHTMLPolicy.Sanitize(rendered.String()))
}

func escapeDocumentWindowsPaths(body string) string {
	if !strings.Contains(body, `:\`) {
		return body
	}
	lines := strings.SplitAfter(body, "\n")
	inFence := false
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "    ") {
			continue
		}
		lines[i] = escapeDocumentWindowsPathsOutsideInlineCode(line)
	}
	return strings.Join(lines, "")
}

func escapeDocumentWindowsPathsOutsideInlineCode(line string) string {
	var out strings.Builder
	for i := 0; i < len(line); {
		if line[i] != '`' {
			next := strings.IndexByte(line[i:], '`')
			if next < 0 {
				out.WriteString(escapeDocumentWindowsPathsInText(line[i:]))
				break
			}
			next += i
			out.WriteString(escapeDocumentWindowsPathsInText(line[i:next]))
			i = next
			continue
		}
		spanEnd := matchingInlineCodeSpanEnd(line, i)
		if spanEnd < 0 {
			out.WriteString(escapeDocumentWindowsPathsInText(line[i:]))
			break
		}
		out.WriteString(line[i:spanEnd])
		i = spanEnd
	}
	return out.String()
}

func matchingInlineCodeSpanEnd(line string, start int) int {
	count := 0
	for start+count < len(line) && line[start+count] == '`' {
		count++
	}
	needle := strings.Repeat("`", count)
	next := strings.Index(line[start+count:], needle)
	if next < 0 {
		return -1
	}
	return start + count + next + count
}

func escapeDocumentWindowsPathsInText(text string) string {
	return documentWindowsPathRegex.ReplaceAllStringFunc(text, func(path string) string {
		path = strings.ReplaceAll(path, `\`, `\\`)
		path = strings.ReplaceAll(path, "<", "&lt;")
		path = strings.ReplaceAll(path, ">", "&gt;")
		return path
	})
}

func renderSiteMessageMarkdown(body string) template.HTML {
	var rendered bytes.Buffer
	body = stripSiteMessageUnsafeRawBlocks(body)
	if err := siteMessageMarkdown.Convert([]byte(body), &rendered); err != nil {
		return template.HTML(template.HTMLEscapeString(body))
	}
	return template.HTML(siteMessageHTMLPolicy.Sanitize(rendered.String()))
}

func stripSiteMessageUnsafeRawBlocks(body string) string {
	body = stripRawHTMLBlock(body, "script")
	body = stripRawHTMLBlock(body, "style")
	return body
}

func stripRawHTMLBlock(body, tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if tag == "" {
		return body
	}
	openPrefix := "<" + tag
	closeTag := "</" + tag + ">"
	for {
		lower := strings.ToLower(body)
		start := strings.Index(lower, openPrefix)
		if start < 0 {
			return body
		}
		openEnd := strings.Index(lower[start:], ">")
		if openEnd < 0 {
			return strings.TrimSpace(body[:start])
		}
		closeStart := strings.Index(lower[start+openEnd+1:], closeTag)
		if closeStart < 0 {
			return strings.TrimSpace(body[:start])
		}
		end := start + openEnd + 1 + closeStart + len(closeTag)
		body = body[:start] + body[end:]
	}
}

func newSiteMessageHTMLPolicy() *bluemonday.Policy {
	policy := bluemonday.NewPolicy()
	policy.SkipElementsContent("script", "style")
	policy.AllowStandardURLs()
	policy.RequireNoFollowOnLinks(true)
	policy.RequireNoReferrerOnLinks(true)
	policy.AddTargetBlankToFullyQualifiedLinks(true)
	policy.AllowAttrs("href").OnElements("a")
	policy.AllowElements(
		"p", "br", "hr",
		"h1", "h2", "h3", "h4", "h5", "h6",
		"strong", "b", "em", "i", "s",
		"code", "pre", "blockquote",
		"ul", "ol", "li",
		"a",
	)
	return policy
}

func documentMetaTitle(document core.Document) string {
	if value := documentSEOSnippet(document.MetaTitle, 120); value != "" {
		return value
	}
	return documentSEOSnippet(document.Title, 120)
}

func documentMetaDescription(document core.Document) string {
	return documentSEOSnippet(document.MetaDescription, 180)
}

func documentSEOSnippet(value string, maxRunes int) string {
	value = strings.Join(strings.Fields(value), " ")
	return truncateRunes(value, maxRunes)
}

func documentMarkdownLinkText(value string) string {
	value = documentSEOSnippet(value, 160)
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `[`, `\[`)
	value = strings.ReplaceAll(value, `]`, `\]`)
	return value
}

func documentCanonicalURL(document core.Document, baseURL string) string {
	if value := strings.TrimSpace(document.CanonicalURL); value != "" && controlplane.DocumentSEOIndexable(document) {
		return value
	}
	return strings.TrimRight(baseURL, "/") + "/docs/" + document.Slug
}

func documentStructuredData(document core.Document, baseURL string) template.JS {
	payload := map[string]any{
		"@context":     "https://schema.org",
		"@type":        "TechArticle",
		"headline":     documentMetaTitle(document),
		"url":          documentCanonicalURL(document, baseURL),
		"dateModified": document.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if description := documentMetaDescription(document); description != "" {
		payload["description"] = description
	}
	if document.PublishedAt != nil && !document.PublishedAt.IsZero() {
		payload["datePublished"] = document.PublishedAt.UTC().Format(time.RFC3339)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return template.JS(raw)
}
