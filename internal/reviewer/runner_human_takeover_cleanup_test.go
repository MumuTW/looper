package reviewer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

// The fixer's terminal cleanup is not the only door onto a taken-over checkout:
// every terminal reviewer path ends here too, and takeover cancelling the queue
// item is what drives a running reviewer into one. Same invariant, same
// authority — the durable row, not the loop the run has been holding since
// before takeover committed.
func TestCleanupReviewerWorktreePreservesHumanHeldCheckout(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	prNumber := int64(41)
	target := "pr:acme/looper:41"

	seed := func(loopID, status string, seq int64) {
		t.Helper()
		record := storage.LoopRecord{
			ID: loopID, Seq: seq, ProjectID: "project_1", Type: "reviewer",
			TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &prNumber,
			Status: status, CreatedAt: nowISO, UpdatedAt: nowISO,
		}
		write := fixture.repos.Loops.Upsert
		if status == "human_takeover" {
			write = fixture.repos.Loops.UpsertChangingHumanHold
		}
		if err := write(ctx, record); err != nil {
			t.Fatalf("seed loop %s error = %v", loopID, err)
		}
	}
	seed("loop_held_reviewer", "human_takeover", 41)
	seed("loop_unheld_reviewer", "running", 42)

	project, err := fixture.repos.Projects.GetByID(ctx, "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v)", project, err)
	}
	newCheckpoint := func() *reviewerCheckpoint {
		path := filepath.Join(t.TempDir(), "looper-review-pr-41")
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		return &reviewerCheckpoint{Worktree: &checkpointWorktree{Path: path, Branch: "pr-41-head", PreparedAt: nowISO}}
	}

	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})

	held := newCheckpoint()
	runner.cleanupReviewerWorktreeIfTerminal(ctx, *project, "loop_held_reviewer", held)
	if len(git.cleanupCalls) != 0 {
		t.Fatalf("cleanup calls = %#v, want none while a human holds the loop", git.cleanupCalls)
	}
	if held.Worktree.CleanedAt != "" {
		t.Fatalf("CleanedAt = %q, want empty", held.Worktree.CleanedAt)
	}

	// And the guard is narrow: an unheld reviewer loop still cleans up.
	unheld := newCheckpoint()
	runner.cleanupReviewerWorktreeIfTerminal(ctx, *project, "loop_unheld_reviewer", unheld)
	if len(git.cleanupCalls) != 1 {
		t.Fatalf("cleanup calls = %#v, want one cleanup for an unheld loop", git.cleanupCalls)
	}
}
