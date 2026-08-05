// Package runpipe holds the mechanical scaffolding shared by the planner,
// worker, fixer, and reviewer runners: queue failure classification, run
// record persistence, queue item failure handling, and (under #537) the
// StepRunner step-loop engine. Only the planner has migrated its
// ProcessClaimedItem loop onto StepRunner so far; worker, reviewer, and
// fixer still own their historical step-loop copies until their slices
// land. Each runner retains its own step execution logic and
// ProcessClaimedItem orchestration beyond what StepRunner owns; runpipe
// centralizes the persistence primitives and the shared ordering contract.
//
// Authority: the runner remains the authority for step execution and
// orchestration. runpipe is the authority for the mechanical shape of run
// record persistence (CurrentStep/LastCompletedStep/CheckpointJSON updates)
// and queue failure/retry accounting. The storage layer (SQLite) remains the
// truth authority for all persisted state.
//
// Trade-off (AGENTS.md "new concept" requirement):
//
//	Delete this six months from now — what breaks? Each runner would need to
//	re-duplicate persistStepStarted/persistStepCompleted/completeRun/failQueueItem
//	and the shared types (QueueFailureKind, ProcessResult, LoopError). Any bug
//	fix to the scaffolding would again require four copies. The cost of keeping
//	it in sync is low: the scaffolding is mechanical and has no branching on
//	runner-specific state.
//
//	What does it still not catch? StepRunner adoption is incremental: only
//	the planner uses it today, so worker/reviewer/fixer still carry their
//	own step-loop copies until migrated. Even after full adoption, runner-
//	specific failure dispatch (awaiting-human, hold-skip, label-mismatch)
//	stays in OnFailure hooks rather than a generic step-execution interface
//	that would abstract away real behavioral differences.
package runpipe

import (
	"time"

	"github.com/MumuTW/looper/internal/loops/failureclass"
)

// QueueFailureKind classifies a queue item failure for retry decisions.
// It is a duplicate of failureclass.Kind with different constant names;
// runners historically used the Failure* prefix. The values are identical
// and persisted as-is in queue_items.error_kind.
type QueueFailureKind string

const (
	FailureRetryableTransient   QueueFailureKind = "retryable_transient"
	FailureRetryableAfterResume QueueFailureKind = "retryable_after_resume"
	FailureNonRetryable         QueueFailureKind = "non_retryable"
	FailureManualIntervention   QueueFailureKind = "manual_intervention"
)

// MaxRetryDelay caps the backoff delay for queue retries across all runners.
const MaxRetryDelay = 300 * time.Second

// ProcessResult is the return value of ProcessClaimedItem across all runners.
// PullRequestNumber is only set by planner and worker; fixer and reviewer
// leave it zero.
type ProcessResult struct {
	LoopID            string
	RunID             string
	QueueItemID       string
	Status            string
	Summary           string
	FailureKind       QueueFailureKind
	PullRequestNumber int64
}

// LoopError carries a failure message and its classified QueueFailureKind.
// The Interrupted field is only used by the reviewer runner; other runners
// leave it false.
type LoopError struct {
	Message     string
	Kind        QueueFailureKind
	Interrupted bool
}

func (e *LoopError) Error() string { return e.Message }

// HoldSkipError signals that a queue item was held (not processed) and
// should be finished with a summary instead of running the step pipeline.
type HoldSkipError struct{ Summary string }

func (e *HoldSkipError) Error() string { return e.Summary }

// FailureKindFromClass maps a failureclass.Kind to the equivalent
// QueueFailureKind. This mapping was previously duplicated as
// plannerFailureKind, workerFailureKind, etc.
func FailureKindFromClass(kind failureclass.Kind) QueueFailureKind {
	switch kind {
	case failureclass.RetryableTransient:
		return FailureRetryableTransient
	case failureclass.RetryableAfterResume:
		return FailureRetryableAfterResume
	case failureclass.ManualIntervention:
		return FailureManualIntervention
	default:
		return FailureNonRetryable
	}
}
