package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"

	_ "modernc.org/sqlite"
)

func TestCreateAndRestoreBackup(t *testing.T) {
	source := t.TempDir()
	configPath := filepath.Join(source, "config.json")
	statePath := filepath.Join(source, "data", "state.db")
	mustWriteFile(t, configPath, `{"port":"8088"}`)
	initPhysicalTestDB(t, statePath, map[string]string{"answer": "source"})

	backupPath := filepath.Join(t.TempDir(), "backup.agbak")
	manifest, err := Create(backupPath, Options{
		ConfigPath: configPath,
		StatePath:  statePath,
		AppVersion: "test",
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if manifest.Format != Format || !manifest.Includes.Config || !manifest.Includes.Database {
		t.Fatalf("manifest = %#v", manifest)
	}

	target := t.TempDir()
	targetConfig := filepath.Join(target, "config.json")
	targetState := filepath.Join(target, "data", "state.db")
	mustWriteFile(t, targetConfig, "old")
	initPhysicalTestDB(t, targetState, map[string]string{"answer": "target"})

	preRestore, err := Restore(backupPath, Options{
		ConfigPath: targetConfig,
		StatePath:  targetState,
	}, target)
	if err != nil {
		t.Fatalf("Restore returned error: %v", err)
	}
	if _, err := os.Stat(preRestore); err != nil {
		t.Fatalf("pre-restore backup missing: %v", err)
	}
	assertFile(t, targetConfig, `{"port":"8088"}`)
	if got := getPhysicalValue(t, targetState, "answer"); got != "source" {
		t.Fatalf("restored database value = %q, want source", got)
	}
}

func TestRestoreRejectsUnsafePath(t *testing.T) {
	for _, name := range []string{"../escape", ".."} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{}); err == nil {
				t.Fatal("expected unsafe path to be rejected")
			}
		})
	}
}

func TestRestoreRejectsUnsupportedEntryType(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name:     "state/link",
		Typeflag: tar.TypeSymlink,
		Linkname: "/etc/passwd",
		Mode:     0o777,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{}); err == nil {
		t.Fatal("expected unsupported symlink entry to be rejected")
	}
}

func TestRestoreRejectsOversizedExtractedEntry(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	manifest := Manifest{Format: Format, Version: Version, CreatedAt: time.Now().UTC()}
	if err := writeManifest(tw, manifest); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "state/state.db",
		Mode: 0o600,
		Size: maxBackupExtractEntryBytes + 1,
	}); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{}); err == nil || !strings.Contains(err.Error(), "exceeds maximum extracted size") {
		t.Fatalf("ReadAndRestore err = %v, want extracted size error", err)
	}
}

func TestLogicalDataBackupRestoresSelectedTablesOnly(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, source)
	initLogicalTestDB(t, target)
	setLogicalPayload(t, source, "system_settings", "global", `{"name":"source-settings"}`)
	setLogicalPayload(t, source, "users", "source_user", `{"ID":"source_user","Username":"source"}`)
	setLogicalPayload(t, target, "system_settings", "global", `{"name":"target-settings"}`)
	setLogicalPayload(t, target, "users", "target_user", `{"ID":"target_user","Username":"target"}`)

	var buf bytes.Buffer
	manifest, err := Write(&buf, Options{StatePath: source, DataSets: []string{DataSetSettings}})
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if !manifest.Includes.Data || manifest.Includes.Database || len(manifest.Includes.DataSets) != 1 || manifest.Includes.DataSets[0] != DataSetSettings {
		t.Fatalf("manifest = %#v", manifest)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target}); err != nil {
		t.Fatalf("ReadAndRestore returned error: %v", err)
	}

	if got := getLogicalPayload(t, target, "system_settings", "key", "global"); got != `{"name":"source-settings"}` {
		t.Fatalf("settings payload = %s", got)
	}
	if got := getLogicalPayload(t, target, "users", "id", "target_user"); got != `{"ID":"target_user","Username":"target"}` {
		t.Fatalf("user payload = %s", got)
	}
}

func TestLogicalRestoreIgnoresUnknownBackupDataSetsWhenSelectingKnownData(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, source)
	initLogicalTestDB(t, target)
	setLogicalPayload(t, source, "system_settings", "global", `{"name":"source-settings"}`)
	setLogicalPayload(t, target, "system_settings", "global", `{"name":"target-settings"}`)

	var original bytes.Buffer
	if _, err := Write(&original, Options{StatePath: source, DataSets: []string{DataSetSettings}}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	mutated := addUnknownLogicalDataSetToBackup(t, original.Bytes(), "plugins")
	if err := ReadAndRestore(bytes.NewReader(mutated), Options{StatePath: target, DataSets: []string{DataSetSettings}}); err != nil {
		t.Fatalf("ReadAndRestore returned error: %v", err)
	}

	if got := getLogicalPayload(t, target, "system_settings", "key", "global"); got != `{"name":"source-settings"}` {
		t.Fatalf("settings payload = %s", got)
	}
}

func TestFullBackupCanRestoreSelectedLogicalDataOnly(t *testing.T) {
	sourceDir := t.TempDir()
	source := filepath.Join(sourceDir, "source.db")
	configPath := filepath.Join(sourceDir, "config.json")
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, source)
	initLogicalTestDB(t, target)
	mustWriteFile(t, configPath, `{"port":"source"}`)
	setLogicalPayload(t, source, "system_settings", "global", `{"name":"source-settings"}`)
	setLogicalPayload(t, source, "users", "source_user", `{"ID":"source_user","Username":"source"}`)
	setLogicalPayload(t, target, "system_settings", "global", `{"name":"target-settings"}`)
	setLogicalPayload(t, target, "users", "target_user", `{"ID":"target_user","Username":"target"}`)

	var buf bytes.Buffer
	manifest, err := Write(&buf, Options{ConfigPath: configPath, StatePath: source})
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if !manifest.Includes.Config || !manifest.Includes.Database || manifest.Includes.Data {
		t.Fatalf("manifest = %#v", manifest)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target, DataSets: []string{DataSetSettings}}); err != nil {
		t.Fatalf("ReadAndRestore returned error: %v", err)
	}

	if got := getLogicalPayload(t, target, "system_settings", "key", "global"); got != `{"name":"source-settings"}` {
		t.Fatalf("settings payload = %s", got)
	}
	if got := getLogicalPayload(t, target, "users", "id", "target_user"); got != `{"ID":"target_user","Username":"target"}` {
		t.Fatalf("user payload = %s", got)
	}
	if _, err := os.Stat(target + "-wal"); err == nil {
		t.Fatal("full database restore should not run during selected logical restore")
	}
}

