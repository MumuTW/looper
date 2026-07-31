package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/storage"
)

// TestSchedulerProjectsForCapturedCatalogRecognizesAgentlessQuarantine
// verifies the scheduler's quarantine predicate matches the agent-independent
// quarantine rule in validateCatalogValidationPolicies. With no Worker/Fixer
// agent and no legacy defaults.validationCommands, an unstanced legacy project
// is intentionally absent from the runtime catalog; the scheduler must treat
// that missing binding as current so durable sticky retries and otherwise
// runnable lanes are not blocked until the legacy record is repaired.
func TestSchedulerProjectsForCapturedCatalogRecognizesAgentlessQuarantine(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(root, "quarantine.sqlite"), t.TempDir())
	t.Cleanup(func() { _ = coordinator.Close() })
	repositories := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	baseBranch := "develop"
	legacyMetadata := `{"repo":null,"worktreeRoot":"/tmp/worktrees","source":"api","registrationDiscovery":{"status":"succeeded","snapshotMode":"off"}}`
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "legacy_inert", Name: "Legacy", RepoPath: filepath.Join(root, "legacy"), BaseBranch: &baseBranch, MetadataJSON: &legacyMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	// No coding agent and no legacy default commands: the unstanced legacy
	// project is quarantined independently of agent configuration.
	cfg.Agent.Vendor = nil
	cfg.Defaults.ValidationCommands = nil
	cfg.Projects = nil
	snapshotCfg := cfg

	_, catalogCurrent, err := schedulerProjectsForCapturedCatalog(context.Background(), defaultSchedulerTickInput{
		Repos:  repositories,
		Now:    func() time.Time { return now },
		Config: &snapshotCfg,
	})
	if err != nil {
		t.Fatalf("schedulerProjectsForCapturedCatalog() error = %v", err)
	}
	if !catalogCurrent {
		t.Fatal("catalogCurrent = false, want true: scheduler must recognize agentless quarantine instead of deferring every claim pass")
	}

	// With legacy default commands present the unstanced legacy project is
	// published, so the missing binding is genuinely stale and the scheduler
	// must defer until the catalog catches up.
	withDefaults := config.CloneConfig(cfg)
	withDefaults.Defaults.ValidationCommands = []string{"go test ./..."}
	withDefaults.Projects = nil
	withDefaultsSnapshot := withDefaults
	_, catalogCurrentWithDefaults, err := schedulerProjectsForCapturedCatalog(context.Background(), defaultSchedulerTickInput{
		Repos:  repositories,
		Now:    func() time.Time { return now },
		Config: &withDefaultsSnapshot,
	})
	if err != nil {
		t.Fatalf("schedulerProjectsForCapturedCatalog(defaults) error = %v", err)
	}
	if catalogCurrentWithDefaults {
		t.Fatal("catalogCurrent with legacy defaults = true, want false: published legacy project missing from catalog must defer")
	}
}

// TestQuarantinedProjectIDsForClaimPreservesStickyRetryScope verifies the
// catalog-to-claim contract: an unstanced legacy project quarantined by
// validateCatalogValidationPolicies (empty defaults.validationCommands, no
// catalog binding) is returned by quarantinedProjectIDsForClaim and threaded
// through codingClaimProjectScope as stickyOnlyProjectIDs, so durable sticky
// retries remain claimable without admitting fresh work. With legacy default
// commands present the project is published and nothing is quarantined.
func TestQuarantinedProjectIDsForClaimPreservesStickyRetryScope(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(root, "claim_scope.sqlite"), t.TempDir())
	t.Cleanup(func() { _ = coordinator.Close() })
	repositories := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	baseBranch := "develop"
	legacyMetadata := `{"repo":null,"worktreeRoot":"/tmp/worktrees","source":"api","registrationDiscovery":{"status":"succeeded","snapshotMode":"off"}}`
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "legacy_inert", Name: "Legacy", RepoPath: filepath.Join(root, "legacy"), BaseBranch: &baseBranch, MetadataJSON: &legacyMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Agent.Vendor = nil
	cfg.Defaults.ValidationCommands = nil
	cfg.Projects = nil
	snapshotCfg := cfg

	quarantined, err := quarantinedProjectIDsForClaim(context.Background(), defaultSchedulerTickInput{
		Repos:  repositories,
		Now:    func() time.Time { return now },
		Config: &snapshotCfg,
	})
	if err != nil {
		t.Fatalf("quarantinedProjectIDsForClaim() error = %v", err)
	}
	if len(quarantined) != 1 || quarantined[0] != "legacy_inert" {
		t.Fatalf("quarantinedProjectIDsForClaim() = %v, want [legacy_inert]", quarantined)
	}

	projectScopedTypes, runnableProjectIDs, stickyOnlyProjectIDs := codingClaimProjectScope(&snapshotCfg, quarantined)
	if got, want := projectScopedTypes, []string{"fixer", "worker"}; !sliceEqual(got, want) {
		t.Fatalf("projectScopedTypes = %v, want %v", got, want)
	}
	if len(runnableProjectIDs) != 0 {
		t.Fatalf("runnableProjectIDs = %v, want empty: quarantined project is absent from the catalog", runnableProjectIDs)
	}
	if len(stickyOnlyProjectIDs) != 1 || stickyOnlyProjectIDs[0] != "legacy_inert" {
		t.Fatalf("stickyOnlyProjectIDs = %v, want [legacy_inert]", stickyOnlyProjectIDs)
	}

	// With legacy default commands present the project is published, so no
	// project is quarantined and the sticky-only scope is empty.
	withDefaults := config.CloneConfig(cfg)
	withDefaults.Defaults.ValidationCommands = []string{"go test ./..."}
	withDefaults.Projects = nil
	withDefaultsSnapshot := withDefaults
	notQuarantined, err := quarantinedProjectIDsForClaim(context.Background(), defaultSchedulerTickInput{
		Repos:  repositories,
		Now:    func() time.Time { return now },
		Config: &withDefaultsSnapshot,
	})
	if err != nil {
		t.Fatalf("quarantinedProjectIDsForClaim(defaults) error = %v", err)
	}
	if len(notQuarantined) != 0 {
		t.Fatalf("quarantinedProjectIDsForClaim(defaults) = %v, want empty: published legacy project is not quarantined", notQuarantined)
	}
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
