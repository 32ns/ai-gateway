package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

const Format = "ai-gateway-backup"
const Version = 1

const (
	maxBackupExtractEntryBytes = int64(2) << 30
	maxBackupExtractTotalBytes = int64(4) << 30
)

type Options struct {
	ConfigPath      string
	StatePath       string
	DatabaseBackend string
	PostgresDSN     string
	AppVersion      string
	DataSets        []string
	SourceMasterKey string
	TargetMasterKey string
}

type Manifest struct {
	Format     string    `json:"format"`
	Version    int       `json:"version"`
	AppVersion string    `json:"app_version,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	Includes   Includes  `json:"includes"`
}

type Includes struct {
	Config   bool     `json:"config"`
	Database bool     `json:"database"`
	Data     bool     `json:"data,omitempty"`
	DataSets []string `json:"data_sets,omitempty"`
}

const (
	DataSetSettings  = "settings"
	DataSetUsers     = "users"
	DataSetAccounts  = "accounts"
	DataSetModels    = "models"
	DataSetClients   = "clients"
	DataSetMonitors  = "monitors"
	DataSetBilling   = "billing"
	DataSetMessages  = "messages"
	DataSetDocuments = "documents"
	DataSetAudit     = "audit"
)

var orderedDataSets = []string{
	DataSetSettings,
	DataSetUsers,
	DataSetAccounts,
	DataSetModels,
	DataSetClients,
	DataSetMonitors,
	DataSetBilling,
	DataSetMessages,
	DataSetDocuments,
	DataSetAudit,
}

var dataSetTables = map[string][]string{
	DataSetSettings: []string{"system_settings"},
	DataSetUsers:    []string{"users", "user_balances", "user_oauth_identities", "user_invitation_codes", "mcp_tokens"},
	DataSetAccounts: []string{"accounts", "account_credentials", "account_runtime", "account_groups"},
	DataSetModels:   []string{"models"},
	DataSetClients:  []string{"clients", "client_spend", "openai_response_bindings"},
	DataSetMonitors: []string{"monitor_targets", "monitor_results"},
	DataSetBilling: []string{
		"billing_requests",
		"billing_ledger",
		"billing_plan_groups",
		"billing_plans",
		"user_plan_entitlements",
		"plan_quota_ledger",
		"billing_funding_allocations",
		"payment_orders",
		"payment_refunds",
	},
	DataSetMessages:  []string{"site_messages", "site_message_reads"},
	DataSetDocuments: []string{"documents", "document_redirects"},
	DataSetAudit:     []string{"audit", "audit_terms"},
}

type logicalDataBackup struct {
	DataSets []string             `json:"data_sets"`
	Tables   []logicalTableBackup `json:"tables"`
}

type logicalTableBackup struct {
	Name    string              `json:"name"`
	Columns []string            `json:"columns"`
	Rows    [][]json.RawMessage `json:"rows"`
}

func Create(outPath string, opts Options) (Manifest, error) {
	outPath = strings.TrimSpace(outPath)
	if outPath == "" {
		return Manifest{}, errors.New("backup output path is required")
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil && filepath.Dir(outPath) != "." {
		return Manifest{}, err
	}
	file, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Manifest{}, err
	}
	defer file.Close()
	return Write(file, opts)
}

func Write(w io.Writer, opts Options) (Manifest, error) {
	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	backend, err := resolveDatabaseBackend(opts)
	if err != nil {
		return Manifest{}, err
	}
	manifest := Manifest{
		Format:     Format,
		Version:    Version,
		AppVersion: strings.TrimSpace(opts.AppVersion),
		CreatedAt:  time.Now().UTC(),
	}
	dataSets, err := normalizeDataSetsStrict(opts.DataSets)
	if err != nil {
		return Manifest{}, err
	}
	logicalDataOnly := len(dataSets) > 0
	postgresFullBackup := backend == databaseBackendPostgres && len(dataSets) == 0
	if postgresFullBackup {
		dataSets = AllDataSets()
		logicalDataOnly = true
	}
	if !logicalDataOnly && strings.TrimSpace(opts.ConfigPath) != "" && fileExists(opts.ConfigPath) {
		manifest.Includes.Config = true
	}
	if postgresFullBackup && strings.TrimSpace(opts.ConfigPath) != "" && fileExists(opts.ConfigPath) {
		manifest.Includes.Config = true
	}
	if !logicalDataOnly && backend == databaseBackendSQLite && strings.TrimSpace(opts.StatePath) != "" && fileExists(opts.StatePath) {
		manifest.Includes.Database = true
	}
	if len(dataSets) > 0 {
		manifest.Includes.Data = true
		manifest.Includes.DataSets = dataSets
	}
	if err := writeManifest(tw, manifest); err != nil {
		return Manifest{}, err
	}
	if manifest.Includes.Data {
		data, err := exportLogicalData(opts, dataSets)
		if err != nil {
			return Manifest{}, err
		}
		raw, err := json.MarshalIndent(data, "", "  ")
		if err != nil {
			return Manifest{}, err
		}
		raw = append(raw, '\n')
		if err := writeBytes(tw, "data/logical.json", raw, 0o600); err != nil {
			return Manifest{}, err
		}
	}
	if manifest.Includes.Config {
		if err := addFile(tw, opts.ConfigPath, "config.json"); err != nil {
			return Manifest{}, err
		}
	}
	if manifest.Includes.Database {
		if err := addSQLiteDatabaseSnapshot(tw, opts.StatePath); err != nil {
			return Manifest{}, err
		}
	}
	return manifest, nil
}

func Restore(backupPath string, opts Options, preRestoreDir string) (string, error) {
	backupPath = strings.TrimSpace(backupPath)
	if backupPath == "" {
		return "", errors.New("backup path is required")
	}
	if strings.TrimSpace(preRestoreDir) == "" {
		preRestoreDir = filepath.Dir(backupPath)
	}
	preRestorePath := PreRestoreBackupPath(preRestoreDir)
	preRestoreOpts := opts
	preRestoreOpts.DataSets = nil
	if _, err := Create(preRestorePath, preRestoreOpts); err != nil {
		return "", fmt.Errorf("create pre-restore backup: %w", err)
	}
	if err := RestoreOnly(backupPath, opts); err != nil {
		return preRestorePath, err
	}
	return preRestorePath, nil
}

func PreRestoreBackupPath(preRestoreDir string) string {
	return filepath.Join(preRestoreDir, "pre-restore-"+time.Now().UTC().Format("20060102-150405.000000000")+".agbak")
}

func RestoreOnly(backupPath string, opts Options) error {
	file, err := os.Open(backupPath)
	if err != nil {
		return err
	}
	defer file.Close()
	return ReadAndRestore(file, opts)
}

func ReadAndRestore(r io.Reader, opts Options) error {
	tempDir, err := os.MkdirTemp("", "ai-gateway-restore-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)

	manifest, err := extract(r, tempDir)
	if err != nil {
		return err
	}
	if manifest.Format != Format || manifest.Version != Version {
		return fmt.Errorf("unsupported backup format %q version %d", manifest.Format, manifest.Version)
	}
	backend, err := resolveDatabaseBackend(opts)
	if err != nil {
		return err
	}
	dataSets, err := normalizeDataSetsStrict(opts.DataSets)
	if err != nil {
		return err
	}
	selectiveDataRestore := len(dataSets) > 0
	encryptedRestoreOpts := opts
	encryptedRestoreOpts.TargetMasterKey, err = targetMasterKeyForRestore(tempDir, manifest, selectiveDataRestore, opts)
	if err != nil {
		return err
	}
	if err := prepareEncryptedRestore(tempDir, manifest, dataSets, encryptedRestoreOpts); err != nil {
		return err
	}
	if manifest.Includes.Config && !selectiveDataRestore && strings.TrimSpace(opts.ConfigPath) != "" {
		if err := replaceFile(filepath.Join(tempDir, "config.json"), opts.ConfigPath, 0o600); err != nil {
			return fmt.Errorf("restore config: %w", err)
		}
	}
	if manifest.Includes.Database && !selectiveDataRestore {
		switch backend {
		case databaseBackendSQLite:
			if strings.TrimSpace(opts.StatePath) != "" {
				if err := restoreStateFiles(tempDir, opts.StatePath); err != nil {
					return fmt.Errorf("restore database: %w", err)
				}
			}
		case databaseBackendPostgres:
			if err := restoreLogicalDataFromExtractedState(tempDir, opts, AllDataSets()); err != nil {
				return fmt.Errorf("restore data: %w", err)
			}
		}
	}
	if manifest.Includes.Data && (backend == databaseBackendPostgres || strings.TrimSpace(opts.StatePath) != "") {
		if err := restoreLogicalData(filepath.Join(tempDir, "data", "logical.json"), opts, dataSets); err != nil {
			return fmt.Errorf("restore data: %w", err)
		}
	}
	if !manifest.Includes.Data && manifest.Includes.Database && selectiveDataRestore {
		if err := restoreLogicalDataFromExtractedState(tempDir, opts, dataSets); err != nil {
			return fmt.Errorf("restore data: %w", err)
		}
	}
	return nil
}

func AllDataSets() []string {
	return append([]string(nil), orderedDataSets...)
}

func normalizeDataSets(input []string) []string {
	if len(input) == 0 {
		return nil
	}
	selected := map[string]struct{}{}
	for _, value := range input {
		value = strings.ToLower(strings.TrimSpace(value))
		if _, ok := dataSetTables[value]; ok {
			selected[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(selected))
	for _, value := range orderedDataSets {
		if _, ok := selected[value]; ok {
			out = append(out, value)
		}
	}
	return out
}

func normalizeDataSetsStrict(input []string) ([]string, error) {
	if len(input) == 0 {
		return nil, nil
	}
	invalid := make([]string, 0)
	for _, value := range input {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, ok := dataSetTables[value]; !ok {
			invalid = append(invalid, value)
		}
	}
	if len(invalid) > 0 {
		return nil, fmt.Errorf("unknown backup data type %q", invalid[0])
	}
	dataSets := normalizeDataSets(input)
	if len(dataSets) == 0 {
		return nil, errors.New("backup data types are empty")
	}
	return dataSets, nil
}

func normalizeKnownDataSets(input []string) []string {
	if len(input) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(input))
	out := make([]string, 0, len(input))
	for _, value := range input {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, ok := dataSetTables[value]; !ok {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func exportLogicalData(opts Options, dataSets []string) (logicalDataBackup, error) {
	db, err := openDatabase(opts)
	if err != nil {
		return logicalDataBackup{}, err
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return logicalDataBackup{}, err
	}
	defer tx.Rollback()
	if db.backend == databaseBackendPostgres {
		if _, err := tx.Exec(`SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY`); err != nil {
			return logicalDataBackup{}, err
		}
	}

	data := logicalDataBackup{DataSets: dataSets}
	for _, table := range tablesForDataSets(dataSets) {
		backup, err := exportLogicalTable(tx, table)
		if err != nil {
			return logicalDataBackup{}, err
		}
		data.Tables = append(data.Tables, backup)
	}
	if err := tx.Commit(); err != nil {
		return logicalDataBackup{}, err
	}
	return data, nil
}

type backupQuerier interface {
	Query(string, ...any) (*sql.Rows, error)
}

func exportLogicalTable(db backupQuerier, table string) (logicalTableBackup, error) {
	query := fmt.Sprintf("SELECT * FROM %s", table)
	rows, err := db.Query(query)
	if err != nil {
		return logicalTableBackup{}, err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return logicalTableBackup{}, err
	}
	tableBackup := logicalTableBackup{Name: table, Columns: columns}
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return logicalTableBackup{}, err
		}
		rawRow := make([]json.RawMessage, len(columns))
		for i, value := range values {
			if bytes, ok := value.([]byte); ok {
				value = string(bytes)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				return logicalTableBackup{}, err
			}
			rawRow[i] = raw
		}
		tableBackup.Rows = append(tableBackup.Rows, rawRow)
	}
	return tableBackup, rows.Err()
}

func restoreLogicalDataFromExtractedState(tempDir string, opts Options, requestedDataSets []string) error {
	sourceStatePath, err := extractedStatePath(tempDir)
	if err != nil {
		return err
	}
	sourceOpts := opts
	sourceOpts.DatabaseBackend = databaseBackendSQLite
	sourceOpts.PostgresDSN = ""
	sourceOpts.StatePath = sourceStatePath
	data, err := exportLogicalData(sourceOpts, requestedDataSets)
	if err != nil {
		return err
	}
	logicalPath := filepath.Join(tempDir, "data", "logical-from-state.json")
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logicalPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(logicalPath, raw, 0o600); err != nil {
		return err
	}
	return restoreLogicalData(logicalPath, opts, requestedDataSets)
}

func restoreLogicalData(path string, opts Options, requestedDataSets []string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var data logicalDataBackup
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}
	dataSets := normalizeKnownDataSets(data.DataSets)
	if requested, err := normalizeDataSetsStrict(requestedDataSets); err != nil {
		return err
	} else if len(requested) > 0 {
		available := map[string]struct{}{}
		for _, dataSet := range dataSets {
			available[dataSet] = struct{}{}
		}
		selected := make([]string, 0, len(requested))
		for _, dataSet := range requested {
			if _, ok := available[dataSet]; ok {
				selected = append(selected, dataSet)
			}
		}
		if len(selected) == 0 {
			return errors.New("selected data types are not included in backup")
		}
		dataSets = selected
	}
	allowedTables := map[string]struct{}{}
	for _, table := range tablesForDataSets(dataSets) {
		allowedTables[table] = struct{}{}
	}
	db, err := openDatabase(opts)
	if err != nil {
		return err
	}
	defer db.Close()

	if db.backend == databaseBackendSQLite {
		if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
			return err
		}
		defer db.Exec(`PRAGMA foreign_keys = ON`)
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	switch db.backend {
	case databaseBackendSQLite:
	case databaseBackendPostgres:
		if _, err := tx.Exec(`SET CONSTRAINTS ALL DEFERRED`); err != nil {
			_ = tx.Rollback()
			return err
		}
	}

	tables := tablesForDataSets(dataSets)
	tableColumns := map[string]map[string]struct{}{}
	for _, table := range tables {
		columns, err := tableColumnSet(tx, table)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		tableColumns[table] = columns
	}
	if err := preflightLogicalRestoreTx(tx, dataSets); err != nil {
		_ = tx.Rollback()
		return err
	}
	for i := len(tables) - 1; i >= 0; i-- {
		if _, err := tx.Exec(fmt.Sprintf("DELETE FROM %s", tables[i])); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	tablePayloads := map[string]logicalTableBackup{}
	for _, table := range data.Tables {
		if _, ok := allowedTables[table.Name]; ok {
			tablePayloads[table.Name] = table
		}
	}
	for _, table := range tables {
		payload, ok := tablePayloads[table]
		if !ok {
			_ = tx.Rollback()
			return fmt.Errorf("backup data is missing table %s", table)
		}
		if err := restoreLogicalTable(tx, payload, tableColumns[table]); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := rebuildLogicalRestoreUserIdentityIndexesTx(tx, dataSets); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := cleanupLogicalRestoreReferencesTx(tx, dataSets); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := reconcileLogicalRestoreClientSpendTx(tx, dataSets); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := reconcileLogicalRestoreFinanceRollupsTx(tx, dataSets); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := resetRestoreSequencesTx(tx, db.backend); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := validateLogicalRestoreReferencesTx(tx, dataSets); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func restoreLogicalTable(tx *backupTx, table logicalTableBackup, allowedColumns map[string]struct{}) error {
	seen := make(map[string]struct{}, len(table.Columns))
	for _, column := range table.Columns {
		if _, ok := allowedColumns[column]; !ok {
			return fmt.Errorf("table %s contains unknown column %s", table.Name, column)
		}
		if _, ok := seen[column]; ok {
			return fmt.Errorf("table %s contains duplicate column %s", table.Name, column)
		}
		seen[column] = struct{}{}
	}
	for column := range allowedColumns {
		if _, ok := seen[column]; !ok {
			if !logicalRestoreColumnCanUseDefault(table.Name, column) {
				return fmt.Errorf("table %s is missing column %s", table.Name, column)
			}
		}
	}
	placeholders := make([]string, len(table.Columns))
	for i := range placeholders {
		placeholders[i] = "?"
	}
	query := fmt.Sprintf("INSERT INTO %s(%s) VALUES(%s)", table.Name, strings.Join(table.Columns, ","), strings.Join(placeholders, ","))
	for _, row := range table.Rows {
		if len(row) != len(table.Columns) {
			return fmt.Errorf("table %s row has %d values for %d columns", table.Name, len(row), len(table.Columns))
		}
		values := make([]any, len(row))
		for i, raw := range row {
			value, err := decodeLogicalCell(raw)
			if err != nil {
				return err
			}
			value, err = normalizeLogicalCell(tx.backend, table.Name, table.Columns[i], value)
			if err != nil {
				return err
			}
			values[i] = value
		}
		if _, err := tx.Exec(query, values...); err != nil {
			return err
		}
	}
	return nil
}

func logicalRestoreColumnCanUseDefault(table, column string) bool {
	return table == "billing_plan_groups" && column == "quota_price_ratio"
}

func decodeLogicalCell(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if number, ok := value.(json.Number); ok {
		if integer, err := number.Int64(); err == nil {
			return integer, nil
		}
		if floating, err := number.Float64(); err == nil {
			return floating, nil
		}
	}
	return value, nil
}

func normalizeLogicalCell(backend, table, column string, value any) (any, error) {
	if !logicalBooleanColumn(table, column) {
		return value, nil
	}
	enabled, err := logicalBool(value)
	if err != nil {
		return nil, fmt.Errorf("table %s column %s expects boolean value: %w", table, column, err)
	}
	if backend == databaseBackendSQLite {
		if enabled {
			return int64(1), nil
		}
		return int64(0), nil
	}
	return enabled, nil
}

func logicalBooleanColumn(table, column string) bool {
	switch table {
	case "billing_plans":
		return column == "enabled"
	case "billing_requests":
		return column == "fast_mode"
	case "mcp_tokens":
		return column == "enabled"
	case "monitor_targets":
		return column == "enabled" || column == "public_visible"
	case "site_messages":
		return column == "enabled"
	default:
		return false
	}
}

func logicalBool(value any) (bool, error) {
	switch typed := value.(type) {
	case bool:
		return typed, nil
	case int64:
		return typed != 0, nil
	case int:
		return typed != 0, nil
	case float64:
		return typed != 0, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "t", "yes", "y", "on":
			return true, nil
		case "0", "false", "f", "no", "n", "off":
			return false, nil
		}
	case nil:
		return false, nil
	}
	return false, fmt.Errorf("unsupported boolean value %T", value)
}

func rebuildLogicalRestoreUserIdentityIndexesTx(tx *backupTx, dataSets []string) error {
	if !dataSetSelection(dataSets)[DataSetUsers] {
		return nil
	}
	if _, err := tx.Exec(`DELETE FROM user_oauth_identities`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM user_invitation_codes`); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT payload FROM users`)
	if err != nil {
		return err
	}
	defer rows.Close()
	nowNS := time.Now().UTC().UnixNano()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return err
		}
		var user core.User
		if err := json.Unmarshal([]byte(payload), &user); err != nil {
			return err
		}
		userID := strings.TrimSpace(user.ID)
		if userID == "" {
			continue
		}
		for _, identity := range user.OAuthIdentities {
			provider := strings.ToLower(strings.TrimSpace(identity.Provider))
			subject := strings.TrimSpace(identity.Subject)
			if provider == "" || subject == "" {
				continue
			}
			if _, err := tx.Exec(`
				INSERT INTO user_oauth_identities(user_id, provider, subject, email, username, linked_at_ns)
				VALUES(?, ?, ?, ?, ?, ?)
			`, userID, provider, subject, strings.TrimSpace(identity.Email), strings.TrimSpace(identity.Username), logicalTimeNS(identity.LinkedAt)); err != nil {
				return err
			}
		}
		signature := core.UserInvitationSignature(user)
		if signature == "" {
			continue
		}
		if _, err := tx.Exec(`
			INSERT INTO user_invitation_codes(user_id, signature, updated_at_ns)
			VALUES(?, ?, ?)
			ON CONFLICT(user_id) DO UPDATE SET
				signature = excluded.signature,
				updated_at_ns = excluded.updated_at_ns
		`, userID, signature, nowNS); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

func logicalTimeNS(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UTC().UnixNano()
}

func cleanupLogicalRestoreReferencesTx(tx *backupTx, dataSets []string) error {
	selected := dataSetSelection(dataSets)
	if selected[DataSetUsers] {
		if _, err := tx.Exec(`DELETE FROM user_sessions WHERE NOT EXISTS (SELECT 1 FROM users WHERE users.id = user_sessions.user_id)`); err != nil {
			return err
		}
	}
	if selected[DataSetClients] || selected[DataSetAccounts] {
		if _, err := tx.Exec(`DELETE FROM openai_response_bindings WHERE client_id <> '' AND NOT EXISTS (SELECT 1 FROM clients WHERE clients.id = openai_response_bindings.client_id)`); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM openai_response_bindings WHERE account_id <> '' AND NOT EXISTS (SELECT 1 FROM accounts WHERE accounts.id = openai_response_bindings.account_id)`); err != nil {
			return err
		}
	}
	return nil
}

