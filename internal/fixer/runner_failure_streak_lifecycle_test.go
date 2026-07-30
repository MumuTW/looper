package fixer

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestEarlyRunFailuresParkAgainstDiscoveryState(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	prNumber := int64(42)
	loopTarget := buildPullRequestTargetID(repo, prNumber)
	nowISO := fixture.nowISO()
	loop := storage.LoopRecord{ID: "loop_early_failure_streak", Seq: 201, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	stateHash := "discovery-state-1"
	payload := mustMarshalJSON(map[string]any{
		"discoveryFingerprint": buildFixerDiscoveryFingerprint(repo, prNumber, "head-1", stateHash),
		"fixItemsStateHash":    stateHash,
	})
	projectID := "project_1"
	queue := storage.QueueItemRecord{ID: "queue_early_failure_streak", ProjectID: &projectID, LoopID: &loop.ID, Type: "fixer", TargetType: "pull_request", TargetID: loopTarget, Repo: &repo, PRNumber: &prNumber, DedupeKey: buildFixerDedupeKey(projectID, loop.ID, repo, prNumber, "head-1", stateHash), Priority: storage.QueuePriorityFixer, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: -1, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Queue.Upsert(context.Background(), queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{currentUserErr: context.DeadlineExceeded}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, RetryMaxAttempts: -1, Logger: fixture.logger, Now: fixture.now, Sleep: func(time.Duration) {}})

	for attempt := 1; attempt <= maxConsecutiveFixerFailures; attempt++ {
		claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
		if err != nil || claim == nil {
			t.Fatalf("attempt %d ClaimNextOfType() = (%#v, %v), want claim", attempt, claim, err)
		}
		result, err := runner.ProcessClaimedQueueItem(context.Background(), *claim)
		if err != nil || result == nil || result.Status != "failed" {
			t.Fatalf("attempt %d ProcessClaimedQueueItem() = (%#v, %v), want failed result", attempt, result, err)
		}
		fixture.advance(time.Minute)
	}

	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || persisted == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", persisted, err)
	}
	state, ok := parseFailureStreakState(parseJSONObject(persisted.MetadataJSON))
	if !ok || state.ConsecutiveCount != maxConsecutiveFixerFailures || state.FixItemsStateHash != stateHash || state.Step != string(stepDiscoverPR) {
		t.Fatalf("failure streak = %#v, %v, want discovery state/step at threshold", state, ok)
	}
	if persisted.Status != "paused" {
		t.Fatalf("loop status = %q, want paused", persisted.Status)
	}

	fixItems := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", ThreadFingerprint: "fingerprint-1"}}
	result, err := runner.ensureLoopForPullRequest(context.Background(), storage.ProjectRecord{ID: projectID}, repo, prNumber, "head-2", "fix-hash-2", stateHash, fixItems, []string{"t1"})
	if err != nil {
		t.Fatalf("ensureLoopForPullRequest() error = %v", err)
	}
	if result.record.Status != "paused" {
		t.Fatalf("same-state rediscovery status = %q, want paused", result.record.Status)
	}
}

