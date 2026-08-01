package planner

import (
	"context"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/labels"
)

func personalPlannerConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []config.ProjectRefConfig{{ID: "project_1", PersonalProject: true}}
	return cfg
}

func TestDiscoverIssuesPersonalProjectAssignsSelfAuthoredIssueIdempotently(t *testing.T) {
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{login: "octocat", issues: []IssueSummary{{Number: 501, Title: "Personal work", Author: "octocat", Labels: []string{labels.DefaultPlanTrigger}}}}
	cfg := personalPlannerConfig(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Logger: fixture.logger, Now: fixture.now, CustomInstructions: &cfg, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, Labels: []string{labels.DefaultPlanTrigger}, LabelMode: config.LabelModeAll, RequireAssigneeCurrentUser: true}})

	first, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("first DiscoverIssues() error = %v", err)
	}
	if len(first.QueueItems) != 1 || len(github.addAssigneeCalls) != 1 || github.listOpenIssueCalls[0].Limit != personalIssueQueryLimit {
		t.Fatalf("first result = %#v, assignee calls = %#v, want one queue and one assignment", first, github.addAssigneeCalls)
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
		"author mismatch":   {Number: 502, Title: "Other work", Author: "someone-else", Labels: []string{labels.DefaultPlanTrigger}},
		"existing assignee": {Number: 503, Title: "Assigned work", Author: "octocat", Assignees: []string{"teammate"}, Labels: []string{labels.DefaultPlanTrigger}},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newRunnerFixture(t)
			github := &fakeGitHubGateway{login: "octocat", issues: []IssueSummary{issue}}
			cfg := personalPlannerConfig(t)
			runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Logger: fixture.logger, Now: fixture.now, CustomInstructions: &cfg, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, Labels: []string{labels.DefaultPlanTrigger}, LabelMode: config.LabelModeAll, RequireAssigneeCurrentUser: true}})
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
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{login: "octocat", issues: []IssueSummary{{Number: 504, Title: "Opt-out", Author: "octocat", Labels: []string{labels.DefaultPlanTrigger}}}}
	cfg := personalPlannerConfig(t)
	cfg.Roles.Planner.Triggers.RequireAssigneeCurrentUser = false
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Logger: fixture.logger, Now: fixture.now, CustomInstructions: &cfg, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, Labels: []string{labels.DefaultPlanTrigger}, LabelMode: config.LabelModeAll, RequireAssigneeCurrentUser: false}})
	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 1 || len(github.addAssigneeCalls) != 0 {
		t.Fatalf("result = %#v, assignee calls = %#v, want queue without assignment", result, github.addAssigneeCalls)
	}
}

func TestDiscoverIssuesPersonalProjectRoutedNetworkDoesNotAssign(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &fakeGitHubGateway{login: "octocat", issues: []IssueSummary{{Number: 505, Author: "octocat", Labels: []string{labels.DefaultPlanTrigger}}}}
	cfg := personalPlannerConfig(t)
	cfg.Projects[0].Network.Mode = config.NetworkModeRouted
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: gateway, Logger: fixture.logger, Now: fixture.now, CustomInstructions: &cfg, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, Labels: []string{labels.DefaultPlanTrigger}, LabelMode: config.LabelModeAll, RequireAssigneeCurrentUser: true}})
	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 0 || len(gateway.addAssigneeCalls) != 0 || gateway.listOpenIssueCalls[0].Assignee != "octocat" {
		t.Fatalf("result=%#v calls=%#v query=%#v, want routed personal policy disabled", result, gateway.addAssigneeCalls, gateway.listOpenIssueCalls)
	}
}

func TestDiscoverIssuesPersonalProjectHoldSkipsBeforeAssignment(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &fakeGitHubGateway{login: "octocat", issues: []IssueSummary{{Number: 506, Author: "octocat", Labels: []string{labels.DefaultPlanTrigger, labels.HoldGlobal}}}}
	cfg := personalPlannerConfig(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: gateway, Logger: fixture.logger, Now: fixture.now, CustomInstructions: &cfg, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, Labels: []string{labels.DefaultPlanTrigger}, LabelMode: config.LabelModeAll, RequireAssigneeCurrentUser: true}})
	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 0 || len(gateway.addAssigneeCalls) != 0 {
		t.Fatalf("result=%#v calls=%#v, want held issue skipped before assignment", result, gateway.addAssigneeCalls)
	}
}
