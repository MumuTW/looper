package worktreecleanup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/worktreesafety"
)

var sweepNow = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func sweepCutoff() time.Time { return sweepNow.Add(-7 * 24 * time.Hour) }

func old() time.Time { return sweepNow.Add(-30 * 24 * time.Hour) }

func recent() time.Time { return sweepNow.Add(-1 * time.Hour) }

func reasonFor(t *testing.T, plan DiskSweepPlan, path string) (string, string) {
	t.Helper()
	for _, candidate := range plan.Candidates {
		if candidate.Path == path {
			return candidate.Action, candidate.Reason
		}
	}
	t.Fatalf("no candidate for %q in %+v", path, plan.Candidates)
	return "", ""
}

func TestPlanDiskSweepGates(t *testing.T) {
	root := "/looper/worktrees"
	repo := "/repos/app"

	tests := []struct {
		name       string
		entry      DiskEntry
		registered []string
		tracked    []string
		budget     int
		wantAction string
		wantReason string
	}{
		{
			name:       "unregistered directory older than retention is removed",
			entry:      DiskEntry{Path: root + "/dead", IsDir: true, ModifiedAt: old()},
			budget:     10,
			wantAction: DiskSweepActionRemove,
			wantReason: "unregistered_debris",
		},
		{
			name:       "file is not a worktree",
			entry:      DiskEntry{Path: root + "/notes.txt", IsDir: false, ModifiedAt: old()},
			budget:     10,
			wantAction: ActionSkipped,
			wantReason: "not_a_directory",
		},
		{
			name:       "registered path belongs to the record pass",
			entry:      DiskEntry{Path: root + "/managed", IsDir: true, ModifiedAt: old()},
			registered: []string{root + "/managed"},
			budget:     10,
			wantAction: ActionSkipped,
			wantReason: "registered",
		},
		{
			name:       "registered path matches after cleaning",
			entry:      DiskEntry{Path: root + "/managed", IsDir: true, ModifiedAt: old()},
			registered: []string{root + "//managed/"},
			budget:     10,
			wantAction: ActionSkipped,
			wantReason: "registered",
		},
		{
			name:       "git still administers the path",
			entry:      DiskEntry{Path: root + "/live", IsDir: true, ModifiedAt: old()},
			tracked:    []string{root + "/live"},
			budget:     10,
			wantAction: ActionSkipped,
			wantReason: "git_tracked",
		},
		{
			name:       "within retention window",
			entry:      DiskEntry{Path: root + "/fresh", IsDir: true, ModifiedAt: recent()},
			budget:     10,
			wantAction: ActionSkipped,
			wantReason: "within_retention",
		},
		{
			name:       "budget exhausted",
			entry:      DiskEntry{Path: root + "/dead", IsDir: true, ModifiedAt: old()},
			budget:     0,
			wantAction: ActionSkipped,
			wantReason: "budget_exhausted",
		},
		{
			name:       "path outside the worktree root is never touched",
			entry:      DiskEntry{Path: "/etc", IsDir: true, ModifiedAt: old()},
			budget:     10,
			wantAction: ActionSkipped,
			wantReason: "unsafe_path",
		},
		{
			name:       "the root itself is never touched",
			entry:      DiskEntry{Path: root, IsDir: true, ModifiedAt: old()},
			budget:     10,
			wantAction: ActionSkipped,
			wantReason: "unsafe_path",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := PlanDiskSweep(DiskSweepPlanInput{
				Root:            DiskSweepRoot{ProjectID: "app", RepoPath: repo, WorktreeRoot: root},
				Entries:         []DiskEntry{test.entry},
				RegisteredPaths: test.registered,
				GitTrackedPaths: test.tracked,
				Budget:          test.budget,
				RetentionCutoff: sweepCutoff(),
			})
			action, reason := reasonFor(t, plan, test.entry.Path)
			if action != test.wantAction || reason != test.wantReason {
				t.Fatalf("got action %q reason %q, want action %q reason %q", action, reason, test.wantAction, test.wantReason)
			}
		})
	}
}

