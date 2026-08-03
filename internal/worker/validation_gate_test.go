package worker

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/lifecycle"
	"github.com/MumuTW/looper/internal/loops/runpipe"
)

func TestRunValidationRunsConfiguredCommands(t *testing.T) {
	t.Parallel()

	runner := New(Options{ValidationCommands: []string{"exit 0"}, ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
		return ValidationResult{Passed: true, Summary: "Validation passed"}, nil
	}})
	result, err := runner.runValidation(context.Background(), ValidationInput{CWD: t.TempDir(), Commands: runner.validationCommands})
	if err != nil {
		t.Fatalf("runValidation() error = %v", err)
	}
	if !result.Passed {
		t.Fatalf("runValidation() result = %#v, want passed", result)
	}
}

func TestValidatedAgentCreatedPRPublishesExactValidatedHead(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	worktreePath := filepath.Join(t.TempDir(), "wt")
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{WorktreePath: worktreePath, Branch: "looper/feature", BaseBranch: "main", HeadSHA: "base-head", WorktreeID: "worktree_1"},
		inspectResult: InspectHeadResult{HeadSHA: "validated-head", NewCommitSHAs: []string{"validated-head"}},
	}
	github := &fakeGitHubGateway{prDetail: PullRequestDetail{Number: 311, URL: "https://example/pr/311", State: "open", HeadRefName: "looper/feature", BaseRefName: "main", HeadSHA: "agent-pushed-head"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{
		Status: "completed", Summary: "done", Stdout: "ok", ParseStatus: "parsed",
		Lifecycle: &lifecycle.State{Branch: "looper/feature", BaseBranch: "main", CommitSHAs: []string{"agent-pushed-head"}, Pushed: true, PRNumber: 311, PRURL: "https://example/pr/311", Actions: lifecycle.Actions{Commit: lifecycle.ActionSourceAgent, Push: lifecycle.ActionSourceAgent, PR: lifecycle.ActionSourceAgent}},
	}}}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent,
		Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true,
		OpenPRStrategy: config.OpenPRStrategyAllDone, ValidationCommands: []string{"go test ./..."},
		ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
			return ValidationResult{Passed: true, Summary: "Validation passed"}, nil
		},
	})

	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker-1", "worker")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" || result.PullRequestNumber != 311 {
		t.Fatalf("result = %#v, want adopted PR success", result)
	}
	if len(git.pushCalls) != 1 {
		t.Fatalf("push calls = %d, want daemon correction push", len(git.pushCalls))
	}
	if got := git.pushCalls[0]; got.LocalHeadSHA != "validated-head" || got.ExpectedRemoteHeadSHA != "agent-pushed-head" || got.Branch != "looper/feature" {
		t.Fatalf("push = %#v, want validated-head to looper/feature with lease on agent-pushed-head", got)
	}
}

func TestRunValidationBlocksOnNonZeroExit(t *testing.T) {
	t.Parallel()

	runner := New(Options{ValidationCommands: []string{"exit 0", "exit 3"}, ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
		return ValidationResult{Passed: false, Summary: "Validation failed: exit 3"}, nil
	}})
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

	runner := New(Options{ValidationCommands: []string{`test -z "$OPENAI_API_KEY"`}, ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
		return ValidationResult{Passed: true, Summary: "Validation passed"}, nil
	}})
	result, err := runner.runValidation(context.Background(), ValidationInput{CWD: t.TempDir(), Commands: runner.validationCommands})
	if err != nil {
		t.Fatalf("runValidation() error = %v", err)
	}
	if !result.Passed {
		t.Fatalf("runValidation() result = %#v, daemon secret leaked to command", result)
	}
}