func TestLogicalBillingRestoreDefaultsOldPlanGroupRatio(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, source)
	initLogicalTestDB(t, target)

	repo, err := storage.NewSQLiteRepository(source, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository source returned error: %v", err)
	}
	if err := repo.UpsertBillingPlanGroup(core.BillingPlanGroup{
		ID:              "group_old_backup",
		Name:            "Old Backup",
		QuotaPriceRatio: "1:0.8",
	}); err != nil {
		_ = repo.Close()
		t.Fatalf("UpsertBillingPlanGroup returned error: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("close source repo: %v", err)
	}

	var buf bytes.Buffer
	if _, err := Write(&buf, Options{StatePath: source, DataSets: []string{DataSetBilling}}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	oldBackup := removeLogicalBackupColumn(t, buf.Bytes(), "billing_plan_groups", "quota_price_ratio")
	if err := ReadAndRestore(bytes.NewReader(oldBackup), Options{StatePath: target, DataSets: []string{DataSetBilling}}); err != nil {
		t.Fatalf("ReadAndRestore returned error: %v", err)
	}

	db, err := sql.Open("sqlite", target)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var ratio string
	if err := db.QueryRow(`SELECT quota_price_ratio FROM billing_plan_groups WHERE id = ?`, "group_old_backup").Scan(&ratio); err != nil {
		t.Fatalf("query restored quota_price_ratio: %v", err)
	}
	if ratio != core.DefaultBillingPlanGroupQuotaPriceRatio {
		t.Fatalf("restored quota_price_ratio = %q, want %q", ratio, core.DefaultBillingPlanGroupQuotaPriceRatio)
	}
}

func TestSQLitePhysicalBackupUsesSnapshotWithoutSidecars(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	initPhysicalTestDB(t, source, map[string]string{"before": "1"})
	appendPhysicalValue(t, source, "after", "2")

	var buf bytes.Buffer
	manifest, err := Write(&buf, Options{StatePath: source})
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if !manifest.Includes.Database || manifest.Includes.Data {
		t.Fatalf("manifest = %#v", manifest)
	}

	entries := archiveEntries(t, buf.Bytes())
	if !containsString(entries, "state/source.db") {
		t.Fatalf("archive entries = %#v, want state/source.db", entries)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry, "-wal") || strings.HasSuffix(entry, "-shm") {
			t.Fatalf("archive entry %q should not be included in snapshot backup", entry)
		}
	}

	target := filepath.Join(t.TempDir(), "target.db")
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target}); err != nil {
		t.Fatalf("ReadAndRestore returned error: %v", err)
	}
	if got := getPhysicalValue(t, target, "after"); got != "2" {
		t.Fatalf("restored WAL-era value = %q, want 2", got)
	}
}

func TestWriteRejectsUnknownDataSet(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	initPhysicalTestDB(t, source, map[string]string{"answer": "source"})

	var buf bytes.Buffer
	if _, err := Write(&buf, Options{StatePath: source, DataSets: []string{"bogus"}}); err == nil || !strings.Contains(err.Error(), "unknown backup data type") {
		t.Fatalf("Write err = %v, want unknown data type", err)
	}
}

func TestReadAndRestoreRejectsUnknownDataSet(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	target := filepath.Join(t.TempDir(), "target.db")
	initPhysicalTestDB(t, source, map[string]string{"answer": "source"})
	initPhysicalTestDB(t, target, map[string]string{"answer": "target"})

	var buf bytes.Buffer
	if _, err := Write(&buf, Options{StatePath: source}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target, DataSets: []string{"bogus"}})
	if err == nil || !strings.Contains(err.Error(), "unknown backup data type") {
		t.Fatalf("ReadAndRestore err = %v, want unknown data type", err)
	}
	if got := getPhysicalValue(t, target, "answer"); got != "target" {
		t.Fatalf("target value after rejected restore = %q, want target", got)
	}
}

func TestLogicalDataSetsIncludeRuntimeTables(t *testing.T) {
	if !containsString(tablesForDataSets([]string{DataSetUsers}), "mcp_tokens") {
		t.Fatalf("users tables = %#v, want mcp_tokens", tablesForDataSets([]string{DataSetUsers}))
	}
	for _, table := range []string{"user_oauth_identities", "user_invitation_codes"} {
		if !containsString(tablesForDataSets([]string{DataSetUsers}), table) {
			t.Fatalf("users tables = %#v, want %s", tablesForDataSets([]string{DataSetUsers}), table)
		}
	}
	if !postgresForeignKeyChecksContain("mcp_tokens", "owner_user_id", "users") {
		t.Fatalf("postgres FK checks = %#v, want mcp_tokens.owner_user_id -> users.id", postgresForeignKeyChecks)
	}
	for _, table := range []string{"user_oauth_identities", "user_invitation_codes"} {
		if !postgresForeignKeyChecksContain(table, "user_id", "users") {
			t.Fatalf("postgres FK checks = %#v, want %s.user_id -> users.id", postgresForeignKeyChecks, table)
		}
	}
	if !containsString(tablesForDataSets([]string{DataSetClients}), "openai_response_bindings") {
		t.Fatalf("clients tables = %#v, want openai_response_bindings", tablesForDataSets([]string{DataSetClients}))
	}
	for _, table := range []string{"monitor_targets", "monitor_results"} {
		if !containsString(tablesForDataSets([]string{DataSetMonitors}), table) {
			t.Fatalf("monitors tables = %#v, want %s", tablesForDataSets([]string{DataSetMonitors}), table)
		}
	}
	for _, table := range []string{"documents", "document_redirects"} {
		if !containsString(tablesForDataSets([]string{DataSetDocuments}), table) {
			t.Fatalf("documents tables = %#v, want %s", tablesForDataSets([]string{DataSetDocuments}), table)
		}
	}
}

func TestLogicalUserRestoreRebuildsIdentityIndexes(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, source)
	initLogicalTestDB(t, target)
	user := core.User{
		ID:           "user_identity_restore",
		Username:     "identity-restore",
		PasswordHash: "password-hash",
		OAuthIdentities: []core.UserOAuthIdentity{{
			Provider: "GitHub",
			Subject:  "oauth-subject",
			Email:    "identity@example.com",
			Username: "identity-oauth",
			LinkedAt: time.Unix(1700000000, 0).UTC(),
		}},
	}
	payload, err := json.Marshal(user)
	if err != nil {
		t.Fatalf("Marshal user: %v", err)
	}
	setLogicalPayload(t, source, "users", user.ID, string(payload))

	var buf bytes.Buffer
	if _, err := Write(&buf, Options{StatePath: source, DataSets: []string{DataSetUsers}}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target, DataSets: []string{DataSetUsers}}); err != nil {
		t.Fatalf("ReadAndRestore returned error: %v", err)
	}

	db, err := sql.Open("sqlite", target)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var signature string
	if err := db.QueryRow(`SELECT signature FROM user_invitation_codes WHERE user_id = ?`, user.ID).Scan(&signature); err != nil {
		t.Fatalf("query invitation signature: %v", err)
	}
	if want := core.UserInvitationSignature(user); signature != want {
		t.Fatalf("signature = %q, want %q", signature, want)
	}
	var userID string
	if err := db.QueryRow(`SELECT user_id FROM user_oauth_identities WHERE provider = ? AND subject = ?`, "github", "oauth-subject").Scan(&userID); err != nil {
		t.Fatalf("query oauth identity: %v", err)
	}
	if userID != user.ID {
		t.Fatalf("oauth user_id = %q, want %q", userID, user.ID)
	}
}

