package fixer

import (
	"context"
	"errors"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// GitHub ask delivery must succeed after durable park; transient CreateIssueComment
// failure rolls the park back so claim recovery can retry instead of leaving an
// awaiting_human loop with no PR comment (poll skips AskCommentID=0).
func TestHITLContract_GitHubDeliveryFailureRollsBackPark(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_gh_delivery_fail", Seq: 201, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	loopID := loop.ID
	projectID := loop.ProjectID
	queueItem := storage.QueueItemRecord{
		ID: "queue_gh_delivery_fail", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		DedupeKey: "fixer:gh-delivery-fail", Priority: storage.QueuePriorityFixer, AvailableAt: nowISO,
		MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Queue.Upsert(ctx, queueItem)
	run := storage.RunRecord{
		ID: "run_gh_delivery_fail", LoopID: loop.ID, Status: "running",
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Runs.Upsert(ctx, run)
	project, _ := fixture.repos.Projects.GetByID(ctx, "project_1")

	github := &fakeGitHubGateway{createIssueCommentErr: errors.New("GitHub API 502")}
	var notified int
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLAnswerTransport: "github",
		HITLNotify: func(context.Context, HITLAskNotification) error {
			notified++
			return nil
		},
	})
	_, err := runner.suspendForHuman(ctx, stepInput{
		Project: *project, Loop: loop, Run: run, QueueItem: queueItem,
		Repo: repo, PRNumber: pr,
	}, run, fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: pr87Head}}, &awaitingHumanError{
		question: "delivery fail?", options: []string{"a", "b"},
		sessionID: "sess-del", executionID: "agent-del", vendor: "codex",
	})
	if err == nil {
		t.Fatal("suspendForHuman error = nil, want GitHub delivery failure")
	}
	var loopErr *loopError
	if !errors.As(err, &loopErr) || loopErr.kind != FailureRetryableTransient {
		t.Fatalf("err = %v, want retryable loopError", err)
	}
	if notified != 0 {
		t.Fatalf("notify calls = %d, want 0 on delivery failure", notified)
	}
	got, gerr := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if gerr != nil || got == nil {
		t.Fatalf("Loops.GetByID: %v", gerr)
	}
	if got.Status != "running" {
		t.Fatalf("loop status = %q, want running after delivery-failure rollback", got.Status)
	}
	if _, ok := loops.ReadHITLAsk(got.MetadataJSON); ok {
		t.Fatal("HITL ask must be cleared after delivery-failure rollback")
	}
	if len(github.createIssueComments) != 1 {
		t.Fatalf("createIssueComments = %d, want 1 attempt", len(github.createIssueComments))
	}
}

// Successful GitHub delivery after park must persist Transport + AskCommentID
// so the poll lane can detect answers.
func TestHITLContract_GitHubDeliveryPersistsAskCommentID(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_gh_delivery_ok", Seq: 202, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	loopID := loop.ID
	projectID := loop.ProjectID
	queueItem := storage.QueueItemRecord{
		ID: "queue_gh_delivery_ok", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		DedupeKey: "fixer:gh-delivery-ok", Priority: storage.QueuePriorityFixer, AvailableAt: nowISO,
		MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Queue.Upsert(ctx, queueItem)
	run := storage.RunRecord{
		ID: "run_gh_delivery_ok", LoopID: loop.ID, Status: "running",
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Runs.Upsert(ctx, run)
	project, _ := fixture.repos.Projects.GetByID(ctx, "project_1")

	github := &fakeGitHubGateway{nextIssueCommentID: 4242}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLAnswerTransport: "github",
	})
	result, err := runner.suspendForHuman(ctx, stepInput{
		Project: *project, Loop: loop, Run: run, QueueItem: queueItem,
		Repo: repo, PRNumber: pr,
	}, run, fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: pr87Head}}, &awaitingHumanError{
		question: "delivery ok?", options: []string{"a", "b"},
		sessionID: "sess-ok", executionID: "agent-ok", vendor: "codex",
	})
	if err != nil {
		t.Fatalf("suspendForHuman: %v", err)
	}
	if result.Status != "awaiting_human" {
		t.Fatalf("result.Status = %q", result.Status)
	}
	got, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID: %v", err)
	}
	ask, ok := loops.ReadHITLAsk(got.MetadataJSON)
	if !ok {
		t.Fatal("HITL ask missing after successful github suspend")
	}
	if ask.Transport != "github" || ask.PRNumber != pr || ask.AskCommentID != 4242 {
		t.Fatalf("ask transport/pr/comment = %q/%d/%d, want github/%d/4242",
			ask.Transport, ask.PRNumber, ask.AskCommentID, pr)
	}
}

