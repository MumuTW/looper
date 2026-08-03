package worktreecleanup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/worktreesafety"
)

// The record-driven pass (Service.Plan) enumerates the worktrees table, so a
// directory that never reached AdoptPath is invisible to it forever. Two
// routine paths produce exactly that: a crash between `git worktree add` and
// the durable claim, and any test binary that resolves a project's worktree
// root without an isolated LOOPER_HOME. Neither is rare enough to leave to an
// operator with `rm -rf`, and the directories accumulate without bound.
//
// The sweep deliberately does not adopt debris into the worktrees table to
// reuse the record-driven pass. Adoption would write durable rows whose only
// purpose is to be deleted, and would hand garbage a project/branch identity
// that AdoptPath then defends by retiring colliding labels on real checkouts.
// Unregistered means unmanaged; unmanaged directories are removed as
// directories.

// DiskSweepActionRemove marks an unregistered directory the sweep would delete.
const DiskSweepActionRemove = "would_remove"

// DiskSweepRoot is one project's managed worktree root.
type DiskSweepRoot struct {
	ProjectID    string
	RepoPath     string
	WorktreeRoot string
}

// DiskSweepPlanInput is everything the planner needs, pre-read by the caller so
// planning itself stays pure and table-testable.
type DiskSweepPlanInput struct {
	Root DiskSweepRoot
	// Entries are the immediate children of Root.WorktreeRoot.
	Entries []DiskEntry
	// RegisteredPaths is every path the worktrees table claims, any status.
	RegisteredPaths []string
	// GitTrackedPaths is every path `git worktree list` reports for the repo.
	GitTrackedPaths []string
	// Budget caps would_remove decisions for this root. Zero or less disables
	// the sweep for the root.
	Budget int
	// RetentionCutoff is the newest modification time still protected.
	RetentionCutoff time.Time
}

// DiskEntry is one child of a worktree root.
type DiskEntry struct {
	Path       string
	IsDir      bool
	ModifiedAt time.Time
}

// DiskCandidate is the planner's decision for one directory.
type DiskCandidate struct {
	ProjectID string `json:"projectId"`
	Path      string `json:"path"`
	Action    string `json:"action"`
	Reason    string `json:"reason"`
	Error     string `json:"error,omitempty"`
}

// DiskSweepSummary counts a sweep pass.
type DiskSweepSummary struct {
	Scanned      int `json:"scanned"`
	Unregistered int `json:"unregistered"`
	WouldRemove  int `json:"wouldRemove"`
	Removed      int `json:"removed"`
	Skipped      int `json:"skipped"`
	Errors       int `json:"errors"`
}

// DiskSweepPlan is the planner's output for one root.
type DiskSweepPlan struct {
	Summary    DiskSweepSummary
	Candidates []DiskCandidate
}

// PlanDiskSweep decides, for each child of one worktree root, whether it is
// unmanaged debris old enough to remove. Every gate that can be answered from
// already-read state lives here; the two that need to run against the
// filesystem at removal time (checkout usability and dirtiness) are applied by
// RunDiskSweep so a directory cannot go dirty between plan and delete.
func PlanDiskSweep(input DiskSweepPlanInput) DiskSweepPlan {
	registered := normalizedPathSet(input.RegisteredPaths)
	tracked := normalizedPathSet(input.GitTrackedPaths)

	entries := append([]DiskEntry(nil), input.Entries...)
	// Oldest first: a bounded budget should retire the longest-dead debris
	// rather than whichever name the filesystem happened to return first.
	sort.SliceStable(entries, func(a, b int) bool {
		return entries[a].ModifiedAt.Before(entries[b].ModifiedAt)
	})

	plan := DiskSweepPlan{}
	removing := 0
	for _, entry := range entries {
		plan.Summary.Scanned++
		candidate := DiskCandidate{ProjectID: input.Root.ProjectID, Path: entry.Path, Action: ActionSkipped}

		if !entry.IsDir {
			candidate.Reason = "not_a_directory"
			plan.appendSkip(candidate)
			continue
		}
		if err := worktreesafety.Validate(worktreesafety.CheckInput{
			WorktreePath: entry.Path,
			RepoPath:     input.Root.RepoPath,
			WorktreeRoot: input.Root.WorktreeRoot,
		}); err != nil {
			candidate.Reason = "unsafe_path"
			candidate.Error = err.Error()
			plan.appendSkip(candidate)
			continue
		}
		if registered[normalizeSweepPath(entry.Path)] {
			candidate.Reason = "registered"
			plan.appendSkip(candidate)
			continue
		}
		plan.Summary.Unregistered++
		if tracked[normalizeSweepPath(entry.Path)] {
			// Git still administers this path even though looper lost the row.
			// Removing the directory would leave the repo's worktree metadata
			// pointing at nothing; that is a repair, not a sweep.
			candidate.Reason = "git_tracked"
			plan.appendSkip(candidate)
			continue
		}
		if !entry.ModifiedAt.Before(input.RetentionCutoff) {
			candidate.Reason = "within_retention"
			plan.appendSkip(candidate)
			continue
		}
		if removing >= input.Budget {
			candidate.Reason = "budget_exhausted"
			plan.appendSkip(candidate)
			continue
		}

		candidate.Action = DiskSweepActionRemove
		candidate.Reason = "unregistered_debris"
		plan.Summary.WouldRemove++
		removing++
		plan.Candidates = append(plan.Candidates, candidate)
	}
	return plan
}

