package fixer

import (
	"context"
	"errors"
	"path/filepath"
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

func TestTerminalCleanupPersistsSuccessTimestamps(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})
	checkpoint := seedTerminalCleanupRun(t, fixture, "run_cleanup_ok", filepath.Join(t.TempDir(), "wt-42"))

	runner.cleanupFixerWorktreeIfTerminal(context.Background(), storage.ProjectRecord{
		ID: "project_1", RepoPath: t.TempDir(), BaseBranch: stringPtr("main"),
	}, "run_cleanup_ok", &checkpoint)

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

func TestTerminalCleanupPersistsFailureAsAttemptWithoutCleaned(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{cleanupErr: errors.New("worktree is dirty")}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})
	checkpoint := seedTerminalCleanupRun(t, fixture, "run_cleanup_failed", filepath.Join(t.TempDir(), "wt-43"))

	runner.cleanupFixerWorktreeIfTerminal(context.Background(), storage.ProjectRecord{
		ID: "project_1", RepoPath: t.TempDir(), BaseBranch: stringPtr("main"),
	}, "run_cleanup_failed", &checkpoint)

	attempted, cleaned := storedCleanupTimestamps(t, fixture, "run_cleanup_failed")
	if attempted == "" {
		t.Fatal("stored CleanupAttemptedAt is empty, want the failed attempt recorded durably")
	}
	if cleaned != "" {
		t.Fatalf("stored CleanedAt = %q, want empty so the failure stays distinguishable", cleaned)
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
	}, "run_cleanup_policy_race", &checkpoint)

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
	}, "", &checkpoint)

	if checkpoint.Worktree.CleanedAt == "" {
		t.Fatal("in-memory CleanedAt is empty, want the cleanup still reflected for the caller")
	}
}
