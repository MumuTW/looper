package planner

import (
	"context"
	"strings"
	"testing"
)

// stubReproductionGate withholds exactly the Issues it was told to.
type stubReproductionGate struct {
	withheld map[int64]bool
	asked    []int64
}

func (s *stubReproductionGate) IssueAllowed(_ context.Context, _, _ string, issueNumber int64) (bool, error) {
	s.asked = append(s.asked, issueNumber)
	return !s.withheld[issueNumber], nil
}

// Gating only Triager's explicit route left the other door open: an Issue that
// also carries Planner's labels is discovered directly, so a bug whose
// reproduction has not settled — an exhausted per-tick decision budget is
// enough — was planned anyway. The guarantee cannot hold only while nothing
// else is busy.
func TestDiscoveryWithholdsAnIssueWhoseReproductionHasNotSettled(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{
		{Number: 42, Title: "Awaiting reproduction", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}},
		{Number: 44, Title: "No triage report", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}},
	}}
	gate := &stubReproductionGate{withheld: map[int64]bool{42: true}}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{},
		AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now,
		AllowAutoPush: boolPtr(true), ReproductionGate: gate,
	})

	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	// The ungated Issue still goes through untouched: an Issue with no governing
	// report keeps today's path, which is what makes the gate inert for every
	// repository that does not run the Role.
	if len(result.QueueItems) != 1 || len(result.CreatedLoopIDs) != 1 {
		t.Fatalf("result = %#v, want only the ungated Issue queued", result)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.CreatedLoopIDs[0])
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %v, %v", loop, err)
	}
	if loop.TargetID == nil || !strings.HasSuffix(*loop.TargetID, "44") {
		t.Fatalf("loop = %#v, want the loop created for the ungated Issue only", loop)
	}
	if len(gate.asked) != 2 {
		t.Fatalf("gate asked = %#v, want every claimable Issue checked", gate.asked)
	}
}

// A nil gate is the pre-Reproducer path, which is what every project with the
// Role disabled gets.
func TestDiscoveryIsUnchangedWithoutAReproductionGate(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{},
		AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true),
	})

	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("result = %#v, want unchanged discovery with no gate configured", result)
	}
}
