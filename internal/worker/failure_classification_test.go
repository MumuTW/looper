package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/MumuTW/looper/internal/agent"
	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/loops/failureclass"
)

func TestClassifyFailureDoesNotRetryUnknownExternalLookingMessage(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailure(errors.New("git fetch origin failed: broken pipe"))
	if got.kind != FailureNonRetryable {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, FailureNonRetryable)
	}
}

func TestClassifyFailureRetriesBoundaryExternalTransport(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailure(failureclass.WithBoundary(errors.New("git fetch origin failed: broken pipe"), failureclass.BoundaryGitRemote))
	if got.kind != FailureRetryableTransient {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, FailureRetryableTransient)
	}
}

func TestClassifyFailurePreservesContextTransient(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailure(context.DeadlineExceeded)
	if got.kind != FailureRetryableTransient {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, FailureRetryableTransient)
	}
}

// Same fail-closed refusal as the fixer path: the worker's agent boundary must
// not read a vendor capability mismatch as a transient failure.
func TestClassifyFailureHoldsUnsupportedToolNetworkVendorForOperator(t *testing.T) {
	runner := &Runner{}
	_, err := agent.New(agent.ExecutorOptions{Config: agent.ExecutorConfig{Vendor: config.AgentVendorClaudeCode}}).
		Start(context.Background(), agent.RunInput{Prompt: "hello", WorkingDirectory: t.TempDir(), RestrictToolNetwork: true})
	if err == nil {
		t.Fatal("agent Start() error = nil, want fail-closed refusal")
	}
	got := runner.classifyFailureWithBoundary(err, workerFailureBoundaryForStep(stepExecute))
	if got.kind != FailureManualIntervention {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, FailureManualIntervention)
	}
}
