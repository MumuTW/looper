package fixer

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestFixerHITLParksResumesAndConsumesAnswer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newRunnerFixture(t)
	project, err := fixture.repos.Projects.GetByID(ctx, "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v)", project, err)
	}
	worktreeRoot := t.TempDir()
	worktreePath := filepath.Join(worktreeRoot, "wt")
	projectMetadata := `{"worktreeRoot":` + mustJSON(t, worktreeRoot) + `}`
	project.MetadataJSON = &projectMetadata

	repo := "acme/looper"
	prNumber := int64(42)
	projectID := project.ID
	loopID := "loop_fixer_hitl"
	targetID := buildPullRequestTargetID(repo, prNumber)
	loop := storage.LoopRecord{
		ID: loopID, Seq: 91, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &targetID, Repo: &repo,
		PRNumber: &prNumber, Status: "running",
		CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO(),
	}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	run1 := storage.RunRecord{
		ID: "run_fixer_hitl_1", LoopID: loopID, Status: "running",
		CurrentStep: stringPtr(string(stepRepair)), StartedAt: fixture.nowISO(),
		LastCompletedStep: stringPtr(string(stepPrepareWorktree)),
		CreatedAt:         fixture.nowISO(), UpdatedAt: fixture.nowISO(),
	}
	if err := fixture.repos.Runs.Upsert(ctx, run1); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	queue := storage.QueueItemRecord{
		ID: "queue_fixer_hitl_1", ProjectID: &projectID, LoopID: &loopID,
		Type: "fixer", TargetType: "pull_request", TargetID: targetID,
		Repo: &repo, PRNumber: &prNumber, DedupeKey: "fixer:hitl:test",
		Priority: storage.QueuePriorityFixer, Status: "running",
		AvailableAt: fixture.nowISO(), MaxAttempts: 3,
		CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO(),
	}
	if err := fixture.repos.Queue.Upsert(ctx, queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	sessionID := "session-fixer-hitl"
	if err := fixture.repos.AgentExecutions.Upsert(ctx, storage.AgentExecutionRecord{
		ID: "agent_fixer_hitl_1", ProjectID: &projectID, LoopID: &loopID,
		RunID: &run1.ID, Vendor: "codex", Status: "completed",
		NativeSessionID: &sessionID, StartedAt: fixture.nowISO(),
		CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO(),
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	needsHuman := `__LOOPER_RESULT__={"summary":"blocked on intent","review_thread_replies":[{"fixItemId":"c1","threadId":"t1","action":"needs_human","explanation":"The reviewer request conflicts with the documented PR intent; choose which authority should win."}]}`
	fixed := `__LOOPER_RESULT__={"summary":"applied decision","review_thread_replies":[{"fixItemId":"c1","threadId":"t1","action":"fixed","explanation":"Kept the documented PR behavior as directed."}]}`
	agent := &fakeAgentExecutor{results: []AgentResult{
		{Status: "completed", Summary: "blocked on intent", ParseStatus: "parsed", Stdout: needsHuman},
		{Status: "completed", Summary: "applied decision", ParseStatus: "parsed", Stdout: fixed},
	}}
	git := &fakeGitGateway{}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: &fakeGitHubGateway{}, Git: git,
		AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now,
		AgentRuntime: "codex", AllowAutoPush: true, HITLEnabled: true,
	})
	checkpoint := fixerCheckpoint{
		Detail:       &checkpointDetail{HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main"},
		FixItems:     []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Summary: "conflicting request"}},
		FixItemsHash: "fix-items-1",
		Worktree:     &checkpointWorktree{Path: worktreePath, Branch: "feature/fix-42", PreparedAt: fixture.nowISO()},
	}
	step := stepInput{Project: *project, Loop: loop, Run: run1, QueueItem: queue, Repo: repo, PRNumber: prNumber, Checkpoint: checkpoint}

	parkedCheckpoint, err := runner.runRepairStep(ctx, step)
	awaiting, ok := asAwaitingHumanError(err)
	if !ok {
		t.Fatalf("runRepairStep() error = %v, want awaitingHumanError", err)
	}
	if !strings.Contains(agent.starts[0].Prompt, "Do not push") || agent.starts[0].NativeSessionID != "" {
		t.Fatalf("initial agent input = %#v, want local-only fresh turn", agent.starts[0])
	}
	result, err := runner.suspendForHuman(ctx, step, run1, parkedCheckpoint, awaiting)
	if err != nil || result.Status != "awaiting_human" {
		t.Fatalf("suspendForHuman() = (%#v, %v)", result, err)
	}
	persistedLoop, _ := fixture.repos.Loops.GetByID(ctx, loopID)
	ask, ok := loops.ReadHITLAsk(persistedLoop.MetadataJSON)
	if !ok || ask.Status != "awaiting" || ask.SessionID != sessionID {
		t.Fatalf("parked ask = %#v, found=%v", ask, ok)
	}
	persistedQueue, _ := fixture.repos.Queue.GetByID(ctx, queue.ID)
	persistedRun, _ := fixture.repos.Runs.GetByID(ctx, run1.ID)
	if persistedQueue.Status != "cancelled" || persistedRun.Status != "interrupted" {
		t.Fatalf("parked states: queue=%s run=%s", persistedQueue.Status, persistedRun.Status)
	}

	ask.Answer = "Keep the documented PR intent."
	ask.Status = "answered"
	metadata, err := loops.WriteHITLAsk(persistedLoop.MetadataJSON, ask)
	if err != nil {
		t.Fatalf("WriteHITLAsk() error = %v", err)
	}
	persistedLoop.MetadataJSON = &metadata
	persistedLoop.Status = "running"
	if err := fixture.repos.Loops.Upsert(ctx, *persistedLoop); err != nil {
		t.Fatalf("Loops.Upsert(answer) error = %v", err)
	}
	resume, err := runner.createRunContext(ctx, *persistedLoop)
	if err != nil {
		t.Fatalf("createRunContext(resume) error = %v", err)
	}
	if !resume.Resumed || resume.StartStep != stepRepair ||
		resume.Checkpoint.Worktree == nil || resume.Checkpoint.Worktree.PreparedAt == "" {
		t.Fatalf("resume context = %#v, want direct repair with prepared worktree preserved", resume)
	}
	step.Loop = *persistedLoop
	step.Run = resume.Run
	step.Checkpoint = resume.Checkpoint
	resumed, err := runner.runRepairStep(ctx, step)
	if err != nil || resumed.Repair == nil {
		t.Fatalf("resumed runRepairStep() = (%#v, %v)", resumed.Repair, err)
	}
	if agent.starts[1].NativeSessionID != sessionID || !strings.Contains(agent.starts[1].NativeResumePrompt, "Keep the documented PR intent") {
		t.Fatalf("resume agent input = %#v", agent.starts[1])
	}
	finishedLoop, _ := fixture.repos.Loops.GetByID(ctx, loopID)
	consumed, _ := loops.ReadHITLAsk(finishedLoop.MetadataJSON)
	if consumed.Status != "consumed" {
		t.Fatalf("answer status = %q, want consumed", consumed.Status)
	}
	if len(git.prepareCalls) != 0 {
		t.Fatalf("PrepareWorktree calls = %d, want no reset-capable prepare on HITL resume", len(git.prepareCalls))
	}
}

func TestValidateNeedsHumanRepliesRejectsMixedValidAndUnboundEntries(t *testing.T) {
	t.Parallel()
	stdout := `__LOOPER_RESULT__={"review_thread_replies":[` +
		`{"fixItemId":"c1","threadId":"t1","action":"needs_human","explanation":"Choose the intended behavior."},` +
		`{"fixItemId":"stale","threadId":"old","action":"needs_human","explanation":"Stale question."}` +
		`]}`
	items := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1"}}
	accepted := normalizeReplyExplanationActions(parseReplyExplanations(stdout, "", items))
	if len(accepted) != 1 {
		t.Fatalf("accepted replies = %#v, want only the bound entry", accepted)
	}
	if err := validateNeedsHumanReplies(stdout, "", items, accepted); err == nil {
		t.Fatal("validateNeedsHumanReplies() error = nil, want mixed decision set rejected")
	}
}
