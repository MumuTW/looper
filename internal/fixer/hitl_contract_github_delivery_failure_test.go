package fixer

import (
	"context"
	"errors"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// GitHub ask delivery must succeed after durable park; transient CreateIssueComment
// failure rolls the park back and requeues the cancelled claim so the scheduler
// can retry instead of leaving running + no claimable work.
func TestHITLContract_GitHubDeliveryFailureRollsBackPark(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_gh_delivery_fail", Seq: 201, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	loopID := loop.ID
	projectID := loop.ProjectID
	queueItem := storage.QueueItemRecord{
		ID: "queue_gh_delivery_fail", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		DedupeKey: "fixer:gh-delivery-fail", Priority: storage.QueuePriorityFixer, AvailableAt: nowISO,
		MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Queue.Upsert(ctx, queueItem)
	run := storage.RunRecord{
		ID: "run_gh_delivery_fail", LoopID: loop.ID, Status: "running",
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Runs.Upsert(ctx, run)
	project, _ := fixture.repos.Projects.GetByID(ctx, "project_1")

	github := &fakeGitHubGateway{createIssueCommentErr: errors.New("GitHub API 502")}
	var notified int
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLAnswerTransport: "github",
		HITLNotify: func(context.Context, HITLAskNotification) error {
			notified++
			return nil
		},
	})
	_, err := runner.suspendForHuman(ctx, stepInput{
		Project: *project, Loop: loop, Run: run, QueueItem: queueItem,
		Repo: repo, PRNumber: pr,
	}, run, fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: pr87Head}}, &awaitingHumanError{
		question: "delivery fail?", options: []string{"a", "b"},
		sessionID: "sess-del", executionID: "agent-del", vendor: "codex",
	})
	if err == nil {
		t.Fatal("suspendForHuman error = nil, want GitHub delivery failure")
	}
	var loopErr *loopError
	if !errors.As(err, &loopErr) || loopErr.kind != FailureRetryableTransient {
		t.Fatalf("err = %v, want retryable loopError", err)
	}
	if notified != 0 {
		t.Fatalf("notify calls = %d, want 0 on delivery failure", notified)
	}
	got, gerr := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if gerr != nil || got == nil {
		t.Fatalf("Loops.GetByID: %v", gerr)
	}
	if got.Status != "running" {
		t.Fatalf("loop status = %q, want running after delivery-failure rollback", got.Status)
	}
	if _, ok := loops.ReadHITLAsk(got.MetadataJSON); ok {
		t.Fatal("HITL ask must be cleared after delivery-failure rollback")
	}
	if len(github.createIssueComments) != 1 {
		t.Fatalf("createIssueComments = %d, want 1 attempt", len(github.createIssueComments))
	}
	// Rollback must requeue the cancelled claim without relying on recoverClaimedItem.
	q, qerr := fixture.repos.Queue.GetByID(ctx, queueItem.ID)
	if qerr != nil || q == nil {
		t.Fatalf("Queue.GetByID: %v", qerr)
	}
	if q.Status != "queued" {
		t.Fatalf("queue status = %q, want queued after delivery-failure rollback requeue", q.Status)
	}
}

// Full scheduler path: ProcessClaimedQueueItem after a mid-suspend delivery
// failure must leave claimable work (rollback requeue + recover MarkRetry).
func TestHITLContract_GitHubDeliveryFailureSchedulerLifecycle(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_gh_delivery_sched", Seq: 211, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	loopID := loop.ID
	projectID := loop.ProjectID
	// Simulate park already cancelled the claim (as suspend does before delivery).
	queueItem := storage.QueueItemRecord{
		ID: "queue_gh_delivery_sched", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "cancelled",
		DedupeKey: "fixer:gh-delivery-sched", Priority: storage.QueuePriorityFixer, AvailableAt: nowISO,
		MaxAttempts: 3, Attempts: 0, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Queue.Upsert(ctx, queueItem)

	// Seed awaiting_human + ask as post-park pre-delivery state, then roll back
	// through the production helper used on CreateIssueComment failure.
	meta, err := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: "sched?", Status: "awaiting", AskedAt: nowISO,
		Transport: "github", Provider: "github", PRNumber: pr, Role: "fixer",
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk: %v", err)
	}
	parked := loop
	parked.Status = "awaiting_human"
	parked.MetadataJSON = &meta
	_ = fixture.repos.Loops.Upsert(ctx, parked)

	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLAnswerTransport: "github",
	})
	if err := runner.rollbackHITLParkForDeliveryRetry(ctx, parked, nowISO); err != nil {
		t.Fatalf("rollbackHITLParkForDeliveryRetry: %v", err)
	}
	got, gerr := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if gerr != nil || got == nil {
		t.Fatalf("Loops.GetByID: %v", gerr)
	}
	if got.Status != "running" {
		t.Fatalf("loop status = %q, want running", got.Status)
	}
	if _, ok := loops.ReadHITLAsk(got.MetadataJSON); ok {
		t.Fatal("HITL ask must be cleared by rollback")
	}
	q, qerr := fixture.repos.Queue.GetByID(ctx, queueItem.ID)
	if qerr != nil || q == nil {
		t.Fatalf("Queue.GetByID: %v", qerr)
	}
	if q.Status != "queued" {
		t.Fatalf("queue status = %q, want queued so scheduler can claim retry", q.Status)
	}
	// Claimable again.
	claim, cerr := fixture.repos.Queue.ClaimNextOfType(ctx, nowISO, "fixer-retry-worker", "fixer")
	if cerr != nil || claim == nil {
		t.Fatalf("ClaimNextOfType = (%#v, %v), want requeued item", claim, cerr)
	}
	if claim.ID != queueItem.ID {
		t.Fatalf("claimed id = %q, want %q", claim.ID, queueItem.ID)
	}
}
