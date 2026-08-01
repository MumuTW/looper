package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/network/protocol"
	"github.com/MumuTW/looper/internal/storage"
)

func personalWorkerConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []config.ProjectRefConfig{{ID: "project_1", PersonalProject: true}}
	return cfg
}

func newPersonalWorkerRunner(t *testing.T, github *fakeGitHubGateway, cfg *config.Config) (*runnerFixture, *Runner) {
	t.Helper()
	fixture := newRunnerFixture(t)
	runner := New(Options{
		DB:                 fixture.coordinator.DB(),
		Repos:              fixture.repos,
		GitHub:             github,
		Git:                &fakeGitGateway{},
		AgentExecutor:      &fakeAgentExecutor{},
		Logger:             fixture.logger,
		Now:                fixture.now,
		CustomInstructions: cfg,
		DiscoveryPolicy: DiscoveryPolicy{
			AutoDiscovery:              true,
			Labels:                     []string{labels.DefaultWorkerReadyTrigger},
			LabelMode:                  config.LabelModeAll,
			RequireAssigneeCurrentUser: true,
		},
	})
	return fixture, runner
}

func TestDiscoverIssuesPersonalProjectAssignsSelfAuthoredIssueIdempotently(t *testing.T) {
	github := &fakeGitHubGateway{currentLogin: "octocat", issues: []IssueSummary{{Number: 601, Title: "Personal worker", Author: "octocat", Labels: []string{labels.DefaultWorkerReadyTrigger}}}}
	cfg := personalWorkerConfig(t)
	fixture, runner := newPersonalWorkerRunner(t, github, &cfg)

	first, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("first DiscoverIssues() error = %v", err)
	}
	if len(first.QueueItems) != 1 || len(github.addAssigneeCalls) != 1 || github.listIssueCalls[0].Limit != personalIssueQueryLimit || github.listIssueCalls[0].Search != "author:octocat" {
		t.Fatalf("first result = %#v, assignee calls = %#v, want one queue and one assignment", first, github.addAssigneeCalls)
	}
	if got := github.listIssueCalls[0].Assignee; got != "" {
		t.Fatalf("ListOpenIssues assignee filter = %q, want empty for personal project", got)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), first.QueueItems[0].ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.PayloadJSON == nil {
		t.Fatalf("queue = %#v, want payload", queue)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(*queue.PayloadJSON), &payload); err != nil {
		t.Fatalf("queue payload unmarshal error = %v", err)
	}
	if payload["personalProjectAutoAssigned"] != true {
		t.Fatalf("queue payload = %#v, want personal assignment audit marker", payload)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), derefString(first.QueueItems[0].LoopID))
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || !strings.Contains(derefString(loop.MetadataJSON), `"personalProjectAutoAssigned":true`) {
		t.Fatalf("loop metadata = %#v, want personal assignment audit marker", loop)
	}

	github.issues[0].Assignees = []string{"octocat"}
	second, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("second DiscoverIssues() error = %v", err)
	}
	if len(second.QueueItems) != 1 || len(github.addAssigneeCalls) != 1 {
		t.Fatalf("second result = %#v, assignee calls = %#v, want idempotent rediscovery", second, github.addAssigneeCalls)
	}
}

func TestDiscoverIssuesPersonalProjectPreservesMismatchAndExistingAssignee(t *testing.T) {
	for name, issue := range map[string]IssueSummary{
		"author mismatch":   {Number: 602, Title: "Other work", Author: "someone-else", Labels: []string{labels.DefaultWorkerReadyTrigger}},
		"existing assignee": {Number: 603, Title: "Assigned work", Author: "octocat", Assignees: []string{"teammate"}, Labels: []string{labels.DefaultWorkerReadyTrigger}},
	} {
		t.Run(name, func(t *testing.T) {
			github := &fakeGitHubGateway{currentLogin: "octocat", issues: []IssueSummary{issue}}
			cfg := personalWorkerConfig(t)
			_, runner := newPersonalWorkerRunner(t, github, &cfg)
			result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
			if err != nil {
				t.Fatalf("DiscoverIssues() error = %v", err)
			}
			if len(result.QueueItems) != 0 || len(github.addAssigneeCalls) != 0 {
				t.Fatalf("result = %#v, assignee calls = %#v, want no admission mutation", result, github.addAssigneeCalls)
			}
		})
	}
}

func TestDiscoverIssuesPersonalProjectDisabledAssigneePolicyDoesNotAssign(t *testing.T) {
	github := &fakeGitHubGateway{currentLogin: "octocat", issues: []IssueSummary{{Number: 604, Title: "Opt-out", Author: "octocat", Labels: []string{labels.DefaultWorkerReadyTrigger}}}}
	cfg := personalWorkerConfig(t)
	cfg.Roles.Worker.Triggers.RequireAssigneeCurrentUser = false
	_, runner := newPersonalWorkerRunner(t, github, &cfg)
	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 1 || len(github.addAssigneeCalls) != 0 {
		t.Fatalf("result = %#v, assignee calls = %#v, want queue without assignment", result, github.addAssigneeCalls)
	}
}

func TestDiscoverIssuesPersonalProjectRoutedNetworkDoesNotAssign(t *testing.T) {
	gateway := &fakeGitHubGateway{currentLogin: "octocat", issues: []IssueSummary{{Number: 605, Author: "octocat", Labels: []string{labels.DefaultWorkerReadyTrigger}}}}
	cfg := personalWorkerConfig(t)
	cfg.Projects[0].Network.Mode = config.NetworkModeRouted
	_, runner := newPersonalWorkerRunner(t, gateway, &cfg)
	runner.network = &stubWorkerNetwork{status: protocol.NodeStatusResponse{Membership: protocol.Membership{NodeName: "worker-1"}}}
	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 0 || len(gateway.addAssigneeCalls) != 0 || gateway.listIssueCalls[0].Assignee != "" {
		t.Fatalf("result=%#v calls=%#v query=%#v, want routed personal policy disabled", result, gateway.addAssigneeCalls, gateway.listIssueCalls)
	}
}

