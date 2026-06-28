package app

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/backup"
	"github.com/32ns/ai-gateway/internal/config"
)

type CommandOptions struct {
	Stdout io.Writer
	Now    func() time.Time
}

func RunBackupCommand(args []string) error {
	return RunBackupCommandWithOptions(args, CommandOptions{})
}

func RunBackupCommandWithOptions(args []string, options CommandOptions) error {
	stdout := options.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	configPath := flags.String("config", config.DefaultPath, "config file path")
	outPath := flags.String("out", "", "backup output path")
	dataSets := flags.String("data", "", "comma-separated logical data types to back up")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := rejectUnexpectedArgs(flags); err != nil {
		return err
	}
	cfg, err := config.LoadWithOptions(config.Options{Path: *configPath})
	if err != nil {
		return err
	}
	out := *outPath
	if out == "" {
		out = "ai-gateway-" + now().UTC().Format("20060102-150405") + ".agbak"
	}
	manifest, err := backup.Create(out, backup.Options{
		ConfigPath:      AbsoluteConfigPath(*configPath),
		StatePath:       cfg.StatePath,
		DatabaseBackend: cfg.DatabaseBackend,
		PostgresDSN:     cfg.PostgresDSN,
		DataSets:        splitDataSetsFlag(*dataSets),
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Backup created: %s\n", out)
	fmt.Fprintf(stdout, "Includes: config=%t database=%t data=%t\n", manifest.Includes.Config, manifest.Includes.Database, manifest.Includes.Data)
	return nil
}

func RunRestoreCommand(args []string) error {
	return RunRestoreCommandWithOptions(args, CommandOptions{})
}

func RunRestoreCommandWithOptions(args []string, options CommandOptions) error {
	stdout := options.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}

	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	configPath := flags.String("config", config.DefaultPath, "config file path")
	fromPath := flags.String("from", "", "backup input path")
	preRestoreDir := flags.String("pre-restore-dir", "", "directory for automatic pre-restore backup")
	dataSets := flags.String("data", "", "comma-separated logical data types to restore")
	sourceMasterKey := flags.String("source-master-key", "", "master_key from the encrypted backup source")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := rejectUnexpectedArgs(flags); err != nil {
		return err
	}
	if *fromPath == "" {
		return fmt.Errorf("restore requires --from backup.agbak")
	}
	cfg, err := config.LoadWithOptions(config.Options{Path: *configPath})
	if err != nil {
		return err
	}
	dir := *preRestoreDir
	if dir == "" {
		dir = filepath.Dir(*fromPath)
	}
	preRestorePath, err := backup.Restore(*fromPath, backup.Options{
		ConfigPath:      AbsoluteConfigPath(*configPath),
		StatePath:       cfg.StatePath,
		DatabaseBackend: cfg.DatabaseBackend,
		PostgresDSN:     cfg.PostgresDSN,
		DataSets:        splitDataSetsFlag(*dataSets),
		SourceMasterKey: *sourceMasterKey,
		TargetMasterKey: cfg.MasterKey,
	}, dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Restore completed from: %s\n", *fromPath)
	fmt.Fprintf(stdout, "Pre-restore backup: %s\n", preRestorePath)
	fmt.Fprintln(stdout, "Command-line restore cannot reload a running ai-gateway process; use the admin console restore for live reload, or restart any running service manually.")
	return nil
}

func splitDataSetsFlag(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func rejectUnexpectedArgs(flags *flag.FlagSet) error {
	if flags.NArg() == 0 {
		return nil
	}
	return fmt.Errorf("%s received unexpected argument: %s", flags.Name(), flags.Arg(0))
}
