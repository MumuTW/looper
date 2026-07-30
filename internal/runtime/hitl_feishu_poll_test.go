package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestPollFeishuHITLInboxOnceAllSucceed(t *testing.T) {
	rootToLoop := map[string]string{"om_root_1": "loop-a"}
	seqToLoop := map[int64]string{71: "loop-seq71"}
	var answers, messages []string
	deps := feishuHITLPollDeps{
		loopByRoot: func(_ contextType, root string) string { return rootToLoop[root] },
		loopBySeq:  func(_ contextType, seq int64) string { return seqToLoop[seq] },
		deliverAnswer: func(_ contextType, loopID, answer string) error {
			answers = append(answers, loopID+"="+answer)
			return nil
		},
		enqueueMessage: func(_ contextType, loopID, text string) error {
			messages = append(messages, loopID+"="+text)
			return nil
		},
	}
	events := []feishuInboxEvent{
		{ID: 10, Kind: "message", RootID: "om_root_1", Text: "用 A"},
		{ID: 11, Kind: "message", RootID: "om_other", Text: "not ours"},
		{ID: 12, Kind: "message", RootID: "om_root_1", Text: "   "},
		makeCardAction(15, "71", "redis"),
	}
	n, newCursor := pollFeishuHITLInboxOnce(context.Background(), events, deps, 0)
	if n != 2 {
		t.Fatalf("handled = %d, want 2", n)
	}
	if newCursor != 15 {
		t.Fatalf("newCursor = %d, want 15", newCursor)
	}
	if len(messages) != 1 || messages[0] != "loop-a=用 A" {
		t.Fatalf("enqueued messages = %v", messages)
	}
	if len(answers) != 1 || answers[0] != "loop-seq71=redis" {
		t.Fatalf("delivered answers = %v", answers)
	}
}

func TestPollFeishuHITLInboxRetriesFailedDeliveryOnNextPoll(t *testing.T) {
	deps := feishuHITLPollDeps{
		loopBySeq: func(_ contextType, seq int64) string { return "loop-seq" },
	}
	events := []feishuInboxEvent{
		makeCardAction(9, "1", "ok"),
		makeCardAction(10, "1", "ok"),
		makeCardAction(11, "1", "ok"),
		makeCardAction(12, "1", "ok"),
	}

	var callCount int
	deps.deliverAnswer = func(_ contextType, loopID, answer string) error {
		callCount++
		if callCount == 2 {
			return fmt.Errorf("temporary failure")
		}
		return nil
	}

	n, newCursor := pollFeishuHITLInboxOnce(context.Background(), events, deps, 0)
	if n != 1 {
		t.Fatalf("handled = %d, want 1 before the failed event", n)
	}
	if newCursor != 9 {
		t.Fatalf("newCursor = %d, want 9 (blocked on failed event 10)", newCursor)
	}
	if callCount != 2 {
		t.Fatalf("delivery attempts = %d, want 2 (do not apply events after failure)", callCount)
	}

	n, newCursor = pollFeishuHITLInboxOnce(context.Background(), events[1:], deps, newCursor)
	if n != 3 {
		t.Fatalf("retry handled = %d, want 3", n)
	}
	if newCursor != 12 {
		t.Fatalf("retry newCursor = %d, want 12", newCursor)
	}
	if callCount != 5 {
		t.Fatalf("delivery attempts after retry = %d, want 5", callCount)
	}
}

func TestPollFeishuHITLInboxCursorStaysBlockedAfterRepeatedFailures(t *testing.T) {
	var calls int
	deps := feishuHITLPollDeps{
		loopBySeq: func(_ contextType, seq int64) string { return "loop-seq" },
		deliverAnswer: func(_ contextType, _, _ string) error {
			calls++
			return fmt.Errorf("still failing")
		},
	}
	events := []feishuInboxEvent{makeCardAction(10, "1", "ok")}

	for attempt := 1; attempt <= 2; attempt++ {
		n, newCursor := pollFeishuHITLInboxOnce(context.Background(), events, deps, 9)
		if n != 0 || newCursor != 9 {
			t.Fatalf("attempt %d = (%d, %d), want (0, 9)", attempt, n, newCursor)
		}
	}
	if calls != 2 {
		t.Fatalf("delivery attempts = %d, want 2", calls)
	}
}

func TestPollFeishuHITLInboxCursorAdvancesPastSkippedEvents(t *testing.T) {
	events := []feishuInboxEvent{
		{ID: 10, Kind: "message", RootID: "om_root", Text: "   "},
		makeCardAction(11, "not-a-sequence", "yes"),
		{ID: 12, Kind: "unsupported"},
	}

	n, newCursor := pollFeishuHITLInboxOnce(context.Background(), events, feishuHITLPollDeps{}, 9)
	if n != 0 {
		t.Fatalf("handled = %d, want 0", n)
	}
	if newCursor != 12 {
		t.Fatalf("newCursor = %d, want 12 after skipped events", newCursor)
	}
}

func TestPollFeishuHITLInboxRollsBackMessageWhenRequeueFails(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.July, 30, 14, 0, 0, 0, time.UTC)
	nowISO := now.Format("2006-01-02T15:04:05.000Z")
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{}); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())

	const projectID = "project-feishu-replay"
	const loopID = "loop-feishu-replay"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Feishu replay", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "waiting", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	loopIDPtr, projectIDPtr := loopID, projectID
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue-feishu-replay", LoopID: &loopIDPtr, ProjectID: &projectIDPtr, Type: "worker", TargetType: "project", TargetID: projectID, DedupeKey: "worker:feishu-replay", Priority: storage.QueuePriorityWorker, Status: "cancelled", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	if _, err := coordinator.DB().ExecContext(ctx, `
		CREATE TRIGGER fail_feishu_requeue
		BEFORE UPDATE ON queue_items
		BEGIN
			SELECT RAISE(ABORT, 'forced requeue failure');
		END`); err != nil {
		t.Fatalf("create requeue failure trigger: %v", err)
	}

	n, newCursor := pollFeishuHITLInboxOnce(ctx, []feishuInboxEvent{{ID: 10, Kind: "message", RootID: "root-1", Text: "continue"}}, feishuHITLPollDeps{
		loopByRoot: func(contextType, string) string { return loopID },
		enqueueMessage: func(ctx contextType, id, text string) error {
			return enqueueHumanMessageToLoop(ctx, repos, nowISO, id, text)
		},
	}, 9)
	if n != 0 || newCursor != 9 {
		t.Fatalf("poll result = (%d, %d), want failed delivery to leave cursor at 9", n, newCursor)
	}
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "waiting" {
		t.Fatalf("loop status = %q, want waiting after rollback", loop.Status)
	}
	if messages := loops.ReadHumanInbox(loop.MetadataJSON); len(messages) != 0 {
		t.Fatalf("human inbox = %#v, want no partial message", messages)
	}
}

func makeCardAction(id int64, seq, answer string) feishuInboxEvent {
	return feishuInboxEvent{
		ID:   id,
		Kind: "card_action",
		Value: struct {
			LoopSeq string `json:"loopSeq"`
			Answer  string `json:"answer"`
		}{LoopSeq: seq, Answer: answer},
	}
}
