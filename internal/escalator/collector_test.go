package escalator

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/eventlog"
	"github.com/MumuTW/looper/internal/gatekeeper"
	"github.com/MumuTW/looper/internal/storage"
	"github.com/MumuTW/looper/internal/triager"
)

type testLinks struct{}

func (testLinks) Issue(_ string, repo string, number int64) string {
	return fmt.Sprintf("https://example.test/%s/issues/%d", repo, number)
}
func (testLinks) PullRequest(_ string, repo string, number int64) string {
	return fmt.Sprintf("https://example.test/%s/pull/%d", repo, number)
}
func (testLinks) Loop(_ string, seq int64) string {
	return fmt.Sprintf("http://localhost/loops/%d", seq)
}

func TestCollectorDerivesWaitingStuckAndBacklogFromDurableState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	repos := openCollectorRepositories(t)
	projectID, repo := "project-a", "acme/widget"
	old := eventlog.FormatJavaScriptISOString(now.Add(-3 * time.Hour))
	veryOld := eventlog.FormatJavaScriptISOString(now.Add(-48 * time.Hour))
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Widget", RepoPath: "/tmp/widget", CreatedAt: old, UpdatedAt: old}); err != nil {
		t.Fatal(err)
	}

	hitlMeta := `{"hitl":{"status":"awaiting","question":"Choose an API","askedAt":"` + old + `"}}`
	circuitMeta := `{"pauseReason":"agent_failure_streak"}`
	prNumber := int64(44)
	issueTarget, prTarget := "12", "44"
	loops := []storage.LoopRecord{
		{ID: "loop-hitl", Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "issue", TargetID: &issueTarget, Status: "awaiting_human", MetadataJSON: &hitlMeta, CreatedAt: old, UpdatedAt: old},
		{ID: "loop-circuit", Seq: 2, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", TargetID: &prTarget, Status: "paused", MetadataJSON: &circuitMeta, Repo: &repo, PRNumber: &prNumber, CreatedAt: veryOld, UpdatedAt: old},
		{ID: "loop-review", Seq: 3, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", TargetID: &prTarget, Status: "paused", Repo: &repo, PRNumber: &prNumber, CreatedAt: old, UpdatedAt: old},
	}
	for _, loop := range loops {
		if err := repos.Loops.Upsert(ctx, loop); err != nil {
			t.Fatalf("upsert loop %s: %v", loop.ID, err)
		}
	}
	queueLoopID := "loop-hitl"
	queueErrorKind := "retryable_transient"
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{
		ID: "queue-retry", ProjectID: &projectID, LoopID: &queueLoopID, Type: "worker", TargetType: "issue", TargetID: "12",
		DedupeKey: "worker:retry", Priority: storage.QueuePriorityWorker, Status: "queued", AvailableAt: old, Attempts: 2, MaxAttempts: 5, LastErrorKind: &queueErrorKind,
		CreatedAt: old, UpdatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}

	appendEvent(t, repos, "triage-await", triager.ReportEventType, "github_issue", triager.Report{
		IdempotencyKey: "triage-await", ProjectID: projectID, Repo: repo, IssueNumber: 12,
		Policy: triager.PolicyDecision{Action: triager.ActionAwaitHuman}, ConfirmationToken: "confirm-token", CreatedAt: old,
	}, projectID, old)
	appendEvent(t, repos, "triage-unrouted", triager.EnrollmentEventType, "github_issue", triager.Enrollment{
		IdempotencyKey: "triage-unrouted", ProjectID: projectID, Repo: repo, IssueNumber: 13, EnrolledAt: old,
	}, projectID, old)
	appendEvent(t, repos, "triage-reported-enrollment", triager.EnrollmentEventType, "github_issue", triager.Enrollment{
		IdempotencyKey: "triage-reported", ProjectID: projectID, Repo: repo, IssueNumber: 14, EnrolledAt: old,
	}, projectID, old)
	appendEvent(t, repos, "triage-reported", triager.ReportEventType, "github_issue", triager.Report{
		IdempotencyKey: "triage-reported", ProjectID: projectID, Repo: repo, IssueNumber: 14,
		Policy: triager.PolicyDecision{Action: triager.ActionRoutePlanner}, CreatedAt: old,
	}, projectID, old)
	appendEvent(t, repos, "gate-eligible", gatekeeper.GateReportEventType, "pull_request", gatekeeper.Report{
		Version: 2, Mode: "advise", Status: gatekeeper.StatusEligible, Eligible: true, ProjectID: projectID,
		Repo: repo, PRNumber: prNumber, ObservedHeadSHA: "head-44", EvaluatedAt: old,
	}, projectID, old)
	if err := repos.PullRequestSnapshots.Upsert(ctx, storage.PullRequestSnapshotRecord{
		ID: "snapshot-old", ProjectID: projectID, Repo: repo, PRNumber: prNumber, HeadSHA: "head-44", CapturedAt: veryOld, CreatedAt: veryOld,
	}); err != nil {
		t.Fatal(err)
	}

	collector := NewCollector(repos, testLinks{}, CollectorOptions{Now: func() time.Time { return now }})
	snapshot, err := collector.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	wantReasons := map[Reason]bool{
		ReasonTriageConfirmation: false, ReasonHITLQuestion: false, ReasonReviewStall: false,
		ReasonEligibleAdvisePR: false, ReasonCircuitBreaker: false, ReasonQueueRetries: false,
		ReasonTriageNotRouted: false, ReasonStalePRHead: false,
	}
	for _, item := range snapshot.Items {
		if _, expected := wantReasons[item.Reason]; expected {
			wantReasons[item.Reason] = true
		}
		if item.Link == "" || item.AgeSeconds < 0 || item.Fingerprint == "" {
			t.Fatalf("incomplete item = %#v", item)
		}
	}
	var reportedWithoutRoute bool
	for _, item := range snapshot.Items {
		if item.Reason == ReasonTriageNotRouted && strings.Contains(item.Detail, "Triage report exists") {
			reportedWithoutRoute = true
		}
	}
	if !reportedWithoutRoute {
		t.Fatalf("unprojected Triage report missing from digest: %#v", snapshot.Items)
	}
	for reason, found := range wantReasons {
		if !found {
			t.Errorf("missing reason %q in %#v", reason, snapshot.Items)
		}
	}
	if len(snapshot.Backlog) != 3 {
		t.Fatalf("backlog = %#v, want three active stages", snapshot.Backlog)
	}
	if snapshot.Items[0].Reason != ReasonHITLQuestion || snapshot.Items[0].BlockedWork != 2 {
		t.Fatalf("first item = %#v, want HITL item blocking its queue row", snapshot.Items[0])
	}
}

func TestCollectorExcludesArchivedAndCurrentOrResolvedSources(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	repos := openCollectorRepositories(t)
	old := eventlog.FormatJavaScriptISOString(now.Add(-2 * time.Hour))
	archived := "archived"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: archived, Name: archived, RepoPath: "/tmp/archived", Archived: true, CreatedAt: old, UpdatedAt: old}); err != nil {
		t.Fatal(err)
	}
	meta := `{"pauseReason":"agent_failure_streak"}`
	target := "1"
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: "archived-loop", Seq: 9, ProjectID: archived, Type: "fixer", TargetType: "pull_request", TargetID: &target, Status: "paused", MetadataJSON: &meta, CreatedAt: old, UpdatedAt: old}); err != nil {
		t.Fatal(err)
	}
	collector := NewCollector(repos, testLinks{}, CollectorOptions{Now: func() time.Time { return now }})
	snapshot, err := collector.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 0 || len(snapshot.Backlog) != 0 {
		t.Fatalf("archived project leaked into digest: %#v", snapshot)
	}
}

