//go:build linux

package app

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/backup"
)

func RunServiceCommand(command string, args []string) error {
	switch command {
	case "install":
		return RunInstallCommand(args)
	case "uninstall":
		return RunUninstallCommand(args)
	case "upgrade":
		return RunUpgradeCommand(args)
	case "rollback":
		return RunRollbackCommand(args)
	case "reinstall":
		return RunUpgradeCommand(args)
	case "reboot", "restart":
		return runServiceActionCommand(command, args, "restart")
	case "stop":
		return runServiceActionCommand(command, args, "stop")
	default:
		return fmt.Errorf("unknown service command: %s", command)
	}
}

func RunInstallCommand(args []string) error {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	configPath := flags.String("config", "", "config file path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := rejectUnexpectedArgs(flags); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("install requires root; run sudo ./ag install")
	}
	cfg, ok, err := promptLinuxInstall(bufio.NewReader(os.Stdin), os.Stdout, InstallWizardOptions{
		ConfigPath: *configPath,
	})
	if err != nil || !ok {
		return err
	}
	if err := applyLinuxInstall(cfg); err != nil {
		return err
	}
	printLinuxInstallSummary(cfg)
	return nil
}

func RunUninstallCommand(args []string) error {
	flags := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	serviceName := flags.String("service", defaultServiceName, "systemd service name")
	installDir := flags.String("install-dir", defaultInstallDir, "installed binary and data directory")
	configDir := flags.String("config-dir", defaultConfigDir, "configuration directory")
	keepFiles := flags.Bool("keep-files", false, "keep install and config directories")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := rejectUnexpectedArgs(flags); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("uninstall requires root; run sudo ./ag uninstall")
	}

	record, hasRecord, err := readLinuxInstallRecord(*installDir)
	if err != nil {
		return err
	}
	if hasRecord && filepath.Clean(record.InstallDir) != filepath.Clean(*installDir) {
		return fmt.Errorf("install record directory mismatch: %s", record.InstallDir)
	}
	if !safeInstallDir(*installDir) {
		return fmt.Errorf("invalid install directory: %s", *installDir)
	}
	name := normalizeSystemdServiceName(*serviceName)
	if hasRecord && name == defaultServiceName && strings.TrimSpace(record.ServiceName) != "" {
		name = normalizeSystemdServiceName(record.ServiceName)
	}
	if !validSystemdServiceName(name) {
		return fmt.Errorf("invalid systemd service name: %s", *serviceName)
	}
	for _, slot := range []string{slotBlue, slotGreen} {
		slotName := slotServiceName(name, slot)
		_ = runCommand("systemctl", "stop", slotName)
		_ = runCommand("systemctl", "disable", slotName)
		if err := os.Remove(serviceUnitPath(slotName)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	_ = runCommand("systemctl", "stop", name)
	_ = runCommand("systemctl", "disable", name)
	if err := os.Remove(serviceUnitPath(name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := runCommand("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if !*keepFiles {
		if hasRecord {
			if err := cleanLinuxInstallRecord(record); err != nil {
				return err
			}
		} else {
			for _, path := range []string{*installDir, *configDir} {
				if err := removeManagedPath(path); err != nil {
					return err
				}
			}
		}
	}
	fmt.Fprintf(os.Stdout, "Service uninstalled: %s\n", name)
	return nil
}

func RunReinstallCommand(args []string) error {
	return RunUpgradeCommand(args)
}

func RunUpgradeCommand(args []string) error {
	flags := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	serviceName := flags.String("service", defaultServiceName, "systemd service name")
	installDir := flags.String("install-dir", defaultInstallDir, "installed binary and release directory")
	timeout := flags.Duration("timeout", 30*time.Second, "readiness timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := rejectUnexpectedArgs(flags); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("upgrade requires root; run sudo ./ag upgrade")
	}
	record, hasRecord, err := readLinuxInstallRecord(*installDir)
	if err != nil {
		return err
	}
	if !hasRecord {
		return fmt.Errorf("install record not found: %s", installRecordPath(*installDir))
	}
	if name := normalizeSystemdServiceName(*serviceName); name != defaultServiceName || strings.TrimSpace(record.ServiceName) == "" {
		record.ServiceName = name
	}
	return switchRelease(record, *timeout, "")
}

func RunRollbackCommand(args []string) error {
	flags := flag.NewFlagSet("rollback", flag.ContinueOnError)
	serviceName := flags.String("service", defaultServiceName, "systemd service name")
	installDir := flags.String("install-dir", defaultInstallDir, "installed binary and release directory")
	timeout := flags.Duration("timeout", 30*time.Second, "readiness timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := rejectUnexpectedArgs(flags); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("rollback requires root; run sudo ./ag rollback")
	}
	record, hasRecord, err := readLinuxInstallRecord(*installDir)
	if err != nil {
		return err
	}
	if !hasRecord {
		return fmt.Errorf("install record not found: %s", installRecordPath(*installDir))
	}
	if strings.TrimSpace(record.PreviousRelease) == "" {
		return fmt.Errorf("previous release is not recorded")
	}
	if name := normalizeSystemdServiceName(*serviceName); name != defaultServiceName || strings.TrimSpace(record.ServiceName) == "" {
		record.ServiceName = name
	}
	return switchRelease(record, *timeout, record.PreviousRelease)
}

func switchRelease(record linuxInstallRecord, timeout time.Duration, rollbackRelease string) error {
	if !record.CaddyConfigured {
		return fmt.Errorf("zero-downtime upgrade requires managed Caddy reverse proxy")
	}
	if strings.TrimSpace(record.ServiceName) == "" {
		record.ServiceName = defaultServiceName
	}
	if strings.TrimSpace(record.AppHost) == "" {
		record.AppHost = defaultServiceHost
	}
	activeSlot := normalizeSlot(record.ActiveSlot)
	if activeSlot == "" {
		return fmt.Errorf("active release slot is not recorded")
	}
	nextSlot := inactiveSlot(activeSlot)
	nextPort := strings.TrimSpace(record.StandbyPort)
	if nextPort == "" {
		nextPort = defaultStandbyPort
	}
	currentPort := strings.TrimSpace(record.AppPort)
	if currentPort == "" {
		currentPort = defaultServicePort
	}
	if nextPort == currentPort {
		nextPort = repairedStandbyPort(currentPort, nextPort)
	}
	releaseID := strings.TrimSpace(rollbackRelease)
	if releaseID == "" {
		if strings.TrimSpace(record.CurrentRelease) == "" {
			return fmt.Errorf("current release is not recorded")
		}
		var err error
		releaseID, err = installExecutableRelease(record.InstallDir)
		if err != nil {
			return err
		}
		if err := createPreUpgradeBackup(record, releaseID); err != nil {
			return err
		}
	} else if _, err := os.Stat(filepath.Join(releaseDir(record.InstallDir, releaseID), "ag")); err != nil {
		return fmt.Errorf("rollback release %q is not usable: %w", releaseID, err)
	}
	candidate := linuxInstallConfig{
		InstallDir:     record.InstallDir,
		ConfigFile:     record.ConfigFile,
		ServiceName:    record.ServiceName,
		AppHost:        record.AppHost,
		AppPort:        nextPort,
		StandbyPort:    currentPort,
		ActiveSlot:     nextSlot,
		StatePath:      record.StatePath,
		CaddySite:      record.CaddySite,
		ConfigureCaddy: record.CaddyConfigured,
	}
	if err := writeSystemdSlotService(candidate, nextSlot, nextPort, filepath.Join(releaseDir(record.InstallDir, releaseID), "ag")); err != nil {
		return err
	}
	if err := runCommand("systemctl", "daemon-reload"); err != nil {
		return err
	}
	nextService := slotServiceName(record.ServiceName, nextSlot)
	if err := runCommand("systemctl", "restart", nextService); err != nil {
		return err
	}
	if err := waitReady(record.AppHost, nextPort, timeout); err != nil {
		_ = runCommand("systemctl", "stop", nextService)
		return err
	}
	if err := runCommand("systemctl", "enable", nextService); err != nil {
		_ = runCommand("systemctl", "stop", nextService)
		return err
	}
	if record.CaddyConfigured {
		caddyCfg := candidate
		caddyCfg.AppPort = nextPort
		if _, err := writeCaddyfile(caddyCfg, record.CaddyBackupPath, true); err != nil {
			_ = runCommand("systemctl", "stop", nextService)
			return err
		}
		if err := runCommand("systemctl", "reload", "caddy"); err != nil {
			_ = runCommand("systemctl", "stop", nextService)
			return err
		}
	}
	oldService := slotServiceName(record.ServiceName, activeSlot)
	if err := switchCurrentRelease(record.InstallDir, releaseID); err != nil {
		return err
	}
	nextRecord := record
	nextRecord.ActiveSlot = nextSlot
	nextRecord.AppPort = nextPort
	nextRecord.StandbyPort = currentPort
	nextRecord.PreviousRelease = record.CurrentRelease
	nextRecord.CurrentRelease = releaseID
	if err := writeLinuxInstallRecord(nextRecord); err != nil {
		return err
	}
	_ = runCommand("systemctl", "stop", oldService)
	_ = runCommand("systemctl", "disable", oldService)
	fmt.Fprintf(os.Stdout, "Service upgraded: %s -> %s (%s)\n", oldService, nextService, releaseID)
	return nil
}

func installExecutableRelease(installDir string) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}
	releaseID := newReleaseID()
	targetDir := releaseDir(installDir, releaseID)
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", err
	}
	if err := copyFile(executable, filepath.Join(targetDir, "ag"), 0o755); err != nil {
		return "", err
	}
	return releaseID, nil
}

