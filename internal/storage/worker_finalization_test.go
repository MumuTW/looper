package storage

import (
	"context"
	"strings"
	"testing"
)

func TestFinalizeWorkerSuccessRollsBackRunWhenQueueCompletionFails(t *testing.T) {
	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	ctx := context.Background()
	startedAt := "2026-07-30T12:00:00.000Z"
	finishedAt := "2026-07-30T12:01:00.000Z"
	projectID := "project_atomic_worker"
	loopID := "loop_atomic_worker"
	queueID := "queue_atomic_worker"

	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: "Atomic worker", RepoPath: t.TempDir(), CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(ctx, RunRecord{ID: "run_atomic_worker", LoopID: loopID, Status: "running", StartedAt: startedAt, CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: "project:" + projectID, DedupeKey: "worker:" + loopID, Priority: QueuePriorityWorker, Status: "running", AvailableAt: startedAt, Attempts: 1, MaxAttempts: 3, CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	if _, err := coordinator.DB().ExecContext(ctx, `
		CREATE TRIGGER fail_worker_queue_completion
		BEFORE UPDATE OF status ON queue_items
		WHEN OLD.id = 'queue_atomic_worker' AND NEW.status = 'completed'
		BEGIN
			SELECT RAISE(ABORT, 'injected queue completion failure');
		END
	`); err != nil {
		t.Fatalf("create fault trigger: %v", err)
	}

	run, err := repos.Runs.GetByID(ctx, "run_atomic_worker")
	if err != nil || run == nil {
		t.Fatalf("Runs.GetByID() = %#v, %v", run, err)
	}
	completed := *run
	completed.Status = "success"
	completed.EndedAt = &finishedAt
	completed.UpdatedAt = finishedAt
	err = FinalizeWorkerSuccess(ctx, coordinator.DB(), WorkerSuccessFinalizationInput{Run: completed, QueueItemID: queueID, LoopID: loopID, LoopStatus: "completed", FinishedAt: finishedAt})
	if err == nil || !strings.Contains(err.Error(), "injected queue completion failure") {
		t.Fatalf("FinalizeWorkerSuccess() error = %v, want injected seam failure", err)
	}

	run, _ = repos.Runs.GetByID(ctx, completed.ID)
	queueItem, _ := repos.Queue.GetByID(ctx, queueID)
	loop, _ := repos.Loops.GetByID(ctx, loopID)
	if run == nil || run.Status != "running" || run.EndedAt != nil {
		t.Fatalf("run after rollback = %#v, want original running run", run)
	}
	if queueItem == nil || queueItem.Status != "running" {
		t.Fatalf("queue after rollback = %#v, want running", queueItem)
	}
	if loop == nil || loop.Status != "running" {
		t.Fatalf("loop after rollback = %#v, want running", loop)
	}
}
