package fixer

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// Forgejo projects post PR-comment asks through the provider-aware adapter but
// must persist provider=forgejo so the resume poll uses the Forgejo client, not
// raw githubinfra.Gateway (which never sees Forgejo replies).
func TestHITLContract_ForgejoSuspendPreservesProvider(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()
	cfg := forgejoFixerDiscoveryConfig(t, fixture)

	repo := "acme/looper"
	pr := int64(42)
	loop := storage.LoopRecord{
		ID: "loop_forgejo_provider", Seq: 105, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	loopID := loop.ID
	projectID := loop.ProjectID
	queueItem := storage.QueueItemRecord{
		ID: "queue_forgejo_provider", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		DedupeKey: "fixer:forgejo-provider", Priority: storage.QueuePriorityFixer, AvailableAt: nowISO,
		MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Queue.Upsert(ctx, queueItem)
	run := storage.RunRecord{
		ID: "run_forgejo_provider", LoopID: loop.ID, Status: "running",
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Runs.Upsert(ctx, run)
	project, err := fixture.repos.Projects.GetByID(ctx, "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID: %v", err)
	}

	github := &fakeGitHubGateway{nextIssueCommentID: 900}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLAnswerTransport: "github",
		CustomInstructions: cfg,
	})
	_, err = runner.suspendForHuman(ctx, stepInput{
		Project: *project, Loop: loop, Run: run, QueueItem: queueItem,
		Repo: repo, PRNumber: pr,
	}, run, fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: "head-forgejo"}}, &awaitingHumanError{
		question: "forgejo provider?", options: []string{"keep", "change"},
		sessionID: "sess-fj", executionID: "agent-fj", vendor: "codex",
	})
	if err != nil {
		t.Fatalf("suspendForHuman: %v", err)
	}
	got, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID: %v", err)
	}
	if got.Status != "awaiting_human" {
		t.Fatalf("status = %q, want awaiting_human", got.Status)
	}
	ask, ok := loops.ReadHITLAsk(got.MetadataJSON)
	if !ok {
		t.Fatal("HITL ask missing after forgejo suspend")
	}
	if ask.Transport != "github" {
		t.Fatalf("transport = %q, want github (answer channel)", ask.Transport)
	}
	if ask.Provider != "forgejo" {
		t.Fatalf("provider = %q, want forgejo so poll uses Forgejo client", ask.Provider)
	}
	if ask.PRNumber != pr || ask.AskCommentID == 0 {
		t.Fatalf("ask PR/comment = %d/%d, want PR=%d and non-zero comment id", ask.PRNumber, ask.AskCommentID, pr)
	}
	if len(github.createIssueComments) != 1 {
		t.Fatalf("createIssueComments = %d, want 1 (adapter posts ask)", len(github.createIssueComments))
	}
	if len(github.addLabelCalls) != 1 {
		t.Fatalf("addLabelCalls = %d, want 1 (awaiting-human label)", len(github.addLabelCalls))
	}
}
