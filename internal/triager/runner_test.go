package triager

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/planner"
	"github.com/nexu-io/looper/internal/storage"
)

func TestDiscoverIssuesRoutesEligibleReportToPlanner(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.llm.responses = []string{eligibleDecisionJSON()}

	result, err := fixture.runner().DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if result.ReportsPersisted != 1 || result.Routed != 1 || result.AwaitingConfirmation != 0 {
		t.Fatalf("DiscoverIssues() = %#v, want one persisted and routed report", result)
	}
	if len(fixture.planner.inputs) != 1 || fixture.planner.inputs[0].Issue.Number != 23 {
		t.Fatalf("planner routes = %#v, want issue 23", fixture.planner.inputs)
	}
	report := fixture.singleReport(t)
	if report.Decision.Classification != ClassificationFeature || report.Decision.RecommendedNextRole != NextRolePlanner {
		t.Fatalf("report decision = %#v", report.Decision)
	}
	if report.Policy.Action != ActionRoutePlanner || report.IdempotencyKey == "" {
		t.Fatalf("report policy/idempotency = %#v / %q", report.Policy, report.IdempotencyKey)
	}
}

func TestDiscoverIssuesReplaysPersistedRouteWithoutRepeatingLLM(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.llm.responses = []string{eligibleDecisionJSON()}
	runner := fixture.runner()

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("first DiscoverIssues() error = %v", err)
	}
	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("second DiscoverIssues() error = %v", err)
	}
	if fixture.llm.calls != 1 {
		t.Fatalf("LLM calls = %d, want 1", fixture.llm.calls)
	}
	if len(fixture.planner.inputs) != 2 {
		t.Fatalf("planner calls = %d, want idempotent route projection retried", len(fixture.planner.inputs))
	}
	events, err := fixture.repos.Events.List(context.Background(), 10)
	if err != nil {
		t.Fatalf("Events.List() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("triage reports = %d, want 1", len(events))
	}
}

func TestDiscoverIssuesTreatsReopenedIssueAsNewSourceEvent(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.llm.responses = []string{eligibleDecisionJSON(), eligibleDecisionJSON()}
	runner := fixture.runner()

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("new issue DiscoverIssues() error = %v", err)
	}
	fixture.github.timeline = []map[string]any{{
		"id": 91, "event": "reopened", "created_at": fixture.now.Add(-30 * time.Second).Format(time.RFC3339Nano),
	}}
	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("reopened issue DiscoverIssues() error = %v", err)
	}
	events, err := fixture.repos.Events.List(context.Background(), 10)
	if err != nil {
		t.Fatalf("Events.List() error = %v", err)
	}
	if fixture.llm.calls != 2 || len(events) != 2 {
		t.Fatalf("LLM calls/events = %d/%d, want a report per new and reopened event", fixture.llm.calls, len(events))
	}
}

func TestDiscoverIssuesHonorsExistingHoldBeforeLLM(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.github.detail.Labels = []string{"looper:hold"}
	fixture.llm.responses = []string{eligibleDecisionJSON()}

	result, err := fixture.runner().DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if fixture.llm.calls != 0 || len(fixture.planner.inputs) != 0 {
		t.Fatalf("held issue invoked LLM/planner: calls=%d routes=%d", fixture.llm.calls, len(fixture.planner.inputs))
	}
	if result.Skipped != 1 {
		t.Fatalf("result = %#v, want held issue skipped", result)
	}
}

func TestDiscoverIssuesPersistsUnsafeOrUncertainDecisionsForConfirmation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		decision string
		reason   string
	}{
		{
			name:     "high risk",
			decision: `{"classification":"feature","scope":"in_scope","risk":"high","confidence":0.98,"missingInformation":[],"recommendedNextRole":"planner","rationale":"Touches credentials."}`,
			reason:   "risk_not_low",
		},
		{
			name:     "uncertain and underspecified",
			decision: `{"classification":"bug","scope":"in_scope","risk":"low","confidence":0.62,"missingInformation":["expected behavior"],"recommendedNextRole":"planner","rationale":"The report lacks a reproducible outcome."}`,
			reason:   "confidence_below_threshold",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newRunnerFixture(t)
			fixture.llm.responses = []string{tt.decision}

			result, err := fixture.runner().DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
			if err != nil {
				t.Fatalf("DiscoverIssues() error = %v", err)
			}
			if result.AwaitingConfirmation != 1 || result.Routed != 0 || len(fixture.planner.inputs) != 0 {
				t.Fatalf("DiscoverIssues() = %#v routes=%d", result, len(fixture.planner.inputs))
			}
			report := fixture.singleReport(t)
			if report.Policy.Action != ActionAwaitHuman || len(report.Policy.Reasons) == 0 || report.Policy.Reasons[0] != tt.reason {
				t.Fatalf("policy = %#v, want reason %q", report.Policy, tt.reason)
			}
		})
	}
}

