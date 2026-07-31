package projects

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/storage"
)

func TestServiceSyncConfiguredIgnoresProviderDetectionForGitHubDefault(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	repoPath := "/tmp/odcrew"
	baseBranch := "main"
	service := &Service{
		Repos: repos,
		Now:   func() time.Time { return now },
		DetectRepo: func(context.Context, string) (DetectedRepo, error) {
			return DetectedRepo{Repo: "core/odcrew", Provider: "ghes-main"}, nil
		},
	}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []config.ProjectRefConfig{{ID: "odcrew", Name: "ODCrew", RepoPath: repoPath, BaseBranch: &baseBranch}}

	if err := service.SyncConfigured(context.Background(), cfg, now); err != nil {
		t.Fatalf("SyncConfigured() error = %v", err)
	}
	project, err := repos.Projects.GetByID(context.Background(), "odcrew")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if project == nil || metadataString(parseMetadata(project.MetadataJSON), "repo") != "" {
		t.Fatalf("stored project = %#v, want no GitHub repo inferred from a non-default provider origin", project)
	}
}

func TestServiceAddProjectAllowsExplicitGitHubRepoOnDetectedProviderOrigin(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	tokenEnv := "LOOPER_GHES_TOKEN"
	cfg.Providers = []config.ProviderConfig{{ID: "ghes-main", Kind: config.ProviderKindGitHub, BaseURL: "https://code.example.com", TokenEnv: &tokenEnv}}
	service := &Service{
		DB:     coordinator.DB(),
		Repos:  repos,
		Config: cfg,
		Now:    time.Now,
		DetectRepo: func(context.Context, string) (DetectedRepo, error) {
			return DetectedRepo{Repo: "other/checkout", Provider: "ghes-main"}, nil
		},
	}
	repo := "github-org/repo"

	result, err := service.AddProject(context.Background(), AddInput{
		ID: "project", Name: "Project", RepoPath: "/tmp/project", Repo: &repo,
	})
	if err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}
	metadata := parseMetadata(result.Project.MetadataJSON)
	if got := metadataString(metadata, "repo"); got != repo {
		t.Fatalf("stored repo = %q, want explicit repo %q", got, repo)
	}
	if got := metadataString(metadata, "provider"); got != "" {
		t.Fatalf("stored provider = %q, want GitHub default", got)
	}
}

func TestServiceAddProjectRejectsDetectedProviderBinding(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	published := false
	service := &Service{
		DB:     coordinator.DB(),
		Repos:  repos,
		Config: cfg,
		Now:    time.Now,
		DetectRepo: func(context.Context, string) (DetectedRepo, error) {
			return DetectedRepo{Repo: "owner/repo", Provider: "forgejo-main"}, nil
		},
		PublishProjects: func([]config.ProjectRefConfig) { published = true },
	}

	_, err = service.AddProject(context.Background(), AddInput{ID: "project", Name: "Project", RepoPath: "/tmp/project"})
	if err == nil || !strings.Contains(err.Error(), "provider bindings are config-file authority") {
		t.Fatalf("AddProject() error = %v, want config-file authority guidance", err)
	}
	stored, getErr := repos.Projects.GetByID(context.Background(), "project")
	if getErr != nil {
		t.Fatalf("GetByID() error = %v", getErr)
	}
	if stored != nil || published {
		t.Fatalf("stored = %#v, published = %v; want rejection before persistence", stored, published)
	}
}

