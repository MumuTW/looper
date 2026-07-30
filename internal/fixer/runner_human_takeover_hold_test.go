package fixer

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreecleanup"
)

// takeoverHoldFixture builds the exact state #162 was reproduced from: a fixer
// loop for an open PR that a human took over *while its run was still active*,
// with the daemon's queue item for that loop still sitting queued (the claim was
// already in flight when takeover landed) and the loop's worktree still on disk.
type takeoverHoldFixture struct {
	*runnerFixture
	repo      string
	prNumber  int64
	loopID    string
	queueID   string
	worktree  storage.WorktreeRecord
	github    *fakeGitHubGateway
	git       *fakeGitGateway
	runner    *Runner
	nowString string
}

// withQueuedItem controls whether the daemon's queue item for the loop already
// exists. Both halves of the incident are covered: a queue item that was already
// materialised when takeover landed (claim leg) and a fresh rediscovery tick with
// nothing queued yet (discovery leg). An existing active PR item makes the PR
// ineligible for a repo scan, so the two cannot share one fixture.
func newTakeoverHoldFixture(t *testing.T, withQueuedItem bool, runStatus string) *takeoverHoldFixture {
	t.Helper()
	base := newRunnerFixture(t)
	ctx := context.Background()
	repo := "acme/looper"
	prNumber := int64(41)
	nowISO := base.nowISO()
	worktreePath := filepath.Join(t.TempDir(), "looper-fix-project_1-pr-41")
	branch := "fix/pr-41"

	loopMetadata, err := json.Marshal(map[string]any{"worktreePath": worktreePath, "branch": branch})
	if err != nil {
		t.Fatalf("marshal loop metadata: %v", err)
	}
	loopMetadataJSON := string(loopMetadata)
	loop := storage.LoopRecord{
		ID: "loop_takeover", Seq: 41, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber,
		Status: "human_takeover", MetadataJSON: &loopMetadataJSON,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := base.repos.Loops.UpsertChangingHumanHold(ctx, loop); err != nil {
		t.Fatalf("Loops.UpsertChangingHumanHold() error = %v", err)
	}

	// The takeover happened while this run was active. runStatus is how far the
	// run row has settled since: "running" is the instant of takeover (the agent
	// is killed but the row survives), "interrupted" is after the killed run is
	// reconciled, which is when a later rediscovery tick sees no active run and
	// would otherwise revive the loop.
	if err := base.repos.Runs.Upsert(ctx, storage.RunRecord{
		ID: "run_takeover", LoopID: loop.ID, Status: runStatus,
		CurrentStep: stringPtr(string(stepRepair)), StartedAt: nowISO,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	queuePayload, err := json.Marshal(map[string]any{"worktreePath": worktreePath, "branch": branch, "projectId": "project_1"})
	if err != nil {
		t.Fatalf("marshal queue payload: %v", err)
	}
	queuePayloadJSON := string(queuePayload)
	queueID := "queue_takeover"
	projectID := "project_1"
	if withQueuedItem {
		if err := base.repos.Queue.Upsert(ctx, storage.QueueItemRecord{
			ID: queueID, ProjectID: &projectID, LoopID: &loop.ID, Type: "fixer",
			TargetType: "pull_request", TargetID: buildPullRequestTargetID(repo, prNumber),
			Repo: &repo, PRNumber: &prNumber, DedupeKey: "fixer:" + repo + ":41",
			Status: "queued", Priority: 5, AvailableAt: nowISO, MaxAttempts: 5,
			PayloadJSON: &queuePayloadJSON, CreatedAt: nowISO, UpdatedAt: nowISO,
		}); err != nil {
			t.Fatalf("Queue.Upsert() error = %v", err)
		}
	}

	worktree := storage.WorktreeRecord{
		ID: "wt_takeover", ProjectID: "project_1", RepoPath: filepath.Join(t.TempDir(), "repo"),
		WorktreePath: worktreePath, Branch: branch, Status: "active",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := base.repos.Worktrees.Upsert(ctx, worktree); err != nil {
		t.Fatalf("Worktrees.Upsert() error = %v", err)
	}

	github := &fakeGitHubGateway{
		// The fake stamps discovered PRs as authored by "looper"; the default
		// author filter is current_user, so discovery only reaches the loop when
		// the current login matches.
		currentUser: "looper",
		listOpen:    []PullRequestSummary{{Number: prNumber, State: "OPEN", HeadSHA: "head-41"}},
		viewResponses: []PullRequestDetail{{
			Number: prNumber, State: "OPEN", HeadSHA: "head-41",
			Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}},
		}},
	}
	git := &fakeGitGateway{}
	runner := New(Options{DB: base.coordinator.DB(), Repos: base.repos, GitHub: github, Git: git, AgentExecutor: &fakeAgentExecutor{}, Logger: base.logger, Now: base.now})

	return &takeoverHoldFixture{
		runnerFixture: base, repo: repo, prNumber: prNumber, loopID: loop.ID,
		queueID: queueID, worktree: worktree, github: github, git: git,
		runner: runner, nowString: nowISO,
	}
}

// TestHumanTakeoverSurvivesDiscoveryRediscovery is the #162 discovery leg: a
// rediscovery tick for the PR must not rewrite the taken-over loop's status back
// to queued, and must not enqueue work for it. Rewriting the status is what left
// loop 41 at `queued` with handback no longer the exit it was promised to be.
func TestHumanTakeoverSurvivesDiscoveryRediscovery(t *testing.T) {
	t.Parallel()
	f := newTakeoverHoldFixture(t, false, "interrupted")
	ctx := context.Background()

	result, err := f.runner.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: f.repo})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.CreatedLoopIDs) != 0 || len(result.QueueItems) != 0 {
		t.Fatalf("discovery result = %#v, want no created loops or queue items", result)
	}

	loop, err := f.repos.Loops.GetByID(ctx, f.loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "human_takeover" {
		t.Fatalf("loop = %#v, want status human_takeover after discovery", loop)
	}
	if loop.NextRunAt != nil {
		t.Fatalf("loop.NextRunAt = %v, want nil (discovery must not re-arm a taken-over loop)", *loop.NextRunAt)
	}

	items, err := f.repos.Queue.List(ctx)
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("queue items = %#v, want discovery to enqueue nothing for a taken-over loop", items)
	}
	assertNoGitMutations(t, f.git)
}

