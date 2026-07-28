package fixer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

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
