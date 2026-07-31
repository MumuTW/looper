package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/MumuTW/looper/internal/upgradebackup"
)

func (h *Handler) createUpgradeBackup(ctx context.Context) (upgradebackup.Result, error) {
	if h == nil || h.context.Runtime == nil {
		return upgradebackup.Result{}, fmt.Errorf("runtime is unavailable")
	}
	cfg := h.effectiveConfig()
	if cfg.Tools.LooperPath == nil || strings.TrimSpace(*cfg.Tools.LooperPath) == "" {
		return upgradebackup.Result{}, fmt.Errorf("tools.looperPath is required to create a matching upgrade backup")
	}
	metadata := ConfigMetadata{}
	if h.context.ConfigMetadata != nil {
		metadata = h.context.ConfigMetadata()
	}
	if strings.TrimSpace(metadata.ConfigPath) == "" {
		return upgradebackup.Result{}, fmt.Errorf("configured config path is unavailable")
	}
	services := h.context.Runtime.Services()
	if services.Coordinator == nil {
		return upgradebackup.Result{}, fmt.Errorf("storage coordinator is unavailable")
	}
	if cfg.Storage.BackupDir == nil || strings.TrimSpace(*cfg.Storage.BackupDir) == "" {
		return upgradebackup.Result{}, fmt.Errorf("storage.backupDir is required")
	}
	return upgradebackup.Create(ctx, upgradebackup.Input{
		RootDir: *cfg.Storage.BackupDir, ConfigPath: metadata.ConfigPath, DatabasePath: cfg.Storage.DBPath,
		CLIBinaryPath: *cfg.Tools.LooperPath, DaemonBinaryPath: daemonExecutablePath(), Now: h.now,
		Snapshot: services.Coordinator.Backup,
	})
}
