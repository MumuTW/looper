package reproduction

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/processcontainment"
	"github.com/nexu-io/looper/internal/storage"
)

// LoopMetadataIssueKey pins which Issue's Reproduction Record governs a loop.
//
// A PR-rooted Role (Fixer) has no Issue number of its own. It learns one from
// the committed manifest on its first pass and pins it here, so a later pass
// still gates even if the manifest has since been deleted — deletion is then a
// detected tamper rather than a way out of the gate.
const LoopMetadataIssueKey = "reproductionIssueNumber"

// LoopGateInput describes one completion-gate evaluation.
type LoopGateInput struct {
	Repos        *storage.Repositories
	ProjectID    string
	Repo         string
	IssueNumber  int64
	WorktreePath string
	Timeout      time.Duration
	CodexCommand string
	Tracker      processcontainment.LiveTracker

	// run is an in-package test seam.
	run func(context.Context, Input) (Result, error)
}

// GateForLoop evaluates the reproduction completion gate for one worktree.
//
// The second return reports whether the gate applies at all. It is false for
// every Issue with no Reproduction Record — every non-bug Issue, every project
// with the Role disabled, and every pre-existing loop — so the gate is inert
// rather than newly restrictive when nothing was reproduced.
//
// The command and the file hashes come from the persisted record, not from the
// worktree copy. A Role that rewrote the committed manifest would therefore
// still be measured against what Reproducer actually observed.
func GateForLoop(ctx context.Context, input LoopGateInput) (Result, bool, error) {
	if input.Repos == nil || input.Repos.Events == nil {
		return Result{}, false, nil
	}
	issueNumber := input.IssueNumber
	repo := strings.TrimSpace(input.Repo)
	if issueNumber <= 0 {
		manifest, present, err := ReadManifest(input.WorktreePath)
		if err != nil || !present {
			return Result{}, false, nil
		}
		issueNumber = manifest.IssueNumber
		if strings.TrimSpace(manifest.Repo) != "" {
			repo = strings.TrimSpace(manifest.Repo)
		}
	}
	if issueNumber <= 0 || repo == "" {
		return Result{}, false, nil
	}
	status, err := LoadStatus(ctx, input.Repos, input.ProjectID, repo, issueNumber)
	if err != nil {
		return Result{}, false, err
	}
	if status.Record == nil {
		return Result{}, false, nil
	}
	verify := input.run
	if verify == nil {
		verify = Verify
	}
	result, err := verify(ctx, Input{
		WorktreePath: input.WorktreePath,
		Record:       *status.Record,
		Timeout:      input.Timeout,
		CodexCommand: input.CodexCommand,
		Tracker:      input.Tracker,
	})
	if err != nil {
		return Result{}, true, err
	}
	return result, true, nil
}

// GovernedIssueNumber returns the Issue number a loop's reproduction gate is
// pinned to, or zero when it has not been resolved yet.
func GovernedIssueNumber(metadata map[string]any) int64 {
	if metadata == nil {
		return 0
	}
	switch value := metadata[LoopMetadataIssueKey].(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case int:
		return int64(value)
	default:
		return 0
	}
}

// FailureSummary renders a gate failure as a message that names the
// reproduction explicitly. "Validation failed" would be indistinguishable from
// an ordinary red suite, which is the one thing this Role exists to prevent.
func FailureSummary(repo string, issueNumber int64, result Result) string {
	prefix := "Reproduction gate"
	if repo != "" && issueNumber > 0 {
		prefix = fmt.Sprintf("Reproduction gate for %s#%d", repo, issueNumber)
	}
	return fmt.Sprintf("%s [%s]: %s", prefix, result.Reason, result.Summary)
}