func (p *DiskSweepPlan) appendSkip(candidate DiskCandidate) {
	p.Summary.Skipped++
	p.Candidates = append(p.Candidates, candidate)
}

// Worktree roots nest as <shared root>/<repo container>/<project>/<worktree>,
// and the container name is a pure function of the repo path
// (config.ToRepoWorktreeDirectoryName). A container whose name matches no live
// project's repo path is therefore unreachable: no project resolves a root
// inside it, so the per-root sweep above can never enumerate its contents.
//
// That is where an abandoned repo path lands — a deleted checkout, or a test
// binary whose repo lived in a temporary directory that no longer exists. Each
// such run mints a fresh hash and a fresh container, so the count grows without
// any single container growing. Only the shared root sees them.

// PlanContainerSweep decides which repo containers under the shared worktree
// root belong to no live project. LiveContainerNames must list the container
// name of every project whose checkouts are still wanted, archived included.
func PlanContainerSweep(input ContainerSweepPlanInput) DiskSweepPlan {
	live := make(map[string]bool, len(input.LiveContainerNames))
	for _, name := range input.LiveContainerNames {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			live[trimmed] = true
		}
	}

	entries := append([]DiskEntry(nil), input.Entries...)
	sort.SliceStable(entries, func(a, b int) bool {
		return entries[a].ModifiedAt.Before(entries[b].ModifiedAt)
	})

	plan := DiskSweepPlan{}
	removing := 0
	for _, entry := range entries {
		plan.Summary.Scanned++
		candidate := DiskCandidate{Path: entry.Path, Action: ActionSkipped}

		if !entry.IsDir {
			candidate.Reason = "not_a_directory"
			plan.appendSkip(candidate)
			continue
		}
		if err := worktreesafety.Validate(worktreesafety.CheckInput{
			WorktreePath: entry.Path,
			WorktreeRoot: input.SharedRoot,
		}); err != nil {
			candidate.Reason = "unsafe_path"
			candidate.Error = err.Error()
			plan.appendSkip(candidate)
			continue
		}
		if live[filepath.Base(entry.Path)] {
			candidate.Reason = "live_project_container"
			plan.appendSkip(candidate)
			continue
		}
		plan.Summary.Unregistered++
		if !entry.ModifiedAt.Before(input.RetentionCutoff) {
			candidate.Reason = "within_retention"
			plan.appendSkip(candidate)
			continue
		}
		if removing >= input.Budget {
			candidate.Reason = "budget_exhausted"
			plan.appendSkip(candidate)
			continue
		}

		candidate.Action = DiskSweepActionRemove
		candidate.Reason = "unreachable_container"
		plan.Summary.WouldRemove++
		removing++
		plan.Candidates = append(plan.Candidates, candidate)
	}
	return plan
}

// ContainerSweepPlanInput is the shared-root equivalent of DiskSweepPlanInput.
type ContainerSweepPlanInput struct {
	SharedRoot         string
	Entries            []DiskEntry
	LiveContainerNames []string
	Budget             int
	RetentionCutoff    time.Time
}

