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

// An operator who pauses a loop that was already quarantine-parked leaves no
// trace in the queue item: CancelByLoop only rewrites queued/running items, so
// the manual_intervention item and its text survive untouched and the loop reads
// `paused` either way. The one thing that does move is the loop's own
// updated_at, and that is the newer decision this settlement must not override.
func TestQuarantineParkIsReleasablePreservesAnOperatorPauseAfterTheQuarantine(t *testing.T) {
	t.Parallel()

	const parkedAt = "2026-07-29T16:33:00.000Z"
	quarantineFailure := &storage.QueueItemRecord{
		LastError:     stringPtr("startup recovery: uncertain (pid_not_running_not_confirmed_dead); no PID Authority"),
		LastErrorKind: stringPtr("manual_intervention"),
		UpdatedAt:     parkedAt,
	}

	// The park itself: recovery wrote the loop and failed the queue item at the
	// same instant.
	if !quarantineParkIsReleasable(storage.LoopRecord{Status: "paused", UpdatedAt: parkedAt}, quarantineFailure) {
		t.Fatal("quarantineParkIsReleasable(untouched park) = false, want the park released")
	}
	// A loop paused again later carries someone else's intent.
	if quarantineParkIsReleasable(storage.LoopRecord{Status: "paused", UpdatedAt: "2026-07-30T09:00:00.000Z"}, quarantineFailure) {
		t.Fatal("quarantineParkIsReleasable(operator pause after the park) = true, want the newer decision preserved")
	}
}

// The release is two writes — the loop, then its replacement queue item — and a
// crash between them leaves the loop queued with the old manual_intervention
// item still latest. The next boot must finish that, because it runs before the
// pass that would otherwise normalize the loop back out of queued, and the
// one-shot marker means there is no third chance.
func TestQuarantineParkIsReleasableFinishesAPartialRelease(t *testing.T) {
	t.Parallel()

	partial := &storage.QueueItemRecord{
		LastError:     stringPtr("needs confirmation: startup liveness evidence is not authoritative (pid_not_running_not_confirmed_dead)"),
		LastErrorKind: stringPtr("manual_intervention"),
		UpdatedAt:     "2026-07-29T16:33:00.000Z",
	}
	loop := storage.LoopRecord{Status: "queued", UpdatedAt: "2026-07-30T10:00:00.000Z"}
	if !quarantineParkIsReleasable(loop, partial) {
		t.Fatal("quarantineParkIsReleasable(partial release) = false, want the interrupted release finished")
	}
	// Only quarantine text qualifies; an ordinary queued loop is not adopted.
	partial.LastError = stringPtr("Skipped because risky conflict fixes require manual intervention")
	if quarantineParkIsReleasable(loop, partial) {
		t.Fatal("quarantineParkIsReleasable(queued, domain hold) = true, want false")
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
