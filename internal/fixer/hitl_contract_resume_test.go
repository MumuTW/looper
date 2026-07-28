package fixer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestHITLContract_HumanAnswerInjectedOnResume(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()

	detail := &checkpointDetail{
		HeadSHA: pr87Head, BaseSHA: pr87Base, BaseRefName: "main", HeadRefName: "feature/pr87",
		Title: pr87Title, Body: pr87Body, State: "OPEN",
	}
	// GitHub collect-time shape: review text in Summary, Body empty; ThreadFingerprint
	// covers the full non-Looper reply chain so mid-park replies can invalidate.
	const threadUpdated = "2026-07-28T00:00:00Z"
	fixItems := []FixItem{{
		Type: "comment", ID: "c-strategy", ThreadID: "t-strategy",
		Summary: pr87ReviewerBody, Body: "",
		ThreadFingerprint: "c-strategy@" + threadUpdated,
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
		// Live thread body + fingerprint must match ask-time review FP for inject.
		threads: []ReviewThread{{
			ID: "t-strategy",
			Comments: []ReviewThreadComment{{
				ID: "c-strategy", Body: pr87ReviewerBody, Author: "reviewer", UpdatedAt: threadUpdated,
			}},
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
