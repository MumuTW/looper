package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/daemonbinary"
	"github.com/MumuTW/looper/internal/storage"
	"github.com/MumuTW/looper/internal/upgradebackup"
	"github.com/MumuTW/looper/internal/version"
)

// cliIdentityTimeout bounds the short-lived `looper version --json` probe used
// to prove tools.looperPath matches the running daemon build before backup.
const cliIdentityTimeout = 15 * time.Second

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
	if err := requireMatchingCLIBuild(ctx, cliPath); err != nil {
		return upgradebackup.Result{}, err
	}
	services := h.context.Runtime.Services()
	if services.Coordinator == nil {
		return upgradebackup.Result{}, fmt.Errorf("storage coordinator is unavailable")
	}
	if cfg.Storage.BackupDir == nil || strings.TrimSpace(*cfg.Storage.BackupDir) == "" {
		return upgradebackup.Result{}, fmt.Errorf("storage.backupDir is required")
	}
	// Prefer the path frozen when the coordinator opened SQLite so parent
	// symlink retargets cannot rename restore destinations away from the open inode.
	databasePath := strings.TrimSpace(services.Coordinator.DatabasePath())
	if databasePath == "" {
		var err error
		databasePath, err = filesystemDatabasePath(cfg.Storage.DBPath)
		if err != nil {
			return upgradebackup.Result{}, err
		}
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
	if !metadata.FilePresent {
		// Refuse to materialize effective config: env/CLI overrides (including
		// LOOPER_TOKEN → server.localToken) must not be persisted into a
		// rollback bundle. Operators need an on-disk applied config file.
		return upgradebackup.Result{}, fmt.Errorf("refusing upgrade backup: applied config file is not present at %s; write the applied generation to disk before backup", metadata.ConfigPath)
	}
	// Resolve destination and pin bytes from the same open: a leaf/parent
	// symlink retarget between ReadFile and Create must not leave Source
	// pointing at a different file than ConfigContents.
	configPath, raw, err := pinConfigPathAndBytes(metadata.ConfigPath)
	if err != nil {
		return upgradebackup.Result{}, err
	}
	if strings.TrimSpace(metadata.Revision) != "" {
		if got := config.ConfigFileRevision(raw, true); got != metadata.Revision {
			return upgradebackup.Result{}, fmt.Errorf("refusing upgrade backup: on-disk config revision %q does not match applied revision %q", got, metadata.Revision)
		}
	}
	input.ConfigPath = configPath
	input.ConfigContents = raw
	return upgradebackup.Create(ctx, input)
}

// pinConfigPathAndBytes freezes the restore destination and the bytes together.
// Leaf symlinks are refused; parent symlinks are resolved once before read.
func pinConfigPathAndBytes(configPath string) (string, []byte, error) {
	path := strings.TrimSpace(configPath)
	if path == "" {
		return "", nil, fmt.Errorf("configured config path is unavailable")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, fmt.Errorf("resolve config path: %w", err)
	}
	abs = filepath.Clean(abs)
	info, err := os.Lstat(abs)
	if err != nil {
		return "", nil, fmt.Errorf("refusing upgrade backup: cannot stat config path %s: %w", abs, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", nil, fmt.Errorf("refusing upgrade backup: config path %s is a leaf symlink; point tools at the real applied file so restore metadata cannot diverge", abs)
	}
	if info.IsDir() {
		return "", nil, fmt.Errorf("refusing upgrade backup: config path %s is a directory", abs)
	}
	// Resolve parent symlink components once, then read that frozen path.
	resolved := abs
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		resolved = filepath.Clean(r)
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		return "", nil, fmt.Errorf("refusing upgrade backup: cannot read applied config path %s: %w", resolved, err)
	}
	return resolved, raw, nil
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

// requireMatchingCLIBuild ensures tools.looperPath is the matching pair for the
// running daemon (version.Current). A mismatched CLI would produce a rollback
// bundle that cannot restore the live build.
func requireMatchingCLIBuild(ctx context.Context, cliPath string) error {
	cli, err := readCLIBuildIdentity(ctx, cliPath)
	if err != nil {
		return fmt.Errorf("refusing upgrade backup: cannot read CLI build identity at %s: %w", cliPath, err)
	}
	daemon := version.Current()
	if !cli.SameBuild(daemon) {
		return fmt.Errorf("refusing upgrade backup: CLI at %s does not match the running daemon build (cli=%s daemon=%s)", cliPath, cli.Version, daemon.Version)
	}
	return nil
}

func readCLIBuildIdentity(ctx context.Context, cliPath string) (version.Info, error) {
	probeCtx, cancel := context.WithTimeout(ctx, cliIdentityTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, cliPath, "version", "--json")
	out, err := cmd.Output()
	if err != nil {
		return version.Info{}, err
	}
	var info version.Info
	if err := json.Unmarshal(out, &info); err != nil {
		return version.Info{}, fmt.Errorf("decode version --json: %w", err)
	}
	if !info.Complete() {
		return version.Info{}, fmt.Errorf("CLI build identity is incomplete")
	}
	return info, nil
}
