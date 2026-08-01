package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/storage"
	"github.com/MumuTW/looper/internal/upgradebackup"
	"github.com/MumuTW/looper/internal/upgraderelease"
	"github.com/MumuTW/looper/internal/upgraderestore"
	"github.com/MumuTW/looper/internal/version"
)

// upgradeBackupRequestTimeout bounds CLI wait for daemon-owned SQLite snapshot
// + file copies/checksums. The generic requestTimeout is too short for large DBs.
const upgradeBackupRequestTimeout = 15 * time.Minute

// upgradePreflight is deliberately a read-only report. The current daemon is
// the authority for its live storage/work state, while each target binary is
// the authority for its own embedded build identity.
type upgradePreflight struct {
	Current struct {
		CLI    version.Info  `json:"cli"`
		Daemon version.Info  `json:"daemon"`
		Status upgradeStatus `json:"status"`
	} `json:"current"`
	Target struct {
		CLI    version.Info `json:"cli"`
		Daemon version.Info `json:"daemon"`
	} `json:"target"`
	CurrentPairMatches     bool   `json:"currentPairMatches"`
	TargetPairMatches      bool   `json:"targetPairMatches"`
	TargetIdentityValid    bool   `json:"targetIdentityValid"`
	TargetConfigCompatible bool   `json:"targetConfigCompatible"`
	TargetConfigError      string `json:"targetConfigError,omitempty"`
	Relationship           string `json:"relationship"`
	// CanStartDrain and StartDrainBlockers are a point-in-time operator report,
	// not a durable authorization for a later cutover. Runtime status and target
	// binaries must be queried again when a state-changing command is added.
	CanStartDrain    bool     `json:"canStartDrain"`
	StartDrainBlocks []string `json:"startDrainBlocks"`
	DrainRequired    bool     `json:"drainRequired"`
}

type upgradeStatus struct {
	Service struct {
		Healthy        bool                  `json:"healthy"`
		Version        string                `json:"version"`
		AdmissionState string                `json:"admissionState"`
		StartedAt      *string               `json:"startedAt"`
		Build          version.BuildMetadata `json:"build"`
		Recovery       struct {
			Outstanding struct {
				QuarantinedActiveExecutions int `json:"quarantinedActiveExecutions"`
				QuarantinedRunningRuns      int `json:"quarantinedRunningRuns"`
			} `json:"outstanding"`
		} `json:"recovery"`
	} `json:"service"`
	Storage struct {
		SchemaVersion     string   `json:"schemaVersion"`
		PendingMigrations []string `json:"pendingMigrations"`
		Healthy           bool     `json:"healthy"`
	} `json:"storage"`
	Scheduler struct {
		ActiveRuns   int `json:"activeRuns"`
		RunningItems int `json:"runningItems"`
	} `json:"scheduler"`
}

type upgradeDaemonVersion struct {
	Version string                `json:"version"`
	Build   version.BuildMetadata `json:"build"`
}

type upgradeProjects struct {
	Items []struct {
		ID string `json:"id"`
	} `json:"items"`
}

type upgradeEvents struct {
	Items []struct {
		EventType string `json:"eventType"`
	} `json:"items"`
}

type upgradePostStartReport struct {
	ExpectedBuild   version.Info  `json:"expectedBuild"`
	ExpectedRelease string        `json:"expectedRelease"`
	DaemonBuild     version.Info  `json:"daemonBuild"`
	CurrentRelease  string        `json:"currentRelease"`
	Status          upgradeStatus `json:"status"`
	ProjectCount    int           `json:"projectCount"`
	StartedEvent    bool          `json:"startedEvent"`
	Verified        bool          `json:"verified"`
	Blocks          []string      `json:"blocks"`
}

