package loops

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/MumuTW/looper/internal/domain"
	"github.com/MumuTW/looper/internal/storage"
)

func TestRecordHITLAnswerPersistsAnswerWithoutChangingLifecycleState(t *testing.T) {
	ctx, db, repos, loop, nowISO := reactivationFixture(t)
	metadata, err := WriteHITLAsk(loop.MetadataJSON, HITLAsk{Question: "continue?", Status: "awaiting"})
	if err != nil {
		t.Fatalf("WriteHITLAsk() error = %v", err)
	}
	loop.Status, loop.MetadataJSON = string(domain.LoopStatusAwaitingHuman), &metadata
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	result, err := storage.WithTransactionValue(ctx, db, nil, func(tx *sql.Tx) (HITLAnswerResult, error) {
		return RecordHITLAnswer(ctx, storage.NewRepositories(tx), HITLAnswerInput{LoopID: loop.ID, Answer: "yes", NowISO: nowISO, RequireExistingAsk: true})
	})
	if err != nil || !result.Applied || result.Loop.Status != string(domain.LoopStatusAwaitingHuman) {
		t.Fatalf("RecordHITLAnswer() = %#v, %v; want applied awaiting_human loop", result, err)
	}
	ask, ok := ReadHITLAsk(result.Loop.MetadataJSON)
	if !ok || ask.Answer != "yes" || ask.Status != "answered" || ask.AnsweredAt != nowISO {
		t.Fatalf("ReadHITLAsk() = %#v, %v; want answered ask", ask, ok)
	}
}

func TestRecordHITLAnswerRequiresAskForPollingLane(t *testing.T) {
	ctx, db, repos, loop, nowISO := reactivationFixture(t)
	loop.Status = string(domain.LoopStatusAwaitingHuman)
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	result, err := storage.WithTransactionValue(ctx, db, nil, func(tx *sql.Tx) (HITLAnswerResult, error) {
		return RecordHITLAnswer(ctx, storage.NewRepositories(tx), HITLAnswerInput{LoopID: loop.ID, Answer: "stale", NowISO: nowISO, RequireExistingAsk: true})
	})
	if err != nil || result.Applied || result.Loop.MetadataJSON != nil {
		t.Fatalf("RecordHITLAnswer() = %#v, %v; want polling no-op without ask", result, err)
	}
}

func TestRecordHITLAnswerCanResumeAndRequeueInSameTransaction(t *testing.T) {
	ctx, db, repos, loop, nowISO := reactivationFixture(t)
	metadata, err := WriteHITLAsk(loop.MetadataJSON, HITLAsk{Question: "continue?", Status: "awaiting"})
	if err != nil {
		t.Fatalf("WriteHITLAsk() error = %v", err)
	}
	loop.Status, loop.MetadataJSON = string(domain.LoopStatusAwaitingHuman), &metadata
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID, loopID := loop.ProjectID, loop.ID
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{
		ID: "queue_hitl_resume", ProjectID: &projectID, LoopID: &loopID,
		Type: loop.Type, TargetType: loop.TargetType, TargetID: *loop.TargetID,
		DedupeKey: "hitl-resume", Priority: storage.QueuePriorityReviewer,
		Status: "cancelled", AvailableAt: nowISO, MaxAttempts: 3,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	result, err := storage.WithTransactionValue(ctx, db, nil, func(tx *sql.Tx) (HITLAnswerResult, error) {
		return RecordHITLAnswer(ctx, storage.NewRepositories(tx), HITLAnswerInput{
			LoopID: loop.ID, Answer: "yes", NowISO: nowISO,
			RequireExistingAsk: true, Resume: true,
		})
	})
	if err != nil || !result.Applied || result.Loop.Status != string(domain.LoopStatusRunning) {
		t.Fatalf("RecordHITLAnswer() = %#v, %v; want applied running loop", result, err)
	}
	active, err := repos.Queue.FindActiveByLoopID(ctx, loop.ID)
	if err != nil {
		t.Fatalf("FindActiveByLoopID() error = %v", err)
	}
	if active == nil || active.Status != "queued" {
		t.Fatalf("active queue = %#v, want queued", active)
	}
}

func TestRecordHITLAnswerRollsBackLifecycleWhenTransactionIsInterrupted(t *testing.T) {
	ctx, db, repos, loop, nowISO := reactivationFixture(t)
	metadata, err := WriteHITLAsk(loop.MetadataJSON, HITLAsk{Question: "continue?", Status: "awaiting"})
	if err != nil {
		t.Fatalf("WriteHITLAsk() error = %v", err)
	}
	loop.Status, loop.MetadataJSON = string(domain.LoopStatusAwaitingHuman), &metadata
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID, loopID := loop.ProjectID, loop.ID
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{
		ID: "queue_hitl_rollback", ProjectID: &projectID, LoopID: &loopID,
		Type: loop.Type, TargetType: loop.TargetType, TargetID: *loop.TargetID,
		DedupeKey: "hitl-rollback", Priority: storage.QueuePriorityReviewer,
		Status: "cancelled", AvailableAt: nowISO, MaxAttempts: 3,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	interrupt := errors.New("simulated interruption after lifecycle write")
	_, err = storage.WithTransactionValue(ctx, db, nil, func(tx *sql.Tx) (HITLAnswerResult, error) {
		result, recordErr := RecordHITLAnswer(ctx, storage.NewRepositories(tx), HITLAnswerInput{
			LoopID: loop.ID, Answer: "yes", NowISO: nowISO,
			RequireExistingAsk: true, Resume: true,
		})
		if recordErr != nil {
			return result, recordErr
		}
		return result, interrupt
	})
	if !errors.Is(err, interrupt) {
		t.Fatalf("transaction error = %v, want interruption", err)
	}
	stored, err := repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || stored == nil || stored.Status != string(domain.LoopStatusAwaitingHuman) {
		t.Fatalf("stored loop after rollback = %#v, %v; want awaiting_human", stored, err)
	}
	ask, ok := ReadHITLAsk(stored.MetadataJSON)
	if !ok || ask.Status != "awaiting" || ask.Answer != "" {
		t.Fatalf("ask after rollback = %#v, %v; want unanswered ask", ask, ok)
	}
	active, err := repos.Queue.FindActiveByLoopID(ctx, loop.ID)
	if err != nil {
		t.Fatalf("FindActiveByLoopID() after rollback error = %v", err)
	}
	if active != nil {
		t.Fatalf("active queue after rollback = %#v, want nil", active)
	}
}
