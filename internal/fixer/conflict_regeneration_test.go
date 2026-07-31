package fixer

import (
	"context"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/labels"
)

func TestRegenerateConflictOrdersCommentCloseAndPlannerRoute(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{
			currentUser:   "looper",
			viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", BaseRefName: "main", HeadSHA: "head-42"}},
		},
		issue:   IssueDetail{Number: 7, Title: "Original issue", Body: "please implement", URL: "https://example.test/issues/7", State: "OPEN"},
		commits: []PullRequestCommit{{SHA: "head-42", AuthorLogin: "looper", CommitterLogin: "looper"}},
	}
	routes := 0
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, input RegenerateIssueInput) error {
		routes++
		if input.Authority != "coordinator-conflict:acme/looper#42" {
			t.Fatalf("route authority = %q", input.Authority)
		}
		return nil
	})
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	result, err := runner.RegenerateConflict(context.Background(), ConflictRegenerationInput{
		ProjectID: "project_1", Repo: "acme/looper", IssueRepo: "acme/looper", IssueNumber: 7, PRNumber: 42, ConflictRepairs: 2, CWD: project.RepoPath,
	})
	if err != nil {
		t.Fatalf("RegenerateConflict() error = %v", err)
	}
	if !result.Completed || result.Escalated || routes != 1 {
		t.Fatalf("result/routes = %#v/%d, want completed/one route", result, routes)
	}
	if len(gateway.closeCalls) != 1 || !gateway.closeCalls[0].DeleteBranch {
		t.Fatalf("close calls = %#v, want one Looper branch deletion", gateway.closeCalls)
	}
	if len(gateway.fakeGitHubGateway.createIssueComments) != 2 {
		t.Fatalf("comments = %d, want pending and completed markers", len(gateway.fakeGitHubGateway.createIssueComments))
	}
	if len(gateway.issueLabelCalls) != 1 || gateway.issueLabelCalls[0].Labels[0] != labels.DefaultPlanTrigger {
		t.Fatalf("issue labels = %#v, want planner trigger", gateway.issueLabelCalls)
	}
	if len(gateway.removeIssueLabelCalls) != 1 || gateway.removeIssueLabelCalls[0].Labels[0] != labels.DefaultWorkerReadyTrigger {
		t.Fatalf("removed labels = %#v, want worker-ready cleanup", gateway.removeIssueLabelCalls)
	}
}

func TestRegenerateConflictEscalatesHumanCommitWithoutClosing(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{
			currentUser:   "looper",
			viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}},
		},
		issue:   IssueDetail{Number: 7, State: "OPEN"},
		commits: []PullRequestCommit{{AuthorLogin: "alice", CommitterLogin: "alice"}},
	}
	routes := 0
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, _ RegenerateIssueInput) error { routes++; return nil })
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	result, err := runner.RegenerateConflict(context.Background(), ConflictRegenerationInput{
		ProjectID: "project_1", Repo: "acme/looper", IssueRepo: "acme/looper", IssueNumber: 7, PRNumber: 42, ConflictRepairs: 2, CWD: project.RepoPath,
	})
	if err != nil {
		t.Fatalf("RegenerateConflict() error = %v", err)
	}
	if !result.Escalated || result.Completed || routes != 0 || len(gateway.closeCalls) != 0 {
		t.Fatalf("result/routes/closes = %#v/%d/%d, want escalated/no route/no close", result, routes, len(gateway.closeCalls))
	}
	if len(gateway.addLabelCalls) != 1 || gateway.addLabelCalls[0].Labels[0] != labels.NeedsHuman {
		t.Fatalf("PR labels = %#v, want needs-human", gateway.addLabelCalls)
	}
	if len(gateway.fakeGitHubGateway.createIssueComments) != 1 || !strings.Contains(gateway.fakeGitHubGateway.createIssueComments[0].Body, "outcome=escalated") {
		t.Fatalf("escalation comments = %#v, want durable escalation marker", gateway.fakeGitHubGateway.createIssueComments)
	}
}

func TestRegenerateConflictReplaysCompletedMarkerWithoutSideEffects(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "CLOSED", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, State: "OPEN"},
		comments:          []IssueComment{{Body: regenerationCommentMarker + " authority=coordinator-conflict:acme/looper#42 outcome=completed -->"}},
	}
	routes := 0
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, _ RegenerateIssueInput) error { routes++; return nil })
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	result, err := runner.RegenerateConflict(context.Background(), ConflictRegenerationInput{ProjectID: "project_1", Repo: "acme/looper", IssueRepo: "acme/looper", IssueNumber: 7, PRNumber: 42, ConflictRepairs: 2, CWD: project.RepoPath})
	if err != nil {
		t.Fatalf("RegenerateConflict() error = %v", err)
	}
	if !result.Completed || routes != 0 || len(gateway.closeCalls) != 0 || len(gateway.fakeGitHubGateway.createIssueComments) != 0 {
		t.Fatalf("replay result/routes/closes/comments = %#v/%d/%d/%d, want completed/no side effects", result, routes, len(gateway.closeCalls), len(gateway.fakeGitHubGateway.createIssueComments))
	}
}
