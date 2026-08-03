package loops

import (
	"context"
	"database/sql"
	"testing"

	"github.com/MumuTW/looper/internal/domain"
	"github.com/MumuTW/looper/internal/storage"
)

func TestPrepareHandbackCapturesSessionAndCancelsQueueTogether(t *testing.T) {
	ctx, db, repos, loop, nowISO := reactivationFixture(t)
	loop.Status = string(domain.LoopStatusHumanTakeover)
	if err := repos.Loops.UpsertChangingHumanHold(ctx, loop); err != nil {
		t.Fatalf("Loops.UpsertChangingHumanHold() error = %v", err)
	}
	projectID, loopID := loop.ProjectID, loop.ID
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue_handback", ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:acme/looper:42", DedupeKey: "reviewer:handback", Priority: storage.QueuePriorityReviewer, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	sessionID := "human-session-42"
	if err := repos.AgentExecutions.Upsert(ctx, storage.AgentExecutionRecord{ID: "execution_handback", LoopID: &loopID, Vendor: "codex", Status: "completed", NativeSessionID: &sessionID, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	err := storage.WithTransaction(ctx, db, nil, func(tx *sql.Tx) error {
		return PrepareHandback(ctx, storage.NewRepositories(tx), HandbackPreparationInput{LoopID: loop.ID, NowISO: nowISO})
	})
	if err != nil {
		t.Fatalf("PrepareHandback() error = %v", err)
	}
	persistedLoop, err := repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || persistedLoop == nil || persistedLoop.MetadataJSON == nil {
		t.Fatalf("PrepareHandback() persisted loop = %#v, %v; want metadata", persistedLoop, err)
	}
	resume, ok := ReadTakeoverResume(persistedLoop.MetadataJSON)
	if !ok || resume.SessionID != sessionID {
		t.Fatalf("ReadTakeoverResume() = %#v, %v; want session %q", resume, ok, sessionID)
	}
	queue, err := repos.Queue.GetByID(context.Background(), "queue_handback")
	if err != nil || queue == nil || queue.Status != "cancelled" {
		t.Fatalf("Queue.GetByID() = %#v, %v; want cancelled", queue, err)
	}
}

func TestTakeoverResumeRoundTrip(t *testing.T) {
	if _, ok := ReadTakeoverResume(nil); ok {
		t.Fatal("ReadTakeoverResume(nil) should be absent")
	}
	base := `{"worker":{"title":"x"}}`
	meta, err := WriteTakeoverResume(&base, TakeoverResume{SessionID: "019f-abc"})
	if err != nil {
		t.Fatalf("WriteTakeoverResume error = %v", err)
	}
	tr, ok := ReadTakeoverResume(&meta)
	if !ok || tr.SessionID != "019f-abc" {
		t.Fatalf("ReadTakeoverResume = %+v ok=%v", tr, ok)
	}
	// Preserves other keys.
	if got, _ := ReadTakeoverResume(&meta); got.SessionID == "" {
		t.Fatal("session id lost")
	}
	cleared, err := ClearTakeoverResume(&meta)
	if err != nil {
		t.Fatalf("ClearTakeoverResume error = %v", err)
	}
	if _, ok := ReadTakeoverResume(&cleared); ok {
		t.Fatal("marker should be gone after clear")
	}
}
