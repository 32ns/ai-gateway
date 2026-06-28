package controlplane

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestSiteMessagesRequireAdminAndTrackReads(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := service.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user, err := service.CreateUser(UserInput{
		Username: "alice",
		Password: "alice-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if _, err := service.CreateSiteMessage(user, SiteMessageInput{Title: "No", Body: "Denied", Enabled: true}); err == nil || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("CreateSiteMessage as user err = %v, want admin error", err)
	}
	enabled, err := service.CreateSiteMessage(admin, SiteMessageInput{Title: "Notice", Body: "Visible", Enabled: true})
	if err != nil {
		t.Fatalf("CreateSiteMessage enabled returned error: %v", err)
	}
	disabled, err := service.CreateSiteMessage(admin, SiteMessageInput{Title: "Draft", Body: "Hidden", Enabled: false})
	if err != nil {
		t.Fatalf("CreateSiteMessage disabled returned error: %v", err)
	}
	if deliveries := service.ListSiteMessages(user); len(deliveries) != 1 || deliveries[0].Message.ID != enabled.ID {
		t.Fatalf("user deliveries = %#v, want only enabled message", deliveries)
	}
	if deliveries := service.ListSiteMessages(admin); len(deliveries) != 2 || !siteMessageDeliveryContains(deliveries, disabled.ID) {
		t.Fatalf("admin deliveries = %#v, want enabled and disabled", deliveries)
	}
	if got := service.SiteMessageUnreadCount(user); got != 1 {
		t.Fatalf("unread count = %d, want 1", got)
	}
	if err := service.MarkSiteMessageRead(user, enabled.ID); err != nil {
		t.Fatalf("MarkSiteMessageRead returned error: %v", err)
	}
	if got := service.SiteMessageUnreadCount(user); got != 0 {
		t.Fatalf("unread count after read = %d, want 0", got)
	}
	if err := service.MarkSiteMessageRead(user, disabled.ID); err == nil {
		t.Fatal("expected regular user to be blocked from reading disabled message")
	}
	updated, err := service.UpdateSiteMessage(admin, disabled.ID, SiteMessageInput{Title: "Published", Body: "Now visible", Enabled: true})
	if err != nil {
		t.Fatalf("UpdateSiteMessage returned error: %v", err)
	}
	if !updated.Enabled || updated.Title != "Published" {
		t.Fatalf("updated message = %#v", updated)
	}
	if _, err := service.DeleteSiteMessage(user, updated.ID); err == nil || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("DeleteSiteMessage as user err = %v, want admin error", err)
	}
	deleted, err := service.DeleteSiteMessage(admin, updated.ID)
	if err != nil {
		t.Fatalf("DeleteSiteMessage returned error: %v", err)
	}
	if deleted.ID != updated.ID {
		t.Fatalf("deleted message = %#v", deleted)
	}
	if deliveries := service.ListSiteMessages(admin); siteMessageDeliveryContains(deliveries, updated.ID) {
		t.Fatalf("deleted message still listed: %#v", deliveries)
	}
}

