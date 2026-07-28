package fixer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/hitl"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestHITLContract_ConflictEscalatesAwaitingHuman(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()

	var notified []HITLAskNotification
	wt := filepath.Join(t.TempDir(), "wt-87")
	github := &fakeGitHubGateway{
		listOpen: []PullRequestSummary{{Number: 87, State: "OPEN", HeadSHA: pr87Head}},
		viewResponses: []PullRequestDetail{
			pr87Detail(pr87Head),
			pr87Detail(pr87Head),
			pr87Detail(pr87Head),
			pr87Detail(pr87Head),
			pr87Detail(pr87Head),
		},
	}
	git := &fakeGitGateway{
		createResult:   CreateWorktreeResult{WorktreePath: wt, Branch: "feature/pr87", HeadSHA: pr87Head},
		prepareResult:  PrepareWorktreeResult{HeadSHA: pr87Head, Clean: true},
		inspectResults: []InspectHeadResult{{HeadSHA: pr87Head}},
	}
	agent := &hitlScriptedAgent{
		writeAskJSON: true,
		writeDismiss: true,
		results: []AgentResult{{
			Status: "completed", Summary: "needs human", ParseStatus: "parsed",
			Stdout: pr87NeedsHumanStdout("c-strategy", "t-strategy"),
		}},
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: github, Git: git, AgentExecutor: agent,
		ValidationRunner: passValidation,
		AllowAutoCommit:  true, AllowAutoPush: true, AllowRiskyFixes: true,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLAnswerTransport: "feishu",
		HITLNotify: func(_ context.Context, n HITLAskNotification) error {
			notified = append(notified, n)
			return nil
		},
	})
	if _, err := runner.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "fixer-hitl-1", "fixer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v)", claim, err)
	}

	result, err := runner.ProcessClaimedItem(ctx, *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "awaiting_human" {
		t.Fatalf("result.Status = %q, want awaiting_human (got summary=%q)", result.Status, result.Summary)
	}

	// Loop + HITLAsk persisted with fixer role and fingerprints.
	loopsList, err := fixture.repos.Loops.List(ctx)
	if err != nil {
		t.Fatalf("Loops.List() error = %v", err)
	}
	var loop *storage.LoopRecord
	for i := range loopsList {
		if loopsList[i].Type == "fixer" && derefInt64(loopsList[i].PRNumber) == 87 {
			loop = &loopsList[i]
			break
		}
	}
	if loop == nil {
		t.Fatal("fixer loop for PR 87 not found")
	}
	if loop.Status != "awaiting_human" {
		t.Fatalf("loop.Status = %q, want awaiting_human", loop.Status)
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok {
		t.Fatal("HITLAsk not persisted")
	}
	if ask.Status != "awaiting" || ask.Role != "fixer" {
		t.Fatalf("ask status/role = %q/%q, want awaiting/fixer", ask.Status, ask.Role)
	}
	if strings.TrimSpace(ask.HeadSHA) == "" || strings.TrimSpace(ask.PRIntentFingerprint) == "" || strings.TrimSpace(ask.ReviewContentFingerprint) == "" {
		t.Fatalf("ask missing fingerprints: %#v", ask)
	}
	if !strings.Contains(strings.ToLower(ask.Question), "rolling") && !strings.Contains(strings.ToLower(ask.Question), "strategy") && !strings.Contains(ask.Question, "configurable") {
		// Accept either ask.json question or synthesized needs_human brief.
		if ask.Question == "" {
			t.Fatalf("empty ask question: %#v", ask)
		}
	}
	if len(notified) != 1 {
		t.Fatalf("HITLNotify calls = %d, want 1", len(notified))
	}

	// Queue cancelled so /respond can requeue.
	q, err := fixture.repos.Queue.GetByID(ctx, claim.ID)
	if err != nil || q == nil {
		t.Fatalf("Queue.GetByID() = (%#v, %v)", q, err)
	}
	if q.Status != "cancelled" {
		t.Fatalf("queue status = %q, want cancelled", q.Status)
	}

	// Run interrupted; repair NOT complete.
	run, err := fixture.repos.Runs.GetByID(ctx, result.RunID)
	if err != nil || run == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v)", run, err)
	}
	if run.Status != "interrupted" {
		t.Fatalf("run.Status = %q, want interrupted", run.Status)
	}
	cp := parseCheckpoint(run.CheckpointJSON)
	if cp.Repair != nil {
		t.Fatalf("checkpoint.Repair = %#v, want nil so resume re-enters repair", cp.Repair)
	}
}

