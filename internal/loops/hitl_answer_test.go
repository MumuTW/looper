package loops

import (
	"database/sql"
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