// ContainerSweepOptions configures one executed sweep of the shared root.
type ContainerSweepOptions struct {
	SharedRoot         string
	LiveContainerNames []string
	// LiveProjectPaths protects archived and active project directories inside
	// live containers while allowing orphaned project directories to drain.
	LiveProjectPaths []string
	RegisteredPaths  []string
	Git              DiskSweepGit
	Budget           int
	RetentionCutoff  time.Time
	DryRun           bool
	ReadDir          func(string) ([]DiskEntry, error)
	RemoveAll        func(string) error
}

// RunContainerSweep removes unreachable repo containers. A container is only
// removed once every checkout inside it passes the same gates a single
// unregistered directory must pass, so an abandoned repo path cannot take
// uncommitted work down with it.
func RunContainerSweep(ctx context.Context, options ContainerSweepOptions) (DiskSweepPlan, error) {
	if options.Git == nil {
		return DiskSweepPlan{}, fmt.Errorf("git gateway is required")
	}
	if strings.TrimSpace(options.SharedRoot) == "" {
		return DiskSweepPlan{}, fmt.Errorf("shared worktree root is required")
	}
	readDir := options.ReadDir
	if readDir == nil {
		readDir = readDirEntries
	}
	removeAll := options.RemoveAll
	if removeAll == nil {
		removeAll = os.RemoveAll
	}
	options.ReadDir = readDir
	options.RemoveAll = removeAll

	entries, err := readDir(options.SharedRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return DiskSweepPlan{}, nil
		}
		return DiskSweepPlan{}, err
	}

	plan := PlanContainerSweep(ContainerSweepPlanInput{
		SharedRoot:         options.SharedRoot,
		Entries:            entries,
		LiveContainerNames: options.LiveContainerNames,
		Budget:             options.Budget,
		RetentionCutoff:    options.RetentionCutoff,
	})

	registered := normalizedPathSet(options.RegisteredPaths)
	result := DiskSweepPlan{Summary: DiskSweepSummary{
		Scanned:      plan.Summary.Scanned,
		Unregistered: plan.Summary.Unregistered,
	}}
	liveProjects := normalizedPathSet(options.LiveProjectPaths)
	for _, candidate := range plan.Candidates {
		if candidate.Action != DiskSweepActionRemove {
			if candidate.Reason == "live_project_container" && len(options.LiveProjectPaths) > 0 {
				releaseMutation := worktreesafety.AcquireManagedMutationLock(candidate.Path)
				used := result.Summary.Removed
				if options.DryRun {
					used = result.Summary.WouldRemove
				}
				nested := sweepLiveContainerProjects(ctx, options, candidate.Path, liveProjects, registered, options.Budget-used)
				releaseMutation()
				result.Summary.Scanned += nested.Summary.Scanned
				result.Summary.Unregistered += nested.Summary.Unregistered
				result.Summary.WouldRemove += nested.Summary.WouldRemove
				result.Summary.Removed += nested.Summary.Removed
				result.Summary.Skipped += nested.Summary.Skipped
				result.Summary.Errors += nested.Summary.Errors
				result.Candidates = append(result.Candidates, nested.Candidates...)
			}
			result.Summary.Skipped++
			result.Candidates = append(result.Candidates, candidate)
			continue
		}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		releaseMutation := worktreesafety.AcquireManagedMutationLock(candidate.Path)
		decided, eligible := revalidateContainerCandidate(candidate, options.RetentionCutoff)
		if !eligible {
			releaseMutation()
			if decided.Action == "error" {
				result.Summary.Errors++
			} else {
				result.Summary.Skipped++
			}
			result.Candidates = append(result.Candidates, decided)
			continue
		}
		decided, ok := admitContainer(ctx, options.Git, readDir, registered, decided)
		if !ok {
			releaseMutation()
			if decided.Action == "error" {
				result.Summary.Errors++
			} else {
				result.Summary.Skipped++
			}
			result.Candidates = append(result.Candidates, decided)
			continue
		}
		// Re-stat after the nested Git/ownership probes and immediately before
		// RemoveAll; a replacement can appear during those probes.
		decided, eligible = revalidateContainerCandidate(decided, options.RetentionCutoff)
		if !eligible {
			releaseMutation()
			if decided.Action == "error" {
				result.Summary.Errors++
			} else {
				result.Summary.Skipped++
			}
			result.Candidates = append(result.Candidates, decided)
			continue
		}

		result.Summary.WouldRemove++
		if !options.DryRun {
			if err := removeAll(candidate.Path); err != nil {
				decided.Action = "error"
				decided.Reason = "remove_failed"
				decided.Error = err.Error()
				result.Summary.Errors++
				result.Candidates = append(result.Candidates, decided)
				releaseMutation()
				continue
			}
			result.Summary.Removed++
		}
		result.Candidates = append(result.Candidates, decided)
		releaseMutation()
	}
	return result, nil
}

