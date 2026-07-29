package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// Benchmarks for the event-log hot path (append + read), measured against
// the budgets documented in docs/performance-budgets.md. Benchmarks never
// run during plain `go test ./...`, so CI stays fast; run them explicitly:
//
//	go test ./internal/storage -run '^$' -bench BenchmarkEvents -benchtime 200x -v
//
// Each benchmark asserts a ceiling at 10x the documented budget so genuine
// regressions fail the run without flaking on machine jitter.

// Budget: 5ms per append; assertion ceiling 50ms.
func BenchmarkEventsAppend(b *testing.B) {
	repos := benchmarkRepositories(b)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		record := EventLogRecord{
			ID:          fmt.Sprintf("event_bench_append_%d", i),
			EventType:   "loop.status",
			PayloadJSON: `{"status":"running","seq":1}`,
			CreatedAt:   "2026-04-11T12:00:00.000Z",
		}
		if err := repos.Events.Append(ctx, record); err != nil {
			b.Fatalf("Events.Append() error = %v", err)
		}
	}
	b.StopTimer()

	assertPerfCeiling(b, 50*time.Millisecond)
}

// Budget: 10ms to read the newest 100 events out of a 10k-row log;
// assertion ceiling 100ms.
func BenchmarkEventsList(b *testing.B) {
	repos := benchmarkRepositories(b)
	ctx := context.Background()
	for i := 0; i < 10000; i++ {
		record := EventLogRecord{
			ID:          fmt.Sprintf("event_bench_list_%06d", i),
			EventType:   "loop.status",
			PayloadJSON: `{"status":"running","seq":1}`,
			CreatedAt:   fmt.Sprintf("2026-04-11T12:%02d:%02d.%03dZ", (i/60)%60, i%60, i%1000),
		}
		if err := repos.Events.Append(ctx, record); err != nil {
			b.Fatalf("Events.Append() error = %v", err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		items, err := repos.Events.List(ctx, 100)
		if err != nil {
			b.Fatalf("Events.List() error = %v", err)
		}
		if len(items) != 100 {
			b.Fatalf("Events.List() returned %d items, want 100", len(items))
		}
	}
	b.StopTimer()

	assertPerfCeiling(b, 100*time.Millisecond)
}

func benchmarkRepositories(b *testing.B) *Repositories {
	b.Helper()

	root := b.TempDir()
	coordinator, err := OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), SQLiteCoordinatorOptions{
		Migrations: EmbeddedMigrations,
		BackupDir:  filepath.Join(root, "backups"),
	})
	if err != nil {
		b.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	b.Cleanup(func() {
		if err := coordinator.Close(); err != nil {
			b.Fatalf("coordinator.Close() error = %v", err)
		}
	})

	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		b.Fatalf("MigrationRunner.RunPending() error = %v", err)
	}

	return NewRepositories(coordinator.DB())
}

func assertPerfCeiling(b *testing.B, ceiling time.Duration) {
	b.Helper()
	perOp := b.Elapsed() / time.Duration(b.N)
	if perOp > ceiling {
		b.Fatalf("per-op time = %s exceeds ceiling %s (10x budget)", perOp, ceiling)
	}
}