func reconcileLogicalRestoreClientSpendTx(tx *backupTx, dataSets []string) error {
	selected := dataSetSelection(dataSets)
	if selected[DataSetBilling] {
		return reconcileClientSpendFromBillingTx(tx)
	}
	if selected[DataSetClients] {
		return clearClientSpendUsageTx(tx)
	}
	return nil
}

func reconcileLogicalRestoreFinanceRollupsTx(tx *backupTx, dataSets []string) error {
	selected := dataSetSelection(dataSets)
	if !selected[DataSetUsers] && !selected[DataSetClients] && !selected[DataSetBilling] {
		return nil
	}
	if err := rebuildFinanceUserRollupsTx(tx); err != nil {
		return err
	}
	if err := rebuildFinanceClientRollupsTx(tx); err != nil {
		return err
	}
	return rebuildFinanceModelRollupsTx(tx)
}

func rebuildFinanceUserRollupsTx(tx *backupTx) error {
	if _, err := tx.Exec(`DELETE FROM finance_user_rollups`); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO finance_user_rollups(user_id, username, balance_nano_usd)
		SELECT u.id, COALESCE(NULLIF(TRIM(u.username), ''), u.id), COALESCE(b.balance_nano_usd, 0)
		FROM users u
		LEFT JOIN user_balances b ON b.user_id = u.id
		WHERE u.id <> ''
	`); err != nil {
		return err
	}
	if err := addFinanceUserPaymentRollupsTx(tx); err != nil {
		return err
	}
	if err := addFinanceUserLedgerRollupsTx(tx); err != nil {
		return err
	}
	return addFinanceUserUsageRollupsTx(tx)
}

func addFinanceUserPaymentRollupsTx(tx *backupTx) error {
	if _, err := tx.Exec(`
		INSERT INTO finance_user_rollups(user_id, recharge_nano_usd, last_payment_at_ns)
		SELECT user_id,
			COALESCE(SUM(amount_nano_usd), 0),
			COALESCE(MAX(COALESCE(NULLIF(paid_at_ns, 0), updated_at_ns, created_at_ns)), 0)
		FROM payment_orders
		WHERE user_id <> '' AND status = 'paid'
		GROUP BY user_id
		ON CONFLICT(user_id) DO UPDATE SET
			recharge_nano_usd = excluded.recharge_nano_usd,
			last_payment_at_ns = excluded.last_payment_at_ns
	`); err != nil {
		return err
	}
	_, err := tx.Exec(`
		INSERT INTO finance_user_rollups(user_id, refund_nano_usd)
		SELECT user_id, COALESCE(SUM(amount_nano_usd), 0)
		FROM payment_refunds
		WHERE user_id <> '' AND status = 'done'
		GROUP BY user_id
		ON CONFLICT(user_id) DO UPDATE SET refund_nano_usd = finance_user_rollups.refund_nano_usd + excluded.refund_nano_usd
	`)
	return err
}

func addFinanceUserLedgerRollupsTx(tx *backupTx) error {
	_, err := tx.Exec(`
		INSERT INTO finance_user_rollups(user_id, reward_nano_usd, spend_nano_usd, last_spend_at_ns)
		SELECT user_id,
			COALESCE(SUM(CASE WHEN kind IN ('manual_credit', 'account_merge') THEN amount_nano_usd ELSE 0 END), 0),
			COALESCE(SUM(CASE
				WHEN kind = 'manual_debit' AND amount_nano_usd < 0 THEN -amount_nano_usd
				WHEN kind = 'plan_purchase' AND amount_nano_usd < 0 THEN -amount_nano_usd
				ELSE 0
			END), 0),
			COALESCE(MAX(CASE
				WHEN kind = 'manual_debit' AND amount_nano_usd < 0 THEN created_at_ns
				WHEN kind = 'plan_purchase' AND amount_nano_usd < 0 THEN created_at_ns
				ELSE 0
			END), 0)
		FROM billing_ledger
		WHERE user_id <> '' AND kind IN ('manual_credit', 'account_merge', 'manual_debit', 'plan_purchase')
		GROUP BY user_id
		ON CONFLICT(user_id) DO UPDATE SET
			reward_nano_usd = finance_user_rollups.reward_nano_usd + excluded.reward_nano_usd,
			spend_nano_usd = CASE
				WHEN finance_user_rollups.spend_nano_usd + excluded.spend_nano_usd < 0 THEN 0
				ELSE finance_user_rollups.spend_nano_usd + excluded.spend_nano_usd
			END,
			last_spend_at_ns = CASE
				WHEN finance_user_rollups.last_spend_at_ns > excluded.last_spend_at_ns THEN finance_user_rollups.last_spend_at_ns
				ELSE excluded.last_spend_at_ns
			END
	`)
	return err
}

func addFinanceUserUsageRollupsTx(tx *backupTx) error {
	_, err := tx.Exec(`
		INSERT INTO finance_user_rollups(user_id, spend_nano_usd, usage_spend_nano_usd, plan_spend_nano_usd, last_spend_at_ns)
		SELECT user_id,
			COALESCE(SUM(CASE WHEN billing_source = 'plan' THEN 0 ELSE spend_nano_usd END), 0),
			COALESCE(SUM(spend_nano_usd), 0),
			COALESCE(SUM(CASE WHEN billing_source = 'plan' THEN spend_nano_usd ELSE 0 END), 0),
			COALESCE(MAX(spend_at_ns), 0)
		FROM (
			SELECT user_id,
				CASE
					WHEN COALESCE(NULLIF(TRIM(billing_source), ''), 'cash') IN ('plan', 'package', 'subscription', 'entitlement') THEN 'plan'
					ELSE 'cash'
				END AS billing_source,
				CASE
					WHEN status = 'reserved' THEN 0
					WHEN status = 'settled' AND actual_nano_usd > 0 THEN actual_nano_usd
					WHEN status NOT IN ('reserved', 'released', 'usage_missing') AND actual_nano_usd > 0 THEN actual_nano_usd
					ELSE 0
				END AS spend_nano_usd,
				CASE
					WHEN actual_nano_usd > 0 THEN COALESCE(NULLIF(settled_at_ns, 0), created_at_ns)
					WHEN status = 'reserved' THEN 0
					ELSE 0
				END AS spend_at_ns
			FROM billing_requests
			WHERE user_id <> ''
				AND client_id <> ''
				AND request_id <> ''
		)
		GROUP BY user_id
		ON CONFLICT(user_id) DO UPDATE SET
			spend_nano_usd = CASE
				WHEN finance_user_rollups.spend_nano_usd + excluded.spend_nano_usd < 0 THEN 0
				ELSE finance_user_rollups.spend_nano_usd + excluded.spend_nano_usd
			END,
			usage_spend_nano_usd = CASE
				WHEN finance_user_rollups.usage_spend_nano_usd + excluded.usage_spend_nano_usd < 0 THEN 0
				ELSE finance_user_rollups.usage_spend_nano_usd + excluded.usage_spend_nano_usd
			END,
			plan_spend_nano_usd = CASE
				WHEN finance_user_rollups.plan_spend_nano_usd + excluded.plan_spend_nano_usd < 0 THEN 0
				ELSE finance_user_rollups.plan_spend_nano_usd + excluded.plan_spend_nano_usd
			END,
			last_spend_at_ns = CASE
				WHEN finance_user_rollups.last_spend_at_ns > excluded.last_spend_at_ns THEN finance_user_rollups.last_spend_at_ns
				ELSE excluded.last_spend_at_ns
			END
	`)
	return err
}

func rebuildFinanceClientRollupsTx(tx *backupTx) error {
	if _, err := tx.Exec(`DELETE FROM finance_client_rollups`); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO finance_client_rollups(client_id, client_name, owner_user_id, spend_limit_nano_usd, spend_used_nano_usd, usage_nano_usd, plan_nano_usd)
		SELECT c.id,
			COALESCE(NULLIF(TRIM(c.name), ''), c.id),
			COALESCE(NULLIF(TRIM(c.owner_user_id), ''), ''),
			COALESCE(cs.spend_limit_nano_usd, 0),
			0,
			0,
			0
		FROM clients c
		LEFT JOIN client_spend cs ON cs.client_id = c.id
		WHERE c.id <> ''
	`); err != nil {
		return err
	}
	return addFinanceClientUsageRollupsTx(tx)
}

