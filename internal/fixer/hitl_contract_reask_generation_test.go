package fixer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/hitl"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// After resume binds the answered park to an execution, a same-text ask.json is a
// new generation and must re-suspend — not delete + re-inject the old answer.
func TestHITLContract_AnsweredResumeGenerationPreservesReAsk(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	worktreeRoot := t.TempDir()
	wt := filepath.Join(worktreeRoot, "wt-reask")
	_ = os.MkdirAll(filepath.Join(wt, ".looper"), 0o755)
	repoPath := t.TempDir()
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)

	const question = "Keep RollingUpdate or restore configurable strategy?"
	detail := &checkpointDetail{
		HeadSHA: pr87Head, BaseSHA: pr87Base, BaseRefName: "main", HeadRefName: "feature/pr87",
		Title: pr87Title, Body: pr87Body,
	}
	fixItems := []FixItem{{Type: "comment", ID: "c-strategy", ThreadID: "t-strategy", Summary: pr87ReviewerBody}}
	meta, _ := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: question, Answer: "keep RollingUpdate (PR intent)", Status: "answered",
		SessionID: "sess-reask", Vendor: "codex", ExecutionID: "agent-ask-gen1",
		// Resume already started — same-text file is a re-escalation.
		ResumeExecutionID:        "agent-resume-1",
		HeadSHA:                  pr87Head,
		ReviewContentFingerprint: computeReviewContentFingerprint(fixItems),
		PRIntentFingerprint:      computePRIntentFingerprint(detail),
		Role:                     "fixer",
	})
	askPath := filepath.Join(wt, hitl.AskSentinelRelPath)
	if err := os.WriteFile(askPath, []byte(`{"question":"`+question+`","options":["keep","restore"],"executionId":"agent-reask-2"}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_reask_gen", Seq: 103, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr,
		Status: "running", MetadataJSON: &meta,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	run := storage.RunRecord{ID: "run_reask_gen", LoopID: loop.ID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
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
			Detail: detail, FixItems: fixItems,
			Worktree: &checkpointWorktree{Path: wt, Branch: "feature/pr87", HeadSHA: pr87Head, PreparedAt: nowISO},
		},
	})
	awaiting, ok := asAwaitingHumanError(err)
	if !ok || awaiting == nil {
		t.Fatalf("err = %v, want awaitingHumanError for same-text re-escalation", err)
	}
	if awaiting.question != question {
		t.Fatalf("question = %q", awaiting.question)
	}
	if len(agent.starts) != 0 {
		t.Fatalf("agent must not start on re-escalation recovery; starts=%d", len(agent.starts))
	}
	if _, err := os.Stat(askPath); err != nil {
		t.Fatalf("re-ask ask.json must not be deleted; stat err = %v", err)
	}
}

func TestDurableHITLParkForAsk_ResumeGeneration(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	pr := int64(1)

	meta, _ := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: "Q?", Answer: "A", Status: "answered",
		ExecutionID: "exec-1", ResumeExecutionID: "resume-1", Role: "fixer",
	})
	loop := storage.LoopRecord{
		ID: "loop_gen_unit", Seq: 1, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr,
		Status: "running", MetadataJSON: &meta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	runner := New(Options{Repos: fixture.repos, HITLEnabled: true})

	if runner.durableHITLParkForAsk(ctx, &loop, &hitl.AskPayload{Question: "Q?", Options: []string{"a"}}) {
		t.Fatal("answered park with ResumeExecutionID must not match same-text ask")
	}

	// Clear resume stamp: leftover from original park generation still matches.
	meta2, _ := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: "Q?", Answer: "A", Status: "answered", ExecutionID: "exec-1", Role: "fixer",
	})
	loop.MetadataJSON = &meta2
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	if !runner.durableHITLParkForAsk(ctx, &loop, &hitl.AskPayload{Question: "Q?", Options: []string{"a"}}) {
		t.Fatal("answered park without resume stamp should match leftover same-text ask")
	}

	// Explicit execution id mismatch also blocks.
	if runner.durableHITLParkForAsk(ctx, &loop, &hitl.AskPayload{
		Question: "Q?", Options: []string{"a"}, ExecutionID: "exec-other",
	}) {
		t.Fatal("file execution id mismatch must not match parked generation")
	}
}