func TestLogicalDocumentDataBackupRestoresDocumentsAndRedirects(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, source)
	initLogicalTestDB(t, target)
	setLogicalPayload(t, source, "users", "user_doc", `{"ID":"user_doc","Username":"doc-user"}`)
	setLogicalPayload(t, target, "users", "user_doc", `{"ID":"user_doc","Username":"doc-user"}`)
	setLogicalDocument(t, source, "doc_source", "guides/start", `{"ID":"doc_source","Slug":"guides/start","Title":"Start"}`)
	setLogicalDocument(t, target, "doc_target", "guides/old", `{"ID":"doc_target","Slug":"guides/old","Title":"Old"}`)
	setLogicalDocumentRedirect(t, source, "old/start", "guides/start")

	var buf bytes.Buffer
	manifest, err := Write(&buf, Options{StatePath: source, DataSets: []string{DataSetDocuments}})
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if !manifest.Includes.Data || len(manifest.Includes.DataSets) != 1 || manifest.Includes.DataSets[0] != DataSetDocuments {
		t.Fatalf("manifest = %#v", manifest)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target}); err != nil {
		t.Fatalf("ReadAndRestore returned error: %v", err)
	}

	if got := getLogicalPayload(t, target, "documents", "id", "doc_source"); got != `{"ID":"doc_source","Slug":"guides/start","Title":"Start"}` {
		t.Fatalf("restored document payload = %s", got)
	}
	if got := countRows(t, target, "documents"); got != 1 {
		t.Fatalf("documents count = %d, want 1", got)
	}
	if got := documentRedirectTarget(t, target, "old/start"); got != "guides/start" {
		t.Fatalf("document redirect target = %q, want guides/start", got)
	}
}

func TestInspectEncryptionDetectsOnlySelectedEncryptedData(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	initLogicalTestDB(t, source)
	encrypted := encryptBackupTestValue(t, "source-key", "smtp-secret")
	setLogicalPayload(t, source, "system_settings", "global", `{"Email":{"SMTPPassword":"`+encrypted+`"}}`)
	setLogicalPayload(t, source, "users", "user_plain", `{"ID":"user_plain","Username":"plain"}`)

	var buf bytes.Buffer
	if _, err := Write(&buf, Options{StatePath: source, DataSets: []string{DataSetSettings, DataSetUsers}}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	inspection, err := InspectEncryption(bytes.NewReader(buf.Bytes()), []string{DataSetSettings})
	if err != nil {
		t.Fatalf("InspectEncryption settings returned error: %v", err)
	}
	if !inspection.Encrypted {
		t.Fatal("settings inspection did not detect encrypted data")
	}
	inspection, err = InspectEncryption(bytes.NewReader(buf.Bytes()), []string{DataSetUsers})
	if err != nil {
		t.Fatalf("InspectEncryption users returned error: %v", err)
	}
	if inspection.Encrypted {
		t.Fatal("users-only inspection should ignore encrypted settings data")
	}
}

func TestReadAndRestoreReencryptsEncryptedLogicalData(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, source)
	initLogicalTestDB(t, target)
	encrypted := encryptBackupTestValue(t, "source-key", "smtp-secret")
	setLogicalPayload(t, source, "system_settings", "global", `{"Email":{"SMTPPassword":"`+encrypted+`"}}`)
	setLogicalPayload(t, target, "system_settings", "global", `{"Email":{"SMTPPassword":"target"}}`)

	var buf bytes.Buffer
	if _, err := Write(&buf, Options{StatePath: source, DataSets: []string{DataSetSettings}}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target, DataSets: []string{DataSetSettings}, TargetMasterKey: "source-key"}); err != nil {
		t.Fatalf("ReadAndRestore with current key returned error: %v", err)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target, DataSets: []string{DataSetSettings}, SourceMasterKey: "wrong-key", TargetMasterKey: "target-key"}); err == nil || !strings.Contains(err.Error(), "decrypt backup encrypted data") {
		t.Fatalf("ReadAndRestore with wrong source key err = %v, want decrypt error", err)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target, DataSets: []string{DataSetSettings}, SourceMasterKey: "wrong-key", TargetMasterKey: "source-key"}); err != nil {
		t.Fatalf("ReadAndRestore should ignore stale source key when current key works: %v", err)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target, DataSets: []string{DataSetSettings}, SourceMasterKey: "source-key", TargetMasterKey: "target-key"}); err != nil {
		t.Fatalf("ReadAndRestore returned error: %v", err)
	}

	payload := getLogicalPayload(t, target, "system_settings", "key", "global")
	if strings.Contains(payload, "smtp-secret") {
		t.Fatalf("restored payload contains plaintext secret: %s", payload)
	}
	sample, err := firstEncryptedValueInJSONPayload(payload)
	if err != nil {
		t.Fatalf("inspect restored payload: %v", err)
	}
	if sample == "" {
		t.Fatalf("restored payload is not encrypted: %s", payload)
	}
	targetCodec, err := newBackupCredentialCodec("target-key")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := targetCodec.decryptValue(sample)
	if err != nil {
		t.Fatalf("decrypt restored payload with target key: %v", err)
	}
	if plain != "smtp-secret" {
		t.Fatalf("restored secret = %q, want smtp-secret", plain)
	}
}

func TestReadAndRestoreEncryptedUnselectedDataDoesNotRequireSourceMasterKey(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, source)
	initLogicalTestDB(t, target)
	encrypted := encryptBackupTestValue(t, "source-key", "smtp-secret")
	setLogicalPayload(t, source, "system_settings", "global", `{"Email":{"SMTPPassword":"`+encrypted+`"}}`)
	setLogicalPayload(t, source, "users", "user_plain", `{"ID":"user_plain","Username":"plain"}`)

	var buf bytes.Buffer
	if _, err := Write(&buf, Options{StatePath: source, DataSets: []string{DataSetSettings, DataSetUsers}}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target, DataSets: []string{DataSetUsers}}); err != nil {
		t.Fatalf("ReadAndRestore users-only returned error: %v", err)
	}
	if got := getLogicalPayload(t, target, "users", "id", "user_plain"); got != `{"ID":"user_plain","Username":"plain"}` {
		t.Fatalf("restored user payload = %s", got)
	}
}

func TestLogicalUserRestoreRejectsUnselectedClientOrphans(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, source)
	initLogicalTestDB(t, target)
	setLogicalPayload(t, source, "users", "user_source", `{"ID":"user_source","Username":"source"}`)
	setLogicalPayload(t, target, "users", "user_target", `{"ID":"user_target","Username":"target"}`)
	setLogicalClient(t, target, "client_target", "user_target")

	var buf bytes.Buffer
	if _, err := Write(&buf, Options{StatePath: source, DataSets: []string{DataSetUsers}}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target})
	if err == nil || !strings.Contains(err.Error(), "missing user user_target") {
		t.Fatalf("ReadAndRestore err = %v, want missing owner rejection", err)
	}
	if got := countRows(t, target, "clients"); got != 1 {
		t.Fatalf("clients count = %d, want 1", got)
	}
	if got := getLogicalPayload(t, target, "users", "id", "user_target"); got != `{"ID":"user_target","Username":"target"}` {
		t.Fatalf("target user after rejected restore = %s", got)
	}
}

