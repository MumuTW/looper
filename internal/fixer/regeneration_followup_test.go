package fixer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/storage"
)

func TestHandleTerminalExhaustionUsesDurableTerminalAttemptCount(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, State: "OPEN"},
		commits:           []PullRequestCommit{{AuthorLogin: "looper", CommitterLogin: "looper"}},
	}
	var routed RegenerateIssueInput
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, input RegenerateIssueInput) error {
		routed = input
		return nil
	})
	loop, queue := setupRegenerationRecords(t, fixture)
	queue.Attempts = 2 // claimed snapshot; the durable terminal row is 3.
	queue.Priority = storage.QueuePriorityFixer
	queue.ID = "queue_exhausted_attempts"
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: queue.ID, ProjectID: queue.ProjectID, LoopID: queue.LoopID, Type: queue.Type, TargetType: queue.TargetType, TargetID: queue.TargetID, Repo: queue.Repo, PRNumber: queue.PRNumber, DedupeKey: "fixer:attempt-count", Priority: storage.QueuePriorityFixer, Status: "failed", Attempts: 3, MaxAttempts: 3, CreatedAt: queue.CreatedAt, UpdatedAt: queue.UpdatedAt}); err != nil {
		t.Fatalf("Queue.Upsert(terminal) error = %v", err)
	}
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	_, action, err := runner.applyTerminalRegeneration(context.Background(), *project, loop, queue, fixerCheckpoint{}, &loopError{message: "failed", kind: FailureRetryableTransient})
	if err != nil || action != regenerationCompleted {
		t.Fatalf("applyTerminalRegeneration() = action %q err %v", action, err)
	}
	if routed.Attempts != 3 || routed.MaxAttempts != 3 {
		t.Fatalf("route attempts = %d/%d, want durable terminal 3/3", routed.Attempts, routed.MaxAttempts)
	}
	comments := gateway.fakeGitHubGateway.createIssueComments
	if len(comments) != 1 || !strings.Contains(comments[0].Body, "Attempts: 3/3") {
		t.Fatalf("regeneration comment = %#v, want Attempts: 3/3", comments)
	}
}

func TestTerminalRegenerationReplayRequeuesAndClaimsAfterHandoffFailure(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, State: "OPEN"},
		commits:           []PullRequestCommit{{AuthorLogin: "looper", CommitterLogin: "looper"}},
	}
	routes := 0
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, _ RegenerateIssueInput) error {
		routes++
		if routes == 1 {
			return errors.New("planner unavailable")
		}
		return nil
	})
	loop, queue := setupRegenerationRecords(t, fixture)
	queue.Priority = storage.QueuePriorityFixer
	if err := fixture.repos.Queue.Upsert(context.Background(), queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if _, _, err := runner.applyTerminalRegeneration(context.Background(), *project, loop, queue, fixerCheckpoint{}, &loopError{message: "failed", kind: FailureRetryableTransient}); err == nil {
		t.Fatal("first applyTerminalRegeneration() error = nil, want route failure")
	}
	persistedQueue, err := fixture.repos.Queue.GetByID(context.Background(), queue.ID)
	if err != nil || persistedQueue == nil || persistedQueue.Status != "queued" {
		t.Fatalf("replay queue = %#v err=%v, want queued", persistedQueue, err)
	}
	persistedLoop, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || persistedLoop == nil || persistedLoop.Status != "queued" {
		t.Fatalf("replay loop = %#v err=%v, want queued", persistedLoop, err)
	}
	fixture.advance(6 * time.Second)
	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "replay", "fixer")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextOfType() = %#v err=%v, want replay claim", claimed, err)
	}
	result, err := runner.ProcessClaimedQueueItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(replay) error = %v", err)
	}
	if result.Status != "failed" || routes != 2 || len(gateway.closeCalls) != 1 {
		t.Fatalf("replay result=%#v routes=%d closes=%d, want failed/2/1", result, routes, len(gateway.closeCalls))
	}
}

