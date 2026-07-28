package fixer

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

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
	// GitHub.com ask records provider=github so the resume poll uses gh, not Forgejo.
	got, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID: %v", err)
	}
	ask, ok := loops.ReadHITLAsk(got.MetadataJSON)
	if !ok {
		t.Fatal("HITL ask missing after github suspend")
	}
	if ask.Transport != "github" || ask.Provider != "github" {
		t.Fatalf("ask transport/provider = %q/%q, want github/github", ask.Transport, ask.Provider)
	}
}

// respond transport must not send interactive Feishu cards — /respond is the
// only configured answer channel (answerTransport=respond is API-only).
func TestHITLContract_RespondTransportNotifyOnly(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_respond_notify", Seq: 105, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	loopID := loop.ID
	projectID := loop.ProjectID
	queueItem := storage.QueueItemRecord{
		ID: "queue_respond_notify", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		DedupeKey: "fixer:respond-notify", Priority: storage.QueuePriorityFixer, AvailableAt: nowISO,
		MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Queue.Upsert(ctx, queueItem)
	run := storage.RunRecord{
		ID: "run_respond_notify", LoopID: loop.ID, Status: "running",
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Runs.Upsert(ctx, run)
	project, _ := fixture.repos.Projects.GetByID(ctx, "project_1")

	var notified []HITLAskNotification
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLAnswerTransport: "respond",
		HITLNotify: func(_ context.Context, n HITLAskNotification) error {
			notified = append(notified, n)
			return nil
		},
	})
	_, err := runner.suspendForHuman(ctx, stepInput{
		Project: *project, Loop: loop, Run: run, QueueItem: queueItem,
		Repo: repo, PRNumber: pr,
	}, run, fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: pr87Head}}, &awaitingHumanError{
		question: "respond notify only?", options: []string{"a", "b"},
		sessionID: "sess-respond", executionID: "agent-respond", vendor: "codex",
	})
	if err != nil {
		t.Fatalf("suspendForHuman: %v", err)
	}
	if len(notified) != 1 || !notified[0].NotifyOnly {
		t.Fatalf("respond transport notification = %#v, want NotifyOnly=true", notified)
	}
	got, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID: %v", err)
	}
	ask, ok := loops.ReadHITLAsk(got.MetadataJSON)
	if !ok || ask.Transport != "respond" {
		t.Fatalf("ask.Transport = %#v (ok=%v), want respond stamp", ask, ok)
	}
}
