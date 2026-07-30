package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Retiring a generation is the whole fence: the next claim lands on a different
// path, so the previous writer's open handles cannot reach it. This is the one
// assertion that proves containment does not depend on knowing whether the old
// process exited.
func TestGatewayRetiredGenerationDivergesFromNextClaimPath(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/fixer")
	gateway := fixture.gateway()

	first, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       "feature/fixer",
		BaseBranch:   "main",
		PRNumber:     42,
		CheckoutMode: CheckoutModeDetached,
	})
	if err != nil {
		t.Fatalf("CreateWorktree(first) error = %v", err)
	}
	if first.Generation != 1 {
		t.Fatalf("first.Generation = %d, want 1", first.Generation)
	}
	if strings.Contains(filepath.Base(first.WorktreePath), "-g") {
		t.Fatalf("first.WorktreePath = %q, want generation 1 to keep the historical name", first.WorktreePath)
	}

	// A stale agent holds an open write handle in generation 1's directory.
	staleFile, err := os.Create(filepath.Join(first.WorktreePath, "stale-agent-output.txt"))
	if err != nil {
		t.Fatalf("open stale writer error = %v", err)
	}
	defer staleFile.Close()

	if err := fixture.repos.Worktrees.Retire(ctx, first.ID, "2026-07-30T12:00:00.000Z"); err != nil {
		t.Fatalf("Retire() error = %v", err)
	}

	second, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       "feature/fixer",
		BaseBranch:   "main",
		PRNumber:     42,
		CheckoutMode: CheckoutModeDetached,
	})
	if err != nil {
		t.Fatalf("CreateWorktree(second) error = %v", err)
	}
	if second.Generation != 2 {
		t.Fatalf("second.Generation = %d, want 2", second.Generation)
	}
	if second.WorktreePath == first.WorktreePath {
		t.Fatalf("second.WorktreePath = %q, want a path the retired generation cannot reach", second.WorktreePath)
	}
	if !strings.HasSuffix(second.WorktreePath, "-g2") {
		t.Fatalf("second.WorktreePath = %q, want the generation in the directory name", second.WorktreePath)
	}
	if _, err := os.Stat(first.WorktreePath); err != nil {
		t.Fatalf("retired generation directory missing: %v", err)
	}

	// The stale writer keeps working, and its writes land harmlessly: they are
	// invisible to the live generation. Proving the write succeeds is the honest
	// assertion for a filesystem fence — we do not claim the process is gone.
	if _, err := staleFile.WriteString("still writing after retirement\n"); err != nil {
		t.Fatalf("stale write error = %v", err)
	}
	if err := staleFile.Sync(); err != nil {
		t.Fatalf("stale sync error = %v", err)
	}
	liveStatus := runGit(t, second.WorktreePath, "status", "--porcelain")
	if strings.Contains(liveStatus, "stale-agent-output.txt") {
		t.Fatalf("live generation status = %q, want the stale writer's output to be invisible", liveStatus)
	}

	// GetByBranch resolves the live generation only.
	live, err := fixture.repos.Worktrees.GetByBranch(ctx, fixture.projectID, "feature/fixer")
	if err != nil {
		t.Fatalf("GetByBranch() error = %v", err)
	}
	if live == nil || live.ID != second.ID {
		t.Fatalf("GetByBranch() = %#v, want the live generation", live)
	}

	// Cleaning the live generation must not touch the retired directory.
	if err := gateway.CleanupWorktree(ctx, CleanupWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot, WorktreePath: second.WorktreePath, Branch: "feature/fixer",
	}); err != nil {
		t.Fatalf("CleanupWorktree() error = %v", err)
	}
	if _, err := os.Stat(first.WorktreePath); err != nil {
		t.Fatalf("retired generation removed by live cleanup: %v", err)
	}
}