type upgradeRestorePreflight struct {
	Bundle     string               `json:"bundle"`
	Source     upgradebackup.Source `json:"source"`
	OpenPIDs   []int                `json:"openPids"`
	ProbeError string               `json:"probeError,omitempty"`
	Ready      bool                 `json:"ready"`
	Blocks     []string             `json:"blocks"`
}

type upgradeRestoreResult struct {
	Bundle   string               `json:"bundle"`
	Source   upgradebackup.Source `json:"source"`
	Restored bool                 `json:"restored"`
}

var upgradeRestoreOpenPIDs = findUpgradeRestoreOpenPIDs

func runUpgrade(ctx context.Context, global, operands []string, stdout interface{ Write([]byte) (int, error) }) error {
	if len(operands) == 0 {
		return badUsage("upgrade requires activate-release, backup, drain, preflight, restore, restore-preflight, stage-release, verify, or verify-start")
	}
	if operands[0] == "backup" {
		if len(operands) != 1 {
			return badUsage("upgrade backup accepts no operands")
		}
		cfg, err := loadConfig(global)
		if err != nil {
			return err
		}
		result, err := requestJSONWithin[upgradeBackupResult](ctx, upgradeBackupRequestTimeout, cfg, "POST", "/api/v1/upgrade/backup", nil)
		if err != nil {
			return err
		}
		return writeVersionJSON(stdout, result)
	}
	if operands[0] == "drain" {
		deadline, err := parseUpgradeDrainArgs(operands[1:])
		if err != nil {
			return err
		}
		return runUpgradeDrain(ctx, global, deadline, stdout)
	}
	if operands[0] == "verify" {
		bundle, err := parseUpgradeVerifyArgs(operands[1:])
		if err != nil {
			return err
		}
		result, err := upgradebackup.Verify(bundle)
		if err != nil {
			return err
		}
		return writeVersionJSON(stdout, result)
	}
	if operands[0] == "stage-release" {
		targetLooper, targetLooperd, root, err := parseUpgradeStageReleaseArgs(operands[1:])
		if err != nil {
			return err
		}
		return runUpgradeStageRelease(ctx, targetLooper, targetLooperd, root, stdout)
	}
	if operands[0] == "activate-release" {
		root, releaseID, err := parseUpgradeActivateReleaseArgs(operands[1:])
		if err != nil {
			return err
		}
		return runUpgradeActivateRelease(ctx, root, releaseID, stdout)
	}
	if operands[0] == "verify-start" {
		root, releaseID, err := parseUpgradeActivateReleaseArgs(operands[1:])
		if err != nil {
			return err
		}
		return runUpgradeVerifyStart(ctx, global, root, releaseID, stdout)
	}
	if operands[0] == "restore-preflight" {
		bundle, err := parseUpgradeVerifyArgs(operands[1:])
		if err != nil {
			return badUsage("upgrade restore-preflight requires --bundle <directory>")
		}
		return runUpgradeRestorePreflight(ctx, bundle, stdout)
	}
	if operands[0] == "restore" {
		bundle, err := parseUpgradeRestoreArgs(operands[1:])
		if err != nil {
			return err
		}
		return runUpgradeRestore(ctx, bundle, stdout)
	}
	if operands[0] != "preflight" {
		return badUsage("upgrade requires activate-release, backup, drain, preflight, restore, restore-preflight, stage-release, verify, or verify-start")
	}
	targetLooper, targetLooperd, jsonOutput, err := parseUpgradePreflightArgs(operands[1:])
	if err != nil {
		return err
	}
	cfg, err := loadConfig(global)
	if err != nil {
		return err
	}
	remote, err := requestJSON[upgradeDaemonVersion](ctx, cfg, "GET", "/api/v1/version", nil)
	if err != nil {
		return err
	}
	status, err := requestJSON[upgradeStatus](ctx, cfg, "GET", "/api/v1/status", nil)
	if err != nil {
		return err
	}
	currentDaemon := version.Info{Version: remote.Version, Metadata: remote.Build}
	targetCLI, err := targetBuildIdentity(ctx, targetLooper, "version", "--json")
	if err != nil {
		return err
	}
	targetDaemon, err := targetBuildIdentity(ctx, targetLooperd, "--version-json")
	if err != nil {
		return err
	}
	configCompatible, configErr := targetConfigCompatibility(ctx, targetLooperd, global)
	report := upgradePreflight{CurrentPairMatches: version.Current().SameBuild(currentDaemon), TargetPairMatches: targetCLI.SameBuild(targetDaemon), TargetIdentityValid: targetCLI.Complete() && targetDaemon.Complete(), TargetConfigCompatible: configCompatible, TargetConfigError: configErr}
	report.Current.CLI, report.Current.Daemon, report.Current.Status = version.Current(), currentDaemon, status
	report.Target.CLI, report.Target.Daemon = targetCLI, targetDaemon
	report.Relationship = buildRelationship(currentDaemon, targetDaemon)
	report.StartDrainBlocks = upgradeStartDrainBlocks(report)
	report.CanStartDrain = len(report.StartDrainBlocks) == 0
	report.DrainRequired = status.Scheduler.ActiveRuns > 0 || status.Scheduler.RunningItems > 0
	if jsonOutput {
		return writeVersionJSON(stdout, report)
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode upgrade preflight: %w", err)
	}
	_, _ = fmt.Fprintln(stdout, string(encoded))
	return nil
}

