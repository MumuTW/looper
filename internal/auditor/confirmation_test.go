package auditor

import (
	"slices"
	"testing"
)

func TestConfirmFailureRequiresMatchingCompletedRerun(t *testing.T) {
	cases := []struct {
		name string
		in   ConfirmationInput
		want ConfirmationResult
	}{
		{name: "matching rerun confirms regression", in: ConfirmationInput{InitialFailedChecks: []string{"CI", "lint"}, RerunCompleted: true, RerunFailedChecks: []string{"ci"}}, want: ConfirmationResult{Outcome: ConfirmationConfirmed, ConfirmedChecks: []string{"ci"}}},
		{name: "passing rerun is a suspected flake", in: ConfirmationInput{InitialFailedChecks: []string{"ci"}, RerunCompleted: true}, want: ConfirmationResult{Outcome: ConfirmationSuspectedFlake}},
		{name: "different failing check is not confirmation", in: ConfirmationInput{InitialFailedChecks: []string{"ci"}, RerunCompleted: true, RerunFailedChecks: []string{"integration"}}, want: ConfirmationResult{Outcome: ConfirmationDifferentFailure}},
		{name: "unfinished rerun is inconclusive", in: ConfirmationInput{InitialFailedChecks: []string{"ci"}, RerunCompleted: false, RerunFailedChecks: []string{"ci"}}, want: ConfirmationResult{Outcome: ConfirmationInconclusive}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := ConfirmFailure(test.in)
			if got.Outcome != test.want.Outcome || !slices.Equal(got.ConfirmedChecks, test.want.ConfirmedChecks) {
				t.Fatalf("ConfirmFailure() = %#v, want %#v", got, test.want)
			}
		})
	}
}
