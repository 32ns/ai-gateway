package web

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/backup"
)

const backupRestoreBodyLimit = 512 << 20

func (s *Server) handleBackupPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/backup" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	target := "/admin/settings#settings-backup"
	if r.URL.RawQuery != "" {
		target = "/admin/settings?" + r.URL.RawQuery + "#settings-backup"
	}
	redirectLocalSeeOther(w, r, target)
}

func (s *Server) handleBackupExport(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/backup/export" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	filename := "ai-gateway-" + time.Now().UTC().Format("20060102-150405") + ".agbak"
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	opts := s.backupOptions()
	opts.DataSets = backupDataSetsFromForm(r)
	if len(opts.DataSets) == 0 {
		http.Error(w, "select at least one backup data type", http.StatusBadRequest)
		return
	}
	if _, err := backup.Write(w, opts); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (s *Server) handleBackupInspect(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/backup/inspect" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, backupRestoreBodyLimit)
	if err := r.ParseMultipartForm(consoleMultipartFormMemoryLimit); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": err.Error()})
		return
	}
	dataSets := restoreDataSetsFromForm(r)
	if len(dataSets) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": "select at least one restore data type"})
		return
	}
	file, _, err := r.FormFile("backup")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": err.Error()})
		return
	}
	defer file.Close()
	inspection, err := backup.InspectEncryption(file, dataSets)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "message": err.Error()})
		return
	}
	requiresSourceMasterKey := inspection.RequiresSourceMasterKey(s.masterKey)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":                     "ok",
		"encrypted":                  inspection.Encrypted,
		"requires_source_master_key": requiresSourceMasterKey,
	})
}

func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/backup/restore" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, backupRestoreBodyLimit)
	if err := r.ParseMultipartForm(consoleMultipartFormMemoryLimit); err != nil {
		s.redirectWithNoticeError(w, r, settingsBackupURL(), err)
		return
	}
	if strings.TrimSpace(r.FormValue("confirm")) != "RESTORE" {
		s.redirectWithNoticeError(w, r, settingsBackupURL(), fmt.Errorf("type RESTORE to confirm"))
		return
	}
	file, _, err := r.FormFile("backup")
	if err != nil {
		s.redirectWithNoticeError(w, r, settingsBackupURL(), err)
		return
	}
	defer file.Close()
	tempDir, err := os.MkdirTemp("", "ai-gateway-upload-*")
	if err != nil {
		s.redirectWithNoticeError(w, r, settingsBackupURL(), err)
		return
	}
	defer os.RemoveAll(tempDir)
	tempPath := filepath.Join(tempDir, "upload.agbak")
	out, err := os.OpenFile(tempPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		s.redirectWithNoticeError(w, r, settingsBackupURL(), err)
		return
	}
	if _, err := out.ReadFrom(file); err != nil {
		_ = out.Close()
		s.redirectWithNoticeError(w, r, settingsBackupURL(), err)
		return
	}
	if err := out.Close(); err != nil {
		s.redirectWithNoticeError(w, r, settingsBackupURL(), err)
		return
	}
	preRestoreDir := filepath.Join(filepath.Dir(s.statePath), "backups")
	restoreBackup := backup.Restore
	if s.restoreBackup != nil {
		restoreBackup = s.restoreBackup
	}
	opts := s.backupOptions()
	opts.DataSets = restoreDataSetsFromForm(r)
	opts.SourceMasterKey = r.FormValue("backup_master_key")
	if len(opts.DataSets) == 0 {
		s.redirectWithNoticeError(w, r, settingsBackupURL(), fmt.Errorf("select at least one restore data type"))
		return
	}
	preRestorePath, err := restoreBackup(tempPath, opts, preRestoreDir)
	if err != nil {
		s.redirectWithNoticeError(w, r, settingsBackupURL(), err)
		return
	}
	http.Redirect(w, r, "/admin/settings?restored=1&backup="+url.QueryEscape(preRestorePath)+"#settings-backup", http.StatusSeeOther)
}

func settingsBackupURL() string {
	return "/admin/settings#settings-backup"
}

func (s *Server) backupOptions() backup.Options {
	return backup.Options{
		ConfigPath:      s.configPath,
		StatePath:       s.statePath,
		DatabaseBackend: s.databaseBackend,
		PostgresDSN:     s.postgresDSN,
		TargetMasterKey: s.masterKey,
	}
}

type backupDataSetOption struct {
	Value    string
	LabelKey string
	HintKey  string
}

func backupDataSetOptions() []backupDataSetOption {
	return []backupDataSetOption{
		{Value: backup.DataSetSettings, LabelKey: "backup_data_settings", HintKey: "backup_data_settings_hint"},
		{Value: backup.DataSetUsers, LabelKey: "backup_data_users", HintKey: "backup_data_users_hint"},
		{Value: backup.DataSetAccounts, LabelKey: "backup_data_accounts", HintKey: "backup_data_accounts_hint"},
		{Value: backup.DataSetModels, LabelKey: "backup_data_models", HintKey: "backup_data_models_hint"},
		{Value: backup.DataSetClients, LabelKey: "backup_data_clients", HintKey: "backup_data_clients_hint"},
		{Value: backup.DataSetMonitors, LabelKey: "backup_data_monitors", HintKey: "backup_data_monitors_hint"},
		{Value: backup.DataSetBilling, LabelKey: "backup_data_billing", HintKey: "backup_data_billing_hint"},
		{Value: backup.DataSetMessages, LabelKey: "backup_data_messages", HintKey: "backup_data_messages_hint"},
		{Value: backup.DataSetDocuments, LabelKey: "backup_data_documents", HintKey: "backup_data_documents_hint"},
		{Value: backup.DataSetAudit, LabelKey: "backup_data_audit", HintKey: "backup_data_audit_hint"},
	}
}

func backupDataSetsFromForm(r *http.Request) []string {
	return selectedBackupDataSets(r.Form["backup_data"])
}

func restoreDataSetsFromForm(r *http.Request) []string {
	return selectedBackupDataSets(r.Form["restore_data"])
}

func selectedBackupDataSets(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	selected := make(map[string]struct{}, len(values))
	for _, value := range values {
		selected[strings.ToLower(strings.TrimSpace(value))] = struct{}{}
	}
	out := make([]string, 0, len(selected))
	for _, option := range backupDataSetOptions() {
		if _, ok := selected[option.Value]; ok {
			out = append(out, option.Value)
		}
	}
	return out
}
