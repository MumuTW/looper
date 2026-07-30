package triager

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/planner"
)

func TestTriagerReportRoutesThroughProcessedPlannerWithoutLabelsOrAssignee(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.llm.responses = []string{eligibleDecisionJSON()}
	plannerGitHub := &contractPlannerGitHub{
		detail: planner.IssueDetail{Number: 23, Title: "Add autonomous triage", Body: "Route clear reports into planning.", URL: "https://github.com/acme/looper/issues/23"},
	}
	plannerRole := planner.New(planner.Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: plannerGitHub,
		Git: &contractPlannerGit{}, AgentExecutor: contractPlannerAgent{}, Now: func() time.Time { return fixture.now },
		AllowAutoPush: boolPointer(true),
	})
	runner := New(Options{Repos: fixture.repos, GitHub: fixture.github, LLM: fixture.llm, Planner: plannerRole, Now: func() time.Time { return fixture.now }})

	discovered, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("Triager DiscoverIssues() error = %v", err)
	}
	if discovered.Routed != 1 || len(discovered.QueueItems) != 1 {
		t.Fatalf("Triager DiscoverIssues() = %#v", discovered)
	}
	processed, err := plannerRole.ProcessNext(context.Background(), "contract-planner")
	if err != nil {
		t.Fatalf("Planner ProcessNext() error = %v", err)
	}
	if processed == nil || processed.Status != "success" || processed.PullRequestNumber != 101 {
		t.Fatalf("Planner ProcessNext() = %#v", processed)
	}
}

type contractPlannerGitHub struct {
	detail planner.IssueDetail
	branch string
}

func (g *contractPlannerGitHub) ListOpenIssues(context.Context, planner.ListOpenIssuesInput) ([]planner.IssueSummary, error) {
	return nil, nil
}

func (g *contractPlannerGitHub) ViewIssue(context.Context, planner.ViewIssueInput) (planner.IssueDetail, error) {
	return g.detail, nil
}

func (g *contractPlannerGitHub) GetCurrentUserLogin(context.Context, string, string) (string, error) {
	return "octocat", nil
}

func (g *contractPlannerGitHub) AddIssueAssignees(context.Context, planner.IssueAssigneesInput) error {
	return nil
}

func (g *contractPlannerGitHub) ListOpenPullRequests(context.Context, planner.ListOpenPullRequestsInput) ([]planner.PullRequestSummary, error) {
	return nil, nil
}

func (g *contractPlannerGitHub) ViewPullRequest(_ context.Context, input planner.ViewPullRequestInput) (planner.PullRequestDetail, error) {
	return planner.PullRequestDetail{Number: input.PRNumber, URL: "https://example.test/pr/101", State: "OPEN", HeadRefName: g.branch, BaseRefName: "main"}, nil
}

func (g *contractPlannerGitHub) CreatePullRequest(_ context.Context, input planner.CreatePullRequestInput) (planner.CreatePullRequestResult, error) {
	g.branch = input.HeadBranch
	return planner.CreatePullRequestResult{Number: 101, URL: "https://example.test/pr/101"}, nil
}

func (g *contractPlannerGitHub) UpdatePullRequestBody(context.Context, planner.UpdatePullRequestBodyInput) error {
	return nil
}

func (g *contractPlannerGitHub) AddPullRequestLabels(context.Context, planner.PullRequestLabelsInput) error {
	return nil
}

func (g *contractPlannerGitHub) AddPullRequestReviewers(context.Context, planner.PullRequestReviewersInput) error {
	return nil
}

type contractPlannerGit struct{}

func (*contractPlannerGit) CreateWorktree(_ context.Context, input planner.CreateWorktreeInput) (planner.CreateWorktreeResult, error) {
	path := filepath.Join(input.WorktreeRoot, "contract")
	if err := os.MkdirAll(path, 0o755); err != nil {
		return planner.CreateWorktreeResult{}, err
	}
	return planner.CreateWorktreeResult{ID: "worktree_contract", WorktreePath: path, Branch: input.Branch, BaseBranch: input.BaseBranch}, nil
}

func (*contractPlannerGit) InspectHead(context.Context, planner.InspectHeadInput) (planner.InspectHeadResult, error) {
	return planner.InspectHeadResult{HeadSHA: "abc123", NewCommitSHAs: []string{"abc123"}}, nil
}

func (*contractPlannerGit) Commit(context.Context, planner.CommitInput) (planner.CommitResult, error) {
	return planner.CommitResult{CommitSHA: "abc123"}, nil
}

func (*contractPlannerGit) Push(context.Context, planner.PushInput) error { return nil }

type contractPlannerAgent struct{}

func (contractPlannerAgent) Start(context.Context, planner.AgentRunInput) (planner.AgentExecution, error) {
	return contractPlannerExecution{}, nil
}

type contractPlannerExecution struct{}

func (contractPlannerExecution) Wait(context.Context) (planner.AgentResult, error) {
	return planner.AgentResult{Status: "completed", Summary: "wrote spec"}, nil
}

func boolPointer(value bool) *bool { return &value }
