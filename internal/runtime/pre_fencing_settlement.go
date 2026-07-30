package runtime

import (
	"context"
	"sort"
	"strings"

	"github.com/nexu-io/looper/internal/storage"
)

// preFencingSettlementEventType is the durable marker that the one-shot
// settlement below already ran. It is an event rather than a schema migration
// because the work it does is runtime reconciliation (loops, queue items), not
// a shape change SQL can express.
const preFencingSettlementEventType = "looperd.recovery.pre_fencing_settlement"

// quarantineParkReasonFragments are the exact messages the old quarantine path
// wrote into the queue item it failed. Matching on them is what distinguishes a
// park this function may release from a park it must not touch: a loop held for
// risky conflict fixes, or one a human took over, is also `paused`, and only the
// failure text says which is which.
// Fragments no current code path can produce are load-bearing here, not dead
// weight: this function exists precisely to clear parks written by daemons that
// predate the fence, so the set it matches is the set that appears in databases
// in the field, not the set today's source writes.
var quarantineParkReasonFragments = []string{
	"startup liveness evidence is not authoritative",
	"execution liveness is not authoritative",
	"recovery quarantine: execution evidence without containment confirmation",
	"startup recovery: matching live process from previous daemon",
	"this daemon still owns a live handle",
	// Written by pre-#149 daemons only; current source builds the
	// "startup liveness evidence is not authoritative" phrasing above instead.
	// On a live database this fragment is what holds the oldest parks — the
	// ones #149 opens with — so dropping it strands exactly the rows this
	// settlement was written for.
	"startup recovery: uncertain",
}

// settlePreFencingParks releases loops that a pre-#149 daemon parked with
// quarantine evidence and then had no way to release.
//
// Everything after this change settles itself: an execution no daemon owns has
// its worktree generation retired and its row finalized on the next boot. But
// rows written before the fence existed left behind loops at `paused` and queue
// items at `manual_intervention`, and those parks are durable state no later
// pass reinterprets. This runs exactly once to clear them.
//
// Returns the number of audit events written.
func (r *Runtime) settlePreFencingParks(ctx context.Context, repositories *storage.Repositories, nowISO string) (int64, error) {
	if repositories == nil || repositories.Events == nil || repositories.Loops == nil || repositories.Queue == nil {
		return 0, nil
	}
	alreadyRan, err := repositories.Events.ExistsByType(ctx, preFencingSettlementEventType)
	if err != nil {
		return 0, err
	}
	if alreadyRan {
		return 0, nil
	}

	quarantineEvents, err := repositories.Events.ListByType(ctx, recoveryExecutionQuarantinedEventType)
	if err != nil {
		return 0, err
	}
	loopIDs := make(map[string]struct{}, len(quarantineEvents))
	for _, event := range quarantineEvents {
		// Only parks from a previous daemon. This same pass can write quarantine
		// evidence for work it legitimately parked, and releasing that would
		// hand the loop back out from under a live agent.
		if event.CreatedAt >= nowISO {
			continue
		}
		if loopID := strings.TrimSpace(derefString(event.LoopID)); loopID != "" {
			loopIDs[loopID] = struct{}{}
		}
	}

	released := make([]string, 0, len(loopIDs))
	held := make([]string, 0)
	for _, loopID := range sortedKeys(loopIDs) {
		loop, err := repositories.Loops.GetByID(ctx, loopID)
		if err != nil {
			return 0, err
		}
		if loop == nil {
			continue
		}
		latestQueue, err := repositories.Queue.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return 0, err
		}
		if !quarantineParkIsReleasable(*loop, latestQueue) {
			if loop.Status == "paused" || loop.Status == "human_takeover" {
				held = append(held, loop.ID)
			}
			continue
		}
		requeued := *loop
		requeued.Status = "queued"
		requeued.NextRunAt = stringPtr(nowISO)
		requeued.UpdatedAt = nowISO
		if err := repositories.Loops.Upsert(ctx, requeued); err != nil {
			return 0, err
		}
		if err := ensureRecoveryQueueItem(ctx, repositories, requeued, nowISO, int64(r.Config().Scheduler.RetryMaxAttempts)); err != nil {
			return 0, err
		}
		released = append(released, loop.ID)
	}

	if err := appendSystemEvent(ctx, repositories, storage.EventLogRecord{
		ID:         newRuntimeEventID(),
		EventType:  preFencingSettlementEventType,
		EntityType: stringPtr("notification"),
		EntityID:   stringPtr("looperd-recovery"),
		PayloadJSON: mustMarshalJSON(map[string]any{
			"settledAt":       nowISO,
			"releasedLoopIds": released,
			"releasedLoops":   len(released),
			"heldLoopIds":     held,
			"containment":     "worktree_generation_retired",
		}),
		CreatedAt: nowISO,
	}); err != nil {
		return 0, err
	}
	if r.logger != nil && len(released) > 0 {
		r.logger.Info("released loops parked by pre-fencing recovery quarantine", map[string]any{
			"releasedLoops": len(released),
			"heldLoops":     len(held),
		})
	}
	return 1, nil
}

// quarantineParkIsReleasable is true only for a loop this settlement itself
// could have parked, or has already half-released. Human takeovers, terminal
// loops, and loops paused for a domain reason all fail at least one check.
func quarantineParkIsReleasable(loop storage.LoopRecord, latestQueue *storage.QueueItemRecord) bool {
	if latestQueue == nil || !isQuarantineParkFailure(latestQueue) {
		return false
	}
	switch loop.Status {
	case "paused":
		// A decision recorded AFTER the park is someone else's, and it wins.
		// The old quarantine wrote the loop and failed the queue item at the
		// same instant, so a loop touched later than its own park carries a
		// newer intent. An operator /pause is exactly that shape and is
		// otherwise invisible here: CancelByLoop only rewrites queued/running
		// items, so it leaves the manual_intervention item and its text
		// untouched, and the loop reads `paused` either way.
		return loop.UpdatedAt <= latestQueue.UpdatedAt
	case "queued":
		// A partial release: an earlier pass flipped the loop and died before
		// replacing the queue item. Finishing it is idempotent, and it has to be
		// finished here — the later recovery pass normalizes a queued loop with
		// a manual_intervention item straight back out of queued, by which time
		// this one-shot has already written its durable marker.
		return true
	default:
		return false
	}
}

func isQuarantineParkFailure(latestQueue *storage.QueueItemRecord) bool {
	if strings.TrimSpace(derefString(latestQueue.LastErrorKind)) != "manual_intervention" {
		return false
	}
	lastError := strings.ToLower(derefString(latestQueue.LastError))
	for _, fragment := range quarantineParkReasonFragments {
		if strings.Contains(lastError, fragment) {
			return true
		}
	}
	return false
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
