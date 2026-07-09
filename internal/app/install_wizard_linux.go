//go:build linux

package app

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultInstallDir  = "/opt/ai-gateway"
	defaultConfigDir   = "/etc/ai-gateway"
	defaultDataDir     = "/var/lib/ai-gateway"
	defaultServiceName = "ai-gateway"
	defaultServiceHost = "127.0.0.1"
	defaultServicePort = "18088"
	defaultStandbyPort = "18089"
	slotBlue           = "blue"
	slotGreen          = "green"
	installManifest    = "install.json"
)

type linuxInstallConfig struct {
	InstallDir     string
	ConfigFile     string
	ServiceName    string
	AppHost        string
	AppPort        string
	StandbyPort    string
	ActiveSlot     string
	StatePath      string
	CaddySite      string
	PublicBaseURL  string
	InstallCaddy   bool
	ConfigureCaddy bool
}

type linuxInstallRecord struct {
	InstallDir        string `json:"install_dir"`
	InstallDirCreated bool   `json:"install_dir_created"`
	ConfigFile        string `json:"config_file"`
	ConfigCreated     bool   `json:"config_created"`
	ConfigDirCreated  bool   `json:"config_dir_created"`
	ServiceName       string `json:"service_name"`
	AppHost           string `json:"app_host"`
	AppPort           string `json:"app_port"`
	StandbyPort       string `json:"standby_port"`
	ActiveSlot        string `json:"active_slot"`
	StatePath         string `json:"state_path"`
	StateDirCreated   bool   `json:"state_dir_created"`
	CaddySite         string `json:"caddy_site,omitempty"`
	CaddyConfigured   bool   `json:"caddy_configured"`
	CaddyBackupPath   string `json:"caddy_backup_path,omitempty"`
	CaddyInstalled    bool   `json:"caddy_installed"`
	CurrentRelease    string `json:"current_release,omitempty"`
	PreviousRelease   string `json:"previous_release,omitempty"`
}

func printLinuxInstallSummary(cfg linuxInstallConfig) {
	fmt.Fprintln(os.Stdout)
	fmt.Fprintln(os.Stdout, "Deployment finished.")
	fmt.Fprintf(os.Stdout, "App service: systemctl status %s\n", slotServiceName(cfg.ServiceName, cfg.ActiveSlot))
	fmt.Fprintf(os.Stdout, "App config:   %s\n", cfg.ConfigFile)
	fmt.Fprintf(os.Stdout, "App data:     %s\n", defaultDataDir)
	if cfg.ConfigureCaddy {
		fmt.Fprintf(os.Stdout, "Caddy site:   %s -> %s:%s\n", cfg.CaddySite, cfg.AppHost, cfg.AppPort)
	}
}

func promptLinuxInstall(reader *bufio.Reader, output io.Writer, options InstallWizardOptions) (linuxInstallConfig, bool, error) {
	cfg := defaultLinuxInstallConfig(options)
	fmt.Fprintln(output, "AI Gateway install wizard")
	fmt.Fprintln(output, "This wizard will install AI Gateway as a systemd service.")
	fmt.Fprintln(output)

	var err error
	if cfg.CaddySite, err = promptString(reader, output, "Domain or address", cfg.CaddySite); err != nil {
		return cfg, false, err
	}
	cfg.PublicBaseURL = publicBaseURLForCaddySite(cfg.CaddySite)
	if cfg.InstallDir, err = promptString(reader, output, "Install directory", cfg.InstallDir); err != nil {
		return cfg, false, err
	}
	if cfg.ConfigFile, err = promptString(reader, output, "Config file", cfg.ConfigFile); err != nil {
		return cfg, false, err
	}
	if cfg.ServiceName, err = promptString(reader, output, "Systemd service name", cfg.ServiceName); err != nil {
		return cfg, false, err
	}
	cfg.ServiceName = normalizeSystemdServiceName(cfg.ServiceName)
	if cfg.AppHost, err = promptString(reader, output, "Internal listen host", cfg.AppHost); err != nil {
		return cfg, false, err
	}
	if cfg.AppPort, err = promptString(reader, output, "Internal listen port", cfg.AppPort); err != nil {
		return cfg, false, err
	}
	if cfg.InstallCaddy, err = promptBool(reader, output, "Install Caddy if missing", cfg.InstallCaddy); err != nil {
		return cfg, false, err
	}
	if cfg.ConfigureCaddy, err = promptBool(reader, output, "Write /etc/caddy/Caddyfile reverse proxy", cfg.ConfigureCaddy); err != nil {
		return cfg, false, err
	}
	cfg.StatePath = filepath.Join(defaultDataDir, "state.db")

	fmt.Fprintln(output)
	fmt.Fprintln(output, "Summary:")
	fmt.Fprintf(output, "  Binary:       %s/current/ag\n", cfg.InstallDir)
	fmt.Fprintf(output, "  Config:       %s\n", cfg.ConfigFile)
	fmt.Fprintf(output, "  Data:         %s\n", defaultDataDir)
	fmt.Fprintf(output, "  Service:      %s\n", cfg.ServiceName)
	fmt.Fprintf(output, "  App slots:    %s:%s / %s\n", cfg.AppHost, cfg.AppPort, cfg.StandbyPort)
	if cfg.ConfigureCaddy {
		fmt.Fprintf(output, "  Caddy:        %s -> %s:%s\n", cfg.CaddySite, cfg.AppHost, cfg.AppPort)
	}
	if cfg.PublicBaseURL != "" {
		fmt.Fprintf(output, "  Public URL:   %s\n", cfg.PublicBaseURL)
	}
	proceed, err := promptBool(reader, output, "Proceed with installation", true)
	return cfg, proceed, err
}

