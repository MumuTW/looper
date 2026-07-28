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

// TestHITLContract_ReopenedThreadDeclinedReplyInjectsAnswer covers the full
// second-reopen HITL lifecycle: ask-time ThreadFingerprint excludes a prior
// declined Looper reply, live refresh on the reopened thread still matches, and
// runRepairStep injects the parked human decision (not blocked by the marker).
func TestHITLContract_ReopenedThreadDeclinedReplyInjectsAnswer(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	const (
		rootUpdated     = "2026-07-28T00:00:00Z"
		declinedUpdated = "2026-07-28T01:00:00Z"
	)
	declinedBody := "Not acting: conflicts with PR intent.\n\n<!-- looper-fixer-reply-declined thread:t-strategy fingerprint:fp1 -->"
	// Collect-time ask FP: declined excluded (shared IsLooperFixerReplyBody authority).
	askThreadFP := "c-strategy@" + rootUpdated
	fixItems := []FixItem{{
		Type: "comment", ID: "c-strategy", ThreadID: "t-strategy",
		Summary: pr87ReviewerBody, Body: "",
		ThreadFingerprint: askThreadFP,
	}}
	detail := &checkpointDetail{
		HeadSHA: pr87Head, BaseSHA: pr87Base, BaseRefName: "main", HeadRefName: "feature/pr87",
		Title: pr87Title, Body: pr87Body, State: "OPEN",
	}
	reviewFP := computeReviewContentFingerprint(fixItems)
	intentFP := computePRIntentFingerprint(detail)

	meta, err := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: "Keep hard-coded RollingUpdate or restore configurable strategy?",
		Options:  []string{"keep RollingUpdate (PR intent)", "restore configurable strategy (reviewer)"},
		Answer:   "keep RollingUpdate (PR intent)", Status: "answered",
		SessionID: "sess-reopen-declined", Vendor: "codex",
		HeadSHA: pr87Head, ReviewContentFingerprint: reviewFP, PRIntentFingerprint: intentFP,
		Role: "fixer", ExecutionID: "agent-ask-reopen",
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk: %v", err)
	}
	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_reopen_declined", Seq: 609, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr,
		Status: "awaiting_human", MetadataJSON: &meta,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	run := storage.RunRecord{ID: "run_reopen_declined", LoopID: loop.ID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert: %v", err)
	}

	worktreeRoot := t.TempDir()
	wt := filepath.Join(worktreeRoot, "wt-reopen-declined")
	_ = os.MkdirAll(wt, 0o755)
	repoPath := t.TempDir()
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)

	// Live reopened thread still carries the prior declined Looper reply.
	github := &fakeGitHubGateway{
		viewResponses: []PullRequestDetail{pr87Detail(pr87Head)},
		threads: []ReviewThread{{
			ID: "t-strategy",
			Comments: []ReviewThreadComment{
				{ID: "c-strategy", Body: pr87ReviewerBody, Author: "reviewer", UpdatedAt: rootUpdated},
				{ID: "c-declined", Body: declinedBody, UpdatedAt: declinedUpdated},
			},
		}},
	}
	agent := &hitlScriptedAgent{
		results: []AgentResult{{
			Status: "completed", Summary: "kept RollingUpdate", ParseStatus: "parsed",
			Stdout: `__LOOPER_RESULT__={"summary":"kept RollingUpdate"}` + "\n",
		}},
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, AgentRuntime: "codex",
	})

	// Live refresh must match ask FP despite declined marker on the thread.
	liveFP, err := runner.liveReviewContentFingerprint(ctx, stepInput{
		Project: storage.ProjectRecord{RepoPath: repoPath},
		Repo:    repo, PRNumber: pr,
	}, fixItems)
	if err != nil {
		t.Fatalf("liveReviewContentFingerprint: %v", err)
	}
	if liveFP != reviewFP {
		t.Fatalf("reopened declined-reply thread FP mismatch:\n ask=%s\nlive=%s", reviewFP, liveFP)
	}

	// Unit: pendingHumanAnswer injects when fingerprints match after declined exclusion.
	prompt, session := runner.pendingHumanAnswer(ctx, &loop, "codex", pr87Head, liveFP, intentFP)
	if session != "sess-reopen-declined" || !strings.Contains(prompt, "keep RollingUpdate (PR intent)") {
		t.Fatalf("expected inject on reopened declined thread; prompt=%q session=%q", prompt, session)
	}

	// Integration: runRepairStep live-refreshes and injects the decision into the agent.
	checkpoint, err := runner.runRepairStep(ctx, stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &projectMeta},
		Loop:    loop, Run: run, Repo: repo, PRNumber: pr,
		Checkpoint: fixerCheckpoint{
			Detail: detail, FixItems: fixItems,
			Worktree: &checkpointWorktree{Path: wt, Branch: "feature/pr87", HeadSHA: pr87Head, PreparedAt: nowISO},
		},
	})
	if err != nil {
		t.Fatalf("runRepairStep: %v", err)
	}
	if checkpoint.Repair == nil {
		t.Fatal("expected repair complete after reopened-thread HITL inject")
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts = %d, want 1", len(agent.starts))
	}
	if !strings.Contains(agent.starts[0].Prompt, "keep RollingUpdate (PR intent)") {
		t.Fatalf("agent prompt missing human decision:\n%s", agent.starts[0].Prompt)
	}
	if agent.starts[0].NativeSessionID != "sess-reopen-declined" {
		t.Fatalf("NativeSessionID = %q, want sess-reopen-declined", agent.starts[0].NativeSessionID)
	}
	fresh, _ := fixture.repos.Loops.GetByID(ctx, loop.ID)
	ask, ok := loops.ReadHITLAsk(fresh.MetadataJSON)
	if !ok || ask.Status != "consumed" {
		t.Fatalf("ask after resume = %#v (ok=%v), want consumed", ask, ok)
	}
}
