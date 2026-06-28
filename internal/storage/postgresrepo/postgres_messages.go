package postgresrepo

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func (r *PostgresRepository) CreateSiteMessage(message core.SiteMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	message.ID = strings.TrimSpace(message.ID)
	message.Title = strings.TrimSpace(message.Title)
	message.Body = strings.TrimSpace(message.Body)
	message.CreatedBy = strings.TrimSpace(message.CreatedBy)
	if message.ID == "" || message.Title == "" || message.Body == "" {
		return fmt.Errorf("message id, title, and body are required")
	}
	now := time.Now().UTC()
	if message.CreatedAt.IsZero() {
		message.CreatedAt = now
	}
	if message.UpdatedAt.IsZero() {
		message.UpdatedAt = message.CreatedAt
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(`INSERT INTO site_messages(id, enabled, payload, created_at_ns, updated_at_ns) VALUES(?, ?, ?, ?, ?)`, message.ID, boolParam(message.Enabled), string(payload), sqliteTimeNS(message.CreatedAt), sqliteTimeNS(message.UpdatedAt))
	return err
}

func (r *PostgresRepository) UpdateSiteMessage(message core.SiteMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, err := r.getSiteMessageLocked(strings.TrimSpace(message.ID))
	if err != nil {
		return err
	}
	message.Title = strings.TrimSpace(message.Title)
	message.Body = strings.TrimSpace(message.Body)
	if message.Title == "" || message.Body == "" {
		return fmt.Errorf("message title and body are required")
	}
	message.ID = existing.ID
	message.CreatedBy = existing.CreatedBy
	message.CreatedAt = existing.CreatedAt
	message.UpdatedAt = time.Now().UTC()
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(`UPDATE site_messages SET enabled = ?, payload = ?, updated_at_ns = ? WHERE id = ?`, boolParam(message.Enabled), string(payload), sqliteTimeNS(message.UpdatedAt), message.ID)
	return err
}

func (r *PostgresRepository) GetSiteMessage(id string) (core.SiteMessage, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.getSiteMessageLocked(strings.TrimSpace(id))
}

func (r *PostgresRepository) DeleteSiteMessage(id string) error {
	return r.deleteByID("site_messages", id)
}

func (r *PostgresRepository) ListSiteMessages() []core.SiteMessage {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`SELECT payload FROM site_messages ORDER BY created_at_ns DESC, id ASC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]core.SiteMessage, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		var message core.SiteMessage
		if err := json.Unmarshal([]byte(payload), &message); err != nil {
			continue
		}
		out = append(out, cloneSiteMessage(message))
	}
	return out
}

func (r *PostgresRepository) ListSiteMessageDeliveries(userID string, includeDisabled bool) []core.SiteMessageDelivery {
	r.mu.RLock()
	defer r.mu.RUnlock()

	userID = strings.TrimSpace(userID)
	where := ""
	if !includeDisabled {
		where = "WHERE m.enabled"
	}
	rows, err := r.db.Query(`
		SELECT m.payload, COALESCE(r.read_at_ns, 0)
		FROM site_messages m
		LEFT JOIN site_message_reads r ON r.message_id = m.id AND r.user_id = ?
		`+where+`
		ORDER BY m.created_at_ns DESC, m.id ASC`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]core.SiteMessageDelivery, 0)
	for rows.Next() {
		var payload string
		var readNS int64
		if err := rows.Scan(&payload, &readNS); err != nil {
			continue
		}
		var message core.SiteMessage
		if err := json.Unmarshal([]byte(payload), &message); err != nil {
			continue
		}
		delivery := core.SiteMessageDelivery{Message: cloneSiteMessage(message)}
		if readNS > 0 {
			delivery.Read = true
			value := timeFromNS(readNS)
			delivery.ReadAt = &value
		}
		out = append(out, delivery)
	}
	return out
}

func (r *PostgresRepository) ListSiteMessageDeliveriesPage(userID string, includeDisabled bool, offset, limit int) ([]core.SiteMessageDelivery, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 25
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	userID = strings.TrimSpace(userID)
	where := ""
	if !includeDisabled {
		where = "WHERE m.enabled"
	}
	total := 0
	_ = r.db.QueryRow(`SELECT COUNT(*) FROM site_messages m ` + where).Scan(&total)
	rows, err := r.db.Query(`
		SELECT m.payload, COALESCE(r.read_at_ns, 0)
		FROM site_messages m
		LEFT JOIN site_message_reads r ON r.message_id = m.id AND r.user_id = ?
		`+where+`
		ORDER BY m.created_at_ns DESC, m.id ASC
		LIMIT ? OFFSET ?`, userID, limit, offset)
	if err != nil {
		return nil, total
	}
	defer rows.Close()

	out := make([]core.SiteMessageDelivery, 0, limit)
	for rows.Next() {
		var payload string
		var readNS int64
		if err := rows.Scan(&payload, &readNS); err != nil {
			continue
		}
		delivery, ok := siteMessageDeliveryFromPayload(payload, readNS)
		if !ok {
			continue
		}
		out = append(out, delivery)
	}
	return out, total
}

func (r *PostgresRepository) ListVisibleSiteMessageDeliveriesPage(userID string, accountGroups []string, offset, limit int) ([]core.SiteMessageDelivery, int) {
	userID, accountGroups, offset, limit = normalizeSiteMessageVisibilityInputs(userID, accountGroups, offset, limit)
	if userID == "" {
		return nil, 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	whereSQL, whereArgs := postgresSiteMessageVisibilityWhere(userID, accountGroups)
	total := 0
	_ = r.db.QueryRow(`SELECT COUNT(*) FROM site_messages m WHERE `+whereSQL, whereArgs...).Scan(&total)

	args := append([]any(nil), userID)
	args = append(args, whereArgs...)
	args = append(args, limit, offset)
	rows, err := r.db.Query(`
		SELECT m.payload, COALESCE(r.read_at_ns, 0)
		FROM site_messages m
		LEFT JOIN site_message_reads r ON r.message_id = m.id AND r.user_id = ?
		WHERE `+whereSQL+`
		ORDER BY m.created_at_ns DESC, m.id ASC
		LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, total
	}
	defer rows.Close()

	out := make([]core.SiteMessageDelivery, 0, limit)
	for rows.Next() {
		var payload string
		var readNS int64
		if err := rows.Scan(&payload, &readNS); err != nil {
			continue
		}
		delivery, ok := siteMessageDeliveryFromPayload(payload, readNS)
		if !ok {
			continue
		}
		out = append(out, delivery)
	}
	return out, total
}

