package gatekeeper

import (
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
)

// MergeOutcomeEventType records a historical Gatekeeper merge attempt at the
// auto trust level. Auto trust now publishes commit status only and does not
// merge; this event type remains so Auditor and post-merge digest can read
// outcomes written before that contract change.
const MergeOutcomeEventType = "pull_request.merge_gate.merge_attempted"

// MergeOutcome is the durable record of one merge attempt.
type MergeOutcome struct {
	Version   int    `json:"version"`
	ProjectID string `json:"projectId"`
	Repo      string `json:"repo"`
	PRNumber  int64  `json:"prNumber"`
	// HeadSHA is the commit the decision was made about and, on success, the
	// commit that was merged.
	HeadSHA string `json:"headSha"`
	// MergeStrategy records the configured forge strategy so Auditor can reject
	// a rebase tip as incomplete revert provenance.
	MergeStrategy string `json:"mergeStrategy,omitempty"`
	MergeCommitSHA string `json:"mergeCommitSha,omitempty"`
	SourceIssue *githubinfra.IssueReference `json:"sourceIssue,omitempty"`
	// TouchedFiles is GitHub's authoritative pull-request file list captured
	// after a successful merge. Auditor may use it as attribution evidence.
	TouchedFiles []string `json:"touchedFiles,omitempty"`
	TouchedFilesAvailable bool `json:"touchedFilesAvailable,omitempty"`
	Merged  bool   `json:"merged"`
	// Reason explains a refusal. Empty on success.
	Reason string `json:"reason,omitempty"`
	// ConfirmingReasons are the gates that blocked the confirming evaluation, when
	// the first pass said eligible and the second did not.
	ConfirmingReasons []Reason `json:"confirmingReasons,omitempty"`
	AttemptedAt       string   `json:"attemptedAt"`
}
