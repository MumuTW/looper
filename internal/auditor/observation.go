package auditor

// ObservedFailureEventType is the append-only record that a configured Auditor
// saw failed checks on a default-branch head after one or more Looper merges.
const ObservedFailureEventType = "post_merge_audit.observed_failure"

type FailureObservation struct {
	Version       int      `json:"version"`
	ProjectID     string   `json:"projectId"`
	Repo          string   `json:"repo"`
	HeadSHA       string   `json:"headSha"`
	FailedChecks  []string `json:"failedChecks"`
	FailingPaths  []string `json:"failingPaths"`
	CheckSuiteIDs []int64  `json:"checkSuiteIds"`
	CandidatePRs  []int64  `json:"candidatePrs"`
	ObservedAt    string   `json:"observedAt"`
}
