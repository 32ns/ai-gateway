package web

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
)

func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/docs" && !strings.HasPrefix(r.URL.Path, "/docs/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/docs/" {
		http.Redirect(w, r, "/docs", http.StatusMovedPermanently)
		return
	}
	r, user := s.withOptionalConsoleUser(w, r)
	if r.URL.Path == "/docs" {
		s.handleDocsIndex(w, r, user)
		return
	}
	slug := strings.TrimPrefix(r.URL.Path, "/docs/")
	if strings.HasSuffix(slug, ".md") {
		s.handleDocumentMarkdown(w, r, strings.TrimSuffix(slug, ".md"))
		return
	}
	s.handleDocumentDetail(w, r, user, slug)
}

func (s *Server) handleDocsIndex(w http.ResponseWriter, r *http.Request, user core.User) {
	locale := resolveLocale(w, r)
	baseURL := s.publicDocsBaseURL(r)
	robots := "index,follow"
	if strings.TrimSpace(user.ID) != "" {
		robots = "noindex,nofollow"
		w.Header().Set("X-Robots-Tag", "noindex")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=300")
	}
	data := withCSRFData(map[string]any{
		"TitleKey":        "page_title_docs",
		"PageTitle":       translate(locale, "page_title_docs"),
		"MetaDescription": translate(locale, "docs_meta_description"),
		"CanonicalURL":    baseURL + "/docs",
		"Robots":          robots,
		"ActiveNav":       "docs",
		"Locale":          locale,
		"Document":        core.Document{},
		"Documents":       s.control.ListVisibleDocuments(user),
		"BaseURL":         baseURL,
	}, r)
	s.render(w, "docs.html", locale, data)
}

func (s *Server) handleDocumentDetail(w http.ResponseWriter, r *http.Request, user core.User, rawSlug string) {
	slug, err := controlplane.NormalizeDocumentSlug(rawSlug)
	if err != nil {
		w.Header().Set("X-Robots-Tag", "noindex")
		http.NotFound(w, r)
		return
	}
	document, err := s.control.GetDocumentForUser(slug, user)
	if err != nil {
		if redirect, redirectErr := s.control.GetDocumentRedirect(slug); redirectErr == nil && strings.TrimSpace(redirect.ToSlug) != "" {
			if _, targetErr := s.control.GetDocumentForUser(redirect.ToSlug, user); targetErr != nil {
				w.Header().Set("X-Robots-Tag", "noindex")
				http.NotFound(w, r)
				return
			}
			status := redirect.StatusCode
			if status < http.StatusMultipleChoices || status > http.StatusPermanentRedirect {
				status = http.StatusMovedPermanently
			}
			http.Redirect(w, r, "/docs/"+redirect.ToSlug, status)
			return
		}
		w.Header().Set("X-Robots-Tag", "noindex")
		http.NotFound(w, r)
		return
	}
	if slug != document.Slug || rawSlug != document.Slug {
		http.Redirect(w, r, "/docs/"+document.Slug, http.StatusMovedPermanently)
		return
	}
	locale := resolveLocale(w, r)
	baseURL := s.publicDocsBaseURL(r)
	robots := "index,follow"
	if controlplane.DocumentRobotsNoIndex(document) || strings.TrimSpace(user.ID) != "" {
		robots = "noindex,nofollow"
		w.Header().Set("X-Robots-Tag", "noindex")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=300")
	}
	data := withCSRFData(map[string]any{
		"TitleKey":           "page_title_docs",
		"PageTitle":          documentMetaTitle(document),
		"MetaDescription":    documentMetaDescription(document),
		"CanonicalURL":       documentCanonicalURL(document, baseURL),
		"Robots":             robots,
		"StructuredDataJSON": documentStructuredData(document, baseURL),
		"ActiveNav":          "docs",
		"Locale":             locale,
		"Document":           document,
		"CanPublishDocument": user.IsAdmin() && document.Status == core.DocumentStatusDraft,
		"DocumentHTML":       renderDocumentMarkdown(document.Body),
		"BaseURL":            baseURL,
	}, r)
	s.render(w, "docs.html", locale, data)
}

