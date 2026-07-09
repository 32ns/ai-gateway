package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/accounts"
	"github.com/32ns/ai-gateway/internal/backup"
	"github.com/32ns/ai-gateway/internal/config"
	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/failover"
	"github.com/32ns/ai-gateway/internal/gateway"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/routing"
	"github.com/32ns/ai-gateway/internal/storage"
	personalpay "personalpay/sdk-go"
)

func TestBuildCreatesReusableService(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.Host = "127.0.0.1"
	cfg.Port = "18088"
	cfg.APIKey = "test-client-key"
	cfg.StatePath = filepath.Join(dir, "state.db")
	cfg.AuditLimit = 42
	cfg.MaxInFlight = 3

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	svc, err := Build(Options{ConfigPath: configPath})
	if err != nil {
		t.Fatalf("build app: %v", err)
	}
	defer func() {
		if err := svc.Close(); err != nil {
			t.Fatalf("close app: %v", err)
		}
	}()

	if svc.HTTPServer == nil {
		t.Fatal("expected HTTP server")
	}
	if svc.HTTPServer.Addr != "127.0.0.1:18088" {
		t.Fatalf("HTTP server addr = %q", svc.HTTPServer.Addr)
	}
	if svc.Control == nil || svc.Gateway == nil || svc.Web == nil {
		t.Fatal("expected app dependencies to be wired")
	}
	if svc.Startup.AdminAccount.Username == "" {
		t.Fatal("expected admin account in startup info")
	}
	if !svc.Startup.AdminSeeded {
		t.Fatal("expected initial admin user to be seeded")
	}
	if svc.Startup.AdminAccount.Username != "root" {
		t.Fatalf("initial admin username = %q, want root", svc.Startup.AdminAccount.Username)
	}
	if !svc.Startup.AdminAccount.ForcePasswordChange {
		t.Fatal("initial admin should be forced to change the default password")
	}
	if _, err := svc.Control.AuthenticateUser("root", "toor"); err != nil {
		t.Fatalf("fresh app did not accept default root/toor credentials: %v", err)
	}
	if !svc.Startup.ProtocolClientSeeded {
		t.Fatal("expected protocol client to be seeded")
	}
	if svc.Startup.ProtocolClient.APIKey != cfg.APIKey {
		t.Fatalf("seeded protocol key = %q", svc.Startup.ProtocolClient.APIKey)
	}
	if svc.Startup.AuditLimit != cfg.AuditLimit {
		t.Fatalf("audit limit = %d", svc.Startup.AuditLimit)
	}
}

type cancelMonitorAdapter struct{}

func (a *cancelMonitorAdapter) Kind() core.ProviderKind { return core.ProviderOpenAI }

func (a *cancelMonitorAdapter) DisplayName() string { return "Cancel Monitor" }

func (a *cancelMonitorAdapter) ListModels(context.Context) []core.ModelSpec {
	return []core.ModelSpec{{Name: "gpt-5", Provider: core.ProviderOpenAI}}
}

func (a *cancelMonitorAdapter) Invoke(ctx context.Context, decision core.RouteDecision, _ *core.GatewayRequest) (*core.GatewayResponse, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return &core.GatewayResponse{
			ID:           "resp_monitor_cancel",
			Model:        decision.Model,
			Provider:     decision.Provider,
			AccountID:    decision.Account.ID,
			AccountLabel: decision.Account.Label,
			Content:      "pong",
			FinishReason: "stop",
			CreatedAt:    time.Now().UTC(),
			Usage:        core.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		}, nil
	}
}

