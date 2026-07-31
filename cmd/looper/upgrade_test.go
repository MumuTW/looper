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
	current := version.Current()
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
			if !report.CurrentPairMatches || report.TargetPairMatches != test.wantPairMatches || !report.TargetIdentityValid || !report.TargetConfigCompatible || report.Relationship != test.wantRelation || !report.DrainRequired || report.CanStartDrain {
				t.Fatalf("report = %#v", report)
			}
			wantBlocks := []string{"current daemon has outstanding quarantine debt"}
			if !test.wantPairMatches {
				wantBlocks = append([]string{"target CLI and daemon build identities differ"}, wantBlocks...)
			}
			if !slices.Equal(report.StartDrainBlocks, wantBlocks) {
				t.Fatalf("start drain blocks = %v, want %v", report.StartDrainBlocks, wantBlocks)
			}
			if report.Current.Status.Storage.SchemaVersion != "0021" || report.Current.Status.Scheduler.ActiveRuns != 2 || report.Current.Status.Service.Recovery.Outstanding.QuarantinedActiveExecutions != 1 {
				t.Fatalf("current status missing from report: %#v", report.Current.Status)
			}
		})
	}
}

func TestUpgradePreflightReportsTargetConfigFailure(t *testing.T) {
	current := version.Current()
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
	if report.TargetConfigCompatible || report.TargetConfigError != "configuration schema rejected" || report.CanStartDrain || len(report.StartDrainBlocks) != 2 {
		t.Fatalf("config result = (%v, %q)", report.TargetConfigCompatible, report.TargetConfigError)
	}
}

func TestUpgradeStartDrainBlocksReportsOnlyRealPreDrainConstraints(t *testing.T) {
	report := upgradePreflight{CurrentPairMatches: true, TargetPairMatches: true, TargetIdentityValid: true, TargetConfigCompatible: true}
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
	bundle, err := upgradebackup.Create(context.Background(), upgradebackup.Input{RootDir: filepath.Join(root, "backups"), ConfigPath: config, CLIBinaryPath: cli, DaemonBinaryPath: daemon, Snapshot: func(context.Context) (string, error) {
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
	dev := version.Current()
	cli := writeIdentityProgram(t, dev)
	daemon := writeIdentityProgram(t, dev)
	err := runUpgrade(context.Background(), nil, []string{"stage-release", "--target-looper", cli, "--target-looperd", daemon, "--release-root", t.TempDir()}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "development snapshots are unsupported") {
		t.Fatalf("runUpgrade() error = %v", err)
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

func upgradeTestDaemon(t *testing.T, identity version.Info) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/version":
			writeEnvelope(w, http.StatusOK, upgradeDaemonVersion{Version: identity.Version, Build: identity.Metadata})
		case "/api/v1/status":
			writeEnvelope(w, http.StatusOK, map[string]any{"service": map[string]any{"healthy": true, "admissionState": "ready", "build": identity.Metadata, "recovery": map[string]any{"outstanding": map[string]any{"quarantinedActiveExecutions": 1, "quarantinedRunningRuns": 0}}}, "storage": map[string]any{"schemaVersion": "0021", "pendingMigrations": []string{}, "healthy": true}, "scheduler": map[string]any{"activeRuns": 2, "runningItems": 2}})
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
