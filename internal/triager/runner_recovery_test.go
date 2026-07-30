package triager

import (
	"context"
	"errors"
	"testing"
	"time"

	githubinfra "github.com/MumuTW/looper/internal/infra/github"
)

func TestDiscoverIssuesRetriesPersistedReportAfterLookbackAndRouteFailure(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.llm.responses = []string{eligibleDecisionJSON()}
	fixture.planner.err = errors.New("planner unavailable")
	runner := fixture.runner()

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err == nil {
		t.Fatal("DiscoverIssues() error = nil, want planner failure")
	}
	fixture.planner.err = nil
	fixture.github.listEmpty = true
	fixture.now = fixture.now.Add(30 * time.Minute)
	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("retry DiscoverIssues() error = %v", err)
	}
	if result.Routed != 1 || fixture.llm.calls != 1 || len(fixture.planner.inputs) != 2 {
		t.Fatalf("retry result = %#v, LLM/planner calls = %d/%d", result, fixture.llm.calls, len(fixture.planner.inputs))
	}
}

func TestDiscoverIssuesRetriesEnrolledSourceAfterAgentOutageAndLookback(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.llm.responses = []string{"", eligibleDecisionJSON()}
	fixture.llm.errors = []error{errors.New("agent outage"), nil}
	runner := fixture.runner()

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err == nil {
		t.Fatal("DiscoverIssues() error = nil, want agent outage")
	}
	fixture.github.listEmpty = true
	fixture.now = fixture.now.Add(30 * time.Minute)
	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("retry DiscoverIssues() error = %v", err)
	}
	if result.ReportsPersisted != 1 || result.Routed != 1 || fixture.llm.calls != 2 {
		t.Fatalf("retry result = %#v, LLM calls = %d", result, fixture.llm.calls)
	}
}

func TestDiscoverIssuesRevalidatesClosedIssueAfterDecision(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	open := fixture.github.detail
	closed := open
	closed.State = "closed"
	fixture.github.viewSequence = []githubinfra.IssueDetail{open, open, closed}
	fixture.llm.responses = []string{eligibleDecisionJSON()}

	result, err := fixture.runner().DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if result.Retired != 1 || result.ReportsPersisted != 0 || len(fixture.planner.inputs) != 0 {
		t.Fatalf("DiscoverIssues() = %#v, planner calls = %d", result, len(fixture.planner.inputs))
	}
}

func TestDiscoverIssuesBoundsDecisionsAndContinuesPendingNextTick(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	second := fixture.github.detail
	second.Number = 24
	second.Title = "Second issue"
	second.URL = "https://github.com/acme/looper/issues/24"
	fixture.github.details = map[int64]githubinfra.IssueDetail{23: fixture.github.detail, 24: second}
	fixture.llm.responses = []string{eligibleDecisionJSON(), eligibleDecisionJSON()}
	runner := New(Options{Repos: fixture.repos, GitHub: fixture.github, LLM: fixture.llm, Planner: fixture.planner, Now: func() time.Time { return fixture.now }, DecisionLimit: 1})

	first, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("first DiscoverIssues() error = %v", err)
	}
	if first.Enrolled != 2 || first.DecisionsAttempted != 1 || fixture.llm.calls != 1 {
		t.Fatalf("first result = %#v, LLM calls = %d", first, fixture.llm.calls)
	}
	fixture.github.listEmpty = true
	secondResult, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("second DiscoverIssues() error = %v", err)
	}
	if secondResult.DecisionsAttempted != 1 || fixture.llm.calls != 2 || len(fixture.planner.inputs) != 2 {
		t.Fatalf("second result = %#v, LLM/planner calls = %d/%d", secondResult, fixture.llm.calls, len(fixture.planner.inputs))
	}
}
