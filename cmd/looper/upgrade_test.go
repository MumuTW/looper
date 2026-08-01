package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/upgradebackup"
	"github.com/MumuTW/looper/internal/upgraderelease"
	"github.com/MumuTW/looper/internal/version"
)

func TestUpgradePreflightReadsCurrentDaemonAndTargetPair(t *testing.T) {
	current := releasedUpgradeIdentity("1.2.3", "release-a")
	for _, test := range []struct {
		name            string
		targetCLI       version.Info
		targetDaemon    version.Info
		wantPairMatches bool
		wantRelation    string
	}{
		{name: "same build", targetCLI: current, targetDaemon: current, wantPairMatches: true, wantRelation: "same_build"},
		{name: "divergent target pair", targetCLI: current, targetDaemon: withUpgradeCommit(current, "other"), wantPairMatches: false, wantRelation: "divergent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := upgradeTestDaemon(t, current)
			configForDaemon(t, server.URL)
			looper := writeIdentityProgram(t, test.targetCLI)
			looperd := writeIdentityProgram(t, test.targetDaemon)
			stdout := &bytes.Buffer{}
			if err := runUpgrade(context.Background(), nil, []string{"preflight", "--target-looper", looper, "--target-looperd", looperd, "--json"}, stdout); err != nil {
				t.Fatalf("runUpgrade() error = %v", err)
			}
			var report upgradePreflight
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatalf("decode report: %v\n%s", err, stdout.String())
			}
			if report.CurrentPairMatches {
				t.Fatalf("CurrentPairMatches = true for incomplete test binary; want fail-closed false: %#v", report)
			}
			if report.TargetPairMatches != test.wantPairMatches || !report.TargetIdentityValid || !report.TargetConfigCompatible || report.Relationship != test.wantRelation || !report.DrainRequired || report.CanStartDrain {
				t.Fatalf("report = %#v", report)
			}
			wantBlocks := []string{"current CLI and daemon build identities differ", "current daemon has outstanding quarantine debt"}
			if !test.wantPairMatches {
				wantBlocks = []string{"current CLI and daemon build identities differ", "target CLI and daemon build identities differ", "current daemon has outstanding quarantine debt"}
			}
			if !slices.Equal(report.StartDrainBlocks, wantBlocks) {
				t.Fatalf("start drain blocks = %v, want %v", report.StartDrainBlocks, wantBlocks)
			}
			if report.Current.Status.Storage.SchemaVersion != "0022_durable_payload_baseline" || report.Current.Status.Scheduler.ActiveRuns != 2 || report.Current.Status.Service.Recovery.Outstanding.QuarantinedActiveExecutions != 1 {
				t.Fatalf("current status missing from report: %#v", report.Current.Status)
			}
		})
	}
}

func TestUpgradePreflightReportsTargetConfigFailure(t *testing.T) {
	current := releasedUpgradeIdentity("1.2.3", "release-a")
	server := upgradeTestDaemon(t, current)
	configForDaemon(t, server.URL)
	looper := writeIdentityProgram(t, current)
	looperd := writeConfigRejectingIdentityProgram(t, current)
	stdout := &bytes.Buffer{}
	if err := runUpgrade(context.Background(), nil, []string{"preflight", "--target-looper", looper, "--target-looperd", looperd, "--json"}, stdout); err != nil {
		t.Fatalf("runUpgrade() error = %v", err)
	}
	var report upgradePreflight
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.TargetConfigCompatible || report.TargetConfigError != "configuration schema rejected" || report.CanStartDrain {
		t.Fatalf("config result = (%v, %q, canStart=%v, blocks=%v)", report.TargetConfigCompatible, report.TargetConfigError, report.CanStartDrain, report.StartDrainBlocks)
	}
}