type runnerFixture struct {
	now         time.Time
	coordinator *storage.SQLiteCoordinator
	repos       *storage.Repositories
	github      *fakeGitHub
	llm         *fakeLLM
	planner     *fakePlanner
}

func newRunnerFixture(t *testing.T) *runnerFixture {
	t.Helper()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "triager.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner.RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	project := storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), CreatedAt: now.Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano)}
	if err := repos.Projects.Upsert(context.Background(), project); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	detail := githubinfra.IssueDetail{
		Number: 23, Title: "Add autonomous triage", Body: "Route clear reports into planning.", URL: "https://github.com/acme/looper/issues/23",
		State: "open", CreatedAt: now.Add(-time.Minute).Format(time.RFC3339Nano), UpdatedAt: now.Add(-time.Minute).Format(time.RFC3339Nano),
	}
	return &runnerFixture{
		now: now, coordinator: coordinator, repos: repos,
		github:  &fakeGitHub{detail: detail},
		llm:     &fakeLLM{},
		planner: &fakePlanner{},
	}
}

func (f *runnerFixture) runner() *Runner {
	return New(Options{Repos: f.repos, GitHub: f.github, LLM: f.llm, Planner: f.planner, Now: func() time.Time { return f.now }})
}

func (f *runnerFixture) singleReport(t *testing.T) Report {
	t.Helper()
	events, err := f.repos.Events.List(context.Background(), 10)
	if err != nil {
		t.Fatalf("Events.List() error = %v", err)
	}
	if len(events) != 1 || events[0].EventType != ReportEventType {
		t.Fatalf("events = %#v, want one triage report", events)
	}
	var report Report
	if err := json.Unmarshal([]byte(events[0].PayloadJSON), &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	return report
}

type fakeGitHub struct {
	detail     githubinfra.IssueDetail
	timeline   []map[string]any
	listInput  githubinfra.ListOpenIssuesInput
	permission string
}

func (f *fakeGitHub) ListOpenIssues(_ context.Context, input githubinfra.ListOpenIssuesInput) ([]githubinfra.IssueSummary, error) {
	f.listInput = input
	return []githubinfra.IssueSummary{{Number: f.detail.Number, Title: f.detail.Title, Body: f.detail.Body, URL: f.detail.URL, State: f.detail.State, UpdatedAt: f.detail.UpdatedAt, Labels: f.detail.Labels}}, nil
}

func (f *fakeGitHub) ViewIssue(context.Context, githubinfra.ViewIssueInput) (githubinfra.IssueDetail, error) {
	return f.detail, nil
}

func (f *fakeGitHub) ListIssueTimeline(context.Context, githubinfra.IssueTimelineInput) ([]map[string]any, error) {
	return f.timeline, nil
}

func (f *fakeGitHub) GetRepositoryPermission(context.Context, githubinfra.RepositoryPermissionInput) (string, error) {
	return f.permission, nil
}

type fakeLLM struct {
	responses []string
	calls     int
}

func (f *fakeLLM) Complete(context.Context, Request) (string, error) {
	response := f.responses[f.calls]
	f.calls++
	return response, nil
}

type fakePlanner struct {
	inputs []planner.RouteIssueInput
}

func (f *fakePlanner) RouteIssue(_ context.Context, input planner.RouteIssueInput) (planner.DiscoveryResult, error) {
	f.inputs = append(f.inputs, input)
	return planner.DiscoveryResult{}, nil
}

func eligibleDecisionJSON() string {
	return `{"classification":"feature","scope":"in_scope","risk":"low","confidence":0.94,"missingInformation":[],"recommendedNextRole":"planner","rationale":"The request is bounded and ready for planning."}`
}