func TestSiteMessagesTargetUsersAndAccountGroups(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := service.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	alice, err := service.CreateUser(UserInput{Username: "alice", Password: "alice-secret", Role: core.UserRoleUser, Enabled: true})
	if err != nil {
		t.Fatalf("CreateUser(alice) returned error: %v", err)
	}
	bob, err := service.CreateUser(UserInput{Username: "bob", Password: "bob-secret", Role: core.UserRoleUser, Enabled: true})
	if err != nil {
		t.Fatalf("CreateUser(bob) returned error: %v", err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_alice", Name: "Alice Plus", APIKey: "gw_alice", OwnerUserID: alice.ID, AccountGroup: "Plus", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	direct, err := service.CreateSiteMessage(admin, SiteMessageInput{
		Title:         "Direct",
		Body:          "Only Bob",
		Enabled:       true,
		TargetUserIDs: []string{bob.ID},
	})
	if err != nil {
		t.Fatalf("CreateSiteMessage direct returned error: %v", err)
	}
	group, err := service.CreateSiteMessage(admin, SiteMessageInput{
		Title:               "Plus",
		Body:                "Only Plus",
		Enabled:             true,
		TargetAccountGroups: []string{"Plus"},
	})
	if err != nil {
		t.Fatalf("CreateSiteMessage group returned error: %v", err)
	}
	if deliveries := service.ListSiteMessages(alice); len(deliveries) != 1 || deliveries[0].Message.ID != group.ID {
		t.Fatalf("alice deliveries = %#v, want group message only", deliveries)
	}
	if deliveries := service.ListSiteMessages(bob); len(deliveries) != 1 || deliveries[0].Message.ID != direct.ID {
		t.Fatalf("bob deliveries = %#v, want direct message only", deliveries)
	}
	if got := service.SiteMessageUnreadCount(alice); got != 1 {
		t.Fatalf("alice unread = %d, want 1", got)
	}
	if err := service.MarkSiteMessageRead(alice, direct.ID); err == nil {
		t.Fatal("expected alice to be blocked from reading bob direct message")
	}
	if _, err := service.CreateSiteMessage(admin, SiteMessageInput{Title: "Bad", Body: "Bad", Enabled: true, TargetAccountGroups: []string{"Missing"}}); err == nil {
		t.Fatal("expected unknown target group to be rejected")
	}
}