// A bounded budget must spend itself on the longest-dead debris, not on
// whatever order the filesystem returned. Without this an operator draining a
// large backlog would keep retiring recently-abandoned directories while the
// oldest ones survive every tick.
func TestPlanDiskSweepSpendsBudgetOldestFirst(t *testing.T) {
	root := "/looper/worktrees"
	plan := PlanDiskSweep(DiskSweepPlanInput{
		Root: DiskSweepRoot{ProjectID: "app", RepoPath: "/repos/app", WorktreeRoot: root},
		Entries: []DiskEntry{
			{Path: root + "/newer", IsDir: true, ModifiedAt: sweepNow.Add(-8 * 24 * time.Hour)},
			{Path: root + "/oldest", IsDir: true, ModifiedAt: sweepNow.Add(-90 * 24 * time.Hour)},
			{Path: root + "/middle", IsDir: true, ModifiedAt: sweepNow.Add(-40 * 24 * time.Hour)},
		},
		Budget:          1,
		RetentionCutoff: sweepCutoff(),
	})
	if plan.Summary.WouldRemove != 1 {
		t.Fatalf("wouldRemove = %d, want 1", plan.Summary.WouldRemove)
	}
	if action, _ := reasonFor(t, plan, root+"/oldest"); action != DiskSweepActionRemove {
		t.Fatalf("oldest entry action = %q, want %q", action, DiskSweepActionRemove)
	}
}

func TestPlanDiskSweepCountsUnregisteredBeyondBudget(t *testing.T) {
	root := "/looper/worktrees"
	entries := make([]DiskEntry, 0, 5)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		entries = append(entries, DiskEntry{Path: root + "/" + name, IsDir: true, ModifiedAt: old()})
	}
	plan := PlanDiskSweep(DiskSweepPlanInput{
		Root:            DiskSweepRoot{ProjectID: "app", RepoPath: "/repos/app", WorktreeRoot: root},
		Entries:         entries,
		Budget:          2,
		RetentionCutoff: sweepCutoff(),
	})
	// The backlog signal must survive the budget: an operator reading only
	// wouldRemove would conclude the root was almost drained.
	if plan.Summary.Unregistered != 5 {
		t.Fatalf("unregistered = %d, want 5", plan.Summary.Unregistered)
	}
	if plan.Summary.WouldRemove != 2 {
		t.Fatalf("wouldRemove = %d, want 2", plan.Summary.WouldRemove)
	}
}

type stubSweepGit struct {
	clean map[string]bool
	err   error
}

type mutatingSweepGit struct {
	path   string
	mutate func()
}

func (g mutatingSweepGit) WorktreeClean(_ context.Context, path string) (bool, error) {
	if path == g.path && g.mutate != nil {
		g.mutate()
	}
	return true, nil
}

func (s stubSweepGit) WorktreeClean(_ context.Context, path string) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.clean[path], nil
}

func mkdirAt(t *testing.T, path string, modified time.Time) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", path, err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatalf("chtimes %q: %v", path, err)
	}
	return path
}

// writeUsableCheckout produces a directory LocalFixerWorktreeCheckoutUsable
// accepts, so the executor treats it as a checkout that can hold real work.
func writeUsableCheckout(t *testing.T, path string, modified time.Time) string {
	t.Helper()
	gitDir := filepath.Join(path, ".git")
	for _, dir := range []string{filepath.Join(gitDir, "objects"), filepath.Join(gitDir, "refs")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatalf("chtimes %q: %v", path, err)
	}
	return path
}

func sweepOptions(root DiskSweepRoot, git DiskSweepGit, removed *[]string) DiskSweepOptions {
	return DiskSweepOptions{
		Roots:            []DiskSweepRoot{root},
		Git:              git,
		Budget:           100,
		RetentionCutoff:  sweepCutoff(),
		GitTrackedPaths:  func(context.Context, string) ([]string, error) { return nil, nil },
		IsRegisteredPath: func(context.Context, string) (bool, error) { return false, nil },
		RemoveAll: func(path string) error {
			*removed = append(*removed, path)
			return os.RemoveAll(path)
		},
	}
}

