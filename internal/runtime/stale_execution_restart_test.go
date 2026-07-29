package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestRuntimeStartupRequeuesExpiredExecutionWithInvalidIdentity(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(root, "runtime.sqlite")
	backupDir := filepath.Join(root, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)

	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	loopID, runID, queueID := seedStaleExecutionLeaseRun(t, repos, now, "startup")
	oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: "execution_startup_stale", ProjectID: stringPtr("project_1"),
		LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running",
		CommandJSON:     stringPtr(`{"command":"codex","args":["exec"]}`),
		LastHeartbeatAt: &oldISO, StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatalf("coordinator.Close() error = %v", err)
	}

	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		Now:    func() time.Time { return now },
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	repos = rt.Services().Repositories
	execution, _ := repos.AgentExecutions.GetByID(context.Background(), "execution_startup_stale")
	run, _ := repos.Runs.GetByID(context.Background(), runID)
	loop, _ := repos.Loops.GetByID(context.Background(), loopID)
	queue, _ := repos.Queue.GetByID(context.Background(), queueID)
	if execution == nil || execution.Status != "failed" {
		t.Fatalf("execution = %#v, want failed stale execution", execution)
	}
	if run == nil || run.Status != "interrupted" {
		t.Fatalf("run = %#v, want interrupted", run)
	}
	if loop == nil || loop.Status != "queued" {
		t.Fatalf("loop = %#v, want queued", loop)
	}
	if queue == nil || queue.Status != "queued" {
		t.Fatalf("queue = %#v, want queued", queue)
	}
	recovery := rt.RecoverySummary()
	if recovery.InterruptedRunsMarked != 1 || recovery.LoopsRequeued != 1 || recovery.OrphanAgentCleanup.CleanedCount != 1 {
		t.Fatalf("RecoverySummary() = %#v, want startup stale execution repaired", recovery)
	}
	events, _ := repos.Events.ListByEntity(context.Background(), "agent_execution", "execution_startup_stale")
	if !containsEventType(events, "looperd.recovery.execution_stale_requeued") {
		t.Fatalf("events = %#v, want startup stale-requeued outcome", events)
	}
}

func TestRuntimeStartupReleasesPriorLivenessQuarantineAfterLeaseExpires(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(root, "runtime.sqlite")
	backupDir := filepath.Join(root, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)

	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	loopID, runID, _ := seedStaleExecutionLeaseRun(t, repos, now, "restart_after_confirmation")
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: "execution_restart_after_confirmation", ProjectID: stringPtr("project_1"),
		LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running",
		LastHeartbeatAt: &nowISO, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatalf("coordinator.Close() error = %v", err)
	}

	first := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	loop, _ := first.Services().Repositories.Loops.GetByID(context.Background(), loopID)
	if loop == nil || loop.Status != "paused" {
		t.Fatalf("loop after first startup = %#v, want confirmation pause", loop)
	}
	first.Stop("restart test")

	later := now.Add(executionLivenessLeaseTTL + time.Minute)
	second := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return later }})
	if err := second.Start(context.Background()); err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	t.Cleanup(func() { second.Stop("test cleanup") })
	repos = second.Services().Repositories
	execution, _ := repos.AgentExecutions.GetByID(context.Background(), "execution_restart_after_confirmation")
	loop, _ = repos.Loops.GetByID(context.Background(), loopID)
	activeQueue, _ := repos.Queue.FindActiveByLoopID(context.Background(), loopID)
	if execution == nil || execution.Status != "failed" {
		t.Fatalf("execution = %#v, want stale terminal", execution)
	}
	if loop == nil || loop.Status != "queued" || activeQueue == nil || activeQueue.Status != "queued" {
		t.Fatalf("loop = %#v activeQueue = %#v, want restart recovery queued", loop, activeQueue)
	}
	events, _ := repos.Events.ListByEntity(context.Background(), "agent_execution", "execution_restart_after_confirmation")
	if !containsEventType(events, "looperd.recovery.execution_stale_requeued") {
		t.Fatalf("events = %#v, want stale-requeued outcome after restart", events)
	}
}
