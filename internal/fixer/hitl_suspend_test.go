package fixer

import (
	"context"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestSuspendForHumanTransitionsAndNotifies(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	repo := "acme/looper"
	pr := int64(42)
	loop := storage.LoopRecord{
		ID: "loop_fixer_hitl", Seq: 7, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert error = %v", err)
	}
	loopID := loop.ID
	projectID := loop.ProjectID
	queueItem := storage.QueueItemRecord{
		ID: "queue_fixer_hitl", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		DedupeKey: "fixer:hitl", Priority: storage.QueuePriorityFixer, AvailableAt: nowISO,
		MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Queue.Upsert(ctx, queueItem); err != nil {
		t.Fatalf("Queue.Upsert error = %v", err)
	}
	run := storage.RunRecord{
		ID: "run_fixer_hitl", LoopID: loop.ID, Status: "running",
		CurrentStep: stringPtr(string(stepRepair)), StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert error = %v", err)
	}
	project, err := fixture.repos.Projects.GetByID(ctx, "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID error = %v", err)
	}

	var sent []HITLAskNotification
	runner := New(Options{
		DB:                  fixture.coordinator.DB(),
		Repos:               fixture.repos,
		Logger:              fixture.logger,
		Now:                 fixture.now,
		HITLEnabled:         true,
		HITLAnswerTransport: "feishu",
		HITLNotify: func(_ context.Context, n HITLAskNotification) error {
			sent = append(sent, n)
			return nil
		},
	})

	awaiting := &awaitingHumanError{
		question:        "Keep dark mode or follow reviewer light-only request?",
		options:         []string{"keep dark mode", "follow reviewer"},
		sessionID:       "sess-fixer-1",
		executionID:     "agent-fixer-1",
		vendor:          "codex",
		headSHA:         "head-abc",
		reviewThreadID:  "thread-1",
		reviewContentFP: "rev-fp",
		prIntentFP:      "intent-fp",
	}
	// Checkpoint still has incomplete repair (nil) — suspend must keep it that way.
	checkpoint := fixerCheckpoint{
		Detail:   &checkpointDetail{HeadSHA: "head-abc"},
		FixItems: []FixItem{{Type: "comment", ID: "c1", ThreadID: "thread-1", Body: "remove dark mode"}},
		// Deliberately set a repair that suspend must clear.
		Repair: &checkpointRepair{Summary: "should be cleared"},
	}
	result, err := runner.suspendForHuman(ctx, stepInput{
		Project: *project, Loop: loop, Run: run, QueueItem: queueItem,
		Repo: repo, PRNumber: pr, Checkpoint: checkpoint,
	}, run, checkpoint, awaiting)
	if err != nil {
		t.Fatalf("suspendForHuman error = %v", err)
	}
	if result.Status != "awaiting_human" {
		t.Fatalf("result.Status = %q, want awaiting_human", result.Status)
	}

	got, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID error = %v", err)
	}
	if got.Status != "awaiting_human" {
		t.Fatalf("loop status = %q, want awaiting_human", got.Status)
	}
	ask, ok := loops.ReadHITLAsk(got.MetadataJSON)
	if !ok || ask.Question != awaiting.question || ask.SessionID != "sess-fixer-1" || ask.Status != "awaiting" {
		t.Fatalf("persisted ask = %#v (ok=%v)", ask, ok)
	}
	if ask.Role != "fixer" || ask.HeadSHA != "head-abc" || ask.ReviewThreadID != "thread-1" {
		t.Fatalf("fingerprint/role fields = %#v", ask)
	}

	q, err := fixture.repos.Queue.GetByID(ctx, queueItem.ID)
	if err != nil || q == nil {
		t.Fatalf("Queue.GetByID error = %v", err)
	}
	if q.Status != "cancelled" {
		t.Fatalf("queue status = %q, want cancelled", q.Status)
	}

	finishedRun, err := fixture.repos.Runs.GetByID(ctx, run.ID)
	if err != nil || finishedRun == nil {
		t.Fatalf("Runs.GetByID error = %v", err)
	}
	if finishedRun.Status != "interrupted" {
		t.Fatalf("run status = %q, want interrupted", finishedRun.Status)
	}
	// Repair must not be marked complete on the interrupted checkpoint.
	cp := parseCheckpoint(finishedRun.CheckpointJSON)
	if cp.Repair != nil {
		t.Fatalf("checkpoint.Repair = %#v, want nil so resume re-enters repair", cp.Repair)
	}

	if len(sent) != 1 {
		t.Fatalf("HITLNotify calls = %d, want 1", len(sent))
	}
	if sent[0].LoopSeq != 7 || !strings.Contains(sent[0].Title, "Fixer") || sent[0].Question != awaiting.question {
		t.Fatalf("notification = %#v", sent[0])
	}
}

func TestPendingHumanAnswerRejectsStaleHeadFingerprint(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	repo := "acme/looper"
	pr := int64(42)
	loop := storage.LoopRecord{
		ID: "loop_fixer_stale", Seq: 8, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "awaiting_human",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	meta, err := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question:  "Keep dark mode?",
		Answer:    "keep dark mode",
		Status:    "answered",
		SessionID: "sess-stale",
		Vendor:    "codex",
		HeadSHA:   "old-head",
		Role:      "fixer",
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk error = %v", err)
	}
	loop.MetadataJSON = &meta
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert error = %v", err)
	}

	runner := New(Options{Repos: fixture.repos, AgentRuntime: "codex", HITLEnabled: true})

	// Matching head → inject decision.
	prompt, session := runner.pendingHumanAnswer(ctx, &loop, "codex", "old-head", "", "")
	if !strings.Contains(prompt, "keep dark mode") || session != "sess-stale" {
		t.Fatalf("fresh head resume = (%q, %q)", prompt, session)
	}

	// Mismatched head → do not inject as executable decision.
	prompt, session = runner.pendingHumanAnswer(ctx, &loop, "codex", "new-head", "", "")
	if prompt != "" || session != "" {
		t.Fatalf("stale head must not inject decision; got (%q, %q)", prompt, session)
	}
}
