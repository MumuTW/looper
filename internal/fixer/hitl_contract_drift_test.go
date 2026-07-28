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
	const askUpdated = "2026-07-28T00:00:00Z"
	fixItems := []FixItem{{
		Type: "comment", ID: "c-strategy", ThreadID: "t-strategy",
		Summary: pr87ReviewerBody, Body: "",
		ThreadFingerprint: "c-strategy@" + askUpdated,
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
				UpdatedAt: askUpdated,
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