func TestUpgradeStartDrainBlocksReportsOnlyRealPreDrainConstraints(t *testing.T) {
	eligible := releasedUpgradeIdentity("1.2.3", "release-a")
	report := upgradePreflight{CurrentPairMatches: true, TargetPairMatches: true, TargetIdentityValid: true, TargetConfigCompatible: true}
	report.Target.CLI = eligible
	report.Target.Daemon = eligible
	report.Current.Status.Service.Healthy = true
	report.Current.Status.Service.AdmissionState = "ready"
	report.Current.Status.Storage.Healthy = true
	if blocks := upgradeStartDrainBlocks(report); len(blocks) != 0 {
		t.Fatalf("ready report blocks = %v", blocks)
	}
	report.Current.Status.Scheduler.ActiveRuns = 3
	report.Current.Status.Scheduler.RunningItems = 2
	if blocks := upgradeStartDrainBlocks(report); len(blocks) != 0 {
		t.Fatalf("active work blocks starting drain = %v", blocks)
	}
	report.Current.Status.Storage.PendingMigrations = []string{"0022"}
	if blocks := upgradeStartDrainBlocks(report); len(blocks) != 1 || blocks[0] != "current storage has pending migrations" {
		t.Fatalf("migration blocks = %v", blocks)
	}
	// Stage-release rejects dev channel; preflight must not authorize drain.
	dev := releasedUpgradeIdentity("1.2.4", "devcommit")
	dev.Metadata.Channel = "dev"
	report.Current.Status.Storage.PendingMigrations = nil
	report.Target.Daemon = dev
	report.Target.CLI = dev
	if blocks := upgradeStartDrainBlocks(report); len(blocks) != 1 || !strings.Contains(blocks[0], "development snapshots are unsupported") {
		t.Fatalf("dev channel blocks = %v", blocks)
	}
}

func TestTargetBuildIdentityRejectsIncompleteJSON(t *testing.T) {
	path := writeIdentityProgram(t, version.Info{})
	if _, err := targetBuildIdentity(context.Background(), path, "version", "--json"); err == nil {
		t.Fatal("targetBuildIdentity() error = nil, want incomplete identity rejection")
	}
}

func TestUpgradeBackupRequestsDaemonOwnedBundle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/upgrade/backup" {
			t.Fatalf("request = %s %s, want POST /api/v1/upgrade/backup", r.Method, r.URL.Path)
		}
		writeEnvelope(w, http.StatusOK, map[string]any{"directory": "/backups/upgrade-1", "manifest": map[string]any{"version": 1}})
	}))
	t.Cleanup(server.Close)
	configForDaemon(t, server.URL)
	stdout := &bytes.Buffer{}
	if err := runUpgrade(context.Background(), nil, []string{"backup"}, stdout); err != nil {
		t.Fatal(err)
	}
	var result upgradeBackupResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Directory != "/backups/upgrade-1" {
		t.Fatalf("backup result = %#v", result)
	}
}

func TestUpgradeDrainWaitsForDaemonOwnedSnapshot(t *testing.T) {
	var gets int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/upgrade/drain":
			if r.Method == http.MethodPost {
				writeEnvelope(w, http.StatusOK, upgradeDrainResult{AdmissionState: "draining", Snapshot: upgradeDrainSnapshot{LiveExecutions: 1}})
				return
			}
			if r.Method == http.MethodGet {
				gets++
				writeEnvelope(w, http.StatusOK, upgradeDrainResult{AdmissionState: "draining", Drained: gets >= 1})
				return
			}
		}
		t.Fatalf("request = %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(server.Close)
	configForDaemon(t, server.URL)
	stdout := &bytes.Buffer{}
	if err := runUpgrade(context.Background(), nil, []string{"drain", "--deadline", "1s"}, stdout); err != nil {
		t.Fatal(err)
	}
	var result upgradeDrainResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Drained || result.DeadlineExceeded || gets != 1 {
		t.Fatalf("result = %#v, GETs = %d", result, gets)
	}
}

