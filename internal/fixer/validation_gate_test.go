package fixer

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/fixer/failurepolicy"
	"github.com/MumuTW/looper/internal/lifecycle"
	"github.com/MumuTW/looper/internal/loops"
	"github.com/MumuTW/looper/internal/loops/failureclass"
	"github.com/MumuTW/looper/internal/roles"
	"github.com/MumuTW/looper/internal/storage"
	"github.com/MumuTW/looper/internal/validation"
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

// TestFixerPromptNeverInstructsAPush replaces the old fixerAgentMayPush unit test.
// The agent used to be told to push whenever auto-push was on and no validation
// gate was configured, which let it publish a partial fix before the run's outcome
// existed. Looper is now the publishing boundary for every repair, so no
// configuration produces a push instruction.
func TestFixerPromptNeverInstructsAPush(t *testing.T) {
	t.Parallel()

	detail := &checkpointDetail{State: "OPEN", HeadSHA: "abc123", HeadRefName: "feature/fix-42", BaseRefName: "main"}
	prompt, _ := buildFixerPrompt("project_1", customInstructionConfig(nil), "acme/looper", 42,
		detail, []FixItem{{ID: "fix-1", Summary: "repair"}}, config.DefaultDisclosureConfig(), "codex", "gpt-5.5")

	if contains(prompt, "Commit and push") || contains(prompt, "push the repair changes") {
		t.Fatalf("prompt instructs a push:\n%s", prompt)
	}
	if !contains(prompt, "Do not push the branch or update remote pull request state") {
		t.Fatalf("prompt = %q, want the local-only instruction", prompt)
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
	if !errors.As(err, &loopErr) || loopErr.kind != roles.FailureManualIntervention {
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
			Lifecycle:        &lifecycle.State{Actions: lifecycle.Actions{Commit: lifecycle.ActionSourceNone}},
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
	if checkpoint.Lifecycle == nil || checkpoint.Lifecycle.Actions.Commit != lifecycle.ActionSourceAgent || !slices.Equal(checkpoint.Lifecycle.CommitSHAs, []string{"repair-head", "validation-head"}) {
		t.Fatalf("Lifecycle = %#v, want validation-created commits attributed to the agent", checkpoint.Lifecycle)
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