func addFinanceClientUsageRollupsTx(tx *backupTx) error {
	_, err := tx.Exec(`
		WITH usage_rows AS (
			SELECT client_id,
				COALESCE(NULLIF(TRIM(client_name), ''), client_id) AS client_name,
				COALESCE(NULLIF(TRIM(user_id), ''), '') AS owner_user_id,
				CASE
					WHEN COALESCE(NULLIF(TRIM(billing_source), ''), 'cash') IN ('plan', 'package', 'subscription', 'entitlement') THEN 'plan'
					ELSE 'cash'
				END AS billing_source,
				CASE
					WHEN status = 'reserved' THEN 0
					WHEN status = 'settled' AND actual_nano_usd > 0 THEN actual_nano_usd
					WHEN status NOT IN ('reserved', 'released', 'usage_missing') AND actual_nano_usd > 0 THEN actual_nano_usd
					ELSE 0
				END AS spend_nano_usd
			FROM billing_requests
			WHERE client_id <> ''
				AND request_id <> ''
		),
		client_usage AS (
			SELECT client_id,
				COALESCE(MAX(NULLIF(TRIM(client_name), '')), client_id) AS client_name,
				COALESCE(MAX(NULLIF(TRIM(owner_user_id), '')), '') AS owner_user_id,
				COALESCE(SUM(CASE WHEN billing_source = 'plan' THEN 0 ELSE spend_nano_usd END), 0) AS spend_used_nano_usd,
				COALESCE(SUM(spend_nano_usd), 0) AS usage_nano_usd,
				COALESCE(SUM(CASE WHEN billing_source = 'plan' THEN spend_nano_usd ELSE 0 END), 0) AS plan_nano_usd
			FROM usage_rows
			GROUP BY client_id
		)
		INSERT INTO finance_client_rollups(client_id, client_name, owner_user_id, spend_limit_nano_usd, spend_used_nano_usd, usage_nano_usd, plan_nano_usd)
		SELECT client_id, client_name, owner_user_id, 0,
			CASE WHEN spend_used_nano_usd < 0 THEN 0 ELSE spend_used_nano_usd END,
			CASE WHEN usage_nano_usd < 0 THEN 0 ELSE usage_nano_usd END,
			CASE WHEN plan_nano_usd < 0 THEN 0 ELSE plan_nano_usd END
		FROM client_usage
		WHERE client_id <> '' AND usage_nano_usd > 0
		ON CONFLICT(client_id) DO UPDATE SET
			client_name = COALESCE(NULLIF(excluded.client_name, ''), finance_client_rollups.client_name),
			owner_user_id = COALESCE(NULLIF(finance_client_rollups.owner_user_id, ''), NULLIF(excluded.owner_user_id, ''), ''),
			spend_used_nano_usd = excluded.spend_used_nano_usd,
			usage_nano_usd = excluded.usage_nano_usd,
			plan_nano_usd = excluded.plan_nano_usd
	`)
	return err
}

