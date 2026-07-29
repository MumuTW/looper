package forge

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/nexu-io/looper/internal/config"
)

// Resolver is the production provider-selection seam. It resolves only the
// captured config snapshot supplied by its caller; persistent project catalog
// and SQLite binding remain authorities outside this package.
//
// A Resolver deliberately replaces the unused Provider/Registry abstraction:
// its selection, capability policy, and adapter construction are all used by
// runtime roles and review submission.
type Resolver struct {
	config config.Config
}

func NewResolver(cfg config.Config) Resolver {
	return Resolver{config: cfg}
}

// Selection is the immutable result of resolving one configured project. A
// selection exposes role-facing capability terms rather than provider config or
// concrete SDK types.
type Selection struct {
	project  config.ProjectRefConfig
	provider config.ProviderConfig
	kind     ProviderKind
	bound    bool
}

// ForProject resolves a catalog project ID. Unknown IDs preserve the legacy
// GitHub-default behavior used by role configuration lookup.
func (resolver Resolver) ForProject(projectID string) Selection {
	projectID = strings.TrimSpace(projectID)
	for _, project := range resolver.config.Projects {
		if strings.TrimSpace(project.ID) == projectID {
			selection, err := resolver.forProject(project)
			if err == nil {
				return selection
			}
			return Selection{project: project, bound: true}
		}
	}
	return Selection{kind: ProviderKindGitHub}
}

// ForProjectRef resolves a project already selected by an external authority
// (for example review-submit's merged file/SQLite catalog). It never performs
// catalog lookup itself.
func (resolver Resolver) ForProjectRef(project config.ProjectRefConfig) (Selection, error) {
	return resolver.forProject(project)
}

func (resolver Resolver) forProject(project config.ProjectRefConfig) (Selection, error) {
	selection := Selection{project: project, bound: true, kind: config.ResolvedProjectProviderKind(resolver.config, project)}
	if selection.kind == "" {
		return selection, fmt.Errorf("provider %q is not configured for project %s", strings.TrimSpace(project.Provider), strings.TrimSpace(project.ID))
	}
	if strings.TrimSpace(project.Provider) == "" {
		return selection, nil
	}
	for _, provider := range resolver.config.Providers {
		if strings.TrimSpace(provider.ID) == strings.TrimSpace(project.Provider) {
			selection.provider = provider
			return selection, nil
		}
	}
	return selection, fmt.Errorf("provider %q is not configured for project %s", strings.TrimSpace(project.Provider), strings.TrimSpace(project.ID))
}

// ForLocation resolves CWD before repository name. A configured CWD binding is
// authoritative even when it is GitHub, preventing same-slug cross-provider
// fallback. The bool is false only when no configured project matches.
func (resolver Resolver) ForLocation(repo, cwd string) (Selection, bool, error) {
	if strings.TrimSpace(cwd) != "" {
		matches := make([]config.ProjectRefConfig, 0, 1)
		for _, project := range resolver.config.Projects {
			if cwdBelongsToProject(project, cwd) {
				matches = append(matches, project)
			}
		}
		switch len(matches) {
		case 1:
			selection, err := resolver.forProject(matches[0])
			return selection, true, err
		case 0:
		default:
			ids := make([]string, 0, len(matches))
			for _, project := range matches {
				ids = append(ids, project.ID)
			}
			return Selection{}, false, fmt.Errorf("working directory %s matches multiple projects: %s", strings.TrimSpace(cwd), strings.Join(ids, ", "))
		}
	}

	repo = strings.TrimSpace(repo)
	var matched *config.ProjectRefConfig
	for _, project := range resolver.config.Projects {
		if !strings.EqualFold(strings.TrimSpace(project.Repo), repo) {
			continue
		}
		if matched != nil {
			return Selection{}, false, fmt.Errorf("repository %s is bound to multiple projects; project path or id is required", repo)
		}
		projectCopy := project
		matched = &projectCopy
	}
	if matched == nil {
		return Selection{}, false, nil
	}
	selection, err := resolver.forProject(*matched)
	return selection, true, err
}

