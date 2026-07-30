package reproducer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/reproduction"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/triager"
)

// Cannot-reproduce is a decision, not a failure: it must stop the Issue before
// Planner, park it for a human, and record what was tried — not retry, not
// increment an attempt counter, not read as a crash.
func TestCannotReproduceParksForAHumanInsteadOfRoutingToPlanner(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	fixture.seedTriageReport(triager.ClassificationBug)
	fixture.writeCannotReproduce(CannotReproduce{
		Attempted:          []string{"ran the reported command", "wrote a table test over the boundary"},
		ObservedInstead:    "the function returned the expected value",
		MissingInformation: []string{"the exact input that panics"},
		Summary:            "The report does not contain an input that fails.",
	})

	result := fixture.discover()
	if result.Unreproducible != 1 || result.Reproduced != 0 {
		t.Fatalf("DiscoverIssues() = %#v, want a cannot-reproduce decision", result)
	}
	if len(fixture.git.commits) != 0 {
		t.Fatalf("commits = %#v, want nothing committed", fixture.git.commits)
	}
	if len(fixture.planner.parked) != 1 {
		t.Fatalf("parked = %#v, want the Issue parked for a human exactly once", fixture.planner.parked)
	}
	ask := fixture.planner.parked[0].Ask
	for _, want := range []string{"Cannot reproduce acme/looper#41", "ran the reported command", "the function returned the expected value", "the exact input that panics"} {
		if !strings.Contains(ask.Question, want) {
			t.Fatalf("ask.Question = %q, want it to contain %q", ask.Question, want)
		}
	}
	if len(ask.Options) != 2 || ask.RecommendedOption != AnswerReject {
		t.Fatalf("ask = %#v, want two options with a recommendation to stop", ask)
	}

	status := fixture.status()
	if status.Unreproducible == nil || status.PlannerAllowed() {
		t.Fatalf("status = %#v, want a recorded cannot-reproduce that blocks Planner", status)
	}
	if status.Unreproducible.ObservedInstead != "the function returned the expected value" {
		t.Fatalf("record = %#v, want the observed behaviour preserved", status.Unreproducible)
	}
	if len(status.Unreproducible.MissingInformation) != 1 {
		t.Fatalf("record missing information = %#v, want it preserved", status.Unreproducible.MissingInformation)
	}
}

func TestCannotReproduceIsNotRetriedOnTheNextTick(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	fixture.seedTriageReport(triager.ClassificationBug)
	fixture.writeCannotReproduce(CannotReproduce{ObservedInstead: "no failure"})

	fixture.discover()
	second := fixture.discover()
	if second.Unreproducible != 0 || second.Reproduced != 0 {
		t.Fatalf("second DiscoverIssues() = %#v, want the decision to stand", second)
	}
	if fixture.agent.starts != 1 {
		t.Fatalf("agent starts = %d, want no re-run of a settled decision", fixture.agent.starts)
	}
	if len(fixture.planner.parked) != 1 {
		t.Fatalf("parked = %d, want a single park", len(fixture.planner.parked))
	}
}

// A human resuming the parked loop is the only way an unreproducible Issue
// proceeds. Recording the waiver keeps the state machine closed rather than
// leaving the Issue blocked forever.
func TestHumanResumingTheParkedLoopWaivesTheReproduction(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	fixture.seedTriageReport(triager.ClassificationBug)
	fixture.writeCannotReproduce(CannotReproduce{ObservedInstead: "no failure"})

	stamp := fixture.now.Format(time.RFC3339Nano)
	answered, err := loops.WriteHITLAsk(nil, loops.HITLAsk{Status: "answered", Answer: AnswerProceed})
	if err != nil {
		t.Fatalf("WriteHITLAsk() error = %v", err)
	}
	fixture.planner.loop = storage.LoopRecord{
		ID: "loop_parked", ProjectID: testProjectID, Type: "planner", TargetType: "issue",
		Status: "queued", MetadataJSON: &answered, CreatedAt: stamp, UpdatedAt: stamp,
	}
	if err := fixture.repos.Loops.Upsert(context.Background(), fixture.planner.loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	fixture.discover()
	second := fixture.discover()
	if second.Waived != 1 {
		t.Fatalf("second DiscoverIssues() = %#v, want the human's authorization recorded", second)
	}
	status := fixture.status()
	if status.Waived == nil || !status.PlannerAllowed() {
		t.Fatalf("status = %#v, want Planner unblocked by the waiver", status)
	}

	third := fixture.discover()
	if third.Waived != 0 {
		t.Fatalf("third DiscoverIssues() = %#v, want the waiver recorded exactly once", third)
	}
}

func TestUndecodableAgentOutputIsRecordedAsCannotReproduce(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	fixture.seedTriageReport(triager.ClassificationBug)
	fixture.writeWorktreeJSON(reproduction.ManifestRelPath, map[string]any{"version": 1, "command": "go test ./..."})

	result := fixture.discover()
	if result.Unreproducible != 1 {
		t.Fatalf("DiscoverIssues() = %#v, want an unusable manifest treated as cannot-reproduce", result)
	}
	status := fixture.status()
	if status.Unreproducible == nil || !strings.Contains(status.Unreproducible.Summary, "neither a reproduction manifest nor a cannot-reproduce record") {
		t.Fatalf("record = %#v, want a specific summary", status.Unreproducible)
	}
}
