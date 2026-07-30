package storage

import (
	"context"
	"database/sql"
	"fmt"
)

// RecoveryQueueItemSeed supplies the identity recovery needs to publish a queue
// item for a loop that has none: DerivedID names a replacement derived from the
// loop's latest terminal item, and Fallback builds the item to publish when
// there is no prior item to derive from. Queue history is authoritative, so
// Fallback is invoked only after the transaction has found none. ID minting and
// queue-item construction remain runtime concerns; a zero seed publishes
// nothing.
type RecoveryQueueItemSeed struct {
	DerivedID string
	Fallback  func() (QueueItemRecord, bool, error)
}

// StaleRunRequeueInput is the complete durable outcome of stale-run
// reconciliation's decision to requeue one loop: the loop record it wants to
// write and the queue repair that belongs with it.
type StaleRunRequeueInput struct {
	// Loop is the requeued record. The human-hold guard inside Loops.Upsert's
	// writing statement is this transaction's authority: a caller holding a
	// pre-takeover snapshot is refused by the write itself, and because the queue
	// repair rides the same transaction there is no window left in which it could
	// publish work against a loop a human now owns.
	Loop   LoopRecord
	NowISO string
	// Seed publishes a claimable queue item when the requeue moved no running
	// item and the loop has none active.
	Seed RecoveryQueueItemSeed
	// CancelDuplicates cancels every other active item once the surviving one is
	// known.
	CancelDuplicates bool
}

// StaleRunRequeueResult reports what the transaction committed. Applied is false
// when the loop is held by a human takeover and the whole repair was refused.
type StaleRunRequeueResult struct {
	Applied             bool
	QueueItemsRequeued  int64
	QueueItemsCancelled int64
}

// RequeueStaleRunLoop atomically requeues a stale loop and repairs its queue
// item. Loop status, queue claim and duplicate cancellation are one commit, so a
// takeover either lands before it — and the hold guard inside the loop write
// refuses the whole repair — or after it, against state that is already
// consistent.
func RequeueStaleRunLoop(ctx context.Context, db *sql.DB, input StaleRunRequeueInput) (StaleRunRequeueResult, error) {
	if db == nil {
		return StaleRunRequeueResult{}, fmt.Errorf("requeue stale run loop: database is not configured")
	}
	if input.Loop.ID == "" || input.NowISO == "" {
		return StaleRunRequeueResult{}, fmt.Errorf("requeue stale run loop: loop and nowISO authority are required")
	}
	if input.Loop.Status != "queued" {
		return StaleRunRequeueResult{}, fmt.Errorf("requeue stale run loop: requeued loop status must be queued, got %q", input.Loop.Status)
	}

	return WithTransactionValue(ctx, db, nil, func(tx *sql.Tx) (StaleRunRequeueResult, error) {
		result := StaleRunRequeueResult{}
		repos := NewRepositories(tx)
		// While `looper takeover` holds the loop, its status and schedule belong
		// to the human, so a refusal is not an error: recovery requeues loops, and
		// that is not the daemon's call to make under a hold. Nothing has been
		// written when it happens, so the caller simply skips this loop's repair.
		if err := repos.Loops.Upsert(ctx, input.Loop); err != nil {
			if IsLoopHumanHeldError(err) {
				return StaleRunRequeueResult{}, nil
			}
			return StaleRunRequeueResult{}, err
		}
		result.Applied = true

		keepID := ""
		active, err := repos.Queue.FindActiveByLoopID(ctx, input.Loop.ID)
		if err != nil {
			return StaleRunRequeueResult{}, err
		}
		if active != nil {
			keepID = active.ID
		}
		requeued, err := repos.Queue.RequeueRunningByLoop(ctx, input.Loop.ID, input.NowISO)
		if err != nil {
			return StaleRunRequeueResult{}, err
		}
		result.QueueItemsRequeued = requeued
		if requeued == 0 && (input.Seed.DerivedID != "" || input.Seed.Fallback != nil) {
			if err := EnsureActiveQueueItem(ctx, repos, input.Loop.ID, input.Seed, input.NowISO); err != nil {
				return StaleRunRequeueResult{}, err
			}
			published, err := repos.Queue.FindActiveByLoopID(ctx, input.Loop.ID)
			if err != nil {
				return StaleRunRequeueResult{}, err
			}
			if published != nil {
				keepID = published.ID
				result.QueueItemsRequeued++
			}
		}
		if input.CancelDuplicates && keepID != "" {
			reason := "Cancelled duplicate active queue items during stale-run reconciliation"
			cancelled, err := repos.Queue.CancelActiveByLoopExcept(ctx, input.Loop.ID, keepID, input.NowISO, &reason)
			if err != nil {
				return StaleRunRequeueResult{}, err
			}
			result.QueueItemsCancelled = cancelled
		}
		return result, nil
	})
}

// EnsureActiveQueueItem gives a recovered loop something claimable when it has
// nothing active: the loop's latest terminal item re-published under seed's
// DerivedID, or seed's Fallback when the loop has no queue history at all.
func EnsureActiveQueueItem(ctx context.Context, repositories *Repositories, loopID string, seed RecoveryQueueItemSeed, nowISO string) error {
	if repositories == nil || repositories.Queue == nil {
		return fmt.Errorf("ensure active queue item: queue storage is not configured")
	}
	activeQueue, err := repositories.Queue.FindActiveByLoopID(ctx, loopID)
	if err != nil || activeQueue != nil {
		return err
	}

	latestQueue, err := repositories.Queue.GetLatestByLoopID(ctx, loopID)
	if err != nil {
		return err
	}
	if latestQueue != nil {
		if latestQueue.Status == "queued" || latestQueue.Status == "running" {
			return nil
		}
		if latestQueue.DedupeKey != "" {
			activeByDedupe, err := repositories.Queue.FindActiveByDedupe(ctx, latestQueue.DedupeKey)
			if err != nil {
				return err
			}
			if activeByDedupe != nil {
				return nil
			}
		}
		if seed.DerivedID == "" {
			return nil
		}

		replacement := *latestQueue
		replacement.ID = seed.DerivedID
		replacement.Status = "queued"
		replacement.AvailableAt = nowISO
		replacement.Attempts = 0
		replacement.ClaimedBy = nil
		replacement.ClaimedAt = nil
		replacement.StartedAt = nil
		replacement.FinishedAt = nil
		replacement.LastError = nil
		replacement.LastErrorKind = nil
		replacement.CreatedAt = nowISO
		replacement.UpdatedAt = nowISO
		_, _, err := repositories.Queue.UpsertActiveByDedupeOrGetExisting(ctx, replacement)
		return err
	}

	if seed.Fallback == nil {
		return nil
	}
	fallback, ok, err := seed.Fallback()
	if err != nil || !ok {
		return err
	}
	_, _, err = repositories.Queue.UpsertActiveByDedupeOrGetExisting(ctx, fallback)
	return err
}