func rebuildFinanceModelRollupsTx(tx *backupTx) error {
	if _, err := tx.Exec(`DELETE FROM finance_model_rollups`); err != nil {
		return err
	}
	_, err := tx.Exec(`
		INSERT INTO finance_model_rollups(model, request_count, prompt_tokens, completion_tokens, spend_nano_usd)
		SELECT COALESCE(NULLIF(TRIM(model), ''), 'unknown') AS model_name,
			COUNT(*),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(SUM(CASE
				WHEN status IN ('reserved', 'released', 'usage_missing') THEN 0
				WHEN status = 'settled' AND actual_nano_usd > 0 THEN actual_nano_usd
				WHEN actual_nano_usd > 0 THEN actual_nano_usd
				ELSE 0
			END), 0) AS spend_nano_usd
		FROM billing_requests
		GROUP BY model_name
	`)
	return err
}

type logicalClientSpendRow struct {
	limitNanoUSD int64
	updatedAtNS  int64
}

func reconcileClientSpendFromBillingTx(tx *backupTx) error {
	clients, err := logicalClientSpendLimitsTx(tx)
	if err != nil {
		return err
	}
	spend, err := logicalClientSpendUsageFromBillingTx(tx)
	if err != nil {
		return err
	}
	for clientID, row := range clients {
		used := spend[clientID]
		updatedAtNS := row.updatedAtNS
		if updatedAtNS <= 0 {
			updatedAtNS = time.Now().UTC().UnixNano()
		}
		if _, err := tx.Exec(
			`INSERT INTO client_spend(client_id, spend_limit_nano_usd, spend_used_nano_usd, updated_at_ns)
			VALUES(?, ?, ?, ?)
			ON CONFLICT(client_id) DO UPDATE SET
				spend_limit_nano_usd = excluded.spend_limit_nano_usd,
				spend_used_nano_usd = excluded.spend_used_nano_usd,
				updated_at_ns = excluded.updated_at_ns`,
			clientID,
			row.limitNanoUSD,
			used,
			updatedAtNS,
		); err != nil {
			return err
		}
	}
	return nil
}

