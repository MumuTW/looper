package triager

import (
	"context"
	"testing"
	"time"
)

type stubReproductionGate struct {
	allow  bool
	calls  int
	report Report
}

func (s *stubReproductionGate) PlannerAllowed(_ context.Context, report Report) (bool, error) {
	s.calls++
	s.report = report
	return s.allow, nil
}

// The gate must hold the projection open rather than retire the enrollment, so
// the same accepted report routes unchanged on the tick after the reproduction
// lands. Retiring it would lose the Issue.
func TestReproductionGateHoldsRoutingAndReplaysAfterItOpens(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.llm.responses = []string{eligibleDecisionJSON()}
	gate := &stubReproductionGate{}
	runner := New(Options{
		Repos: fixture.repos, GitHub: fixture.github, LLM: fixture.llm, Planner: fixture.planner,
		Now: func() time.Time { return fixture.now }, ReproductionGate: gate,
	})

	held, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if held.AwaitingReproduction != 1 || held.Routed != 0 || held.Retired != 0 {
		t.Fatalf("DiscoverIssues() = %#v, want the projection held, not routed or retired", held)
	}
	if len(fixture.planner.inputs) != 0 {
		t.Fatalf("planner routes = %#v, want Planner untouched while the gate is closed", fixture.planner.inputs)
	}
	if gate.calls != 1 || gate.report.IdempotencyKey == "" {
		t.Fatalf("gate calls = %d, report = %#v, want the persisted report handed to the gate", gate.calls, gate.report)
	}

	gate.allow = true
	released, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("second DiscoverIssues() error = %v", err)
	}
	if released.Routed != 1 || released.AwaitingReproduction != 0 {
		t.Fatalf("second DiscoverIssues() = %#v, want the held report routed once the gate opens", released)
	}
	if fixture.llm.calls != 1 {
		t.Fatalf("LLM calls = %d, want the persisted report reused rather than re-derived", fixture.llm.calls)
	}
}

// A nil gate is the pre-Reproducer path, which is what every project with the
// Role disabled gets.
func TestNilReproductionGateRoutesExactlyAsBefore(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.llm.responses = []string{eligibleDecisionJSON()}

	result, err := fixture.runner().DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if result.Routed != 1 || result.AwaitingReproduction != 0 {
		t.Fatalf("DiscoverIssues() = %#v, want unchanged routing", result)
	}
}

func TestLoadAcceptedReportsExposesOnlyAuthorizedReports(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.llm.responses = []string{`{"classification":"bug","scope":"in_scope","risk":"high","confidence":0.94,"missingInformation":[],"recommendedNextRole":"planner","rationale":"Risky."}`}

	if _, err := fixture.runner().DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	reports, err := LoadAcceptedReports(context.Background(), fixture.repos, "project_1", "acme/looper")
	if err != nil {
		t.Fatalf("LoadAcceptedReports() error = %v", err)
	}
	if len(reports) != 0 {
		t.Fatalf("LoadAcceptedReports() = %#v, want a policy-held report excluded until confirmed", reports)
	}
}
