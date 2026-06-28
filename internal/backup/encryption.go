package backup

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/scrypt"
)

const (
	encryptedValuePrefixV3 = "enc:v3:aesgcm:"
	kdfSaltBytes           = 16
)

type EncryptionInspection struct {
	Manifest  Manifest `json:"manifest"`
	Encrypted bool     `json:"encrypted"`

	sample string
}

type backupCredentialCodec struct {
	dataAEAD cipher.AEAD
}

var encryptedPayloadTables = map[string]struct{}{
	"system_settings":     {},
	"accounts":            {},
	"account_credentials": {},
	"account_groups":      {},
	"clients":             {},
}

func InspectEncryption(r io.Reader, requestedDataSets []string) (EncryptionInspection, error) {
	tempDir, err := os.MkdirTemp("", "ai-gateway-inspect-*")
	if err != nil {
		return EncryptionInspection{}, err
	}
	defer os.RemoveAll(tempDir)

	manifest, err := extract(r, tempDir)
	if err != nil {
		return EncryptionInspection{}, err
	}
	if manifest.Format != Format || manifest.Version != Version {
		return EncryptionInspection{}, fmt.Errorf("unsupported backup format %q version %d", manifest.Format, manifest.Version)
	}
	return inspectExtractedEncryption(tempDir, manifest, requestedDataSets)
}

func (i EncryptionInspection) RequiresSourceMasterKey(masterKey string) bool {
	if !i.Encrypted {
		return false
	}
	if strings.TrimSpace(i.sample) == "" {
		return true
	}
	codec, err := newBackupCredentialCodec(masterKey)
	if err != nil || codec == nil {
		return true
	}
	_, err = codec.decryptValue(i.sample)
	return err != nil
}

func prepareEncryptedRestore(tempDir string, manifest Manifest, requestedDataSets []string, opts Options) error {
	inspection, err := inspectExtractedEncryption(tempDir, manifest, requestedDataSets)
	if err != nil {
		return err
	}
	if !inspection.Encrypted {
		return nil
	}
	decryptCodec, err := chooseEncryptedRestoreCodec(inspection.sample, opts.TargetMasterKey, opts.SourceMasterKey)
	if err != nil {
		return err
	}
	targetCodec, err := newBackupCredentialCodec(opts.TargetMasterKey)
	if err != nil {
		return err
	}
	if manifest.Includes.Data {
		if err := transformLogicalDataFile(filepath.Join(tempDir, "data", "logical.json"), requestedDataSets, decryptCodec, targetCodec); err != nil {
			return err
		}
	}
	if manifest.Includes.Database {
		if err := transformExtractedStateDatabase(tempDir, requestedDataSets, decryptCodec, targetCodec); err != nil {
			return err
		}
	}
	return nil
}

func targetMasterKeyForRestore(tempDir string, manifest Manifest, selectiveDataRestore bool, opts Options) (string, error) {
	if !manifest.Includes.Config || selectiveDataRestore || strings.TrimSpace(opts.ConfigPath) == "" {
		return opts.TargetMasterKey, nil
	}
	masterKey, found, err := masterKeyFromExtractedConfig(filepath.Join(tempDir, "config.json"))
	if err != nil {
		return "", err
	}
	if found {
		return masterKey, nil
	}
	return opts.TargetMasterKey, nil
}

func chooseEncryptedRestoreCodec(sample, targetMasterKey, sourceMasterKey string) (*backupCredentialCodec, error) {
	tryKey := func(key string) (*backupCredentialCodec, error) {
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, nil
		}
		codec, err := newBackupCredentialCodec(key)
		if err != nil {
			return nil, err
		}
		if codec == nil {
			return nil, nil
		}
		if _, err := codec.decryptValue(sample); err != nil {
			return nil, err
		}
		return codec, nil
	}

	if codec, err := tryKey(targetMasterKey); err != nil {
		if sourceMasterKey == "" {
			return nil, errors.New("backup contains encrypted data; source master_key is required")
		}
	} else if codec != nil {
		return codec, nil
	}

	if codec, err := tryKey(sourceMasterKey); err != nil {
		return nil, fmt.Errorf("decrypt backup encrypted data with source master_key: %w", err)
	} else if codec != nil {
		return codec, nil
	}

	return nil, errors.New("backup contains encrypted data; source master_key is required")
}