func TestLogicalUserRestorePreservesExistingBillingWithoutBillingSelection(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, source)
	initLogicalTestDB(t, target)
	setLogicalPayload(t, source, "users", "user_source", `{"ID":"user_source","Username":"source"}`)
	setLogicalPayload(t, target, "users", "user_target", `{"ID":"user_target","Username":"target"}`)
	setBillingRequest(t, target, "billing_target", "client_deleted", "user_target")

	var buf bytes.Buffer
	if _, err := Write(&buf, Options{StatePath: source, DataSets: []string{DataSetUsers}}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target}); err != nil {
		t.Fatalf("ReadAndRestore returned error: %v", err)
	}
	if got := countRows(t, target, "billing_requests"); got != 1 {
		t.Fatalf("billing_requests count = %d, want 1", got)
	}
	if got := getLogicalPayload(t, target, "users", "id", "user_source"); got != `{"ID":"user_source","Username":"source"}` {
		t.Fatalf("restored user payload = %s", got)
	}
}

func TestLogicalClientRestoreRejectsMissingOwners(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, source)
	initLogicalTestDB(t, target)
	setLogicalPayload(t, target, "users", "user_target", `{"ID":"user_target","Username":"target"}`)
	setLogicalClient(t, source, "client_source", "user_source")

	var buf bytes.Buffer
	if _, err := Write(&buf, Options{StatePath: source, DataSets: []string{DataSetClients}}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target})
	if err == nil || !strings.Contains(err.Error(), "missing user user_source") {
		t.Fatalf("ReadAndRestore err = %v, want missing owner rejection", err)
	}
	if got := countRows(t, target, "clients"); got != 0 {
		t.Fatalf("clients count = %d, want rollback to empty", got)
	}
}

func TestLogicalClientRestorePreservesExistingBillingWithoutBillingSelection(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, source)
	initLogicalTestDB(t, target)
	setLogicalPayload(t, source, "users", "user_restore_client", `{"ID":"user_restore_client","Username":"source"}`)
	setLogicalPayload(t, target, "users", "user_restore_client", `{"ID":"user_restore_client","Username":"target"}`)
	setLogicalClient(t, source, "client_source", "user_restore_client")
	setBillingRequest(t, target, "billing_target", "client_deleted", "user_restore_client")

	var buf bytes.Buffer
	if _, err := Write(&buf, Options{StatePath: source, DataSets: []string{DataSetClients}}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target}); err != nil {
		t.Fatalf("ReadAndRestore returned error: %v", err)
	}
	if got := countRows(t, target, "billing_requests"); got != 1 {
		t.Fatalf("billing_requests count = %d, want 1", got)
	}
	if got := getLogicalPayload(t, target, "clients", "id", "client_source"); got != `{"ID":"client_source","OwnerUserID":"user_restore_client"}` {
		t.Fatalf("restored client payload = %s", got)
	}
}

func TestLogicalUserRestoreRemovesStaleSessions(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, source)
	initLogicalTestDB(t, target)
	setLogicalPayload(t, source, "users", "user_source", `{"ID":"user_source","Username":"source"}`)
	setLogicalPayload(t, target, "users", "user_target", `{"ID":"user_target","Username":"target"}`)
	setLogicalSession(t, target, "session_target", "user_target")

	var buf bytes.Buffer
	if _, err := Write(&buf, Options{StatePath: source, DataSets: []string{DataSetUsers}}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target}); err != nil {
		t.Fatalf("ReadAndRestore returned error: %v", err)
	}
	if got := countRows(t, target, "user_sessions"); got != 0 {
		t.Fatalf("user sessions count = %d, want 0", got)
	}
}

func TestBillingDataSetIncludesPaymentRefunds(t *testing.T) {
	tables := tablesForDataSets([]string{DataSetBilling})
	for _, table := range tables {
		if table == "payment_refunds" {
			return
		}
	}
	t.Fatalf("billing tables = %#v, want payment_refunds", tables)
}

func TestLogicalMonitorRestoreReplacesTargetsAndResults(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, source)
	initLogicalTestDB(t, target)
	setMonitorTarget(t, source, "mon_source", "Default", "gpt-5", true, true, `{"ID":"mon_source","Name":"Source","AccountGroup":"Default","Model":"gpt-5","Enabled":true,"PublicVisible":true}`)
	setMonitorResult(t, source, "res_source", "mon_source", "ok", `{"ID":"res_source","TargetID":"mon_source","Status":"ok","LatencyMS":42}`)
	setMonitorTarget(t, target, "mon_target", "Default", "gpt-4.1", true, false, `{"ID":"mon_target","Name":"Target","AccountGroup":"Default","Model":"gpt-4.1","Enabled":true,"PublicVisible":false}`)
	setMonitorResult(t, target, "res_target", "mon_target", "failed", `{"ID":"res_target","TargetID":"mon_target","Status":"failed"}`)

	var buf bytes.Buffer
	manifest, err := Write(&buf, Options{StatePath: source, DataSets: []string{DataSetMonitors}})
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if !manifest.Includes.Data || len(manifest.Includes.DataSets) != 1 || manifest.Includes.DataSets[0] != DataSetMonitors {
		t.Fatalf("manifest data sets = %#v", manifest.Includes)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target}); err != nil {
		t.Fatalf("ReadAndRestore returned error: %v", err)
	}
	if got := countRows(t, target, "monitor_targets"); got != 1 {
		t.Fatalf("monitor_targets count = %d, want 1", got)
	}
	if got := countRows(t, target, "monitor_results"); got != 1 {
		t.Fatalf("monitor_results count = %d, want 1", got)
	}
	if got := getLogicalPayload(t, target, "monitor_targets", "id", "mon_source"); got != `{"ID":"mon_source","Name":"Source","AccountGroup":"Default","Model":"gpt-5","Enabled":true,"PublicVisible":true}` {
		t.Fatalf("restored monitor target payload = %s", got)
	}
	if got := getLogicalPayload(t, target, "monitor_results", "id", "res_source"); got != `{"ID":"res_source","TargetID":"mon_source","Status":"ok","LatencyMS":42}` {
		t.Fatalf("restored monitor result payload = %s", got)
	}
}

