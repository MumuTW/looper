package worker

import (
	"context"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/labels"
	"github.com/nexu-io/looper/internal/storage"
)

// Worker discovery refreshes every tracked loop's metadata as it walks a batch
// of issues, which for a human-held loop meant a read-modify-write that carried
// the hold. That write is now refused, and a refusal here is not one issue lost
// but the whole batch: ensureLoopForDiscoveredIssue's error aborts the pass for
// every other issue in it. The loop must therefore be skipped before any write.

// TestDiscoverIssuesSkipsHumanHeldWorkerLoop covers the direct case: the held
// issue is a skip, produces no queue item, and leaves the hold untouched.
func TestDiscoverIssuesSkipsHumanHeldWorkerLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	repo := "acme/looper"
	nowISO := fixture.nowISO()
	loopTarget := buildIssueTargetID(repo, 46)
	metadataJSON := `{"worker":{"title":"Implement worker-ready","repo":"acme/looper","baseBranch":"main","executionMode":"create-pr","issueNumber":46,"autoDiscovered":true}}`
	held := storage.LoopRecord{ID: "loop_worker_held", Seq: 90, ProjectID: "project_1", Type: "worker", TargetType: "issue", TargetID: &loopTarget, Repo: &repo, Status: string(domain.LoopStatusHumanTakeover), MetadataJSON: &metadataJSON, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.UpsertChangingHumanHold(ctx, held); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	github := &fakeGitHubGateway{currentLogin: "octocat", issues: []IssueSummary{{Number: 46, Title: "Implement worker-ready", URL: "https://github.com/acme/looper/issues/46", Assignees: []string{"octocat"}, Labels: []string{labels.DefaultWorkerReadyTrigger}}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.DiscoverIssues(ctx, DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v, want the hold reported as a skip", err)
	}
	if len(result.QueueItems) != 0 || len(result.CreatedLoopIDs) != 0 || result.Skipped != 1 {
		t.Fatalf("result = %#v, want the held issue skipped with nothing enqueued", result)
	}
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

// TestDiscoverIssuesHeldLoopDoesNotAbortBatch pins the blast radius: a held loop
// early in the batch must not cost the unrelated issues behind it their work.
func TestDiscoverIssuesHeldLoopDoesNotAbortBatch(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	repo := "acme/looper"
	nowISO := fixture.nowISO()
	loopTarget := buildIssueTargetID(repo, 46)
	metadataJSON := `{"worker":{"title":"Implement worker-ready","repo":"acme/looper","baseBranch":"main","executionMode":"create-pr","issueNumber":46,"autoDiscovered":true}}`
	held := storage.LoopRecord{ID: "loop_worker_held_batch", Seq: 91, ProjectID: "project_1", Type: "worker", TargetType: "issue", TargetID: &loopTarget, Repo: &repo, Status: string(domain.LoopStatusHumanTakeover), MetadataJSON: &metadataJSON, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.UpsertChangingHumanHold(ctx, held); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	github := &fakeGitHubGateway{currentLogin: "octocat", issues: []IssueSummary{
		{Number: 46, Title: "Held by a human", Assignees: []string{"octocat"}, Labels: []string{labels.DefaultWorkerReadyTrigger}},
		{Number: 47, Title: "Ordinary work", Assignees: []string{"octocat"}, Labels: []string{labels.DefaultWorkerReadyTrigger}},
	}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.DiscoverIssues(ctx, DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v, want the batch to survive one held loop", err)
	}
	if len(result.QueueItems) != 1 || result.QueueItems[0].TargetID != buildIssueTargetID(repo, 47) {
		t.Fatalf("result.QueueItems = %#v, want issue 47 still enqueued behind the held issue 46", result.QueueItems)
	}
	if result.Skipped != 1 {
		t.Fatalf("result.Skipped = %d, want only the held issue skipped", result.Skipped)
	}
}

// TestDiscoverIssuesHandbackDirection covers why the skip has to precede the
// write rather than merely preserve the status. The metadata merge is a
// read-modify-write carrying the loop's status, so a tick that read the loop
// while it was held would write human_takeover back after handback released it —
// restoring a hold the human already ended and leaving the loop unclaimable. The
// guard now refuses that write, which turns the resurrection into a failed pass
// unless discovery declines to write held loops at all.
func TestDiscoverIssuesHandbackDirection(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	repo := "acme/looper"
	nowISO := fixture.nowISO()
	loopTarget := buildIssueTargetID(repo, 46)
	metadataJSON := `{"worker":{"title":"Stale title","repo":"acme/looper","baseBranch":"main","executionMode":"create-pr","issueNumber":46,"autoDiscovered":true}}`
	loopID := "loop_worker_handback"
	seeded := storage.LoopRecord{ID: loopID, Seq: 92, ProjectID: "project_1", Type: "worker", TargetType: "issue", TargetID: &loopTarget, Repo: &repo, Status: string(domain.LoopStatusHumanTakeover), MetadataJSON: &metadataJSON, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.UpsertChangingHumanHold(ctx, seeded); err != nil {
		t.Fatalf("Loops.UpsertChangingHumanHold() error = %v", err)
	}
	github := &fakeGitHubGateway{currentLogin: "octocat", issues: []IssueSummary{{Number: 46, Title: "Fresh title from GitHub", Assignees: []string{"octocat"}, Labels: []string{labels.DefaultWorkerReadyTrigger}}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	// A pass over the held loop must not touch the row at all — not even the
	// otherwise harmless metadata refresh, which is the write that carries the
	// status.
	fixture.advance(time.Hour)
	if _, err := runner.DiscoverIssues(ctx, DiscoveryInput{ProjectID: "project_1", Repo: repo}); err != nil {
		t.Fatalf("DiscoverIssues() over a held loop error = %v", err)
	}
	afterHeldPass, err := fixture.repos.Loops.GetByID(ctx, loopID)
	if err != nil || afterHeldPass == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", afterHeldPass, err)
	}
	if afterHeldPass.UpdatedAt != seeded.UpdatedAt || afterHeldPass.MetadataJSON == nil || *afterHeldPass.MetadataJSON != metadataJSON {
		t.Fatalf("loop = %#v, want the row untouched: a held loop gets no write, so none can carry its status", afterHeldPass)
	}

	// The human hands the loop back.
	released := *afterHeldPass
	released.Status = "queued"
	if err := fixture.repos.Loops.UpsertChangingHumanHold(ctx, released); err != nil {
		t.Fatalf("UpsertChangingHumanHold(handback) error = %v", err)
	}

	// This is the write a mid-flight tick would have issued: the pre-handback
	// snapshot, status and all. The guard refuses it, so a discovery pass that
	// still produced it would fail every issue in its batch rather than merely
	// mis-handle this one.
	if err := fixture.repos.Loops.Upsert(ctx, seeded); !storage.IsLoopHumanHeldError(err) {
		t.Fatalf("Upsert(stale held snapshot) error = %v, want storage.ErrLoopHumanHeld", err)
	}

	// And after the handback the loop is ordinary work again: discovery picks it
	// up and the hold stays gone.
	fixture.advance(time.Hour)
	result, err := runner.DiscoverIssues(ctx, DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverIssues() after handback error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("result = %#v, want the handed-back loop enqueued again", result)
	}
	afterHandbackPass, err := fixture.repos.Loops.GetByID(ctx, loopID)
	if err != nil || afterHandbackPass == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", afterHandbackPass, err)
	}
	if afterHandbackPass.Status == string(domain.LoopStatusHumanTakeover) {
		t.Fatalf("status = %q, want the handback to stand after a discovery pass", afterHandbackPass.Status)
	}
}