func TestRunMonitorProbeSkipsCanceledResults(t *testing.T) {
	repo := storage.NewMemoryRepository()
	registry := providers.NewRegistry(&cancelMonitorAdapter{})
	control := controlplane.New(repo, registry)
	if err := repo.UpsertModel(core.ModelConfig{
		ID:            "gpt-5",
		Provider:      core.ProviderOpenAI,
		UpstreamID:    "gpt-5",
		Enabled:       true,
		VisibleGroups: []string{core.DefaultAccountGroupName},
		Source:        core.ModelSourceManual,
	}); err != nil {
		t.Fatalf("UpsertModel returned error: %v", err)
	}
	if err := repo.UpsertAccount(core.Account{
		ID:       "acct_cancel",
		Provider: core.ProviderOpenAI,
		Label:    "Cancel Account",
		Group:    core.DefaultAccountGroupName,
		Status:   core.AccountStatusActive,
		Priority: 100,
		Weight:   100,
		Credential: core.Credential{
			Mode:        "manual-token",
			AccessToken: "token",
		},
	}); err != nil {
		t.Fatalf("UpsertAccount returned error: %v", err)
	}
	gatewayService := gateway.New(repo, routing.NewRouter(), failover.NewEngine(accounts.NewPool(repo), registry))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := RunMonitorProbe(ctx, control, gatewayService, core.MonitorTarget{
		ID:              "mon_cancel",
		AccountGroup:    core.DefaultAccountGroupName,
		Model:           "gpt-5",
		TimeoutSeconds:  30,
		IntervalSeconds: 300,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunMonitorProbe error = %v, want context.Canceled", err)
	}
	if history := repo.ListMonitorResults("mon_cancel", 10); len(history) != 0 {
		t.Fatalf("history after canceled probe = %#v, want empty", history)
	}
}

func TestPersonalPayAndroidTokenRequiresEnabledPersonalPay(t *testing.T) {
	settings := core.DefaultSystemSettings()
	settings.Payment.PersonalPay.AndroidToken = "android-token"
	settings.Backup.AndroidAutoEnabled = true
	settings.Payment.PersonalPay.Enabled = false

	if got := personalPayAndroidToken(settings); got != "personalpay-disabled" {
		t.Fatalf("backup-only android token = %q, want disabled marker", got)
	}

	settings.Backup.AndroidAutoEnabled = false
	if got := personalPayAndroidToken(settings); got != "personalpay-disabled" {
		t.Fatalf("disabled android token = %q, want disabled marker", got)
	}

	settings.Payment.PersonalPay.Enabled = true
	if got := personalPayAndroidToken(settings); got != "android-token" {
		t.Fatalf("payment-enabled android token = %q, want configured token", got)
	}
}

func TestNextDailyBackupTimeUsesLocalDay(t *testing.T) {
	location := time.FixedZone("test", 8*60*60)
	now := time.Date(2026, 5, 25, 2, 59, 30, 0, location)
	want := time.Date(2026, 5, 25, 3, 0, 0, 0, location)
	if got := nextDailyBackupTime(now, "03:00"); !got.Equal(want) {
		t.Fatalf("next backup time = %v, want %v", got, want)
	}

	now = time.Date(2026, 5, 25, 3, 0, 0, 0, location)
	want = time.Date(2026, 5, 25, 3, 0, 0, 0, location)
	if got := nextDailyBackupTime(now, "03:00"); !got.Equal(want) {
		t.Fatalf("next backup time at boundary = %v, want %v", got, want)
	}

	now = time.Date(2026, 5, 25, 1, 0, 0, 0, location)
	want = time.Date(2026, 5, 25, 3, 0, 0, 0, location)
	if got := nextDailyBackupTime(now, "bad"); !got.Equal(want) {
		t.Fatalf("fallback backup time = %v, want %v", got, want)
	}
}

func TestPendingAndroidBackupRunCurrentOnlyForSameLocalDay(t *testing.T) {
	location := time.FixedZone("test", 8*60*60)
	pending := time.Date(2026, 5, 25, 3, 0, 0, 0, location)

	if !pendingAndroidBackupRunCurrent(time.Date(2026, 5, 25, 3, 30, 0, 0, location), pending, "03:00") {
		t.Fatal("pending run should remain current after the scheduled time on the same day")
	}
	if pendingAndroidBackupRunCurrent(time.Date(2026, 5, 26, 0, 30, 0, 0, location), pending, "03:00") {
		t.Fatal("pending run should expire on the next local day")
	}
	if pendingAndroidBackupRunCurrent(time.Date(2026, 5, 25, 3, 30, 0, 0, location), pending, "04:00") {
		t.Fatal("pending run should expire when scheduled time changes")
	}
}

func TestRunAndroidBackupRequiresOnlineDeviceBeforeCreatingBackup(t *testing.T) {
	sdk, err := personalpay.Open(personalpay.Options{AndroidToken: "android-token"})
	if err != nil {
		t.Fatalf("open personalpay sdk: %v", err)
	}
	defer func() { _ = sdk.Shutdown(context.Background()) }()

	err = runAndroidBackup(context.Background(), sdk, backup.Options{
		StatePath: filepath.Join(t.TempDir(), "missing.db"),
	}, core.SystemBackupSettings{
		AndroidTimeOfDay: "03:00",
		AndroidDataSets:  []string{backup.DataSetBilling},
	})
	if !errors.Is(err, personalpay.ErrNoDevice) {
		t.Fatalf("runAndroidBackup err = %v, want ErrNoDevice", err)
	}
}

func TestBuildUsesPersistedRetentionBeforeInitialTrim(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")
	cfg := config.Default()
	cfg.Host = "127.0.0.1"
	cfg.Port = "18089"
	cfg.StatePath = statePath
	cfg.AuditLimit = 1

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	repo, err := storage.NewSQLiteRepository(statePath, "")
	if err != nil {
		t.Fatalf("NewSQLiteRepository returned error: %v", err)
	}
	settings := core.DefaultSystemSettings()
	settings.Retention.AuditLimit = 3
	settings.Retention.UsageLogMaxAgeDays = 7
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatalf("UpsertSystemSettings returned error: %v", err)
	}
	if err := repo.UpsertUser(core.User{ID: "user_retention", Username: "retention", Enabled: true, BalanceNanoUSD: core.NanoUSDPerUSD}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_retention", Name: "Retention", APIKey: "gw_retention", OwnerUserID: "user_retention", Enabled: true}); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_retention",
		ClientID:        "client_retention",
		UserID:          "user_retention",
		Model:           "gpt-retention",
		ReservedNanoUSD: 1000,
		Fingerprint:     "req_retention",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_retention",
		ClientID:      "client_retention",
		Model:         "gpt-retention",
		ActualNanoUSD: 1000,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := repo.AppendAudit(core.AuditEvent{
			ID:        fmt.Sprintf("audit_retention_%d", i),
			Kind:      core.AuditKindAdmin,
			Status:    "ok",
			Actor:     "test",
			Action:    "retention.test",
			CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("AppendAudit returned error: %v", err)
		}
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("Close seed repository returned error: %v", err)
	}

	db, err := sql.Open("sqlite", statePath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	oldBillingNS := time.Now().UTC().Add(-48 * time.Hour).UnixNano()
	if _, err := db.Exec(`UPDATE billing_requests SET created_at_ns = ?, settled_at_ns = ? WHERE request_id = ?`, oldBillingNS, oldBillingNS, "req_retention"); err != nil {
		_ = db.Close()
		t.Fatalf("backdate billing request: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}

	svc, err := Build(Options{ConfigPath: configPath})
	if err != nil {
		t.Fatalf("build app: %v", err)
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("close app: %v", err)
	}

	db, err = sql.Open("sqlite", statePath)
	if err != nil {
		t.Fatalf("reopen sqlite: %v", err)
	}
	defer db.Close()
	assertCount := func(query string, want int, args ...any) {
		t.Helper()
		var got int
		if err := db.QueryRow(query, args...).Scan(&got); err != nil {
			t.Fatalf("query count: %v", err)
		}
		if got != want {
			t.Fatalf("count for %q = %d, want %d", query, got, want)
		}
	}
	assertCount(`SELECT COUNT(*) FROM billing_requests WHERE request_id = ?`, 1, "req_retention")
	assertCount(`SELECT COUNT(*) FROM audit WHERE event_id LIKE 'audit_retention_%'`, 3)
}

func TestLogStartupEmitsDeploymentHardeningWarnings(t *testing.T) {
	t.Parallel()

	svc := &Service{
		Config: config.Config{
			Host:      "0.0.0.0",
			Address:   "0.0.0.0:8088",
			StatePath: "state.db",
		},
		Startup: StartupInfo{
			AdminAccount: core.User{Username: "root"},
			AdminSeeded:  true,
		},
	}

	var lines []string
	svc.LogStartup(func(format string, v ...any) {
		lines = append(lines, fmt.Sprintf(format, v...))
	})
	output := strings.Join(lines, "\n")
	for _, want := range []string{
		"WARNING: server is bound to non-loopback host 0.0.0.0",
		"WARNING: master_key is not configured",
		"WARNING: initial admin credentials were created",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("startup log missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "Initial admin password") {
		t.Fatalf("startup log still mentions initial admin password files or values:\n%s", output)
	}
}

func TestLogStartupOmitsDeploymentHardeningWarningsForHardenedLoopbackConfig(t *testing.T) {
	t.Parallel()

	svc := &Service{
		Config: config.Config{
			Host:      "127.0.0.1",
			Address:   "127.0.0.1:8088",
			StatePath: "state.db",
			MasterKey: "configured",
		},
		Startup: StartupInfo{
			AdminAccount: core.User{Username: "admin"},
		},
	}

	var lines []string
	svc.LogStartup(func(format string, v ...any) {
		lines = append(lines, fmt.Sprintf(format, v...))
	})
	output := strings.Join(lines, "\n")
	if strings.Contains(output, "WARNING:") {
		t.Fatalf("startup log contains unexpected warning:\n%s", output)
	}
}

func TestLogStartupUsesEffectiveGatewayAuditSettings(t *testing.T) {
	t.Parallel()

	svc := &Service{
		Config: config.Config{
			Host:      "127.0.0.1",
			Address:   "127.0.0.1:8088",
			StatePath: "state.db",
			MasterKey: "configured",
		},
		Startup: StartupInfo{
			AdminAccount:              core.User{Username: "admin"},
			GatewayAuditErrors:        true,
			GatewayAuditRetentionDays: 3,
		},
	}

	var lines []string
	svc.LogStartup(func(format string, v ...any) {
		lines = append(lines, fmt.Sprintf(format, v...))
	})
	output := strings.Join(lines, "\n")
	if !strings.Contains(output, "Gateway request audit: errors only, retention=3d") {
		t.Fatalf("startup log missing effective gateway audit settings:\n%s", output)
	}
}

func TestBuildAppliesPublicBaseURLFromConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.Host = "127.0.0.1"
	cfg.Port = "18088"
	cfg.StatePath = filepath.Join(dir, "state.db")
	cfg.PublicBaseURL = "https://example.com/"
	svc, err := NewBuilder().WithConfig(cfg).Build()
	if err != nil {
		t.Fatalf("build app: %v", err)
	}
	defer func() { _ = svc.Close() }()
	settings, err := svc.Control.GetSystemSettings()
	if err != nil {
		t.Fatalf("GetSystemSettings returned error: %v", err)
	}
	if settings.Runtime.PublicBaseURL != "https://example.com" {
		t.Fatalf("PublicBaseURL = %q", settings.Runtime.PublicBaseURL)
	}
}

func TestBuildAppliesProtocolRequestBodyLimitFromConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.Host = "127.0.0.1"
	cfg.Port = "18088"
	cfg.StatePath = filepath.Join(dir, "state.db")
	cfg.ProtocolRequestBodyLimit = 8

	svc, err := NewBuilder().WithConfig(cfg).Build()
	if err != nil {
		t.Fatalf("build app: %v", err)
	}
	defer func() { _ = svc.Close() }()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("0123456789"))
	req.Header.Set("Authorization", "Bearer "+svc.Startup.ProtocolClient.APIKey)
	rr := httptest.NewRecorder()
	svc.Web.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rr.Body.String(), "request body too large") {
		t.Fatalf("response = %q, want request body too large", rr.Body.String())
	}
}

func TestBuildMountsPersonalPayAndroidWebSocket(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.Host = "127.0.0.1"
	cfg.Port = "18090"
	cfg.StatePath = filepath.Join(dir, "state.db")
	repo := storage.NewMemoryRepository()
	settings := core.DefaultSystemSettings()
	settings.Payment.PersonalPay.Enabled = true
	settings.Payment.PersonalPay.AndroidToken = "android-token"
	if err := repo.UpsertSystemSettings(settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	svc, err := NewBuilder().WithConfig(cfg).WithRepository(repo, nil).Build()
	if err != nil {
		t.Fatalf("build app: %v", err)
	}
	defer func() { _ = svc.Close() }()

	recorder := httptest.NewRecorder()
	svc.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/personalpay/android/ws", nil))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if !strings.Contains(recorder.Body.String(), "missing_device_id") {
		t.Fatalf("response = %q, want missing_device_id", recorder.Body.String())
	}
}

func TestBuilderSupportsInjectedRepositoryAndHooks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.Host = "127.0.0.1"
	cfg.Port = "18089"
	cfg.StatePath = filepath.Join(dir, "state.db")

	repo := storage.NewMemoryRepository()
	closed := false
	beforeSeedCalled := false
	afterBuildCalled := false
	svc, err := NewBuilder().
		WithConfig(cfg).
		WithRepository(repo, func() error {
			closed = true
			return nil
		}).
		WithHooks(Hooks{
			OnBeforeSeed: func(ctx *BuildContext) error {
				beforeSeedCalled = ctx != nil && ctx.Repository == repo && ctx.Control != nil
				return nil
			},
			OnAfterBuild: func(svc *Service) error {
				afterBuildCalled = svc != nil && svc.HTTPServer != nil
				return nil
			},
		}).
		Build()
	if err != nil {
		t.Fatalf("build app: %v", err)
	}
	if !beforeSeedCalled {
		t.Fatal("expected before seed hook")
	}
	if !afterBuildCalled {
		t.Fatal("expected after build hook")
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("close app: %v", err)
	}
	if !closed {
		t.Fatal("expected injected repository closer")
	}
}

