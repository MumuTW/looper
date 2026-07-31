package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/upgradebackup"
	"github.com/MumuTW/looper/internal/version"
)

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
		AdmissionState string                `json:"admissionState"`
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

func runUpgrade(ctx context.Context, global, operands []string, stdout interface{ Write([]byte) (int, error) }) error {
	if len(operands) == 0 {
		return badUsage("upgrade requires backup, drain, preflight, or verify")
	}
	if operands[0] == "backup" {
		if len(operands) != 1 {
			return badUsage("upgrade backup accepts no operands")
		}
		cfg, err := loadConfig(global)
		if err != nil {
			return err
		}
		result, err := requestJSON[upgradeBackupResult](ctx, cfg, "POST", "/api/v1/upgrade/backup", nil)
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
	if operands[0] != "preflight" {
		return badUsage("upgrade requires backup, drain, preflight, or verify")
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
	report := upgradePreflight{CurrentPairMatches: version.Current().SameBuild(currentDaemon), TargetPairMatches: targetCLI.SameBuild(targetDaemon), TargetIdentityValid: validBuildIdentity(targetCLI) && validBuildIdentity(targetDaemon), TargetConfigCompatible: configCompatible, TargetConfigError: configErr}
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
	LiveExecutions    int `json:"liveExecutions"`
	PendingSpawns     int `json:"pendingSpawns"`
	BoundOperations   int `json:"boundOperations"`
	PendingOperations int `json:"pendingOperations"`
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
			return fmt.Errorf("upgrade drain deadline reached with %d live executions, %d pending spawns, %d bound operations, and %d pending operations", result.Snapshot.LiveExecutions, result.Snapshot.PendingSpawns, result.Snapshot.BoundOperations, result.Snapshot.PendingOperations)
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

func targetBuildIdentity(ctx context.Context, binary string, args ...string) (version.Info, error) {
	out, err := exec.CommandContext(ctx, binary, args...).Output()
	if err != nil {
		return version.Info{}, fmt.Errorf("read target build identity from %s: %w", binary, err)
	}
	var identity version.Info
	if err := json.Unmarshal(out, &identity); err != nil {
		return version.Info{}, fmt.Errorf("decode target build identity from %s: %w", binary, err)
	}
	if !validBuildIdentity(identity) {
		return version.Info{}, fmt.Errorf("target build identity from %s is incomplete", binary)
	}
	return identity, nil
}

func validBuildIdentity(identity version.Info) bool {
	return strings.TrimSpace(identity.Version) != "" &&
		strings.TrimSpace(identity.Metadata.VersionSource) != "" &&
		strings.TrimSpace(identity.Metadata.Channel) != "" &&
		strings.TrimSpace(identity.Metadata.APIVersion) != ""
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