// Mid-delivery race: dashboard/poll answers after park but before AskCommentID
// persistence. Attach only correlation fields; do not rewrite status/answer.
func TestHITLContract_PersistAskCommentIDPreservesAnsweredAsk(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	repo := "acme/looper"
	pr := int64(87)
	// Durable ask already answered (dashboard won the race) while local delivery
	// still holds a stale awaiting snapshot without a comment id.
	meta, err := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: "keep or restore?", Options: []string{"keep", "restore"},
		SessionID: "sess-race", ExecutionID: "agent-race", Vendor: "codex",
		Status: "answered", Answer: "keep RollingUpdate", AskedAt: nowISO, AnsweredAt: nowISO,
		Transport: "github", Provider: "github", PRNumber: pr, Role: "fixer",
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk: %v", err)
	}
	loop := storage.LoopRecord{
		ID: "loop_gh_attach_cas", Seq: 204, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		MetadataJSON: &meta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}

	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLAnswerTransport: "github",
	})
	staleLocal := loops.HITLAsk{
		Question: "keep or restore?", Options: []string{"keep", "restore"},
		SessionID: "sess-race", ExecutionID: "agent-race", Vendor: "codex",
		Status: "awaiting", AskedAt: nowISO,
		Transport: "github", Provider: "github", PRNumber: pr,
		AskCommentID: 9999, Role: "fixer",
	}
	if err := runner.persistParkedHITLAsk(ctx, loop, staleLocal, nowISO); err != nil {
		t.Fatalf("persistParkedHITLAsk: %v", err)
	}
	got, gerr := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if gerr != nil || got == nil {
		t.Fatalf("Loops.GetByID: %v", gerr)
	}
	ask, ok := loops.ReadHITLAsk(got.MetadataJSON)
	if !ok {
		t.Fatal("HITL ask missing after attach")
	}
	if ask.Status != "answered" || ask.Answer != "keep RollingUpdate" {
		t.Fatalf("ask status/answer = %q/%q, want answered/keep RollingUpdate (must not clobber)", ask.Status, ask.Answer)
	}
	if ask.AskCommentID != 9999 {
		t.Fatalf("AskCommentID = %d, want 9999 attached without rewriting decision", ask.AskCommentID)
	}
	if got.Status != "running" {
		t.Fatalf("loop status = %q, want still running", got.Status)
	}
}

// Awaiting park generation receives AskCommentID without losing question/options.
func TestHITLContract_PersistAskCommentIDOnAwaitingPark(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	repo := "acme/looper"
	pr := int64(87)
	meta, err := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: "keep or restore?", Options: []string{"keep", "restore"},
		SessionID: "sess-ok", ExecutionID: "agent-ok", Vendor: "codex",
		Status: "awaiting", AskedAt: nowISO,
		Transport: "github", Provider: "github", PRNumber: pr, Role: "fixer",
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk: %v", err)
	}
	loop := storage.LoopRecord{
		ID: "loop_gh_attach_await", Seq: 205, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "awaiting_human",
		MetadataJSON: &meta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}

	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLAnswerTransport: "github",
	})
	delivered := loops.HITLAsk{
		Question: "stale question text", Options: []string{"x"},
		SessionID: "sess-ok", ExecutionID: "agent-ok",
		Status: "awaiting", AskedAt: nowISO,
		Transport: "github", Provider: "github", PRNumber: pr, AskCommentID: 4242,
	}
	if err := runner.persistParkedHITLAsk(ctx, loop, delivered, nowISO); err != nil {
		t.Fatalf("persistParkedHITLAsk: %v", err)
	}
	got, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID: %v", err)
	}
	ask, ok := loops.ReadHITLAsk(got.MetadataJSON)
	if !ok {
		t.Fatal("HITL ask missing")
	}
	if ask.AskCommentID != 4242 {
		t.Fatalf("AskCommentID = %d, want 4242", ask.AskCommentID)
	}
	if ask.Question != "keep or restore?" || ask.Status != "awaiting" {
		t.Fatalf("question/status = %q/%q; must keep durable park fields", ask.Question, ask.Status)
	}
}

