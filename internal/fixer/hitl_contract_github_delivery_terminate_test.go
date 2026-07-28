package fixer

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// Concurrent terminate during park must not return awaiting_human or notify.
func TestHITLContract_TerminatedLoopAbortsSuspend(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_hitl_terminated", Seq: 203, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	loopID := loop.ID
	projectID := loop.ProjectID
	queueItem := storage.QueueItemRecord{
		ID: "queue_hitl_terminated", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		DedupeKey: "fixer:hitl-terminated", Priority: storage.QueuePriorityFixer, AvailableAt: nowISO,
		MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Queue.Upsert(ctx, queueItem)
	run := storage.RunRecord{
		ID: "run_hitl_terminated", LoopID: loop.ID, Status: "running",
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Runs.Upsert(ctx, run)
	project, _ := fixture.repos.Projects.GetByID(ctx, "project_1")

	// Terminate the loop before suspend's park transaction reads it.
	// parkHITLLoop loads current status inside the transaction.
	terminated := loop
	terminated.Status = "terminated"
	terminated.UpdatedAt = nowISO
	_ = fixture.repos.Loops.Upsert(ctx, terminated)

	var notified int
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLAnswerTransport: "feishu",
		HITLNotify: func(context.Context, HITLAskNotification) error {
			notified++
			return nil
		},
	})
	result, err := runner.suspendForHuman(ctx, stepInput{
		Project: *project, Loop: loop, Run: run, QueueItem: queueItem,
		Repo: repo, PRNumber: pr,
	}, run, fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: pr87Head}}, &awaitingHumanError{
		question: "already stopped?", options: []string{"a"},
		sessionID: "sess-term", executionID: "agent-term", vendor: "codex",
	})
	if err != nil {
		t.Fatalf("suspendForHuman: %v", err)
	}
	if result.Status != "terminated" {
		t.Fatalf("result.Status = %q, want terminated", result.Status)
	}
	if notified != 0 {
		t.Fatalf("notify calls = %d, want 0 when loop terminated", notified)
	}
	got, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID: %v", err)
	}
	if got.Status != "terminated" {
		t.Fatalf("loop status = %q, want still terminated", got.Status)
	}
	if _, ok := loops.ReadHITLAsk(got.MetadataJSON); ok {
		t.Fatal("must not persist HITL ask on terminated loop")
	}
	finishedRun, err := fixture.repos.Runs.GetByID(ctx, run.ID)
	if err != nil || finishedRun == nil {
		t.Fatalf("Runs.GetByID: %v", err)
	}
	if finishedRun.Status != "interrupted" {
		t.Fatalf("run status = %q, want interrupted", finishedRun.Status)
	}
}
