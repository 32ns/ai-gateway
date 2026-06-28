//go:build linux

package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultLinuxInstallConfigUsesSystemPaths(t *testing.T) {
	t.Parallel()

	cfg := defaultLinuxInstallConfig(InstallWizardOptions{})
	if cfg.InstallDir != defaultInstallDir {
		t.Fatalf("install dir = %q", cfg.InstallDir)
	}
	if cfg.ConfigFile != "/etc/ai-gateway/config.json" {
		t.Fatalf("config file = %q", cfg.ConfigFile)
	}
	if cfg.AppHost != "127.0.0.1" || cfg.AppPort != "18088" {
		t.Fatalf("app address = %s:%s", cfg.AppHost, cfg.AppPort)
	}
}

func TestDefaultLinuxInstallConfigUsesExistingPublicBaseURL(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"public_base_url":"https://example.com/"}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := defaultLinuxInstallConfig(InstallWizardOptions{ConfigPath: configPath})
	if cfg.CaddySite != "example.com" {
		t.Fatalf("CaddySite = %q, want example.com", cfg.CaddySite)
	}
	if cfg.PublicBaseURL != "https://example.com" {
		t.Fatalf("PublicBaseURL = %q", cfg.PublicBaseURL)
	}
}

func TestDefaultLinuxInstallConfigMakesCustomConfigAbsolute(t *testing.T) {
	t.Parallel()

	cfg := defaultLinuxInstallConfig(InstallWizardOptions{ConfigPath: "custom.json"})
	if !strings.HasSuffix(cfg.ConfigFile, "custom.json") {
		t.Fatalf("config file = %q", cfg.ConfigFile)
	}
	if !strings.HasPrefix(cfg.ConfigFile, "/") {
		t.Fatalf("config file should be absolute: %q", cfg.ConfigFile)
	}
}

func TestCaddySiteAndPublicBaseURLConversion(t *testing.T) {
	t.Parallel()

	if got := publicBaseURLForCaddySite("example.com"); got != "https://example.com" {
		t.Fatalf("publicBaseURLForCaddySite domain = %q", got)
	}
	if got := publicBaseURLForCaddySite(":80"); got != "" {
		t.Fatalf("publicBaseURLForCaddySite :80 = %q, want empty", got)
	}
	if got := caddySiteForPublicBaseURL("https://example.com/"); got != "example.com" {
		t.Fatalf("caddySiteForPublicBaseURL = %q", got)
	}
	if got := caddySiteForPublicBaseURL("http://127.0.0.1"); got != "" {
		t.Fatalf("caddySiteForPublicBaseURL ip = %q, want empty", got)
	}
}

func TestValidateLinuxInstallConfigRejectsInvalidPublicBaseURL(t *testing.T) {
	t.Parallel()

	cfg := defaultLinuxInstallConfig(InstallWizardOptions{})
	cfg.PublicBaseURL = "https://example.com/path"
	if err := validateLinuxInstallConfig(cfg); err == nil {
		t.Fatal("expected invalid public base URL")
	}
}

func TestPromptLinuxInstallCanAcceptDefaults(t *testing.T) {
	t.Parallel()

	input := strings.NewReader("\n\n\n\n\n\n\n\n\n")
	var output bytes.Buffer
	cfg, proceed, err := promptLinuxInstall(bufio.NewReader(input), &output, InstallWizardOptions{})
	if err != nil {
		t.Fatalf("prompt install: %v", err)
	}
	if !proceed {
		t.Fatal("expected proceed")
	}
	if cfg.StatePath != "/var/lib/ai-gateway/state.db" {
		t.Fatalf("state path = %q", cfg.StatePath)
	}
	if !strings.Contains(output.String(), "AI Gateway install wizard") {
		t.Fatalf("missing heading: %s", output.String())
	}
}

func TestPromptLinuxInstallStartsWithDomain(t *testing.T) {
	t.Parallel()

	input := strings.NewReader("example.com\n\n\n\n\n\n\n\n\n")
	var output bytes.Buffer
	cfg, proceed, err := promptLinuxInstall(bufio.NewReader(input), &output, InstallWizardOptions{})
	if err != nil {
		t.Fatalf("prompt install: %v", err)
	}
	if !proceed {
		t.Fatal("expected proceed")
	}
	if cfg.CaddySite != "example.com" || cfg.PublicBaseURL != "https://example.com" {
		t.Fatalf("caddy/public url = %q/%q", cfg.CaddySite, cfg.PublicBaseURL)
	}
	lines := strings.Split(output.String(), "\n")
	if len(lines) < 4 || !strings.HasPrefix(lines[3], "Domain or address") {
		t.Fatalf("first prompt should be domain, output=%s", output.String())
	}
	if strings.Contains(output.String(), "Public Base URL [") {
		t.Fatalf("public base URL should be derived, not prompted: %s", output.String())
	}
}