func TestBuilderReturnsHookErrors(t *testing.T) {
	t.Parallel()

	expected := errors.New("stop")
	_, err := NewBuilder().
		WithConfig(config.Default()).
		WithRepository(storage.NewMemoryRepository(), nil).
		WithHooks(Hooks{
			OnBeforeSeed: func(*BuildContext) error {
				return expected
			},
		}).
		Build()
	if !errors.Is(err, expected) {
		t.Fatalf("build error = %v, want %v", err, expected)
	}
}

func TestParseServerFlagsSupportsListenOverrides(t *testing.T) {
	t.Parallel()

	flags, err := ParseServerFlags([]string{"-config", "custom.json", "-host", "127.0.0.2", "-port", "19090"})
	if err != nil {
		t.Fatalf("ParseServerFlags returned error: %v", err)
	}
	if flags.ConfigPath != "custom.json" || flags.Host != "127.0.0.2" || flags.Port != "19090" {
		t.Fatalf("flags = %#v", flags)
	}
}

func TestRunRestoreCommandRequiresBackupPath(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	err := RunRestoreCommandWithOptions(nil, CommandOptions{Stdout: &out})
	if err == nil {
		t.Fatal("expected error")
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestRunBackupCommandRejectsUnexpectedArgs(t *testing.T) {
	t.Parallel()

	err := RunBackupCommandWithOptions([]string{"extra"}, CommandOptions{})
	if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("RunBackupCommandWithOptions err=%v, want unexpected argument", err)
	}
}

func TestRunRestoreCommandRejectsUnexpectedArgs(t *testing.T) {
	t.Parallel()

	err := RunRestoreCommandWithOptions([]string{"-from", "backup.agbak", "extra"}, CommandOptions{})
	if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("RunRestoreCommandWithOptions err=%v, want unexpected argument", err)
	}
}

