package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/storage"
)

func TestRuntimeStartupQuarantinesExitedLeaderBecauseDescendantsAreUnproven(t *testing.T) {
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
	if execution == nil || execution.Status != "running" {
		t.Fatalf("execution = %#v, want active evidence retained for retry", execution)
	}
	if run == nil || run.Status != "running" {
		t.Fatalf("run = %#v, want running quarantine", run)
	}
	if loop == nil || loop.Status != "paused" {
		t.Fatalf("loop = %#v, want paused", loop)
	}
	if queue == nil || queue.Status != "manual_intervention" {
		t.Fatalf("queue = %#v, want manual intervention", queue)
	}
	recovery := rt.RecoverySummary()
	if recovery.InterruptedRunsMarked != 0 || recovery.LoopsRequeued != 0 || recovery.OrphanAgentCleanup.CleanedCount != 0 || recovery.OrphanAgentCleanup.QuarantinedCount != 1 {
		t.Fatalf("RecoverySummary() = %#v, want conservative descendant-safe quarantine", recovery)
	}
	events, _ := repos.Events.ListByEntity(context.Background(), "agent_execution", "execution_startup_stale")
	if !containsEventType(events, "looperd.recovery.execution_confirmation_needed") {
		t.Fatalf("events = %#v, want confirmation-needed outcome", events)
	}
}

func TestRuntimeStartupPreservesPriorLivenessQuarantineAfterLeaseExpires(t *testing.T) {
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
	latestQueue, _ := repos.Queue.GetLatestByLoopID(context.Background(), loopID)
	if execution == nil || execution.Status != "running" {
		t.Fatalf("execution = %#v, want retryable active evidence", execution)
	}
	if loop == nil || loop.Status != "paused" || latestQueue == nil || latestQueue.Status != "manual_intervention" {
		t.Fatalf("loop = %#v queue = %#v, want restart quarantine preserved", loop, latestQueue)
	}
	events, _ := repos.Events.ListByEntity(context.Background(), "agent_execution", "execution_restart_after_confirmation")
	if containsEventType(events, "looperd.recovery.execution_stale_requeued") {
		t.Fatalf("events = %#v, must not claim stale-requeued without containment Authority", events)
	}
}
