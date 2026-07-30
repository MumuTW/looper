package auditor

import "testing"

func TestDecideProposesRevertOnlyForConfirmedHighConfidenceCandidate(t *testing.T) {
	candidate := &MergeCandidate{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, HeadSHA: "abc"}
	cases := []struct {
		name         string
		confirmation ConfirmationResult
		attribution  Attribution
		want         Action
	}{
		{name: "confirmed high confidence", confirmation: ConfirmationResult{Outcome: ConfirmationConfirmed}, attribution: Attribution{Confidence: ConfidenceHigh, Candidate: candidate, Reason: "unique"}, want: ActionProposeRevert},
		{name: "confirmed low confidence escalates", confirmation: ConfirmationResult{Outcome: ConfirmationConfirmed}, attribution: Attribution{Confidence: ConfidenceLow, Reason: "tie"}, want: ActionEscalate},
		{name: "flake is recorded", confirmation: ConfirmationResult{Outcome: ConfirmationSuspectedFlake}, want: ActionRecordFlake},
		{name: "different failure escalates", confirmation: ConfirmationResult{Outcome: ConfirmationDifferentFailure}, want: ActionEscalate},
		{name: "inconclusive rerun escalates", confirmation: ConfirmationResult{Outcome: ConfirmationInconclusive}, want: ActionEscalate},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := Decide(test.confirmation, test.attribution)
			if got.Action != test.want {
				t.Fatalf("Decide() = %#v, want %s", got, test.want)
			}
			if test.want == ActionProposeRevert && got.Candidate != candidate {
				t.Fatalf("candidate = %#v, want %#v", got.Candidate, candidate)
			}
		})
	}
}