func runUpgradeRestorePreflight(ctx context.Context, bundle string, stdout interface{ Write([]byte) (int, error) }) error {
	verified, err := upgradebackup.Verify(bundle)
	if err != nil {
		return err
	}
	source, err := upgradebackup.RestoreSource(verified.Manifest)
	if err != nil {
		return err
	}
	// Only probe paths restore will mutate. Backup-copied binaries are evidence
	// and are not restored; including tools.looperPath would self-block this CLI.
	paths := []string{source.ConfigPath, source.DatabasePath, source.DatabasePath + "-wal", source.DatabasePath + "-shm"}
	pids, probeErr := upgradeRestoreOpenPIDs(ctx, paths)
	report := upgradeRestorePreflight{Bundle: verified.Directory, Source: source, OpenPIDs: pids}
	if probeErr != nil {
		report.ProbeError = probeErr.Error()
		report.Blocks = append(report.Blocks, "cannot prove rollback targets are unused")
	}
	if len(pids) > 0 {
		report.Blocks = append(report.Blocks, "rollback targets are still open by one or more processes")
	}
	report.Ready = len(report.Blocks) == 0
	if err := writeVersionJSON(stdout, report); err != nil {
		return err
	}
	if !report.Ready {
		return fmt.Errorf("rollback restore preflight failed: %s", strings.Join(report.Blocks, "; "))
	}
	return nil
}

// runUpgradeRestore restores only the config and SQLite snapshot recorded by a
// verified v2 rollback bundle. Binary selection remains the release pointer's
// responsibility, because a backup's copied binaries are evidence, not a
// supported executable release artifact.
func runUpgradeRestore(ctx context.Context, bundle string, stdout interface{ Write([]byte) (int, error) }) error {
	verified, err := upgradebackup.Verify(bundle)
	if err != nil {
		return err
	}
	source, err := upgradebackup.RestoreSource(verified.Manifest)
	if err != nil {
		return err
	}
	databasePath, filesystem, err := storage.SQLiteFilesystemPath(source.DatabasePath)
	if err != nil {
		return fmt.Errorf("resolve rollback database target: %w", err)
	}
	if !filesystem {
		return fmt.Errorf("rollback restore requires an on-disk SQLite database target")
	}
	source.DatabasePath = databasePath
	paths := []string{source.ConfigPath, source.DatabasePath, source.DatabasePath + "-wal", source.DatabasePath + "-shm"}
	if err := ensureUpgradeRestoreTargetsUnused(ctx, paths); err != nil {
		return err
	}
	lock, err := storage.AcquireDatabaseLock(source.DatabasePath, storage.DatabaseLockExclusive)
	if err != nil {
		return fmt.Errorf("acquire rollback database lock: %w", err)
	}
	defer lock.Release()
	if err := ensureUpgradeRestoreTargetsUnused(ctx, paths); err != nil {
		return err
	}
	if err := upgraderestore.Recover(upgraderestore.JournalPath(source.DatabasePath)); err != nil {
		return fmt.Errorf("recover interrupted rollback restore: %w", err)
	}
	if err := upgraderestore.Restore(verified.Directory, source); err != nil {
		return err
	}
	return writeVersionJSON(stdout, upgradeRestoreResult{Bundle: verified.Directory, Source: source, Restored: true})
}