func sweepLiveContainerProjects(ctx context.Context, options ContainerSweepOptions, containerPath string, liveProjects, registered map[string]bool, budget int) DiskSweepPlan {
	plan := DiskSweepPlan{}
	projects, err := options.ReadDir(containerPath)
	if err != nil {
		plan.Summary.Errors++
		plan.Candidates = append(plan.Candidates, DiskCandidate{Path: containerPath, Action: "error", Reason: "read_container_failed", Error: err.Error()})
		return plan
	}
	for _, project := range projects {
		if ctx.Err() != nil {
			return plan
		}
		if !project.IsDir {
			continue
		}
		plan.Summary.Scanned++
		if liveProjects[normalizeSweepPath(project.Path)] {
			continue
		}
		plan.Summary.Unregistered++
		freshProject, projectEligible := revalidateContainerCandidate(DiskCandidate{Path: project.Path}, options.RetentionCutoff)
		if !projectEligible {
			plan.Summary.Skipped++
			plan.Candidates = append(plan.Candidates, freshProject)
			continue
		}
		if budget <= 0 {
			plan.Summary.Skipped++
			plan.Candidates = append(plan.Candidates, DiskCandidate{Path: project.Path, Action: ActionSkipped, Reason: "budget_exhausted"})
			continue
		}
		children, err := options.ReadDir(project.Path)
		if err != nil {
			plan.Summary.Errors++
			plan.Candidates = append(plan.Candidates, DiskCandidate{Path: project.Path, Action: "error", Reason: "read_project_failed", Error: err.Error()})
			continue
		}
		protected := false
		for _, child := range children {
			if !child.IsDir {
				protected = true
				break
			}
			freshChild, childEligible := revalidateContainerCandidate(DiskCandidate{Path: child.Path}, options.RetentionCutoff)
			if registered[normalizeSweepPath(child.Path)] || !childEligible {
				protected = true
				break
			}
			decided, ok := admitRemoval(ctx, options.Git, freshChild)
			if !ok {
				protected = true
				if decided.Action == "error" {
					plan.Summary.Errors++
				}
				break
			}
		}
		if protected {
			plan.Summary.Skipped++
			plan.Candidates = append(plan.Candidates, DiskCandidate{Path: project.Path, Action: ActionSkipped, Reason: "protected_project"})
			continue
		}
		candidate := DiskCandidate{Path: project.Path, Action: DiskSweepActionRemove, Reason: "orphaned_project"}
		plan.Summary.WouldRemove++
		if options.DryRun {
			budget--
			plan.Candidates = append(plan.Candidates, candidate)
			continue
		}
		if err := options.RemoveAll(project.Path); err != nil {
			plan.Summary.Errors++
			candidate.Action = "error"
			candidate.Reason = "remove_failed"
			candidate.Error = err.Error()
			plan.Candidates = append(plan.Candidates, candidate)
			continue
		}
		budget--
		plan.Summary.Removed++
		plan.Candidates = append(plan.Candidates, candidate)
	}
	return plan
}

func revalidateContainerCandidate(candidate DiskCandidate, cutoff time.Time) (DiskCandidate, bool) {
	info, err := os.Stat(candidate.Path)
	if err != nil {
		if os.IsNotExist(err) {
			candidate.Action = ActionSkipped
			candidate.Reason = "missing_at_removal"
			return candidate, false
		}
		candidate.Action = "error"
		candidate.Reason = "stat_failed"
		candidate.Error = err.Error()
		return candidate, false
	}
	if !info.IsDir() {
		candidate.Action = ActionSkipped
		candidate.Reason = "not_a_directory"
		return candidate, false
	}
	if !latestPathModTime(candidate.Path, info.ModTime()).Before(cutoff) {
		candidate.Action = ActionSkipped
		candidate.Reason = "within_retention"
		return candidate, false
	}
	return candidate, true
}

