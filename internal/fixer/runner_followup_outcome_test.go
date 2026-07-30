package fixer

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

func TestRunResolveCommentsSchedulesNewFailingCheckAfterRepair(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	prNumber := int64(42)
	target := buildPullRequestTargetID(repo, prNumber)
	loop := storage.LoopRecord{ID: "loop_new_check_followup", ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	thread := ReviewThread{ID: "t1", Comments: []ReviewThreadComment{{ID: "c1", Body: "please fix"}}}
	liveDetail := PullRequestDetail{
		Number:      prNumber,
		State:       "OPEN",
		HeadSHA:     "new-head",
		HeadRefName: "feature/fix-42",
		BaseRefName: "main",
		Comments:    []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}},
		Checks:      []map[string]any{{"name": "new-ci", "conclusion": "FAILURE"}},
	}
	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{liveDetail}, threads: []ReviewThread{thread}}
	snapshot := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", Summary: "please fix"}}
	checkpoint := fixerCheckpoint{
		FixItems:         snapshot,
		FixItemsHash:     hashFixItems(snapshot),
		Repair:           &checkpointRepair{CompletedAt: fixture.nowISO(), ReplyExplanations: []replyExplanationEntry{{FixItemID: "c1", ThreadID: "t1", Action: string(replyActionFixed), Explanation: "Applied the fix.", ThreadCommentsObserved: hashReviewThreadComments(thread)}}},
		Validation:       &ValidationResult{Passed: true, HeadSHA: "new-head"},
		Push:             &checkpointPush{Pushed: false, SkippedReason: "No new commits to push"},
		ReconcileCommits: &checkpointReconcileCommits{WorkingTreeClean: true},
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Logger: fixture.logger, Now: fixture.now})

	updated, err := runner.runResolveCommentsStep(context.Background(), stepInput{Project: storage.ProjectRecord{RepoPath: t.TempDir()}, Loop: loop, Repo: repo, PRNumber: prNumber, Checkpoint: checkpoint})
	if err != nil {
		t.Fatalf("runResolveCommentsStep() error = %v", err)
	}
	if updated.Outcome == nil || len(updated.Outcome.FollowUpThreadIDs) != 0 {
		t.Fatalf("Outcome = %#v, want non-thread follow-up omitted from thread presentation", updated.Outcome)
	}
	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || persisted == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", persisted, err)
	}
	pending, ok := parsePendingFixerRediscoveryState(parseJSONObject(persisted.MetadataJSON))
	if !ok || pending.FixItemsStateHash != hashFixItemsState(collectFixItems(liveDetail)) {
		t.Fatalf("pending rediscovery = %#v, want new failing-check fingerprint", pending)
	}
	if scheduled, err := runner.schedulePendingRediscoveryAfterRun(context.Background(), *persisted, repo, prNumber); err != nil || !scheduled {
		t.Fatalf("schedulePendingRediscoveryAfterRun() = (%t, %v), want new failing check queued", scheduled, err)
	}
}
