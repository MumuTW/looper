package roles_test

import (
	"testing"

	"github.com/nexu-io/looper/internal/fixer"
	"github.com/nexu-io/looper/internal/planner"
	"github.com/nexu-io/looper/internal/reviewer"
	"github.com/nexu-io/looper/internal/roles"
	"github.com/nexu-io/looper/internal/worker"
)

func TestRoleFailureKindsShareTheCentralContract(t *testing.T) {
	t.Parallel()

	for role, kinds := range map[string]map[string]roles.QueueFailureKind{
		"fixer": {
			"retryable_transient":    fixer.FailureRetryableTransient,
			"retryable_after_resume": fixer.FailureRetryableAfterResume,
			"non_retryable":          fixer.FailureNonRetryable,
			"manual_intervention":    fixer.FailureManualIntervention,
		},
		"planner": {
			"retryable_transient":    planner.FailureRetryableTransient,
			"retryable_after_resume": planner.FailureRetryableAfterResume,
			"non_retryable":          planner.FailureNonRetryable,
			"manual_intervention":    planner.FailureManualIntervention,
		},
		"reviewer": {
			"retryable_transient":    reviewer.FailureRetryableTransient,
			"retryable_after_resume": reviewer.FailureRetryableAfterResume,
			"non_retryable":          reviewer.FailureNonRetryable,
			"manual_intervention":    reviewer.FailureManualIntervention,
		},
		"worker": {
			"retryable_transient":    worker.FailureRetryableTransient,
			"retryable_after_resume": worker.FailureRetryableAfterResume,
			"non_retryable":          worker.FailureNonRetryable,
			"manual_intervention":    worker.FailureManualIntervention,
		},
	} {
		for name, got := range kinds {
			want := roles.QueueFailureKind(name)
			if got != want {
				t.Fatalf("%s %s = %q, want shared contract %q", role, name, got, want)
			}
		}
	}
}
