package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

// newStaleRepairFixture starts a runtime over its own SQLite file and seeds one
// worker loop, so the stale-run repair can be driven directly.
func newStaleRepairFixture(t *testing.T, loopID, loopStatus, nowISO string) (*Runtime, *storage.Repositories) {
	t.Helper()
	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now, err := time.Parse(time.RFC3339Nano, nowISO)
	if err != nil {
		t.Fatalf("Parse(nowISO) error = %v", err)
	}
	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	repos := rt.Services().Repositories
	ctx := context.Background()
	targetID := "project_stale_repair"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: targetID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.UpsertChangingHumanHold(ctx, storage.LoopRecord{ID: loopID, Seq: 4201, ProjectID: targetID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: loopStatus, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.UpsertChangingHumanHold() error = %v", err)
	}
	return rt, repos
}

func seedRunningQueueItem(t *testing.T, repos *storage.Repositories, id, loopID, dedupeKey, updatedAt string, errorKind *string) {
	t.Helper()
	projectID := "project_stale_repair"
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: id, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: dedupeKey, Priority: storage.QueuePriorityWorker, Status: "running", AvailableAt: updatedAt,
		StartedAt: &updatedAt, MaxAttempts: 3, LastErrorKind: errorKind, CreatedAt: updatedAt, UpdatedAt: updatedAt,
	}); err != nil {
		t.Fatalf("Queue.Upsert(%s) error = %v", id, err)
	}
}

// TestRepairStaleRunQueueStateRefusesRequeueUnderHumanTakeover drives the repair
// with the snapshot reconciliation captured before a takeover committed. The
// hold guard inside the loop write must refuse, and because the queue repair
// shares that transaction nothing may be published against the held loop.
func TestRepairStaleRunQueueStateRefusesRequeueUnderHumanTakeover(t *testing.T) {
	t.Parallel()

	nowISO := "2026-07-31T09:00:00.000Z"
	oldISO := "2026-07-31T07:00:00.000Z"
	loopID := "loop_stale_repair_held"
	rt, repos := newStaleRepairFixture(t, loopID, "human_takeover", nowISO)
	ctx := context.Background()
	seedRunningQueueItem(t, repos, "queue_stale_repair_held", loopID, "worker:"+loopID, oldISO, nil)

	// The pre-takeover snapshot reconciliation is still holding.
	staleLoop := storage.LoopRecord{ID: loopID, Seq: 4201, ProjectID: "project_stale_repair", Type: "worker", TargetType: "project", TargetID: stringPtr("project_stale_repair"), Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}
	latestRun := &storage.RunRecord{ID: "run_stale_repair_held", LoopID: loopID, Status: "interrupted", StartedAt: oldISO, EndedAt: &oldISO, CreatedAt: oldISO, UpdatedAt: oldISO}

	summary, err := rt.repairStaleRunQueueState(ctx, repos, staleLoop, latestRun, false, nowISO)
	if err != nil {
		t.Fatalf("repairStaleRunQueueState() error = %v", err)
	}
	if summary != (staleRunQueueRepairSummary{}) {
		t.Fatalf("summary = %#v, want nothing applied under a human hold", summary)
	}
	heldLoop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || heldLoop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", heldLoop, err)
	}
	if heldLoop.Status != "human_takeover" || heldLoop.NextRunAt != nil {
		t.Fatalf("loop = %#v, want the takeover hold intact", heldLoop)
	}
	queueItem, err := repos.Queue.GetByID(ctx, "queue_stale_repair_held")
	if err != nil || queueItem == nil {
		t.Fatalf("Queue.GetByID() = (%#v, %v)", queueItem, err)
	}
	if queueItem.Status != "running" {
		t.Fatalf("queue = %#v, want the queue repair refused with the loop write", queueItem)
	}
}

