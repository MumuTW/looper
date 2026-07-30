package runtime

import (
	"fmt"
	"testing"
)

func (r *ActiveExecutionRegistry) stopStateSize() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stoppingLoops)
}

func TestStopStateReclaimedAfterTerminalLoopChurn(t *testing.T) {
	t.Parallel()

	registry := NewActiveExecutionRegistry()
	for i := 0; i < 1000; i++ {
		loopID := fmt.Sprintf("loop_churn_%d", i)
		release, err := registry.BeginLoopStop(loopID, "churn test")
		if err != nil {
			t.Fatalf("BeginLoopStop(%s) error = %v", loopID, err)
		}
		release()

		// The intentional-reactivation path must also leave nothing behind.
		release, err = registry.BeginLoopStop(loopID, "churn test reactivation")
		if err != nil {
			t.Fatalf("BeginLoopStop(%s) second error = %v", loopID, err)
		}
		registry.ClearLoopStop(loopID)
		release()
	}

	if size := registry.stopStateSize(); size != 0 {
		t.Fatalf("stop state after churn = %d entries, want 0", size)
	}
}

func TestStaleReleaseCannotClearRestoredGateAcrossReclaim(t *testing.T) {
	t.Parallel()

	registry := NewActiveExecutionRegistry()
	const loopID = "loop_restore_guarded"

	// Temporary stop window captures epoch zero.
	release, err := registry.BeginLoopStop(loopID, "temporary window")
	if err != nil {
		t.Fatalf("BeginLoopStop() error = %v", err)
	}

	// Intentional reactivation, then a failed reactivation restores the gate.
	if wasActive := registry.ClearLoopStop(loopID); !wasActive {
		t.Fatal("ClearLoopStop() = false, want active gate")
	}
	if err := registry.RestoreLoopStop(loopID); err != nil {
		t.Fatalf("RestoreLoopStop() error = %v", err)
	}

	// The outdated temporary release must not reopen the restored gate, and
	// its epoch entry must not have been reclaimed while it was outstanding.
	release()
	if !registry.LoopStopActive(loopID) {
		t.Fatal("LoopStopActive() = false after stale release, want restored gate still closed")
	}

	// Final intentional reactivation reclaims every stop-state entry.
	if wasActive := registry.ClearLoopStop(loopID); !wasActive {
		t.Fatal("final ClearLoopStop() = false, want active gate")
	}
	if size := registry.stopStateSize(); size != 0 {
		t.Fatalf("stop state after final clear = %d entries, want 0", size)
	}
}

func TestTerminalStickyStopRetainsExactlyOneEntry(t *testing.T) {
	t.Parallel()

	registry := NewActiveExecutionRegistry()
	// A successful terminal close abandons its release and never reactivates:
	// the sticky gate entry is required forever, but it must be the only
	// per-loop stop state retained.
	for i := 0; i < 500; i++ {
		loopID := fmt.Sprintf("loop_terminal_%d", i)
		if _, err := registry.BeginLoopStop(loopID, "terminal close"); err != nil {
			t.Fatalf("BeginLoopStop(%s) error = %v", loopID, err)
		}
		if !registry.LoopStopActive(loopID) {
			t.Fatalf("LoopStopActive(%s) = false, want sticky gate closed", loopID)
		}
	}

	if size := registry.stopStateSize(); size != 500 {
		t.Fatalf("stop state after 500 terminal closes = %d entries, want exactly 500 sticky gates", size)
	}
}

func TestAbandonedStickyStopReleaseIsRetiredByClear(t *testing.T) {
	t.Parallel()

	registry := NewActiveExecutionRegistry()
	const loopID = "loop_durable_pause"

	// haltLoop's durable pause keeps the returned release uncalled on purpose:
	// the sticky gate must survive until an intentional reactivation.
	release, err := registry.BeginLoopStop(loopID, "durable pause")
	if err != nil {
		t.Fatalf("BeginLoopStop() error = %v", err)
	}
	if !registry.LoopStopActive(loopID) {
		t.Fatal("LoopStopActive() = false, want sticky gate closed")
	}

	// Intentional reactivation must retire the abandoned closure's state even
	// though that closure never runs.
	if wasActive := registry.ClearLoopStop(loopID); !wasActive {
		t.Fatal("ClearLoopStop() = false, want active gate")
	}
	if size := registry.stopStateSize(); size != 0 {
		t.Fatalf("stop state after reactivation = %d entries, want 0", size)
	}

	// If the abandoned closure does run later (defensive), it must be a no-op
	// against a fresh generation.
	again, err := registry.BeginLoopStop(loopID, "fresh generation")
	if err != nil {
		t.Fatalf("BeginLoopStop(fresh) error = %v", err)
	}
	release()
	if !registry.LoopStopActive(loopID) {
		t.Fatal("LoopStopActive() = false: abandoned stale release cleared a fresh generation's gate")
	}
	again()
	if size := registry.stopStateSize(); size != 0 {
		t.Fatalf("stop state after fresh release = %d entries, want 0", size)
	}
}