// admitContainer refuses the whole container if any checkout inside it is
// registered, fixer-owned, or a usable checkout with uncommitted work. One
// protected leaf protects its container: partial removal would leave a
// half-deleted tree that no later pass can reason about.
func admitContainer(ctx context.Context, git DiskSweepGit, readDir func(string) ([]DiskEntry, error), registered map[string]bool, container DiskCandidate) (DiskCandidate, bool) {
	projects, err := readDir(container.Path)
	if err != nil {
		container.Action = "error"
		container.Reason = "read_container_failed"
		container.Error = err.Error()
		return container, false
	}
	for _, project := range projects {
		if !project.IsDir {
			continue
		}
		checkouts, err := readDir(project.Path)
		if err != nil {
			container.Action = "error"
			container.Reason = "read_container_failed"
			container.Error = err.Error()
			return container, false
		}
		for _, checkout := range checkouts {
			if !checkout.IsDir {
				continue
			}
			if registered[normalizeSweepPath(checkout.Path)] {
				container.Action = ActionSkipped
				container.Reason = "registered_checkout_inside"
				return container, false
			}
			leaf, ok := admitRemoval(ctx, git, DiskCandidate{Path: checkout.Path})
			if !ok {
				container.Action = leaf.Action
				container.Reason = leaf.Reason + "_inside"
				container.Error = leaf.Error
				return container, false
			}
		}
	}
	return container, true
}

// DiskSweepGit is the git surface the executor needs.
type DiskSweepGit interface {
	WorktreeClean(context.Context, string) (bool, error)
}

// DiskSweepOptions configures one executed sweep across all configured roots.
type DiskSweepOptions struct {
	Roots           []DiskSweepRoot
	RegisteredPaths []string
	Git             DiskSweepGit
	Budget          int
	// RootStartIndex rotates the first root examined so a perpetually busy
	// root cannot starve later roots when the shared removal budget is small.
	RootStartIndex  int
	RetentionCutoff time.Time
	DryRun          bool
	// GitTrackedPaths resolves `git worktree list` for one repo. A root whose
	// repo cannot be listed is skipped entirely rather than swept blind.
	GitTrackedPaths func(context.Context, string) ([]string, error)
	// IsRegisteredPath rechecks the authoritative worktrees table for one path
	// while the mutation scope is held. The initial RegisteredPaths snapshot is
	// only a planning hint; deletion is unsafe without this final authority
	// query because a creator can adopt a checkout after planning completes.
	IsRegisteredPath func(context.Context, string) (bool, error)
	// ReadDir defaults to os.ReadDir semantics; injected for tests.
	ReadDir func(string) ([]DiskEntry, error)
	// RemoveAll defaults to os.RemoveAll; injected for tests.
	RemoveAll func(string) error
}

