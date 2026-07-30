package reviewer

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/storage"
)

// A reviewer loop under human takeover is one a human owns the worktree for. The
// dangerous shape is not "discovery enqueues too much" but the ordering: the
// queue item is created before the loop status write, so a held loop that got as
// far as the enqueue left an active queue item behind that no lane may claim,
// and then failed the whole discovery pass on the refused status write. These
// exercise the skip that now happens before any of that.

// TestDiscoverPullRequestsSkipsHumanHeldReviewerLoop covers the loop-level
// invariant: an eligible new head on a held PR is a skip, not work and not an
// error. followUpdates is set so the PR is otherwise fully eligible — without it
// the pass would skip for an unrelated reason and prove nothing.
func TestDiscoverPullRequestsSkipsHumanHeldReviewerLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	github := &fakeGitHubGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true}`
	held := storage.LoopRecord{ID: "loop_reviewer_held", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: string(domain.LoopStatusHumanTakeover), MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(ctx, held); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	result, err := runner.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v, want the hold reported as a skip", err)
	}
	if len(result.QueueItems) != 0 || len(result.CreatedLoopIDs) != 0 {
		t.Fatalf("result = %#v, want no queue items and no created loops for a held PR", result)
	}
	if result.Skipped == 0 {
		t.Fatalf("result.Skipped = %d, want the held PR counted as skipped", result.Skipped)
	}

	// The queue is what actually matters: an active item for a held loop is
	// dormant work no lane can claim.
	active, err := fixture.repos.Queue.FindActiveByLoopID(ctx, held.ID)
	if err != nil {
		t.Fatalf("Queue.FindActiveByLoopID() error = %v", err)
	}
	if active != nil {
		t.Fatalf("Queue.FindActiveByLoopID() = %#v, want nothing enqueued against a human-held loop", active)
	}

	persisted, err := fixture.repos.Loops.GetByID(ctx, held.ID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if persisted == nil || persisted.Status != string(domain.LoopStatusHumanTakeover) {
		t.Fatalf("loop = %#v, want the hold intact after discovery", persisted)
	}
}

// TestDiscoverPullRequestsHeldLoopDoesNotAbortPass is the blast-radius half: the
// refused status write used to surface as a discovery error, so one held PR cost
// every other PR in the same pass its review.
func TestDiscoverPullRequestsHeldLoopDoesNotAbortPass(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	github := &fakeGitHubGateway{listOpenByLabel: map[string][]PullRequestSummary{"": {
		{Number: 42, Title: "Held by a human", State: "OPEN", HeadSHA: "abc123", BaseSHA: "base123"},
		{Number: 43, Title: "Ordinary review", State: "OPEN", HeadSHA: "def456", BaseSHA: "base123"},
	}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true}`
	held := storage.LoopRecord{ID: "loop_reviewer_held_batch", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: string(domain.LoopStatusHumanTakeover), MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(ctx, held); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	result, err := runner.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v, want the pass to survive one held PR", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("len(QueueItems) = %d, want PR 43 still reviewed alongside the held PR 42", len(result.QueueItems))
	}
	if queued := result.QueueItems[0]; queued.LoopID == nil || *queued.LoopID == held.ID {
		t.Fatalf("queue item = %#v, want it to belong to the unheld PR", queued)
	}
	if len(result.CreatedLoopIDs) != 1 {
		t.Fatalf("len(CreatedLoopIDs) = %d, want a loop created for PR 43", len(result.CreatedLoopIDs))
	}
	persisted, err := fixture.repos.Loops.GetByID(ctx, held.ID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if persisted == nil || persisted.Status != string(domain.LoopStatusHumanTakeover) {
		t.Fatalf("loop = %#v, want the hold intact after discovery", persisted)
	}
}

// TestMarkLoopQueuedForReviewLeavesHumanHeldLoopAlone covers updateLoop, which
// every reviewer status write funnels through. A takeover that commits mid-turn
// must make those writes no-ops rather than failing the turn: the mutations all
// move status or scheduling, and neither is the daemon's to decide while a human
// owns the worktree.
func TestMarkLoopQueuedForReviewLeavesHumanHeldLoopAlone(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"followUpdates":true}`
	held := storage.LoopRecord{ID: "loop_reviewer_held_update", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: string(domain.LoopStatusHumanTakeover), MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(ctx, held); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	// The stale record a turn in flight carries: it still believes the loop is
	// schedulable.
	stale := held
	stale.Status = "queued"
	if err := runner.markLoopQueuedForReview(ctx, stale, nowISO); err != nil {
		t.Fatalf("markLoopQueuedForReview() error = %v, want the held loop left alone without an error", err)
	}

	persisted, err := fixture.repos.Loops.GetByID(ctx, held.ID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if persisted == nil || persisted.Status != string(domain.LoopStatusHumanTakeover) {
		t.Fatalf("loop = %#v, want human_takeover preserved", persisted)
	}
	if persisted.NextRunAt != nil {
		t.Fatalf("loop.NextRunAt = %v, want no scheduling for a loop a human owns", *persisted.NextRunAt)
	}

	// updateLoop reports what is persisted, not the caller's stale snapshot, so
	// callers that keep using the returned record do not act on a released hold.
	returned, err := runner.updateLoop(ctx, stale, func(updated *storage.LoopRecord) {
		updated.Status = "queued"
	})
	if err != nil {
		t.Fatalf("updateLoop() error = %v, want the persisted record returned", err)
	}
	if returned.Status != string(domain.LoopStatusHumanTakeover) {
		t.Fatalf("updateLoop() returned %#v, want the persisted held record", returned)
	}
}
