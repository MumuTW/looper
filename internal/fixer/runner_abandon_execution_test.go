package fixer

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/loops/runpipe"
	"github.com/MumuTW/looper/internal/storage"
)

// newAbandonFixture builds the minimal stepInput that reaches agent Start in
// runRepairStep: one fix item, a worktree checkpoint under the project's
// metadata-declared worktree root, and a persisted project to derive it from.
func newAbandonFixture(t *testing.T) (*runnerFixture, *Runner, *fakeAgentExecutor, stepInput) {
	t.Helper()
	fixture := newRunnerFixture(t)
	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v)", project, err)
	}
	worktreeRoot := t.TempDir()
	metadata := `{"worktreeRoot":` + quoteJSONString(worktreeRoot) + `}`
	project.MetadataJSON = &metadata

	git := &fakeGitGateway{}
	agentExec := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", ParseStatus: "parsed", Summary: "fixed"}}}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: &fakeGitHubGateway{}, Git: git, AgentExecutor: agentExec,
		AllowAutoCommit: true, AllowAutoPush: true,
		Logger: fixture.logger, Now: fixture.now,
	})
	detail := &checkpointDetail{State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1"}
	input := stepInput{
		Project: *project,
		Loop:    storage.LoopRecord{ID: "loop_abandon", ProjectID: "project_1", Type: "fixer", TargetType: "pull_request"},
		Run:     storage.RunRecord{ID: "run_abandon", LoopID: "loop_abandon", Status: "running"},
		Repo:    "acme/looper", PRNumber: 42,
		Checkpoint: fixerCheckpoint{
			Detail:   detail,
			FixItems: []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Author: "alice", Summary: "use fmt.Errorf"}},
			Worktree: &checkpointWorktree{Path: filepath.Join(worktreeRoot, "wt-42"), Branch: "feature/fix-42", HeadSHA: "head-1"},
		},
	}
	return fixture, runner, agentExec, input
}

// If the durable pending-execution checkpoint write fails after the agent has
// started, the run must not return while the agent keeps editing or pushing
// from the abandoned worktree: the execution is killed and drained before the
// retryable error surfaces.
func TestRunRepairStepDrainsAgentWhenPersistFailsAfterStart(t *testing.T) {
	_, runner, agentExec, input := newAbandonFixture(t)
	runner.persistCheckpoint = func(_ context.Context, _ string, _ FixerStep, checkpoint fixerCheckpoint) error {
		// The repair step persists once more before Start; only the
		// pending-execution write that must follow a successful Start fails.
		if strings.TrimSpace(checkpoint.PendingAgentExecutionID) == "" {
			return nil
		}
		return errors.New("checkpoint store unavailable")
	}

	_, err := runner.runRepairStep(context.Background(), input)
	if err == nil {
		t.Fatal("runRepairStep() succeeded with the checkpoint store failing, want a retryable error")
	}
	loopErr, ok := err.(*runpipe.LoopError)
	if !ok || loopErr.Kind != runpipe.FailureRetryableAfterResume {
		t.Fatalf("runRepairStep() error = %#v, want FailureRetryableAfterResume", err)
	}
	if len(agentExec.starts) != 1 {
		t.Fatalf("agent starts = %d, want 1 before the persist failure", len(agentExec.starts))
	}
	execution := agentExec.lastExecution
	if execution == nil {
		t.Fatal("agent execution was never created")
	}
	if len(execution.kills) != 1 || !contains(execution.kills[0], "persist failed after agent start") {
		t.Fatalf("execution kills = %#v, want one kill naming the persist failure", execution.kills)
	}
	if execution.waits == 0 {
		t.Fatal("execution was never drained after the kill; the agent would outlive the run")
	}
}

// The agent-started notification failure sits between Start and Wait too, so
// the same drain must apply before its error returns.
func TestRunRepairStepDrainsAgentWhenNotificationFailsAfterStart(t *testing.T) {
	_, runner, agentExec, input := newAbandonFixture(t)
	runner.onAgentExecutionStarted = func(context.Context, AgentExecutionStartedInput) error {
		return errors.New("notification gateway unavailable")
	}

	_, err := runner.runRepairStep(context.Background(), input)
	if err == nil || !contains(err.Error(), "notification gateway unavailable") {
		t.Fatalf("runRepairStep() error = %v, want the notification failure surfaced", err)
	}
	execution := agentExec.lastExecution
	if execution == nil {
		t.Fatal("agent execution was never created")
	}
	if len(execution.kills) != 1 {
		t.Fatalf("execution kills = %#v, want one kill for the notification failure", execution.kills)
	}
	if execution.waits == 0 {
		t.Fatal("execution was never drained after the kill")
	}
}

func quoteJSONString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
