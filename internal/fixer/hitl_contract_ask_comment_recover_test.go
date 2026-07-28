package fixer

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/hitl"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// CreateIssueComment can succeed before WriteDeliveredCommentStash. A retry must
// recover the remote ask by its deterministic generation marker instead of posting
// a second question.
func TestHITLContract_RecoverAskCommentByGenerationMarker(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	worktree := t.TempDir()
	const (
		gen         = "agent-recover-1"
		deliveredID = int64(8801)
		question    = "recover me?"
	)
	if err := hitl.WriteDeliveryGeneration(worktree, hitl.DeliveryGeneration{
		Generation: gen, PRNumber: 87, Question: question,
	}); err != nil {
		t.Fatalf("WriteDeliveryGeneration: %v", err)
	}

	repo := "acme/looper"
	pr := int64(87)
	loop := storage.LoopRecord{
		ID: "loop_gh_recover_marker", Seq: 310, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "awaiting_human",
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	_ = fixture.repos.Loops.Upsert(ctx, loop)

	body := buildFixerGitHubAskComment(loop.Seq, gen, question, []string{"yes", "no"}, nil)
	github := &fakeGitHubGateway{
		nextIssueCommentID: deliveredID + 100, // would post a different id if not recovered
		viewResponses: []PullRequestDetail{{
			Number: pr, State: "OPEN", HeadSHA: pr87Head,
			IssueComments: []map[string]any{
				{"id": deliveredID, "body": body},
			},
		}},
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLAnswerTransport: "github",
	})
	ask := loops.HITLAsk{
		Question: question, Options: []string{"yes", "no"},
		SessionID: "sess-recover", ExecutionID: "agent-recover-retry", Status: "awaiting",
		Transport: "github", Provider: "github", PRNumber: pr,
	}
	if err := runner.deliverAskToGitHub(ctx, stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: "/tmp"},
		Loop:    loop, Repo: repo, PRNumber: pr,
		Checkpoint: fixerCheckpoint{Worktree: &checkpointWorktree{Path: worktree}},
	}, &awaitingHumanError{
		question: question, options: []string{"yes", "no"},
		executionID: "agent-recover-retry", worktreePath: worktree,
	}, &ask); err != nil {
		t.Fatalf("deliverAskToGitHub: %v", err)
	}
	if len(github.createIssueComments) != 0 {
		t.Fatalf("createIssueComments = %d, want 0 when marker recovered", len(github.createIssueComments))
	}
	if ask.AskCommentID != deliveredID {
		t.Fatalf("AskCommentID = %d, want recovered %d", ask.AskCommentID, deliveredID)
	}
	stash, err := hitl.ReadDeliveredCommentStash(worktree)
	if err != nil || stash == nil || stash.AskCommentID != deliveredID {
		t.Fatalf("stash after recover = (%v, %v), want AskCommentID=%d", stash, err, deliveredID)
	}
	if stash.Generation != gen {
		t.Fatalf("stash.Generation = %q, want %q", stash.Generation, gen)
	}
}
