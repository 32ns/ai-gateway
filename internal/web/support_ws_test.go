package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestSupportEventJSONUsesFrontendFieldNames(t *testing.T) {
	now := time.Date(2026, 6, 14, 10, 11, 12, 0, time.UTC)
	body := supportEventJSON(supportEvent{
		Type: "support.message.created",
		Ticket: &core.SupportTicket{
			ID:          "sup_1",
			UserID:      "user_1",
			Username:    "alice",
			Title:       "在线客服",
			Status:      core.SupportTicketPendingAdmin,
			LastMessage: "hello",
			UpdatedAt:   now,
		},
		Message: &core.SupportMessage{
			ID:        "smsg_1",
			TicketID:  "sup_1",
			ActorID:   "user_1",
			ActorRole: core.UserRoleUser,
			Body:      "hello",
			CreatedAt: now,
		},
	})

	for _, want := range []string{`"id":"sup_1"`, `"user_id":"user_1"`, `"last_message":"hello"`, `"actor_role":"user"`, `"created_at":"2026-06-14T10:11:12Z"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("support event JSON missing %s in %s", want, body)
		}
	}
	for _, unwanted := range []string{`"ID":"sup_1"`, `"ActorRole":"user"`, `"CreatedAt":"2026-06-14T10:11:12Z"`} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("support event JSON must not expose Go field name %s in %s", unwanted, body)
		}
	}
}

func TestSupportErrorFromErrorUsesStableCodes(t *testing.T) {
	notFound := supportErrorFromError("req_1", storage.ErrNotFound)
	if notFound.ErrorCode != "not_found" || strings.Contains(strings.ToLower(notFound.Error), "not found") {
		t.Fatalf("not found support error = %#v", notFound)
	}

	conflict := supportErrorFromError("req_2", errors.New("constraint failed: FOREIGN KEY constraint failed (787)"))
	if conflict.ErrorCode != "data_conflict" || strings.Contains(conflict.Error, "FOREIGN KEY") {
		t.Fatalf("foreign key support error = %#v", conflict)
	}
}

func TestSupportTicketDeletedEventJSONUsesTicketAndUnread(t *testing.T) {
	body := supportEventJSON(supportEvent{
		Type: "support.ticket.deleted",
		Ticket: &core.SupportTicket{
			ID:     "sup_deleted",
			UserID: "user_deleted",
			Title:  "在线客服",
		},
		Unread: 3,
	})

	for _, want := range []string{`"type":"support.ticket.deleted"`, `"id":"sup_deleted"`, `"user_id":"user_deleted"`, `"unread":3`} {
		if !strings.Contains(body, want) {
			t.Fatalf("deleted support event JSON missing %s in %s", want, body)
		}
	}
}

func TestSupportNotificationServiceWorkerRoute(t *testing.T) {
	server := NewServerWithOptions(controlplane.New(storage.NewMemoryRepository(), providers.NewRegistry()), nil, ServerOptions{})
	req := httptest.NewRequest(http.MethodGet, "/support-notification-sw.js", nil)
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("service worker status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/javascript") {
		t.Fatalf("service worker content type = %q", got)
	}
	if got := rec.Header().Get("Service-Worker-Allowed"); got != "/" {
		t.Fatalf("service worker allowed scope = %q", got)
	}
	body := rec.Body.String()
	for _, want := range []string{`skipWaiting`, `clients.claim`, `notificationclick`, `clients.matchAll`, `clients.openWindow`} {
		if !strings.Contains(body, want) {
			t.Fatalf("service worker script missing %q in %s", want, body)
		}
	}
}

func TestAdminSupportPageRendersUnreadBadges(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry())
	user := core.User{ID: "user_support_page_unread", Username: "alice", Role: core.UserRoleUser, Enabled: true}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	if _, _, err := control.EnsureAdminUser("admin", "admin-secret"); err != nil {
		t.Fatalf("EnsureAdminUser: %v", err)
	}
	if _, _, err := control.CreateSupportTicket(user, controlplane.SupportTicketInput{Title: "在线客服", Body: "hello"}); err != nil {
		t.Fatalf("CreateSupportTicket: %v", err)
	}

	server := NewServerWithOptions(control, nil, ServerOptions{})
	handler := authenticatedAdminHandler(t, control, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/admin/support", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin support status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-nav-support-link`) || !strings.Contains(body, `<span class="nav-badge">1</span>`) {
		t.Fatalf("admin support nav unread badge missing: %s", body)
	}
	if !strings.Contains(body, `support-ticket-unread">1</span>`) {
		t.Fatalf("admin support ticket unread badge missing: %s", body)
	}
	if strings.Contains(body, `method="post" action="/admin/support/`) || strings.Contains(body, `data-support-csrf`) {
		t.Fatalf("admin support page must not render legacy POST delete form: %s", body)
	}
	if !strings.Contains(body, `data-support-ticket-delete`) {
		t.Fatalf("admin support page must render websocket delete button: %s", body)
	}
}

func TestDeleteSupportTicketRemovesStoredMessages(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry())
	user := core.User{ID: "user_support_delete", Username: "alice", Role: core.UserRoleUser, Enabled: true}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser: %v", err)
	}
	ticket, _, err := control.CreateSupportTicket(user, controlplane.SupportTicketInput{Title: "在线客服", Body: "hello"})
	if err != nil {
		t.Fatalf("CreateSupportTicket: %v", err)
	}
	if _, _, err := control.ReplySupportTicket(user, ticket.ID, controlplane.SupportReplyInput{Body: "again"}); err != nil {
		t.Fatalf("ReplySupportTicket: %v", err)
	}

	deleted, err := control.DeleteSupportTicket(admin, ticket.ID)
	if err != nil {
		t.Fatalf("DeleteSupportTicket: %v", err)
	}
	if deleted.ID != ticket.ID || deleted.UserID != user.ID {
		t.Fatalf("deleted ticket context = %#v, want id=%s user=%s", deleted, ticket.ID, user.ID)
	}
	if _, err := control.GetSupportTicket(admin, ticket.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("deleted ticket lookup error = %v, want not found", err)
	}
	if messages := repo.ListSupportMessages(ticket.ID, 20); len(messages) != 0 {
		t.Fatalf("deleted ticket messages remain: %#v", messages)
	}
}
