package roles

import "testing"

func TestQueueFailureKindsAreStableDurableVocabulary(t *testing.T) {
	t.Parallel()

	want := map[QueueFailureKind]string{
		FailureRetryableTransient:   "retryable_transient",
		FailureRetryableAfterResume: "retryable_after_resume",
		FailureNonRetryable:         "non_retryable",
		FailureManualIntervention:   "manual_intervention",
	}
	for kind, value := range want {
		if string(kind) != value {
			t.Fatalf("QueueFailureKind %q = %q, want %q", kind, string(kind), value)
		}
	}
}
