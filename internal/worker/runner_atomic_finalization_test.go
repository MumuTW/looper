package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/MumuTW/looper/internal/storage"
)

func TestProcessClaimedQueueItemParksAtomicFinalizationFailureThenConvergesWithoutAgentReplay(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	claim, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "worker-atomic", "worker")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want running claim", claim, err)
	}
	loop, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", loop, err)
	}
	loop.Status = "running"
	loop.NextRunAt = nil
	loop.UpdatedAt = fixture.nowISO()
	if err := fixture.repos.Loops.Upsert(ctx, *loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	lastStep := string(stepOpenPR)
	checkpointJSON := mustMarshalJSON(workerCheckpoint{PullRequest: &checkpointPullPR{Number: 42, URL: "https://example.test/acme/looper/pull/42"}})
	run := storage.RunRecord{ID: "run_atomic_finalization", LoopID: loop.ID, Status: "running", LastCompletedStep: &lastStep, CheckpointJSON: &checkpointJSON, StartedAt: fixture.nowISO(), LastHeartbeatAt: stringPtr(fixture.nowISO()), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if _, err := fixture.coordinator.DB().ExecContext(ctx, `
		CREATE TRIGGER fail_worker_atomic_queue_completion
		BEFORE UPDATE OF status ON queue_items
		WHEN OLD.id = 'queue_worker_1' AND NEW.status = 'completed'
		BEGIN
			SELECT RAISE(ABORT, 'injected worker finalization seam failure');
		END
	`); err != nil {
		t.Fatalf("create fault trigger: %v", err)
	}

	agent := &fakeAgentExecutor{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})
	result, err := runner.ProcessClaimedQueueItem(ctx, *claim)
	if result != nil || !errors.Is(err, ErrSuccessfulClaimFinalization) {
		t.Fatalf("ProcessClaimedQueueItem(fault) = (%#v, %v), want parked finalization error", result, err)
	}
	assertWorkerAtomicFinalizationState(t, fixture, "running", "running", "running")
	if len(agent.starts) != 0 {
		t.Fatalf("agent starts after finalization fault = %d, want 0", len(agent.starts))
	}

	if _, err := fixture.coordinator.DB().ExecContext(ctx, `DROP TRIGGER fail_worker_atomic_queue_completion`); err != nil {
		t.Fatalf("drop fault trigger: %v", err)
	}
	result, err = runner.ProcessClaimedQueueItem(ctx, *claim)
	if err != nil || result == nil || result.Status != "success" || result.PullRequestNumber != 42 {
		t.Fatalf("ProcessClaimedQueueItem(recovery) = (%#v, %v), want recovered success", result, err)
	}
	assertWorkerAtomicFinalizationState(t, fixture, "success", "completed", "completed")
	if len(agent.starts) != 0 {
		t.Fatalf("agent starts after live finalization recovery = %d, want 0", len(agent.starts))
	}
}

func TestSuccessfulClaimRecoveryRequiresFinalStepEvidence(t *testing.T) {
	if IsSuccessfulClaimFinalizationCandidate(storage.RunRecord{Status: "success", LastCompletedStep: stringPtr(string(stepExecute))}) {
		t.Fatal("success before open-pr must not consume an active queue claim")
	}
	if !IsSuccessfulClaimFinalizationCandidate(storage.RunRecord{Status: "success", LastCompletedStep: stringPtr(string(stepOpenPR))}) {
		t.Fatal("success after open-pr should be recoverable")
	}
}

func assertWorkerAtomicFinalizationState(t *testing.T, fixture *runnerFixture, runStatus, queueStatus, loopStatus string) {
	t.Helper()
	ctx := context.Background()
	run, _ := fixture.repos.Runs.GetByID(ctx, "run_atomic_finalization")
	queueItem, _ := fixture.repos.Queue.GetByID(ctx, "queue_worker_1")
	loop, _ := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if run == nil || run.Status != runStatus {
		t.Fatalf("run = %#v, want status %q", run, runStatus)
	}
	if queueItem == nil || queueItem.Status != queueStatus {
		t.Fatalf("queue = %#v, want status %q", queueItem, queueStatus)
	}
	if loop == nil || loop.Status != loopStatus {
		t.Fatalf("loop = %#v, want status %q", loop, loopStatus)
	}
}
