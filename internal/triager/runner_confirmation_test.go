package triager

import (
	"context"
	"strings"
	"testing"
	"time"

	githubinfra "github.com/nexu-io/looper/internal/infra/github"
)

func TestDiscoverIssuesSkipsHistoricalOpenBacklog(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.github.detail.CreatedAt = fixture.now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	fixture.github.detail.UpdatedAt = fixture.github.detail.CreatedAt

	result, err := fixture.runner().DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if result.Skipped != 1 || fixture.llm.calls != 0 {
		t.Fatalf("DiscoverIssues() = %#v LLM calls=%d, want historical issue skipped", result, fixture.llm.calls)
	}
	if !strings.Contains(fixture.github.listInput.Search, "sort:updated-desc") {
		t.Fatalf("source search = %q, want updated-event ordering", fixture.github.listInput.Search)
	}
}

func TestDiscoverIssuesRoutesConfirmedUnsafeReportWithoutRepeatingLLM(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.llm.responses = []string{`{"classification":"feature","scope":"in_scope","risk":"high","confidence":0.98,"missingInformation":[],"recommendedNextRole":"planner","rationale":"Touches credentials."}`}
	runner := fixture.runner()

	first, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("first DiscoverIssues() error = %v", err)
	}
	if first.AwaitingConfirmation != 1 {
		t.Fatalf("first DiscoverIssues() = %#v, want confirmation wait", first)
	}
	fixture.now = fixture.now.Add(10 * time.Minute)
	fixture.github.detail.UpdatedAt = fixture.now.Format(time.RFC3339Nano)
	fixture.github.detail.Comments = []githubinfra.CommentInfo{{
		ID: 77, Author: "maintainer", Body: "/plan", CreatedAt: fixture.now.Format(time.RFC3339Nano),
	}}
	fixture.github.permission = "write"

	second, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("confirmed DiscoverIssues() error = %v", err)
	}
	if second.Confirmed != 1 || second.Routed != 1 || second.AwaitingConfirmation != 0 {
		t.Fatalf("confirmed DiscoverIssues() = %#v", second)
	}
	if fixture.llm.calls != 1 || len(fixture.planner.inputs) != 1 {
		t.Fatalf("LLM/planner calls = %d/%d, want persisted report replay", fixture.llm.calls, len(fixture.planner.inputs))
	}
	events, err := fixture.repos.Events.List(context.Background(), 10)
	if err != nil {
		t.Fatalf("Events.List() error = %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("events = %d, want enrollment, report, confirmation, and projection", len(events))
	}
}

func TestDiscoverIssuesAcceptsNewConfirmationInReportTimestampSecond(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.now = fixture.now.Add(500 * time.Millisecond)
	old := githubinfra.CommentInfo{ID: 76, Author: "maintainer", Body: "/plan", CreatedAt: fixture.now.Add(-time.Second).Format(time.RFC3339)}
	fixture.github.detail.Comments = []githubinfra.CommentInfo{old}
	fixture.llm.responses = []string{`{"classification":"feature","scope":"in_scope","risk":"high","confidence":0.98,"missingInformation":[],"recommendedNextRole":"planner","rationale":"Touches credentials."}`}
	runner := fixture.runner()

	first, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil || first.AwaitingConfirmation != 1 {
		t.Fatalf("first DiscoverIssues() = (%#v, %v)", first, err)
	}
	fixture.github.listEmpty = true
	fixture.github.detail.Comments = append(fixture.github.detail.Comments, githubinfra.CommentInfo{
		ID: 77, Author: "maintainer", Body: "/plan", CreatedAt: fixture.now.Truncate(time.Second).Format(time.RFC3339),
	})
	fixture.github.permission = "write"
	second, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("second DiscoverIssues() error = %v", err)
	}
	if second.Confirmed != 1 || second.Routed != 1 {
		t.Fatalf("second DiscoverIssues() = %#v", second)
	}
}