// Every push that updates an existing remote branch carries a lease, so a
// remote that moved behind our back is a typed, recoverable error rather than
// an opaque git rejection or a silent divergence.
func TestGatewayPushLeasesEveryUpdateToAnExistingRemoteBranch(t *testing.T) {
	ctx := context.Background()

	t.Run("stale explicit lease reports the remote head change", func(t *testing.T) {
		fixture := newFixture(t)
		fixture.createRemoteRepo(t, "feature/fixer")
		gateway := fixture.gateway()

		worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
			ProjectID: fixture.projectID, RepoPath: fixture.repoPath,
			WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main",
		})
		if err != nil {
			t.Fatalf("CreateWorktree() error = %v", err)
		}
		staleLease := stringsTrimSpace(runGit(t, worktree.WorktreePath, "rev-parse", "HEAD"))
		writeFile(t, filepath.Join(worktree.WorktreePath, "local.txt"), "local work\n")
		if _, err := gateway.Commit(ctx, CommitInput{WorktreePath: worktree.WorktreePath, Message: "fixer: local work"}); err != nil {
			t.Fatalf("Commit() error = %v", err)
		}
		// Someone else moved the branch after we read the lease.
		fixture.advanceRemoteBranch(t, "feature/fixer", "other.txt", "out of band\n")

		err = gateway.Push(ctx, PushInput{
			RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
			WorktreePath: worktree.WorktreePath, Branch: "feature/fixer",
			ExpectedRemoteHeadSHA: staleLease,
		})
		var changed *RemoteHeadChangedError
		if err == nil || !errors.As(err, &changed) {
			t.Fatalf("Push() error = %v, want *RemoteHeadChangedError", err)
		}
	})

	t.Run("unleased update to an existing branch is refused", func(t *testing.T) {
		fixture := newFixture(t)
		fixture.createRemoteRepo(t, "feature/fixer")
		gateway := fixture.gateway()

		worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
			ProjectID: fixture.projectID, RepoPath: fixture.repoPath,
			WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main",
		})
		if err != nil {
			t.Fatalf("CreateWorktree() error = %v", err)
		}
		writeFile(t, filepath.Join(worktree.WorktreePath, "local.txt"), "local work\n")
		if _, err := gateway.Commit(ctx, CommitInput{WorktreePath: worktree.WorktreePath, Message: "fixer: local work"}); err != nil {
			t.Fatalf("Commit() error = %v", err)
		}
		fixture.advanceRemoteBranch(t, "feature/fixer", "other.txt", "out of band\n")

		// No ExpectedRemoteHeadSHA: the gateway derives the lease from the
		// current remote head, which our local HEAD does not descend from.
		err = gateway.Push(ctx, PushInput{
			RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
			WorktreePath: worktree.WorktreePath, Branch: "feature/fixer",
		})
		var changed *RemoteHeadChangedError
		if err == nil || !errors.As(err, &changed) {
			t.Fatalf("Push() error = %v, want *RemoteHeadChangedError for an unleased update", err)
		}
		remoteHead := stringsTrimSpace(runGit(t, fixture.remotePath, "rev-parse", "refs/heads/feature/fixer"))
		localHead := stringsTrimSpace(runGit(t, worktree.WorktreePath, "rev-parse", "HEAD"))
		if remoteHead == localHead {
			t.Fatal("remote branch was overwritten despite the refusal")
		}
	})

	t.Run("creating a branch that does not exist remotely needs no lease", func(t *testing.T) {
		fixture := newFixture(t)
		fixture.createMainOnlyRepo(t)
		gateway := fixture.gateway()

		worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
			ProjectID: fixture.projectID, RepoPath: fixture.repoPath,
			WorktreeRoot: fixture.worktreeRoot, Branch: "looper/brand-new", BaseBranch: "main",
		})
		if err != nil {
			t.Fatalf("CreateWorktree() error = %v", err)
		}
		writeFile(t, filepath.Join(worktree.WorktreePath, "new.txt"), "new branch\n")
		if _, err := gateway.Commit(ctx, CommitInput{WorktreePath: worktree.WorktreePath, Message: "worker: new branch"}); err != nil {
			t.Fatalf("Commit() error = %v", err)
		}
		if err := gateway.Push(ctx, PushInput{
			RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
			WorktreePath: worktree.WorktreePath, Branch: "looper/brand-new",
		}); err != nil {
			t.Fatalf("Push() error = %v, want a plain create to succeed", err)
		}
	})
}
