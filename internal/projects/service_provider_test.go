package projects

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
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

func TestServiceAddProjectRejectsDetectedRepoFromMismatchedProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		detectedProvider string
		wantMessage      string
	}{
		{name: "GitHub origin", detectedProvider: "", wantMessage: "belongs to the GitHub default"},
		{name: "different provider", detectedProvider: "ghes-other", wantMessage: "belongs to ghes-other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			coordinator := openCoordinator(t)
			repos := storage.NewRepositories(coordinator.DB())
			cfg, err := config.DefaultConfig(t.TempDir())
			if err != nil {
				t.Fatalf("DefaultConfig() error = %v", err)
			}
			tokenEnv := "LOOPER_GHES_TOKEN"
			cfg.Providers = []config.ProviderConfig{
				{ID: "ghes-main", Kind: config.ProviderKindGitHub, BaseURL: "https://code.example.com", TokenEnv: &tokenEnv},
				{ID: "ghes-other", Kind: config.ProviderKindGitHub, BaseURL: "https://other.example.com", TokenEnv: &tokenEnv},
			}
			published := false
			service := &Service{
				DB:     coordinator.DB(),
				Repos:  repos,
				Config: cfg,
				Now:    time.Now,
				DetectRepo: func(context.Context, string) (DetectedRepo, error) {
					return DetectedRepo{Repo: "owner/repo", Provider: tt.detectedProvider}, nil
				},
				PublishProjects: func([]config.ProjectRefConfig) { published = true },
			}
			provider := "ghes-main"

			_, err = service.AddProject(context.Background(), AddInput{
				ID: "project", Name: "Project", RepoPath: "/tmp/project", Provider: &provider,
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantMessage) {
				t.Fatalf("AddProject() error = %v, want %q", err, tt.wantMessage)
			}
			stored, getErr := repos.Projects.GetByID(context.Background(), "project")
			if getErr != nil {
				t.Fatalf("GetByID() error = %v", getErr)
			}
			if stored != nil || published {
				t.Fatalf("stored = %#v, published = %v; want rejection before persistence", stored, published)
			}
		})
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

// The repair path the inert-project warning advertises must not destroy what it
// repairs. RemoveProject terminates every loop and cancels every queue item for
// a project, and "terminated" has no outbound transition, so a DELETE-then-POST
// repair would permanently kill the automation the operator is trying to
// restore. Re-registering the same repoPath updates in place instead.
func TestAdvertisedRepairPreservesProjectIdentityAndLoops(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	nowISO := now.UTC().Format(time.RFC3339Nano)
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	repoPath := t.TempDir()
	projectID := deriveProjectIDFromRepoPath(repoPath)
	service := &Service{
		DB:              coordinator.DB(),
		Repos:           repos,
		Config:          cfg,
		Now:             func() time.Time { return now },
		PublishProjects: func([]config.ProjectRefConfig) {},
	}

	registered, err := service.AddProject(context.Background(), AddInput{
		ID: projectID, Name: "Custom Name", RepoPath: repoPath, BaseBranch: "develop", IDSource: "derived",
	})
	if err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}
	if registered.Repo != nil {
		t.Fatalf("registered.Repo = %v, want no repository detected", registered.Repo)
	}

	targetID := "pr:acme/app:1"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: "loop_existing", Seq: 1, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &targetID, Status: "queued",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	// The advertised repair: same repoPath, explicit repo, existing settings.
	// IDSource is "explicit" because the advice tells the operator to send the
	// project's current id; that must be an in-place update, not a collision.
	repo := "acme/app"
	repaired, err := service.AddProject(context.Background(), AddInput{
		ID: projectID, Name: "Custom Name", RepoPath: repoPath, BaseBranch: "develop", IDSource: "explicit", Repo: &repo,
	})
	if err != nil {
		t.Fatalf("repair AddProject() error = %v, want an in-place update", err)
	}
	if repaired.Project.ID != projectID {
		t.Fatalf("repaired project id = %q, want the original %q", repaired.Project.ID, projectID)
	}
	if repaired.Project.Archived {
		t.Fatal("repaired project is archived; the repair must not remove and recreate it")
	}
	if repaired.Repo == nil || *repaired.Repo != repo {
		t.Fatalf("repaired.Repo = %v, want %q", repaired.Repo, repo)
	}
	if repaired.Project.Name != "Custom Name" {
		t.Fatalf("repaired name = %q, want the supplied name preserved", repaired.Project.Name)
	}

	loop, err := repos.Loops.GetByID(context.Background(), "loop_existing")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil {
		t.Fatal("existing loop was removed by the repair")
	}
	if loop.Status == "terminated" {
		t.Fatal("existing loop was terminated by the repair; the advertised path must be non-destructive")
	}
}
