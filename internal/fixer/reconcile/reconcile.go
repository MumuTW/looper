// Package reconcile holds the fixer's commit-reconciliation state and
// decision authority: the persisted record of what reconciliation observed
// and did, the base-revision precedence, outcome classification, and commit
// attribution. It has no I/O — the fixer package owns git access and
// checkpoint persistence, and calls into this package to decide what the
// inspection results mean. Third extraction under issue #120, following
// internal/reviewer/workflow (#131) and internal/fixer/workflow (#309).
package reconcile

import "github.com/nexu-io/looper/internal/lifecycle"

// State is the durable record of one commit-reconciliation pass. It is
// embedded in the fixer checkpoint JSON; the field tags are a persisted
// contract and must not change. CommittedByLoop deliberately serializes as
// "committedByLooperd" — the historical key.
type State struct {
	BaseHeadSHA      string   `json:"baseHeadSha,omitempty"`
	FinalHeadSHA     string   `json:"finalHeadSha,omitempty"`
	NewCommitSHAs    []string `json:"newCommitShas,omitempty"`
	CommittedByAgent bool     `json:"committedByAgent,omitempty"`
	CommittedByLoop  bool     `json:"committedByLooperd,omitempty"`
	WorkingTreeClean bool     `json:"workingTreeClean,omitempty"`
	ChangedFiles     []string `json:"changedFiles,omitempty"`
	CompletedAt      string   `json:"completedAt,omitempty"`
}

// BaseHeadSHA returns the recorded base revision, or "" when no pass has
// recorded one. Nil-safe so callers can pass a checkpoint field directly.
func BaseHeadSHA(state *State) string {
	if state == nil {
		return ""
	}
	return state.BaseHeadSHA
}

// IsComplete reports whether a reconciliation pass already ran to
// completion, making another pass a no-op rather than a repeat.
func IsComplete(state *State) bool {
	return state != nil && state.CompletedAt != ""
}

// SelectBase picks the base revision a reconciliation pass diffs against,
// in authority order: a base recorded by an earlier pass wins over the
// worktree's prepared base, which wins over the worktree's head at
// preparation time. A re-reconcile after drift must keep diffing against
// the ORIGINAL base or it under-reports the round's commits.
func SelectBase(state *State, worktreeBaseSHA, worktreeHeadSHA string) string {
	if recorded := BaseHeadSHA(state); recorded != "" {
		return recorded
	}
	if worktreeBaseSHA != "" {
		return worktreeBaseSHA
	}
	return worktreeHeadSHA
}

// Inspection is what one git head inspection observed, expressed without
// depending on the fixer package's git types.
type Inspection struct {
	HeadSHA               string
	NewCommitSHAs         []string
	ChangedFiles          []string
	HasUncommittedChanges bool
}

// Complete classifies a finished reconciliation pass into its durable
// State: the agent committed if the initial inspection already saw new
// commits, the loop committed if reconciliation had to commit the
// remainder itself, and the tree is clean if the final inspection found
// nothing uncommitted. Slices are copied so the State does not alias the
// inspection's backing arrays.
func Complete(baseHeadSHA string, initial, final Inspection, committedByLoop bool, completedAt string) *State {
	return &State{
		BaseHeadSHA:      baseHeadSHA,
		FinalHeadSHA:     final.HeadSHA,
		NewCommitSHAs:    append([]string(nil), final.NewCommitSHAs...),
		CommittedByAgent: len(initial.NewCommitSHAs) > 0,
		CommittedByLoop:  committedByLoop,
		WorkingTreeClean: !final.HasUncommittedChanges,
		ChangedFiles:     append([]string(nil), final.ChangedFiles...),
		CompletedAt:      completedAt,
	}
}

// CommitAttribution decides how the lifecycle record attributes this
// round's commits: a loop-authored commit is always the fallback source; a
// round where only the agent committed claims agent attribution only if
// nothing already claimed the action. No new commits leave the current
// attribution untouched.
func CommitAttribution(hasNewCommits, committedByLoop bool, current string) string {
	if !hasNewCommits {
		return current
	}
	if committedByLoop {
		return lifecycle.ActionSourceFallback
	}
	if current == lifecycle.ActionSourceNone {
		return lifecycle.ActionSourceAgent
	}
	return current
}

// ProducedNewCommits reports whether the recorded pass created or observed
// any commit beyond its base: an explicit new-commit list, or a final head
// that moved off a known base.
func ProducedNewCommits(state *State) bool {
	if state == nil {
		return false
	}
	if len(state.NewCommitSHAs) > 0 {
		return true
	}
	return state.FinalHeadSHA != "" && state.BaseHeadSHA != "" && state.FinalHeadSHA != state.BaseHeadSHA
}
