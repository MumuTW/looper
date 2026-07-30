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
