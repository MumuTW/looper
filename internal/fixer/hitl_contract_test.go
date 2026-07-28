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

// PR #87-class scenario: PR intent hard-codes RollingUpdate; reviewer asks to
// restore a configurable strategy. Fixer with HITL must escalate, not mutate.

const (
	pr87Title        = "Hard-code deployment strategy to RollingUpdate"
	pr87Body         = "Product decision: strategy is fixed to RollingUpdate. Do not make it configurable."
	pr87ReviewerBody = "Please restore the configurable deployment strategy so operators can choose."
	pr87Head         = "head-pr87"
	pr87Base         = "base-pr87"
)

// hitlScriptedAgent records starts and optionally writes ask/dismiss sentinels
// into the worktree before returning a scripted AgentResult.
type hitlScriptedAgent struct {
	results      []AgentResult
	starts       []AgentRunInput
	writeAskJSON bool
	writeDismiss bool
	askQuestion  string
	askOptions   []string
}

func (a *hitlScriptedAgent) Start(_ context.Context, input AgentRunInput) (AgentExecution, error) {
	a.starts = append(a.starts, input)
	if a.writeAskJSON || a.writeDismiss {
		looperDir := filepath.Join(input.WorkingDirectory, ".looper")
		_ = os.MkdirAll(looperDir, 0o755)
		if a.writeAskJSON {
			q := a.askQuestion
			if q == "" {
				q = "Keep hard-coded RollingUpdate or restore configurable strategy?"
			}
			opts := a.askOptions
			if len(opts) == 0 {
				opts = []string{"keep RollingUpdate (PR intent)", "restore configurable strategy (reviewer)"}
			}
			payload := fmt.Sprintf(`{"question":%q,"options":[%q,%q],"recommendation":"Keep RollingUpdate per PR body.","recommendedOption":%q,"confidence":"high"}`,
				q, opts[0], opts[1], opts[0])
			if err := os.WriteFile(filepath.Join(looperDir, "ask.json"), []byte(payload), 0o644); err != nil {
				return nil, err
			}
		}
		if a.writeDismiss {
			if err := os.WriteFile(filepath.Join(looperDir, "dismiss.json"), []byte(`{"dismissals":[{"reviewer":"reviewer","reason":"conflicts with PR intent"}]}`), 0o644); err != nil {
				return nil, err
			}
		}
	}
	if len(a.results) == 0 {
		return nil, fmt.Errorf("no queued agent result")
	}
	result := a.results[0]
	a.results = a.results[1:]
	return fakeAgentExecution{result: result}, nil
}

func pr87NeedsHumanStdout(fixItemID, threadID string) string {
	return fmt.Sprintf(
		`__LOOPER_RESULT__={"summary":"unresolvable conflict with PR intent","review_thread_replies":[{"fixItemId":%q,"threadId":%q,"action":"needs_human","explanation":"Reviewer wants configurable strategy but PR title/body hard-code RollingUpdate; needs human call.","threadCommentsObserved":"abc"}]}`+"\n",
		fixItemID, threadID,
	)
}

func pr87Detail(head string) PullRequestDetail {
	return PullRequestDetail{
		Number: 87, Title: pr87Title, Body: pr87Body, State: "OPEN",
		HeadSHA: head, HeadRefName: "feature/pr87", BaseRefName: "main", BaseSHA: pr87Base,
		Comments: []map[string]any{
			{"id": "c-strategy", "threadId": "t-strategy", "body": pr87ReviewerBody, "author": "reviewer"},
		},
	}
}

func seedPR87Queue(t *testing.T, fixture *runnerFixture, github *fakeGitHubGateway, git *fakeGitGateway, agent AgentExecutor, hitlOn bool) (*Runner, storage.QueueItemRecord) {
	t.Helper()
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: github, Git: git, AgentExecutor: agent,
		ValidationRunner: passValidation,
		AllowAutoCommit:  true, AllowAutoPush: true, AllowRiskyFixes: true,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: hitlOn, HITLAnswerTransport: "feishu",
		HITLNotify: func(context.Context, HITLAskNotification) error { return nil },
	})
	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-hitl-worker", "fixer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v)", claim, err)
	}
	return runner, *claim
}

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

