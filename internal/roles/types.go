// Package roles holds shared type definitions used across the four
// automation roles (fixer, reviewer, worker, planner). Keeping the
// cross-role vocabulary in one package removes the double ledger where
// each role independently redefines the same failure kinds.
package roles

// QueueFailureKind classifies how a queue item failed.
type QueueFailureKind string

const (
	// FailureRetryableTransient is a transient error that resolves on retry
	// without human intervention (network blip, lock contention, etc.).
	FailureRetryableTransient QueueFailureKind = "retryable_transient"

	// FailureRetryableAfterResume is a failure that resolves once the loop
	// resumes from where it left off (agent timeout, worktree conflict, etc.).
	FailureRetryableAfterResume QueueFailureKind = "retryable_after_resume"

	// FailureNonRetryable is a deterministic failure that will not succeed
	// on retry without a code or config change.
	FailureNonRetryable QueueFailureKind = "non_retryable"

	// FailureManualIntervention requires a human to inspect and resolve
	// before the item can be retried.
	FailureManualIntervention QueueFailureKind = "manual_intervention"
)
