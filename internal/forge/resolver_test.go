package forge

import (
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestResolverSelectsGitHubAndForgejoWithSharedContract(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	forgejoRoot := filepath.Join(root, "forgejo")
	config := config.Config{
		Providers: []config.ProviderConfig{{ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: "https://forgejo.example.test"}},
		Projects: []config.ProjectRefConfig{
			{ID: "github", Repo: "acme/github", RepoPath: filepath.Join(root, "github")},
			{ID: "forgejo", Provider: "forgejo", Repo: "acme/forgejo", RepoPath: forgejoRoot},
		},
	}

	tests := []struct {
		name           string
		repo           string
		cwd            string
		wantNativeAPI  bool
		wantTaskSource string
		wantGitHubPRs  bool
		wantGitHubCLI  bool
	}{
		{name: "github by repo", repo: "acme/github", wantTaskSource: "GitHub", wantGitHubPRs: true, wantGitHubCLI: true},
		{name: "forgejo by cwd", repo: "acme/forgejo", cwd: filepath.Join(forgejoRoot, "feature"), wantNativeAPI: true, wantTaskSource: "Forgejo"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selection, matched, err := NewResolver(config).ForLocation(test.repo, test.cwd)
			if err != nil || !matched {
				t.Fatalf("ForLocation() = matched %v, err %v", matched, err)
			}
			capabilities := selection.Capabilities()
			if !capabilities.Issues || !capabilities.PullRequests || !capabilities.NativeReviews {
				t.Fatalf("Capabilities() = %#v, want common issue/PR/review contract", capabilities)
			}
			if got := selection.UsesNativePullRequestAPI(); got != test.wantNativeAPI {
				t.Fatalf("UsesNativePullRequestAPI() = %v, want %v", got, test.wantNativeAPI)
			}
			if got := selection.TaskSourceName(); got != test.wantTaskSource {
				t.Fatalf("TaskSourceName() = %q, want %q", got, test.wantTaskSource)
			}
			if got := capabilities.GitHubPullRequests; got != test.wantGitHubPRs {
				t.Fatalf("GitHubPullRequests = %v, want %v", got, test.wantGitHubPRs)
			}
			if got := capabilities.GitHubCLIPullRequestCreation; got != test.wantGitHubCLI {
				t.Fatalf("GitHubCLIPullRequestCreation = %v, want %v", got, test.wantGitHubCLI)
			}
		})
	}
}

func TestResolverMakesPlaneTaskSourceExplicit(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "plane", Kind: config.ProviderKindPlane}},
		Projects:  []config.ProjectRefConfig{{ID: "plane", Provider: "plane", Repo: "acme/code", RepoPath: t.TempDir()}},
	}

	selection := NewResolver(cfg).ForProject("plane")
	capabilities := selection.Capabilities()
	if !selection.UsesExternalTaskSource() || selection.TaskSourceName() != "Plane" {
		t.Fatalf("Plane selection = %#v, want explicit external task source", selection)
	}
	if capabilities.PullRequests || capabilities.NativeReviews || !capabilities.GitHubPullRequests {
		t.Fatalf("Plane capabilities = %#v, want task source without native PRs and GitHub code PR delegation", capabilities)
	}
}

func TestResolverCWDSelectionIsAuthoritative(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	githubWorktrees := filepath.Join(root, "github-worktrees")
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: "https://forgejo.example.test"}},
		Projects: []config.ProjectRefConfig{
			{ID: "github", Repo: "acme/shared", RepoPath: filepath.Join(root, "github"), WorktreeRoot: &githubWorktrees},
			{ID: "forgejo", Provider: "forgejo", Repo: "acme/shared", RepoPath: filepath.Join(root, "forgejo")},
		},
	}

	selection, matched, err := NewResolver(cfg).ForLocation("acme/shared", filepath.Join(githubWorktrees, "feature"))
	if err != nil || !matched {
		t.Fatalf("ForLocation() = matched %v, err %v", matched, err)
	}
	if selection.UsesNativePullRequestAPI() {
		t.Fatal("configured GitHub CWD fell through to same-slug Forgejo project")
	}
}
