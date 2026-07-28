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
