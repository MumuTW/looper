package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

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

// rowsAffectedErrorQuerier makes RowsAffected fail without failing the Exec, the
// one shape where the guard cannot tell an applied write from a refused one.
type rowsAffectedErrorQuerier struct{ sqliteQuerier }

type rowsAffectedErrorResult struct{}

func (rowsAffectedErrorResult) LastInsertId() (int64, error) { return 0, nil }
func (rowsAffectedErrorResult) RowsAffected() (int64, error) {
	return 0, fmt.Errorf("driver lost the row count")
}

func (q rowsAffectedErrorQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return rowsAffectedErrorResult{}, nil
}

// TestUpsertFailsClosedWhenRowsAffectedErrors: reporting success here would be
// exactly the silent hold change the guard exists to stop, so an unreadable row
// count is an error, not a pass.
func TestUpsertFailsClosedWhenRowsAffectedErrors(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)

	repo := &LoopsRepository{q: rowsAffectedErrorQuerier{}}
	err := repo.Upsert(f.ctx, LoopRecord{
		ID: "loop_unknown_rows", Seq: 64, ProjectID: "project_1", Type: "worker",
		TargetType: "project", Status: "queued", CreatedAt: humanHoldNow, UpdatedAt: humanHoldNow,
	})
	if err == nil {
		t.Fatal("Upsert() error = nil, want a failure: an unreadable row count cannot be reported as an applied write")
	}
}
