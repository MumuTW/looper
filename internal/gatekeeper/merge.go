package gatekeeper

// MergeOutcomeEventType is retained for historical direct-merge audit events.
// Gatekeeper now routes eligible auto-trust reports through the Mergify label
// contract, so new evaluations do not emit this event; post-merge consumers must
// continue to understand records written by older daemon versions.
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
	Merged  bool   `json:"merged"`
	// Reason explains a refusal. Empty on success.
	Reason string `json:"reason,omitempty"`
	// ConfirmingReasons are the gates that blocked the confirming evaluation, when
	// the first pass said eligible and the second did not.
	ConfirmingReasons []Reason `json:"confirmingReasons,omitempty"`
	AttemptedAt       string   `json:"attemptedAt"`
}
