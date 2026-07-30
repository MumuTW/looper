package fixer

import (
	"context"
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
