package triager

import (
	"context"
	"strings"
	"testing"
	"time"

	githubinfra "github.com/nexu-io/looper/internal/infra/github"
)

const underspecifiedDecision = `{"classification":"bug","scope":"in_scope","risk":"low","confidence":0.95,"missingInformation":["Which page shows the bug?","What did you expect to happen?"],"recommendedNextRole":"planner","rationale":"Report lacks reproduction detail."}`

// The confirmation token is minted locally and never leaves the daemon by any
// other route, so the ask comment is the only thing that makes a held report
// answerable by a human reading the Issue.
func TestHeldReportAsksOnTheIssueAndDeliversTheConfirmationToken(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.llm.responses = []string{underspecifiedDecision}

	result, err := fixture.runner().DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if result.AwaitingConfirmation != 1 {
		t.Fatalf("DiscoverIssues() = %#v, want an awaiting report", result)
	}
	if len(fixture.github.comments) != 1 {
		t.Fatalf("comments = %d, want the question posted on the issue", len(fixture.github.comments))
	}
	report := fixture.singleReport(t)
	body := fixture.github.comments[0].Body
	for _, want := range []string{
		askCommentMarker,
		"Which page shows the bug?",
		"What did you expect to happen?",
		confirmationCommand(report.ConfirmationToken),
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("ask comment missing %q:\n%s", want, body)
		}
	}
}

// The question must be asked once, not once per scheduler tick, or a report that
// waits a day becomes a day of comment spam.
func TestHeldReportAsksExactlyOnceAcrossTicks(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.llm.responses = []string{underspecifiedDecision}
	runner := fixture.runner()

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("first DiscoverIssues() error = %v", err)
	}
	fixture.now = fixture.now.Add(time.Hour)
	fixture.github.detail.UpdatedAt = fixture.now.Format(time.RFC3339Nano)
	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("second DiscoverIssues() error = %v", err)
	}

	if len(fixture.github.comments) != 1 {
		t.Fatalf("comments = %d, want exactly one ask", len(fixture.github.comments))
	}
}

func TestClarificationFromConfirmCommandReachesPlanner(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.llm.responses = []string{underspecifiedDecision}
	runner := fixture.runner()

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("first DiscoverIssues() error = %v", err)
	}
	report := fixture.singleReport(t)
	fixture.now = fixture.now.Add(10 * time.Minute)
	fixture.github.detail.UpdatedAt = fixture.now.Format(time.RFC3339Nano)
	fixture.github.detail.Comments = []githubinfra.CommentInfo{{
		ID:     77,
		Author: "maintainer",
		Body:   confirmationCommand(report.ConfirmationToken) + " 設定頁,預期是存檔後留在原頁",
		//nolint:lll // the command is the point of the fixture
		CreatedAt: fixture.now.Format(time.RFC3339Nano),
	}}
	fixture.github.permission = "write"

	second, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("second DiscoverIssues() error = %v", err)
	}
	if second.Routed != 1 {
		t.Fatalf("second DiscoverIssues() = %#v, want routed", second)
	}
	if len(fixture.planner.inputs) != 1 {
		t.Fatalf("planner inputs = %d, want 1", len(fixture.planner.inputs))
	}
	clarifications := fixture.planner.inputs[0].Issue.Clarifications
	if len(clarifications) != 1 || !strings.Contains(clarifications[0], "設定頁") {
		t.Fatalf("planner clarifications = %#v, want the answer carried through", clarifications)
	}
}

func TestBareConfirmCommandRoutesWithoutClarification(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.llm.responses = []string{underspecifiedDecision}
	runner := fixture.runner()

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("first DiscoverIssues() error = %v", err)
	}
	report := fixture.singleReport(t)
	fixture.now = fixture.now.Add(10 * time.Minute)
	fixture.github.detail.UpdatedAt = fixture.now.Format(time.RFC3339Nano)
	fixture.github.detail.Comments = []githubinfra.CommentInfo{{
		ID: 78, Author: "maintainer", Body: confirmationCommand(report.ConfirmationToken),
		CreatedAt: fixture.now.Format(time.RFC3339Nano),
	}}
	fixture.github.permission = "write"

	second, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("second DiscoverIssues() error = %v", err)
	}
	if second.Routed != 1 {
		t.Fatalf("second DiscoverIssues() = %#v, want routed", second)
	}
	if got := fixture.planner.inputs[0].Issue.Clarifications; len(got) != 0 {
		t.Fatalf("clarifications = %#v, want none for a bare command", got)
	}
}

func TestParseConfirmComment(t *testing.T) {
	t.Parallel()
	const token = "triage-confirm-abc123"
	command := confirmationCommand(token)
	cases := []struct {
		name          string
		body          string
		token         string
		confirms      bool
		clarification string
	}{
		{name: "bare command", body: command, token: token, confirms: true},
		{name: "command with answer", body: command + " it is the settings page", token: token, confirms: true, clarification: "it is the settings page"},
		{name: "command with answer on later lines", body: command + "\nthe settings page", token: token, confirms: true, clarification: "the settings page"},
		{name: "surrounding whitespace", body: "  " + command + "  \n", token: token, confirms: true},
		{name: "unrelated comment", body: "nice catch", token: token},
		// The ask comment tells people to reply with the command. Quoting that
		// instruction mid-discussion must not authorize anything.
		{name: "command quoted mid-sentence", body: "you should reply " + command + " here", token: token},
		{name: "command not on the first line", body: "some context\n" + command, token: token},
		{name: "another report's token", body: confirmationCommand("triage-confirm-other"), token: token},
		{name: "bare /plan without a token", body: "/plan", token: token},
		{name: "empty token", body: command, token: ""},
		{name: "empty body", body: "   ", token: token},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			confirms, clarification := parseConfirmComment(tc.body, tc.token)
			if confirms != tc.confirms || clarification != tc.clarification {
				t.Fatalf("parseConfirmComment(%q, %q) = (%v, %q), want (%v, %q)", tc.body, tc.token, confirms, clarification, tc.confirms, tc.clarification)
			}
		})
	}
}
