package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

// Startup recovery is the daemon's boot path, and every write it makes —
// requeue, normalize, quarantine-park — is one the takeover hold refuses. Before
// the hold existed those writes could not fail for this reason; now they can,
// and this pass aborts on the first error. So one operator running
// `looper takeover` at the wrong moment could have taken the whole recovery pass
// down with it, and with it daemon startup, for every other loop.
func TestRunRecoveryPipelineRecoversOtherLoopsWhenOneIsHumanHeld(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, "")
	defer coordinator.Close()
	repositories := storage.NewRepositories(coordinator.DB())
	ctx := context.Background()
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	if err := repositories.Projects.Upsert(ctx, storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	seedRun := func(loopID string) {
		t.Helper()
		if err := repositories.Runs.Upsert(ctx, storage.RunRecord{ID: "run_" + loopID, LoopID: loopID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", loopID, err)
		}
	}
	// The held loop is one recovery actively wants to write: a reviewer whose
	// metadata says the PR is gone, which recovery normalizes to terminated. It
	// sorts first, so an abort here takes the loop behind it down too.
	repo := "acme/looper"
	prNumber := int64(41)
	heldTarget := "pr:acme/looper:41"
	heldMetadata := `{"loop":{"status":"terminated","terminationReason":"pr_closed_or_merged"}}`
	if err := repositories.Loops.UpsertChangingHumanHold(ctx, storage.LoopRecord{
		ID: "loop_held", Seq: 1, ProjectID: "project_1", Type: "reviewer",
		TargetType: "pull_request", TargetID: &heldTarget, Repo: &repo, PRNumber: &prNumber,
		Status: "human_takeover", MetadataJSON: &heldMetadata, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("seed loop_held error = %v", err)
	}
	seedRun("loop_held")
	if err := repositories.Loops.Upsert(ctx, storage.LoopRecord{
		ID: "loop_ordinary", Seq: 2, ProjectID: "project_1", Type: "worker",
		TargetType: "project", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("seed loop_ordinary error = %v", err)
	}
	seedRun("loop_ordinary")

	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})
	if _, err := rt.runRecoveryPipeline(ctx, repositories, nil, now); err != nil {
		t.Fatalf("runRecoveryPipeline() error = %v; one held loop must not abort startup recovery", err)
	}

	assertLoopStatus(t, repositories, "loop_held", "human_takeover")
	held, err := repositories.Loops.GetByID(ctx, "loop_held")
	if err != nil || held == nil {
		t.Fatalf("Loops.GetByID(loop_held) = (%#v, %v)", held, err)
	}
	if held.NextRunAt != nil {
		t.Fatalf("loop_held.NextRunAt = %#v, want nil: recovery must not re-arm a loop a human owns", held.NextRunAt)
	}
	// The point of not aborting: the loop beside it still got recovered.
	ordinary, err := repositories.Loops.GetByID(ctx, "loop_ordinary")
	if err != nil || ordinary == nil {
		t.Fatalf("Loops.GetByID(loop_ordinary) = (%#v, %v)", ordinary, err)
	}
	if ordinary.Status == "running" {
		t.Fatal("loop_ordinary.Status = running; recovery did not process the loop beside the held one")
	}
}

// recoveryUpsertLoop is the backstop for the same thing one layer down: the
// pipeline's status read happens before the write, so a takeover that commits in
// between still reaches the guarded write. A refusal there means "not applied",
// not "recovery failed".
func TestRecoveryUpsertLoopReportsAHeldLoopAsNotApplied(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "runtime.sqlite"), "")
	defer coordinator.Close()
	repositories := storage.NewRepositories(coordinator.DB())
	ctx := context.Background()
	nowISO := formatJavaScriptISOString(time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC))
	if err := repositories.Projects.Upsert(ctx, storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loop := storage.LoopRecord{ID: "loop_raced", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "project", Status: "human_takeover", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repositories.Loops.UpsertChangingHumanHold(ctx, loop); err != nil {
		t.Fatalf("Loops.UpsertChangingHumanHold() error = %v", err)
	}

	// The record recovery loaded before takeover committed.
	stale := loop
	stale.Status = "queued"
	stale.NextRunAt = &nowISO
	applied, err := recoveryUpsertLoop(ctx, repositories, stale)
	if err != nil {
		t.Fatalf("recoveryUpsertLoop() error = %v, want a refusal reported as not applied", err)
	}
	if applied {
		t.Fatal("recoveryUpsertLoop() applied = true, want false: the write was refused by the hold guard")
	}
	persisted, err := repositories.Loops.GetByID(ctx, "loop_raced")
	if err != nil || persisted == nil || persisted.Status != "human_takeover" {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want the loop still held", persisted, err)
	}
}
