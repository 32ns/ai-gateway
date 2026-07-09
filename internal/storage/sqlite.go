package storage

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	_ "modernc.org/sqlite"
)

type SQLiteRepository struct {
	mu                  sync.RWMutex
	db                  *sql.DB
	path                string
	codec               *credentialCodec
	limit               int
	auditTrimAt         time.Time
	auditTrimOps        int
	gatewayAuditMaxAge  time.Duration
	gatewayAuditTrimAt  time.Time
	gatewayAuditTrimOps int
	usageMaxAge         time.Duration
	usageTrimAt         time.Time
	usageTrimOps        int
	ledgerMaxAge        time.Duration
	ledgerTrimAt        time.Time
	ledgerTrimOps       int
}

const retentionTrimInterval = time.Minute
const retentionTrimOperationInterval = 1024

func NewSQLiteRepository(path, masterKey string) (*SQLiteRepository, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("state path is empty")
	}
	codec, err := newCredentialCodec(masterKey)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", sqliteOpenDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(16)

	repo := &SQLiteRepository{
		db:           db,
		path:         path,
		codec:        codec,
		limit:        defaultAuditLimit,
		usageMaxAge:  defaultUsageLogMaxAge,
		ledgerMaxAge: defaultBillingLedgerRetentionAge,
	}
	if err := repo.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return repo, nil
}

func sqliteOpenDSN(path string) string {
	values := url.Values{}
	values.Add("_pragma", "busy_timeout=5000")
	values.Add("_pragma", "foreign_keys=ON")
	values.Add("_pragma", "journal_mode=WAL")
	values.Add("_pragma", "synchronous=NORMAL")
	values.Add("_pragma", "temp_store=MEMORY")
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + values.Encode()
}

func (r *SQLiteRepository) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return nil
	}
	err := r.db.Close()
	r.db = nil
	return err
}

func (r *SQLiteRepository) init() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	pragmas := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA temp_store = MEMORY",
	}
	for _, pragma := range pragmas {
		if _, err := r.db.Exec(pragma); err != nil {
			return err
		}
	}
	if err := r.initSchemaLocked(); err != nil {
		return err
	}
	if err := r.validateCredentialsLocked(); err != nil {
		return err
	}
	return nil
}

func (r *SQLiteRepository) ConfigureAuditLimit(limit int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	r.limit = normalizeAuditLimit(limit)
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	if err := r.trimAuditTx(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.auditTrimAt = now
	r.auditTrimOps = 0
	return nil
}

func (r *SQLiteRepository) ConfigureGatewayAuditRetention(maxAgeDays int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	r.gatewayAuditMaxAge = normalizeGatewayAuditRetentionAge(maxAgeDays)
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	if err := trimGatewayAuditTx(tx, now, r.gatewayAuditMaxAge); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.gatewayAuditTrimAt = now
	r.gatewayAuditTrimOps = 0
	return nil
}

func (r *SQLiteRepository) ConfigureUsageLogRetention(maxAgeDays int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	r.usageMaxAge = normalizeUsageLogMaxAge(maxAgeDays)
	if err := r.trimBillingRequestsLocked(now); err != nil {
		return err
	}
	r.usageTrimAt = now
	r.usageTrimOps = 0
	return nil
}

func (r *SQLiteRepository) ConfigureBillingLedgerRetention(maxAgeDays int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	r.ledgerMaxAge = normalizeBillingLedgerRetentionAge(maxAgeDays)
	if err := r.trimBillingLedgerLocked(now); err != nil {
		return err
	}
	r.ledgerTrimAt = now
	r.ledgerTrimOps = 0
	return nil
}

func (r *SQLiteRepository) GetSystemSettings() (core.SystemSettings, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var payload string
	err := r.db.QueryRow(`SELECT payload FROM system_settings WHERE key = ?`, "global").Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return core.DefaultSystemSettings(), nil
	}
	if err != nil {
		return core.SystemSettings{}, err
	}
	return r.decodeSystemSettingsPayload(payload)
}

func (r *SQLiteRepository) GetStartupSystemSettings() (core.SystemSettings, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var payload string
	err := r.db.QueryRow(`SELECT payload FROM system_settings WHERE key = ?`, "global").Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return core.DefaultSystemSettings(), nil
	}
	if err != nil {
		return core.SystemSettings{}, err
	}
	return r.decodeStartupSystemSettingsPayload(payload)
}

func (r *SQLiteRepository) UpsertSystemSettings(settings core.SystemSettings) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	settings = core.NormalizeSystemSettings(settings)
	settings.UpdatedAt = time.Now().UTC()
	payload, err := r.encodeSystemSettingsPayload(settings)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(
		`INSERT INTO system_settings(key, payload, updated_at_ns)
		VALUES(?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET payload = excluded.payload, updated_at_ns = excluded.updated_at_ns`,
		"global",
		payload,
		settings.UpdatedAt.UnixNano(),
	)
	return err
}
