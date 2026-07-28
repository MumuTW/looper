package fixer

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

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