// TestRepairStaleRunQueueStateReportsActualManualInterventionRequeueCount pins
// the manual-intervention requeue branch to the count the queue write actually
// moved rather than an assumed single item.
func TestRepairStaleRunQueueStateReportsActualManualInterventionRequeueCount(t *testing.T) {
	t.Parallel()

	nowISO := "2026-07-31T09:00:00.000Z"
	oldISO := "2026-07-31T07:00:00.000Z"
	olderISO := "2026-07-31T06:00:00.000Z"
	loopID := "loop_stale_repair_manual"
	rt, repos := newStaleRepairFixture(t, loopID, "running", nowISO)
	ctx := context.Background()
	manualIntervention := "manual_intervention"
	seedRunningQueueItem(t, repos, "queue_stale_repair_manual_old", loopID, "worker:"+loopID+":old", olderISO, nil)
	seedRunningQueueItem(t, repos, "queue_stale_repair_manual", loopID, "worker:"+loopID, oldISO, &manualIntervention)

	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", loop, err)
	}
	latestRun := &storage.RunRecord{ID: "run_stale_repair_manual", LoopID: loopID, Status: "interrupted", StartedAt: oldISO, EndedAt: &oldISO, CreatedAt: oldISO, UpdatedAt: oldISO}

	summary, err := rt.repairStaleRunQueueState(ctx, repos, *loop, latestRun, false, nowISO)
	if err != nil {
		t.Fatalf("repairStaleRunQueueState() error = %v", err)
	}
	if summary.LoopsRequeued != 1 || summary.QueueItemsRequeued != 2 || summary.QueueItemsCancelled != 0 {
		t.Fatalf("summary = %#v, want both running items counted as requeued", summary)
	}
	for _, id := range []string{"queue_stale_repair_manual_old", "queue_stale_repair_manual"} {
		queueItem, err := repos.Queue.GetByID(ctx, id)
		if err != nil || queueItem == nil {
			t.Fatalf("Queue.GetByID(%s) = (%#v, %v)", id, queueItem, err)
		}
		if queueItem.Status != "queued" {
			t.Fatalf("queue %s = %#v, want requeued", id, queueItem)
		}
	}
}

// TestRepairStaleRunQueueStateReusesQueueHistoryBeforeFallback keeps legacy
// loops recoverable: queue history can still supply all target details even
// after old loop rows no longer have enough metadata to build a fresh item.
func TestRepairStaleRunQueueStateReusesQueueHistoryBeforeFallback(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		queueStatus string
	}{
		{name: "active", queueStatus: "queued"},
		{name: "terminal", queueStatus: "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			nowISO := "2026-07-31T09:00:00.000Z"
			oldISO := "2026-07-31T07:00:00.000Z"
			loopID := "loop_stale_repair_legacy_" + test.name
			rt, repos := newStaleRepairFixture(t, loopID, "running", nowISO)
			ctx := context.Background()
			projectID := "project_stale_repair"
			repo := "acme/looper"
			prNumber := int64(42)
			targetID := "pr:acme/looper:42"
			queue := storage.QueueItemRecord{
				ID: "queue_stale_repair_legacy_" + test.name, ProjectID: &projectID, LoopID: &loopID,
				Type: "reviewer", TargetType: "pull_request", TargetID: targetID, Repo: &repo, PRNumber: &prNumber,
				DedupeKey: "reviewer:project_stale_repair:" + loopID + ":acme/looper:42", Priority: storage.QueuePriorityReviewer,
				Status: test.queueStatus, AvailableAt: oldISO, MaxAttempts: 3, CreatedAt: oldISO, UpdatedAt: oldISO,
			}
			if test.queueStatus == "failed" {
				queue.FinishedAt = &oldISO
			}
			if err := repos.Queue.Upsert(ctx, queue); err != nil {
				t.Fatalf("Queue.Upsert() error = %v", err)
			}

			// This is a legacy row: its queue history knows the PR, but the loop
			// itself no longer does. Eager fallback construction rejects it.
			legacyLoop := storage.LoopRecord{
				ID: loopID, Seq: 4201, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request",
				Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO,
			}
			latestRun := &storage.RunRecord{ID: "run_stale_repair_legacy_" + test.name, LoopID: loopID, Status: "interrupted", StartedAt: oldISO, EndedAt: &oldISO, CreatedAt: oldISO, UpdatedAt: oldISO}

			summary, err := rt.repairStaleRunQueueState(ctx, repos, legacyLoop, latestRun, false, nowISO)
			if err != nil {
				t.Fatalf("repairStaleRunQueueState() error = %v", err)
			}
			if summary.LoopsRequeued != 1 {
				t.Fatalf("summary = %#v, want the legacy loop requeued", summary)
			}
			active, err := repos.Queue.FindActiveByLoopID(ctx, loopID)
			if err != nil || active == nil {
				t.Fatalf("Queue.FindActiveByLoopID() = (%#v, %v)", active, err)
			}
			if active.Repo == nil || *active.Repo != repo || active.PRNumber == nil || *active.PRNumber != prNumber || active.TargetID != targetID {
				t.Fatalf("active queue = %#v, want the historical PR target", active)
			}
		})
	}
}