func ensureUpgradeRestoreTargetsUnused(ctx context.Context, paths []string) error {
	pids, err := upgradeRestoreOpenPIDs(ctx, paths)
	if err != nil {
		return fmt.Errorf("cannot prove rollback targets are unused: %w", err)
	}
	if len(pids) > 0 {
		return fmt.Errorf("rollback restore refused because targets are open by processes: %s", formatUpgradeRestorePIDs(pids))
	}
	return nil
}

func formatUpgradeRestorePIDs(pids []int) string {
	values := make([]string, 0, len(pids))
	for _, pid := range pids {
		values = append(values, strconv.Itoa(pid))
	}
	return strings.Join(values, ", ")
}

func findUpgradeRestoreOpenPIDs(ctx context.Context, paths []string) ([]int, error) {
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return nil, fmt.Errorf("lsof is required to prove rollback targets are unused: %w", err)
	}
	// Probe only paths that currently exist. Absent WAL/SHM is normal; asking
	// lsof about them can exit 1 while still emitting PIDs for open targets,
	// and Output would discard that evidence as a hard error.
	existing := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if _, err := os.Lstat(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("inspect rollback target %s: %w", path, err)
		}
		existing = append(existing, path)
	}
	if len(existing) == 0 {
		return nil, nil
	}
	output, err := exec.CommandContext(ctx, lsof, append([]string{"-t", "--"}, existing...)...).Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
			// lsof exits 1 when no process has the files open. Still parse any
			// stdout in case a mixed lookup partially succeeded.
			if len(output) == 0 {
				return nil, nil
			}
		} else {
			return nil, fmt.Errorf("inspect rollback target users: %w", err)
		}
	}
	seen := map[int]bool{}
	pids := make([]int, 0)
	for _, line := range strings.Fields(string(output)) {
		pid, err := strconv.Atoi(line)
		if err != nil || pid <= 0 || seen[pid] {
			continue
		}
		seen[pid] = true
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids, nil
}

func upgradeStartDrainBlocks(report upgradePreflight) []string {
	blocks := make([]string, 0, 8)
	if !report.CurrentPairMatches {
		blocks = append(blocks, "current CLI and daemon build identities differ")
	}
	if !report.TargetPairMatches {
		blocks = append(blocks, "target CLI and daemon build identities differ")
	}
	if !report.TargetIdentityValid {
		blocks = append(blocks, "target build identity is incomplete")
	}
	if !report.TargetConfigCompatible {
		blocks = append(blocks, "target daemon rejects the selected configuration")
	}
	if !report.Current.Status.Service.Healthy {
		blocks = append(blocks, "current daemon service is unhealthy")
	}
	if report.Current.Status.Service.AdmissionState != "ready" {
		blocks = append(blocks, "current daemon admission is not ready")
	}
	if !report.Current.Status.Storage.Healthy {
		blocks = append(blocks, "current storage is unhealthy")
	}
	if len(report.Current.Status.Storage.PendingMigrations) > 0 {
		blocks = append(blocks, "current storage has pending migrations")
	}
	if report.Current.Status.Service.Recovery.Outstanding.QuarantinedActiveExecutions > 0 || report.Current.Status.Service.Recovery.Outstanding.QuarantinedRunningRuns > 0 {
		blocks = append(blocks, "current daemon has outstanding quarantine debt")
	}
	return blocks
}

