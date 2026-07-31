package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/storage"
)

// TestRuntimeReloadPreservesMaterializedCatalogWithoutRematerializing drives
// the supported hot-reload path (ReloadConfig) the way a common agent.vendor
// flip does. A hot reload cannot change filtered catalog membership — the only
// policy input that does is defaults.validationCommands, which is restart-bound
// and rejected before the publication boundary, and an unstanced legacy project
// is quarantined independently of agent configuration. The previously
// materialized SQLite slice must therefore be preserved as-is instead of
// re-reading SQLite and running post-publication hooks under configBoundary.
//
// The out-of-band stanced record inserted after startup is the signal: if the
// reload re-materialized, it would appear in the catalog. The original stanced
// project stays published and the legacy inert row stays quarantined, proving
// the preserved slice is coherent without re-materialization or hook work.
func TestRuntimeReloadPreservesMaterializedCatalogWithoutRematerializing(t *testing.T) {
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
	legacyMetadata := `{"repo":null,"worktreeRoot":"/tmp/worktrees","source":"api","registrationDiscovery":{"status":"succeeded","snapshotMode":"off"}}`
	if err := seedRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "legacy_inert", Name: "Legacy", RepoPath: filepath.Join(workingDir, "legacy"), BaseBranch: &baseBranch, MetadataJSON: &legacyMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(legacy) error = %v", err)
	}
	stancedMetadata := `{"repo":"acme/one","worktreeRoot":"/tmp/worktrees","source":"api","validation":{"optOut":true},"registrationDiscovery":{"status":"succeeded","snapshotMode":"off"}}`
	if err := seedRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "stanced_one", Name: "One", RepoPath: filepath.Join(workingDir, "one"), BaseBranch: &baseBranch, MetadataJSON: &stancedMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(stanced) error = %v", err)
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
	if got := rt.Config().Projects; len(got) != 1 || got[0].ID != "stanced_one" {
		t.Fatalf("runtime catalog projects at startup = %#v, want only stanced_one (legacy inert quarantined)", got)
	}

	// Insert a second stanced project out-of-band after startup. Re-materialize
	// on reload would publish it; preserving the slice leaves it absent.
	extraCoordinator, err := storage.OpenSQLiteCoordinator(context.Background(), cfg.Storage.DBPath, storage.SQLiteCoordinatorOptions{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	extraRepos := storage.NewRepositories(extraCoordinator.DB())
	extraMetadata := `{"repo":"acme/two","worktreeRoot":"/tmp/worktrees","source":"api","validation":{"optOut":true},"registrationDiscovery":{"status":"succeeded","snapshotMode":"off"}}`
	if err := extraRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "stanced_two", Name: "Two", RepoPath: filepath.Join(workingDir, "two"), BaseBranch: &baseBranch, MetadataJSON: &extraMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(extra) error = %v", err)
	}
	if err := extraCoordinator.Close(); err != nil {
		t.Fatalf("extra coordinator Close() error = %v", err)
	}

	// A common agent.vendor hot reload: revoke the only coding agent. This is
	// the reachable coding-role flip the removed re-materialization trigger
	// fired on; it must not re-read SQLite or run post-publication hooks.
	loaded.Config.Agent.Vendor = nil
	if err := rt.ReloadConfig(context.Background()); err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}
	got := rt.Config().Projects
	if len(got) != 1 || got[0].ID != "stanced_one" {
		t.Fatalf("runtime catalog projects after reload = %#v, want preserved slice with only stanced_one (no re-materialization)", got)
	}
}
