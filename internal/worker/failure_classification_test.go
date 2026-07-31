package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/MumuTW/looper/internal/loops/failureclass"
	"github.com/MumuTW/looper/internal/roles"
)

func TestClassifyFailureDoesNotRetryUnknownExternalLookingMessage(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailure(errors.New("git fetch origin failed: broken pipe"))
	if got.kind != roles.FailureNonRetryable {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, roles.FailureNonRetryable)
	}
}

func TestClassifyFailureRetriesBoundaryExternalTransport(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailure(failureclass.WithBoundary(errors.New("git fetch origin failed: broken pipe"), failureclass.BoundaryGitRemote))
	if got.kind != roles.FailureRetryableTransient {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, roles.FailureRetryableTransient)
	}
}

func TestClassifyFailurePreservesContextTransient(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailure(context.DeadlineExceeded)
	if got.kind != roles.FailureRetryableTransient {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.kind, roles.FailureRetryableTransient)
	}
}
