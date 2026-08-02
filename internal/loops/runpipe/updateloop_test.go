package runpipe

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/storage"
)

func updateLoopFixture(t *testing.T) (*storage.Repositories, func() time.Time, string) {
	t.Helper()
	dir := t.TempDir()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(dir, "runpipe.sqlite"), storage.SQLiteCoordinatorOptions{BackupDir: dir})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Runpipe", RepoPath: dir, CreatedAt: "2026-08-02T08:00:00.000Z", UpdatedAt: "2026-08-02T08:00:00.000Z"}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	now := time.Date(2026, time.August, 2, 9, 0, 0, 0, time.UTC)
	return repos, func() time.Time { return now }, "2026-08-02T09:00:00.000Z"
}

func seedLoop(t *testing.T, repos *storage.Repositories, id, status, metadata string) storage.LoopRecord {
	t.Helper()
	record := storage.LoopRecord{ID: id, Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "project", Status: status, CreatedAt: "2026-08-02T08:00:00.000Z", UpdatedAt: "2026-08-02T08:00:00.000Z"}
	if metadata != "" {
		record.MetadataJSON = &metadata
	}
	if err := repos.Loops.Upsert(context.Background(), record); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	return record
}

func TestUpdateLoopMissingLoopPolicy(t *testing.T) {
	t.Parallel()
	repos, now, _ := updateLoopFixture(t)

	ghost := storage.LoopRecord{ID: "loop_ghost", Seq: 9, ProjectID: "project_1", Type: "worker", TargetType: "project", Status: "queued", CreatedAt: "t", UpdatedAt: "t"}

	// Strict (planner): a missing loop refuses the update.
	if _, err := UpdateLoop(context.Background(), repos, now, ghost, UpdateLoopOptions{RequireExists: true}, func(*storage.LoopRecord) {}); err == nil || !strings.Contains(err.Error(), "loop not found") {
		t.Fatalf("strict UpdateLoop(missing) error = %v, want loop-not-found refusal", err)
	}

	// Tolerant (worker/reviewer/fixer): fall back to the caller's copy.
	updated, err := UpdateLoop(context.Background(), repos, now, ghost, UpdateLoopOptions{}, func(l *storage.LoopRecord) { l.Status = "paused" })
	if err != nil {
		t.Fatalf("tolerant UpdateLoop(missing) error = %v", err)
	}
	if updated.Status != "paused" {
		t.Fatalf("tolerant update = %+v, want mutation applied to the fallback copy", updated)
	}
}

func TestUpdateLoopNeverResurrectsTerminated(t *testing.T) {
	t.Parallel()
	repos, now, _ := updateLoopFixture(t)
	seedLoop(t, repos, "loop_done", "terminated", "")

	stale := storage.LoopRecord{ID: "loop_done", Status: "queued"}
	got, err := UpdateLoop(context.Background(), repos, now, stale, UpdateLoopOptions{}, func(l *storage.LoopRecord) { l.Status = "queued" })
	if err != nil {
		t.Fatalf("UpdateLoop(terminated) error = %v", err)
	}
	if got.Status != "terminated" {
		t.Fatalf("UpdateLoop(terminated) = %+v, want short-circuit without mutation", got)
	}
}

func TestUpdateLoopMutatesFreshestCopy(t *testing.T) {
	t.Parallel()
	repos, now, nowISO := updateLoopFixture(t)
	seedLoop(t, repos, "loop_live", "running", "")

	// The caller holds a stale copy; the stored record moved on.
	stale := storage.LoopRecord{ID: "loop_live", Status: "queued"}
	got, err := UpdateLoop(context.Background(), repos, now, stale, UpdateLoopOptions{}, func(l *storage.LoopRecord) { l.LastRunAt = StringPtr(nowISO) })
	if err != nil {
		t.Fatalf("UpdateLoop() error = %v", err)
	}
	if got.Status != "running" {
		t.Fatalf("UpdateLoop() status = %q, want the freshest stored status, not the caller's stale copy", got.Status)
	}
	if got.UpdatedAt != nowISO {
		t.Fatalf("UpdatedAt = %q, want %q", got.UpdatedAt, nowISO)
	}
}

func TestUpdateLoopMetadataGuard(t *testing.T) {
	t.Parallel()
	repos, now, _ := updateLoopFixture(t)
	seedLoop(t, repos, "loop_bad_meta", "queued", `{"worktree":`)

	corrupt := storage.LoopRecord{ID: "loop_bad_meta"}
	mutateMeta := func(l *storage.LoopRecord) { l.MetadataJSON = StringPtr(`{"fresh":true}`) }

	// Guarded (reviewer/fixer): a malformed stored value blocks a metadata
	// change instead of being silently replaced.
	if _, err := UpdateLoop(context.Background(), repos, now, corrupt, UpdateLoopOptions{GuardMetadata: true}, mutateMeta); err == nil {
		t.Fatal("guarded UpdateLoop(malformed metadata change) = nil error, want fail-loud refusal")
	}

	// A mutation that does not touch metadata passes even when guarded.
	if _, err := UpdateLoop(context.Background(), repos, now, corrupt, UpdateLoopOptions{GuardMetadata: true}, func(l *storage.LoopRecord) { l.Status = "paused" }); err != nil {
		t.Fatalf("guarded UpdateLoop(no metadata change) error = %v", err)
	}

	// Unguarded (planner/worker): the historical silent replacement stands.
	if _, err := UpdateLoop(context.Background(), repos, now, corrupt, UpdateLoopOptions{}, mutateMeta); err != nil {
		t.Fatalf("unguarded UpdateLoop(metadata change) error = %v", err)
	}
}

func TestUpdateLoopMonotonicUpdatedAt(t *testing.T) {
	t.Parallel()
	repos, now, nowISO := updateLoopFixture(t)
	record := seedLoop(t, repos, "loop_fast", "queued", "")
	record.UpdatedAt = nowISO
	if err := repos.Loops.Upsert(context.Background(), record); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	// The clock has not advanced past the stored UpdatedAt; the monotonic
	// stamp must still move strictly forward.
	got, err := UpdateLoop(context.Background(), repos, now, record, UpdateLoopOptions{MonotonicUpdatedAt: true}, func(*storage.LoopRecord) {})
	if err != nil {
		t.Fatalf("UpdateLoop(monotonic) error = %v", err)
	}
	if got.UpdatedAt <= nowISO {
		t.Fatalf("monotonic UpdatedAt = %q, want strictly after %q", got.UpdatedAt, nowISO)
	}
}