func masterKeyFromExtractedConfig(path string) (string, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var payload struct {
		MasterKey string `json:"master_key"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", false, err
	}
	return strings.TrimSpace(payload.MasterKey), true, nil
}

func inspectExtractedEncryption(tempDir string, manifest Manifest, requestedDataSets []string) (EncryptionInspection, error) {
	inspection := EncryptionInspection{Manifest: manifest}
	if manifest.Includes.Data {
		sample, err := firstEncryptedValueInLogicalDataFile(filepath.Join(tempDir, "data", "logical.json"), requestedDataSets)
		if err != nil {
			return EncryptionInspection{}, err
		}
		if sample != "" {
			inspection.Encrypted = true
			inspection.sample = sample
			return inspection, nil
		}
	}
	if manifest.Includes.Database {
		dataSets, err := dataSetsForStateInspection(requestedDataSets)
		if err != nil {
			return EncryptionInspection{}, err
		}
		sample, err := firstEncryptedValueInExtractedState(tempDir, dataSets)
		if err != nil {
			return EncryptionInspection{}, err
		}
		if sample != "" {
			inspection.Encrypted = true
			inspection.sample = sample
		}
	}
	return inspection, nil
}

func firstEncryptedValueInLogicalDataFile(path string, requestedDataSets []string) (string, error) {
	data, dataSets, err := readLogicalDataForSelection(path, requestedDataSets)
	if err != nil {
		return "", err
	}
	tables := encryptedTablesForDataSets(dataSets)
	if len(tables) == 0 {
		return "", nil
	}
	for _, table := range data.Tables {
		if _, ok := tables[table.Name]; !ok {
			continue
		}
		payloadIndex := logicalPayloadColumnIndex(table)
		if payloadIndex < 0 {
			continue
		}
		for _, row := range table.Rows {
			if payloadIndex >= len(row) {
				return "", fmt.Errorf("table %s row has %d values for %d columns", table.Name, len(row), len(table.Columns))
			}
			payload, err := decodeLogicalPayloadCell(row[payloadIndex], table.Name)
			if err != nil {
				return "", err
			}
			sample, err := firstEncryptedValueInJSONPayload(payload)
			if err != nil {
				return "", fmt.Errorf("inspect encrypted %s payload: %w", table.Name, err)
			}
			if sample != "" {
				return sample, nil
			}
		}
	}
	return "", nil
}

func transformLogicalDataFile(path string, requestedDataSets []string, sourceCodec, targetCodec *backupCredentialCodec) error {
	data, dataSets, err := readLogicalDataForSelection(path, requestedDataSets)
	if err != nil {
		return err
	}
	tables := encryptedTablesForDataSets(dataSets)
	if len(tables) == 0 {
		return nil
	}
	changed := false
	for tableIndex := range data.Tables {
		table := &data.Tables[tableIndex]
		if _, ok := tables[table.Name]; !ok {
			continue
		}
		payloadIndex := logicalPayloadColumnIndex(*table)
		if payloadIndex < 0 {
			continue
		}
		for rowIndex := range table.Rows {
			row := table.Rows[rowIndex]
			if payloadIndex >= len(row) {
				return fmt.Errorf("table %s row has %d values for %d columns", table.Name, len(row), len(table.Columns))
			}
			payload, err := decodeLogicalPayloadCell(row[payloadIndex], table.Name)
			if err != nil {
				return err
			}
			nextPayload, payloadChanged, err := transformEncryptedJSONPayload(payload, sourceCodec, targetCodec)
			if err != nil {
				return fmt.Errorf("transform encrypted %s payload: %w", table.Name, err)
			}
			if !payloadChanged {
				continue
			}
			raw, err := json.Marshal(nextPayload)
			if err != nil {
				return err
			}
			table.Rows[rowIndex][payloadIndex] = raw
			changed = true
		}
	}
	if !changed {
		return nil
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o600)
}

func firstEncryptedValueInExtractedState(tempDir string, dataSets []string) (string, error) {
	tables := encryptedTablesForDataSets(dataSets)
	if len(tables) == 0 {
		return "", nil
	}
	statePath, err := extractedStatePath(tempDir)
	if err != nil {
		return "", err
	}
	db, err := sql.Open("sqlite", statePath)
	if err != nil {
		return "", err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	for table := range tables {
		rows, err := db.Query(fmt.Sprintf(`SELECT payload FROM %s`, table))
		if err != nil {
			return "", err
		}
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				_ = rows.Close()
				return "", err
			}
			sample, err := firstEncryptedValueInJSONPayload(payload)
			if err != nil {
				_ = rows.Close()
				return "", fmt.Errorf("inspect encrypted %s payload: %w", table, err)
			}
			if sample != "" {
				_ = rows.Close()
				return sample, nil
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return "", err
		}
		if err := rows.Close(); err != nil {
			return "", err
		}
	}
	return "", nil
}

func transformExtractedStateDatabase(tempDir string, requestedDataSets []string, sourceCodec, targetCodec *backupCredentialCodec) error {
	dataSets, err := dataSetsForStateInspection(requestedDataSets)
	if err != nil {
		return err
	}
	tables := encryptedTablesForDataSets(dataSets)
	if len(tables) == 0 {
		return nil
	}
	statePath, err := extractedStatePath(tempDir)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", statePath)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	wrapped := &backupTx{tx: tx, backend: databaseBackendSQLite}
	for table := range tables {
		rows, err := wrapped.Query(fmt.Sprintf(`SELECT rowid, payload FROM %s`, table))
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		type payloadUpdate struct {
			rowID   int64
			payload string
		}
		updates := make([]payloadUpdate, 0)
		for rows.Next() {
			var rowID int64
			var payload string
			if err := rows.Scan(&rowID, &payload); err != nil {
				_ = rows.Close()
				_ = tx.Rollback()
				return err
			}
			nextPayload, changed, err := transformEncryptedJSONPayload(payload, sourceCodec, targetCodec)
			if err != nil {
				_ = rows.Close()
				_ = tx.Rollback()
				return fmt.Errorf("transform encrypted %s payload: %w", table, err)
			}
			if changed {
				updates = append(updates, payloadUpdate{rowID: rowID, payload: nextPayload})
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			_ = tx.Rollback()
			return err
		}
		if err := rows.Close(); err != nil {
			_ = tx.Rollback()
			return err
		}
		for _, update := range updates {
			if _, err := wrapped.Exec(fmt.Sprintf(`UPDATE %s SET payload = ? WHERE rowid = ?`, table), update.payload, update.rowID); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
	}
	return tx.Commit()
}

func readLogicalDataForSelection(path string, requestedDataSets []string) (logicalDataBackup, []string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return logicalDataBackup{}, nil, err
	}
	var data logicalDataBackup
	if err := json.Unmarshal(raw, &data); err != nil {
		return logicalDataBackup{}, nil, err
	}
	dataSets := normalizeKnownDataSets(data.DataSets)
	requested, err := normalizeDataSetsStrict(requestedDataSets)
	if err != nil {
		return logicalDataBackup{}, nil, err
	}
	if len(requested) == 0 {
		return data, dataSets, nil
	}
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
		return logicalDataBackup{}, nil, errors.New("selected data types are not included in backup")
	}
	return data, selected, nil
}

func dataSetsForStateInspection(requestedDataSets []string) ([]string, error) {
	dataSets, err := normalizeDataSetsStrict(requestedDataSets)
	if err != nil {
		return nil, err
	}
	if len(dataSets) == 0 {
		return AllDataSets(), nil
	}
	return dataSets, nil
}

func encryptedTablesForDataSets(dataSets []string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, table := range tablesForDataSets(dataSets) {
		if _, ok := encryptedPayloadTables[table]; ok {
			out[table] = struct{}{}
		}
	}
	return out
}

func logicalPayloadColumnIndex(table logicalTableBackup) int {
	for index, column := range table.Columns {
		if column == "payload" {
			return index
		}
	}
	return -1
}

func decodeLogicalPayloadCell(raw json.RawMessage, table string) (string, error) {
	var payload string
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("table %s payload is not a string: %w", table, err)
	}
	return payload, nil
}

func firstEncryptedValueInJSONPayload(payload string) (string, error) {
	if !jsonPayloadMayContainEncryptedValue(payload) {
		return "", nil
	}
	value, err := decodeJSONPayload(payload)
	if err != nil {
		return "", err
	}
	return firstEncryptedValueInJSON(value), nil
}

func firstEncryptedValueInJSON(value any) string {
	switch typed := value.(type) {
	case string:
		if isEncryptedValue(typed) {
			return typed
		}
	case []any:
		for _, item := range typed {
			if sample := firstEncryptedValueInJSON(item); sample != "" {
				return sample
			}
		}
	case map[string]any:
		for _, item := range typed {
			if sample := firstEncryptedValueInJSON(item); sample != "" {
				return sample
			}
		}
	}
	return ""
}

func transformEncryptedJSONPayload(payload string, sourceCodec, targetCodec *backupCredentialCodec) (string, bool, error) {
	if !jsonPayloadMayContainEncryptedValue(payload) {
		return payload, false, nil
	}
	value, err := decodeJSONPayload(payload)
	if err != nil {
		return "", false, err
	}
	next, changed, err := transformEncryptedJSONValue(value, sourceCodec, targetCodec)
	if err != nil {
		return "", false, err
	}
	if !changed {
		return payload, false, nil
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return "", false, err
	}
	return string(raw), true, nil
}

func transformEncryptedJSONValue(value any, sourceCodec, targetCodec *backupCredentialCodec) (any, bool, error) {
	switch typed := value.(type) {
	case string:
		if !isEncryptedValue(typed) {
			return typed, false, nil
		}
		if sourceCodec == nil {
			return nil, false, errors.New("source master_key is required")
		}
		plain, err := sourceCodec.decryptValue(typed)
		if err != nil {
			return nil, false, err
		}
		encoded, err := targetCodec.encryptValue(plain)
		if err != nil {
			return nil, false, err
		}
		return encoded, true, nil
	case []any:
		changed := false
		for index, item := range typed {
			next, itemChanged, err := transformEncryptedJSONValue(item, sourceCodec, targetCodec)
			if err != nil {
				return nil, false, err
			}
			if itemChanged {
				typed[index] = next
				changed = true
			}
		}
		return typed, changed, nil
	case map[string]any:
		changed := false
		for key, item := range typed {
			next, itemChanged, err := transformEncryptedJSONValue(item, sourceCodec, targetCodec)
			if err != nil {
				return nil, false, err
			}
			if itemChanged {
				typed[key] = next
				changed = true
			}
		}
		return typed, changed, nil
	default:
		return typed, false, nil
	}
}

func decodeJSONPayload(payload string) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader([]byte(payload)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func newBackupCredentialCodec(masterKey string) (*backupCredentialCodec, error) {
	masterKey = strings.TrimSpace(masterKey)
	if masterKey == "" {
		return nil, nil
	}
	dataAEAD, err := deriveV3AEAD(masterKey)
	if err != nil {
		return nil, err
	}
	return &backupCredentialCodec{dataAEAD: dataAEAD}, nil
}

func (c *backupCredentialCodec) encryptValue(value string) (string, error) {
	if c == nil || value == "" {
		return value, nil
	}
	aead := c.dataAEAD
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nil, nonce, []byte(value), nil)
	return encryptedValuePrefixV3 +
		base64.StdEncoding.EncodeToString(nonce) + ":" +
		base64.StdEncoding.EncodeToString(sealed), nil
}

func (c *backupCredentialCodec) decryptValue(value string) (string, error) {
	if !isEncryptedValue(value) {
		return value, nil
	}
	if c == nil {
		return "", errors.New("source master_key is required")
	}
	if strings.HasPrefix(value, encryptedValuePrefixV3) {
		return c.decryptV3Value(value)
	}
	return "", errors.New("unsupported encrypted credential format")
}

func (c *backupCredentialCodec) decryptV3Value(value string) (string, error) {
	payload := strings.TrimPrefix(value, encryptedValuePrefixV3)
	noncePart, cipherPart, ok := strings.Cut(payload, ":")
	if !ok {
		return "", errors.New("encrypted credential payload is malformed")
	}
	return decryptWithAEAD(c.dataAEAD, noncePart, cipherPart)
}

func deriveV3AEAD(masterKey string) (cipher.AEAD, error) {
	key, err := scrypt.Key([]byte(masterKey), []byte("ai-gateway-state-secret-v3"), 1<<15, 8, 1, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func decryptWithAEAD(aead cipher.AEAD, noncePart, cipherPart string) (string, error) {
	nonce, err := base64.StdEncoding.DecodeString(noncePart)
	if err != nil {
		return "", err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(cipherPart)
	if err != nil {
		return "", err
	}
	plain, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt credential: %w", err)
	}
	return string(plain), nil
}

func jsonPayloadMayContainEncryptedValue(payload string) bool {
	return strings.Contains(payload, encryptedValuePrefixV3)
}

func isEncryptedValue(value string) bool {
	return strings.HasPrefix(value, encryptedValuePrefixV3)
}
