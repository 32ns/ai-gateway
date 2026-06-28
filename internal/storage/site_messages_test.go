package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestMemoryRepositorySiteMessageDeliveriesAndReads(t *testing.T) {
	repo := NewMemoryRepository()
	testSiteMessageDeliveriesAndReads(t, repo)
}

func TestSQLiteRepositorySiteMessageDeliveriesAndReads(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	testSiteMessageDeliveriesAndReads(t, repo)
}

func TestSQLiteRepositorySiteMessageDeliveriesPage(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.UpsertUser(core.User{ID: "user_msg_page", Username: "message-page", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	messages := []core.SiteMessage{
		{ID: "msg_page_old", Title: "Old", Body: "Old body", CreatedBy: "admin", Enabled: true, CreatedAt: base, UpdatedAt: base},
		{ID: "msg_page_read", Title: "Read", Body: "Read body", CreatedBy: "admin", Enabled: true, CreatedAt: base.Add(time.Minute), UpdatedAt: base.Add(time.Minute)},
		{ID: "msg_page_disabled", Title: "Disabled", Body: "Disabled body", CreatedBy: "admin", Enabled: false, CreatedAt: base.Add(2 * time.Minute), UpdatedAt: base.Add(2 * time.Minute)},
		{ID: "msg_page_new", Title: "New", Body: "New body", CreatedBy: "admin", Enabled: true, CreatedAt: base.Add(3 * time.Minute), UpdatedAt: base.Add(3 * time.Minute)},
	}
	for _, message := range messages {
		if err := repo.CreateSiteMessage(message); err != nil {
			t.Fatalf("CreateSiteMessage(%s) returned error: %v", message.ID, err)
		}
	}
	readAt := base.Add(4 * time.Minute)
	if err := repo.MarkSiteMessageRead("msg_page_read", "user_msg_page", readAt); err != nil {
		t.Fatalf("MarkSiteMessageRead returned error: %v", err)
	}

	page, total := repo.ListSiteMessageDeliveriesPage("user_msg_page", true, 1, 2)
	if total != 4 {
		t.Fatalf("include disabled total = %d, want 4", total)
	}
	if len(page) != 2 || page[0].Message.ID != "msg_page_disabled" || page[1].Message.ID != "msg_page_read" {
		t.Fatalf("include disabled page = %#v", page)
	}
	if !page[1].Read || page[1].ReadAt == nil || !page[1].ReadAt.Equal(readAt) {
		t.Fatalf("read delivery = %#v, want read timestamp %s", page[1], readAt)
	}

	page, total = repo.ListSiteMessageDeliveriesPage("user_msg_page", false, 1, 2)
	if total != 3 {
		t.Fatalf("enabled total = %d, want 3", total)
	}
	if len(page) != 2 || page[0].Message.ID != "msg_page_read" || page[1].Message.ID != "msg_page_old" {
		t.Fatalf("enabled page = %#v", page)
	}
}

func TestSQLiteRepositoryVisibleSiteMessageDeliveriesPage(t *testing.T) {
	repo, err := NewSQLiteRepository(filepath.Join(t.TempDir(), "state.db"), "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.UpsertUser(core.User{ID: "user_visible", Username: "visible-user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	messages := []core.SiteMessage{
		{ID: "msg_visible_all", Title: "All", Body: "All body", CreatedBy: "admin", Enabled: true, CreatedAt: base, UpdatedAt: base},
		{ID: "msg_visible_direct", Title: "Direct", Body: "Direct body", CreatedBy: "admin", Enabled: true, TargetUserIDs: []string{"user_visible"}, CreatedAt: base.Add(time.Minute), UpdatedAt: base.Add(time.Minute)},
		{ID: "msg_visible_group", Title: "Group", Body: "Group body", CreatedBy: "admin", Enabled: true, TargetAccountGroups: []string{"Plus"}, CreatedAt: base.Add(2 * time.Minute), UpdatedAt: base.Add(2 * time.Minute)},
		{ID: "msg_visible_other", Title: "Other", Body: "Other body", CreatedBy: "admin", Enabled: true, TargetUserIDs: []string{"other_user"}, CreatedAt: base.Add(3 * time.Minute), UpdatedAt: base.Add(3 * time.Minute)},
		{ID: "msg_visible_disabled", Title: "Disabled", Body: "Disabled body", CreatedBy: "admin", Enabled: false, CreatedAt: base.Add(4 * time.Minute), UpdatedAt: base.Add(4 * time.Minute)},
		{ID: "msg_visible_public_popup", Title: "Public popup", Body: "Public body", CreatedBy: "admin", Enabled: true, PublicPopup: true, CreatedAt: base.Add(5 * time.Minute), UpdatedAt: base.Add(5 * time.Minute)},
	}
	for _, message := range messages {
		if err := repo.CreateSiteMessage(message); err != nil {
			t.Fatalf("CreateSiteMessage(%s) returned error: %v", message.ID, err)
		}
	}
	readAt := base.Add(5 * time.Minute)
	if err := repo.MarkSiteMessageRead("msg_visible_direct", "user_visible", readAt); err != nil {
		t.Fatalf("MarkSiteMessageRead returned error: %v", err)
	}

	query := SiteMessageVisibilityQuery{UserID: "user_visible", AccountGroups: []string{"plus"}, Offset: 0, Limit: 10}
	page, total := repo.ListVisibleSiteMessageDeliveriesPage(query)
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(page) != 3 || page[0].Message.ID != "msg_visible_group" || page[1].Message.ID != "msg_visible_direct" || page[2].Message.ID != "msg_visible_all" {
		t.Fatalf("visible page = %#v", page)
	}
	if !page[1].Read || page[1].ReadAt == nil || !page[1].ReadAt.Equal(readAt) {
		t.Fatalf("read delivery = %#v, want timestamp %s", page[1], readAt)
	}
	if got := repo.VisibleSiteMessageUnreadCount(query); got != 2 {
		t.Fatalf("visible unread count = %d, want 2", got)
	}

	page, total = repo.ListVisibleSiteMessageDeliveriesPage(SiteMessageVisibilityQuery{UserID: "user_visible", Offset: 1, Limit: 1})
	if total != 2 {
		t.Fatalf("direct/all total = %d, want 2", total)
	}
	if len(page) != 1 || page[0].Message.ID != "msg_visible_all" {
		t.Fatalf("direct/all page = %#v", page)
	}
}

func testSiteMessageDeliveriesAndReads(t *testing.T, repo Repository) {
	t.Helper()
	if err := repo.UpsertUser(core.User{ID: "user_msg", Username: "message-user", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	enabled := core.SiteMessage{
		ID:        "msg_enabled",
		Title:     "Enabled notice",
		Body:      "Visible to users",
		CreatedBy: "admin",
		Enabled:   true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	disabled := core.SiteMessage{
		ID:        "msg_disabled",
		Title:     "Disabled notice",
		Body:      "Hidden from users",
		CreatedBy: "admin",
		Enabled:   false,
		CreatedAt: now.Add(time.Second),
		UpdatedAt: now.Add(time.Second),
	}
	if err := repo.CreateSiteMessage(enabled); err != nil {
		t.Fatalf("CreateSiteMessage(enabled) returned error: %v", err)
	}
	if err := repo.CreateSiteMessage(disabled); err != nil {
		t.Fatalf("CreateSiteMessage(disabled) returned error: %v", err)
	}
	if deliveries := repo.ListSiteMessageDeliveries("user_msg", false); len(deliveries) != 1 || deliveries[0].Message.ID != enabled.ID || deliveries[0].Read {
		t.Fatalf("user deliveries = %#v, want only unread enabled message", deliveries)
	}
	if deliveries := repo.ListSiteMessageDeliveries("user_msg", true); len(deliveries) != 2 || deliveries[0].Message.ID != disabled.ID {
		t.Fatalf("admin deliveries = %#v, want both messages sorted newest first", deliveries)
	}
	if got := repo.SiteMessageUnreadCount("user_msg"); got != 1 {
		t.Fatalf("unread count = %d, want 1", got)
	}
	if err := repo.MarkSiteMessageRead(enabled.ID, "user_msg", now.Add(2*time.Second)); err != nil {
		t.Fatalf("MarkSiteMessageRead returned error: %v", err)
	}
	deliveries := repo.ListSiteMessageDeliveries("user_msg", false)
	if len(deliveries) != 1 || !deliveries[0].Read || deliveries[0].ReadAt == nil {
		t.Fatalf("deliveries after read = %#v, want read timestamp", deliveries)
	}
	if got := repo.SiteMessageUnreadCount("user_msg"); got != 0 {
		t.Fatalf("unread count after read = %d, want 0", got)
	}
	if err := repo.DeleteSiteMessage(enabled.ID); err != nil {
		t.Fatalf("DeleteSiteMessage returned error: %v", err)
	}
	if _, err := repo.GetSiteMessage(enabled.ID); err == nil {
		t.Fatal("expected deleted site message to be missing")
	}
	if got := repo.SiteMessageUnreadCount("user_msg"); got != 0 {
		t.Fatalf("unread count after delete = %d, want 0", got)
	}

	enabled.Title = "Updated notice"
	enabled.Body = "Updated body"
	enabled.ID = disabled.ID
	if err := repo.UpdateSiteMessage(enabled); err != nil {
		t.Fatalf("UpdateSiteMessage returned error: %v", err)
	}
	updated, err := repo.GetSiteMessage(disabled.ID)
	if err != nil {
		t.Fatalf("GetSiteMessage returned error: %v", err)
	}
	if updated.Title != "Updated notice" || updated.Body != "Updated body" || updated.CreatedAt.IsZero() {
		t.Fatalf("updated message = %#v", updated)
	}
}
