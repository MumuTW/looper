package runtime

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/storage"
)

// newQueueSettleGateway fakes `gh issue list` to return the given open issue
// numbers (as OPEN), so the discovery snapshot can be populated without a real
// forge. Any other gh invocation returns an empty array so the snapshot fetch
// stays single-call.
func newQueueSettleGateway(t *testing.T, openIssueNumbers []int64) *githubinfra.Gateway {
	t.Helper()
	rows := make([]string, 0, len(openIssueNumbers))
	for _, n := range openIssueNumbers {
		num := strconv.FormatInt(n, 10)
		rows = append(rows, `{"number":`+num+`,"title":"open","state":"OPEN","url":"https://example/issues/`+num+`"}`)
	}
	gateway := githubinfra.New(githubinfra.Options{GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		if strings.Contains(args, "issue list") {
			return shell.Result{Stdout: "[" + strings.Join(rows, ",") + "]"}, nil
		}
		return shell.Result{Stdout: "[]"}, nil
	}})
	return gateway
}

// seedQueuedIssueWorker seeds a project, a queued worker loop targeting an
// issue, and its queued queue item, returning the loopID.
func seedQueuedIssueWorker(t *testing.T, repos *storage.Repositories, now time.Time, projectID, repo string, issueNumber int64) string {
	t.Helper()
	nowISO := formatJavaScriptISOString(now)
	baseBranch := "main"
	projectMetadata := `{"repo":"` + repo + `"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(t.TempDir(), "repo"), BaseBranch: &baseBranch, MetadataJSON: &projectMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loopID := "loop_queue_settle_" + projectID
	num := strconv.FormatInt(issueNumber, 10)
	targetID := "issue:" + repo + ":" + num
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "issue", TargetID: &targetID, Repo: stringPtr(repo), Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_" + loopID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "issue", TargetID: targetID, Repo: stringPtr(repo), DedupeKey: "worker:" + projectID + ":" + repo + ":" + num, Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	return loopID
}

// TestSettleQueuedWorkerIssueTargetsRetiresClosedIssue retires a queued worker
// whose target issue is absent from the discovery snapshot's complete open-issue
// list, recording the reason and pausing the loop — without claiming it first.
func TestSettleQueuedWorkerIssueTargetsRetiresClosedIssue(t *testing.T) {
	workingDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "queue-settle-closed.sqlite"), t.TempDir())
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 31, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	projectID := "looper"
	repo := "nexu-io/looper"
	loopID := seedQueuedIssueWorker(t, repos, now, projectID, repo, 77)

	// Snapshot fetched open issues: 78 and 79 are open, 77 is not (it closed).
	gateway := newQueueSettleGateway(t, []int64{78, 79})
	snapshot := githubinfra.NewDiscoverySnapshot(gateway, githubinfra.NewDiscoveryTickState(), githubinfra.DiscoverySnapshotOptions{IssueLimit: 30})
	if err := snapshot.EnsureOpenIssues(context.Background(), githubinfra.ListOpenIssuesInput{Repo: repo, Limit: 30}); err != nil {
		t.Fatalf("snapshot listOpenIssues() error = %v", err)
	}

	if err := settleQueuedWorkerIssueTargets(context.Background(), repos, snapshot, nil, projectID, repo, nowISO, nil); err != nil {
		t.Fatalf("settleQueuedWorkerIssueTargets() error = %v", err)
	}

	item, err := repos.Queue.GetByID(context.Background(), "queue_"+loopID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if item.Status != "cancelled" {
		t.Fatalf("queue status = %q, want cancelled (retired without claim)", item.Status)
	}
	if item.LastError == nil || !strings.Contains(*item.LastError, "no longer an open issue") {
		t.Fatalf("queue last error = %#v, want 'no longer an open issue' reason", item.LastError)
	}
	loop, err := repos.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop.Status != "paused" {
		t.Fatalf("loop status = %q, want paused (not left queued)", loop.Status)
	}
}

// TestRunDefaultSchedulerTickSettlesClosedIssueBeforePreDiscoveryClaim covers
// the scheduler lifecycle ordering: a queued worker for a now-closed issue
// must be retired before the first claim phase can dispatch its processor.
func TestRunDefaultSchedulerTickSettlesClosedIssueBeforePreDiscoveryClaim(t *testing.T) {
	workingDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "queue-settle-scheduler.sqlite"), t.TempDir())
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 31, 8, 0, 0, 0, time.UTC)
	projectID := "looper"
	repo := "nexu-io/looper"
	loopID := seedQueuedIssueWorker(t, repos, now, projectID, repo, 77)
	workerRunner := &stubWorkerScheduler{}

	if err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:             repos,
		GitHubGateway:     newQueueSettleGateway(t, []int64{78}),
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 1,
		Worker:            workerRunner,
	}); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if workerRunner.processItemCount() != 0 {
		t.Fatalf("worker processed %d items, want none for the closed target", workerRunner.processItemCount())
	}
	item, err := repos.Queue.GetByID(context.Background(), "queue_"+loopID)
	if err != nil || item == nil || item.Status != "cancelled" {
		t.Fatalf("queue item = %#v, %v; want cancelled before claim", item, err)
	}
}

// TestSettleQueuedWorkerIssueTargetsKeepsOpenIssue does not retire a queued
// worker whose target issue is still in the snapshot's open-issue set.
func TestSettleQueuedWorkerIssueTargetsKeepsOpenIssue(t *testing.T) {
	workingDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "queue-settle-open.sqlite"), t.TempDir())
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 31, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	projectID := "looper"
	repo := "nexu-io/looper"
	loopID := seedQueuedIssueWorker(t, repos, now, projectID, repo, 77)

	// 77 is still open — must not be retired.
	gateway := newQueueSettleGateway(t, []int64{77})
	snapshot := githubinfra.NewDiscoverySnapshot(gateway, githubinfra.NewDiscoveryTickState(), githubinfra.DiscoverySnapshotOptions{IssueLimit: 30})
	if err := snapshot.EnsureOpenIssues(context.Background(), githubinfra.ListOpenIssuesInput{Repo: repo, Limit: 30}); err != nil {
		t.Fatalf("snapshot listOpenIssues() error = %v", err)
	}

	if err := settleQueuedWorkerIssueTargets(context.Background(), repos, snapshot, nil, projectID, repo, nowISO, nil); err != nil {
		t.Fatalf("settleQueuedWorkerIssueTargets() error = %v", err)
	}

	item, err := repos.Queue.GetByID(context.Background(), "queue_"+loopID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if item.Status != "queued" {
		t.Fatalf("queue status = %q, want queued (open issue must not be retired)", item.Status)
	}
}

// TestSettleQueuedWorkerIssueTargetsNoopsWhenSnapshotIncomplete does not retire
// anything when the open-issue list is truncated (count meets the limit), so a
// closed issue past the limit is not falsely inferred.
func TestSettleQueuedWorkerIssueTargetsNoopsWhenSnapshotIncomplete(t *testing.T) {
	workingDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "queue-settle-incomplete.sqlite"), t.TempDir())
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 31, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	projectID := "looper"
	repo := "nexu-io/looper"
	loopID := seedQueuedIssueWorker(t, repos, now, projectID, repo, 77)

	// The snapshot clamps the issue limit to a minimum of 30, so to simulate a
	// truncated fetch we return exactly 30 open issues (none is 77). With
	// count == limit the list is incomplete, so the pass must no-op rather than
	// infer 77 closed from absence.
	truncated := make([]int64, 0, 30)
	for n := int64(100); n < 130; n++ {
		truncated = append(truncated, n)
	}
	gateway := newQueueSettleGateway(t, truncated)
	snapshot := githubinfra.NewDiscoverySnapshot(gateway, githubinfra.NewDiscoveryTickState(), githubinfra.DiscoverySnapshotOptions{IssueLimit: 30})
	if err := snapshot.EnsureOpenIssues(context.Background(), githubinfra.ListOpenIssuesInput{Repo: repo, Limit: 30}); err != nil {
		t.Fatalf("snapshot EnsureOpenIssues() error = %v", err)
	}

	if err := settleQueuedWorkerIssueTargets(context.Background(), repos, snapshot, nil, projectID, repo, nowISO, nil); err != nil {
		t.Fatalf("settleQueuedWorkerIssueTargets() error = %v", err)
	}

	item, err := repos.Queue.GetByID(context.Background(), "queue_"+loopID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if item.Status != "queued" {
		t.Fatalf("queue status = %q, want queued (incomplete snapshot must not retire)", item.Status)
	}
}

// TestSettleQueuedWorkerIssueTargetsRefreshesUnfetchedSnapshot confirms that
// retirement obtains its own fresh authority rather than relying on whether a
// discovery lane happened to fetch issues first.
func TestSettleQueuedWorkerIssueTargetsRefreshesUnfetchedSnapshot(t *testing.T) {
	workingDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "queue-settle-unfetched.sqlite"), t.TempDir())
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 31, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	projectID := "looper"
	repo := "nexu-io/looper"
	loopID := seedQueuedIssueWorker(t, repos, now, projectID, repo, 77)

	// Snapshot built but listOpenIssues never called. Settlement performs its
	// own fresh read and can safely retire the absent issue.
	gateway := newQueueSettleGateway(t, nil)
	snapshot := githubinfra.NewDiscoverySnapshot(gateway, githubinfra.NewDiscoveryTickState(), githubinfra.DiscoverySnapshotOptions{IssueLimit: 30})

	if err := settleQueuedWorkerIssueTargets(context.Background(), repos, snapshot, nil, projectID, repo, nowISO, nil); err != nil {
		t.Fatalf("settleQueuedWorkerIssueTargets() error = %v", err)
	}

	item, err := repos.Queue.GetByID(context.Background(), "queue_"+loopID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if item.Status != "cancelled" {
		t.Fatalf("queue status = %q, want cancelled after fresh authority read", item.Status)
	}
}