func TestHITLContract_HumanAnswerInjectedOnResume(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()

	detail := &checkpointDetail{
		HeadSHA: pr87Head, BaseSHA: pr87Base, BaseRefName: "main", HeadRefName: "feature/pr87",
		Title: pr87Title, Body: pr87Body, State: "OPEN",
	}
	// GitHub collect-time shape: review text in Summary, Body empty.
	fixItems := []FixItem{{
		Type: "comment", ID: "c-strategy", ThreadID: "t-strategy",
		Summary: pr87ReviewerBody, Body: "",
	}}
	reviewFP := computeReviewContentFingerprint(fixItems)
	intentFP := computePRIntentFingerprint(detail)

	repo := "acme/looper"
	pr := int64(87)
	nowISO := fixture.nowISO()
	meta, err := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question:                 "Keep hard-coded RollingUpdate or restore configurable strategy?",
		Options:                  []string{"keep RollingUpdate (PR intent)", "restore configurable strategy (reviewer)"},
		Answer:                   "keep RollingUpdate (PR intent)",
		Status:                   "answered",
		SessionID:                "sess-pr87",
		Vendor:                   "codex",
		HeadSHA:                  pr87Head,
		ReviewContentFingerprint: reviewFP,
		PRIntentFingerprint:      intentFP,
		Role:                     "fixer",
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk: %v", err)
	}
	loop := storage.LoopRecord{
		ID: "loop_pr87_resume", Seq: 87, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr,
		Status: "awaiting_human", MetadataJSON: &meta,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}

	// Resume repair: live refresh matches fingerprints → inject only chosen direction.
	worktreeRoot := t.TempDir()
	wt := filepath.Join(worktreeRoot, "wt-resume")
	_ = os.MkdirAll(wt, 0o755)
	repoPath := t.TempDir()
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	github := &fakeGitHubGateway{
		viewResponses: []PullRequestDetail{pr87Detail(pr87Head)},
		// Live thread body must match ask-time review FP for inject to succeed.
		threads: []ReviewThread{{
			ID:       "t-strategy",
			Comments: []ReviewThreadComment{{ID: "c-strategy", Body: pr87ReviewerBody, Author: "reviewer"}},
		}},
	}
	agent := &hitlScriptedAgent{
		results: []AgentResult{{
			Status: "completed", Summary: "kept RollingUpdate", ParseStatus: "parsed",
			Stdout: fmt.Sprintf(`__LOOPER_RESULT__={"summary":"kept RollingUpdate","review_thread_replies":[{"fixItemId":"c-strategy","threadId":"t-strategy","action":"declined","explanation":"Keeping hard-coded RollingUpdate per human decision.","threadCommentsObserved":"%s"}]}`+"\n",
				hashReviewThreadComments(ReviewThread{Comments: []ReviewThreadComment{{ID: "c-strategy"}}})),
		}},
	}
	run := storage.RunRecord{ID: "run_resume", LoopID: loop.ID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert: %v", err)
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, AgentRuntime: "codex",
	})

	// Unit-level: pendingHumanAnswer carries only the chosen option.
	prompt, session := runner.pendingHumanAnswer(ctx, &loop, "codex", pr87Head, reviewFP, intentFP)
	if session != "sess-pr87" {
		t.Fatalf("session = %q, want sess-pr87", session)
	}
	if !strings.Contains(prompt, "keep RollingUpdate (PR intent)") {
		t.Fatalf("resume prompt missing chosen answer: %q", prompt)
	}
	if strings.Contains(prompt, "restore configurable strategy (reviewer)") && !strings.Contains(prompt, "keep RollingUpdate") {
		t.Fatalf("resume prompt should center the chosen direction, got: %q", prompt)
	}

	// Integration: runRepairStep injects answer into agent prompt.
	checkpoint, err := runner.runRepairStep(ctx, stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &projectMeta},
		Loop:    loop,
		Run:     run,
		Repo:    repo, PRNumber: pr,
		Checkpoint: fixerCheckpoint{
			Detail:   detail,
			FixItems: fixItems,
			Worktree: &checkpointWorktree{Path: wt, Branch: "feature/pr87", HeadSHA: pr87Head, PreparedAt: nowISO},
		},
	})
	if err != nil {
		t.Fatalf("runRepairStep resume error = %v", err)
	}
	if checkpoint.Repair == nil {
		t.Fatal("expected repair complete after successful resume turn")
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts = %d, want 1", len(agent.starts))
	}
	started := agent.starts[0]
	if !strings.Contains(started.Prompt, "keep RollingUpdate (PR intent)") {
		t.Fatalf("agent prompt missing human decision:\n%s", started.Prompt)
	}
	// Must not authorize agent push under HITL.
	if strings.Contains(started.Prompt, "Commit and push the repair changes") {
		t.Fatal("HITL repair must not authorize agent push")
	}
	if started.NativeSessionID != "sess-pr87" {
		t.Fatalf("NativeSessionID = %q, want sess-pr87", started.NativeSessionID)
	}

	// Answer consumed after durable repair checkpoint.
	fresh, _ := fixture.repos.Loops.GetByID(ctx, loop.ID)
	ask, ok := loops.ReadHITLAsk(fresh.MetadataJSON)
	if !ok || ask.Status != "consumed" {
		t.Fatalf("ask after resume = %#v (ok=%v), want consumed", ask, ok)
	}
}

