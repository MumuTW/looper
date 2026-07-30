package runtime

import (
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

// The park reasons below were read off a live 44MB looper.sqlite that had been
// accumulating quarantine debt across several daemon restarts. They are the
// authority for this predicate, not the strings current source happens to
// build: two of the releasable phrasings are no longer produced by any code
// path, and they are what held the oldest parks in the field.
func TestQuarantineParkIsReleasableAgainstObservedFieldReasons(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		lastError string
		want      bool
	}{
		{
			name:      "current startup quarantine phrasing",
			lastError: "needs confirmation: startup liveness evidence is not authoritative (pid_not_running_not_confirmed_dead)",
			want:      true,
		},
		{
			name:      "pre-#149 phrasing, pid probe",
			lastError: "startup recovery: uncertain (pid_not_running_not_confirmed_dead); no PID Authority",
			want:      true,
		},
		{
			name:      "pre-#149 phrasing, identity mismatch",
			lastError: "startup recovery: uncertain (process_identity_mismatch); no PID Authority",
			want:      true,
		},
		{
			name:      "domain hold: risky conflict",
			lastError: "Skipped Neuverse-ai/novel#785 because risky conflict fixes require manual intervention",
			want:      false,
		},
		{
			name:      "domain hold: dirty worktree",
			lastError: "Fixer worktree is dirty for branch task/enforceable-glossary; manual intervention required",
			want:      false,
		},
		{
			name:      "domain hold: unparseable agent result",
			lastError: "Fixer agent completed without a valid structured result (parse status: missing)",
			want:      false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			loop := storage.LoopRecord{Status: "paused"}
			queue := &storage.QueueItemRecord{
				LastError:     stringPtr(test.lastError),
				LastErrorKind: stringPtr("manual_intervention"),
			}
			if got := quarantineParkIsReleasable(loop, queue); got != test.want {
				t.Fatalf("quarantineParkIsReleasable(%q) = %v, want %v", test.lastError, got, test.want)
			}
		})
	}
}

// A human takeover is never releasable regardless of how its queue item failed.
func TestQuarantineParkIsReleasableRefusesHumanTakeover(t *testing.T) {
	t.Parallel()

	loop := storage.LoopRecord{Status: "human_takeover"}
	queue := &storage.QueueItemRecord{
		LastError:     stringPtr("startup recovery: uncertain (pid_not_running_not_confirmed_dead); no PID Authority"),
		LastErrorKind: stringPtr("manual_intervention"),
	}
	if quarantineParkIsReleasable(loop, queue) {
		t.Fatal("quarantineParkIsReleasable() = true for human_takeover, want false")
	}
}
