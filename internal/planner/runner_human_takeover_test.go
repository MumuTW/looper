package planner

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

// newHeldPlannerLoopFixture seeds a planner loop for acme/looper#42 that a human
// took over, and a discovery gateway that will rediscover exactly that issue.
func newHeldPlannerLoopFixture(t *testing.T) (*runnerFixture, *Runner) {
	t.Helper()
	base := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := base.nowISO()
	repo := "acme/looper"
	targetID := buildIssueTargetID(repo, 42)
	metadata := `{"issueNumber":42,"issueTitle":"Plan this"}`
	if err := base.repos.Loops.UpsertChangingHumanHold(ctx, storage.LoopRecord{
		ID: "loop_held", Seq: 42, ProjectID: "project_1", Type: "planner",
		TargetType: "issue", TargetID: &targetID, Repo: &repo,
		Status: "human_takeover", MetadataJSON: &metadata,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	github := &fakeGitHubGateway{
		issues:      []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}},
		issueDetail: IssueDetail{Number: 42, Title: "Plan this", Body: "details", URL: "https://example/issues/42", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}},
	}
	runner := New(Options{DB: base.coordinator.DB(), Repos: base.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: base.logger, Now: base.now})
	return base, runner
}

// TestPlannerRediscoveryDoesNotMaterializeHeldLoop: preserving the status was
// only half the job. materializeIssue enumerated paused/completed/awaiting_human/
// failed and not human_takeover, so rediscovery still created an active planner
// queue item and woke the scheduler for a loop a human owns. The claim boundary
// kept it dormant, but discovery reported queued work and left active state
// behind until handback cancelled it.
func TestPlannerRediscoveryDoesNotMaterializeHeldLoop(t *testing.T) {
	t.Parallel()
	f, runner := newHeldPlannerLoopFixture(t)
	ctx := context.Background()

	result, err := runner.DiscoverIssues(ctx, DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 0 {
		t.Fatalf("DiscoverIssues().QueueItems = %#v, want none for a human-held loop", result.QueueItems)
	}
	if len(result.CreatedLoopIDs) != 0 {
		t.Fatalf("DiscoverIssues().CreatedLoopIDs = %#v, want none", result.CreatedLoopIDs)
	}
	if result.Skipped != 1 {
		t.Fatalf("DiscoverIssues().Skipped = %d, want 1 (the held loop reported as skipped, as the fixer path does)", result.Skipped)
	}

	items, err := f.repos.Queue.List(ctx)
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("queue items = %#v, want no active planner item for a held loop", items)
	}

	loop, err := f.repos.Loops.GetByID(ctx, "loop_held")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "human_takeover" {
		t.Fatalf("status = %q, want human_takeover preserved", loop.Status)
	}
	if loop.NextRunAt != nil {
		t.Fatalf("NextRunAt = %v, want nil (a held loop must not be re-armed)", *loop.NextRunAt)
	}
}

// TestPlannerRediscoveryDoesNotOverwriteConcurrentHandback is the inverse
// ordering of the same race. The rediscovery pass listed the loops while the
// hold was still in place; by the time it writes, handback has released it. A
// read-modify-Upsert that "preserves" the status it read would restore the hold
// over the human's release and strand the loop.
func TestPlannerRediscoveryDoesNotOverwriteConcurrentHandback(t *testing.T) {
	t.Parallel()
	f, runner := newHeldPlannerLoopFixture(t)
	ctx := context.Background()

	// The snapshot a discovery pass took while the loop was still held.
	stale, err := f.repos.Loops.GetByID(ctx, "loop_held")
	if err != nil || stale == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", stale, err)
	}

	// Handback commits.
	released := *stale
	released.Status = "queued"
	released.UpdatedAt = "2026-04-11T12:05:00.000Z"
	if err := f.repos.Loops.UpsertChangingHumanHold(ctx, released); err != nil {
		t.Fatalf("UpsertChangingHumanHold() error = %v", err)
	}

	// The pass now refreshes the loop from its stale snapshot.
	if _, err := runner.refreshIssueLoop(ctx, *stale, "acme/looper", IssueSummary{Number: 42, Title: "Plan this"}, f.nowISO(), "", ""); err != nil {
		t.Fatalf("refreshIssueLoop() error = %v", err)
	}

	loop, err := f.repos.Loops.GetByID(ctx, "loop_held")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "queued" {
		t.Fatalf("status = %q, want queued: a stale discovery snapshot must not restore a released hold", loop.Status)
	}
}
