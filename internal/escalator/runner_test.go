package escalator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/eventlog"
	notifyinfra "github.com/MumuTW/looper/internal/infra/notify"
	"github.com/MumuTW/looper/internal/storage"
	"github.com/MumuTW/looper/internal/triager"
)

type fakeCollector struct {
	snapshot Snapshot
	err      error
}

func (f *fakeCollector) Collect(context.Context) (Snapshot, error) {
	return f.snapshot, f.err
}

type fakeNotifier struct {
	status   string
	payloads []notifyinfra.SystemNotificationPayload
}

func (f *fakeNotifier) Notify(_ context.Context, payload notifyinfra.SystemNotificationPayload) []storage.NotificationRecord {
	f.payloads = append(f.payloads, payload)
	return []storage.NotificationRecord{{Status: f.status}}
}

func TestRunnerNotifiesOnceAndSuppressesAgeOnlyChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repos := openCollectorRepositories(t)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	collector := &fakeCollector{snapshot: Snapshot{GeneratedAt: now.Format(time.RFC3339Nano), Items: []Item{{
		ID: "item", Reason: ReasonHITLQuestion, Title: "Needs a choice", Link: "https://example.test/choice",
		AgeSeconds: 60, Fingerprint: "stable",
	}}}}
	notifier := &fakeNotifier{status: "success"}
	runner := NewRunner(collector, notifier, repos, RunnerOptions{Now: func() time.Time { return now }})

	first, err := runner.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Notified || first.Suppressed || len(notifier.payloads) != 1 {
		t.Fatalf("first result = %#v, payloads = %d", first, len(notifier.payloads))
	}
	collector.snapshot.GeneratedAt = now.Add(time.Hour).Format(time.RFC3339Nano)
	collector.snapshot.Items[0].AgeSeconds = 3660
	second, err := runner.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Suppressed || second.Notified || !second.Delta.Empty() || len(notifier.payloads) != 1 {
		t.Fatalf("second result = %#v, payloads = %d", second, len(notifier.payloads))
	}
	events, err := repos.Events.ListByEntityTypeAndEventTypes(ctx, digestEntityType, []string{DigestEventType})
	if err != nil || len(events) != 1 {
		t.Fatalf("baseline events = %d, error = %v; want one unchanged baseline", len(events), err)
	}
}

func TestRunnerCollapsesEmptyDigestAndAdvancesBaseline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repos := openCollectorRepositories(t)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	collector := &fakeCollector{snapshot: Snapshot{GeneratedAt: now.Format(time.RFC3339Nano)}}
	notifier := &fakeNotifier{status: "success"}
	result, err := NewRunner(collector, notifier, repos, RunnerOptions{Now: func() time.Time { return now }}).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Suppressed || result.Notified || len(notifier.payloads) != 0 {
		t.Fatalf("result = %#v, payloads = %d", result, len(notifier.payloads))
	}
	events, err := repos.Events.ListByEntityTypeAndEventTypes(ctx, digestEntityType, []string{DigestEventType})
	if err != nil || len(events) != 1 {
		t.Fatalf("baseline events = %d, error = %v", len(events), err)
	}
}

func TestRunnerDoesNotAdvanceBaselineOnCollectionOrDeliveryFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repos := openCollectorRepositories(t)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	collector := &fakeCollector{err: errors.New("partial census")}
	runner := NewRunner(collector, &fakeNotifier{status: "success"}, repos, RunnerOptions{Now: func() time.Time { return now }})
	if _, err := runner.Run(ctx); err == nil {
		t.Fatal("collection failure was swallowed")
	}
	collector.err = nil
	collector.snapshot = Snapshot{GeneratedAt: now.Format(time.RFC3339Nano), Items: []Item{{ID: "item", Link: "https://example.test", Fingerprint: "one"}}}
	runner = NewRunner(collector, &fakeNotifier{status: "failed"}, repos, RunnerOptions{Now: func() time.Time { return now }})
	if _, err := runner.Run(ctx); err == nil {
		t.Fatal("delivery failure was swallowed")
	}
	events, err := repos.Events.ListByEntityTypeAndEventTypes(ctx, digestEntityType, []string{DigestEventType})
	if err != nil || len(events) != 0 {
		t.Fatalf("failed run advanced baseline: events=%d error=%v", len(events), err)
	}
}

