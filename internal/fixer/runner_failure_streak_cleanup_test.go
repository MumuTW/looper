package fixer

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

func TestRecoveredPreStepBreakerFailureCleansPreparedWorktree(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(91)
	loopID := "loop_recovered_pre_step_cleanup"
	projectID := "project_1"
	stateHash := "pre-step-failure-state"
	metadata := mustMarshalJSON(map[string]any{
		"fixerFailureStreak": failureStreakState{
			LastRunID:         "run-pre-step-2",
			FixItemsStateHash: stateHash,
			Step:              string(stepDiscoverPR),
			ConsecutiveCount:  2,
			RecordedAt:        nowISO,
		},
	})
	loopTarget := buildPullRequestTargetID(repo, prNumber)
	loop := storage.LoopRecord{ID: loopID, Seq: 209, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber, Status: "running", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	payload := mustMarshalJSON(map[string]any{"fixItemsStateHash": stateHash})
	queue := storage.QueueItemRecord{ID: "queue_recovered_pre_step_cleanup", ProjectID: &projectID, LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: loopTarget, Repo: &repo, PRNumber: &prNumber, Priority: storage.QueuePriorityFixer, Status: "running", AvailableAt: nowISO, Attempts: 2, MaxAttempts: -1, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Queue.Upsert(context.Background(), queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})
	checkpoint := fixerCheckpoint{Worktree: &checkpointWorktree{Path: filepath.Join(t.TempDir(), "wt-91"), Branch: "fix/pr-91", PreparedAt: nowISO}}
	runFailure := &claimedRunFailureError{cause: errors.New("ownership API failed"), runID: "run-pre-step-3", checkpoint: checkpoint, step: stepDiscoverPR}

	result, err := runner.recoverClaimedItem(context.Background(), queue, runFailure)
	if err != nil || result == nil {
		t.Fatalf("recoverClaimedItem() = (%#v, %v), want terminal result", result, err)
	}
	if len(git.cleanupCalls) != 1 {
		t.Fatalf("cleanup calls = %#v, want one terminal cleanup", git.cleanupCalls)
	}
	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loopID)
	if err != nil || persisted == nil || persisted.Status != "paused" {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want paused loop", persisted, err)
	}
}

