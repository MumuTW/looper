package fixer

import (
	"context"
	"sync"
	"testing"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// Status preservation in discovery is a read-modify-write. A tick that read the
// loop just before takeover committed would write its stale record back as
// `queued` afterwards, recreating the exact window this change closes; the
// inverse ordering would restore a hold over a handback that already landed.
// The refusal is therefore evaluated inside the writing statement, and these
// exercise takeover against discovery concurrently.

// TestHumanTakeoverWinsConcurrentDiscoveryTick asserts the invariant that holds
// under every interleaving: once takeover commits, no discovery tick can move
// the loop out of human_takeover — whether it started before or after. If
// discovery wins the ordering, takeover simply applies on top; if takeover wins,
// the tick's stale write is refused.
func TestHumanTakeoverWinsConcurrentDiscoveryTick(t *testing.T) {
	f := newTakeoverHoldFixture(t, false, "interrupted")
	ctx := context.Background()
	service := &loops.Service{DB: f.coordinator.DB(), Repos: f.repos, Now: f.now}

	// Repeat so the scheduler has many chances to land the write on either side
	// of the takeover commit. Every round must commit the takeover: the window
	// that used to make some rounds unassertable — stop first, park second — is
	// closed now that the hold is the first write (#177).
	heldRounds := 0
	for i := 0; i < 25; i++ {
		// Re-arm the loop as an ordinary queued loop before each round.
		loop, err := f.repos.Loops.GetByID(ctx, f.loopID)
		if err != nil || loop == nil {
			t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
		}
		reset := *loop
		reset.Status = "queued"
		reset.NextRunAt = nil
		if err := f.repos.Loops.UpsertChangingHumanHold(ctx, reset); err != nil {
			t.Fatalf("reset loop error = %v", err)
		}
		if _, err := f.repos.Queue.CancelByLoop(ctx, f.loopID, f.nowString, nil); err != nil {
			t.Fatalf("Queue.CancelByLoop() error = %v", err)
		}

		var wg sync.WaitGroup
		var discoveryErr, takeoverErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, discoveryErr = f.runner.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: f.repo})
		}()
		go func() {
			defer wg.Done()
			// The daemon's takeover: the hold is one commit, taken before the run
			// is stopped, so no concurrent status write can invalidate it.
			_, takeoverErr = service.Hold(ctx, f.loopID, nil)
		}()
		wg.Wait()

		// A refused stale write must not surface as a discovery failure: the
		// human owns the loop, which is a skip, not an error.
		if discoveryErr != nil {
			t.Fatalf("round %d: DiscoverPullRequests() error = %v, want the hold reported as a skip", i, discoveryErr)
		}
		if takeoverErr != nil {
			// No skipped rounds: takeover must commit whatever discovery is doing.
			t.Fatalf("round %d: takeover error = %v, want it to commit against any concurrent tick", i, takeoverErr)
		}
		heldRounds++

		after, err := f.repos.Loops.GetByID(ctx, f.loopID)
		if err != nil || after == nil {
			t.Fatalf("round %d: Loops.GetByID() = %#v, %v", i, after, err)
		}
		if after.Status != string(domain.LoopStatusHumanTakeover) {
			t.Fatalf("round %d: status = %q, want human_takeover: a discovery tick holding a pre-takeover snapshot must not revive the loop", i, after.Status)
		}
		// The hold is only worth anything if the claim boundary agrees.
		scheduled, err := f.repos.Queue.ListScheduled(ctx, f.nowString, 50)
		if err != nil {
			t.Fatalf("round %d: ListScheduled() error = %v", i, err)
		}
		if len(scheduled) != 0 {
			t.Fatalf("round %d: ListScheduled() = %#v, want nothing claimable while the loop is held", i, scheduled)
		}
	}
	if heldRounds != 25 {
		t.Fatalf("committed takeovers = %d, want all 25 rounds", heldRounds)
	}
	assertNoGitMutations(t, f.git)
}

// TestDiscoveryStaleWriteCannotReviveHeldLoop is the deterministic form of the
// same race at the fixer's write boundary: the exact stale record a mid-flight
// tick would produce, written after takeover committed.
func TestDiscoveryStaleWriteCannotReviveHeldLoop(t *testing.T) {
	t.Parallel()
	f := newTakeoverHoldFixture(t, false, "interrupted")
	ctx := context.Background()

	held, err := f.repos.Loops.GetByID(ctx, f.loopID)
	if err != nil || held == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", held, err)
	}
	// What the tick read before takeover landed.
	stale := *held
	stale.Status = "queued"
	nextRunAt := f.nowString
	stale.NextRunAt = &nextRunAt

	writeErr := f.repos.Loops.Upsert(ctx, stale)
	if !storage.IsLoopHumanHeldError(writeErr) {
		t.Fatalf("Upsert(stale queued) error = %v, want storage.ErrLoopHumanHeld", writeErr)
	}

	// And the runner turns that refusal into the same skipped result as the
	// pre-write check, rather than failing the discovery pass.
	skipped, err := f.runner.humanHeldLoopSkip(ctx, f.loopID)
	if err != nil {
		t.Fatalf("humanHeldLoopSkip() error = %v", err)
	}
	if !skipped.skipped || skipped.record.Status != string(domain.LoopStatusHumanTakeover) {
		t.Fatalf("humanHeldLoopSkip() = %#v, want a skipped result carrying the held record", skipped)
	}
}

// TestHumanHeldLoopSkipPropagatesReadFailure: the guarded write refusing a stale
// record is routine, but a failed re-read is a storage failure. Converting it
// into a successful skipped result would report a clean discovery pass with an
// empty record and hide the failure from retry and error handling — the same
// class of false success this change exists to remove.
func TestHumanHeldLoopSkipPropagatesReadFailure(t *testing.T) {
	t.Parallel()
	f := newTakeoverHoldFixture(t, false, "interrupted")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := f.runner.humanHeldLoopSkip(cancelled, f.loopID); err == nil {
		t.Fatal("humanHeldLoopSkip() error = nil on a cancelled context, want the storage failure propagated")
	}
}

// TestHumanHeldLoopSkipReportsMissingRow keeps the fallback meaningful: an
// intentionally absent loop is a skip, not an error.
func TestHumanHeldLoopSkipReportsMissingRow(t *testing.T) {
	t.Parallel()
	f := newTakeoverHoldFixture(t, false, "interrupted")

	result, err := f.runner.humanHeldLoopSkip(context.Background(), "loop_missing")
	if err != nil {
		t.Fatalf("humanHeldLoopSkip() error = %v, want a missing row reported as skipped", err)
	}
	if !result.skipped || result.record.ID != "" {
		t.Fatalf("result = %#v, want an empty skipped result", result)
	}
}
