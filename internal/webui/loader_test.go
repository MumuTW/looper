package webui

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/escalator"
	"github.com/MumuTW/looper/internal/gatekeeper"
	"github.com/MumuTW/looper/internal/storage"
)

// countingLoader records how often durable state was actually read.
func countingLoader(calls *int) Loader {
	return func(context.Context) Input {
		*calls++
		return Input{Notices: []string{"loaded"}}
	}
}

func TestLoadCacheServesOneReadInsideTheWindow(t *testing.T) {
	t.Parallel()

	calls := 0
	clock := testNow
	cache := NewLoadCache(RefreshInterval, func() time.Time { return clock })
	load := cache.Wrap(countingLoader(&calls))
	ctx := context.Background()

	first := load(ctx)
	clock = clock.Add(RefreshInterval - time.Millisecond)
	second := load(ctx)

	if calls != 1 {
		t.Fatalf("underlying loads = %d, want 1", calls)
	}
	if len(second.Notices) != len(first.Notices) {
		t.Fatalf("cached input = %#v, want the stored one", second)
	}
	// Only the clock moves on: the ages the board renders stay honest.
	if !second.Now.Equal(clock.UTC()) {
		t.Fatalf("cached Now = %v, want %v", second.Now, clock.UTC())
	}
}

func TestLoadCacheAdvancesEscalatorAgesInsideTheWindow(t *testing.T) {
	t.Parallel()

	clock := testNow
	cache := NewLoadCache(RefreshInterval, func() time.Time { return clock })
	load := cache.Wrap(func(context.Context) Input {
		return Input{Now: clock, Escalator: escalator.Snapshot{
			GeneratedAt: clock.Add(-2 * time.Second).Format(time.RFC3339Nano),
			Items:       []escalator.Item{{ID: "item", AgeSeconds: 7}},
			Backlog:     []escalator.StageBacklog{{ProjectID: "project", OldestAgeSeconds: 11}},
		}}
	})

	first := load(context.Background())
	clock = clock.Add(5 * time.Second)
	second := load(context.Background())

	if first.Escalator.Items[0].AgeSeconds != 7 || first.Escalator.Backlog[0].OldestAgeSeconds != 11 {
		t.Fatalf("cached source was mutated: %#v", first.Escalator)
	}
	if second.Escalator.Items[0].AgeSeconds != 14 || second.Escalator.Backlog[0].OldestAgeSeconds != 18 {
		t.Fatalf("cached Escalator ages = %#v, want collector-relative item 14s/backlog 18s", second.Escalator)
	}
}

func TestLoadCacheReloadsAfterTheWindow(t *testing.T) {
	t.Parallel()

	calls := 0
	clock := testNow
	cache := NewLoadCache(RefreshInterval, func() time.Time { return clock })
	load := cache.Wrap(countingLoader(&calls))
	ctx := context.Background()

	load(ctx)
	clock = clock.Add(RefreshInterval)
	load(ctx)
	clock = clock.Add(RefreshInterval)
	load(ctx)

	if calls != 3 {
		t.Fatalf("underlying loads = %d, want 3", calls)
	}
}

func TestLoadCacheDoesNotPublishCanceledLoad(t *testing.T) {
	t.Parallel()

	calls := 0
	cache := NewLoadCache(RefreshInterval, func() time.Time { return testNow })
	load := cache.Wrap(func(ctx context.Context) Input {
		calls++
		if calls == 1 {
			return Input{Notices: []string{"partial"}}
		}
		return Input{Notices: []string{"healthy"}}
	})
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	load(canceled)
	healthy := load(context.Background())
	if calls != 2 {
		t.Fatalf("underlying loads = %d, want canceled load excluded from cache", calls)
	}
	if len(healthy.Notices) != 1 || healthy.Notices[0] != "healthy" {
		t.Fatalf("healthy load = %#v, want uncached retry", healthy)
	}
}

func TestLoadCacheWithoutTTLAlwaysReadsThrough(t *testing.T) {
	t.Parallel()

	calls := 0
	load := NewLoadCache(0, func() time.Time { return testNow }).Wrap(countingLoader(&calls))
	ctx := context.Background()

	load(ctx)
	load(ctx)

	if calls != 2 {
		t.Fatalf("underlying loads = %d, want 2", calls)
	}
}

// Two tabs poll the same page at once; the cache must not race even though it
// deliberately does not coalesce the misses.
func TestLoadCacheIsSafeForConcurrentPolls(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	calls := 0
	inner := Loader(func(context.Context) Input {
		mu.Lock()
		calls++
		mu.Unlock()
		return Input{Notices: []string{"loaded"}}
	})
	load := NewLoadCache(RefreshInterval, func() time.Time { return testNow }).Wrap(inner)

	var group sync.WaitGroup
	for index := 0; index < 8; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			load(context.Background())
		}()
	}
	group.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls == 0 || calls > 8 {
		t.Fatalf("underlying loads = %d, want between 1 and 8", calls)
	}
}

func TestUnreadableGateReportBecomesBlockedEvidence(t *testing.T) {
	t.Parallel()

	projectID, entityID := "proj", "acme/widgets#1"
	record := storage.EventLogRecord{
		ProjectID: &projectID, EntityID: &entityID, CreatedAt: iso(testNow.Add(-time.Minute)),
		PayloadJSON: "{not-json",
	}
	report, ok := unreadableGateReport(record)
	if !ok {
		t.Fatal("unreadableGateReport() = false, want durable identity projection")
	}
	if report.Status != gatekeeper.StatusBlocked || report.Eligible || report.Repo != "acme/widgets" || report.PRNumber != 1 {
		t.Fatalf("unreadable report = %#v, want blocked acme/widgets#1", report)
	}
	if len(report.Reasons) != 1 || report.Reasons[0].Code != gatekeeper.ReasonProviderStateUnavailable {
		t.Fatalf("unreadable report reasons = %#v, want provider-state block", report.Reasons)
	}
}
