package coordinator

import (
	"context"
	"testing"
	"time"
)

func TestLockIssueReclaimsInactiveEntries(t *testing.T) {
	state := NewRuntimeState()
	runner := &Runner{state: state}

	unlock, locked := runner.lockIssue(context.Background(), "project", "Acme/Looper", 42)
	if !locked {
		t.Fatal("first lock was not acquired")
	}
	state.mu.Lock()
	if len(state.issueLocks) != 1 {
		state.mu.Unlock()
		t.Fatalf("issueLocks length = %d, want one active entry", len(state.issueLocks))
	}
	state.mu.Unlock()

	acquired := make(chan struct{})
	go func() {
		secondUnlock, ok := runner.lockIssue(context.Background(), "project", "acme/looper", 42)
		if !ok {
			return
		}
		close(acquired)
		secondUnlock()
	}()
	select {
	case <-acquired:
		t.Fatal("second lock acquired before first unlock")
	case <-time.After(20 * time.Millisecond):
	}

	unlock()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("second lock did not acquire after first unlock")
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.issueLocks) != 0 {
		t.Fatalf("issueLocks length = %d, want inactive entry reclaimed", len(state.issueLocks))
	}
}

func TestLockIssueHonorsCancellationWhileWaiting(t *testing.T) {
	state := NewRuntimeState()
	runner := &Runner{state: state}

	unlock, locked := runner.lockIssue(context.Background(), "project", "acme/looper", 42)
	if !locked {
		t.Fatal("first lock was not acquired")
	}
	defer unlock()

	ctx, cancel := context.WithCancel(context.Background())
	acquired := make(chan bool, 1)
	go func() {
		secondUnlock, ok := runner.lockIssue(ctx, "project", "acme/looper", 42)
		if ok {
			secondUnlock()
		}
		acquired <- ok
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case ok := <-acquired:
		if ok {
			t.Fatal("canceled waiter acquired issue lock")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter did not return")
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.issueLocks) != 1 {
		t.Fatalf("issueLocks length = %d, want owner entry retained", len(state.issueLocks))
	}
}
