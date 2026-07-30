package storage

import (
	"context"
	"database/sql"
	"fmt"
)

// WorkerSuccessFinalizationInput is the complete durable outcome of a worker
// attempt whose execution steps have already finished.
type WorkerSuccessFinalizationInput struct {
	Run         RunRecord
	QueueItemID string
	LoopID      string
	LoopStatus  string
	FinishedAt  string
}

// FinalizeWorkerSuccess atomically records run success, consumes the queue
// claim, and completes the loop. Existing terminal queue state is already a
// consumed claim, so replay may still converge the run and loop idempotently.
func FinalizeWorkerSuccess(ctx context.Context, db *sql.DB, input WorkerSuccessFinalizationInput) error {
	if db == nil {
		return fmt.Errorf("finalize worker success: database is not configured")
	}
	if input.Run.ID == "" || input.Run.Status != "success" {
		return fmt.Errorf("finalize worker success: successful run is required")
	}
	if input.QueueItemID == "" || input.LoopID == "" || input.Run.LoopID != input.LoopID || input.FinishedAt == "" {
		return fmt.Errorf("finalize worker success: run, queue, loop, and finishedAt authority are required")
	}
	if input.LoopStatus != "completed" && input.LoopStatus != "queued" {
		return fmt.Errorf("finalize worker success: loop status must be completed or queued")
	}

	return WithTransaction(ctx, db, nil, func(tx *sql.Tx) error {
		repos := NewRepositories(tx)
		queueItem, err := repos.Queue.GetByID(ctx, input.QueueItemID)
		if err != nil {
			return err
		}
		if queueItem == nil || queueItem.LoopID == nil || *queueItem.LoopID != input.LoopID {
			return fmt.Errorf("finalize worker success: queue item %s does not belong to loop %s", input.QueueItemID, input.LoopID)
		}
		loop, err := repos.Loops.GetByID(ctx, input.LoopID)
		if err != nil {
			return err
		}
		if loop == nil {
			return fmt.Errorf("finalize worker success: loop not found: %s", input.LoopID)
		}

		if err := repos.Runs.Upsert(ctx, input.Run); err != nil {
			return err
		}
		if queueItem.Status == "queued" || queueItem.Status == "running" {
			if err := repos.Queue.Complete(ctx, input.QueueItemID, input.FinishedAt); err != nil {
				return err
			}
		}
		if loop.Status != "terminated" {
			loop.Status = input.LoopStatus
			loop.LastRunAt = &input.FinishedAt
			loop.NextRunAt = nil
			loop.UpdatedAt = input.FinishedAt
			if err := repos.Loops.Upsert(ctx, *loop); err != nil {
				return err
			}
		}
		return nil
	})
}
