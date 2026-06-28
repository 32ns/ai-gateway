package controlplane

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"
)

const (
	supportTicketPageSizeDefault = 50
	supportMessageLimitDefault   = 200
	supportTitleMaxRunes         = 120
	supportBodyMaxRunes          = 8000
)

type SupportTicketInput struct {
	Title string
	Body  string
}

type SupportReplyInput struct {
	Body string
}

type SupportTicketFilter struct {
	UserID   string
	Status   core.SupportTicketStatus
	Query    string
	Page     int
	PageSize int
}

type SupportTicketPage struct {
	Tickets  []core.SupportTicket
	Total    int
	Page     int
	PageSize int
	HasPrev  bool
	PrevPage int
	HasNext  bool
	NextPage int
}

func (s *Service) CreateSupportTicket(user core.User, input SupportTicketInput) (core.SupportTicket, core.SupportMessage, error) {
	if strings.TrimSpace(user.ID) == "" {
		return core.SupportTicket{}, core.SupportMessage{}, fmt.Errorf("authentication required")
	}
	title, err := normalizeSupportTitle(input.Title)
	if err != nil {
		return core.SupportTicket{}, core.SupportMessage{}, err
	}
	body, err := normalizeSupportBody(input.Body)
	if err != nil {
		return core.SupportTicket{}, core.SupportMessage{}, err
	}
	if existing, ok := s.latestReusableSupportTicket(user.ID); ok {
		return s.ReplySupportTicket(user, existing.ID, SupportReplyInput{Body: body})
	}
	now := time.Now().UTC()
	ticketID := generateSupportTicketID()
	message := core.SupportMessage{
		ID:        generateSupportMessageID(),
		TicketID:  ticketID,
		ActorID:   user.ID,
		ActorRole: user.Role,
		Body:      body,
		CreatedAt: now,
	}
	ticket := core.SupportTicket{
		ID:               ticketID,
		UserID:           user.ID,
		Username:         strings.TrimSpace(user.Username),
		Title:            title,
		Status:           core.SupportTicketPendingAdmin,
		LastMessage:      body,
		LastActorID:      user.ID,
		LastReadByUserAt: &now,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := s.repo.CreateSupportTicket(ticket, message); err != nil {
		return core.SupportTicket{}, core.SupportMessage{}, err
	}
	return ticket, message, nil
}

func (s *Service) latestReusableSupportTicket(userID string) (core.SupportTicket, bool) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return core.SupportTicket{}, false
	}
	tickets, _ := s.repo.ListSupportTicketsPage(storage.SupportTicketQuery{
		UserID: userID,
		Limit:  50,
	})
	for _, ticket := range tickets {
		if ticket.Status != core.SupportTicketClosed {
			return ticket, true
		}
	}
	return core.SupportTicket{}, false
}

func (s *Service) ReplySupportTicket(actor core.User, ticketID string, input SupportReplyInput) (core.SupportTicket, core.SupportMessage, error) {
	ticket, err := s.GetSupportTicket(actor, ticketID)
	if err != nil {
		return core.SupportTicket{}, core.SupportMessage{}, err
	}
	body, err := normalizeSupportBody(input.Body)
	if err != nil {
		return core.SupportTicket{}, core.SupportMessage{}, err
	}
	now := time.Now().UTC()
	message := core.SupportMessage{
		ID:        generateSupportMessageID(),
		TicketID:  ticket.ID,
		ActorID:   actor.ID,
		ActorRole: actor.Role,
		Body:      body,
		CreatedAt: now,
	}
	ticket.LastMessage = body
	ticket.LastActorID = actor.ID
	ticket.UpdatedAt = now
	if actor.IsAdmin() {
		ticket.Status = core.SupportTicketPendingUser
		ticket.LastReadByAdminAt = &now
	} else {
		if ticket.Status == core.SupportTicketClosed {
			return core.SupportTicket{}, core.SupportMessage{}, fmt.Errorf("support ticket is closed")
		}
		ticket.Status = core.SupportTicketPendingAdmin
		ticket.LastReadByUserAt = &now
	}
	updated, err := s.repo.AppendSupportMessage(ticket.ID, message, ticket)
	if err != nil {
		return core.SupportTicket{}, core.SupportMessage{}, err
	}
	return updated, message, nil
}

func (s *Service) GetSupportTicket(actor core.User, ticketID string) (core.SupportTicket, error) {
	ticket, err := s.repo.GetSupportTicket(strings.TrimSpace(ticketID))
	if err != nil {
		return core.SupportTicket{}, err
	}
	if !actor.IsAdmin() && strings.TrimSpace(ticket.UserID) != strings.TrimSpace(actor.ID) {
		return core.SupportTicket{}, storage.ErrNotFound
	}
	return ticket, nil
}

