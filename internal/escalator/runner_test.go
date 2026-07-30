package escalator

import (
	"context"
	"errors"
	"testing"
	"time"

	notifyinfra "github.com/nexu-io/looper/internal/infra/notify"
	"github.com/nexu-io/looper/internal/storage"
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
