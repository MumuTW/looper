package fixer

import (
	"context"
	"errors"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

func TestFailureStreakPauseEventRequiresDurablePausedLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	projectID := "project_1"
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{
		ID:         "loop_failure_streak_pause_event",
		Seq:        209,
		ProjectID:  projectID,
		Type:       "fixer",
		TargetType: "pull_request",
		Repo:       &repo,
		PRNumber:   &prNumber,
		Status:     "running",
		CreatedAt:  fixture.nowISO(),
		UpdatedAt:  fixture.nowISO(),
	}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue := storage.QueueItemRecord{
		ID:          "queue_failure_streak_pause_event",
		ProjectID:   &projectID,
		LoopID:      &loop.ID,
		Type:        "fixer",
		TargetType:  "pull_request",
		Repo:        &repo,
		PRNumber:    &prNumber,
		Priority:    storage.QueuePriorityFixer,
		Status:      "running",
		AvailableAt: fixture.nowISO(),
		MaxAttempts: -1,
		CreatedAt:   fixture.nowISO(),
		UpdatedAt:   fixture.nowISO(),
	}
	if err := fixture.repos.Queue.Upsert(ctx, queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	run := storage.RunRecord{
		ID:        "run_failure_streak_pause_event",
		LoopID:    loop.ID,
		Status:    "failed",
		StartedAt: fixture.nowISO(),
		EndedAt:   stringPtr(fixture.nowISO()),
		CreatedAt: fixture.nowISO(),
		UpdatedAt: fixture.nowISO(),
	}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if _, err := fixture.coordinator.DB().ExecContext(ctx, `
		CREATE TRIGGER loops_fail_failure_streak_pause
		BEFORE UPDATE ON loops
		FOR EACH ROW
		WHEN NEW.id = 'loop_failure_streak_pause_event' AND NEW.status = 'paused'
		BEGIN
			SELECT RAISE(FAIL, 'forced loop pause failure');
		END;
	`); err != nil {
		t.Fatalf("create trigger error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, MaxConsecutiveFixerFailures: 1, Logger: fixture.logger, Now: fixture.now})
	runFailure := &claimedRunFailureError{
		cause: errors.New("agent failed"),
		runID: run.ID,
		checkpoint: fixerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: "head-1"},
			FixItems: []FixItem{{Type: "comment", ID: "comment-1", ThreadID: "thread-1"}},
		},
		step: stepRepair,
	}

	if _, err := runner.recoverClaimedItem(ctx, queue, runFailure); err == nil {
		t.Fatal("recoverClaimedItem() error = nil, want forced loop pause failure")
	}
	persistedQueue, err := fixture.repos.Queue.GetByID(ctx, queue.ID)
	if err != nil || persistedQueue == nil || persistedQueue.Status != "manual_intervention" {
		t.Fatalf("Queue.GetByID() = (%#v, %v), want terminal manual_intervention", persistedQueue, err)
	}
	persistedLoop, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || persistedLoop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", persistedLoop, err)
	}
	if persistedLoop.Status == "paused" || parseJSONObject(persistedLoop.MetadataJSON)["pauseReason"] != failureStreakPauseReason {
		t.Fatalf("loop = %#v, want durable pause metadata without paused status", persistedLoop)
	}
	events, err := fixture.repos.Events.ListByEntity(ctx, "loop", loop.ID)
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	for _, event := range events {
		if event.EventType == "loop.paused" {
			t.Fatalf("events = %#v, want no loop.paused before the loop pause is durable", events)
		}
	}
}
