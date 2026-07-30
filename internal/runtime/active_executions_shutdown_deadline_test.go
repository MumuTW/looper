package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/storage"
)

func TestBeginShutdownUsesOneDeadlineAndReportsEveryUnfinishedOwner(t *testing.T) {
	reg := NewActiveExecutionRegistry()
	reg.killTimeout = 40 * time.Millisecond

	pendingSpawn, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-spawn", RunID: "run-spawn", ExecutionID: "exec-spawn",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn(pending): %v", err)
	}
	_ = pendingSpawn

	rebindingSpawn, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: "loop-rebind", RunID: "run-rebind", ExecutionID: "exec-rebind",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn(rebinding): %v", err)
	}
	rebindingLease := rebindingSpawn.(*spawnLease)
	reg.mu.Lock()
	delete(reg.pending, rebindingLease.id)
	reg.active[rebindingLease.id] = rebindingLease
	rebindingLease.rebinding = true
	rebindingLease.rebindDone = make(chan struct{})
	reg.mu.Unlock()
	rebindingLease.closeSpawnDone()

	if _, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "pending-worker"}); err != nil {
		t.Fatalf("AdmitOperation(pending): %v", err)
	}
	boundOperation, err := reg.AdmitOperation(context.Background(), OperationMeta{ClaimedBy: "scheduler"})
	if err != nil {
		t.Fatalf("AdmitOperation(bound): %v", err)
	}
	loopID := "loop-bound"
	if _, err := boundOperation.BindClaim(storage.QueueItemRecord{ID: "queue-bound", LoopID: &loopID}); err != nil {
		t.Fatalf("BindClaim: %v", err)
	}

	started := time.Now()
	err = reg.BeginShutdown("test deadline")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("BeginShutdown() error = nil, want unfinished ownership diagnostics")
	}
	if elapsed > 120*time.Millisecond {
		t.Fatalf("BeginShutdown() elapsed = %v, want one 40ms budget plus scheduling tolerance", elapsed)
	}

	message := err.Error()
	for _, want := range []string{
		"pending-spawn(loop=loop-spawn run=run-spawn execution=exec-spawn)",
		"rebind(loop=loop-rebind run=run-rebind execution=exec-rebind)",
		"pending-operation(lease=1 claimedBy=pending-worker)",
		"bound-operation(lease=2 queueItem=queue-bound loop=loop-bound)",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("BeginShutdown() error = %q, want unfinished owner %q", message, want)
		}
	}
}