func TestRunRestoreCommandClarifiesLiveReloadLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.StatePath = filepath.Join(dir, "state.db")
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	backupPath := filepath.Join(dir, "backup.agbak")
	if _, err := backup.Create(backupPath, backup.Options{
		ConfigPath: AbsoluteConfigPath(configPath),
		StatePath:  cfg.StatePath,
	}); err != nil {
		t.Fatalf("create backup: %v", err)
	}
	var out bytes.Buffer
	if err := RunRestoreCommandWithOptions([]string{"-config", configPath, "-from", backupPath}, CommandOptions{Stdout: &out}); err != nil {
		t.Fatalf("restore command: %v", err)
	}
	output := out.String()
	if strings.Contains(output, "restart that process") {
		t.Fatalf("restore output should not use stale restart wording: %s", output)
	}
	if !strings.Contains(output, "admin console restore for live reload") {
		t.Fatalf("restore output missing live reload guidance: %s", output)
	}
}

func TestParseConfigPathFlag(t *testing.T) {
	t.Parallel()

	path, err := ParseConfigPathFlag([]string{"-config", "custom.json"})
	if err != nil {
		t.Fatalf("parse config flag: %v", err)
	}
	if path != "custom.json" {
		t.Fatalf("config path = %q", path)
	}
	if _, err := ParseConfigPathFlag([]string{"-unknown"}); err == nil {
		t.Fatal("expected unknown flag error")
	}
	if _, err := ParseConfigPathFlag([]string{"extra"}); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("ParseConfigPathFlag unexpected arg err=%v", err)
	}
}

