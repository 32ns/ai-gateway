package postgresrepo

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func (r *PostgresRepository) AppendAudit(event core.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	copyEvent := cloneAuditEvent(event)
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	if err := insertAuditTx(tx, copyEvent); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := r.maybeTrimAuditTx(tx, time.Now().UTC(), 1); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := r.maybeTrimGatewayAuditTx(tx, time.Now().UTC(), 1); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (r *PostgresRepository) AppendAuditBatch(events []core.AuditEvent) error {
	if len(events) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(insertAuditSQL)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, event := range events {
		if err := insertAuditStmt(tx, stmt, cloneAuditEvent(event)); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return err
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := r.maybeTrimAuditTx(tx, time.Now().UTC(), len(events)); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := r.maybeTrimGatewayAuditTx(tx, time.Now().UTC(), len(events)); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

const insertAuditSQL = `INSERT INTO audit(event_id, created_at, kind, status, actor_text, resource_text, payload, summary_payload) VALUES(?, ?, ?, ?, ?, ?, ?, ?) RETURNING seq`

func insertAuditTx(tx *sqlTx, event core.AuditEvent) error {
	eventID, createdAt, kind, status, actorText, resourceText, payload, summaryPayload, err := auditInsertValues(event)
	if err != nil {
		return err
	}
	var seq int64
	if err := tx.QueryRow(insertAuditSQL, eventID, createdAt, kind, status, actorText, resourceText, payload, summaryPayload).Scan(&seq); err != nil {
		return err
	}
	return insertAuditTermsTx(tx, seq, actorText, resourceText)
}

func insertAuditStmt(tx *sqlTx, stmt *sql.Stmt, event core.AuditEvent) error {
	eventID, createdAt, kind, status, actorText, resourceText, payload, summaryPayload, err := auditInsertValues(event)
	if err != nil {
		return err
	}
	var seq int64
	if err := stmt.QueryRow(eventID, createdAt, kind, status, actorText, resourceText, payload, summaryPayload).Scan(&seq); err != nil {
		return err
	}
	return insertAuditTermsTx(tx, seq, actorText, resourceText)
}

func (r *PostgresRepository) compactIfWasteHighLocked() error {
	return nil
}

func auditInsertValues(event core.AuditEvent) (eventID, createdAt, kind, status, actorText, resourceText, payload, summaryPayload string, err error) {
	payloadBytes, err := json.Marshal(event)
	if err != nil {
		return "", "", "", "", "", "", "", "", err
	}
	summaryBytes, err := json.Marshal(auditSummaryEvent(event))
	if err != nil {
		return "", "", "", "", "", "", "", "", err
	}
	summaryPayload = string(summaryBytes)
	if summaryPayload == string(payloadBytes) {
		summaryPayload = ""
	}
	kind, status, actorText, resourceText = auditIndexValues(event)
	return event.ID,
		event.CreatedAt.UTC().Format(time.RFC3339Nano),
		kind,
		status,
		actorText,
		resourceText,
		string(payloadBytes),
		summaryPayload,
		nil
}

func (r *PostgresRepository) ListAudit(limit int) []core.AuditEvent {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.listAuditPayloadsLocked(`payload`, limit)
}

func (r *PostgresRepository) ListAuditSummaries(limit int) []core.AuditEvent {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.listAuditPayloadsLocked(`CASE WHEN summary_payload = '' THEN payload ELSE summary_payload END`, limit)
}

func (r *PostgresRepository) ListAuditSummariesPage(query AuditQuery) ([]core.AuditEvent, int) {
	query = normalizeAuditQuery(query)
	r.mu.RLock()
	defer r.mu.RUnlock()

	where, args := auditQueryWhere(query)
	countQuery := `SELECT COUNT(*) FROM audit` + where
	total := 0
	if err := r.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, 0
	}

	selectArgs := append([]any(nil), args...)
	selectArgs = append(selectArgs, query.Limit, query.Offset)
	rows, err := r.db.Query(
		`SELECT CASE WHEN summary_payload = '' THEN payload ELSE summary_payload END FROM audit`+where+` ORDER BY seq DESC LIMIT ? OFFSET ?`,
		selectArgs...,
	)
	if err != nil {
		return nil, total
	}
	defer rows.Close()

	out := make([]core.AuditEvent, 0, query.Limit)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		var event core.AuditEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		out = append(out, auditSummaryEvent(event))
	}
	return out, total
}

func auditQueryWhere(query AuditQuery) (string, []any) {
	clauses := make([]string, 0, 4)
	args := make([]any, 0, 4)
	if query.Kind != "" {
		clauses = append(clauses, `kind = ?`)
		args = append(args, string(query.Kind))
	}
	if query.Status != "" {
		clauses = append(clauses, `status = ?`)
		args = append(args, query.Status)
	}
	if query.Actor != "" {
		clause, clauseArgs := auditTermClause("actor", query.Actor)
		clauses = append(clauses, clause)
		args = append(args, clauseArgs...)
	}
	if query.Resource != "" {
		clause, clauseArgs := auditTermClause("resource", query.Resource)
		clauses = append(clauses, clause)
		args = append(args, clauseArgs...)
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func auditTermClause(termType, text string) (string, []any) {
	terms := auditIndexTerms(text)
	if len(terms) == 0 {
		return `0 = 1`, nil
	}
	args := make([]any, 0, len(terms)+2)
	args = append(args, termType)
	for _, term := range terms {
		args = append(args, term)
	}
	args = append(args, len(terms))
	return `seq IN (
		SELECT seq
		FROM audit_terms
		WHERE term_type = ? AND term IN (` + placeholders(len(terms)) + `)
		GROUP BY seq
		HAVING COUNT(DISTINCT term) = ?
	)`, args
}

func (r *PostgresRepository) listAuditPayloadsLocked(payloadExpr string, limit int) []core.AuditEvent {
	query := fmt.Sprintf(`SELECT %s FROM audit ORDER BY seq DESC`, payloadExpr)
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]core.AuditEvent, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		var event core.AuditEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		out = append(out, event)
	}
	return out
}