func cwdBelongsToProject(project config.ProjectRefConfig, cwd string) bool {
	cwd = filepath.Clean(strings.TrimSpace(cwd))
	if cwd == "." || cwd == "" {
		return false
	}
	repoPath := filepath.Clean(strings.TrimSpace(project.RepoPath))
	if repoPath != "." && cwd == repoPath {
		return true
	}
	worktreeRoot := ""
	if project.WorktreeRoot != nil {
		worktreeRoot = strings.TrimSpace(*project.WorktreeRoot)
	}
	if worktreeRoot == "" {
		resolved, err := config.DefaultProjectWorktreeRoot(project.ID, project.RepoPath)
		if err != nil {
			return false
		}
		worktreeRoot = resolved
	}
	worktreeRoot = filepath.Clean(worktreeRoot)
	relative, err := filepath.Rel(worktreeRoot, cwd)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (selection Selection) Bound() bool { return selection.bound }

func (selection Selection) Project() (config.ProjectRefConfig, bool) {
	return selection.project, selection.bound
}

func (selection Selection) Capabilities() Capabilities {
	capabilities, _ := StaticCapabilities(selection.kind)
	return capabilities
}

// UsesNativePullRequestAPI is true when role-facing pull-request operations
// must use the Forgejo adapter instead of the GitHub code-repository gateway.
func (selection Selection) UsesNativePullRequestAPI() bool {
	return selection.kind == ProviderKindForgejo
}

// UsesExternalTaskSource is true for Plane. Plane remains explicit while its
// SDK and provider configuration stay contained in this package.
func (selection Selection) UsesExternalTaskSource() bool {
	return selection.kind == ProviderKindPlane
}

func (selection Selection) PullRequestProviderName() string {
	if selection.UsesNativePullRequestAPI() {
		return "Forgejo"
	}
	return "GitHub"
}

func (selection Selection) TaskSourceName() string {
	switch selection.kind {
	case ProviderKindForgejo:
		return "Forgejo"
	case ProviderKindPlane:
		return "Plane"
	default:
		return "GitHub"
	}
}

func (selection Selection) ForgejoClient() (*ForgejoClient, bool, error) {
	if !selection.UsesNativePullRequestAPI() {
		return nil, false, nil
	}
	if strings.TrimSpace(selection.project.Repo) == "" {
		return nil, true, fmt.Errorf("forgejo project %s is missing repo", strings.TrimSpace(selection.project.ID))
	}
	client, err := NewForgejoClientFromConfig(selection.provider, strings.TrimSpace(selection.project.Repo))
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}

func (selection Selection) PlaneClient() (*PlaneClient, bool, error) {
	if !selection.UsesExternalTaskSource() {
		return nil, false, nil
	}
	if strings.TrimSpace(selection.project.Repo) == "" {
		return nil, true, fmt.Errorf("plane project %s is missing repo", strings.TrimSpace(selection.project.ID))
	}
	client, err := NewPlaneClientFromConfig(selection.provider, strings.TrimSpace(selection.project.Repo))
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}

// ProbeNativeReviewCommentResolution keeps the provider configuration and the
// Forgejo-specific capability probe inside the selection seam.
func (selection Selection) ProbeNativeReviewCommentResolution(ctx context.Context) (ProbeState, error) {
	if !selection.UsesNativePullRequestAPI() {
		return ProbeStateSupported, nil
	}
	return ProbeForgejoReviewCommentResolution(ctx, selection.provider, selection.project.Repo)
}

func (resolver Resolver) ForgejoForLocation(repo, cwd string) (*ForgejoClient, bool, error) {
	selection, matched, err := resolver.ForLocation(repo, cwd)
	if err != nil || !matched {
		return nil, false, err
	}
	return selection.ForgejoClient()
}

func (resolver Resolver) PlaneForLocation(repo, cwd string) (*PlaneClient, bool, error) {
	selection, matched, err := resolver.ForLocation(repo, cwd)
	if err != nil || !matched {
		return nil, false, err
	}
	return selection.PlaneClient()
}