func TestInlineLateStepBreakerCleansAndImmediatelyQueuesPendingState(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(92)
	loopID := "loop_inline_breaker_pending"
	projectID := "project_1"
	stateA := "failure-state-a"
	stateB := "pending-state-b"
	pending := pendingFixerRediscoveryState{HeadSHA: "head-b", FixItemsStateHash: stateB, UnresolvedThreadIDs: []string{"thread-b"}, RecordedAt: nowISO}
	metadata := mustMarshalJSON(map[string]any{
		"fixerFailureStreak":      failureStreakState{LastRunID: "run-inline-2", FixItemsStateHash: stateA, Step: string(stepValidate), ConsecutiveCount: 2, RecordedAt: nowISO},
		"pendingFixerRediscovery": pending,
	})
	loopTarget := buildPullRequestTargetID(repo, prNumber)
	loop := storage.LoopRecord{ID: loopID, Seq: 210, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber, Status: "running", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	payloadA := mustMarshalJSON(map[string]any{"discoveryFingerprint": buildFixerDiscoveryFingerprint(repo, prNumber, "head-a", stateA), "fixItemsStateHash": stateA})
	queue := storage.QueueItemRecord{ID: "queue_inline_breaker_pending", ProjectID: &projectID, LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: loopTarget, Repo: &repo, PRNumber: &prNumber, DedupeKey: buildFixerDedupeKey(projectID, loopID, repo, prNumber, "head-a", stateA), Priority: storage.QueuePriorityFixer, Status: "running", AvailableAt: nowISO, Attempts: 2, MaxAttempts: -1, PayloadJSON: &payloadA, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Queue.Upsert(context.Background(), queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	checkpoint := fixerCheckpoint{ResumePolicy: "advance_from_checkpoint", Worktree: &checkpointWorktree{Path: filepath.Join(t.TempDir(), "wt-92"), Branch: "fix/pr-92", PreparedAt: nowISO}}
	checkpointJSON := mustMarshalJSON(checkpoint)
	failedRun := storage.RunRecord{ID: "run-inline-3", LoopID: loopID, Status: "failed", CurrentStep: stringPtr(string(stepValidate)), CheckpointJSON: &checkpointJSON, StartedAt: nowISO, EndedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Runs.Upsert(context.Background(), failedRun); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	project, err := fixture.repos.Projects.GetByID(context.Background(), projectID)
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want project", project, err)
	}
	git := &fakeGitGateway{}
	var wakeStatuses []string
	var wakeErr error
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, RetryMaxAttempts: -1, Logger: fixture.logger, Now: fixture.now, Sleep: func(time.Duration) {}, OnQueueItemEnqueued: func() {
		persisted, err := fixture.repos.Loops.GetByID(context.Background(), loopID)
		if err != nil {
			wakeErr = err
			return
		}
		if persisted != nil {
			wakeStatuses = append(wakeStatuses, persisted.Status)
		}
	}})

	failedQueue, breakerStreak, err := runner.failQueueItemWithBreaker(context.Background(), loop, queue, failedRun.ID, checkpoint, stepValidate, &loopError{message: "validation failed", kind: FailureRetryableTransient})
	if err != nil || breakerStreak != maxConsecutiveFixerFailures || failedQueue == nil || failedQueue.Status != "manual_intervention" {
		t.Fatalf("failQueueItemWithBreaker() = (%#v, %d, %v), want breaker terminal", failedQueue, breakerStreak, err)
	}
	paused, err := runner.updateLoop(context.Background(), loop, func(updated *storage.LoopRecord) {
		updated.Status = "paused"
		updated.NextRunAt = nil
	})
	if err != nil {
		t.Fatalf("updateLoop(paused) error = %v", err)
	}
	resumed, err := runner.finishFailureStreakBreaker(context.Background(), *project, paused, queue, &checkpoint)
	if err != nil || !resumed {
		t.Fatalf("finishFailureStreakBreaker() = (%v, %v), want immediate pending resume", resumed, err)
	}
	if wakeErr != nil || len(wakeStatuses) != 1 || wakeStatuses[0] != "queued" {
		t.Fatalf("scheduler wake observed statuses %v (err %v), want one durable queued status", wakeStatuses, wakeErr)
	}
	if len(git.cleanupCalls) != 1 {
		t.Fatalf("cleanup calls = %#v, want one inline breaker cleanup", git.cleanupCalls)
	}
	active, err := fixture.repos.Queue.FindActiveByLoopID(context.Background(), loopID)
	if err != nil || active == nil || active.Status != "queued" {
		t.Fatalf("Queue.FindActiveByLoopID() = (%#v, %v), want pending state queue", active, err)
	}
	activePayload := parseJSONObject(active.PayloadJSON)
	if got, _ := stringFromAny(activePayload["fixItemsStateHash"]); got != stateB {
		t.Fatalf("queued fixItemsStateHash = %q, want %q", got, stateB)
	}
	persistedLoop, err := fixture.repos.Loops.GetByID(context.Background(), loopID)
	if err != nil || persistedLoop == nil || persistedLoop.Status != "queued" {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want queued loop", persistedLoop, err)
	}
	if persistedLoop.UpdatedAt <= paused.UpdatedAt {
		t.Fatalf("handoff revision = %q, want after breaker pause %q", persistedLoop.UpdatedAt, paused.UpdatedAt)
	}
	loopMeta := parseJSONObject(persistedLoop.MetadataJSON)
	if _, ok := loopMeta["pendingFixerRediscovery"]; ok {
		t.Fatalf("pending rediscovery survived threshold handoff: %#v", loopMeta)
	}
	if _, ok := loopMeta["fixerFailureStreak"]; ok {
		t.Fatalf("failure streak survived threshold handoff: %#v", loopMeta)
	}
	persistedRun, err := fixture.repos.Runs.GetByID(context.Background(), failedRun.ID)
	if err != nil || persistedRun == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v), want failed run", persistedRun, err)
	}
	if got := parseCheckpoint(persistedRun.CheckpointJSON).ResumePolicy; got != "restart_from_discover" {
		t.Fatalf("resume policy = %q, want restart_from_discover", got)
	}
}