// TestHumanTakeoverHoldsAgainstClaim is the #162 claim leg: the loop was taken
// over while its run was active and a queue item was already materialised. No
// role lane may claim it, and nothing may touch the human's worktree.
func TestHumanTakeoverHoldsAgainstClaim(t *testing.T) {
	t.Parallel()
	f := newTakeoverHoldFixture(t, true, "running")
	ctx := context.Background()

	// Claim cycle: the queue item that was already in flight must stay unclaimed
	// on every lane entry point the scheduler and the fixer runner use.
	claimedBy := "scheduler_test"
	if item, err := f.repos.Queue.ClaimNext(ctx, f.nowString, claimedBy); err != nil {
		t.Fatalf("Queue.ClaimNext() error = %v", err)
	} else if item != nil {
		t.Fatalf("Queue.ClaimNext() = %#v, want no claim while loop is human_takeover", item)
	}
	if item, err := f.repos.Queue.ClaimNextOfType(ctx, f.nowString, claimedBy, "fixer"); err != nil {
		t.Fatalf("Queue.ClaimNextOfType() error = %v", err)
	} else if item != nil {
		t.Fatalf("Queue.ClaimNextOfType() = %#v, want no claim while loop is human_takeover", item)
	}
	if item, err := f.repos.Queue.ClaimNextNonLongTermRetryAmongTypeSets(ctx, f.nowString, claimedBy, []string{"fixer", "reviewer", "worker", "planner"}, nil); err != nil {
		t.Fatalf("Queue.ClaimNextNonLongTermRetryAmongTypeSets() error = %v", err)
	} else if item != nil {
		t.Fatalf("ClaimNextNonLongTermRetryAmongTypeSets() = %#v, want no claim while loop is human_takeover", item)
	}
	if item, err := f.repos.Queue.ClaimNextLongTermRetryAmongTypeSets(ctx, f.nowString, claimedBy, []string{"fixer", "reviewer", "worker", "planner"}, nil); err != nil {
		t.Fatalf("Queue.ClaimNextLongTermRetryAmongTypeSets() error = %v", err)
	} else if item != nil {
		t.Fatalf("ClaimNextLongTermRetryAmongTypeSets() = %#v, want no claim while loop is human_takeover", item)
	}

	// The fixer lane's own entry point is the same boundary; it must find nothing.
	if processed, err := f.runner.ProcessNext(ctx, claimedBy); err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	} else if processed != nil {
		t.Fatalf("ProcessNext() = %#v, want nil (no claimable work)", processed)
	}

	// The item is also invisible to the scheduler's eligible-work listing.
	scheduled, err := f.repos.Queue.ListScheduled(ctx, f.nowString, 50)
	if err != nil {
		t.Fatalf("Queue.ListScheduled() error = %v", err)
	}
	if len(scheduled) != 0 {
		t.Fatalf("Queue.ListScheduled() = %#v, want no eligible items", scheduled)
	}

	persistedQueue, err := f.repos.Queue.List(ctx)
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(persistedQueue) != 1 || persistedQueue[0].Status != "queued" || persistedQueue[0].ClaimedBy != nil {
		t.Fatalf("queue items = %#v, want the single pre-existing item untouched and unclaimed", persistedQueue)
	}
	assertNoGitMutations(t, f.git)
}