func defaultLinuxInstallConfig(options InstallWizardOptions) linuxInstallConfig {
	configFile := strings.TrimSpace(options.ConfigPath)
	if configFile == "" || configFile == "config.json" {
		configFile = filepath.Join(defaultConfigDir, "config.json")
	} else if absConfigFile, err := filepath.Abs(configFile); err == nil {
		configFile = absConfigFile
	}
	caddySite := defaultCaddySite(options.ConfigPath)
	return linuxInstallConfig{
		InstallDir:     defaultInstallDir,
		ConfigFile:     configFile,
		ServiceName:    defaultServiceName,
		AppHost:        defaultServiceHost,
		AppPort:        defaultServicePort,
		StandbyPort:    defaultStandbyPort,
		ActiveSlot:     slotBlue,
		StatePath:      filepath.Join(defaultDataDir, "state.db"),
		CaddySite:      caddySite,
		PublicBaseURL:  publicBaseURLForCaddySite(caddySite),
		InstallCaddy:   true,
		ConfigureCaddy: true,
	}
}

func defaultCaddySite(configPath string) string {
	if publicBaseURL := publicBaseURLFromExistingConfig(configPath); publicBaseURL != "" {
		if site := caddySiteForPublicBaseURL(publicBaseURL); site != "" {
			return site
		}
	}
	return ":80"
}

