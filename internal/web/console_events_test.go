package web

import (
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

func TestConsoleEventBusFiltersUserScopedEvents(t *testing.T) {
	bus := newConsoleEventBus()
	_, aliceEvents, unsubscribeAlice := bus.subscribe(core.User{ID: "alice", Username: "alice", Enabled: true})
	defer unsubscribeAlice()
	_, bobEvents, unsubscribeBob := bus.subscribe(core.User{ID: "bob", Username: "bob", Enabled: true})
	defer unsubscribeBob()
	_, adminEvents, unsubscribeAdmin := bus.subscribe(core.User{ID: "admin", Username: "admin", Role: core.UserRoleAdmin, Enabled: true})
	defer unsubscribeAdmin()

	bus.publish(consoleEvent{Type: consoleEventBalanceUpdated, Scope: "user", UserID: "alice"})

	if got := readConsoleEventForTest(t, aliceEvents); got.Type != consoleEventBalanceUpdated || got.UserID != "alice" {
		t.Fatalf("alice event = %#v", got)
	}
	if got := readConsoleEventForTest(t, adminEvents); got.Type != consoleEventBalanceUpdated || got.UserID != "alice" {
		t.Fatalf("admin event = %#v", got)
	}
	assertNoConsoleEventForTest(t, bobEvents)
}

func TestConsoleEventBusFiltersAdminAndBroadcastEvents(t *testing.T) {
	bus := newConsoleEventBus()
	_, userEvents, unsubscribeUser := bus.subscribe(core.User{ID: "user", Username: "user", Enabled: true})
	defer unsubscribeUser()
	_, adminEvents, unsubscribeAdmin := bus.subscribe(core.User{ID: "admin", Username: "admin", Role: core.UserRoleAdmin, Enabled: true})
	defer unsubscribeAdmin()

	bus.publish(consoleEvent{Type: consoleEventSettingsUpdated, Scope: "admin"})

	if got := readConsoleEventForTest(t, adminEvents); got.Type != consoleEventSettingsUpdated {
		t.Fatalf("admin event = %#v", got)
	}
	assertNoConsoleEventForTest(t, userEvents)

	bus.publish(consoleEvent{Type: consoleEventModelsChanged, Scope: "user"})

	if got := readConsoleEventForTest(t, userEvents); got.Type != consoleEventModelsChanged {
		t.Fatalf("user broadcast event = %#v", got)
	}
	if got := readConsoleEventForTest(t, adminEvents); got.Type != consoleEventModelsChanged {
		t.Fatalf("admin broadcast event = %#v", got)
	}

	bus.publish(consoleEvent{Type: consoleEventFinanceChanged, Scope: "admin"})

	if got := readConsoleEventForTest(t, adminEvents); got.Type != consoleEventFinanceChanged {
		t.Fatalf("admin finance event = %#v", got)
	}
	assertNoConsoleEventForTest(t, userEvents)
}

func TestConsoleEventBusDoesNotMirrorUserSupportEventsToAdmins(t *testing.T) {
	bus := newConsoleEventBus()
	_, userEvents, unsubscribeUser := bus.subscribe(core.User{ID: "support-user", Username: "support-user", Enabled: true})
	defer unsubscribeUser()
	_, adminEvents, unsubscribeAdmin := bus.subscribe(core.User{ID: "admin", Username: "admin", Role: core.UserRoleAdmin, Enabled: true})
	defer unsubscribeAdmin()

	bus.publish(consoleEvent{Type: consoleEventSupportMessage, Scope: "user", UserID: "support-user"})

	if got := readConsoleEventForTest(t, userEvents); got.Type != consoleEventSupportMessage || got.UserID != "support-user" {
		t.Fatalf("user support event = %#v", got)
	}
	assertNoConsoleEventForTest(t, adminEvents)
}

func TestConsoleEventStateReturnsCurrentUserState(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user, err := control.CreateUser(controlplane.UserInput{
		Username: "event-state-user",
		Password: "event-state-secret",
		Role:     core.UserRoleUser,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if _, _, err := repo.AdjustUserBalance(user.ID, 2*core.NanoUSDPerUSD, "test credit"); err != nil {
		t.Fatalf("AdjustUserBalance returned error: %v", err)
	}
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	if _, err := control.CreateSiteMessage(admin, controlplane.SiteMessageInput{
		Title:   "Unread",
		Body:    "Unread body",
		Enabled: true,
	}); err != nil {
		t.Fatalf("CreateSiteMessage returned error: %v", err)
	}
	if _, _, err := control.CreateSupportTicket(user, controlplane.SupportTicketInput{Title: "在线客服", Body: "hello"}); err != nil {
		t.Fatalf("CreateSupportTicket returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, user, server.Handler())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console/events/state", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("state status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"status":"ok"`, `"unread_messages":1`, `"unread_support_messages":0`, `"balance":"2"`, `"balance_display":"$2.00"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("state response missing %q: %s", want, body)
		}
	}
}

func TestPublishSupportMessageCreatedUsesConsoleEventBus(t *testing.T) {
	bus := newConsoleEventBus()
	_, userEvents, unsubscribeUser := bus.subscribe(core.User{ID: "user_support_event", Username: "alice", Enabled: true})
	defer unsubscribeUser()
	_, adminEvents, unsubscribeAdmin := bus.subscribe(core.User{ID: "admin", Username: "admin", Role: core.UserRoleAdmin, Enabled: true})
	defer unsubscribeAdmin()

	server := &Server{consoleEvents: bus}
	ticket := core.SupportTicket{
		ID:          "sup_event",
		UserID:      "user_support_event",
		Username:    "alice",
		Title:       "在线客服",
		LastMessage: "hello",
	}
	message := core.SupportMessage{
		ID:        "smsg_event",
		TicketID:  "sup_event",
		ActorID:   "user_support_event",
		ActorRole: core.UserRoleUser,
		Body:      "hello",
	}
	userTicket := ticket
	adminTicket := ticket
	adminTicket.UnreadCount = 1

	server.publishSupportMessageCreated(ticket, message, userTicket, adminTicket, 0, 1)

	userEvent := readConsoleEventForTest(t, userEvents)
	if userEvent.Type != consoleEventSupportMessage || userEvent.UserID != "user_support_event" || userEvent.ResourceID != "smsg_event" {
		t.Fatalf("user support event = %#v", userEvent)
	}
	if got := payloadIntForTest(userEvent.Payload, "unread"); got != 0 {
		t.Fatalf("user support unread payload = %d, want 0", got)
	}

	adminEvent := readConsoleEventForTest(t, adminEvents)
	if adminEvent.Type != consoleEventSupportMessage || adminEvent.Scope != "admin" || adminEvent.ResourceID != "smsg_event" {
		t.Fatalf("admin support event = %#v", adminEvent)
	}
	if got := payloadIntForTest(adminEvent.Payload, "unread"); got != 1 {
		t.Fatalf("admin support unread payload = %d, want 1", got)
	}
}

func TestPublishPaymentUpdatedNotifiesUserBalanceAndFinance(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	user := core.User{ID: "user_payment_event", Username: "payment-event", Enabled: true}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.SetUserBalance(user.ID, 7*core.NanoUSDPerUSD); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}
	bus := newConsoleEventBus()
	_, userEvents, unsubscribeUser := bus.subscribe(user)
	defer unsubscribeUser()
	_, adminEvents, unsubscribeAdmin := bus.subscribe(core.User{ID: "admin", Username: "admin", Role: core.UserRoleAdmin, Enabled: true})
	defer unsubscribeAdmin()

	server := &Server{control: control, consoleEvents: bus}
	order := core.PaymentOrder{ID: "pay_event", OutTradeNo: "out_event", UserID: user.ID, Provider: core.PaymentProviderPersonalPay, Status: core.PaymentOrderPaid, AmountNanoUSD: 7 * core.NanoUSDPerUSD}
	server.publishPaymentUpdated(user, order)

	userPayment := readConsoleEventForTest(t, userEvents)
	if userPayment.Type != consoleEventPaymentUpdated || userPayment.UserID != user.ID || userPayment.ResourceID != order.ID {
		t.Fatalf("user payment event = %#v", userPayment)
	}
	userBalance := readConsoleEventForTest(t, userEvents)
	if userBalance.Type != consoleEventBalanceUpdated || userBalance.UserID != user.ID {
		t.Fatalf("user balance event = %#v", userBalance)
	}
	adminPayment := readConsoleEventForTest(t, adminEvents)
	if adminPayment.Type != consoleEventPaymentUpdated || adminPayment.UserID != user.ID || adminPayment.ResourceID != order.ID {
		t.Fatalf("admin payment event = %#v", adminPayment)
	}
	adminBalance := readConsoleEventForTest(t, adminEvents)
	if adminBalance.Type != consoleEventBalanceUpdated || adminBalance.UserID != user.ID {
		t.Fatalf("admin balance event = %#v", adminBalance)
	}
	adminFinance := readConsoleEventForTest(t, adminEvents)
	if adminFinance.Type != consoleEventFinanceChanged || adminFinance.Scope != "admin" || adminFinance.ResourceID != order.ID {
		t.Fatalf("admin finance event = %#v", adminFinance)
	}
	assertNoConsoleEventForTest(t, userEvents)
}

func TestPublishSupportUnreadUsesConsoleEventBus(t *testing.T) {
	bus := newConsoleEventBus()
	_, userEvents, unsubscribeUser := bus.subscribe(core.User{ID: "user_support_unread", Username: "alice", Enabled: true})
	defer unsubscribeUser()
	_, otherEvents, unsubscribeOther := bus.subscribe(core.User{ID: "other", Username: "other", Enabled: true})
	defer unsubscribeOther()
	_, adminEvents, unsubscribeAdmin := bus.subscribe(core.User{ID: "admin", Username: "admin", Role: core.UserRoleAdmin, Enabled: true})
	defer unsubscribeAdmin()

	server := &Server{consoleEvents: bus}
	server.publishSupportUnread("user_support_unread", 2, 3)

	if got := readConsoleEventForTest(t, userEvents); got.Type != consoleEventSupportUnread || payloadIntForTest(got.Payload, "count") != 2 {
		t.Fatalf("user support unread event = %#v", got)
	}
	assertNoConsoleEventForTest(t, otherEvents)
	if got := readConsoleEventForTest(t, adminEvents); got.Type != consoleEventSupportUnread || got.Scope != "admin" || payloadIntForTest(got.Payload, "count") != 3 {
		t.Fatalf("admin support unread event = %#v", got)
	}
}

func TestBillingEventShouldRefreshFinanceSkipsHighFrequencyUsageEvents(t *testing.T) {
	for _, reason := range []string{"usage_settled", "usage_released", "usage_account_updated"} {
		if billingEventShouldRefreshFinance(reason) {
			t.Fatalf("billing reason %q should not refresh finance page", reason)
		}
	}
	for _, reason := range []string{"payment", "balance_adjustment", "plan_purchase", "reserved_billing_release", ""} {
		if !billingEventShouldRefreshFinance(reason) {
			t.Fatalf("billing reason %q should refresh finance page", reason)
		}
	}
}

func readConsoleEventForTest(t *testing.T, events <-chan consoleEvent) consoleEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for console event")
		return consoleEvent{}
	}
}

func assertNoConsoleEventForTest(t *testing.T, events <-chan consoleEvent) {
	t.Helper()
	select {
	case event := <-events:
		t.Fatalf("unexpected console event: %#v", event)
	case <-time.After(25 * time.Millisecond):
	}
}

func payloadIntForTest(payload map[string]any, key string) int {
	value, _ := payload[key].(int)
	return value
}
