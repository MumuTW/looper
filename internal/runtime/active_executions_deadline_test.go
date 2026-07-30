package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/processcontainment"
)

// TestShutdownDeadlineSingleBudget verifies that BeginShutdown computes one
// deadline at the start and shares it across all shutdown phases, so the
// total elapsed time stays within one killBudget (not one per waiting channel).
func TestShutdownDeadlineSingleBudget(t *testing.T) {
	t.Parallel()

	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 100 * time.Millisecond

	const numLeases = 5
	for i := 0; i < numLeases; i++ {
		_, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
			LoopID:      "loop-sd",
			RunID:       "run-sd",
			ExecutionID: "exec-sd",
		})
		if err != nil {
			t.Fatalf("AdmitSpawn: %v", err)
		}
	}

	start := time.Now()
	err := reg.BeginShutdown("test")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("BeginShutdown should time out with never-closing channels")
	}

	maxBudget := 200 * time.Millisecond
	if elapsed > maxBudget {
		t.Fatalf("BeginShutdown took %v for %d pending leases, expected <= %v (single deadline)", elapsed, numLeases, maxBudget)
	}
}

// TestLoopStopDeadlineSingleBudget verifies the same invariant for per-loop stops.
func TestLoopStopDeadlineSingleBudget(t *testing.T) {
	t.Parallel()

	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 100 * time.Millisecond

	const numLeases = 5
	for i := 0; i < numLeases; i++ {
		_, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
			LoopID:      "loop-ls",
			RunID:       "run-ls",
			ExecutionID: "exec-ls",
		})
		if err != nil {
			t.Fatalf("AdmitSpawn: %v", err)
		}
	}

	lease, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID:      "loop-ls",
		RunID:       "run-ls",
		ExecutionID: "exec-ls",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}
	lease.BindHandle(&processcontainment.Handle{}, func(string) error { return nil })

	start := time.Now()
	_, err = reg.BeginLoopStop("loop-ls", "test")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("BeginLoopStop should time out with never-closing channels")
	}

	maxBudget := 200 * time.Millisecond
	if elapsed > maxBudget {
		t.Fatalf("BeginLoopStop took %v for %d pending leases, expected <= %v (single deadline)", elapsed, numLeases, maxBudget)
	}
}
