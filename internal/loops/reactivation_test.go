package loops

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/domain"
	"github.com/MumuTW/looper/internal/storage"
)

func TestReactivateQueueRevivesCancelledQueueWithoutDuplicate(t *testing.T) {
	t.Parallel()
	ctx, db, repos, loop, nowISO := reactivationFixture(t)
	projectID, loopID := loop.ProjectID, loop.ID
	repo := "acme/looper"
	pr := int64(42)
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue_cancelled", ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:acme/looper:42", Repo: &repo, PRNumber: &pr, DedupeKey: "reviewer:project_1:" + loopID + ":acme/looper:42", Priority: storage.QueuePriorityReviewer, Status: "cancelled", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	_ = reactivateForTest(t, ctx, db, repos, loop, nowISO)
	active, err := repos.Queue.FindActiveByLoopID(ctx, loop.ID)
	if err != nil || active == nil || active.ID != "queue_cancelled" || active.Status != "queued" {
		t.Fatalf("active queue = %#v, %v; want cancelled queue revived", active, err)
	}
	if count := countQueuesForLoop(t, ctx, repos, loop.ID); count != 1 {
		t.Fatalf("queue count = %d, want one", count)
	}
}

func TestReactivateQueueCreatesCleanReplacementForTerminalQueue(t *testing.T) {
	t.Parallel()
	ctx, db, repos, loop, nowISO := reactivationFixture(t)
	projectID, loopID := loop.ProjectID, loop.ID
	repo := "acme/looper"
	pr := int64(42)
	claimedBy, lastError := "worker", "validation failed"
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue_failed", ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:acme/looper:42", Repo: &repo, PRNumber: &pr, DedupeKey: "reviewer:project_1:" + loopID + ":acme/looper:42", Priority: storage.QueuePriorityReviewer, Status: "failed", AvailableAt: nowISO, Attempts: 3, MaxAttempts: 3, ClaimedBy: &claimedBy, LastError: &lastError, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	_ = reactivateForTest(t, ctx, db, repos, loop, nowISO)
	active, err := repos.Queue.FindActiveByLoopID(ctx, loop.ID)
	if err != nil || active == nil || active.ID == "queue_failed" || active.Status != "queued" || active.Attempts != 0 || active.ClaimedBy != nil || active.LastError != nil {
		t.Fatalf("active queue = %#v, %v; want clean replacement", active, err)
	}
}

func TestReactivateQueueCreatesFirstReviewerQueueAndRejectsSiblingConflict(t *testing.T) {
	t.Parallel()
	ctx, db, repos, loop, nowISO := reactivationFixture(t)
	_ = reactivateForTest(t, ctx, db, repos, loop, nowISO)
	active, err := repos.Queue.FindActiveByLoopID(ctx, loop.ID)
	if err != nil || active == nil || active.Type != "reviewer" {
		t.Fatalf("active queue = %#v, %v; want first reviewer queue", active, err)
	}
	otherID := "loop_conflict"
	repo := "acme/looper"
	pr := int64(42)
	targetID := "pull_request:acme/looper:42"
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: otherID, Seq: 2, ProjectID: loop.ProjectID, Type: string(domain.LoopTypeReviewer), TargetType: string(domain.LoopTargetTypePullRequest), TargetID: &targetID, Repo: &repo, PRNumber: &pr, Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert(conflict) error = %v", err)
	}
	if err := storage.WithTransaction(ctx, db, nil, func(tx *sql.Tx) error {
		_, err := ReactivateQueue(ctx, storage.NewRepositories(tx), QueueReactivationInput{Loop: loop, NowISO: nowISO, MaxAttempts: 3})
		return err
	}); err == nil {
		t.Fatal("ReactivateQueue() error = nil, want active sibling conflict")
	}
}

func reactivationFixture(t *testing.T) (context.Context, *sql.DB, *storage.Repositories, storage.LoopRecord, string) {
	t.Helper()
	ctx := context.Background()
	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	nowISO := now.Format("2006-01-02T15:04:05.000Z")
	seedProject(t, repos, now)
	repo := "acme/looper"
	pr := int64(42)
	targetID := "pull_request:acme/looper:42"
	loop := storage.LoopRecord{ID: "loop_reactivate", Seq: 1, ProjectID: "project_1", Type: string(domain.LoopTypeReviewer), TargetType: string(domain.LoopTargetTypePullRequest), TargetID: &targetID, Repo: &repo, PRNumber: &pr, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	return ctx, coordinator.DB(), repos, loop, nowISO
}

func reactivateForTest(t *testing.T, ctx context.Context, db *sql.DB, repos *storage.Repositories, loop storage.LoopRecord, nowISO string) QueueReactivationResult {
	t.Helper()
	result, err := storage.WithTransactionValue(ctx, db, nil, func(tx *sql.Tx) (QueueReactivationResult, error) {
		return ReactivateQueue(ctx, storage.NewRepositories(tx), QueueReactivationInput{Loop: loop, NowISO: nowISO, MaxAttempts: 3})
	})
	if err != nil {
		t.Fatalf("ReactivateQueue() error = %v", err)
	}
	return result
}

func countQueuesForLoop(t *testing.T, ctx context.Context, repos *storage.Repositories, loopID string) int {
	t.Helper()
	items, err := repos.Queue.List(ctx)
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	count := 0
	for _, item := range items {
		if item.LoopID != nil && *item.LoopID == loopID {
			count++
		}
	}
	return count
}
