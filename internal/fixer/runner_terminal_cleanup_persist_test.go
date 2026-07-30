package fixer

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// Terminal worktree cleanup runs after completeRun has already written the run,
// and its timestamps are the only record of what happened: CleanupAttemptedAt with
// no CleanedAt is a cleanup that failed. These tests pin that the timestamps reach
// the database, and that the narrow write does not revert a concurrent transition.

func seedTerminalCleanupRun(t *testing.T, fixture *runnerFixture, runID, worktreePath string) fixerCheckpoint {
	t.Helper()
	nowISO := fixture.nowISO()
	loopID := "loop_" + runID
	repo := "acme/looper"
	prNumber := int64(42)
	target := "pr:acme/looper:42"
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 1, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &prNumber,
		Status: "completed", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	checkpoint := fixerCheckpoint{
		ResumePolicy: "advance_from_checkpoint",
		Worktree:     &checkpointWorktree{Path: worktreePath, Branch: "feature/fix-42", PreparedAt: nowISO},
	}
	encoded := mustMarshalJSON(checkpoint)
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID: runID, LoopID: loopID, Status: "completed",
		CurrentStep: stringPtr(string(stepRecheck)), LastCompletedStep: stringPtr(string(stepRecheck)),
		CheckpointJSON: &encoded, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	return checkpoint
}

func storedCleanupTimestamps(t *testing.T, fixture *runnerFixture, runID string) (attempted, cleaned string) {
	t.Helper()
	run, err := fixture.repos.Runs.GetByID(context.Background(), runID)
	if err != nil || run == nil {
		t.Fatalf("Runs.GetByID(%s) = (%#v, %v)", runID, run, err)
	}
	stored := parseCheckpoint(run.CheckpointJSON)
	if stored.Worktree == nil {
		t.Fatalf("stored checkpoint has no worktree: %#v", stored)
	}
	return stored.Worktree.CleanupAttemptedAt, stored.Worktree.CleanedAt
}

type observingCleanupGitGateway struct {
	*fakeGitGateway
	beforeCleanup func()
}

func (g *observingCleanupGitGateway) CleanupWorktree(ctx context.Context, input CleanupWorktreeInput) error {
	if g.beforeCleanup != nil {
		g.beforeCleanup()
	}
	return g.fakeGitGateway.CleanupWorktree(ctx, input)
}

func TestTerminalCleanupPersistsSuccessTimestamps(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})
	checkpoint := seedTerminalCleanupRun(t, fixture, "run_cleanup_ok", filepath.Join(t.TempDir(), "wt-42"))

	runner.cleanupFixerWorktreeIfTerminal(context.Background(), storage.ProjectRecord{
		ID: "project_1", RepoPath: t.TempDir(), BaseBranch: stringPtr("main"),
	}, "", "run_cleanup_ok", &checkpoint)

	if len(git.cleanupCalls) != 1 {
		t.Fatalf("len(git.cleanupCalls) = %d, want 1", len(git.cleanupCalls))
	}
	attempted, cleaned := storedCleanupTimestamps(t, fixture, "run_cleanup_ok")
	if attempted == "" {
		t.Fatal("stored CleanupAttemptedAt is empty, want the attempt recorded durably")
	}
	if cleaned == "" {
		t.Fatal("stored CleanedAt is empty, want a successful cleanup recorded durably")
	}
}

func TestTerminalCleanupPersistsAttemptBeforeWorktreeMutation(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &observingCleanupGitGateway{fakeGitGateway: &fakeGitGateway{}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})
	checkpoint := seedTerminalCleanupRun(t, fixture, "run_cleanup_attempt_boundary", filepath.Join(t.TempDir(), "wt-boundary"))

	git.beforeCleanup = func() {
		attempted, cleaned := storedCleanupTimestamps(t, fixture, "run_cleanup_attempt_boundary")
		if attempted == "" {
			t.Fatal("stored CleanupAttemptedAt is empty when CleanupWorktree starts")
		}
		if cleaned != "" {
			t.Fatalf("stored CleanedAt = %q before CleanupWorktree completes, want empty", cleaned)
		}
	}
	runner.cleanupFixerWorktreeIfTerminal(context.Background(), storage.ProjectRecord{
		ID: "project_1", RepoPath: t.TempDir(), BaseBranch: stringPtr("main"),
	}, "", "run_cleanup_attempt_boundary", &checkpoint)

	if len(git.cleanupCalls) != 1 {
		t.Fatalf("len(git.cleanupCalls) = %d, want 1", len(git.cleanupCalls))
	}
}

