package loops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/MumuTW/looper/internal/domain"
	"github.com/MumuTW/looper/internal/eventlog"
	"github.com/MumuTW/looper/internal/storage"
)

// These errors describe durable queue-reactivation failures. Entry points map
// them to their own transport errors; the authority remains the loop and queue
// records passed to ReactivateQueue, not the HTTP handler.
var (
	ErrActiveLoopConflict       = errors.New("active loop conflict")
	ErrInvalidQueueTarget       = errors.New("invalid queue target")
	ErrReactivationUnconfigured = errors.New("queue reactivation is not configured")
)

// QueueReactivationInput is a transaction-local activation request. Callers
// must acquire any process/worktree locks before opening their transaction and
// pass the freshly read loop record from that transaction.
type QueueReactivationInput struct {
	Loop        storage.LoopRecord
	NowISO      string
	MaxAttempts int64
}

type QueueReactivationResult struct {
	Loop      storage.LoopRecord
	QueueItem *storage.QueueItemRecord
}

// ReactivateQueue moves a loop into running and ensures exactly one claimable
// queue item exists. It is deliberately transaction-local: lifecycle callers
// retain their external process and worktree locks while this function owns the
// durable loop/queue transition.
func ReactivateQueue(ctx context.Context, repos *storage.Repositories, input QueueReactivationInput) (QueueReactivationResult, error) {
	if repos == nil || repos.Loops == nil || repos.Queue == nil {
		return QueueReactivationResult{}, ErrReactivationUnconfigured
	}
	if strings.TrimSpace(input.Loop.ID) == "" || strings.TrimSpace(input.NowISO) == "" {
		return QueueReactivationResult{}, fmt.Errorf("%w: loop id and time are required", ErrInvalidQueueTarget)
	}

	current, err := repos.Loops.GetByID(ctx, input.Loop.ID)
	if err != nil {
		return QueueReactivationResult{}, err
	}
	if current == nil {
		return QueueReactivationResult{}, fmt.Errorf("%w: %s", ErrLoopNotFound, input.Loop.ID)
	}
	if err := AssertUniqueActiveLoopRecords(ctx, repos.Loops, *current, domain.LoopStatusRunning); err != nil {
		return QueueReactivationResult{}, err
	}
	target, err := targetFromRecord(*current)
	if err != nil {
		return QueueReactivationResult{}, fmt.Errorf("%w: %v", ErrInvalidQueueTarget, err)
	}

	updated := *current
	updated.Status = string(domain.LoopStatusRunning)
	updated.NextRunAt = &input.NowISO
	updated.UpdatedAt = input.NowISO
	if err := repos.Loops.Upsert(ctx, updated); err != nil {
		return QueueReactivationResult{}, err
	}
	result := QueueReactivationResult{Loop: updated}

	requeued, err := repos.Queue.RequeueLatestCancelledByLoop(ctx, updated.ID, input.NowISO)
	if err != nil {
		return QueueReactivationResult{}, err
	}
	if requeued > 0 {
		queue, err := repos.Queue.GetLatestByLoopID(ctx, updated.ID)
		if err != nil {
			return QueueReactivationResult{}, err
		}
		result.QueueItem = queue
		return result, nil
	}

	activeQueue, err := repos.Queue.FindActiveByLoopID(ctx, updated.ID)
	if err != nil {
		return QueueReactivationResult{}, err
	}
	if activeQueue != nil {
		result.QueueItem = activeQueue
		return result, nil
	}

	latestQueue, err := repos.Queue.GetLatestByLoopID(ctx, updated.ID)
	if err != nil {
		return QueueReactivationResult{}, err
	}
	if latestQueue != nil {
		if latestQueue.Status == "queued" || latestQueue.Status == "running" {
			result.QueueItem = latestQueue
			return result, nil
		}
		if latestQueue.DedupeKey != "" {
			activeDedupe, err := repos.Queue.FindActiveByDedupe(ctx, latestQueue.DedupeKey)
			if err != nil {
				return QueueReactivationResult{}, err
			}
			if activeDedupe != nil {
				result.QueueItem = activeDedupe
				return result, nil
			}
		}
		replacement := *latestQueue
		replacement.ID = eventlog.NewEventID("queue")
		replacement.Status = "queued"
		replacement.AvailableAt = input.NowISO
		replacement.Attempts = 0
		replacement.ClaimedBy = nil
		replacement.ClaimedAt = nil
		replacement.StartedAt = nil
		replacement.FinishedAt = nil
		replacement.LastError = nil
		replacement.LastErrorKind = nil
		replacement.CreatedAt = input.NowISO
		replacement.UpdatedAt = input.NowISO
		persisted, _, err := repos.Queue.UpsertActiveByDedupeOrGetExisting(ctx, replacement)
		if err != nil {
			return QueueReactivationResult{}, err
		}
		result.QueueItem = &persisted
		return result, nil
	}

	queue, ok, err := BuildQueuedLoopQueueRecord(updated, target, input.NowISO, updated.MetadataJSON, input.MaxAttempts)
	if err != nil {
		return QueueReactivationResult{}, err
	}
	if !ok {
		return result, nil
	}
	persisted, _, err := repos.Queue.UpsertActiveByDedupeOrGetExisting(ctx, queue)
	if err != nil {
		return QueueReactivationResult{}, err
	}
	result.QueueItem = &persisted
	return result, nil
}

