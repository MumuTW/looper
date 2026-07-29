package fixer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/fixer/failurepolicy"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/loops/failureclass"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/validation"
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
		return ValidationResult{Passed: false, Summary: "Validation timed out: sleep 5", Output: "Command timed out", FailureCategory: validation.FailureSupervisorTimeout}, nil
	}})
	result, err := runner.runValidation(context.Background(), ValidationInput{CWD: t.TempDir(), Commands: runner.validationCommands})
	if err != nil {
		t.Fatalf("runValidation() error = %v", err)
	}
	if result.Passed || !strings.Contains(strings.ToLower(result.Output), "timed out") {
		t.Fatalf("runValidation() result = %#v, want bounded timeout failure", result)
	}
	failure := failurepolicy.ClassifyValidation(result.FailureCategory, result.Summary)
	if failure.Kind != failureclass.RetryableTransient {
		t.Fatalf("ClassifyValidation() = %#v, want retryable timeout", failure)
	}
}

func TestClassifyFixerValidationFailureParksDeterministicFailures(t *testing.T) {
	t.Parallel()

	failure := failurepolicy.ClassifyValidation(validation.FailureNonZeroExit, "go test failed")
	if failure.Kind != failureclass.ManualIntervention || failure.ResumePolicy != loops.ResumePolicyManualIntervention {
		t.Fatalf("ClassifyValidation() = %#v, want manual intervention", failure)
	}
}

func TestClassifyFixerValidationFailureDoesNotInferTimeoutFromTestOutput(t *testing.T) {
	t.Parallel()

	failure := failurepolicy.ClassifyValidation(validation.FailureNonZeroExit, "TestTimeoutPolicy: head changed")
	if failure.Kind != failureclass.ManualIntervention || failure.ResumePolicy != loops.ResumePolicyManualIntervention {
		t.Fatalf("ClassifyValidation() = %#v, want deterministic failure parked", failure)
	}
}

func TestConfiguredGateKeepsFixerAgentFromPushing(t *testing.T) {
	t.Parallel()

	if fixerAgentMayPush(true, []string{"go test ./..."}) {
		t.Fatal("fixerAgentMayPush() = true with a configured gate")
	}
	if !fixerAgentMayPush(true, nil) {
		t.Fatal("fixerAgentMayPush() = false without a configured gate")
	}
}

func TestRunValidateStepPreservesFailedOutput(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	runner := New(Options{ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
		return ValidationResult{Passed: false, Summary: "go test failed", Output: "expected 2, got 3"}, nil
	}})
	checkpoint, err := runner.runValidateStep(context.Background(), stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()},
		Checkpoint: fixerCheckpoint{
			Worktree: &checkpointWorktree{Path: worktree, Branch: "feature/fix"},
		},
	})
	var loopErr *loopError
	if !errors.As(err, &loopErr) {
		t.Fatalf("runValidateStep() error = %v, want loopError", err)
	}
	if checkpoint.Validation == nil || checkpoint.Validation.Output != "expected 2, got 3" {
		t.Fatalf("checkpoint.Validation = %#v, want failed diagnostics preserved", checkpoint.Validation)
	}
}

func TestRunValidateStepParksValidationThatRepeatedlyDirtiesWorktree(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	validationCalls := 0
	git := &fakeGitGateway{inspectResults: []InspectHeadResult{
		{HeadSHA: "repair-head", HasUncommittedChanges: true},
		{HeadSHA: "repair-head", HasUncommittedChanges: true, ChangedFiles: []string{"generated.go"}},
		{HeadSHA: "validation-head", NewCommitSHAs: []string{"validation-head"}},
		{HeadSHA: "validation-head", HasUncommittedChanges: true, ChangedFiles: []string{"generated.go"}},
	}}
	runner := New(Options{
		Git:             git,
		AllowAutoCommit: true,
		ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
			validationCalls++
			return ValidationResult{Passed: true, Summary: "Validation passed"}, nil
		},
	})
	checkpoint, err := runner.runValidateStep(context.Background(), stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()},
		Run:     storage.RunRecord{ID: "run_1"},
		Checkpoint: fixerCheckpoint{
			Worktree:         &checkpointWorktree{Path: worktree, Branch: "feature/fix", BaseHeadSHA: "base-head"},
			ReconcileCommits: &checkpointReconcileCommits{BaseHeadSHA: "base-head", FinalHeadSHA: "repair-head", WorkingTreeClean: true, CompletedAt: "2026-07-29T00:00:00Z"},
		},
	})
	var loopErr *loopError
	if !errors.As(err, &loopErr) || loopErr.kind != FailureManualIntervention {
		t.Fatalf("runValidateStep() error = %v, want manual intervention", err)
	}
	if checkpoint.ResumePolicy != "manual_intervention" || checkpoint.Pause == nil || checkpoint.Pause.Reason != string(checkpointPauseReasonDirtyWorktree) {
		t.Fatalf("checkpoint lifecycle = policy %q pause %#v, want dirty-worktree manual intervention", checkpoint.ResumePolicy, checkpoint.Pause)
	}
	if validationCalls != 2 || len(git.commitCalls) != 1 {
		t.Fatalf("validation calls=%d commit calls=%d, want 2/1", validationCalls, len(git.commitCalls))
	}
}