func (s *Server) handleDocumentMarkdown(w http.ResponseWriter, r *http.Request, rawSlug string) {
	slug, err := controlplane.NormalizeDocumentSlug(rawSlug)
	if err != nil {
		w.Header().Set("X-Robots-Tag", "noindex")
		http.NotFound(w, r)
		return
	}
	document, err := s.control.GetDocumentForUser(slug, core.User{})
	if err != nil || !controlplane.DocumentSEOIndexable(document) {
		w.Header().Set("X-Robots-Tag", "noindex")
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if r.Method == http.MethodHead {
		return
	}
	var builder strings.Builder
	builder.WriteString("# ")
	builder.WriteString(documentSEOSnippet(document.Title, 160))
	builder.WriteString("\n\n")
	builder.WriteString(strings.TrimSpace(document.Body))
	builder.WriteString("\n")
	_, _ = w.Write([]byte(builder.String()))
}

func (s *Server) handleAdminDocsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/docs" {
		http.NotFound(w, r)
		return
	}
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.renderAdminDocs(w, r, user)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) renderAdminDocs(w http.ResponseWriter, r *http.Request, user core.User) {
	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":  "page_title_docs_admin",
		"ActiveNav": "docs",
		"Locale":    locale,
		"Documents": s.control.ListDocuments(user),
		"BaseURL":   s.publicDocsBaseURL(r),
	}, r)
	s.render(w, "docs_admin.html", locale, data)
}

func (s *Server) handleAdminDocActions(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/docs/"), "/")
	if path == "new" {
		switch r.Method {
		case http.MethodGet:
			s.renderAdminDocForm(w, r, core.Document{Status: core.DocumentStatusDraft}, "create")
		case http.MethodPost:
			s.handleAdminDocCreateSubmit(w, r, user)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	switch parts[1] {
	case "edit":
		switch r.Method {
		case http.MethodGet:
			document, err := s.control.GetDocument(user, parts[0])
			if err != nil {
				http.NotFound(w, r)
				return
			}
			s.renderAdminDocForm(w, r, document, "edit")
		case http.MethodPost:
			s.handleAdminDocEditSubmit(w, r, user, parts[0])
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case "delete":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		document, err := s.control.DeleteDocument(user, parts[0])
		if err != nil {
			s.recordAdminAudit(r, "error", "document.delete", "document", parts[0], "", err.Error())
			s.redirectWithNoticeError(w, r, "/admin/docs", err)
			return
		}
		s.recordAdminAudit(r, "ok", "document.delete", "document", document.ID, document.Title, "")
		http.Redirect(w, r, "/admin/docs", http.StatusSeeOther)
	case "publish":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleAdminDocPublishSubmit(w, r, user, parts[0])
	default:
		http.NotFound(w, r)
		return
	}
}

func (s *Server) renderAdminDocForm(w http.ResponseWriter, r *http.Request, document core.Document, mode string) {
	locale := resolveLocale(w, r)
	formAction := "/admin/docs/new"
	if mode == "edit" {
		formAction = "/admin/docs/" + document.ID + "/edit"
	}
	data := withCSRFData(map[string]any{
		"TitleKey":   "page_title_docs_admin",
		"ActiveNav":  "docs",
		"Locale":     locale,
		"Mode":       mode,
		"IsEdit":     mode == "edit",
		"Document":   document,
		"FormAction": formAction,
	}, r)
	s.render(w, "docs_admin_form.html", locale, data)
}

func (s *Server) handleAdminDocCreateSubmit(w http.ResponseWriter, r *http.Request, user core.User) {
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, "/admin/docs/new", err)
		return
	}
	document, err := s.control.CreateDocument(user, documentInputFromForm(r))
	if err != nil {
		s.recordAdminAudit(r, "error", "document.create", "document", "", strings.TrimSpace(r.FormValue("title")), err.Error())
		s.redirectWithNoticeError(w, r, "/admin/docs/new", err)
		return
	}
	s.recordAdminAudit(r, "ok", "document.create", "document", document.ID, document.Title, auditDocumentUpdateChangeMessage(core.Document{}, document, false))
	http.Redirect(w, r, "/admin/docs", http.StatusSeeOther)
}

func (s *Server) handleAdminDocEditSubmit(w http.ResponseWriter, r *http.Request, user core.User, documentID string) {
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, "/admin/docs/"+documentID+"/edit", err)
		return
	}
	before, beforeErr := s.control.GetDocument(user, documentID)
	hasBefore := beforeErr == nil
	document, err := s.control.UpdateDocument(user, documentID, documentInputFromForm(r))
	if err != nil {
		s.recordAdminAudit(r, "error", "document.update", "document", documentID, strings.TrimSpace(r.FormValue("title")), err.Error())
		s.redirectWithNoticeError(w, r, "/admin/docs/"+documentID+"/edit", err)
		return
	}
	s.recordAdminAudit(r, "ok", "document.update", "document", document.ID, document.Title, auditDocumentUpdateChangeMessage(before, document, hasBefore))
	http.Redirect(w, r, "/admin/docs", http.StatusSeeOther)
}

