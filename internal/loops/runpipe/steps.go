package runpipe

import (
	"context"

	"github.com/MumuTW/looper/internal/storage"
)

// StepRunner is the shared step-iteration engine behind every runner's
// ProcessClaimedItem loop: the ordering of persist-started → started event
// → execute → failure dispatch → persist-completed → completed event is
// single-sourced here, so a fix to checkpoint-persistence order or event
// timing lands once instead of four times (#537). Everything a runner does
// differently rides in the hooks; the engine owns only the ordering.
//
// It is a struct of functions rather than an interface, matching the
// package's function-first style and letting runners build it from
// closures over their step-input context.
type StepRunner[S ~string, C any] struct {
	// PersistStarted durably records that step is beginning.
	PersistStarted func(ctx context.Context, run storage.RunRecord, step S, checkpoint C) (storage.RunRecord, error)
	// PersistCompleted durably records that step finished.
	PersistCompleted func(ctx context.Context, run storage.RunRecord, step S, checkpoint C) (storage.RunRecord, error)
	// EmitStepEvent publishes a step lifecycle event ("loop.step.started",
	// "loop.step.completed"); failure events stay inside OnFailure, whose
	// runners name them differently (failed vs interrupted).
	EmitStepEvent func(ctx context.Context, eventType string, step S, run storage.RunRecord)
	// Execute runs the step and returns the advanced checkpoint.
	Execute func(ctx context.Context, step S, run storage.RunRecord, checkpoint C) (C, error)
	// OnFailure dispatches a step error. Returning handled=true converts
	// the error into the run's terminal outcome (suspend, hold-skip,
	// label-mismatch, or the runner's failure tail); handled=false with a
	// non-nil error aborts the pipeline as an infrastructure failure.
	OnFailure func(ctx context.Context, step S, run storage.RunRecord, checkpoint C, stepErr error) (ProcessResult, bool, error)
	// AfterCompleted runs post-step bookkeeping (lock rebinding, skip
	// detection) and reports whether iteration should stop early.
	AfterCompleted func(ctx context.Context, step S, checkpoint C) (stop bool)
}

// Run iterates steps under the shared ordering. It returns the final run
// record and checkpoint, plus a non-nil terminal result when a step error
// was converted into one by OnFailure — the caller then returns that
// result as-is. A nil terminal with nil error means every step completed
// (or AfterCompleted stopped early) and the caller owns the success tail.
func (s StepRunner[S, C]) Run(ctx context.Context, steps []S, run storage.RunRecord, checkpoint C) (storage.RunRecord, C, *ProcessResult, error) {
	for _, step := range steps {
		var err error
		run, err = s.PersistStarted(ctx, run, step, checkpoint)
		if err != nil {
			return run, checkpoint, nil, err
		}
		if s.EmitStepEvent != nil {
			s.EmitStepEvent(ctx, "loop.step.started", step, run)
		}
		// The step's returned checkpoint is adopted even on error: steps
		// mutate resume policy and pause state on their error paths, and
		// the failure dispatch must see those mutations (this mirrors the
		// runners' historical `checkpoint, err = executeStep(...)`).
		checkpoint, err = s.Execute(ctx, step, run, checkpoint)
		if err != nil {
			result, handled, failErr := s.OnFailure(ctx, step, run, checkpoint, err)
			if failErr != nil {
				return run, checkpoint, nil, failErr
			}
			if handled {
				return run, checkpoint, &result, nil
			}
			return run, checkpoint, nil, err
		}
		run, err = s.PersistCompleted(ctx, run, step, checkpoint)
		if err != nil {
			return run, checkpoint, nil, err
		}
		if s.EmitStepEvent != nil {
			s.EmitStepEvent(ctx, "loop.step.completed", step, run)
		}
		if s.AfterCompleted != nil && s.AfterCompleted(ctx, step, checkpoint) {
			break
		}
	}
	return run, checkpoint, nil, nil
}
