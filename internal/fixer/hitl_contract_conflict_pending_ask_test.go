package fixer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/hitl"
	"github.com/nexu-io/looper/internal/storage"
)

// Delivery-failure rollback leaves ask.json + MERGE_HEAD while clearing the
// durable park. Repair must recover the pending ask BEFORE re-merging base, or
// MergeBaseIntoWorktree fails into manual intervention and never re-parks.
func TestHITLContract_ConflictPendingAskRecoversBeforeReMerge(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	worktreeRoot := t.TempDir()
	wt := filepath.Join(worktreeRoot, "wt-conflict-pending-ask")
	_ = os.MkdirAll(filepath.Join(wt, ".git"), 0o755)
	_ = os.MkdirAll(filepath.Join(wt, ".looper"), 0o755)
	if err := os.WriteFile(filepath.Join(wt, ".git", "MERGE_HEAD"), []byte("abc\n"), 0o644); err != nil {
		t.Fatalf("write MERGE_HEAD: %v", err)
	}
	askPath := filepath.Join(wt, hitl.AskSentinelRelPath)
	if err := os.WriteFile(askPath, []byte(`{"question":"How should conflict resolution proceed?","options":["keep feature","take base"]}`), 0o644); err != nil {
		t.Fatalf("write ask.json: %v", err)
	}
	repoPath := t.TempDir()
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)

	detail := &checkpointDetail{
		HeadSHA: pr87Head, BaseSHA: pr87Base, BaseRefName: "main", HeadRefName: "feature/pr87",
		Title: pr87Title, Body: pr87Body, State: "OPEN", HasConflicts: true,
	}
	fixItems := []FixItem{{Type: "conflict", Summary: "merge conflict with main"}}
	repo := "acme/looper"
	pr := int64(87)
	// No durable park: models post-delivery-failure rollback.
	loop := storage.LoopRecord{
		ID: "loop_conflict_pending_ask", Seq: 301, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	run := storage.RunRecord{ID: "run_conflict_pending_ask", LoopID: loop.ID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	_ = fixture.repos.Runs.Upsert(ctx, run)

	git := &fakeGitGateway{mergeBaseResult: MergeBaseResult{Conflicted: true}}
	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{pr87Detail(pr87Head)}}
	agent := &hitlScriptedAgent{results: []AgentResult{{Status: "completed", ParseStatus: "parsed", Summary: "should not run"}}}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: github, Git: git, AgentExecutor: agent,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, AllowRiskyFixes: true, AgentRuntime: "codex",
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
		t.Fatalf("err = %v, want awaitingHumanError for pending conflict ask recovery", err)
	}
	if awaiting.question != "How should conflict resolution proceed?" {
		t.Fatalf("question = %q", awaiting.question)
	}
	if len(git.mergeBaseCalls) != 0 {
		t.Fatalf("pending ask recovery must skip MergeBaseIntoWorktree; calls=%d", len(git.mergeBaseCalls))
	}
	if len(agent.starts) != 0 {
		t.Fatalf("agent must not start when recovering pending ask; starts=%d", len(agent.starts))
	}
	if _, err := os.Stat(askPath); err != nil {
		t.Fatalf("ask.json must remain for suspendForHuman; stat err = %v", err)
	}
}
