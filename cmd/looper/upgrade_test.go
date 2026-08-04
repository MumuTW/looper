package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/upgradebackup"
	"github.com/MumuTW/looper/internal/version"
)

func TestUpgradePreflightReadsCurrentDaemonAndTargetPair(t *testing.T) {
	current := completeUpgradeIdentity("release-a")
	for _, test := range []struct {
		name            string
		targetCLI       version.Info
		targetDaemon    version.Info
		wantPairMatches bool
		wantRelation    string
	}{
		{name: "same build", targetCLI: current, targetDaemon: current, wantPairMatches: true, wantRelation: "same_build"},
		{name: "divergent target pair", targetCLI: current, targetDaemon: withUpgradeCommit(current, "other-commit"), wantPairMatches: false, wantRelation: "divergent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := upgradeTestDaemon(t, current)
			configForDaemon(t, server.URL)
			looper := writeIdentityProgram(t, test.targetCLI)
			looperd := writeIdentityProgram(t, test.targetDaemon)
			stdout := &bytes.Buffer{}
			if err := runUpgrade(context.Background(), nil, []string{"preflight", "--target-looper", looper, "--target-looperd", looperd, "--json"}, stdout); err != nil {
				t.Fatalf("runUpgrade() error = %v\n%s", err, stdout.String())
			}
			var report upgradePreflight
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatalf("decode report: %v\n%s", err, stdout.String())
			}
			// CurrentPairMatches compares this process's version.Current() to the
			// daemon — in unit tests Current() is typically incomplete, so it must
			// never report a match (fail-closed identity).
			if report.CurrentPairMatches {
				t.Fatalf("CurrentPairMatches = true for incomplete test binary identity; want fail-closed false")
			}
			if report.TargetPairMatches != test.wantPairMatches || !report.TargetIdentityValid || !report.TargetConfigCompatible || report.Relationship != test.wantRelation {
				t.Fatalf("report = %#v", report)
			}
			if report.Current.Status.Storage.SchemaVersion != "0022" || report.Current.Status.Scheduler.ActiveRuns != 2 || report.Current.Status.Service.Recovery.Outstanding.QuarantinedActiveExecutions != 1 {
				t.Fatalf("current status missing from report: %#v", report.Current.Status)
			}
		})
	}
}

func TestUpgradePreflightIncompleteIdentityNeverMatches(t *testing.T) {
	// Proves shipped SameBuild/Complete gate: empty optional fields cannot prove equality.
	incomplete := version.Info{
		Version: "1.0.0",
		Metadata: version.BuildMetadata{
			VersionSource: "test",
			Channel:       "dev",
			APIVersion:    "v1",
		},
	}
	if incomplete.SameBuild(incomplete) {
		t.Fatal("incomplete identity SameBuild(self) = true, want false")
	}
	if incomplete.Complete() {
		t.Fatal("incomplete identity Complete() = true, want false")
	}
	path := writeRawIdentityProgram(t, incomplete)
	if _, err := targetBuildIdentity(context.Background(), path, "version", "--json"); err == nil {
		t.Fatal("targetBuildIdentity accepted incomplete identity")
	}
}

func TestUpgradePreflightReportsTargetConfigFailure(t *testing.T) {
	current := completeUpgradeIdentity("release-a")
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
	if report.TargetConfigCompatible || report.TargetConfigError != "configuration schema rejected" {
		t.Fatalf("config result = (%v, %q)", report.TargetConfigCompatible, report.TargetConfigError)
	}
}

func TestTargetBuildIdentityRejectsIncompleteJSON(t *testing.T) {
	path := writeRawIdentityProgram(t, version.Info{})
	if _, err := targetBuildIdentity(context.Background(), path, "version", "--json"); err == nil {
		t.Fatal("targetBuildIdentity() error = nil, want incomplete identity rejection")
	}
}

func TestUpgradePostStartRequiresVerifyHoldAdmission(t *testing.T) {
	identity := completeUpgradeIdentity("candidate")
	startedAt := "2026-08-03T12:00:00Z"
	report := upgradePostStartReport{
		ExpectedBuild:   identity,
		ExpectedRelease: "candidate-release",
		DaemonBuild:     identity,
		CurrentRelease:  "candidate-release",
		StartedEvent:    true,
		Status: upgradeStatus{
			Service: struct {
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
			}{Healthy: true, Version: identity.Version, AdmissionState: "draining", StartedAt: &startedAt, Build: identity.Metadata},
			Storage: struct {
				SchemaVersion     string   `json:"schemaVersion"`
				PendingMigrations []string `json:"pendingMigrations"`
				Healthy           bool     `json:"healthy"`
			}{Healthy: true},
		},
	}

	if blocks := upgradePostStartBlocks(report); len(blocks) != 0 {
		t.Fatalf("upgradePostStartBlocks() with draining admission = %v, want no blocks", blocks)
	}

	report.StartedEvent = false
	if blocks := upgradePostStartBlocks(report); len(blocks) != 0 {
		t.Fatalf("upgradePostStartBlocks() with held startup and no durable event = %v, want no blocks", blocks)
	}

	report.StartedEvent = true
	report.Status.Service.AdmissionState = "ready"
	blocks := upgradePostStartBlocks(report)
	if len(blocks) != 1 || blocks[0] != "daemon admission is not draining under verify hold" {
		t.Fatalf("upgradePostStartBlocks() with ready admission = %v, want verify-hold block", blocks)
	}

	report.Status.Service.AdmissionState = "draining"
	report.Status.Scheduler.ActiveRuns = 1
	blocks = upgradePostStartBlocks(report)
	if len(blocks) != 1 || blocks[0] != "scheduler still owns active work after cutover" {
		t.Fatalf("upgradePostStartBlocks() with active scheduler work = %v, want scheduler blocker", blocks)
	}
}

