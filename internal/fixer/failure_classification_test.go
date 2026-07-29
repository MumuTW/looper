package fixer

import (
	"context"
	"errors"
	"testing"

	"github.com/nexu-io/looper/internal/loops/failureclass"
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

// The classification exists because an unreachable-GitHub run is retried
// forever under the default scheduler.retryMaxAttempts of -1. It reads the
// agent's transcript, which is the only place the evidence exists: looper
// never made the failing call, the agent did, inside its own sandbox.

func TestCheckpointRepairDetectsUnreachableGitHubAwayFromTheLastLine(t *testing.T) {
	t.Parallel()

	// Summary degrades to the last non-empty log line once the completion
	// marker fails to parse, so a connection failure the agent hit earlier is
	// absent from it. Scanning only the summary — as this did before the flag
	// existed — classified this run transient and retried it forever.
	result := AgentResult{
		Status:      "completed",
		ParseStatus: "missing",
		Stderr:      "gh: could not connect to api.github.com\n",
		Stdout:      "reading review threads\nwriting patch\ncleaning up worktree\n",
		Summary:     "cleaning up worktree",
	}

	repair := checkpointRepairFromAgentResult("exec-1", "abc123", result, "2026-07-29T00:00:00.000Z")
	if repair.GitHubUnreachable == nil || !*repair.GitHubUnreachable {
		t.Fatalf("GitHubUnreachable = %v, want recorded true", repair.GitHubUnreachable)
	}

	err := validateCompletedRepairCheckpoint(repair)
	var failure *loopError
	if !errors.As(err, &failure) {
		t.Fatalf("validateCompletedRepairCheckpoint() error = %v, want *loopError", err)
	}
	if failure.kind != FailureNonRetryable {
		t.Fatalf("kind = %s, want %s", failure.kind, FailureNonRetryable)
	}
}

func TestCheckpointRepairKeepsIncidentalNetworkWordsRetryable(t *testing.T) {
	t.Parallel()

	// Widening the search from one line to the whole transcript raises the cost
	// of loose patterns: an agent editing networking code prints these words
	// constantly, and a false positive parks a loop that deserved a retry.
	result := AgentResult{
		Status:      "completed",
		ParseStatus: "missing",
		Stdout:      "editing internal/net/network.go\nrunning TestNetworkUnreachableFallback\napi.github.com rate limit: 4998\n",
		Summary:     "api.github.com rate limit: 4998",
	}

	repair := checkpointRepairFromAgentResult("exec-1", "abc123", result, "2026-07-29T00:00:00.000Z")
	if repair.GitHubUnreachable == nil || *repair.GitHubUnreachable {
		t.Fatalf("GitHubUnreachable = %v, want recorded false", repair.GitHubUnreachable)
	}

	err := validateCompletedRepairCheckpoint(repair)
	var failure *loopError
	if !errors.As(err, &failure) {
		t.Fatalf("validateCompletedRepairCheckpoint() error = %v, want *loopError", err)
	}
	if failure.kind != FailureRetryableTransient {
		t.Fatalf("kind = %s, want %s", failure.kind, FailureRetryableTransient)
	}
}

func TestValidateCompletedRepairCheckpointFallsBackForPreFlagCheckpoints(t *testing.T) {
	t.Parallel()

	// A checkpoint persisted before the flag existed carries no transcript and
	// no decision. Scanning its one-line summary is lossy, but it is the whole
	// evidence such a record has, and in-flight loops must keep classifying.
	repair := &checkpointRepair{ParseStatus: "missing", Summary: "gh: could not connect to api.github.com"}
	if repair.GitHubUnreachable != nil {
		t.Fatal("fixture should leave GitHubUnreachable unset")
	}

	err := validateCompletedRepairCheckpoint(repair)
	var failure *loopError
	if !errors.As(err, &failure) {
		t.Fatalf("validateCompletedRepairCheckpoint() error = %v, want *loopError", err)
	}
	if failure.kind != FailureNonRetryable {
		t.Fatalf("kind = %s, want %s", failure.kind, FailureNonRetryable)
	}
}

func TestValidateCompletedRepairCheckpointTrustsTheRecordedDecision(t *testing.T) {
	t.Parallel()

	// The recorded decision wins over the summary text. Re-deriving would
	// reintroduce exactly the truncated-evidence guess the flag removes.
	recorded := false
	repair := &checkpointRepair{
		ParseStatus:       "missing",
		Summary:           "gh: could not connect to api.github.com",
		GitHubUnreachable: &recorded,
	}

	err := validateCompletedRepairCheckpoint(repair)
	var failure *loopError
	if !errors.As(err, &failure) {
		t.Fatalf("validateCompletedRepairCheckpoint() error = %v, want *loopError", err)
	}
	if failure.kind != FailureRetryableTransient {
		t.Fatalf("kind = %s, want %s", failure.kind, FailureRetryableTransient)
	}
}

func TestValidateCompletedRepairCheckpointAcceptsParsedResults(t *testing.T) {
	t.Parallel()

	repair := &checkpointRepair{ParseStatus: "parsed", Summary: "gh: could not connect to api.github.com"}
	if err := validateCompletedRepairCheckpoint(repair); err != nil {
		t.Fatalf("validateCompletedRepairCheckpoint() = %v, want nil for a parsed result", err)
	}
}