func TestSystemdEscape(t *testing.T) {
	t.Parallel()

	got := systemdEscape("/opt/ai gateway/%i/ag\tline\nnext\r")
	want := `/opt/ai\x20gateway/%%i/ag\x09line\x0anext\x0d`
	if got != want {
		t.Fatalf("escape = %q, want %q", got, want)
	}
}

func TestWriteSystemdServiceUsesUnquotedAbsoluteWorkingDirectory(t *testing.T) {
	t.Parallel()

	cfg := defaultLinuxInstallConfig(InstallWizardOptions{})
	cfg.ServiceName = "ai-gateway-test"
	servicePath := serviceUnitPath(slotServiceName(cfg.ServiceName, cfg.ActiveSlot))
	if err := os.Remove(servicePath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove old service file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(servicePath) })
	if err := writeSystemdService(cfg); err != nil {
		t.Fatalf("write systemd service: %v", err)
	}
	raw, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatalf("read service file: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, "WorkingDirectory=/opt/ai-gateway\n") {
		t.Fatalf("service file should use unquoted WorkingDirectory: %s", body)
	}
	if !strings.Contains(body, "ExecStart=/opt/ai-gateway/current/ag -config /etc/ai-gateway/config.json -host 127.0.0.1 -port 18088") {
		t.Fatalf("service file should use current release and slot port: %s", body)
	}
	if strings.Contains(body, `WorkingDirectory="/opt/ai-gateway"`) {
		t.Fatalf("service file has invalid quoted WorkingDirectory: %s", body)
	}
}

func TestValidateLinuxInstallConfig(t *testing.T) {
	t.Parallel()

	cfg := defaultLinuxInstallConfig(InstallWizardOptions{})
	if err := validateLinuxInstallConfig(cfg); err != nil {
		t.Fatalf("validate defaults: %v", err)
	}
	cfg.ServiceName = "../bad"
	if err := validateLinuxInstallConfig(cfg); err == nil {
		t.Fatal("expected bad service name error")
	}
	cfg = defaultLinuxInstallConfig(InstallWizardOptions{})
	cfg.CaddySite = "example.com {\n}"
	if err := validateLinuxInstallConfig(cfg); err == nil {
		t.Fatal("expected bad Caddy site error")
	}
	cfg = defaultLinuxInstallConfig(InstallWizardOptions{})
	cfg.AppPort = "70000"
	if err := validateLinuxInstallConfig(cfg); err == nil {
		t.Fatal("expected bad port error")
	}
	cfg = defaultLinuxInstallConfig(InstallWizardOptions{})
	cfg.ServiceName = "ai-gateway.service"
	if err := validateLinuxInstallConfig(cfg); err != nil {
		t.Fatalf("validate service suffix: %v", err)
	}
	if got := normalizeSystemdServiceName(cfg.ServiceName); got != "ai-gateway" {
		t.Fatalf("normalized service = %q", got)
	}
}

func TestInstallCaddyDNFEnablesCoprRepository(t *testing.T) {
	t.Parallel()

	var commands []string
	installed, err := installCaddyIfMissingWith(
		func(string) (string, error) { return "", errors.New("missing") },
		func(name string) bool { return name == "dnf" },
		func(name string, args ...string) error {
			commands = append(commands, name+" "+strings.Join(args, " "))
			return nil
		},
		func(string) error { return nil },
	)
	if err != nil {
		t.Fatalf("install caddy: %v", err)
	}
	if !installed {
		t.Fatal("expected install flag")
	}
	want := []string{
		"dnf install -y dnf5-plugins",
		"dnf copr enable -y @caddy/caddy",
		"dnf install -y caddy",
	}
	if strings.Join(commands, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands = %#v, want %#v", commands, want)
	}
}

func TestInstallCaddyDNFFallsBackToDnfPluginsCore(t *testing.T) {
	t.Parallel()

	var commands []string
	installed, err := installCaddyIfMissingWith(
		func(string) (string, error) { return "", errors.New("missing") },
		func(name string) bool { return name == "dnf" },
		func(name string, args ...string) error {
			command := name + " " + strings.Join(args, " ")
			commands = append(commands, command)
			if strings.Contains(command, "dnf5-plugins") {
				return errors.New("no dnf5")
			}
			return nil
		},
		func(string) error { return nil },
	)
	if err != nil {
		t.Fatalf("install caddy: %v", err)
	}
	if !installed {
		t.Fatal("expected install flag")
	}
	want := []string{
		"dnf install -y dnf5-plugins",
		"dnf install -y dnf-plugins-core",
		"dnf copr enable -y @caddy/caddy",
		"dnf install -y caddy",
	}
	if strings.Join(commands, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands = %#v, want %#v", commands, want)
	}
}

func TestRecoverablePartialInstallDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "releases"), 0o755); err != nil {
		t.Fatalf("create releases: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "releases", "old"), filepath.Join(dir, "current")); err != nil {
		t.Fatalf("create current symlink: %v", err)
	}
	ok, err := recoverablePartialInstallDir(dir)
	if err != nil {
		t.Fatalf("recoverable partial: %v", err)
	}
	if !ok {
		t.Fatal("expected release/current directory to be recoverable")
	}

	if err := os.WriteFile(filepath.Join(dir, "custom.txt"), []byte("user data"), 0o644); err != nil {
		t.Fatalf("write custom file: %v", err)
	}
	ok, err = recoverablePartialInstallDir(dir)
	if err != nil {
		t.Fatalf("recoverable partial with custom file: %v", err)
	}
	if ok {
		t.Fatal("expected custom file to block recovery")
	}
}

func TestRepairedStandbyPortAvoidsActivePort(t *testing.T) {
	t.Parallel()

	if got := repairedStandbyPort("18088", "18088"); got != "18089" {
		t.Fatalf("standby = %q, want 18089", got)
	}
	if got := repairedStandbyPort("18089", "18089"); got != "18088" {
		t.Fatalf("standby = %q, want 18088", got)
	}
	if got := repairedStandbyPort("18088", "19000"); got != "19000" {
		t.Fatalf("standby = %q, want preserved custom port", got)
	}
}

func TestServiceUnitPathNormalizesServiceSuffix(t *testing.T) {
	t.Parallel()

	got := serviceUnitPath("ai-gateway.service")
	want := "/etc/systemd/system/ai-gateway.service"
	if got != want {
		t.Fatalf("unit path = %q, want %q", got, want)
	}
}

func TestLinuxServiceCommandsRejectUnexpectedArgs(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		run  func() error
	}{
		{name: "install", run: func() error { return RunInstallCommand([]string{"extra"}) }},
		{name: "uninstall", run: func() error { return RunUninstallCommand([]string{"extra"}) }},
		{name: "upgrade", run: func() error { return RunUpgradeCommand([]string{"extra"}) }},
		{name: "rollback", run: func() error { return RunRollbackCommand([]string{"extra"}) }},
		{name: "reinstall", run: func() error { return RunReinstallCommand([]string{"extra"}) }},
		{name: "restart", run: func() error { return runServiceActionCommand("restart", []string{"extra"}, "restart") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.run()
			if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
				t.Fatalf("%s err=%v, want unexpected argument", tt.name, err)
			}
		})
	}
}

func TestRemoveManagedPathRejectsCustomPath(t *testing.T) {
	t.Parallel()

	if err := removeManagedPath("/tmp/ai-gateway"); err == nil {
		t.Fatal("expected unmanaged path rejection")
	}
}

func TestWriteAndReadLinuxInstallRecord(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	record := linuxInstallRecord{
		InstallDir:      dir,
		ConfigFile:      filepath.Join(dir, "config.json"),
		ConfigCreated:   true,
		ServiceName:     "ai-gateway",
		AppHost:         "127.0.0.1",
		AppPort:         "18088",
		CaddyConfigured: true,
		CaddySite:       ":80",
		CaddyInstalled:  true,
	}
	if err := writeLinuxInstallRecord(record); err != nil {
		t.Fatalf("write install record: %v", err)
	}
	got, ok, err := readLinuxInstallRecord(dir)
	if err != nil || !ok {
		t.Fatalf("read install record ok=%t err=%v", ok, err)
	}
	if got.InstallDir != record.InstallDir || got.ConfigFile != record.ConfigFile || !got.CaddyConfigured || !got.CaddyInstalled {
		t.Fatalf("install record = %#v, want %#v", got, record)
	}
}

func TestRemoveRecordedInstallDirRequiresManifest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	record := linuxInstallRecord{InstallDir: dir, InstallDirCreated: true}
	if err := cleanRecordedInstallDir(record); err == nil {
		t.Fatal("expected install record requirement")
	}
	if err := writeLinuxInstallRecord(record); err != nil {
		t.Fatalf("write install record: %v", err)
	}
	if err := cleanRecordedInstallDir(record); err != nil {
		t.Fatalf("remove recorded install dir: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("install dir err=%v, want not exist", err)
	}
}

func TestCleanRecordedInstallDirPreservesPreexistingDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	keepPath := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(keepPath, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write keep file: %v", err)
	}
	stateDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", stateDir, err)
	}
	record := linuxInstallRecord{
		InstallDir:        dir,
		InstallDirCreated: false,
		StatePath:         filepath.Join(stateDir, "state.db"),
		StateDirCreated:   true,
	}
	if err := os.WriteFile(filepath.Join(dir, "ag"), []byte("binary"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if err := writeLinuxInstallRecord(record); err != nil {
		t.Fatalf("write install record: %v", err)
	}
	if err := cleanRecordedInstallDir(record); err != nil {
		t.Fatalf("clean recorded install dir: %v", err)
	}
	if _, err := os.Stat(keepPath); err != nil {
		t.Fatalf("preexisting file should remain: %v", err)
	}
	for _, path := range []string{filepath.Join(dir, "ag"), installRecordPath(dir), stateDir} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s err=%v, want not exist", path, err)
		}
	}
}

