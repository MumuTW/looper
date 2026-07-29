package fixer

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// The breaker may observe a pending rediscovery, then race an operator stop.
// The handoff must re-check the durable loop revision before creating a queue;
// a pause can otherwise retain the exact same status and breaker metadata.
func TestFailureStreakHandoffDoesNotOverrideConcurrentOperatorStop(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		stop       func(*loops.Service, context.Context, string, *string) error
		wantStatus string
	}{
		{
			name: "pause",
			stop: func(service *loops.Service, ctx context.Context, loopID string, reason *string) error {
				_, err := service.Pause(ctx, loopID, reason)
				return err
			},
			wantStatus: "paused",
		},
		{
			name: "terminate",
			stop: func(service *loops.Service, ctx context.Context, loopID string, reason *string) error {
				_, err := service.Terminate(ctx, loopID, reason)
				return err
			},
			wantStatus: "terminated",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := newRunnerFixture(t)
			ctx := context.Background()
			nowISO := fixture.nowISO()
			projectID := "project_1"
			repo := "acme/looper"
			prNumber := int64(97)
			loopID := "loop_failure_streak_handoff_" + tc.name
			loopTarget := buildPullRequestTargetID(repo, prNumber)
			failedState := "failed-state"
			pendingState := "pending-state"
			metadata := mustMarshalJSON(map[string]any{
				"pauseReason": failureStreakPauseReason,
				"fixerFailureStreak": failureStreakState{
					LastRunID: "run-failed", LastHeadSHA: "head-a", FixItemsStateHash: failedState,
					Step: string(stepValidate), ConsecutiveCount: maxConsecutiveFixerFailures, RecordedAt: nowISO,
				},
				"pendingFixerRediscovery": pendingFixerRediscoveryState{
					HeadSHA: "head-b", FixItemsStateHash: pendingState, UnresolvedThreadIDs: []string{"thread-b"}, RecordedAt: nowISO,
				},
			})
			loop := storage.LoopRecord{ID: loopID, Seq: 401, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber, Status: "paused", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
			if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
				t.Fatalf("Loops.Upsert() error = %v", err)
			}
			payload := mustMarshalJSON(map[string]any{"fixItemsStateHash": failedState})
			queue := storage.QueueItemRecord{ID: "queue_failure_streak_handoff_" + tc.name, ProjectID: &projectID, LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: loopTarget, Repo: &repo, PRNumber: &prNumber, DedupeKey: buildFixerDedupeKey(projectID, loopID, repo, prNumber, "head-a", failedState), Priority: storage.QueuePriorityFixer, Status: "manual_intervention", AvailableAt: nowISO, Attempts: 3, MaxAttempts: -1, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}
			if err := fixture.repos.Queue.Upsert(ctx, queue); err != nil {
				t.Fatalf("Queue.Upsert() error = %v", err)
			}
			checkpoint := fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: "head-a"}}
			var wakes int
			runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now, OnQueueItemEnqueued: func() { wakes++ }})
			runner.failureStreakHandoffReadHook = func() {
				// The clock deliberately does not advance: a repeated Pause keeps the
				// same status and metadata, so only the strictly monotonic loop
				// revision can prevent the stale handoff from resuming it.
				reason := "operator stop"
				service := &loops.Service{DB: fixture.coordinator.DB(), Repos: fixture.repos, Now: fixture.now}
				if err := tc.stop(service, ctx, loopID, &reason); err != nil {
					t.Fatalf("operator %s error = %v", tc.name, err)
				}
			}

			resumed, err := runner.finishFailureStreakBreaker(ctx, storage.ProjectRecord{ID: projectID}, loop, queue, &checkpoint)
			if err != nil {
				t.Fatalf("finishFailureStreakBreaker() error = %v", err)
			}
			if resumed {
				t.Fatal("finishFailureStreakBreaker() resumed after operator stop")
			}
			if wakes != 0 {
				t.Fatalf("scheduler wakes = %d, want 0 after operator stop", wakes)
			}
			persistedLoop, err := fixture.repos.Loops.GetByID(ctx, loopID)
			if err != nil || persistedLoop == nil || persistedLoop.Status != tc.wantStatus {
				t.Fatalf("Loops.GetByID() = (%#v, %v), want %s", persistedLoop, err, tc.wantStatus)
			}
			active, err := fixture.repos.Queue.FindActiveByLoopID(ctx, loopID)
			if err != nil || active != nil {
				t.Fatalf("Queue.FindActiveByLoopID() = (%#v, %v), want no active queue", active, err)
			}
		})
	}
}
