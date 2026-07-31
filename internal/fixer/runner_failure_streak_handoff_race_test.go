package fixer

import (
	"context"
	"testing"

	"github.com/MumuTW/looper/internal/loops"
	"github.com/MumuTW/looper/internal/storage"
)

// A repeated operator Pause can finish before handoff begins and retain the
// breaker status and metadata. The handoff must still compare against the
// original breaker transition, not use that later Pause as a fresh baseline.
func TestFailureStreakHandoffRejectsOperatorRepeatedPauseBeforeInitialRead(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()
	projectID := "project_1"
	repo := "acme/looper"
	prNumber := int64(97)
	loopID := "loop_failure_streak_handoff_repeated_pause"
	loopTarget := buildPullRequestTargetID(repo, prNumber)
	failedState := "failed-state"
	pendingState := "pending-state"
	metadata := mustMarshalJSON(map[string]any{
		"pauseReason":             failureStreakPauseReason,
		"fixerFailureStreak":      failureStreakState{LastRunID: "run-failed", LastHeadSHA: "head-a", FixItemsStateHash: failedState, Step: string(stepValidate), ConsecutiveCount: maxConsecutiveFixerFailures, RecordedAt: nowISO},
		"pendingFixerRediscovery": pendingFixerRediscoveryState{HeadSHA: "head-b", FixItemsStateHash: pendingState, UnresolvedThreadIDs: []string{"thread-b"}, RecordedAt: nowISO},
	})
	breakerPause := storage.LoopRecord{ID: loopID, Seq: 401, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber, Status: "paused", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(ctx, breakerPause); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	payload := mustMarshalJSON(map[string]any{"fixItemsStateHash": failedState})
	queue := storage.QueueItemRecord{ID: "queue_failure_streak_handoff_repeated_pause", ProjectID: &projectID, LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: loopTarget, Repo: &repo, PRNumber: &prNumber, DedupeKey: buildFixerDedupeKey(projectID, loopID, repo, prNumber, "head-a", failedState), Priority: storage.QueuePriorityFixer, Status: "manual_intervention", AvailableAt: nowISO, Attempts: 3, MaxAttempts: -1, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Queue.Upsert(ctx, queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	service := &loops.Service{DB: fixture.coordinator.DB(), Repos: fixture.repos, Now: fixture.now}
	reason := "operator stop"
	if _, err := service.Pause(ctx, loopID, &reason); err != nil {
		t.Fatalf("first Pause() error = %v", err)
	}
	operatorPause, err := service.Pause(ctx, loopID, &reason)
	if err != nil {
		t.Fatalf("second Pause() error = %v", err)
	}
	if operatorPause.Loop.UpdatedAt <= breakerPause.UpdatedAt {
		t.Fatalf("operator pause revision = %q, want after breaker revision %q", operatorPause.Loop.UpdatedAt, breakerPause.UpdatedAt)
	}

	checkpoint := fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: "head-a"}}
	var wakes int
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now, OnQueueItemEnqueued: func() { wakes++ }})
	resumed, err := runner.finishFailureStreakBreaker(ctx, storage.ProjectRecord{ID: projectID}, breakerPause, queue, "", &checkpoint)
	if err != nil {
		t.Fatalf("finishFailureStreakBreaker() error = %v", err)
	}
	if resumed {
		t.Fatal("finishFailureStreakBreaker() resumed after completed operator pauses")
	}
	if wakes != 0 {
		t.Fatalf("scheduler wakes = %d, want 0 after operator stop", wakes)
	}
	persistedLoop, err := fixture.repos.Loops.GetByID(ctx, loopID)
	if err != nil || persistedLoop == nil || persistedLoop.Status != "paused" {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want paused", persistedLoop, err)
	}
	active, err := fixture.repos.Queue.FindActiveByLoopID(ctx, loopID)
	if err != nil || active != nil {
		t.Fatalf("Queue.FindActiveByLoopID() = (%#v, %v), want no active queue", active, err)
	}
}

// Terminate is also a stop authority. A termination completed before handoff
// begins must not be replaced by the breaker's pending-rediscovery queue.
func TestFailureStreakHandoffRejectsOperatorTerminateBeforeInitialRead(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()
	projectID := "project_1"
	repo := "acme/looper"
	prNumber := int64(98)
	loopID := "loop_failure_streak_handoff_terminated"
	loopTarget := buildPullRequestTargetID(repo, prNumber)
	failedState := "failed-state"
	pendingState := "pending-state"
	metadata := mustMarshalJSON(map[string]any{
		"pauseReason":             failureStreakPauseReason,
		"fixerFailureStreak":      failureStreakState{LastRunID: "run-failed", LastHeadSHA: "head-a", FixItemsStateHash: failedState, Step: string(stepValidate), ConsecutiveCount: maxConsecutiveFixerFailures, RecordedAt: nowISO},
		"pendingFixerRediscovery": pendingFixerRediscoveryState{HeadSHA: "head-b", FixItemsStateHash: pendingState, UnresolvedThreadIDs: []string{"thread-b"}, RecordedAt: nowISO},
	})
	breakerPause := storage.LoopRecord{ID: loopID, Seq: 402, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber, Status: "paused", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(ctx, breakerPause); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	payload := mustMarshalJSON(map[string]any{"fixItemsStateHash": failedState})
	queue := storage.QueueItemRecord{ID: "queue_failure_streak_handoff_terminated", ProjectID: &projectID, LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: loopTarget, Repo: &repo, PRNumber: &prNumber, DedupeKey: buildFixerDedupeKey(projectID, loopID, repo, prNumber, "head-a", failedState), Priority: storage.QueuePriorityFixer, Status: "manual_intervention", AvailableAt: nowISO, Attempts: 3, MaxAttempts: -1, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Queue.Upsert(ctx, queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	service := &loops.Service{DB: fixture.coordinator.DB(), Repos: fixture.repos, Now: fixture.now}
	reason := "operator terminate"
	operatorTerminate, err := service.Terminate(ctx, loopID, &reason)
	if err != nil {
		t.Fatalf("Terminate() error = %v", err)
	}
	if operatorTerminate.Loop.UpdatedAt <= breakerPause.UpdatedAt {
		t.Fatalf("terminate revision = %q, want after breaker revision %q", operatorTerminate.Loop.UpdatedAt, breakerPause.UpdatedAt)
	}

	checkpoint := fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: "head-a"}}
	var wakes int
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now, OnQueueItemEnqueued: func() { wakes++ }})
	resumed, err := runner.finishFailureStreakBreaker(ctx, storage.ProjectRecord{ID: projectID}, breakerPause, queue, "", &checkpoint)
	if err != nil {
		t.Fatalf("finishFailureStreakBreaker() error = %v", err)
	}
	if resumed {
		t.Fatal("finishFailureStreakBreaker() resumed after completed operator terminate")
	}
	if wakes != 0 {
		t.Fatalf("scheduler wakes = %d, want 0 after operator terminate", wakes)
	}
	persistedLoop, err := fixture.repos.Loops.GetByID(ctx, loopID)
	if err != nil || persistedLoop == nil || persistedLoop.Status != "terminated" {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want terminated", persistedLoop, err)
	}
	active, err := fixture.repos.Queue.FindActiveByLoopID(ctx, loopID)
	if err != nil || active != nil {
		t.Fatalf("Queue.FindActiveByLoopID() = (%#v, %v), want no active queue", active, err)
	}
}
