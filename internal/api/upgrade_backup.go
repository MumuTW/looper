package api

import (
	"context"
	"fmt"
	"strings"

	looperdruntime "github.com/MumuTW/looper/internal/runtime"
	"github.com/MumuTW/looper/internal/upgradebackup"
)

func (h *Handler) createUpgradeBackup(ctx context.Context) (upgradebackup.Result, error) {
	if h == nil || h.context.Runtime == nil {
		return upgradebackup.Result{}, fmt.Errorf("runtime is unavailable")
	}
	drainRuntime, ok := any(h.context.Runtime).(upgradeDrainRuntime)
	if !ok || drainRuntime.AdmissionState() != looperdruntime.AdmissionDraining || !drainRuntime.DrainSnapshot().Drained() {
		return upgradebackup.Result{}, fmt.Errorf("upgrade backup requires a drained daemon")
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
		RootDir: *cfg.Storage.BackupDir, ConfigPath: metadata.ConfigPath,
		CLIBinaryPath: *cfg.Tools.LooperPath, DaemonBinaryPath: daemonExecutablePath(), Now: h.now,
		Snapshot: services.Coordinator.Backup,
	})
}
