package projects

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestServiceSyncConfiguredAllowsConfigToClaimArchivedAPIProject(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 31, 4, 0, 0, 0, time.UTC)
	createdAt := now.Add(-time.Hour).Format(time.RFC3339Nano)
	baseBranch := "main"
	metadata := `{"repo":"acme/old","source":"api"}`
	existing := storage.ProjectRecord{ID: "shared", Name: "Old API project", RepoPath: "/tmp/old", BaseBranch: &baseBranch, Archived: true, MetadataJSON: &metadata, CreatedAt: createdAt, UpdatedAt: createdAt}
	if err := repos.Projects.Upsert(context.Background(), existing); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers = []config.ProviderConfig{{ID: "acme", Kind: config.ProviderKindGitHub}}
	cfg.Projects = []config.ProjectRefConfig{{ID: "shared", Name: "Configured project", RepoPath: "/tmp/config", Provider: "acme", Repo: "acme/new"}}
	service := &Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }}

	if err := service.SyncConfigured(context.Background(), cfg, now); err != nil {
		t.Fatalf("SyncConfigured() error = %v", err)
	}
	stored, err := repos.Projects.GetByID(context.Background(), "shared")
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.Archived || stored.Name != "Configured project" || stored.RepoPath != "/tmp/config" {
		t.Fatalf("stored = %#v, want active config-owned replacement", stored)
	}
	if source := metadataString(parseMetadata(stored.MetadataJSON), "source"); source != "config" {
		t.Fatalf("stored source = %q, want config", source)
	}
	if provider := metadataString(parseMetadata(stored.MetadataJSON), "provider"); provider != "acme" {
		t.Fatalf("stored provider = %q, want config-owned acme binding", provider)
	}
}

func TestServiceSyncConfiguredActiveAPICollisionNamesSafeHandoff(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 31, 4, 0, 0, 0, time.UTC)
	baseBranch := "main"
	metadata := `{"repo":"acme/api","source":"api"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "shared", Name: "API", RepoPath: "/tmp/api", BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: now.Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Projects = []config.ProjectRefConfig{{ID: "shared", Name: "Configured", RepoPath: "/tmp/config"}}
	service := &Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }}

	err = service.SyncConfigured(context.Background(), cfg, now)
	var validationErr ProjectValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("SyncConfigured() error = %T %v, want ProjectValidationError", err, err)
	}
	for _, want := range []string{"API-managed project that is still active", "temporarily remove", "DELETE /api/v1/projects/shared"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("SyncConfigured() error = %q, want %q", err, want)
		}
	}
}
