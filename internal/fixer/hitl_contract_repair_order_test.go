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

// Conflicted PR + answered HITL resume must not re-run MergeBaseIntoWorktree
// (first merge left MERGE_HEAD; a second git merge aborts and discards work).
func TestHITLContract_ConflictResumeSkipsBaseMerge(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	worktreeRoot := t.TempDir()
	wt := filepath.Join(worktreeRoot, "wt-conflict-resume")
	_ = os.MkdirAll(wt, 0o755)
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
	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{pr87Detail(pr87Head)}}
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
		t.Fatalf("answered HITL conflict resume must skip MergeBaseIntoWorktree; calls=%d", len(git.mergeBaseCalls))
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts = %d, want 1", len(agent.starts))
	}
	if !contains(agent.starts[0].Prompt, "keep feature side") {
		t.Fatalf("agent prompt missing human decision:\n%s", agent.starts[0].Prompt)
	}
}

// parkHITLLoop must cancel queue work before the loop becomes answerable
// (awaiting_human). Publishing status first races /respond into a lost-wakeup.
func TestHITLContract_ParkCancelsQueueBeforeAwaitingHuman(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_park_order", Seq: 102, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	loopID := loop.ID
	projectID := loop.ProjectID
	queueItem := storage.QueueItemRecord{
		ID: "queue_park_order", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		DedupeKey: "fixer:park-order", Priority: storage.QueuePriorityFixer, AvailableAt: nowISO,
		MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Queue.Upsert(ctx, queueItem)
	run := storage.RunRecord{
		ID: "run_park_order", LoopID: loop.ID, Status: "running",
		CurrentStep: stringPtr(string(stepRepair)), StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Runs.Upsert(ctx, run)
	project, err := fixture.repos.Projects.GetByID(ctx, "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID: %v", err)
	}

	var notified []HITLAskNotification
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLAnswerTransport: "feishu",
		HITLNotify: func(_ context.Context, n HITLAskNotification) error {
			// At notify time, park must already be durable: cancelled queue + awaiting_human.
			got, gerr := fixture.repos.Loops.GetByID(ctx, loop.ID)
			if gerr != nil || got == nil || got.Status != "awaiting_human" {
				t.Errorf("at notify: loop status = %v err=%v, want awaiting_human", got, gerr)
			}
			q, qerr := fixture.repos.Queue.GetByID(ctx, queueItem.ID)
			if qerr != nil || q == nil || q.Status != "cancelled" {
				t.Errorf("at notify: queue status = %v err=%v, want cancelled", q, qerr)
			}
			notified = append(notified, n)
			return nil
		},
	})
	result, err := runner.suspendForHuman(ctx, stepInput{
		Project: *project, Loop: loop, Run: run, QueueItem: queueItem,
		Repo: repo, PRNumber: pr,
	}, run, fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: pr87Head}}, &awaitingHumanError{
		question: "park order?", options: []string{"a", "b"},
		sessionID: "sess-park", executionID: "agent-park", vendor: "codex",
	})
	if err != nil {
		t.Fatalf("suspendForHuman: %v", err)
	}
	if result.Status != "awaiting_human" {
		t.Fatalf("result.Status = %q", result.Status)
	}
	if len(notified) != 1 {
		t.Fatalf("notify calls = %d, want 1", len(notified))
	}
	// After park, no active queue remains for lost-wakeup requeue-skip.
	active, err := fixture.repos.Queue.FindActiveByLoopID(ctx, loop.ID)
	if err != nil {
		t.Fatalf("FindActiveByLoopID: %v", err)
	}
	if active != nil {
		t.Fatalf("active queue after park = %#v, want nil", active)
	}
}

func TestHITLContract_GitHubTransportNotifyOnly(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_notify_only", Seq: 104, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	loopID := loop.ID
	projectID := loop.ProjectID
	queueItem := storage.QueueItemRecord{
		ID: "queue_notify_only", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		DedupeKey: "fixer:notify-only", Priority: storage.QueuePriorityFixer, AvailableAt: nowISO,
		MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Queue.Upsert(ctx, queueItem)
	run := storage.RunRecord{
		ID: "run_notify_only", LoopID: loop.ID, Status: "running",
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Runs.Upsert(ctx, run)
	project, _ := fixture.repos.Projects.GetByID(ctx, "project_1")

	var notified []HITLAskNotification
	github := &fakeGitHubGateway{}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLAnswerTransport: "github",
		HITLNotify: func(_ context.Context, n HITLAskNotification) error {
			notified = append(notified, n)
			return nil
		},
	})
	_, err := runner.suspendForHuman(ctx, stepInput{
		Project: *project, Loop: loop, Run: run, QueueItem: queueItem,
		Repo: repo, PRNumber: pr,
	}, run, fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: pr87Head}}, &awaitingHumanError{
		question: "github notify only?", options: []string{"a", "b"},
		sessionID: "sess-gh", executionID: "agent-gh", vendor: "codex",
	})
	if err != nil {
		t.Fatalf("suspendForHuman: %v", err)
	}
	if len(notified) != 1 || !notified[0].NotifyOnly {
		t.Fatalf("github transport notification = %#v, want NotifyOnly=true", notified)
	}
}

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