func TestCleanRecordedInstallDirKeepsPreexistingEmptyInstallDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	record := linuxInstallRecord{
		InstallDir:        dir,
		InstallDirCreated: false,
	}
	if err := os.WriteFile(filepath.Join(dir, "ag"), []byte("binary"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if err := writeLinuxInstallRecord(record); err != nil {
		t.Fatalf("write install record: %v", err)
	}
	if err := cleanRecordedInstallDir(record); err != nil {
		t.Fatalf("clean recorded install dir: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("preexisting empty install dir should remain, info=%v err=%v", info, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("preexisting install dir entries=%v err=%v, want empty", entries, err)
	}
}

func TestInstallRecordTracksPreexistingEmptyInstallDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	installDir := filepath.Join(dir, "install")
	if err := os.Mkdir(installDir, 0o755); err != nil {
		t.Fatalf("mkdir install dir: %v", err)
	}
	record := linuxInstallRecord{
		InstallDir:        installDir,
		InstallDirCreated: !mustPathExists(t, installDir),
		StatePath:         filepath.Join(installDir, "data", "state.db"),
	}
	if record.InstallDirCreated {
		t.Fatal("preexisting empty install dir should not be marked as created")
	}
}

func TestDirectoryCreatedFlagFollowsCurrentPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	existingDir := filepath.Join(dir, "existing")
	newDir := filepath.Join(dir, "new")
	if err := os.Mkdir(existingDir, 0o755); err != nil {
		t.Fatalf("mkdir existing: %v", err)
	}
	existingCreated := true
	if exists, err := pathExists(existingDir); err != nil {
		t.Fatalf("path exists existing: %v", err)
	} else if exists {
		existingCreated = false
	}
	newCreated := false
	if exists, err := pathExists(newDir); err != nil {
		t.Fatalf("path exists new: %v", err)
	} else if !exists {
		newCreated = true
	}
	if existingCreated {
		t.Fatal("existing directory should not be marked as created")
	}
	if !newCreated {
		t.Fatal("missing directory should be marked as created before mkdir")
	}
}

func TestValidateLinuxInstallRecordRejectsExternalManagedPaths(t *testing.T) {
	t.Parallel()

	record := linuxInstallRecord{
		InstallDir: "/opt/ai-gateway",
		StatePath:  "/var/lib/ai-gateway/state.db",
	}
	if err := validateLinuxInstallRecord(record); err != nil {
		t.Fatalf("expected managed data path to be accepted: %v", err)
	}
	record.StatePath = "/opt/ai-gateway/data/state.db"
	if err := validateLinuxInstallRecord(record); err == nil {
		t.Fatal("expected install-dir data path rejection")
	}
	record.StatePath = "/tmp/state.db"
	if err := validateLinuxInstallRecord(record); err == nil {
		t.Fatal("expected unmanaged state path rejection")
	}
}

func mustPathExists(t *testing.T, path string) bool {
	t.Helper()
	exists, err := pathExists(path)
	if err != nil {
		t.Fatalf("path exists %s: %v", path, err)
	}
	return exists
}

func TestCaddyfileBodyIsStable(t *testing.T) {
	t.Parallel()

	got := caddyfileBody(linuxInstallConfig{CaddySite: "example.com", AppHost: "127.0.0.1", AppPort: "18088"})
	for _, want := range []string{"example.com {", "encode gzip", "reverse_proxy 127.0.0.1:18088"} {
		if !strings.Contains(got, want) {
			t.Fatalf("caddyfile body missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "health_uri") {
		t.Fatalf("caddyfile should not enable active health checks: %s", got)
	}
}

func TestWriteConfigIfMissingIncludesPublicBaseURL(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := defaultLinuxInstallConfig(InstallWizardOptions{ConfigPath: filepath.Join(dir, "config.json")})
	cfg.PublicBaseURL = "https://example.com"
	created, err := writeConfigIfMissing(cfg)
	if err != nil || !created {
		t.Fatalf("writeConfigIfMissing created=%t err=%v", created, err)
	}
	raw, err := os.ReadFile(cfg.ConfigFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if payload["public_base_url"] != "https://example.com" {
		t.Fatalf("public_base_url = %#v", payload["public_base_url"])
	}
	if payload["database_backend"] != "sqlite" {
		t.Fatalf("database_backend = %#v", payload["database_backend"])
	}
}
