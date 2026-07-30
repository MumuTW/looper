package runtime

import (
	"fmt"
	"testing"
)

func (r *ActiveExecutionRegistry) stopStateSizes() (gates, stopping int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stopGates), len(r.stoppingLoops)
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

	gates, stopping := registry.stopStateSizes()
	if gates != 0 || stopping != 0 {
		t.Fatalf("stop state after churn = gates:%d stopping:%d, want all 0", gates, stopping)
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
	gates, stopping := registry.stopStateSizes()
	if gates != 0 || stopping != 0 {
		t.Fatalf("stop state after final clear = gates:%d stopping:%d, want all 0", gates, stopping)
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
	gates, stopping := registry.stopStateSizes()
	if gates != 0 || stopping != 0 {
		t.Fatalf("stop state after reactivation = gates:%d stopping:%d, want all 0", gates, stopping)
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
	gates, stopping = registry.stopStateSizes()
	if gates != 0 || stopping != 0 {
		t.Fatalf("stop state after fresh release = gates:%d stopping:%d, want all 0", gates, stopping)
	}
}
