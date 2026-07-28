package fixer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/hitl"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestNormalizeReplyActionAcceptsNeedsHuman(t *testing.T) {
	if got := normalizeReplyAction("needs_human"); got != string(replyActionNeedsHuman) {
		t.Fatalf("normalizeReplyAction(needs_human) = %q", got)
	}
	if got := normalizeReplyAction("NEEDS_HUMAN"); got != string(replyActionNeedsHuman) {
		t.Fatalf("normalizeReplyAction case-insensitive = %q", got)
	}
	action, ok := parseReplyAction("needs_human")
	if !ok || action != string(replyActionNeedsHuman) {
		t.Fatalf("parseReplyAction(needs_human) = (%q, %v)", action, ok)
	}
	if got := canonicalizeReplyAction("needs_human"); got != string(replyActionNeedsHuman) {
		t.Fatalf("canonicalizeReplyAction(needs_human) = %q", got)
	}
	if got := normalizeNativeRepairAction("needs_human"); got != string(replyActionNeedsHuman) {
		t.Fatalf("normalizeNativeRepairAction(needs_human) = %q", got)
	}
	// deferred remains valid for Forgejo native path
	if got := normalizeNativeRepairAction("deferred"); got != "deferred" {
		t.Fatalf("normalizeNativeRepairAction(deferred) = %q", got)
	}
}

func TestFixerPromptAuthorityOrderAndNoBlindObey(t *testing.T) {
	cfg, err := config.Normalize("")
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	cfg.Instructions.Enabled = false
	items := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Body: "please rename"}}
	detail := &checkpointDetail{HeadSHA: "abc", Title: "Keep dark mode", Body: "Ship dark mode toggle"}

	// HITL off: no needs_human vocabulary; may still authorize push when allowAutoPush.
	off, _ := buildFixerPrompt("project_1", cfg, "acme/looper", 42, detail, items, true, false, config.DefaultDisclosureConfig(), "codex", "gpt")
	if strings.Contains(off, "when in doubt, implement the requested change") {
		t.Fatal("prompt must not contain blind-obey guidance")
	}
	if !strings.Contains(off, "Authority order") {
		t.Fatal("prompt must contain authority order")
	}
	if !strings.Contains(off, "documented project rules") {
		t.Fatal("prompt must name documented project rules, not vague stable norms")
	}
	if strings.Contains(off, "AGENTS.md / stable norms") {
		t.Fatal("authority tier must not use vague stable norms")
	}
	if strings.Contains(off, "needs_human") {
		t.Fatal("HITL-off prompt must not advertise needs_human")
	}
	if !strings.Contains(off, "Commit and push") {
		t.Fatal("HITL-off with allowAutoPush should still authorize agent push")
	}

	// HITL on: needs_human allowed; agent push forced off by caller (allowAutoPush=false).
	on, _ := buildFixerPrompt("project_1", cfg, "acme/looper", 42, detail, items, false, true, config.DefaultDisclosureConfig(), "codex", "gpt")
	if !strings.Contains(on, "needs_human") {
		t.Fatal("HITL-on prompt must allow needs_human")
	}
	if strings.Contains(on, "Commit and push") {
		t.Fatal("HITL-on repair prompt must not authorize agent push")
	}
}

func TestFixerHITLPromptInstructionAppendedWhenEnabled(t *testing.T) {
	if !strings.Contains(hitlPromptInstruction, "AUTHORITY ORDER") {
		t.Fatal("hitl instruction missing authority order")
	}
	if !strings.Contains(hitlPromptInstruction, hitl.AskSentinelRelPath) {
		t.Fatal("hitl instruction missing ask.json path")
	}
	if !strings.Contains(hitlPromptInstruction, "needs_human") {
		t.Fatal("hitl instruction missing needs_human")
	}
	if strings.Contains(hitlPromptInstruction, "AGENTS.md / stable norms") {
		t.Fatal("hitl instruction authority tier must not use vague stable norms")
	}
	if strings.Contains(hitlPromptInstruction, "when in doubt, implement") {
		t.Fatal("hitl instruction must not reintroduce blind obey")
	}
}

