package fixer

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestFixerRepairTaskOutcomeUsesStructuredAuthority(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		result      AgentResult
		blocked     bool
		failureKind QueueFailureKind
		wantError   bool
	}{
		{
			name: "successful prose is not a blocking signal",
			result: AgentResult{
				Summary: "Could not reproduce the GitHub failure, but fixed the null handling",
				Stdout:  `__LOOPER_RESULT__={"outcome":"completed","summary":"Could not reproduce the GitHub failure, but fixed the null handling"}`,
			},
		},
		{
			name:   "translated JSON event payload remains authoritative",
			result: AgentResult{Stdout: `{"type":"item.completed","item":{"text":"embedded marker"}}`, CompletionPayload: `{"outcome":"completed","summary":"done"}`},
		},
		{
			name:      "unstructured auth prose is diagnostic only",
			result:    AgentResult{Summary: "GitHub auth failed before editing"},
			wantError: true,
		},
		{
			name:      "blocked outcome requires failure kind",
			result:    AgentResult{Stdout: `__LOOPER_RESULT__={"outcome":"blocked","summary":"blocked"}`},
			wantError: true,
		},
		{
			name:      "unrecognized outcome is rejected",
			result:    AgentResult{Stdout: `__LOOPER_RESULT__={"outcome":"failed","summary":"failed"}`},
			wantError: true,
		},
		{
			name: "structured auth block leaves autonomous lane",
			result: AgentResult{
				Summary: "GitHub auth failed before editing",
				Stdout:  `__LOOPER_RESULT__={"outcome":"blocked","failure_kind":"manual_intervention","summary":"GitHub auth failed before editing"}`,
			},
			blocked:     true,
			failureKind: FailureManualIntervention,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			blocked, _, kind, outcomeErr := fixerRepairTaskOutcome(tt.result)
			if blocked != tt.blocked || kind != tt.failureKind || (outcomeErr != nil) != tt.wantError {
				t.Fatalf("fixerRepairTaskOutcome() = (%t, %q, %v), want (%t, %q, error=%t)", blocked, kind, outcomeErr, tt.blocked, tt.failureKind, tt.wantError)
			}
		})
	}
}

func TestFixerOutcomePreservesFailureOrderAndCurrentRunProgress(t *testing.T) {
	t.Parallel()

	checkpoint := fixerCheckpoint{
		ReconcileCommits: &checkpointReconcileCommits{NewCommitSHAs: []string{"commit-1"}},
		ResolvedComments: &checkpointResolvedComments{Items: []checkpointResolvedComment{
			{Status: "already_resolved"},
			{Status: "stale_missing_from_review"},
		}},
	}
	checkpoint.recordFailure(stepRepair, &loopError{message: "repair failed", kind: FailureRetryableTransient})
	checkpoint.recordFailure(stepCleanupWorktree, &loopError{message: "cleanup failed", kind: FailureRetryableTransient})
	checkpoint.refreshOutcomeProgress()

	if checkpoint.Outcome == nil || checkpoint.Outcome.PrimaryFailure == nil {
		t.Fatal("Outcome.PrimaryFailure = nil")
	}
	if checkpoint.Outcome.PrimaryFailure.Message != "repair failed" || len(checkpoint.Outcome.SecondaryIssues) != 1 || checkpoint.Outcome.SecondaryIssues[0].Message != "cleanup failed" {
		t.Fatalf("Outcome = %#v, want stable primary then cleanup secondary", checkpoint.Outcome)
	}
	if !checkpoint.Outcome.PartialSuccess || !checkpoint.Outcome.Progress.CommitProduced {
		t.Fatalf("Outcome = %#v, want commit-backed partial success", checkpoint.Outcome)
	}
	if checkpoint.Outcome.Progress.ThreadsResolved != 0 {
		t.Fatalf("ThreadsResolved = %d, want external/stale observations excluded", checkpoint.Outcome.Progress.ThreadsResolved)
	}
}

