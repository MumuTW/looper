package failurepolicy

import (
	"context"
	"errors"
	"strings"

	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/loops/failureclass"
	"github.com/MumuTW/looper/internal/validation"
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

// ClassifyValidation translates the shared validation execution category into
// fixer vocabulary. The shell supervision boundary owns the category; summary
// remains diagnostic and never changes policy.
func ClassifyValidation(category validation.FailureCategory, summary string) Decision {
	message := strings.TrimSpace(summary)
	if message == "" {
		message = "Validation failed"
	}
	policy := validation.PolicyFor(category)
	return Decision{Kind: failureclass.Kind(policy.FailureKind), ResumePolicy: policy.ResumePolicy, Message: message}
}