func TestReadAskSentinelFailClosedAndDeferredDelete(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".looper"), 0o755); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	path := filepath.Join(dir, hitl.AskSentinelRelPath)
	if err := os.WriteFile(path, []byte(`{"question":"Keep PR intent or follow reviewer?","options":["keep PR intent","follow reviewer"]}`), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	ask, err := hitl.ReadAskSentinel(dir)
	if err != nil {
		t.Fatalf("ReadAskSentinel error = %v", err)
	}
	if ask == nil || ask.Question == "" || len(ask.Options) != 2 {
		t.Fatalf("ask = %#v", ask)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatal("ReadAskSentinel must not delete the file")
	}
	hitl.RemoveAskSentinel(dir)
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("RemoveAskSentinel must delete the file")
	}

	// Malformed → error (fail closed)
	if err := os.WriteFile(path, []byte(`{not-json`), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	if _, err := hitl.ReadAskSentinel(dir); err == nil {
		t.Fatal("malformed ask must error")
	}
	// Missing options → error
	if err := os.WriteFile(path, []byte(`{"question":"only q"}`), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	if _, err := hitl.ReadAskSentinel(dir); err == nil {
		t.Fatal("ask without options must error")
	}
}

func TestSuspendForHumanTransitionsAndNotifies(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	repo := "acme/looper"
	pr := int64(42)
	loop := storage.LoopRecord{
		ID: "loop_fixer_hitl", Seq: 7, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert error = %v", err)
	}
	loopID := loop.ID
	projectID := loop.ProjectID
	queueItem := storage.QueueItemRecord{
		ID: "queue_fixer_hitl", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		DedupeKey: "fixer:hitl", Priority: storage.QueuePriorityFixer, AvailableAt: nowISO,
		MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Queue.Upsert(ctx, queueItem); err != nil {
		t.Fatalf("Queue.Upsert error = %v", err)
	}
	run := storage.RunRecord{
		ID: "run_fixer_hitl", LoopID: loop.ID, Status: "running",
		CurrentStep: stringPtr(string(stepRepair)), StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert error = %v", err)
	}
	project, err := fixture.repos.Projects.GetByID(ctx, "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID error = %v", err)
	}

	var sent []HITLAskNotification
	runner := New(Options{
		DB:                  fixture.coordinator.DB(),
		Repos:               fixture.repos,
		Logger:              fixture.logger,
		Now:                 fixture.now,
		HITLEnabled:         true,
		HITLAnswerTransport: "feishu",
		HITLNotify: func(_ context.Context, n HITLAskNotification) error {
			sent = append(sent, n)
			return nil
		},
	})

	awaiting := &awaitingHumanError{
		question:        "Keep dark mode or follow reviewer light-only request?",
		options:         []string{"keep dark mode", "follow reviewer"},
		sessionID:       "sess-fixer-1",
		executionID:     "agent-fixer-1",
		vendor:          "codex",
		headSHA:         "head-abc",
		reviewThreadID:  "thread-1",
		reviewContentFP: "rev-fp",
		prIntentFP:      "intent-fp",
	}
	// Checkpoint still has incomplete repair (nil) — suspend must keep it that way.
	checkpoint := fixerCheckpoint{
		Detail:   &checkpointDetail{HeadSHA: "head-abc"},
		FixItems: []FixItem{{Type: "comment", ID: "c1", ThreadID: "thread-1", Body: "remove dark mode"}},
		// Deliberately set a repair that suspend must clear.
		Repair: &checkpointRepair{Summary: "should be cleared"},
	}
	result, err := runner.suspendForHuman(ctx, stepInput{
		Project: *project, Loop: loop, Run: run, QueueItem: queueItem,
		Repo: repo, PRNumber: pr, Checkpoint: checkpoint,
	}, run, checkpoint, awaiting)
	if err != nil {
		t.Fatalf("suspendForHuman error = %v", err)
	}
	if result.Status != "awaiting_human" {
		t.Fatalf("result.Status = %q, want awaiting_human", result.Status)
	}

	got, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID error = %v", err)
	}
	if got.Status != "awaiting_human" {
		t.Fatalf("loop status = %q, want awaiting_human", got.Status)
	}
	ask, ok := loops.ReadHITLAsk(got.MetadataJSON)
	if !ok || ask.Question != awaiting.question || ask.SessionID != "sess-fixer-1" || ask.Status != "awaiting" {
		t.Fatalf("persisted ask = %#v (ok=%v)", ask, ok)
	}
	if ask.Role != "fixer" || ask.HeadSHA != "head-abc" || ask.ReviewThreadID != "thread-1" {
		t.Fatalf("fingerprint/role fields = %#v", ask)
	}

	q, err := fixture.repos.Queue.GetByID(ctx, queueItem.ID)
	if err != nil || q == nil {
		t.Fatalf("Queue.GetByID error = %v", err)
	}
	if q.Status != "cancelled" {
		t.Fatalf("queue status = %q, want cancelled", q.Status)
	}

	finishedRun, err := fixture.repos.Runs.GetByID(ctx, run.ID)
	if err != nil || finishedRun == nil {
		t.Fatalf("Runs.GetByID error = %v", err)
	}
	if finishedRun.Status != "interrupted" {
		t.Fatalf("run status = %q, want interrupted", finishedRun.Status)
	}
	// Repair must not be marked complete on the interrupted checkpoint.
	cp := parseCheckpoint(finishedRun.CheckpointJSON)
	if cp.Repair != nil {
		t.Fatalf("checkpoint.Repair = %#v, want nil so resume re-enters repair", cp.Repair)
	}

	if len(sent) != 1 {
		t.Fatalf("HITLNotify calls = %d, want 1", len(sent))
	}
	if sent[0].LoopSeq != 7 || !strings.Contains(sent[0].Title, "Fixer") || sent[0].Question != awaiting.question {
		t.Fatalf("notification = %#v", sent[0])
	}
}

