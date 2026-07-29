package worker

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestRunValidationRunsConfiguredCommands(t *testing.T) {
	t.Parallel()

	runner := New(Options{ValidationCommands: []string{"exit 0"}})
	result, err := runner.runValidation(context.Background(), ValidationInput{CWD: t.TempDir(), Commands: runner.validationCommands})
	if err != nil {
		t.Fatalf("runValidation() error = %v", err)
	}
	if !result.Passed {
		t.Fatalf("runValidation() result = %#v, want passed", result)
	}
}

func TestRunValidationBlocksOnNonZeroExit(t *testing.T) {
	t.Parallel()

	runner := New(Options{ValidationCommands: []string{"exit 0", "exit 3"}})
	result, err := runner.runValidation(context.Background(), ValidationInput{CWD: t.TempDir(), Commands: runner.validationCommands})
	if err != nil {
		t.Fatalf("runValidation() error = %v", err)
	}
	if result.Passed {
		t.Fatalf("runValidation() result = %#v, want blocked", result)
	}
	if !strings.Contains(result.Summary, "exit 3") {
		t.Fatalf("runValidation() summary = %q, want the failing command named", result.Summary)
	}
	failure := classifyValidationFailure(result)
	if failure.kind == "" {
		t.Fatalf("classifyValidationFailure() = %#v, want a failure kind", failure)
	}
}

func TestRunValidationWithoutCommandsIsANoOpPass(t *testing.T) {
	t.Parallel()

	runner := New(Options{})
	if len(runner.validationCommands) != 0 {
		t.Fatalf("New(Options{}).validationCommands = %#v, want empty", runner.validationCommands)
	}
	result, err := runner.runValidation(context.Background(), ValidationInput{CWD: t.TempDir(), Commands: runner.validationCommands})
	if err != nil {
		t.Fatalf("runValidation() error = %v", err)
	}
	if !result.Passed || result.Summary != "No validation commands configured" {
		t.Fatalf("runValidation() result = %#v, want the no-op pass", result)
	}
}

func TestRunValidationDoesNotInheritDaemonSecrets(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "daemon-secret")

	runner := New(Options{ValidationCommands: []string{`test -z "$OPENAI_API_KEY"`}})
	result, err := runner.runValidation(context.Background(), ValidationInput{CWD: t.TempDir(), Commands: runner.validationCommands})
	if err != nil {
		t.Fatalf("runValidation() error = %v", err)
	}
	if !result.Passed {
		t.Fatalf("runValidation() result = %#v, daemon secret leaked to command", result)
	}
}

func TestClassifyValidationFailureParksDeterministicFailures(t *testing.T) {
	t.Parallel()

	failure := classifyValidationFailure(ValidationResult{Passed: false, Summary: "go test failed", Output: "assertion mismatch"})
	if failure.kind != FailureManualIntervention || failure.resumePolicy != "manual_intervention" {
		t.Fatalf("classifyValidationFailure() = %#v, want manual intervention", failure)
	}
}

func TestProcessClaimedItemRevalidatesChangesCreatedByValidation(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{
		createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "base-head", WorktreeID: "worktree_1"},
		inspectResults: []InspectHeadResult{
			{HeadSHA: "agent-head"},
			{HeadSHA: "agent-head", HasUncommittedChanges: true},
			{HeadSHA: "validated-head"},
			{HeadSHA: "validated-head"},
			{HeadSHA: "validated-head"},
		},
	}
	validationCalls := 0
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: &fakeGitHubGateway{createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}},
		Git:    git, AgentExecutor: &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", Stdout: "ok", ParseStatus: "parsed"}}},
		Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true,
		OpenPRStrategy: config.OpenPRStrategyAllDone, ValidationCommands: []string{"generate && test"},
		ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
			validationCalls++
			return ValidationResult{Passed: true, Summary: "Validation passed"}, nil
		},
	})

	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" || result.PullRequestNumber != 101 {
		t.Fatalf("result = %#v, want published success", result)
	}
	if validationCalls != 2 {
		t.Fatalf("validation calls = %d, want initial and post-reconcile passes", validationCalls)
	}
	if len(git.commitCalls) != 1 || len(git.pushCalls) != 1 {
		t.Fatalf("commit calls=%d push calls=%d, want reconciled commit published once", len(git.commitCalls), len(git.pushCalls))
	}
}
