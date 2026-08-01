package api

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/daemonbinary"
	"github.com/MumuTW/looper/internal/storage"
	"github.com/MumuTW/looper/internal/upgradebackup"
)

func (h *Handler) createUpgradeBackup(ctx context.Context) (upgradebackup.Result, error) {
	if h == nil || h.context.Runtime == nil {
		return upgradebackup.Result{}, fmt.Errorf("runtime is unavailable")
	}
	cfg, metadata := h.upgradeConfigAndMetadata()
	if cfg.Tools.LooperPath == nil || strings.TrimSpace(*cfg.Tools.LooperPath) == "" {
		return upgradebackup.Result{}, fmt.Errorf("tools.looperPath is required to create a matching upgrade backup")
	}
	if strings.TrimSpace(metadata.ConfigPath) == "" {
		return upgradebackup.Result{}, fmt.Errorf("configured config path is unavailable")
	}
	if metadata.LastError != nil && strings.TrimSpace(*metadata.LastError) != "" {
		return upgradebackup.Result{}, fmt.Errorf("refusing upgrade backup while a rejected config generation is pending: %s", strings.TrimSpace(*metadata.LastError))
	}
	if len(metadata.RejectedPaths) > 0 {
		return upgradebackup.Result{}, fmt.Errorf("refusing upgrade backup while config paths remain rejected: %s", strings.Join(metadata.RejectedPaths, ", "))
	}
	if err := refuseSwappedDaemonBinary(h); err != nil {
		return upgradebackup.Result{}, err
	}
	cliPath := strings.TrimSpace(*cfg.Tools.LooperPath)
	if err := requireExecutableFile(cliPath, "CLI binary"); err != nil {
		return upgradebackup.Result{}, err
	}
	services := h.context.Runtime.Services()
	if services.Coordinator == nil {
		return upgradebackup.Result{}, fmt.Errorf("storage coordinator is unavailable")
	}
	if cfg.Storage.BackupDir == nil || strings.TrimSpace(*cfg.Storage.BackupDir) == "" {
		return upgradebackup.Result{}, fmt.Errorf("storage.backupDir is required")
	}
	databasePath, err := filesystemDatabasePath(cfg.Storage.DBPath)
	if err != nil {
		return upgradebackup.Result{}, err
	}
	daemonPath := daemonExecutablePath()
	if err := requireExecutableFile(daemonPath, "daemon binary"); err != nil {
		return upgradebackup.Result{}, err
	}
	input := upgradebackup.Input{
		RootDir:          *cfg.Storage.BackupDir,
		DatabasePath:     databasePath,
		CLIBinaryPath:    cliPath,
		DaemonBinaryPath: daemonPath,
		Now:              h.now,
		Snapshot:         services.Coordinator.Backup,
	}
	// Always record the configured path as the restore destination even when
	// contents are materialized (no on-disk file present at backup time).
	input.ConfigPath = metadata.ConfigPath
	if !metadata.FilePresent {
		encoded, err := config.MarshalConfigFile(metadata.ConfigPath, cfg)
		if err != nil {
			// Path may not have an extension; force toml for materialization.
			encoded, err = config.MarshalConfigFile("config.toml", cfg)
			if err != nil {
				return upgradebackup.Result{}, fmt.Errorf("materialize effective config for backup: %w", err)
			}
		}
		input.ConfigContents = encoded
	} else {
		raw, err := os.ReadFile(metadata.ConfigPath)
		if err != nil {
			return upgradebackup.Result{}, fmt.Errorf("refusing upgrade backup: cannot read applied config path %s: %w", metadata.ConfigPath, err)
		}
		if len(raw) == 0 {
			return upgradebackup.Result{}, fmt.Errorf("refusing upgrade backup: config file at %s is empty", metadata.ConfigPath)
		}
		if strings.TrimSpace(metadata.Revision) != "" {
			if got := config.ConfigFileRevision(raw, true); got != metadata.Revision {
				return upgradebackup.Result{}, fmt.Errorf("refusing upgrade backup: on-disk config revision %q does not match applied revision %q", got, metadata.Revision)
			}
		}
	}
	return upgradebackup.Create(ctx, input)
}

func (h *Handler) upgradeConfigAndMetadata() (config.Config, ConfigMetadata) {
	if h != nil && h.context.ConfigSnapshot != nil {
		return h.context.ConfigSnapshot()
	}
	metadata := ConfigMetadata{}
	if h != nil && h.context.ConfigMetadata != nil {
		metadata = h.context.ConfigMetadata()
	}
	if h == nil {
		return config.Config{}, metadata
	}
	return h.context.Config, metadata
}

func refuseSwappedDaemonBinary(h *Handler) error {
	if h != nil && h.context.DaemonBinaryStatus != nil {
		status := h.context.DaemonBinaryStatus()
		if !status.Known {
			return fmt.Errorf("refusing upgrade backup: daemon binary identity is unknown")
		}
		if status.Swapped {
			return fmt.Errorf("refusing upgrade backup: daemon binary on disk no longer matches the running image (%s)", status.Reason)
		}
		return nil
	}
	identity, err := daemonbinary.Self()
	if err != nil {
		// Cannot prove either way when Self fails in constrained environments.
		return nil
	}
	status := daemonbinary.Verify(identity)
	if status.Swapped {
		return fmt.Errorf("refusing upgrade backup: daemon binary on disk no longer matches the running image (%s)", status.Reason)
	}
	return nil
}

func filesystemDatabasePath(dbPath string) (string, error) {
	path, isFile, err := storage.SQLiteFilesystemPath(dbPath)
	if err != nil {
		return "", fmt.Errorf("resolve storage.dbPath for upgrade backup: %w", err)
	}
	if !isFile || strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("refusing upgrade backup: storage.dbPath %q is not a filesystem SQLite database", dbPath)
	}
	return path, nil
}

func requireExecutableFile(path, label string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("%s path is unavailable", label)
	}
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("refusing upgrade backup: %s %s: %w", label, path, err)
	}
	if st.IsDir() {
		return fmt.Errorf("refusing upgrade backup: %s %s is a directory", label, path)
	}
	return nil
}