func TestPendingHumanAnswerRejectsStaleHeadFingerprint(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	repo := "acme/looper"
	pr := int64(42)
	loop := storage.LoopRecord{
		ID: "loop_fixer_stale", Seq: 8, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "awaiting_human",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	meta, err := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question:  "Keep dark mode?",
		Answer:    "keep dark mode",
		Status:    "answered",
		SessionID: "sess-stale",
		Vendor:    "codex",
		HeadSHA:   "old-head",
		Role:      "fixer",
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk error = %v", err)
	}
	loop.MetadataJSON = &meta
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert error = %v", err)
	}

	runner := New(Options{Repos: fixture.repos, AgentRuntime: "codex", HITLEnabled: true})

	// Matching head → inject decision.
	prompt, session := runner.pendingHumanAnswer(ctx, &loop, "codex", "old-head", "", "")
	if !strings.Contains(prompt, "keep dark mode") || session != "sess-stale" {
		t.Fatalf("fresh head resume = (%q, %q)", prompt, session)
	}

	// Mismatched head → do not inject as executable decision.
	prompt, session = runner.pendingHumanAnswer(ctx, &loop, "codex", "new-head", "", "")
	if prompt != "" || session != "" {
		t.Fatalf("stale head must not inject decision; got (%q, %q)", prompt, session)
	}
}

func TestDetectHumanAskFromNeedsHumanReply(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	runner := New(Options{
		Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now, HITLEnabled: true,
	})
	input := stepInput{
		Project:  storage.ProjectRecord{ID: "project_1"},
		Loop:     storage.LoopRecord{ID: "loop_x", Seq: 1},
		Repo:     "acme/looper",
		PRNumber: 9,
		Checkpoint: fixerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: "h1"},
			FixItems: []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Body: "remove feature"}},
		},
	}
	replies := []replyExplanationEntry{{
		FixItemID: "c1", ThreadID: "t1", Action: string(replyActionNeedsHuman),
		Explanation: "Reviewer wants to remove dark mode but PR intent keeps it.",
	}}
	awaiting, err := runner.detectHumanAsk(ctx, input, t.TempDir(), "exec-1", replies)
	if err != nil {
		t.Fatalf("detectHumanAsk error = %v", err)
	}
	if awaiting == nil {
		t.Fatal("expected awaitingHumanError from needs_human reply")
	}
	if !strings.Contains(awaiting.question, "acme/looper#9") && !strings.Contains(awaiting.recommendation, "dark mode") {
		t.Fatalf("brief = %#v", awaiting)
	}
	if awaiting.headSHA != "h1" || awaiting.reviewThreadID != "t1" {
		t.Fatalf("fingerprints = head=%q thread=%q", awaiting.headSHA, awaiting.reviewThreadID)
	}
}

func TestDetectHumanAskDisabledWhenHITLOff(t *testing.T) {
	runner := New(Options{HITLEnabled: false})
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, ".looper"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, hitl.AskSentinelRelPath), []byte(`{"question":"q","options":["a"]}`), 0o644)
	awaiting, err := runner.detectHumanAsk(context.Background(), stepInput{}, dir, "e", []replyExplanationEntry{
		{Action: string(replyActionNeedsHuman), Explanation: "x"},
	})
	if err != nil || awaiting != nil {
		t.Fatalf("HITL off must ignore ask; got (%#v, %v)", awaiting, err)
	}
}
