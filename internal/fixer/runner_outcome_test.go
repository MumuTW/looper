package fixer

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

func TestFixerRepairTaskBlockedUsesStructuredAuthority(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		result      AgentResult
		blocked     bool
		failureKind QueueFailureKind
	}{
		{
			name: "successful prose is not a blocking signal",
			result: AgentResult{
				Summary: "Could not reproduce the GitHub failure, but fixed the null handling",
				Stdout:  `__LOOPER_RESULT__={"outcome":"completed","summary":"Could not reproduce the GitHub failure, but fixed the null handling"}`,
			},
		},
		{
			name:   "unstructured auth prose is diagnostic only",
			result: AgentResult{Summary: "GitHub auth failed before editing"},
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
			blocked, _, kind := fixerRepairTaskBlocked(tt.result)
			if blocked != tt.blocked || kind != tt.failureKind {
				t.Fatalf("fixerRepairTaskBlocked() = (%t, %q), want (%t, %q)", blocked, kind, tt.blocked, tt.failureKind)
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
	if !outcome.Progress.CommitProduced || !outcome.Progress.Pushed || outcome.Progress.RepliesSent != 1 || outcome.Progress.ThreadsResolved != 1 || !outcome.PartialSuccess {
		t.Fatalf("DeriveRunOutcome() = %#v, want derived durable progress", outcome)
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

func TestRecordFailedRunCleanupPersistsSecondaryIssue(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{cleanupErr: errors.New("worktree remove failed")}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})
	checkpoint := fixerCheckpoint{
		RunStartedRunID: "run_cleanup_secondary",
		Worktree: &checkpointWorktree{
			Path:       filepath.Join(t.TempDir(), "worktree"),
			Branch:     "feature/fix-42",
			PreparedAt: fixture.nowISO(),
		},
	}
	checkpoint.recordFailure(stepRepair, &loopError{message: "repair failed", kind: FailureRetryableTransient})
	checkpointJSON := mustMarshalJSON(checkpoint)
	loop := storage.LoopRecord{ID: "loop_cleanup_secondary", ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	run := storage.RunRecord{ID: "run_cleanup_secondary", LoopID: "loop_cleanup_secondary", Status: "failed", CheckpointJSON: &checkpointJSON, StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Runs.Upsert(context.Background(), run); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	newerStep := string(stepRecheck)
	newerHeartbeat := "2026-04-11T12:00:01.000Z"
	run.Status = "interrupted"
	run.CurrentStep = &newerStep
	run.LastHeartbeatAt = &newerHeartbeat
	if err := fixture.repos.Runs.Upsert(context.Background(), run); err != nil {
		t.Fatalf("Runs.Upsert(newer state) error = %v", err)
	}

	runner.recordFailedRunCleanup(context.Background(), storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()}, run.ID, &checkpoint)

	if checkpoint.Outcome == nil || checkpoint.Outcome.PrimaryFailure == nil || checkpoint.Outcome.PrimaryFailure.Message != "repair failed" || len(checkpoint.Outcome.SecondaryIssues) != 1 || checkpoint.Outcome.SecondaryIssues[0].Step != string(stepCleanupWorktree) {
		t.Fatalf("checkpoint.Outcome = %#v, want preserved primary and cleanup secondary", checkpoint.Outcome)
	}
	persisted, err := fixture.repos.Runs.GetByID(context.Background(), run.ID)
	if err != nil || persisted == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v)", persisted, err)
	}
	durable := parseCheckpoint(persisted.CheckpointJSON)
	if durable.Outcome == nil || len(durable.Outcome.SecondaryIssues) != 1 {
		t.Fatalf("durable Outcome = %#v, want persisted cleanup issue", durable.Outcome)
	}
	if persisted.Status != "interrupted" || persisted.CurrentStep == nil || *persisted.CurrentStep != newerStep || persisted.LastHeartbeatAt == nil || *persisted.LastHeartbeatAt != newerHeartbeat {
		t.Fatalf("persisted run = %#v, want checkpoint-only update to preserve newer state", persisted)
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
		viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadSHA: "new-head", HeadRefName: "feature/fix-42", BaseRefName: "main", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}}},
		threads:       []ReviewThread{thread},
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
	if updated.Outcome == nil || len(updated.Outcome.FollowUpThreadIDs) != 0 {
		t.Fatalf("Outcome = %#v, want stale follow-up cleared", updated.Outcome)
	}
	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || persisted == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", persisted, err)
	}
	if _, ok := parsePendingFixerRediscoveryState(parseJSONObject(persisted.MetadataJSON)); ok {
		t.Fatalf("loop metadata = %s, want stale pending rediscovery cleared", derefString(persisted.MetadataJSON))
	}
}
