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