func siteMessageDeliveryFromPayload(payload string, readNS int64) (core.SiteMessageDelivery, bool) {
	var message core.SiteMessage
	if err := json.Unmarshal([]byte(payload), &message); err != nil {
		return core.SiteMessageDelivery{}, false
	}
	delivery := core.SiteMessageDelivery{Message: cloneSiteMessage(message)}
	if readNS > 0 {
		delivery.Read = true
		value := timeFromNS(readNS)
		delivery.ReadAt = &value
	}
	return delivery, true
}

func (r *PostgresRepository) MarkSiteMessageRead(messageID, userID string, readAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	messageID = strings.TrimSpace(messageID)
	userID = strings.TrimSpace(userID)
	if _, err := r.getSiteMessageLocked(messageID); err != nil {
		return err
	}
	if _, err := r.getPayloadByID("users", userID); err != nil {
		return err
	}
	if readAt.IsZero() {
		readAt = time.Now().UTC()
	}
	_, err := r.db.Exec(`INSERT INTO site_message_reads(message_id, user_id, read_at_ns) VALUES(?, ?, ?) ON CONFLICT(message_id, user_id) DO UPDATE SET read_at_ns = excluded.read_at_ns`, messageID, userID, sqliteTimeNS(readAt.UTC()))
	return err
}

