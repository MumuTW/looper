package loops

import "github.com/MumuTW/looper/internal/loops/policy"

// The failure-kind and resume-policy vocabulary lives in the leaf package
// internal/loops/policy so that decision-only packages can depend on it
// without pulling in this package's storage and eventlog dependencies. These
// aliases keep every existing loops.* call site working.
const (
	FailureKindRetryableAfterResume = policy.FailureKindRetryableAfterResume
	FailureKindManualIntervention   = policy.FailureKindManualIntervention

	ResumePolicyAdvanceFromCheckpoint = policy.ResumePolicyAdvanceFromCheckpoint
	ResumePolicyManualIntervention    = policy.ResumePolicyManualIntervention
	ResumePolicyReplayStep            = policy.ResumePolicyReplayStep
	ResumePolicyRestartFromDiscover   = policy.ResumePolicyRestartFromDiscover
)

func NormalizeResumePolicy(failureKind, resumePolicy string) string {
	return policy.NormalizeResumePolicy(failureKind, resumePolicy)
}

func IsManualHoldResumePolicy(resumePolicy string) bool {
	return policy.IsManualHoldResumePolicy(resumePolicy)
}

func IsHardHold(failureKind, resumePolicy string) bool {
	return policy.IsHardHold(failureKind, resumePolicy)
}

func SuppressesAutonomousRecovery(failureKind, resumePolicy string) bool {
	return policy.SuppressesAutonomousRecovery(failureKind, resumePolicy)
}

func ShouldRestartFromDiscover(status, resumePolicy string) bool {
	return policy.ShouldRestartFromDiscover(status, resumePolicy)
}