func TestMergeHITLAskDeliveryCorrelation_GenerationMismatch(t *testing.T) {
	t.Parallel()
	current := loops.HITLAsk{
		Status: "awaiting", ExecutionID: "exec-new", Question: "new ask",
		Transport: "github", PRNumber: 1,
	}
	delivered := loops.HITLAsk{
		Status: "awaiting", ExecutionID: "exec-old", AskCommentID: 7,
		Transport: "github", PRNumber: 1,
	}
	got, changed := mergeHITLAskDeliveryCorrelation(current, delivered)
	if changed {
		t.Fatalf("different ExecutionID must not attach: got=%+v", got)
	}
	if got.AskCommentID != 0 {
		t.Fatalf("AskCommentID = %d, want 0 on generation mismatch", got.AskCommentID)
	}
}

// Concurrent terminate during park must not return awaiting_human or notify.
func TestHITLContract_TerminatedLoopAbortsSuspend(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_hitl_terminated", Seq: 203, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	loopID := loop.ID
	projectID := loop.ProjectID
	queueItem := storage.QueueItemRecord{
		ID: "queue_hitl_terminated", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		DedupeKey: "fixer:hitl-terminated", Priority: storage.QueuePriorityFixer, AvailableAt: nowISO,
		MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Queue.Upsert(ctx, queueItem)
	run := storage.RunRecord{
		ID: "run_hitl_terminated", LoopID: loop.ID, Status: "running",
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Runs.Upsert(ctx, run)
	project, _ := fixture.repos.Projects.GetByID(ctx, "project_1")

	// Terminate the loop before suspend's park transaction reads it.
	// parkHITLLoop loads current status inside the transaction.
	terminated := loop
	terminated.Status = "terminated"
	terminated.UpdatedAt = nowISO
	_ = fixture.repos.Loops.Upsert(ctx, terminated)

	var notified int
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLAnswerTransport: "feishu",
		HITLNotify: func(context.Context, HITLAskNotification) error {
			notified++
			return nil
		},
	})
	result, err := runner.suspendForHuman(ctx, stepInput{
		Project: *project, Loop: loop, Run: run, QueueItem: queueItem,
		Repo: repo, PRNumber: pr,
	}, run, fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: pr87Head}}, &awaitingHumanError{
		question: "already stopped?", options: []string{"a"},
		sessionID: "sess-term", executionID: "agent-term", vendor: "codex",
	})
	if err != nil {
		t.Fatalf("suspendForHuman: %v", err)
	}
	if result.Status != "terminated" {
		t.Fatalf("result.Status = %q, want terminated", result.Status)
	}
	if notified != 0 {
		t.Fatalf("notify calls = %d, want 0 when loop terminated", notified)
	}
	got, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID: %v", err)
	}
	if got.Status != "terminated" {
		t.Fatalf("loop status = %q, want still terminated", got.Status)
	}
	if _, ok := loops.ReadHITLAsk(got.MetadataJSON); ok {
		t.Fatal("must not persist HITL ask on terminated loop")
	}
	finishedRun, err := fixture.repos.Runs.GetByID(ctx, run.ID)
	if err != nil || finishedRun == nil {
		t.Fatalf("Runs.GetByID: %v", err)
	}
	if finishedRun.Status != "interrupted" {
		t.Fatalf("run status = %q, want interrupted", finishedRun.Status)
	}
}
