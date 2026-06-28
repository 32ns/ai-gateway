package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
)

const accountPoolImportBodyLimit = 32 << 20

func (s *Server) handleAccountPoolExport(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/accounts/export" {
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

	scope := strings.TrimSpace(r.FormValue("scope"))
	allGroups := scope == "all"
	selectedIDs := r.Form["account_id"]
	exported := s.control.ExportAccountPool(controlplane.AccountPoolExportInput{
		AccountIDs: selectedIDs,
		AllGroups:  allGroups,
	})
	if !allGroups && len(exported.Accounts) == 0 {
		http.Error(w, "select at least one account", http.StatusBadRequest)
		return
	}

	message := fmt.Sprintf("scope=%s accounts=%d groups=%d", exportScopeText(allGroups), len(exported.Accounts), len(exported.AccountGroups))
	s.recordAdminAudit(r, "ok", "account.export", "account", "", "", message)

	filename := "ai-gateway-accounts-" + time.Now().UTC().Format("20060102-150405") + ".json"
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(exported); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleAccountPoolImport(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/accounts/import" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, accountPoolImportBodyLimit)
	if err := r.ParseMultipartForm(consoleMultipartFormMemoryLimit); err != nil {
		s.recordAdminAudit(r, "error", "account.import", "account", "", "", err.Error())
		s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
		return
	}

	file, header, err := r.FormFile("account_file")
	if err != nil {
		s.recordAdminAudit(r, "error", "account.import", "account", "", "", err.Error())
		s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
		return
	}
	defer file.Close()

	payload, err := readUploadPart(file, accountPoolImportBodyLimit)
	if err != nil {
		s.recordAdminAudit(r, "error", "account.import", "account", "", strings.TrimSpace(header.Filename), err.Error())
		s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
		return
	}

	options := controlplane.AccountPoolImportOptions{
		ReplaceExisting: parseBoolFormValue(r.FormValue("replace_existing")),
	}
	if strings.TrimSpace(r.FormValue("group_mode")) == "current" {
		group := strings.TrimSpace(r.FormValue("current_group"))
		options.GroupOverride = &group
	}

	result, err := s.control.ImportAccountPoolPayload(payload, options)
	if err != nil {
		s.recordAdminAudit(r, "error", "account.import", "account", "", strings.TrimSpace(header.Filename), err.Error())
		s.redirectWithNoticeError(w, r, accountsGroupFilterHref(accountGroupReturnKey(r), accountFilterReturnValue(r)), err)
		return
	}

	locale := resolveLocale(w, r)
	message := translatef(locale, "account_import_result", result.Imported, result.Failed, result.Skipped)
	tone := "good"
	auditStatus := "ok"
	if result.Failed > 0 {
		tone = "bad"
		auditStatus = "error"
	}
	auditMessage := fmt.Sprintf("file=%q total=%d imported=%d failed=%d skipped=%d groups=%d", strings.TrimSpace(header.Filename), result.Total, result.Imported, result.Failed, result.Skipped, result.GroupsImported)
	s.recordAdminAudit(r, auditStatus, "account.import", "account", "", strings.TrimSpace(header.Filename), auditMessage)
	if result.Imported > 0 || result.GroupsImported > 0 {
		s.publishAccountPoolChanged()
	}
	http.Redirect(w, r, accountBatchRedirect(accountGroupReturnKey(r), accountFilterReturnValue(r), tone, message), http.StatusSeeOther)
}

func exportScopeText(allGroups bool) string {
	if allGroups {
		return "all"
	}
	return "selected"
}
