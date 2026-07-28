package fixer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// Conflicted PR + answered HITL resume must not re-run MergeBaseIntoWorktree
// when the retained worktree still has MERGE_HEAD (first merge left markers).
func TestHITLContract_ConflictResumeSkipsBaseMerge(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	worktreeRoot := t.TempDir()
	wt := filepath.Join(worktreeRoot, "wt-conflict-resume")
	_ = os.MkdirAll(filepath.Join(wt, ".git"), 0o755)
	// Simulate retained in-progress merge from the first repair attempt.
	if err := os.WriteFile(filepath.Join(wt, ".git", "MERGE_HEAD"), []byte("abc\n"), 0o644); err != nil {
		t.Fatalf("write MERGE_HEAD: %v", err)
	}
	repoPath := t.TempDir()
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)

	detail := &checkpointDetail{
		HeadSHA: pr87Head, BaseSHA: pr87Base, BaseRefName: "main", HeadRefName: "feature/pr87",
		Title: pr87Title, Body: pr87Body, State: "OPEN", HasConflicts: true,
	}
	// Conflict-only scope: no review-thread live-refresh requirements.
	fixItems := []FixItem{{Type: "conflict", Summary: "merge conflict with main"}}
	intentFP := computePRIntentFingerprint(detail)
	reviewFP := computeReviewContentFingerprint(fixItems)
	meta, err := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: "How should conflict resolution proceed?", Answer: "keep feature side",
		Status: "answered", SessionID: "sess-conflict", Vendor: "codex",
		HeadSHA: pr87Head, ReviewContentFingerprint: reviewFP, PRIntentFingerprint: intentFP,
		Role: "fixer", ExecutionID: "agent-ask-1",
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk: %v", err)
	}
	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_conflict_resume", Seq: 101, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr,
		Status: "awaiting_human", MetadataJSON: &meta,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	run := storage.RunRecord{ID: "run_conflict_resume", LoopID: loop.ID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	_ = fixture.repos.Runs.Upsert(ctx, run)

	git := &fakeGitGateway{}
	// Empty threads (non-nil): pure conflict park with no open review surfaces.
	// Avoid pr87Detail.Comments leaking into ListReviewThreads via viewIndex.
	github := &fakeGitHubGateway{
		viewResponses: []PullRequestDetail{{
			Number: 87, Title: pr87Title, Body: pr87Body, State: "OPEN",
			HeadSHA: pr87Head, HeadRefName: "feature/pr87", BaseRefName: "main", BaseSHA: pr87Base,
			HasConflicts: true,
		}},
		threads: []ReviewThread{},
	}
	agent := &hitlScriptedAgent{
		results: []AgentResult{{
			Status: "completed", Summary: "resolved", ParseStatus: "parsed",
			Stdout: `__LOOPER_RESULT__={"summary":"resolved"}` + "\n",
		}},
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: github, Git: git, AgentExecutor: agent,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, AllowRiskyFixes: true, AgentRuntime: "codex",
	})
	_, err = runner.runRepairStep(ctx, stepInput{
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
	if len(git.mergeBaseCalls) != 0 {
		t.Fatalf("answered HITL conflict resume must skip MergeBaseIntoWorktree when MERGE_HEAD present; calls=%d", len(git.mergeBaseCalls))
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts = %d, want 1", len(agent.starts))
	}
	if !contains(agent.starts[0].Prompt, "keep feature side") {
		t.Fatalf("agent prompt missing human decision:\n%s", agent.starts[0].Prompt)
	}
}

// Replaced worktree (no MERGE_HEAD) on answered conflict resume must re-run
// MergeBaseIntoWorktree so the agent still sees conflict markers.
func TestHITLContract_ConflictResumeReplacedWorktreeReMerges(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	worktreeRoot := t.TempDir()
	wt := filepath.Join(worktreeRoot, "wt-conflict-replaced")
	_ = os.MkdirAll(wt, 0o755) // fresh worktree: no .git/MERGE_HEAD
	repoPath := t.TempDir()
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)

	detail := &checkpointDetail{
		HeadSHA: pr87Head, BaseSHA: pr87Base, BaseRefName: "main", HeadRefName: "feature/pr87",
		Title: pr87Title, Body: pr87Body, State: "OPEN", HasConflicts: true,
	}
	fixItems := []FixItem{{Type: "conflict", Summary: "merge conflict with main"}}
	intentFP := computePRIntentFingerprint(detail)
	reviewFP := computeReviewContentFingerprint(fixItems)
	meta, err := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: "How should conflict resolution proceed?", Answer: "keep feature side",
		Status: "answered", SessionID: "sess-conflict-repl", Vendor: "codex",
		HeadSHA: pr87Head, ReviewContentFingerprint: reviewFP, PRIntentFingerprint: intentFP,
		Role: "fixer", ExecutionID: "agent-ask-repl",
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk: %v", err)
	}
	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_conflict_replaced", Seq: 102, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr,
		Status: "awaiting_human", MetadataJSON: &meta,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	run := storage.RunRecord{ID: "run_conflict_replaced", LoopID: loop.ID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	_ = fixture.repos.Runs.Upsert(ctx, run)

	git := &fakeGitGateway{mergeBaseResult: MergeBaseResult{Conflicted: true}}
	github := &fakeGitHubGateway{
		viewResponses: []PullRequestDetail{{
			Number: 87, Title: pr87Title, Body: pr87Body, State: "OPEN",
			HeadSHA: pr87Head, HeadRefName: "feature/pr87", BaseRefName: "main", BaseSHA: pr87Base,
			HasConflicts: true,
		}},
		threads: []ReviewThread{},
	}
	agent := &hitlScriptedAgent{
		results: []AgentResult{{
			Status: "completed", Summary: "resolved", ParseStatus: "parsed",
			Stdout: `__LOOPER_RESULT__={"summary":"resolved"}` + "\n",
		}},
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: github, Git: git, AgentExecutor: agent,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, AllowRiskyFixes: true, AgentRuntime: "codex",
	})
	_, err = runner.runRepairStep(ctx, stepInput{
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
	if len(git.mergeBaseCalls) != 1 {
		t.Fatalf("replaced worktree answered resume must re-run MergeBaseIntoWorktree; calls=%d", len(git.mergeBaseCalls))
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts = %d, want 1", len(agent.starts))
	}
	if !contains(agent.starts[0].Prompt, "keep feature side") {
		t.Fatalf("agent prompt missing human decision:\n%s", agent.starts[0].Prompt)
	}
}

// Fingerprint drift on a conflicted PR must abort the retained MERGE_HEAD before
// restart_from_discover so the next prepare does not treat obsolete merge dirt
// as manual intervention.
func TestHITLContract_ConflictDriftAbortsMergeBeforeRediscover(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	worktreeRoot := t.TempDir()
	wt := filepath.Join(worktreeRoot, "wt-conflict-drift")
	_ = os.MkdirAll(filepath.Join(wt, ".git"), 0o755)
	if err := os.WriteFile(filepath.Join(wt, ".git", "MERGE_HEAD"), []byte("abc\n"), 0o644); err != nil {
		t.Fatalf("write MERGE_HEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "conflicted.go"), []byte("<<<<<<< HEAD\na\n=======\nb\n>>>>>>> base\n"), 0o644); err != nil {
		t.Fatalf("write conflicted.go: %v", err)
	}
	repoPath := t.TempDir()
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)

	detail := &checkpointDetail{
		HeadSHA: pr87Head, BaseSHA: pr87Base, BaseRefName: "main", HeadRefName: "feature/pr87",
		Title: pr87Title, Body: pr87Body, State: "OPEN", HasConflicts: true,
	}
	// Conflict park that also scoped a review thread; a new top-level thread
	// mid-park changes the live review fingerprint and forces rediscovery.
	const askUpdated = "2026-07-28T00:00:00Z"
	fixItems := []FixItem{
		{Type: "conflict", Summary: "merge conflict with main"},
		{
			Type: "comment", ID: "c-strategy", ThreadID: "t-strategy",
			Summary: pr87ReviewerBody, Body: "",
			ThreadFingerprint: "c-strategy@" + askUpdated,
		},
	}
	intentFP := computePRIntentFingerprint(detail)
	reviewFP := computeReviewContentFingerprint(fixItems)
	meta, err := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: "How should conflict resolution proceed?", Answer: "keep feature side",
		Status: "answered", SessionID: "sess-conflict-drift", Vendor: "codex",
		HeadSHA: pr87Head, ReviewContentFingerprint: reviewFP, PRIntentFingerprint: intentFP,
		Role: "fixer", ExecutionID: "agent-ask-drift",
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk: %v", err)
	}
	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_conflict_drift", Seq: 103, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr,
		Status: "awaiting_human", MetadataJSON: &meta,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	run := storage.RunRecord{ID: "run_conflict_drift", LoopID: loop.ID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	_ = fixture.repos.Runs.Upsert(ctx, run)

	git := &fakeGitGateway{}
	github := &fakeGitHubGateway{
		viewResponses: []PullRequestDetail{pr87Detail(pr87Head)},
		threads: []ReviewThread{
			{
				ID: "t-strategy",
				Comments: []ReviewThreadComment{{
					ID: "c-strategy", Body: pr87ReviewerBody, UpdatedAt: askUpdated,
				}},
			},
			// New top-level review thread while parked → review FP drift.
			{
				ID: "t-new-during-park",
				Comments: []ReviewThreadComment{{
					ID: "c-new", Body: "Also prefer strategy X", UpdatedAt: "2026-07-28T04:00:00Z", Author: "reviewer",
				}},
			},
		},
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: github, Git: git, AgentExecutor: &hitlScriptedAgent{},
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, AllowRiskyFixes: true, AgentRuntime: "codex",
	})
	cp, err := runner.runRepairStep(ctx, stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &projectMeta},
		Loop:    loop, Run: run, Repo: repo, PRNumber: pr,
		Checkpoint: fixerCheckpoint{
			Detail: detail, FixItems: fixItems,
			Worktree: &checkpointWorktree{Path: wt, Branch: "feature/pr87", HeadSHA: pr87Head, PreparedAt: nowISO},
		},
	})
	if err == nil {
		t.Fatal("expected fingerprint-drift abort before agent start")
	}
	if cp.ResumePolicy != loops.ResumePolicyRestartFromDiscover {
		t.Fatalf("ResumePolicy = %q, want %q", cp.ResumePolicy, loops.ResumePolicyRestartFromDiscover)
	}
	if len(git.abortMergeCalls) != 1 || git.abortMergeCalls[0] != wt {
		t.Fatalf("abortMergeCalls = %#v, want one abort of retained conflict worktree", git.abortMergeCalls)
	}
}