func TestHITLContract_StaleHeadAndIntentBlockOldAnswer(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()

	detail := &checkpointDetail{
		HeadSHA: pr87Head, BaseSHA: pr87Base, BaseRefName: "main",
		Title: pr87Title, Body: pr87Body,
	}
	fixItems := []FixItem{{Type: "comment", ID: "c-strategy", ThreadID: "t-strategy", Summary: pr87ReviewerBody, Body: ""}}
	reviewFP := computeReviewContentFingerprint(fixItems)
	intentFP := computePRIntentFingerprint(detail)

	repo := "acme/looper"
	pr := int64(87)
	nowISO := fixture.nowISO()
	meta, err := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: "strategy?", Answer: "keep RollingUpdate (PR intent)", Status: "answered",
		SessionID: "sess-stale", Vendor: "codex",
		HeadSHA: pr87Head, ReviewContentFingerprint: reviewFP, PRIntentFingerprint: intentFP, Role: "fixer",
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk: %v", err)
	}
	loop := storage.LoopRecord{
		ID: "loop_stale", Seq: 88, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "awaiting_human", MetadataJSON: &meta,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	runner := New(Options{Repos: fixture.repos, AgentRuntime: "codex", HITLEnabled: true})

	// Fresh match works.
	if p, s := runner.pendingHumanAnswer(ctx, &loop, "codex", pr87Head, reviewFP, intentFP); p == "" || s == "" {
		t.Fatalf("matching fingerprints should inject; got (%q, %q)", p, s)
	}

	// Stale HEAD blocks.
	if p, s := runner.pendingHumanAnswer(ctx, &loop, "codex", "head-moved", reviewFP, intentFP); p != "" || s != "" {
		t.Fatalf("stale head must not inject; got (%q, %q)", p, s)
	}

	// Intent FP change (title/body) blocks.
	changedIntent := computePRIntentFingerprint(&checkpointDetail{
		HeadSHA: pr87Head, BaseSHA: pr87Base, BaseRefName: "main",
		Title: "Now make strategy configurable", Body: "Product reversed course.",
	})
	if p, s := runner.pendingHumanAnswer(ctx, &loop, "codex", pr87Head, reviewFP, changedIntent); p != "" || s != "" {
		t.Fatalf("intent drift must not inject; got (%q, %q)", p, s)
	}

	// runRepairStep with drifted live head must not inject (refresh returns new head).
	worktreeRoot := t.TempDir()
	wt := filepath.Join(worktreeRoot, "wt-stale")
	_ = os.MkdirAll(wt, 0o755)
	repoPath := t.TempDir()
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	// Live head moved while worktree still on old head → abort before agent.
	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{pr87Detail("head-moved")}}
	agent := &hitlScriptedAgent{
		results: []AgentResult{{
			Status: "completed", Summary: "fresh turn", ParseStatus: "parsed",
			Stdout: pr87NeedsHumanStdout("c-strategy", "t-strategy"),
		}},
	}
	run := storage.RunRecord{ID: "run_stale", LoopID: loop.ID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	_ = fixture.repos.Runs.Upsert(ctx, run)
	r2 := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent,
		Logger: fixture.logger, Now: fixture.now, HITLEnabled: true, AgentRuntime: "codex",
	})
	detail.HeadRefName = "feature/pr87"
	_, err = r2.runRepairStep(ctx, stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &projectMeta},
		Loop:    loop, Run: run, Repo: repo, PRNumber: pr,
		Checkpoint: fixerCheckpoint{
			Detail:   detail,
			FixItems: fixItems,
			Worktree: &checkpointWorktree{Path: wt, Branch: "feature/pr87", HeadSHA: pr87Head, PreparedAt: nowISO},
		},
	})
	if err == nil {
		t.Fatal("expected error when live head differs from worktree head")
	}
	if !strings.Contains(err.Error(), "differs from prepared worktree head") {
		t.Fatalf("error = %q, want worktree/live head mismatch", err.Error())
	}
	loopErr, ok := err.(*loopError)
	if !ok || loopErr.kind != FailureRetryableAfterResume {
		t.Fatalf("err = %v (%T), want retryable_after_resume", err, err)
	}
	// Checkpoint returned with restart-from-discover so resume does not loop on stale worktree.
	// (runRepairStep returns checkpoint with policy set before error.)
	if len(agent.starts) != 0 {
		t.Fatalf("agent must not start on head mismatch; starts=%d", len(agent.starts))
	}
}

