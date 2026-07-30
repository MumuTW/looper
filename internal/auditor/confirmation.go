package auditor

import (
	"sort"
	"strings"
)

type ConfirmationOutcome string

const (
	ConfirmationConfirmed        ConfirmationOutcome = "confirmed"
	ConfirmationSuspectedFlake   ConfirmationOutcome = "suspected_flake"
	ConfirmationDifferentFailure ConfirmationOutcome = "different_failure"
	ConfirmationInconclusive     ConfirmationOutcome = "inconclusive"
)

// ConfirmationInput compares the failed checks observed on the default branch
// with one completed re-run on the same audited ref.
type ConfirmationInput struct {
	InitialFailedChecks []string
	RerunCompleted      bool
	RerunFailedChecks   []string
}

type ConfirmationResult struct {
	Outcome         ConfirmationOutcome
	ConfirmedChecks []string
}

// ConfirmFailure enforces the Auditor flake discipline. Only a completed rerun
// that repeats at least one originally failing check is confirmed; every other
// result is non-actionable and must not produce a revert proposal.
func ConfirmFailure(input ConfirmationInput) ConfirmationResult {
	initial := normalizedSet(input.InitialFailedChecks)
	if !input.RerunCompleted {
		return ConfirmationResult{Outcome: ConfirmationInconclusive}
	}
	rerun := normalizedSet(input.RerunFailedChecks)
	if len(rerun) == 0 {
		return ConfirmationResult{Outcome: ConfirmationSuspectedFlake}
	}
	confirmed := make([]string, 0)
	for check := range rerun {
		if _, exists := initial[check]; exists {
			confirmed = append(confirmed, check)
		}
	}
	if len(confirmed) == 0 {
		return ConfirmationResult{Outcome: ConfirmationDifferentFailure}
	}
	sort.Strings(confirmed)
	return ConfirmationResult{Outcome: ConfirmationConfirmed, ConfirmedChecks: confirmed}
}

func normalizedSet(checks []string) map[string]struct{} {
	result := make(map[string]struct{}, len(checks))
	for _, check := range checks {
		if normalized := strings.TrimSpace(strings.ToLower(check)); normalized != "" {
			result[normalized] = struct{}{}
		}
	}
	return result
}