// RunDiskSweep plans and executes the sweep for every configured root.
func RunDiskSweep(ctx context.Context, options DiskSweepOptions) (DiskSweepPlan, error) {
	if options.Git == nil {
		return DiskSweepPlan{}, fmt.Errorf("git gateway is required")
	}
	if options.GitTrackedPaths == nil {
		return DiskSweepPlan{}, fmt.Errorf("git worktree listing is required")
	}
	if options.IsRegisteredPath == nil {
		return DiskSweepPlan{}, fmt.Errorf("registered path authority is required")
	}
	readDir := options.ReadDir
	if readDir == nil {
		readDir = readDirEntries
	}
	removeAll := options.RemoveAll
	if removeAll == nil {
		removeAll = os.RemoveAll
	}

	result := DiskSweepPlan{}
	budget := options.Budget
	rootCount := len(options.Roots)
	if rootCount == 0 {
		return result, nil
	}
	start := options.RootStartIndex % rootCount
	if start < 0 {
		start += rootCount
	}
	for offset := 0; offset < rootCount; offset++ {
		root := options.Roots[(start+offset)%rootCount]
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if strings.TrimSpace(root.WorktreeRoot) == "" {
			continue
		}
		entries, err := readDir(root.WorktreeRoot)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			result.Summary.Errors++
			result.Candidates = append(result.Candidates, DiskCandidate{
				ProjectID: root.ProjectID,
				Path:      root.WorktreeRoot,
				Action:    ActionSkipped,
				Reason:    "read_root_failed",
				Error:     err.Error(),
			})
			continue
		}
		tracked, err := options.GitTrackedPaths(ctx, root.RepoPath)
		if err != nil {
			// Without the git view every unregistered directory looks like
			// debris, including live linked worktrees. Fail the root closed.
			result.Summary.Errors++
			result.Candidates = append(result.Candidates, DiskCandidate{
				ProjectID: root.ProjectID,
				Path:      root.WorktreeRoot,
				Action:    ActionSkipped,
				Reason:    "git_list_failed",
				Error:     err.Error(),
			})
			continue
		}

		plan := PlanDiskSweep(DiskSweepPlanInput{
			Root:            root,
			Entries:         entries,
			RegisteredPaths: options.RegisteredPaths,
			GitTrackedPaths: tracked,
			Budget:          budget,
			RetentionCutoff: options.RetentionCutoff,
		})
		result.Summary.Scanned += plan.Summary.Scanned
		result.Summary.Unregistered += plan.Summary.Unregistered

		for _, candidate := range plan.Candidates {
			if candidate.Action != DiskSweepActionRemove {
				result.Summary.Skipped++
				result.Candidates = append(result.Candidates, candidate)
				continue
			}
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			releaseMutation := worktreesafety.AcquireManagedMutationLock(worktreesafety.WorktreeMutationScope(root.WorktreeRoot))
			decided, eligible := revalidateDiskCandidate(root, candidate, options.RetentionCutoff)
			if !eligible {
				releaseMutation()
				if decided.Action == "error" {
					result.Summary.Errors++
				} else {
					result.Summary.Skipped++
				}
				result.Candidates = append(result.Candidates, decided)
				continue
			}
			decided, ok := admitRemoval(ctx, options.Git, decided)
			if !ok {
				releaseMutation()
				if decided.Action == "error" {
					result.Summary.Errors++
				} else {
					result.Summary.Skipped++
				}
				result.Candidates = append(result.Candidates, decided)
				continue
			}
			registered, err := options.IsRegisteredPath(ctx, decided.Path)
			if err != nil {
				decided.Action = "error"
				decided.Reason = "registered_path_refresh_failed"
				decided.Error = err.Error()
				result.Summary.Errors++
				result.Candidates = append(result.Candidates, decided)
				releaseMutation()
				continue
			}
			if registered {
				decided.Action = ActionSkipped
				decided.Reason = "registered_at_removal"
				result.Summary.Skipped++
				result.Candidates = append(result.Candidates, decided)
				releaseMutation()
				continue
			}
			tracked, err := options.GitTrackedPaths(ctx, root.RepoPath)
			if err != nil {
				decided.Action = "error"
				decided.Reason = "git_list_refresh_failed"
				decided.Error = err.Error()
				result.Summary.Errors++
				result.Candidates = append(result.Candidates, decided)
				releaseMutation()
				continue
			}
			if normalizedPathSet(tracked)[normalizeSweepPath(decided.Path)] {
				decided.Action = ActionSkipped
				decided.Reason = "git_tracked_at_removal"
				result.Summary.Skipped++
				result.Candidates = append(result.Candidates, decided)
				releaseMutation()
				continue
			}
			decided, eligible = revalidateDiskCandidate(root, decided, options.RetentionCutoff)
			if !eligible {
				releaseMutation()
				if decided.Action == "error" {
					result.Summary.Errors++
				} else {
					result.Summary.Skipped++
				}
				result.Candidates = append(result.Candidates, decided)
				continue
			}

			result.Summary.WouldRemove++
			budget--
			if !options.DryRun {
				if err := removeAll(candidate.Path); err != nil {
					decided.Action = "error"
					decided.Reason = "remove_failed"
					decided.Error = err.Error()
					result.Summary.Errors++
					result.Candidates = append(result.Candidates, decided)
					budget++
					releaseMutation()
					continue
				}
				result.Summary.Removed++
			}
			result.Candidates = append(result.Candidates, decided)
			releaseMutation()
		}
	}
	return result, nil
}