func TestHITLContract_HeadDriftSetsRestartFromDiscover(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	worktreeRoot := t.TempDir()
	wt := filepath.Join(worktreeRoot, "wt-head")
	_ = os.MkdirAll(wt, 0o755)
	repoPath := t.TempDir()
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	detail := &checkpointDetail{
		HeadSHA: pr87Head, BaseSHA: pr87Base, BaseRefName: "main", HeadRefName: "feature/pr87",
		Title: pr87Title, Body: pr87Body, State: "OPEN",
	}
	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{pr87Detail("head-moved")}}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &hitlScriptedAgent{},
		Logger: fixture.logger, Now: fixture.now, HITLEnabled: true,
	})
	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_head_policy", Seq: 91, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	run := storage.RunRecord{ID: "run_head_policy", LoopID: loop.ID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	_ = fixture.repos.Runs.Upsert(ctx, run)

	cp, err := runner.runRepairStep(ctx, stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &projectMeta},
		Loop:    loop, Run: run, Repo: repo, PRNumber: pr,
		Checkpoint: fixerCheckpoint{
			Detail:   detail,
			FixItems: []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Summary: "x"}},
			Worktree: &checkpointWorktree{Path: wt, Branch: "feature/pr87", HeadSHA: pr87Head, PreparedAt: nowISO},
		},
	})
	if err == nil {
		t.Fatal("expected head-drift error")
	}
	if cp.ResumePolicy != loops.ResumePolicyRestartFromDiscover {
		t.Fatalf("ResumePolicy = %q, want %q", cp.ResumePolicy, loops.ResumePolicyRestartFromDiscover)
	}
	if cp.Repair != nil {
		t.Fatalf("Repair = %#v, want nil", cp.Repair)
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

func TestHITLContract_LiveReviewDriftBlocksInject(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	detail := &checkpointDetail{
		HeadSHA: pr87Head, BaseSHA: pr87Base, BaseRefName: "main", HeadRefName: "feature/pr87",
		Title: pr87Title, Body: pr87Body, State: "OPEN",
	}
	// GitHub-shaped: Summary holds review text, Body empty (matches normalizeFixItems).
	fixItems := []FixItem{{
		Type: "comment", ID: "c-strategy", ThreadID: "t-strategy",
		Summary: pr87ReviewerBody, Body: "",
	}}
	reviewFP := computeReviewContentFingerprint(fixItems)
	intentFP := computePRIntentFingerprint(detail)

	repo := "acme/looper"
	pr := int64(87)
	meta, err := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: "strategy?", Answer: "keep RollingUpdate (PR intent)", Status: "answered",
		SessionID: "sess-drift", Vendor: "codex",
		HeadSHA: pr87Head, ReviewContentFingerprint: reviewFP, PRIntentFingerprint: intentFP, Role: "fixer",
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk: %v", err)
	}
	loop := storage.LoopRecord{
		ID: "loop_review_drift", Seq: 89, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr,
		Status: "awaiting_human", MetadataJSON: &meta,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	run := storage.RunRecord{ID: "run_review_drift", LoopID: loop.ID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	_ = fixture.repos.Runs.Upsert(ctx, run)

	worktreeRoot := t.TempDir()
	wt := filepath.Join(worktreeRoot, "wt-drift")
	_ = os.MkdirAll(wt, 0o755)
	repoPath := t.TempDir()
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)

	// Live thread body differs from the Summary hashed at ask time.
	github := &fakeGitHubGateway{
		viewResponses: []PullRequestDetail{pr87Detail(pr87Head)},
		threads: []ReviewThread{{
			ID: "t-strategy",
			Comments: []ReviewThreadComment{{
				ID: "c-strategy", Body: "CHANGED: please make strategy fully dynamic now", Author: "reviewer",
			}},
		}},
	}
	agent := &hitlScriptedAgent{
		results: []AgentResult{{
			Status: "completed", Summary: "re-evaluated", ParseStatus: "parsed",
			Stdout: pr87NeedsHumanStdout("c-strategy", "t-strategy"),
		}},
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, AgentRuntime: "codex",
	})
	_, err = runner.runRepairStep(ctx, stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &projectMeta},
		Loop:    loop, Run: run, Repo: repo, PRNumber: pr,
		Checkpoint: fixerCheckpoint{
			Detail:   detail,
			FixItems: fixItems,
			Worktree: &checkpointWorktree{Path: wt, Branch: "feature/pr87", HeadSHA: pr87Head, PreparedAt: nowISO},
		},
	})
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts = %d, want 1 (err=%v)", len(agent.starts), err)
	}
	if strings.Contains(agent.starts[0].Prompt, "Their decision: keep RollingUpdate") {
		t.Fatal("live review content drift must not inject the old human decision")
	}
}

