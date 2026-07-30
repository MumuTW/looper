package forge

import (
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
	projects  []projectBinding
	providers map[string]config.ProviderConfig
}

func NewResolver(cfg config.Config) Resolver {
	resolver := Resolver{
		projects:  make([]projectBinding, 0, len(cfg.Projects)),
		providers: make(map[string]config.ProviderConfig, len(cfg.Providers)),
	}
	for _, provider := range cfg.Providers {
		providerID := strings.TrimSpace(provider.ID)
		if _, exists := resolver.providers[providerID]; exists {
			continue
		}
		resolver.providers[providerID] = cloneProviderConfig(provider)
	}
	for _, project := range cfg.Projects {
		resolver.projects = append(resolver.projects, projectBindingFromConfig(project))
	}
	return resolver
}

// projectBinding is the complete project projection needed for provider
// selection and adapter construction. Keeping this separate from
// config.ProjectRefConfig prevents role configuration pointers from escaping
// through a Selection.
type projectBinding struct {
	id           string
	providerID   string
	repo         string
	repoPath     string
	worktreeRoot string
}

func projectBindingFromConfig(project config.ProjectRefConfig) projectBinding {
	worktreeRoot := ""
	if project.WorktreeRoot != nil {
		worktreeRoot = strings.TrimSpace(*project.WorktreeRoot)
	}
	return projectBinding{
		id:           project.ID,
		providerID:   strings.TrimSpace(project.Provider),
		repo:         project.Repo,
		repoPath:     project.RepoPath,
		worktreeRoot: worktreeRoot,
	}
}

func cloneProviderConfig(provider config.ProviderConfig) config.ProviderConfig {
	cloned := provider
	cloned.GHPath = cloneStringPointer(provider.GHPath)
	cloned.TokenEnv = cloneStringPointer(provider.TokenEnv)
	return cloned
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// Selection is the immutable result of resolving one configured project. A
// selection exposes role-facing capability terms rather than provider config or
// concrete SDK types.
type Selection struct {
	project  projectBinding
	provider config.ProviderConfig
	kind     ProviderKind
	bound    bool
}

// ForProject resolves a catalog project ID. Unknown IDs preserve the legacy
// GitHub-default behavior used by role configuration lookup.
func (resolver Resolver) ForProject(projectID string) Selection {
	projectID = strings.TrimSpace(projectID)
	for _, project := range resolver.projects {
		if strings.TrimSpace(project.id) == projectID {
			selection, err := resolver.forProject(project)
			if err == nil {
				return selection
			}
			return Selection{project: project, kind: ProviderKindGitHub, bound: true}
		}
	}
	return Selection{kind: ProviderKindGitHub}
}

// ForProjectRef resolves a project already selected by an external authority
// (for example review-submit's merged file/SQLite catalog). It never performs
// catalog lookup itself.
func (resolver Resolver) ForProjectRef(project config.ProjectRefConfig) (Selection, error) {
	return resolver.forProject(projectBindingFromConfig(project))
}

func (resolver Resolver) forProject(project projectBinding) (Selection, error) {
	selection := Selection{project: project, bound: true}
	if project.providerID == "" {
		selection.kind = ProviderKindGitHub
		return selection, nil
	}
	provider, ok := resolver.providers[project.providerID]
	if !ok || provider.Kind == "" {
		return selection, fmt.Errorf("provider %q is not configured for project %s", project.providerID, strings.TrimSpace(project.id))
	}
	selection.provider = provider
	selection.kind = provider.Kind
	return selection, nil
}

// ForLocation resolves CWD before repository name. A configured CWD binding is
// authoritative even when it is GitHub, preventing same-slug cross-provider
// fallback. The bool is false only when no configured project matches.
func (resolver Resolver) ForLocation(repo, cwd string) (Selection, bool, error) {
	if strings.TrimSpace(cwd) != "" {
		matches := make([]projectBinding, 0, 1)
		for _, project := range resolver.projects {
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
				ids = append(ids, project.id)
			}
			return Selection{}, false, fmt.Errorf("working directory %s matches multiple projects: %s", strings.TrimSpace(cwd), strings.Join(ids, ", "))
		}
	}

	repo = strings.TrimSpace(repo)
	if repo == "" {
		return Selection{}, false, nil
	}
	var matched *projectBinding
	for _, project := range resolver.projects {
		if !strings.EqualFold(strings.TrimSpace(project.repo), repo) {
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

func cwdBelongsToProject(project projectBinding, cwd string) bool {
	cwd = filepath.Clean(strings.TrimSpace(cwd))
	if cwd == "." || cwd == "" {
		return false
	}
	repoPath := filepath.Clean(strings.TrimSpace(project.repoPath))
	if repoPath != "." && cwd == repoPath {
		return true
	}
	worktreeRoot := project.worktreeRoot
	if worktreeRoot == "" {
		resolved, err := config.DefaultProjectWorktreeRoot(project.id, project.repoPath)
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

// ProjectID returns the externally-owned catalog binding selected by this
// resolver. It deliberately does not expose a config.ProjectRefConfig because
// its pointer fields would let callers mutate the resolver snapshot.
func (selection Selection) ProjectID() (string, bool) {
	return selection.project.id, selection.bound
}

func (selection Selection) Capabilities() Capabilities {
	capabilities, _ := StaticCapabilities(selection.kind)
	return capabilities
}

func (selection Selection) PullRequestProviderName() string { return "GitHub" }

func (selection Selection) TaskSourceName() string { return "GitHub" }