func TestRestoreBackupRebuildsRuntimeWithoutRestart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.Host = "127.0.0.1"
	cfg.Port = "18091"
	cfg.APIKey = "restore-test-key"
	cfg.StatePath = filepath.Join(dir, "state.db")

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	svc, err := Build(Options{ConfigPath: configPath})
	if err != nil {
		t.Fatalf("build app: %v", err)
	}
	defer func() {
		if err := svc.Close(); err != nil {
			t.Fatalf("close app: %v", err)
		}
	}()

	backupPath := filepath.Join(dir, "before-model.agbak")
	opts := backup.Options{
		ConfigPath: AbsoluteConfigPath(configPath),
		StatePath:  cfg.StatePath,
	}
	if _, err := backup.Create(backupPath, opts); err != nil {
		t.Fatalf("create backup: %v", err)
	}
	if _, err := svc.Control.CreateModel(controlplane.ModelInput{
		ID:       "restore-only-model",
		Provider: core.ProviderOpenAI,
		Enabled:  true,
	}); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if got := svc.Control.ModelPage(t.Context()).Stats.Total; got == 0 {
		t.Fatal("expected model before restore")
	}

	preRestorePath, err := svc.RestoreBackup(backupPath, opts, filepath.Join(dir, "pre"))
	if err != nil {
		t.Fatalf("restore backup: %v", err)
	}
	if preRestorePath == "" {
		t.Fatal("expected pre-restore backup path")
	}
	if _, err := os.Stat(preRestorePath); err != nil {
		t.Fatalf("pre-restore backup missing: %v", err)
	}
	for _, model := range svc.Control.ModelPage(t.Context()).Models {
		if model.ID == "restore-only-model" {
			t.Fatalf("model %q survived restore", model.ID)
		}
	}
	if svc.HTTPServer == nil || svc.HTTPServer.Handler == nil {
		t.Fatal("expected HTTP server to remain available")
	}
}