type upgradeBackupResult struct {
	Directory string `json:"directory"`
	Manifest  any    `json:"manifest"`
}

type upgradeDrainSnapshot struct {
	LiveExecutions      int `json:"liveExecutions"`
	PendingSpawns       int `json:"pendingSpawns"`
	BoundOperations     int `json:"boundOperations"`
	PendingOperations   int `json:"pendingOperations"`
	NonAgentHandles     int `json:"nonAgentHandles"`
	WorkProducersActive int `json:"workProducersActive"`
}

type upgradeDrainResult struct {
	AdmissionState   string               `json:"admissionState"`
	Snapshot         upgradeDrainSnapshot `json:"snapshot"`
	Drained          bool                 `json:"drained"`
	DeadlineExceeded bool                 `json:"deadlineExceeded"`
}

func runUpgradeDrain(ctx context.Context, global []string, deadline time.Duration, stdout interface{ Write([]byte) (int, error) }) error {
	cfg, err := loadConfig(global)
	if err != nil {
		return err
	}
	result, err := requestJSON[upgradeDrainResult](ctx, cfg, "POST", "/api/v1/upgrade/drain", nil)
	if err != nil {
		return err
	}
	if result.Drained {
		return writeVersionJSON(stdout, result)
	}

	drainCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-drainCtx.Done():
			result.DeadlineExceeded = true
			if err := writeVersionJSON(stdout, result); err != nil {
				return err
			}
			return fmt.Errorf("upgrade drain deadline reached with %d live executions, %d pending spawns, %d bound operations, %d pending operations, %d non-agent handles, and %d work producers", result.Snapshot.LiveExecutions, result.Snapshot.PendingSpawns, result.Snapshot.BoundOperations, result.Snapshot.PendingOperations, result.Snapshot.NonAgentHandles, result.Snapshot.WorkProducersActive)
		case <-ticker.C:
			result, err = requestJSON[upgradeDrainResult](drainCtx, cfg, "GET", "/api/v1/upgrade/drain", nil)
			if err != nil {
				return err
			}
			if result.Drained {
				return writeVersionJSON(stdout, result)
			}
		}
	}
}

func targetConfigCompatibility(ctx context.Context, binary string, global []string) (bool, string) {
	args := append([]string{"--check-config"}, global...)
	out, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	if err == nil {
		return true, ""
	}
	message := strings.TrimSpace(string(out))
	if message == "" {
		message = err.Error()
	}
	return false, message
}

func parseUpgradePreflightArgs(args []string) (string, string, bool, error) {
	var looper, looperd string
	jsonOutput := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			if jsonOutput {
				return "", "", false, badUsage("upgrade preflight accepts --json at most once")
			}
			jsonOutput = true
		case "--target-looper", "--target-looperd":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return "", "", false, badUsage("%s requires a binary path", args[i])
			}
			i++
			if args[i-1] == "--target-looper" {
				looper = args[i]
			} else {
				looperd = args[i]
			}
		default:
			return "", "", false, badUsage("upgrade preflight does not accept %q", args[i])
		}
	}
	if strings.TrimSpace(looper) == "" || strings.TrimSpace(looperd) == "" {
		return "", "", false, badUsage("upgrade preflight requires --target-looper and --target-looperd")
	}
	return looper, looperd, jsonOutput, nil
}

func parseUpgradeDrainArgs(args []string) (time.Duration, error) {
	if len(args) != 2 || args[0] != "--deadline" || strings.TrimSpace(args[1]) == "" {
		return 0, badUsage("upgrade drain requires --deadline <duration>")
	}
	deadline, err := time.ParseDuration(args[1])
	if err != nil || deadline <= 0 {
		return 0, badUsage("upgrade drain requires a positive Go duration for --deadline")
	}
	return deadline, nil
}