func clearClientSpendUsageTx(tx *backupTx) error {
	_, err := tx.Exec(`UPDATE client_spend SET spend_used_nano_usd = 0`)
	return err
}

func logicalClientSpendLimitsTx(tx *backupTx) (map[string]logicalClientSpendRow, error) {
	clients := make(map[string]logicalClientSpendRow)
	rows, err := tx.Query(`SELECT id, payload FROM clients`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, payload string
		if err := rows.Scan(&id, &payload); err != nil {
			_ = rows.Close()
			return nil, err
		}
		var client struct {
			SpendLimitNanoUSD int64
		}
		if err := json.Unmarshal([]byte(payload), &client); err != nil {
			_ = rows.Close()
			return nil, err
		}
		clients[strings.TrimSpace(id)] = logicalClientSpendRow{limitNanoUSD: client.SpendLimitNanoUSD}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	rows, err = tx.Query(`SELECT client_id, spend_limit_nano_usd, updated_at_ns FROM client_spend`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var clientID string
		var limitNanoUSD, updatedAtNS int64
		if err := rows.Scan(&clientID, &limitNanoUSD, &updatedAtNS); err != nil {
			return nil, err
		}
		clientID = strings.TrimSpace(clientID)
		if _, ok := clients[clientID]; !ok {
			continue
		}
		clients[clientID] = logicalClientSpendRow{limitNanoUSD: limitNanoUSD, updatedAtNS: updatedAtNS}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return clients, nil
}

func logicalClientSpendUsageFromBillingTx(tx *backupTx) (map[string]int64, error) {
	ledgerSpend, ledgerClients, err := logicalClientSpendUsageFromLedgerTx(tx)
	if err != nil {
		return nil, err
	}
	requestSpend, err := logicalClientSpendUsageFromRequestsTx(tx)
	if err != nil {
		return nil, err
	}
	for clientID, used := range requestSpend {
		if ledgerClients[clientID] {
			continue
		}
		ledgerSpend[clientID] = used
	}
	return ledgerSpend, nil
}

func logicalClientSpendUsageFromLedgerTx(tx *backupTx) (map[string]int64, map[string]bool, error) {
	spend := make(map[string]int64)
	clients := make(map[string]bool)
	rows, err := tx.Query(`SELECT client_id, kind, amount_nano_usd FROM billing_ledger WHERE client_id <> ''`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var clientID, kind string
		var amountNanoUSD int64
		if err := rows.Scan(&clientID, &kind, &amountNanoUSD); err != nil {
			return nil, nil, err
		}
		clientID = strings.TrimSpace(clientID)
		if clientID == "" {
			continue
		}
		amount := logicalBillingLedgerSpendDelta(kind, amountNanoUSD)
		if amount == 0 {
			continue
		}
		clients[clientID] = true
		spend[clientID] = addLogicalNanoUSDSaturating(spend[clientID], amount)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for clientID, used := range spend {
		if used < 0 {
			spend[clientID] = 0
		}
	}
	if err := addLogicalClientSpendUsageFromPlanLedgerTx(tx, spend, clients); err != nil {
		return nil, nil, err
	}
	return spend, clients, nil
}

func addLogicalClientSpendUsageFromPlanLedgerTx(tx *backupTx, spend map[string]int64, clients map[string]bool) error {
	rows, err := tx.Query(`SELECT client_id, kind, amount_nano_usd FROM plan_quota_ledger WHERE client_id <> ''`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var clientID, kind string
		var amountNanoUSD int64
		if err := rows.Scan(&clientID, &kind, &amountNanoUSD); err != nil {
			return err
		}
		clientID = strings.TrimSpace(clientID)
		if clientID == "" {
			continue
		}
		amount := logicalBillingLedgerSpendDelta(kind, amountNanoUSD)
		if amount == 0 {
			continue
		}
		clients[clientID] = true
		spend[clientID] = addLogicalNanoUSDSaturating(spend[clientID], amount)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for clientID, used := range spend {
		if used < 0 {
			spend[clientID] = 0
		}
	}
	return nil
}

func logicalClientSpendUsageFromRequestsTx(tx *backupTx) (map[string]int64, error) {
	rows, err := tx.Query(`SELECT client_id, status, reserved_nano_usd, actual_nano_usd FROM billing_requests`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	spend := make(map[string]int64)
	for rows.Next() {
		var clientID, status string
		var reservedNanoUSD, actualNanoUSD int64
		if err := rows.Scan(&clientID, &status, &reservedNanoUSD, &actualNanoUSD); err != nil {
			return nil, err
		}
		amount := logicalBillingRequestSpend(status, reservedNanoUSD, actualNanoUSD)
		if amount <= 0 {
			continue
		}
		spend[strings.TrimSpace(clientID)] = addLogicalNanoUSDSaturating(spend[strings.TrimSpace(clientID)], amount)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return spend, nil
}

func logicalBillingLedgerSpendDelta(kind string, amountNanoUSD int64) int64 {
	switch strings.TrimSpace(kind) {
	case "settle":
		return -amountNanoUSD
	default:
		return 0
	}
}

func logicalBillingRequestSpend(status string, reservedNanoUSD, actualNanoUSD int64) int64 {
	switch strings.TrimSpace(status) {
	case "reserved":
		return 0
	case "settled":
		if actualNanoUSD > 0 {
			return actualNanoUSD
		}
	default:
		if actualNanoUSD > 0 {
			return actualNanoUSD
		}
	}
	return 0
}

func addLogicalNanoUSDSaturating(a, b int64) int64 {
	if b > 0 && a > (1<<63-1)-b {
		return 1<<63 - 1
	}
	if b < 0 && a < (-1<<63)-b {
		return -1 << 63
	}
	return a + b
}

func preflightLogicalRestoreTx(tx *backupTx, dataSets []string) error {
	selected := dataSetSelection(dataSets)
	if selected[DataSetUsers] {
		if !selected[DataSetMessages] {
			if hasRows, table, err := anyTableHasRowsTx(tx, []string{"site_message_reads"}); err != nil {
				return err
			} else if hasRows {
				return fmt.Errorf("restore data type %q together with %q before replacing users; existing %s rows would otherwise be removed or orphaned", DataSetMessages, DataSetUsers, table)
			}
		}
	}
	return nil
}

func anyTableHasRowsTx(tx *backupTx, tables []string) (bool, string, error) {
	for _, table := range tables {
		var marker int
		err := tx.QueryRow(fmt.Sprintf(`SELECT 1 FROM %s LIMIT 1`, table)).Scan(&marker)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, "", err
		}
		return true, table, nil
	}
	return false, "", nil
}

func validateLogicalRestoreReferencesTx(tx *backupTx, dataSets []string) error {
	if err := validateForeignKeysTx(tx); err != nil {
		return err
	}
	selected := dataSetSelection(dataSets)
	if selected[DataSetUsers] || selected[DataSetClients] {
		return validateClientOwnersTx(tx)
	}
	return nil
}

var postgresForeignKeyChecks = []struct {
	childTable  string
	childColumn string
	parentTable string
	ignoreEmpty bool
}{
	{childTable: "user_balances", childColumn: "user_id", parentTable: "users"},
	{childTable: "user_sessions", childColumn: "user_id", parentTable: "users"},
	{childTable: "user_oauth_identities", childColumn: "user_id", parentTable: "users"},
	{childTable: "user_invitation_codes", childColumn: "user_id", parentTable: "users"},
	{childTable: "mcp_tokens", childColumn: "owner_user_id", parentTable: "users"},
	{childTable: "client_spend", childColumn: "client_id", parentTable: "clients"},
	{childTable: "site_message_reads", childColumn: "message_id", parentTable: "site_messages"},
	{childTable: "site_message_reads", childColumn: "user_id", parentTable: "users"},
	{childTable: "audit_terms", childColumn: "seq", parentTable: "audit"},
	{childTable: "account_credentials", childColumn: "account_id", parentTable: "accounts"},
	{childTable: "account_runtime", childColumn: "account_id", parentTable: "accounts"},
	{childTable: "openai_response_bindings", childColumn: "account_id", parentTable: "accounts"},
	{childTable: "openai_response_bindings", childColumn: "client_id", parentTable: "clients", ignoreEmpty: true},
}

func validateForeignKeysTx(tx *backupTx) error {
	switch tx.backend {
	case databaseBackendSQLite:
		rows, err := tx.Query(`PRAGMA foreign_key_check`)
		if err != nil {
			return err
		}
		defer rows.Close()
		if rows.Next() {
			var tableName, parent string
			var rowID sql.NullInt64
			var fkID int
			if err := rows.Scan(&tableName, &rowID, &parent, &fkID); err != nil {
				return err
			}
			rowText := "unknown"
			if rowID.Valid {
				rowText = fmt.Sprintf("%d", rowID.Int64)
			}
			return fmt.Errorf("logical restore would violate foreign key: table %s row %s references %s (fk %d)", tableName, rowText, parent, fkID)
		}
		return rows.Err()
	case databaseBackendPostgres:
		for _, check := range postgresForeignKeyChecks {
			query := postgresForeignKeyViolationQuery(check)
			var marker int
			err := tx.QueryRow(query).Scan(&marker)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			return fmt.Errorf("logical restore would violate foreign key: table %s references %s via %s", check.childTable, check.parentTable, check.childColumn)
		}
		return nil
	default:
		return nil
	}
}

func postgresForeignKeyViolationQuery(check struct {
	childTable  string
	childColumn string
	parentTable string
	ignoreEmpty bool
}) string {
	emptyFilter := ""
	if check.ignoreEmpty {
		emptyFilter = fmt.Sprintf(`%s.%s <> '' AND `, check.childTable, check.childColumn)
	}
	return fmt.Sprintf(`SELECT 1 FROM %s WHERE %sNOT EXISTS (SELECT 1 FROM %s WHERE %s.id = %s.%s) LIMIT 1`, check.childTable, emptyFilter, check.parentTable, check.parentTable, check.childTable, check.childColumn)
}

func resetRestoreSequencesTx(tx *backupTx, backend string) error {
	switch backend {
	case databaseBackendSQLite:
		for _, table := range []string{"audit", "billing_ledger"} {
			if _, err := tx.Exec(`DELETE FROM sqlite_sequence WHERE name = ?`, table); err != nil {
				return err
			}
		}
	case databaseBackendPostgres:
		for _, table := range []string{"audit", "billing_ledger"} {
			var maxSeq int64
			if err := tx.QueryRow(fmt.Sprintf(`SELECT COALESCE(MAX(seq), 0) FROM %s`, table)).Scan(&maxSeq); err != nil {
				return err
			}
			isCalled := maxSeq > 0
			if !isCalled {
				maxSeq = 1
			}
			if _, err := tx.Exec(fmt.Sprintf(`SELECT setval(pg_get_serial_sequence('%s', 'seq'), ?, ?)`, table), maxSeq, isCalled); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateClientOwnersTx(tx *backupTx) error {
	rows, err := tx.Query(`SELECT id, payload FROM clients`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return err
		}
		var client struct {
			OwnerUserID string
		}
		if err := json.Unmarshal([]byte(payload), &client); err != nil {
			return err
		}
		ownerID := strings.TrimSpace(client.OwnerUserID)
		if ownerID == "" {
			continue
		}
		var marker int
		err := tx.QueryRow(`SELECT 1 FROM users WHERE id = ? LIMIT 1`, ownerID).Scan(&marker)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("logical restore would leave client %s owned by missing user %s", id, ownerID)
		}
		if err != nil {
			return err
		}
	}
	return rows.Err()
}

func dataSetSelection(dataSets []string) map[string]bool {
	selected := make(map[string]bool, len(dataSets))
	for _, dataSet := range normalizeDataSets(dataSets) {
		selected[dataSet] = true
	}
	return selected
}

func extractedStatePath(tempDir string) (string, error) {
	stateDir := filepath.Join(tempDir, "state")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") {
			continue
		}
		return filepath.Join(stateDir, name), nil
	}
	return "", errors.New("backup database file not found")
}

func tableColumnSet(tx *backupTx, table string) (map[string]struct{}, error) {
	columns := map[string]struct{}{}
	switch tx.backend {
	case databaseBackendPostgres:
		rows, err := tx.Query(`SELECT column_name FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = ? ORDER BY ordinal_position`, table)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return nil, err
			}
			columns[name] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	default:
		rows, err := tx.Query("PRAGMA table_info(" + table + ")")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var cid int
			var name, columnType string
			var notNull int
			var defaultValue any
			var pk int
			if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
				return nil, err
			}
			columns[name] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("table %s has no columns", table)
	}
	return columns, nil
}

func tablesForDataSets(dataSets []string) []string {
	seen := map[string]struct{}{}
	var tables []string
	for _, dataSet := range normalizeDataSets(dataSets) {
		for _, table := range dataSetTables[dataSet] {
			if _, ok := seen[table]; ok {
				continue
			}
			seen[table] = struct{}{}
			tables = append(tables, table)
		}
	}
	return tables
}

func Inspect(r io.Reader) (Manifest, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return Manifest{}, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return Manifest{}, errors.New("manifest not found")
		}
		if err != nil {
			return Manifest{}, err
		}
		if header.Name != "manifest.json" {
			continue
		}
		var manifest Manifest
		if err := json.NewDecoder(tr).Decode(&manifest); err != nil {
			return Manifest{}, err
		}
		return manifest, nil
	}
}