func TestRestoreBackupCreatesFullPreRestoreBackupForSelectedRestore(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.Host = "127.0.0.1"
	cfg.Port = "18093"
	cfg.APIKey = "restore-selected-key"
	cfg.StatePath = filepath.Join(dir, "state.db")
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	svc, err := Build(Options{ConfigPath: configPath})
	if err != nil {
		t.Fatalf("build app: %v", err)
	}
	defer func() {
		if err := svc.Close(); err != nil {
			t.Fatalf("close app: %v", err)
		}
	}()

	restorePath := filepath.Join(dir, "models-only.agbak")
	opts := backup.Options{
		ConfigPath: AbsoluteConfigPath(configPath),
		StatePath:  cfg.StatePath,
		DataSets:   []string{backup.DataSetModels},
	}
	if _, err := backup.Create(restorePath, opts); err != nil {
		t.Fatalf("create selected backup: %v", err)
	}
	preRestorePath, err := svc.RestoreBackup(restorePath, opts, filepath.Join(dir, "pre"))
	if err != nil {
		t.Fatalf("restore selected backup: %v", err)
	}
	file, err := os.Open(preRestorePath)
	if err != nil {
		t.Fatalf("open pre-restore backup: %v", err)
	}
	defer file.Close()
	manifest, err := backup.Inspect(file)
	if err != nil {
		t.Fatalf("inspect pre-restore backup: %v", err)
	}
	if !manifest.Includes.Config || !manifest.Includes.Database || manifest.Includes.Data {
		t.Fatalf("pre-restore manifest = %#v, want full sqlite backup", manifest)
	}
}

