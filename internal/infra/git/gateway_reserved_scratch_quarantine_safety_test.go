package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CleanupWorktree must refuse when the quarantine root is a symlink so RemoveAll
// cannot follow it and delete a directory outside Looper's quarantine tree.
func TestGatewayCleanupRefusesSymlinkedQuarantineRoot(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	branch := "feature/review-quarantine-symlink-root"
	fixture.createRemoteRepo(t, branch)
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: branch, BaseBranch: "main", PRNumber: 1080,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	wt := worktree.WorktreePath
	payload := `{"body":"symlink-root"}` + "\n"
	writeFile(t, filepath.Join(wt, ".looper-review-1080.json"), payload)
	if _, err := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: wt, Branch: branch}); err != nil {
		t.Fatalf("PrepareWorktree() error = %v", err)
	}
	gen, err := readReservedReviewScratchGeneration(wt)
	if err != nil || gen == "" {
		t.Fatalf("expected generation after prepare: gen=%q err=%v", gen, err)
	}
	qdir := quarantineDirForGeneration(wt, gen)
	if payloads := quarantinePayloadBytes(t, qdir); !payloads[payload] {
		t.Fatalf("expected quarantine before symlink swap: %#v", payloads)
	}

	// Outside target that would be destroyed if RemoveAll followed a root symlink.
	outsideRoot := filepath.Join(t.TempDir(), "outside-quarantine-target")
	mustMkdirAll(t, outsideRoot)
	// Move real quarantine contents under outside, then replace the quarantine
	// root with a symlink to outside (attack: agent-written symlink at worktree root).
	realRoot := filepath.Join(fixture.worktreeRoot, reservedReviewScratchQuarantineDirName)
	entryName := filepath.Base(qdir)
	if err := os.Rename(qdir, filepath.Join(outsideRoot, entryName)); err != nil {
		t.Fatalf("rename quarantine entry outside: %v", err)
	}
	// Remove empty real root and any remaining entries so we can replace with symlink.
	if err := os.RemoveAll(realRoot); err != nil {
		t.Fatalf("RemoveAll real root: %v", err)
	}
	if err := os.Symlink(outsideRoot, realRoot); err != nil {
		t.Fatalf("Symlink quarantine root: %v", err)
	}
	// Sentinel that must survive terminal cleanup.
	sentinel := filepath.Join(outsideRoot, "must-survive.txt")
	writeFile(t, sentinel, "keep\n")
	outsideEntry := filepath.Join(outsideRoot, entryName)
	if _, err := os.Stat(outsideEntry); err != nil {
		t.Fatalf("outside quarantine entry missing before cleanup: %v", err)
	}

	err = gateway.CleanupWorktree(ctx, CleanupWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		WorktreePath: wt, Branch: branch,
	})
	if err == nil {
		t.Fatal("CleanupWorktree() error = nil, want refuse symlinked quarantine root")
	}
	if _, statErr := os.Stat(sentinel); statErr != nil {
		t.Fatalf("outside sentinel deleted via symlink follow: %v", statErr)
	}
	if _, statErr := os.Stat(outsideEntry); statErr != nil {
		t.Fatalf("outside quarantine entry deleted via symlink follow: %v", statErr)
	}
}

// Terminal cleanup of a recreated worktree path must not delete a prior
// lifecycle's still-retained quarantine generation.
func TestGatewayCleanupPreservesPriorGenerationQuarantineOnPathRecreate(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	branch := "feature/review-quarantine-generation"
	fixture.createRemoteRepo(t, branch)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	gateway := New(Options{GitPath: "git", Repos: fixture.repos, Now: func() time.Time { return now }})

	first, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: branch, BaseBranch: "main", PRNumber: 1081,
	})
	if err != nil {
		t.Fatalf("CreateWorktree(first) error = %v", err)
	}
	wt := first.WorktreePath
	priorPayload := `{"body":"prior-lifecycle"}` + "\n"
	writeFile(t, filepath.Join(wt, ".looper-review-1081.json"), priorPayload)
	if _, err := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: wt, Branch: branch}); err != nil {
		t.Fatalf("PrepareWorktree(first) error = %v", err)
	}
	priorGen, err := readReservedReviewScratchGeneration(wt)
	if err != nil || priorGen == "" {
		t.Fatalf("prior generation: gen=%q err=%v", priorGen, err)
	}
	priorQ := quarantineDirForGeneration(wt, priorGen)
	if payloads := quarantinePayloadBytes(t, priorQ); !payloads[priorPayload] {
		t.Fatalf("prior quarantine missing: %#v", payloads)
	}

	// Abandon without CleanupWorktree (path recreated before retention expires).
	if err := os.RemoveAll(wt); err != nil {
		t.Fatalf("RemoveAll worktree: %v", err)
	}
	runGit(t, fixture.repoPath, "worktree", "prune")

	second, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: branch, BaseBranch: "main", PRNumber: 1081,
	})
	if err != nil {
		t.Fatalf("CreateWorktree(second) error = %v", err)
	}
	if second.WorktreePath != wt {
		t.Fatalf("recreated path = %q, want same path %q", second.WorktreePath, wt)
	}
	activePayload := `{"body":"active-lifecycle"}` + "\n"
	writeFile(t, filepath.Join(wt, ".looper-review-1081b.json"), activePayload)
	if _, err := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: wt, Branch: branch}); err != nil {
		t.Fatalf("PrepareWorktree(second) error = %v", err)
	}
	activeGen, err := readReservedReviewScratchGeneration(wt)
	if err != nil || activeGen == "" {
		t.Fatalf("active generation: gen=%q err=%v", activeGen, err)
	}
	if activeGen == priorGen {
		t.Fatalf("recreated worktree reused generation %q; want a new lifecycle id", activeGen)
	}
	activeQ := quarantineDirForGeneration(wt, activeGen)
	if payloads := quarantinePayloadBytes(t, activeQ); !payloads[activePayload] {
		t.Fatalf("active quarantine missing: %#v", payloads)
	}
	if payloads := quarantinePayloadBytes(t, priorQ); !payloads[priorPayload] {
		t.Fatalf("prior quarantine lost after recreate prepare: %#v", payloads)
	}

	if err := gateway.CleanupWorktree(ctx, CleanupWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		WorktreePath: wt, Branch: branch,
	}); err != nil {
		t.Fatalf("CleanupWorktree(second) error = %v", err)
	}
	if _, err := os.Stat(activeQ); !os.IsNotExist(err) {
		t.Fatalf("active generation quarantine still present after cleanup: err=%v", err)
	}
	if payloads := quarantinePayloadBytes(t, priorQ); !payloads[priorPayload] {
		t.Fatalf("prior generation quarantine deleted early by recreated cleanup: %#v", payloads)
	}

	// Fresh orphan prior generation must survive opportunistic prune (not expired).
	other, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: branch + "-other", BaseBranch: "main", PRNumber: 1082,
	})
	if err != nil {
		t.Fatalf("CreateWorktree(other) error = %v", err)
	}
	if err := gateway.relocateReservedReviewerScratch(ctx, other.WorktreePath); err != nil {
		t.Fatalf("relocate(other) error = %v", err)
	}
	if payloads := quarantinePayloadBytes(t, priorQ); !payloads[priorPayload] {
		t.Fatalf("prior generation pruned before retention: %#v", payloads)
	}
}