func TestTerminalCleanupRecordsRefusedRemovalAsUnverified(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{cleanupErr: errors.New("worktree is dirty")}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})
	checkpoint := seedTerminalCleanupRun(t, fixture, "run_cleanup_failed", filepath.Join(t.TempDir(), "wt-43"))

	runner.cleanupFixerWorktreeIfTerminal(context.Background(), storage.ProjectRecord{
		ID: "project_1", RepoPath: t.TempDir(), BaseBranch: stringPtr("main"),
	}, "", "run_cleanup_failed", &checkpoint)

	attempted, cleaned := storedCleanupTimestamps(t, fixture, "run_cleanup_failed")
	if attempted == "" {
		t.Fatal("stored CleanupAttemptedAt is empty, want the unconfirmed attempt recorded durably")
	}
	if cleaned != "" {
		t.Fatalf("stored CleanedAt = %q, want empty so the outcome reads as unverified", cleaned)
	}
}

// TestTerminalCleanupPreservesConcurrentRunState is the assertion that
// matters most here, and the one the first version of this file missed. Replacing
// checkpoint_json wholesale leaves the scalar columns intact while still erasing a
// concurrent checkpoint transition — an operator retry rewriting resumePolicy to
// restart_from_discover between completion and cleanup — so the requeued run would
// resume the invalid downstream checkpoint it was retried to escape.
func TestTerminalCleanupPreservesConcurrentRunState(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})
	checkpoint := seedTerminalCleanupRun(t, fixture, "run_cleanup_policy_race", filepath.Join(t.TempDir(), "wt-46"))

	// Something else advances the run after the in-memory copy was captured: an
	// operator retry rewrites resumePolicy — MarkInvalidCompletionRunRestartFromDiscover
	// sets exactly this policy — and the run moves on to a new status and step.
	stored, err := fixture.repos.Runs.GetByID(context.Background(), "run_cleanup_policy_race")
	if err != nil || stored == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v)", stored, err)
	}
	concurrent := parseCheckpoint(stored.CheckpointJSON)
	concurrent.ResumePolicy = loops.ResumePolicyRestartFromDiscover
	rewritten := mustMarshalJSON(concurrent)
	updated := *stored
	updated.CheckpointJSON = &rewritten
	updated.Status = "interrupted"
	updated.CurrentStep = stringPtr(string(stepDiscoverPR))
	if err := fixture.repos.Runs.Upsert(context.Background(), updated); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	runner.cleanupFixerWorktreeIfTerminal(context.Background(), storage.ProjectRecord{
		ID: "project_1", RepoPath: t.TempDir(), BaseBranch: stringPtr("main"),
	}, "", "run_cleanup_policy_race", &checkpoint)

	after, err := fixture.repos.Runs.GetByID(context.Background(), "run_cleanup_policy_race")
	if err != nil || after == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v)", after, err)
	}
	final := parseCheckpoint(after.CheckpointJSON)
	if final.ResumePolicy != loops.ResumePolicyRestartFromDiscover {
		t.Fatalf("ResumePolicy = %q, want restart_from_discover preserved through cleanup", final.ResumePolicy)
	}
	if final.Worktree == nil || final.Worktree.CleanedAt == "" {
		t.Fatalf("stored worktree = %#v, want cleanup recorded alongside the retry rewrite", final.Worktree)
	}
	// The merge must not damage the rest of the worktree object either.
	if final.Worktree.Path == "" || final.Worktree.Branch == "" {
		t.Fatalf("stored worktree = %#v, want path and branch preserved by the field-level merge", final.Worktree)
	}
	// Scalar columns too: the stale in-memory record must not be pushed back.
	if after.Status != "interrupted" {
		t.Fatalf("run Status = %q, want interrupted preserved across cleanup", after.Status)
	}
	if got := derefString(after.CurrentStep); got != string(stepDiscoverPR) {
		t.Fatalf("run CurrentStep = %q, want %q preserved across cleanup", got, stepDiscoverPR)
	}
}

