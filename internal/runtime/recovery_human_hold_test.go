package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

// Startup recovery is the daemon's boot path, and every write it makes —
// requeue, normalize, quarantine-park — is one the takeover hold refuses. Before
// the hold existed those writes could not fail for this reason; now they can,
// and this pass aborts on the first error. So one operator running
// `looper takeover` at the wrong moment could have taken the whole recovery pass
// down with it, and with it daemon startup, for every other loop.
func TestRunRecoveryPipelineRecoversOtherLoopsWhenOneIsHumanHeld(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, "")
	defer coordinator.Close()
	repositories := storage.NewRepositories(coordinator.DB())
	ctx := context.Background()
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	if err := repositories.Projects.Upsert(ctx, storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	seedRun := func(loopID string) {
		t.Helper()
		if err := repositories.Runs.Upsert(ctx, storage.RunRecord{ID: "run_" + loopID, LoopID: loopID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", loopID, err)
		}
	}
	// The held loop is one recovery actively wants to write: a reviewer whose
	// metadata says the PR is gone, which recovery normalizes to terminated. It
	// sorts first, so an abort here takes the loop behind it down too.
	repo := "acme/looper"
	prNumber := int64(41)
	heldTarget := "pr:acme/looper:41"
	heldMetadata := `{"loop":{"status":"terminated","terminationReason":"pr_closed_or_merged"}}`
	if err := repositories.Loops.UpsertChangingHumanHold(ctx, storage.LoopRecord{
		ID: "loop_held", Seq: 1, ProjectID: "project_1", Type: "reviewer",
		TargetType: "pull_request", TargetID: &heldTarget, Repo: &repo, PRNumber: &prNumber,
		Status: "human_takeover", MetadataJSON: &heldMetadata, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("seed loop_held error = %v", err)
	}
	seedRun("loop_held")
	if err := repositories.Loops.Upsert(ctx, storage.LoopRecord{
		ID: "loop_ordinary", Seq: 2, ProjectID: "project_1", Type: "worker",
		TargetType: "project", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("seed loop_ordinary error = %v", err)
	}
	seedRun("loop_ordinary")

	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})
	if _, err := rt.runRecoveryPipeline(ctx, repositories, nil, now); err != nil {
		t.Fatalf("runRecoveryPipeline() error = %v; one held loop must not abort startup recovery", err)
	}

	assertLoopStatus(t, repositories, "loop_held", "human_takeover")
	held, err := repositories.Loops.GetByID(ctx, "loop_held")
	if err != nil || held == nil {
		t.Fatalf("Loops.GetByID(loop_held) = (%#v, %v)", held, err)
	}
	if held.NextRunAt != nil {
		t.Fatalf("loop_held.NextRunAt = %#v, want nil: recovery must not re-arm a loop a human owns", held.NextRunAt)
	}
	// The point of not aborting: the loop beside it still got recovered.
	ordinary, err := repositories.Loops.GetByID(ctx, "loop_ordinary")
	if err != nil || ordinary == nil {
		t.Fatalf("Loops.GetByID(loop_ordinary) = (%#v, %v)", ordinary, err)
	}
	if ordinary.Status == "running" {
		t.Fatal("loop_ordinary.Status = running; recovery did not process the loop beside the held one")
	}
}

// recoveryUpsertLoop is the backstop for the same thing one layer down: the
// pipeline's status read happens before the write, so a takeover that commits in
// between still reaches the guarded write. A refusal there means "not applied",
// not "recovery failed".
func TestRecoveryUpsertLoopReportsAHeldLoopAsNotApplied(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "runtime.sqlite"), "")
	defer coordinator.Close()
	repositories := storage.NewRepositories(coordinator.DB())
	ctx := context.Background()
	nowISO := formatJavaScriptISOString(time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC))
	if err := repositories.Projects.Upsert(ctx, storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loop := storage.LoopRecord{ID: "loop_raced", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "project", Status: "human_takeover", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repositories.Loops.UpsertChangingHumanHold(ctx, loop); err != nil {
		t.Fatalf("Loops.UpsertChangingHumanHold() error = %v", err)
	}

	// The record recovery loaded before takeover committed.
	stale := loop
	stale.Status = "queued"
	stale.NextRunAt = &nowISO
	applied, err := recoveryUpsertLoop(ctx, repositories, stale)
	if err != nil {
		t.Fatalf("recoveryUpsertLoop() error = %v, want a refusal reported as not applied", err)
	}
	if applied {
		t.Fatal("recoveryUpsertLoop() applied = true, want false: the write was refused by the hold guard")
	}
	persisted, err := repositories.Loops.GetByID(ctx, "loop_raced")
	if err != nil || persisted == nil || persisted.Status != "human_takeover" {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want the loop still held", persisted, err)
	}
}

// Stale-run reconciliation's requeue is not one write, it is a loop write
// followed by a multi-statement queue repair. recoveryUpsertLoop only guards the
// first: a takeover that commits *after* the loop write cancels the loop's queue
// item, and the repair then publishes a replacement one for a loop the human now
// owns. The claim predicate keeps that item dormant, so nothing runs — but it is
// durable state contradicting the hold, and it is the exact blocker
// assertLoopRetryPreconditions rejects on, so leaving it there puts a landmine
// under the operator's release path. Unlike the cleanup races deferred to #210,
// both halves here are durable writes on the same rows, so the window closes
// rather than narrows.
func TestStaleRunRepairAbandonsRequeueWhenTakeoverCommitsMidRepair(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, "")
	defer coordinator.Close()
	repositories := storage.NewRepositories(coordinator.DB())
	ctx := context.Background()
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	oldISO := formatJavaScriptISOString(now.Add(-time.Hour))

	projectID := "project_1"
	loopID := "loop_reconciled"
	targetID := projectID
	if err := repositories.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loop := storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}
	if err := repositories.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	latestRun := &storage.RunRecord{ID: "run_reconciled", LoopID: loopID, Status: "interrupted", StartedAt: oldISO, EndedAt: &oldISO, CreatedAt: oldISO, UpdatedAt: oldISO}
	if err := repositories.Runs.Upsert(ctx, *latestRun); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if err := repositories.Queue.Upsert(ctx, storage.QueueItemRecord{
		ID: "queue_reconciled", ProjectID: &projectID, LoopID: &loopID, Type: "worker",
		TargetType: "project", TargetID: targetID, DedupeKey: "worker:project_1:loop_reconciled",
		Priority: storage.QueuePriorityWorker, Status: "queued", AvailableAt: oldISO,
		MaxAttempts: 3, CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})
	// The takeover transaction, committed in the window the fix closes: the hold
	// lands and the loop's queue item is cancelled, exactly as looperd's
	// Loops.Hold does.
	takeoverOnce := false
	rt.staleRepairAfterLoopWriteHook = func(id string) {
		if takeoverOnce || id != loopID {
			return
		}
		takeoverOnce = true
		held, err := repositories.Loops.GetByID(ctx, loopID)
		if err != nil || held == nil {
			t.Errorf("Loops.GetByID(%s) = (%#v, %v)", loopID, held, err)
			return
		}
		held.Status = "human_takeover"
		held.NextRunAt = nil
		held.UpdatedAt = nowISO
		if err := repositories.Loops.UpsertChangingHumanHold(ctx, *held); err != nil {
			t.Errorf("UpsertChangingHumanHold() error = %v", err)
			return
		}
		reason := "Taken over by a human via looper takeover"
		if _, err := repositories.Queue.CancelByLoop(ctx, loopID, nowISO, &reason); err != nil {
			t.Errorf("Queue.CancelByLoop() error = %v", err)
		}
	}

	summary, err := rt.repairStaleRunQueueState(ctx, repositories, loop, latestRun, false, nowISO)
	if err != nil {
		t.Fatalf("repairStaleRunQueueState() error = %v", err)
	}
	if !takeoverOnce {
		t.Fatal("the takeover hook never fired; the test did not reach the requeue path it claims to cover")
	}
	if summary.LoopsRequeued != 0 || summary.QueueItemsRequeued != 0 {
		t.Fatalf("summary = %#v, want no requeue reported: the human owns the loop", summary)
	}

	persisted, err := repositories.Loops.GetByID(ctx, loopID)
	if err != nil || persisted == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", persisted, err)
	}
	if persisted.Status != "human_takeover" {
		t.Fatalf("loop status = %q, want human_takeover preserved", persisted.Status)
	}
	// The assertion that matters for the operator: nothing active is left behind.
	// An active queue item here is precisely what assertLoopRetryPreconditions
	// refuses, so this is the state /handback would have had to clean up.
	active, err := repositories.Queue.FindActiveByLoopID(ctx, loopID)
	if err != nil {
		t.Fatalf("Queue.FindActiveByLoopID() error = %v", err)
	}
	if active != nil {
		t.Fatalf("Queue.FindActiveByLoopID() = %#v, want nothing active against a held loop", active)
	}
}
