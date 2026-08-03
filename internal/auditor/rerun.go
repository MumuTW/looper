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
	Version             int      `json:"version"`
	ObservationEventID  string   `json:"observationEventId"`
	Repo                string   `json:"repo"`
	HeadSHA             string   `json:"headSha"`
	CheckSuiteID        int64    `json:"checkSuiteId"`
	InitialFailedChecks []string `json:"initialFailedChecks,omitempty"`
	InitialFailedPaths  []string `json:"initialFailedPaths,omitempty"`
	RequestedAt         string   `json:"requestedAt"`
}

type ConfirmationRecord struct {
	Version            int                 `json:"version"`
	ObservationEventID string              `json:"observationEventId"`
	HeadSHA            string              `json:"headSha"`
	Outcome            ConfirmationOutcome `json:"outcome"`
	ConfirmedChecks    []string            `json:"confirmedChecks,omitempty"`
	Decision           Action              `json:"decision"`
	Reason             string              `json:"reason"`
	ConfirmedAt        string              `json:"confirmedAt"`
}
