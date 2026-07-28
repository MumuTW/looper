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

// When the PR is closed while awaiting a human, resume must skip before starting
// the agent (state/draft are not in the intent fingerprint).
func TestHITLContract_ClosedPRAbortsResumeBeforeAgent(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	worktreeRoot := t.TempDir()
	wt := filepath.Join(worktreeRoot, "wt-closed")
	_ = os.MkdirAll(filepath.Join(wt, ".looper"), 0o755)
	repoPath := t.TempDir()
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)

	repo := "acme/looper"
	pr := int64(87)
	detail := &checkpointDetail{
		State: "OPEN", HeadSHA: pr87Head, BaseSHA: pr87Base,
		BaseRefName: "main", HeadRefName: "feature/pr87",
		Title: pr87Title, Body: pr87Body,
	}
	fixItems := []FixItem{{
		Type: "comment", ID: "c1", ThreadID: "t1",
		Summary: pr87ReviewerBody, Body: "",
		ThreadFingerprint: "c1@",
	}}
	meta, _ := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: "keep?", Answer: "keep", Status: "answered",
		SessionID: "sess-closed", Vendor: "codex",
		HeadSHA:                  pr87Head,
		ReviewContentFingerprint: computeReviewContentFingerprint(fixItems),
		PRIntentFingerprint:      computePRIntentFingerprint(detail),
		Role:                     "fixer",
	})
	loop := storage.LoopRecord{
		ID: "loop_hitl_closed_pr", Seq: 401, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr,
		Status: "running", MetadataJSON: &meta,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	run := storage.RunRecord{
		ID: "run_hitl_closed_pr", LoopID: loop.ID, Status: "running",
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Runs.Upsert(ctx, run)

	closed := pr87Detail(pr87Head)
	closed.State = "CLOSED"
	github := &fakeGitHubGateway{
		viewResponses: []PullRequestDetail{closed},
		threads: []ReviewThread{{
			ID:       "t1",
			Comments: []ReviewThreadComment{{ID: "c1", Body: pr87ReviewerBody}},
		}},
	}
	agent := &hitlScriptedAgent{results: []AgentResult{{Status: "completed", ParseStatus: "parsed", Summary: "should not run"}}}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent,
		Logger: fixture.logger, Now: fixture.now, HITLEnabled: true, AgentRuntime: "codex",
	})
	checkpoint, err := runner.runRepairStep(ctx, stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &projectMeta},
		Loop:    loop, Run: run, Repo: repo, PRNumber: pr,
		Checkpoint: fixerCheckpoint{
			Detail:   detail,
			FixItems: fixItems,
			Worktree: &checkpointWorktree{Path: wt, Branch: "feature/pr87", HeadSHA: pr87Head, PreparedAt: nowISO},
		},
	})
	if err != nil {
		t.Fatalf("runRepairStep error = %v, want skip without error", err)
	}
	if checkpoint.SkipReason == "" {
		t.Fatal("closed PR must set SkipReason before agent start")
	}
	if len(agent.starts) != 0 {
		t.Fatalf("agent must not start on closed PR; starts=%d", len(agent.starts))
	}
	// Answered park must be retired so rediscovery/skip does not thrash.
	got, _ := fixture.repos.Loops.GetByID(ctx, loop.ID)
	ask, ok := loops.ReadHITLAsk(got.MetadataJSON)
	if !ok || ask.Status != "consumed" {
		t.Fatalf("answered park status = %q ok=%v, want consumed", ask.Status, ok)
	}
}

// Draft exclusion after park must also abort resume when IncludeDrafts is false.
func TestHITLContract_DraftPRAbortsResumeBeforeAgent(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	worktreeRoot := t.TempDir()
	wt := filepath.Join(worktreeRoot, "wt-draft")
	_ = os.MkdirAll(filepath.Join(wt, ".looper"), 0o755)
	repoPath := t.TempDir()
	projectMeta := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)

	repo := "acme/looper"
	pr := int64(87)
	detail := &checkpointDetail{
		State: "OPEN", IsDraft: false, HeadSHA: pr87Head, BaseSHA: pr87Base,
		BaseRefName: "main", HeadRefName: "feature/pr87",
		Title: pr87Title, Body: pr87Body,
	}
	fixItems := []FixItem{{
		Type: "comment", ID: "c1", ThreadID: "t1",
		Summary: pr87ReviewerBody, Body: "",
		ThreadFingerprint: "c1@",
	}}
	meta, _ := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: "keep?", Answer: "keep", Status: "answered",
		SessionID: "sess-draft", Vendor: "codex",
		HeadSHA:                  pr87Head,
		ReviewContentFingerprint: computeReviewContentFingerprint(fixItems),
		PRIntentFingerprint:      computePRIntentFingerprint(detail),
		Role:                     "fixer",
	})
	loop := storage.LoopRecord{
		ID: "loop_hitl_draft_pr", Seq: 402, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr,
		Status: "running", MetadataJSON: &meta,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	run := storage.RunRecord{
		ID: "run_hitl_draft_pr", LoopID: loop.ID, Status: "running",
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Runs.Upsert(ctx, run)

	draft := pr87Detail(pr87Head)
	draft.IsDraft = true
	github := &fakeGitHubGateway{
		viewResponses: []PullRequestDetail{draft},
		threads: []ReviewThread{{
			ID:       "t1",
			Comments: []ReviewThreadComment{{ID: "c1", Body: pr87ReviewerBody}},
		}},
	}
	agent := &hitlScriptedAgent{results: []AgentResult{{Status: "completed", ParseStatus: "parsed", Summary: "should not run"}}}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent,
		Logger: fixture.logger, Now: fixture.now, HITLEnabled: true, AgentRuntime: "codex",
		DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, IncludeDrafts: false},
	})
	checkpoint, err := runner.runRepairStep(ctx, stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &projectMeta},
		Loop:    loop, Run: run, Repo: repo, PRNumber: pr,
		Checkpoint: fixerCheckpoint{
			Detail:   detail,
			FixItems: fixItems,
			Worktree: &checkpointWorktree{Path: wt, Branch: "feature/pr87", HeadSHA: pr87Head, PreparedAt: nowISO},
		},
	})
	if err != nil {
		t.Fatalf("runRepairStep error = %v, want skip without error", err)
	}
	if checkpoint.SkipReason == "" {
		t.Fatal("draft PR must set SkipReason when IncludeDrafts is false")
	}
	if len(agent.starts) != 0 {
		t.Fatalf("agent must not start on draft PR; starts=%d", len(agent.starts))
	}
}
