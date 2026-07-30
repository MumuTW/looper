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
	pinned := reproduction.GovernedIssueNumber(parseJSONObject(input.Loop.MetadataJSON))
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
	if pinned <= 0 {
		if err := r.pinReproductionIssue(ctx, input.Loop, worktreePath); err != nil {
			return &loopError{message: "Reproduction gate could not be pinned to its Issue: " + err.Error(), kind: FailureRetryableTransient}
		}
	}
	if result.Passed {
		return nil
	}
	return &loopError{
		message: reproduction.FailureSummary(repo, pinned, result),
		kind:    FailureManualIntervention,
	}
}

func (r *Runner) pinReproductionIssue(ctx context.Context, loop storage.LoopRecord, worktreePath string) error {
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