func parseUpgradeVerifyArgs(args []string) (string, error) {
	if len(args) != 2 || args[0] != "--bundle" || strings.TrimSpace(args[1]) == "" {
		return "", badUsage("upgrade verify requires --bundle <directory>")
	}
	return args[1], nil
}

func parseUpgradeRestoreArgs(args []string) (string, error) {
	var bundle string
	confirmed := false
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--confirm":
			if confirmed {
				return "", badUsage("upgrade restore accepts --confirm at most once")
			}
			confirmed = true
		case "--bundle":
			if bundle != "" || index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") || strings.TrimSpace(args[index+1]) == "" {
				return "", badUsage("upgrade restore requires --bundle <directory> --confirm")
			}
			index++
			bundle = args[index]
		default:
			return "", badUsage("upgrade restore requires --bundle <directory> --confirm")
		}
	}
	if strings.TrimSpace(bundle) == "" || !confirmed {
		return "", badUsage("upgrade restore requires --bundle <directory> --confirm")
	}
	return bundle, nil
}

func runUpgradeStageRelease(ctx context.Context, targetLooper, targetLooperd, root string, stdout interface{ Write([]byte) (int, error) }) error {
	cli, err := targetBuildIdentity(ctx, targetLooper, "version", "--json")
	if err != nil {
		return err
	}
	daemon, err := targetBuildIdentity(ctx, targetLooperd, "--version-json")
	if err != nil {
		return err
	}
	if !cli.SameBuild(daemon) {
		return fmt.Errorf("target CLI and daemon build identities differ")
	}
	if err := releaseCandidateAllowed(cli); err != nil {
		return err
	}
	releaseID := releaseIDFor(cli)
	staged, err := upgraderelease.Stage(upgraderelease.StageInput{RootDir: root, ReleaseID: releaseID, CLIBinaryPath: targetLooper, DaemonBinaryPath: targetLooperd, Build: cli})
	if err != nil {
		return err
	}
	if err := verifyStagedReleaseIdentity(ctx, staged); err != nil {
		return err
	}
	return writeVersionJSON(stdout, staged)
}

func runUpgradeActivateRelease(ctx context.Context, root, releaseID string, stdout interface{ Write([]byte) (int, error) }) error {
	staged, err := upgraderelease.Verify(root, releaseID)
	if err != nil {
		return err
	}
	if err := releaseCandidateAllowed(staged.Manifest.Build); err != nil {
		return err
	}
	if err := verifyStagedReleaseIdentity(ctx, staged); err != nil {
		return err
	}
	result, err := upgraderelease.Activate(root, releaseID)
	if err != nil {
		return err
	}
	return writeVersionJSON(stdout, result)
}

func runUpgradeVerifyStart(ctx context.Context, global []string, root, releaseID string, stdout interface{ Write([]byte) (int, error) }) error {
	staged, err := upgraderelease.Verify(root, releaseID)
	if err != nil {
		return err
	}
	if err := releaseCandidateAllowed(staged.Manifest.Build); err != nil {
		return err
	}
	if err := verifyStagedReleaseIdentity(ctx, staged); err != nil {
		return err
	}
	currentRelease, err := upgraderelease.CurrentReleaseID(root)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(global)
	if err != nil {
		return err
	}
	remote, err := requestJSON[upgradeDaemonVersion](ctx, cfg, "GET", "/api/v1/version", nil)
	if err != nil {
		return err
	}
	status, err := requestJSON[upgradeStatus](ctx, cfg, "GET", "/api/v1/status", nil)
	if err != nil {
		return err
	}
	projects, err := requestJSON[upgradeProjects](ctx, cfg, "GET", "/api/v1/projects", nil)
	if err != nil {
		return err
	}
	events, err := requestJSON[upgradeEvents](ctx, cfg, "GET", "/api/v1/events/notification/looperd", nil)
	if err != nil {
		return err
	}
	daemon := version.Info{Version: remote.Version, Metadata: remote.Build}
	report := upgradePostStartReport{ExpectedBuild: staged.Manifest.Build, ExpectedRelease: releaseID, DaemonBuild: daemon, CurrentRelease: currentRelease, Status: status, ProjectCount: len(projects.Items), StartedEvent: containsUpgradeEvent(events, "looperd.started")}
	report.Blocks = upgradePostStartBlocks(report)
	report.Verified = len(report.Blocks) == 0
	if err := writeVersionJSON(stdout, report); err != nil {
		return err
	}
	if !report.Verified {
		return fmt.Errorf("post-start verification failed: %s", strings.Join(report.Blocks, "; "))
	}
	return nil
}

