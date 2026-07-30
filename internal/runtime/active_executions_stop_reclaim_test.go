package runtime

import (
	"fmt"
	"testing"
)

func (r *ActiveExecutionRegistry) stopStateSizes() (epochs, releases, stopping int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stopEpoch), len(r.stopReleases), len(r.stoppingLoops)
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

	epochs, releases, stopping := registry.stopStateSizes()
	if epochs != 0 || releases != 0 || stopping != 0 {
		t.Fatalf("stop state after churn = epochs:%d releases:%d stopping:%d, want all 0", epochs, releases, stopping)
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
	epochs, releases, stopping := registry.stopStateSizes()
	if epochs != 0 || releases != 0 || stopping != 0 {
		t.Fatalf("stop state after final clear = epochs:%d releases:%d stopping:%d, want all 0", epochs, releases, stopping)
	}
}