func TestParseUpgradeVerifyStartArgsRequiresBundle(t *testing.T) {
	root, release, bundle, err := parseUpgradeVerifyStartArgs([]string{
		"--release-root", "/srv/looper",
		"--release", "candidate-release",
		"--bundle", "/srv/backups/rollback",
	})
	if err != nil {
		t.Fatalf("parseUpgradeVerifyStartArgs() error = %v", err)
	}
	if root != "/srv/looper" || release != "candidate-release" || bundle != "/srv/backups/rollback" {
		t.Fatalf("parseUpgradeVerifyStartArgs() = (%q, %q, %q)", root, release, bundle)
	}
	if _, _, _, err := parseUpgradeVerifyStartArgs([]string{"--release-root", "/srv/looper", "--release", "candidate-release"}); err == nil {
		t.Fatal("parseUpgradeVerifyStartArgs() without bundle error = nil")
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

func TestFindUpgradeRestoreOpenPIDsSkipsMissingTargets(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "database.sqlite")
	missing := filepath.Join(root, "database.sqlite-wal")
	if err := os.WriteFile(existing, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	argsLog := filepath.Join(root, "lsof-args")
	t.Setenv("LOOPER_TEST_LSOF_LOG", argsLog)
	lsof := filepath.Join(root, "lsof")
	if err := os.WriteFile(lsof, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$LOOPER_TEST_LSOF_LOG\"\nprintf '123\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	pids, err := findUpgradeRestoreOpenPIDs(context.Background(), []string{existing, missing})
	if err != nil {
		t.Fatal(err)
	}
	if len(pids) != 1 || pids[0] != 123 {
		t.Fatalf("pids = %#v, want [123]", pids)
	}
	args, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), missing) {
		t.Fatalf("lsof received missing target %q: args = %q", missing, args)
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

func completeUpgradeIdentity(commit string) version.Info {
	ts := "2026-07-31T12:00:00Z"
	dirty := false
	c := commit
	return version.Info{
		Version: "1.2.3",
		Metadata: version.BuildMetadata{
			VersionSource:  "test",
			Channel:        "release",
			APIVersion:     "v1",
			GitCommitSHA:   &c,
			BuildTimestamp: &ts,
			Dirty:          &dirty,
		},
	}
}

func upgradeTestDaemon(t *testing.T, identity version.Info) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/version":
			writeEnvelope(w, http.StatusOK, upgradeDaemonVersion{Version: identity.Version, Build: identity.Metadata})
		case "/api/v1/status":
			writeEnvelope(w, http.StatusOK, map[string]any{
				"service": map[string]any{
					"healthy": true, "admissionState": "ready", "build": identity.Metadata,
					"recovery": map[string]any{"outstanding": map[string]any{"quarantinedActiveExecutions": 1, "quarantinedRunningRuns": 0}},
				},
				"storage":   map[string]any{"schemaVersion": "0022", "pendingMigrations": []string{}, "healthy": true},
				"scheduler": map[string]any{"activeRuns": 2, "runningItems": 2},
			})
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
	if !identity.Complete() {
		t.Fatalf("test identity must be complete: %#v", identity)
	}
	return writeRawIdentityProgram(t, identity)
}

func writeRawIdentityProgram(t *testing.T, identity version.Info) string {
	t.Helper()
	encoded, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "identity")
	// Accept both CLI "version --json" and daemon "--version-json" shapes by always printing identity JSON.
	script := "#!/bin/sh\nprintf '%s\\n' '" + string(encoded) + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
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
	c := commit
	identity.Metadata.GitCommitSHA = &c
	return identity
}

func writeUpgradeBundleFile(t *testing.T, directory, name, contents string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func createUpgradeRestoreBundle(t *testing.T) string {
	t.Helper()
	bundle, _, _ := createUpgradeRestoreBundleWithSource(t)
	return bundle
}

func createUpgradeRestoreBundleWithSource(t *testing.T) (bundleDir, databasePath, configPath string) {
	t.Helper()
	root := t.TempDir()
	configPath = writeUpgradeBundleFile(t, root, "config.toml", "[server]\n")
	cli := writeUpgradeBundleFile(t, root, "looper", "cli")
	daemon := writeUpgradeBundleFile(t, root, "looperd", "daemon")
	databasePath = filepath.Join(root, "looper.sqlite")
	bundle, err := upgradebackup.Create(context.Background(), upgradebackup.Input{RootDir: filepath.Join(root, "backups"), ConfigPath: configPath, DatabasePath: databasePath, CLIBinaryPath: cli, DaemonBinaryPath: daemon, Snapshot: func(context.Context) (string, error) {
		return writeUpgradeBundleFile(t, root, "snapshot.sqlite", "sqlite"), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return bundle.Directory, databasePath, configPath
}
