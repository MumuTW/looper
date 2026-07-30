package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/storage"
)

func TestCountOutstandingQuarantineDebtCountsRecoveryQuarantineEvidence(t *testing.T) {
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
	entityType := "agent_execution"
	executionID := "exec_debt"
	if err := repos.Events.Append(context.Background(), storage.EventLogRecord{
		ID: "event_debt", EventType: recoveryExecutionQuarantinedEventType,
		ProjectID: &projectID, LoopID: &loopID, RunID: &runID,
		EntityType: &entityType, EntityID: &executionID, PayloadJSON: `{}`, CreatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Events.Append() error = %v", err)
	}

	debt, err := CountOutstandingQuarantineDebt(context.Background(), repos)
	if err != nil {
		t.Fatalf("CountOutstandingQuarantineDebt() error = %v", err)
	}
	if debt.QuarantinedActiveExecutions != 1 || debt.QuarantinedRunningRuns != 1 {
		t.Fatalf("debt = %#v, want 1 execution and 1 running run", debt)
	}
}

func TestCountOutstandingQuarantineDebtIgnoresNormalPausedCancellingWork(t *testing.T) {
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

	projectID := "project_normal_stop"
	loopID := "loop_normal_stop"
	runID := "run_normal_stop"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Normal stop", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	pid := int64(4343)
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: "exec_normal_stop", ProjectID: &projectID, LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "cancelling",
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
		t.Fatalf("debt = %#v, want zero without recovery quarantine evidence", debt)
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

// Contract: the counters and the roster are one query pass over one body of
// durable evidence, so an operator sees which loops the counters are about.
func TestCountOutstandingQuarantineDebtReportsQuarantinedLoopRoster(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	quarantinedAt := formatJavaScriptISOString(time.Date(2026, time.July, 30, 8, 18, 12, 0, time.UTC))
	nowISO := formatJavaScriptISOString(time.Date(2026, time.July, 30, 9, 0, 0, 0, time.UTC))

	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time {
		return time.Date(2026, time.July, 30, 9, 0, 0, 0, time.UTC)
	}})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })
	repos := rt.Services().Repositories

	projectID := "project_roster"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Roster", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "MumuTW/looper"
	entityType := "agent_execution"
	// Seeded out of seq order: the roster must come back ordered by seq.
	for _, seeded := range []struct {
		suffix    string
		seq       int64
		prNumber  int64
		pid       int64
		loopType  string
		loopState string
	}{
		{suffix: "b", seq: 36, prNumber: 125, pid: 6001, loopType: "fixer", loopState: "paused"},
		{suffix: "a", seq: 35, prNumber: 126, pid: 6002, loopType: "fixer", loopState: "paused"},
	} {
		loopID := "loop_roster_" + seeded.suffix
		runID := "run_roster_" + seeded.suffix
		executionID := "exec_roster_" + seeded.suffix
		prNumber := seeded.prNumber
		targetID := fmt.Sprintf("pr:%s:%d", repo, prNumber)
		if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{
			ID: loopID, Seq: seeded.seq, ProjectID: projectID, Type: seeded.loopType, TargetType: "pull_request",
			TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: seeded.loopState, CreatedAt: nowISO, UpdatedAt: nowISO,
		}); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", seeded.suffix, err)
		}
		if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", seeded.suffix, err)
		}
		pid := seeded.pid
		if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
			ID: executionID, ProjectID: &projectID, LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running",
			PID: &pid, CommandJSON: stringPtr(`{"command":"codex"}`), CWD: stringPtr(workingDir),
			StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
		}); err != nil {
			t.Fatalf("AgentExecutions.Upsert(%s) error = %v", seeded.suffix, err)
		}
		eventID := "event_roster_" + seeded.suffix
		if err := repos.Events.Append(context.Background(), storage.EventLogRecord{
			ID: eventID, EventType: recoveryExecutionQuarantinedEventType,
			ProjectID: &projectID, LoopID: &loopID, RunID: &runID,
			EntityType: &entityType, EntityID: &executionID, PayloadJSON: `{}`, CreatedAt: quarantinedAt,
		}); err != nil {
			t.Fatalf("Events.Append(%s) error = %v", seeded.suffix, err)
		}
	}

	debt, err := CountOutstandingQuarantineDebt(context.Background(), repos)
	if err != nil {
		t.Fatalf("CountOutstandingQuarantineDebt() error = %v", err)
	}
	if debt.QuarantinedActiveExecutions != 2 || debt.QuarantinedRunningRuns != 2 {
		t.Fatalf("debt counters = %#v, want 2 executions and 2 running runs", debt)
	}
	want := []OutstandingQuarantinedLoop{
		{LoopID: "loop_roster_a", Seq: 35, Type: "fixer", Target: "MumuTW/looper#126", Status: "paused", QuarantinedAt: quarantinedAt},
		{LoopID: "loop_roster_b", Seq: 36, Type: "fixer", Target: "MumuTW/looper#125", Status: "paused", QuarantinedAt: quarantinedAt},
	}
	if !reflect.DeepEqual(debt.Loops, want) {
		t.Fatalf("debt.Loops = %#v, want %#v", debt.Loops, want)
	}
}