func TestLogicalBillingRestoreRecomputesClientSpendUsage(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, source)
	initLogicalTestDB(t, target)
	setLogicalPayload(t, source, "users", "user_restore_spend", `{"ID":"user_restore_spend","Username":"source"}`)
	setLogicalClient(t, source, "client_restore_spend", "user_restore_spend")
	setClientSpend(t, source, "client_restore_spend", 9000, 9999)
	setBillingRequestWithAmounts(t, source, "billing_restore_spend", "client_restore_spend", "user_restore_spend", "settled", 5000, 1234)

	setLogicalPayload(t, target, "users", "user_restore_spend", `{"ID":"user_restore_spend","Username":"target"}`)
	setLogicalClient(t, target, "client_restore_spend", "user_restore_spend")
	setClientSpend(t, target, "client_restore_spend", 9000, 7777)

	var buf bytes.Buffer
	if _, err := Write(&buf, Options{StatePath: source, DataSets: []string{DataSetBilling}}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target}); err != nil {
		t.Fatalf("ReadAndRestore returned error: %v", err)
	}
	limit, used := getClientSpend(t, target, "client_restore_spend")
	if limit != 9000 || used != 1234 {
		t.Fatalf("client_spend limit=%d used=%d, want 9000/1234", limit, used)
	}
}

func TestLogicalBillingRestorePrefersLedgerForClientSpendUsage(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, source)
	initLogicalTestDB(t, target)
	setLogicalPayload(t, source, "users", "user_restore_ledger_spend", `{"ID":"user_restore_ledger_spend","Username":"source"}`)
	setLogicalClient(t, source, "client_restore_ledger_spend", "user_restore_ledger_spend")
	setClientSpend(t, source, "client_restore_ledger_spend", 9000, 9999)
	setBillingRequestWithAmounts(t, source, "billing_restore_ledger_spend", "client_restore_ledger_spend", "user_restore_ledger_spend", "settled", 5000, 1234)
	setBillingLedgerEntry(t, source, "ledger_restore_ledger_settle", "client_restore_ledger_spend", "settle", -4321)

	setLogicalPayload(t, target, "users", "user_restore_ledger_spend", `{"ID":"user_restore_ledger_spend","Username":"target"}`)
	setLogicalClient(t, target, "client_restore_ledger_spend", "user_restore_ledger_spend")
	setClientSpend(t, target, "client_restore_ledger_spend", 9000, 7777)

	var buf bytes.Buffer
	if _, err := Write(&buf, Options{StatePath: source, DataSets: []string{DataSetBilling}}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target}); err != nil {
		t.Fatalf("ReadAndRestore returned error: %v", err)
	}
	limit, used := getClientSpend(t, target, "client_restore_ledger_spend")
	if limit != 9000 || used != 4321 {
		t.Fatalf("client_spend limit=%d used=%d, want 9000/4321", limit, used)
	}
}

func TestLogicalBillingRestoreIgnoresNonUsageLedgerForClientSpendUsage(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, source)
	initLogicalTestDB(t, target)
	setLogicalPayload(t, source, "users", "user_restore_non_usage_ledger", `{"ID":"user_restore_non_usage_ledger","Username":"source"}`)
	setLogicalClient(t, source, "client_restore_non_usage_ledger", "user_restore_non_usage_ledger")
	setClientSpend(t, source, "client_restore_non_usage_ledger", 9000, 9999)
	setBillingRequestWithAmounts(t, source, "billing_restore_non_usage_ledger", "client_restore_non_usage_ledger", "user_restore_non_usage_ledger", "settled", 5000, 1234)
	setBillingLedgerEntry(t, source, "ledger_restore_non_usage_manual", "client_restore_non_usage_ledger", "manual_credit", 500)

	setLogicalPayload(t, target, "users", "user_restore_non_usage_ledger", `{"ID":"user_restore_non_usage_ledger","Username":"target"}`)
	setLogicalClient(t, target, "client_restore_non_usage_ledger", "user_restore_non_usage_ledger")
	setClientSpend(t, target, "client_restore_non_usage_ledger", 9000, 7777)

	var buf bytes.Buffer
	if _, err := Write(&buf, Options{StatePath: source, DataSets: []string{DataSetBilling}}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target}); err != nil {
		t.Fatalf("ReadAndRestore returned error: %v", err)
	}
	limit, used := getClientSpend(t, target, "client_restore_non_usage_ledger")
	if limit != 9000 || used != 1234 {
		t.Fatalf("client_spend limit=%d used=%d, want 9000/1234", limit, used)
	}
}

func TestLogicalBillingRestoreRebuildsFinanceRollups(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, source)
	initLogicalTestDB(t, target)
	setLogicalPayload(t, source, "users", "user_rollup", `{"ID":"user_rollup","Username":"source"}`)
	setLogicalPayload(t, target, "users", "user_rollup", `{"ID":"user_rollup","Username":"target"}`)
	setLogicalClient(t, source, "client_rollup", "user_rollup")
	setLogicalClient(t, target, "client_rollup", "user_rollup")
	setClientSpend(t, source, "client_rollup", 9000, 9999)
	setClientSpend(t, target, "client_rollup", 9000, 7777)
	setBillingRequestWithAmounts(t, source, "billing_rollup", "client_rollup", "user_rollup", "settled", 5000, 1234)
	setFinanceUserRollup(t, target, "user_rollup", 9999)
	setFinanceClientRollup(t, target, "client_rollup", 9999)
	setFinanceModelRollup(t, target, "gpt-test", 9999)

	var buf bytes.Buffer
	if _, err := Write(&buf, Options{StatePath: source, DataSets: []string{DataSetBilling}}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target}); err != nil {
		t.Fatalf("ReadAndRestore returned error: %v", err)
	}

	if got := getFinanceUserRollupSpend(t, target, "user_rollup"); got != 1234 {
		t.Fatalf("finance user spend = %d, want 1234", got)
	}
	if got := getFinanceClientRollupSpend(t, target, "client_rollup"); got != 1234 {
		t.Fatalf("finance client spend = %d, want 1234", got)
	}
	if got := getFinanceModelRollupSpend(t, target, "gpt-test"); got != 1234 {
		t.Fatalf("finance model spend = %d, want 1234", got)
	}
}

