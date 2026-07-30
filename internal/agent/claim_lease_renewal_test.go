package agent

import (
	"context"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

// A legitimately silent agent must keep its claim.
//
// Renewing only from onOutput ties the lease to chatter rather than to life: a
// role whose idle timeout exceeds the lease TTL can be working normally and
// still lose its lock, at which point Locks.Acquire hands the same target to
// another owner and two agents overlap. The supervisor loop that waits on the
// process is the right renewal site — it runs for exactly as long as the
// process does, and a silent agent that has genuinely stalled is still killed
// by its own idle timeout.
// Deliberately not parallel: this test drives a real process and a 50ms write
// loop, and its neighbours here assert wall-clock budgets (kill escalation must
// finish inside 500ms). Running it in the serial phase keeps its load off their
// measurements.
func TestExecutorRenewsClaimLeaseForASilentLiveExecution(t *testing.T) {
	ctx := context.Background()
	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())

	const (
		startedAt = "2026-07-30T12:00:00.000Z"
		expiresAt = "2026-07-30T12:05:00.000Z"
		lockKey   = "pr:MumuTW/looper:205"
		projectID = "project_silent"
		loopID    = "loop_silent"
		runID     = "run_silent"
		queueID   = "queue_silent"
	)
	projectIDValue, loopIDValue, lockKeyValue, reason := projectID, loopID, lockKey, "worker-claim"

	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", StartedAt: startedAt, CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectIDValue, LoopID: &loopIDValue, Type: "worker", TargetType: "project",
		TargetID: projectID, DedupeKey: "worker:" + loopID, Priority: 1, Status: "running",
		AvailableAt: startedAt, MaxAttempts: 3, LockKey: &lockKeyValue, CreatedAt: startedAt, UpdatedAt: startedAt,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	if acquired, err := repos.Locks.Acquire(ctx, storage.LockRecord{Key: lockKey, Owner: queueID, Reason: &reason, ExpiresAt: expiresAt, CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil || !acquired {
		t.Fatalf("Locks.Acquire() = %v, %v", acquired, err)
	}

	// Produces no output at all, then exits: nothing here can drive a renewal
	// through onOutput.
	custom := config.AgentVendor("custom")
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{Vendor: custom, Params: map[string]any{
			"command": "/bin/sh", "args": []any{"-c", "sleep 1"},
		}},
		Repos:             repos,
		ParamsOwnerVendor: &custom,
		// Scoped to this executor: a package-level override would put every
		// other execution in this package on a 50ms write timer too.
		ClaimLeaseRenewalInterval: 50 * time.Millisecond,
	})

	execution, err := executor.Start(ctx, RunInput{
		ExecutionID: "execution_silent", RunID: runID, LoopID: loopID, ProjectID: projectID,
		WorkingDirectory: t.TempDir(), Prompt: "ignored", Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := execution.Wait(ctx); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}

	lock, err := repos.Locks.Get(ctx, lockKey)
	if err != nil || lock == nil {
		t.Fatalf("Locks.Get() = %#v, %v", lock, err)
	}
	if lock.ExpiresAt <= expiresAt {
		t.Fatalf("lock.ExpiresAt = %q, want it renewed past %q while the silent agent was live", lock.ExpiresAt, expiresAt)
	}
	if lock.Owner != queueID {
		t.Fatalf("lock.Owner = %q, want the original claim to still hold it", lock.Owner)
	}
}
