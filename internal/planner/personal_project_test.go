package planner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/storage"
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
	if len(first.QueueItems) != 1 || len(github.addAssigneeCalls) != 1 || github.listOpenIssueCalls[0].Limit != personalIssueQueryLimit || github.listOpenIssueCalls[0].Search != "author:octocat" {
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

func TestDiscoverIssuesPersonalProjectAssignmentFailureSkipsIssue(t *testing.T) {
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{login: "octocat", addAssigneeErr: errors.New("assignment unavailable"), issues: []IssueSummary{
		{Number: 507, Author: "octocat", Labels: []string{labels.DefaultPlanTrigger}},
		{Number: 508, Author: "octocat", Labels: []string{labels.DefaultPlanTrigger}},
	}}
	cfg := personalPlannerConfig(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Logger: fixture.logger, Now: fixture.now, CustomInstructions: &cfg, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, Labels: []string{labels.DefaultPlanTrigger}, LabelMode: config.LabelModeAll, RequireAssigneeCurrentUser: true}})
	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v, want per-issue assignment failure to be skipped", err)
	}
	if result.Skipped != 2 || len(result.QueueItems) != 0 || len(github.addAssigneeCalls) != 2 {
		t.Fatalf("result=%#v calls=%#v, want both failed assignments skipped", result, github.addAssigneeCalls)
	}
}

// TestDiscoverIssuesPersonalProjectSkipsInactiveLoopBeforeAssignment verifies
// that discovery resolves the durable loop (admission) before mutating the
// GitHub assignee. A self-authored issue whose Planner loop is intentionally
// inactive (paused/completed/awaiting_human) must be skipped without assigning
// the daemon, so rediscovery does not re-add an assignee to work that will not
// be queued.
func TestDiscoverIssuesPersonalProjectSkipsInactiveLoopBeforeAssignment(t *testing.T) {
	for name, status := range map[string]string{
		"paused":         "paused",
		"completed":      "completed",
		"awaiting_human": "awaiting_human",
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newRunnerFixture(t)
			repo := "acme/looper"
			target := buildIssueTargetID(repo, 509)
			nowISO := fixture.nowISO()
			metadata := `{"issueNumber":509,"repo":"acme/looper"}`
			if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_planner_inactive", Seq: 1, ProjectID: "project_1", Type: "planner", TargetType: "issue", TargetID: &target, Repo: &repo, Status: status, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
				t.Fatalf("seed inactive loop: %v", err)
			}
			github := &fakeGitHubGateway{login: "octocat", issues: []IssueSummary{{Number: 509, Title: "Paused work", Author: "octocat", Labels: []string{labels.DefaultPlanTrigger}}}}
			cfg := personalPlannerConfig(t)
			runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Logger: fixture.logger, Now: fixture.now, CustomInstructions: &cfg, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, Labels: []string{labels.DefaultPlanTrigger}, LabelMode: config.LabelModeAll, RequireAssigneeCurrentUser: true}})
			result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
			if err != nil {
				t.Fatalf("DiscoverIssues() error = %v", err)
			}
			if len(result.QueueItems) != 0 || len(github.addAssigneeCalls) != 0 || result.Skipped == 0 {
				t.Fatalf("result=%#v calls=%#v, want inactive loop skipped before assignment", result, github.addAssigneeCalls)
			}
		})
	}
}

// TestDiscoverIssuesPersonalProjectReconcilesAssignmentMarker verifies that
// rediscovery reconciles the personalProjectAutoAssigned audit marker after a
// prior partial success: the GitHub assignee was persisted but the durable
// marker was not. The daemon is already an assignee, so no second assignment
// mutation occurs, yet the loop and queue markers are recorded.
func TestDiscoverIssuesPersonalProjectReconcilesAssignmentMarker(t *testing.T) {
	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	target := buildIssueTargetID(repo, 510)
	nowISO := fixture.nowISO()
	// Existing loop without the personalProjectAutoAssigned marker, simulating
	// a prior run that assigned the daemon on GitHub but failed before the
	// marker was persisted.
	metadata := `{"issueNumber":510,"repo":"acme/looper"}`
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_planner_marker", Seq: 1, ProjectID: "project_1", Type: "planner", TargetType: "issue", TargetID: &target, Repo: &repo, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("seed loop: %v", err)
	}
	github := &fakeGitHubGateway{login: "octocat", issues: []IssueSummary{{Number: 510, Title: "Reconcile", Author: "octocat", Assignees: []string{"octocat"}, Labels: []string{labels.DefaultPlanTrigger}}}}
	cfg := personalPlannerConfig(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Logger: fixture.logger, Now: fixture.now, CustomInstructions: &cfg, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, Labels: []string{labels.DefaultPlanTrigger}, LabelMode: config.LabelModeAll, RequireAssigneeCurrentUser: true}})
	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(github.addAssigneeCalls) != 0 {
		t.Fatalf("addAssigneeCalls=%#v, want no second assignment for already-assigned issue", github.addAssigneeCalls)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("result=%#v, want one queue item for reconciled issue", result)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_planner_marker")
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