func TestDiscoverIssuesPersonalProjectHoldSkipsBeforeAssignment(t *testing.T) {
	gateway := &fakeGitHubGateway{currentLogin: "octocat", issues: []IssueSummary{{Number: 606, Author: "octocat", Labels: []string{labels.DefaultWorkerReadyTrigger, labels.HoldGlobal}}}}
	cfg := personalWorkerConfig(t)
	_, runner := newPersonalWorkerRunner(t, gateway, &cfg)
	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 0 || len(gateway.addAssigneeCalls) != 0 {
		t.Fatalf("result=%#v calls=%#v, want held issue skipped before assignment", result, gateway.addAssigneeCalls)
	}
}

func TestDiscoverIssuesPersonalProjectClaimConflictSkipsBeforeAssignment(t *testing.T) {
	gateway := &fakeGitHubGateway{currentLogin: "octocat", issues: []IssueSummary{{Number: 607, Author: "octocat", Labels: []string{labels.DefaultWorkerReadyTrigger}}}}
	cfg := personalWorkerConfig(t)
	fixture, runner := newPersonalWorkerRunner(t, gateway, &cfg)
	repo := "acme/looper"
	prNumber := int64(707)
	prTarget := "pr:acme/looper:707"
	claimMetadata := `{"worker":{"repo":"acme/looper","issueNumber":607}}`
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_reviewer_claim", Seq: 2, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", TargetID: &prTarget, Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &claimMetadata, CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("seed reviewer claim: %v", err)
	}
	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if result.Skipped == 0 || len(result.QueueItems) != 0 || len(gateway.addAssigneeCalls) != 0 {
		t.Fatalf("result=%#v calls=%#v, want claim conflict skipped before assignment", result, gateway.addAssigneeCalls)
	}
}

func TestDiscoverIssuesPersonalProjectAssignmentFailureSkipsIssue(t *testing.T) {
	gateway := &fakeGitHubGateway{currentLogin: "octocat", addAssigneeErr: errors.New("assignment unavailable"), issues: []IssueSummary{
		{Number: 608, Author: "octocat", Labels: []string{labels.DefaultWorkerReadyTrigger}},
		{Number: 609, Author: "octocat", Labels: []string{labels.DefaultWorkerReadyTrigger}},
	}}
	cfg := personalWorkerConfig(t)
	_, runner := newPersonalWorkerRunner(t, gateway, &cfg)
	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v, want per-issue assignment failure to be skipped", err)
	}
	if result.Skipped != 2 || len(result.QueueItems) != 0 || len(gateway.addAssigneeCalls) != 2 {
		t.Fatalf("result=%#v calls=%#v, want both failed assignments skipped", result, gateway.addAssigneeCalls)
	}
	if len(result.CreatedLoopIDs) != 2 {
		t.Fatalf("created loops=%#v, want both claim-admitted issues retained for retry", result.CreatedLoopIDs)
	}
}

// TestDiscoverIssuesPersonalProjectReconcilesAssignmentMarker verifies that
// rediscovery reconciles the personalProjectAutoAssigned audit marker after a
// prior partial success: the GitHub assignee was persisted but the durable
// marker was not. The daemon is already an assignee, so no second assignment
// mutation occurs, yet the loop and queue markers are recorded.
func TestDiscoverIssuesPersonalProjectReconcilesAssignmentMarker(t *testing.T) {
	repo := "acme/looper"
	target := buildIssueTargetID(repo, 610)
	gateway := &fakeGitHubGateway{currentLogin: "octocat", issues: []IssueSummary{{Number: 610, Title: "Reconcile", Author: "octocat", Assignees: []string{"octocat"}, Labels: []string{labels.DefaultWorkerReadyTrigger}}}}
	cfg := personalWorkerConfig(t)
	fixture, runner := newPersonalWorkerRunner(t, gateway, &cfg)
	nowISO := fixture.nowISO()
	// Existing worker loop without the personalProjectAutoAssigned marker,
	// simulating a prior run that assigned the daemon on GitHub but failed
	// before the marker was persisted.
	loopMetadata := `{"worker":{"title":"Reconcile","repo":"acme/looper","issueNumber":610,"baseBranch":"main"}}`
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_marker", Seq: 2, ProjectID: "project_1", Type: "worker", TargetType: "issue", TargetID: &target, Repo: &repo, Status: "queued", MetadataJSON: &loopMetadata, NextRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("seed loop: %v", err)
	}
	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(gateway.addAssigneeCalls) != 0 {
		t.Fatalf("addAssigneeCalls=%#v, want no second assignment for already-assigned issue", gateway.addAssigneeCalls)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("result=%#v, want one queue item for reconciled issue", result)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_worker_marker")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || !strings.Contains(derefString(loop.MetadataJSON), `"personalProjectAutoAssigned":true`) {
		t.Fatalf("loop metadata = %#v, want reconciled personal assignment marker", loop)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), result.QueueItems[0].ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || !strings.Contains(derefString(queue.PayloadJSON), `"personalProjectAutoAssigned":true`) {
		t.Fatalf("queue payload = %#v, want reconciled personal assignment marker", queue)
	}
}
