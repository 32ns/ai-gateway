package postgresrepo

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storageerr"
)

const (
	maxMonitorResultsPerTarget    = 2881
	monitorResultsRetentionWindow = 24 * time.Hour
)

func (r *PostgresRepository) ListMonitorTargets() []core.MonitorTarget {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`SELECT payload FROM monitor_targets`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]core.MonitorTarget, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		var target core.MonitorTarget
		if err := json.Unmarshal([]byte(payload), &target); err != nil {
			continue
		}
		out = append(out, cloneMonitorTarget(target))
	}
	return sortMonitorTargets(out)
}

func (r *PostgresRepository) GetMonitorTarget(id string) (core.MonitorTarget, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	payload, err := r.getPayloadByID("monitor_targets", strings.TrimSpace(id))
	if err != nil {
		return core.MonitorTarget{}, err
	}
	var target core.MonitorTarget
	if err := json.Unmarshal([]byte(payload), &target); err != nil {
		return core.MonitorTarget{}, err
	}
	return cloneMonitorTarget(target), nil
}

func (r *PostgresRepository) UpsertMonitorTarget(target core.MonitorTarget) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	target, err := normalizeMonitorTargetForStorage(target, func(id string) (core.MonitorTarget, bool) {
		payload, err := r.getPayloadByID("monitor_targets", id)
		if err != nil {
			return core.MonitorTarget{}, false
		}
		var existing core.MonitorTarget
		if err := json.Unmarshal([]byte(payload), &existing); err != nil {
			return core.MonitorTarget{}, false
		}
		return existing, true
	})
	if err != nil {
		return err
	}
	payload, err := json.Marshal(cloneMonitorTarget(target))
	if err != nil {
		return err
	}
	_, err = r.db.Exec(
		`INSERT INTO monitor_targets(id, account_group, model, enabled, public_visible, interval_seconds, updated_at_ns, payload)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			account_group = excluded.account_group,
			model = excluded.model,
			enabled = excluded.enabled,
			public_visible = excluded.public_visible,
			interval_seconds = excluded.interval_seconds,
			updated_at_ns = excluded.updated_at_ns,
			payload = excluded.payload`,
		target.ID,
		target.AccountGroup,
		target.Model,
		target.Enabled,
		target.PublicVisible,
		target.IntervalSeconds,
		target.UpdatedAt.UnixNano(),
		string(payload),
	)
	return err
}