// TestTerminalCleanupWithoutRunIDStaysInMemory covers the paths where no durable
// run owns the cleanup: the timestamps still land on the in-memory checkpoint for
// the returned ProcessResult, and nothing is written.
func TestTerminalCleanupWithoutRunIDStaysInMemory(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})
	checkpoint := fixerCheckpoint{
		Worktree: &checkpointWorktree{Path: filepath.Join(t.TempDir(), "wt-45"), Branch: "feature/fix-45", PreparedAt: fixture.nowISO()},
	}

	runner.cleanupFixerWorktreeIfTerminal(context.Background(), storage.ProjectRecord{
		ID: "project_1", RepoPath: t.TempDir(), BaseBranch: stringPtr("main"),
	}, "", "", &checkpoint)

	if checkpoint.Worktree.CleanedAt == "" {
		t.Fatal("in-memory CleanedAt is empty, want the cleanup still reflected for the caller")
	}
}

// TestTerminalCleanupSurvivesLaterRetryPolicyWrite covers the opposite
// interleaving from TestTerminalCleanupPreservesConcurrentRunState: cleanup
// persists first, and an operator retry rewrites the resume policy afterwards.
// While the retry writers replaced the whole checkpoint, that second write pushed
// their earlier snapshot back and the completed cleanup vanished from durable
// state again. Both orders have to hold, since the two writers are unordered.
func TestTerminalCleanupSurvivesLaterRetryPolicyWrite(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})
	checkpoint := seedTerminalCleanupRun(t, fixture, "run_cleanup_then_retry", filepath.Join(t.TempDir(), "wt-47"))

	// The retry writer reads the run here, before cleanup records anything.
	before, err := fixture.repos.Runs.GetByID(context.Background(), "run_cleanup_then_retry")
	if err != nil || before == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v)", before, err)
	}

	runner.cleanupFixerWorktreeIfTerminal(context.Background(), storage.ProjectRecord{
		ID: "project_1", RepoPath: t.TempDir(), BaseBranch: stringPtr("main"),
	}, "", "run_cleanup_then_retry", &checkpoint)

	// ...and writes its policy change afterwards, from that stale read.
	if err := fixture.repos.Runs.MergeRunResumePolicy(context.Background(), before.ID, loops.ResumePolicyRestartFromDiscover, fixture.nowISO()); err != nil {
		t.Fatalf("MergeRunResumePolicy() error = %v", err)
	}

	after, err := fixture.repos.Runs.GetByID(context.Background(), "run_cleanup_then_retry")
	if err != nil || after == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v)", after, err)
	}
	final := parseCheckpoint(after.CheckpointJSON)
	if final.ResumePolicy != loops.ResumePolicyRestartFromDiscover {
		t.Fatalf("ResumePolicy = %q, want the retry rewrite applied", final.ResumePolicy)
	}
	if final.Worktree == nil || final.Worktree.CleanedAt == "" {
		t.Fatalf("stored worktree = %#v, want the earlier cleanup preserved under the later policy write", final.Worktree)
	}
}

