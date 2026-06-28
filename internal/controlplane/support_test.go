package controlplane

import (
	"testing"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestSupportUnreadCountsForAdminUntilExplicitRead(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry())
	user := core.User{ID: "user_support_unread", Username: "alice", Role: core.UserRoleUser, Enabled: true}
	admin := core.User{ID: "admin_support_unread", Username: "admin", Role: core.UserRoleAdmin, Enabled: true}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatalf("UpsertUser user: %v", err)
	}
	if err := repo.UpsertUser(admin); err != nil {
		t.Fatalf("UpsertUser admin: %v", err)
	}

	ticket, _, err := service.CreateSupportTicket(user, SupportTicketInput{Title: "在线客服", Body: "hello"})
	if err != nil {
		t.Fatalf("CreateSupportTicket: %v", err)
	}
	if got := service.SupportUnreadCount(admin); got != 1 {
		t.Fatalf("admin unread after user message = %d, want 1", got)
	}
	page := service.ListSupportTickets(admin, SupportTicketFilter{Page: 1, PageSize: 10})
	if len(page.Tickets) != 1 || page.Tickets[0].UnreadCount != 1 {
		t.Fatalf("admin ticket unread page = %#v, want one unread ticket", page.Tickets)
	}

	if _, err := service.MarkSupportTicketRead(admin, ticket.ID); err != nil {
		t.Fatalf("MarkSupportTicketRead admin: %v", err)
	}
	if got := service.SupportUnreadCount(admin); got != 0 {
		t.Fatalf("admin unread after explicit read = %d, want 0", got)
	}
}