func TestRunValidateStepRefreshesReconcileMetadataWhenValidationCommitsCleanly(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	git := &fakeGitGateway{inspectResults: []InspectHeadResult{{
		HeadSHA: "validation-head", NewCommitSHAs: []string{"repair-head", "validation-head"}, ChangedFiles: []string{"generated.go"},
	}}}
	runner := New(Options{
		Git: git,
		ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
			return ValidationResult{Passed: true, Summary: "Validation passed"}, nil
		},
	})
	checkpoint, err := runner.runValidateStep(context.Background(), stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()},
		Checkpoint: fixerCheckpoint{
			Detail:           &checkpointDetail{BaseRefName: "main"},
			Worktree:         &checkpointWorktree{Path: worktree, Branch: "feature/fix", BaseHeadSHA: "base-head"},
			ReconcileCommits: &checkpointReconcileCommits{BaseHeadSHA: "base-head", FinalHeadSHA: "repair-head", NewCommitSHAs: []string{"repair-head"}, WorkingTreeClean: true, CompletedAt: "2026-07-29T00:00:00Z"},
		},
	})
	if err != nil {
		t.Fatalf("runValidateStep() error = %v", err)
	}
	if checkpoint.Validation == nil || checkpoint.Validation.HeadSHA != "validation-head" {
		t.Fatalf("Validation = %#v, want validation-head", checkpoint.Validation)
	}
	if checkpoint.ReconcileCommits == nil || checkpoint.ReconcileCommits.FinalHeadSHA != "validation-head" || !checkpoint.ReconcileCommits.WorkingTreeClean {
		t.Fatalf("ReconcileCommits = %#v, want refreshed clean validation head", checkpoint.ReconcileCommits)
	}
}

func TestRunValidateStepRefreshesReconcileMetadataAfterSecondValidation(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	git := &fakeGitGateway{
		inspectResults: []InspectHeadResult{
			{HeadSHA: "repair-head", HasUncommittedChanges: true},
			{HeadSHA: "repair-head", HasUncommittedChanges: true, ChangedFiles: []string{"generated.go"}},
			{HeadSHA: "commit-1", NewCommitSHAs: []string{"commit-1"}},
			{HeadSHA: "validation-head", NewCommitSHAs: []string{"commit-1", "validation-head"}, ChangedFiles: []string{"generated.go"}},
		},
	}
	validationCalls := 0
	runner := New(Options{
		Git: git, AllowAutoCommit: true,
		ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
			validationCalls++
			return ValidationResult{Passed: true, Summary: "Validation passed"}, nil
		},
	})
	checkpoint, err := runner.runValidateStep(context.Background(), stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()},
		Run:     storage.RunRecord{ID: "run_1"},
		Checkpoint: fixerCheckpoint{
			Detail:           &checkpointDetail{BaseRefName: "main"},
			Worktree:         &checkpointWorktree{Path: worktree, Branch: "feature/fix", BaseHeadSHA: "base-head"},
			ReconcileCommits: &checkpointReconcileCommits{BaseHeadSHA: "base-head", FinalHeadSHA: "repair-head", WorkingTreeClean: true},
		},
	})
	if err != nil {
		t.Fatalf("runValidateStep() error = %v", err)
	}
	if validationCalls != 2 {
		t.Fatalf("validation calls = %d, want 2", validationCalls)
	}
	if checkpoint.Validation == nil || checkpoint.Validation.HeadSHA != "validation-head" {
		t.Fatalf("Validation = %#v, want validation-head", checkpoint.Validation)
	}
	if checkpoint.ReconcileCommits == nil || checkpoint.ReconcileCommits.FinalHeadSHA != "validation-head" {
		t.Fatalf("ReconcileCommits = %#v, want final validation head", checkpoint.ReconcileCommits)
	}
}
