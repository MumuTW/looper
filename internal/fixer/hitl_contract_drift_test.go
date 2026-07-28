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
	fixItems := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Summary: "x", ThreadFingerprint: "c1@2026-07-28T00:00:00Z"}}
	meta, err := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: "strategy?", Answer: "keep RollingUpdate", Status: "answered",
		SessionID: "sess-head", Vendor: "codex",
		HeadSHA: pr87Head, ReviewContentFingerprint: computeReviewContentFingerprint(fixItems),
		PRIntentFingerprint: computePRIntentFingerprint(detail), Role: "fixer",
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk: %v", err)
	}
	loop := storage.LoopRecord{
		ID: "loop_head_policy", Seq: 91, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "awaiting_human",
		MetadataJSON: &meta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	run := storage.RunRecord{ID: "run_head_policy", LoopID: loop.ID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	_ = fixture.repos.Runs.Upsert(ctx, run)

	cp, err := runner.runRepairStep(ctx, stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &projectMeta},
		Loop:    loop, Run: run, Repo: repo, PRNumber: pr,
		Checkpoint: fixerCheckpoint{
			Detail:   detail,
			FixItems: fixItems,
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
	// Answered park must be retired so post-rediscovery repair is not stuck on
	// stored HeadSHA vs new live head mismatch thrashing restart_from_discover.
	freshLoop, getErr := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if getErr != nil || freshLoop == nil {
		t.Fatalf("Loops.GetByID after head drift: (%v, %#v)", getErr, freshLoop)
	}
	ask, ok := loops.ReadHITLAsk(freshLoop.MetadataJSON)
	if !ok {
		t.Fatal("expected HITL ask metadata retained after head-drift retire")
	}
	if ask.Status != "consumed" {
		t.Fatalf("ask.Status = %q, want consumed after head-drift retire", ask.Status)
	}
	if runner.hasAnsweredHITLAsk(ctx, freshLoop) {
		t.Fatal("hasAnsweredHITLAsk must be false after head-drift retire")
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

	const liveBody = "CHANGED: please make strategy fully dynamic now"
	// Live thread body differs from the Summary hashed at ask time.
	github := &fakeGitHubGateway{
		viewResponses: []PullRequestDetail{pr87Detail(pr87Head), pr87Detail(pr87Head)},
		threads: []ReviewThread{{
			ID: "t-strategy",
			Comments: []ReviewThreadComment{{
				ID: "c-strategy", Body: liveBody, Author: "reviewer",
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
	project := storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &projectMeta}
	cp, err := runner.runRepairStep(ctx, stepInput{
		Project: project, Loop: loop, Run: run, Repo: repo, PRNumber: pr,
		Checkpoint: fixerCheckpoint{
			Detail:   detail,
			FixItems: fixItems,
			Worktree: &checkpointWorktree{Path: wt, Branch: "feature/pr87", HeadSHA: pr87Head, PreparedAt: nowISO},
		},
	})
	if err == nil {
		t.Fatal("expected review fingerprint drift to abort repair before agent start")
	}
	if len(agent.starts) != 0 {
		t.Fatalf("agent starts = %d, want 0 when review fingerprints drift (prompt must not run against stale FixItems)", len(agent.starts))
	}
	if cp.ResumePolicy != loops.ResumePolicyRestartFromDiscover {
		t.Fatalf("ResumePolicy = %q, want %q", cp.ResumePolicy, loops.ResumePolicyRestartFromDiscover)
	}
	if cp.Repair != nil {
		t.Fatalf("Repair = %#v, want nil so rediscovery rebuilds fix items", cp.Repair)
	}
	if !strings.Contains(err.Error(), "fingerprint drift") {
		t.Fatalf("error = %v, want fingerprint drift message", err)
	}

	// Stale answered decision must be retired before rediscovery; otherwise the next
	// repair still sees hasAnsweredHITLAsk and re-aborts on the same fingerprint gap.
	freshLoop, getErr := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if getErr != nil || freshLoop == nil {
		t.Fatalf("Loops.GetByID after drift: (%v, %#v)", getErr, freshLoop)
	}
	ask, ok := loops.ReadHITLAsk(freshLoop.MetadataJSON)
	if !ok {
		t.Fatal("expected HITL ask metadata retained after retire")
	}
	if ask.Status != "consumed" {
		t.Fatalf("ask.Status = %q, want consumed after fingerprint-drift retire", ask.Status)
	}
	if runner.hasAnsweredHITLAsk(ctx, freshLoop) {
		t.Fatal("hasAnsweredHITLAsk must be false after retiring invalidated answer")
	}

	// Second repair after rediscovery rebuilds FixItems from live content: must not
	// re-abort on fingerprint drift (no injectable answered park remains).
	rediscovered := []FixItem{{
		Type: "comment", ID: "c-strategy", ThreadID: "t-strategy",
		Summary: liveBody, Body: "",
		ThreadFingerprint: "c-strategy@" + askUpdated,
	}}
	agent.results = []AgentResult{{
		Status: "completed", Summary: "re-evaluated after rediscovery", ParseStatus: "parsed",
		Stdout: pr87NeedsHumanStdout("c-strategy", "t-strategy"),
	}}
	_, err2 := runner.runRepairStep(ctx, stepInput{
		Project: project, Loop: *freshLoop, Run: run, Repo: repo, PRNumber: pr,
		Checkpoint: fixerCheckpoint{
			Detail:   detail,
			FixItems: rediscovered,
			Worktree: &checkpointWorktree{Path: wt, Branch: "feature/pr87", HeadSHA: pr87Head, PreparedAt: nowISO},
		},
	})
	// May park for needs_human or complete; must not re-hit the fingerprint-drift abort.
	if err2 != nil && strings.Contains(err2.Error(), "fingerprint drift") {
		t.Fatalf("second repair after rediscovery must not re-abort on fingerprint drift: %v", err2)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts after rediscovery repair = %d, want 1 (fresh agent turn without stale answer inject)", len(agent.starts))
	}
	if strings.Contains(agent.starts[0].Prompt, "keep RollingUpdate (PR intent)") {
		t.Fatal("stale human answer must not be injected after drift retire + rediscovery")
	}
}
