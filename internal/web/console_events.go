package web

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
)

const (
	consoleEventPaymentUpdated      = "payment.updated"
	consoleEventBalanceUpdated      = "balance.updated"
	consoleEventSiteMessageUpdated  = "message.updated"
	consoleEventSiteMessageUnread   = "message.unread_count"
	consoleEventAccountBatchUpdated = "account_batch.updated"
	consoleEventAccountPoolChanged  = "account_pool.changed"
	consoleEventImageJobUpdated     = "image_job.updated"
	consoleEventModelsChanged       = "models.changed"
	consoleEventSettingsUpdated     = "settings.updated"
	consoleEventFinanceChanged      = "finance.changed"
	consoleEventUsageLogChanged     = "usage_log.changed"
	consoleEventAuditUpdated        = "audit.updated"
	consoleEventSupportMessage      = "support.message.created"
	consoleEventSupportUnread       = "support.unread_count"
	consoleEventPing                = "ping"
)

type consoleEvent struct {
	Type       string         `json:"type"`
	Scope      string         `json:"scope,omitempty"`
	UserID     string         `json:"user_id,omitempty"`
	ResourceID string         `json:"resource_id,omitempty"`
	Version    int64          `json:"version"`
	Payload    map[string]any `json:"payload,omitempty"`
	CreatedAt  string         `json:"created_at"`
}

type consoleEventBus struct {
	mu          sync.Mutex
	nextID      int64
	subscribers map[int64]consoleEventSubscriber
}

type consoleEventSubscriber struct {
	id     int64
	userID string
	admin  bool
	ch     chan consoleEvent
}

func newConsoleEventBus() *consoleEventBus {
	return &consoleEventBus{subscribers: make(map[int64]consoleEventSubscriber)}
}

func (b *consoleEventBus) subscribe(user core.User) (int64, <-chan consoleEvent, func()) {
	if b == nil {
		ch := make(chan consoleEvent)
		close(ch)
		return 0, ch, func() {}
	}
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	ch := make(chan consoleEvent, 32)
	b.subscribers[id] = consoleEventSubscriber{
		id:     id,
		userID: strings.TrimSpace(user.ID),
		admin:  user.IsAdmin(),
		ch:     ch,
	}
	b.mu.Unlock()
	return id, ch, func() {
		b.mu.Lock()
		delete(b.subscribers, id)
		b.mu.Unlock()
	}
}

func (b *consoleEventBus) publish(event consoleEvent) {
	if b == nil {
		return
	}
	event.Type = strings.TrimSpace(event.Type)
	if event.Type == "" {
		return
	}
	event.UserID = strings.TrimSpace(event.UserID)
	event.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	b.mu.Lock()
	b.nextID++
	event.Version = b.nextID
	subscribers := make([]consoleEventSubscriber, 0, len(b.subscribers))
	for _, sub := range b.subscribers {
		subscribers = append(subscribers, sub)
	}
	b.mu.Unlock()
	for _, sub := range subscribers {
		if !consoleEventVisibleToSubscriber(event, sub) {
			continue
		}
		select {
		case sub.ch <- event:
		default:
			select {
			case <-sub.ch:
			default:
			}
			select {
			case sub.ch <- event:
			default:
			}
		}
	}
}

func consoleEventVisibleToSubscriber(event consoleEvent, sub consoleEventSubscriber) bool {
	if sub.admin {
		if userOnlyConsoleEvent(event) && strings.TrimSpace(event.Scope) == "user" {
			userID := strings.TrimSpace(event.UserID)
			return userID != "" && strings.EqualFold(userID, sub.userID)
		}
		return true
	}
	switch strings.TrimSpace(event.Scope) {
	case "admin":
		return false
	}
	userID := strings.TrimSpace(event.UserID)
	return userID == "" || strings.EqualFold(userID, sub.userID)
}

func userOnlyConsoleEvent(event consoleEvent) bool {
	switch strings.TrimSpace(event.Type) {
	case consoleEventSupportMessage, consoleEventSupportUnread, consoleEventImageJobUpdated:
		return true
	default:
		return false
	}
}

