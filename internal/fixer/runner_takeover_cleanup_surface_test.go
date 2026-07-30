package fixer

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

// Where the two halves of this subsystem meet. Main added the machinery to
// *surface* a refused cleanup on the run's outcome; this branch added a new
// reason to refuse one — the loop is held by `looper takeover`. A hold that
// returned early would be the one refusal an operator could not see on the run,
// which is the failure mode this whole change exists to stop: a control that
// does something other than what it reports.
//
// The refusal is also raised *before* the filesystem is touched, which is why
// the timestamp pair must stay empty. Main's three-state reading of those
// timestamps — neither set means cleanup never ran, attempted-without-cleaned
// means the outcome is unverified — only holds if a deliberate refusal declines
// to write an attempt it never made.

func seedHeldTerminalCleanupRun(t *testing.T, fixture *runnerFixture, runID, loopID, worktreePath string) fixerCheckpoint {
	t.Helper()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(43)
	target := "pr:acme/looper:43"
	if err := fixture.repos.Loops.UpsertChangingHumanHold(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 43, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &prNumber,
		Status: "human_takeover", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.UpsertChangingHumanHold() error = %v", err)
	}
	checkpoint := fixerCheckpoint{
		ResumePolicy: "advance_from_checkpoint",
		Worktree:     &checkpointWorktree{Path: worktreePath, Branch: "fix/pr-43", PreparedAt: nowISO},
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

func TestTakeoverRefusedCleanupSurfacesOnTheRunOutcome(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})
	checkpoint := seedHeldTerminalCleanupRun(t, fixture, "run_cleanup_held", "loop_cleanup_held", filepath.Join(t.TempDir(), "wt-43"))

	runner.cleanupFixerWorktreeIfTerminal(context.Background(), storage.ProjectRecord{
		ID: "project_1", RepoPath: t.TempDir(), BaseBranch: stringPtr("main"),
	}, "loop_cleanup_held", "run_cleanup_held", &checkpoint)

	// The hold itself: the checkout survives.
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("git.cleanupCalls = %#v, want none while the loop is human_takeover", git.cleanupCalls)
	}

	// In memory, for the returned ProcessResult.
	if checkpoint.Outcome == nil || len(checkpoint.Outcome.SecondaryIssues) != 1 {
		t.Fatalf("in-memory Outcome = %#v, want the hold recorded as one secondary issue", checkpoint.Outcome)
	}
	if !strings.Contains(checkpoint.Outcome.SecondaryIssues[0].Message, "held by human takeover") {
		t.Fatalf("issue = %#v, want the takeover named as the refusal cause", checkpoint.Outcome.SecondaryIssues[0])
	}

	// And durably, where an operator reads the run.
	run, err := fixture.repos.Runs.GetByID(context.Background(), "run_cleanup_held")
	if err != nil || run == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v)", run, err)
	}
	stored := parseCheckpoint(run.CheckpointJSON)
	if stored.Outcome == nil || len(stored.Outcome.SecondaryIssues) != 1 {
		t.Fatalf("stored Outcome = %#v, want the hold persisted on the run", stored.Outcome)
	}
	if !strings.Contains(stored.Outcome.SecondaryIssues[0].Message, "held by human takeover") {
		t.Fatalf("stored issue = %#v, want the takeover named", stored.Outcome.SecondaryIssues[0])
	}

	// Nothing was attempted, so the timestamps must not claim otherwise: an
	// attempted-without-cleaned pair means "removal started, outcome unknown",
	// which is exactly what a refusal-before-touching-disk is not.
	attempted, cleaned := storedCleanupTimestamps(t, fixture, "run_cleanup_held")
	if attempted != "" || cleaned != "" {
		t.Fatalf("stored cleanup timestamps = (%q, %q), want both empty for a refusal raised before the filesystem is touched", attempted, cleaned)
	}
}
