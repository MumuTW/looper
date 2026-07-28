package fixer

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

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

// After a successful CreateIssueComment, correlation must survive even when the
// normal CAS attach cannot find the park record — never clear / re-post.
func TestHITLContract_ForcePersistDeliveredCommentKeepsPark(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	repo := "acme/looper"
	pr := int64(87)
	// Simulate post-delivery state where park exists without AskCommentID.
	meta, err := loops.WriteHITLAsk(nil, loops.HITLAsk{
		Question: "force attach?", Options: []string{"a", "b"},
		SessionID: "sess-force", ExecutionID: "agent-force", Vendor: "codex",
		Status: "awaiting", AskedAt: nowISO,
		Transport: "github", Provider: "github", PRNumber: pr, Role: "fixer",
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk: %v", err)
	}
	loop := storage.LoopRecord{
		ID: "loop_gh_force_attach", Seq: 206, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "awaiting_human",
		MetadataJSON: &meta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)

	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLAnswerTransport: "github",
	})
	delivered := loops.HITLAsk{
		Question: "force attach?", Options: []string{"a", "b"},
		SessionID: "sess-force", ExecutionID: "agent-force",
		Status: "awaiting", AskedAt: nowISO,
		Transport: "github", Provider: "github", PRNumber: pr, AskCommentID: 7777,
	}
	if err := runner.forcePersistDeliveredAskComment(ctx, loop, delivered, nowISO); err != nil {
		t.Fatalf("forcePersistDeliveredAskComment: %v", err)
	}
	got, gerr := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if gerr != nil || got == nil {
		t.Fatalf("Loops.GetByID: %v", gerr)
	}
	if got.Status != "awaiting_human" {
		t.Fatalf("status = %q, want awaiting_human (must not rollback after delivery)", got.Status)
	}
	ask, ok := loops.ReadHITLAsk(got.MetadataJSON)
	if !ok {
		t.Fatal("HITL ask must remain after force-attach")
	}
	if ask.AskCommentID != 7777 {
		t.Fatalf("AskCommentID = %d, want 7777", ask.AskCommentID)
	}
	if ask.Question != "force attach?" {
		t.Fatalf("question = %q, want preserved", ask.Question)
	}
}

// Missing park metadata after remote delivery re-seeds a pending record with the
// delivered comment id so a retry does not post a second question.
func TestHITLContract_ForcePersistReseedsMissingPark(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_gh_force_reseed", Seq: 207, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "awaiting_human",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)

	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLAnswerTransport: "github",
	})
	delivered := loops.HITLAsk{
		Question: "reseed?", Options: []string{"yes"},
		SessionID: "sess-reseed", ExecutionID: "agent-reseed", Vendor: "codex",
		Status: "awaiting", AskedAt: nowISO,
		Transport: "github", Provider: "github", PRNumber: pr, AskCommentID: 8888, Role: "fixer",
	}
	if err := runner.forcePersistDeliveredAskComment(ctx, loop, delivered, nowISO); err != nil {
		t.Fatalf("forcePersistDeliveredAskComment: %v", err)
	}
	got, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID: %v", err)
	}
	ask, ok := loops.ReadHITLAsk(got.MetadataJSON)
	if !ok {
		t.Fatal("force-attach must re-seed park with delivered comment id")
	}
	if ask.AskCommentID != 8888 || ask.Status != "awaiting" {
		t.Fatalf("ask = %+v, want AskCommentID=8888 status=awaiting", ask)
	}

	// deliverAskToGitHub must not CreateIssueComment again when durable id exists.
	github := &fakeGitHubGateway{nextIssueCommentID: 1}
	runner.github = github
	askLocal := delivered
	askLocal.AskCommentID = 0 // local snapshot lost the id; durable park has it
	if err := runner.deliverAskToGitHub(ctx, stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp"},
		Loop:    *got, Repo: repo, PRNumber: pr,
	}, &awaitingHumanError{question: "reseed?", options: []string{"yes"}, executionID: "agent-reseed"}, &askLocal); err != nil {
		t.Fatalf("deliverAskToGitHub: %v", err)
	}
	if len(github.createIssueComments) != 0 {
		t.Fatalf("createIssueComments = %d, want 0 (idempotent recovery)", len(github.createIssueComments))
	}
	if askLocal.AskCommentID != 8888 {
		t.Fatalf("local AskCommentID = %d, want 8888 from durable park", askLocal.AskCommentID)
	}
}