func TestCollectorAggregatesBacklogAcrossActiveProjects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	old := eventlog.FormatJavaScriptISOString(now.Add(-2 * time.Hour))
	repos := openCollectorRepositories(t)
	for index, projectID := range []string{"alpha", "beta"} {
		if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: projectID, RepoPath: "/tmp/" + projectID, CreatedAt: old, UpdatedAt: old}); err != nil {
			t.Fatal(err)
		}
		target := fmt.Sprintf("%d", index+1)
		if err := repos.Loops.Upsert(ctx, storage.LoopRecord{
			ID: projectID + "-loop", Seq: int64(index + 1), ProjectID: projectID, Type: "worker",
			TargetType: "issue", TargetID: &target, Status: "queued", CreatedAt: old, UpdatedAt: old,
		}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := NewCollector(repos, testLinks{}, CollectorOptions{Now: func() time.Time { return now }}).Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Backlog) != 2 || snapshot.Backlog[0].ProjectID != "alpha" || snapshot.Backlog[1].ProjectID != "beta" {
		t.Fatalf("backlog = %#v, want both active projects in stable order", snapshot.Backlog)
	}
	for _, backlog := range snapshot.Backlog {
		if backlog.Depth != 1 || backlog.OldestAgeSeconds != int64((2*time.Hour)/time.Second) {
			t.Fatalf("backlog entry = %#v", backlog)
		}
	}
}

func openCollectorRepositories(t *testing.T) *storage.Repositories {
	t.Helper()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "escalator.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations, BackupDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	return storage.NewRepositories(coordinator.DB())
}

func appendEvent(t *testing.T, repos *storage.Repositories, id, eventType, entityType string, payload any, projectID, createdAt string) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	entityID := id
	if err := repos.Events.Append(context.Background(), storage.EventLogRecord{
		ID: id, EventType: eventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID,
		PayloadJSON: string(encoded), CreatedAt: createdAt,
	}); err != nil {
		t.Fatal(err)
	}
}
