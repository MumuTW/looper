package planner

import (
	"context"
	"testing"
)

// The planner half of the takeover-versus-discovery race. The write guard is
// the authority — it decides the race inside the writing statement — but a
// rejected write leaves the caller holding a record that is stale by definition.
// materializeIssue reads that record's status to decide whether to create an
// active queue item, so returning the pre-takeover `queued` copy enqueued work
// against a loop a human owns: the refusal would have been correct and the
// outcome still wrong.

// TestPlannerRefreshReturnsPersistedRowAfterRejectedWrite stages the race
// directly: the pass holds a `queued` snapshot, takeover has since committed.
func TestPlannerRefreshReturnsPersistedRowAfterRejectedWrite(t *testing.T) {
	t.Parallel()
	f, runner := newHeldPlannerLoopFixture(t)
	ctx := context.Background()

	held, err := f.repos.Loops.GetByID(ctx, "loop_held")
	if err != nil || held == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", held, err)
	}
	// What the discovery pass read before takeover committed.
	stale := *held
	stale.Status = "queued"

	updated, err := runner.refreshIssueLoop(ctx, stale, "acme/looper", IssueSummary{Number: 42, Title: "Plan this"}, f.nowISO(), "", "")
	if err != nil {
		t.Fatalf("refreshIssueLoop() error = %v, want the hold reported, not a failure", err)
	}
	if updated.Status != "human_takeover" {
		t.Fatalf("status = %q, want human_takeover: the refused write must not hand back the stale row", updated.Status)
	}
	// The consequence the status feeds: materializeIssue must park this issue.
	if !plannerLoopSkipsMaterialization(updated.Status) {
		t.Fatalf("plannerLoopSkipsMaterialization(%q) = false; the returned record would enqueue against a held loop", updated.Status)
	}
	if _, err := f.repos.Loops.GetByID(ctx, "loop_held"); err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
}

// TestPlannerHeldLoopResultPropagatesReadFailure: losing the race is routine, a
// failed re-read is not. Reporting a storage failure as a successful skip would
// hide it behind exactly the kind of false success this change exists to remove.
func TestPlannerHeldLoopResultPropagatesReadFailure(t *testing.T) {
	t.Parallel()
	f, runner := newHeldPlannerLoopFixture(t)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := runner.heldLoopResult(cancelled, "loop_held", false); err == nil {
		t.Fatal("heldLoopResult() error = nil on a cancelled context, want the storage failure propagated")
	}
	if _, err := f.repos.Loops.GetByID(context.Background(), "loop_held"); err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
}

// TestPlannerHeldLoopResultReportsMissingRowAsBlocked: an intentionally missing
// row is not a failure, but it is also not something to materialize against.
func TestPlannerHeldLoopResultReportsMissingRowAsBlocked(t *testing.T) {
	t.Parallel()
	_, runner := newHeldPlannerLoopFixture(t)

	result, err := runner.heldLoopResult(context.Background(), "loop_missing", false)
	if err != nil {
		t.Fatalf("heldLoopResult() error = %v, want a missing row reported as blocked", err)
	}
	if !result.blocked {
		t.Fatalf("result = %#v, want blocked so materializeIssue skips", result)
	}
	if result.record.ID != "" {
		t.Fatalf("record = %#v, want empty", result.record)
	}
}
