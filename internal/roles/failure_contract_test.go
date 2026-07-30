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

	for name, kind := range map[string]roles.QueueFailureKind{
		"fixer":    fixer.FailureRetryableAfterResume,
		"planner":  planner.FailureRetryableAfterResume,
		"reviewer": reviewer.FailureRetryableAfterResume,
		"worker":   worker.FailureRetryableAfterResume,
	} {
		if kind != roles.FailureRetryableAfterResume {
			t.Fatalf("%s retry-after-resume kind = %q, want shared contract %q", name, kind, roles.FailureRetryableAfterResume)
		}
	}
}
