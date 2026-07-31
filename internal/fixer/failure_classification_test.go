package fixer

import (
	"context"
	"errors"
	"testing"

	"github.com/MumuTW/looper/internal/agent"
	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/fixer/failurepolicy"
	"github.com/MumuTW/looper/internal/loops/failureclass"
)

func TestClassifyFailureRetriesContextCancellation(t *testing.T) {
	runner := &Runner{}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		got := runner.classifyFailure(err)
		if got.kind != FailureRetryableTransient {
			t.Fatalf("classifyFailure(%v) kind = %s, want %s", err, got.kind, FailureRetryableTransient)
		}
	}
}

func TestClassifyFailureDoesNotRetryUnknownExternalLookingMessage(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailure(errors.New("git push failed: connection reset by peer"))
	if got.kind != FailureNonRetryable {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, FailureNonRetryable)
	}
}

func TestClassifyFailureRetriesBoundaryExternalTransport(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailure(failureclass.WithBoundary(errors.New("git push failed: connection reset by peer"), failureclass.BoundaryGitRemote))
	if got.kind != FailureRetryableTransient {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, FailureRetryableTransient)
	}
}

func TestClassifyFailureRetriesInvalidProjectRepoPath(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailureWithBoundary(errors.New("git worktree list --porcelain: fatal: not a git repository (or any of the parent directories): .git"), failureclass.BoundaryGitRemote)
	if got.kind != FailureRetryableTransient {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, FailureRetryableTransient)
	}
}

func TestClassifyFailureRetriesMissingProjectRepoDirectory(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailureWithBoundary(errors.New("start command: chdir /tmp/missing-repo: no such file or directory"), failureclass.BoundaryGitRemote)
	if got.kind != FailureRetryableTransient {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, FailureRetryableTransient)
	}
}

func TestValidateCompletedRepairCheckpointAcceptsParsedResults(t *testing.T) {
	t.Parallel()

	repair := &checkpointRepair{ParseStatus: "parsed", Summary: "gh: could not connect to api.github.com"}
	if err := validateCompletedRepairCheckpoint(repair, nil); err != nil {
		t.Fatalf("validateCompletedRepairCheckpoint() = %v, want nil for a parsed result", err)
	}
}

// A vendor that cannot deny tool network access is a static configuration
// mismatch: without this the repair step's model-provider boundary reads it as
// transient and burns every retry attempt into the failure-streak breaker.
func TestClassifyFailureHoldsUnsupportedToolNetworkVendorForOperator(t *testing.T) {
	runner := &Runner{}
	_, err := agent.New(agent.ExecutorOptions{Config: agent.ExecutorConfig{Vendor: config.AgentVendorClaudeCode}}).
		Start(context.Background(), agent.RunInput{Prompt: "hello", WorkingDirectory: t.TempDir(), RestrictToolNetwork: true})
	if err == nil {
		t.Fatal("agent Start() error = nil, want fail-closed refusal")
	}
	got := runner.classifyFailureWithBoundary(err, failurepolicy.BoundaryForStep(string(stepRepair)))
	if got.kind != FailureManualIntervention {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, FailureManualIntervention)
	}
}
