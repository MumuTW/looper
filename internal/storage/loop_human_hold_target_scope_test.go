package storage

import "testing"

// The hold's blast radius. loop_human_hold_claim_test.go covers what the hold
// must fence; this covers what it must NOT, which round one got wrong in two
// directions: a PR key that ignored `repo`, and a project target treated as if
// it were a shared checkout.

func (f *humanHoldFixture) seedProjectLoop(t *testing.T, id, status string, seq int64) {
	t.Helper()
	targetID := "project_1"
	f.seedLoop(t, LoopRecord{
		ID: id, Seq: seq, ProjectID: "project_1", Type: "worker",
		TargetType: "project", TargetID: &targetID,
		Status: status, CreatedAt: humanHoldNow, UpdatedAt: humanHoldNow,
	})
}

func (f *humanHoldFixture) seedProjectQueueItem(t *testing.T, id, loopID string) {
	t.Helper()
	projectID := "project_1"
	if err := f.repos.Queue.Upsert(f.ctx, QueueItemRecord{
		ID: id, ProjectID: &projectID, LoopID: &loopID, Type: "worker",
		TargetType: "project", TargetID: "project_1",
		DedupeKey: "worker:" + loopID, LockKey: stringPointer("worker:" + loopID),
		Status: "queued", Priority: 5, AvailableAt: humanHoldNow, MaxAttempts: 5,
		CreatedAt: humanHoldNow, UpdatedAt: humanHoldNow,
	}); err != nil {
		t.Fatalf("Queue.Upsert(%s) error = %v", id, err)
	}
}

func stringPointer(value string) *string { return &value }

// TestHumanTakeoverDoesNotHoldOtherRepoWithSamePRNumber pins `repo` into the
// pull-request half of the shared-worktree key. The checkout is
// looper-fix-<project>-pr-<N>-detached *in a given repo*; matching on
// project + pr_number alone made a hold on acme/looper#41 fence acme/vela#41,
// which is a different checkout entirely. The reviewer/fixer blocker predicate
// in the same query already compared both, so the two disagreed about what a
// PR target is.
func TestHumanTakeoverDoesNotHoldOtherRepoWithSamePRNumber(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)

	f.seedPRLoop(t, "loop_held_fixer", "fixer", "human_takeover", 41)

	otherRepo := "acme/vela"
	otherTarget := "pr:acme/vela:41"
	otherPR := int64(41)
	f.seedLoop(t, LoopRecord{
		ID: "loop_other_repo", Seq: 43, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", TargetID: &otherTarget, Repo: &otherRepo, PRNumber: &otherPR,
		Status: "queued", CreatedAt: humanHoldNow, UpdatedAt: humanHoldNow,
	})
	projectID := "project_1"
	otherLoopID := "loop_other_repo"
	if err := f.repos.Queue.Upsert(f.ctx, QueueItemRecord{
		ID: "queue_other_repo", ProjectID: &projectID, LoopID: &otherLoopID, Type: "fixer",
		TargetType: "pull_request", TargetID: otherTarget, Repo: &otherRepo, PRNumber: &otherPR,
		DedupeKey: "fixer:acme/vela:41", Status: "queued", Priority: 5,
		AvailableAt: humanHoldNow, MaxAttempts: 5, CreatedAt: humanHoldNow, UpdatedAt: humanHoldNow,
	}); err != nil {
		t.Fatalf("Queue.Upsert(queue_other_repo) error = %v", err)
	}

	item, err := f.repos.Queue.ClaimNext(f.ctx, humanHoldNow, "scheduler")
	if err != nil {
		t.Fatalf("ClaimNext() error = %v", err)
	}
	if item == nil {
		t.Fatal("ClaimNext() = nil, want the acme/vela#41 item: a hold on acme/looper#41 is a different checkout")
	}
	if item.ID != "queue_other_repo" {
		t.Fatalf("ClaimNext() claimed %q, want queue_other_repo", item.ID)
	}
}

// TestHumanTakeoverDoesNotHoldSiblingProjectWorkers is the other over-block. A
// project target is not a shared-worktree key: assertUniqueActiveLoopCompat
// permits concurrent project workers, their queue lock is worker:<loopID>, and
// each gets its own worktree branch. Holding one must fence that one loop, not
// every project worker in the project.
func TestHumanTakeoverDoesNotHoldSiblingProjectWorkers(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)

	f.seedProjectLoop(t, "loop_held_worker", "human_takeover", 51)
	f.seedProjectLoop(t, "loop_sibling_worker", "queued", 52)
	f.seedProjectQueueItem(t, "queue_sibling_worker", "loop_sibling_worker")

	item, err := f.repos.Queue.ClaimNext(f.ctx, humanHoldNow, "scheduler")
	if err != nil {
		t.Fatalf("ClaimNext() error = %v", err)
	}
	if item == nil {
		t.Fatal("ClaimNext() = nil, want the sibling project worker: project workers are independent by design")
	}
	if item.ID != "queue_sibling_worker" {
		t.Fatalf("ClaimNext() claimed %q, want queue_sibling_worker", item.ID)
	}
}

// TestHumanTakeoverStillHoldsTheHeldProjectWorker is the half that must survive
// the narrowing above: scoping project holds to the held loop is only correct
// if the held loop's own queue item is still refused.
func TestHumanTakeoverStillHoldsTheHeldProjectWorker(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)

	f.seedProjectLoop(t, "loop_held_worker", "human_takeover", 51)
	f.seedProjectQueueItem(t, "queue_held_worker", "loop_held_worker")

	item, err := f.repos.Queue.ClaimNext(f.ctx, humanHoldNow, "scheduler")
	if err != nil {
		t.Fatalf("ClaimNext() error = %v", err)
	}
	if item != nil {
		t.Fatalf("ClaimNext() = %#v, want no claim on the held loop's own queue item", item)
	}
}
