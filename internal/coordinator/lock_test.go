package coordinator

import (
	"testing"
	"time"
)

func TestLockIssueReclaimsInactiveEntries(t *testing.T) {
	state := NewRuntimeState()
	runner := &Runner{state: state}

	unlock := runner.lockIssue("project", "Acme/Looper", 42)
	state.mu.Lock()
	if len(state.issueLocks) != 1 {
		state.mu.Unlock()
		t.Fatalf("issueLocks length = %d, want one active entry", len(state.issueLocks))
	}
	state.mu.Unlock()

	acquired := make(chan struct{})
	go func() {
		secondUnlock := runner.lockIssue("project", "acme/looper", 42)
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
