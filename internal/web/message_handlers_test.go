package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestMessagesPageCreateAndMarkRead(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "alice",
		Password: "alice-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	adminHandler := authenticatedAdminHandler(t, control, server.Handler())

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("title", "System notice")
	form.Set("body", "Maintenance tonight")
	form.Set("enabled", "on")
	req := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create message status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if events := repo.ListAudit(10); len(events) == 0 || events[0].Action != "site_message.create" || events[0].Actor != admin.Username {
		t.Fatalf("audit events = %#v", events)
	}
	messages := repo.ListSiteMessages()
	if len(messages) != 1 {
		t.Fatalf("messages = %#v, want one", messages)
	}

	userHandler := authenticatedUserHandler(t, control, user, server.Handler())
	rec = httptest.NewRecorder()
	userHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/messages", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("messages page status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "System notice") || !strings.Contains(body, "Maintenance tonight") || !strings.Contains(body, `class="nav-badge">1`) {
		t.Fatalf("messages page missing expected content: %s", body)
	}

	readForm := url.Values{}
	readForm.Set("csrf_token", testConsoleCSRFToken)
	req = httptest.NewRequest(http.MethodPost, "/messages/"+messages[0].ID+"/read", strings.NewReader(readForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	userHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("mark read status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if got := control.SiteMessageUnreadCount(user); got != 0 {
		t.Fatalf("unread count after mark read = %d, want 0", got)
	}

	deleteForm := url.Values{}
	deleteForm.Set("csrf_token", testConsoleCSRFToken)
	req = httptest.NewRequest(http.MethodPost, "/messages/"+messages[0].ID+"/delete", strings.NewReader(deleteForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete message status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if messages := repo.ListSiteMessages(); len(messages) != 0 {
		t.Fatalf("messages after delete = %#v, want none", messages)
	}
	if events := repo.ListAudit(10); len(events) == 0 || events[0].Action != "site_message.delete" {
		t.Fatalf("audit events after delete = %#v", events)
	}
}

func TestMessagesPopupRendersUntilRead(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	_, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "popup-user",
		Password: "popup-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	adminHandler := authenticatedAdminHandler(t, control, server.Handler())

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("title", "Popup notice")
	form.Set("body", "Read this first")
	form.Set("enabled", "on")
	form.Set("popup", "on")
	req := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create message status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	messages := repo.ListSiteMessages()
	if len(messages) != 1 || !messages[0].Popup {
		t.Fatalf("messages = %#v, want popup message", messages)
	}

	userHandler := authenticatedUserHandler(t, control, user, server.Handler())
	rec = httptest.NewRecorder()
	userHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-site-message-popup-overlay`) ||
		!strings.Contains(body, `data-site-message-popup-time`) ||
		!strings.Contains(body, `data-site-message-popup-item-time`) ||
		!strings.Contains(body, "Popup notice") ||
		!strings.Contains(body, `/messages/`+messages[0].ID+`/read`) {
		t.Fatalf("dashboard missing popup message: %s", body)
	}

	readForm := url.Values{}
	readForm.Set("csrf_token", testConsoleCSRFToken)
	req = httptest.NewRequest(http.MethodPost, "/messages/"+messages[0].ID+"/read", strings.NewReader(readForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	userHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("mark read status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	userHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard after read status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `data-site-message-popup-overlay`) {
		t.Fatalf("read popup message should not render again: %s", rec.Body.String())
	}
}

func TestPublicPopupMessageRendersForAnonymousHome(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	message, err := control.CreateSiteMessage(admin, controlplane.SiteMessageInput{
		Title:       "Website notice",
		Body:        "Shown before login",
		Enabled:     true,
		PublicPopup: true,
	})
	if err != nil {
		t.Fatalf("CreateSiteMessage returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("home status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-site-message-popup-overlay`) ||
		!strings.Contains(body, `data-read-mode="browser"`) ||
		!strings.Contains(body, `data-site-message-popup-item-time`) ||
		!strings.Contains(body, siteMessageBrowserReadKey(message)) ||
		!strings.Contains(body, "Website notice") ||
		!strings.Contains(body, "Shown before login") {
		t.Fatalf("home missing public popup message: %s", body)
	}
	if strings.Contains(body, `/messages/`+message.ID+`/read`) {
		t.Fatalf("public popup should not use server read endpoint: %s", body)
	}

	user, err := control.CreateUser(controlplane.UserInput{Username: "public-popup-user", Password: "popup-secret", Role: core.UserRoleUser, Enabled: true})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	userHandler := authenticatedUserHandler(t, control, user, server.Handler())
	rec = httptest.NewRecorder()
	userHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/messages", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("messages status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body = rec.Body.String()
	if strings.Contains(body, `<article class="message-item`) || strings.Contains(body, `class="nav-badge"`) {
		t.Fatalf("public popup should not appear in regular user inbox or unread badge: %s", body)
	}
}

func TestMessagesPageCreatesWebsiteMessageFromTargetMode(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	adminHandler := authenticatedAdminHandler(t, control, server.Handler())

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("title", "Website target")
	form.Set("body", "Shown to visitors")
	form.Set("enabled", "on")
	form.Set("target_mode", "website")
	form.Set("popup", "on")
	req := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create website message status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	messages := repo.ListSiteMessages()
	if len(messages) != 1 {
		t.Fatalf("messages = %#v, want one", messages)
	}
	if !messages[0].PublicPopup {
		t.Fatalf("message = %#v, want public website popup", messages[0])
	}
	if messages[0].Popup {
		t.Fatalf("message = %#v, website message must not use unread popup reminder", messages[0])
	}
}

func TestMessagesPageRendersMarkdownSafely(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "markdown-user",
		Password: "markdown-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if _, err := control.CreateSiteMessage(admin, controlplane.SiteMessageInput{
		Title:   "Markdown",
		Body:    "**Bold** [Link](https://example.com) <script>alert(1)</script>",
		Enabled: true,
	}); err != nil {
		t.Fatalf("CreateSiteMessage returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/messages", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<strong>Bold</strong>") || !strings.Contains(body, `href="https://example.com"`) {
		t.Fatalf("markdown formatting missing: %s", body)
	}
	if strings.Contains(body, "<script>") || strings.Contains(body, "alert(1)") {
		t.Fatalf("unsafe markdown content rendered: %s", body)
	}
}

func TestMessagesUserSearchRequiresAdminAndFilters(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "target-search",
		Password: "target-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser target returned error: %v", err)
	}
	other, err := control.CreateUser(controlplane.UserInput{
		Username: "plain-user",
		Password: "plain-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser other returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")

	adminHandler := authenticatedAdminHandler(t, control, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/messages/users/search?q=target", nil)
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	adminHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin search status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, user.ID) || !strings.Contains(body, "target-search") {
		t.Fatalf("search response missing target user: %s", body)
	}
	if strings.Contains(body, other.ID) || strings.Contains(body, "plain-user") {
		t.Fatalf("search response included unrelated user: %s", body)
	}

	userHandler := authenticatedUserHandler(t, control, other, server.Handler())
	req = httptest.NewRequest(http.MethodGet, "/messages/users/search?q=target", nil)
	req.Header.Set("Accept", "application/json")
	rec = httptest.NewRecorder()
	userHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("regular user search status = %d, want %d body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
}

func TestMessagesCreateErrorRedirectsWithNotice(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("title", "")
	form.Set("body", "Missing title")
	req := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/messages?") || !strings.Contains(location, "notice_error=") {
		t.Fatalf("location = %q, want messages notice redirect", location)
	}
	noticeReq := httptest.NewRequest(http.MethodGet, location, nil)
	noticeRec := httptest.NewRecorder()
	handler.ServeHTTP(noticeRec, noticeReq)
	if noticeRec.Code != http.StatusOK {
		t.Fatalf("notice status = %d, want %d", noticeRec.Code, http.StatusOK)
	}
	body := noticeRec.Body.String()
	if !strings.Contains(body, `data-clear-url-params="notice_error"`) || !strings.Contains(body, "message title and body are required") {
		t.Fatalf("notice missing create error: %s", body)
	}
}

