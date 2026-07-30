package storage

import (
	"context"
	"testing"
)

// The claim lease is written once at claim time and then outlived by its own
// run — #149's "the lease is decorative" finding. Renewing it from the run's
// heartbeat is what makes "expired lease" mean something.
func TestRefreshClaimLeaseForRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestSQLiteDB(t)
	runner := NewMigrationRunner(db, MigrationRunnerOptions{Migrations: EmbeddedMigrations})
	if _, err := runner.RunPending(ctx); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := NewRepositories(db)

	const (
		claimedAt = "2026-07-30T12:00:00.000Z"
		expiresAt = "2026-07-30T12:05:00.000Z"
		renewedAt = "2026-07-30T12:04:00.000Z"
		renewedTo = "2026-07-30T12:19:00.000Z"
		lockKey   = "pr:MumuTW/looper:126"
	)
	projectID := "project_lease"
	loopID := "loop_lease"
	runID := "run_lease"
	queueID := "queue_lease"

	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: claimedAt, UpdatedAt: claimedAt}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", Status: "running", CreatedAt: claimedAt, UpdatedAt: claimedAt}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(ctx, RunRecord{ID: runID, LoopID: loopID, Status: "running", StartedAt: claimedAt, CreatedAt: claimedAt, UpdatedAt: claimedAt}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	lockKeyValue := lockKey
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "fixer", TargetType: "pull_request",
		TargetID: "pr:MumuTW/looper:126", DedupeKey: "fixer:" + loopID, Priority: QueuePriorityFixer,
		Status: "running", AvailableAt: claimedAt, MaxAttempts: 3, LockKey: &lockKeyValue,
		CreatedAt: claimedAt, UpdatedAt: claimedAt,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	reason := "fixer-claim"
	acquired, err := repos.Locks.Acquire(ctx, LockRecord{Key: lockKey, Owner: queueID, Reason: &reason, ExpiresAt: expiresAt, CreatedAt: claimedAt, UpdatedAt: claimedAt})
	if err != nil || !acquired {
		t.Fatalf("Locks.Acquire() = %v, %v", acquired, err)
	}

	refreshed, err := repos.RefreshClaimLeaseForRun(ctx, runID, renewedTo, renewedAt)
	if err != nil {
		t.Fatalf("RefreshClaimLeaseForRun() error = %v", err)
	}
	if !refreshed {
		t.Fatal("RefreshClaimLeaseForRun() = false, want the held lease renewed")
	}
	lock, err := repos.Locks.Get(ctx, lockKey)
	if err != nil || lock == nil {
		t.Fatalf("Locks.Get() = %#v, %v", lock, err)
	}
	if lock.ExpiresAt != renewedTo {
		t.Fatalf("lock.ExpiresAt = %q, want %q", lock.ExpiresAt, renewedTo)
	}

	// A heartbeat must never take a lease it does not already hold.
	if err := repos.Locks.Release(ctx, lockKey); err != nil {
		t.Fatalf("Locks.Release() error = %v", err)
	}
	stolen := "someone-else"
	if acquired, err := repos.Locks.Acquire(ctx, LockRecord{Key: lockKey, Owner: stolen, Reason: &reason, ExpiresAt: expiresAt, CreatedAt: claimedAt, UpdatedAt: claimedAt}); err != nil || !acquired {
		t.Fatalf("Locks.Acquire(other owner) = %v, %v", acquired, err)
	}
	refreshed, err = repos.RefreshClaimLeaseForRun(ctx, runID, renewedTo, renewedAt)
	if err != nil {
		t.Fatalf("RefreshClaimLeaseForRun(other owner) error = %v", err)
	}
	if refreshed {
		t.Fatal("RefreshClaimLeaseForRun() = true for a lock held by someone else")
	}
	lock, err = repos.Locks.Get(ctx, lockKey)
	if err != nil || lock == nil {
		t.Fatalf("Locks.Get() = %#v, %v", lock, err)
	}
	if lock.ExpiresAt != expiresAt || lock.Owner != stolen {
		t.Fatalf("lock = %#v, want the other owner's lease untouched", lock)
	}

	// A run with no active claim is a no-op, not an error.
	refreshed, err = repos.RefreshClaimLeaseForRun(ctx, "run_that_does_not_exist", renewedTo, renewedAt)
	if err != nil || refreshed {
		t.Fatalf("RefreshClaimLeaseForRun(unknown run) = %v, %v; want false, nil", refreshed, err)
	}
}
