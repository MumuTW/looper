package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

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
	if len(operands) == 0 || operands[0] != "preflight" {
		return badUsage("upgrade requires the preflight subcommand")
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
