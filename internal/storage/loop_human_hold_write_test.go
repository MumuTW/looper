package storage

import (
	"errors"
	"testing"
)

// The write half of the takeover hold. Every revival path in #162 had the same
// shape — read the loop, set status, blind-upsert the whole row — which is also
// what bypassed domain.AssertLoopStatusTransition. The refusal is therefore a
// property of the write itself, evaluated inside the writing statement so a
// caller holding a pre-takeover snapshot cannot win by writing later.
// The shared fixture lives in loop_human_hold_claim_test.go.

// TestLoopUpsertRefusesToReleaseHumanHold is the write boundary. Every revival
// path in #162 had this shape: read the loop, set status to queued, blind-upsert
// the whole row. The refusal is evaluated inside the writing statement, so a
// caller holding a pre-takeover snapshot cannot win by writing later.
func TestLoopUpsertRefusesToReleaseHumanHold(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)

	// The record a discovery tick read *before* takeover committed.
	stale := f.seedPRLoop(t, "loop_held", "fixer", "queued", 41)

	held := stale
	held.Status = "human_takeover"
	held.UpdatedAt = "2026-07-30T12:01:00.000Z"
	// Taking the hold goes through the sanctioned path; a blind Upsert may not
	// author it in either direction.
	if err := f.repos.Loops.UpsertChangingHumanHold(f.ctx, held); err != nil {
		t.Fatalf("UpsertChangingHumanHold(takeover) error = %v", err)
	}

	// The tick now writes its stale record back. Reviving a held loop is exactly
	// what re-armed the lane in #162.
	revived := stale
	revived.Status = "queued"
	revived.UpdatedAt = "2026-07-30T12:02:00.000Z"
	err := f.repos.Loops.Upsert(f.ctx, revived)
	if !errors.Is(err, ErrLoopHumanHeld) {
		t.Fatalf("Upsert(stale queued over human_takeover) error = %v, want ErrLoopHumanHeld", err)
	}
	if !IsLoopHumanHeldError(err) {
		t.Fatalf("IsLoopHumanHeldError(%v) = false, want true", err)
	}

	got, err := f.repos.Loops.GetByID(f.ctx, stale.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if got == nil || got.Status != "human_takeover" {
		t.Fatalf("loop = %#v, want the hold intact", got)
	}
}

// TestLoopUpsertAllowsWritesThatKeepTheHold: the guard is on releasing the hold,
// not on touching the row. Handback stamps the resume session id onto a loop
// that is still human_takeover, and that write must land.
func TestLoopUpsertAllowsWritesThatKeepTheHold(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)
	held := f.seedPRLoop(t, "loop_held", "fixer", "human_takeover", 41)

	metadata := `{"takeoverResume":{"sessionId":"session_human"}}`
	held.MetadataJSON = &metadata
	held.UpdatedAt = "2026-07-30T12:02:00.000Z"
	if err := f.repos.Loops.Upsert(f.ctx, held); err != nil {
		t.Fatalf("Upsert(metadata while held) error = %v, want success", err)
	}

	got, err := f.repos.Loops.GetByID(f.ctx, held.ID)
	if err != nil || got == nil {
		t.Fatalf("GetByID() = %#v, %v", got, err)
	}
	if got.MetadataJSON == nil || *got.MetadataJSON != metadata {
		t.Fatalf("metadata = %v, want the resume session id stamped", got.MetadataJSON)
	}
	if got.Status != "human_takeover" {
		t.Fatalf("status = %q, want human_takeover preserved", got.Status)
	}
}

// TestLoopUpsertChangingHumanHoldSucceeds covers the sanctioned exit: the loops
// service (which applies domain.AssertLoopStatusTransition) and /handback.
func TestLoopUpsertChangingHumanHoldSucceeds(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)
	held := f.seedPRLoop(t, "loop_held", "fixer", "human_takeover", 41)

	released := held
	released.Status = "queued"
	released.UpdatedAt = "2026-07-30T12:03:00.000Z"
	if err := f.repos.Loops.UpsertChangingHumanHold(f.ctx, released); err != nil {
		t.Fatalf("UpsertChangingHumanHold() error = %v", err)
	}
	got, err := f.repos.Loops.GetByID(f.ctx, held.ID)
	if err != nil || got == nil || got.Status != "queued" {
		t.Fatalf("loop = %#v (err %v), want queued after handback", got, err)
	}
}

// TestLoopUpsertGuardDoesNotAffectUnheldLoops: the guard must be invisible to
// every ordinary loop write, otherwise it would be a new failure mode rather
// than a hold.
func TestLoopUpsertGuardDoesNotAffectUnheldLoops(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)
	loop := f.seedPRLoop(t, "loop_plain", "fixer", "paused", 41)

	loop.Status = "queued"
	loop.UpdatedAt = "2026-07-30T12:04:00.000Z"
	if err := f.repos.Loops.Upsert(f.ctx, loop); err != nil {
		t.Fatalf("Upsert(paused -> queued) error = %v, want success", err)
	}
	got, err := f.repos.Loops.GetByID(f.ctx, loop.ID)
	if err != nil || got == nil || got.Status != "queued" {
		t.Fatalf("loop = %#v (err %v), want queued", got, err)
	}
}
