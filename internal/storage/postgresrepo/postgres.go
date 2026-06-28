package postgresrepo

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type sqlDB struct {
	db *sql.DB
}

type sqlTx struct {
	tx *sql.Tx
}

type PostgresRepository struct {
	mu            sync.RWMutex
	retentionMu   sync.Mutex
	db            *sqlDB
	dsn           string
	codec         *credentialCodec
	limit         int
	auditTrimAt   time.Time
	auditTrimOps  int
	usageMaxAge   time.Duration
	usageTrimAt   time.Time
	usageTrimOps  int
	ledgerMaxAge  time.Duration
	ledgerTrimAt  time.Time
	ledgerTrimOps int
}

const retentionTrimInterval = time.Minute
const retentionTrimOperationInterval = 1024

func NewPostgresRepository(dsn, masterKey string) (*PostgresRepository, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("postgres dsn is empty")
	}
	codec, err := newCredentialCodec(masterKey)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(32)
	db.SetMaxIdleConns(32)

	repo := &PostgresRepository{
		db:           &sqlDB{db: db},
		dsn:          dsn,
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

func (db *sqlDB) Close() error {
	if db == nil || db.db == nil {
		return nil
	}
	return db.db.Close()
}

func (db *sqlDB) SetMaxOpenConns(n int) {
	if db != nil && db.db != nil {
		db.db.SetMaxOpenConns(n)
	}
}

func (db *sqlDB) SetMaxIdleConns(n int) {
	if db != nil && db.db != nil {
		db.db.SetMaxIdleConns(n)
	}
}

func (db *sqlDB) Exec(query string, args ...any) (sql.Result, error) {
	return db.db.Exec(rebindPostgresQuery(query), args...)
}

func (db *sqlDB) Query(query string, args ...any) (*sql.Rows, error) {
	return db.db.Query(rebindPostgresQuery(query), args...)
}

func (db *sqlDB) QueryRow(query string, args ...any) *sql.Row {
	return db.db.QueryRow(rebindPostgresQuery(query), args...)
}

func (db *sqlDB) Prepare(query string) (*sql.Stmt, error) {
	return db.db.Prepare(rebindPostgresQuery(query))
}

func (db *sqlDB) Begin() (*sqlTx, error) {
	tx, err := db.db.Begin()
	if err != nil {
		return nil, err
	}
	return &sqlTx{tx: tx}, nil
}

func (tx *sqlTx) Exec(query string, args ...any) (sql.Result, error) {
	return tx.tx.Exec(rebindPostgresQuery(query), args...)
}

func (tx *sqlTx) Query(query string, args ...any) (*sql.Rows, error) {
	return tx.tx.Query(rebindPostgresQuery(query), args...)
}

func (tx *sqlTx) QueryRow(query string, args ...any) *sql.Row {
	return tx.tx.QueryRow(rebindPostgresQuery(query), args...)
}

func (tx *sqlTx) Prepare(query string) (*sql.Stmt, error) {
	return tx.tx.Prepare(rebindPostgresQuery(query))
}

func (tx *sqlTx) Commit() error {
	return tx.tx.Commit()
}

func (tx *sqlTx) Rollback() error {
	return tx.tx.Rollback()
}

func rebindPostgresQuery(query string) string {
	if !strings.Contains(query, "?") {
		return query
	}
	var builder strings.Builder
	builder.Grow(len(query) + 16)
	arg := 1
	inSingleQuote := false
	inDoubleQuote := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		switch ch {
		case '\'':
			builder.WriteByte(ch)
			if !inDoubleQuote {
				if inSingleQuote && i+1 < len(query) && query[i+1] == '\'' {
					builder.WriteByte(query[i+1])
					i++
					continue
				}
				inSingleQuote = !inSingleQuote
			}
		case '"':
			builder.WriteByte(ch)
			if !inSingleQuote {
				inDoubleQuote = !inDoubleQuote
			}
		case '?':
			if inSingleQuote || inDoubleQuote {
				builder.WriteByte(ch)
				continue
			}
			builder.WriteByte('$')
			builder.WriteString(fmt.Sprintf("%d", arg))
			arg++
		default:
			builder.WriteByte(ch)
		}
	}
	return builder.String()
}

func (r *PostgresRepository) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return nil
	}
	err := r.db.Close()
	r.db = nil
	return err
}

func (r *PostgresRepository) init() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.initSchemaLocked(); err != nil {
		return err
	}
	if err := r.validateCredentialsLocked(); err != nil {
		return err
	}
	return r.compactIfWasteHighLocked()
}

func (r *PostgresRepository) ConfigureAuditLimit(limit int) error {
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

func (r *PostgresRepository) ConfigureUsageLogRetention(maxAgeDays int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	r.retentionMu.Lock()
	defer r.retentionMu.Unlock()
	r.usageMaxAge = normalizeUsageLogMaxAge(maxAgeDays)
	if err := r.trimBillingRequestsLocked(now); err != nil {
		return err
	}
	r.usageTrimAt = now
	r.usageTrimOps = 0
	return nil
}

func (r *PostgresRepository) ConfigureBillingLedgerRetention(maxAgeDays int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	r.retentionMu.Lock()
	defer r.retentionMu.Unlock()
	r.ledgerMaxAge = normalizeBillingLedgerRetentionAge(maxAgeDays)
	if err := r.trimBillingLedgerLocked(now); err != nil {
		return err
	}
	r.ledgerTrimAt = now
	r.ledgerTrimOps = 0
	return nil
}

func (r *PostgresRepository) GetSystemSettings() (core.SystemSettings, error) {
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

func (r *PostgresRepository) GetStartupSystemSettings() (core.SystemSettings, error) {
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

func (r *PostgresRepository) UpsertSystemSettings(settings core.SystemSettings) error {
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