func (s *Server) handleConsoleEvents(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/console/events" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	_, events, unsubscribe := s.consoleEvents.subscribe(user)
	defer unsubscribe()

	if err := writeSSEJSON(w, "ready", consoleEvent{
		Type:      "ready",
		Scope:     "user",
		UserID:    user.ID,
		Version:   time.Now().UnixNano(),
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		return
	}
	flusher.Flush()

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			if err := writeSSEJSON(w, event.Type, event); err != nil {
				return
			}
			flusher.Flush()
		case now := <-ticker.C:
			if err := writeSSEJSON(w, consoleEventPing, consoleEvent{
				Type:      consoleEventPing,
				Version:   now.UnixNano(),
				CreatedAt: now.UTC().Format(time.RFC3339Nano),
			}); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) handleConsoleEventState(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/console/events/state" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	currentUser := user
	if s != nil && s.control != nil {
		if loaded, err := s.control.GetUser(user.ID); err == nil {
			currentUser = loaded
		}
	}
	unread := 0
	supportUnread := 0
	if s != nil && s.control != nil {
		unread = s.control.SiteMessageUnreadCount(currentUser)
		supportUnread = s.control.SupportUnreadCount(currentUser)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":                  "ok",
		"unread_messages":         unread,
		"unread_support_messages": supportUnread,
		"balance":                 core.FormatNanoUSD(currentUser.BalanceNanoUSD),
		"balance_display":         formatUSDDisplay(currentUser.BalanceNanoUSD),
		"balance_nano_usd":        currentUser.BalanceNanoUSD,
	})
}

func (s *Server) publishConsoleEvent(event consoleEvent) {
	if s == nil || s.consoleEvents == nil {
		return
	}
	s.consoleEvents.publish(event)
}

func (s *Server) publishBalanceUpdated(userID string) {
	userID = strings.TrimSpace(userID)
	if userID == "" || s == nil || s.control == nil {
		return
	}
	user, err := s.control.GetUser(userID)
	if err != nil {
		return
	}
	s.publishConsoleEvent(consoleEvent{
		Type:   consoleEventBalanceUpdated,
		Scope:  "user",
		UserID: user.ID,
		Payload: map[string]any{
			"balance":          core.FormatNanoUSD(user.BalanceNanoUSD),
			"balance_display":  formatUSDDisplay(user.BalanceNanoUSD),
			"balance_nano_usd": user.BalanceNanoUSD,
		},
	})
}

func (s *Server) publishPaymentUpdated(user core.User, order core.PaymentOrder) {
	if strings.TrimSpace(order.ID) == "" {
		return
	}
	userID := strings.TrimSpace(order.UserID)
	s.publishConsoleEvent(consoleEvent{
		Type:       consoleEventPaymentUpdated,
		Scope:      "user",
		UserID:     userID,
		ResourceID: order.ID,
		Payload:    s.paymentStatusPayload(user, order),
	})
	if order.Status == core.PaymentOrderPaid {
		s.publishBalanceUpdated(userID)
	}
	s.publishFinanceChanged("payment", order.ID)
}

func (s *Server) publishSiteMessageUnread(user core.User) {
	if strings.TrimSpace(user.ID) == "" || s == nil || s.control == nil {
		return
	}
	s.publishConsoleEvent(consoleEvent{
		Type:   consoleEventSiteMessageUnread,
		Scope:  "user",
		UserID: user.ID,
		Payload: map[string]any{
			"count": s.control.SiteMessageUnreadCount(user),
		},
	})
}

func (s *Server) publishSiteMessagesChanged() {
	s.publishConsoleEvent(consoleEvent{
		Type:  consoleEventSiteMessageUpdated,
		Scope: "user",
		Payload: map[string]any{
			"refresh": true,
		},
	})
}

func (s *Server) publishAccountBatchUpdated(locale string, snapshot controlplane.AccountBatchJobSnapshot) {
	if strings.TrimSpace(snapshot.ID) == "" {
		return
	}
	s.publishConsoleEvent(consoleEvent{
		Type:       consoleEventAccountBatchUpdated,
		Scope:      "admin",
		ResourceID: snapshot.ID,
		Payload:    s.accountBatchJobPayload(locale, snapshot),
	})
}

func (s *Server) publishAccountPoolChanged() {
	s.publishConsoleEvent(consoleEvent{
		Type:  consoleEventAccountPoolChanged,
		Scope: "admin",
		Payload: map[string]any{
			"refresh": true,
		},
	})
}

func (s *Server) publishImageJobUpdated(snapshot imageLabTaskSnapshot) {
	if strings.TrimSpace(snapshot.ID) == "" || strings.TrimSpace(snapshot.UserID) == "" {
		return
	}
	s.publishConsoleEvent(consoleEvent{
		Type:       consoleEventImageJobUpdated,
		Scope:      "user",
		UserID:     strings.TrimSpace(snapshot.UserID),
		ResourceID: strings.TrimSpace(snapshot.ID),
		Payload: map[string]any{
			"job": snapshot,
		},
	})
}