func TestRunDiskSweepRechecksRegisteredOwnershipBeforeRemoval(t *testing.T) {
	worktreeRoot := t.TempDir()
	path := mkdirAt(t, filepath.Join(worktreeRoot, "looper-app-adopted"), old())
	var removed []string
	options := sweepOptions(DiskSweepRoot{ProjectID: "app", RepoPath: filepath.Join(t.TempDir(), "repo"), WorktreeRoot: worktreeRoot}, stubSweepGit{}, &removed)
	options.IsRegisteredPath = func(_ context.Context, candidate string) (bool, error) {
		return candidate == path, nil
	}

	plan, err := RunDiskSweep(context.Background(), options)
	if err != nil {
		t.Fatalf("RunDiskSweep() error = %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none after delete-time ownership claim", removed)
	}
	if action, reason := reasonFor(t, plan, path); action != ActionSkipped || reason != "registered_at_removal" {
		t.Fatalf("candidate = (%q, %q), want registered_at_removal skip", action, reason)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("claimed checkout was removed: %v", err)
	}
}

func TestRunDiskSweepRechecksGitOwnershipBeforeRemoval(t *testing.T) {
	worktreeRoot := t.TempDir()
	path := mkdirAt(t, filepath.Join(worktreeRoot, "looper-app-linked"), old())
	var removed []string
	options := sweepOptions(DiskSweepRoot{ProjectID: "app", RepoPath: filepath.Join(t.TempDir(), "repo"), WorktreeRoot: worktreeRoot}, stubSweepGit{}, &removed)
	gitListCalls := 0
	options.GitTrackedPaths = func(_ context.Context, _ string) ([]string, error) {
		gitListCalls++
		if gitListCalls == 1 {
			return nil, nil
		}
		return []string{path}, nil
	}

	plan, err := RunDiskSweep(context.Background(), options)
	if err != nil {
		t.Fatalf("RunDiskSweep() error = %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none after delete-time Git claim", removed)
	}
	if action, reason := reasonFor(t, plan, path); action != ActionSkipped || reason != "git_tracked_at_removal" {
		t.Fatalf("candidate = (%q, %q), want git_tracked_at_removal skip", action, reason)
	}
	if gitListCalls != 2 {
		t.Fatalf("GitTrackedPaths calls = %d, want planning and delete-time refresh", gitListCalls)
	}
}

func TestRunDiskSweepRemovesUnusableDebris(t *testing.T) {
	worktreeRoot := t.TempDir()
	debris := mkdirAt(t, filepath.Join(worktreeRoot, "looper-app-dead"), old())

	var removed []string
	plan, err := RunDiskSweep(context.Background(), sweepOptions(
		DiskSweepRoot{ProjectID: "app", RepoPath: filepath.Join(t.TempDir(), "repo"), WorktreeRoot: worktreeRoot},
		stubSweepGit{}, &removed))
	if err != nil {
		t.Fatalf("RunDiskSweep() error = %v", err)
	}
	if len(removed) != 1 || removed[0] != debris {
		t.Fatalf("removed = %v, want [%s]", removed, debris)
	}
	if plan.Summary.Removed != 1 {
		t.Fatalf("removed count = %d, want 1", plan.Summary.Removed)
	}
	if _, err := os.Stat(debris); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("debris still on disk: %v", err)
	}
}

// An unregistered directory that is still a usable checkout can hold work an
// operator has not pushed. Age plus a missing row is not enough to delete it.
func TestRunDiskSweepPreservesDirtyUnregisteredCheckout(t *testing.T) {
	worktreeRoot := t.TempDir()
	dirty := writeUsableCheckout(t, filepath.Join(worktreeRoot, "looper-app-dirty"), old())

	var removed []string
	plan, err := RunDiskSweep(context.Background(), sweepOptions(
		DiskSweepRoot{ProjectID: "app", RepoPath: filepath.Join(t.TempDir(), "repo"), WorktreeRoot: worktreeRoot},
		stubSweepGit{clean: map[string]bool{dirty: false}}, &removed))
	if err != nil {
		t.Fatalf("RunDiskSweep() error = %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none", removed)
	}
	if plan.Summary.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", plan.Summary.Skipped)
	}
	if _, err := os.Stat(dirty); err != nil {
		t.Fatalf("dirty checkout was removed: %v", err)
	}
}