func TestResumeCheckpointFailureBypassesBreaker(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	prNumber := int64(43)
	loopTarget := buildPullRequestTargetID(repo, prNumber)
	nowISO := fixture.nowISO()
	loop := storage.LoopRecord{ID: "loop_resume_checkpoint_streak", Seq: 202, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	checkpointJSON := mustMarshalJSON(fixerCheckpoint{ResumePolicy: "advance_from_checkpoint", Repair: &checkpointRepair{ParseStatus: "missing", Summary: "invalid repair result"}})
	previous := storage.RunRecord{ID: "run_resume_checkpoint_previous", LoopID: loop.ID, Status: "failed", CurrentStep: stringPtr(string(stepRepair)), LastCompletedStep: stringPtr(string(stepRepair)), CheckpointJSON: &checkpointJSON, StartedAt: nowISO, EndedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Runs.Upsert(context.Background(), previous); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	stateHash := "resume-state-1"
	payload := mustMarshalJSON(map[string]any{"fixItemsStateHash": stateHash})
	projectID := "project_1"
	queue := storage.QueueItemRecord{ID: "queue_resume_checkpoint_streak", ProjectID: &projectID, LoopID: &loop.ID, Type: "fixer", TargetType: "pull_request", TargetID: loopTarget, Repo: &repo, PRNumber: &prNumber, DedupeKey: "fixer:resume-checkpoint-streak", Priority: storage.QueuePriorityFixer, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: -1, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Queue.Upsert(context.Background(), queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, RetryMaxAttempts: -1, Logger: fixture.logger, Now: fixture.now})
	// Advance the clock so the replacement run createRunContext creates is
	// unambiguously the latest run (GetLatestByLoopID orders by started_at DESC);
	// the predecessor run was seeded at the fixture's base time.
	fixture.advance(time.Minute)

	result, err := runner.ProcessClaimedItem(context.Background(), queue)
	if err != nil || result.Status != "failed" {
		t.Fatalf("ProcessClaimedItem() = (%#v, %v), want failed result", result, err)
	}
	if result.FailureKind != FailureManualIntervention {
		t.Fatalf("result.FailureKind = %v, want manual_intervention for missing repair result", result.FailureKind)
	}
	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || persisted == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", persisted, err)
	}
	// Manual-intervention completion failures bypass the breaker: they are
	// already terminal and park the loop for human recovery. Recording them in
	// the streak would trip the breaker, clean the preserved worktree, and
	// enqueue a rediscovery handoff that destroys recovery evidence.
	state, ok := parseFailureStreakState(parseJSONObject(persisted.MetadataJSON))
	if ok && state.ConsecutiveCount > 0 {
		t.Fatalf("failure streak = %#v, want no streak recorded for manual-intervention park", state)
	}
	if persisted.Status != "paused" {
		t.Fatalf("loop.Status = %q, want paused for manual intervention", persisted.Status)
	}
	// The resume-validation manual-intervention failure must durably park the
	// checkpoint as manual_intervention so operator retry can escape via
	// MarkManualInterventionRunRestartFromDiscover. createRunContext sets the
	// resumed checkpoint policy to advance_from_checkpoint; preserving that
	// nonempty advance policy would leave the failed run ineligible for the
	// retry escape and re-park on every retry.
	parkedRun, err := fixture.repos.Runs.GetByID(context.Background(), result.RunID)
	if err != nil || parkedRun == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v)", parkedRun, err)
	}
	if got := parseCheckpoint(parkedRun.CheckpointJSON).ResumePolicy; got != loops.ResumePolicyManualIntervention {
		t.Fatalf("parked checkpoint ResumePolicy = %q, want manual_intervention", got)
	}
	rewrote, err := MarkManualInterventionRunRestartFromDiscover(context.Background(), fixture.repos, loop.ID, fixture.nowISO())
	if err != nil || !rewrote {
		t.Fatalf("MarkManualInterventionRunRestartFromDiscover() = (%v, %v), want escape rewrite after durable manual park", rewrote, err)
	}
	escaped, err := fixture.repos.Runs.GetByID(context.Background(), result.RunID)
	if err != nil || escaped == nil {
		t.Fatalf("Runs.GetByID() escaped = (%#v, %v)", escaped, err)
	}
	if got := parseCheckpoint(escaped.CheckpointJSON).ResumePolicy; got != loops.ResumePolicyRestartFromDiscover {
		t.Fatalf("escaped checkpoint ResumePolicy = %q, want restart_from_discover after operator retry", got)
	}
}

