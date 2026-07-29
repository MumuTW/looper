package failurepolicy

import (
	"context"
	"errors"
	"strings"

	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/loops/failureclass"
)

// Decision is the structured output of the fixer failure-policy boundary.
type Decision struct {
	Kind         failureclass.Kind
	ResumePolicy string
	Message      string
}

// ClassifyError maps an error and its runtime boundary to a failure decision.
// It is the authority for translating infra/model/Git errors into the retry /
// resume / manual-intervention policy. It does not assume a *loopError or any
// other fixer-package type.
func ClassifyError(err error, boundary failureclass.Boundary) Decision {
	if err == nil {
		return Decision{Kind: failureclass.NonRetryable, Message: ""}
	}

	message := err.Error()
	if strings.Contains(strings.ToLower(message), "remote head changed") {
		return Decision{Kind: failureclass.RetryableAfterResume, Message: message}
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Decision{Kind: failureclass.RetryableTransient, Message: message}
	}

	if githubinfra.IsTransientError(err) {
		return Decision{Kind: failureclass.RetryableTransient, Message: message}
	}

	kind := failureclass.Classify(err, failureclass.Context{Runner: failureclass.RunnerFixer, Boundary: boundary})
	return Decision{Kind: kind, Message: message}
}

// BoundaryForStep maps a fixer step name to the failure boundary that owns its
// external interactions.
func BoundaryForStep(step string) failureclass.Boundary {
	switch step {
	case "discover-pr", "claim-pr", "collect-fixes", "resolve-comments", "recheck":
		return failureclass.BoundaryGitHubAPI
	case "prepare-worktree", "push":
		return failureclass.BoundaryGitRemote
	case "repair":
		return failureclass.BoundaryModelProvider
	case "validate", "reconcile-commits":
		return failureclass.BoundaryAgentProcess
	default:
		return failureclass.BoundaryUnknown
	}
}

// ClassifyValidation maps a validation command result to a failure decision.
// It is the authority for deciding whether a failed validation is a transient
// tooling problem (replay the step) or a deterministic test/repair failure
// (manual intervention). It operates on the structured validation summary, not
// on the raw command output.
func ClassifyValidation(summary, output string) Decision {
	_ = output // output is preserved for diagnostics but not used for classification
	message := strings.TrimSpace(summary)
	if message == "" {
		message = "Validation failed"
	}
	if strings.HasPrefix(summary, "Validation timed out:") {
		return Decision{Kind: failureclass.RetryableTransient, ResumePolicy: loops.ResumePolicyReplayStep, Message: message}
	}
	lowered := strings.ToLower(strings.TrimSpace(summary))
	if containsAny(lowered, []string{
		"command not found",
		"executable file not found",
		"connection reset",
		"connection refused",
		"temporary failure",
		"service unavailable",
		"network is unreachable",
		"transport error",
	}) {
		return Decision{Kind: failureclass.RetryableTransient, ResumePolicy: loops.ResumePolicyReplayStep, Message: message}
	}
	return Decision{Kind: failureclass.ManualIntervention, ResumePolicy: loops.ResumePolicyManualIntervention, Message: message}
}

func containsAny(message string, hints []string) bool {
	for _, hint := range hints {
		if strings.Contains(message, hint) {
			return true
		}
	}
	return false
}