func (r *PostgresRepository) ClearSiteMessageReads(messageID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	messageID = strings.TrimSpace(messageID)
	if _, err := r.getSiteMessageLocked(messageID); err != nil {
		return err
	}
	_, err := r.db.Exec(`DELETE FROM site_message_reads WHERE message_id = ?`, messageID)
	return err
}

func (r *PostgresRepository) SiteMessageUnreadCount(userID string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var count int
	if err := r.db.QueryRow(`
		SELECT COUNT(*)
		FROM site_messages m
		LEFT JOIN site_message_reads r ON r.message_id = m.id AND r.user_id = ?
		WHERE m.enabled
			AND COALESCE(((m.payload::jsonb)->>'PublicPopup')::boolean, false) = false
			AND r.message_id IS NULL
	`, strings.TrimSpace(userID)).Scan(&count); err != nil {
		return 0
	}
	return count
}

func (r *PostgresRepository) VisibleSiteMessageUnreadCount(userID string, accountGroups []string) int {
	userID, accountGroups, _, _ = normalizeSiteMessageVisibilityInputs(userID, accountGroups, 0, 25)
	if userID == "" {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	whereSQL, whereArgs := postgresSiteMessageVisibilityWhere(userID, accountGroups)
	args := append([]any{userID}, whereArgs...)
	var count int
	if err := r.db.QueryRow(`
		SELECT COUNT(*)
		FROM site_messages m
		LEFT JOIN site_message_reads r ON r.message_id = m.id AND r.user_id = ?
		WHERE `+whereSQL+` AND r.message_id IS NULL
	`, args...).Scan(&count); err != nil {
		return 0
	}
	return count
}

func postgresSiteMessageVisibilityWhere(userID string, accountGroups []string) (string, []any) {
	targetUsersJSON := `CASE WHEN jsonb_typeof((m.payload::jsonb)->'TargetUserIDs') = 'array' THEN (m.payload::jsonb)->'TargetUserIDs' ELSE '[]'::jsonb END`
	targetGroupsJSON := `CASE WHEN jsonb_typeof((m.payload::jsonb)->'TargetAccountGroups') = 'array' THEN (m.payload::jsonb)->'TargetAccountGroups' ELSE '[]'::jsonb END`
	visibility := []string{
		`
			(jsonb_array_length(` + targetUsersJSON + `) = 0
				AND jsonb_array_length(` + targetGroupsJSON + `) = 0)
			OR EXISTS (
				SELECT 1
				FROM jsonb_array_elements_text(` + targetUsersJSON + `) target_user(value)
				WHERE trim(target_user.value) = ?
			)`,
	}
	args := []any{userID}
	if len(accountGroups) > 0 {
		visibility = append(visibility, `
			OR EXISTS (
				SELECT 1
				FROM jsonb_array_elements_text(`+targetGroupsJSON+`) target_group(value)
				WHERE lower(trim(target_group.value)) IN (`+placeholders(len(accountGroups))+`)
			)`)
		for _, group := range accountGroups {
			args = append(args, group)
		}
	}
	return `m.enabled AND COALESCE(((m.payload::jsonb)->>'PublicPopup')::boolean, false) = false AND (` + strings.Join(visibility, " ") + `)`, args
}

func normalizeSiteMessageVisibilityInputs(userID string, accountGroups []string, offset, limit int) (string, []string, int, int) {
	userID = strings.TrimSpace(userID)
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 25
	}
	seen := make(map[string]struct{}, len(accountGroups))
	groups := make([]string, 0, len(accountGroups))
	for _, group := range accountGroups {
		group = strings.ToLower(strings.TrimSpace(group))
		if group == "" {
			continue
		}
		if _, ok := seen[group]; ok {
			continue
		}
		seen[group] = struct{}{}
		groups = append(groups, group)
	}
	return userID, groups, offset, limit
}