func TestMessagesPagePaginatesMessages(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := range 30 {
		if err := repo.CreateSiteMessage(core.SiteMessage{
			ID:        fmt.Sprintf("msg_page_%02d", i),
			Title:     fmt.Sprintf("Message %02d", i),
			Body:      "Body",
			CreatedBy: admin.ID,
			Enabled:   true,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
			UpdatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("CreateSiteMessage(%d) returned error: %v", i, err)
		}
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/messages?page=2", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Message 04") || !strings.Contains(body, "Message 00") {
		t.Fatalf("second page missing expected messages: %s", body)
	}
	if strings.Contains(body, "Message 29") {
		t.Fatalf("second page should not render first page messages: %s", body)
	}
	if !strings.Contains(body, `/messages?page=1`) {
		t.Fatalf("previous page link missing: %s", body)
	}
}

func TestMessagesPageAdminUsesPagedDeliveries(t *testing.T) {
	baseRepo := storage.NewMemoryRepository()
	admin := core.User{ID: "admin", Username: "admin", Role: core.UserRoleAdmin, Enabled: true}
	if err := baseRepo.UpsertUser(admin); err != nil {
		t.Fatal(err)
	}
	baseTime := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := range 30 {
		if err := baseRepo.CreateSiteMessage(core.SiteMessage{
			ID:        fmt.Sprintf("msg_admin_page_%02d", i),
			Title:     fmt.Sprintf("Admin Message %02d", i),
			Body:      "Body",
			CreatedBy: admin.ID,
			Enabled:   true,
			CreatedAt: baseTime.Add(time.Duration(i) * time.Minute),
			UpdatedAt: baseTime.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("CreateSiteMessage(%d) returned error: %v", i, err)
		}
	}

	repo := &messageFullSiteMessageListPanicRepository{MemoryRepository: baseRepo}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, admin, server.Handler())

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/messages?page=2", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if repo.deliveryPageCalls == 0 {
		t.Fatal("messages page did not use ListSiteMessageDeliveriesPage")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Admin Message 04") || strings.Contains(body, "Admin Message 29") {
		t.Fatalf("messages page did not render only the requested page: %s", body)
	}
}

