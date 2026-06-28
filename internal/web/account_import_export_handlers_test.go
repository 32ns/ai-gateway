package web

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestAccountPoolExportHandlerDownloadsJSON(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_export_web",
		Provider: core.ProviderOpenAI,
		Label:    "Export Web",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "secret",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_other_web",
		Provider: core.ProviderOpenAI,
		Label:    "Other Web",
		Status:   core.AccountStatusActive,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "other",
		},
	}); err != nil {
		t.Fatal(err)
	}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	form := url.Values{}
	form.Set("csrf_token", testConsoleCSRFToken)
	form.Set("scope", "selected")
	form.Add("account_id", "acct_export_web")
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/export", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "ai-gateway-accounts-") || !strings.Contains(got, ".json") {
		t.Fatalf("Content-Disposition = %q", got)
	}
	var exported controlplane.AccountPoolExport
	if err := json.Unmarshal(rec.Body.Bytes(), &exported); err != nil {
		t.Fatalf("Unmarshal export: %v", err)
	}
	if len(exported.Accounts) != 1 || exported.Accounts[0].ID != "acct_export_web" || exported.Accounts[0].Credential.AccessToken != "secret" {
		t.Fatalf("exported = %#v", exported.Accounts)
	}
	if len(exported.AccountGroups) != 1 || exported.AccountGroups[0].Name != core.DefaultAccountGroupName {
		t.Fatalf("exported groups = %#v", exported.AccountGroups)
	}
}

func TestAccountPoolImportHandlerUploadsJSON(t *testing.T) {
	repo := storage.NewMemoryRepository()
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedAdminHandler(t, control, server.Handler())

	payload, err := json.Marshal(controlplane.AccountPoolExport{
		Version: controlplane.AccountPoolExportVersion,
		AccountGroups: []core.AccountGroup{{
			ID:                   "group_upload",
			Name:                 "Upload",
			BillingMultiplierBps: 12000,
		}},
		Accounts: []core.Account{{
			ID:       "acct_import_web",
			Provider: core.ProviderOpenAI,
			Label:    "Import Web",
			Group:    "Upload",
			Status:   core.AccountStatusActive,
			Credential: core.Credential{
				Mode:        "manual-token",
				AccessToken: "uploaded-secret",
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("csrf_token", testConsoleCSRFToken); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("current_group", "Upload"); err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFormFile("account_file", "accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/import", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	account, err := repo.GetAccount("acct_import_web")
	if err != nil {
		t.Fatalf("GetAccount returned error: %v", err)
	}
	if account.Credential.AccessToken != "uploaded-secret" || account.Group != "Upload" {
		t.Fatalf("account = %#v", account)
	}
	if !strings.Contains(rec.Header().Get("Location"), "batch_message=") {
		t.Fatalf("redirect location = %q", rec.Header().Get("Location"))
	}
}