func (r *PostgresRepository) DeleteMonitorTarget(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = strings.TrimSpace(id)
	result, err := r.db.Exec(`DELETE FROM monitor_targets WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return storageerr.ErrNotFound
	}
	_, err = r.db.Exec(`DELETE FROM monitor_results WHERE target_id = ?`, id)
	return err
}

func (r *PostgresRepository) AppendMonitorResult(result core.MonitorResult) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	result, err := normalizeMonitorResultForStorage(result)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(cloneMonitorResult(result))
	if err != nil {
		return err
	}
	_, err = r.db.Exec(
		`INSERT INTO monitor_results(id, target_id, status, latency_ms, checked_at_ns, payload)
		VALUES(?, ?, ?, ?, ?, ?)`,
		result.ID,
		result.TargetID,
		string(result.Status),
		result.LatencyMS,
		result.CheckedAt.UnixNano(),
		string(payload),
	)
	if err != nil {
		return err
	}
	return r.trimMonitorResultsLocked(result.TargetID)
}

func (r *PostgresRepository) ListMonitorResults(targetID string, limit int) []core.MonitorResult {
	r.mu.RLock()
	defer r.mu.RUnlock()

	targetID = strings.TrimSpace(targetID)
	if limit <= 0 {
		limit = 100
	}
	var rows *sql.Rows
	var err error
	if targetID == "" {
		rows, err = r.db.Query(`SELECT payload FROM monitor_results ORDER BY checked_at_ns DESC, id ASC LIMIT ?`, limit)
	} else {
		rows, err = r.db.Query(`SELECT payload FROM monitor_results WHERE target_id = ? ORDER BY checked_at_ns DESC, id ASC LIMIT ?`, targetID, limit)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]core.MonitorResult, 0, limit)
	for rows.Next() {
		if result, ok := scanMonitorResultPayload(rows); ok {
			out = append(out, result)
		}
	}
	return sortMonitorResults(out)
}

func (r *PostgresRepository) ListLatestMonitorResults() map[string]core.MonitorResult {
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows, err := r.db.Query(`
		SELECT mr.payload
		FROM monitor_results mr
		INNER JOIN (
			SELECT target_id, MAX(checked_at_ns) AS checked_at_ns
			FROM monitor_results
			GROUP BY target_id
		) latest ON latest.target_id = mr.target_id AND latest.checked_at_ns = mr.checked_at_ns
		ORDER BY mr.checked_at_ns DESC, mr.id ASC
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make(map[string]core.MonitorResult)
	for rows.Next() {
		result, ok := scanMonitorResultPayload(rows)
		if !ok || result.TargetID == "" {
			continue
		}
		if _, exists := out[result.TargetID]; exists {
			continue
		}
		out[result.TargetID] = result
	}
	return out
}

func normalizeMonitorTargetForStorage(target core.MonitorTarget, existing func(string) (core.MonitorTarget, bool)) (core.MonitorTarget, error) {
	target.ID = strings.TrimSpace(target.ID)
	if target.ID == "" {
		return core.MonitorTarget{}, fmt.Errorf("monitor target id is required")
	}
	target.AccountGroup = core.NormalizeAccountGroupName(target.AccountGroup)
	target.Model = strings.TrimSpace(target.Model)
	if target.Model == "" {
		return core.MonitorTarget{}, fmt.Errorf("monitor model is required")
	}
	target.Name = strings.TrimSpace(target.Name)
	if target.Name == "" {
		target.Name = target.AccountGroup + " / " + target.Model
	}
	target.Prompt = strings.TrimSpace(target.Prompt)
	now := time.Now().UTC()
	if existing != nil {
		if previous, ok := existing(target.ID); ok && !previous.CreatedAt.IsZero() {
			target.CreatedAt = previous.CreatedAt
		}
	}
	if target.CreatedAt.IsZero() {
		target.CreatedAt = now
	} else {
		target.CreatedAt = target.CreatedAt.UTC()
	}
	target.UpdatedAt = now
	return target, nil
}

func normalizeMonitorResultForStorage(result core.MonitorResult) (core.MonitorResult, error) {
	result.ID = strings.TrimSpace(result.ID)
	if result.ID == "" {
		return core.MonitorResult{}, fmt.Errorf("monitor result id is required")
	}
	result.TargetID = strings.TrimSpace(result.TargetID)
	if result.TargetID == "" {
		return core.MonitorResult{}, fmt.Errorf("monitor target id is required")
	}
	if result.Status == "" {
		result.Status = core.MonitorStatusUnknown
	}
	if result.CheckedAt.IsZero() {
		result.CheckedAt = time.Now().UTC()
	} else {
		result.CheckedAt = result.CheckedAt.UTC()
	}
	result.Attempts = append([]core.AttemptRecord(nil), result.Attempts...)
	return result, nil
}

func scanMonitorResultPayload(scanner interface{ Scan(dest ...any) error }) (core.MonitorResult, bool) {
	var payload string
	if err := scanner.Scan(&payload); err != nil {
		return core.MonitorResult{}, false
	}
	var result core.MonitorResult
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		return core.MonitorResult{}, false
	}
	return cloneMonitorResult(result), true
}

func (r *PostgresRepository) trimMonitorResultsLocked(targetID string) error {
	targetID = strings.TrimSpace(targetID)
	if targetID == "" || maxMonitorResultsPerTarget <= 0 {
		return nil
	}
	_, err := r.db.Exec(`
		DELETE FROM monitor_results
		WHERE target_id = ?
			AND (
				checked_at_ns < (
					SELECT MAX(checked_at_ns) - ?
					FROM monitor_results
					WHERE target_id = ?
				)
				OR id NOT IN (
					SELECT id
					FROM (
						SELECT id
						FROM monitor_results
						WHERE target_id = ?
						ORDER BY checked_at_ns DESC, id ASC
						LIMIT ?
					) retained
				)
			)
		`, targetID, monitorResultsRetentionWindow.Nanoseconds(), targetID, targetID, maxMonitorResultsPerTarget)
	return err
}

func cloneMonitorTarget(target core.MonitorTarget) core.MonitorTarget {
	return target
}

func cloneMonitorResult(result core.MonitorResult) core.MonitorResult {
	result.Attempts = append([]core.AttemptRecord(nil), result.Attempts...)
	return result
}

func sortMonitorTargets(targets []core.MonitorTarget) []core.MonitorTarget {
	slices.SortFunc(targets, func(a, b core.MonitorTarget) int {
		if a.Enabled != b.Enabled {
			if a.Enabled {
				return -1
			}
			return 1
		}
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			if a.UpdatedAt.After(b.UpdatedAt) {
				return -1
			}
			return 1
		}
		an := strings.ToLower(strings.TrimSpace(a.Name))
		bn := strings.ToLower(strings.TrimSpace(b.Name))
		if an != bn {
			return strings.Compare(an, bn)
		}
		return strings.Compare(a.ID, b.ID)
	})
	return targets
}

func sortMonitorResults(results []core.MonitorResult) []core.MonitorResult {
	slices.SortFunc(results, func(a, b core.MonitorResult) int {
		if !a.CheckedAt.Equal(b.CheckedAt) {
			if a.CheckedAt.After(b.CheckedAt) {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return results
}