func (s *Server) handleAdminDocPublishSubmit(w http.ResponseWriter, r *http.Request, user core.User, documentID string) {
	before, err := s.control.GetDocument(user, documentID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	document, err := s.control.UpdateDocument(user, documentID, documentInputFromDocument(before, core.DocumentStatusPublished))
	if err != nil {
		s.recordAdminAudit(r, "error", "document.update", "document", documentID, before.Title, err.Error())
		s.redirectWithNoticeError(w, r, "/docs/"+before.Slug, err)
		return
	}
	s.recordAdminAudit(r, "ok", "document.update", "document", document.ID, document.Title, auditDocumentUpdateChangeMessage(before, document, true))
	http.Redirect(w, r, "/docs/"+document.Slug, http.StatusSeeOther)
}

func (s *Server) handleRobots(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/robots.txt" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	baseURL := s.publicDocsBaseURL(r)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if r.Method == http.MethodHead {
		return
	}
	body := strings.Join([]string{
		"User-agent: *",
		"Allow: /docs",
		"Allow: /docs/",
		"Disallow: /admin/",
		"Disallow: /clients",
		"Disallow: /logs",
		"Disallow: /messages",
		"Disallow: /payments",
		"Disallow: /profile/",
		"Disallow: /login",
		"Disallow: /register",
		"",
		"IndexNow: " + baseURL + "/" + indexNowKey + ".txt",
		"",
		"Sitemap: " + baseURL + "/sitemap.xml",
		"Sitemap: " + baseURL + "/sitemap-docs.xml",
		"",
	}, "\n")
	_, _ = w.Write([]byte(body))
}

const indexNowKey = "e0d6c52cc11d4f3e8eb3d067f89ddf1f"

func (s *Server) handleIndexNowKey(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/"+indexNowKey+".txt" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte(indexNowKey))
}

func (s *Server) handleSitemap(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/sitemap.xml" && r.URL.Path != "/sitemap-docs.xml" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	baseURL := s.publicDocsBaseURL(r)
	documents := s.control.ListPublicDocumentsForSEO()
	urls := []sitemapURL{{Loc: baseURL + "/docs", ChangeFreq: "weekly", Priority: "0.7"}}
	if r.URL.Path == "/sitemap.xml" {
		urls = append([]sitemapURL{{Loc: baseURL + "/", ChangeFreq: "weekly", Priority: "0.6"}}, urls...)
	}
	for _, document := range documents {
		urls = append(urls, sitemapURL{
			Loc:        baseURL + "/docs/" + document.Slug,
			LastMod:    sitemapTime(document.UpdatedAt),
			ChangeFreq: "weekly",
			Priority:   "0.8",
		})
	}
	payload := sitemapURLSet{
		Xmlns: "http://www.sitemaps.org/schemas/sitemap/0.9",
		URLs:  urls,
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(payload)
}

func (s *Server) handleLLMSText(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/llms.txt" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	baseURL := s.publicDocsBaseURL(r)
	documents := s.control.ListPublicDocumentsForSEO()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if r.Method == http.MethodHead {
		return
	}
	var builder strings.Builder
	builder.WriteString("# AI Gateway Documentation\n\n")
	builder.WriteString("> Public documentation index for search engines and AI assistants.\n\n")
	builder.WriteString("Base URL: ")
	builder.WriteString(baseURL)
	builder.WriteString("\n\n")
	builder.WriteString("## Docs\n\n")
	for _, document := range documents {
		builder.WriteString("- [")
		builder.WriteString(documentMarkdownLinkText(document.Title))
		builder.WriteString("](")
		builder.WriteString(baseURL + "/docs/" + document.Slug)
		builder.WriteString(")")
		builder.WriteString("\n")
	}
	_, _ = w.Write([]byte(builder.String()))
}

type sitemapURLSet struct {
	XMLName xml.Name     `xml:"urlset"`
	Xmlns   string       `xml:"xmlns,attr"`
	URLs    []sitemapURL `xml:"url"`
}

type sitemapURL struct {
	Loc        string `xml:"loc"`
	LastMod    string `xml:"lastmod,omitempty"`
	ChangeFreq string `xml:"changefreq,omitempty"`
	Priority   string `xml:"priority,omitempty"`
}

func sitemapTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format("2006-01-02")
}