// AssertUniqueActiveLoopRecords applies the domain active-loop invariant to
// storage records. It accepts the candidate's own id, so it can validate a
// reactivation without conflicting with itself.
func AssertUniqueActiveLoopRecords(ctx context.Context, loopsRepo *storage.LoopsRepository, candidate storage.LoopRecord, status domain.LoopStatus) error {
	if loopsRepo == nil {
		return ErrReactivationUnconfigured
	}
	if !domain.IsConflictingActiveLoopStatus(status) {
		return nil
	}
	existing, err := loopsRepo.List(ctx)
	if err != nil {
		return err
	}
	target, err := targetFromRecord(candidate)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidQueueTarget, err)
	}
	for _, loop := range existing {
		if loop.ID == candidate.ID || !domain.IsConflictingActiveLoopStatus(domain.LoopStatus(loop.Status)) {
			continue
		}
		other, err := targetFromRecord(loop)
		if err != nil {
			continue
		}
		if loop.ProjectID == candidate.ProjectID &&
			domain.LoopType(loop.Type) == domain.LoopTypeWorker &&
			domain.LoopType(candidate.Type) == domain.LoopTypeWorker &&
			other.TargetType == domain.LoopTargetTypeProject &&
			target.TargetType == domain.LoopTargetTypeProject {
			continue
		}
		if loop.ProjectID == candidate.ProjectID && domain.LoopType(loop.Type) == domain.LoopType(candidate.Type) && domain.LoopTargetKey(other) == domain.LoopTargetKey(target) {
			return fmt.Errorf("%w: active loop already exists for %s:%s:%s", ErrActiveLoopConflict, candidate.ProjectID, candidate.Type, domain.LoopTargetKey(target))
		}
	}
	return nil
}

