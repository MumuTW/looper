package fixer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/loops/runpipe"
	"github.com/MumuTW/looper/internal/storage"
)

type regenerationFakeGateway struct {
	*fakeGitHubGateway
	issue                 IssueDetail
	comments              []IssueComment
	commits               []PullRequestCommit
	closeCalls            []ClosePullRequestInput
	closeErr              error
	issueLabelCalls       []IssueLabelsInput
	issueLabelErr         error
	removeIssueLabelCalls []IssueLabelsInput
	removeIssueLabelErr   error
	prLabelErr            error
	// commentErrs are consumed one per CreateIssueComment call (nil entry =
	// success) before falling through to the embedded gateway.
	commentErrs []error
}

func (f *regenerationFakeGateway) CreateIssueComment(ctx context.Context, input IssueCommentInput) (IssueCommentResult, error) {
	if len(f.commentErrs) > 0 {
		err := f.commentErrs[0]
		f.commentErrs = f.commentErrs[1:]
		if err != nil {
			f.createIssueComments = append(f.createIssueComments, input)
			return IssueCommentResult{}, err
		}
	}
	return f.fakeGitHubGateway.CreateIssueComment(ctx, input)
}

func (f *regenerationFakeGateway) ViewIssue(context.Context, ViewIssueInput) (IssueDetail, error) {
	return f.issue, nil
}

func (f *regenerationFakeGateway) ListIssueComments(context.Context, ViewIssueInput) ([]IssueComment, error) {
	return append([]IssueComment(nil), f.comments...), nil
}

func (f *regenerationFakeGateway) ClosePullRequest(_ context.Context, input ClosePullRequestInput) error {
	f.closeCalls = append(f.closeCalls, input)
	return f.closeErr
}

func (f *regenerationFakeGateway) AddIssueLabels(_ context.Context, input IssueLabelsInput) error {
	f.issueLabelCalls = append(f.issueLabelCalls, input)
	return f.issueLabelErr
}

func (f *regenerationFakeGateway) RemoveIssueLabels(_ context.Context, input IssueLabelsInput) error {
	f.removeIssueLabelCalls = append(f.removeIssueLabelCalls, input)
	return f.removeIssueLabelErr
}

func (f *regenerationFakeGateway) AddPullRequestLabels(ctx context.Context, input PullRequestLabelsInput) error {
	if f.prLabelErr != nil {
		return f.prLabelErr
	}
	return f.fakeGitHubGateway.AddPullRequestLabels(ctx, input)
}

func (f *regenerationFakeGateway) ListPullRequestCommits(context.Context, ViewPullRequestInput) ([]PullRequestCommit, error) {
	return append([]PullRequestCommit(nil), f.commits...), nil
}

func setupRegenerationRecords(t *testing.T, fixture *runnerFixture) (storage.LoopRecord, storage.QueueItemRecord) {
	t.Helper()
	ctx := context.Background()
	repo := "acme/looper"
	prNumber := int64(42)
	workerMetadata := `{"worker":{"issueRepo":"acme/looper","issueNumber":7,"issueUrl":"https://example.test/issues/7","title":"Original issue"}}`
	worker := storage.LoopRecord{ID: "worker_origin", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: runpipe.StringPtr(workerMetadata), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(ctx, worker); err != nil {
		t.Fatalf("Loops.Upsert(worker) error = %v", err)
	}
	fixerMetadata := `{"sourceWorkerId":"worker_origin"}`
	fixer := storage.LoopRecord{ID: "fixer_exhausted", Seq: 2, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: runpipe.StringPtr(buildPullRequestTargetID(repo, prNumber)), Repo: &repo, PRNumber: &prNumber, Status: "paused", MetadataJSON: runpipe.StringPtr(fixerMetadata), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(ctx, fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer) error = %v", err)
	}
	queue := storage.QueueItemRecord{ID: "queue_exhausted", ProjectID: runpipe.StringPtr("project_1"), LoopID: runpipe.StringPtr(fixer.ID), Type: "fixer", TargetType: "pull_request", TargetID: buildPullRequestTargetID(repo, prNumber), Repo: &repo, PRNumber: &prNumber, Status: "failed", Attempts: 3, MaxAttempts: 3, CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	return fixer, queue
}

func newRegenerationRunner(t *testing.T, fixture *runnerFixture, gateway *regenerationFakeGateway, route RegenerateIssueFunc) *Runner {
	t.Helper()
	return New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: gateway,
		Now: fixture.now, Logger: fixture.logger, OnRegenerateIssue: route,
		DeleteBranchOnRegeneration: func(string) bool { return true },
	})
}

