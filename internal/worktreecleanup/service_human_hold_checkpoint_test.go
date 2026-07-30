package worktreecleanup

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

// The takeover-to-cleanup lifecycle for the shape the hold actually has to
// survive. Round one added a held-status check to the *loop* pass, which only
// fires when loop metadata names the worktree — true for planner and project
// worker loops, false for fixer and reviewer, whose checkout is recorded in the
// run checkpoint instead. The run pass could match the checkpoint but blocked
// only a `running` run, and takeover's first act is to cancel the queue item and
// drive the run terminal. So for the common case the protection was inert: age
// the checkout past retention and background cleanup would delete the directory
// the human had just been handed.

// heldCheckpointLoop seeds a fixer-shaped loop: no worktree in loop metadata, so
// only the run checkpoint can associate it with a checkout.
func (f *cleanupFixture) heldCheckpointLoop(id, status string, updatedAt time.Time) {
	f.t.Helper()
	f.seq++
	metadata := `{}`
	record := storage.LoopRecord{
		ID: id, Seq: f.seq, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Status: status, MetadataJSON: &metadata,
		CreatedAt: iso(updatedAt), UpdatedAt: iso(updatedAt),
	}
	// Only `looper takeover` may create a hold; seed it the way takeover does.
	write := f.repos.Loops.Upsert
	if status == "human_takeover" {
		write = f.repos.Loops.UpsertChangingHumanHold
	}
	if err := write(context.Background(), record); err != nil {
		f.t.Fatalf("seed loop %s (status %s) error = %v", id, status, err)
	}
}

func (f *cleanupFixture) worktreePath(id string) string {
	return filepath.Join(f.worktreeRoot, id)
}

// checkpointNaming reproduces the real fixer/reviewer checkpoint shape, which is
// the whole point of this file: those checkpoints carry `path` and `branch` and
// no worktree id, so the reference can only be resolved by path. A synthetic
// id-bearing checkpoint would match through an easier route than production ever
// takes.
func checkpointNaming(worktreePath, branch string) string {
	return `{"worktree":{"path":"` + worktreePath + `","branch":"` + branch + `","preparedAt":"` + iso(time.Date(2026, time.April, 20, 12, 0, 0, 0, time.UTC)) + `"}}`
}

// TestPlanProtectsCheckpointDerivedWorktreeOfHeldLoop is the invariant the
// committed wording promises: after takeover, the held loop's checkout stays out
// of cleanup even once every runtime reference has gone terminal and the
// directory has aged past retention.
func TestPlanProtectsCheckpointDerivedWorktreeOfHeldLoop(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	stale := f.now.Add(-30 * 24 * time.Hour)

	f.worktree("wt_held_fixer", "looper-fix-project_1-pr-41-detached", stale)
	f.heldCheckpointLoop("loop_held_fixer", "human_takeover", stale)
	// Takeover cancelled the queue item and the run settled terminal, so neither
	// the "active queue item" nor the "running run" guard applies any more.
	f.run("run_held_fixer", "loop_held_fixer", "completed", checkpointNaming(f.worktreePath("wt_held_fixer"), "looper-fix-project_1-pr-41-detached"), stale)
	f.queue("queue_held_fixer", "loop_held_fixer", "cancelled", stale)

	result, err := f.service().Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	assertDecision(t, result, "wt_held_fixer", ActionSkipped, "referenced by protected loop status human_takeover")
}

// TestPlanStillReclaimsCheckpointDerivedWorktreeOfUnheldLoop is what the fix
// must not cost. Applying the full protectsLoopStatus set to checkpoint-derived
// references would have blocked every fixer and reviewer checkout forever —
// those loops sit in idle/queued/paused between runs, all of which are
// "protected" — turning retention cleanup into a no-op for exactly the loops it
// exists to serve. Only the human hold blocks here.
func TestPlanStillReclaimsCheckpointDerivedWorktreeOfUnheldLoop(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	stale := f.now.Add(-30 * 24 * time.Hour)

	f.worktree("wt_idle_fixer", "looper-fix-project_1-pr-42-detached", stale)
	f.heldCheckpointLoop("loop_idle_fixer", "idle", stale)
	f.run("run_idle_fixer", "loop_idle_fixer", "completed", checkpointNaming(f.worktreePath("wt_idle_fixer"), "looper-fix-project_1-pr-42-detached"), stale)

	result, err := f.service().Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	assertDecision(t, result, "wt_idle_fixer", ActionWouldClean, "eligible in dry-run plan")
}
