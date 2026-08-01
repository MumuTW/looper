package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDrainSnapshotUsesSupervisorOwnershipRatherThanDurableRunState(t *testing.T) {
	t.Parallel()
	registry := NewActiveExecutionRegistry()
	if snapshot := registry.DrainSnapshot(); !snapshot.Drained() {
		t.Fatalf("empty snapshot = %#v", snapshot)
	}
	registry.mu.Lock()
	registry.executions["agent"] = &ownedExecution{}
	registry.pending[1] = &spawnLease{}
	registry.boundOps[2] = &operationLease{}
	registry.pendingOps[3] = &operationLease{}
	registry.mu.Unlock()
	snapshot := registry.DrainSnapshot()
	if snapshot.LiveExecutions != 1 || snapshot.PendingSpawns != 1 || snapshot.BoundOperations != 1 || snapshot.PendingOperations != 1 || snapshot.Drained() {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestWaitForDrainReturnsLatestSnapshotAtDeadlineWithoutKilling(t *testing.T) {
	t.Parallel()
	registry := NewActiveExecutionRegistry()
	registry.mu.Lock()
	registry.executions["agent"] = &ownedExecution{}
	registry.mu.Unlock()
	runtime := &Runtime{activeExecutions: registry}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	snapshot, err := runtime.WaitForDrain(ctx, time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || snapshot.LiveExecutions != 1 {
		t.Fatalf("WaitForDrain() = (%#v, %v)", snapshot, err)
	}
	if registry.LiveCount() != 1 {
		t.Fatal("WaitForDrain changed live ownership")
	}
}
