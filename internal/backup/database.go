package backup

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

const (
	databaseBackendSQLite   = "sqlite"
	databaseBackendPostgres = "postgres"
)

type backupDB struct {
	db      *sql.DB
	backend string
}

type backupTx struct {
	tx      *sql.Tx
	backend string
}

func resolveDatabaseBackend(opts Options) (string, error) {
	backend := strings.ToLower(strings.TrimSpace(opts.DatabaseBackend))
	switch backend {
	case "":
		backend = databaseBackendSQLite
	case databaseBackendSQLite:
	case databaseBackendPostgres:
	default:
		return "", fmt.Errorf("invalid database backend %q: expected sqlite or postgres", opts.DatabaseBackend)
	}
	if backend == databaseBackendPostgres && strings.TrimSpace(opts.PostgresDSN) == "" {
		return "", errors.New("postgres_dsn is required when database_backend is postgres")
	}
	return backend, nil
}

func openDatabase(opts Options) (*backupDB, error) {
	backend, err := resolveDatabaseBackend(opts)
	if err != nil {
		return nil, err
	}
	var db *sql.DB
	switch backend {
	case databaseBackendSQLite:
		statePath := strings.TrimSpace(opts.StatePath)
		if statePath == "" {
			return nil, errors.New("state path is required")
		}
		db, err = sql.Open("sqlite", statePath)
	case databaseBackendPostgres:
		db, err = sql.Open("pgx", strings.TrimSpace(opts.PostgresDSN))
	default:
		return nil, fmt.Errorf("unsupported database backend %q", backend)
	}
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return &backupDB{db: db, backend: backend}, nil
}

func (db *backupDB) Close() error {
	if db == nil || db.db == nil {
		return nil
	}
	return db.db.Close()
}

func (db *backupDB) Exec(query string, args ...any) (sql.Result, error) {
	return db.db.Exec(rebindBackupQuery(db.backend, query), args...)
}

func (db *backupDB) Query(query string, args ...any) (*sql.Rows, error) {
	return db.db.Query(rebindBackupQuery(db.backend, query), args...)
}

func (db *backupDB) QueryRow(query string, args ...any) *sql.Row {
	return db.db.QueryRow(rebindBackupQuery(db.backend, query), args...)
}

func (db *backupDB) Begin() (*backupTx, error) {
	tx, err := db.db.Begin()
	if err != nil {
		return nil, err
	}
	return &backupTx{tx: tx, backend: db.backend}, nil
}

func (tx *backupTx) Exec(query string, args ...any) (sql.Result, error) {
	return tx.tx.Exec(rebindBackupQuery(tx.backend, query), args...)
}

func (tx *backupTx) Query(query string, args ...any) (*sql.Rows, error) {
	return tx.tx.Query(rebindBackupQuery(tx.backend, query), args...)
}

func (tx *backupTx) QueryRow(query string, args ...any) *sql.Row {
	return tx.tx.QueryRow(rebindBackupQuery(tx.backend, query), args...)
}

func (tx *backupTx) Commit() error {
	return tx.tx.Commit()
}

func (tx *backupTx) Rollback() error {
	return tx.tx.Rollback()
}

func rebindBackupQuery(backend, query string) string {
	if backend != databaseBackendPostgres || !strings.Contains(query, "?") {
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