// TestTerminalCleanupRecordsRefusedRemovalAsSecondaryIssue covers the durable half of
// a refused removal. The run has already completed and been written, so this is a
// secondary issue by construction -- it must not disturb the primary result, and it
// has to be visible where an operator reads the run rather than only in the event log.
func TestTerminalCleanupRecordsRefusedRemovalAsSecondaryIssue(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{cleanupErr: errors.New("worktree is dirty")}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})
	checkpoint := seedTerminalCleanupRun(t, fixture, "run_cleanup_issue", filepath.Join(t.TempDir(), "wt-50"))

	runner.cleanupFixerWorktreeIfTerminal(context.Background(), storage.ProjectRecord{
		ID: "project_1", RepoPath: t.TempDir(), BaseBranch: stringPtr("main"),
	}, "", "run_cleanup_issue", &checkpoint)

	// In memory, for the returned ProcessResult.
	if checkpoint.Outcome == nil || len(checkpoint.Outcome.SecondaryIssues) != 1 {
		t.Fatalf("in-memory Outcome = %#v, want one secondary issue", checkpoint.Outcome)
	}
	if !strings.Contains(checkpoint.Outcome.SecondaryIssues[0].Message, "worktree is dirty") {
		t.Fatalf("issue = %#v, want the refusal cause carried", checkpoint.Outcome.SecondaryIssues[0])
	}

	// And durably.
	run, err := fixture.repos.Runs.GetByID(context.Background(), "run_cleanup_issue")
	if err != nil || run == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v)", run, err)
	}
	stored := parseCheckpoint(run.CheckpointJSON)
	if stored.Outcome == nil || len(stored.Outcome.SecondaryIssues) != 1 {
		t.Fatalf("stored Outcome = %#v, want the issue persisted", stored.Outcome)
	}
	if stored.Outcome.SecondaryIssues[0].Retryable == nil || !*stored.Outcome.SecondaryIssues[0].Retryable {
		t.Fatalf("stored issue = %#v, want retryable", stored.Outcome.SecondaryIssues[0])
	}
	// The cleanup timestamps still read as unverified, and nothing claimed a primary
	// failure on a run whose own result stands.
	if _, cleaned := storedCleanupTimestamps(t, fixture, "run_cleanup_issue"); cleaned != "" {
		t.Fatalf("stored CleanedAt = %q, want empty after a refused removal", cleaned)
	}
	if stored.Outcome.PrimaryFailure != nil {
		t.Fatalf("stored PrimaryFailure = %#v, want none: cleanup must not become the run's primary result", stored.Outcome.PrimaryFailure)
	}
}

// TestTerminalCleanupAppendsToExistingSecondaryIssues pins that the durable write
// appends rather than replaces, since the run may already carry issues recorded by
// the failure path.
func TestTerminalCleanupAppendsToExistingSecondaryIssues(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{cleanupErr: errors.New("worktree is dirty")}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})
	checkpoint := seedTerminalCleanupRun(t, fixture, "run_cleanup_append", filepath.Join(t.TempDir(), "wt-51"))

	// A pre-existing outcome, as the failure path would have stored.
	existing := checkpoint
	existing.Outcome = &FixerRunOutcome{
		PrimaryFailure:  &FixerOutcomeFailure{Step: string(stepRepair), Message: "agent timed out"},
		SecondaryIssues: []FixerOutcomeFailure{{Step: string(stepPush), Message: "remote moved"}},
	}
	stored, err := fixture.repos.Runs.GetByID(context.Background(), "run_cleanup_append")
	if err != nil || stored == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v)", stored, err)
	}
	encoded := mustMarshalJSON(existing)
	updated := *stored
	updated.CheckpointJSON = &encoded
	if err := fixture.repos.Runs.Upsert(context.Background(), updated); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	runner.cleanupFixerWorktreeIfTerminal(context.Background(), storage.ProjectRecord{
		ID: "project_1", RepoPath: t.TempDir(), BaseBranch: stringPtr("main"),
	}, "", "run_cleanup_append", &checkpoint)

	after, err := fixture.repos.Runs.GetByID(context.Background(), "run_cleanup_append")
	if err != nil || after == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v)", after, err)
	}
	final := parseCheckpoint(after.CheckpointJSON)
	if final.Outcome == nil || len(final.Outcome.SecondaryIssues) != 2 {
		t.Fatalf("stored SecondaryIssues = %#v, want the cleanup issue appended to the existing one", final.Outcome)
	}
	if final.Outcome.PrimaryFailure == nil || final.Outcome.PrimaryFailure.Message != "agent timed out" {
		t.Fatalf("stored PrimaryFailure = %#v, want the causal failure untouched", final.Outcome.PrimaryFailure)
	}
}