func TestRunnerRejectsUnboundedBaseline(t *testing.T) {
	t.Parallel()
	collector := &fakeCollector{snapshot: Snapshot{Items: []Item{{ID: "one"}, {ID: "two"}}}}
	runner := NewRunner(collector, &fakeNotifier{status: "success"}, openCollectorRepositories(t), RunnerOptions{MaxItems: 1})
	if _, err := runner.Run(context.Background()); err == nil {
		t.Fatal("oversized baseline was accepted")
	}
}

func TestRunnerDoesNotBaselineRotatingPartialTriageWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	old := eventlog.FormatJavaScriptISOString(now.Add(-2 * time.Hour))
	repos := openCollectorRepositories(t)
	projectID, repo := "project-window", "acme/widget"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Window", RepoPath: "/tmp/window", CreatedAt: old, UpdatedAt: old}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < triageSourceScanLimit+1; i++ {
		key := fmt.Sprintf("triage-window-%03d", i)
		appendEvent(t, repos, "enrollment-"+key, triager.EnrollmentEventType, "github_issue", triager.Enrollment{
			IdempotencyKey: key, ProjectID: projectID, Repo: repo, IssueNumber: int64(i + 1), EnrolledAt: old,
		}, projectID, old)
	}

	collector := NewCollector(repos, testLinks{}, CollectorOptions{Now: func() time.Time { return now }, UnroutedAfter: time.Hour})
	notifier := &fakeNotifier{status: "success"}
	runner := NewRunner(collector, notifier, repos, RunnerOptions{Now: func() time.Time { return now }})
	first, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	if !first.Snapshot.Partial || !first.Suppressed || first.Notified || len(notifier.payloads) != 0 {
		t.Fatalf("first result = %#v, payloads = %d; want suppressed partial census", first, len(notifier.payloads))
	}
	events, err := repos.Events.ListByEntityTypeAndEventTypes(ctx, digestEntityType, []string{DigestEventType})
	if err != nil || len(events) != 0 {
		t.Fatalf("partial census baseline events = %d, error = %v; want none", len(events), err)
	}

	second, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	if second.Snapshot.Partial || !second.Notified || second.Suppressed || len(second.Snapshot.Items) != triageSourceScanLimit+1 {
		t.Fatalf("second result = %#v, payloads = %d; want complete 65-item digest", second, len(notifier.payloads))
	}
	events, err = repos.Events.ListByEntityTypeAndEventTypes(ctx, digestEntityType, []string{DigestEventType})
	if err != nil || len(events) != 1 {
		t.Fatalf("complete census baseline events = %d, error = %v; want one", len(events), err)
	}
}

func TestBuildNotificationGroupsReasonsAndRendersBacklogDelta(t *testing.T) {
	snapshot := Snapshot{Items: []Item{
		{Reason: ReasonHITLQuestion, Title: "Choose database", Link: "https://example.test/one", AgeSeconds: 7200},
		{Reason: ReasonQueueRetries, Title: "Worker retried", Link: "https://example.test/two", AgeSeconds: 3600},
	}, Backlog: []StageBacklog{{ProjectID: "demo", Stage: "worker", Depth: 3, OldestAgeSeconds: 86400}}}
	payload := buildNotification(snapshot, Delta{Added: []Item{{ID: "new"}}, Resolved: []Item{{Title: "Old stall"}}})
	for _, want := range []string{"1 new, 0 changed, 1 resolved", "### hitl_question", "### queue_retries", "### resolved", "Old stall", "demo/worker: 3 item(s), oldest 1d"} {
		if !strings.Contains(payload.Body, want) {
			t.Fatalf("body missing %q:\n%s", want, payload.Body)
		}
	}
}