func revalidateDiskCandidate(root DiskSweepRoot, candidate DiskCandidate, cutoff time.Time) (DiskCandidate, bool) {
	if err := worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: candidate.Path, RepoPath: root.RepoPath, WorktreeRoot: root.WorktreeRoot}); err != nil {
		candidate.Action = ActionSkipped
		candidate.Reason = "unsafe_path"
		candidate.Error = err.Error()
		return candidate, false
	}
	info, err := os.Stat(candidate.Path)
	if err != nil {
		if os.IsNotExist(err) {
			candidate.Action = ActionSkipped
			candidate.Reason = "missing_at_removal"
			return candidate, false
		}
		candidate.Action = "error"
		candidate.Reason = "stat_failed"
		candidate.Error = err.Error()
		return candidate, false
	}
	if !info.IsDir() {
		candidate.Action = ActionSkipped
		candidate.Reason = "not_a_directory"
		return candidate, false
	}
	if !info.ModTime().Before(cutoff) {
		candidate.Action = ActionSkipped
		candidate.Reason = "within_retention"
		return candidate, false
	}
	return candidate, true
}

// admitRemoval applies the two filesystem gates that must hold at delete time.
// A directory that is still a usable checkout gets the same dirty-work
// protection as a registered worktree; one that is not a usable checkout
// cannot be holding committed work reachable from any repo we know, so age and
// the registration gates are the whole test.
func admitRemoval(ctx context.Context, git DiskSweepGit, candidate DiskCandidate) (DiskCandidate, bool) {
	if worktreesafety.IsLinkedWorktree(candidate.Path) {
		candidate.Action = ActionSkipped
		candidate.Reason = "linked_git_metadata"
		return candidate, false
	}
	// Usability is tested before ownership on purpose. A fixer owner token on a
	// directory that is not a usable checkout protects nothing: there is no git
	// state to recover, and the fixer itself treats such a worktree as
	// recreatable (isMissingOrUnusableFixerWorktree). Checking the token first
	// would strand every abandoned test fixture that stamped one, forever.
	if !worktreesafety.LocalFixerWorktreeCheckoutUsable(candidate.Path) {
		if worktreesafety.LocalGitMetadataIndeterminate(candidate.Path) {
			candidate.Action = ActionSkipped
			candidate.Reason = "git_metadata_indeterminate"
			return candidate, false
		}
		return candidate, true
	}
	token, err := worktreesafety.ReadFixerOwnerToken(candidate.Path)
	if err != nil {
		candidate.Action = "error"
		candidate.Reason = "owner_token_unreadable"
		candidate.Error = err.Error()
		return candidate, false
	}
	if token != "" {
		candidate.Action = ActionSkipped
		candidate.Reason = "fixer_owned"
		return candidate, false
	}
	clean, err := git.WorktreeClean(ctx, candidate.Path)
	if err != nil {
		candidate.Action = "error"
		candidate.Reason = "status_failed"
		candidate.Error = err.Error()
		return candidate, false
	}
	if !clean {
		candidate.Action = ActionSkipped
		candidate.Reason = "dirty_worktree"
		return candidate, false
	}
	return candidate, true
}

func readDirEntries(root string) ([]DiskEntry, error) {
	dirEntries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	entries := make([]DiskEntry, 0, len(dirEntries))
	for _, dirEntry := range dirEntries {
		entry := DiskEntry{Path: filepath.Join(root, dirEntry.Name()), IsDir: dirEntry.IsDir()}
		if info, err := dirEntry.Info(); err == nil {
			entry.ModifiedAt = latestPathModTime(entry.Path, info.ModTime())
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func latestPathModTime(path string, fallback time.Time) time.Time {
	latest := fallback
	err := filepath.WalkDir(path, func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if current != path && entry.Name() == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		// Directory mtimes mostly record the creation of nested checkout
		// scaffolding. User activity is represented by regular files (and
		// symlinks, which are protected conservatively), so do not let a newly
		// created empty .git/project directory defeat an old root's retention.
		if current != path && !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
		return nil
	})
	if err != nil {
		// An unreadable child is evidence that the directory may contain recent
		// activity. Preserve it by treating the candidate as within retention.
		return time.Now()
	}
	return latest
}

func normalizedPathSet(paths []string) map[string]bool {
	set := make(map[string]bool, len(paths))
	for _, path := range paths {
		if trimmed := strings.TrimSpace(path); trimmed != "" {
			set[normalizeSweepPath(trimmed)] = true
		}
	}
	return set
}

func normalizeSweepPath(path string) string {
	return worktreesafety.NormalizePath(strings.TrimSpace(path))
}
