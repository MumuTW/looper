// Package workflow holds the fixer runner's workflow and state-transition
// authority: the ordered step pipeline and the pure decision for where a new
// run starts relative to its predecessor. It has no I/O and no dependency on
// storage or checkpoint types — the fixer package owns fetching and
// persisting state, and calls into this package to decide what to do with
// it. It mirrors internal/reviewer/workflow, the first extraction under the
// decomposition tracked by issue #120.
package workflow

import "slices"

// Step identifies a stage in the fixer run pipeline.
type Step string

const (
	StepDiscoverPR       Step = "discover-pr"
	StepClaimPR          Step = "claim-pr"
	StepCollectFixes     Step = "collect-fixes"
	StepPrepareWorktree  Step = "prepare-worktree"
	StepRepair           Step = "repair"
	StepReconcileCommits Step = "reconcile-commits"
	StepValidate         Step = "validate"
	StepPush             Step = "push"
	StepResolveComments  Step = "resolve-comments"
	StepRecheck          Step = "recheck"
)

var sequence = []Step{
	StepDiscoverPR,
	StepClaimPR,
	StepCollectFixes,
	StepPrepareWorktree,
	StepRepair,
	StepReconcileCommits,
	StepValidate,
	StepPush,
	StepResolveComments,
	StepRecheck,
}

// Sequence returns the full ordered fixer step pipeline. The result is a
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

// Previous returns the step before step, or "" if step is the first step or
// is not recognized.
func Previous(step Step) Step {
	for i, candidate := range sequence {
		if candidate == step && i > 0 {
			return sequence[i-1]
		}
	}
	return ""
}

// Parse returns the Step named by value, or "" if value names no step in
// the pipeline.
func Parse(value string) Step {
	for _, candidate := range sequence {
		if string(candidate) == value {
			return candidate
		}
	}
	return ""
}

// ResumeMode names the branch DecideResume took, so the caller can apply the
// matching checkpoint transformation without re-deriving the precedence.
type ResumeMode string

const (
	// ResumeModeFresh starts at discover with no mid-pipeline continuation:
	// either there is no failed/interrupted predecessor, or the predecessor
	// left nothing to continue from (no advance target and no restart or
	// prepare-rewind signal).
	ResumeModeFresh ResumeMode = "fresh"
	// ResumeModeRestartDiscover restarts at discover because the failure
	// invalidated the claimed PR or its preparation.
	ResumeModeRestartDiscover ResumeMode = "restart_discover"
	// ResumeModeResumePrepare rewinds to prepare-worktree to retry
	// preparation against the already-claimed PR.
	ResumeModeResumePrepare ResumeMode = "resume_prepare"
	// ResumeModeAdvance continues at the step after the predecessor's last
	// completed step.
	ResumeModeAdvance ResumeMode = "advance"
)

// ResumeDecision is where a new run starts relative to its predecessor.
type ResumeDecision struct {
	Mode      ResumeMode
	StartStep Step
	// Resumed reports a mid-pipeline continuation: the predecessor failed or
	// was interrupted and the new run starts past discover.
	Resumed bool
	// StickyAgentSnapshot reports that the predecessor failed or was
	// interrupted, so the new run inherits its agent snapshot even when it
	// restarts from discover.
	StickyAgentSnapshot bool
	// LastCompletedStep is the value the new run should record as already
	// completed, or "" for none.
	LastCompletedStep Step
}

// DecideResume decides where a run starts given its predecessor's terminal
// status and the already-classified restart signals. Precedence: a discover
// restart beats a prepare rewind beats advancing past the last completed
// step. Only a "failed" or "interrupted" predecessor can be continued; any
// other status (including none) starts fresh.
func DecideResume(status string, lastCompleted Step, restartFromDiscover, resumeFromPrepare bool) ResumeDecision {
	if status != "failed" && status != "interrupted" {
		return ResumeDecision{Mode: ResumeModeFresh, StartStep: StepDiscoverPR}
	}
	decision := ResumeDecision{Mode: ResumeModeFresh, StartStep: StepDiscoverPR, StickyAgentSnapshot: true}
	switch {
	case restartFromDiscover:
		decision.Mode = ResumeModeRestartDiscover
	case resumeFromPrepare:
		decision.Mode = ResumeModeResumePrepare
		decision.StartStep = StepPrepareWorktree
		decision.Resumed = true
		decision.LastCompletedStep = Previous(StepPrepareWorktree)
	case lastCompleted != "" && Next(lastCompleted) != "":
		decision.Mode = ResumeModeAdvance
		decision.StartStep = Next(lastCompleted)
		decision.Resumed = true
		decision.LastCompletedStep = lastCompleted
	}
	return decision
}