func TestHITLContract_NoMutateOnEscalate(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()

	wt := filepath.Join(t.TempDir(), "wt-87-nomut")
	github := &fakeGitHubGateway{
		listOpen:      []PullRequestSummary{{Number: 87, State: "OPEN", HeadSHA: pr87Head}},
		viewResponses: []PullRequestDetail{pr87Detail(pr87Head), pr87Detail(pr87Head), pr87Detail(pr87Head), pr87Detail(pr87Head), pr87Detail(pr87Head)},
	}
	git := &fakeGitGateway{
		createResult:   CreateWorktreeResult{WorktreePath: wt, Branch: "feature/pr87", HeadSHA: pr87Head},
		prepareResult:  PrepareWorktreeResult{HeadSHA: pr87Head, Clean: true},
		inspectResults: []InspectHeadResult{{HeadSHA: pr87Head}},
	}
	agent := &hitlScriptedAgent{
		writeAskJSON: true,
		writeDismiss: true,
		results: []AgentResult{{
			Status: "completed", Summary: "needs human", ParseStatus: "parsed",
			Stdout: pr87NeedsHumanStdout("c-strategy", "t-strategy"),
		}},
	}
	runner, claim := seedPR87Queue(t, fixture, github, git, agent, true)

	result, err := runner.ProcessClaimedItem(ctx, claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "awaiting_human" {
		t.Fatalf("result.Status = %q, want awaiting_human", result.Status)
	}

	if len(git.pushCalls) != 0 {
		t.Fatalf("push calls = %d, want 0 on escalate", len(git.pushCalls))
	}
	if len(github.resolveCalls) != 0 {
		t.Fatalf("ResolveReviewThread calls = %d, want 0 on escalate", len(github.resolveCalls))
	}
	if len(github.dismissedReviews) != 0 {
		t.Fatalf("DismissReview calls = %d, want 0 (dismiss.json must not fire on escalate)", len(github.dismissedReviews))
	}
	if len(github.replyCalls) != 0 {
		t.Fatalf("thread reply calls = %d, want 0 on escalate", len(github.replyCalls))
	}
	// Sentinels cleared after durable suspend so resume cannot inherit dismiss.
	if _, err := os.Stat(filepath.Join(wt, hitl.AskSentinelRelPath)); !os.IsNotExist(err) {
		t.Fatal("ask.json must be removed after durable suspend")
	}
	if _, err := os.Stat(filepath.Join(wt, ".looper", "dismiss.json")); !os.IsNotExist(err) {
		t.Fatal("dismiss.json must be removed on HITL suspend")
	}
}

func TestHITLContract_HITLDisabledNeedsHumanIsInvalidContract(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	worktreeRoot := t.TempDir()
	wt := filepath.Join(worktreeRoot, "wt-87-off")
	_ = os.MkdirAll(wt, 0o755)
	repoPath := t.TempDir()
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	detail := &checkpointDetail{
		HeadSHA: pr87Head, BaseSHA: pr87Base, BaseRefName: "main", HeadRefName: "feature/pr87",
		Title: pr87Title, Body: pr87Body, State: "OPEN",
	}
	fixItems := []FixItem{{
		Type: "comment", ID: "c-strategy", ThreadID: "t-strategy",
		Body: pr87ReviewerBody, Summary: "restore configurable strategy",
	}}
	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{pr87Detail(pr87Head)}}
	agent := &hitlScriptedAgent{
		writeAskJSON: true, // ask.json while HITL off is also invalid
		results: []AgentResult{{
			Status: "completed", Summary: "would escalate", ParseStatus: "parsed",
			Stdout: pr87NeedsHumanStdout("c-strategy", "t-strategy"),
		}},
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: false, AllowAutoPush: true,
	})
	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_hitl_off", Seq: 90, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	run := storage.RunRecord{ID: "run_hitl_off", LoopID: loop.ID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	_ = fixture.repos.Runs.Upsert(ctx, run)

	_, err := runner.runRepairStep(ctx, stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &projectMeta},
		Loop:    loop, Run: run, Repo: repo, PRNumber: pr,
		Checkpoint: fixerCheckpoint{
			Detail: detail, FixItems: fixItems, FixItemsHash: "h1",
			Worktree: &checkpointWorktree{Path: wt, Branch: "feature/pr87", HeadSHA: pr87Head, PreparedAt: nowISO},
		},
	})
	if err == nil {
		t.Fatal("HITL off + needs_human/ask.json must fail closed")
	}
	if _, ok := asAwaitingHumanError(err); ok {
		t.Fatal("must not suspend as awaiting_human when HITL disabled")
	}
	loopErr, ok := err.(*loopError)
	if !ok || loopErr.kind != FailureManualIntervention {
		t.Fatalf("err = %v (%T), want *loopError manual_intervention", err, err)
	}
	if !strings.Contains(err.Error(), "hitl.enabled is false") {
		t.Fatalf("error = %q, want invalid-contract message", err.Error())
	}
	if len(agent.starts) == 0 {
		t.Fatal("expected agent start")
	}
	if strings.Contains(agent.starts[0].Prompt, "or \"needs_human\"") {
		t.Fatal("HITL-off prompt must not advertise needs_human action")
	}
}
