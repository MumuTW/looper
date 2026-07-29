package fixer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
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

func TestClassifyFixerValidationFailureParksDeterministicFailures(t *testing.T) {
	t.Parallel()

	failure := classifyFixerValidationFailure(ValidationResult{Passed: false, Summary: "go test failed", Output: "assertion mismatch"})
	if failure.kind != FailureManualIntervention || failure.resumePolicy != "manual_intervention" {
		t.Fatalf("classifyFixerValidationFailure() = %#v, want manual intervention", failure)
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
