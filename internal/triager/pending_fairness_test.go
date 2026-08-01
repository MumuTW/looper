package triager

import (
	"context"
	"fmt"
	"testing"
	"time"

	githubinfra "github.com/MumuTW/looper/internal/infra/github"
)

// A bounded quiet-source window must advance based on work actually performed.
// Re-selecting the oldest prefix on every tick permanently starves every source
// after that prefix while confidently reporting a bounded scan.
func TestDiscoverIssuesAdvancesQuietAwaitingRechecksAcrossTicks(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.github.listEmpty = true
	fixture.github.details = map[int64]githubinfra.IssueDetail{}
	runner := fixture.runner()

	for index := 0; index < 5; index++ {
		number := int64(100 + index)
		createdAt := fixture.now.Add(-time.Hour).Add(time.Duration(index) * time.Second).Format(time.RFC3339Nano)
		detail := githubinfra.IssueDetail{
			Number:    number,
			Title:     fmt.Sprintf("Awaiting issue %d", number),
			State:     "open",
			URL:       fmt.Sprintf("https://github.com/acme/looper/issues/%d", number),
			CreatedAt: createdAt,
		}
		fixture.github.details[number] = detail
		source := SourceEvent{Kind: sourceEventNew, OccurredAt: createdAt}
		report := Report{
			Version:           2,
			IdempotencyKey:    buildIdempotencyKey("project_1", "acme/looper", number, source),
			ProjectID:         "project_1",
			Repo:              "acme/looper",
			IssueNumber:       number,
			Source:            source,
			Policy:            PolicyDecision{Action: ActionAwaitHuman},
			ConfirmationToken: fmt.Sprintf("triage-confirm-%032d", number),
			CreatedAt:         createdAt,
		}
		if err := runner.persistReport(context.Background(), report); err != nil {
			t.Fatalf("persistReport(%d) error = %v", number, err)
		}
	}

	for tick := 0; tick < 2; tick++ {
		if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
			t.Fatalf("DiscoverIssues(tick %d) error = %v", tick+1, err)
		}
	}

	seen := map[int64]struct{}{}
	for _, number := range fixture.github.viewRequests {
		seen[number] = struct{}{}
	}
	if len(fixture.github.viewRequests) != 2*awaitingRecheckBudget {
		t.Fatalf("ViewIssue requests = %v, want exactly %d bounded rechecks", fixture.github.viewRequests, 2*awaitingRecheckBudget)
	}
	if len(seen) != 5 {
		t.Fatalf("ViewIssue requests = %v, reached %d/5 sources; quiet backlog did not advance", fixture.github.viewRequests, len(seen))
	}
}