// BuildQueuedLoopQueueRecord constructs the first queue item for a durable
// loop. It is shared by explicit retry preflight and transaction-local
// activation so those paths cannot disagree about queue identity.
func BuildQueuedLoopQueueRecord(record storage.LoopRecord, target domain.LoopTarget, nowISO string, metadataJSON *string, maxAttempts int64) (storage.QueueItemRecord, bool, error) {
	queueType := domain.LoopType(record.Type)
	if queueType != domain.LoopTypeReviewer && queueType != domain.LoopTypeFixer && queueType != domain.LoopTypeWorker && queueType != domain.LoopTypePlanner {
		return storage.QueueItemRecord{}, false, nil
	}
	projectID, loopID := record.ProjectID, record.ID
	queue := storage.QueueItemRecord{ID: eventlog.NewEventID("queue"), ProjectID: &projectID, LoopID: &loopID, Type: record.Type, TargetType: record.TargetType, TargetID: stringOrEmpty(record.TargetID), Repo: record.Repo, PRNumber: record.PRNumber, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: maxAttempts, CreatedAt: nowISO, UpdatedAt: nowISO}
	switch queueType {
	case domain.LoopTypePlanner:
		repo := strings.TrimSpace(stringOrEmpty(record.Repo))
		if target.TargetType != domain.LoopTargetTypeIssue || repo == "" || target.IssueNumber <= 0 {
			return storage.QueueItemRecord{}, false, fmt.Errorf("%w: planner loop requires repo and issueNumber", ErrInvalidQueueTarget)
		}
		lockKey := storage.IssueLockKey(record.ProjectID, repo, target.IssueNumber)
		payload := map[string]any{"issueNumber": target.IssueNumber}
		if metadataBool(metadataJSON, "manual") {
			payload["manual"] = true
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return storage.QueueItemRecord{}, false, err
		}
		payloadJSON := string(encoded)
		queue.TargetType, queue.TargetID, queue.Repo, queue.PRNumber = string(domain.LoopTargetTypeIssue), fmt.Sprintf("issue:%s:%d", repo, target.IssueNumber), &repo, nil
		queue.DedupeKey, queue.Priority, queue.LockKey, queue.PayloadJSON = fmt.Sprintf("planner:%s:%s:%s:%d", record.ProjectID, record.ID, repo, target.IssueNumber), storage.QueuePriorityPlanner, &lockKey, &payloadJSON
	case domain.LoopTypeReviewer, domain.LoopTypeFixer:
		repo := strings.TrimSpace(stringOrEmpty(record.Repo))
		if repo == "" || record.PRNumber == nil {
			return storage.QueueItemRecord{}, false, fmt.Errorf("%w: %s loop requires repo and prNumber", ErrInvalidQueueTarget, record.Type)
		}
		pr := *record.PRNumber
		lockKey := storage.PullRequestLockKey(record.ProjectID, repo, pr)
		queue.TargetType, queue.TargetID, queue.Repo, queue.PRNumber = string(domain.LoopTargetTypePullRequest), fmt.Sprintf("pr:%s:%d", repo, pr), &repo, &pr
		queue.Priority, queue.LockKey = storage.QueuePriorityReviewer, &lockKey
		if queueType == domain.LoopTypeReviewer {
			queue.DedupeKey = fmt.Sprintf("reviewer:%s:%s:%s:%d", record.ProjectID, record.ID, repo, pr)
		} else {
			queue.DedupeKey, queue.Priority = fmt.Sprintf("fixer:%s", record.ID), storage.QueuePriorityFixer
		}
	case domain.LoopTypeWorker:
		queue.Priority, queue.DedupeKey = storage.QueuePriorityWorker, fmt.Sprintf("worker:%s", record.ID)
		lockKey := fmt.Sprintf("worker:%s", record.ID)
		if payload := workerPayloadJSON(metadataJSON); payload != nil {
			queue.PayloadJSON = payload
		}
		switch target.TargetType {
		case domain.LoopTargetTypeIssue:
			repo := strings.TrimSpace(stringOrEmpty(record.Repo))
			if repo == "" || target.IssueNumber <= 0 {
				return storage.QueueItemRecord{}, false, fmt.Errorf("%w: worker loop requires repo and issueNumber", ErrInvalidQueueTarget)
			}
			lockKey = storage.IssueLockKey(record.ProjectID, repo, target.IssueNumber)
			queue.TargetType, queue.TargetID, queue.Repo, queue.PRNumber = string(domain.LoopTargetTypeIssue), fmt.Sprintf("issue:%s:%d", repo, target.IssueNumber), &repo, nil
			queue.DedupeKey = fmt.Sprintf("worker:%s:%s:%d", record.ProjectID, repo, target.IssueNumber)
		case domain.LoopTargetTypePullRequest:
			repo := strings.TrimSpace(stringOrEmpty(record.Repo))
			if repo == "" || record.PRNumber == nil {
				return storage.QueueItemRecord{}, false, fmt.Errorf("%w: worker loop requires repo and prNumber", ErrInvalidQueueTarget)
			}
			pr := *record.PRNumber
			lockKey = storage.PullRequestLockKey(record.ProjectID, repo, pr)
			queue.TargetType, queue.TargetID, queue.Repo, queue.PRNumber = string(domain.LoopTargetTypePullRequest), fmt.Sprintf("pr:%s:%d", repo, pr), &repo, &pr
			queue.DedupeKey = fmt.Sprintf("worker:%s:%s:%d", record.ProjectID, repo, pr)
		}
		queue.LockKey = &lockKey
	}
	return queue, true, nil
}

func workerPayloadJSON(metadataJSON *string) *string {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return nil
	}
	var metadata map[string]any
	if json.Unmarshal([]byte(*metadataJSON), &metadata) != nil {
		return nil
	}
	worker, ok := metadata["worker"].(map[string]any)
	if !ok || len(worker) == 0 {
		return nil
	}
	encoded, err := json.Marshal(worker)
	if err != nil {
		return nil
	}
	value := string(encoded)
	return &value
}

func metadataBool(metadataJSON *string, key string) bool {
	if metadataJSON == nil {
		return false
	}
	var metadata map[string]any
	return json.Unmarshal([]byte(*metadataJSON), &metadata) == nil && metadata[key] == true
}

func stringOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
