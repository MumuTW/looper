package gatekeeper

// AdviceAgreementEventType records the forge-observed outcome of one advise
// verdict in one terminal lifecycle epoch. The Gate report remains the
// authority for what Gatekeeper said; the terminal pull-request state returned
// by the forge is the authority for what happened afterwards.
const AdviceAgreementEventType = "pull_request.merge_gate.advice_agreement_recorded"

type AdviceOutcome string

const (
	AdviceOutcomeMergedAsIs        AdviceOutcome = "merged_as_is"
	AdviceOutcomeMergedAfterChange AdviceOutcome = "merged_after_changes"
	AdviceOutcomeClosed            AdviceOutcome = "closed"
	AdviceOutcomeHeld              AdviceOutcome = "held"
	AdviceOutcomeOverridden        AdviceOutcome = "overridden"
)

// AdviceAgreement is immutable attribution for an advise verdict in a
// terminal lifecycle epoch. CausationID points at the exact Gate report it
// resolves, so a later verdict cannot change the historical outcome of an
// earlier epoch.
type AdviceAgreement struct {
	Version         int           `json:"version"`
	VerdictEventID  string        `json:"verdictEventId"`
	ProjectID       string        `json:"projectId"`
	Repo            string        `json:"repo"`
	PRNumber        int64         `json:"prNumber"`
	VerdictEligible bool          `json:"verdictEligible"`
	VerdictHeadSHA  string        `json:"verdictHeadSha,omitempty"`
	Outcome         AdviceOutcome `json:"outcome"`
	Agreement       bool          `json:"agreement"`
	TerminalState   string        `json:"terminalState"`
	TerminalHeadSHA string        `json:"terminalHeadSha,omitempty"`
	TerminalAt      string        `json:"terminalAt"`
	// TerminalEpoch identifies the forge-observed closure/merge epoch. A
	// closure can later be reopened, so a subsequent terminal observation is a
	// distinct immutable agreement rather than being suppressed by the first
	// closed-state record.
	TerminalEpoch string `json:"terminalEpoch"`
	RecordedAt    string `json:"recordedAt"`
}
