package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func openHITLRecoveryRepos(t *testing.T) (*storage.Repositories, string) {
	t.Helper()
	root := t.TempDir()
	now := time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC)
	nowISO := now.Format("2006-01-02T15:04:05.000Z")
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), storage.SQLiteCoordinatorOptions{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background(), storage.RunPendingOptions{}); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: "project_1", Name: "HITL recovery", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert: %v", err)
	}
	return repos, nowISO
}

// Crash after parkHITLLoop (DeliveryPending, AskCommentID==0) must be recoverable
// at startup: roll back park and requeue so suspend can finish delivery.
func TestRecoverIncompleteHITLGitHubDelivery_BeforePost(t *testing.T) {
	t.Parallel()
	repos, nowISO := openHITLRecoveryRepos(t)
	ctx := context.Background()

	repo := "acme/looper"
	pr := int64(87)
	meta, err := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: "recover before post?", Options: []string{"a", "b"},
		SessionID: "sess-rec", ExecutionID: "agent-rec", Status: "awaiting",
		AskedAt: nowISO, Transport: "github", Provider: "github", PRNumber: pr,
		DeliveryPending: true, Role: "fixer",
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk: %v", err)
	}
	loop := storage.LoopRecord{
		ID: "loop_hitl_delivery_pending", Seq: 501, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr,
		Status: "awaiting_human", MetadataJSON: &meta,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	loopID := loop.ID
	projectID := loop.ProjectID
	_ = repos.Queue.Upsert(ctx, storage.QueueItemRecord{
		ID: "queue_hitl_delivery_pending", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "cancelled",
		DedupeKey: "fixer:delivery-pending", Priority: storage.QueuePriorityFixer, AvailableAt: nowISO,
		MaxAttempts: 3, FinishedAt: &nowISO,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	})

	recovered, err := recoverIncompleteHITLGitHubDelivery(ctx, repos, loop, nowISO)
	if err != nil {
		t.Fatalf("recoverIncompleteHITLGitHubDelivery: %v", err)
	}
	if !recovered {
		t.Fatal("want recovered=true for delivery-pending park")
	}
	got, err := repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID: %v", err)
	}
	if got.Status != "running" {
		t.Fatalf("status = %q, want running", got.Status)
	}
	if _, ok := loops.ReadHITLAsk(got.MetadataJSON); ok {
		t.Fatal("incomplete HITL ask must be cleared so suspend re-parks cleanly")
	}
	qi, err := repos.Queue.GetLatestByLoopID(ctx, loop.ID)
	if err != nil || qi == nil {
		t.Fatalf("Queue.GetLatestByLoopID: %v", err)
	}
	if qi.Status != "queued" {
		t.Fatalf("queue status = %q, want queued", qi.Status)
	}
}

// Complete parks (AskCommentID set, DeliveryPending false) must not be recovered.
func TestRecoverIncompleteHITLGitHubDelivery_CompleteParkUntouched(t *testing.T) {
	t.Parallel()
	repos, nowISO := openHITLRecoveryRepos(t)
	ctx := context.Background()

	repo := "acme/looper"
	pr := int64(87)
	meta, err := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: "complete park?", Options: []string{"a"},
		Status: "awaiting", Transport: "github", Provider: "github", PRNumber: pr,
		AskCommentID: 99, DeliveryPending: false,
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk: %v", err)
	}
	loop := storage.LoopRecord{
		ID: "loop_hitl_complete_park", Seq: 502, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr,
		Status: "awaiting_human", MetadataJSON: &meta,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = repos.Loops.Upsert(ctx, loop)

	recovered, err := recoverIncompleteHITLGitHubDelivery(ctx, repos, loop, nowISO)
	if err != nil {
		t.Fatalf("recoverIncompleteHITLGitHubDelivery: %v", err)
	}
	if recovered {
		t.Fatal("complete park must not be recovered")
	}
	got, _ := repos.Loops.GetByID(ctx, loop.ID)
	if got.Status != "awaiting_human" {
		t.Fatalf("status = %q, want still awaiting_human", got.Status)
	}
	ask, ok := loops.ReadHITLAsk(got.MetadataJSON)
	if !ok || ask.AskCommentID != 99 {
		t.Fatalf("complete park ask must remain; got ok=%v ask=%+v", ok, ask)
	}
}
