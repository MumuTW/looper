package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestCountOutstandingQuarantineDebtCountsPausedLoopRunningEvidence(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	nowISO := formatJavaScriptISOString(time.Date(2026, time.July, 30, 3, 0, 0, 0, time.UTC))

	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time {
		return time.Date(2026, time.July, 30, 3, 0, 0, 0, time.UTC)
	}})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })
	repos := rt.Services().Repositories

	projectID := "project_debt"
	loopID := "loop_debt"
	runID := "run_debt"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Debt", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	mi := "manual_intervention"
	reason := "startup recovery: uncertain; no PID Authority"
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_debt", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:project_debt:loop_debt", Priority: storage.QueuePriorityWorker, Status: "manual_intervention",
		AvailableAt: nowISO, Attempts: 1, MaxAttempts: 3, LastError: &reason, LastErrorKind: &mi, FinishedAt: &nowISO,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	pid := int64(4242)
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: "exec_debt", ProjectID: &projectID, LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running",
		PID: &pid, CommandJSON: stringPtr(`{"command":"codex"}`), CWD: stringPtr(workingDir),
		HeartbeatCount: 0, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	debt, err := CountOutstandingQuarantineDebt(context.Background(), repos)
	if err != nil {
		t.Fatalf("CountOutstandingQuarantineDebt() error = %v", err)
	}
	if debt.QuarantinedActiveExecutions != 1 || debt.QuarantinedRunningRuns != 1 {
		t.Fatalf("debt = %#v, want 1 execution and 1 running run", debt)
	}
}

func TestCountOutstandingQuarantineDebtIgnoresHealthyActiveWork(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	nowISO := formatJavaScriptISOString(time.Date(2026, time.July, 30, 3, 0, 0, 0, time.UTC))

	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time {
		return time.Date(2026, time.July, 30, 3, 0, 0, 0, time.UTC)
	}})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })
	repos := rt.Services().Repositories

	projectID := "project_ok"
	loopID := "loop_ok"
	runID := "run_ok"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "OK", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_ok", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:project_ok:loop_ok", Priority: storage.QueuePriorityWorker, Status: "running",
		AvailableAt: nowISO, Attempts: 1, MaxAttempts: 3, ClaimedBy: stringPtr("scheduler"), ClaimedAt: &nowISO,
		StartedAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	pid := int64(5252)
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: "exec_ok", ProjectID: &projectID, LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running",
		PID: &pid, CommandJSON: stringPtr(`{"command":"codex"}`), CWD: stringPtr(workingDir),
		HeartbeatCount: 0, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	debt, err := CountOutstandingQuarantineDebt(context.Background(), repos)
	if err != nil {
		t.Fatalf("CountOutstandingQuarantineDebt() error = %v", err)
	}
	if debt.QuarantinedActiveExecutions != 0 || debt.QuarantinedRunningRuns != 0 {
		t.Fatalf("debt = %#v, want zero for healthy active work", debt)
	}
}
