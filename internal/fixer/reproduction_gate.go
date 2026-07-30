package fixer

import (
	"context"
	"strings"

	"github.com/nexu-io/looper/internal/reproduction"
	"github.com/nexu-io/looper/internal/storage"
)

// enforceReproductionGate is the completion half of the Reproducer Role for a
// pull-request-rooted Role.
//
// It runs after the repository's own validation suite has passed and adds one
// requirement: the Reproduction Record's command must pass and its files must
// still hash to what Reproducer committed. The full-suite gate is unchanged and
// still required.
//
// Fixer has no Issue number of its own, so it learns which record governs it
// from the committed manifest on the branch and pins that Issue onto the loop.
// Pinning is what stops "delete the manifest" from being a way out of the gate:
// once pinned, a later pass loads the record regardless of what the worktree
// still contains.
func (r *Runner) enforceReproductionGate(ctx context.Context, input stepInput, worktreePath string) error {
	if r.repos == nil {
		return nil
	}
	// The loop record is re-read rather than trusted from stepInput: the
	// governing Issue is pinned before the agent starts, and that write happened
	// after this step's input was captured.
	pinned := reproduction.GovernedIssueNumber(parseJSONObject(r.currentLoopMetadata(ctx, input.Loop)))
	repo := strings.TrimSpace(input.Repo)
	result, applies, err := reproduction.GateForLoop(ctx, reproduction.LoopGateInput{
		Repos:        r.repos,
		ProjectID:    input.Project.ID,
		Repo:         repo,
		IssueNumber:  pinned,
		WorktreePath: worktreePath,
		Timeout:      r.agentTimeout,
		CodexCommand: r.validationCodexCommand,
		Tracker:      r.containmentTracker,
	})
	if err != nil {
		return &loopError{message: "Reproduction gate could not be evaluated: " + err.Error(), kind: FailureRetryableTransient}
	}
	if !applies {
		return nil
	}
	if result.Passed {
		return nil
	}
	return &loopError{
		message: reproduction.FailureSummary(repo, pinned, result),
		kind:    FailureManualIntervention,
	}
}

// pinReproductionIssue records which Issue's Reproduction Record governs this
// loop, and is called *before* the Fixer agent runs.
//
// Pinning at validation time was too late to be a gate at all. On a loop's first
// pass the Issue is unknown until the manifest is read, so an agent that deleted
// `.looper/reproduction.json` during its turn left GateForLoop with nothing to
// discover: `applies` came back false and both the integrity check and the
// command check were skipped — the precise tampering the gate exists to catch.
// Resolving the Issue before the agent starts means deletion is detected as
// tampering rather than being the way out.
func (r *Runner) pinReproductionIssue(ctx context.Context, loop storage.LoopRecord, worktreePath string) error {
	if r.repos == nil || r.repos.Loops == nil {
		return nil
	}
	if reproduction.GovernedIssueNumber(parseJSONObject(loop.MetadataJSON)) > 0 {
		return nil
	}
	manifest, present, err := reproduction.ReadManifest(worktreePath)
	if err != nil || !present || manifest.IssueNumber <= 0 {
		return nil
	}
	_, err = r.updateLoop(ctx, loop, func(record *storage.LoopRecord) {
		metadataJSON, mergeErr := mergeLoopMetadataJSON(record.MetadataJSON, map[string]any{
			reproduction.LoopMetadataIssueKey: manifest.IssueNumber,
		})
		if mergeErr == nil {
			record.MetadataJSON = &metadataJSON
		}
	})
	return err
}

// currentLoopMetadata re-reads the loop so metadata written earlier in this run
// — the pinned Issue in particular — is visible to the gate.
func (r *Runner) currentLoopMetadata(ctx context.Context, loop storage.LoopRecord) *string {
	if r.repos == nil || r.repos.Loops == nil {
		return loop.MetadataJSON
	}
	current, err := r.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || current == nil {
		return loop.MetadataJSON
	}
	return current.MetadataJSON
}
