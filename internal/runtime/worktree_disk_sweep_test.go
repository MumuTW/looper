package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
)

// mkDebris creates a directory under the fixture's worktree root that no
// worktrees row will ever claim — the shape left behind by an interrupted
// `git worktree add` or a test binary that resolved the real worktree root.
func mkDebris(t *testing.T, root, name string, modified time.Time) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", path, err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatalf("Chtimes(%q) error = %v", path, err)
	}
	return path
}

func TestWorktreeCleanupPassRemovesUnregisteredDirectories(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	fixture.config.Daemon.WorktreeCleanup.RetentionDays = 7
	debris := mkDebris(t, fixture.root, "looper-orphan", fixture.now.Add(-30*24*time.Hour))
	git := &fakeWorktreeCleanupGit{}

	summary := fixture.runtime.runWorktreeCleanupPass(context.Background(), fixture.repos, git, fixture.config)

	if summary.DiskSweep.Removed != 1 || summary.DiskSweep.Unregistered != 1 {
		t.Fatalf("diskSweep = %#v, want unregistered=1 removed=1", summary.DiskSweep)
	}
	if _, err := os.Stat(debris); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unregistered directory survived the sweep: %v", err)
	}
}

// The sweep must never touch a directory the worktrees table claims, even when
// that row is already cleaned: the record pass owns those paths and its
// provenance is the only thing distinguishing a failed removal from debris.
func TestWorktreeCleanupPassLeavesRegisteredDirectories(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	fixture.config.Daemon.WorktreeCleanup.RetentionDays = 7
	worktree := fixture.seedWorktreeAt(t, "wt_registered", "feature/registered", true, fixture.now.Add(-30*24*time.Hour))
	if err := os.Chtimes(worktree.WorktreePath, fixture.now.Add(-30*24*time.Hour), fixture.now.Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
	git := &fakeWorktreeCleanupGit{clean: map[string]bool{worktree.WorktreePath: false}}

	summary := fixture.runtime.runWorktreeCleanupPass(context.Background(), fixture.repos, git, fixture.config)

	if summary.DiskSweep.Removed != 0 || summary.DiskSweep.Unregistered != 0 {
		t.Fatalf("diskSweep = %#v, want the registered path left to the record pass", summary.DiskSweep)
	}
	if _, err := os.Stat(worktree.WorktreePath); err != nil {
		t.Fatalf("registered worktree was swept: %v", err)
	}
}

func TestWorktreeCleanupPassKeepsUnregisteredDirectoriesWithinRetention(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	fixture.config.Daemon.WorktreeCleanup.RetentionDays = 7
	fresh := mkDebris(t, fixture.root, "looper-fresh", fixture.now.Add(-time.Hour))
	git := &fakeWorktreeCleanupGit{}

	summary := fixture.runtime.runWorktreeCleanupPass(context.Background(), fixture.repos, git, fixture.config)

	if summary.DiskSweep.Removed != 0 {
		t.Fatalf("diskSweep = %#v, want no removal inside the retention window", summary.DiskSweep)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("recent directory was swept: %v", err)
	}
}

// maxDiskSweepPerTick = 0 disables the sweep without disabling the record pass,
// which is the escape hatch an operator needs if a root ever holds something
// the gates misjudge.
func TestWorktreeCleanupPassSkipsDiskSweepWhenBudgetIsZero(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	fixture.config.Daemon.WorktreeCleanup.RetentionDays = 7
	fixture.config.Daemon.WorktreeCleanup.MaxDiskSweepPerTick = 0
	debris := mkDebris(t, fixture.root, "looper-orphan", fixture.now.Add(-30*24*time.Hour))
	git := &fakeWorktreeCleanupGit{}

	summary := fixture.runtime.runWorktreeCleanupPass(context.Background(), fixture.repos, git, fixture.config)

	if summary.DiskSweep.Scanned != 0 || summary.DiskSweep.Removed != 0 {
		t.Fatalf("diskSweep = %#v, want an inert sweep", summary.DiskSweep)
	}
	if _, err := os.Stat(debris); err != nil {
		t.Fatalf("directory was swept with a zero budget: %v", err)
	}
}

func TestWorktreeCleanupPassDiskSweepHonorsDryRun(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	fixture.config.Daemon.WorktreeCleanup.RetentionDays = 7
	fixture.config.Daemon.WorktreeCleanup.DryRun = true
	debris := mkDebris(t, fixture.root, "looper-orphan", fixture.now.Add(-30*24*time.Hour))
	git := &fakeWorktreeCleanupGit{}

	summary := fixture.runtime.runWorktreeCleanupPass(context.Background(), fixture.repos, git, fixture.config)

	if summary.DiskSweep.Unregistered != 1 || summary.DiskSweep.Removed != 0 {
		t.Fatalf("diskSweep = %#v, want unregistered reported and nothing removed", summary.DiskSweep)
	}
	if _, err := os.Stat(debris); err != nil {
		t.Fatalf("dry run removed %q: %v", debris, err)
	}
}

func TestWorktreeCleanupPassPropagatesDiskSweepFailure(t *testing.T) {
	fixture := newWorktreeCleanupFixture(t)
	fixture.config.Daemon.WorktreeCleanup.RetentionDays = 7
	listErr := errors.New("git worktree listing failed")
	summary := fixture.runtime.runWorktreeCleanupPass(context.Background(), fixture.repos, &fakeWorktreeCleanupGit{listErr: listErr}, fixture.config)

	if summary.DiskSweep.Failed == 0 {
		t.Fatalf("diskSweep = %#v, want a disk-sweep failure", summary.DiskSweep)
	}
	if summary.Failed != summary.DiskSweep.Failed || summary.LastStatus != "failed" {
		t.Fatalf("summary = %#v, want top-level failure propagation", summary)
	}
	if !strings.Contains(summary.LastError, listErr.Error()) {
		t.Fatalf("lastError = %q, want %q", summary.LastError, listErr.Error())
	}
}

func TestContainerNameUnder(t *testing.T) {
	t.Parallel()

	shared := filepath.Join("/looper", "worktrees")
	tests := []struct {
		name string
		root string
		want string
		ok   bool
	}{
		{name: "default layout", root: filepath.Join(shared, "repo-abc", "project_1"), want: "repo-abc", ok: true},
		{name: "custom single-segment root", root: filepath.Join(shared, "my-project"), want: "my-project", ok: true},
		{name: "outside the shared root", root: "/elsewhere/worktrees/repo-abc", ok: false},
		{name: "the shared root itself", root: shared, ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := containerNameUnder(shared, test.root)
			if ok != test.ok || got != test.want {
				t.Fatalf("containerNameUnder(%q, %q) = (%q, %v), want (%q, %v)", shared, test.root, got, ok, test.want, test.ok)
			}
		})
	}
}

