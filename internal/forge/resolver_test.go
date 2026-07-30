package forge

import (
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestResolverCWDSelectionIsAuthoritative(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	githubWorktrees := filepath.Join(root, "github-worktrees")
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "ghes", Kind: config.ProviderKindGitHub, BaseURL: "https://ghe.example.test"}},
		Projects: []config.ProjectRefConfig{
			{ID: "github", Repo: "acme/shared", RepoPath: filepath.Join(root, "github"), WorktreeRoot: &githubWorktrees},
			{ID: "ghes", Provider: "ghes", Repo: "acme/shared", RepoPath: filepath.Join(root, "ghes")},
		},
	}

	selection, matched, err := NewResolver(cfg).ForLocation("acme/shared", filepath.Join(githubWorktrees, "feature"))
	if err != nil || !matched {
		t.Fatalf("ForLocation() = matched %v, err %v", matched, err)
	}
	if projectID, bound := selection.ProjectID(); !bound || projectID != "github" {
		t.Fatalf("ProjectID() = (%q, %v), want the configured GitHub CWD project, not the same-slug GHES project", projectID, bound)
	}
}

func TestResolverForProjectFallsBackToExplicitGitHubKindWhenProviderIsInvalid(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Projects: []config.ProjectRefConfig{{ID: "project", Provider: "missing", Repo: "acme/repo"}},
	}

	selection := NewResolver(cfg).ForProject("project")
	if !selection.Bound() || !selection.Capabilities().GitHubPullRequests {
		t.Fatalf("selection = %#v, want bound explicit GitHub fallback", selection)
	}
}

func TestResolverForLocationDoesNotMatchBlankRepository(t *testing.T) {
	t.Parallel()
	cfg := config.Config{Projects: []config.ProjectRefConfig{{ID: "unbound"}}}

	selection, matched, err := NewResolver(cfg).ForLocation(" ", "")
	if err != nil || matched || selection.Bound() {
		t.Fatalf("ForLocation(blank) = (%#v, %v, %v), want no match", selection, matched, err)
	}
}

func TestResolverDetachesCapturedConfig(t *testing.T) {
	t.Parallel()

	baseBranch := "main"
	worktreeRoot := t.TempDir()
	originalWorktreeRoot := worktreeRoot
	roles := &config.PartialRoleConfigs{}
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "ghes", Kind: config.ProviderKindGitHub, BaseURL: "https://ghe.example.test"}},
		Projects: []config.ProjectRefConfig{{
			ID:           "project",
			Provider:     "ghes",
			Repo:         "acme/repo",
			RepoPath:     t.TempDir(),
			BaseBranch:   &baseBranch,
			WorktreeRoot: &worktreeRoot,
			Roles:        roles,
		}},
	}
	resolver := NewResolver(cfg)

	cfg.Providers[0].BaseURL = "https://mutated.example.test"
	cfg.Projects[0].Provider = ""
	cfg.Projects[0].Repo = "mutated/repo"
	*cfg.Projects[0].BaseBranch = "release"
	*cfg.Projects[0].WorktreeRoot = filepath.Join(worktreeRoot, "mutated")
	cfg.Projects[0].Roles.Reviewer = &config.PartialReviewerRoleConfig{}

	selection := resolver.ForProject("project")
	if projectID, bound := selection.ProjectID(); !bound || projectID != "project" {
		t.Fatalf("ProjectID() = (%q, %v), want (project, true)", projectID, bound)
	}
	selection, matched, err := resolver.ForLocation("acme/repo", filepath.Join(originalWorktreeRoot, "feature"))
	if err != nil || !matched {
		t.Fatalf("ForLocation() after caller mutation = matched %v, selection %#v, err %v", matched, selection, err)
	}
	if projectID, bound := selection.ProjectID(); !bound || projectID != "project" {
		t.Fatalf("ForLocation() ProjectID() = (%q, %v), want the pre-mutation snapshot binding", projectID, bound)
	}
}
