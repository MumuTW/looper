package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/storage"
)

// TestRuntimeReloadRematerializesStoredProjects drives the shared publication
// boundary the way a hot-safe policy-requirement change would: the legacy
// default-commands fallback appears, so the previously quarantined stored row
// must re-enter the runtime catalog from SQLite instead of waiting for a
// restart or an unrelated project mutation. defaults.validationCommands is
// restart-bound through ReloadConfig, so the test holds configBoundary and
// calls the boundary function directly.
func TestRuntimeReloadRematerializesStoredProjects(t *testing.T) {
	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	cfg.Storage.BackupDir = &backupDir
	cfg.Defaults.ValidationCommands = nil
	vendor := config.AgentVendorOpenCode
	cfg.Agent.Vendor = &vendor

	startedAt := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	nowISO := formatJavaScriptISOString(startedAt)
	seedCoordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	seedRepos := storage.NewRepositories(seedCoordinator.DB())
	baseBranch := "develop"
	metadata := `{"repo":null,"worktreeRoot":"/tmp/worktrees","source":"api","registrationDiscovery":{"status":"succeeded","snapshotMode":"off"}}`
	if err := seedRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "legacy_inert", Name: "Legacy", RepoPath: filepath.Join(workingDir, "legacy"), BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := seedCoordinator.Close(); err != nil {
		t.Fatalf("seed coordinator Close() error = %v", err)
	}

	loaded := config.LoadedFileConfig{Config: cfg}
	rt := New(Options{
		Config:        cfg,
		InitialConfig: loaded,
		ReloadConfig: func() (config.LoadedFileConfig, error) {
			return loaded, nil
		},
		Logger: &testLogger{},
		Now:    func() time.Time { return startedAt },
		RunSchedulerTick: func(context.Context, Services) error {
			return nil
		},
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := rt.CompleteStartup(context.Background()); err != nil {
		rt.Stop("test cleanup")
		t.Fatalf("CompleteStartup() error = %v", err)
	}
	defer rt.Stop("test cleanup")
	if got := rt.Config().Projects; len(got) != 0 {
		t.Fatalf("runtime catalog projects = %#v, want legacy inert project quarantined", got)
	}

	next := loaded
	next.Config.Defaults.ValidationCommands = []string{"go test ./..."}
	rt.configBoundary.Lock()
	applyErr := rt.applyLoadedConfigBoundaryLocked(context.Background(), next, startedAt)
	rt.configBoundary.Unlock()
	if applyErr != nil {
		t.Fatalf("applyLoadedConfigBoundaryLocked() error = %v", applyErr)
	}
	if got := rt.Config().Projects; len(got) != 1 || got[0].ID != "legacy_inert" {
		t.Fatalf("runtime catalog projects after policy-requirement reload = %#v, want legacy project re-materialized", got)
	}
}

func TestStoredProjectPolicyRequirementChanged(t *testing.T) {
	t.Parallel()

	base, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	base.Defaults.ValidationCommands = nil
	vendor := config.AgentVendorOpenCode

	withAgent := config.CloneConfig(base)
	withAgent.Agent.Vendor = &vendor
	if !storedProjectPolicyRequirementChanged(base, withAgent) {
		t.Fatal("storedProjectPolicyRequirementChanged(agent configured) = false, want true")
	}
	if !storedProjectPolicyRequirementChanged(withAgent, base) {
		t.Fatal("storedProjectPolicyRequirementChanged(agent revoked) = false, want true")
	}

	withDefaults := config.CloneConfig(base)
	withDefaults.Defaults.ValidationCommands = []string{"go test ./..."}
	if !storedProjectPolicyRequirementChanged(base, withDefaults) {
		t.Fatal("storedProjectPolicyRequirementChanged(defaults added) = false, want true")
	}
	if !storedProjectPolicyRequirementChanged(withDefaults, base) {
		t.Fatal("storedProjectPolicyRequirementChanged(defaults removed) = false, want true")
	}

	if storedProjectPolicyRequirementChanged(base, base) {
		t.Fatal("storedProjectPolicyRequirementChanged(unchanged) = true, want false")
	}
	unrelated := config.CloneConfig(base)
	unrelated.Daemon.LogDir = filepath.Join(t.TempDir(), "logs")
	if storedProjectPolicyRequirementChanged(base, unrelated) {
		t.Fatal("storedProjectPolicyRequirementChanged(unrelated change) = true, want false")
	}
}
