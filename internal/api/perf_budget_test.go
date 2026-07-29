package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

// perfBudgetCeilingFactor is how far above a documented budget an assertion
// ceiling sits. Budgets are targets; ceilings exist so genuine regressions
// trip the test without flaky failures on shared or slow machines.
const perfBudgetCeilingFactor = 10

// TestPerfBudgets measures the daemon's hot read endpoints against the
// budgets documented in docs/performance-budgets.md. It is opt-in so normal
// `go test ./...` and CI stay fast:
//
//	LOOPER_PERF_BUDGETS=1 go test ./internal/api -run TestPerfBudgets -v
func TestPerfBudgets(t *testing.T) {
	if os.Getenv("LOOPER_PERF_BUDGETS") == "" {
		t.Skip("set LOOPER_PERF_BUDGETS=1 to run perf budget measurements (see docs/performance-budgets.md)")
	}

	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	seedPerfBudgetEvents(t, rt.Services().Repositories, 1000)

	server := httptest.NewServer(h)
	t.Cleanup(server.Close)

	endpoints := []struct {
		name string
		path string
	}{
		{name: "healthz", path: "/api/v1/healthz"},
		{name: "status", path: "/api/v1/status"},
		{name: "events", path: "/api/v1/events?limit=100"},
	}

	// Documented budgets (docs/performance-budgets.md):
	// - p95 read latency per endpoint: 50ms against a local daemon
	// - aggregate read throughput with 8 concurrent readers: 100 req/s
	const (
		sequentialRequests = 200
		concurrentReaders  = 8
		requestsPerReader  = 50
		p95Budget          = 50 * time.Millisecond
		throughputBudget   = 100.0
	)
	p95Ceiling := perfBudgetCeilingFactor * p95Budget
	throughputFloor := throughputBudget / perfBudgetCeilingFactor

	for _, endpoint := range endpoints {
		t.Run(endpoint.name, func(t *testing.T) {
			p95 := measurePerfSequentialP95(t, server.URL+endpoint.path, sequentialRequests)
			t.Logf("sequential: p95=%s (budget %s, ceiling %s)", p95.Round(time.Millisecond), p95Budget, p95Ceiling)
			if p95 > p95Ceiling {
				t.Fatalf("sequential p95 = %s exceeds ceiling %s (10x budget)", p95, p95Ceiling)
			}

			throughput := measurePerfConcurrentThroughput(t, server.URL+endpoint.path, concurrentReaders, requestsPerReader)
			t.Logf("concurrent(%d readers): throughput=%.0f req/s (budget %.0f req/s, floor %.0f req/s)", concurrentReaders, throughput, throughputBudget, throughputFloor)
			if throughput < throughputFloor {
				t.Fatalf("concurrent throughput = %.0f req/s below floor %.0f req/s (budget/10)", throughput, throughputFloor)
			}
		})
	}
}

func seedPerfBudgetEvents(t *testing.T, repos *storage.Repositories, count int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < count; i++ {
		createdAt := fmt.Sprintf("2026-04-11T12:%02d:%02d.%03dZ", (i/60)%60, i%60, i%1000)
		err := repos.Events.Append(ctx, storage.EventLogRecord{
			ID:          fmt.Sprintf("event_perf_%06d", i),
			EventType:   "loop.status",
			PayloadJSON: `{"status":"running","seq":1}`,
			CreatedAt:   createdAt,
		})
		if err != nil {
			t.Fatalf("Events.Append() error = %v", err)
		}
	}
}

func measurePerfSequentialP95(t *testing.T, url string, requests int) time.Duration {
	t.Helper()
	latencies := make([]time.Duration, 0, requests)
	for i := 0; i < requests; i++ {
		start := time.Now()
		statusCode := perfGET(t, url)
		latencies = append(latencies, time.Since(start))
		if statusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", url, statusCode)
		}
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	return latencies[int(float64(len(latencies))*0.95)-1]
}

func measurePerfConcurrentThroughput(t *testing.T, url string, readers, requestsPerReader int) float64 {
	t.Helper()
	var failures atomic.Int64
	start := time.Now()
	var wg sync.WaitGroup
	for reader := 0; reader < readers; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < requestsPerReader; i++ {
				if statusCode := perfGET(t, url); statusCode != http.StatusOK {
					failures.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	total := readers * requestsPerReader
	if got := failures.Load(); got > 0 {
		t.Fatalf("GET %s returned non-200 for %d/%d concurrent requests", url, got, total)
	}
	return float64(total) / elapsed.Seconds()
}

func perfGET(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url) //nolint:bodyclose // body closed below
	if err != nil {
		t.Fatalf("GET %s error = %v", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
