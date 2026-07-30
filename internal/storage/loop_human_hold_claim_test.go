package storage

import (
	"context"
	"testing"
)

// The claim half of the takeover hold. #162's original fix filtered only the
// queue item's own loop status, which left the invariant that matters
// uncovered: the thing a human takes over is a *worktree*, and a PR's managed
// checkout is shared by every role targeting it. The write half is in
// loop_human_hold_write_test.go.

const humanHoldNow = "2026-07-30T12:00:00.000Z"

type humanHoldFixture struct {
	repos *Repositories
	ctx   context.Context
}

func newHumanHoldFixture(t *testing.T) *humanHoldFixture {
	t.Helper()
	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	ctx := context.Background()
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: humanHoldNow, UpdatedAt: humanHoldNow}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	return &humanHoldFixture{repos: repos, ctx: ctx}
}

// seedPRLoop creates a loop of the given role targeting acme/looper#41, the
// shared detached PR worktree every PR-targeted role checks out.
func (f *humanHoldFixture) seedPRLoop(t *testing.T, id, loopType, status string, seq int64) LoopRecord {
	t.Helper()
	repo := "acme/looper"
	prNumber := int64(41)
	targetID := "pr:acme/looper:41"
	loop := LoopRecord{
		ID: id, Seq: seq, ProjectID: "project_1", Type: loopType,
		TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber,
		Status: status, CreatedAt: humanHoldNow, UpdatedAt: humanHoldNow,
	}
	f.seedLoop(t, loop)
	return loop
}

// seedLoop writes a fixture loop through whichever write is sanctioned for its
// status: only `looper takeover` may create a hold, so a held fixture has to be
// seeded the way takeover takes one.
func (f *humanHoldFixture) seedLoop(t *testing.T, loop LoopRecord) {
	t.Helper()
	write := f.repos.Loops.Upsert
	if loop.Status == "human_takeover" {
		write = f.repos.Loops.UpsertChangingHumanHold
	}
	if err := write(f.ctx, loop); err != nil {
		t.Fatalf("seed loop %s (status %s) error = %v", loop.ID, loop.Status, err)
	}
}

func (f *humanHoldFixture) seedQueueItem(t *testing.T, id, loopID, itemType string) {
	t.Helper()
	repo := "acme/looper"
	prNumber := int64(41)
	projectID := "project_1"
	if err := f.repos.Queue.Upsert(f.ctx, QueueItemRecord{
		ID: id, ProjectID: &projectID, LoopID: &loopID, Type: itemType,
		TargetType: "pull_request", TargetID: "pr:acme/looper:41",
		Repo: &repo, PRNumber: &prNumber, DedupeKey: itemType + ":acme/looper:41",
		Status: "queued", Priority: 5, AvailableAt: humanHoldNow, MaxAttempts: 5,
		CreatedAt: humanHoldNow, UpdatedAt: humanHoldNow,
	}); err != nil {
		t.Fatalf("Queue.Upsert(%s) error = %v", id, err)
	}
}

// TestHumanTakeoverHoldsSiblingRoleOnSamePR is the cross-role worktree
// invariant. The PR that motivated #162 carried loops of more than one role;
// they share looper-fix-<project>-pr-41-detached, and takeover cancels only the
// held loop's own queue item, so a sibling's queued item had nothing left
// blocking it and could prepare or clean the human's checkout.
func TestHumanTakeoverHoldsSiblingRoleOnSamePR(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)

	f.seedPRLoop(t, "loop_held_fixer", "fixer", "human_takeover", 41)
	f.seedPRLoop(t, "loop_sibling_reviewer", "reviewer", "queued", 42)
	// Only the sibling has queued work: takeover cancelled the held loop's item.
	f.seedQueueItem(t, "queue_sibling_reviewer", "loop_sibling_reviewer", "reviewer")

	if item, err := f.repos.Queue.ClaimNext(f.ctx, humanHoldNow, "scheduler"); err != nil {
		t.Fatalf("ClaimNext() error = %v", err)
	} else if item != nil {
		t.Fatalf("ClaimNext() = %#v, want no claim: a sibling loop on the taken-over PR shares the human's worktree", item)
	}
	if item, err := f.repos.Queue.ClaimNextOfType(f.ctx, humanHoldNow, "scheduler", "reviewer"); err != nil {
		t.Fatalf("ClaimNextOfType(reviewer) error = %v", err)
	} else if item != nil {
		t.Fatalf("ClaimNextOfType(reviewer) = %#v, want no claim while the fixer sibling is human_takeover", item)
	}
	if item, err := f.repos.Queue.ClaimNextNonLongTermRetryAmongTypeSets(f.ctx, humanHoldNow, "scheduler", []string{"fixer", "reviewer", "worker", "planner"}, nil); err != nil {
		t.Fatalf("ClaimNextNonLongTermRetryAmongTypeSets() error = %v", err)
	} else if item != nil {
		t.Fatalf("ClaimNextNonLongTermRetryAmongTypeSets() = %#v, want no claim", item)
	}
	scheduled, err := f.repos.Queue.ListScheduled(f.ctx, humanHoldNow, 50)
	if err != nil {
		t.Fatalf("ListScheduled() error = %v", err)
	}
	if len(scheduled) != 0 {
		t.Fatalf("ListScheduled() = %#v, want the sibling item invisible to the scheduler", scheduled)
	}
}

