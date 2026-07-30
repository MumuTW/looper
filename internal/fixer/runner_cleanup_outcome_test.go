package fixer

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

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

	runner.recordTerminalCleanup(context.Background(), storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()}, run.ID, &checkpoint)

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

func TestRecordSuccessfulRunCleanupPersistsSecondaryIssue(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{cleanupErr: errors.New("worktree remove failed")}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})
	loop := storage.LoopRecord{ID: "loop_success_cleanup", ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", Status: "completed", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	checkpoint := fixerCheckpoint{
		RunStartedAt:     fixture.nowISO(),
		ReconcileCommits: &checkpointReconcileCommits{NewCommitSHAs: []string{"commit-1"}, CompletedAt: fixture.nowISO()},
		Worktree:         &checkpointWorktree{Path: filepath.Join(t.TempDir(), "worktree"), Branch: "feature/fix-42", PreparedAt: fixture.nowISO()},
	}
	checkpointJSON := mustMarshalJSON(checkpoint)
	run := storage.RunRecord{ID: "run_success_cleanup", LoopID: loop.ID, Status: "success", CheckpointJSON: &checkpointJSON, StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Runs.Upsert(context.Background(), run); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	runner.recordTerminalCleanup(context.Background(), storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()}, run.ID, &checkpoint)

	persisted, err := fixture.repos.Runs.GetByID(context.Background(), run.ID)
	if err != nil || persisted == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v)", persisted, err)
	}
	durable := parseCheckpoint(persisted.CheckpointJSON)
	if durable.Outcome == nil || durable.Outcome.PrimaryFailure != nil || len(durable.Outcome.SecondaryIssues) != 1 || !durable.Outcome.Progress.CommitProduced {
		t.Fatalf("durable Outcome = %#v, want successful progress plus cleanup issue", durable.Outcome)
	}
}
