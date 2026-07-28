package fixer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/hitl"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// When CreateIssueComment succeeds but both correlation attach paths fail, suspend
// must not complete with AskCommentID==0. It stashes the delivered id, rolls the
// incomplete park back, and returns retryable so a later attempt can re-attach
// without posting a second PR question.
func TestHITLContract_CorrelationAttachFailureRetriesWithoutRepost(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	worktree := t.TempDir()
	_ = os.MkdirAll(filepath.Join(worktree, ".looper"), 0o755)

	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_gh_corr_retry", Seq: 302, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)
	loopID := loop.ID
	projectID := loop.ProjectID
	queueItem := storage.QueueItemRecord{
		ID: "queue_gh_corr_retry", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "running",
		DedupeKey: "fixer:gh-corr-retry", Priority: storage.QueuePriorityFixer, AvailableAt: nowISO,
		MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Queue.Upsert(ctx, queueItem)
	run := storage.RunRecord{
		ID: "run_gh_corr_retry", LoopID: loop.ID, Status: "running",
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Runs.Upsert(ctx, run)
	project, _ := fixture.repos.Projects.GetByID(ctx, "project_1")

	const deliveredID int64 = 7771
	github := &fakeGitHubGateway{
		nextIssueCommentID: deliveredID,
		afterCreateIssueComment: func(IssueCommentResult) {
			// Simulate durable correlation being unwritable: terminate the loop so
			// persistParkedHITLAsk / forcePersistDeliveredAskComment fail closed.
			got, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
			if err != nil || got == nil {
				t.Fatalf("Loops.GetByID after delivery: %v", err)
			}
			got.Status = "terminated"
			got.UpdatedAt = nowISO
			if err := fixture.repos.Loops.Upsert(ctx, *got); err != nil {
				t.Fatalf("Loops.Upsert(terminated): %v", err)
			}
		},
	}
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
		Checkpoint: fixerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: pr87Head},
			Worktree: &checkpointWorktree{Path: worktree, HeadSHA: pr87Head, PreparedAt: nowISO},
		},
	}, run, fixerCheckpoint{
		Detail:   &checkpointDetail{HeadSHA: pr87Head},
		Worktree: &checkpointWorktree{Path: worktree, HeadSHA: pr87Head, PreparedAt: nowISO},
	}, &awaitingHumanError{
		question: "correlation retry?", options: []string{"a", "b"},
		sessionID: "sess-corr", executionID: "agent-corr", vendor: "codex",
		worktreePath: worktree,
	})
	if err == nil {
		t.Fatal("suspendForHuman error = nil, want correlation attach failure")
	}
	var loopErr *loopError
	if !errors.As(err, &loopErr) || loopErr.kind != FailureRetryableTransient {
		t.Fatalf("err = %v, want retryable loopError", err)
	}
	if notified != 0 {
		t.Fatalf("notify calls = %d, want 0 when correlation fails (suspension incomplete)", notified)
	}
	if len(github.createIssueComments) != 1 {
		t.Fatalf("createIssueComments = %d, want 1", len(github.createIssueComments))
	}
	stash, stashErr := hitl.ReadDeliveredCommentStash(worktree)
	if stashErr != nil || stash == nil {
		t.Fatalf("delivered comment stash = (%v, %v), want present", stash, stashErr)
	}
	if stash.AskCommentID != deliveredID {
		t.Fatalf("stash.AskCommentID = %d, want %d", stash.AskCommentID, deliveredID)
	}

	// Retry delivery must reuse the stash and not post again.
	// Restore a claimable running loop for the second suspend attempt.
	restored := loop
	restored.Status = "running"
	restored.UpdatedAt = nowISO
	_ = fixture.repos.Loops.Upsert(ctx, restored)
	queueItem.Status = "running"
	_ = fixture.repos.Queue.Upsert(ctx, queueItem)

	github.afterCreateIssueComment = nil // allow correlation on retry
	github.nextIssueCommentID = deliveredID + 100
	result, err := runner.suspendForHuman(ctx, stepInput{
		Project: *project, Loop: restored, Run: run, QueueItem: queueItem,
		Repo: repo, PRNumber: pr,
		Checkpoint: fixerCheckpoint{
			Detail:   &checkpointDetail{HeadSHA: pr87Head},
			Worktree: &checkpointWorktree{Path: worktree, HeadSHA: pr87Head, PreparedAt: nowISO},
		},
	}, run, fixerCheckpoint{
		Detail:   &checkpointDetail{HeadSHA: pr87Head},
		Worktree: &checkpointWorktree{Path: worktree, HeadSHA: pr87Head, PreparedAt: nowISO},
	}, &awaitingHumanError{
		question: "correlation retry?", options: []string{"a", "b"},
		sessionID: "sess-corr", executionID: "agent-corr", vendor: "codex",
		worktreePath: worktree,
	})
	if err != nil {
		t.Fatalf("retry suspendForHuman: %v", err)
	}
	if result.Status != "awaiting_human" {
		t.Fatalf("retry result.Status = %q, want awaiting_human", result.Status)
	}
	if len(github.createIssueComments) != 1 {
		t.Fatalf("createIssueComments after retry = %d, want still 1 (no re-post)", len(github.createIssueComments))
	}
	got, gerr := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if gerr != nil || got == nil {
		t.Fatalf("Loops.GetByID: %v", gerr)
	}
	ask, ok := loops.ReadHITLAsk(got.MetadataJSON)
	if !ok {
		t.Fatal("HITL ask missing after successful correlation retry")
	}
	if ask.AskCommentID != deliveredID {
		t.Fatalf("AskCommentID = %d, want stashed %d", ask.AskCommentID, deliveredID)
	}
	if stash, _ := hitl.ReadDeliveredCommentStash(worktree); stash != nil {
		t.Fatal("delivered comment stash must be removed after successful attach")
	}
}

// deliverAskToGitHub alone must load a worktree stash and skip CreateIssueComment.
func TestHITLContract_DeliverAskToGitHubLoadsStash(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	worktree := t.TempDir()
	if err := hitl.WriteDeliveredCommentStash(worktree, hitl.DeliveredCommentStash{
		AskCommentID: 4242, ExecutionID: "agent-stash", PRNumber: 87,
		Provider: "github", Transport: "github",
	}); err != nil {
		t.Fatalf("WriteDeliveredCommentStash: %v", err)
	}

	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_gh_stash_load", Seq: 303, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "awaiting_human",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)

	github := &fakeGitHubGateway{nextIssueCommentID: 1}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLAnswerTransport: "github",
	})
	ask := loops.HITLAsk{
		Question: "stash?", Options: []string{"yes"},
		SessionID: "sess-stash", ExecutionID: "agent-stash", Status: "awaiting",
		Transport: "github", Provider: "github", PRNumber: pr,
	}
	if err := runner.deliverAskToGitHub(ctx, stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp"},
		Loop:    loop, Repo: repo, PRNumber: pr,
		Checkpoint: fixerCheckpoint{Worktree: &checkpointWorktree{Path: worktree}},
	}, &awaitingHumanError{
		question: "stash?", options: []string{"yes"}, executionID: "agent-stash",
		worktreePath: worktree,
	}, &ask); err != nil {
		t.Fatalf("deliverAskToGitHub: %v", err)
	}
	if len(github.createIssueComments) != 0 {
		t.Fatalf("createIssueComments = %d, want 0 when stash present", len(github.createIssueComments))
	}
	if ask.AskCommentID != 4242 {
		t.Fatalf("AskCommentID = %d, want 4242 from stash", ask.AskCommentID)
	}
}
