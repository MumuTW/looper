package storage

import (
	"context"
	"errors"
	"testing"
)

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

// seedPRLoop creates a loop targeting acme/looper#41 through whichever write is
// sanctioned for its status: only `looper takeover` may create a hold, so a held
// fixture has to be seeded the way takeover takes one.
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
	write := f.repos.Loops.Upsert
	if status == "human_takeover" {
		write = f.repos.Loops.UpsertChangingHumanHold
	}
	if err := write(f.ctx, loop); err != nil {
		t.Fatalf("seed loop %s (status %s) error = %v", id, status, err)
	}
	return loop
}

// The insert half of the write guard. Round two guarded only the
// ON CONFLICT ... DO UPDATE branch, so `POST /loops` with
// status: "human_takeover" walked straight past it and minted the target-wide
// fence with no `looper takeover`, no queue cancellation and no stop.

func TestUpsertRefusesToCreateLoopAlreadyHumanHeld(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)

	repo := "acme/looper"
	prNumber := int64(41)
	targetID := "pr:acme/looper:41"
	err := f.repos.Loops.Upsert(f.ctx, LoopRecord{
		ID: "loop_minted_hold", Seq: 61, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber,
		Status: "human_takeover", CreatedAt: humanHoldNow, UpdatedAt: humanHoldNow,
	})
	if !errors.Is(err, ErrLoopHumanHeld) {
		t.Fatalf("Upsert(new loop in human_takeover) error = %v, want ErrLoopHumanHeld", err)
	}
	stored, getErr := f.repos.Loops.GetByID(f.ctx, "loop_minted_hold")
	if getErr != nil {
		t.Fatalf("Loops.GetByID() error = %v", getErr)
	}
	if stored != nil {
		t.Fatalf("Loops.GetByID() = %#v, want no row: the refused insert must not have landed", stored)
	}
}

// The sanctioned write still creates one, and an ordinary insert of an unheld
// loop is untouched — the guard is about held-ness, not about inserts.
func TestUpsertChangingHumanHoldMayCreateHeldLoopAndOrdinaryInsertsStillWork(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)

	if err := f.repos.Loops.UpsertChangingHumanHold(f.ctx, LoopRecord{
		ID: "loop_sanctioned_hold", Seq: 62, ProjectID: "project_1", Type: "worker",
		TargetType: "project", Status: "human_takeover", CreatedAt: humanHoldNow, UpdatedAt: humanHoldNow,
	}); err != nil {
		t.Fatalf("UpsertChangingHumanHold(new held loop) error = %v", err)
	}
	if err := f.repos.Loops.Upsert(f.ctx, LoopRecord{
		ID: "loop_ordinary", Seq: 63, ProjectID: "project_1", Type: "worker",
		TargetType: "project", Status: "queued", CreatedAt: humanHoldNow, UpdatedAt: humanHoldNow,
	}); err != nil {
		t.Fatalf("Upsert(new queued loop) error = %v", err)
	}
	held, err := f.repos.Loops.GetByID(f.ctx, "loop_sanctioned_hold")
	if err != nil || held == nil || held.Status != "human_takeover" {
		t.Fatalf("Loops.GetByID(loop_sanctioned_hold) = (%#v, %v), want a held loop", held, err)
	}
}

// A metadata refresh on an already-held loop still lands: the guard refuses
// writes that *change* held-ness, and the insert check must not turn every
// held-status write into a refusal.
func TestUpsertStillRefreshesAnAlreadyHeldLoop(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)

	f.seedPRLoop(t, "loop_held_fixer", "fixer", "human_takeover", 41)
	current, err := f.repos.Loops.GetByID(f.ctx, "loop_held_fixer")
	if err != nil || current == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want the held loop", current, err)
	}
	metadata := `{"worktreePath":"/tmp/looper-fix-project_1-pr-41"}`
	refreshed := *current
	refreshed.MetadataJSON = &metadata
	if err := f.repos.Loops.Upsert(f.ctx, refreshed); err != nil {
		t.Fatalf("Upsert(metadata refresh on held loop) error = %v, want it to land", err)
	}
	stored, err := f.repos.Loops.GetByID(f.ctx, "loop_held_fixer")
	if err != nil || stored == nil || stored.MetadataJSON == nil || *stored.MetadataJSON != metadata {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want the refreshed metadata", stored, err)
	}
}
