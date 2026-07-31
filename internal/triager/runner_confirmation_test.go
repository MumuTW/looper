package triager

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	githubinfra "github.com/MumuTW/looper/internal/infra/github"
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
	report := fixture.singleReport(t)
	if report.ConfirmationToken == "" {
		t.Fatal("ConfirmationToken is empty, want a report-specific token")
	}
	fixture.now = fixture.now.Add(10 * time.Minute)
	fixture.github.detail.UpdatedAt = fixture.now.Format(time.RFC3339Nano)
	fixture.github.detail.Comments = []githubinfra.CommentInfo{{
		ID: 77, Author: "maintainer", Body: confirmationCommand(report.ConfirmationToken), CreatedAt: fixture.now.Format(time.RFC3339Nano),
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
	if len(events) != 5 {
		t.Fatalf("events = %d, want enrollment, report, ask, confirmation, and projection", len(events))
	}
}

func TestDiscoverIssuesRequiresReportSpecificConfirmationToken(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.llm.responses = []string{`{"classification":"feature","scope":"in_scope","risk":"high","confidence":0.98,"missingInformation":[],"recommendedNextRole":"planner","rationale":"Touches credentials."}`}
	runner := fixture.runner()

	first, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil || first.AwaitingConfirmation != 1 {
		t.Fatalf("first DiscoverIssues() = (%#v, %v)", first, err)
	}
	report := fixture.singleReport(t)
	if report.Version != 2 || report.ConfirmationToken == "" {
		t.Fatalf("report version/token = %d/%q, want v2 report-specific token", report.Version, report.ConfirmationToken)
	}
	fixture.github.listEmpty = true
	fixture.github.detail.Comments = []githubinfra.CommentInfo{{
		ID: 77, Author: "maintainer", Body: "/plan", CreatedAt: fixture.now.Format(time.RFC3339Nano),
	}}
	fixture.github.permission = "write"
	second, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("second DiscoverIssues() error = %v", err)
	}
	if second.AwaitingConfirmation != 1 || second.Confirmed != 0 || second.Routed != 0 {
		t.Fatalf("second DiscoverIssues() = %#v, want plain /plan rejected", second)
	}
	fixture.github.detail.Comments = append(fixture.github.detail.Comments, githubinfra.CommentInfo{
		ID: 78, Author: "maintainer", Body: confirmationCommand(report.ConfirmationToken), CreatedAt: fixture.now.Format(time.RFC3339Nano),
	})
	third, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("third DiscoverIssues() error = %v", err)
	}
	if third.Confirmed != 1 || third.Routed != 1 {
		t.Fatalf("third DiscoverIssues() = %#v", third)
	}
}

func TestDiscoverIssuesDoesNotUsePlanPostedBeforeReportTokenToConfirmLaterReport(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.llm.responses = []string{`{"classification":"feature","scope":"in_scope","risk":"high","confidence":0.98,"missingInformation":[],"recommendedNextRole":"planner","rationale":"Touches credentials."}`}
	fixture.github.permission = "write"
	fixture.llm.onComplete = func() {
		fixture.github.detail.Comments = []githubinfra.CommentInfo{{
			ID: 77, Author: "maintainer", Body: "/plan", CreatedAt: fixture.now.Format(time.RFC3339Nano),
		}}
		fixture.github.onTimeline = func() {
			fixture.github.detail.Comments = append(fixture.github.detail.Comments, githubinfra.CommentInfo{
				ID: 78, Author: "maintainer", Body: "/plan", CreatedAt: fixture.now.Format(time.RFC3339Nano),
			})
		}
	}
	runner := fixture.runner()

	first, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("first DiscoverIssues() error = %v", err)
	}
	if first.AwaitingConfirmation != 1 || first.Confirmed != 0 || first.Routed != 0 {
		t.Fatalf("first DiscoverIssues() = %#v, want the mid-decision /plan excluded", first)
	}
	report := fixture.singleReport(t)
	if report.ConfirmationToken == "" {
		t.Fatal("ConfirmationToken is empty, want a report-specific token")
	}

	fixture.github.listEmpty = true
	fixture.now = fixture.now.Add(time.Minute)
	fixture.github.detail.Comments = append(fixture.github.detail.Comments, githubinfra.CommentInfo{
		ID: 79, Author: "maintainer", Body: "/plan", CreatedAt: fixture.now.Format(time.RFC3339Nano),
	})
	second, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("second DiscoverIssues() error = %v", err)
	}
	if second.AwaitingConfirmation != 1 || second.Confirmed != 0 || second.Routed != 0 || len(fixture.planner.inputs) != 0 {
		t.Fatalf("second DiscoverIssues() = %#v planner=%d, want plain /plan rejected", second, len(fixture.planner.inputs))
	}

	fixture.github.detail.Comments = append(fixture.github.detail.Comments, githubinfra.CommentInfo{
		ID: 80, Author: "maintainer", Body: confirmationCommand(report.ConfirmationToken), CreatedAt: fixture.now.Format(time.RFC3339Nano),
	})
	third, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("third DiscoverIssues() error = %v", err)
	}
	if third.Confirmed != 1 || third.Routed != 1 || len(fixture.planner.inputs) != 1 {
		t.Fatalf("third DiscoverIssues() = %#v planner=%d, want token-bound confirmation routed once", third, len(fixture.planner.inputs))
	}
	events, err := fixture.repos.Events.List(context.Background(), 10)
	if err != nil {
		t.Fatalf("Events.List() error = %v", err)
	}
	var confirmed Confirmation
	for _, event := range events {
		if event.EventType == ConfirmationEventType {
			if err := json.Unmarshal([]byte(event.PayloadJSON), &confirmed); err != nil {
				t.Fatalf("decode confirmation: %v", err)
			}
		}
	}
	if confirmed.CommentID != 80 {
		t.Fatalf("confirmation comment = %d, want only token-bound comment 80", confirmed.CommentID)
	}

	fourth, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("fourth DiscoverIssues() error = %v", err)
	}
	if fourth.Confirmed != 0 || fourth.Routed != 0 || fixture.llm.calls != 1 || len(fixture.planner.inputs) != 1 {
		t.Fatalf("fourth replay = %#v llm/planner=%d/%d, want idempotent no-op", fourth, fixture.llm.calls, len(fixture.planner.inputs))
	}
}