func createPreUpgradeBackup(record linuxInstallRecord, releaseID string) error {
	backupDir := filepath.Join(defaultDataDir, "backups")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return err
	}
	databaseBackend, postgresDSN, err := databaseBackendFromConfigFile(record.ConfigFile)
	if err != nil {
		return err
	}
	_, err = backup.Create(filepath.Join(backupDir, "pre-upgrade-"+releaseID+".agbak"), backup.Options{
		ConfigPath:      record.ConfigFile,
		StatePath:       record.StatePath,
		DatabaseBackend: databaseBackend,
		PostgresDSN:     postgresDSN,
		AppVersion:      releaseID,
	})
	return err
}

func databaseBackendFromConfigFile(path string) (string, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	var payload struct {
		DatabaseBackend string `json:"database_backend"`
		PostgresDSN     string `json:"postgres_dsn"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", "", err
	}
	return payload.DatabaseBackend, payload.PostgresDSN, nil
}

func waitReady(host, port string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	url := "http://" + net.JoinHostPort(host, port) + "/readyz"
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			lastErr = fmt.Errorf("readiness status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout")
	}
	return fmt.Errorf("service did not become ready at %s: %w", url, lastErr)
}

func runServiceActionCommand(command string, args []string, action string) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	serviceName := flags.String("service", defaultServiceName, "systemd service name")
	installDir := flags.String("install-dir", defaultInstallDir, "installed binary and release directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := rejectUnexpectedArgs(flags); err != nil {
		return err
	}
	name := normalizeSystemdServiceName(*serviceName)
	if !validSystemdServiceName(name) {
		return fmt.Errorf("invalid systemd service name: %s", *serviceName)
	}
	if record, ok, err := readLinuxInstallRecord(*installDir); err != nil {
		return err
	} else if ok {
		if name == defaultServiceName && strings.TrimSpace(record.ServiceName) != "" {
			name = normalizeSystemdServiceName(record.ServiceName)
		}
		name = slotServiceName(name, record.ActiveSlot)
	}
	if err := runCommand("systemctl", action, name); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Service %s: %s\n", action, name)
	return nil
}

func serviceUnitPath(serviceName string) string {
	return filepath.Join("/etc/systemd/system", normalizeSystemdServiceName(serviceName)+".service")
}

func removeManagedPath(path string) error {
	cleaned := filepath.Clean(path)
	switch cleaned {
	case defaultInstallDir, defaultConfigDir:
		return os.RemoveAll(cleaned)
	default:
		return fmt.Errorf("refusing to remove unmanaged path: %s", path)
	}
}

func readLinuxInstallRecord(installDir string) (linuxInstallRecord, bool, error) {
	raw, err := os.ReadFile(installRecordPath(installDir))
	if os.IsNotExist(err) {
		return linuxInstallRecord{}, false, nil
	}
	if err != nil {
		return linuxInstallRecord{}, false, err
	}
	var record linuxInstallRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return linuxInstallRecord{}, false, err
	}
	if err := validateLinuxInstallRecord(record); err != nil {
		return linuxInstallRecord{}, false, err
	}
	return record, true, nil
}

func validateLinuxInstallRecord(record linuxInstallRecord) error {
	if !filepath.IsAbs(record.InstallDir) || record.InstallDir == "/" {
		return fmt.Errorf("invalid install record install directory: %s", record.InstallDir)
	}
	if strings.TrimSpace(record.ConfigFile) != "" && !filepath.IsAbs(record.ConfigFile) {
		return fmt.Errorf("invalid install record config file: %s", record.ConfigFile)
	}
	if strings.TrimSpace(record.ServiceName) != "" && !validSystemdServiceName(record.ServiceName) {
		return fmt.Errorf("invalid install record service name: %s", record.ServiceName)
	}
	if strings.TrimSpace(record.ActiveSlot) != "" && normalizeSlot(record.ActiveSlot) == "" {
		return fmt.Errorf("invalid install record active slot: %s", record.ActiveSlot)
	}
	if strings.TrimSpace(record.AppPort) != "" {
		if err := validateInstallRecordPort(record.AppPort); err != nil {
			return fmt.Errorf("invalid install record app port: %s", record.AppPort)
		}
	}
	if strings.TrimSpace(record.StandbyPort) != "" {
		if err := validateInstallRecordPort(record.StandbyPort); err != nil {
			return fmt.Errorf("invalid install record standby port: %s", record.StandbyPort)
		}
	}
	if strings.TrimSpace(record.StatePath) != "" && !managedInstallOrDataPath(record.StatePath, record.InstallDir) {
		return fmt.Errorf("invalid install record state path: %s", record.StatePath)
	}
	if strings.TrimSpace(record.CaddyBackupPath) != "" && !strings.HasPrefix(filepath.Clean(record.CaddyBackupPath), "/etc/caddy/Caddyfile.bak.") {
		return fmt.Errorf("invalid install record Caddy backup path: %s", record.CaddyBackupPath)
	}
	return nil
}

func validateInstallRecordPort(port string) error {
	value := 0
	for _, r := range strings.TrimSpace(port) {
		if r < '0' || r > '9' {
			return fmt.Errorf("invalid port")
		}
		value = value*10 + int(r-'0')
	}
	if value < 1 || value > 65535 {
		return fmt.Errorf("invalid port")
	}
	return nil
}

func managedInstallOrDataPath(path, installDir string) bool {
	return pathWithin(path, defaultDataDir) || pathWithin(path, installDir)
}

func pathWithin(path, parent string) bool {
	cleanedPath := filepath.Clean(path)
	cleanedParent := filepath.Clean(parent)
	if !filepath.IsAbs(cleanedPath) || !filepath.IsAbs(cleanedParent) {
		return false
	}
	rel, err := filepath.Rel(cleanedParent, cleanedPath)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func cleanLinuxInstallRecord(record linuxInstallRecord) error {
	if err := restoreOrRemoveCaddyfile(record); err != nil {
		return err
	}
	if record.CaddyInstalled {
		removeCaddyConfigDir := strings.TrimSpace(record.CaddyBackupPath) == ""
		if err := uninstallCaddyPackage(removeCaddyConfigDir); err != nil {
			return err
		}
	}
	if record.ConfigCreated && strings.TrimSpace(record.ConfigFile) != "" {
		if err := os.Remove(record.ConfigFile); err != nil && !os.IsNotExist(err) {
			return err
		}
		if record.ConfigDirCreated {
			_ = os.Remove(filepath.Dir(record.ConfigFile))
		}
	}
	return cleanRecordedInstallDir(record)
}

func restoreOrRemoveCaddyfile(record linuxInstallRecord) error {
	if !record.CaddyConfigured {
		return nil
	}
	caddyfile := "/etc/caddy/Caddyfile"
	if strings.TrimSpace(record.CaddyBackupPath) != "" {
		if _, err := os.Stat(record.CaddyBackupPath); err == nil {
			if err := copyFile(record.CaddyBackupPath, caddyfile, 0o644); err != nil {
				return err
			}
			if err := os.Remove(record.CaddyBackupPath); err != nil && !os.IsNotExist(err) {
				return err
			}
			if !record.CaddyInstalled {
				_ = runCommand("systemctl", "reload", "caddy")
			}
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	raw, err := os.ReadFile(caddyfile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	expected := caddyfileBody(linuxInstallConfig{
		CaddySite: record.CaddySite,
		AppHost:   record.AppHost,
		AppPort:   record.AppPort,
	})
	if string(raw) != expected {
		return fmt.Errorf("refusing to remove modified Caddyfile: %s", caddyfile)
	}
	if err := os.Remove(caddyfile); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove("/etc/caddy")
	if !record.CaddyInstalled {
		_ = runCommand("systemctl", "reload", "caddy")
	}
	return nil
}

func uninstallCaddyPackage(removeConfigDir bool) error {
	_ = runCommand("systemctl", "stop", "caddy")
	_ = runCommand("systemctl", "disable", "caddy")
	switch {
	case commandExists("apt-get"):
		if err := runCommand("apt-get", "remove", "-y", "caddy"); err != nil {
			return err
		}
		_ = os.Remove("/etc/apt/sources.list.d/caddy-stable.list")
		_ = os.Remove("/usr/share/keyrings/caddy-stable-archive-keyring.gpg")
		_ = runCommand("apt-get", "update")
	case commandExists("dnf"):
		if err := runCommand("dnf", "remove", "-y", "caddy"); err != nil {
			return err
		}
	case commandExists("yum"):
		if err := runCommand("yum", "remove", "-y", "caddy"); err != nil {
			return err
		}
	case commandExists("apk"):
		if err := runCommand("apk", "del", "caddy"); err != nil {
			return err
		}
	default:
		return fmt.Errorf("no supported package manager found to uninstall Caddy")
	}
	if removeConfigDir {
		_ = os.RemoveAll("/etc/caddy")
	}
	return nil
}

func cleanRecordedInstallDir(record linuxInstallRecord) error {
	cleaned := filepath.Clean(record.InstallDir)
	if !safeInstallDir(cleaned) {
		return fmt.Errorf("refusing to remove unsafe install directory: %s", record.InstallDir)
	}
	if _, err := os.Stat(installRecordPath(cleaned)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("refusing to remove install directory without install record: %s", record.InstallDir)
		}
		return err
	}
	if err := cleanRecordedDataDirs(record, cleaned); err != nil {
		return err
	}
	if record.InstallDirCreated {
		return os.RemoveAll(cleaned)
	}
	for _, path := range []string{
		filepath.Join(cleaned, "current"),
		filepath.Join(cleaned, "ag"),
		installRecordPath(cleaned),
	} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	_ = os.RemoveAll(filepath.Join(cleaned, "releases"))
	return nil
}

func cleanRecordedDataDirs(record linuxInstallRecord, installDir string) error {
	stateDir := ""
	if strings.TrimSpace(record.StatePath) != "" {
		stateDir = filepath.Dir(record.StatePath)
	}
	for _, dir := range []struct {
		path    string
		created bool
	}{
		{path: stateDir, created: record.StateDirCreated},
	} {
		if dir.created && strings.TrimSpace(dir.path) != "" {
			cleaned := filepath.Clean(dir.path)
			if cleaned == installDir {
				continue
			}
			if pathWithin(installDir, cleaned) {
				return fmt.Errorf("refusing to remove data directory containing install directory: %s", dir.path)
			}
			if !managedInstallOrDataPath(cleaned, installDir) {
				return fmt.Errorf("refusing to remove unmanaged data directory: %s", dir.path)
			}
			if err := os.RemoveAll(dir.path); err != nil {
				return err
			}
		}
	}
	return nil
}

func safeInstallDir(path string) bool {
	cleaned := filepath.Clean(path)
	return filepath.IsAbs(cleaned) && cleaned != "/" && cleaned != "/etc" && cleaned != "/opt" && cleaned != "/usr" && cleaned != "/var"
}
