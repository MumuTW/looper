package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/loops"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
	"github.com/nexu-io/looper/internal/storage"
)

// A takeover response and transition must derive from one successfully loaded
// ownership snapshot: a transient lookup failure must abort takeover before
// stop or lifecycle mutation, not stop automation while returning no session
// id, vendor, or worktree to resume with.
func TestTakeoverLoopAbortsWhenExecutionLookupFails(t *testing.T) {
	ctx := context.Background()
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(ctx); err != nil {
		t.Fatalf("MigrationRunner().RunPending() error = %v", err)
	}

	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 12, 0, 0, 0, time.UTC)
	nowISO := "2026-04-21T12:00:00.000Z"
	project := storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Projects.Upsert(ctx, project); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loop := storage.LoopRecord{ID: "loop_takeover", Seq: 41, ProjectID: project.ID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	registry := looperdruntime.NewActiveExecutionRegistry()
	lease, err := registry.AdmitSpawn(ctx, agent.SpawnMeta{
		LoopID: loop.ID, RunID: "run_live", ExecutionID: "exec_live",
	})
	if err != nil {
		t.Fatalf("AdmitSpawn() error = %v", err)
	}

	services := looperdruntime.Services{
		Coordinator:      coordinator,
		Repositories:     repos,
		Loops:            &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }},
		ActiveExecutions: registry,
	}

	// Force AgentExecutions.GetLatestByLoopID to fail: close the SQLite
	// coordinator under the services.
	if err := coordinator.Close(); err != nil {
		t.Fatalf("coordinator.Close() error = %v", err)
	}

	_, err = takeoverLoop(ctx, services, loop.ID, "Interactive takeover", func() time.Time { return now }, nil, nil)
	if err == nil {
		t.Fatal("takeoverLoop() error = nil, want execution lookup failure")
	}
	if !strings.Contains(err.Error(), "load latest agent execution before takeover") {
		t.Fatalf("takeoverLoop() error = %v, want the lookup failure distinguished from an absent execution", err)
	}

	// No stop or lifecycle mutation may have happened: the live lease is not
	// cancelled and spawn admission never gated.
	if err := lease.Context().Err(); err != nil {
		t.Fatalf("lease cancelled after aborted takeover preflight: %v", err)
	}
	if registry.LoopStopActive(loop.ID) {
		t.Fatal("LoopStopActive = true after aborted takeover preflight, want gate never opened")
	}
}

// Takeover holds before it stops, so a stop failure is a partial failure: the
// hold and the queue-item cancel are already durable. Round two accepted that
// residue as safe; a residue nobody is told about is not safe, it is a loop the
// operator believes is still running. The error return must carry the committed
// state so the API can name it.
func TestTakeoverLoopReportsCommittedHoldWhenStopFails(t *testing.T) {
	ctx := context.Background()
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	defer coordinator.Close()
	if _, err := coordinator.MigrationRunner().RunPending(ctx); err != nil {
		t.Fatalf("MigrationRunner().RunPending() error = %v", err)
	}

	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 12, 0, 0, 0, time.UTC)
	nowISO := "2026-04-21T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loop := storage.LoopRecord{ID: "loop_takeover", Seq: 41, ProjectID: "project_1", Type: "worker", TargetType: "project", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{ID: "run_live", LoopID: loop.ID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	// A stoppable execution with a persisted PID but no registry live handle is
	// the ownership violation stopHeldLoop refuses to paper over.
	pid := int64(4242)
	loopID := loop.ID
	runID := "run_live"
	projectID := "project_1"
	cwd := "/tmp/worktree"
	session := "session_1"
	if err := repos.AgentExecutions.Upsert(ctx, storage.AgentExecutionRecord{
		ID: "exec_live", ProjectID: &projectID, LoopID: &loopID, RunID: &runID,
		Vendor: "codex", Status: "running", PID: &pid, CWD: &cwd, NativeSessionID: &session,
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	services := looperdruntime.Services{
		Coordinator:      coordinator,
		Repositories:     repos,
		Loops:            &loops.Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }},
		ActiveExecutions: looperdruntime.NewActiveExecutionRegistry(),
	}

	result, err := takeoverLoop(ctx, services, loop.ID, "Interactive takeover", func() time.Time { return now }, nil, nil)
	if !errors.Is(err, looperdruntime.ErrAgentLiveHandleMissing) {
		t.Fatalf("takeoverLoop() error = %v, want ErrAgentLiveHandleMissing", err)
	}
	if !result.HoldCommitted {
		t.Fatal("takeoverLoop() result.HoldCommitted = false on the error return; the operator would not be told the loop is already parked")
	}

	// And the residue the flag claims is real.
	held, err := repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || held == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", held, err)
	}
	if held.Status != "human_takeover" {
		t.Fatalf("loop status = %q, want human_takeover: the hold committed before the stop failed", held.Status)
	}
	active, err := repos.Queue.FindActiveByLoopID(ctx, loop.ID)
	if err != nil {
		t.Fatalf("Queue.FindActiveByLoopID() error = %v", err)
	}
	if active != nil {
		t.Fatalf("Queue.FindActiveByLoopID() = %#v, want the queue item cancelled with the hold", active)
	}
}
