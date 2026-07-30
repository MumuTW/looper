package github

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/infra/shell"
)

// Prewarming captures pages before the scheduler's serial project walk begins, so
// without an expiry a later project on a multi-minute tick would consume pages as
// old as the tick itself. Reuse is bounded by the gateway's discovery TTL, which
// is the operator's existing discoveryCacheTtlSeconds knob, so staleness no longer
// scales with tick duration.
func TestSnapshotPageIsRefetchedOnceItOutlivesTheDiscoveryTTL(t *testing.T) {
	t.Parallel()

	const ttl = 5 * time.Minute
	var (
		mu    sync.Mutex
		clock = time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
		calls int
	)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	advance := func(d time.Duration) {
		mu.Lock()
		clock = clock.Add(d)
		mu.Unlock()
	}
	gateway := New(Options{DiscoveryCacheTTL: ttl, Now: now, GHRun: func(context.Context, shell.Options) (shell.Result, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return shell.Result{Stdout: "[]"}, nil
	}})
	snapshot := NewDiscoverySnapshot(gateway, NewDiscoveryTickState(), DiscoverySnapshotOptions{PullRequestLimit: 30, IssueLimit: 30})

	if err := snapshot.Prefetch(context.Background(), "acme/repo", "/tmp/repo", true, false); err != nil {
		t.Fatalf("first Prefetch() error = %v", err)
	}
	if got := callCount(&mu, &calls); got != 1 {
		t.Fatalf("forge calls after warm = %d, want 1", got)
	}

	// Inside the window the warm page is reused.
	advance(ttl - time.Second)
	if err := snapshot.Prefetch(context.Background(), "acme/repo", "/tmp/repo", true, false); err != nil {
		t.Fatalf("in-window Prefetch() error = %v", err)
	}
	if got := callCount(&mu, &calls); got != 1 {
		t.Fatalf("forge calls inside the freshness window = %d, want 1 (page should be reused)", got)
	}

	// Past the window it must be refetched rather than served stale.
	advance(2 * time.Second)
	if err := snapshot.Prefetch(context.Background(), "acme/repo", "/tmp/repo", true, false); err != nil {
		t.Fatalf("expired Prefetch() error = %v", err)
	}
	if got := callCount(&mu, &calls); got != 2 {
		t.Fatalf("forge calls after the freshness window = %d, want 2 (stale page should be refetched)", got)
	}
}

// A non-positive TTL means discovery caching is off, in which case a page is still
// reused for the lifetime of the snapshot exactly as it was before expiry existed.
func TestSnapshotPageNeverExpiresWithoutADiscoveryTTL(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		clock = time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
		calls int
	)
	gateway := New(Options{Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }, GHRun: func(context.Context, shell.Options) (shell.Result, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return shell.Result{Stdout: "[]"}, nil
	}})
	snapshot := NewDiscoverySnapshot(gateway, NewDiscoveryTickState(), DiscoverySnapshotOptions{PullRequestLimit: 30, IssueLimit: 30})

	for i := 0; i < 2; i++ {
		if err := snapshot.Prefetch(context.Background(), "acme/repo", "/tmp/repo", true, false); err != nil {
			t.Fatalf("Prefetch() error = %v", err)
		}
		mu.Lock()
		clock = clock.Add(24 * time.Hour)
		mu.Unlock()
	}
	if got := callCount(&mu, &calls); got != 1 {
		t.Fatalf("forge calls with caching disabled = %d, want 1", got)
	}
}

func callCount(mu *sync.Mutex, calls *int) int {
	mu.Lock()
	defer mu.Unlock()
	return *calls
}