func TestFixerOutcomeExcludesInheritedRetryProgress(t *testing.T) {
	t.Parallel()

	checkpoint := fixerCheckpoint{
		RunPreStartAt:    "2026-04-11T12:00:00.000Z",
		ReconcileCommits: &checkpointReconcileCommits{NewCommitSHAs: []string{"old-commit"}, CompletedAt: "2026-04-11T11:59:00.000Z"},
		Push:             &checkpointPush{Pushed: true, PushedAt: "2026-04-11T11:59:10.000Z"},
		ResolvedComments: &checkpointResolvedComments{Items: []checkpointResolvedComment{{Status: "resolved", ReplyState: "sent", UpdatedAt: "2026-04-11T11:59:20.000Z"}}},
	}
	checkpoint.recordFailure(stepRepair, &loopError{message: "retry failed before progress", kind: FailureRetryableTransient})
	checkpoint.refreshOutcomeProgress()

	if checkpoint.Outcome == nil || checkpoint.Outcome.PartialSuccess || checkpoint.Outcome.Progress != (FixerDurableProgress{}) {
		t.Fatalf("Outcome = %#v, want inherited predecessor progress excluded", checkpoint.Outcome)
	}
}

func TestCreateRunContextRebasesOutcomeProgressForRetry(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	prNumber := int64(42)
	target := buildPullRequestTargetID(repo, prNumber)
	loop := storage.LoopRecord{ID: "loop_retry_progress", ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	oldAt := "2026-04-11T11:59:00.000Z"
	oldCheckpoint := fixerCheckpoint{
		ResumePolicy:     loops.ResumePolicyReplayStep,
		FixItems:         []FixItem{{ID: "c1", Type: "comment", ThreadID: "t1"}},
		Worktree:         &checkpointWorktree{Path: filepath.Join(t.TempDir(), "worktree"), Branch: "feature/fix-42", PreparedAt: oldAt},
		ReconcileCommits: &checkpointReconcileCommits{NewCommitSHAs: []string{"old-commit"}, CompletedAt: oldAt},
		Push:             &checkpointPush{Pushed: true, PushedAt: oldAt},
		ResolvedComments: &checkpointResolvedComments{Items: []checkpointResolvedComment{{Status: "resolved", ReplyState: "sent", UpdatedAt: oldAt}}},
	}
	checkpointJSON := mustMarshalJSON(oldCheckpoint)
	failedStep := string(stepRepair)
	lastCompleted := string(stepPrepareWorktree)
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_old_progress", LoopID: loop.ID, Status: "failed", CurrentStep: &failedStep, LastCompletedStep: &lastCompleted, CheckpointJSON: &checkpointJSON, StartedAt: oldAt, CreatedAt: oldAt, UpdatedAt: oldAt}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})

	resumed, err := runner.createRunContext(context.Background(), loop)
	if err != nil {
		t.Fatalf("createRunContext() error = %v", err)
	}
	resumed.Checkpoint.recordFailure(stepRepair, &loopError{message: "retry failed before progress", kind: FailureRetryableTransient})
	resumed.Checkpoint.refreshOutcomeProgress()
	if resumed.Checkpoint.Outcome == nil || resumed.Checkpoint.Outcome.PartialSuccess || resumed.Checkpoint.Outcome.Progress != (FixerDurableProgress{}) {
		t.Fatalf("retry Outcome = %#v, want predecessor progress excluded", resumed.Checkpoint.Outcome)
	}
}

func TestFixerOutcomeReportsNonRetryableFailureAsNonRetryable(t *testing.T) {
	t.Parallel()

	checkpoint := fixerCheckpoint{}
	checkpoint.recordFailure(stepRepair, &loopError{message: "invalid repository state", kind: FailureNonRetryable})
	if checkpoint.Outcome == nil || checkpoint.Outcome.PrimaryFailure == nil || checkpoint.Outcome.PrimaryFailure.Retryable == nil || *checkpoint.Outcome.PrimaryFailure.Retryable {
		t.Fatalf("Outcome = %#v, want non-retryable operator contract", checkpoint.Outcome)
	}
}

func TestDeriveRunOutcomeDecoratesHistoricalFixerCheckpoint(t *testing.T) {
	t.Parallel()

	checkpointJSON := mustMarshalJSON(fixerCheckpoint{
		FixItems:         []FixItem{{ID: "c1", Type: "comment", ThreadID: "t1"}},
		ReconcileCommits: &checkpointReconcileCommits{NewCommitSHAs: []string{"commit-1"}},
		Push:             &checkpointPush{Pushed: true},
		ResolvedComments: &checkpointResolvedComments{Items: []checkpointResolvedComment{{Status: "resolved", ReplyState: "sent"}}},
	})
	run := storage.RunRecord{Status: "failed", CurrentStep: stringPtr(string(stepRecheck)), CheckpointJSON: &checkpointJSON, ErrorMessage: stringPtr("recheck failed")}

	outcome := DeriveRunOutcome(run)
	if outcome == nil || outcome.PrimaryFailure == nil || outcome.PrimaryFailure.Step != string(stepRecheck) || outcome.PrimaryFailure.Message != "recheck failed" {
		t.Fatalf("DeriveRunOutcome() = %#v, want historical primary failure", outcome)
	}
	if outcome.PrimaryFailure.Kind != "" || outcome.PrimaryFailure.Retryable != nil {
		t.Fatalf("historical PrimaryFailure = %#v, want unknown kind and retryability preserved", outcome.PrimaryFailure)
	}
	if !outcome.Progress.CommitProduced || !outcome.Progress.Pushed || outcome.Progress.RepliesSent != 1 || outcome.Progress.ThreadsResolved != 1 || !outcome.PartialSuccess {
		t.Fatalf("DeriveRunOutcome() = %#v, want derived durable progress", outcome)
	}
}

func TestDeriveRunOutcomeRejectsNonFixerCheckpoint(t *testing.T) {
	t.Parallel()

	checkpointJSON := `{"detail":{"title":"worker task"},"worktree":{"path":"/tmp/worker"}}`
	step := string(stepPrepareWorktree)
	run := storage.RunRecord{Status: "failed", CurrentStep: &step, CheckpointJSON: &checkpointJSON, ErrorMessage: stringPtr("worker failed")}

	if outcome := DeriveRunOutcome(run); outcome != nil {
		t.Fatalf("DeriveRunOutcome() = %#v, want shared worker checkpoint fields rejected", outcome)
	}
}

func TestDeriveRunOutcomeRecognizesEarlyAndOutcomeOnlyFixerCheckpoints(t *testing.T) {
	t.Parallel()

	earlyJSON := `{"resumePolicy":"replay_step"}`
	earlyStep := string(stepDiscoverPR)
	earlyMessage := "discovery failed"
	early := DeriveRunOutcome(storage.RunRecord{Status: "failed", CurrentStep: &earlyStep, CheckpointJSON: &earlyJSON, ErrorMessage: &earlyMessage})
	if early == nil || early.PrimaryFailure == nil || early.PrimaryFailure.Step != earlyStep {
		t.Fatalf("early DeriveRunOutcome() = %#v, want discover failure", early)
	}

	outcomeOnlyJSON := `{"outcome":{"progress":{"pushed":true}}}`
	outcomeOnly := DeriveRunOutcome(storage.RunRecord{Status: "success", CheckpointJSON: &outcomeOnlyJSON})
	if outcomeOnly == nil || !outcomeOnly.Progress.Pushed {
		t.Fatalf("outcome-only DeriveRunOutcome() = %#v, want persisted outcome", outcomeOnly)
	}
}

func TestRunResolveCommentsDeferredClearsStaleFollowupWithoutMutation(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	prNumber := int64(42)
	target := buildPullRequestTargetID(repo, prNumber)
	metadata := mustMarshalJSON(map[string]any{"pendingFixerRediscovery": map[string]any{"headSha": "old-head", "fixItemsStateHash": "old-state", "unresolvedThreadIds": []string{"t2"}}})
	loop := storage.LoopRecord{ID: "loop_deferred_clear", ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	thread := ReviewThread{ID: "t1", Comments: []ReviewThreadComment{{ID: "c1", Body: "please fix"}}}
	github := &fakeGitHubGateway{
		viewResponses: []PullRequestDetail{
			{Number: 42, State: "OPEN", HeadSHA: "new-head", HeadRefName: "feature/fix-42", BaseRefName: "main", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
			{Number: 42, State: "OPEN", HeadSHA: "new-head", HeadRefName: "feature/fix-42", BaseRefName: "main", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}},
		},
		threads: []ReviewThread{thread},
	}
	fixItems := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Summary: "please fix"}}
	checkpoint := fixerCheckpoint{
		FixItems:         fixItems,
		FixItemsHash:     hashFixItems(fixItems),
		Repair:           &checkpointRepair{CompletedAt: fixture.nowISO(), ReplyExplanations: []replyExplanationEntry{{FixItemID: "c1", ThreadID: "t1", Action: string(replyActionDeferred), Explanation: "Needs a later coordinated change."}}},
		Validation:       &ValidationResult{Passed: true, HeadSHA: "new-head"},
		Push:             &checkpointPush{Pushed: false, SkippedReason: "No new commits to push"},
		ReconcileCommits: &checkpointReconcileCommits{WorkingTreeClean: true},
		Outcome:          &FixerRunOutcome{FollowUpThreadIDs: []string{"t2"}},
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Logger: fixture.logger, Now: fixture.now})

	updated, err := runner.runResolveCommentsStep(context.Background(), stepInput{Project: storage.ProjectRecord{RepoPath: t.TempDir()}, Loop: loop, Repo: repo, PRNumber: prNumber, Checkpoint: checkpoint})
	if err != nil {
		t.Fatalf("runResolveCommentsStep() error = %v", err)
	}
	if len(github.replyCalls) != 0 || len(github.resolveCalls) != 0 {
		t.Fatalf("reply calls=%d resolve calls=%d, want deferred no-op", len(github.replyCalls), len(github.resolveCalls))
	}
	if updated.ResolvedComments == nil || len(updated.ResolvedComments.Items) != 1 || updated.ResolvedComments.Items[0].Status != "deferred" {
		t.Fatalf("ResolvedComments = %#v, want deferred state", updated.ResolvedComments)
	}
	if updated.Outcome == nil || !sameStringSlices(updated.Outcome.FollowUpThreadIDs, []string{"t1"}) {
		t.Fatalf("Outcome = %#v, want deferred t1 as current follow-up", updated.Outcome)
	}
	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || persisted == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", persisted, err)
	}
	persistedMetadata := parseJSONObject(persisted.MetadataJSON)
	if _, ok := parsePendingFixerRediscoveryState(persistedMetadata); ok {
		t.Fatalf("loop metadata = %s, want deferred thread excluded from immediate rediscovery", derefString(persisted.MetadataJSON))
	}
	followup, ok := parseFixerFollowupState(persistedMetadata)
	if !ok || followup.Terminal || followup.AttemptsForFingerprint != 1 || !sameStringSlices(followup.UnresolvedThreadIDs, []string{"t1"}) || parseRFC3339OrZero(followup.NextEligibleAt).IsZero() {
		t.Fatalf("loop metadata = %s, want bounded deferred follow-up", derefString(persisted.MetadataJSON))
	}
	rechecked, err := runner.runRecheckStep(context.Background(), stepInput{Project: storage.ProjectRecord{RepoPath: t.TempDir()}, Loop: *persisted, Repo: repo, PRNumber: prNumber, Checkpoint: updated})
	if err != nil || rechecked.ResumePolicy != loops.ResumePolicyAdvanceFromCheckpoint {
		t.Fatalf("runRecheckStep() = (%#v, %v), want deferred thread to remain follow-up without manual intervention", rechecked, err)
	}
	if scheduled, err := runner.schedulePendingRediscoveryAfterRun(context.Background(), *persisted, repo, prNumber); err != nil || scheduled {
		t.Fatalf("schedulePendingRediscoveryAfterRun() = (%t, %v), want no immediate deferred rerun", scheduled, err)
	}
	if scheduled, err := runner.scheduleFollowupRetryAfterSuccess(context.Background(), *persisted, repo, prNumber, true); err != nil || !scheduled {
		t.Fatalf("scheduleFollowupRetryAfterSuccess() = (%t, %v), want delayed bounded rerun", scheduled, err)
	}
	queued, err := fixture.repos.Queue.FindActiveByLoopID(context.Background(), loop.ID)
	if err != nil || queued == nil || !parseRFC3339OrZero(queued.AvailableAt).After(fixture.now()) {
		t.Fatalf("queued follow-up = (%#v, %v), want future availability", queued, err)
	}
}

