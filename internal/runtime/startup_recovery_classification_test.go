package runtime

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

type startupRecoveryFixture struct {
	runtime     *Runtime
	repos       *storage.Repositories
	signals     *[]syscall.Signal
	loopID      string
	runID       string
	queueID     string
	executionID string
}

// seedStartupRecoveryFixture builds the exact durable shape a hard daemon death
// leaves behind: a running loop, a running run, a claimed queue item, and a
// running agent_executions row.
func seedStartupRecoveryFixture(t *testing.T, name string, startedAt time.Time, configure func(*Options)) startupRecoveryFixture {
	t.Helper()
	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	nowISO := formatJavaScriptISOString(startedAt)
	oldISO := formatJavaScriptISOString(startedAt.Add(-time.Hour))

	projectID := "project_" + name
	loopID := "loop_" + name
	runID := "run_" + name
	queueID := "queue_" + name
	executionID := "agent_" + name

	seedCoordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	seedRepos := storage.NewRepositories(seedCoordinator.DB())
	if err := seedRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: name, RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := seedRepos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := seedRepos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", CurrentStep: stringPtr("execute"), StartedAt: oldISO, LastHeartbeatAt: &oldISO, CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if err := seedRepos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:" + projectID + ":" + loopID, Priority: storage.QueuePriorityWorker, Status: "running",
		AvailableAt: oldISO, Attempts: 1, MaxAttempts: 3, ClaimedBy: stringPtr("scheduler"), ClaimedAt: stringPtr(oldISO),
		StartedAt: stringPtr(oldISO), CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	pid := int64(7777)
	if err := seedRepos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: executionID, ProjectID: &projectID, LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running",
		PID: &pid, CommandJSON: stringPtr(`{"command":"codex","args":["exec"]}`), CWD: stringPtr(workingDir),
		LastHeartbeatAt: &nowISO, StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	if err := seedCoordinator.Close(); err != nil {
		t.Fatalf("seed close error = %v", err)
	}

	signals := []syscall.Signal{}
	options := Options{
		Config: cfg,
		Logger: &testLogger{},
		Now:    func() time.Time { return startedAt },
		SignalProcess: func(_ int, sig syscall.Signal) error {
			signals = append(signals, sig)
			return nil
		},
	}
	if configure != nil {
		configure(&options)
	}
	rt := New(options)
	return startupRecoveryFixture{runtime: rt, signals: &signals, loopID: loopID, runID: runID, queueID: queueID, executionID: executionID}
}

// Contract (ADR-0015 R8, revised for #149): recovery settles a row no daemon
// owns, and it does so without ever signalling a process. Containment comes from
// retiring the worktree generation, not from proving the leader exited — which
// is why the settlement is safe even though descendants may still be alive.
func TestStartupRecoverySettlesPreviousDaemonRowWithoutSignalling(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, time.July, 19, 10, 0, 0, 0, time.UTC)
	fixture := seedStartupRecoveryFixture(t, "leader_exit", startedAt, nil)
	rt := fixture.runtime
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	if len(*fixture.signals) != 0 {
		t.Fatalf("SignalProcess called with %v; want no raw PID/PGID action", *fixture.signals)
	}
	repos := rt.Services().Repositories
	execution, err := repos.AgentExecutions.GetByID(context.Background(), fixture.executionID)
	if err != nil {
		t.Fatalf("GetByID error = %v", err)
	}
	if execution == nil || execution.Status != "killed" || execution.EndedAt == nil {
		t.Fatalf("execution = %#v, want finalized row", execution)
	}
	queue, err := repos.Queue.GetByID(context.Background(), fixture.queueID)
	if err != nil {
		t.Fatalf("Queue.GetByID error = %v", err)
	}
	if queue == nil || queue.Status == "manual_intervention" {
		t.Fatalf("queue = %#v, want the claim released rather than parked", queue)
	}

	recovery := rt.RecoverySummary()
	if recovery.OrphanAgentCleanup.ConfirmedDeadCount != 1 {
		t.Fatalf("ConfirmedDeadCount = %d, want 1", recovery.OrphanAgentCleanup.ConfirmedDeadCount)
	}
	if recovery.OrphanAgentCleanup.QuarantinedCount != 0 {
		t.Fatalf("QuarantinedCount = %d, want 0 — nothing is left waiting for an operator", recovery.OrphanAgentCleanup.QuarantinedCount)
	}

	events, err := repos.Events.ListByEntity(context.Background(), "agent_execution", fixture.executionID)
	if err != nil {
		t.Fatalf("ListByEntity error = %v", err)
	}
	if !containsEventType(events, "looperd.recovery.containment_classified") {
		t.Fatalf("events = %#v, want containment_classified", events)
	}
	if !containsEventType(events, recoveryExecutionQuarantinedEventType) {
		t.Fatalf("events = %#v, want the settlement recorded for audit", events)
	}
}

