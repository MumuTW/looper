package auditor

type Action string

const (
	ActionNone          Action = "none"
	ActionRecordFlake   Action = "record_suspected_flake"
	ActionEscalate      Action = "escalate"
	ActionProposeRevert Action = "propose_revert"
)

type Decision struct {
	Action    Action
	Candidate *MergeCandidate
	Reason    string
}

// Decide composes the Auditor's action gates. A revert proposal is possible
// only after a matching rerun confirmed the failure and attribution uniquely
// identifies a high-confidence Looper merge candidate. All other outcomes are
// intentionally non-destructive records or human escalation.
func Decide(confirmation ConfirmationResult, attribution Attribution) Decision {
	switch confirmation.Outcome {
	case ConfirmationSuspectedFlake:
		return Decision{Action: ActionRecordFlake, Reason: string(confirmation.Outcome)}
	case ConfirmationConfirmed:
		// A candidate projected from a merge outcome with authoritative file
		// evidence must also carry the merge commit that a future revert would
		// target. Synthetic unit candidates without that provenance remain
		// usable for the pure decision contract; the runtime never treats them
		// as action authority.
		if attribution.Confidence == ConfidenceHigh && attribution.Candidate != nil && (attribution.Candidate.MergeCommitSHA != "" || !attribution.Candidate.TouchedFilesAvailable) {
			return Decision{Action: ActionProposeRevert, Candidate: attribution.Candidate, Reason: attribution.Reason}
		}
		return Decision{Action: ActionEscalate, Reason: attribution.Reason}
	case ConfirmationDifferentFailure, ConfirmationInconclusive:
		return Decision{Action: ActionEscalate, Reason: string(confirmation.Outcome)}
	default:
		return Decision{Action: ActionNone, Reason: "unknown_confirmation_outcome"}
	}
}
