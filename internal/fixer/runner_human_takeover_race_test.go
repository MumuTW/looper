package fixer

import (
	"context"
	"strings"
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
	// of the takeover commit.
	heldRounds := 0
	racedTakeovers := 0
	for i := 0; i < 25; i++ {
		// Re-arm the loop as an ordinary queued loop before each round.
		loop, err := f.repos.Loops.GetByID(ctx, f.loopID)
		if err != nil || loop == nil {
			t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
		}
		reset := *loop
		reset.Status = "queued"
		reset.NextRunAt = nil
		if err := f.repos.Loops.UpsertReleasingHumanHold(ctx, reset); err != nil {
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
			// The daemon's takeover: stop (which pauses) and then park.
			if _, err := service.Pause(ctx, f.loopID, nil); err != nil {
				takeoverErr = err
				return
			}
			_, takeoverErr = service.TransitionStatus(ctx, f.loopID, loops.TransitionInput{Status: domain.LoopStatusHumanTakeover})
		}()
		wg.Wait()

		// A refused stale write must not surface as a discovery failure: the
		// human owns the loop, which is a skip, not an error.
		if discoveryErr != nil {
			t.Fatalf("round %d: DiscoverPullRequests() error = %v, want the hold reported as a skip", i, discoveryErr)
		}
		if takeoverErr != nil {
			// Discovery re-queued the loop between stop and park; takeover as a
			// whole failed loudly and this round has no hold to assert. That is a
			// pre-existing ordering hazard in the stop path, not this guard.
			if !strings.Contains(takeoverErr.Error(), "invalid loop status transition") {
				t.Fatalf("round %d: takeover error = %v", i, takeoverErr)
			}
			racedTakeovers++
			continue
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
	if heldRounds == 0 {
		t.Fatalf("no round produced a committed takeover (%d raced); the invariant was never exercised", racedTakeovers)
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