func (s *Server) publishModelsChanged(modelID string) {
	s.publishConsoleEvent(consoleEvent{
		Type:       consoleEventModelsChanged,
		Scope:      "user",
		ResourceID: strings.TrimSpace(modelID),
		Payload: map[string]any{
			"refresh": true,
		},
	})
}

func (s *Server) publishSettingsUpdated(section string) {
	s.publishConsoleEvent(consoleEvent{
		Type:  consoleEventSettingsUpdated,
		Scope: "admin",
		Payload: map[string]any{
			"section": strings.TrimSpace(section),
		},
	})
}

func (s *Server) publishFinanceChanged(reason string, resourceID string) {
	s.publishConsoleEvent(consoleEvent{
		Type:       consoleEventFinanceChanged,
		Scope:      "admin",
		ResourceID: strings.TrimSpace(resourceID),
		Payload: map[string]any{
			"reason": strings.TrimSpace(reason),
		},
	})
}

func (s *Server) publishUsageLogChanged(reason string, userID string, resourceID string) {
	s.publishConsoleEvent(consoleEvent{
		Type:       consoleEventUsageLogChanged,
		Scope:      "user",
		UserID:     strings.TrimSpace(userID),
		ResourceID: strings.TrimSpace(resourceID),
		Payload: map[string]any{
			"reason": strings.TrimSpace(reason),
		},
	})
}

func (s *Server) publishAuditUpdated() {
	s.publishConsoleEvent(consoleEvent{
		Type:  consoleEventAuditUpdated,
		Scope: "admin",
		Payload: map[string]any{
			"refresh": true,
		},
	})
}

func (s *Server) publishSupportMessageCreated(ticket core.SupportTicket, message core.SupportMessage, userTicket core.SupportTicket, adminTicket core.SupportTicket, userUnread int, adminUnread int) {
	if strings.TrimSpace(message.ID) == "" {
		return
	}
	userID := strings.TrimSpace(ticket.UserID)
	if userID != "" {
		s.publishConsoleEvent(consoleEvent{
			Type:       consoleEventSupportMessage,
			Scope:      "user",
			UserID:     userID,
			ResourceID: message.ID,
			Payload: map[string]any{
				"ticket":  userTicket,
				"message": message,
				"unread":  userUnread,
			},
		})
	}
	s.publishConsoleEvent(consoleEvent{
		Type:       consoleEventSupportMessage,
		Scope:      "admin",
		ResourceID: message.ID,
		Payload: map[string]any{
			"ticket":  adminTicket,
			"message": message,
			"unread":  adminUnread,
		},
	})
}

func (s *Server) publishSupportUnread(userID string, userUnread int, adminUnread int) {
	userID = strings.TrimSpace(userID)
	if userID != "" {
		s.publishConsoleEvent(consoleEvent{
			Type:   consoleEventSupportUnread,
			Scope:  "user",
			UserID: userID,
			Payload: map[string]any{
				"count": userUnread,
			},
		})
	}
	s.publishConsoleEvent(consoleEvent{
		Type:  consoleEventSupportUnread,
		Scope: "admin",
		Payload: map[string]any{
			"count": adminUnread,
		},
	})
}

func (s *Server) watchAccountBatchJob(locale, jobID string) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" || s == nil || s.control == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.After(2 * time.Hour)
		lastSignature := ""
		for {
			select {
			case <-deadline:
				return
			case <-ticker.C:
				snapshot, ok := s.control.GetAccountBatchJob(jobID)
				if !ok {
					return
				}
				signature := accountBatchJobEventSignature(snapshot)
				if signature != lastSignature {
					lastSignature = signature
					s.publishAccountBatchUpdated(locale, snapshot)
				}
				if snapshot.Status == controlplane.AccountBatchJobCompleted || snapshot.Status == controlplane.AccountBatchJobCancelled {
					s.publishAccountPoolChanged()
					return
				}
			}
		}
	}()
}

func accountBatchJobEventSignature(snapshot controlplane.AccountBatchJobSnapshot) string {
	return strings.Join([]string{
		snapshot.ID,
		string(snapshot.Status),
		strings.TrimSpace(snapshot.Current),
		strconv.Itoa(snapshot.Done),
		strconv.Itoa(snapshot.Succeeded),
		strconv.Itoa(snapshot.Failed),
		strconv.Itoa(snapshot.Skipped),
	}, "|")
}