func TestMessagesUpdateAuditAvoidsFullMessageList(t *testing.T) {
	base := storage.NewMemoryRepository()
	admin := core.User{ID: "admin", Username: "admin", Role: core.UserRoleAdmin, Enabled: true}
	if err := base.UpsertUser(admin); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := base.CreateSiteMessage(core.SiteMessage{
		ID:        "msg_update_no_full_list",
		Title:     "Before",
		Body:      "Before body",
		CreatedBy: admin.ID,
		Enabled:   true,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	repo := &messageFullSiteMessageListPanicRepository{MemoryRepository: base}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, admin, server.Handler())

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("title", "After")
	form.Set("body", "After body")
	form.Set("enabled", "on")
	req := httptest.NewRequest(http.MethodPost, "/messages/msg_update_no_full_list/update", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	updated, err := base.GetSiteMessage("msg_update_no_full_list")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Title != "After" || updated.Body != "After body" {
		t.Fatalf("updated message = %#v", updated)
	}
}

func TestMessagesPageAvoidsFullUserListForTargetPicker(t *testing.T) {
	base := storage.NewMemoryRepository()
	if err := base.UpsertUser(core.User{ID: "admin", Username: "admin", Role: core.UserRoleAdmin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for i := range 120 {
		if err := base.UpsertUser(core.User{
			ID:       fmt.Sprintf("user_picker_%03d", i),
			Username: fmt.Sprintf("picker-%03d", i),
			Role:     core.UserRoleUser,
			Enabled:  true,
		}); err != nil {
			t.Fatalf("UpsertUser(%d) returned error: %v", i, err)
		}
	}
	target := core.User{ID: "user_picker_target", Username: "zz-target", Role: core.UserRoleUser, Enabled: true}
	if err := base.UpsertUser(target); err != nil {
		t.Fatal(err)
	}
	if err := base.CreateSiteMessage(core.SiteMessage{
		ID:            "msg_target",
		Title:         "Targeted",
		Body:          "Target body",
		CreatedBy:     "admin",
		Enabled:       true,
		TargetUserIDs: []string{target.ID},
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	repo := &messageFullUserListPanicRepository{MemoryRepository: base}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, err := control.GetUser("admin")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, admin, server.Handler())

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/messages", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Targeted") || !strings.Contains(body, "zz-target") {
		t.Fatalf("messages page missing selected target user: %s", body)
	}
}

type messageFullUserListPanicRepository struct {
	*storage.MemoryRepository
}

func (r *messageFullUserListPanicRepository) ListUsers() []core.User {
	panic("messages page should use ListUsersPage instead of full ListUsers")
}

type messageFullSiteMessageListPanicRepository struct {
	*storage.MemoryRepository
	deliveryPageCalls int
}

func (r *messageFullSiteMessageListPanicRepository) ListSiteMessages() []core.SiteMessage {
	panic("messages page should not use full ListSiteMessages")
}

func (r *messageFullSiteMessageListPanicRepository) ListSiteMessageDeliveries(userID string, includeDisabled bool) []core.SiteMessageDelivery {
	panic("messages page should use ListSiteMessageDeliveriesPage instead of full ListSiteMessageDeliveries")
}

func (r *messageFullSiteMessageListPanicRepository) ListSiteMessageDeliveriesPage(userID string, includeDisabled bool, offset, limit int) ([]core.SiteMessageDelivery, int) {
	r.deliveryPageCalls++
	return r.MemoryRepository.ListSiteMessageDeliveriesPage(userID, includeDisabled, offset, limit)
}
