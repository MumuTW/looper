package fixer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// While suspendForHuman holds the shared requeue lock through delivery/complete,
// answer requeue (same lock as deliverHITLAnswerToLoop / /respond) must wait so
// the cancelled claim is not claimed mid-suspension.
func TestHITLContract_SuspendHoldsRequeueLockUntilFinished(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_gh_suspend_lock", Seq: 208, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	loopID := loop.ID
	projectID := loop.ProjectID
	queueItem := storage.QueueItemRecord{
		ID: "queue_gh_suspend_lock", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		DedupeKey: "fixer:gh-suspend-lock", Priority: storage.QueuePriorityFixer, AvailableAt: nowISO,
		MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Queue.Upsert(ctx, queueItem)
	run := storage.RunRecord{
		ID: "run_gh_suspend_lock", LoopID: loop.ID, Status: "running",
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Runs.Upsert(ctx, run)
	project, _ := fixture.repos.Projects.GetByID(ctx, "project_1")

	enteredNotify := make(chan struct{})
	releaseNotify := make(chan struct{})
	var once sync.Once
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: &fakeGitHubGateway{nextIssueCommentID: 5001},
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLAnswerTransport: "github",
		HITLNotify: func(context.Context, HITLAskNotification) error {
			once.Do(func() { close(enteredNotify) })
			<-releaseNotify
			return nil
		},
	})

	done := make(chan error, 1)
	go func() {
		_, err := runner.suspendForHuman(ctx, stepInput{
			Project: *project, Loop: loop, Run: run, QueueItem: queueItem,
			Repo: repo, PRNumber: pr,
		}, run, fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: pr87Head}}, &awaitingHumanError{
			question: "lock?", options: []string{"a", "b"},
			sessionID: "sess-lock", executionID: "agent-lock", vendor: "codex",
		})
		done <- err
	}()

	select {
	case <-enteredNotify:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for suspend to reach notify (post-park)")
	}

	// Park is durable and answerable, but requeue must block until suspend exits.
	lockAcquired := make(chan struct{})
	go func() {
		unlock := loops.LockLoopRequeue(loop.ID)
		close(lockAcquired)
		unlock()
	}()
	select {
	case <-lockAcquired:
		t.Fatal("LockLoopRequeue acquired while suspend still in notify; answer could requeue mid-suspension")
	case <-time.After(150 * time.Millisecond):
		// expected: still blocked
	}

	close(releaseNotify)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("suspendForHuman: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for suspendForHuman to finish")
	}
	select {
	case <-lockAcquired:
	case <-time.After(2 * time.Second):
		t.Fatal("LockLoopRequeue did not complete after suspend finished")
	}

	got, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID: %v", err)
	}
	if got.Status != "awaiting_human" {
		t.Fatalf("status = %q, want awaiting_human", got.Status)
	}
	q, qerr := fixture.repos.Queue.GetByID(ctx, queueItem.ID)
	if qerr != nil || q == nil {
		t.Fatalf("Queue.GetByID: %v", qerr)
	}
	if q.Status != "cancelled" {
		t.Fatalf("queue status = %q, want cancelled until a post-suspend answer requeues", q.Status)
	}
}