func writeManifest(tw *tar.Writer, manifest Manifest) error {
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return writeBytes(tw, "manifest.json", raw, 0o644)
}

func writeBytes(tw *tar.Writer, name string, raw []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(raw)), ModTime: time.Now().UTC()}); err != nil {
		return err
	}
	_, err := tw.Write(raw)
	return err
}

func addFile(tw *tar.Writer, src, name string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("backup path is not a regular file: %s", src)
	}
	file, err := os.Open(src)
	if err != nil {
		return err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return err
	}
	if !openedInfo.Mode().IsRegular() {
		return fmt.Errorf("backup path is not a regular file: %s", src)
	}
	if !os.SameFile(info, openedInfo) {
		return fmt.Errorf("backup path changed while reading: %s", src)
	}
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = filepath.ToSlash(name)
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err = io.Copy(tw, file)
	return err
}

func addSQLiteDatabaseSnapshot(tw *tar.Writer, statePath string) error {
	statePath = strings.TrimSpace(statePath)
	if statePath == "" {
		return errors.New("state path is required")
	}
	tempDir, err := os.MkdirTemp("", "ai-gateway-sqlite-backup-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)

	snapshotPath := filepath.Join(tempDir, filepath.Base(statePath))
	db, err := sql.Open("sqlite", statePath)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		_ = db.Close()
		return fmt.Errorf("configure sqlite backup timeout: %w", err)
	}
	if _, err := db.Exec(`VACUUM INTO ?`, snapshotPath); err != nil {
		_ = db.Close()
		return fmt.Errorf("snapshot sqlite database: %w", err)
	}
	if err := db.Close(); err != nil {
		return err
	}
	if err := removeLegacyTraceSnapshotData(snapshotPath); err != nil {
		return err
	}
	return addFile(tw, snapshotPath, "state/"+filepath.Base(statePath))
}