func TestLogicalBillingRestoreRebuildsFinanceRollupSpendSources(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, source)
	initLogicalTestDB(t, target)
	setLogicalPayload(t, source, "users", "user_rollup_split", `{"ID":"user_rollup_split","Username":"source"}`)
	setLogicalPayload(t, target, "users", "user_rollup_split", `{"ID":"user_rollup_split","Username":"target"}`)
	setLogicalClient(t, source, "client_rollup_cash", "user_rollup_split")
	setLogicalClient(t, source, "client_rollup_plan", "user_rollup_split")
	setLogicalClient(t, target, "client_rollup_cash", "user_rollup_split")
	setLogicalClient(t, target, "client_rollup_plan", "user_rollup_split")
	setClientSpend(t, source, "client_rollup_cash", 9000, 9999)
	setClientSpend(t, source, "client_rollup_plan", 9000, 9999)
	setBillingRequestWithSource(t, source, "billing_rollup_cash", "client_rollup_cash", "user_rollup_split", "cash", "settled", 5000, 1234)
	setBillingRequestWithSource(t, source, "billing_rollup_plan", "client_rollup_plan", "user_rollup_split", "plan", "settled", 5000, 4321)
	setFinanceUserRollup(t, target, "user_rollup_split", 9999)
	setFinanceClientRollup(t, target, "client_rollup_cash", 9999)
	setFinanceClientRollup(t, target, "client_rollup_plan", 9999)

	var buf bytes.Buffer
	if _, err := Write(&buf, Options{StatePath: source, DataSets: []string{DataSetBilling}}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target}); err != nil {
		t.Fatalf("ReadAndRestore returned error: %v", err)
	}

	if got := getFinanceUserRollupSpend(t, target, "user_rollup_split"); got != 1234 {
		t.Fatalf("finance user cash spend = %d, want 1234", got)
	}
	if got := getFinanceUserRollupColumn(t, target, "user_rollup_split", "usage_spend_nano_usd"); got != 5555 {
		t.Fatalf("finance user usage spend = %d, want 5555", got)
	}
	if got := getFinanceUserRollupColumn(t, target, "user_rollup_split", "plan_spend_nano_usd"); got != 4321 {
		t.Fatalf("finance user plan spend = %d, want 4321", got)
	}
	if got := getFinanceClientRollupSpend(t, target, "client_rollup_cash"); got != 1234 {
		t.Fatalf("cash client spend = %d, want 1234", got)
	}
	if got := getFinanceClientRollupColumn(t, target, "client_rollup_cash", "usage_nano_usd"); got != 1234 {
		t.Fatalf("cash client usage = %d, want 1234", got)
	}
	if got := getFinanceClientRollupColumn(t, target, "client_rollup_cash", "plan_nano_usd"); got != 0 {
		t.Fatalf("cash client plan = %d, want 0", got)
	}
	if got := getFinanceClientRollupSpend(t, target, "client_rollup_plan"); got != 0 {
		t.Fatalf("plan client cash spend = %d, want 0", got)
	}
	if got := getFinanceClientRollupColumn(t, target, "client_rollup_plan", "usage_nano_usd"); got != 4321 {
		t.Fatalf("plan client usage = %d, want 4321", got)
	}
	if got := getFinanceClientRollupColumn(t, target, "client_rollup_plan", "plan_nano_usd"); got != 4321 {
		t.Fatalf("plan client plan = %d, want 4321", got)
	}
}

func TestLogicalClientRestoreWithoutBillingClearsClientSpendUsage(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, source)
	initLogicalTestDB(t, target)
	setLogicalPayload(t, source, "users", "user_client_spend", `{"ID":"user_client_spend","Username":"source"}`)
	setLogicalClient(t, source, "client_client_spend", "user_client_spend")
	setClientSpend(t, source, "client_client_spend", 9000, 7777)

	setLogicalPayload(t, target, "users", "user_client_spend", `{"ID":"user_client_spend","Username":"target"}`)

	var buf bytes.Buffer
	if _, err := Write(&buf, Options{StatePath: source, DataSets: []string{DataSetClients}}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target}); err != nil {
		t.Fatalf("ReadAndRestore returned error: %v", err)
	}
	limit, used := getClientSpend(t, target, "client_client_spend")
	if limit != 9000 || used != 0 {
		t.Fatalf("client_spend limit=%d used=%d, want 9000/0", limit, used)
	}
}

func TestPostgresForeignKeyViolationQueryIgnoresEmptyOptionalClientID(t *testing.T) {
	query := postgresForeignKeyViolationQuery(struct {
		childTable  string
		childColumn string
		parentTable string
		ignoreEmpty bool
	}{
		childTable:  "openai_response_bindings",
		childColumn: "client_id",
		parentTable: "clients",
		ignoreEmpty: true,
	})
	if !strings.Contains(query, "openai_response_bindings.client_id <> ''") {
		t.Fatalf("query = %s, want empty client id filter", query)
	}
}

func TestPostgresLogicalRestoreDoesNotRequireFinancialRowsToReferenceLiveTables(t *testing.T) {
	financialTables := map[string]bool{
		"billing_requests": true,
		"billing_ledger":   true,
		"payment_orders":   true,
		"payment_refunds":  true,
	}
	for _, check := range postgresForeignKeyChecks {
		if financialTables[check.childTable] {
			t.Fatalf("postgres restore check still treats %s as live FK dependency: %#v", check.childTable, check)
		}
	}
}

func TestResolveDatabaseBackend(t *testing.T) {
	backend, err := resolveDatabaseBackend(Options{})
	if err != nil {
		t.Fatalf("resolve default backend: %v", err)
	}
	if backend != databaseBackendSQLite {
		t.Fatalf("default backend = %q, want sqlite", backend)
	}

	backend, err = resolveDatabaseBackend(Options{PostgresDSN: "postgres://user:pass@localhost/db"})
	if err != nil {
		t.Fatalf("resolve DSN-only backend: %v", err)
	}
	if backend != databaseBackendSQLite {
		t.Fatalf("DSN-only backend = %q, want sqlite", backend)
	}

	backend, err = resolveDatabaseBackend(Options{DatabaseBackend: databaseBackendPostgres, PostgresDSN: "postgres://user:pass@localhost/db"})
	if err != nil {
		t.Fatalf("resolve explicit postgres backend: %v", err)
	}
	if backend != databaseBackendPostgres {
		t.Fatalf("explicit postgres backend = %q, want postgres", backend)
	}

	if _, err := resolveDatabaseBackend(Options{DatabaseBackend: databaseBackendPostgres}); err == nil {
		t.Fatal("expected postgres backend without DSN to fail")
	}
	if _, err := resolveDatabaseBackend(Options{DatabaseBackend: "mysql"}); err == nil {
		t.Fatal("expected invalid backend to fail")
	}
}

func TestRebindBackupQuery(t *testing.T) {
	query := `INSERT INTO table_name(a, b, c) VALUES(?, '?', "?") WHERE id = ?`
	got := rebindBackupQuery(databaseBackendPostgres, query)
	want := `INSERT INTO table_name(a, b, c) VALUES($1, '?', "?") WHERE id = $2`
	if got != want {
		t.Fatalf("rebuilt query = %q, want %q", got, want)
	}
	if got := rebindBackupQuery(databaseBackendSQLite, query); got != query {
		t.Fatalf("sqlite query was rewritten: %q", got)
	}
}

func TestDecodeLogicalCellPreservesInt64(t *testing.T) {
	const timestamp = int64(1712345678901234567)
	value, err := decodeLogicalCell(json.RawMessage(`1712345678901234567`))
	if err != nil {
		t.Fatalf("decodeLogicalCell returned error: %v", err)
	}
	if value != timestamp {
		t.Fatalf("decoded value = %#v (%T), want %d", value, value, timestamp)
	}
}

