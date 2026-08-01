package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/network/protocol"
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
	if len(first.QueueItems) != 1 || len(github.addAssigneeCalls) != 1 || github.listIssueCalls[0].Limit != personalIssueQueryLimit {
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
	fixture, runner := newPersonalWorkerRunner(t, gateway, &cfg)
	runner.network = &stubWorkerNetwork{status: protocol.NodeStatusResponse{Membership: protocol.Membership{NodeName: "worker-1"}}}
	_ = fixture
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
