package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGatewayCleanupRescuesUnreferencedDetachedCommits(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/fixer")
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	writeFile(t, filepath.Join(worktree.WorktreePath, "fixer.txt"), "agent fix that never got pushed\n")
	commit, err := gateway.Commit(ctx, CommitInput{WorktreePath: worktree.WorktreePath, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Message: "fixer: unpushed work"})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if got := stringsTrimSpace(runGitMaybe(t, fixture.repoPath, "for-each-ref", "--contains", commit.CommitSHA, "--count=1", "--format=%(refname)")); got != "" {
		t.Fatalf("commit %q already contained by ref %q, want unreferenced fixture", commit.CommitSHA, got)
	}

	if err := gateway.CleanupWorktree(ctx, CleanupWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, WorktreePath: worktree.WorktreePath, Branch: "feature/fixer"}); err != nil {
		t.Fatalf("CleanupWorktree() error = %v", err)
	}
	if _, err := os.Stat(worktree.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree path still present after cleanup: err = %v", err)
	}

	wantRef := "refs/looper/rescue/" + sanitizeBranchName(filepath.Base(worktree.WorktreePath)) + "-" + commit.CommitSHA[:8]
	rescued := stringsTrimSpace(runGit(t, fixture.repoPath, "rev-parse", wantRef))
	if rescued != commit.CommitSHA {
		t.Fatalf("%s = %q, want pre-removal HEAD %q", wantRef, rescued, commit.CommitSHA)
	}
	if _, err := runGitCommand(fixture.repoPath, "cat-file", "-e", commit.CommitSHA+"^{commit}"); err != nil {
		t.Fatalf("rescued commit unreachable after cleanup: %v", err)
	}
}

func TestGatewayCleanupSkipsRescueWhenCommitAlreadyReferenced(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/fixer")
	gateway := fixture.gateway()

	// A freshly created detached worktree sits on the PR head commit, which the
	// branch refs already contain — nothing to rescue.
	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	if err := gateway.CleanupWorktree(ctx, CleanupWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, WorktreePath: worktree.WorktreePath, Branch: "feature/fixer"}); err != nil {
		t.Fatalf("CleanupWorktree() error = %v", err)
	}
	if got := stringsTrimSpace(runGitMaybe(t, fixture.repoPath, "for-each-ref", "--format=%(refname)", "refs/looper/rescue/")); got != "" {
		t.Fatalf("rescue refs = %q, want none for an already-referenced HEAD", got)
	}
}

func TestGatewayCleanupSkipsRescueForBranchWorktree(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/fixer")
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeBranch})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	writeFile(t, filepath.Join(worktree.WorktreePath, "fixer.txt"), "committed on the branch\n")
	if _, err := gateway.Commit(ctx, CommitInput{WorktreePath: worktree.WorktreePath, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Message: "fixer: branch work"}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	if err := gateway.CleanupWorktree(ctx, CleanupWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, WorktreePath: worktree.WorktreePath, Branch: "feature/fixer"}); err != nil {
		t.Fatalf("CleanupWorktree() error = %v", err)
	}
	if got := stringsTrimSpace(runGitMaybe(t, fixture.repoPath, "for-each-ref", "--format=%(refname)", "refs/looper/rescue/")); got != "" {
		t.Fatalf("rescue refs = %q, want none for a branch-mode worktree", got)
	}
}

func TestGatewayCleanupSucceedsWhenDetachedWorktreePathIsMissing(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/fixer")
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	if err := os.RemoveAll(worktree.WorktreePath); err != nil {
		t.Fatalf("RemoveAll(%q) error = %v", worktree.WorktreePath, err)
	}

	if err := gateway.CleanupWorktree(ctx, CleanupWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, WorktreePath: worktree.WorktreePath, Branch: "feature/fixer"}); err != nil {
		t.Fatalf("CleanupWorktree() error = %v", err)
	}
	if got := stringsTrimSpace(runGitMaybe(t, fixture.repoPath, "for-each-ref", "--format=%(refname)", "refs/looper/rescue/")); got != "" {
		t.Fatalf("rescue refs = %q, want none for a missing worktree path", got)
	}
}