func removeLegacyTraceSnapshotData(snapshotPath string) error {
	db, err := sql.Open("sqlite", snapshotPath)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	var tableCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'gateway_traces'`).Scan(&tableCount); err != nil {
		return fmt.Errorf("inspect legacy trace snapshot table: %w", err)
	}
	if tableCount == 0 {
		return nil
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS gateway_traces`); err != nil {
		return fmt.Errorf("remove legacy trace snapshot table: %w", err)
	}
	if _, err := db.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("compact sqlite snapshot after legacy trace removal: %w", err)
	}
	return nil
}

func extract(r io.Reader, target string) (Manifest, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return Manifest{}, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var manifest Manifest
	extractedBytes := int64(0)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, err
		}
		clean := filepath.Clean(header.Name)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
			return Manifest{}, fmt.Errorf("unsafe backup path %q", header.Name)
		}
		path := filepath.Join(target, clean)
		switch header.Typeflag {
		case tar.TypeDir:
			mode, err := backupEntryMode(header.Mode)
			if err != nil {
				return Manifest{}, err
			}
			if err := os.MkdirAll(path, mode); err != nil {
				return Manifest{}, err
			}
			continue
		case tar.TypeReg:
		default:
			return Manifest{}, fmt.Errorf("unsupported backup entry type %q for %q", header.Typeflag, header.Name)
		}
		if header.Size < 0 {
			return Manifest{}, fmt.Errorf("backup entry %q has invalid size", header.Name)
		}
		if header.Size > maxBackupExtractEntryBytes {
			return Manifest{}, fmt.Errorf("backup entry %q exceeds maximum extracted size", header.Name)
		}
		if extractedBytes > maxBackupExtractTotalBytes-header.Size {
			return Manifest{}, errors.New("backup exceeds maximum extracted size")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return Manifest{}, err
		}
		mode, err := backupEntryMode(header.Mode)
		if err != nil {
			return Manifest{}, err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return Manifest{}, err
		}
		written, err := io.CopyN(file, tr, header.Size)
		if err != nil {
			_ = file.Close()
			return Manifest{}, err
		}
		if written != header.Size {
			_ = file.Close()
			return Manifest{}, fmt.Errorf("backup entry %q size mismatch", header.Name)
		}
		extractedBytes += written
		if err := file.Close(); err != nil {
			return Manifest{}, err
		}
		if clean == "manifest.json" {
			raw, err := os.ReadFile(path)
			if err != nil {
				return Manifest{}, err
			}
			if err := json.Unmarshal(raw, &manifest); err != nil {
				return Manifest{}, err
			}
		}
	}
	if manifest.Format == "" {
		return Manifest{}, errors.New("manifest not found")
	}
	return manifest, nil
}