// TestHumanTakeoverReleaseRestoresSiblingClaims proves the hold is a hold and
// not a permanent block: once handback releases the held loop, the sibling's
// queued work becomes claimable again.
func TestHumanTakeoverReleaseRestoresSiblingClaims(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)

	held := f.seedPRLoop(t, "loop_held_fixer", "fixer", "human_takeover", 41)
	f.seedPRLoop(t, "loop_sibling_reviewer", "reviewer", "queued", 42)
	f.seedQueueItem(t, "queue_sibling_reviewer", "loop_sibling_reviewer", "reviewer")

	released := held
	released.Status = "queued"
	released.UpdatedAt = "2026-07-30T12:05:00.000Z"
	if err := f.repos.Loops.UpsertChangingHumanHold(f.ctx, released); err != nil {
		t.Fatalf("UpsertChangingHumanHold() error = %v", err)
	}

	item, err := f.repos.Queue.ClaimNextOfType(f.ctx, humanHoldNow, "scheduler", "reviewer")
	if err != nil {
		t.Fatalf("ClaimNextOfType(reviewer) error = %v", err)
	}
	if item == nil || item.ID != "queue_sibling_reviewer" {
		t.Fatalf("ClaimNextOfType(reviewer) = %#v, want the sibling item claimable after handback", item)
	}
}

// The issue-target half of this invariant moved to
// loop_human_hold_issue_scope_test.go once it was narrowed: an issue target is a
// shared checkout only among planner loops, so the cross-role case this file used
// to assert (a held planner fencing a worker on the same issue) is now an
// over-block with its own test.

// TestHumanTakeoverClaimHoldIsScopedToItsTarget guards the other direction: an
// unrelated PR must stay claimable, so the predicate is a hold on one worktree
// rather than a daemon-wide stop.
func TestHumanTakeoverClaimHoldIsScopedToItsTarget(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)
	f.seedPRLoop(t, "loop_held_fixer", "fixer", "human_takeover", 41)

	repo := "acme/looper"
	otherPR := int64(99)
	otherTarget := "pr:acme/looper:99"
	if err := f.repos.Loops.Upsert(f.ctx, LoopRecord{
		ID: "loop_other_pr", Seq: 99, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", TargetID: &otherTarget, Repo: &repo, PRNumber: &otherPR,
		Status: "queued", CreatedAt: humanHoldNow, UpdatedAt: humanHoldNow,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "project_1"
	loopID := "loop_other_pr"
	if err := f.repos.Queue.Upsert(f.ctx, QueueItemRecord{
		ID: "queue_other_pr", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", TargetID: otherTarget, Repo: &repo, PRNumber: &otherPR,
		DedupeKey: "fixer:acme/looper:99", Status: "queued", Priority: 5,
		AvailableAt: humanHoldNow, MaxAttempts: 5, CreatedAt: humanHoldNow, UpdatedAt: humanHoldNow,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	item, err := f.repos.Queue.ClaimNext(f.ctx, humanHoldNow, "scheduler")
	if err != nil {
		t.Fatalf("ClaimNext() error = %v", err)
	}
	if item == nil || item.ID != "queue_other_pr" {
		t.Fatalf("ClaimNext() = %#v, want the unrelated PR still claimable", item)
	}
}
