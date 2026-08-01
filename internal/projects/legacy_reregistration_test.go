package projects

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/storage"
)

// TestServiceAddProjectReregistrationRequiresStanceForRepoMetadata verifies
// the shared materialization/Add path enforces the same stance requirement as
// UpdateProject. Re-registering a legacy inert record with repository metadata
// but no validation stance must be rejected while no coding agent and no legacy
// defaults.validationCommands are configured; otherwise the unstanced
// repo-bearing row is persisted and published, and enabling Worker/Fixer later
// turns it into a startup failure with no running PATCH API left to repair it.
func TestServiceAddProjectReregistrationRequiresStanceForRepoMetadata(t *testing.T) {
	t.Parallel()

	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	// No coding agent and no legacy default commands: an unstanced legacy
	// project is quarantined independently of agent configuration.
	cfg.Agent.Vendor = nil
	cfg.Defaults.ValidationCommands = nil
	catalog := NewCatalog(cfg)
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	service := &Service{
		Repos:             repos,
		ConfigSource:      catalog,
		PublishProjects:   catalog.Publish,
		Now:               func() time.Time { return now },
		ScheduleDiscovery: func(func()) {},
	}

	nowISO := currentISO(func() time.Time { return now })
	baseBranch := "develop"
	legacyMetadata := `{"repo":null,"worktreeRoot":"/tmp/worktrees","source":"api","registrationDiscovery":{"status":"succeeded","snapshotMode":"off"}}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "legacy_inert", Name: "Legacy", RepoPath: "/tmp/legacy", BaseBranch: &baseBranch, MetadataJSON: &legacyMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	repo := "acme/app"
	if _, err := service.AddProject(context.Background(), AddInput{ID: "legacy_inert", Name: "Legacy", RepoPath: "/tmp/legacy", BaseBranch: "develop", Repo: &repo}); err == nil {
		t.Fatal("AddProject(repo without stance) error = nil, want validation rejection")
	} else if !strings.Contains(err.Error(), "validation commands or optOut=true") {
		t.Fatalf("AddProject(repo without stance) error = %v, want stance requirement", err)
	}
	stored, err := repos.Projects.GetByID(context.Background(), "legacy_inert")
	if err != nil || stored == nil || stored.MetadataJSON == nil {
		t.Fatalf("Projects.GetByID() after rejected re-registration = %#v, %v", stored, err)
	}
	if strings.Contains(*stored.MetadataJSON, `"repo":"acme/app"`) {
		t.Fatalf("metadata after rejected re-registration = %s, want legacy inert record unchanged", *stored.MetadataJSON)
	}
	if got := catalog.Snapshot().Projects; len(got) != 0 {
		t.Fatalf("catalog after rejected re-registration = %#v, want legacy inert project quarantined", got)
	}

	if _, err := service.AddProject(context.Background(), AddInput{ID: "legacy_inert", Name: "Legacy", RepoPath: "/tmp/legacy", BaseBranch: "develop", Repo: &repo, Validation: &config.ProjectValidationConfig{OptOut: true}}); err != nil {
		t.Fatalf("AddProject(repo with stance) error = %v", err)
	}
	stored, err = repos.Projects.GetByID(context.Background(), "legacy_inert")
	if err != nil || stored == nil || stored.MetadataJSON == nil {
		t.Fatalf("Projects.GetByID() after repair = %#v, %v", stored, err)
	}
	if !strings.Contains(*stored.MetadataJSON, `"repo":"acme/app"`) || !strings.Contains(*stored.MetadataJSON, `"validation":{"optOut":true}`) {
		t.Fatalf("metadata after repair = %s, want repo and explicit validation opt-out", *stored.MetadataJSON)
	}
	if got := catalog.Snapshot().Projects; len(got) != 1 || got[0].ID != "legacy_inert" {
		t.Fatalf("catalog after repair = %#v, want repaired project published", got)
	}
}
