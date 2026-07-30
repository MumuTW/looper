// Package workflow holds the worker runner's workflow and state-transition
// authority: the ordered step pipeline and the pure decision for where a
// new run starts relative to its predecessor. It has no I/O and no
// dependency on storage or checkpoint types — the worker package owns
// fetching and persisting state, and calls into this package to decide
// what to do with it. Worker mirror of internal/fixer/workflow (#309),
// under the decomposition tracked by issue #120.
package workflow

import "slices"

// Step identifies a stage in the worker run pipeline.
type Step string

const (
	StepPrepareWork     Step = "prepare-work"
	StepPrepareWorktree Step = "prepare-worktree"
	StepPlan            Step = "plan"
	StepExecute         Step = "execute"
	StepValidate        Step = "validate"
	StepOpenPR          Step = "open-pr"
)

var sequence = []Step{
	StepPrepareWork,
	StepPrepareWorktree,
	StepPlan,
	StepExecute,
	StepValidate,
	StepOpenPR,
}

// Sequence returns the full ordered worker step pipeline. The result is a
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

// Previous returns the step before step, or "" if step is the first step
// or is not recognized.
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

// ResumeMode names the branch DecideResume took, so the caller can apply
// the matching checkpoint carry-over without re-deriving the precedence.
type ResumeMode string

const (
	// ResumeModeFresh starts at prepare-work carrying the predecessor's
	// checkpoint fields forward: there is no failed/interrupted
	// predecessor, it is held for manual resume, or it left nothing to
	// continue from.
	ResumeModeFresh ResumeMode = "fresh"
	// ResumeModeRestart starts at prepare-work with an EMPTY checkpoint
	// because the recorded resume policy demands rediscovery.
	ResumeModeRestart ResumeMode = "restart"
	// ResumeModeReplayExecute rewinds to the execute step with the
	// checkpoint rewound for an execute retry.
	ResumeModeReplayExecute ResumeMode = "replay_execute"
	// ResumeModeAdvance continues at the step after the predecessor's
	// last completed step.
	ResumeModeAdvance ResumeMode = "advance"
)

// ResumeDecision is where a new run starts relative to its predecessor.
type ResumeDecision struct {
	Mode      ResumeMode
	StartStep Step
	// Resumed reports a mid-pipeline continuation: the predecessor failed
	// or was interrupted and the new run starts past prepare-work.
	Resumed bool
	// StickyAgentSnapshot reports that the predecessor failed or was
	// interrupted, so the new run inherits its agent snapshot even when
	// it restarts from the top.
	StickyAgentSnapshot bool
	// LastCompletedStep is the value the new run should record as already
	// completed, or "" for none.
	LastCompletedStep Step
}

// DecideResume decides where a run starts given its predecessor's
// terminal status and the already-classified restart signals. Only a
// "failed" or "interrupted" predecessor can be continued, and never one
// held for manual resume or without a recorded completed step. Within a
// continuable predecessor the precedence is: a rediscovery restart beats
// an execute replay beats advancing past the last completed step.
func DecideResume(status string, lastCompleted Step, manualHold, restartFromDiscover, replayExecute bool) ResumeDecision {
	if status != "failed" && status != "interrupted" {
		return ResumeDecision{Mode: ResumeModeFresh, StartStep: StepPrepareWork}
	}
	decision := ResumeDecision{Mode: ResumeModeFresh, StartStep: StepPrepareWork, StickyAgentSnapshot: true}
	if manualHold || lastCompleted == "" {
		return decision
	}
	switch {
	case restartFromDiscover:
		decision.Mode = ResumeModeRestart
	case replayExecute:
		decision.Mode = ResumeModeReplayExecute
		decision.StartStep = StepExecute
		decision.Resumed = true
		decision.LastCompletedStep = Previous(StepExecute)
	case Next(lastCompleted) != "":
		decision.Mode = ResumeModeAdvance
		decision.StartStep = Next(lastCompleted)
		decision.Resumed = true
		decision.LastCompletedStep = lastCompleted
	}
	return decision
}
