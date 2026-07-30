package storage

import "testing"

// The issue half of the hold's blast radius. loop_human_hold_target_scope_test.go
// pinned the PR key and the project over-block; this pins the issue key, which
// had the same defect in the same direction.
//
// An issue target is only a shared checkout among planner loops. Planner branches
// off the issue itself (looper/planner/<issue>-<slug>), so a replacement planner
// loop on the same issue lands on the exact checkout a takeover is holding. A
// worker's branch carries its own loop hash (looper/<issue>-<slug>-<loopHash>), so
// it shares a checkout with neither the planner nor another worker. Fencing the
// worker off a planner's takeover blocked it indefinitely for no shared resource.

const humanHoldIssueTargetID = "issue:acme/looper:73"

func (f *humanHoldFixture) seedIssueLoop(t *testing.T, id, loopType, status string, seq int64) {
	t.Helper()
	repo := "acme/looper"
	targetID := humanHoldIssueTargetID
	f.seedLoop(t, LoopRecord{
		ID: id, Seq: seq, ProjectID: "project_1", Type: loopType,
		TargetType: "issue", TargetID: &targetID, Repo: &repo,
		Status: status, CreatedAt: humanHoldNow, UpdatedAt: humanHoldNow,
	})
}

func (f *humanHoldFixture) seedIssueQueueItem(t *testing.T, id, loopID, itemType string) {
	t.Helper()
	repo := "acme/looper"
	projectID := "project_1"
	if err := f.repos.Queue.Upsert(f.ctx, QueueItemRecord{
		ID: id, ProjectID: &projectID, LoopID: &loopID, Type: itemType,
		TargetType: "issue", TargetID: humanHoldIssueTargetID, Repo: &repo,
		DedupeKey: itemType + ":" + humanHoldIssueTargetID,
		LockKey:   stringPointer(humanHoldIssueTargetID),
		Status:    "queued", Priority: 5, AvailableAt: humanHoldNow, MaxAttempts: 5,
		CreatedAt: humanHoldNow, UpdatedAt: humanHoldNow,
	}); err != nil {
		t.Fatalf("Queue.Upsert(%s) error = %v", id, err)
	}
}

// TestHumanTakeoverDoesNotHoldWorkerOnHeldPlannerIssue is the over-block. The
// planner's checkout is looper/planner/73-...; the worker's is
// looper/73-...-<loopHash>. They are different directories, the ordinary issue
// lock already serialises the two roles while one is running, and takeover
// cancels the planner's queue item — so after the hold there is nothing shared
// left to protect and the worker must still be claimable.
func TestHumanTakeoverDoesNotHoldWorkerOnHeldPlannerIssue(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)

	f.seedIssueLoop(t, "loop_held_planner", "planner", "human_takeover", 73)
	f.seedIssueLoop(t, "loop_worker", "worker", "queued", 74)
	f.seedIssueQueueItem(t, "queue_worker", "loop_worker", "worker")

	item, err := f.repos.Queue.ClaimNext(f.ctx, humanHoldNow, "scheduler")
	if err != nil {
		t.Fatalf("ClaimNext() error = %v", err)
	}
	if item == nil {
		t.Fatal("ClaimNext() = nil, want the worker item: it does not share the held planner's checkout")
	}
	if item.ID != "queue_worker" {
		t.Fatalf("ClaimNext() claimed %q, want queue_worker", item.ID)
	}
}

// TestHumanTakeoverDoesNotHoldSiblingWorkerOnHeldWorkerIssue is the same
// narrowing in the other direction: two worker loops on one issue have distinct
// loop hashes, so a held worker fences itself, not its sibling.
func TestHumanTakeoverDoesNotHoldSiblingWorkerOnHeldWorkerIssue(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)

	f.seedIssueLoop(t, "loop_held_worker", "worker", "human_takeover", 73)
	f.seedIssueLoop(t, "loop_sibling_worker", "worker", "queued", 74)
	f.seedIssueQueueItem(t, "queue_sibling_worker", "loop_sibling_worker", "worker")

	item, err := f.repos.Queue.ClaimNext(f.ctx, humanHoldNow, "scheduler")
	if err != nil {
		t.Fatalf("ClaimNext() error = %v", err)
	}
	if item == nil {
		t.Fatal("ClaimNext() = nil, want the sibling worker: worker branches carry their own loop hash")
	}
	if item.ID != "queue_sibling_worker" {
		t.Fatalf("ClaimNext() claimed %q, want queue_sibling_worker", item.ID)
	}
}

// TestHumanTakeoverHoldsSiblingPlannerOnHeldIssue is the half the narrowing must
// not cost. Two planner loops on one issue derive the same branch, so a
// replacement planner would check out the directory the human is editing.
func TestHumanTakeoverHoldsSiblingPlannerOnHeldIssue(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)

	f.seedIssueLoop(t, "loop_held_planner", "planner", "human_takeover", 73)
	f.seedIssueLoop(t, "loop_replacement_planner", "planner", "queued", 74)
	f.seedIssueQueueItem(t, "queue_replacement_planner", "loop_replacement_planner", "planner")

	item, err := f.repos.Queue.ClaimNext(f.ctx, humanHoldNow, "scheduler")
	if err != nil {
		t.Fatalf("ClaimNext() error = %v", err)
	}
	if item != nil {
		t.Fatalf("ClaimNext() = %#v, want no claim: a second planner on this issue shares looper/planner/73-...", item)
	}
}

// TestHumanTakeoverStillHoldsTheHeldWorkerIssueItem keeps the per-loop arm
// honest: scoping non-planner issue holds to the held loop is only correct if
// the held loop's own queue item is still refused.
func TestHumanTakeoverStillHoldsTheHeldWorkerIssueItem(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)

	f.seedIssueLoop(t, "loop_held_worker", "worker", "human_takeover", 73)
	f.seedIssueQueueItem(t, "queue_held_worker", "loop_held_worker", "worker")

	item, err := f.repos.Queue.ClaimNext(f.ctx, humanHoldNow, "scheduler")
	if err != nil {
		t.Fatalf("ClaimNext() error = %v", err)
	}
	if item != nil {
		t.Fatalf("ClaimNext() = %#v, want no claim on the held loop's own queue item", item)
	}
}
