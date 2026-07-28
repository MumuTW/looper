package fixer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/hitl"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestCanonicalFingerprintsMatchAskAndResume(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	runner := New(Options{
		Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now, HITLEnabled: true,
	})

	detail := &checkpointDetail{
		HeadSHA: "head-1", BaseSHA: "base-1", BaseRefName: "main",
		Title: "Keep dark mode", Body: "Ship the dark mode toggle as designed.",
	}
	fixItems := []FixItem{
		{Type: "comment", ID: "c1", ThreadID: "t1", Summary: "remove dark", Body: "Please remove dark mode"},
		{Type: "comment", ID: "c2", ThreadID: "t2", Summary: "typo", Body: "Fix typo"},
	}
	input := stepInput{
		Project:  storage.ProjectRecord{ID: "project_1"},
		Loop:     storage.LoopRecord{ID: "loop_fp", Seq: 1},
		Repo:     "acme/looper",
		PRNumber: 9,
		Checkpoint: fixerCheckpoint{
			Detail:   detail,
			FixItems: fixItems,
		},
	}
	replies := []replyExplanationEntry{{
		FixItemID: "c1", ThreadID: "t1", Action: string(replyActionNeedsHuman),
		Explanation: "Conflicts with PR intent to keep dark mode.",
	}}

	awaiting, err := runner.detectHumanAsk(ctx, input, t.TempDir(), "exec-1", replies)
	if err != nil {
		t.Fatalf("detectHumanAsk error = %v", err)
	}
	if awaiting == nil {
		t.Fatal("expected awaitingHumanError")
	}

	// Resume-time recompute with the same inputs must MaterialFingerprintsMatch.
	liveReview := computeReviewContentFingerprint(fixItems)
	liveIntent := computePRIntentFingerprint(detail)
	if !hitl.MaterialFingerprintsMatch(awaiting.headSHA, detail.HeadSHA, awaiting.reviewContentFP, liveReview, awaiting.prIntentFP, liveIntent) {
		t.Fatalf("ask/resume fingerprints diverge: ask review=%q live=%q intent ask=%q live=%q",
			awaiting.reviewContentFP, liveReview, awaiting.prIntentFP, liveIntent)
	}

	// Intent must cover title/body, not reviewer bodies alone.
	intentNoTitle := computePRIntentFingerprint(&checkpointDetail{HeadSHA: "head-1", BaseSHA: "base-1", BaseRefName: "main"})
	if intentNoTitle == liveIntent {
		t.Fatal("PR intent fingerprint must change when title/body are present")
	}

	// Persist answered ask with those fingerprints, then pendingHumanAnswer.
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	pr := int64(9)
	loop := storage.LoopRecord{
		ID: "loop_fp", Seq: 1, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "awaiting_human",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	meta, err := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: awaiting.question, Answer: "keep dark mode", Status: "answered",
		SessionID: "sess-fp", Vendor: "codex",
		HeadSHA: awaiting.headSHA, ReviewContentFingerprint: awaiting.reviewContentFP,
		PRIntentFingerprint: awaiting.prIntentFP, Role: "fixer",
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk error = %v", err)
	}
	loop.MetadataJSON = &meta
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert error = %v", err)
	}
	runner = New(Options{Repos: fixture.repos, AgentRuntime: "codex", HITLEnabled: true})

	prompt, session := runner.pendingHumanAnswer(ctx, &loop, "codex", detail.HeadSHA, liveReview, liveIntent)
	if !strings.Contains(prompt, "keep dark mode") || session != "sess-fp" {
		t.Fatalf("matching fingerprints should inject answer; got (%q, %q)", prompt, session)
	}

	// Head change invalidates.
	prompt, session = runner.pendingHumanAnswer(ctx, &loop, "codex", "head-changed", liveReview, liveIntent)
	if prompt != "" || session != "" {
		t.Fatalf("head change must invalidate; got (%q, %q)", prompt, session)
	}

	// Review content change invalidates.
	changedItems := append([]FixItem{}, fixItems...)
	changedItems[0].Body = "Please remove dark mode NOW"
	prompt, session = runner.pendingHumanAnswer(ctx, &loop, "codex", detail.HeadSHA, computeReviewContentFingerprint(changedItems), liveIntent)
	if prompt != "" || session != "" {
		t.Fatalf("review content change must invalidate; got (%q, %q)", prompt, session)
	}
}

