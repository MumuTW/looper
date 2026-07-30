package auditor

// RerunRequestedEventType records one explicit GitHub check-suite rerequest
// for a post-merge failure observation. It is emitted only after GitHub accepts
// the request, so later ticks do not repeatedly request the same suite.
const RerunRequestedEventType = "post_merge_audit.rerun_requested"

// ConfirmationEventType records the result of a completed rerequest. Its
// outcome makes flakes and non-actionable escalation durable without creating
// a new status table.
const ConfirmationEventType = "post_merge_audit.confirmation"

type RerunRequest struct {
	Version            int    `json:"version"`
	ObservationEventID string `json:"observationEventId"`
	Repo               string `json:"repo"`
	HeadSHA            string `json:"headSha"`
	CheckSuiteID       int64  `json:"checkSuiteId"`
	RequestedAt        string `json:"requestedAt"`
}

type ConfirmationRecord struct {
	Version            int                 `json:"version"`
	ObservationEventID string              `json:"observationEventId"`
	HeadSHA            string              `json:"headSha"`
	Outcome            ConfirmationOutcome `json:"outcome"`
	ConfirmedChecks    []string            `json:"confirmedChecks,omitempty"`
	Decision           Action              `json:"decision"`
	Reason             string              `json:"reason"`
	Candidate          *ConfirmedCandidate `json:"candidate,omitempty"`
	ConfirmedAt        string              `json:"confirmedAt"`
}

// ConfirmedCandidate freezes the exact merge selected by attribution. A later
// revert proposal consumes this record instead of re-ranking mutable history.
type ConfirmedCandidate struct {
	PRNumber          int64  `json:"prNumber"`
	MergeCommitSHA    string `json:"mergeCommitSha"`
	SourceIssueNumber int64  `json:"sourceIssueNumber"`
	SourceIssueRepo   string `json:"sourceIssueRepo"`
}