func containsUpgradeEvent(events upgradeEvents, eventType string) bool {
	for _, event := range events.Items {
		if event.EventType == eventType {
			return true
		}
	}
	return false
}

func upgradePostStartBlocks(report upgradePostStartReport) []string {
	blocks := make([]string, 0, 9)
	if !report.DaemonBuild.SameBuild(report.ExpectedBuild) {
		blocks = append(blocks, "running daemon build does not match staged release")
	}
	if report.CurrentRelease == "" || report.CurrentRelease != report.ExpectedRelease {
		blocks = append(blocks, "current release pointer does not select the verified release")
	}
	statusBuild := version.Info{Version: report.Status.Service.Version, Metadata: report.Status.Service.Build}
	if !statusBuild.SameBuild(report.ExpectedBuild) {
		blocks = append(blocks, "daemon status build does not match staged release")
	}
	if !report.Status.Service.Healthy {
		blocks = append(blocks, "daemon service is unhealthy")
	}
	if report.Status.Service.AdmissionState != "ready" {
		blocks = append(blocks, "daemon admission is not ready")
	}
	if report.Status.Service.StartedAt == nil || strings.TrimSpace(*report.Status.Service.StartedAt) == "" {
		blocks = append(blocks, "daemon startup time is unavailable")
	}
	if !report.Status.Storage.Healthy {
		blocks = append(blocks, "daemon storage is unhealthy")
	}
	if len(report.Status.Storage.PendingMigrations) > 0 {
		blocks = append(blocks, "daemon storage has pending migrations")
	}
	// Do not treat normal scheduler ownership as a startup failure. Drain
	// leaves queued work intact; once the replacement daemon is ready it may
	// claim that work before a separately invoked verify-start runs. Identity,
	// admission readiness, storage health, and quarantine debt remain the
	// cutover gates; active/running counts are operator telemetry only.
	if report.Status.Service.Recovery.Outstanding.QuarantinedActiveExecutions > 0 || report.Status.Service.Recovery.Outstanding.QuarantinedRunningRuns > 0 {
		blocks = append(blocks, "daemon has outstanding quarantine debt")
	}
	if !report.StartedEvent {
		blocks = append(blocks, "daemon startup recovery event is unavailable")
	}
	return blocks
}

func verifyStagedReleaseIdentity(ctx context.Context, staged upgraderelease.StageResult) error {
	cli, err := targetBuildIdentity(ctx, filepath.Join(staged.Directory, "looper"), "version", "--json")
	if err != nil {
		return fmt.Errorf("verify staged CLI identity: %w", err)
	}
	daemon, err := targetBuildIdentity(ctx, filepath.Join(staged.Directory, "looperd"), "--version-json")
	if err != nil {
		return fmt.Errorf("verify staged daemon identity: %w", err)
	}
	if !cli.SameBuild(daemon) || !cli.SameBuild(staged.Manifest.Build) {
		return fmt.Errorf("staged CLI and daemon do not match the release manifest build identity")
	}
	return nil
}

