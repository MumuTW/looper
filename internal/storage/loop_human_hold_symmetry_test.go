package storage

import (
	"errors"
	"strings"
	"testing"
)

// The hold guard is symmetric: a blind read-modify-Upsert may not author
// held-ness in either direction. Round one guarded only held → unheld, which
// left handback undoable — a tick that read the loop while it was held would
// write its preserved human_takeover status back after handback committed, and
// the operator's released loop silently became unclaimable again. That is the
// same failure shape as the bug this whole change fixes: a control reporting
// success while something else quietly undoes it.
//
// The shared fixture lives in loop_human_hold_claim_test.go.

// TestLoopUpsertRefusesToRestoreHoldAfterHandback is the mirror image of
// TestLoopUpsertRefusesToReleaseHumanHold.
func TestLoopUpsertRefusesToRestoreHoldAfterHandback(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)

	held := f.seedPRLoop(t, "loop_held", "fixer", "human_takeover", 41)
	// The record a discovery tick read while the loop was still held.
	stale := held

	// Handback releases the hold through the sanctioned path.
	released := held
	released.Status = "queued"
	released.UpdatedAt = "2026-07-30T12:03:00.000Z"
	if err := f.repos.Loops.UpsertChangingHumanHold(f.ctx, released); err != nil {
		t.Fatalf("UpsertChangingHumanHold(handback) error = %v", err)
	}

	// The tick now writes its stale record back, preserving human_takeover.
	stale.UpdatedAt = "2026-07-30T12:04:00.000Z"
	err := f.repos.Loops.Upsert(f.ctx, stale)
	if !errors.Is(err, ErrLoopHumanHeld) {
		t.Fatalf("Upsert(stale human_takeover over queued) error = %v, want ErrLoopHumanHeld", err)
	}

	got, err := f.repos.Loops.GetByID(f.ctx, held.ID)
	if err != nil || got == nil {
		t.Fatalf("GetByID() = %#v, %v", got, err)
	}
	if got.Status != "queued" {
		t.Fatalf("status = %q, want queued: handback must stay released", got.Status)
	}
}

// TestLoopUpsertRefusesToTakeHoldBlindly: taking the hold is a decision with an
// authority (`looper takeover`, via the transition service). A lane must not be
// able to park a loop for a human by accident any more than it can un-park one.
func TestLoopUpsertRefusesToTakeHoldBlindly(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)

	loop := f.seedPRLoop(t, "loop_plain", "fixer", "queued", 41)
	loop.Status = "human_takeover"
	loop.UpdatedAt = "2026-07-30T12:05:00.000Z"
	if err := f.repos.Loops.Upsert(f.ctx, loop); !errors.Is(err, ErrLoopHumanHeld) {
		t.Fatalf("Upsert(queued -> human_takeover) error = %v, want ErrLoopHumanHeld", err)
	}
	got, err := f.repos.Loops.GetByID(f.ctx, loop.ID)
	if err != nil || got == nil || got.Status != "queued" {
		t.Fatalf("loop = %#v (err %v), want the loop still queued", got, err)
	}
}

// TestLoopHumanHoldRejectionNamesTheDirection: the refusal is surfaced to
// operators as a 409, so it must say which side of the hold it is on rather than
// always claiming the loop is held.
func TestLoopHumanHoldRejectionNamesTheDirection(t *testing.T) {
	t.Parallel()
	f := newHumanHoldFixture(t)

	held := f.seedPRLoop(t, "loop_held", "fixer", "human_takeover", 41)
	revived := held
	revived.Status = "queued"
	revived.UpdatedAt = "2026-07-30T12:06:00.000Z"
	err := f.repos.Loops.Upsert(f.ctx, revived)
	if err == nil || !strings.Contains(err.Error(), "looper handback") {
		t.Fatalf("release refusal = %v, want it to name the release command", err)
	}

	released := f.seedPRLoop(t, "loop_released", "fixer", "queued", 42)
	restored := released
	restored.Status = "human_takeover"
	restored.UpdatedAt = "2026-07-30T12:07:00.000Z"
	err = f.repos.Loops.Upsert(f.ctx, restored)
	if err == nil || !strings.Contains(err.Error(), "no longer held") {
		t.Fatalf("restore refusal = %v, want it to say the loop is not held", err)
	}
}
