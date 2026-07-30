package triager

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestDiscoverIssuesBoundsPendingForgeReadsForLargeAwaitingBacklog(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.github.listEmpty = true
	runner := fixture.runner()
	const pendingBacklog = 512
	for i := 0; i < pendingBacklog; i++ {
		report := Report{Version: 2, IdempotencyKey: fmt.Sprintf("awaiting-%d", i), ProjectID: "project_1", Repo: "acme/looper", IssueNumber: int64(100 + i), Policy: PolicyDecision{Action: ActionAwaitHuman}, CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339Nano)}
		if err := runner.persistReport(context.Background(), report); err != nil {
			t.Fatal(err)
		}
	}
	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper", PendingReadBudget: NewReadBudget(4)})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if fixture.github.viewRequests != 1 || fixture.github.timelineCalls != 1 || result.PendingReadSkipped != 2 {
		t.Fatalf("views/timelines/skipped = %d/%d/%d, want 1/1/2", fixture.github.viewRequests, fixture.github.timelineCalls, result.PendingReadSkipped)
	}
}

func TestDiscoverIssuesReservesWorstCasePendingForgeReads(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.github.listEmpty = true
	fixture.llm.responses = []string{eligibleDecisionJSON()}
	runner := fixture.runner()
	source := SourceEvent{Kind: sourceEventNew, OccurredAt: fixture.github.detail.CreatedAt}
	enrollment := Enrollment{
		Version: 1, IdempotencyKey: buildIdempotencyKey("project_1", "acme/looper", fixture.github.detail.Number, source), ProjectID: "project_1", Repo: "acme/looper",
		IssueNumber: fixture.github.detail.Number, Source: source,
		EnrolledAt: fixture.now.Add(-time.Hour).Format(time.RFC3339Nano),
	}
	if err := runner.persistEnrollment(context.Background(), enrollment); err != nil {
		t.Fatal(err)
	}

	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper", PendingReadBudget: NewReadBudget(4)})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if fixture.github.viewRequests != 2 || fixture.github.timelineCalls != 2 || result.PendingReadSkipped != 0 {
		t.Fatalf("views/timelines/skipped = %d/%d/%d, want 2/2/0", fixture.github.viewRequests, fixture.github.timelineCalls, result.PendingReadSkipped)
	}
}
