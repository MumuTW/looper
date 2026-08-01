package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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

func TestUpgradePostStartBlocksIgnoresResumedWork(t *testing.T) {
	// CompleteStartup marks admission ready and wakes the scheduler before
	// verify-start can observe status, so queued work left in the drained
	// database can legitimately be running here; it must not fail the cutover.
	build := completeUpgradeIdentity("commit-a")
	started := "2026-08-01T12:00:00Z"
	report := upgradePostStartReport{
		ExpectedBuild:   build,
		ExpectedRelease: "release-b",
		DaemonBuild:     build,
		CurrentRelease:  "release-b",
		StartedEvent:    true,
	}
	report.Status.Service.Healthy = true
	report.Status.Service.Version = build.Version
	report.Status.Service.Build = build.Metadata
	report.Status.Service.AdmissionState = "ready"
	report.Status.Service.StartedAt = &started
	report.Status.Storage.Healthy = true
	report.Status.Scheduler.ActiveRuns = 2
	report.Status.Scheduler.RunningItems = 1
	if blocks := upgradePostStartBlocks(report); len(blocks) != 0 {
		t.Fatalf("blocks = %v, want none for a healthy cutover with resumed work", blocks)
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
