package worker

import (
	"context"

	"github.com/nexu-io/looper/internal/reproduction"
)

// enforceReproductionGate is the completion half of the Reproducer Role.
//
// It runs after the repository's own validation suite has passed and the
// worktree is clean, and it adds one requirement: the Reproduction Record's
// command must now pass, and the reproduction files must still hash to what
// Reproducer committed. Red→green *and* no regression — the existing full-suite
// gate is untouched and still required.
//
// The gate is inert unless a Reproduction Record exists for this Issue, so
// every non-bug Issue and every project with the Role disabled behaves exactly
// as before.
func (r *Runner) enforceReproductionGate(ctx context.Context, input stepInput, work workerInput, worktreePath string) error {
	if r.repos == nil {
		return nil
	}
	repo := issueLookupRepo(work)
	result, applies, err := reproduction.GateForLoop(ctx, reproduction.LoopGateInput{
		Repos:        r.repos,
		ProjectID:    input.Project.ID,
		Repo:         repo,
		IssueNumber:  work.IssueNumber,
		WorktreePath: worktreePath,
		Timeout:      r.agentTimeout,
		CodexCommand: r.validationCodexCommand,
		Tracker:      r.containmentTracker,
	})
	if err != nil {
		return &loopError{message: "Reproduction gate could not be evaluated: " + err.Error(), kind: FailureRetryableTransient}
	}
	if !applies || result.Passed {
		return nil
	}
	// Every reproduction-gate failure needs a human: a tampered reproduction
	// will not un-tamper on retry, and a still-failing reproduction means the
	// bug is not fixed. Neither is a transient condition.
	return &loopError{
		message: reproduction.FailureSummary(repo, work.IssueNumber, result),
		kind:    FailureManualIntervention,
	}
}
