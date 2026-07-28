package fixer

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// Successful GitHub suspend must leave DeliveryPending=false once AskCommentID
// is correlated so startup recovery will not requeue a complete park.
func TestHITLContract_GitHubDeliveryClearsDeliveryPending(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_gh_delivery_pending_clear", Seq: 410, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	loopID := loop.ID
	projectID := loop.ProjectID
	queueItem := storage.QueueItemRecord{
		ID: "queue_gh_delivery_pending_clear", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		DedupeKey: "fixer:gh-delivery-pending-clear", Priority: storage.QueuePriorityFixer, AvailableAt: nowISO,
		MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Queue.Upsert(ctx, queueItem)
	run := storage.RunRecord{
		ID: "run_gh_delivery_pending_clear", LoopID: loop.ID, Status: "running",
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Runs.Upsert(ctx, run)
	project, _ := fixture.repos.Projects.GetByID(ctx, "project_1")

	github := &fakeGitHubGateway{nextIssueCommentID: 5555}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLAnswerTransport: "github",
	})
	_, err := runner.suspendForHuman(ctx, stepInput{
		Project: *project, Loop: loop, Run: run, QueueItem: queueItem,
		Repo: repo, PRNumber: pr,
	}, run, fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: pr87Head}}, &awaitingHumanError{
		question: "pending clear?", options: []string{"a", "b"},
		sessionID: "sess-pending", executionID: "agent-pending", vendor: "codex",
	})
	if err != nil {
		t.Fatalf("suspendForHuman: %v", err)
	}
	got, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID: %v", err)
	}
	ask, ok := loops.ReadHITLAsk(got.MetadataJSON)
	if !ok {
		t.Fatal("HITL ask missing")
	}
	if ask.AskCommentID != 5555 {
		t.Fatalf("AskCommentID = %d, want 5555", ask.AskCommentID)
	}
	if ask.DeliveryPending {
		t.Fatal("DeliveryPending must be false after successful correlation")
	}
	if loops.GitHubAskDeliveryPending(ask) {
		t.Fatal("GitHubAskDeliveryPending must be false for complete park")
	}
}

// mergeHITLAskDeliveryCorrelation clears DeliveryPending when attaching a comment id.
func TestMergeHITLAskDeliveryCorrelation_ClearsDeliveryPending(t *testing.T) {
	t.Parallel()
	current := loops.HITLAsk{
		Status: "awaiting", ExecutionID: "exec-1", Question: "q",
		Transport: "github", PRNumber: 1, DeliveryPending: true,
	}
	delivered := loops.HITLAsk{
		Status: "awaiting", ExecutionID: "exec-1", AskCommentID: 9,
		Transport: "github", PRNumber: 1,
	}
	got, changed := mergeHITLAskDeliveryCorrelation(current, delivered)
	if !changed {
		t.Fatal("attach + clear DeliveryPending must report changed")
	}
	if got.AskCommentID != 9 || got.DeliveryPending {
		t.Fatalf("got AskCommentID=%d DeliveryPending=%v, want 9/false", got.AskCommentID, got.DeliveryPending)
	}
}