// assertNoGitMutations covers the three losses from #162 that were git-shaped:
// the human's edits were committed, pushed, and their worktree deleted.
func assertNoGitMutations(t *testing.T, git *fakeGitGateway) {
	t.Helper()
	if len(git.commitCalls) != 0 {
		t.Fatalf("git commits = %#v, want none in a taken-over worktree", git.commitCalls)
	}
	if len(git.pushCalls) != 0 {
		t.Fatalf("git pushes = %#v, want none in a taken-over worktree", git.pushCalls)
	}
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("worktree cleanups = %#v, want none while the takeover holds", git.cleanupCalls)
	}
	if len(git.createCalls) != 0 || len(git.prepareCalls) != 0 {
		t.Fatalf("worktree create/prepare = %#v/%#v, want none while the takeover holds", git.createCalls, git.prepareCalls)
	}
}

// TestHumanTakeoverBlocksWorktreeCleanup covers the retention-driven cleanup
// service, which deletes by age rather than by claim: it protected every other
// non-terminal loop status but not human_takeover, so a worktree a human was
// told they owned aged out and was removed.
func TestHumanTakeoverBlocksWorktreeCleanup(t *testing.T) {
	t.Parallel()
	// The run is reconciled, so the only thing that can protect the worktree from
	// the retention sweep is the loop's human_takeover status.
	f := newTakeoverHoldFixture(t, false, "interrupted")
	ctx := context.Background()

	service := &worktreecleanup.Service{
		Repos:  f.repos,
		Config: config.WorktreeCleanupConfig{RetentionDays: 1, MaxPerTick: 10, IncludeOrphans: true},
		// Well past the retention window for every record in the fixture.
		Now: func() time.Time { return f.current.Add(30 * 24 * time.Hour) },
	}
	plan, err := service.Plan(ctx)
	if err != nil {
		t.Fatalf("worktreecleanup Plan() error = %v", err)
	}
	if plan.Summary.WouldClean != 0 {
		t.Fatalf("plan = %#v, want nothing cleaned while a takeover holds", plan.Summary)
	}
	found := false
	for _, decision := range plan.Decisions {
		if decision.Worktree.ID != f.worktree.ID {
			continue
		}
		found = true
		if decision.Action != worktreecleanup.ActionSkipped {
			t.Fatalf("decision = %#v, want skipped", decision)
		}
		if decision.Reason != "referenced by protected loop status human_takeover" {
			t.Fatalf("decision.Reason = %q, want the human_takeover protection reason", decision.Reason)
		}
	}
	if !found {
		t.Fatalf("plan decisions = %#v, want a decision for the taken-over worktree", plan.Decisions)
	}
}
