package reproducer

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/triager"
)

// Bug-only. Feature work is already served by Planner's spec and acceptance
// criteria; reproducing it here would only rename Planner.
func TestNonBugClassificationsAreNotReproducedAndDoNotBlockPlanner(t *testing.T) {
	t.Parallel()
	for _, classification := range []triager.Classification{
		triager.ClassificationFeature, triager.ClassificationDocs,
		triager.ClassificationRefactor, triager.ClassificationChore,
	} {
		fixture := newFixture(t)
		report := fixture.seedTriageReport(classification)

		result := fixture.discover()
		if result.Attempted != 0 || result.Reproduced != 0 || result.Unreproducible != 0 {
			t.Fatalf("DiscoverIssues(%s) = %#v, want no reproduction work", classification, result)
		}
		if fixture.agent.starts != 0 || fixture.git.createCalls != 0 {
			t.Fatalf("%s started an agent (%d) or a worktree (%d)", classification, fixture.agent.starts, fixture.git.createCalls)
		}
		allowed, err := NewGate(fixture.repos).PlannerAllowed(context.Background(), report)
		if err != nil {
			t.Fatalf("PlannerAllowed() error = %v", err)
		}
		if !allowed {
			t.Fatalf("PlannerAllowed(%s) = false, want non-bug work on today's path", classification)
		}
	}
}

// Planner can also be reached by label/assignee discovery with no Triage Report
// at all. There is then no classification to gate on, so there is no Reproducer
// work and the existing path is unchanged.
func TestIssuesWithNoTriageReportAreOutOfScope(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	fixture.writeAgentDraft("go test ./pkg -run TestCrash", map[string]string{"pkg/crash_test.go": "reproduction"})

	result := fixture.discover()
	if result.Attempted != 0 || result.Reproduced != 0 || result.Unreproducible != 0 || result.Skipped != 0 {
		t.Fatalf("DiscoverIssues() = %#v, want nothing in scope without a Triage Report", result)
	}
	if fixture.agent.starts != 0 || fixture.github.calls != 0 {
		t.Fatalf("agent starts / issue lookups = %d / %d, want no work", fixture.agent.starts, fixture.github.calls)
	}
	if fixture.status().Settled() {
		t.Fatalf("status = %#v, want no reproduction state for an unreported Issue", fixture.status())
	}
}

// The gate is what makes ordering real: a bug with no reproduction yet must not
// reach Planner, and the same report must pass once the record exists.
func TestGateBlocksBugsUntilAReproductionExists(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	report := fixture.seedTriageReport(triager.ClassificationBug)
	gate := NewGate(fixture.repos)

	allowed, err := gate.PlannerAllowed(context.Background(), report)
	if err != nil {
		t.Fatalf("PlannerAllowed() error = %v", err)
	}
	if allowed {
		t.Fatalf("PlannerAllowed() = true before a reproduction, want Planner withheld")
	}

	fixture.writeAgentDraft("go test ./pkg -run TestCrash", map[string]string{"pkg/crash_test.go": "reproduction"})
	fixture.discover()

	allowed, err = gate.PlannerAllowed(context.Background(), report)
	if err != nil {
		t.Fatalf("PlannerAllowed() error = %v", err)
	}
	if !allowed {
		t.Fatalf("PlannerAllowed() = false after a reproduction, want Planner released")
	}
}