func TestRestoreBackupRollsBackWhenRestoreFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.Host = "127.0.0.1"
	cfg.Port = "18092"
	cfg.APIKey = "restore-rollback-key"
	cfg.StatePath = filepath.Join(dir, "state.db")
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	svc, err := Build(Options{ConfigPath: configPath})
	if err != nil {
		t.Fatalf("build app: %v", err)
	}
	defer func() {
		if err := svc.Close(); err != nil {
			t.Fatalf("close app: %v", err)
		}
	}()
	if _, err := svc.Control.CreateModel(controlplane.ModelInput{
		ID:       "survives-failed-restore",
		Provider: core.ProviderOpenAI,
		Enabled:  true,
	}); err != nil {
		t.Fatalf("create model: %v", err)
	}
	badBackup := filepath.Join(dir, "bad.agbak")
	if err := os.WriteFile(badBackup, []byte("not a backup"), 0o600); err != nil {
		t.Fatalf("write bad backup: %v", err)
	}

	_, err = svc.RestoreBackup(badBackup, backup.Options{
		ConfigPath: AbsoluteConfigPath(configPath),
		StatePath:  cfg.StatePath,
	}, filepath.Join(dir, "pre"))
	if err == nil {
		t.Fatal("expected restore error")
	}
	found := false
	for _, model := range svc.Control.ModelPage(t.Context()).Models {
		if model.ID == "survives-failed-restore" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected rollback to preserve existing model")
	}
	recorder := httptest.NewRecorder()
	svc.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("health after rollback = %d", recorder.Code)
	}
}

func TestServeHTTPReturnsUnavailableDuringRuntimeReload(t *testing.T) {
	t.Parallel()

	svc := &Service{}
	closers := svc.beginRuntimeReload()
	defer closeRuntime(closers)

	recorder := httptest.NewRecorder()
	svc.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "service runtime is reloading") {
		t.Fatalf("body = %q", recorder.Body.String())
	}
}

func TestReadinessReflectsRuntimeState(t *testing.T) {
	t.Parallel()

	svc := &Service{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})}
	recorder := httptest.NewRecorder()
	svc.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("ready status = %d", recorder.Code)
	}

	closers := svc.beginRuntimeReload()
	defer closeRuntime(closers)
	recorder = httptest.NewRecorder()
	svc.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("reload ready status = %d", recorder.Code)
	}
}

func TestShutdownCancelsRequestContexts(t *testing.T) {
	t.Parallel()

	requestCtx, cancelRequests := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan struct{})
	svc := &Service{
		cancelRequests: cancelRequests,
		handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-r.Context().Done()
			close(done)
		}),
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	svc.HTTPServer = &http.Server{
		Handler: http.HandlerFunc(svc.ServeHTTP),
		BaseContext: func(net.Listener) context.Context {
			return requestCtx
		},
	}
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- svc.HTTPServer.Serve(listener)
	}()

	clientDone := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + listener.Addr().String() + "/busy")
		if err != nil {
			clientDone <- err
			return
		}
		defer resp.Body.Close()
		_, err = io.Copy(io.Discard, resp.Body)
		clientDone <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for request to start")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := svc.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request context was not cancelled")
	}
	select {
	case err := <-serverDone:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("server returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
	select {
	case err := <-clientDone:
		if err != nil {
			t.Fatalf("client returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("client did not finish")
	}
}

func TestLimitInFlightBypassesHealth(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	started := make(chan struct{})
	handler := limitInFlight(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/busy" {
			close(started)
			<-block
		}
		w.WriteHeader(http.StatusNoContent)
	}), 1)

	go handler.ServeHTTP(httptest.NewRecorder(), requestForPath("/busy"))
	<-started
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, requestForPath("/healthz"))
	close(block)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("health status = %d", recorder.Code)
	}
}

func TestLimitInFlightReservesCapacityForConsole(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	started := make(chan struct{}, 2)
	handler := limitInFlight(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			started <- struct{}{}
			<-block
		}
		w.WriteHeader(http.StatusNoContent)
	}), 3)

	go handler.ServeHTTP(httptest.NewRecorder(), requestForPath("/v1/chat/completions"))
	go handler.ServeHTTP(httptest.NewRecorder(), requestForPath("/v1/responses"))
	<-started
	<-started

	protocolRecorder := httptest.NewRecorder()
	handler.ServeHTTP(protocolRecorder, requestForPath("/v1/models"))
	if protocolRecorder.Code != http.StatusServiceUnavailable {
		close(block)
		t.Fatalf("protocol status = %d, want %d", protocolRecorder.Code, http.StatusServiceUnavailable)
	}

	consoleRecorder := httptest.NewRecorder()
	handler.ServeHTTP(consoleRecorder, requestForPath("/admin/settings"))
	close(block)
	if consoleRecorder.Code != http.StatusNoContent {
		t.Fatalf("console status = %d, want %d", consoleRecorder.Code, http.StatusNoContent)
	}
}

func requestForPath(path string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "http://example.test"+path, nil)
}