// The container tier is what reaches debris the per-root tier structurally
// cannot: a repo path no project resolves any more.
func TestWorktreeCleanupPassRemovesUnreachableContainers(t *testing.T) {
	fixture := newWorktreeCleanupFixture(t)
	fixture.config.Daemon.WorktreeCleanup.RetentionDays = 7

	sharedRoot, err := worktreeSharedRoot()
	if err != nil {
		t.Fatalf("worktreeSharedRoot() error = %v", err)
	}
	dead := filepath.Join(sharedRoot, "repo-unreachable-fixture")
	mkDebris(t, filepath.Dir(filepath.Join(dead, "project_1", "wt")), "wt", fixture.now.Add(-30*24*time.Hour))
	if err := os.Chtimes(dead, fixture.now.Add(-30*24*time.Hour), fixture.now.Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dead) })

	// The fixture project's own container must survive the same pass.
	live := filepath.Join(sharedRoot, config.ToRepoWorktreeDirectoryName(fixture.project.RepoPath))
	mkDebris(t, filepath.Dir(filepath.Join(live, "project_1", "wt")), "wt", fixture.now.Add(-30*24*time.Hour))
	if err := os.Chtimes(live, fixture.now.Add(-30*24*time.Hour), fixture.now.Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(live) })

	summary := fixture.runtime.runWorktreeCleanupPass(context.Background(), fixture.repos, &fakeWorktreeCleanupGit{}, fixture.config)

	if summary.DiskSweep.ContainersRemoved < 1 {
		t.Fatalf("diskSweep = %#v, want at least one container removed", summary.DiskSweep)
	}
	if _, err := os.Stat(dead); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreachable container survived: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live project container was removed: %v", err)
	}
}
