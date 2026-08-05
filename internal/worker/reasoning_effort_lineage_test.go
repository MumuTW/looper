package worker

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/loops/runpipe"
)

// This contract starts a real run so its snapshot is created by the normal
// runner path, then retries it with a different live effort and verifies the
// spawned execution still receives the persisted value. Unit tests of the
// snapshot/parser helpers alone would not catch a dropped runner-to-snapshot
// argument or a retry that silently re-resolves live config.
func TestReasoningEffortSnapshotSurvivesCreatedRunRetry(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	ctx := context.Background()
	loop, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", loop, err)
	}

	model := "gpt-5"
	createdEffort := config.ReasoningEffortHigh
	creator := New(Options{
		DB:                   fixture.coordinator.DB(),
		Repos:                fixture.repos,
		Now:                  fixture.now,
		AgentRuntime:         string(config.AgentVendorCodex),
		AgentModel:           &model,
		AgentProfileID:       "fast",
		AgentReasoningEffort: &createdEffort,
	})
	created, err := creator.createRunContext(ctx, *loop)
	if err != nil {
		t.Fatalf("createRunContext(initial) error = %v", err)
	}
	if created.Run.AgentSnapshotJSON == nil {
		t.Fatal("initial AgentSnapshotJSON = nil, want persisted identity")
	}
	initialSnapshot, err := config.ParseAgentSnapshot(*created.Run.AgentSnapshotJSON)
	if err != nil {
		t.Fatalf("ParseAgentSnapshot(initial) error = %v", err)
	}
	if initialSnapshot.ReasoningEffort == nil || *initialSnapshot.ReasoningEffort != createdEffort {
		t.Fatalf("initial snapshot reasoning effort = %v, want %q", initialSnapshot.ReasoningEffort, createdEffort)
	}

	worktreeRoot := t.TempDir()
	worktreePath := makeUsableTestWorktree(t, worktreeRoot, "retry")
	checkpointJSON := runpipe.MustMarshalJSON(workerCheckpoint{
		Work: &workerInput{
			Title:         "Retry worker",
			Repo:          "acme/looper",
			BaseBranch:    "main",
			ExecutionMode: "create-pr",
		},
		ClaimedLockKey: "issue:acme/looper:27",
		Worktree:       &checkpointWorktree{Path: worktreePath, Branch: "looper/retry", BaseBranch: "main"},
		Plan:           &checkpointPlan{Summary: "retry", Items: []string{"retry"}},
	})
	failed := created.Run
	failed.Status = "failed"
	failed.CurrentStep = runpipe.StringPtr(string(stepExecute))
	failed.LastCompletedStep = runpipe.StringPtr(string(stepPlan))
	failed.CheckpointJSON = &checkpointJSON
	failed.UpdatedAt = fixture.nowISO()
	if err := fixture.repos.Runs.Upsert(ctx, failed); err != nil {
		t.Fatalf("Runs.Upsert(failed) error = %v", err)
	}

	liveEffort := config.ReasoningEffortLow
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "failed", Summary: "retry requested"}}}
	retrier := New(Options{
		DB:                   fixture.coordinator.DB(),
		Repos:                fixture.repos,
		Git:                  &fakeGitGateway{},
		AgentExecutor:        agent,
		Now:                  fixture.now,
		AllowAutoCommit:      true,
		AgentRuntime:         string(config.AgentVendorCodex),
		AgentModel:           &model,
		AgentProfileID:       "fast",
		AgentReasoningEffort: &liveEffort,
	})
	resumed, err := retrier.createRunContext(ctx, *loop)
	if err != nil {
		t.Fatalf("createRunContext(retry) error = %v", err)
	}
	if !resumed.Resumed || resumed.StartStep != stepExecute {
		t.Fatalf("resumed context = %#v, want execute retry", resumed)
	}

	project, err := fixture.repos.Projects.GetByID(ctx, "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want project", project, err)
	}
	project.RepoPath = t.TempDir()
	project.MetadataJSON = runpipe.StringPtr(`{"worktreeRoot":"` + filepath.ToSlash(worktreeRoot) + `"}`)
	_, err = retrier.runExecuteStep(ctx, stepInput{Project: *project, Loop: *loop, Run: resumed.Run, Checkpoint: resumed.Checkpoint})
	if err == nil {
		t.Fatal("runExecuteStep() error = nil, want failed agent result")
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts = %d, want one retry spawn", len(agent.starts))
	}
	started := agent.starts[0]
	if !started.UseSnapshot {
		t.Fatal("retry spawn UseSnapshot = false, want persisted snapshot authority")
	}
	if started.SnapshotReasoningEffort == nil || *started.SnapshotReasoningEffort != createdEffort {
		t.Fatalf("retry SnapshotReasoningEffort = %v, want persisted %q", started.SnapshotReasoningEffort, createdEffort)
	}
}