func TestNormalizeLogicalCellConvertsBooleanColumns(t *testing.T) {
	postgresValue, err := normalizeLogicalCell(databaseBackendPostgres, "billing_requests", "fast_mode", int64(1))
	if err != nil {
		t.Fatalf("normalize postgres boolean returned error: %v", err)
	}
	if postgresValue != true {
		t.Fatalf("postgres boolean value = %#v, want true", postgresValue)
	}

	sqliteValue, err := normalizeLogicalCell(databaseBackendSQLite, "site_messages", "enabled", false)
	if err != nil {
		t.Fatalf("normalize sqlite boolean returned error: %v", err)
	}
	if sqliteValue != int64(0) {
		t.Fatalf("sqlite boolean value = %#v, want int64(0)", sqliteValue)
	}

	mcpPostgresValue, err := normalizeLogicalCell(databaseBackendPostgres, "mcp_tokens", "enabled", int64(1))
	if err != nil {
		t.Fatalf("normalize mcp postgres boolean returned error: %v", err)
	}
	if mcpPostgresValue != true {
		t.Fatalf("mcp postgres boolean value = %#v, want true", mcpPostgresValue)
	}

	mcpSQLiteValue, err := normalizeLogicalCell(databaseBackendSQLite, "mcp_tokens", "enabled", false)
	if err != nil {
		t.Fatalf("normalize mcp sqlite boolean returned error: %v", err)
	}
	if mcpSQLiteValue != int64(0) {
		t.Fatalf("mcp sqlite boolean value = %#v, want int64(0)", mcpSQLiteValue)
	}
}

func TestLogicalRestoreRejectsUnknownColumns(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target.db")
	initLogicalTestDB(t, target)
	payload := logicalDataBackup{
		DataSets: []string{DataSetSettings},
		Tables: []logicalTableBackup{{
			Name:    "system_settings",
			Columns: []string{"key", "payload", "updated_at_ns", "unsafe_column"},
			Rows: [][]json.RawMessage{{
				json.RawMessage(`"global"`),
				json.RawMessage(`"{}"`),
				json.RawMessage(`1`),
				json.RawMessage(`"x"`),
			}},
		}},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	manifest := Manifest{
		Format:    Format,
		Version:   Version,
		CreatedAt: time.Now().UTC(),
		Includes:  Includes{Data: true, DataSets: []string{DataSetSettings}},
	}
	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBytes(tw, "manifest.json", manifestRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeBytes(tw, "data/logical.json", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ReadAndRestore(bytes.NewReader(buf.Bytes()), Options{StatePath: target}); err == nil {
		t.Fatal("expected unknown column to be rejected")
	}
}

func TestPhysicalBackupRemovesLegacyTraceTable(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.db")
	initLogicalTestDB(t, statePath)
	db, err := sql.Open("sqlite", statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE gateway_traces (
		id TEXT PRIMARY KEY,
		request_id TEXT NOT NULL DEFAULT '',
		payload TEXT NOT NULL
	)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO gateway_traces(id, request_id, payload) VALUES(?, ?, ?)`, "legacy", "request", `{"secret":"value"}`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if _, err := Write(&buf, Options{StatePath: statePath}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	snapshotPath := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(snapshotPath, readBackupEntry(t, buf.Bytes(), "state/state.db"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tableCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'gateway_traces'`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if tableCount != 0 {
		t.Fatalf("legacy trace table count = %d, want 0", tableCount)
	}
}

func initLogicalTestDB(t *testing.T, path string) {
	t.Helper()
	repo, err := storage.NewSQLiteRepository(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
}

func setLogicalClient(t *testing.T, path, id, userID string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	payload := `{"ID":"` + id + `","OwnerUserID":"` + userID + `"}`
	if _, err := db.Exec(`INSERT OR REPLACE INTO clients(id, payload, api_key_hash) VALUES(?, ?, ?)`, id, payload, id); err != nil {
		t.Fatal(err)
	}
}

func setLogicalDocument(t *testing.T, path, id, slug, payload string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT OR REPLACE INTO documents(id, slug_key, status, visibility, payload, updated_at_ns) VALUES(?, ?, ?, ?, ?, ?)`, id, slug, "published", "public", payload, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
}

func setLogicalDocumentRedirect(t *testing.T, path, fromSlug, toSlug string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT OR REPLACE INTO document_redirects(from_slug_key, from_slug, to_slug, status_code, created_at_ns) VALUES(?, ?, ?, ?, ?)`, fromSlug, fromSlug, toSlug, 301, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
}

func setBillingRequest(t *testing.T, path, id, clientID, userID string) {
	t.Helper()
	setBillingRequestWithAmounts(t, path, id, clientID, userID, "settled", 0, 0)
}

func setBillingRequestWithAmounts(t *testing.T, path, id, clientID, userID, status string, reservedNanoUSD, actualNanoUSD int64) {
	t.Helper()
	setBillingRequestWithSource(t, path, id, clientID, userID, "cash", status, reservedNanoUSD, actualNanoUSD)
}

func setBillingRequestWithSource(t *testing.T, path, id, clientID, userID, billingSource, status string, reservedNanoUSD, actualNanoUSD int64) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT OR REPLACE INTO billing_requests(id, request_id, client_id, user_id, billing_source, provider, model, status, reserved_nano_usd, actual_nano_usd, created_at_ns) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, id, clientID, userID, billingSource, "openai", "gpt-test", status, reservedNanoUSD, actualNanoUSD, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
}

func setMonitorTarget(t *testing.T, path, id, group, model string, enabled, publicVisible bool, payload string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	enabledInt := 0
	if enabled {
		enabledInt = 1
	}
	publicVisibleInt := 0
	if publicVisible {
		publicVisibleInt = 1
	}
	if _, err := db.Exec(`INSERT OR REPLACE INTO monitor_targets(id, account_group, model, enabled, public_visible, interval_seconds, updated_at_ns, payload) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, id, group, model, enabledInt, publicVisibleInt, 300, time.Now().UnixNano(), payload); err != nil {
		t.Fatal(err)
	}
}

func setMonitorResult(t *testing.T, path, id, targetID, status, payload string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT OR REPLACE INTO monitor_results(id, target_id, status, latency_ms, checked_at_ns, payload) VALUES(?, ?, ?, ?, ?, ?)`, id, targetID, status, 42, time.Now().UnixNano(), payload); err != nil {
		t.Fatal(err)
	}
}

func setBillingLedgerEntry(t *testing.T, path, id, clientID, kind string, amountNanoUSD int64) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT OR REPLACE INTO billing_ledger(id, user_id, client_id, request_id, kind, amount_nano_usd, balance_after_nano_usd, created_at_ns) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, id, "user_ledger", clientID, id, kind, amountNanoUSD, 0, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
}

func setClientSpend(t *testing.T, path, clientID string, limitNanoUSD, usedNanoUSD int64) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT OR REPLACE INTO client_spend(client_id, spend_limit_nano_usd, spend_used_nano_usd, updated_at_ns) VALUES(?, ?, ?, ?)`, clientID, limitNanoUSD, usedNanoUSD, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
}

func getClientSpend(t *testing.T, path, clientID string) (int64, int64) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var limitNanoUSD, usedNanoUSD int64
	if err := db.QueryRow(`SELECT spend_limit_nano_usd, spend_used_nano_usd FROM client_spend WHERE client_id = ?`, clientID).Scan(&limitNanoUSD, &usedNanoUSD); err != nil {
		t.Fatal(err)
	}
	return limitNanoUSD, usedNanoUSD
}

func setFinanceUserRollup(t *testing.T, path, userID string, spendNanoUSD int64) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT OR REPLACE INTO finance_user_rollups(user_id, username, spend_nano_usd) VALUES(?, ?, ?)`, userID, userID, spendNanoUSD); err != nil {
		t.Fatal(err)
	}
}

func setFinanceClientRollup(t *testing.T, path, clientID string, spendNanoUSD int64) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT OR REPLACE INTO finance_client_rollups(client_id, client_name, spend_used_nano_usd) VALUES(?, ?, ?)`, clientID, clientID, spendNanoUSD); err != nil {
		t.Fatal(err)
	}
}

func setFinanceModelRollup(t *testing.T, path, model string, spendNanoUSD int64) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT OR REPLACE INTO finance_model_rollups(model, request_count, spend_nano_usd) VALUES(?, ?, ?)`, model, 1, spendNanoUSD); err != nil {
		t.Fatal(err)
	}
}

