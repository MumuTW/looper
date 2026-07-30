// Package roles contains contracts shared by Looper's role runners.
package roles

// QueueFailureKind is the durable classification written for a failed queue
// claim. Role runners may add role-specific failure policy, but they must use
// this shared vocabulary when communicating retry intent to the scheduler.
type QueueFailureKind string

const (
	FailureRetryableTransient   QueueFailureKind = "retryable_transient"
	FailureRetryableAfterResume QueueFailureKind = "retryable_after_resume"
	FailureNonRetryable         QueueFailureKind = "non_retryable"
	FailureManualIntervention   QueueFailureKind = "manual_intervention"
)
