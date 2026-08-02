package fixer

import (
	"context"
	"errors"
	"strings"
	"testing"

	githubinfra "github.com/MumuTW/looper/internal/infra/github"
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

func TestRegenerateConflictEscalatesForeignBotCommitWithoutClosing(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{
			currentUser:   "looper",
			viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}},
		},
		issue:   IssueDetail{Number: 7, State: "OPEN"},
		commits: []PullRequestCommit{{AuthorLogin: "dependabot[bot]", CommitterLogin: "dependabot[bot]"}},
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
		comments:          []IssueComment{{Author: "looper", Body: regenerationCommentMarker + " authority=coordinator-conflict:acme/looper#42 outcome=completed -->"}},
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

func TestRegenerateConflictReplaysPendingMarkerOnClosedPR(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "CLOSED", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, State: "OPEN"},
		comments:          []IssueComment{{Author: "looper", Body: regenerationCommentMarker + " authority=coordinator-conflict:acme/looper#42 outcome=pending -->"}},
	}
	routes := 0
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, _ RegenerateIssueInput) error { routes++; return nil })
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	result, err := runner.RegenerateConflict(context.Background(), ConflictRegenerationInput{ProjectID: "project_1", Repo: "acme/looper", IssueRepo: "acme/looper", IssueNumber: 7, PRNumber: 42, ConflictRepairs: 2, CWD: project.RepoPath})
	if err != nil {
		t.Fatalf("RegenerateConflict() error = %v", err)
	}
	if !result.Completed || routes != 1 || len(gateway.closeCalls) != 0 || len(gateway.fakeGitHubGateway.createIssueComments) != 1 {
		t.Fatalf("pending replay result/routes/closes/comments = %#v/%d/%d/%d, want completed/one route/no close/completed marker", result, routes, len(gateway.closeCalls), len(gateway.fakeGitHubGateway.createIssueComments))
	}
}

func TestRegenerateConflictSkipsFreshlyMergedPR(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "CLOSED", MergedAt: "2026-04-11T12:00:00Z", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, State: "OPEN"},
		commits:           []PullRequestCommit{{AuthorLogin: "looper", CommitterLogin: "looper"}},
	}
	routes := 0
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, _ RegenerateIssueInput) error { routes++; return nil })
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	result, err := runner.RegenerateConflict(context.Background(), ConflictRegenerationInput{ProjectID: "project_1", Repo: "acme/looper", IssueRepo: "acme/looper", IssueNumber: 7, PRNumber: 42, ConflictRepairs: 2, CWD: project.RepoPath})
	if err != nil {
		t.Fatalf("RegenerateConflict() error = %v", err)
	}
	if result.Completed || result.Escalated || routes != 0 || len(gateway.closeCalls) != 0 || len(gateway.fakeGitHubGateway.createIssueComments) != 0 {
		t.Fatalf("merged result/routes/closes/comments = %#v/%d/%d/%d, want no regeneration side effects", result, routes, len(gateway.closeCalls), len(gateway.fakeGitHubGateway.createIssueComments))
	}
}

func TestRegenerateConflictHonorsGlobalHoldLabels(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", Labels: []string{labels.HoldGlobal}, HeadRefName: "looper/fix-42", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, State: "OPEN"},
		commits:           []PullRequestCommit{{AuthorLogin: "looper", CommitterLogin: "looper"}},
	}
	routes := 0
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, _ RegenerateIssueInput) error { routes++; return nil })
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	result, err := runner.RegenerateConflict(context.Background(), ConflictRegenerationInput{ProjectID: "project_1", Repo: "acme/looper", IssueRepo: "acme/looper", IssueNumber: 7, PRNumber: 42, ConflictRepairs: 2, CWD: project.RepoPath})
	if err != nil {
		t.Fatalf("RegenerateConflict() error = %v", err)
	}
	if result.Completed || result.Escalated || routes != 0 || len(gateway.closeCalls) != 0 || len(gateway.fakeGitHubGateway.createIssueComments) != 0 {
		t.Fatalf("held result/routes/closes/comments = %#v/%d/%d/%d, want no regeneration side effects", result, routes, len(gateway.closeCalls), len(gateway.fakeGitHubGateway.createIssueComments))
	}
}

func TestConflictRegenerationMarkerRequiresAuthenticatedAuthor(t *testing.T) {
	comments := []IssueComment{{Author: "attacker", Body: regenerationCommentMarker + " authority=coordinator-conflict:acme/looper#42 outcome=completed -->"}}
	if got := conflictRegenerationMarkerStatus(comments, "coordinator-conflict:acme/looper#42", "looper"); got != "" {
		t.Fatalf("conflictRegenerationMarkerStatus() = %q, want unauthenticated marker ignored", got)
	}
}

func TestRegenerateConflictLabelsBeforeWritingEscalationMarker(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{
			currentUser:            "looper",
			addPullRequestLabelErr: errors.New("label unavailable"),
			viewResponses:          []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}},
		},
		issue:   IssueDetail{Number: 7, State: "OPEN"},
		commits: []PullRequestCommit{{AuthorLogin: "alice", CommitterLogin: "alice"}},
	}
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, _ RegenerateIssueInput) error { return nil })
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if _, err := runner.RegenerateConflict(context.Background(), ConflictRegenerationInput{ProjectID: "project_1", Repo: "acme/looper", IssueRepo: "acme/looper", IssueNumber: 7, PRNumber: 42, ConflictRepairs: 2, CWD: project.RepoPath}); err == nil {
		t.Fatal("RegenerateConflict() error = nil, want label failure")
	}
	if len(gateway.fakeGitHubGateway.createIssueComments) != 0 {
		t.Fatalf("escalation comments = %d, want no terminal marker before label succeeds", len(gateway.fakeGitHubGateway.createIssueComments))
	}
}

func TestRegenerateConflictTreatsConcurrentMergeAsCompleted(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{
			currentUser:   "looper",
			viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}},
		},
		issue: IssueDetail{Number: 7, State: "OPEN"}, commits: []PullRequestCommit{{AuthorLogin: "looper", CommitterLogin: "looper"}}, closeErr: githubinfra.ErrPullRequestAlreadyMerged,
	}
	routes := 0
	runner := newRegenerationRunner(t, fixture, gateway, func(context.Context, RegenerateIssueInput) error { routes++; return nil })
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	result, err := runner.RegenerateConflict(context.Background(), ConflictRegenerationInput{ProjectID: "project_1", Repo: "acme/looper", IssueRepo: "acme/looper", IssueNumber: 7, PRNumber: 42, ConflictRepairs: 2, CWD: project.RepoPath})
	if err != nil || !result.Completed || routes != 0 {
		t.Fatalf("RegenerateConflict() = (%#v, %v), routes=%d; want completed no-op", result, err, routes)
	}
}
