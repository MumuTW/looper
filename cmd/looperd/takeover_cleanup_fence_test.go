package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	looperdapi "github.com/MumuTW/looper/internal/api"
	looperdruntime "github.com/MumuTW/looper/internal/runtime"
	"github.com/MumuTW/looper/internal/storage"
)

// TestTakeoverAcquiresRequeueGuardBeforePreflightReads is the cross-component
// invariant for the shared per-loop requeue guard between takeoverLoop
// (cmd/looperd) and terminal Fixer cleanup (internal/fixer.cleanupFixerWorktreeIfTerminal).
//
// Terminal cleanup takes LockLoopRequeue before it reads the loop and removes the
// checkout, and skips deletion when the loop is human_takeover. Before this fix
// takeoverLoop read the latest agent execution (the worktree path a human must
// resume) and ran loadHaltPreflight *before* acquiring that guard, then took it
// only around Hold. A terminal cleanup that reached its critical section in that
// gap could acquire the guard first, observe the loop before human_takeover,
// delete the checkout, and leave takeover holding a loop whose worktree path is
// now missing.
//
// The fix acquires the guard before the preflight reads and releases it
// immediately after Hold. This test pins that ordering: while the guard is held,
// takeover must not have read the execution's CWD yet, so a mutation made while
// takeover is parked on the guard is the value takeover returns. Under the old
// ordering the preflight read already completed before takeover parked, so the
// stale (pre-mutation) path would be returned.
func TestTakeoverAcquiresRequeueGuardBeforePreflightReads(t *testing.T) {
	ctx := context.Background()
	f := newTakeoverFixture(t)

	// Two distinct worktree paths: the durable execution starts with the
	// pre-takeover path, and the test rewrites it while takeover is parked on
	// the guard. A takeover that reads the execution after acquiring the guard
	// returns the rewritten path; one that read before parking returns the
	// original.
	preTakeoverPath := filepath.Join(t.TempDir(), "looper-fix-project_1-pr-41-detached")
	rewrittenPath := filepath.Join(t.TempDir(), "looper-fix-project_1-pr-41-rewritten")
	if err := os.MkdirAll(preTakeoverPath, 0o755); err != nil {
		t.Fatalf("os.MkdirAll(preTakeoverPath) error = %v", err)
	}
	if err := os.MkdirAll(rewrittenPath, 0o755); err != nil {
		t.Fatalf("os.MkdirAll(rewrittenPath) error = %v", err)
	}
	exec, err := f.repos.AgentExecutions.GetLatestByLoopID(ctx, f.loopID)
	if err != nil || exec == nil {
		t.Fatalf("AgentExecutions.GetLatestByLoopID() = (%#v, %v)", exec, err)
	}
	exec.CWD = &preTakeoverPath
	if err := f.repos.AgentExecutions.Upsert(ctx, *exec); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	// Hold the shared guard so takeover parks on it before its critical section.
	testUnlock := looperdruntime.LockLoopRequeue(f.loopID)

	type takeoverOutcome struct {
		result looperdapi.TakeoverResult
		err    error
	}
	done := make(chan takeoverOutcome, 1)
	go func() {
		result, err := takeoverLoop(ctx, f.services, f.loopID, "Taken over by test", func() time.Time { return f.now }, func(int, syscall.Signal) error { return nil }, func(context.Context, storage.AgentExecutionRecord, int) (bool, bool, error) {
			return true, true, nil
		})
		done <- takeoverOutcome{result: result, err: err}
	}()

	// Let the takeover goroutine schedule and, under the old ordering, complete
	// its preflight reads before parking on the guard. The preflight is two
	// local SQLite reads, so this window only needs to cover scheduling + those
	// reads; under the new ordering takeover parks on the guard immediately.
	runtime.Gosched()
	time.Sleep(100 * time.Millisecond)

	// Rewrite the execution's worktree path while takeover is parked on the
	// guard. A takeover that has not yet read the execution sees this value.
	rewritten, err := f.repos.AgentExecutions.GetLatestByLoopID(ctx, f.loopID)
	if err != nil || rewritten == nil {
		t.Fatalf("AgentExecutions.GetLatestByLoopID() = (%#v, %v)", rewritten, err)
	}
	rewritten.CWD = &rewrittenPath
	if err := f.repos.AgentExecutions.Upsert(ctx, *rewritten); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	// Release the guard: takeover acquires it, runs its preflight under the
	// guard, commits Hold, and returns.
	testUnlock()

	select {
	case outcome := <-done:
		if outcome.err != nil {
			t.Fatalf("takeoverLoop() error = %v", outcome.err)
		}
		if outcome.result.WorktreePath != rewrittenPath {
			t.Fatalf("takeoverLoop().WorktreePath = %q, want %q: takeover must read the execution after acquiring the guard so a concurrent cleanup cannot delete the checkout between the preflight read and Hold", outcome.result.WorktreePath, rewrittenPath)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("takeoverLoop() did not complete after the guard was released")
	}

	// The durable hold committed under the guard.
	loop, err := f.repos.Loops.GetByID(ctx, f.loopID)
	if err != nil || loop == nil || loop.Status != "human_takeover" {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want human_takeover committed under the guard", loop, err)
	}
}