func TestHandleTerminalExhaustionOrdersCommentCloseAndPlannerRoute(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", BaseRefName: "main", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, Title: "Original issue", Body: "please implement", URL: "https://example.test/issues/7", State: "OPEN"},
		commits:           []PullRequestCommit{{SHA: "head-42", AuthorLogin: "looper", CommitterLogin: "looper"}},
	}
	var routed RegenerateIssueInput
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, input RegenerateIssueInput) error {
		routed = input
		return nil
	})
	loop, queue := setupRegenerationRecords(t, fixture)
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	failure := &runpipe.LoopError{Message: "validation failed", Kind: runpipe.FailureRetryableTransient}
	checkpoint := fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: "head-42"}, FixItems: []FixItem{{ID: "thread-7", Summary: "fix root cause"}}}
	updated, action, err := runner.applyTerminalRegeneration(context.Background(), *project, loop, queue, checkpoint, failure)
	if err != nil {
		t.Fatalf("applyTerminalRegeneration() error = %v", err)
	}
	if action != regenerationCompleted {
		t.Fatalf("action = %q, want completed", action)
	}
	if updated.Status != "failed" {
		t.Fatalf("loop status = %q, want failed", updated.Status)
	}
	if len(gateway.fakeGitHubGateway.createIssueComments) != 1 || len(gateway.closeCalls) != 1 {
		t.Fatalf("remote calls: comments=%d closes=%d, want one each", len(gateway.createIssueComments), len(gateway.closeCalls))
	}
	if !gateway.closeCalls[0].DeleteBranch {
		t.Fatal("ClosePullRequest.DeleteBranch = false, want true for Looper branch")
	}
	if routed.IssueNumber != 7 || routed.Authority != "fixer-exhaustion:fixer_exhausted" {
		t.Fatalf("route input = %#v", routed)
	}
	if !strings.Contains(routed.FailureContext, "headSha=head-42") || !strings.Contains(routed.FailureContext, "fixItemsFingerprint=") {
		t.Fatalf("route failure context missing fingerprints: %q", routed.FailureContext)
	}
	if len(gateway.issueLabelCalls) != 1 || gateway.issueLabelCalls[0].Labels[0] != labels.DefaultPlanTrigger {
		t.Fatalf("issue labels = %#v, want planner trigger", gateway.issueLabelCalls)
	}
}

func TestHandleTerminalExhaustionHonorsDeleteBranchPolicy(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, State: "OPEN"},
		commits:           []PullRequestCommit{{AuthorLogin: "looper", CommitterLogin: "looper"}},
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: gateway,
		Now: fixture.now, Logger: fixture.logger,
		OnRegenerateIssue:          func(context.Context, RegenerateIssueInput) error { return nil },
		DeleteBranchOnRegeneration: func(string) bool { return false },
	})
	loop, queue := setupRegenerationRecords(t, fixture)
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if _, action, err := runner.applyTerminalRegeneration(context.Background(), *project, loop, queue, fixerCheckpoint{}, &runpipe.LoopError{Message: "failed", Kind: runpipe.FailureRetryableTransient}); err != nil || action != regenerationCompleted {
		t.Fatalf("applyTerminalRegeneration() = action %q err %v, want completed", action, err)
	}
	if len(gateway.closeCalls) != 1 || gateway.closeCalls[0].DeleteBranch {
		t.Fatalf("ClosePullRequest calls = %#v, want one call with DeleteBranch=false", gateway.closeCalls)
	}
}

func TestHandleTerminalExhaustionReplaysAfterRouteFailureWithoutDuplicateClose(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, State: "OPEN"},
		commits:           []PullRequestCommit{{AuthorLogin: "looper", CommitterLogin: "looper"}},
	}
	routes := 0
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, _ RegenerateIssueInput) error {
		routes++
		if routes == 1 {
			return errors.New("planner unavailable")
		}
		return nil
	})
	loop, queue := setupRegenerationRecords(t, fixture)
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	failure := &runpipe.LoopError{Message: "agent failed", Kind: runpipe.FailureRetryableTransient}
	checkpoint := fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: "head-42"}}
	if _, _, err := runner.applyTerminalRegeneration(context.Background(), *project, loop, queue, checkpoint, failure); err == nil {
		t.Fatal("first applyTerminalRegeneration() error = nil, want route failure")
	}
	durable, _ := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if durable == nil || !strings.Contains(derefString(durable.MetadataJSON), `"closed":true`) {
		t.Fatalf("durable regeneration state = %#v, want closed checkpoint", durable)
	}
	updated, action, err := runner.applyTerminalRegeneration(context.Background(), *project, *durable, queue, checkpoint, failure)
	if err != nil {
		t.Fatalf("replay applyTerminalRegeneration() error = %v", err)
	}
	if action != regenerationCompleted || updated.Status != "failed" {
		t.Fatalf("replay result = action %q loop=%q, want completed/failed", action, updated.Status)
	}
	if len(gateway.fakeGitHubGateway.createIssueComments) != 1 || len(gateway.closeCalls) != 1 || routes != 2 {
		t.Fatalf("replay side effects: comments=%d closes=%d routes=%d, want 1/1/2", len(gateway.createIssueComments), len(gateway.closeCalls), routes)
	}
}

func TestHandleTerminalExhaustionEscalatesHumanCommitWithoutClosing(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, State: "OPEN"},
		commits:           []PullRequestCommit{{AuthorLogin: "alice", CommitterLogin: "alice"}},
	}
	routes := 0
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, _ RegenerateIssueInput) error { routes++; return nil })
	loop, queue := setupRegenerationRecords(t, fixture)
	project, _ := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	action, err := runner.handleTerminalExhaustion(context.Background(), *project, loop, queue, fixerCheckpoint{}, &runpipe.LoopError{Message: "failed", Kind: runpipe.FailureRetryableTransient})
	if err != nil {
		t.Fatalf("handleTerminalExhaustion() error = %v", err)
	}
	if action != regenerationEscalated || routes != 0 || len(gateway.closeCalls) != 0 {
		t.Fatalf("guard result: action=%q routes=%d closes=%d", action, routes, len(gateway.closeCalls))
	}
	if len(gateway.addLabelCalls) != 1 || len(gateway.addLabelCalls[0].Labels) != 1 || gateway.addLabelCalls[0].Labels[0] != labels.NeedsHuman {
		t.Fatalf("PR labels = %#v, want needs-human", gateway.addLabelCalls)
	}
}