func TestUpgradeDrainReturnsSnapshotWhenDeadlineExpires(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/upgrade/drain" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		writeEnvelope(w, http.StatusOK, upgradeDrainResult{AdmissionState: "draining", Snapshot: upgradeDrainSnapshot{LiveExecutions: 1}})
	}))
	t.Cleanup(server.Close)
	configForDaemon(t, server.URL)
	stdout := &bytes.Buffer{}
	err := runUpgrade(context.Background(), nil, []string{"drain", "--deadline", "10ms"}, stdout)
	if err == nil || !strings.Contains(err.Error(), "deadline reached") {
		t.Fatalf("runUpgrade() error = %v", err)
	}
	var result upgradeDrainResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Drained || !result.DeadlineExceeded || result.Snapshot.LiveExecutions != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestUpgradeVerifyChecksLocalRollbackBundle(t *testing.T) {
	root := t.TempDir()
	config := writeUpgradeBundleFile(t, root, "config.toml", "[server]\n")
	cli := writeUpgradeBundleFile(t, root, "looper-bin", "cli")
	daemon := writeUpgradeBundleFile(t, root, "looperd-bin", "daemon")
	bundle, err := upgradebackup.Create(context.Background(), upgradebackup.Input{RootDir: filepath.Join(root, "backups"), ConfigPath: config, DatabasePath: filepath.Join(root, "looper.sqlite"), CLIBinaryPath: cli, DaemonBinaryPath: daemon, Snapshot: func(context.Context) (string, error) {
		return writeUpgradeBundleFile(t, root, "snapshot.sqlite", "sqlite"), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	stdout := &bytes.Buffer{}
	if err := runUpgrade(context.Background(), nil, []string{"verify", "--bundle", bundle.Directory}, stdout); err != nil {
		t.Fatal(err)
	}
	var result upgradebackup.Verification
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Valid || result.Directory != bundle.Directory {
		t.Fatalf("verification = %#v", result)
	}
}

func TestUpgradeStageAndActivateReleaseUsesVerifiedPair(t *testing.T) {
	identity := releasedUpgradeIdentity("1.2.3", "aaaaaaa")
	cli := writeIdentityProgram(t, identity)
	daemon := writeIdentityProgram(t, identity)
	root := t.TempDir()
	stdout := &bytes.Buffer{}
	if err := runUpgrade(context.Background(), nil, []string{"stage-release", "--target-looperd", daemon, "--release-root", root, "--target-looper", cli}, stdout); err != nil {
		t.Fatal(err)
	}
	var staged upgraderelease.StageResult
	if err := json.Unmarshal(stdout.Bytes(), &staged); err != nil {
		t.Fatal(err)
	}
	if staged.ReleaseID == "" || staged.Manifest.Build.Version != identity.Version {
		t.Fatalf("staged release = %#v", staged)
	}
	if err := os.WriteFile(cli, []byte("not an identity program"), 0o700); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := runUpgrade(context.Background(), nil, []string{"activate-release", "--release", staged.ReleaseID, "--release-root", root}, stdout); err != nil {
		t.Fatal(err)
	}
	var activated upgraderelease.ActivationResult
	if err := json.Unmarshal(stdout.Bytes(), &activated); err != nil {
		t.Fatal(err)
	}
	if activated.CurrentReleaseID != staged.ReleaseID {
		t.Fatalf("activation = %#v", activated)
	}
	target, err := os.Readlink(filepath.Join(root, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join("releases", staged.ReleaseID) {
		t.Fatalf("current target = %q", target)
	}
}

func TestUpgradeStageReleaseRejectsDevelopmentSnapshot(t *testing.T) {
	dirty := false
	commit := "devcommit"
	ts := "2026-07-31T12:00:00Z"
	dev := version.Info{Version: "0.0.0-dev", Metadata: version.BuildMetadata{VersionSource: "test", Channel: "dev", APIVersion: "v1", GitCommitSHA: &commit, BuildTimestamp: &ts, Dirty: &dirty}}
	cli := writeIdentityProgram(t, dev)
	daemon := writeIdentityProgram(t, dev)
	err := runUpgrade(context.Background(), nil, []string{"stage-release", "--target-looper", cli, "--target-looperd", daemon, "--release-root", t.TempDir()}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "development snapshots are unsupported") {
		t.Fatalf("runUpgrade() error = %v", err)
	}
}

func TestUpgradeVerifyStartChecksRestartedDaemonEvidence(t *testing.T) {
	identity := releasedUpgradeIdentity("1.2.3", "aaaaaaa")
	root, releaseID := stageReleaseForUpgradeTest(t, identity)
	bundle, dbPath := createUpgradeRestoreBundleWithSource(t)
	server := upgradePostStartDaemon(t, identity, 0, upgraderelease.CurrentDaemonExecutable(root), dbPath, filepath.Join(root, "current", "looper"))
	configForDaemon(t, server.URL)
	stdout := &bytes.Buffer{}
	if err := runUpgrade(context.Background(), nil, []string{"verify-start", "--release-root", root, "--release", releaseID, "--bundle", bundle}, stdout); err != nil {
		t.Fatal(err)
	}
	var report upgradePostStartReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Verified || report.ProjectCount != 1 || !report.StartedEvent || len(report.Blocks) != 0 {
		t.Fatalf("report = %#v", report)
	}
}

func TestUpgradeVerifyStartAllowsHeldAdmissionWithFinishingWork(t *testing.T) {
	// Under LOOPER_UPGRADE_VERIFY_HOLD, admission stays draining so the
	// scheduler cannot claim. Finishing agent telemetry (ActiveRuns) is not a
	// cutover failure — only identity, hold, health, and quarantine are gates.
	identity := releasedUpgradeIdentity("1.2.3", "aaaaaaa")
	root, releaseID := stageReleaseForUpgradeTest(t, identity)
	bundle, dbPath := createUpgradeRestoreBundleWithSource(t)
	server := upgradePostStartDaemon(t, identity, 1, upgraderelease.CurrentDaemonExecutable(root), dbPath, filepath.Join(root, "current", "looper"))
	configForDaemon(t, server.URL)
	stdout := &bytes.Buffer{}
	if err := runUpgrade(context.Background(), nil, []string{"verify-start", "--release-root", root, "--release", releaseID, "--bundle", bundle}, stdout); err != nil {
		t.Fatalf("runUpgrade() error = %v", err)
	}
	var report upgradePostStartReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Verified || len(report.Blocks) != 0 {
		t.Fatalf("report = %#v, want verified under held draining admission", report)
	}
	if report.Status.Service.AdmissionState != "draining" {
		t.Fatalf("AdmissionState = %q, want draining", report.Status.Service.AdmissionState)
	}
}

func TestUpgradeVerifyStartRejectsUnheldReadyAdmission(t *testing.T) {
	// Without VERIFY_HOLD the candidate opens ready and may claim work before
	// verify-start; that must fail so restore cannot discard post-restart writes.
	identity := releasedUpgradeIdentity("1.2.3", "aaaaaaa")
	root, releaseID := stageReleaseForUpgradeTest(t, identity)
	bundle, dbPath := createUpgradeRestoreBundleWithSource(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/version":
			versionBody := upgradeDaemonVersion{Version: identity.Version, Build: identity.Metadata}
			versionBody.Binary.Name = "looperd"
			versionBody.Binary.Path = upgraderelease.CurrentDaemonExecutable(root)
			writeEnvelope(w, http.StatusOK, versionBody)
		case "/api/v1/status":
			writeEnvelope(w, http.StatusOK, map[string]any{"service": map[string]any{"healthy": true, "version": identity.Version, "build": identity.Metadata, "admissionState": "ready", "startedAt": "2026-07-31T12:35:00.000Z", "recovery": map[string]any{"outstanding": map[string]any{"quarantinedActiveExecutions": 0, "quarantinedRunningRuns": 0}}}, "storage": map[string]any{"schemaVersion": "0022_durable_payload_baseline", "pendingMigrations": []string{}, "healthy": true, "dbPath": dbPath}, "tools": map[string]any{"looperPath": filepath.Join(root, "current", "looper")}, "scheduler": map[string]any{"activeRuns": 0, "runningItems": 0}})
		case "/api/v1/projects":
			writeEnvelope(w, http.StatusOK, map[string]any{"items": []map[string]any{{"id": "project_1"}}})
		case "/api/v1/events/notification/looperd":
			writeEnvelope(w, http.StatusOK, map[string]any{"items": []map[string]any{{"eventType": "looperd.started"}}})
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	configForDaemon(t, server.URL)
	stdout := &bytes.Buffer{}
	err := runUpgrade(context.Background(), nil, []string{"verify-start", "--release-root", root, "--release", releaseID, "--bundle", bundle}, stdout)
	if err == nil {
		t.Fatal("verify-start error = nil, want unheld ready rejection")
	}
	var report upgradePostStartReport
	if decodeErr := json.Unmarshal(stdout.Bytes(), &report); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if report.Verified {
		t.Fatalf("report = %#v, want unverified", report)
	}
	found := false
	for _, block := range report.Blocks {
		if strings.Contains(block, "not held for cutover verify") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("blocks = %v, want held-admission failure", report.Blocks)
	}
}

func TestUpgradeVerifyStartRejectsDaemonNotGovernedByCurrent(t *testing.T) {
	// Same build launched from outside the release tree must fail verify-start:
	// activate-release only rewrites current, so rollback cannot reclaim a
	// miswired unit that still points at a concrete copy.
	identity := releasedUpgradeIdentity("1.2.3", "aaaaaaa")
	root, releaseID := stageReleaseForUpgradeTest(t, identity)
	bundle, dbPath := createUpgradeRestoreBundleWithSource(t)
	foreign := filepath.Join(t.TempDir(), "looperd-copy")
	if err := os.WriteFile(foreign, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	server := upgradePostStartDaemon(t, identity, 0, foreign, dbPath, filepath.Join(root, "current", "looper"))
	configForDaemon(t, server.URL)
	stdout := &bytes.Buffer{}
	err := runUpgrade(context.Background(), nil, []string{"verify-start", "--release-root", root, "--release", releaseID, "--bundle", bundle}, stdout)
	if err == nil {
		t.Fatal("verify-start error = nil, want miswired executable rejection")
	}
	var report upgradePostStartReport
	if decodeErr := json.Unmarshal(stdout.Bytes(), &report); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if report.Verified || len(report.Blocks) == 0 {
		t.Fatalf("report = %#v, want blocks for non-current executable", report)
	}
	found := false
	for _, block := range report.Blocks {
		if strings.Contains(block, "not governed by release current pointer") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("blocks = %v, want governed-by-current failure", report.Blocks)
	}
}

func TestRequireExecutableGovernedByCurrent(t *testing.T) {
	root := t.TempDir()
	releaseDir := filepath.Join(root, "releases", "rel-1")
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	daemon := filepath.Join(releaseDir, "looperd")
	if err := os.WriteFile(daemon, []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("releases", "rel-1"), filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	current := upgraderelease.CurrentDaemonExecutable(root)
	if err := requireExecutableGovernedByCurrent(current, current); err != nil {
		t.Fatalf("current path: %v", err)
	}
	// Concrete release path must fail even though it is the same inode as current.
	if err := requireExecutableGovernedByCurrent(daemon, current); err == nil {
		t.Fatal("concrete release path accepted")
	}
	other := filepath.Join(t.TempDir(), "other-looperd")
	if err := os.WriteFile(other, []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := requireExecutableGovernedByCurrent(other, current); err == nil {
		t.Fatal("foreign path accepted")
	}
	if err := requireExecutableGovernedByCurrent("", current); err == nil {
		t.Fatal("empty running path accepted")
	}
}

func TestUpgradeRestorePreflightRequiresVerifiedInactiveTargets(t *testing.T) {
	bundle := createUpgradeRestoreBundle(t)
	original := upgradeRestoreOpenPIDs
	t.Cleanup(func() { upgradeRestoreOpenPIDs = original })
	upgradeRestoreOpenPIDs = func(context.Context, []string) ([]int, error) { return nil, nil }
	stdout := &bytes.Buffer{}
	if err := runUpgrade(context.Background(), nil, []string{"restore-preflight", "--bundle", bundle}, stdout); err != nil {
		t.Fatal(err)
	}
	var report upgradeRestorePreflight
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Ready || len(report.Blocks) != 0 || report.Source.DatabasePath == "" {
		t.Fatalf("report = %#v", report)
	}
}

func TestUpgradeRestorePreflightReportsOpenTargets(t *testing.T) {
	bundle := createUpgradeRestoreBundle(t)
	original := upgradeRestoreOpenPIDs
	t.Cleanup(func() { upgradeRestoreOpenPIDs = original })
	upgradeRestoreOpenPIDs = func(context.Context, []string) ([]int, error) { return []int{12, 34}, nil }
	stdout := &bytes.Buffer{}
	err := runUpgrade(context.Background(), nil, []string{"restore-preflight", "--bundle", bundle}, stdout)
	if err == nil || !strings.Contains(err.Error(), "still open") {
		t.Fatalf("runUpgrade() error = %v", err)
	}
	var report upgradeRestorePreflight
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Ready || len(report.OpenPIDs) != 2 || len(report.Blocks) != 1 {
		t.Fatalf("report = %#v", report)
	}
}

func TestUpgradeRestoreRestoresVerifiedConfigAndSQLiteSnapshot(t *testing.T) {
	bundle := createUpgradeRestoreBundle(t)
	verified, err := upgradebackup.Verify(bundle)
	if err != nil {
		t.Fatal(err)
	}
	source, err := upgradebackup.RestoreSource(verified.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source.ConfigPath, []byte("changed-config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source.DatabasePath, []byte("changed-database"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(source.DatabasePath+suffix, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	original := upgradeRestoreOpenPIDs
	t.Cleanup(func() { upgradeRestoreOpenPIDs = original })
	upgradeRestoreOpenPIDs = func(context.Context, []string) ([]int, error) { return nil, nil }
	stdout := &bytes.Buffer{}
	if err := runUpgrade(context.Background(), nil, []string{"restore", "--bundle", bundle, "--confirm"}, stdout); err != nil {
		t.Fatal(err)
	}
	var result upgradeRestoreResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Restored || result.Bundle != bundle {
		t.Fatalf("result = %#v", result)
	}
	config, err := os.ReadFile(source.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(config) != "[server]\n" {
		t.Fatalf("config = %q", config)
	}
	database, err := os.ReadFile(source.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(database) != "sqlite" {
		t.Fatalf("database = %q", database)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(source.DatabasePath + suffix); !os.IsNotExist(err) {
			t.Fatalf("sidecar %s exists or could not be inspected: %v", suffix, err)
		}
	}
}

func TestUpgradeRestoreRequiresConfirmationAndFreshUnusedTargets(t *testing.T) {
	bundle := createUpgradeRestoreBundle(t)
	verified, err := upgradebackup.Verify(bundle)
	if err != nil {
		t.Fatal(err)
	}
	source, err := upgradebackup.RestoreSource(verified.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source.ConfigPath, []byte("changed-config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source.DatabasePath, []byte("changed-database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runUpgrade(context.Background(), nil, []string{"restore", "--bundle", bundle}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("restore without confirmation error = %v", err)
	}
	original := upgradeRestoreOpenPIDs
	t.Cleanup(func() { upgradeRestoreOpenPIDs = original })
	checks := 0
	upgradeRestoreOpenPIDs = func(context.Context, []string) ([]int, error) {
		checks++
		if checks == 2 {
			return []int{42}, nil
		}
		return nil, nil
	}
	err = runUpgrade(context.Background(), nil, []string{"restore", "--confirm", "--bundle", bundle}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "open by processes: 42") {
		t.Fatalf("restore with newly open target error = %v", err)
	}
	config, err := os.ReadFile(source.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(config) != "changed-config\n" {
		t.Fatalf("config mutated without a safe restore: %q", config)
	}
	database, err := os.ReadFile(source.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(database) != "changed-database" {
		t.Fatalf("database mutated without a safe restore: %q", database)
	}
}

func writeUpgradeBundleFile(t *testing.T, directory, name, contents string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func releasedUpgradeIdentity(value, commit string) version.Info {
	timestamp := "2026-07-31T12:34:56Z"
	dirty := false
	return version.Info{Version: value, Metadata: version.BuildMetadata{VersionSource: "git-tag:v" + value, Channel: "stable", APIVersion: "v1", GitCommitSHA: &commit, BuildTimestamp: &timestamp, Dirty: &dirty}}
}

func stageReleaseForUpgradeTest(t *testing.T, identity version.Info) (string, string) {
	t.Helper()
	root := t.TempDir()
	stdout := &bytes.Buffer{}
	if err := runUpgrade(context.Background(), nil, []string{"stage-release", "--target-looper", writeIdentityProgram(t, identity), "--target-looperd", writeIdentityProgram(t, identity), "--release-root", root}, stdout); err != nil {
		t.Fatal(err)
	}
	var staged upgraderelease.StageResult
	if err := json.Unmarshal(stdout.Bytes(), &staged); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := runUpgrade(context.Background(), nil, []string{"activate-release", "--release-root", root, "--release", staged.ReleaseID}, stdout); err != nil {
		t.Fatal(err)
	}
	return root, staged.ReleaseID
}

func createUpgradeRestoreBundle(t *testing.T) string {
	t.Helper()
	bundle, _ := createUpgradeRestoreBundleWithSource(t)
	return bundle
}

func createUpgradeRestoreBundleWithSource(t *testing.T) (bundleDir, databasePath string) {
	t.Helper()
	root := t.TempDir()
	config := writeUpgradeBundleFile(t, root, "config.toml", "[server]\n")
	cli := writeUpgradeBundleFile(t, root, "looper", "cli")
	daemon := writeUpgradeBundleFile(t, root, "looperd", "daemon")
	databasePath = filepath.Join(root, "looper.sqlite")
	bundle, err := upgradebackup.Create(context.Background(), upgradebackup.Input{RootDir: filepath.Join(root, "backups"), ConfigPath: config, DatabasePath: databasePath, CLIBinaryPath: cli, DaemonBinaryPath: daemon, Snapshot: func(context.Context) (string, error) {
		return writeUpgradeBundleFile(t, root, "snapshot.sqlite", "sqlite"), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return bundle.Directory, databasePath
}

func upgradePostStartDaemon(t *testing.T, identity version.Info, activeRuns int, runningExecutable, dbPath, looperPath string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/version":
			versionBody := upgradeDaemonVersion{Version: identity.Version, Build: identity.Metadata}
			versionBody.Binary.Name = "looperd"
			versionBody.Binary.Path = runningExecutable
			writeEnvelope(w, http.StatusOK, versionBody)
		case "/api/v1/status":
			// verify-start requires held (draining) admission under LOOPER_UPGRADE_VERIFY_HOLD.
			writeEnvelope(w, http.StatusOK, map[string]any{"service": map[string]any{"healthy": true, "version": identity.Version, "build": identity.Metadata, "admissionState": "draining", "startedAt": "2026-07-31T12:35:00.000Z", "recovery": map[string]any{"outstanding": map[string]any{"quarantinedActiveExecutions": 0, "quarantinedRunningRuns": 0}}}, "storage": map[string]any{"schemaVersion": "0022_durable_payload_baseline", "pendingMigrations": []string{}, "healthy": true, "dbPath": dbPath}, "tools": map[string]any{"looperPath": looperPath}, "scheduler": map[string]any{"activeRuns": activeRuns, "runningItems": activeRuns}})
		case "/api/v1/projects":
			writeEnvelope(w, http.StatusOK, map[string]any{"items": []map[string]any{{"id": "project_1"}}})
		case "/api/v1/events/notification/looperd":
			writeEnvelope(w, http.StatusOK, map[string]any{"items": []map[string]any{{"eventType": "looperd.started"}}})
		default:
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func upgradeTestDaemon(t *testing.T, identity version.Info) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/version":
			writeEnvelope(w, http.StatusOK, upgradeDaemonVersion{Version: identity.Version, Build: identity.Metadata})
		case "/api/v1/status":
			writeEnvelope(w, http.StatusOK, map[string]any{"service": map[string]any{"healthy": true, "admissionState": "ready", "build": identity.Metadata, "recovery": map[string]any{"outstanding": map[string]any{"quarantinedActiveExecutions": 1, "quarantinedRunningRuns": 0}}}, "storage": map[string]any{"schemaVersion": "0022_durable_payload_baseline", "pendingMigrations": []string{}, "healthy": true}, "scheduler": map[string]any{"activeRuns": 2, "runningItems": 2}})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func writeIdentityProgram(t *testing.T, identity version.Info) string {
	t.Helper()
	encoded, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(path, append([]byte("#!/bin/sh\nprintf '%s\\n' '"), append(encoded, []byte("'\n")...)...), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeConfigRejectingIdentityProgram(t *testing.T, identity version.Info) string {
	t.Helper()
	encoded, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "identity")
	body := "#!/bin/sh\nif [ \"$1\" = \"--check-config\" ]; then echo configuration schema rejected >&2; exit 1; fi\nprintf '%s\\n' '" + string(encoded) + "'\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func withUpgradeCommit(identity version.Info, commit string) version.Info {
	identity.Metadata.GitCommitSHA = &commit
	return identity
}

func TestUpgradePreflightIncompleteIdentityNeverMatches(t *testing.T) {
	incomplete := version.Info{Version: "1.0.0", Metadata: version.BuildMetadata{VersionSource: "test", Channel: "dev", APIVersion: "v1"}}
	if incomplete.SameBuild(incomplete) || incomplete.Complete() {
		t.Fatal("incomplete identity must not Complete/SameBuild")
	}
}
