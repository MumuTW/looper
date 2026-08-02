package runpipe

import (
	"context"
	"time"

	"github.com/MumuTW/looper/internal/eventlog"
	"github.com/MumuTW/looper/internal/storage"
)

func PersistStepStarted[C any](ctx context.Context, repos *storage.Repositories, nowISO string, run storage.RunRecord, step string, checkpoint C) (storage.RunRecord, error) {
	updated := run
	updated.CurrentStep = StringPtr(step)
	encoded := MustMarshalJSON(checkpoint)
	updated.CheckpointJSON = &encoded
	updated.LastHeartbeatAt = &nowISO
	updated.UpdatedAt = nowISO
	if err := repos.Runs.Upsert(ctx, updated); err != nil {
		return storage.RunRecord{}, err
	}
	return updated, nil
}

func PersistStepCompleted[C any](ctx context.Context, repos *storage.Repositories, nowISO string, run storage.RunRecord, completedStep, nextStep string, checkpoint C) (storage.RunRecord, error) {
	updated := run
	if nextStep != "" {
		updated.CurrentStep = StringPtr(nextStep)
	} else {
		updated.CurrentStep = nil
	}
	updated.LastCompletedStep = StringPtr(completedStep)
	encoded := MustMarshalJSON(checkpoint)
	updated.CheckpointJSON = &encoded
	updated.LastHeartbeatAt = &nowISO
	updated.UpdatedAt = nowISO
	if err := repos.Runs.Upsert(ctx, updated); err != nil {
		return storage.RunRecord{}, err
	}
	return updated, nil
}

func CompleteRun[C any](ctx context.Context, repos *storage.Repositories, nowISO string, run storage.RunRecord, status, summary, errorMessage string, checkpoint C) (storage.RunRecord, error) {
	updated := run
	updated.Status = status
	if summary != "" {
		updated.Summary = StringPtr(summary)
	}
	if errorMessage != "" {
		updated.ErrorMessage = StringPtr(errorMessage)
	}
	encoded := MustMarshalJSON(checkpoint)
	updated.CheckpointJSON = &encoded
	updated.EndedAt = &nowISO
	updated.LastHeartbeatAt = &nowISO
	updated.UpdatedAt = nowISO
	if err := repos.Runs.Upsert(ctx, updated); err != nil {
		return storage.RunRecord{}, err
	}
	return updated, nil
}

type BackoffFunc func(base time.Duration, attempts int64) time.Duration

func FailQueueItem(ctx context.Context, repos *storage.Repositories, now time.Time, nowISO string, retryBaseDelay time.Duration, queueItem storage.QueueItemRecord, kind QueueFailureKind, message string, backoff BackoffFunc) (*storage.QueueItemRecord, error) {
	nextAttempts := queueItem.Attempts + 1
	if !ShouldRetryQueueFailure(kind, nextAttempts, queueItem.MaxAttempts) {
		if err := repos.Queue.Fail(ctx, storage.QueueFailInput{ID: queueItem.ID, Attempts: nextAttempts, FinishedAt: nowISO, ErrorMessage: OptionalString(message), ErrorKind: string(kind), UpdatedAt: nowISO}); err != nil {
			return nil, err
		}
		return repos.Queue.GetByID(ctx, queueItem.ID)
	}
	retryAt := eventlog.FormatJavaScriptISOString(now.Add(backoff(retryBaseDelay, CappedRetryDelayAttempt(nextAttempts, queueItem.MaxAttempts))))
	if err := repos.Queue.MarkRetry(ctx, storage.QueueMarkRetryInput{ID: queueItem.ID, AvailableAt: retryAt, Attempts: nextAttempts, ErrorMessage: OptionalString(message), ErrorKind: string(kind), UpdatedAt: nowISO}); err != nil {
		return nil, err
	}
	return repos.Queue.GetByID(ctx, queueItem.ID)
}