func (s *Server) publicDocsBaseURL(r *http.Request) string {
	baseURL := ""
	if s != nil && s.control != nil {
		baseURL = strings.TrimRight(strings.TrimSpace(s.control.PublicBaseURL()), "/")
	}
	if baseURL == "" {
		baseURL = strings.TrimRight(requestBaseURL(r), "/")
	}
	return baseURL
}

func (s *Server) withOptionalConsoleUser(w http.ResponseWriter, r *http.Request) (*http.Request, core.User) {
	if user, ok := currentUserFromContext(r.Context()); ok {
		return r, user
	}
	if s == nil || s.control == nil {
		return r, core.User{}
	}
	user, err := s.currentUserFromSession(r)
	if err != nil || user.ID == "" {
		return r, core.User{}
	}
	csrfToken, err := s.ensureConsoleCSRFToken(w, r)
	if err != nil {
		return r, core.User{}
	}
	ctx := withConsoleCSRFToken(r.Context(), csrfToken)
	ctx = withConsoleUser(ctx, user)
	ctx = withSiteMessageUnreadCount(ctx, s.control.SiteMessageUnreadCount(user))
	ctx = withUnreadPopupSiteMessages(ctx, s.control.ListUnreadPopupSiteMessages(user, 5))
	return r.WithContext(ctx), user
}

func documentInputFromForm(r *http.Request) controlplane.DocumentInput {
	return controlplane.DocumentInput{
		Slug:            r.FormValue("slug"),
		Title:           r.FormValue("title"),
		Body:            r.FormValue("body"),
		MetaTitle:       r.FormValue("meta_title"),
		MetaDescription: r.FormValue("meta_description"),
		CanonicalURL:    r.FormValue("canonical_url"),
		Pinned:          r.FormValue("pinned") != "",
		NoIndex:         r.FormValue("noindex") != "",
		Status:          core.DocumentStatus(r.FormValue("status")),
	}
}

func documentInputFromDocument(document core.Document, status core.DocumentStatus) controlplane.DocumentInput {
	return controlplane.DocumentInput{
		Slug:            document.Slug,
		Title:           document.Title,
		Body:            document.Body,
		MetaTitle:       document.MetaTitle,
		MetaDescription: document.MetaDescription,
		CanonicalURL:    document.CanonicalURL,
		Pinned:          document.Pinned,
		NoIndex:         document.NoIndex,
		Status:          status,
	}
}

func auditDocumentUpdateChangeMessage(before, after core.Document, hasBefore bool) string {
	if !hasBefore {
		return auditFieldsMessage(
			auditMessageField{Key: "slug", Value: after.Slug},
			auditMessageField{Key: "status", Value: string(after.Status)},
			auditMessageField{Key: "pinned", Value: strconv.FormatBool(after.Pinned)},
			auditMessageField{Key: "noindex", Value: strconv.FormatBool(after.NoIndex)},
		)
	}
	return auditChangeMessage(
		auditTextChange("title", before.Title, after.Title),
		auditTextChange("slug", before.Slug, after.Slug),
		auditTextChange("body", before.Body, after.Body),
		auditTextChange("status", string(before.Status), string(after.Status)),
		auditBoolChange("pinned", before.Pinned, after.Pinned),
		auditBoolChange("noindex", before.NoIndex, after.NoIndex),
	)
}
