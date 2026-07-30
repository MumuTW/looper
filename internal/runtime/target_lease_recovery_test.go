package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

func TestReclaimVerifiablyGoneTargetLeases(t *testing.T) {
	for _, test := range []struct {
		name      string
		command   string
		start     int64
		wantGone  bool
		withStart bool
	}{
		{name: "absent process", command: "", start: 100, wantGone: true, withStart: true},
		{name: "birth mismatch", command: "codex exec", start: 200, wantGone: true, withStart: true},
		{name: "matching process", command: "codex exec", start: 100, wantGone: false, withStart: true},
		{name: "missing identity", command: "", start: 0, wantGone: false, withStart: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator := openMigratedCoordinator(t, filepath.Join(t.TempDir(), "leases.sqlite"), t.TempDir())
			t.Cleanup(func() { _ = coordinator.Close() })
			repos := storage.NewRepositories(coordinator.DB())
			pid := int64(4242)
			start := int64(100)
			bootID := "boot-test"
			nowISO := "2026-07-31T08:00:00.000Z"
			record := storage.TargetLeaseRecord{TargetKey: "project_1|pull_request:acme/looper:42", OwnerToken: "opaque", OwnerKind: "automation", OwnerID: "queue_1", Purpose: "claim", ProcessPID: &pid, ProcessBootID: &bootID, AcquiredAt: nowISO, UpdatedAt: nowISO}
			if test.withStart {
				record.ProcessStartTime = &start
			}
			acquired, err := repos.TargetLeases.Acquire(context.Background(), record)
			if err != nil || !acquired {
				t.Fatalf("TargetLeases.Acquire() = (%v, %v), want (true, nil)", acquired, err)
			}
			rt := New(Options{
				Now:                func() time.Time { return time.Date(2026, time.July, 31, 8, 0, 0, 0, time.UTC) },
				ReadProcessCommand: func(context.Context, int) (string, error) { return test.command, nil },
				ReadProcessStart:   func(context.Context, int) (int64, error) { return test.start, nil },
				ReadProcessBootID:  func(context.Context, int) (string, error) { return "boot-test", nil },
			})
			reclaimed, err := rt.reclaimVerifiablyGoneTargetLeases(context.Background(), repos)
			if err != nil {
				t.Fatalf("reclaimVerifiablyGoneTargetLeases() error = %v", err)
			}
			if got := reclaimed == 1; got != test.wantGone {
				t.Fatalf("reclaimed = %d, want gone=%v", reclaimed, test.wantGone)
			}
			lease, err := repos.TargetLeases.Get(context.Background(), record.TargetKey)
			if err != nil {
				t.Fatalf("TargetLeases.Get() error = %v", err)
			}
			if got := lease == nil; got != test.wantGone {
				t.Fatalf("lease gone = %v, want %v", got, test.wantGone)
			}
		})
	}
}