// A checkout whose origin is not github.com used to register with no repository
// and no warning: the scheduler skipped it on every tick, so registration looked
// successful while nothing ever ran. Both halves of that gap are now reported.
func TestServiceAddProjectWarnsWhenNoRepositoryCouldBeDetermined(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	service := &Service{
		DB:     coordinator.DB(),
		Repos:  repos,
		Config: cfg,
		Now:    func() time.Time { return now },
		DetectRepo: func(context.Context, string) (DetectedRepo, error) {
			return DetectedRepo{}, fmt.Errorf(`origin host "code.example.com" is not github.com; looper drives GitHub only, so the repository cannot be detected — pass the repository explicitly as owner/name`)
		},
		PublishProjects: func([]config.ProjectRefConfig) {},
	}

	added, err := service.AddProject(context.Background(), AddInput{
		ID: "demo", Name: "Demo", RepoPath: "/tmp/demo",
	})
	if err != nil {
		t.Fatalf("AddProject() error = %v, want registration to succeed with warnings", err)
	}

	joined := strings.Join(added.Warnings, "\n")
	if !strings.Contains(joined, "is not github.com") {
		t.Fatalf("warnings = %#v, want the unrecognized origin host reported", added.Warnings)
	}
	if !strings.Contains(joined, "no automation will run") {
		t.Fatalf("warnings = %#v, want the inert-project consequence reported", added.Warnings)
	}
}

// The inert-project warning must never route an operator through RemoveProject.
// That call terminates every loop and cancels every queue item for the project,
// and "terminated" has no outbound transition, so following the advice would
// destroy the automation state the operator is trying to restore.
func TestInertProjectWarningNeverAdvisesTheDestructivePath(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	service := &Service{
		DB: coordinator.DB(), Repos: repos, Config: cfg,
		Now:             func() time.Time { return now },
		PublishProjects: func([]config.ProjectRefConfig) {},
	}

	added, err := service.AddProject(context.Background(), AddInput{
		ID: "demo", Name: "Demo", RepoPath: t.TempDir(), IDSource: "derived",
	})
	if err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}

	joined := strings.Join(added.Warnings, "\n")
	if !strings.Contains(joined, "no automation will run") {
		t.Fatalf("warnings = %#v, want the inert-project consequence reported", added.Warnings)
	}
	for _, destructive := range []string{"DELETE", "delete", "remove", "Remove"} {
		if strings.Contains(joined, destructive) {
			t.Fatalf("warning names %q, but removal terminates every loop for the project: %q", destructive, joined)
		}
	}
	// The reset that re-registration does cause must be disclosed, not glossed.
	for _, disclosed := range []string{"name", "baseBranch", "snapshotMode", "defaults"} {
		if !strings.Contains(joined, disclosed) {
			t.Fatalf("warning omits %q; the fields re-registration resets must be named: %q", disclosed, joined)
		}
	}
}

// Explicit IDs must have the same non-destructive repair route as derived
// IDs. The API labels a supplied id as explicit, so accepting only derived
// IDs would make the warning's re-registration advice impossible to follow.
func TestServiceAddProjectRepairsExplicitIDAtSameCheckout(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	repoPath := t.TempDir()
	service := &Service{
		DB:              coordinator.DB(),
		Repos:           repos,
		Config:          cfg,
		Now:             time.Now,
		PublishProjects: func([]config.ProjectRefConfig) {},
	}

	initial, err := service.AddProject(context.Background(), AddInput{
		ID: "demo", IDSource: "explicit", Name: "Demo", RepoPath: repoPath,
	})
	if err != nil {
		t.Fatalf("initial AddProject() error = %v", err)
	}
	if !strings.Contains(strings.Join(initial.Warnings, "\n"), "no automation will run") {
		t.Fatalf("initial warnings = %#v, want inert-project warning", initial.Warnings)
	}

	repo := "owner/demo"
	repaired, err := service.AddProject(context.Background(), AddInput{
		ID: "demo", IDSource: "explicit", Name: "Demo", RepoPath: repoPath, Repo: &repo,
	})
	if err != nil {
		t.Fatalf("repair AddProject() error = %v", err)
	}
	if repaired.Project.CreatedAt != initial.Project.CreatedAt {
		t.Fatalf("repaired CreatedAt = %q, want existing project creation time %q", repaired.Project.CreatedAt, initial.Project.CreatedAt)
	}
	if repaired.Repo == nil || *repaired.Repo != repo {
		t.Fatalf("repaired repo = %#v, want %q", repaired.Repo, repo)
	}
}
