// Package workflow holds the reviewer runner's workflow and state-transition
// authority: the ordered step pipeline and the pure decision logic for how a
// run resumes after a failure or interruption. It has no I/O and no
// dependency on storage or repo types — the reviewer package owns fetching
// and persisting state, and calls into this package to decide what to do
// with it.
package workflow

import (
	"slices"
	"strings"

	"github.com/MumuTW/looper/internal/loops/policy"
)

// Step identifies a stage in the reviewer run pipeline.
type Step string

const (
	StepDiscover         Step = "discover"
	StepFilter           Step = "filter"
	StepClaim            Step = "claim"
	StepSnapshot         Step = "snapshot"
	StepWorktree         Step = "worktree"
	StepThreadResolution Step = "thread_resolution"
	StepReview           Step = "review"
	StepPublish          Step = "publish"
)

// ResumePolicyRerunReview is the reviewer-specific resume policy that
// restarts a run at StepReview (or StepDiscover, for non-manual loops)
// because the previously published review can no longer be trusted. It has
// no equivalent in internal/loops, which only knows about the shared
// advance/replay/restart/manual-intervention policies used by every loop
// type.
const ResumePolicyRerunReview = "rerun_review"

var sequence = []Step{
	StepDiscover,
	StepFilter,
	StepClaim,
	StepSnapshot,
	StepWorktree,
	StepThreadResolution,
	StepReview,
	StepPublish,
}

// Sequence returns the full ordered reviewer step pipeline. The result is a
// copy: the package-level order is the workflow authority and callers must
// not be able to reorder it.
func Sequence() []Step {
	return slices.Clone(sequence)
}

// From returns the step sequence starting at start. An unrecognized (or
// empty) start step returns the full sequence — start index defaults to 0
// when no match is found, matching the historical stepsFrom behavior. Like
// Sequence, the result is a copy rather than a view into the backing array.
func From(start Step) []Step {
	startIndex := 0
	for i, step := range sequence {
		if step == start {
			startIndex = i
			break
		}
	}
	return slices.Clone(sequence[startIndex:])
}

// Next returns the step after step, or "" if step is the last step or is
// not recognized.
func Next(step Step) Step {
	for i, candidate := range sequence {
		if candidate == step && i+1 < len(sequence) {
			return sequence[i+1]
		}
	}
	return ""
}

// ShouldRestartFromDiscover reports whether a failed or interrupted run
// should restart from the discover step based on the step it failed on and
// its failure summary, independent of any resume-policy annotation on the
// checkpoint.
//
// This is a different (and reviewer-specific) test from
// policy.ShouldRestartFromDiscover, which only consults an explicit
// resume-policy value already recorded on the checkpoint. This variant
// additionally recognizes specific late-step preflight-failure messages
// (head or review-request drift detected right before publish, or PR change
// during thread reconciliation) that warrant rediscovery even when no
// resume policy was ever recorded.
func ShouldRestartFromDiscover(status string, failedStep Step, failureSummary string) bool {
	if status != "failed" && status != "interrupted" {
		return false
	}
	if failedStep != StepPublish && failedStep != StepReview && failedStep != StepThreadResolution {
		return false
	}
	return strings.Contains(failureSummary, "PR head changed before publish") ||
		strings.Contains(failureSummary, "PR head changed while reviewer was running") ||
		strings.Contains(failureSummary, "review request removed before publish") ||
		strings.Contains(failureSummary, "PR changed during thread reconciliation")
}

// ResumeInput carries the primitives PlanResume needs to decide how a
// reviewer run resumes. It has no I/O of its own; NeedsEligibilityRediscovery
// is a closure so the reviewer can consult its (checkpoint-dependent)
// eligibility check without this package needing to know about checkpoint
// shape.
type ResumeInput struct {
	// HasLatestRun is false when this loop has never run before.
	HasLatestRun bool
	// LatestStatus is the latest run's Status ("failed", "interrupted",
	// "completed", ...). Only meaningful when HasLatestRun is true.
	LatestStatus string
	// LastCompletedStep is the latest run's last completed step, or "" if
	// none completed.
	LastCompletedStep Step
	// FailedStep is the latest run's current step at the time it failed or
	// was interrupted, or "" if unknown.
	FailedStep Step
	// CheckpointResumePolicy is the resume policy recorded on the latest
	// run's checkpoint.
	CheckpointResumePolicy string
	// FailureSummary is the latest run's failure summary or error message,
	// used by ShouldRestartFromDiscover.
	FailureSummary string
	// ManualLoop is true for reviewer loops created for a specific PR by a
	// human rather than through autonomous discovery.
	ManualLoop bool
	// NeedsEligibilityRediscovery reports whether PR eligibility must be
	// rediscovered before the run can resume at startStep — true means the
	// checkpoint lacks the discovery detail that resuming there would
	// require, so PlanResume forces the run back to StepDiscover. May be
	// nil, in which case the override never fires (treated as false).
	NeedsEligibilityRediscovery func(startStep Step) bool
}

