package auditor

import githubinfra "github.com/MumuTW/looper/internal/infra/github"

// RerunRequestedEventType records one explicit GitHub check-suite rerequest
// for a post-merge failure observation. It is emitted only after GitHub accepts
// the request, so later ticks do not repeatedly request the same suite.
const RerunRequestedEventType = "post_merge_audit.rerun_requested"

// ConfirmationEventType records the result of a completed rerequest. Its
// outcome makes flakes and non-actionable escalation durable without creating
// a new status table.
const ConfirmationEventType = "post_merge_audit.confirmation"

// RevertProposalEventType records the draft PR and source-issue reopen after a
// confirmed, provenance-complete regression. It is the idempotency record for
// future scheduler ticks, not authority to merge the proposed revert.
const RevertProposalEventType = "post_merge_audit.revert_proposed"

type RerunRequest struct {
	Version                   int                    `json:"version"`
	ObservationEventID        string                 `json:"observationEventId"`
	Repo                      string                 `json:"repo"`
	HeadSHA                   string                 `json:"headSha"`
	CheckSuiteID              int64                  `json:"checkSuiteId"`
	InitialFailedChecks       []string               `json:"initialFailedChecks,omitempty"`
	InitialFailedPaths        []string               `json:"initialFailedPaths,omitempty"`
	InitialFailedPathsByCheck map[string][]string    `json:"initialFailedPathsByCheck,omitempty"`
	InitialFailureSignatures  []FailurePathSignature `json:"initialFailureSignatures,omitempty"`
	RequestedAt               string                 `json:"requestedAt"`
}

type ConfirmationRecord struct {
	Version                 int                         `json:"version"`
	ObservationEventID      string                      `json:"observationEventId"`
	HeadSHA                 string                      `json:"headSha"`
	Outcome                 ConfirmationOutcome         `json:"outcome"`
	ConfirmedChecks         []string                    `json:"confirmedChecks,omitempty"`
	Decision                Action                      `json:"decision"`
	Reason                  string                      `json:"reason"`
	Candidate               *ConfirmedCandidate         `json:"candidate,omitempty"`
	CandidatePRNumber       int64                       `json:"candidatePrNumber,omitempty"`
	CandidateHeadSHA        string                      `json:"candidateHeadSha,omitempty"`
	CandidateMergeCommitSHA string                      `json:"candidateMergeCommitSha,omitempty"`
	CandidateSourceIssue    *githubinfra.IssueReference `json:"candidateSourceIssue,omitempty"`
	ConfirmedAt             string                      `json:"confirmedAt"`
}

// ConfirmedCandidate freezes the exact merge selected by attribution. A later
// revert proposal consumes this record instead of re-ranking mutable history.
type ConfirmedCandidate struct {
	PRNumber          int64  `json:"prNumber"`
	MergeCommitSHA    string `json:"mergeCommitSha"`
	MergeStrategy     string `json:"mergeStrategy"`
	SourceIssueNumber int64  `json:"sourceIssueNumber"`
	SourceIssueRepo   string `json:"sourceIssueRepo"`
}

type RevertProposal struct {
	Version             int    `json:"version"`
	ConfirmationEventID string `json:"confirmationEventId"`
	Repo                string `json:"repo"`
	HeadSHA             string `json:"headSha"`
	MergeCommitSHA      string `json:"mergeCommitSha"`
	MergeStrategy       string `json:"mergeStrategy"`
	SourceIssueNumber   int64  `json:"sourceIssueNumber"`
	Branch              string `json:"branch"`
	PRNumber            int64  `json:"prNumber"`
	PRURL               string `json:"prUrl"`
	ProposedAt          string `json:"proposedAt"`
}