func getFinanceUserRollupSpend(t *testing.T, path, userID string) int64 {
	t.Helper()
	return getFinanceRollupSpend(t, path, "finance_user_rollups", "user_id", userID, "spend_nano_usd")
}

func getFinanceUserRollupColumn(t *testing.T, path, userID, column string) int64 {
	t.Helper()
	return getFinanceRollupSpend(t, path, "finance_user_rollups", "user_id", userID, column)
}

func getFinanceClientRollupSpend(t *testing.T, path, clientID string) int64 {
	t.Helper()
	return getFinanceRollupSpend(t, path, "finance_client_rollups", "client_id", clientID, "spend_used_nano_usd")
}

func getFinanceClientRollupColumn(t *testing.T, path, clientID, column string) int64 {
	t.Helper()
	return getFinanceRollupSpend(t, path, "finance_client_rollups", "client_id", clientID, column)
}

func getFinanceModelRollupSpend(t *testing.T, path, model string) int64 {
	t.Helper()
	return getFinanceRollupSpend(t, path, "finance_model_rollups", "model", model, "spend_nano_usd")
}

func getFinanceRollupSpend(t *testing.T, path, table, keyColumn, keyValue, spendColumn string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var spend int64
	if err := db.QueryRow(fmt.Sprintf(`SELECT %s FROM %s WHERE %s = ?`, spendColumn, table, keyColumn), keyValue).Scan(&spend); err != nil {
		t.Fatal(err)
	}
	return spend
}

func setLogicalSession(t *testing.T, path, tokenHash, userID string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT OR REPLACE INTO user_sessions(token_hash, user_id, expires_at_ns, payload) VALUES(?, ?, ?, ?)`, tokenHash, userID, time.Now().Add(time.Hour).UnixNano(), `{}`); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, path, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func documentRedirectTarget(t *testing.T, path, fromSlug string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var target string
	if err := db.QueryRow(`SELECT to_slug FROM document_redirects WHERE from_slug_key = ?`, fromSlug).Scan(&target); err != nil {
		t.Fatal(err)
	}
	return target
}

func initPhysicalTestDB(t *testing.T, path string, values map[string]string) {
	t.Helper()
	repo, err := storage.NewSQLiteRepository(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for key, value := range values {
		if _, err := db.Exec(`INSERT OR REPLACE INTO kv(key, value) VALUES(?, ?)`, key, value); err != nil {
			t.Fatal(err)
		}
	}
}

func appendPhysicalValue(t *testing.T, path, key, value string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT OR REPLACE INTO kv(key, value) VALUES(?, ?)`, key, value); err != nil {
		t.Fatal(err)
	}
}

func getPhysicalValue(t *testing.T, path, key string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRow(`SELECT value FROM kv WHERE key = ?`, key).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func archiveEntries(t *testing.T, raw []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var entries []string
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, header.Name)
	}
}

func readBackupEntry(t *testing.T, raw []byte, name string) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			t.Fatalf("%s not found", name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name != name {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
}

func readLogicalBackup(t *testing.T, raw []byte) logicalDataBackup {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			t.Fatal("data/logical.json not found")
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name != "data/logical.json" {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		var payload logicalDataBackup
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func postgresForeignKeyChecksContain(childTable, childColumn, parentTable string) bool {
	for _, check := range postgresForeignKeyChecks {
		if check.childTable == childTable && check.childColumn == childColumn && check.parentTable == parentTable {
			return true
		}
	}
	return false
}

func encryptBackupTestValue(t *testing.T, masterKey, value string) string {
	t.Helper()
	codec, err := newBackupCredentialCodec(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := codec.encryptValue(value)
	if err != nil {
		t.Fatal(err)
	}
	return encrypted
}

func setLogicalPayload(t *testing.T, path, table, id, payload string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	switch table {
	case "system_settings":
		if _, err := db.Exec(`INSERT OR REPLACE INTO system_settings(key, payload, updated_at_ns) VALUES(?, ?, 1)`, id, payload); err != nil {
			t.Fatal(err)
		}
	case "users":
		var parsed struct {
			Username string
		}
		if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT OR REPLACE INTO users(id, username_key, payload) VALUES(?, ?, ?)`, id, parsed.Username, payload); err != nil {
			t.Fatal(err)
		}
	}
}

func getLogicalPayload(t *testing.T, path, table, keyColumn, key string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var payload string
	if err := db.QueryRow("SELECT payload FROM "+table+" WHERE "+keyColumn+" = ?", key).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func addUnknownLogicalDataSetToBackup(t *testing.T, rawBackup []byte, dataSet string) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(rawBackup))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()

	var out bytes.Buffer
	outGz := gzip.NewWriter(&out)
	tw := tar.NewWriter(outGz)
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "data/logical.json" {
			var payload logicalDataBackup
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			payload.DataSets = append(payload.DataSets, dataSet)
			body, err = json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			header.Size = int64(len(body))
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := outGz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func removeLogicalBackupColumn(t *testing.T, rawBackup []byte, tableName, columnName string) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(rawBackup))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()

	var out bytes.Buffer
	outGz := gzip.NewWriter(&out)
	tw := tar.NewWriter(outGz)
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "data/logical.json" {
			var payload logicalDataBackup
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			for tableIndex := range payload.Tables {
				table := &payload.Tables[tableIndex]
				if table.Name != tableName {
					continue
				}
				columnIndex := -1
				for index, column := range table.Columns {
					if column == columnName {
						columnIndex = index
						break
					}
				}
				if columnIndex < 0 {
					t.Fatalf("column %s.%s not found in logical backup", tableName, columnName)
				}
				table.Columns = append(table.Columns[:columnIndex], table.Columns[columnIndex+1:]...)
				for rowIndex := range table.Rows {
					row := table.Rows[rowIndex]
					if columnIndex >= len(row) {
						t.Fatalf("row %d in table %s has %d columns, missing index %d", rowIndex, tableName, len(row), columnIndex)
					}
					table.Rows[rowIndex] = append(row[:columnIndex], row[columnIndex+1:]...)
				}
			}
			body, err = json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			header.Size = int64(len(body))
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := outGz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func mustWriteFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != want {
		t.Fatalf("%s = %q, want %q", path, string(raw), want)
	}
}
