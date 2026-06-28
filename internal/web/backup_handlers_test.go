package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestBackupDataSetsFromFormRequiresExplicitKnownSelection(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/admin/backup/export", nil)
	if got := backupDataSetsFromForm(req); len(got) != 0 {
		t.Fatalf("backup data sets without form values = %#v, want none", got)
	}

	form := url.Values{}
	form.Add("backup_data", "users")
	form.Add("backup_data", "unknown")
	req = httptest.NewRequest(http.MethodPost, "/admin/backup/export", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := req.ParseForm(); err != nil {
		t.Fatal(err)
	}
	got := backupDataSetsFromForm(req)
	if len(got) != 1 || got[0] != "users" {
		t.Fatalf("backup data sets = %#v, want users only", got)
	}
}

func TestRestoreDataSetsFromFormRequiresExplicitKnownSelection(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/admin/backup/restore", nil)
	if got := restoreDataSetsFromForm(req); len(got) != 0 {
		t.Fatalf("restore data sets without form values = %#v, want none", got)
	}

	form := url.Values{}
	form.Add("restore_data", "settings")
	form.Add("restore_data", "billing")
	req = httptest.NewRequest(http.MethodPost, "/admin/backup/restore", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := req.ParseForm(); err != nil {
		t.Fatal(err)
	}
	got := restoreDataSetsFromForm(req)
	if len(got) != 2 || got[0] != "settings" || got[1] != "billing" {
		t.Fatalf("restore data sets = %#v, want settings and billing", got)
	}
}

func TestBackupDataSetOptionsIncludeMonitorsInStableOrder(t *testing.T) {
	form := url.Values{}
	form.Add("backup_data", "monitors")
	form.Add("backup_data", "clients")
	form.Add("backup_data", "billing")
	req := httptest.NewRequest(http.MethodPost, "/admin/backup/export", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := req.ParseForm(); err != nil {
		t.Fatal(err)
	}
	got := backupDataSetsFromForm(req)
	if len(got) != 3 || got[0] != "clients" || got[1] != "monitors" || got[2] != "billing" {
		t.Fatalf("backup data sets = %#v, want clients, monitors, billing", got)
	}
}