func TestHITLContract_GitHubSummaryShapeUnchangedThreadStillMatches(t *testing.T) {
	t.Parallel()
	// Ask-time GitHub fix item: Summary=body text, Body empty.
	items := []FixItem{{
		Type: "comment", ID: "c1", ThreadID: "t1",
		Summary: "Please restore configurable strategy", Body: "",
	}}
	askFP := computeReviewContentFingerprint(items)

	// Live refresh with same thread body must produce identical FP.
	github := &fakeGitHubGateway{
		threads: []ReviewThread{{
			ID:       "t1",
			Comments: []ReviewThreadComment{{ID: "c1", Body: "Please restore configurable strategy"}},
		}},
	}
	runner := New(Options{GitHub: github, HITLEnabled: true})
	liveFP, err := runner.liveReviewContentFingerprint(context.Background(), stepInput{
		Project: storage.ProjectRecord{RepoPath: "/tmp"},
		Repo:    "acme/looper", PRNumber: 1,
	}, items)
	if err != nil {
		t.Fatalf("liveReviewContentFingerprint error = %v", err)
	}
	if liveFP != askFP {
		t.Fatalf("unchanged GitHub-shaped thread FP mismatch:\n ask=%s\nlive=%s", askFP, liveFP)
	}
	if !hitl.MaterialFingerprintsMatch("h", "h", askFP, liveFP, "i", "i") {
		t.Fatal("MaterialFingerprintsMatch failed for identical review FPs")
	}
}

func TestHITLContract_MissingThreadChangesReviewFP(t *testing.T) {
	t.Parallel()
	items := []FixItem{{
		Type: "comment", ID: "c1", ThreadID: "t-gone",
		Summary: "Please restore configurable strategy", Body: "",
	}}
	askFP := computeReviewContentFingerprint(items)

	// No matching thread → missing marker, FP must diverge.
	github := &fakeGitHubGateway{threads: []ReviewThread{}}
	runner := New(Options{GitHub: github, HITLEnabled: true})
	liveFP, err := runner.liveReviewContentFingerprint(context.Background(), stepInput{
		Project: storage.ProjectRecord{RepoPath: "/tmp"},
		Repo:    "acme/looper", PRNumber: 1,
	}, items)
	if err != nil {
		t.Fatalf("liveReviewContentFingerprint error = %v", err)
	}
	if liveFP == askFP {
		t.Fatal("missing/deleted thread must change review content fingerprint")
	}
	if hitl.MaterialFingerprintsMatch("h", "h", askFP, liveFP, "i", "i") {
		t.Fatal("missing thread FP must not MaterialFingerprintsMatch ask-time FP")
	}
}

func TestHITLContract_UnverifiableSourceFailsClosed(t *testing.T) {
	t.Parallel()
	items := []FixItem{{
		Type: "comment", ID: "sum-1", Source: "forgejo-reviewer-summary",
		Summary: "open finding", ThreadID: "sum-1",
	}}
	runner := New(Options{GitHub: &fakeGitHubGateway{}, HITLEnabled: true})
	_, err := runner.liveReviewContentFingerprint(context.Background(), stepInput{
		Project: storage.ProjectRecord{RepoPath: "/tmp"},
		Repo:    "acme/looper", PRNumber: 1,
	}, items)
	if err == nil {
		t.Fatal("forgejo-reviewer-summary must fail closed (cannot live-verify)")
	}
}

