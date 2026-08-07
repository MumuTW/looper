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
	InitialFailedChecks       []string
	InitialFailedPaths        []string
	InitialFailedPathsByCheck map[string][]string
	InitialFailureSignatures  []FailurePathSignature
	RerunCompleted            bool
	RerunFailedChecks         []string
	RerunFailedPaths          []string
	RerunFailedPathsByCheck   map[string][]string
	RerunFailureSignatures    []FailurePathSignature
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
	matching := matchingFailureChecks(input, initial, rerun)
	if len(matching) == 0 {
		// A missing path signature on either side is not proof that the same
		// failure returned. Keep this conservative even when check names match.
		return ConfirmationResult{Outcome: ConfirmationDifferentFailure}
	}
	confirmed := make([]string, 0)
	for check := range matching {
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

func matchingFailureChecks(input ConfirmationInput, initial, rerun map[string]struct{}) map[string]struct{} {
	matching := make(map[string]struct{})
	if len(input.InitialFailureSignatures) > 0 && len(input.RerunFailureSignatures) > 0 {
		initialPaths := signaturePaths(input.InitialFailureSignatures)
		for _, rerunSignature := range input.RerunFailureSignatures {
			check := strings.ToLower(strings.TrimSpace(rerunSignature.Check))
			if check == "" {
				continue
			}
			if _, ok := initial[check]; !ok {
				continue
			}
			if hasPathOverlap(initialPaths[check], rerunSignature.Paths) {
				matching[check] = struct{}{}
			}
		}
		return matching
	}
	if len(input.InitialFailedPathsByCheck) > 0 && len(input.RerunFailedPathsByCheck) > 0 {
		for check, rerunPaths := range input.RerunFailedPathsByCheck {
			normalizedCheck := strings.ToLower(strings.TrimSpace(check))
			if normalizedCheck == "" {
				continue
			}
			if _, ok := initial[normalizedCheck]; !ok {
				continue
			}
			initialPaths := input.InitialFailedPathsByCheck[check]
			if len(initialPaths) == 0 {
				for initialCheck, paths := range input.InitialFailedPathsByCheck {
					if strings.EqualFold(strings.TrimSpace(initialCheck), normalizedCheck) {
						initialPaths = paths
						break
					}
				}
			}
			if hasPathOverlap(initialPaths, rerunPaths) {
				matching[normalizedCheck] = struct{}{}
			}
		}
		return matching
	}
	initialPathCount, rerunPathCount := len(input.InitialFailedPaths), len(input.RerunFailedPaths)
	if !((initialPathCount == 0 && rerunPathCount == 0) || (initialPathCount > 0 && rerunPathCount > 0 && hasPathOverlap(input.InitialFailedPaths, input.RerunFailedPaths))) {
		return matching
	}
	for check := range rerun {
		if _, ok := initial[check]; ok {
			matching[check] = struct{}{}
		}
	}
	return matching
}

func signaturePaths(signatures []FailurePathSignature) map[string][]string {
	paths := make(map[string][]string, len(signatures))
	for _, signature := range signatures {
		check := strings.ToLower(strings.TrimSpace(signature.Check))
		if check == "" {
			continue
		}
		paths[check] = append(paths[check], signature.Paths...)
	}
	return paths
}

func hasPathOverlap(left, right []string) bool {
	set := make(map[string]struct{}, len(left))
	for _, value := range left {
		if normalized := strings.TrimSpace(value); normalized != "" {
			set[normalized] = struct{}{}
		}
	}
	for _, value := range right {
		if _, ok := set[strings.TrimSpace(value)]; ok && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
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
