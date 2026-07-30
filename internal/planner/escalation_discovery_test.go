package planner

import (
	"context"
	"testing"
)

// An escalated Issue must survive discovery ticks untouched: neither the
// label/assignee discovery lane nor a replayed triage route may re-plan it while
// a human still owes an answer.
func TestEscalatedLoopIsNotRePlannedByDiscoveryTick(t *testing.T) {
	t.Parallel()
	harness := newEscalationHarness(t, true, AgentResult{Status: "completed", Stdout: escalatingAssessmentJSON})
	if processed := harness.process(t); processed.Status != "awaiting_human" {
		t.Fatalf("processed = %#v, want awaiting_human", processed)
	}

	harness.github.issues = []IssueSummary{{Number: 114, Title: "Planner needs-human exit", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}
	harness.github.login = "octocat"
	discovered, err := harness.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	// Skipped == 1 proves the issue reached the loop-state check rather than
	// the tick being inert (auto-discovery off, label mismatch, wrong assignee).
	if len(discovered.QueueItems) != 0 || len(discovered.CreatedLoopIDs) != 0 || discovered.Skipped != 1 {
		t.Fatalf("discovery = %#v, want the awaiting_human loop seen and skipped", discovered)
	}

	replayed, err := harness.runner.RouteIssue(context.Background(), RouteIssueInput{
		ProjectID: "project_1", Repo: "acme/looper", Authority: "triage:report-114",
		Issue: IssueSummary{Number: 114, Title: "Planner needs-human exit"},
	})
	if err != nil {
		t.Fatalf("RouteIssue() replay error = %v", err)
	}
	if len(replayed.QueueItems) != 0 || len(replayed.CreatedLoopIDs) != 0 {
		t.Fatalf("replayed route = %#v, want no new work for an escalated issue", replayed)
	}

	loop := harness.loop(t)
	if loop.Status != "awaiting_human" || loop.NextRunAt != nil {
		t.Fatalf("loop = (status %s, nextRunAt %v) after a discovery tick, want an unchanged waiting state", loop.Status, loop.NextRunAt)
	}
	items, err := harness.fixture.repos.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	for _, item := range items {
		if item.LoopID != nil && *item.LoopID == harness.loopID && (item.Status == "queued" || item.Status == "running") {
			t.Fatalf("discovery re-queued the escalated loop: %#v", item)
		}
	}
	if len(harness.executor.starts) != 1 {
		t.Fatalf("agent turns = %d after a discovery tick, want the escalated loop untouched", len(harness.executor.starts))
	}
}
