package postmergedigest

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/eventlog"
	"github.com/MumuTW/looper/internal/gatekeeper"
	"github.com/MumuTW/looper/internal/infra/notify"
	"github.com/MumuTW/looper/internal/storage"
)

func openDigestRepos(t *testing.T) *storage.Repositories {
	t.Helper()
	root := t.TempDir()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations, BackupDir: filepath.Join(root, "backups")})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	return storage.NewRepositories(coordinator.DB())
}

func appendDigestEvent(t *testing.T, repos *storage.Repositories, record storage.EventLogRecord) {
	t.Helper()
	if record.PayloadJSON == "" {
		record.PayloadJSON = `{}`
	}
	if err := repos.Events.Append(context.Background(), record); err != nil {
		t.Fatalf("Events.Append(%s) error = %v", record.ID, err)
	}
}

func TestAssembleUsesDurableEventsSnapshotsAndLoops(t *testing.T) {
	repos := openDigestRepos(t)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	when := "2026-07-31T08:00:00.000Z"
	entityType, entityID := "pull_request", "acme/looper#42"
	projectID := "project_1"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: when, UpdatedAt: when}); err != nil {
		t.Fatalf("project upsert error = %v", err)
	}
	appendDigestEvent(t, repos, storage.EventLogRecord{ID: "merge", EventType: eventlog.CoordinatorPullRequestMergedEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID, PayloadJSON: `{"repo":"acme/looper","prNumber":42,"issueNumber":9,"headSha":"abc123","mergedAt":"2026-07-31T08:00:00Z"}`, CreatedAt: when})
	appendDigestEvent(t, repos, storage.EventLogRecord{ID: "gate", EventType: gatekeeper.GateReportEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID, PayloadJSON: `{"status":"eligible","reasons":[]}`, CreatedAt: "2026-07-31T07:59:00.000Z"})
	title := "Ship digest"
	payload := `{"diff":"diff --git a/a.go b/a.go\n+line\n-line"}`
	if err := repos.PullRequestSnapshots.Upsert(context.Background(), storage.PullRequestSnapshotRecord{ID: "snapshot", ProjectID: projectID, Repo: "acme/looper", PRNumber: 42, HeadSHA: "abc123", Title: &title, PayloadJSON: &payload, CapturedAt: when, CreatedAt: when}); err != nil {
		t.Fatalf("snapshot upsert error = %v", err)
	}
	repo := "acme/looper"
	pr := int64(42)
	metadata := `{"reasonCode":"reviewer_changes_requested","reason":"human decision required"}`
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_human", Seq: 1, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &pr, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: when, UpdatedAt: when}); err != nil {
		t.Fatalf("loop upsert error = %v", err)
	}
	appendDigestEvent(t, repos, storage.EventLogRecord{ID: "regen", EventType: "coordinator.close_and_regenerate", EntityType: &entityType, EntityID: &entityID, PayloadJSON: `{"repo":"acme/looper","prNumber":42,"failureFingerprint":"fp-1","retrySuccess":true}`, CreatedAt: "2026-07-31T09:00:00.000Z"})

	digest, err := Assemble(context.Background(), repos, Config{Enabled: true, Timezone: "UTC", MaxItems: 10}, now)
	if err != nil {
		t.Fatalf("Assemble() error = %v", err)
	}
	if digest.Empty || len(digest.Merged) != 1 || len(digest.ClosedAndRegenerated) != 1 || len(digest.AwaitingHuman) != 1 {
		t.Fatalf("digest = %#v, want all sections populated", digest)
	}
	if digest.Merged[0].Title != title || digest.Merged[0].Diff.Files != 1 || digest.Merged[0].Diff.Additions != 1 || digest.Merged[0].Diff.Deletions != 1 {
		t.Fatalf("merged item = %#v, want snapshot title and diff", digest.Merged[0])
	}
	if !strings.Contains(Format(digest), "### Merged (1)") || !strings.Contains(Format(digest), "### Closed-and-regenerated (1)") {
		t.Fatalf("formatted digest = %q", Format(digest))
	}
}

