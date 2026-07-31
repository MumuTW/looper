package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/MumuTW/looper/internal/upgradebackup"
	"github.com/MumuTW/looper/internal/version"
)

// TestUpgradeCutoverContract exercises the supported operator checkpoints as
// one deterministic command sequence. It deliberately uses a fake daemon:
// this verifies command ownership and rollback boundaries without starting a
// second process against the production SQLite file.
func TestUpgradeCutoverContractRestoresMatchingSnapshotAfterFailedStart(t *testing.T) {
	previous := releasedUpgradeIdentity("1.2.3", "aaaaaaa")
	candidate := releasedUpgradeIdentity("1.2.4", "bbbbbbb")
	bundle := createUpgradeRestoreBundle(t)
	verifiedBundle, err := upgradebackup.Verify(bundle)
	if err != nil {
		t.Fatal(err)
	}
	source, err := upgradebackup.RestoreSource(verifiedBundle.Manifest)
	if err != nil {
		t.Fatal(err)
	}

	current := previous
	healthy := true
	backupRequested := false
	drainRequested := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/upgrade/backup":
			if r.Method != http.MethodPost {
				t.Fatalf("backup request method = %s", r.Method)
			}
			backupRequested = true
			writeEnvelope(w, http.StatusOK, upgradeBackupResult{Directory: bundle, Manifest: verifiedBundle.Manifest})
		case "/api/v1/upgrade/drain":
			if r.Method != http.MethodPost {
				t.Fatalf("drain request method = %s", r.Method)
			}
			drainRequested = true
			writeEnvelope(w, http.StatusOK, upgradeDrainResult{AdmissionState: "draining", Drained: true})
		case "/api/v1/version":
			writeEnvelope(w, http.StatusOK, upgradeDaemonVersion{Version: current.Version, Build: current.Metadata})
		case "/api/v1/status":
			writeEnvelope(w, http.StatusOK, upgradeCutoverStatus(current, healthy))
		case "/api/v1/projects":
			writeEnvelope(w, http.StatusOK, map[string]any{"items": []map[string]any{{"id": "project_1"}}})
		case "/api/v1/events/notification/looperd":
			writeEnvelope(w, http.StatusOK, map[string]any{"items": []map[string]any{{"eventType": "looperd.started"}}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	configForDaemon(t, server.URL)

	releaseRoot, previousReleaseID := stageReleaseForUpgradeTest(t, previous)
	stdout := &bytes.Buffer{}
	if err := runUpgrade(context.Background(), nil, []string{"backup"}, stdout); err != nil {
		t.Fatal(err)
	}
	if !backupRequested {
		t.Fatal("backup endpoint was not requested before cutover")
	}
	stdout.Reset()
	if err := runUpgrade(context.Background(), nil, []string{"drain", "--deadline", "1s"}, stdout); err != nil {
		t.Fatal(err)
	}
	if !drainRequested {
		t.Fatal("drain endpoint was not requested before release activation")
	}
	stdout.Reset()
	if err := runUpgrade(context.Background(), nil, []string{"stage-release", "--target-looper", writeIdentityProgram(t, candidate), "--target-looperd", writeIdentityProgram(t, candidate), "--release-root", releaseRoot}, stdout); err != nil {
		t.Fatal(err)
	}
	candidateReleaseID := releaseIDFor(candidate)
	stdout.Reset()
	if err := runUpgrade(context.Background(), nil, []string{"activate-release", "--release-root", releaseRoot, "--release", candidateReleaseID}, stdout); err != nil {
		t.Fatal(err)
	}
	current = candidate
	stdout.Reset()
	if err := runUpgrade(context.Background(), nil, []string{"verify-start", "--release-root", releaseRoot, "--release", candidateReleaseID}, stdout); err != nil {
		t.Fatalf("verify candidate start: %v", err)
	}

	// A failed restart is not a successful upgrade. Restore the exact snapshot
	// before returning the release pointer to the binary that made it.
	healthy = false
	stdout.Reset()
	if err := runUpgrade(context.Background(), nil, []string{"verify-start", "--release-root", releaseRoot, "--release", candidateReleaseID}, stdout); err == nil {
		t.Fatal("verify failed candidate start = nil, want health failure")
	}
	if err := os.WriteFile(source.ConfigPath, []byte("candidate config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source.DatabasePath, []byte("candidate database"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalOpenPIDs := upgradeRestoreOpenPIDs
	t.Cleanup(func() { upgradeRestoreOpenPIDs = originalOpenPIDs })
	upgradeRestoreOpenPIDs = func(context.Context, []string) ([]int, error) { return nil, nil }
	stdout.Reset()
	if err := runUpgrade(context.Background(), nil, []string{"restore", "--bundle", bundle, "--confirm"}, stdout); err != nil {
		t.Fatalf("restore matching snapshot: %v", err)
	}
	if err := runUpgrade(context.Background(), nil, []string{"activate-release", "--release-root", releaseRoot, "--release", previousReleaseID}, stdout); err != nil {
		t.Fatalf("activate previous release: %v", err)
	}
	current = previous
	healthy = true
	stdout.Reset()
	if err := runUpgrade(context.Background(), nil, []string{"verify-start", "--release-root", releaseRoot, "--release", previousReleaseID}, stdout); err != nil {
		t.Fatalf("verify restored previous release: %v", err)
	}

	config, err := os.ReadFile(source.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(config) != "[server]\n" {
		t.Fatalf("restored config = %q", config)
	}
	database, err := os.ReadFile(source.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(database) != "sqlite" {
		t.Fatalf("restored database = %q", database)
	}
}

func upgradeCutoverStatus(identity version.Info, healthy bool) map[string]any {
	return map[string]any{
		"service": map[string]any{
			"healthy":        healthy,
			"version":        identity.Version,
			"build":          identity.Metadata,
			"admissionState": "ready",
			"startedAt":      "2026-07-31T12:35:00.000Z",
			"recovery": map[string]any{"outstanding": map[string]any{
				"quarantinedActiveExecutions": 0,
				"quarantinedRunningRuns":      0,
			}},
		},
		"storage":   map[string]any{"schemaVersion": "0021", "pendingMigrations": []string{}, "healthy": true},
		"scheduler": map[string]any{"activeRuns": 0, "runningItems": 0},
	}
}