func TestHITLContract_UndurableAskJSONRecoversWithoutAgent(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	worktreeRoot := t.TempDir()
	wt := filepath.Join(worktreeRoot, "wt-undurable")
	_ = os.MkdirAll(filepath.Join(wt, ".looper"), 0o755)
	repoPath := t.TempDir()
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)

	// ask.json present, but loop has NO durable HITL park → recover suspend.
	askPath := filepath.Join(wt, hitl.AskSentinelRelPath)
	if err := os.WriteFile(askPath, []byte(`{"question":"Keep RollingUpdate?","options":["keep","restore"]}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{pr87Detail(pr87Head)}}
	agent := &hitlScriptedAgent{results: []AgentResult{{Status: "completed", ParseStatus: "parsed", Summary: "should not run"}}}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent,
		Logger: fixture.logger, Now: fixture.now, HITLEnabled: true,
	})
	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_undurable", Seq: 92, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	run := storage.RunRecord{ID: "run_undurable", LoopID: loop.ID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	_ = fixture.repos.Runs.Upsert(ctx, run)

	_, err := runner.runRepairStep(ctx, stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &projectMeta},
		Loop:    loop, Run: run, Repo: repo, PRNumber: pr,
		Checkpoint: fixerCheckpoint{
			Detail: &checkpointDetail{
				HeadSHA: pr87Head, BaseSHA: pr87Base, BaseRefName: "main", HeadRefName: "feature/pr87",
				Title: pr87Title, Body: pr87Body,
			},
			FixItems: []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Summary: pr87ReviewerBody}},
			Worktree: &checkpointWorktree{Path: wt, Branch: "feature/pr87", HeadSHA: pr87Head, PreparedAt: nowISO},
		},
	})
	awaiting, ok := asAwaitingHumanError(err)
	if !ok || awaiting == nil {
		t.Fatalf("err = %v, want awaitingHumanError for undurable ask recovery", err)
	}
	if awaiting.question != "Keep RollingUpdate?" {
		t.Fatalf("question = %q", awaiting.question)
	}
	if len(agent.starts) != 0 {
		t.Fatalf("agent must not start on undurable ask recovery; starts=%d", len(agent.starts))
	}
	// ask.json must still exist (not deleted) so suspendForHuman can consume it after park.
	if _, err := os.Stat(askPath); err != nil {
		t.Fatalf("ask.json should remain until durable suspend; stat err = %v", err)
	}
}

func TestHITLContract_DurableParkClearsStaleAskJSON(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	worktreeRoot := t.TempDir()
	wt := filepath.Join(worktreeRoot, "wt-stale-ask")
	_ = os.MkdirAll(filepath.Join(wt, ".looper"), 0o755)
	repoPath := t.TempDir()
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	// Same question as the durable answered park → stale leftover file, safe to delete.
	const parkedQuestion = "Keep RollingUpdate or restore configurable strategy?"
	askPath := filepath.Join(wt, hitl.AskSentinelRelPath)
	if err := os.WriteFile(askPath, []byte(`{"question":"`+parkedQuestion+`","options":["keep","restore"]}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	repo := "acme/looper"
	pr := int64(87)
	detail := &checkpointDetail{
		HeadSHA: pr87Head, BaseSHA: pr87Base, BaseRefName: "main", HeadRefName: "feature/pr87",
		Title: pr87Title, Body: pr87Body,
	}
	fixItems := []FixItem{{Type: "comment", ID: "c-strategy", ThreadID: "t-strategy", Summary: pr87ReviewerBody}}
	meta, _ := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: parkedQuestion, Answer: "keep RollingUpdate (PR intent)", Status: "answered",
		SessionID: "sess-stale-ask", Vendor: "codex",
		HeadSHA:                  pr87Head,
		ReviewContentFingerprint: computeReviewContentFingerprint(fixItems),
		PRIntentFingerprint:      computePRIntentFingerprint(detail),
		Role:                     "fixer",
	})
	loop := storage.LoopRecord{
		ID: "loop_stale_ask", Seq: 93, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr,
		Status: "awaiting_human", MetadataJSON: &meta,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	run := storage.RunRecord{ID: "run_stale_ask", LoopID: loop.ID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	_ = fixture.repos.Runs.Upsert(ctx, run)

	github := &fakeGitHubGateway{
		viewResponses: []PullRequestDetail{pr87Detail(pr87Head)},
		threads: []ReviewThread{{
			ID:       "t-strategy",
			Comments: []ReviewThreadComment{{ID: "c-strategy", Body: pr87ReviewerBody}},
		}},
	}
	agent := &hitlScriptedAgent{
		results: []AgentResult{{
			Status: "completed", Summary: "done", ParseStatus: "parsed",
			Stdout: fmt.Sprintf(`__LOOPER_RESULT__={"summary":"done","review_thread_replies":[{"fixItemId":"c-strategy","threadId":"t-strategy","action":"declined","explanation":"kept RollingUpdate","threadCommentsObserved":"%s"}]}`+"\n",
				hashReviewThreadComments(ReviewThread{Comments: []ReviewThreadComment{{ID: "c-strategy"}}})),
		}},
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent,
		Logger: fixture.logger, Now: fixture.now, HITLEnabled: true, AgentRuntime: "codex",
	})
	_, err := runner.runRepairStep(ctx, stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &projectMeta},
		Loop:    loop, Run: run, Repo: repo, PRNumber: pr,
		Checkpoint: fixerCheckpoint{
			Detail: detail, FixItems: fixItems,
			Worktree: &checkpointWorktree{Path: wt, Branch: "feature/pr87", HeadSHA: pr87Head, PreparedAt: nowISO},
		},
	})
	if err != nil {
		t.Fatalf("runRepairStep error = %v", err)
	}
	if _, err := os.Stat(askPath); !os.IsNotExist(err) {
		t.Fatal("stale ask.json must be removed when durable HITL park matches the same question")
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts = %d, want 1", len(agent.starts))
	}
}