func backupEntryMode(raw int64) (os.FileMode, error) {
	if raw < 0 {
		return 0, fmt.Errorf("backup entry mode is invalid: %d", raw)
	}
	if raw&^int64(0o777) != 0 {
		return 0, fmt.Errorf("backup entry mode is invalid: %d", raw)
	}
	var mode os.FileMode
	if raw&0o400 != 0 {
		mode |= 0o400
	}
	if raw&0o200 != 0 {
		mode |= 0o200
	}
	if raw&0o100 != 0 {
		mode |= 0o100
	}
	if raw&0o040 != 0 {
		mode |= 0o040
	}
	if raw&0o020 != 0 {
		mode |= 0o020
	}
	if raw&0o010 != 0 {
		mode |= 0o010
	}
	if raw&0o004 != 0 {
		mode |= 0o004
	}
	if raw&0o002 != 0 {
		mode |= 0o002
	}
	if raw&0o001 != 0 {
		mode |= 0o001
	}
	return mode, nil
}

func restoreStateFiles(tempDir, statePath string) error {
	stateDir := filepath.Join(tempDir, "state")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		return err
	}
	sourceBase := ""
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") {
			continue
		}
		sourceBase = name
		break
	}
	if sourceBase == "" {
		return errors.New("backup database file not found")
	}
	_ = os.Remove(statePath)
	_ = os.Remove(statePath + "-wal")
	_ = os.Remove(statePath + "-shm")
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		dst := ""
		switch name {
		case sourceBase:
			dst = statePath
		case sourceBase + "-wal":
			dst = statePath + "-wal"
		case sourceBase + "-shm":
			dst = statePath + "-shm"
		default:
			continue
		}
		if err := replaceFile(filepath.Join(stateDir, name), dst, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func replaceFile(src, dst string, mode os.FileMode) error {
	if !fileExists(src) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	temp := dst + ".restore-tmp"
	defer os.Remove(temp)
	input, err := os.Open(src)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	return os.Rename(temp, dst)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