func TestRunEmptyDayMarksOnceWithoutDelivery(t *testing.T) {
	repos := openDigestRepos(t)
	deliveries := 0
	service := New(Options{Repos: repos, Config: Config{Enabled: true, Schedule: "08:00", Timezone: "UTC", MaxItems: 10}, Now: func() time.Time { return time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC) }, Deliver: func(context.Context, notify.SystemNotificationPayload) []storage.NotificationRecord {
		deliveries++
		return nil
	}})
	if err := service.Run(context.Background()); err != nil {
		t.Fatalf("Run() empty error = %v", err)
	}
	if deliveries != 0 {
		t.Fatalf("deliveries = %d, want 0 for quiet day", deliveries)
	}
	markers, err := repos.Events.ListByEntity(context.Background(), digestEntityType, "2026-07-31")
	if err != nil || len(markers) != 1 || markers[0].EventType != eventlog.PostMergeDigestSentEventType {
		t.Fatalf("markers = %#v, err=%v, want one sent marker", markers, err)
	}
	if err := service.Run(context.Background()); err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	markers, _ = repos.Events.ListByEntity(context.Background(), digestEntityType, "2026-07-31")
	if len(markers) != 1 {
		t.Fatalf("markers after second run = %d, want one", len(markers))
	}
}

func TestRunDeliveryFailureLeavesRetryableDay(t *testing.T) {
	repos := openDigestRepos(t)
	projectID := "project_1"
	entityType, entityID := "pull_request", "acme/looper#7"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: "2026-07-31T08:00:00.000Z", UpdatedAt: "2026-07-31T08:00:00.000Z"}); err != nil {
		t.Fatalf("project upsert error = %v", err)
	}
	appendDigestEvent(t, repos, storage.EventLogRecord{ID: "merge", EventType: eventlog.CoordinatorPullRequestMergedEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID, PayloadJSON: `{"repo":"acme/looper","prNumber":7}`, CreatedAt: "2026-07-31T08:30:00.000Z"})
	now := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	deliveries := 0
	service := New(Options{Repos: repos, Config: Config{Enabled: true, Schedule: "08:00", Timezone: "UTC", IncludeEmpty: true, MaxItems: 10}, Now: func() time.Time { return now }, Deliver: func(context.Context, notify.SystemNotificationPayload) []storage.NotificationRecord {
		deliveries++
		status := "failed"
		if deliveries > 1 {
			status = "success"
		}
		return []storage.NotificationRecord{{Status: status}}
	}})
	if err := service.Run(context.Background()); err == nil {
		t.Fatal("first Run() error = nil, want delivery failure")
	}
	markers, _ := repos.Events.ListByEntity(context.Background(), digestEntityType, "2026-07-31")
	if len(markers) != 0 {
		t.Fatalf("markers after failed delivery = %d, want 0", len(markers))
	}
	if err := service.Run(context.Background()); err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	markers, _ = repos.Events.ListByEntity(context.Background(), digestEntityType, "2026-07-31")
	if len(markers) != 1 || deliveries != 2 {
		t.Fatalf("markers=%d deliveries=%d, want one marker and two deliveries", len(markers), deliveries)
	}
}

func TestScheduleStateIsTimezoneAware(t *testing.T) {
	now := time.Date(2026, 7, 31, 0, 30, 0, 0, time.UTC) // 08:30 in Taipei
	_, scheduled, err := scheduleState(Config{Schedule: "08:00", Timezone: "Asia/Taipei"}, now)
	if err != nil || !scheduled {
		t.Fatalf("scheduleState() = scheduled=%v err=%v, want scheduled", scheduled, err)
	}
	_, scheduled, err = scheduleState(Config{Schedule: "09:00", Timezone: "Asia/Taipei"}, now)
	if err != nil || scheduled {
		t.Fatalf("scheduleState() = scheduled=%v err=%v, want not scheduled", scheduled, err)
	}
}