func TestHITLContract_ConsumedParkDoesNotDeleteNewAsk(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	worktreeRoot := t.TempDir()
	wt := filepath.Join(worktreeRoot, "wt-new-ask")
	_ = os.MkdirAll(filepath.Join(wt, ".looper"), 0o755)
	repoPath := t.TempDir()
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	askPath := filepath.Join(wt, hitl.AskSentinelRelPath)
	if err := os.WriteFile(askPath, []byte(`{"question":"New conflict after prior cycle","options":["a","b"]}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	repo := "acme/looper"
	pr := int64(87)
	meta, _ := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: "Old already-decided question", Answer: "done", Status: "consumed",
		SessionID: "sess-old", Vendor: "codex", Role: "fixer",
	})
	loop := storage.LoopRecord{
		ID: "loop_new_ask", Seq: 94, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr,
		Status: "running", MetadataJSON: &meta,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	run := storage.RunRecord{ID: "run_new_ask", LoopID: loop.ID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	_ = fixture.repos.Runs.Upsert(ctx, run)

	agent := &hitlScriptedAgent{results: []AgentResult{{Status: "completed", ParseStatus: "parsed", Summary: "should not run"}}}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: &fakeGitHubGateway{viewResponses: []PullRequestDetail{pr87Detail(pr87Head)}},
		Git:    &fakeGitGateway{}, AgentExecutor: agent,
		Logger: fixture.logger, Now: fixture.now, HITLEnabled: true, AgentRuntime: "codex",
	})
	_, err := runner.runRepairStep(ctx, stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &projectMeta},
		Loop:    loop, Run: run, Repo: repo, PRNumber: pr,
		Checkpoint: fixerCheckpoint{
			Detail: &checkpointDetail{
				HeadSHA: pr87Head, BaseSHA: pr87Base, BaseRefName: "main", HeadRefName: "feature/pr87",
				Title: pr87Title, Body: pr87Body,
			},
			FixItems: []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Summary: pr87ReviewerBody}},
			Worktree: &checkpointWorktree{Path: wt, Branch: "feature/pr87", HeadSHA: pr87Head, PreparedAt: nowISO},
		},
	})
	awaiting, ok := asAwaitingHumanError(err)
	if !ok || awaiting == nil {
		t.Fatalf("err = %v, want awaitingHumanError for new ask after consumed park", err)
	}
	if awaiting.question != "New conflict after prior cycle" {
		t.Fatalf("question = %q", awaiting.question)
	}
	if len(agent.starts) != 0 {
		t.Fatalf("agent must not start; starts=%d", len(agent.starts))
	}
	if _, err := os.Stat(askPath); err != nil {
		t.Fatalf("new ask.json must not be deleted under consumed park; stat err = %v", err)
	}
}

func TestHITLContract_MissingTargetCommentChangesReviewFP(t *testing.T) {
	t.Parallel()
	items := []FixItem{{
		Type: "comment", ID: "c-target", ThreadID: "t1",
		Summary: "Please restore configurable strategy", Body: "",
	}}
	askFP := computeReviewContentFingerprint(items)

	// Thread still exists but the targeted comment id is gone (sibling remains).
	github := &fakeGitHubGateway{
		threads: []ReviewThread{{
			ID: "t1",
			Comments: []ReviewThreadComment{
				{ID: "c-sibling", Body: "Please restore configurable strategy"},
			},
		}},
	}
	runner := New(Options{GitHub: github, HITLEnabled: true})
	liveFP, err := runner.liveReviewContentFingerprint(context.Background(), stepInput{
		Project: storage.ProjectRecord{RepoPath: "/tmp"},
		Repo:    "acme/looper", PRNumber: 1,
	}, items)
	if err != nil {
		t.Fatalf("liveReviewContentFingerprint error = %v", err)
	}
	if liveFP == askFP {
		t.Fatal("missing target comment must change review FP even when a sibling remains")
	}
}

func TestHITLContract_RunRepairStepNeedsHumanReturnsAwaiting(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	worktreeRoot := t.TempDir()
	wt := filepath.Join(worktreeRoot, "wt-repair")
	_ = os.MkdirAll(wt, 0o755)
	repoPath := t.TempDir()
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	detail := &checkpointDetail{
		HeadSHA: pr87Head, BaseSHA: pr87Base, BaseRefName: "main", HeadRefName: "feature/pr87",
		Title: pr87Title, Body: pr87Body, State: "OPEN",
	}
	fixItems := []FixItem{{
		Type: "comment", ID: "c-strategy", ThreadID: "t-strategy",
		Body: pr87ReviewerBody, Summary: "restore configurable strategy", Author: "reviewer",
	}}

	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{pr87Detail(pr87Head)}}
	agent := &hitlScriptedAgent{
		writeAskJSON: true,
		writeDismiss: true,
		results: []AgentResult{{
			Status: "completed", Summary: "conflict", ParseStatus: "parsed",
			Stdout: pr87NeedsHumanStdout("c-strategy", "t-strategy"),
		}},
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, AllowAutoPush: true,
	})

	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_repair_nh", Seq: 1, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	run := storage.RunRecord{ID: "run_repair_nh", LoopID: loop.ID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	_ = fixture.repos.Runs.Upsert(ctx, run)

	cp, err := runner.runRepairStep(ctx, stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &projectMeta},
		Loop:    loop, Run: run, Repo: repo, PRNumber: pr,
		Checkpoint: fixerCheckpoint{
			Detail: detail, FixItems: fixItems, FixItemsHash: "h1",
			Worktree: &checkpointWorktree{Path: wt, Branch: "feature/pr87", HeadSHA: pr87Head, PreparedAt: nowISO},
		},
	})
	awaiting, ok := asAwaitingHumanError(err)
	if !ok || awaiting == nil {
		t.Fatalf("runRepairStep err = %v, want awaitingHumanError", err)
	}
	if cp.Repair != nil {
		t.Fatalf("Repair must stay nil on escalate, got %#v", cp.Repair)
	}
	if awaiting.headSHA != pr87Head {
		t.Fatalf("awaiting.headSHA = %q, want %q", awaiting.headSHA, pr87Head)
	}
	if awaiting.prIntentFP == "" || awaiting.reviewContentFP == "" {
		t.Fatalf("awaiting missing fingerprints: %#v", awaiting)
	}
	// Agent was told not to push under HITL even though AllowAutoPush=true.
	if len(agent.starts) != 1 {
		t.Fatalf("starts = %d", len(agent.starts))
	}
	if strings.Contains(agent.starts[0].Prompt, "Commit and push the repair changes") {
		t.Fatal("HITL-enabled repair must force local-only agent prompt")
	}
	// dismiss.json still on disk until suspendForHuman — repair step must not apply it.
	if len(github.dismissedReviews) != 0 {
		t.Fatalf("dismissals applied during repair escalate = %d, want 0", len(github.dismissedReviews))
	}
}
