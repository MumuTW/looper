package auditor

// ObservedFailureEventType is the append-only record that a configured Auditor
// saw failed checks on a default-branch head after one or more Looper merges.
const ObservedFailureEventType = "post_merge_audit.observed_failure"

// BaselineEventType records a clean default-branch observation. A later failure
// is actionable only when a clean baseline exists inside the audit window.
const BaselineEventType = "post_merge_audit.clean_baseline"

type FailureObservation struct {
	Version      int      `json:"version"`
	ProjectID    string   `json:"projectId"`
	Repo         string   `json:"repo"`
	HeadSHA      string   `json:"headSha"`
	FailedChecks []string `json:"failedChecks"`
	FailingPaths []string `json:"failingPaths"`
	// FailingPathsByCheck keeps attribution evidence tied to the provider check
	// that emitted it; an aggregate list alone cannot exclude paths from a
	// check that later flakes away.
	FailingPathsByCheck map[string][]string `json:"failingPathsByCheck,omitempty"`
	// FailingPathEvidenceComplete is false when one or more failed check
	// annotation reads failed; partial paths cannot authorize attribution.
	FailingPathEvidenceComplete bool    `json:"failingPathEvidenceComplete"`
	CheckSuiteIDs               []int64 `json:"checkSuiteIds"`
	CandidatePRs                []int64 `json:"candidatePrs"`
	BaselineKnown               bool    `json:"baselineKnown"`
	ObservedAt                  string  `json:"observedAt"`
}

type BaselineObservation struct {
	Version    int    `json:"version"`
	ProjectID  string `json:"projectId"`
	Repo       string `json:"repo"`
	HeadSHA    string `json:"headSha"`
	ObservedAt string `json:"observedAt"`
}
