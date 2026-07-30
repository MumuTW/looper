package reviewer

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

// Takeover racing an already-bound claim. The queue item was claimed just before
// takeover; the run-start loop write then executes after Hold commits but before
// the operation lease is cancelled.
//
// updateLoop deliberately swallows that refusal and returns the persisted row —
// correct mid-turn, where failing an in-flight turn over a hold helps nobody.
// At a run boundary it is not: the caller would read back a row saying
// human_takeover and walk into the workflow anyway, which is a window for
// worktree preparation and agent launch after the human was told the checkout is
// theirs. beginLoopRun is the boundary that turns the swallowed refusal back
// into an abort.
func TestProcessClaimedItemAbortsWhenTakeoverWinsTheRunStartRace(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()
	projectID := "project_1"
	repo := "acme/looper"
	prNumber := int64(42)
	loopID := "loop_takeover_run_start"
	target := "pr:acme/looper:42"

	// The row the run-start write reads back: takeover has already committed.
	if err := fixture.repos.Loops.UpsertChangingHumanHold(ctx, storage.LoopRecord{
		ID: loopID, Seq: 42, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &prNumber,
		Status: "human_takeover", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.UpsertChangingHumanHold() error = %v", err)
	}
	// The claim, bound before the hold landed, so the processor still holds it.
	queue := storage.QueueItemRecord{
		ID: "queue_takeover_run_start", ProjectID: &projectID, LoopID: &loopID, Type: "reviewer",
		TargetType: "pull_request", TargetID: target, Repo: &repo, PRNumber: &prNumber,
		DedupeKey: "reviewer:project_1:acme/looper:42", Priority: storage.QueuePriorityReviewer, Status: "running",
		AvailableAt: nowISO, StartedAt: stringPtr(nowISO), Attempts: 1, MaxAttempts: 3,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Queue.Upsert(ctx, queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	git := &fakeGitGateway{}
	github := &fakeGitHubGateway{currentLogin: "reviewer", author: "author", reviewRequests: []string{"reviewer"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.ProcessClaimedItem(ctx, queue)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v, want the hold reported as a skip, not a failure", err)
	}
	if result.Status != "skipped" {
		t.Fatalf("ProcessClaimedItem() status = %q, want skipped", result.Status)
	}
	// The summary is load-bearing, not decoration. Without the boundary abort the
	// processor wanders on and eventually skips for some *other* reason, which is
	// the same Status with different side effects behind it; naming the run-start
	// hold is what distinguishes "never entered the workflow" from "entered it and
	// found nothing to do".
	if result.Summary != reviewerRunStartHeldSummary {
		t.Fatalf("ProcessClaimedItem() summary = %q, want the run-start hold %q", result.Summary, reviewerRunStartHeldSummary)
	}
	// The workflow was never entered: no step ever started on this run.
	events, err := fixture.repos.Events.ListByEntity(ctx, "run", result.RunID)
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	for _, event := range events {
		if event.EventType == "loop.step.started" {
			t.Fatalf("run %s started step events %#v; the hold must abort before the first step", result.RunID, event)
		}
	}

	// The worktree invariant: nothing touched the checkout the human now owns.
	if len(git.createCalls) != 0 || len(git.prepareCalls) != 0 || len(git.cleanupCalls) != 0 {
		t.Fatalf("git calls = create %#v, prepare %#v, cleanup %#v; want none after the hold was granted", git.createCalls, git.prepareCalls, git.cleanupCalls)
	}

	// The lifecycle invariant: the loop is still the human's, and the claim is
	// released rather than left running.
	loop, err := fixture.repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", loop, err)
	}
	if loop.Status != "human_takeover" {
		t.Fatalf("loop status = %q, want human_takeover preserved", loop.Status)
	}
	persistedQueue, err := fixture.repos.Queue.GetByID(ctx, queue.ID)
	if err != nil || persistedQueue == nil {
		t.Fatalf("Queue.GetByID() = (%#v, %v)", persistedQueue, err)
	}
	if persistedQueue.Status == "running" {
		t.Fatalf("queue status = %q, want the claim released", persistedQueue.Status)
	}
}
