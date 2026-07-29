package fixer

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

func TestFailureStreakRecordingIsIdempotentPerRun(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	loop := storage.LoopRecord{ID: "loop_failure_streak_idempotent", Seq: 206, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})
	checkpoint := fixerCheckpoint{FixItems: []FixItem{{Type: "comment", ID: "comment-1", ThreadID: "thread-1", ThreadFingerprint: "fingerprint-1"}}}

	first, _, err := runner.recordFixerFailureStreak(context.Background(), loop, storage.QueueItemRecord{}, "run-idempotent-1", checkpoint, stepRepair)
	if err != nil {
		t.Fatalf("first recordFixerFailureStreak() error = %v", err)
	}
	replayed, _, err := runner.recordFixerFailureStreak(context.Background(), loop, storage.QueueItemRecord{}, "run-idempotent-1", checkpoint, stepRepair)
	if err != nil {
		t.Fatalf("replayed recordFixerFailureStreak() error = %v", err)
	}
	second, _, err := runner.recordFixerFailureStreak(context.Background(), loop, storage.QueueItemRecord{}, "run-idempotent-2", checkpoint, stepRepair)
	if err != nil {
		t.Fatalf("second-run recordFixerFailureStreak() error = %v", err)
	}
	if first != 1 || replayed != 1 || second != 2 {
		t.Fatalf("streaks = (%d, %d, %d), want (1, 1, 2)", first, replayed, second)
	}
}

func TestChangedFailureStateRestartsReplacementRunFromDiscover(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	oldStateHash := "old-fix-items-state"
	metadata := mustMarshalJSON(map[string]any{
		"pauseReason": failureStreakPauseReason,
		"fixerFailureStreak": failureStreakState{
			LastRunID:         "run-stale-checkpoint",
			FixItemsStateHash: oldStateHash,
			Step:              string(stepRepair),
			ConsecutiveCount:  maxConsecutiveFixerFailures,
			RecordedAt:        nowISO,
		},
		"pendingFixerRediscovery": pendingFixerRediscoveryState{
			HeadSHA:             "replacement-head",
			FixItemsStateHash:   "replacement-fix-items-state",
			UnresolvedThreadIDs: []string{"replacement-thread"},
			RecordedAt:          nowISO,
		},
	})
	loop := storage.LoopRecord{ID: "loop_failure_streak_new_feedback", Seq: 207, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", Status: "paused", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	staleCheckpoint := fixerCheckpoint{
		ResumePolicy: "advance_from_checkpoint",
		FixItems:     []FixItem{{Type: "comment", ID: "old-comment", ThreadID: "old-thread", ThreadFingerprint: "old-fingerprint"}},
		Repair:       &checkpointRepair{ParseStatus: "parsed", Summary: "old repair"},
	}
	checkpointJSON := mustMarshalJSON(staleCheckpoint)
	failedRun := storage.RunRecord{ID: "run-stale-checkpoint", LoopID: loop.ID, Status: "failed", CurrentStep: stringPtr(string(stepRepair)), LastCompletedStep: stringPtr(string(stepCollectFixes)), CheckpointJSON: &checkpointJSON, StartedAt: nowISO, EndedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Runs.Upsert(context.Background(), failedRun); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})

	resumed, updatedLoop, err := runner.resumePausedFailureStreakLoop(context.Background(), loop, "replacement-head", "replacement-fix-items-state", []string{"replacement-thread"})
	if err != nil || !resumed || updatedLoop.Status != "queued" {
		t.Fatalf("resumePausedFailureStreakLoop() = (%v, %#v, %v), want queued replacement", resumed, updatedLoop, err)
	}
	replacement, err := runner.createRunContext(context.Background(), updatedLoop)
	if err != nil {
		t.Fatalf("createRunContext() error = %v", err)
	}
	if replacement.StartStep != stepDiscoverPR || replacement.Resumed {
		t.Fatalf("replacement context = %#v, want fresh discover run", replacement)
	}
	if replacement.Checkpoint.FixItems != nil {
		t.Fatalf("replacement checkpoint retained stale fix items: %#v", replacement.Checkpoint.FixItems)
	}
	persistedLoop, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || persistedLoop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want resumed loop", persistedLoop, err)
	}
	if _, ok := parseJSONObject(persistedLoop.MetadataJSON)["pendingFixerRediscovery"]; ok {
		t.Fatalf("matching pending rediscovery survived resume: %#v", parseJSONObject(persistedLoop.MetadataJSON))
	}
}

func TestFailureStreakUsesDiscoveryStateBeforeFilteredCheckpoint(t *testing.T) {
	t.Parallel()
	discoveryStateHash := "all-discovery-fix-items"
	payload := mustMarshalJSON(map[string]any{"fixItemsStateHash": discoveryStateHash})
	queue := storage.QueueItemRecord{PayloadJSON: &payload}
	checkpoint := fixerCheckpoint{FixItems: []FixItem{{Type: "comment", ID: "remaining-comment", ThreadID: "remaining-thread", ThreadFingerprint: "remaining-fingerprint"}}}

	if got := failureStreakFixItemsStateHash(queue, checkpoint); got != discoveryStateHash {
		t.Fatalf("failureStreakFixItemsStateHash() = %q, want discovery state %q", got, discoveryStateHash)
	}
}