func TestRunValidationPreservesCancellation(t *testing.T) {
	t.Parallel()

	runner := New(Options{AgentTimeout: time.Second, ValidationCommands: []string{"sleep 5"}, ValidationRunner: func(ctx context.Context, _ ValidationInput) (ValidationResult, error) {
		return ValidationResult{}, ctx.Err()
	}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runner.runValidation(ctx, ValidationInput{CWD: t.TempDir(), Commands: runner.validationCommands})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runValidation() error = %v, want context.Canceled", err)
	}
}

func TestRunValidationBoundsCommandRuntime(t *testing.T) {
	t.Parallel()

	runner := New(Options{AgentTimeout: 20 * time.Millisecond, ValidationCommands: []string{"sleep 5"}, ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
		return ValidationResult{Passed: false, Summary: "Validation timed out: sleep 5", Output: "Command timed out"}, nil
	}})
	result, err := runner.runValidation(context.Background(), ValidationInput{CWD: t.TempDir(), Commands: runner.validationCommands})
	if err != nil {
		t.Fatalf("runValidation() error = %v", err)
	}
	if result.Passed || !strings.Contains(strings.ToLower(result.Output), "timed out") {
		t.Fatalf("runValidation() result = %#v, want bounded timeout failure", result)
	}
	failure := classifyValidationFailure(result)
	if failure.kind != runpipe.FailureRetryableTransient {
		t.Fatalf("classifyValidationFailure() = %#v, want retryable timeout", failure)
	}
}

func TestClassifyValidationFailureParksDeterministicFailures(t *testing.T) {
	t.Parallel()

	failure := classifyValidationFailure(ValidationResult{Passed: false, Summary: "go test failed", Output: "assertion mismatch"})
	if failure.kind != runpipe.FailureManualIntervention || failure.resumePolicy != "manual_intervention" {
		t.Fatalf("classifyValidationFailure() = %#v, want manual intervention", failure)
	}
}

func TestClassifyValidationFailureDoesNotInferTimeoutFromTestOutput(t *testing.T) {
	t.Parallel()

	failure := classifyValidationFailure(ValidationResult{Passed: false, Summary: "go test failed", Output: "--- FAIL: TestTimeoutPolicy"})
	if failure.kind != runpipe.FailureManualIntervention || failure.resumePolicy != "manual_intervention" {
		t.Fatalf("classifyValidationFailure() = %#v, want deterministic failure parked", failure)
	}
}

func TestProcessClaimedItemRevalidatesChangesCreatedByValidation(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "base-head", WorktreeID: "worktree_1"},
		inspectResult: InspectHeadResult{HeadSHA: "validated-head"},
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
	if git.pushCalls[0].LocalHeadSHA != "validated-head" {
		t.Fatalf("push local head = %q, want validated-head", git.pushCalls[0].LocalHeadSHA)
	}
}

func TestProcessClaimedItemRevalidatesCleanCommitObservedAfterValidation(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "base-head", WorktreeID: "worktree_1"},
		inspectResult: InspectHeadResult{HeadSHA: "late-head", NewCommitSHAs: []string{"agent-head", "late-head"}},
		inspectResults: []InspectHeadResult{
			{HeadSHA: "agent-head", NewCommitSHAs: []string{"agent-head"}},
			{HeadSHA: "agent-head", NewCommitSHAs: []string{"agent-head"}},
			{HeadSHA: "late-head", NewCommitSHAs: []string{"agent-head", "late-head"}},
			{HeadSHA: "late-head", NewCommitSHAs: []string{"agent-head", "late-head"}},
		},
	}
	validationCalls := 0
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: &fakeGitHubGateway{createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}},
		Git:    git, AgentExecutor: &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "done", Stdout: "ok", ParseStatus: "parsed"}}},
		Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true,
		OpenPRStrategy: config.OpenPRStrategyAllDone, ValidationCommands: []string{"go test ./..."},
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
		t.Fatalf("validation calls = %d, want initial and pre-publish passes", validationCalls)
	}
	if len(git.commitCalls) != 0 || len(git.pushCalls) != 1 {
		t.Fatalf("commit calls=%d push calls=%d, want clean late commit revalidated and pushed", len(git.commitCalls), len(git.pushCalls))
	}
	if git.pushCalls[0].LocalHeadSHA != "late-head" {
		t.Fatalf("push local head = %q, want late-head", git.pushCalls[0].LocalHeadSHA)
	}
}
