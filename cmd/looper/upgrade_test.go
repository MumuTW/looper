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
			if !report.CurrentPairMatches || report.TargetPairMatches != test.wantPairMatches || !report.TargetIdentityValid || !report.TargetConfigCompatible || report.Relationship != test.wantRelation {
				t.Fatalf("report = %#v", report)
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
	if report.TargetConfigCompatible || report.TargetConfigError != "configuration schema rejected" {
		t.Fatalf("config result = (%v, %q)", report.TargetConfigCompatible, report.TargetConfigError)
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
