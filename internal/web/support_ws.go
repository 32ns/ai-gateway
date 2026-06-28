package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"
	"github.com/gorilla/websocket"
)

const (
	supportWebSocketReadLimit = 1 << 20
	supportWebSocketWriteWait = 10 * time.Second
	supportWebSocketPongWait  = 70 * time.Second
	supportWebSocketPingEvery = 25 * time.Second
)

type supportHub struct {
	mu     sync.Mutex
	users  map[string]map[*supportConn]struct{}
	admins map[*supportConn]struct{}
}

type supportConn struct {
	hub   *supportHub
	user  core.User
	admin bool
	send  chan supportEvent
}

type supportEvent struct {
	Type      string                `json:"type"`
	RequestID string                `json:"request_id,omitempty"`
	Ticket    *core.SupportTicket   `json:"ticket,omitempty"`
	Message   *core.SupportMessage  `json:"message,omitempty"`
	Tickets   []core.SupportTicket  `json:"tickets,omitempty"`
	Messages  []core.SupportMessage `json:"messages,omitempty"`
	Unread    int                   `json:"unread,omitempty"`
	ErrorCode string                `json:"error_code,omitempty"`
	Error     string                `json:"error,omitempty"`
}

type supportClientEvent struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	TicketID  string `json:"ticket_id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
}

func newSupportHub() *supportHub {
	return &supportHub{
		users:  make(map[string]map[*supportConn]struct{}),
		admins: make(map[*supportConn]struct{}),
	}
}

func (h *supportHub) register(conn *supportConn) {
	if h == nil || conn == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if conn.admin {
		h.admins[conn] = struct{}{}
		return
	}
	userID := strings.TrimSpace(conn.user.ID)
	if userID == "" {
		return
	}
	if h.users[userID] == nil {
		h.users[userID] = make(map[*supportConn]struct{})
	}
	h.users[userID][conn] = struct{}{}
}

func (h *supportHub) unregister(conn *supportConn) {
	if h == nil || conn == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if conn.admin {
		delete(h.admins, conn)
		return
	}
	userID := strings.TrimSpace(conn.user.ID)
	if conns := h.users[userID]; conns != nil {
		delete(conns, conn)
		if len(conns) == 0 {
			delete(h.users, userID)
		}
	}
}

func (h *supportHub) broadcastTicket(ticket core.SupportTicket, message *core.SupportMessage, userTicket core.SupportTicket, adminTicket core.SupportTicket, userUnread int, adminUnread int) {
	if h == nil {
		return
	}
	event := supportEvent{Type: "support.message.created", Message: message}
	adminEvent := event
	adminEvent.Ticket = &adminTicket
	adminEvent.Unread = adminUnread
	userEvent := event
	userEvent.Ticket = &userTicket
	userEvent.Unread = userUnread

	h.mu.Lock()
	admins := make([]*supportConn, 0, len(h.admins))
	for conn := range h.admins {
		admins = append(admins, conn)
	}
	users := make([]*supportConn, 0, len(h.users[ticket.UserID]))
	for conn := range h.users[ticket.UserID] {
		users = append(users, conn)
	}
	h.mu.Unlock()
	for _, conn := range admins {
		conn.enqueue(adminEvent)
	}
	for _, conn := range users {
		conn.enqueue(userEvent)
	}
}

func (h *supportHub) broadcastTicketDeleted(ticket core.SupportTicket, adminUnread int, userUnread int) {
	if h == nil {
		return
	}
	ticketID := strings.TrimSpace(ticket.ID)
	if ticketID == "" {
		return
	}
	adminTicket := ticket
	adminTicket.UnreadCount = 0
	userTicket := ticket
	userTicket.UnreadCount = 0

	h.mu.Lock()
	admins := make([]*supportConn, 0, len(h.admins))
	for conn := range h.admins {
		admins = append(admins, conn)
	}
	users := make([]*supportConn, 0, len(h.users[ticket.UserID]))
	for conn := range h.users[ticket.UserID] {
		users = append(users, conn)
	}
	h.mu.Unlock()

	for _, conn := range admins {
		ticket := adminTicket
		conn.enqueue(supportEvent{Type: "support.ticket.deleted", Ticket: &ticket, Unread: adminUnread})
	}
	for _, conn := range users {
		ticket := userTicket
		conn.enqueue(supportEvent{Type: "support.ticket.deleted", Ticket: &ticket, Unread: userUnread})
	}
}

func (c *supportConn) enqueue(event supportEvent) {
	if c == nil {
		return
	}
	select {
	case c.send <- event:
	default:
		select {
		case <-c.send:
		default:
		}
		select {
		case c.send <- event:
		default:
		}
	}
}

var supportWebSocketUpgrader = websocket.Upgrader{
	CheckOrigin: supportWebSocketOriginAllowed,
}

func supportWebSocketOriginAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Host, requestDomain(r))
}

func (s *Server) handleSupportWebSocket(w http.ResponseWriter, r *http.Request) {
	s.handleSupportWebSocketForRole(w, r, false)
}

func (s *Server) handleAdminSupportWebSocket(w http.ResponseWriter, r *http.Request) {
	s.handleSupportWebSocketForRole(w, r, true)
}

func (s *Server) handleSupportWebSocketForRole(w http.ResponseWriter, r *http.Request, admin bool) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, ok := currentUserFromContext(r.Context())
	if !ok || (admin && !user.IsAdmin()) {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	conn, err := supportWebSocketUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(supportWebSocketReadLimit)
	_ = conn.SetReadDeadline(time.Now().Add(supportWebSocketPongWait))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(supportWebSocketPongWait))
		return nil
	})

	client := &supportConn{
		hub:   s.support,
		user:  user,
		admin: admin,
		send:  make(chan supportEvent, 64),
	}
	s.support.register(client)
	defer func() {
		s.support.unregister(client)
		_ = conn.Close()
		close(client.send)
	}()

	go supportWritePump(conn, client)
	client.enqueue(s.supportBootstrapEvent(user, admin))
	s.supportReadPump(conn, client)
}

func supportWritePump(conn *websocket.Conn, client *supportConn) {
	ticker := time.NewTicker(supportWebSocketPingEvery)
	defer ticker.Stop()
	for {
		select {
		case event, ok := <-client.send:
			_ = conn.SetWriteDeadline(time.Now().Add(supportWebSocketWriteWait))
			if !ok {
				_ = conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := conn.WriteJSON(event); err != nil {
				return
			}
		case <-ticker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(supportWebSocketWriteWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (s *Server) supportReadPump(conn *websocket.Conn, client *supportConn) {
	for {
		var event supportClientEvent
		if err := conn.ReadJSON(&event); err != nil {
			if isClientWebSocketClose(err) || errors.Is(err, websocket.ErrCloseSent) {
				return
			}
			client.enqueue(supportErrorEvent("", supportErrorCodeInvalidRequest, "消息格式无效"))
			return
		}
		s.handleSupportClientEvent(client, event)
	}
}

func (s *Server) handleSupportClientEvent(conn *supportConn, event supportClientEvent) {
	switch strings.TrimSpace(event.Type) {
	case "support.ticket.create":
		if conn.admin {
			conn.enqueue(supportErrorEvent(event.RequestID, supportErrorCodeForbidden, "管理员不能创建用户会话"))
			return
		}
		user, err := s.control.GetUser(conn.user.ID)
		if err != nil {
			conn.enqueue(supportErrorEvent(event.RequestID, supportErrorCodeAuthRequired, "登录状态已失效，请刷新页面后重新登录"))
			return
		}
		ticket, message, err := s.control.CreateSupportTicket(user, controlplane.SupportTicketInput{Title: event.Title, Body: event.Body})
		if err != nil {
			conn.enqueue(supportErrorFromError(event.RequestID, err))
			return
		}
		conn.user = user
		userTicket := s.control.SupportTicketWithUnreadCount(user, ticket)
		adminTicket := s.control.SupportTicketWithUnreadCount(core.User{ID: "admin", Role: core.UserRoleAdmin}, ticket)
		userUnread := s.control.SupportUnreadCount(user)
		adminUnread := s.supportAdminUnreadCount()
		s.support.broadcastTicket(
			ticket,
			&message,
			userTicket,
			adminTicket,
			userUnread,
			adminUnread,
		)
		s.publishSupportMessageCreated(ticket, message, userTicket, adminTicket, userUnread, adminUnread)
	case "support.message.send":
		ticket, message, err := s.control.ReplySupportTicket(conn.user, event.TicketID, controlplane.SupportReplyInput{Body: event.Body})
		if err != nil {
			conn.enqueue(supportErrorFromError(event.RequestID, err))
			return
		}
		user, _ := s.control.GetUser(ticket.UserID)
		if strings.TrimSpace(user.ID) == "" {
			user = core.User{ID: ticket.UserID}
		}
		userTicket := s.control.SupportTicketWithUnreadCount(user, ticket)
		adminTicket := s.control.SupportTicketWithUnreadCount(core.User{ID: "admin", Role: core.UserRoleAdmin}, ticket)
		userUnread := s.supportUserUnreadCount(ticket.UserID)
		adminUnread := s.supportAdminUnreadCount()
		s.support.broadcastTicket(
			ticket,
			&message,
			userTicket,
			adminTicket,
			userUnread,
			adminUnread,
		)
		s.publishSupportMessageCreated(ticket, message, userTicket, adminTicket, userUnread, adminUnread)
	case "support.ticket.read":
		ticket, err := s.control.MarkSupportTicketRead(conn.user, event.TicketID)
		if err != nil {
			conn.enqueue(supportErrorFromError(event.RequestID, err))
			return
		}
		ticket = s.control.SupportTicketWithUnreadCount(conn.user, ticket)
		unread := s.control.SupportUnreadCount(conn.user)
		conn.enqueue(supportEvent{Type: "support.ticket.read", RequestID: event.RequestID, Ticket: &ticket, Unread: unread})
		s.publishSupportUnread(ticket.UserID, s.supportUserUnreadCount(ticket.UserID), s.supportAdminUnreadCount())
	case "support.ticket.load":
		ticket, err := s.control.GetSupportTicket(conn.user, event.TicketID)
		if err != nil {
			conn.enqueue(supportErrorFromError(event.RequestID, err))
			return
		}
		messages, err := s.control.ListSupportMessages(conn.user, ticket.ID, 200)
		if err != nil {
			conn.enqueue(supportErrorFromError(event.RequestID, err))
			return
		}
		ticket = s.control.SupportTicketWithUnreadCount(conn.user, ticket)
		conn.enqueue(supportEvent{Type: "support.ticket.loaded", RequestID: event.RequestID, Ticket: &ticket, Messages: messages, Unread: s.control.SupportUnreadCount(conn.user)})
	case "support.ticket.delete":
		if !conn.user.IsAdmin() {
			conn.enqueue(supportErrorEvent(event.RequestID, supportErrorCodeForbidden, "只有管理员可以删除客服会话"))
			return
		}
		ticket, err := s.control.DeleteSupportTicket(conn.user, event.TicketID)
		if err != nil {
			conn.enqueue(supportErrorFromError(event.RequestID, err))
			return
		}
		s.recordSupportDeleteAudit(conn.user, ticket, "ok", "")
		adminUnread := s.supportAdminUnreadCount()
		userUnread := s.supportUserUnreadCount(ticket.UserID)
		s.support.broadcastTicketDeleted(
			ticket,
			adminUnread,
			userUnread,
		)
		s.publishSupportUnread(ticket.UserID, userUnread, adminUnread)
	default:
		conn.enqueue(supportErrorEvent(event.RequestID, supportErrorCodeUnsupportedEvent, "不支持的客服消息"))
	}
}

func supportErrorFromError(requestID string, err error) supportEvent {
	if errors.Is(err, storage.ErrNotFound) {
		return supportErrorEvent(requestID, supportErrorCodeNotFound, "会话不存在，请重新发送消息")
	}
	message := strings.TrimSpace(err.Error())
	if strings.Contains(strings.ToLower(message), "foreign key constraint") || strings.Contains(message, "FOREIGN KEY constraint failed") {
		return supportErrorEvent(requestID, supportErrorCodeDataConflict, "会话数据状态异常，请刷新页面后重试")
	}
	if message == "" {
		message = "客服请求失败"
	}
	return supportErrorEvent(requestID, supportErrorCodeRequestFailed, message)
}

func supportErrorEvent(requestID, code, message string) supportEvent {
	return supportEvent{Type: "support.error", RequestID: requestID, ErrorCode: code, Error: message}
}

func (s *Server) supportBootstrapEvent(user core.User, admin bool) supportEvent {
	filter := controlplane.SupportTicketFilter{Page: 1, PageSize: 50}
	if !admin {
		filter.UserID = user.ID
	}
	page := s.control.ListSupportTickets(user, filter)
	return supportEvent{
		Type:    "support.bootstrap",
		Tickets: page.Tickets,
		Unread:  s.control.SupportUnreadCount(user),
	}
}

func (s *Server) supportUserUnreadCount(userID string) int {
	user, err := s.control.GetUser(strings.TrimSpace(userID))
	if err != nil {
		return 0
	}
	return s.control.SupportUnreadCount(user)
}

func (s *Server) supportAdminUnreadCount() int {
	return s.control.SupportUnreadCount(core.User{ID: "admin", Role: core.UserRoleAdmin})
}

func (s *Server) recordSupportDeleteAudit(actor core.User, ticket core.SupportTicket, status string, message string) {
	if s.control == nil {
		return
	}
	username := strings.TrimSpace(actor.Username)
	if username == "" {
		username = strings.TrimSpace(actor.ID)
	}
	if username == "" {
		username = "system"
	}
	if strings.TrimSpace(message) == "" {
		message = "deleted_by=" + strings.TrimSpace(actor.ID)
	}
	_ = s.control.AppendAdminAudit(username, "support.delete", "support_ticket", ticket.ID, ticket.Title, status, message)
	s.publishAuditUpdated()
}

func supportEventJSON(event supportEvent) string {
	body, err := json.Marshal(event)
	if err != nil {
		return "{}"
	}
	return string(body)
}