func TestRunDiskSweepRemovesCleanUnregisteredCheckout(t *testing.T) {
	worktreeRoot := t.TempDir()
	clean := writeUsableCheckout(t, filepath.Join(worktreeRoot, "looper-app-clean"), old())

	var removed []string
	if _, err := RunDiskSweep(context.Background(), sweepOptions(
		DiskSweepRoot{ProjectID: "app", RepoPath: filepath.Join(t.TempDir(), "repo"), WorktreeRoot: worktreeRoot},
		stubSweepGit{clean: map[string]bool{clean: true}}, &removed)); err != nil {
		t.Fatalf("RunDiskSweep() error = %v", err)
	}
	if len(removed) != 1 || removed[0] != clean {
		t.Fatalf("removed = %v, want [%s]", removed, clean)
	}
}

func TestRunDiskSweepRevalidatesRetentionAtRemovalBoundary(t *testing.T) {
	root := t.TempDir()
	path := writeUsableCheckout(t, filepath.Join(root, "looper-app-recreated"), old())
	var removed []string
	options := sweepOptions(DiskSweepRoot{ProjectID: "app", RepoPath: filepath.Join(t.TempDir(), "repo"), WorktreeRoot: root}, mutatingSweepGit{path: path, mutate: func() {
		_ = os.Chtimes(path, sweepNow, sweepNow)
	}}, &removed)
	plan, err := RunDiskSweep(context.Background(), options)
	if err != nil {
		t.Fatalf("RunDiskSweep() error = %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want recreated path preserved", removed)
	}
	if action, reason := reasonFor(t, plan, path); action != ActionSkipped || reason != "within_retention" {
		t.Fatalf("candidate = (%q, %q), want within_retention skip", action, reason)
	}
}

