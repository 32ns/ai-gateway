package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func (r *SQLiteRepository) CreateSupportTicket(ticket core.SupportTicket, firstMessage core.SupportMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	ticket = normalizeSupportTicketForStorage(ticket)
	firstMessage = normalizeSupportMessageForStorage(firstMessage)
	if ticket.ID == "" || ticket.UserID == "" || ticket.Title == "" || firstMessage.ID == "" || firstMessage.TicketID != ticket.ID || firstMessage.Body == "" {
		return fmt.Errorf("support ticket id, user, title, and first message are required")
	}
	if _, err := r.getPayloadByID("users", ticket.UserID); err != nil {
		return err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertSupportTicketTx(tx, ticket); err != nil {
		return err
	}
	if err := insertSupportMessageTx(tx, firstMessage); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *SQLiteRepository) GetSupportTicket(id string) (core.SupportTicket, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var payload string
	err := r.db.QueryRow(`SELECT payload FROM support_tickets WHERE id = ?`, strings.TrimSpace(id)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return core.SupportTicket{}, ErrNotFound
	}
	if err != nil {
		return core.SupportTicket{}, err
	}
	return supportTicketFromPayload(payload)
}

func (r *SQLiteRepository) ListSupportTicketsPage(query SupportTicketQuery) ([]core.SupportTicket, int) {
	query = normalizeSupportTicketQuery(query)
	r.mu.RLock()
	defer r.mu.RUnlock()

	where, args := supportTicketWhere(query)
	total := 0
	_ = r.db.QueryRow(`SELECT COUNT(*) FROM support_tickets `+where, args...).Scan(&total)
	args = append(args, query.Limit, query.Offset)
	rows, err := r.db.Query(`SELECT payload FROM support_tickets `+where+` ORDER BY updated_at_ns DESC, id ASC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, total
	}
	defer rows.Close()
	out := make([]core.SupportTicket, 0, query.Limit)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		ticket, err := supportTicketFromPayload(payload)
		if err == nil {
			out = append(out, ticket)
		}
	}
	return out, total
}

func (r *SQLiteRepository) ListSupportMessages(ticketID string, limit int) []core.SupportMessage {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ticketID = strings.TrimSpace(ticketID)
	if limit <= 0 {
		limit = 200
	}
	rows, err := r.db.Query(`
		SELECT payload FROM (
			SELECT payload, created_at_ns, id
			FROM support_messages
			WHERE ticket_id = ?
			ORDER BY created_at_ns DESC, id DESC
			LIMIT ?
		) rows
		ORDER BY created_at_ns ASC, id ASC`, ticketID, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]core.SupportMessage, 0, limit)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		message, err := supportMessageFromPayload(payload)
		if err == nil {
			out = append(out, message)
		}
	}
	return out
}

func (r *SQLiteRepository) AppendSupportMessage(ticketID string, message core.SupportMessage, ticket core.SupportTicket) (core.SupportTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	ticketID = strings.TrimSpace(ticketID)
	existing, err := r.getSupportTicketLocked(ticketID)
	if err != nil {
		return core.SupportTicket{}, err
	}
	message = normalizeSupportMessageForStorage(message)
	ticket = normalizeSupportTicketForStorage(ticket)
	if message.ID == "" || message.TicketID != ticketID || message.Body == "" || ticket.ID != ticketID || existing.UserID != ticket.UserID {
		return core.SupportTicket{}, fmt.Errorf("support message and ticket update are required")
	}
	tx, err := r.db.Begin()
	if err != nil {
		return core.SupportTicket{}, err
	}
	defer tx.Rollback()
	if err := insertSupportMessageTx(tx, message); err != nil {
		return core.SupportTicket{}, err
	}
	if err := updateSupportTicketTx(tx, ticket); err != nil {
		return core.SupportTicket{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.SupportTicket{}, err
	}
	return ticket, nil
}

func (r *SQLiteRepository) DeleteSupportTicket(ticketID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	ticketID = strings.TrimSpace(ticketID)
	if ticketID == "" {
		return ErrNotFound
	}
	res, err := r.db.Exec(`DELETE FROM support_tickets WHERE id = ?`, ticketID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *SQLiteRepository) MarkSupportTicketRead(ticketID string, user core.User, readAt time.Time) (core.SupportTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	ticket, err := r.getSupportTicketLocked(strings.TrimSpace(ticketID))
	if err != nil {
		return core.SupportTicket{}, err
	}
	if !user.IsAdmin() && ticket.UserID != strings.TrimSpace(user.ID) {
		return core.SupportTicket{}, ErrNotFound
	}
	if readAt.IsZero() {
		readAt = time.Now().UTC()
	} else {
		readAt = readAt.UTC()
	}
	if user.IsAdmin() {
		ticket.LastReadByAdminAt = &readAt
	} else {
		ticket.LastReadByUserAt = &readAt
	}
	if err := updateSupportTicketTx(r.db, ticket); err != nil {
		return core.SupportTicket{}, err
	}
	return ticket, nil
}

func (r *SQLiteRepository) SupportUnreadCount(user core.User) int {
	query := SupportTicketQuery{}
	if !user.IsAdmin() {
		query.UserID = user.ID
	}
	tickets, _ := r.ListSupportTicketsPage(SupportTicketQuery{UserID: query.UserID, Limit: 200})
	count := 0
	for _, ticket := range tickets {
		if supportTicketUnreadForUser(ticket, user) {
			count++
		}
	}
	return count
}

type supportSQLExecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func insertSupportTicketTx(exec supportSQLExecutor, ticket core.SupportTicket) error {
	payload, err := json.Marshal(ticket)
	if err != nil {
		return err
	}
	_, err = exec.Exec(`
		INSERT INTO support_tickets(id, user_id, username, status, title, last_message, last_actor_id, last_read_by_user_at_ns, last_read_by_admin_at_ns, created_at_ns, updated_at_ns, closed_at_ns, payload)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ticket.ID, ticket.UserID, ticket.Username, string(ticket.Status), ticket.Title, ticket.LastMessage, ticket.LastActorID,
		sqliteTimeNS(valueTime(ticket.LastReadByUserAt)), sqliteTimeNS(valueTime(ticket.LastReadByAdminAt)), sqliteTimeNS(ticket.CreatedAt), sqliteTimeNS(ticket.UpdatedAt), sqliteTimeNS(valueTime(ticket.ClosedAt)), string(payload))
	return err
}

func updateSupportTicketTx(exec supportSQLExecutor, ticket core.SupportTicket) error {
	payload, err := json.Marshal(ticket)
	if err != nil {
		return err
	}
	_, err = exec.Exec(`
		UPDATE support_tickets
		SET status = ?, title = ?, last_message = ?, last_actor_id = ?, last_read_by_user_at_ns = ?, last_read_by_admin_at_ns = ?, updated_at_ns = ?, closed_at_ns = ?, payload = ?
		WHERE id = ?`,
		string(ticket.Status), ticket.Title, ticket.LastMessage, ticket.LastActorID,
		sqliteTimeNS(valueTime(ticket.LastReadByUserAt)), sqliteTimeNS(valueTime(ticket.LastReadByAdminAt)), sqliteTimeNS(ticket.UpdatedAt), sqliteTimeNS(valueTime(ticket.ClosedAt)), string(payload), ticket.ID)
	return err
}

func insertSupportMessageTx(exec supportSQLExecutor, message core.SupportMessage) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	_, err = exec.Exec(`
		INSERT INTO support_messages(id, ticket_id, actor_id, actor_role, body, created_at_ns, payload)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		message.ID, message.TicketID, message.ActorID, string(message.ActorRole), message.Body, sqliteTimeNS(message.CreatedAt), string(payload))
	return err
}

func (r *SQLiteRepository) getSupportTicketLocked(id string) (core.SupportTicket, error) {
	var payload string
	err := r.db.QueryRow(`SELECT payload FROM support_tickets WHERE id = ?`, strings.TrimSpace(id)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return core.SupportTicket{}, ErrNotFound
	}
	if err != nil {
		return core.SupportTicket{}, err
	}
	return supportTicketFromPayload(payload)
}

func supportTicketWhere(query SupportTicketQuery) (string, []any) {
	clauses := make([]string, 0, 3)
	args := make([]any, 0, 3)
	if query.UserID != "" {
		clauses = append(clauses, "user_id = ?")
		args = append(args, query.UserID)
	}
	if query.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, string(query.Status))
	}
	if query.Query != "" {
		clauses = append(clauses, "(lower(id) LIKE ? OR lower(user_id) LIKE ? OR lower(username) LIKE ? OR lower(title) LIKE ? OR lower(last_message) LIKE ?)")
		pattern := "%" + query.Query + "%"
		args = append(args, pattern, pattern, pattern, pattern, pattern)
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

func supportTicketFromPayload(payload string) (core.SupportTicket, error) {
	var ticket core.SupportTicket
	if err := json.Unmarshal([]byte(payload), &ticket); err != nil {
		return core.SupportTicket{}, err
	}
	return cloneSupportTicket(ticket), nil
}

func supportMessageFromPayload(payload string) (core.SupportMessage, error) {
	var message core.SupportMessage
	if err := json.Unmarshal([]byte(payload), &message); err != nil {
		return core.SupportMessage{}, err
	}
	return cloneSupportMessage(message), nil
}