// The counterpart contract: a row this daemon supervises is never settled,
// never signalled, and keeps its claim parked. This is the case #150 requires
// to keep the daemon degraded, and the guard against over-correcting #149.
func TestStartupRecoveryLeavesCurrentDaemonOwnedExecutionAlone(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, time.July, 19, 11, 0, 0, 0, time.UTC)
	fixture := seedStartupRecoveryFixture(t, "observed_live", startedAt, func(options *Options) {
		options.DeferRecovery = true
	})
	rt := fixture.runtime
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	release := rt.Services().ActiveExecutions.Register(fixture.loopID, fixture.runID, fixture.executionID, stubAgentExecution{})
	defer release()
	if err := rt.CompleteStartup(context.Background()); err != nil {
		t.Fatalf("CompleteStartup() error = %v", err)
	}

	if len(*fixture.signals) != 0 {
		t.Fatalf("SignalProcess called with %v; want no raw PID/PGID action", *fixture.signals)
	}
	repos := rt.Services().Repositories
	execution, err := repos.AgentExecutions.GetByID(context.Background(), fixture.executionID)
	if err != nil {
		t.Fatalf("GetByID error = %v", err)
	}
	if execution == nil || execution.Status != "running" || execution.EndedAt != nil {
		t.Fatalf("execution = %#v, want untouched running row", execution)
	}
	queue, err := repos.Queue.GetByID(context.Background(), fixture.queueID)
	if err != nil {
		t.Fatalf("Queue.GetByID error = %v", err)
	}
	if queue == nil || queue.Status != "manual_intervention" {
		t.Fatalf("queue = %#v, want the claim parked while this daemon owns it", queue)
	}

	recovery := rt.RecoverySummary()
	if recovery.OrphanAgentCleanup.ObservedLiveCount != 1 || recovery.OrphanAgentCleanup.QuarantinedCount != 1 {
		t.Fatalf("cleanup = %#v, want one live execution parked", recovery.OrphanAgentCleanup)
	}
	if recovery.OrphanAgentCleanup.ConfirmedDeadCount != 0 || recovery.OrphanAgentCleanup.CleanedCount != 0 {
		t.Fatalf("cleanup = %#v, want no confirmed-dead / cleaned", recovery.OrphanAgentCleanup)
	}
	if recovery.LoopsRequeued != 0 {
		t.Fatalf("LoopsRequeued = %d, want 0", recovery.LoopsRequeued)
	}

	// The debt is real and keeps the daemon degraded until the work finishes.
	debt, err := CountOutstandingQuarantineDebt(context.Background(), repos)
	if err != nil {
		t.Fatalf("CountOutstandingQuarantineDebt() error = %v", err)
	}
	if debt.QuarantinedActiveExecutions != 1 || len(debt.Loops) != 1 {
		t.Fatalf("debt = %#v, want one outstanding live quarantine", debt)
	}

	// A parked running claim must not be re-claimable (no overlap).
	if err := rt.AllowClaim(); err != nil {
		t.Fatalf("AllowClaim() = %v", err)
	}
	claimed, err := repos.Queue.ClaimNext(context.Background(), formatJavaScriptISOString(startedAt), "scheduler")
	if err != nil {
		t.Fatalf("ClaimNext error = %v", err)
	}
	if claimed != nil {
		t.Fatalf("ClaimNext = %#v, want nil (no overlapping work)", claimed)
	}
}