func TestAdminInboxHidesBillingPlanGrantNotificationsForOtherUsers(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := service.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user, err := service.CreateUser(UserInput{Username: "grant-target", Password: "target-secret", Role: core.UserRoleUser, Enabled: true})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	now := time.Now().UTC()
	grantNotice := core.SiteMessage{
		ID:            "msg_grant_notice",
		Title:         "套餐赠送到账",
		Body:          "管理员已赠送套餐给你。",
		CreatedBy:     admin.ID,
		Enabled:       true,
		TargetUserIDs: []string{user.ID},
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := repo.CreateSiteMessage(grantNotice); err != nil {
		t.Fatalf("CreateSiteMessage returned error: %v", err)
	}
	if deliveries := service.ListSiteMessages(user); len(deliveries) != 1 || deliveries[0].Message.ID != grantNotice.ID {
		t.Fatalf("target deliveries = %#v, want grant notice", deliveries)
	}
	if deliveries := service.ListSiteMessages(admin); siteMessageDeliveryContains(deliveries, grantNotice.ID) {
		t.Fatalf("admin inbox should not include grant notice targeted to another user: %#v", deliveries)
	}
	if got := service.SiteMessageUnreadCount(admin); got != 0 {
		t.Fatalf("admin unread = %d, want 0 for another user's grant notice", got)
	}
}

func TestUpdateSiteMessageClearsReads(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := service.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user, err := service.CreateUser(UserInput{
		Username: "message-reader",
		Password: "reader-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	message, err := service.CreateSiteMessage(admin, SiteMessageInput{Title: "Notice", Body: "Original", Enabled: true})
	if err != nil {
		t.Fatalf("CreateSiteMessage returned error: %v", err)
	}
	if err := service.MarkSiteMessageRead(user, message.ID); err != nil {
		t.Fatalf("MarkSiteMessageRead returned error: %v", err)
	}
	if got := service.SiteMessageUnreadCount(user); got != 0 {
		t.Fatalf("unread count after read = %d, want 0", got)
	}
	if _, err := service.UpdateSiteMessage(admin, message.ID, SiteMessageInput{Title: "Notice", Body: "Updated", Enabled: true}); err != nil {
		t.Fatalf("UpdateSiteMessage returned error: %v", err)
	}
	if got := service.SiteMessageUnreadCount(user); got != 1 {
		t.Fatalf("unread count after update = %d, want 1", got)
	}
	deliveries := service.ListSiteMessages(user)
	if len(deliveries) != 1 || deliveries[0].Message.ID != message.ID {
		t.Fatalf("deliveries = %#v, want updated message", deliveries)
	}
	if deliveries[0].Read {
		t.Fatalf("updated message delivery = %#v, want unread", deliveries[0])
	}
}

func TestSiteMessagesResolveTargetUsersByUsername(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := service.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user, err := service.CreateUser(UserInput{Username: "target-user", Password: "target-secret", Role: core.UserRoleUser, Enabled: true})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}

	message, err := service.CreateSiteMessage(admin, SiteMessageInput{
		Title:         "Direct",
		Body:          "By username",
		Enabled:       true,
		TargetUserIDs: []string{"target-user"},
	})
	if err != nil {
		t.Fatalf("CreateSiteMessage returned error: %v", err)
	}
	if len(message.TargetUserIDs) != 1 || message.TargetUserIDs[0] != user.ID {
		t.Fatalf("target users = %#v, want resolved user id %q", message.TargetUserIDs, user.ID)
	}
}

func TestSiteMessagesPublicPopupRequiresAllUsers(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := service.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	user, err := service.CreateUser(UserInput{Username: "public-target", Password: "target-secret", Role: core.UserRoleUser, Enabled: true})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_public", Name: "PublicGroup"}); err != nil {
		t.Fatal(err)
	}

	if _, err := service.CreateSiteMessage(admin, SiteMessageInput{
		Title:         "Public direct",
		Body:          "Body",
		Enabled:       true,
		PublicPopup:   true,
		TargetUserIDs: []string{user.ID},
	}); err == nil || !strings.Contains(err.Error(), "all users") {
		t.Fatalf("CreateSiteMessage direct public popup err = %v, want all users error", err)
	}
	if _, err := service.CreateSiteMessage(admin, SiteMessageInput{
		Title:               "Public group",
		Body:                "Body",
		Enabled:             true,
		PublicPopup:         true,
		TargetAccountGroups: []string{"PublicGroup"},
	}); err == nil || !strings.Contains(err.Error(), "all users") {
		t.Fatalf("CreateSiteMessage group public popup err = %v, want all users error", err)
	}
	message, err := service.CreateSiteMessage(admin, SiteMessageInput{Title: "Public", Body: "Body", Enabled: true, PublicPopup: true})
	if err != nil {
		t.Fatalf("CreateSiteMessage all-user public popup returned error: %v", err)
	}
	if !message.PublicPopup {
		t.Fatalf("message = %#v, want public popup", message)
	}
	if deliveries := service.ListSiteMessages(user); siteMessageDeliveryContains(deliveries, message.ID) {
		t.Fatalf("public popup should not appear in regular user inbox: %#v", deliveries)
	}
	if got := service.SiteMessageUnreadCount(user); got != 0 {
		t.Fatalf("public popup unread count = %d, want 0", got)
	}
	if got := service.SiteMessageUnreadCount(admin); got != 0 {
		t.Fatalf("admin public popup unread count = %d, want 0", got)
	}
	if _, err := service.GetSiteMessage(user, message.ID); err == nil {
		t.Fatal("expected public popup to be unavailable through regular user inbox lookup")
	}
}

func TestSiteMessagesAccountGroupsAvoidFullClientList(t *testing.T) {
	base := storage.NewMemoryRepository()
	if err := base.UpsertUser(core.User{ID: "admin", Username: "admin", Role: core.UserRoleAdmin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := base.UpsertUser(core.User{ID: "user_message_group", Username: "message-group", Role: core.UserRoleUser, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := base.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := base.UpsertClient(core.APIClient{ID: "client_message_group", Name: "Message Group", APIKey: "gw_message_group", OwnerUserID: "user_message_group", AccountGroup: "Plus", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	repo := &siteMessageFullClientListPanicRepository{MemoryRepository: base}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, err := service.GetUser("admin")
	if err != nil {
		t.Fatal(err)
	}
	user, err := service.GetUser("user_message_group")
	if err != nil {
		t.Fatal(err)
	}
	message, err := service.CreateSiteMessage(admin, SiteMessageInput{
		Title:               "Plus",
		Body:                "Only Plus",
		Enabled:             true,
		TargetAccountGroups: []string{"Plus"},
	})
	if err != nil {
		t.Fatalf("CreateSiteMessage returned error: %v", err)
	}

	deliveries := service.ListSiteMessages(user)
	if len(deliveries) != 1 || deliveries[0].Message.ID != message.ID {
		t.Fatalf("deliveries = %#v, want account group message", deliveries)
	}
	if got := service.SiteMessageUnreadCount(user); got != 1 {
		t.Fatalf("unread count = %d, want 1", got)
	}
}

func TestSiteMessagesRegularUserUsesVisiblePager(t *testing.T) {
	base := storage.NewMemoryRepository()
	if err := base.UpsertUser(core.User{ID: "admin", Username: "admin", Role: core.UserRoleAdmin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := base.UpsertUser(core.User{ID: "user_visible_page", Username: "visible-page", Role: core.UserRoleUser, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := range 30 {
		if err := base.CreateSiteMessage(core.SiteMessage{
			ID:        fmt.Sprintf("msg_visible_page_%02d", i),
			Title:     fmt.Sprintf("Visible %02d", i),
			Body:      "Body",
			CreatedBy: "admin",
			Enabled:   true,
			CreatedAt: now.Add(time.Duration(i) * time.Minute),
			UpdatedAt: now.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("CreateSiteMessage(%d) returned error: %v", i, err)
		}
	}

	repo := &siteMessageVisiblePagerRepository{MemoryRepository: base}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := service.GetUser("user_visible_page")
	if err != nil {
		t.Fatal(err)
	}

	deliveries, total := service.ListSiteMessagesPage(user, 25, 25)
	if total != 30 {
		t.Fatalf("total = %d, want 30", total)
	}
	if len(deliveries) != 5 || deliveries[0].Message.ID != "msg_visible_page_04" {
		t.Fatalf("deliveries = %#v, want second page from visible pager", deliveries)
	}
	if repo.visiblePageCalls == 0 {
		t.Fatal("regular user messages page did not use visible pager")
	}
	if got := service.SiteMessageUnreadCount(user); got != 30 {
		t.Fatalf("unread count = %d, want 30", got)
	}
	if repo.visibleUnreadCalls == 0 {
		t.Fatal("regular user unread count did not use visible unread counter")
	}
}

type siteMessageFullClientListPanicRepository struct {
	*storage.MemoryRepository
}

func (r *siteMessageFullClientListPanicRepository) ListClients() []core.APIClient {
	panic("site message account group checks should use ListClientsByOwner")
}

type siteMessageVisiblePagerRepository struct {
	*storage.MemoryRepository
	visiblePageCalls   int
	visibleUnreadCalls int
}

func (r *siteMessageVisiblePagerRepository) ListSiteMessages() []core.SiteMessage {
	panic("regular user messages page should use visible pager")
}

func (r *siteMessageVisiblePagerRepository) ListSiteMessageDeliveries(userID string, includeDisabled bool) []core.SiteMessageDelivery {
	panic("regular user messages page should use visible pager")
}

func (r *siteMessageVisiblePagerRepository) ListVisibleSiteMessageDeliveriesPage(query storage.SiteMessageVisibilityQuery) ([]core.SiteMessageDelivery, int) {
	r.visiblePageCalls++
	return r.MemoryRepository.ListVisibleSiteMessageDeliveriesPage(query)
}

func (r *siteMessageVisiblePagerRepository) VisibleSiteMessageUnreadCount(query storage.SiteMessageVisibilityQuery) int {
	r.visibleUnreadCalls++
	return r.MemoryRepository.VisibleSiteMessageUnreadCount(query)
}

func siteMessageDeliveryContains(deliveries []core.SiteMessageDelivery, id string) bool {
	for _, delivery := range deliveries {
		if delivery.Message.ID == id {
			return true
		}
	}
	return false
}
