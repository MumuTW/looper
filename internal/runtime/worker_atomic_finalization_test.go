package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

func TestEnsureClaimFinalizedRetainsRunningWorkerClaimAfterAtomicSuccessFailure(t *testing.T) {
	_, repos := newWorkerFinalizationRuntimeFixture(t, "running", "running", nil)
	ctx := context.Background()
	queueItem, err := repos.Queue.GetByID(ctx, "queue_worker_finalization")
	if err != nil || queueItem == nil {
		t.Fatalf("Queue.GetByID() = (%#v, %v)", queueItem, err)
	}
	err = ensureClaimFinalized(ctx, *queueItem, worker.ErrSuccessfulClaimFinalization, defaultSchedulerTickInput{Repos: repos}, time.Now)
	if !errors.Is(err, ErrOperationFinalizeFailed) || !errors.Is(err, worker.ErrSuccessfulClaimFinalization) {
		t.Fatalf("ensureClaimFinalized() error = %v, want retained worker finalization failure", err)
	}
	queueItem, _ = repos.Queue.GetByID(ctx, queueItem.ID)
	if queueItem == nil || queueItem.Status != "running" {
		t.Fatalf("queue after scheduler finalize = %#v, want running ownership retained", queueItem)
	}
}

func TestStartupRecoveryFinalizesLegacyWorkerSuccessWithoutRequeue(t *testing.T) {
	lastError := "complete queue item: injected failure"
	coordinator, repos := newWorkerFinalizationRuntimeFixture(t, "success", "queued", &lastError)
	rt := &Runtime{services: Services{Coordinator: coordinator}}
	ctx := context.Background()
	loop, err := repos.Loops.GetByID(ctx, "loop_worker_finalization")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", loop, err)
	}

	if _, err := rt.repairInterruptedLoopQueueIfNeeded(ctx, repos, *loop, "2026-07-30T12:02:00.000Z"); err != nil {
		t.Fatalf("repairInterruptedLoopQueueIfNeeded() error = %v", err)
	}
	if _, err := rt.repairInterruptedLoopQueueIfNeeded(ctx, repos, *loop, "2026-07-30T12:03:00.000Z"); err != nil {
		t.Fatalf("repairInterruptedLoopQueueIfNeeded(replay) error = %v", err)
	}
	assertRuntimeWorkerFinalizationState(t, repos, "success", "completed", "completed")
}

func newWorkerFinalizationRuntimeFixture(t *testing.T, runStatus, queueStatus string, queueError *string) (*storage.SQLiteCoordinator, *storage.Repositories) {
	t.Helper()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "worker-finalization.sqlite"), storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	ctx := context.Background()
	startedAt := "2026-07-30T12:00:00.000Z"
	endedAt := "2026-07-30T12:01:00.000Z"
	projectID := "project_worker_finalization"
	loopID := "loop_worker_finalization"
	queueID := "queue_worker_finalization"
	loopStatus := "running"
	if queueStatus == "queued" {
		loopStatus = "queued"
	}
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Worker finalization", RepoPath: t.TempDir(), CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: loopStatus, CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	lastCompletedStep := "open-pr"
	run := storage.RunRecord{ID: "run_worker_finalization", LoopID: loopID, Status: runStatus, LastCompletedStep: &lastCompletedStep, StartedAt: startedAt, CreatedAt: startedAt, UpdatedAt: endedAt}
	if runStatus == "success" {
		run.EndedAt = &endedAt
	}
	if err := repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID, DedupeKey: "worker:" + loopID, Priority: storage.QueuePriorityWorker, Status: queueStatus, AvailableAt: startedAt, Attempts: 1, MaxAttempts: 3, LastError: queueError, CreatedAt: startedAt, UpdatedAt: endedAt}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	return coordinator, repos
}

func assertRuntimeWorkerFinalizationState(t *testing.T, repos *storage.Repositories, runStatus, queueStatus, loopStatus string) {
	t.Helper()
	ctx := context.Background()
	run, _ := repos.Runs.GetByID(ctx, "run_worker_finalization")
	queueItem, _ := repos.Queue.GetByID(ctx, "queue_worker_finalization")
	loop, _ := repos.Loops.GetByID(ctx, "loop_worker_finalization")
	if run == nil || run.Status != runStatus {
		t.Fatalf("run = %#v, want %q", run, runStatus)
	}
	if queueItem == nil || queueItem.Status != queueStatus {
		t.Fatalf("queue = %#v, want %q", queueItem, queueStatus)
	}
	if loop == nil || loop.Status != loopStatus {
		t.Fatalf("loop = %#v, want %q", loop, loopStatus)
	}
}