// ResumePlan is PlanResume's decision: where to start, and how to seed the
// new run's checkpoint and metadata. The reviewer package applies this
// against its actual checkpoint/run objects — this package never sees them.
type ResumePlan struct {
	// StartStep is the step the new run should begin at.
	StartStep Step
	// Resumed is true when this run continues a failed or interrupted
	// predecessor rather than starting fresh at discover.
	Resumed bool
	// StickySnapshot is true for any continuation of a failed/interrupted
	// predecessor, including a restart back to discover — it governs agent
	// snapshot reuse, not step placement.
	StickySnapshot bool
	// CarryCheckpoint is true when the new run's initial checkpoint should
	// be seeded from the prior run's checkpoint (with InitialResumePolicy
	// applied). False means start from a fresh checkpoint carrying only
	// InitialResumePolicy.
	CarryCheckpoint bool
	// InitialResumePolicy is the resume policy the new run's initial
	// checkpoint should carry.
	InitialResumePolicy string
	// ClearWorktreePreparedAt is true when the plan resumes at StepReview
	// and, if the carried checkpoint has a worktree, its PreparedAt marker
	// should be cleared so the review step re-validates preparation.
	ClearWorktreePreparedAt bool
	// CarryLastCompletedStep is true when the new run record should carry
	// forward the prior run's LastCompletedStep.
	CarryLastCompletedStep bool
}

// PlanResume decides where and how a reviewer run resumes, given the
// outcome of its immediately preceding run (if any). It performs no I/O.
func PlanResume(in ResumeInput) ResumePlan {
	restartFromDiscover := false
	rerunReview := false
	if in.HasLatestRun {
		restartFromDiscover = in.CheckpointResumePolicy == policy.ResumePolicyRestartFromDiscover ||
			ShouldRestartFromDiscover(in.LatestStatus, in.FailedStep, in.FailureSummary)
		rerunReview = in.CheckpointResumePolicy == ResumePolicyRerunReview
	}
	failedOrInterrupted := in.HasLatestRun && (in.LatestStatus == "failed" || in.LatestStatus == "interrupted")
	startStep := StepDiscover
	if failedOrInterrupted {
		switch {
		case restartFromDiscover:
			startStep = StepDiscover
		case rerunReview && !in.ManualLoop:
			startStep = StepDiscover
		case rerunReview:
			startStep = StepReview
		case in.LastCompletedStep != "":
			if next := Next(in.LastCompletedStep); next != "" {
				startStep = next
			}
		}
	}
	if startStep != StepDiscover && !in.ManualLoop && in.NeedsEligibilityRediscovery != nil && in.NeedsEligibilityRediscovery(startStep) {
		startStep = StepDiscover
	}
	// Every path that sets restartFromDiscover also lands startStep on
	// StepDiscover, so a restart is never a resume: resumed and "restarting
	// from discover" are mutually exclusive by construction.
	resumed := failedOrInterrupted && startStep != StepDiscover
	// stickySnapshot: any continuation of a failed/interrupted predecessor,
	// including first-step retries.
	stickySnapshot := failedOrInterrupted

	plan := ResumePlan{
		StartStep:           startStep,
		Resumed:             resumed,
		StickySnapshot:      stickySnapshot,
		InitialResumePolicy: policy.ResumePolicyReplayStep,
	}
	if resumed {
		plan.CarryCheckpoint = true
		plan.InitialResumePolicy = policy.ResumePolicyAdvanceFromCheckpoint
		plan.ClearWorktreePreparedAt = startStep == StepReview
		plan.CarryLastCompletedStep = in.LastCompletedStep != ""
		// Fixer-owner invalidation for resume-past-worktree is deferred
		// until ProcessClaimedItem successfully reacquires the PR lock.
		// This package cannot do that check — it has no I/O — so the
		// reviewer applies it itself after PlanResume returns.
	}
	return plan
}

// NextResumePolicyOnFailure computes the resume policy a step failure
// should leave on the checkpoint being persisted, given the failure's
// classified kind and the resume policy already recorded on that checkpoint
// (current).
//
// This is deliberately not a call to policy.NormalizeResumePolicy. That
// helper returns any non-empty existing policy unchanged and only derives one
// from failureKind when current is empty, so it can never move a checkpoint
// off the policy it already carries. The reviewer needs the opposite in two
// cases, which is the whole reason this function exists:
//
//   - retryable_after_resume must overwrite a soft existing policy (for
//     example replay_step) with advance_from_checkpoint, while treating
//     restart_from_discover and rerun_review as sticky so a transient retry
//     cannot downgrade a decision to rediscover or to re-review.
//   - manual_intervention must overwrite every existing policy, because a
//     hard hold outranks whatever the previous step recorded.
//
// Only the default branch matches policy.NormalizeResumePolicy's
// preserve-existing behavior. internal/loops is shared with the fixer and
// worker loops and must not gain these reviewer-only overwrites, so the
// reviewer semantics live here instead.
func NextResumePolicyOnFailure(failureKind, current string) string {
	switch failureKind {
	case policy.FailureKindRetryableAfterResume:
		if current != policy.ResumePolicyRestartFromDiscover && current != ResumePolicyRerunReview {
			return policy.ResumePolicyAdvanceFromCheckpoint
		}
		return current
	case policy.FailureKindManualIntervention:
		return policy.ResumePolicyManualIntervention
	default:
		if current == "" {
			return policy.ResumePolicyReplayStep
		}
		return current
	}
}

// PreferInMemoryCheckpoint reports whether ProcessClaimedItem should prefer
// the in-memory checkpoint captured just before the failing step ran over
// the latest checkpoint read back from storage. A rerun_review policy or a
// pending-review marker-verification miss both mean the in-memory state is
// more current than what was last persisted.
func PreferInMemoryCheckpoint(currentResumePolicy string, pendingReviewMarkerMiss bool) bool {
	return currentResumePolicy == ResumePolicyRerunReview || pendingReviewMarkerMiss
}

// CarryRestartFromDiscover reports whether an explicit restart_from_discover
// resume policy already on the in-memory checkpoint should be carried onto
// the checkpoint chosen for persisting after a failure.
func CarryRestartFromDiscover(currentResumePolicy string) bool {
	return currentResumePolicy == policy.ResumePolicyRestartFromDiscover
}
