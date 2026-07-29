package failurepolicy

import (
	"context"
	"errors"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/loops/failureclass"
)

func TestClassifyErrorRetriesContextCancellation(t *testing.T) {
	t.Parallel()

	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		d := ClassifyError(err, failureclass.BoundaryUnknown)
		if d.Kind != failureclass.RetryableTransient {
			t.Fatalf("ClassifyError(%v) kind = %s, want %s", err, d.Kind, failureclass.RetryableTransient)
		}
	}
}

func TestClassifyErrorDoesNotRetryUnknownExternalLookingMessage(t *testing.T) {
	t.Parallel()

	d := ClassifyError(errors.New("git push failed: connection reset by peer"), failureclass.BoundaryUnknown)
	if d.Kind != failureclass.NonRetryable {
		t.Fatalf("ClassifyError() kind = %s, want %s", d.Kind, failureclass.NonRetryable)
	}
}

func TestClassifyErrorRetriesBoundaryExternalTransport(t *testing.T) {
	t.Parallel()

	d := ClassifyError(failureclass.WithBoundary(errors.New("git push failed: connection reset by peer"), failureclass.BoundaryGitRemote), failureclass.BoundaryUnknown)
	if d.Kind != failureclass.RetryableTransient {
		t.Fatalf("ClassifyError() kind = %s, want %s", d.Kind, failureclass.RetryableTransient)
	}
}

func TestClassifyErrorRetriesInvalidProjectRepoPath(t *testing.T) {
	t.Parallel()

	d := ClassifyError(failureclass.WithBoundary(errors.New("git worktree list --porcelain: fatal: not a git repository (or any of the parent directories): .git"), failureclass.BoundaryGitRemote), failureclass.BoundaryUnknown)
	if d.Kind != failureclass.RetryableTransient {
		t.Fatalf("ClassifyError() kind = %s, want %s", d.Kind, failureclass.RetryableTransient)
	}
}

func TestClassifyErrorRetriesMissingProjectRepoDirectory(t *testing.T) {
	t.Parallel()

	d := ClassifyError(failureclass.WithBoundary(errors.New("start command: chdir /tmp/missing-repo: no such file or directory"), failureclass.BoundaryGitRemote), failureclass.BoundaryUnknown)
	if d.Kind != failureclass.RetryableTransient {
		t.Fatalf("ClassifyError() kind = %s, want %s", d.Kind, failureclass.RetryableTransient)
	}
}

func TestClassifyErrorRemoteHeadChanged(t *testing.T) {
	t.Parallel()

	d := ClassifyError(errors.New("remote head changed for feature/fix-42: expected a, got b"), failureclass.BoundaryGitRemote)
	if d.Kind != failureclass.RetryableAfterResume {
		t.Fatalf("ClassifyError() kind = %s, want %s", d.Kind, failureclass.RetryableAfterResume)
	}
}

func TestBoundaryForStepMapsPrepareWorktreeToGitRemote(t *testing.T) {
	t.Parallel()

	if got := BoundaryForStep("prepare-worktree"); got != failureclass.BoundaryGitRemote {
		t.Fatalf("BoundaryForStep(prepare-worktree) = %s, want %s", got, failureclass.BoundaryGitRemote)
	}
	if got := BoundaryForStep("repair"); got != failureclass.BoundaryModelProvider {
		t.Fatalf("BoundaryForStep(repair) = %s, want %s", got, failureclass.BoundaryModelProvider)
	}
	if got := BoundaryForStep("unknown-step"); got != failureclass.BoundaryUnknown {
		t.Fatalf("BoundaryForStep(unknown-step) = %s, want %s", got, failureclass.BoundaryUnknown)
	}
}

func TestClassifyValidationParksDeterministicFailures(t *testing.T) {
	t.Parallel()

	d := ClassifyValidation("go test failed", "assertion mismatch")
	if d.Kind != failureclass.ManualIntervention || d.ResumePolicy != loops.ResumePolicyManualIntervention {
		t.Fatalf("ClassifyValidation() = %#v, want manual intervention", d)
	}
}

func TestClassifyValidationDoesNotInferTimeoutFromTestOutput(t *testing.T) {
	t.Parallel()

	d := ClassifyValidation("go test failed", "--- FAIL: TestTimeoutPolicy")
	if d.Kind != failureclass.ManualIntervention || d.ResumePolicy != loops.ResumePolicyManualIntervention {
		t.Fatalf("ClassifyValidation() = %#v, want deterministic failure parked", d)
	}
}

func TestClassifyValidationRecognizesTimeoutSummary(t *testing.T) {
	t.Parallel()

	d := ClassifyValidation("Validation timed out: sleep 5", "Command timed out")
	if d.Kind != failureclass.RetryableTransient || d.ResumePolicy != loops.ResumePolicyReplayStep {
		t.Fatalf("ClassifyValidation() = %#v, want retryable timeout", d)
	}
}

func TestClassifyValidationRecognizesConnectionHints(t *testing.T) {
	t.Parallel()

	d := ClassifyValidation("command not found: foobar", "")
	if d.Kind != failureclass.RetryableTransient || d.ResumePolicy != loops.ResumePolicyReplayStep {
		t.Fatalf("ClassifyValidation() = %#v, want retryable transient", d)
	}
}

func TestClassifyValidationUsesDefaultMessage(t *testing.T) {
	t.Parallel()

	d := ClassifyValidation("   ", "")
	if d.Message != "Validation failed" {
		t.Fatalf("ClassifyValidation() message = %q, want %q", d.Message, "Validation failed")
	}
}