func TestEscalationCheckpointsCommentBeforeLabelRetry(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, State: "OPEN"},
		commits:           []PullRequestCommit{{AuthorLogin: "alice", CommitterLogin: "alice"}},
		prLabelErr:        errors.New("label unavailable"),
	}
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, _ RegenerateIssueInput) error { return nil })
	loop, queue := setupRegenerationRecords(t, fixture)
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if _, err := runner.handleTerminalExhaustion(context.Background(), *project, loop, queue, fixerCheckpoint{}, &loopError{message: "failed", kind: FailureRetryableTransient}); err == nil {
		t.Fatal("handleTerminalExhaustion() error = nil, want label failure")
	}
	durable, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || durable == nil || !strings.Contains(derefString(durable.MetadataJSON), `"commented":true`) {
		t.Fatalf("durable escalation state = %#v err=%v, want Commented=true", durable, err)
	}
	if len(gateway.fakeGitHubGateway.createIssueComments) != 1 {
		t.Fatalf("comments=%d, want one before label retry", len(gateway.fakeGitHubGateway.createIssueComments))
	}
	gateway.prLabelErr = nil
	if action, err := runner.handleTerminalExhaustion(context.Background(), *project, *durable, queue, fixerCheckpoint{}, &loopError{message: "failed", kind: FailureRetryableTransient}); err != nil || action != regenerationEscalated {
		t.Fatalf("retry escalation = action %q err %v, want escalated", action, err)
	}
	if len(gateway.fakeGitHubGateway.createIssueComments) != 1 {
		t.Fatalf("retry comments=%d, want no duplicate", len(gateway.fakeGitHubGateway.createIssueComments))
	}
}

func TestRegenerationRefusesWhenPlannerUnavailable(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, State: "OPEN"},
		commits:           []PullRequestCommit{{AuthorLogin: "looper", CommitterLogin: "looper"}},
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: gateway, Now: fixture.now, Logger: fixture.logger, OnRegenerateIssue: func(context.Context, RegenerateIssueInput) error { return nil }, RegenerationAvailability: func(string) string { return "Planner agent is not configured" }})
	loop, queue := setupRegenerationRecords(t, fixture)
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	action, err := runner.handleTerminalExhaustion(context.Background(), *project, loop, queue, fixerCheckpoint{}, &loopError{message: "failed", kind: FailureRetryableTransient})
	if err != nil || action != regenerationEscalated {
		t.Fatalf("planner unavailable = action %q err %v, want escalated", action, err)
	}
	if len(gateway.closeCalls) != 0 || len(gateway.addLabelCalls) != 1 || gateway.addLabelCalls[0].Labels[0] != labels.NeedsHuman {
		t.Fatalf("planner unavailable side effects: closes=%d labels=%#v", len(gateway.closeCalls), gateway.addLabelCalls)
	}
}

func TestRegenerationAbortsWhenPullRequestMergesConcurrently(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, State: "OPEN"},
		commits:           []PullRequestCommit{{AuthorLogin: "looper", CommitterLogin: "looper"}},
		closeErr:          githubinfra.ErrPullRequestAlreadyMerged,
	}
	routes := 0
	runner := newRegenerationRunner(t, fixture, gateway, func(context.Context, RegenerateIssueInput) error { routes++; return nil })
	loop, queue := setupRegenerationRecords(t, fixture)
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	action, err := runner.handleTerminalExhaustion(context.Background(), *project, loop, queue, fixerCheckpoint{}, &loopError{message: "failed", kind: FailureRetryableTransient})
	if err != nil || action != regenerationEscalated {
		t.Fatalf("merged concurrent = action %q err %v, want escalated", action, err)
	}
	if routes != 0 || len(gateway.closeCalls) != 1 {
		t.Fatalf("merged concurrent side effects: routes=%d closes=%d, want 0/1", routes, len(gateway.closeCalls))
	}
}
