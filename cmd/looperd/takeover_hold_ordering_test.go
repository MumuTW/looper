package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/loops"
	looperdruntime "github.com/MumuTW/looper/internal/runtime"
	"github.com/MumuTW/looper/internal/storage"
)

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
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{
		ID: "queue_reviewer", ProjectID: &project.ID, LoopID: &siblingLoopID, Type: "reviewer",
		TargetType: "pull_request", TargetID: targetID, Repo: &repo, PRNumber: &prNumber,
		DedupeKey: "reviewer:acme/looper:41", Status: "queued", Priority: 5, AvailableAt: takeoverNowISO,
		MaxAttempts: 5, CreatedAt: takeoverNowISO, UpdatedAt: takeoverNowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert(reviewer) error = %v", err)
	}

	snapshot := `{"vendor":"codex","reasoningEffort":"high"}`
	run := storage.RunRecord{ID: "run_1", LoopID: held.ID, Status: "running", AgentSnapshotJSON: &snapshot, StartedAt: takeoverNowISO, LastHeartbeatAt: stringRef(takeoverNowISO), CreatedAt: takeoverNowISO, UpdatedAt: takeoverNowISO}
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

	return &takeoverFixture{
		repos: repos,
		services: looperdruntime.Services{
			Coordinator: coordinator, Repositories: repos,
			Loops: &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }},
		},
		now: now, loopID: held.ID,
	}
}

func TestTakeoverLoopReturnsPersistedReasoningEffort(t *testing.T) {
	ctx := context.Background()
	f := newTakeoverFixture(t)
	result, err := takeoverLoop(ctx, f.services, f.loopID, "Taken over by test", func() time.Time { return f.now }, func(int, syscall.Signal) error { return nil }, nil)
	if err != nil {
		t.Fatalf("takeoverLoop() error = %v", err)
	}
	if result.ReasoningEffort == nil || *result.ReasoningEffort != config.ReasoningEffortHigh {
		t.Fatalf("ReasoningEffort = %v, want high from persisted run snapshot", result.ReasoningEffort)
	}
}

func TestTakeoverLoopSurvivesMalformedReasoningSnapshot(t *testing.T) {
	ctx := context.Background()
	f := newTakeoverFixture(t)
	malformed := `{"vendor":"codex","reasoningEffort":`
	// Run snapshots are insert-only in production; mutate the fixture row
	// directly to exercise recovery from a legacy/corrupt persisted payload.
	if _, err := f.services.Coordinator.DB().ExecContext(ctx, "UPDATE runs SET agent_snapshot_json = ? WHERE id = ?", malformed, "run_1"); err != nil {
		t.Fatalf("malformed snapshot fixture update error = %v", err)
	}

	result, err := takeoverLoop(ctx, f.services, f.loopID, "Taken over by test", func() time.Time { return f.now }, func(int, syscall.Signal) error { return nil }, nil)
	if err != nil {
		t.Fatalf("takeoverLoop() error = %v, want malformed optional snapshot to degrade gracefully", err)
	}
	if result.ReasoningEffort != nil {
		t.Fatalf("ReasoningEffort = %v, want omitted for malformed snapshot", result.ReasoningEffort)
	}
}

func TestTakeoverLoopReleasesLocksWhenRunLookupFails(t *testing.T) {
	ctx := context.Background()
	f := newTakeoverFixture(t)
	// Keep the loop and execution rows readable, but make the correlated run
	// lookup fail after takeover has acquired both guards.
	if _, err := f.services.Coordinator.DB().ExecContext(ctx, "ALTER TABLE runs RENAME TO runs_missing"); err != nil {
		t.Fatalf("rename runs table error = %v", err)
	}

	_, err := takeoverLoop(ctx, f.services, f.loopID, "Taken over by test", func() time.Time { return f.now }, func(int, syscall.Signal) error { return nil }, nil)
	if err == nil || !strings.Contains(err.Error(), "load agent run before takeover") {
		t.Fatalf("takeoverLoop() error = %v, want correlated run lookup failure", err)
	}

	assertGuardAvailable := func(name string, acquire func() func()) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			release := acquire()
			release()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s remained held after takeover lookup failure", name)
		}
	}
	assertGuardAvailable("loop requeue lock", func() func() {
		return looperdruntime.LockLoopRequeue(f.loopID)
	})
	assertGuardAvailable("target lock", func() func() {
		return loops.LockLoopTarget(loops.LoopTargetGuardKey("project_1", "fixer", "pull_request", "pull_request:acme/looper:41"))
	})
}

func TestTakeoverLoopCorrelatesReasoningEffortWithSelectedExecutionRun(t *testing.T) {
	ctx := context.Background()
	f := newTakeoverFixture(t)

	// A retry can create a newer run after the agent execution row was
	// recorded. The takeover response must keep the selected execution's
	// snapshot instead of pairing that execution with the newer run.
	newerSnapshot := `{"vendor":"codex","reasoningEffort":"low"}`
	if err := f.repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: "run_newer", LoopID: f.loopID, Status: "failed", AgentSnapshotJSON: &newerSnapshot,
		StartedAt: "2026-04-21T12:00:01.000Z", CreatedAt: "2026-04-21T12:00:01.000Z", UpdatedAt: "2026-04-21T12:00:01.000Z",
	}); err != nil {
		t.Fatalf("Runs.Upsert(newer) error = %v", err)
	}

	result, err := takeoverLoop(ctx, f.services, f.loopID, "Taken over by test", func() time.Time { return f.now }, func(int, syscall.Signal) error { return nil }, nil)
	if err != nil {
		t.Fatalf("takeoverLoop() error = %v", err)
	}
	if result.SessionID != "session_human" {
		t.Fatalf("SessionID = %q, want selected execution session", result.SessionID)
	}
	if result.ReasoningEffort == nil || *result.ReasoningEffort != config.ReasoningEffortHigh {
		t.Fatalf("ReasoningEffort = %v, want high from the selected execution's run", result.ReasoningEffort)
	}
}

func stringRef(value string) *string { return &value }

// TestTakeoverFencesSiblingBeforeStoppingTheRun probes the stop window: a
// sibling queue item on the same PR must not become claimable while takeover
// is stopping the in-flight run.
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

// TestTakeoverSurvivesDiscoveryRequeueDuringStop verifies that a stale
// discovery snapshot cannot overwrite the durable human takeover hold.
func TestTakeoverSurvivesDiscoveryRequeueDuringStop(t *testing.T) {
	ctx := context.Background()
	f := newTakeoverFixture(t)

	var requeueErr error
	probe := func(context.Context, storage.AgentExecutionRecord, int) (bool, bool, error) {
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

// TestTakeoverCancelsQueueItemWithTheHold verifies the held loop's queued work
// is cancelled before process stopping can expose a scheduling window.
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
