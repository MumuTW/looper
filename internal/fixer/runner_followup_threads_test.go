package fixer

import (
	"context"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestFollowupThreadHelpersKeepOnlyThreadsOutsideRepairSnapshot(t *testing.T) {
	snapshot := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1"}}
	live := []FixItem{
		{Type: "comment", ID: "c1", ThreadID: "t1"},
		{Type: "comment", ID: "c2", ThreadID: "t2"},
		{Type: "check", ID: "ci-new"},
	}
	if got := newFollowupThreadIDs(snapshot, live); !sameStringSlices(got, []string{"t2"}) {
		t.Fatalf("newFollowupThreadIDs() = %#v, want [t2]", got)
	}
	if !hasNewFollowupFixItems(snapshot, live) {
		t.Fatal("hasNewFollowupFixItems() = false, want true for new review thread")
	}
	if hasNewFollowupFixItems(snapshot, []FixItem{{Type: "check", ID: "ci-new"}}) {
		t.Fatal("hasNewFollowupFixItems() = true for a non-thread item")
	}
}

func TestFollowupAndDeferredThreadsDoNotBlockThisRound(t *testing.T) {
	items := []FixItem{{ID: "c1", ThreadID: "t1"}, {ID: "c2", ThreadID: "t2"}, {ID: "c3", ThreadID: "t3"}}
	deferred := &checkpointResolvedComments{Items: []checkpointResolvedComment{{FixItemID: "c1", ThreadID: "t1", Status: "deferred"}}}
	remaining := excludeFollowupThreads(excludeDeferredFixItems(items, deferred), []string{"t2"})
	if len(remaining) != 1 || remaining[0].ThreadID != "t3" {
		t.Fatalf("remaining = %#v, want only t3", remaining)
	}
}

func TestRunResolveCommentsTreatsNewThreadAfterRepairAsFollowUp(t *testing.T) {
	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	prNumber := int64(42)
	target := buildPullRequestTargetID(repo, prNumber)
	loop := storage.LoopRecord{ID: "loop_followup", ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	t1 := ReviewThread{ID: "t1", Comments: []ReviewThreadComment{{ID: "c1", Body: "please fix"}}}
	live := PullRequestDetail{Number: prNumber, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature", BaseRefName: "main", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}, {"id": "c2", "threadId": "t2", "body": "new feedback"}}}
	liveAfterResolve := live
	liveAfterResolve.Comments = []map[string]any{{"id": "c2", "threadId": "t2", "body": "new feedback"}}
	github := &fakeGitHubGateway{viewResponses: []PullRequestDetail{live, liveAfterResolve}, threads: []ReviewThread{t1, {ID: "t2", Comments: []ReviewThreadComment{{ID: "c2", Body: "new feedback"}}}}}
	snapshot := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1"}}
	checkpoint := fixerCheckpoint{FixItems: snapshot, FixItemsHash: hashFixItems(snapshot), Validation: &ValidationResult{Passed: true, HeadSHA: "head-1"}, Push: &checkpointPush{Pushed: false, SkippedReason: "No new commits to push"}, Repair: &checkpointRepair{CompletedAt: fixture.nowISO(), ReplyExplanations: []replyExplanationEntry{{FixItemID: "c1", ThreadID: "t1", Action: string(replyActionFixed), Explanation: "Applied the fix.", ThreadCommentsObserved: hashReviewThreadComments(t1)}}}, ReconcileCommits: &checkpointReconcileCommits{WorkingTreeClean: true}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Logger: fixture.logger, Now: fixture.now})
	updated, err := runner.runResolveCommentsStep(context.Background(), stepInput{Project: storage.ProjectRecord{RepoPath: t.TempDir()}, Loop: loop, Repo: repo, PRNumber: prNumber, Checkpoint: checkpoint})
	if err != nil {
		t.Fatalf("runResolveCommentsStep: %v", err)
	}
	if updated.Outcome == nil || !sameStringSlices(updated.Outcome.FollowUpThreadIDs, []string{"t2"}) {
		t.Fatalf("Outcome = %#v, want t2 follow-up", updated.Outcome)
	}
	if len(github.resolveCalls) != 1 || github.resolveCalls[0].ThreadID != "t1" {
		t.Fatalf("resolve calls = %#v, want only t1", github.resolveCalls)
	}
	if len(github.replyCalls) != 1 || !strings.Contains(github.replyCalls[0].Body, "Applied the fix.") {
		t.Fatalf("reply calls = %#v, want t1 explanation", github.replyCalls)
	}
	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || persisted == nil {
		t.Fatalf("Loops.GetByID = (%#v, %v)", persisted, err)
	}
	pending, ok := parsePendingFixerRediscoveryState(parseJSONObject(persisted.MetadataJSON))
	if !ok || !sameStringSlices(pending.UnresolvedThreadIDs, []string{"t1", "t2"}) {
		t.Fatalf("pending rediscovery = %#v, want durable live follow-up", pending)
	}
	rechecked, err := runner.runRecheckStep(context.Background(), stepInput{Project: storage.ProjectRecord{RepoPath: t.TempDir()}, Loop: *persisted, Repo: repo, PRNumber: prNumber, Checkpoint: updated})
	if err != nil || rechecked.ResumePolicy != loops.ResumePolicyAdvanceFromCheckpoint {
		t.Fatalf("runRecheckStep = (%#v, %v), want follow-up excluded", rechecked, err)
	}
}
