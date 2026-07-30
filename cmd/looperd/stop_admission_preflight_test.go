package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/loops"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
	"github.com/nexu-io/looper/internal/storage"
)

// Every repository read that establishes stop ownership must happen before
// Pause. A failed run or execution lookup used to leave stopLoop's loop paused
// and its live lease cancelled, even though the caller received an error.
func TestHaltLoopDoesNotMutateWhenOwnershipPreflightLookupFails(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dropTable string
		withRun   bool
		wantError string
		terminal  bool
	}{
		{name: "stop run", dropTable: "runs", wantError: "load latest run before halt"},
		{name: "stop execution", dropTable: "agent_executions", withRun: true, wantError: "load latest agent execution before halt"},
		{name: "close run", dropTable: "runs", wantError: "load latest run before halt", terminal: true},
		{name: "close execution", dropTable: "agent_executions", withRun: true, wantError: "load latest agent execution before halt", terminal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			coordinator, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
			if err != nil {
				t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
			}
			t.Cleanup(func() { _ = coordinator.Close() })
			if _, err := coordinator.MigrationRunner().RunPending(ctx); err != nil {
				t.Fatalf("MigrationRunner().RunPending() error = %v", err)
			}

			repos := storage.NewRepositories(coordinator.DB())
			now := time.Date(2026, time.April, 21, 12, 0, 0, 0, time.UTC)
			nowISO := "2026-04-21T12:00:00.000Z"
			project := storage.ProjectRecord{ID: "project_" + tc.name, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}
			if err := repos.Projects.Upsert(ctx, project); err != nil {
				t.Fatalf("Projects.Upsert() error = %v", err)
			}
			loop := storage.LoopRecord{ID: "loop_" + tc.name, Seq: 30, ProjectID: project.ID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}
			if err := repos.Loops.Upsert(ctx, loop); err != nil {
				t.Fatalf("Loops.Upsert() error = %v", err)
			}
			if tc.withRun {
				run := storage.RunRecord{ID: "run_" + tc.name, LoopID: loop.ID, Status: "running", StartedAt: nowISO, LastHeartbeatAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
				if err := repos.Runs.Upsert(ctx, run); err != nil {
					t.Fatalf("Runs.Upsert() error = %v", err)
				}
			}

			registry := looperdruntime.NewActiveExecutionRegistry()
			lease, err := registry.AdmitSpawn(ctx, agent.SpawnMeta{LoopID: loop.ID, RunID: "run_live", ExecutionID: "exec_live"})
			if err != nil {
				t.Fatalf("AdmitSpawn() error = %v", err)
			}
			services := looperdruntime.Services{
				Coordinator:      coordinator,
				Repositories:     repos,
				Loops:            &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }},
				ActiveExecutions: registry,
			}

			if _, err := coordinator.DB().ExecContext(ctx, "DROP TABLE "+tc.dropTable); err != nil {
				t.Fatalf("DROP TABLE %s: %v", tc.dropTable, err)
			}
			halt := stopLoop
			reason := "Stopped by test"
			if tc.terminal {
				halt = closeLoop
				reason = "Closed by test"
			}
			if _, err := halt(ctx, services, loop.ID, reason, func() time.Time { return now }, nil, nil); err == nil {
				t.Fatal("haltLoop() error = nil, want preflight lookup failure")
			} else if !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("haltLoop() error = %v, want %q", err, tc.wantError)
			}

			storedLoop, err := repos.Loops.GetByID(ctx, loop.ID)
			if err != nil {
				t.Fatalf("Loops.GetByID() error = %v", err)
			}
			if storedLoop == nil || storedLoop.Status != "running" {
				t.Fatalf("Loops.GetByID() = %#v, want running after failed preflight", storedLoop)
			}
			if err := lease.Context().Err(); err != nil {
				t.Fatalf("lease cancelled after failed preflight: %v", err)
			}
			if registry.LoopStopActive(loop.ID) {
				t.Fatal("LoopStopActive = true after failed preflight, want gate never opened")
			}
			if _, err := registry.AdmitSpawn(ctx, agent.SpawnMeta{LoopID: loop.ID, RunID: "run_after", ExecutionID: "exec_after"}); err != nil {
				t.Fatalf("AdmitSpawn after failed preflight error = %v, want success", err)
			}
		})
	}
}

// Failed Pause must not cancel spawn leases: BeginLoopStop is irreversible for
// lease contexts wired into execution.run, so it runs only after Pause succeeds.

// Failed Pause must not cancel spawn leases: BeginLoopStop is irreversible for
// lease contexts wired into execution.run, so it runs only after Pause succeeds.

// Failed Pause must not cancel spawn leases: BeginLoopStop is irreversible for
// lease contexts wired into execution.run, so it runs only after Pause succeeds.
func TestStopLoopDoesNotCancelLeasesWhenPauseFails(t *testing.T) {
	ctx := context.Background()
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(ctx); err != nil {
		t.Fatalf("MigrationRunner().RunPending() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })

	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 12, 0, 0, 0, time.UTC)
	nowISO := "2026-04-21T12:00:00.000Z"
	project := storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Projects.Upsert(ctx, project); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	// Terminal status cannot transition to paused → Pause fails while loop stays terminal.
	loop := storage.LoopRecord{ID: "loop_1", Seq: 30, ProjectID: project.ID, Type: "worker", TargetType: "project", Status: "terminated", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	registry := looperdruntime.NewActiveExecutionRegistry()
	lease, err := registry.AdmitSpawn(ctx, agent.SpawnMeta{
		LoopID: loop.ID, RunID: "run_pending", ExecutionID: "exec_pending",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn() error = %v", err)
	}
	if lease.Context().Err() != nil {
		t.Fatalf("lease already cancelled before stop: %v", lease.Context().Err())
	}

	services := looperdruntime.Services{
		Coordinator:      coordinator,
		Repositories:     repos,
		Loops:            &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }},
		ActiveExecutions: registry,
	}

	if _, err := stopLoop(ctx, services, loop.ID, "Stopped by test", func() time.Time { return now }, nil, nil); err == nil {
		t.Fatal("stopLoop() error = nil, want Pause transition failure")
	}
	if err := lease.Context().Err(); err != nil {
		t.Fatalf("lease cancelled after failed Pause: %v (BeginLoopStop must run only after successful pause)", err)
	}
	// New spawns for the loop must still be admitted after the failed stop.
	if _, err := registry.AdmitSpawn(ctx, agent.SpawnMeta{
		LoopID: loop.ID, RunID: "run_after", ExecutionID: "exec_after",
	}); err != nil {
		t.Fatalf("AdmitSpawn after failed stop error = %v, want success (loop stop admission released)", err)
	}
}

// When the Supervisor registry is present but has no entry for a stoppable
// execution that still has a persisted PID, stop/close must fail loudly rather
// than report success while leaving a live agent process behind (#576 / PR #586).