func (s *Service) ListSupportTickets(actor core.User, filter SupportTicketFilter) SupportTicketPage {
	if filter.Page < 1 {
		filter.Page = 1
	}
	if filter.PageSize <= 0 {
		filter.PageSize = supportTicketPageSizeDefault
	}
	if filter.PageSize > 200 {
		filter.PageSize = 200
	}
	userID := strings.TrimSpace(filter.UserID)
	if !actor.IsAdmin() {
		userID = actor.ID
	}
	offset := (filter.Page - 1) * filter.PageSize
	items, total := s.repo.ListSupportTicketsPage(storage.SupportTicketQuery{
		UserID: userID,
		Status: filter.Status,
		Query:  filter.Query,
		Offset: offset,
		Limit:  filter.PageSize,
	})
	page := SupportTicketPage{
		Tickets:  items,
		Total:    total,
		Page:     filter.Page,
		PageSize: filter.PageSize,
	}
	s.applySupportUnreadCounts(actor, page.Tickets)
	if filter.Page > 1 && total > 0 {
		page.HasPrev = true
		page.PrevPage = filter.Page - 1
	}
	if offset+len(items) < total {
		page.HasNext = true
		page.NextPage = filter.Page + 1
	}
	return page
}

func (s *Service) applySupportUnreadCounts(actor core.User, tickets []core.SupportTicket) []core.SupportTicket {
	if len(tickets) == 0 {
		return tickets
	}
	for i := range tickets {
		if supportTicketUnreadForActor(tickets[i], actor) {
			tickets[i].UnreadCount = 1
			continue
		}
		tickets[i].UnreadCount = 0
	}
	return tickets
}

func supportTicketUnreadForActor(ticket core.SupportTicket, actor core.User) bool {
	if strings.TrimSpace(ticket.LastActorID) == "" {
		return false
	}
	if actor.IsAdmin() {
		if ticket.Status == core.SupportTicketClosed || strings.TrimSpace(ticket.LastActorID) != strings.TrimSpace(ticket.UserID) {
			return false
		}
		return ticket.LastReadByAdminAt == nil || ticket.UpdatedAt.After(*ticket.LastReadByAdminAt)
	}
	if strings.TrimSpace(ticket.UserID) != strings.TrimSpace(actor.ID) || ticket.LastActorID == actor.ID {
		return false
	}
	return ticket.LastReadByUserAt == nil || ticket.UpdatedAt.After(*ticket.LastReadByUserAt)
}

func (s *Service) ListSupportMessages(actor core.User, ticketID string, limit int) ([]core.SupportMessage, error) {
	ticket, err := s.GetSupportTicket(actor, ticketID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = supportMessageLimitDefault
	}
	return s.repo.ListSupportMessages(ticket.ID, limit), nil
}

func (s *Service) MarkSupportTicketRead(actor core.User, ticketID string) (core.SupportTicket, error) {
	if _, err := s.GetSupportTicket(actor, ticketID); err != nil {
		return core.SupportTicket{}, err
	}
	return s.repo.MarkSupportTicketRead(strings.TrimSpace(ticketID), actor, time.Now().UTC())
}

func (s *Service) SupportUnreadCount(actor core.User) int {
	if strings.TrimSpace(actor.ID) == "" {
		return 0
	}
	return s.repo.SupportUnreadCount(actor)
}

func (s *Service) SupportTicketWithUnreadCount(actor core.User, ticket core.SupportTicket) core.SupportTicket {
	if supportTicketUnreadForActor(ticket, actor) {
		ticket.UnreadCount = 1
	} else {
		ticket.UnreadCount = 0
	}
	return ticket
}

func (s *Service) CloseSupportTicket(actor core.User, ticketID string) (core.SupportTicket, error) {
	ticket, err := s.GetSupportTicket(actor, ticketID)
	if err != nil {
		return core.SupportTicket{}, err
	}
	if !actor.IsAdmin() && ticket.Status == core.SupportTicketClosed {
		return ticket, nil
	}
	now := time.Now().UTC()
	ticket.Status = core.SupportTicketClosed
	ticket.ClosedAt = &now
	ticket.UpdatedAt = now
	ticket.LastActorID = actor.ID
	updated, err := s.repo.AppendSupportMessage(ticket.ID, core.SupportMessage{
		ID:        generateSupportMessageID(),
		TicketID:  ticket.ID,
		ActorID:   actor.ID,
		ActorRole: actor.Role,
		Body:      supportSystemCloseBody(actor),
		CreatedAt: now,
	}, ticket)
	if err != nil {
		return core.SupportTicket{}, err
	}
	return updated, nil
}

func (s *Service) DeleteSupportTicket(actor core.User, ticketID string) (core.SupportTicket, error) {
	if !actor.IsAdmin() {
		return core.SupportTicket{}, fmt.Errorf("authentication required")
	}
	ticket, err := s.GetSupportTicket(actor, ticketID)
	if err != nil {
		return core.SupportTicket{}, err
	}
	if err := s.repo.DeleteSupportTicket(strings.TrimSpace(ticketID)); err != nil {
		return core.SupportTicket{}, err
	}
	return ticket, nil
}

func normalizeSupportTitle(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("support title is required")
	}
	return truncateRunes(value, supportTitleMaxRunes), nil
}

func normalizeSupportBody(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("support message is required")
	}
	if len([]rune(value)) > supportBodyMaxRunes {
		return "", fmt.Errorf("support message is too long")
	}
	return value, nil
}

func supportSystemCloseBody(actor core.User) string {
	if actor.IsAdmin() {
		return "客服已关闭会话"
	}
	return "用户已关闭会话"
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func generateSupportTicketID() string {
	return supportID("sup")
}

func generateSupportMessageID() string {
	return supportID("smsg")
}

func supportID(prefix string) string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(buf)
}