func TestExtractCompletionMarkerPayloadReadsCodexJSONL(t *testing.T) {
	t.Parallel()

	// Codex --json embeds the completion marker inside an agent_message event
	// rather than on a raw stdout line. The raw stdout scan must translate the
	// JSONL stream and recover the structured outcome payload.
	message := `__LOOPER_RESULT__={"outcome":"completed","summary":"applied the fix"}`
	event := map[string]any{
		"type": "item.completed",
		"item": map[string]any{"type": "agent_message", "text": message},
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	payload := extractCompletionMarkerPayload(string(encoded))
	if payload == "" {
		t.Fatalf("extractCompletionMarkerPayload() = %q, want decoded JSONL payload", payload)
	}
	blocked, _, _, outcomeErr := fixerRepairTaskOutcome(AgentResult{Status: "completed", Stdout: string(encoded)})
	if outcomeErr != nil || blocked {
		t.Fatalf("fixerRepairTaskOutcome() = (%t, %v), want completed outcome from JSONL marker", blocked, outcomeErr)
	}
}

func TestDeriveRunOutcomeBackfillsPrimaryFailureForEmptyInterruptedOutcome(t *testing.T) {
	t.Parallel()

	// An orphaned pre-start run is completed as interrupted without
	// recordFailure, so completeRun persists a non-nil but empty Outcome.
	emptyOutcome := mustMarshalJSON(fixerCheckpoint{Outcome: &FixerRunOutcome{}, ResumePolicy: loops.ResumePolicyReplayStep})
	step := string(stepDiscoverPR)
	run := storage.RunRecord{Status: "interrupted", CurrentStep: &step, CheckpointJSON: &emptyOutcome, ErrorMessage: stringPtr("Interrupted orphaned fixer run before start")}

	outcome := DeriveRunOutcome(run)
	if outcome == nil || outcome.PrimaryFailure == nil || outcome.PrimaryFailure.Step != step || outcome.PrimaryFailure.Message != "Interrupted orphaned fixer run before start" {
		t.Fatalf("DeriveRunOutcome() = %#v, want backfilled primary failure for empty interrupted outcome", outcome)
	}
	if outcome.PartialSuccess {
		t.Fatalf("DeriveRunOutcome() = %#v, want no partial success for orphaned run", outcome)
	}
}

func TestNewFollowupFixItemsDetectsNewFailingCheckAndConflict(t *testing.T) {
	t.Parallel()

	snapshot := []FixItem{
		{Type: "comment", ID: "c1", ThreadID: "t1"},
		{Type: "check", Name: "ci"},
	}
	live := []FixItem{
		{Type: "comment", ID: "c1", ThreadID: "t1"},
		{Type: "check", Name: "ci"},
		{Type: "check", Name: "lint"},
		{Type: "conflict"},
	}
	if !hasNewFollowupFixItems(snapshot, live) {
		t.Fatalf("hasNewFollowupFixItems() = false, want true for new lint check and conflict")
	}
	// A live snapshot with no new items returns false.
	if hasNewFollowupFixItems(snapshot, snapshot) {
		t.Fatalf("hasNewFollowupFixItems() = true, want false for matching snapshots")
	}
}

func TestCreateRunContextPreservesFollowUpThreadIDsForRecheckRetry(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	prNumber := int64(42)
	target := buildPullRequestTargetID(repo, prNumber)
	loop := storage.LoopRecord{ID: "loop_recheck_retry_followup", ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	oldAt := "2026-04-11T11:59:00.000Z"
	// A no-commit repair run handed off a newly observed thread (t2) as
	// follow-up work, then failed at recheck with a transient error. The
	// retry resumes directly at recheck, so resolve-comments never recomputes
	// FollowUpThreadIDs; they must survive the retry's Outcome reset.
	predecessor := fixerCheckpoint{
		ResumePolicy:     loops.ResumePolicyAdvanceFromCheckpoint,
		FixItems:         []FixItem{{ID: "c1", Type: "comment", ThreadID: "t1"}},
		FixItemsHash:     hashFixItems([]FixItem{{ID: "c1", Type: "comment", ThreadID: "t1"}}),
		Worktree:         &checkpointWorktree{Path: filepath.Join(t.TempDir(), "worktree"), Branch: "feature/fix-42", PreparedAt: oldAt},
		Repair:           &checkpointRepair{CompletedAt: oldAt, ParseStatus: "parsed"},
		ReconcileCommits: &checkpointReconcileCommits{WorkingTreeClean: true, CompletedAt: oldAt},
		Push:             &checkpointPush{Pushed: false, SkippedReason: "No new commits to push", PushedAt: oldAt},
		ResolvedComments: &checkpointResolvedComments{Items: []checkpointResolvedComment{{FixItemID: "c1", ThreadID: "t1", Status: "deferred", UpdatedAt: oldAt}}},
		Outcome:          &FixerRunOutcome{FollowUpThreadIDs: []string{"t2"}},
	}
	checkpointJSON := mustMarshalJSON(predecessor)
	failedStep := string(stepRecheck)
	lastCompleted := string(stepResolveComments)
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_recheck_failed", LoopID: loop.ID, Status: "failed", CurrentStep: &failedStep, LastCompletedStep: &lastCompleted, CheckpointJSON: &checkpointJSON, StartedAt: oldAt, CreatedAt: oldAt, UpdatedAt: oldAt}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})

	resumed, err := runner.createRunContext(context.Background(), loop)
	if err != nil {
		t.Fatalf("createRunContext() error = %v", err)
	}
	if resumed.StartStep != stepRecheck {
		t.Fatalf("StartStep = %q, want recheck retry", resumed.StartStep)
	}
	if resumed.Checkpoint.Outcome == nil || !sameStringSlices(resumed.Checkpoint.Outcome.FollowUpThreadIDs, []string{"t2"}) {
		t.Fatalf("retry Outcome = %#v, want preserved follow-up thread t2", resumed.Checkpoint.Outcome)
	}
	// Inherited durable progress must still be reset for the retry.
	resumed.Checkpoint.recordFailure(stepRecheck, &loopError{message: "retry failed before progress", kind: FailureRetryableTransient})
	resumed.Checkpoint.refreshOutcomeProgress()
	if resumed.Checkpoint.Outcome.PartialSuccess || resumed.Checkpoint.Outcome.Progress != (FixerDurableProgress{}) {
		t.Fatalf("retry Outcome = %#v, want inherited progress excluded", resumed.Checkpoint.Outcome)
	}
	if !sameStringSlices(resumed.Checkpoint.Outcome.FollowUpThreadIDs, []string{"t2"}) {
		t.Fatalf("retry Outcome = %#v, want follow-up thread preserved after failure recording", resumed.Checkpoint.Outcome)
	}

	// The preserved handoff must keep recheck from blocking on the new thread
	// and pausing the loop before rediscovery can enqueue.
	liveDetail := PullRequestDetail{
		Number:      prNumber,
		State:       "OPEN",
		HeadSHA:     "new-head",
		HeadRefName: "feature/fix-42",
		BaseRefName: "main",
		Comments:    []map[string]any{{"id": "c2", "threadId": "t2", "body": "newly failing thread"}},
	}
	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{liveDetail}}
	recheckRunner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Logger: fixture.logger, Now: fixture.now})
	recheckCheckpoint := resumed.Checkpoint
	recheckCheckpoint.RunStartedAt = fixture.nowISO()
	rechecked, err := recheckRunner.runRecheckStep(context.Background(), stepInput{Project: storage.ProjectRecord{RepoPath: t.TempDir()}, Loop: loop, Repo: repo, PRNumber: prNumber, Checkpoint: recheckCheckpoint})
	if err != nil || rechecked.ResumePolicy != loops.ResumePolicyAdvanceFromCheckpoint {
		t.Fatalf("runRecheckStep() = (%#v, %v), want follow-up thread excluded from no-fix gate", rechecked, err)
	}
}

func TestDeriveRunOutcomeSuppressesRunningRun(t *testing.T) {
	t.Parallel()

	// A run that is still executing has no terminal result. Deriving an empty
	// outcome would label it "Completed" in the dashboard while it is active.
	checkpointJSON := mustMarshalJSON(fixerCheckpoint{
		FixItems:         []FixItem{{ID: "c1", Type: "comment", ThreadID: "t1"}},
		ReconcileCommits: &checkpointReconcileCommits{NewCommitSHAs: []string{"commit-1"}},
		Push:             &checkpointPush{Pushed: true},
	})
	step := string(stepRepair)
	run := storage.RunRecord{Status: "running", CurrentStep: &step, CheckpointJSON: &checkpointJSON}

	if outcome := DeriveRunOutcome(run); outcome != nil {
		t.Fatalf("DeriveRunOutcome(running) = %#v, want nil until the run terminates", outcome)
	}

	// A terminal run with the same checkpoint still derives its outcome.
	terminal := run
	terminal.Status = "success"
	if outcome := DeriveRunOutcome(terminal); outcome == nil || !outcome.Progress.Pushed {
		t.Fatalf("DeriveRunOutcome(success) = %#v, want derived durable progress", outcome)
	}
}
