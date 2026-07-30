package main

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/loops"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
	"github.com/nexu-io/looper/internal/storage"
)

// Takeover commits the hold before it stops the run. These tests probe the
// instant *during* the stop, which is the window the old order left open: the
// loop had been paused and its queue item cancelled — releasing the target's
// shared queue lock — but the human_takeover status had not been written yet.

const takeoverNowISO = "2026-04-21T12:00:00.000Z"

type takeoverFixture struct {
	repos    *storage.Repositories
	services looperdruntime.Services
	now      time.Time
	loopID   string
}

// newTakeoverFixture seeds a fixer loop mid-run on acme/looper#41 plus a
// sibling reviewer loop with queued work on the same PR — the shared detached
// checkout both roles would enter.
func newTakeoverFixture(t *testing.T) *takeoverFixture {
	t.Helper()
	ctx := context.Background()
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(ctx); err != nil {
		t.Fatalf("MigrationRunner().RunPending() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })

	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 12, 0, 0, 0, time.UTC)
	project := storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), CreatedAt: takeoverNowISO, UpdatedAt: takeoverNowISO}
	if err := repos.Projects.Upsert(ctx, project); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	repo := "acme/looper"
	prNumber := int64(41)
	targetID := "pr:acme/looper:41"
	held := storage.LoopRecord{ID: "loop_fixer", Seq: 41, ProjectID: project.ID, Type: "fixer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "running", CreatedAt: takeoverNowISO, UpdatedAt: takeoverNowISO}
	if err := repos.Loops.Upsert(ctx, held); err != nil {
		t.Fatalf("Loops.Upsert(fixer) error = %v", err)
	}
	sibling := storage.LoopRecord{ID: "loop_reviewer", Seq: 42, ProjectID: project.ID, Type: "reviewer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: takeoverNowISO, UpdatedAt: takeoverNowISO}
	if err := repos.Loops.Upsert(ctx, sibling); err != nil {
		t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
	}
	siblingLoopID := sibling.ID
	dedupe := "reviewer:acme/looper:41"
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{
		ID: "queue_reviewer", ProjectID: &project.ID, LoopID: &siblingLoopID, Type: "reviewer",
		TargetType: "pull_request", TargetID: targetID, Repo: &repo, PRNumber: &prNumber,
		DedupeKey: dedupe, Status: "queued", Priority: 5, AvailableAt: takeoverNowISO,
		MaxAttempts: 5, CreatedAt: takeoverNowISO, UpdatedAt: takeoverNowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert(reviewer) error = %v", err)
	}

	run := storage.RunRecord{ID: "run_1", LoopID: held.ID, Status: "running", StartedAt: takeoverNowISO, LastHeartbeatAt: stringRef(takeoverNowISO), CreatedAt: takeoverNowISO, UpdatedAt: takeoverNowISO}
	if err := repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	pid := int64(9876)
	cwd := "/tmp/worktrees/looper-fix-project_1-pr-41-detached"
	session := "session_human"
	if err := repos.AgentExecutions.Upsert(ctx, storage.AgentExecutionRecord{
		ID: "agentexec_1", ProjectID: &project.ID, LoopID: &held.ID, RunID: &run.ID,
		Vendor: "codex", Status: "running", PID: &pid, CWD: &cwd, NativeSessionID: &session,
		StartedAt: takeoverNowISO, CreatedAt: takeoverNowISO, UpdatedAt: takeoverNowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	// ActiveExecutions is deliberately nil so haltLoop takes the PID path, where
	// executionMatchesProcess gives the test a hook that runs *inside* the stop.
	return &takeoverFixture{
		repos: repos,
		services: looperdruntime.Services{
			Coordinator:  coordinator,
			Repositories: repos,
			Loops:        &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }},
		},
		now:    now,
		loopID: held.ID,
	}
}

func stringRef(value string) *string { return &value }

// TestTakeoverFencesSiblingBeforeStoppingTheRun: while takeover is stopping the
// in-flight run, no lane may claim a sibling loop on the same PR. The old order
// paused the loop and cancelled its queue item before writing the hold, so for
// the whole of the process stop the sibling's queued item was claimable — and a
// claim already granted cannot be revoked by any later predicate.
func TestTakeoverFencesSiblingBeforeStoppingTheRun(t *testing.T) {
	ctx := context.Background()
	f := newTakeoverFixture(t)

	var claimedDuringStop *storage.QueueItemRecord
	var claimErr error
	probe := func(context.Context, storage.AgentExecutionRecord, int) (bool, bool, error) {
		claimedDuringStop, claimErr = f.repos.Queue.ClaimNext(ctx, takeoverNowISO, "scheduler")
		return true, true, nil
	}

	if _, err := takeoverLoop(ctx, f.services, f.loopID, "Taken over by test", func() time.Time { return f.now }, func(int, syscall.Signal) error { return nil }, probe); err != nil {
		t.Fatalf("takeoverLoop() error = %v", err)
	}
	if claimErr != nil {
		t.Fatalf("ClaimNext() during stop error = %v", claimErr)
	}
	if claimedDuringStop != nil {
		t.Fatalf("sibling %s was claimable during the takeover stop window; the hold must commit before the stop", claimedDuringStop.ID)
	}

	loop, err := f.repos.Loops.GetByID(ctx, f.loopID)
	if err != nil || loop == nil || loop.Status != "human_takeover" {
		t.Fatalf("loop = %#v (err %v), want human_takeover committed", loop, err)
	}
}

// TestTakeoverSurvivesDiscoveryRequeueDuringStop is #177's window: a discovery
// tick that re-queues the loop mid-stop used to make the final
// paused → human_takeover transition invalid, so `looper takeover` failed
// exactly when an operator was most likely to reach for it. With the hold
// committed first, the tick's write is the one that loses.
func TestTakeoverSurvivesDiscoveryRequeueDuringStop(t *testing.T) {
	ctx := context.Background()
	f := newTakeoverFixture(t)

	var requeueErr error
	probe := func(context.Context, storage.AgentExecutionRecord, int) (bool, bool, error) {
		// A discovery tick holding a pre-takeover snapshot writes it back.
		stale, err := f.repos.Loops.GetByID(ctx, f.loopID)
		if err != nil {
			return false, false, err
		}
		requeued := *stale
		requeued.Status = "queued"
		requeued.NextRunAt = stringRef(takeoverNowISO)
		requeued.UpdatedAt = "2026-04-21T12:00:01.000Z"
		requeueErr = f.repos.Loops.Upsert(ctx, requeued)
		return true, true, nil
	}

	if _, err := takeoverLoop(ctx, f.services, f.loopID, "Taken over by test", func() time.Time { return f.now }, func(int, syscall.Signal) error { return nil }, probe); err != nil {
		t.Fatalf("takeoverLoop() error = %v, want takeover to commit despite a concurrent requeue", err)
	}
	if !errors.Is(requeueErr, storage.ErrLoopHumanHeld) {
		t.Fatalf("mid-stop requeue error = %v, want ErrLoopHumanHeld", requeueErr)
	}
	loop, err := f.repos.Loops.GetByID(ctx, f.loopID)
	if err != nil || loop == nil || loop.Status != "human_takeover" {
		t.Fatalf("loop = %#v (err %v), want the hold to survive the requeue", loop, err)
	}
}

// TestTakeoverCancelsQueueItemWithTheHold: the hold and the queue-item cancel
// are one commit, so there is no instant where the lock is released but the
// loop is not yet held.
func TestTakeoverCancelsQueueItemWithTheHold(t *testing.T) {
	ctx := context.Background()
	f := newTakeoverFixture(t)

	loopID := f.loopID
	projectID := "project_1"
	repo := "acme/looper"
	prNumber := int64(41)
	if err := f.repos.Queue.Upsert(ctx, storage.QueueItemRecord{
		ID: "queue_fixer", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", TargetID: "pr:acme/looper:41", Repo: &repo, PRNumber: &prNumber,
		DedupeKey: "fixer:acme/looper:41", Status: "queued", Priority: 5, AvailableAt: takeoverNowISO,
		MaxAttempts: 5, CreatedAt: takeoverNowISO, UpdatedAt: takeoverNowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert(fixer) error = %v", err)
	}

	if _, err := takeoverLoop(ctx, f.services, loopID, "Taken over by test", func() time.Time { return f.now }, func(int, syscall.Signal) error { return nil }, func(context.Context, storage.AgentExecutionRecord, int) (bool, bool, error) {
		return true, true, nil
	}); err != nil {
		t.Fatalf("takeoverLoop() error = %v", err)
	}

	item, err := f.repos.Queue.GetByID(ctx, "queue_fixer")
	if err != nil || item == nil {
		t.Fatalf("Queue.GetByID() = %#v, %v", item, err)
	}
	if item.Status == "queued" || item.Status == "running" {
		t.Fatalf("queue item status = %q, want cancelled by the hold", item.Status)
	}
}
