package planner

import (
	"errors"
	"testing"

	"github.com/MumuTW/looper/internal/loops/failureclass"
	"github.com/MumuTW/looper/internal/loops/runpipe"
)

func TestClassifyFailureDoesNotRetryUnknownExternalLookingMessage(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailure(errors.New("model provider request failed: broken pipe"))
	if got.Kind != runpipe.FailureNonRetryable {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.Kind, runpipe.FailureNonRetryable)
	}
}

func TestClassifyFailureRetriesBoundaryExternalTransport(t *testing.T) {
	runner := &Runner{}
	got := runner.classifyFailure(failureclass.WithBoundary(errors.New("model provider request failed: HTTP 503 Service Unavailable"), failureclass.BoundaryModelProvider))
	if got.Kind != runpipe.FailureRetryableTransient {
		t.Fatalf("classifyFailure() kind = %s, want %s", got.Kind, runpipe.FailureRetryableTransient)
	}
}
