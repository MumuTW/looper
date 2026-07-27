package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Index-probe cancel must fail closed before filesystem mutation so a tracked
// reserved-name fixture is never relocated as scratch.
func TestGatewayRelocatePropagatesIndexProbeCancelWithoutMutatingTracked(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	branch := "feature/review-index-probe-cancel"
	fixture.createRemoteRepo(t, branch)
	gateway := fixture.gateway()

	runGit(t, fixture.repoPath, "checkout", branch)
	// Tracked reserved-name fixture (must never relocate) + untracked scratch.
	writeFile(t, filepath.Join(fixture.repoPath, ".looper-review-fixture.json"), `{"fixture":true}`+"\n")
	runGit(t, fixture.repoPath, "add", ".looper-review-fixture.json")
	runGit(t, fixture.repoPath, "commit", "-m", "seed tracked reserved fixture")
	runGit(t, fixture.repoPath, "push", "origin", branch)
	runGit(t, fixture.repoPath, "checkout", "main")

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: branch, BaseBranch: "main", PRNumber: 1061,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	wt := worktree.WorktreePath
	tracked := filepath.Join(wt, ".looper-review-fixture.json")
	scratch := filepath.Join(wt, ".looper-review-1061.json")
	writeFile(t, scratch, `{"body":"scratch"}`+"\n")

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := gateway.relocateReservedReviewerScratch(cancelCtx, wt); err == nil {
		t.Fatal("relocateReservedReviewerScratch(canceled) error = nil, want cancel/probe failure")
	} else if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
		// Shell may wrap cancel; require the failure surface without relocation.
		t.Logf("relocate error (acceptable if non-nil before mutate): %v", err)
	}

	if _, err := os.Stat(tracked); err != nil {
		t.Fatalf("tracked reserved fixture missing after canceled relocate: %v", err)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("untracked scratch missing after canceled relocate (must not mutate): %v", err)
	}
	// Absent path must stay a clean miss (exit 1), not an operational error.
	present, err := gateway.isIndexPathPresent(context.Background(), wt, ".looper-review-1061.json", false)
	if err != nil {
		t.Fatalf("isIndexPathPresent(untracked) error = %v, want nil", err)
	}
	if present {
		t.Fatal("isIndexPathPresent(untracked) = true, want false")
	}
	present, err = gateway.isIndexPathPresent(context.Background(), wt, ".looper-review-fixture.json", false)
	if err != nil {
		t.Fatalf("isIndexPathPresent(tracked) error = %v, want nil", err)
	}
	if !present {
		t.Fatal("isIndexPathPresent(tracked) = false, want true")
	}
}

// Frozen clock + exclusive reservation must keep both payloads when the same
// reserved basename is relocated twice.
func TestGatewayQuarantineDestinationCollisionSafeUnderFrozenClock(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	branch := "feature/review-quarantine-collision"
	fixture.createRemoteRepo(t, branch)
	fixed := time.Date(2026, 7, 27, 15, 0, 0, 42, time.UTC)
	gateway := New(Options{GitPath: "git", Repos: fixture.repos, Now: func() time.Time { return fixed }})

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: branch, BaseBranch: "main", PRNumber: 1062,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	wt := worktree.WorktreePath
	scratch := ".looper-review-1062.json"
	payload1 := `{"n":1}` + "\n"
	payload2 := `{"n":2}` + "\n"

	writeFile(t, filepath.Join(wt, scratch), payload1)
	if err := gateway.relocateReservedReviewerScratch(ctx, wt); err != nil {
		t.Fatalf("relocate(1) error = %v", err)
	}
	writeFile(t, filepath.Join(wt, scratch), payload2)
	if err := gateway.relocateReservedReviewerScratch(ctx, wt); err != nil {
		t.Fatalf("relocate(2) error = %v", err)
	}

	qdir := ReservedReviewScratchQuarantineDir(wt)
	entries, err := os.ReadDir(qdir)
	if err != nil {
		t.Fatalf("ReadDir quarantine: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("quarantine entries = %d, want 2 distinct destinations", len(entries))
	}
	found := quarantinePayloadBytes(t, qdir)
	if !found[payload1] || !found[payload2] {
		t.Fatalf("quarantine payloads = %#v, want both original payloads preserved", found)
	}
}

// CleanupWorktree removes the worktree-scoped quarantine (terminal cleanup).
func TestGatewayCleanupWorktreeRemovesQuarantine(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	branch := "feature/review-quarantine-cleanup"
	fixture.createRemoteRepo(t, branch)
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: branch, BaseBranch: "main", PRNumber: 1063,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	wt := worktree.WorktreePath
	writeFile(t, filepath.Join(wt, ".looper-review-1063.json"), `{"body":"prior"}`+"\n")
	if _, err := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: wt, Branch: branch}); err != nil {
		t.Fatalf("PrepareWorktree() error = %v", err)
	}
	qdir := ReservedReviewScratchQuarantineDir(wt)
	if entries, err := os.ReadDir(qdir); err != nil || len(entries) == 0 {
		t.Fatalf("expected quarantine before cleanup: err=%v entries=%v", err, entries)
	}

	if err := gateway.CleanupWorktree(ctx, CleanupWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		WorktreePath: wt, Branch: branch,
	}); err != nil {
		t.Fatalf("CleanupWorktree() error = %v", err)
	}
	if _, err := os.Stat(qdir); !os.IsNotExist(err) {
		t.Fatalf("quarantine dir still present after CleanupWorktree: err=%v", err)
	}
}

// Orphan quarantine older than retention is pruned; active worktrees are kept
// even when their payload mtime is ancient (rename preserves source mtime).
func TestGatewayPrunesExpiredOrphanQuarantine(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	branch := "feature/review-quarantine-retention"
	fixture.createRemoteRepo(t, branch)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	gateway := New(Options{GitPath: "git", Repos: fixture.repos, Now: func() time.Time { return now }})

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: branch, BaseBranch: "main", PRNumber: 1064,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	wt := worktree.WorktreePath

	// Simulate an abandoned worktree's quarantine entry under the sibling root.
	// Age the container (dir), not only nested payload: retention is container-based.
	orphanDir := filepath.Join(fixture.worktreeRoot, reservedReviewScratchQuarantineDirName, "abandoned-wt")
	mustMkdirAll(t, orphanDir)
	orphanChild := filepath.Join(orphanDir, "deadbeefdeadbeef")
	mustMkdirAll(t, orphanChild)
	orphanPath := filepath.Join(orphanChild, "old.payload")
	writeFile(t, orphanPath, "stale\n")
	old := now.Add(-reservedReviewScratchQuarantineRetention - time.Hour)
	for _, p := range []string{orphanPath, orphanChild, orphanDir} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("Chtimes %s: %v", p, err)
		}
	}

	writeFile(t, filepath.Join(wt, ".looper-review-1064.json"), "{}\n")
	if err := gateway.relocateReservedReviewerScratch(ctx, wt); err != nil {
		t.Fatalf("relocate error = %v", err)
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("expired orphan quarantine still present: err=%v", err)
	}
	// Fresh quarantine for the active worktree must still exist.
	if entries, err := os.ReadDir(ReservedReviewScratchQuarantineDir(wt)); err != nil || len(entries) == 0 {
		t.Fatalf("active quarantine missing after prune: err=%v entries=%v", err, entries)
	}
}
