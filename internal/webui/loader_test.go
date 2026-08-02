package webui

import (
	"context"
	"sync"
	"testing"
	"time"
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