func publicBaseURLFromExistingConfig(configPath string) string {
	path := strings.TrimSpace(configPath)
	if path == "" || path == "config.json" {
		path = filepath.Join(defaultConfigDir, "config.json")
	} else if absPath, err := filepath.Abs(path); err == nil {
		path = absPath
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var payload struct {
		PublicBaseURL string `json:"public_base_url"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	normalized, err := normalizePublicBaseURL(payload.PublicBaseURL)
	if err != nil {
		return ""
	}
	return normalized
}

func publicBaseURLForCaddySite(site string) string {
	site = strings.TrimSpace(site)
	if site == "" || strings.HasPrefix(site, ":") {
		return ""
	}
	if strings.Contains(site, "://") {
		normalized, err := normalizePublicBaseURL(site)
		if err == nil {
			return normalized
		}
	}
	host := site
	if h, _, err := net.SplitHostPort(site); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" || net.ParseIP(host) != nil || strings.EqualFold(host, "localhost") {
		return ""
	}
	return "https://" + host
}

func caddySiteForPublicBaseURL(publicBaseURL string) string {
	normalized, err := normalizePublicBaseURL(publicBaseURL)
	if err != nil {
		return ""
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	host := parsed.Hostname()
	if net.ParseIP(host) != nil || strings.EqualFold(host, "localhost") {
		return ""
	}
	if port := parsed.Port(); port != "" && port != "443" && port != "80" {
		return net.JoinHostPort(host, port)
	}
	return host
}

func normalizePublicBaseURL(raw string) (string, error) {
	value := strings.TrimRight(strings.TrimSpace(raw), "/")
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid public base URL: %s", raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("invalid public base URL scheme: %s", parsed.Scheme)
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("public base URL must not include path, query, or fragment: %s", raw)
	}
	return value, nil
}

func promptString(reader *bufio.Reader, output io.Writer, label, fallback string) (string, error) {
	fmt.Fprintf(output, "%s [%s]: ", label, fallback)
	raw, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	value := strings.TrimSpace(raw)
	if value == "" {
		value = fallback
	}
	return value, nil
}

func promptBool(reader *bufio.Reader, output io.Writer, label string, fallback bool) (bool, error) {
	suffix := "Y/n"
	if !fallback {
		suffix = "y/N"
	}
	for {
		fmt.Fprintf(output, "%s [%s]: ", label, suffix)
		raw, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		value := strings.ToLower(strings.TrimSpace(raw))
		if value == "" {
			return fallback, nil
		}
		switch value {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		fmt.Fprintln(output, "Please answer yes or no.")
	}
}

func applyLinuxInstall(cfg linuxInstallConfig) error {
	cfg.ServiceName = normalizeSystemdServiceName(cfg.ServiceName)
	if err := validateLinuxInstallConfig(cfg); err != nil {
		return err
	}
	previousRecord, hasPreviousRecord, err := readLinuxInstallRecord(cfg.InstallDir)
	if err != nil {
		return err
	}
	installDirExisted, err := pathExists(cfg.InstallDir)
	if err != nil {
		return err
	}
	if !hasPreviousRecord {
		if installDirExisted {
			empty, err := dirIsEmpty(cfg.InstallDir)
			if err != nil {
				return err
			}
			if !empty {
				recoverable, err := recoverablePartialInstallDir(cfg.InstallDir)
				if err != nil {
					return err
				}
				if !recoverable {
					return fmt.Errorf("install directory already exists and is not managed by AI Gateway: %s", cfg.InstallDir)
				}
			}
		}
	}
	record := linuxInstallRecord{
		InstallDir:        cfg.InstallDir,
		InstallDirCreated: !installDirExisted,
		ConfigFile:        cfg.ConfigFile,
		ConfigDirCreated:  !hasPreviousRecord,
		ServiceName:       cfg.ServiceName,
		AppHost:           cfg.AppHost,
		AppPort:           cfg.AppPort,
		StandbyPort:       cfg.StandbyPort,
		ActiveSlot:        cfg.ActiveSlot,
		StatePath:         cfg.StatePath,
		StateDirCreated:   !hasPreviousRecord,
		CaddySite:         cfg.CaddySite,
		CaddyConfigured:   cfg.ConfigureCaddy,
	}
	if hasPreviousRecord {
		record.InstallDirCreated = previousRecord.InstallDirCreated
		if filepath.Clean(record.ConfigFile) == filepath.Clean(previousRecord.ConfigFile) {
			record.ConfigCreated = previousRecord.ConfigCreated
			record.ConfigDirCreated = previousRecord.ConfigDirCreated
		}
		if filepath.Clean(filepath.Dir(record.StatePath)) == filepath.Clean(filepath.Dir(previousRecord.StatePath)) {
			record.StateDirCreated = previousRecord.StateDirCreated
		}
		if previousRecord.CaddyConfigured && !cfg.ConfigureCaddy {
			record.CaddySite = previousRecord.CaddySite
			record.AppHost = previousRecord.AppHost
			record.AppPort = previousRecord.AppPort
		}
		record.CaddyConfigured = previousRecord.CaddyConfigured || cfg.ConfigureCaddy
		record.CaddyBackupPath = previousRecord.CaddyBackupPath
		record.CaddyInstalled = previousRecord.CaddyInstalled
		record.CurrentRelease = previousRecord.CurrentRelease
		record.PreviousRelease = previousRecord.PreviousRelease
		if strings.TrimSpace(previousRecord.ActiveSlot) != "" {
			record.ActiveSlot = previousRecord.ActiveSlot
		}
		if strings.TrimSpace(previousRecord.StandbyPort) != "" {
			record.StandbyPort = previousRecord.StandbyPort
		}
	}
	record.StandbyPort = repairedStandbyPort(record.AppPort, record.StandbyPort)
	cfg.ActiveSlot = record.ActiveSlot
	cfg.AppPort = record.AppPort
	cfg.StandbyPort = record.StandbyPort
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.InstallDir, 0o755); err != nil {
		return err
	}
	releaseID := newReleaseID()
	releaseDir := releaseDir(cfg.InstallDir, releaseID)
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		return err
	}
	dirs := []struct {
		path    string
		created *bool
	}{
		{path: filepath.Dir(cfg.ConfigFile), created: &record.ConfigDirCreated},
		{path: filepath.Dir(cfg.StatePath), created: &record.StateDirCreated},
	}
	for _, dir := range dirs {
		exists, err := pathExists(dir.path)
		if err != nil {
			return err
		}
		if !exists {
			*dir.created = true
		} else if !hasPreviousRecord {
			*dir.created = false
		}
		if err := os.MkdirAll(dir.path, 0o755); err != nil {
			return err
		}
	}
	if err := copyFile(executable, filepath.Join(releaseDir, "ag"), 0o755); err != nil {
		return err
	}
	if err := switchCurrentRelease(cfg.InstallDir, releaseID); err != nil {
		return err
	}
	record.PreviousRelease = record.CurrentRelease
	record.CurrentRelease = releaseID
	configCreated, err := writeConfigIfMissing(cfg)
	if err != nil {
		return err
	}
	record.ConfigCreated = record.ConfigCreated || configCreated
	if err := writeSystemdService(cfg); err != nil {
		return err
	}
	if err := writeLinuxInstallRecord(record); err != nil {
		return err
	}
	caddyInstalled := false
	if cfg.InstallCaddy {
		installed, err := installCaddyIfMissing()
		if err != nil {
			return err
		}
		caddyInstalled = installed
	}
	record.CaddyInstalled = record.CaddyInstalled || caddyInstalled
	if cfg.ConfigureCaddy {
		backupPath, err := writeCaddyfile(cfg, record.CaddyBackupPath, hasPreviousRecord && previousRecord.CaddyConfigured)
		if err != nil {
			return err
		}
		record.CaddyBackupPath = backupPath
	}
	if err := writeLinuxInstallRecord(record); err != nil {
		return err
	}
	if err := runCommand("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := runCommand("systemctl", "enable", slotServiceName(cfg.ServiceName, cfg.ActiveSlot)); err != nil {
		return err
	}
	if err := runCommand("systemctl", "restart", slotServiceName(cfg.ServiceName, cfg.ActiveSlot)); err != nil {
		return err
	}
	if cfg.ConfigureCaddy {
		_ = runCommand("systemctl", "enable", "caddy")
		if err := runCommand("systemctl", "reload", "caddy"); err != nil {
			if restartErr := runCommand("systemctl", "restart", "caddy"); restartErr != nil {
				return err
			}
		}
	}
	return nil
}

func validateLinuxInstallConfig(cfg linuxInstallConfig) error {
	if !filepath.IsAbs(cfg.InstallDir) {
		return fmt.Errorf("install directory must be absolute: %s", cfg.InstallDir)
	}
	if !filepath.IsAbs(cfg.ConfigFile) {
		return fmt.Errorf("config file must be absolute: %s", cfg.ConfigFile)
	}
	if !validSystemdServiceName(cfg.ServiceName) {
		return fmt.Errorf("invalid systemd service name: %s", cfg.ServiceName)
	}
	if cfg.AppHost == "" || strings.ContainsAny(cfg.AppHost, "\r\n") {
		return fmt.Errorf("invalid internal listen host: %s", cfg.AppHost)
	}
	port, err := strconv.Atoi(cfg.AppPort)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid internal listen port: %s", cfg.AppPort)
	}
	standbyPort, err := strconv.Atoi(cfg.StandbyPort)
	if err != nil || standbyPort < 1 || standbyPort > 65535 || standbyPort == port {
		return fmt.Errorf("invalid standby listen port: %s", cfg.StandbyPort)
	}
	if normalizeSlot(cfg.ActiveSlot) == "" {
		return fmt.Errorf("invalid active slot: %s", cfg.ActiveSlot)
	}
	if cfg.ConfigureCaddy && (strings.TrimSpace(cfg.CaddySite) == "" || strings.ContainsAny(cfg.CaddySite, "{}\r\n")) {
		return fmt.Errorf("invalid Caddy site: %s", cfg.CaddySite)
	}
	if strings.TrimSpace(cfg.PublicBaseURL) != "" {
		if _, err := normalizePublicBaseURL(cfg.PublicBaseURL); err != nil {
			return err
		}
	}
	return nil
}

func repairedStandbyPort(appPort, standbyPort string) string {
	appPort = strings.TrimSpace(appPort)
	standbyPort = strings.TrimSpace(standbyPort)
	if standbyPort != "" && standbyPort != appPort {
		return standbyPort
	}
	if appPort != defaultStandbyPort {
		return defaultStandbyPort
	}
	return defaultServicePort
}

func validSystemdServiceName(name string) bool {
	name = normalizeSystemdServiceName(name)
	if name == "" || strings.Contains(name, "/") || strings.ContainsAny(name, "\r\n") {
		return false
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		switch r {
		case '.', '_', '-', '@':
			continue
		default:
			return false
		}
	}
	return true
}

func normalizeSystemdServiceName(name string) string {
	return strings.TrimSuffix(strings.TrimSpace(name), ".service")
}

func copyFile(src, dst string, mode os.FileMode) error {
	input, err := os.Open(src)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(dst+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
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
	if err := os.Chmod(dst+".tmp", mode); err != nil {
		return err
	}
	return os.Rename(dst+".tmp", dst)
}

func writeConfigIfMissing(cfg linuxInstallConfig) (bool, error) {
	if _, err := os.Stat(cfg.ConfigFile); err == nil {
		return false, nil
	} else if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	payload := map[string]any{
		"host":                             cfg.AppHost,
		"port":                             cfg.AppPort,
		"api_key":                          "",
		"audit_limit":                      512,
		"gateway_audit":                    false,
		"gateway_audit_errors":             false,
		"gateway_audit_retention_days":     1,
		"max_in_flight":                    100000,
		"upstream_max_idle_conns":          100000,
		"upstream_max_idle_conns_per_host": 100000,
		"upstream_max_conns_per_host":      0,
		"database_backend":                 "sqlite",
		"state_path":                       cfg.StatePath,
		"master_key":                       "",
		"public_base_url":                  cfg.PublicBaseURL,
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return false, err
	}
	raw = append(raw, '\n')
	return true, os.WriteFile(cfg.ConfigFile, raw, 0o600)
}

func writeSystemdService(cfg linuxInstallConfig) error {
	releasePath := filepath.Join(cfg.InstallDir, "current", "ag")
	return writeSystemdSlotService(cfg, cfg.ActiveSlot, cfg.AppPort, releasePath)
}

func writeSystemdSlotService(cfg linuxInstallConfig, slot, port, executable string) error {
	slot = normalizeSlot(slot)
	if slot == "" {
		return fmt.Errorf("invalid service slot: %s", slot)
	}
	unit := fmt.Sprintf(`[Unit]
Description=AI Gateway
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%s
ExecStart=%s -config %s -host %s -port %s
Restart=always
RestartSec=3
LimitNOFILE=1048576
KillSignal=SIGTERM
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
`, systemdEscape(cfg.InstallDir), systemdEscape(executable), systemdEscape(cfg.ConfigFile), systemdEscape(cfg.AppHost), systemdEscape(port))
	return os.WriteFile(serviceUnitPath(slotServiceName(cfg.ServiceName, slot)), []byte(unit), 0o644)
}

func slotServiceName(serviceName, slot string) string {
	base := normalizeSystemdServiceName(serviceName)
	slot = normalizeSlot(slot)
	if slot == "" {
		slot = slotBlue
	}
	return base + "-" + slot
}

func normalizeSlot(slot string) string {
	switch strings.ToLower(strings.TrimSpace(slot)) {
	case slotBlue:
		return slotBlue
	case slotGreen:
		return slotGreen
	default:
		return ""
	}
}

func inactiveSlot(active string) string {
	if normalizeSlot(active) == slotGreen {
		return slotBlue
	}
	return slotGreen
}

func newReleaseID() string {
	return time.Now().UTC().Format("20060102-150405.000000000")
}

func releaseDir(installDir, releaseID string) string {
	return filepath.Join(installDir, "releases", releaseID)
}

func switchCurrentRelease(installDir, releaseID string) error {
	target := releaseDir(installDir, releaseID)
	link := filepath.Join(installDir, "current")
	tmp := link + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func systemdEscape(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `%%`)
	value = strings.ReplaceAll(value, " ", `\x20`)
	value = strings.ReplaceAll(value, "\t", `\x09`)
	value = strings.ReplaceAll(value, "\n", `\x0a`)
	value = strings.ReplaceAll(value, "\r", `\x0d`)
	return value
}

func writeCaddyfile(cfg linuxInstallConfig, existingBackupPath string, alreadyManaged bool) (string, error) {
	if err := os.MkdirAll("/etc/caddy", 0o755); err != nil {
		return "", err
	}
	caddyfile := "/etc/caddy/Caddyfile"
	backupPath := strings.TrimSpace(existingBackupPath)
	if _, err := os.Stat(caddyfile); err == nil {
		if backupPath == "" && !alreadyManaged {
			backupPath = caddyfile + ".bak." + strconv.FormatInt(timeNowUnix(), 10)
			if err := copyFile(caddyfile, backupPath, 0o644); err != nil {
				return "", err
			}
		}
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	body := caddyfileBody(cfg)
	if err := os.WriteFile(caddyfile, []byte(body), 0o644); err != nil {
		return "", err
	}
	return backupPath, nil
}

func caddyfileBody(cfg linuxInstallConfig) string {
	return fmt.Sprintf(`%s {
    encode gzip

    reverse_proxy %s:%s {
        transport http {
            dial_timeout 30s
            read_timeout 0
            write_timeout 0
        }
    }
}
`, cfg.CaddySite, cfg.AppHost, cfg.AppPort)
}

func installCaddyIfMissing() (bool, error) {
	return installCaddyIfMissingWith(exec.LookPath, commandExists, runCommand, runShell)
}

func installCaddyIfMissingWith(
	lookPath func(string) (string, error),
	commandExists func(string) bool,
	runCommand func(string, ...string) error,
	runShell func(string) error,
) (bool, error) {
	if _, err := lookPath("caddy"); err == nil {
		return false, nil
	}
	switch {
	case commandExists("apt-get"):
		if err := runCommand("apt-get", "update"); err != nil {
			return false, err
		}
		if err := runCommand("apt-get", "install", "-y", "debian-keyring", "debian-archive-keyring", "apt-transport-https", "curl", "gpg"); err != nil {
			return false, err
		}
		if err := runCommand("install", "-d", "-m", "0755", "/usr/share/keyrings"); err != nil {
			return false, err
		}
		if err := runShell("curl -fsSL https://dl.cloudsmith.io/public/caddy/stable/gpg.key | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg"); err != nil {
			return false, err
		}
		if err := runShell("curl -fsSL https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt >/etc/apt/sources.list.d/caddy-stable.list"); err != nil {
			return false, err
		}
		if err := runCommand("apt-get", "update"); err != nil {
			return false, err
		}
		return true, runCommand("apt-get", "install", "-y", "caddy")
	case commandExists("dnf"):
		if err := installDNFCoprPlugin(runCommand); err != nil {
			return false, err
		}
		if err := runCommand("dnf", "copr", "enable", "-y", "@caddy/caddy"); err != nil {
			return false, err
		}
		return true, runCommand("dnf", "install", "-y", "caddy")
	case commandExists("yum"):
		if err := runCommand("yum", "install", "-y", "yum-plugin-copr"); err != nil {
			return false, err
		}
		if err := runCommand("yum", "copr", "enable", "-y", "@caddy/caddy"); err != nil {
			return false, err
		}
		return true, runCommand("yum", "install", "-y", "caddy")
	case commandExists("apk"):
		return true, runCommand("apk", "add", "--no-cache", "caddy")
	default:
		return false, errors.New("no supported package manager found; install Caddy manually or answer no to Caddy installation")
	}
}

func installDNFCoprPlugin(runCommand func(string, ...string) error) error {
	var lastErr error
	for _, pkg := range []string{"dnf5-plugins", "dnf-plugins-core", "dnf-command(copr)"} {
		if err := runCommand("dnf", "install", "-y", pkg); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	if lastErr == nil {
		lastErr = errors.New("dnf copr plugin install failed")
	}
	return fmt.Errorf("install dnf copr plugin: %w", lastErr)
}

func installRecordPath(installDir string) string {
	return filepath.Join(installDir, installManifest)
}

func pathExists(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return true, nil
	} else if os.IsNotExist(err) {
		return false, nil
	} else {
		return false, err
	}
}

func dirIsEmpty(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

func recoverablePartialInstallDir(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	if len(entries) == 0 {
		return true, nil
	}
	tmpPrefix := "." + installManifest + ".tmp."
	for _, entry := range entries {
		switch entry.Name() {
		case "current", "releases", "ag":
			continue
		default:
			if strings.HasPrefix(entry.Name(), tmpPrefix) {
				continue
			}
			return false, nil
		}
	}
	return true, nil
}

func writeLinuxInstallRecord(record linuxInstallRecord) error {
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	path := installRecordPath(record.InstallDir)
	tmp := filepath.Join(record.InstallDir, "."+installManifest+".tmp."+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func runShell(script string) error {
	return runCommand("sh", "-c", script)
}

func timeNowUnix() int64 {
	return time.Now().Unix()
}
