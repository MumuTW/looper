// Package policy holds the failure-kind and resume-policy vocabulary shared
// by every loop type, plus the pure predicates over it. It is a leaf: it
// imports only the standard library, so packages that need the vocabulary
// (for example internal/reviewer/workflow) can depend on it without pulling
// in the storage stack that the umbrella internal/loops package carries.
//
// internal/loops re-exports every name here, so existing callers keep using
// loops.ResumePolicyReplayStep and friends.
package policy

import "strings"

const (
	FailureKindRetryableAfterResume = "retryable_after_resume"
	FailureKindManualIntervention   = "manual_intervention"

	ResumePolicyAdvanceFromCheckpoint = "advance_from_checkpoint"
	ResumePolicyManualIntervention    = "manual_intervention"
	ResumePolicyReplayStep            = "replay_step"
	ResumePolicyRestartFromDiscover   = "restart_from_discover"
)

// NormalizeResumePolicy returns the resume policy to record for a failure.
// An existing non-empty resumePolicy always wins; only an empty one is
// derived from failureKind.
func NormalizeResumePolicy(failureKind, resumePolicy string) string {
	existing := strings.TrimSpace(resumePolicy)
	if existing != "" {
		return existing
	}
	switch strings.TrimSpace(failureKind) {
	case FailureKindRetryableAfterResume:
		return ResumePolicyAdvanceFromCheckpoint
	case FailureKindManualIntervention:
		return ResumePolicyManualIntervention
	default:
		return ResumePolicyReplayStep
	}
}

func IsManualHoldResumePolicy(resumePolicy string) bool {
	return strings.TrimSpace(resumePolicy) == ResumePolicyManualIntervention
}

func IsHardHold(failureKind, resumePolicy string) bool {
	if IsManualHoldResumePolicy(resumePolicy) {
		return true
	}
	return strings.TrimSpace(failureKind) == FailureKindManualIntervention
}

func SuppressesAutonomousRecovery(failureKind, resumePolicy string) bool {
	return IsHardHold(failureKind, resumePolicy)
}

func ShouldRestartFromDiscover(status, resumePolicy string) bool {
	if status != "failed" && status != "interrupted" {
		return false
	}
	return strings.TrimSpace(resumePolicy) == ResumePolicyRestartFromDiscover
}