func TestSuspendClearsDismissAndAskSentinels(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	worktree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(worktree, ".looper"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	askPath := filepath.Join(worktree, hitl.AskSentinelRelPath)
	dismissPath := filepath.Join(worktree, ".looper", "dismiss.json")
	if err := os.WriteFile(askPath, []byte(`{"question":"q","options":["a","b"]}`), 0o644); err != nil {
		t.Fatalf("write ask: %v", err)
	}
	if err := os.WriteFile(dismissPath, []byte(`{"dismissals":[{"reviewer":"bob","reason":"no"}]}`), 0o644); err != nil {
		t.Fatalf("write dismiss: %v", err)
	}

	repo := "acme/looper"
	pr := int64(42)
	loop := storage.LoopRecord{
		ID: "loop_clear", Seq: 3, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	loopID := loop.ID
	projectID := loop.ProjectID
	queueItem := storage.QueueItemRecord{
		ID: "queue_clear", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		DedupeKey: "fixer:clear", Priority: storage.QueuePriorityFixer, AvailableAt: nowISO,
		MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Queue.Upsert(ctx, queueItem); err != nil {
		t.Fatalf("Queue.Upsert: %v", err)
	}
	run := storage.RunRecord{
		ID: "run_clear", LoopID: loop.ID, Status: "running",
		CurrentStep: stringPtr(string(stepRepair)), StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert: %v", err)
	}
	project, _ := fixture.repos.Projects.GetByID(ctx, "project_1")

	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLAnswerTransport: "feishu",
		HITLNotify: func(context.Context, HITLAskNotification) error { return nil },
	})
	awaiting := &awaitingHumanError{
		question: "q", options: []string{"a", "b"}, headSHA: "h1",
		worktreePath: worktree, reviewContentFP: "r", prIntentFP: "i",
	}
	checkpoint := fixerCheckpoint{Worktree: &checkpointWorktree{Path: worktree, Branch: "b"}}
	if _, err := runner.suspendForHuman(ctx, stepInput{
		Project: *project, Loop: loop, Run: run, QueueItem: queueItem,
		Repo: repo, PRNumber: pr, Checkpoint: checkpoint,
	}, run, checkpoint, awaiting); err != nil {
		t.Fatalf("suspendForHuman: %v", err)
	}
	if _, err := os.Stat(askPath); !os.IsNotExist(err) {
		t.Fatal("ask.json must be removed after durable suspend")
	}
	if _, err := os.Stat(dismissPath); !os.IsNotExist(err) {
		t.Fatal("dismiss.json must be removed on HITL suspend")
	}
}

func TestDetectHumanAskMalformedSentinelFailsClosed(t *testing.T) {
	runner := New(Options{HITLEnabled: true})
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, ".looper"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, hitl.AskSentinelRelPath), []byte(`{bad`), 0o644)
	_, err := runner.detectHumanAsk(context.Background(), stepInput{
		Checkpoint: fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: "h"}},
	}, dir, "e", nil)
	if err == nil {
		t.Fatal("malformed ask sentinel must fail closed")
	}
}

func TestHITLDisabledNeedsHumanIsInvalidContract(t *testing.T) {
	err := errHITLDisabledNeedsHuman()
	loopErr, ok := err.(*loopError)
	if !ok {
		t.Fatalf("err type = %T, want *loopError", err)
	}
	if loopErr.kind != FailureManualIntervention {
		t.Fatalf("kind = %v, want manual_intervention", loopErr.kind)
	}
	if !strings.Contains(loopErr.message, "hitl.enabled is false") {
		t.Fatalf("message = %q", loopErr.message)
	}
}