func releaseCandidateAllowed(identity version.Info) error {
	if !identity.Complete() {
		return fmt.Errorf("release staging requires a complete build identity")
	}
	if identity.Metadata.Channel != "stable" && identity.Metadata.Channel != "beta" {
		return fmt.Errorf("release staging rejects %q channel; development snapshots are unsupported", identity.Metadata.Channel)
	}
	if identity.Metadata.GitCommitSHA == nil || strings.TrimSpace(*identity.Metadata.GitCommitSHA) == "" || identity.Metadata.BuildTimestamp == nil || strings.TrimSpace(*identity.Metadata.BuildTimestamp) == "" || identity.Metadata.Dirty == nil || *identity.Metadata.Dirty {
		return fmt.Errorf("release staging requires a complete, clean build identity")
	}
	return nil
}

func releaseIDFor(identity version.Info) string {
	commit := strings.TrimSpace(*identity.Metadata.GitCommitSHA)
	timestamp := strings.TrimSpace(*identity.Metadata.BuildTimestamp)
	if len(commit) > 12 {
		commit = commit[:12]
	}
	return releaseIDComponent(identity.Version) + "-" + releaseIDComponent(identity.Metadata.Channel) + "-" + releaseIDComponent(commit) + "-" + releaseIDComponent(timestamp)
}

func releaseIDComponent(value string) string {
	var builder strings.Builder
	for _, char := range strings.TrimSpace(value) {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '.' || char == '_' || char == '-' {
			builder.WriteRune(char)
		} else {
			builder.WriteByte('-')
		}
	}
	return strings.Trim(builder.String(), "-")
}

func parseUpgradeStageReleaseArgs(args []string) (string, string, string, error) {
	values, err := parseUpgradeNamedArgs(args, "stage-release", []string{"--target-looper", "--target-looperd", "--release-root"})
	if err != nil {
		return "", "", "", err
	}
	return values["--target-looper"], values["--target-looperd"], values["--release-root"], nil
}

func parseUpgradeActivateReleaseArgs(args []string) (string, string, error) {
	values, err := parseUpgradeNamedArgs(args, "activate-release", []string{"--release-root", "--release"})
	if err != nil {
		return "", "", err
	}
	return values["--release-root"], values["--release"], nil
}

func parseUpgradeNamedArgs(args []string, command string, names []string) (map[string]string, error) {
	allowed := map[string]bool{}
	values := map[string]string{}
	for _, name := range names {
		allowed[name] = true
	}
	for index := 0; index < len(args); index++ {
		name := args[index]
		if !allowed[name] || index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") || strings.TrimSpace(args[index+1]) == "" {
			return nil, badUsage("upgrade %s requires %s", command, strings.Join(names, " "))
		}
		if _, exists := values[name]; exists {
			return nil, badUsage("upgrade %s accepts %s at most once", command, name)
		}
		index++
		values[name] = args[index]
	}
	for _, name := range names {
		if strings.TrimSpace(values[name]) == "" {
			return nil, badUsage("upgrade %s requires %s", command, strings.Join(names, " "))
		}
	}
	return values, nil
}

func targetBuildIdentity(ctx context.Context, binary string, args ...string) (version.Info, error) {
	out, err := exec.CommandContext(ctx, binary, args...).Output()
	if err != nil {
		return version.Info{}, fmt.Errorf("read target build identity from %s: %w", binary, err)
	}
	var identity version.Info
	if err := json.Unmarshal(out, &identity); err != nil {
		return version.Info{}, fmt.Errorf("decode target build identity from %s: %w", binary, err)
	}
	if !identity.Complete() {
		return version.Info{}, fmt.Errorf("target build identity from %s is incomplete", binary)
	}
	return identity, nil
}

func buildRelationship(current, target version.Info) string {
	if current.SameBuild(target) {
		return "same_build"
	}
	if current.Version == target.Version {
		return "divergent"
	}
	return "different_version"
}
