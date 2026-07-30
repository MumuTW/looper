package fixer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// The lifecycle this covers is the one that re-created #162's damage out of
// #177's fix, so it is driven end to end rather than by hand: the real
// loops.Service.Hold that `looper takeover` calls, then the real recovery the
// interrupted fixer run performs. Takeover cancels the running queue item; the
// fixer reads that cancelled row back as its own terminal result;
// queueResultIsTerminalForCleanup calls cancelled terminal; and terminal cleanup
// force-removes the checkout — the one the operator was just told is theirs.
func TestTakeoverDuringFixerRunKeepsWorktreeThroughTerminalCleanup(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(41)
	projectID := "project_1"
	loopID := "loop_taken_over_mid_run"
	target := buildPullRequestTargetID(repo, prNumber)

	if err := fixture.repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: loopID, Seq: 41, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &prNumber,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue := storage.QueueItemRecord{
		ID: "queue_taken_over_mid_run", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", TargetID: target, Repo: &repo, PRNumber: &prNumber,
		DedupeKey: "fixer:acme/looper:41", Priority: storage.QueuePriorityFixer, Status: "running",
		AvailableAt: nowISO, StartedAt: stringPtr(nowISO), Attempts: 1, MaxAttempts: 1,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Queue.Upsert(ctx, queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	worktreePath := filepath.Join(t.TempDir(), "looper-fix-project_1-pr-41-detached")
	if err := os.MkdirAll(filepath.Join(worktreePath, "src"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	humanEdit := filepath.Join(worktreePath, "src", "human.txt")
	if err := os.WriteFile(humanEdit, []byte("uncommitted work by the human"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	// `looper takeover`.
	reason := "Taken over by a human via looper takeover"
	service := &loops.Service{DB: fixture.coordinator.DB(), Repos: fixture.repos, Now: fixture.now}
	held, err := service.Hold(ctx, loopID, &reason)
	if err != nil {
		t.Fatalf("loops.Service.Hold() error = %v", err)
	}
	if held.CancelledQueueItems == 0 {
		t.Fatal("Hold() cancelled no queue item; the regression under test starts with that cancellation")
	}

	// The interrupted run now recovers, holding the pre-takeover queue snapshot.
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})
	checkpoint := fixerCheckpoint{Worktree: &checkpointWorktree{Path: worktreePath, Branch: "fix/pr-41", PreparedAt: nowISO}}
	runFailure := &claimedRunFailureError{cause: errors.New("agent process cancelled"), runID: "run-taken-over", checkpoint: checkpoint, step: stepRepair}
	if _, err := runner.recoverClaimedItem(ctx, queue, runFailure); err != nil {
		t.Fatalf("recoverClaimedItem() error = %v", err)
	}

	// The door: recovery really did classify the persisted item as terminal.
	persistedQueue, err := fixture.repos.Queue.GetByID(ctx, queue.ID)
	if err != nil || persistedQueue == nil {
		t.Fatalf("Queue.GetByID() = (%#v, %v), want the cancelled item", persistedQueue, err)
	}
	if !queueResultIsTerminalForCleanup(persistedQueue) {
		t.Fatalf("persisted queue status %q is not terminal for cleanup; this test no longer exercises the cleanup path", persistedQueue.Status)
	}

	if len(git.cleanupCalls) != 0 {
		t.Fatalf("cleanup calls = %#v, want none: the human owns this checkout until handback", git.cleanupCalls)
	}
	if _, err := os.Stat(humanEdit); err != nil {
		t.Fatalf("os.Stat(%s) error = %v, want the human's uncommitted work intact", humanEdit, err)
	}
	persistedLoop, err := fixture.repos.Loops.GetByID(ctx, loopID)
	if err != nil || persistedLoop == nil || persistedLoop.Status != "human_takeover" {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want the loop still held", persistedLoop, err)
	}
}

// The narrowing must not disarm ordinary cleanup: once handback releases the
// hold, the same terminal path removes the checkout again.
func TestTerminalCleanupStillRunsAfterHandback(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()
	loopID := "loop_released"
	target := "pr:acme/looper:42"
	repo := "acme/looper"
	prNumber := int64(42)
	if err := fixture.repos.Loops.Upsert(ctx, storage.LoopRecord{
		ID: loopID, Seq: 42, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &prNumber,
		Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})
	project, err := fixture.repos.Projects.GetByID(ctx, "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v)", project, err)
	}
	checkpoint := &fixerCheckpoint{Worktree: &checkpointWorktree{Path: filepath.Join(t.TempDir(), "wt-42"), Branch: "fix/pr-42", PreparedAt: nowISO}}
	runner.cleanupFixerWorktreeIfTerminal(ctx, *project, loopID, "", checkpoint)
	if len(git.cleanupCalls) != 1 {
		t.Fatalf("cleanup calls = %#v, want one cleanup for an unheld loop", git.cleanupCalls)
	}
}
