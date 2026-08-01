package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/loops"
	"github.com/MumuTW/looper/internal/storage"
)

type malformedAskAgentExecutor struct {
	inner   fakeAgentExecutor
	content string
}

func TestSuspendMalformedAskSurfacesDiagnosticOnlyOnExistingPR(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	worktree := t.TempDir()
	if err := os.Mkdir(filepath.Join(worktree, ".looper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".looper", "ask.json"), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, evidence, err := loops.StageHITLGateEvidence(worktree, 1024)
	if err != nil {
		t.Fatal(err)
	}
	loop, _ := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	loop.Status = "running"
	pr, repo := int64(42), "acme/widgets"
	loop.PRNumber, loop.Repo = &pr, &repo
	if err := fixture.repos.Loops.Upsert(ctx, *loop); err != nil {
		t.Fatal(err)
	}
	queue, _ := fixture.repos.Queue.GetByID(ctx, "queue_worker_1")
	queue.Status = "running"
	if err := fixture.repos.Queue.Upsert(ctx, *queue); err != nil {
		t.Fatal(err)
	}
	run := storage.RunRecord{ID: "run_malformed_existing_pr", LoopID: loop.ID, Status: "running", StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatal(err)
	}
	project, _ := fixture.repos.Projects.GetByID(ctx, "project_1")
	github := &fakeGitHubGateway{issueCommentResult: IssueCommentResult{ID: 909}}
	github.onCreateIssueComment = func(IssueCommentInput) {
		parked, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
		if err != nil || parked == nil || parked.Status != "awaiting_human" {
			t.Fatalf("GitHub delivery observed loop = (%#v, %v), want durable park first", parked, err)
		}
		stored, ok := loops.ReadHITLAsk(parked.MetadataJSON)
		if !ok || stored.GateEvidence == nil {
			t.Fatalf("GitHub delivery ran before bounded gate evidence persisted: %#v", stored)
		}
	}
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, Logger: fixture.logger, Now: fixture.now, HITLEnabled: true, HITLAnswerTransport: "github", HITLGitHub: HITLGitHubSettings{AwaitingLabel: labels.AwaitingHuman}})
	diagnostic := "malformed sentinel: bounded evidence is staged"
	awaiting := &awaitingHumanError{question: "Discard malformed request?", options: []string{"regenerate"}, recommendation: diagnostic, gateEvidence: evidence}

	result, err := runner.suspendForHuman(ctx, stepInput{Project: *project, Loop: *loop, Run: run, QueueItem: *queue}, run, workerCheckpoint{PullRequest: &checkpointPullPR{Number: pr}}, awaiting)
	if err != nil || result.Status != "awaiting_human" {
		t.Fatalf("suspendForHuman() = (%#v, %v)", result, err)
	}
	if len(git.pushCalls) != 0 || len(github.createPRCalls) != 0 || len(github.createIssueCommentCalls) != 1 {
		t.Fatalf("remote calls = pushes:%d prs:%d comments:%d, want 0/0/1", len(git.pushCalls), len(github.createPRCalls), len(github.createIssueCommentCalls))
	}
	if body := github.createIssueCommentCalls[0].Body; !strings.Contains(body, diagnostic) {
		t.Fatalf("GitHub ask omitted persisted diagnostic: %s", body)
	}
	fresh, _ := fixture.repos.Loops.GetByID(ctx, loop.ID)
	ask, ok := loops.ReadHITLAsk(fresh.MetadataJSON)
	if !ok || ask.Status != "awaiting" || ask.AskCommentID != 909 || ask.GateEvidence == nil {
		t.Fatalf("persisted ask = %#v, want durable gate + GitHub correlation", ask)
	}
}

func (e *malformedAskAgentExecutor) Start(ctx context.Context, input AgentRunInput) (AgentExecution, error) {
	path := filepath.Join(input.WorkingDirectory, hitlSentinelRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(e.content), 0o600); err != nil {
		return nil, err
	}
	return e.inner.Start(ctx, input)
}

func TestProcessClaimedQueueItemParksMalformedAskWithoutDefaultGitHubPublication(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	worktreePath := filepath.Join(t.TempDir(), "wt")
	git := &fakeGitGateway{createResult: CreateWorktreeResult{
		WorktreePath: worktreePath, Branch: "looper/feature", BaseBranch: "main",
		HeadSHA: "base-head", WorktreeID: "worktree_1",
	}}
	github := &fakeGitHubGateway{}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git,
		AgentExecutor: &malformedAskAgentExecutor{content: `{"question":"truncated`},
		Logger:        fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true,
		HITLEnabled: true, // empty transport is the default GitHub path
	})

	claim, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "worker-1", "worker")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v)", claim, err)
	}
	result, err := runner.ProcessClaimedQueueItem(ctx, *claim)
	if err != nil || result == nil || result.Status != "awaiting_human" {
		t.Fatalf("ProcessClaimedQueueItem() = (%#v, %v), want awaiting_human", result, err)
	}
	loop, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || loop == nil || loop.Status != "awaiting_human" {
		t.Fatalf("loop = (%#v, %v), want awaiting_human", loop, err)
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || ask.Status != "awaiting" || ask.GateEvidence == nil || !strings.Contains(ask.Recommendation, "not valid JSON") {
		t.Fatalf("HITL ask = %#v, want persisted bounded malformed-gate evidence", ask)
	}
	queue, err := fixture.repos.Queue.GetByID(ctx, claim.ID)
	if err != nil || queue == nil || queue.Status != "cancelled" {
		t.Fatalf("queue = (%#v, %v), want cancelled with no auto-requeue", queue, err)
	}
	runs, err := fixture.repos.Runs.ListByLoop(ctx, loop.ID)
	if err != nil || len(runs) != 1 || runs[0].Status != "interrupted" {
		t.Fatalf("runs = (%#v, %v), want one interrupted resumable run", runs, err)
	}
	if len(git.pushCalls) != 0 || len(github.createPRCalls) != 0 {
		t.Fatalf("publication = pushes:%d prs:%d, want none before authorization", len(git.pushCalls), len(github.createPRCalls))
	}
	for _, call := range github.createIssueCommentCalls {
		if strings.Contains(call.Body, hitlGitHubAskMarkerPrefix) {
			t.Fatalf("malformed gate was delivered as a GitHub ask before persistence: %#v", call)
		}
	}
	if _, err := os.Lstat(filepath.Join(ask.GateEvidence.WorktreeRoot, ".looper", "ask.pending")); err != nil {
		t.Fatalf("staged gate identity missing: %v", err)
	}
}