func TestRunDiskSweepPreservesIndeterminateGitMetadata(t *testing.T) {
	root := t.TempDir()
	path := mkdirAt(t, filepath.Join(root, "looper-app-indeterminate"), old())
	gitDir := filepath.Join(path, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		t.Fatalf("MkdirAll objects: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(gitDir, "refs"), 0o755); err != nil {
		t.Fatalf("MkdirAll refs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("not-a-head\n"), 0o644); err != nil {
		t.Fatalf("WriteFile HEAD: %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(gitDir, "objects", "aa")); err != nil {
		t.Fatalf("Symlink object entry: %v", err)
	}
	if err := os.Chtimes(path, old(), old()); err != nil {
		t.Fatalf("Chtimes path: %v", err)
	}
	var removed []string
	plan, err := RunDiskSweep(context.Background(), sweepOptions(DiskSweepRoot{ProjectID: "app", RepoPath: filepath.Join(t.TempDir(), "repo"), WorktreeRoot: root}, stubSweepGit{}, &removed))
	if err != nil {
		t.Fatalf("RunDiskSweep() error = %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want indeterminate metadata preserved", removed)
	}
	if action, reason := reasonFor(t, plan, path); action != ActionSkipped || reason != "git_metadata_indeterminate" {
		t.Fatalf("candidate = (%q, %q), want indeterminate skip", action, reason)
	}
}

func TestRunDiskSweepPreservesLinkedGitfile(t *testing.T) {
	root := t.TempDir()
	path := mkdirAt(t, filepath.Join(root, "looper-app-linked"), old())
	if err := os.WriteFile(filepath.Join(path, ".git"), []byte("gitdir: "+filepath.Join(t.TempDir(), "missing-gitdir")+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile linked gitfile: %v", err)
	}
	if err := os.Chtimes(path, old(), old()); err != nil {
		t.Fatalf("Chtimes path: %v", err)
	}
	var removed []string
	plan, err := RunDiskSweep(context.Background(), sweepOptions(DiskSweepRoot{ProjectID: "app", RepoPath: filepath.Join(t.TempDir(), "repo"), WorktreeRoot: root}, stubSweepGit{}, &removed))
	if err != nil {
		t.Fatalf("RunDiskSweep() error = %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want linked metadata preserved", removed)
	}
	if action, reason := reasonFor(t, plan, path); action != ActionSkipped || reason != "linked_git_metadata" {
		t.Fatalf("candidate = (%q, %q), want linked metadata skip", action, reason)
	}
}

func TestRunDiskSweepUsesRecentFileRetention(t *testing.T) {
	root := t.TempDir()
	path := mkdirAt(t, filepath.Join(root, "looper-app-active"), old())
	marker := filepath.Join(path, "agent-output.txt")
	if err := os.WriteFile(marker, []byte("active\n"), 0o644); err != nil {
		t.Fatalf("WriteFile marker: %v", err)
	}
	if err := os.Chtimes(marker, sweepNow, sweepNow); err != nil {
		t.Fatalf("Chtimes marker: %v", err)
	}
	var removed []string
	plan, err := RunDiskSweep(context.Background(), sweepOptions(DiskSweepRoot{ProjectID: "app", RepoPath: filepath.Join(t.TempDir(), "repo"), WorktreeRoot: root}, stubSweepGit{}, &removed))
	if err != nil {
		t.Fatalf("RunDiskSweep() error = %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want recent content preserved", removed)
	}
	if action, reason := reasonFor(t, plan, path); action != ActionSkipped || reason != "within_retention" {
		t.Fatalf("candidate = (%q, %q), want within_retention skip", action, reason)
	}
}

// Losing the git view makes every live linked worktree look unregistered. The
// root must fail closed rather than sweep blind.
func TestRunDiskSweepSkipsRootWhenGitListFails(t *testing.T) {
	worktreeRoot := t.TempDir()
	debris := mkdirAt(t, filepath.Join(worktreeRoot, "looper-app-dead"), old())

	var removed []string
	options := sweepOptions(
		DiskSweepRoot{ProjectID: "app", RepoPath: filepath.Join(t.TempDir(), "repo"), WorktreeRoot: worktreeRoot},
		stubSweepGit{}, &removed)
	options.GitTrackedPaths = func(context.Context, string) ([]string, error) {
		return nil, errors.New("not a git repository")
	}

	plan, err := RunDiskSweep(context.Background(), options)
	if err != nil {
		t.Fatalf("RunDiskSweep() error = %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none", removed)
	}
	if plan.Summary.Errors != 1 {
		t.Fatalf("errors = %d, want 1", plan.Summary.Errors)
	}
	if _, err := os.Stat(debris); err != nil {
		t.Fatalf("debris was removed after a failed git listing: %v", err)
	}
}

func TestRunDiskSweepDryRunRemovesNothing(t *testing.T) {
	worktreeRoot := t.TempDir()
	debris := mkdirAt(t, filepath.Join(worktreeRoot, "looper-app-dead"), old())

	var removed []string
	options := sweepOptions(
		DiskSweepRoot{ProjectID: "app", RepoPath: filepath.Join(t.TempDir(), "repo"), WorktreeRoot: worktreeRoot},
		stubSweepGit{}, &removed)
	options.DryRun = true

	plan, err := RunDiskSweep(context.Background(), options)
	if err != nil {
		t.Fatalf("RunDiskSweep() error = %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none", removed)
	}
	if plan.Summary.WouldRemove != 1 || plan.Summary.Removed != 0 {
		t.Fatalf("wouldRemove = %d removed = %d, want 1 and 0", plan.Summary.WouldRemove, plan.Summary.Removed)
	}
	if _, err := os.Stat(debris); err != nil {
		t.Fatalf("dry run removed %q: %v", debris, err)
	}
}

func TestRunDiskSweepBudgetSpansRoots(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	mkdirAt(t, filepath.Join(firstRoot, "a"), old())
	mkdirAt(t, filepath.Join(firstRoot, "b"), old())
	mkdirAt(t, filepath.Join(secondRoot, "c"), old())

	var removed []string
	options := sweepOptions(DiskSweepRoot{ProjectID: "one", RepoPath: filepath.Join(t.TempDir(), "repo"), WorktreeRoot: firstRoot}, stubSweepGit{}, &removed)
	options.Roots = append(options.Roots, DiskSweepRoot{ProjectID: "two", RepoPath: filepath.Join(t.TempDir(), "repo"), WorktreeRoot: secondRoot})
	options.Budget = 2

	if _, err := RunDiskSweep(context.Background(), options); err != nil {
		t.Fatalf("RunDiskSweep() error = %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed = %v, want 2 entries", removed)
	}
	for _, path := range removed {
		if filepath.Dir(path) != firstRoot {
			t.Fatalf("budget leaked past the first root: removed %q", path)
		}
	}
}

func TestRunDiskSweepRotatesRootStart(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	mkdirAt(t, filepath.Join(firstRoot, "first"), old())
	second := mkdirAt(t, filepath.Join(secondRoot, "second"), old())
	var removed []string
	options := sweepOptions(DiskSweepRoot{ProjectID: "one", RepoPath: filepath.Join(t.TempDir(), "repo-one"), WorktreeRoot: firstRoot}, stubSweepGit{}, &removed)
	options.Roots = append(options.Roots, DiskSweepRoot{ProjectID: "two", RepoPath: filepath.Join(t.TempDir(), "repo-two"), WorktreeRoot: secondRoot})
	options.Budget = 1
	options.RootStartIndex = 1
	if _, err := RunDiskSweep(context.Background(), options); err != nil {
		t.Fatalf("RunDiskSweep() error = %v", err)
	}
	if len(removed) != 1 || removed[0] != second {
		t.Fatalf("removed = %v, want rotated second root %q", removed, second)
	}
}

func TestRunDiskSweepRequiresGitListing(t *testing.T) {
	if _, err := RunDiskSweep(context.Background(), DiskSweepOptions{Git: stubSweepGit{}}); err == nil {
		t.Fatal("RunDiskSweep() error = nil, want an error when git listing is unavailable")
	}
	if _, err := RunDiskSweep(context.Background(), DiskSweepOptions{
		GitTrackedPaths: func(context.Context, string) ([]string, error) { return nil, nil },
	}); err == nil {
		t.Fatal("RunDiskSweep() error = nil, want an error when the git gateway is missing")
	}
}

// A fixer owner token on a usable checkout means a fixer may still be holding
// it, so the sweep must not remove it on age alone.
func TestRunDiskSweepPreservesFixerOwnedUsableCheckout(t *testing.T) {
	worktreeRoot := t.TempDir()
	owned := writeUsableCheckout(t, filepath.Join(worktreeRoot, "looper-app-owned"), old())
	if err := worktreesafety.WriteFixerOwnerToken(owned, "fixer-1"); err != nil {
		t.Fatalf("WriteFixerOwnerToken() error = %v", err)
	}

	var removed []string
	plan, err := RunDiskSweep(context.Background(), sweepOptions(
		DiskSweepRoot{ProjectID: "app", RepoPath: filepath.Join(t.TempDir(), "repo"), WorktreeRoot: worktreeRoot},
		stubSweepGit{clean: map[string]bool{owned: true}}, &removed))
	if err != nil {
		t.Fatalf("RunDiskSweep() error = %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none", removed)
	}
	if _, reason := reasonFor(t, plan, owned); reason != "fixer_owned" {
		t.Fatalf("reason = %q, want fixer_owned", reason)
	}
}

// A token stamped on a directory that is not a usable checkout protects
// nothing. Honouring it would strand every abandoned test fixture that wrote
// one, which is most of them.
func TestRunDiskSweepRemovesFixerOwnedUnusableDirectory(t *testing.T) {
	worktreeRoot := t.TempDir()
	debris := mkdirAt(t, filepath.Join(worktreeRoot, "looper-app-stamped"), old())
	gitDir := filepath.Join(debris, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	// A bare marker with no HEAD/objects/refs: stamped, but unusable.
	if err := os.WriteFile(filepath.Join(gitDir, "fixer-owner"), []byte("fixer-1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Chtimes(debris, old(), old()); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}

	var removed []string
	if _, err := RunDiskSweep(context.Background(), sweepOptions(
		DiskSweepRoot{ProjectID: "app", RepoPath: filepath.Join(t.TempDir(), "repo"), WorktreeRoot: worktreeRoot},
		stubSweepGit{}, &removed)); err != nil {
		t.Fatalf("RunDiskSweep() error = %v", err)
	}
	if len(removed) != 1 || removed[0] != debris {
		t.Fatalf("removed = %v, want [%s]", removed, debris)
	}
}

func containerOptions(sharedRoot string, live []string, git DiskSweepGit, removed *[]string) ContainerSweepOptions {
	return ContainerSweepOptions{
		SharedRoot:         sharedRoot,
		LiveContainerNames: live,
		Git:                git,
		Budget:             100,
		RetentionCutoff:    sweepCutoff(),
		RemoveAll: func(path string) error {
			*removed = append(*removed, path)
			return os.RemoveAll(path)
		},
	}
}

// mkContainer builds <shared>/<container>/<project>/<checkout>, the layout
// DefaultProjectWorktreeRoot produces.
func mkContainer(t *testing.T, sharedRoot, container string, modified time.Time) string {
	t.Helper()
	path := filepath.Join(sharedRoot, container)
	mkdirAt(t, filepath.Join(path, "project_1", "worker-worktree"), modified)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatalf("Chtimes(%q) error = %v", path, err)
	}
	return path
}

func TestRunContainerSweepRemovesUnreachableContainers(t *testing.T) {
	sharedRoot := t.TempDir()
	dead := mkContainer(t, sharedRoot, "repo-dead", old())
	livePath := mkContainer(t, sharedRoot, "repo-live", old())

	var removed []string
	plan, err := RunContainerSweep(context.Background(), containerOptions(sharedRoot, []string{"repo-live"}, stubSweepGit{}, &removed))
	if err != nil {
		t.Fatalf("RunContainerSweep() error = %v", err)
	}
	if len(removed) != 1 || removed[0] != dead {
		t.Fatalf("removed = %v, want [%s]", removed, dead)
	}
	if plan.Summary.Unregistered != 1 {
		t.Fatalf("unreachable = %d, want 1", plan.Summary.Unregistered)
	}
	if _, err := os.Stat(livePath); err != nil {
		t.Fatalf("live project container was removed: %v", err)
	}
}

// One protected checkout protects its whole container: a partially removed
// tree is a state no later pass can reason about.
func TestRunContainerSweepPreservesContainerWithDirtyCheckout(t *testing.T) {
	sharedRoot := t.TempDir()
	container := filepath.Join(sharedRoot, "repo-dead")
	dirty := writeUsableCheckout(t, filepath.Join(container, "project_1", "worker-worktree"), old())
	if err := os.Chtimes(container, old(), old()); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}

	var removed []string
	plan, err := RunContainerSweep(context.Background(), containerOptions(sharedRoot, nil,
		stubSweepGit{clean: map[string]bool{dirty: false}}, &removed))
	if err != nil {
		t.Fatalf("RunContainerSweep() error = %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none", removed)
	}
	if _, reason := reasonFor(t, plan, container); reason != "dirty_worktree_inside" {
		t.Fatalf("reason = %q, want dirty_worktree_inside", reason)
	}
}

func TestRunContainerSweepPreservesContainerWithRegisteredCheckout(t *testing.T) {
	sharedRoot := t.TempDir()
	container := filepath.Join(sharedRoot, "repo-dead")
	checkout := filepath.Join(container, "project_1", "worker-worktree")
	mkdirAt(t, checkout, old())
	if err := os.Chtimes(container, old(), old()); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}

	var removed []string
	options := containerOptions(sharedRoot, nil, stubSweepGit{}, &removed)
	options.RegisteredPaths = []string{checkout}

	plan, err := RunContainerSweep(context.Background(), options)
	if err != nil {
		t.Fatalf("RunContainerSweep() error = %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want none", removed)
	}
	if _, reason := reasonFor(t, plan, container); reason != "registered_checkout_inside" {
		t.Fatalf("reason = %q, want registered_checkout_inside", reason)
	}
}

func TestRunContainerSweepHonorsRetentionAndBudget(t *testing.T) {
	sharedRoot := t.TempDir()
	fresh := mkContainer(t, sharedRoot, "repo-fresh", recent())
	mkContainer(t, sharedRoot, "repo-old-a", sweepNow.Add(-90*24*time.Hour))
	mkContainer(t, sharedRoot, "repo-old-b", sweepNow.Add(-60*24*time.Hour))

	var removed []string
	options := containerOptions(sharedRoot, nil, stubSweepGit{}, &removed)
	options.Budget = 1

	plan, err := RunContainerSweep(context.Background(), options)
	if err != nil {
		t.Fatalf("RunContainerSweep() error = %v", err)
	}
	if len(removed) != 1 || filepath.Base(removed[0]) != "repo-old-a" {
		t.Fatalf("removed = %v, want the oldest container only", removed)
	}
	// The backlog must stay visible past the budget.
	if plan.Summary.Unregistered != 3 {
		t.Fatalf("unreachable = %d, want 3", plan.Summary.Unregistered)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("container inside the retention window was removed: %v", err)
	}
}

func TestRunContainerSweepDoesNotSpendBudgetOnFailedRemoval(t *testing.T) {
	sharedRoot := t.TempDir()
	first := mkContainer(t, sharedRoot, "repo-first", old())
	second := mkContainer(t, sharedRoot, "repo-second", old())
	var attempts []string
	options := containerOptions(sharedRoot, nil, stubSweepGit{}, &attempts)
	options.Budget = 2
	options.RemoveAll = func(path string) error {
		attempts = append(attempts, path)
		if path == first {
			return errors.New("injected removal failure")
		}
		return os.RemoveAll(path)
	}
	plan, err := RunContainerSweep(context.Background(), options)
	if err != nil {
		t.Fatalf("RunContainerSweep() error = %v", err)
	}
	if len(attempts) != 2 || attempts[1] != second || plan.Summary.Removed != 1 || plan.Summary.Errors != 1 {
		t.Fatalf("attempts = %v summary = %#v, want both attempts with one removal and one error", attempts, plan.Summary)
	}
}

func TestRunContainerSweepDrainsOrphanedProjectInsideLiveContainer(t *testing.T) {
	sharedRoot := t.TempDir()
	container := filepath.Join(sharedRoot, "repo-live")
	liveProject := filepath.Join(container, "project-live")
	orphanProject := filepath.Join(container, "project-orphan")
	mkdirAt(t, filepath.Join(liveProject, "checkout"), old())
	mkdirAt(t, filepath.Join(orphanProject, "checkout"), old())
	if err := os.Chtimes(liveProject, old(), old()); err != nil {
		t.Fatalf("Chtimes live project: %v", err)
	}
	if err := os.Chtimes(orphanProject, old(), old()); err != nil {
		t.Fatalf("Chtimes orphan project: %v", err)
	}
	if err := os.Chtimes(container, old(), old()); err != nil {
		t.Fatalf("Chtimes container: %v", err)
	}
	var removed []string
	options := containerOptions(sharedRoot, []string{"repo-live"}, stubSweepGit{}, &removed)
	options.LiveProjectPaths = []string{liveProject}
	plan, err := RunContainerSweep(context.Background(), options)
	if err != nil {
		t.Fatalf("RunContainerSweep() error = %v", err)
	}
	if len(removed) != 1 || removed[0] != orphanProject || plan.Summary.Removed != 1 {
		t.Fatalf("removed = %v summary = %#v, want orphan project only", removed, plan.Summary)
	}
	if _, err := os.Stat(liveProject); err != nil {
		t.Fatalf("live project was removed: %v", err)
	}
}