func TestLabelMismatchFinalizerEmitsCompletionAndCleansWorktree(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})
	repo := "acme/looper"
	prNumber := int64(44)
	loopTarget := buildPullRequestTargetID(repo, prNumber)
	nowISO := fixture.nowISO()
	loop := storage.LoopRecord{ID: "loop_label_cleanup", Seq: 203, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber, Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	run := storage.RunRecord{ID: "run_label_cleanup", LoopID: loop.ID, Status: "running", CurrentStep: stringPtr(string(stepDiscoverPR)), StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Runs.Upsert(context.Background(), run); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	projectID := "project_1"
	queue := storage.QueueItemRecord{ID: "queue_label_cleanup", ProjectID: &projectID, LoopID: &loop.ID, Type: "fixer", TargetType: "pull_request", TargetID: loopTarget, Repo: &repo, PRNumber: &prNumber, DedupeKey: "fixer:label-cleanup", Priority: storage.QueuePriorityFixer, Status: "running", AvailableAt: nowISO, MaxAttempts: -1, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Queue.Upsert(context.Background(), queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	project := storage.ProjectRecord{ID: projectID, RepoPath: t.TempDir()}
	checkpoint := fixerCheckpoint{Worktree: &checkpointWorktree{Path: filepath.Join(t.TempDir(), "wt-44"), Branch: "feature/fix-44", PreparedAt: nowISO}}

	result, err := runner.finishLabelMismatchFixerQueueItem(context.Background(), project, loop, &run, queue, checkpoint, "labels no longer match")
	if err != nil || result.Status != "skipped" {
		t.Fatalf("finishLabelMismatchFixerQueueItem() = (%#v, %v), want skipped", result, err)
	}
	if len(git.cleanupCalls) != 1 {
		t.Fatalf("cleanup calls = %#v, want one", git.cleanupCalls)
	}
	events, err := fixture.repos.Events.ListByEntity(context.Background(), "run", run.ID)
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	foundCompleted := false
	for _, event := range events {
		foundCompleted = foundCompleted || event.EventType == "run.completed"
	}
	if !foundCompleted {
		t.Fatalf("events = %#v, want run.completed", events)
	}
}

func TestActiveRunContentionDoesNotRecordFailureStreak(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	loop := storage.LoopRecord{ID: "loop_active_contention_no_streak", Status: "running", MetadataJSON: nil}
	projectID := "project_1"
	loopID := loop.ID
	queue := storage.QueueItemRecord{ID: "queue_active_contention_no_streak", ProjectID: &projectID, LoopID: &loopID, Type: "fixer", Priority: storage.QueuePriorityFixer, Status: "running", Attempts: 2, MaxAttempts: -1, AvailableAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loop.ID, Seq: 204, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := fixture.repos.Queue.Upsert(context.Background(), queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, RetryMaxAttempts: -1, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.recoverClaimedItem(context.Background(), queue, activeFixerRunError("already running"))
	if err != nil || result == nil {
		t.Fatalf("recoverClaimedItem() = (%#v, %v), want result", result, err)
	}
	persistedQueue, err := fixture.repos.Queue.GetByID(context.Background(), queue.ID)
	if err != nil || persistedQueue == nil {
		t.Fatalf("Queue.GetByID() = (%#v, %v), want queue", persistedQueue, err)
	}
	if persistedQueue.Attempts != queue.Attempts {
		t.Fatalf("attempts = %d, want %d", persistedQueue.Attempts, queue.Attempts)
	}
	persistedLoop, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || persistedLoop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", persistedLoop, err)
	}
	if _, ok := parseJSONObject(persistedLoop.MetadataJSON)["fixerFailureStreak"]; ok {
		t.Fatalf("active-run contention recorded failure streak: %#v", parseJSONObject(persistedLoop.MetadataJSON))
	}
}

func TestRecoveredRunFailureReconcilesStillQueuedLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	loopID := "loop_recovered_queued_failure"
	projectID := "project_1"
	nowISO := fixture.nowISO()
	loop := storage.LoopRecord{ID: loopID, Seq: 205, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue := storage.QueueItemRecord{ID: "queue_recovered_queued_failure", ProjectID: &projectID, LoopID: &loopID, Type: "fixer", Priority: storage.QueuePriorityFixer, Status: "running", AvailableAt: nowISO, Attempts: 2, MaxAttempts: -1, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Queue.Upsert(context.Background(), queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})
	terminal := queue
	terminal.Status = "manual_intervention"

	if _, err := runner.reconcileRecoveredLoop(context.Background(), queue, &terminal); err != nil {
		t.Fatalf("reconcileRecoveredLoop() error = %v", err)
	}
	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loopID)
	if err != nil || persisted == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", persisted, err)
	}
	if persisted.Status != "paused" || persisted.NextRunAt != nil {
		t.Fatalf("loop = %#v, want still-queued run failure reconciled to paused", persisted)
	}
}

func TestRecoveryReconcilesAlreadyTerminalBreakerQueue(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	loopID := "loop_terminal_breaker_recovery"
	projectID := "project_1"
	nowISO := fixture.nowISO()
	metadata := mustMarshalJSON(map[string]any{"pauseReason": failureStreakPauseReason})
	loop := storage.LoopRecord{ID: loopID, Seq: 208, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", Status: "running", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queue := storage.QueueItemRecord{ID: "queue_terminal_breaker_recovery", ProjectID: &projectID, LoopID: &loopID, Type: "fixer", Priority: storage.QueuePriorityFixer, Status: "running", AvailableAt: nowISO, Attempts: 2, MaxAttempts: -1, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Queue.Upsert(context.Background(), queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	if err := fixture.repos.Queue.Fail(context.Background(), storage.QueueFailInput{ID: queue.ID, Attempts: 3, FinishedAt: nowISO, ErrorMessage: stringPtr("breaker threshold"), ErrorKind: string(FailureNonRetryable), UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Fail() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.recoverClaimedItem(context.Background(), queue, errors.New("loop pause persistence failed after queue terminalization"))
	if err != nil || result == nil {
		t.Fatalf("recoverClaimedItem() = (%#v, %v), want reconciled result", result, err)
	}
	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loopID)
	if err != nil || persisted == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", persisted, err)
	}
	if persisted.Status != "paused" || persisted.NextRunAt != nil {
		t.Fatalf("loop = %#v, want terminal queue reconciled to paused", persisted)
	}
}
