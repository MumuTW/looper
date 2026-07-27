package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// PrepareWorktree double-relocate must keep an already-old reserved payload
// (mtime older than retention). Lifecycle, not mtime, owns active quarantine.
func TestGatewayPreparePreservesOldMtimeActiveQuarantine(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	branch := "feature/review-quarantine-old-payload"
	fixture.createRemoteRepo(t, branch)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	gateway := New(Options{GitPath: "git", Repos: fixture.repos, Now: func() time.Time { return now }})

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: branch, BaseBranch: "main", PRNumber: 1065,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	wt := worktree.WorktreePath
	scratch := ".looper-review-1065.json"
	payload := `{"body":"ancient leftover"}` + "\n"
	src := filepath.Join(wt, scratch)
	writeFile(t, src, payload)
	old := now.Add(-reservedReviewScratchQuarantineRetention - 48*time.Hour)
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatalf("Chtimes scratch: %v", err)
	}

	prepared, err := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: wt, Branch: branch})
	if err != nil {
		t.Fatalf("PrepareWorktree() error = %v", err)
	}
	if !prepared.Clean {
		t.Fatal("PrepareWorktree().Clean = false, want true after relocating old reserved scratch")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("scratch still in worktree: err=%v", err)
	}
	qdir := ReservedReviewScratchQuarantineDir(wt)
	if payloads := quarantinePayloadBytes(t, qdir); !payloads[payload] {
		t.Fatalf("active old-mtime quarantine missing after Prepare double-relocate: %#v", payloads)
	}
	// Sibling active quarantine for another still-present worktree name must
	// also survive opportunistic prune (lifecycle, not age).
	otherWT := filepath.Join(fixture.worktreeRoot, "other-active-wt")
	mustMkdirAll(t, otherWT)
	otherQ := filepath.Join(fixture.worktreeRoot, reservedReviewScratchQuarantineDirName, "other-active-wt")
	mustMkdirAll(t, otherQ)
	otherPayload := filepath.Join(otherQ, "held.payload")
	writeFile(t, otherPayload, "keep-active\n")
	if err := os.Chtimes(otherPayload, old, old); err != nil {
		t.Fatalf("Chtimes other: %v", err)
	}
	if err := gateway.relocateReservedReviewerScratch(ctx, wt); err != nil {
		t.Fatalf("relocate (prune pass) error = %v", err)
	}
	if _, err := os.Stat(otherPayload); err != nil {
		t.Fatalf("active sibling quarantine pruned by mtime: %v", err)
	}
}

// Near-NAME_MAX reserved basename must quarantine via real-Git Prepare without
// ENAMETOOLONG (bounded random subdir + original basename layout).
func TestGatewayPrepareQuarantinesNearNameMaxScratch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	branch := "feature/review-quarantine-namemax"
	fixture.createRemoteRepo(t, branch)
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: branch, BaseBranch: "main", PRNumber: 1066,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	wt := worktree.WorktreePath
	// 240-byte root name is valid on common Unix NAME_MAX=255; hex-encoding the
	// whole name would push the destination component past the limit.
	middle := strings.Repeat("n", 240-len(".looper-review-")-len(".json"))
	scratch := ".looper-review-" + middle + ".json"
	if len(scratch) != 240 {
		t.Fatalf("scratch len = %d, want 240", len(scratch))
	}
	if !isReservedReviewerScratchPath(scratch, false) {
		t.Fatalf("scratch %q not classified as reserved", scratch)
	}
	payload := `{"long":true}` + "\n"
	writeFile(t, filepath.Join(wt, scratch), payload)

	prepared, err := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: wt, Branch: branch})
	if err != nil {
		t.Fatalf("PrepareWorktree() error = %v", err)
	}
	if !prepared.Clean {
		t.Fatal("PrepareWorktree().Clean = false, want true")
	}
	if _, err := os.Stat(filepath.Join(wt, scratch)); !os.IsNotExist(err) {
		t.Fatalf("long scratch still in worktree: err=%v", err)
	}
	qdir := ReservedReviewScratchQuarantineDir(wt)
	foundName := false
	err = filepath.WalkDir(qdir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return err
		}
		if d.Name() == scratch {
			foundName = true
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if string(b) != payload {
				t.Fatalf("payload = %q, want %q", b, payload)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk quarantine: %v", err)
	}
	if !foundName {
		t.Fatalf("quarantine missing original basename %q under %s", scratch, qdir)
	}
}

// Git start/removal failures that only match the broad "not found" phrasing
// must not delete quarantine while the worktree path still exists.
func TestGatewayCleanupPreservesQuarantineWhenGitRemoveFailsWorktreePresent(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	branch := "feature/review-quarantine-git-fail"
	fixture.createRemoteRepo(t, branch)
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: branch, BaseBranch: "main", PRNumber: 1067,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	wt := worktree.WorktreePath
	payload := `{"body":"recover-me"}` + "\n"
	writeFile(t, filepath.Join(wt, ".looper-review-1067.json"), payload)
	if _, err := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: wt, Branch: branch}); err != nil {
		t.Fatalf("PrepareWorktree() error = %v", err)
	}
	qdir := ReservedReviewScratchQuarantineDir(wt)
	if payloads := quarantinePayloadBytes(t, qdir); !payloads[payload] {
		t.Fatalf("expected quarantine before cleanup failure: %#v", payloads)
	}

	// Missing git binary yields a start error containing "not found" / "no such
	// file" without ever running worktree remove.
	missingGit := filepath.Join(t.TempDir(), "missing-git-binary")
	broken := New(Options{GitPath: missingGit, Repos: fixture.repos})
	err = broken.CleanupWorktree(ctx, CleanupWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		WorktreePath: wt, Branch: branch,
	})
	if err == nil {
		t.Fatal("CleanupWorktree(missing git) error = nil, want failure")
	}
	if _, statErr := os.Stat(wt); statErr != nil {
		t.Fatalf("worktree path missing after failed cleanup: %v", statErr)
	}
	if payloads := quarantinePayloadBytes(t, qdir); !payloads[payload] {
		t.Fatalf("quarantine deleted after false-positive missing-worktree match: err=%v payloads=%#v", err, payloads)
	}
}