func TestProcessClaimedItemDefersDuringPendingRediscoveryHandoff(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	projectID := "project_1"
	repo := "acme/looper"
	prNumber := int64(94)
	loopID := "loop_paused_claim_gate"
	loopTarget := buildPullRequestTargetID(repo, prNumber)
	headSHA := "head-pending"
	stateHash := "resume-handoff-state"
	metadata := mustMarshalJSON(map[string]any{
		"pauseReason": failureStreakPauseReason,
		"pendingFixerRediscovery": pendingFixerRediscoveryState{
			HeadSHA:           headSHA,
			FixItemsStateHash: stateHash,
			RecordedAt:        nowISO,
		},
	})
	loop := storage.LoopRecord{ID: loopID, Seq: 211, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber, Status: "paused", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue := storage.QueueItemRecord{ID: "queue_paused_claim_gate", ProjectID: &projectID, LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: loopTarget, Repo: &repo, PRNumber: &prNumber, DedupeKey: buildFixerDedupeKey(projectID, loopID, repo, prNumber, headSHA, stateHash), Priority: storage.QueuePriorityFixer, Status: "running", AvailableAt: nowISO, Attempts: 2, MaxAttempts: -1, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Queue.Upsert(context.Background(), queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, RetryMaxAttempts: -1, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.ProcessClaimedItem(context.Background(), queue)
	if err != nil || result.Status != "queued" {
		t.Fatalf("ProcessClaimedItem() = (%#v, %v), want deferred queued result", result, err)
	}
	persisted, err := fixture.repos.Queue.GetByID(context.Background(), queue.ID)
	if err != nil || persisted == nil || persisted.Status != "queued" || persisted.Attempts != queue.Attempts {
		t.Fatalf("Queue.GetByID() = (%#v, %v), want queued with unchanged attempts", persisted, err)
	}
	runs, err := fixture.repos.Runs.ListByLoop(context.Background(), loopID)
	if err != nil || len(runs) != 0 {
		t.Fatalf("Runs.ListByLoop() = (%#v, %v), want no run before resume is durable", runs, err)
	}
}

func TestProcessClaimedItemCancelsPersistentPausedLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	projectID := "project_1"
	repo := "acme/looper"
	prNumber := int64(95)
	loopID := "loop_persistent_pause"
	loopTarget := buildPullRequestTargetID(repo, prNumber)
	metadata := mustMarshalJSON(map[string]any{
		"pauseReason": "operator_pause",
		// A pending rediscovery may be present on a real paused loop, but it is
		// not an authority to resume a running claim.
		"pendingFixerRediscovery": pendingFixerRediscoveryState{HeadSHA: "head-95", FixItemsStateHash: "operator-state", RecordedAt: nowISO},
	})
	loop := storage.LoopRecord{ID: loopID, Seq: 212, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber, Status: "paused", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue := storage.QueueItemRecord{ID: "queue_persistent_pause", ProjectID: &projectID, LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: loopTarget, Repo: &repo, PRNumber: &prNumber, DedupeKey: buildFixerDedupeKey(projectID, loopID, repo, prNumber, "head-95", "operator-state"), Priority: storage.QueuePriorityFixer, Status: "running", AvailableAt: nowISO, Attempts: 2, MaxAttempts: -1, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Queue.Upsert(context.Background(), queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.ProcessClaimedItem(context.Background(), queue)
	if err != nil || result.Status != "cancelled" {
		t.Fatalf("ProcessClaimedItem() = (%#v, %v), want persistent paused claim cancelled", result, err)
	}
	persisted, err := fixture.repos.Queue.GetByID(context.Background(), queue.ID)
	if err != nil || persisted == nil || persisted.Status != "cancelled" {
		t.Fatalf("Queue.GetByID() = (%#v, %v), want durable cancelled claim", persisted, err)
	}
}

func TestProcessClaimedItemResumeHandoffPreservesConcurrentStopCancellation(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	projectID := "project_1"
	repo := "acme/looper"
	prNumber := int64(96)
	loopID := "loop_resume_handoff_stop"
	loopTarget := buildPullRequestTargetID(repo, prNumber)
	headSHA := "head-96"
	stateHash := "resume-stop-state"
	metadata := mustMarshalJSON(map[string]any{
		"pauseReason": failureStreakPauseReason,
		"pendingFixerRediscovery": pendingFixerRediscoveryState{
			HeadSHA:           headSHA,
			FixItemsStateHash: stateHash,
			RecordedAt:        nowISO,
		},
	})
	loop := storage.LoopRecord{ID: loopID, Seq: 213, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber, Status: "paused", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue := storage.QueueItemRecord{ID: "queue_resume_handoff_stop", ProjectID: &projectID, LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: loopTarget, Repo: &repo, PRNumber: &prNumber, DedupeKey: buildFixerDedupeKey(projectID, loopID, repo, prNumber, headSHA, stateHash), Priority: storage.QueuePriorityFixer, Status: "running", AvailableAt: nowISO, Attempts: 2, MaxAttempts: -1, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Queue.Upsert(context.Background(), queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	reason := "operator stop"
	if _, err := fixture.repos.Queue.CancelByLoop(context.Background(), loopID, nowISO, &reason); err != nil {
		t.Fatalf("Queue.CancelByLoop() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.ProcessClaimedItem(context.Background(), queue)
	if err != nil || result.Status != "cancelled" {
		t.Fatalf("ProcessClaimedItem() = (%#v, %v), want cancelled concurrent-stop result", result, err)
	}
	persisted, err := fixture.repos.Queue.GetByID(context.Background(), queue.ID)
	if err != nil || persisted == nil || persisted.Status != "cancelled" {
		t.Fatalf("Queue.GetByID() = (%#v, %v), want stop cancellation preserved", persisted, err)
	}
}

func TestQueuedRetryStateReplacementRestartsFromDiscover(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(93)
	loopID := "loop_queued_retry_state_replacement"
	projectID := "project_1"
	stateA := "queued-state-a"
	stateB := "queued-state-b"
	loopTarget := buildPullRequestTargetID(repo, prNumber)
	loop := storage.LoopRecord{ID: loopID, Seq: 211, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	staleCheckpoint := fixerCheckpoint{ResumePolicy: "advance_from_checkpoint", FixItems: []FixItem{{Type: "comment", ID: "comment-a", ThreadID: "thread-a", ThreadFingerprint: "fingerprint-a"}}, Repair: &checkpointRepair{ParseStatus: "parsed", Summary: "state A repair"}}
	checkpointJSON := mustMarshalJSON(staleCheckpoint)
	failedRun := storage.RunRecord{ID: "run_queued_state_a", LoopID: loopID, Status: "failed", CurrentStep: stringPtr(string(stepRepair)), LastCompletedStep: stringPtr(string(stepCollectFixes)), CheckpointJSON: &checkpointJSON, StartedAt: nowISO, EndedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Runs.Upsert(context.Background(), failedRun); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	payloadA := mustMarshalJSON(map[string]any{"discoveryFingerprint": buildFixerDiscoveryFingerprint(repo, prNumber, "head-a", stateA), "fixItemsStateHash": stateA})
	queue := storage.QueueItemRecord{ID: "queue_queued_state_a", ProjectID: &projectID, LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: loopTarget, Repo: &repo, PRNumber: &prNumber, DedupeKey: buildFixerDedupeKey(projectID, loopID, repo, prNumber, "head-a", stateA), Priority: storage.QueuePriorityFixer, Status: "queued", AvailableAt: nowISO, Attempts: 1, MaxAttempts: -1, PayloadJSON: &payloadA, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Queue.Upsert(context.Background(), queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, RetryMaxAttempts: -1, Logger: fixture.logger, Now: fixture.now})

	replaced, err := runner.enqueue(context.Background(), enqueueInput{ProjectID: projectID, LoopID: loopID, Repo: repo, PRNumber: prNumber, HeadSHA: "head-b", FixItemsHash: stateB, AvailableAt: fixture.now()})
	if err != nil {
		t.Fatalf("enqueue(state B) error = %v", err)
	}
	if replaced.ID != queue.ID {
		t.Fatalf("replacement queue ID = %q, want reused %q", replaced.ID, queue.ID)
	}
	payloadB := parseJSONObject(replaced.PayloadJSON)
	if got, _ := stringFromAny(payloadB["fixItemsStateHash"]); got != stateB {
		t.Fatalf("replacement state hash = %q, want %q", got, stateB)
	}
	persistedRun, err := fixture.repos.Runs.GetByID(context.Background(), failedRun.ID)
	if err != nil || persistedRun == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v), want failed run", persistedRun, err)
	}
	if got := parseCheckpoint(persistedRun.CheckpointJSON).ResumePolicy; got != "restart_from_discover" {
		t.Fatalf("resume policy = %q, want restart_from_discover", got)
	}
	replacementRun, err := runner.createRunContext(context.Background(), loop)
	if err != nil {
		t.Fatalf("createRunContext() error = %v", err)
	}
	if replacementRun.StartStep != stepDiscoverPR || replacementRun.Resumed || replacementRun.Checkpoint.FixItems != nil {
		t.Fatalf("replacement run = %#v, want fresh discover context", replacementRun)
	}
}
