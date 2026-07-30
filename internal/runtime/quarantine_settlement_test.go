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

// quarantineSettlementFixture seeds one quarantined execution whose loop sits at
// loopStatus, plus the run and queue rows recovery leaves behind.
type quarantineSettlementFixture struct {
	runtime *Runtime
	repos   *storage.Repositories
	// signaled records every raw PID signal attempt; settlement must never add one.
	signaled    *[]int
	loopID      string
	runID       string
	executionID string
	nowISO      string
}

func newQuarantineSettlementFixture(t *testing.T, loopStatus string, processAlive bool) quarantineSettlementFixture {
	t.Helper()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir

	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	quarantinedISO := formatJavaScriptISOString(now.Add(-24 * time.Hour))

	quarantinedPID := int64(7777)
	signaled := make([]int, 0)
	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		Now:    func() time.Time { return now },
		ReadProcessCommand: func(_ context.Context, pid int) (string, error) {
			if !processAlive || pid != int(quarantinedPID) {
				return "", nil
			}
			return "codex exec", nil
		},
		ReadProcessStart:  func(context.Context, int) (int64, error) { return 777700, nil },
		ReadProcessBootID: func(context.Context, int) (string, error) { return "boot-test", nil },
		SignalProcess: func(pid int, _ syscall.Signal) error {
			signaled = append(signaled, pid)
			return nil
		},
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	repos := rt.Services().Repositories
	ctx := context.Background()
	projectID := "project_settlement"
	loopID := "loop_settlement"
	runID := "run_settlement"
	executionID := "exec_settlement"

	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Settlement", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: quarantinedISO, UpdatedAt: quarantinedISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: loopStatus, CreatedAt: quarantinedISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", StartedAt: quarantinedISO, LastHeartbeatAt: &quarantinedISO, CreatedAt: quarantinedISO, UpdatedAt: quarantinedISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if err := repos.AgentExecutions.Upsert(ctx, storage.AgentExecutionRecord{
		ID: executionID, ProjectID: &projectID, LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running",
		PID: &quarantinedPID, CommandJSON: stringPtr(`{"command":"codex","args":["exec"]}`), CWD: stringPtr(workingDir),
		MetadataJSON: stringPtr(`{"processIdentity":{"startTime":777700,"bootId":"boot-test"}}`),
		StartedAt:    quarantinedISO, CreatedAt: quarantinedISO, UpdatedAt: quarantinedISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	entityType := "agent_execution"
	entityID := executionID
	if err := repos.Events.Append(ctx, storage.EventLogRecord{
		ID: "event_quarantined_settlement", EventType: recoveryExecutionQuarantinedEventType,
		ProjectID: &projectID, LoopID: &loopID, RunID: &runID,
		EntityType: &entityType, EntityID: &entityID,
		PayloadJSON: `{"reason":"needs confirmation: startup liveness evidence is not authoritative"}`,
		CreatedAt:   quarantinedISO,
	}); err != nil {
		t.Fatalf("Events.Append() error = %v", err)
	}

	return quarantineSettlementFixture{
		runtime: rt, repos: repos, signaled: &signaled,
		loopID: loopID, runID: runID, executionID: executionID, nowISO: nowISO,
	}
}

func (f quarantineSettlementFixture) debt(t *testing.T) OutstandingQuarantineDebt {
	t.Helper()
	debt, err := CountOutstandingQuarantineDebt(context.Background(), f.repos)
	if err != nil {
		t.Fatalf("CountOutstandingQuarantineDebt() error = %v", err)
	}
	return debt
}

func (f quarantineSettlementFixture) eventTypes(t *testing.T) []string {
	t.Helper()
	events, err := f.repos.Events.ListByEntity(context.Background(), "agent_execution", f.executionID)
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.EventType)
	}
	return types
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// A quarantined execution whose loop the operator already retried is evidence of
// a past incident, not an ongoing one: it settles, the daemon leaves the
// degraded state without a restart, and the audit trail survives.
func TestQuarantineSettlementRetiresOperatorDisposedExecution(t *testing.T) {
	t.Parallel()

	fixture := newQuarantineSettlementFixture(t, "queued", false)
	// The loop is already disposed, so the counter stops reporting it even
	// before the settlement pass rewrites the rows.
	if before := fixture.debt(t); before.QuarantinedActiveExecutions != 0 || before.QuarantinedRunningRuns != 0 {
		t.Fatalf("debt before settlement = %#v, want zero for an operator-disposed loop", before)
	}

	summary, err := fixture.runtime.ReconcileStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
	}
	if summary.QuarantineSettlement.SettledExecutions != 1 {
		t.Fatalf("SettledExecutions = %d, want 1 (summary %#v)", summary.QuarantineSettlement.SettledExecutions, summary.QuarantineSettlement)
	}
	if summary.QuarantineSettlement.SettledRuns != 1 {
		t.Fatalf("SettledRuns = %d, want 1", summary.QuarantineSettlement.SettledRuns)
	}

	execution, err := fixture.repos.AgentExecutions.GetByID(context.Background(), fixture.executionID)
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if execution.Status != executionStatusQuarantineSettled {
		t.Fatalf("execution status = %q, want %q", execution.Status, executionStatusQuarantineSettled)
	}
	if durableTerminalExecution(execution.Status) {
		t.Fatalf("settled status %q must not read as durable terminal finalization; settling is not confirmed-dead Authority", execution.Status)
	}

	run, err := fixture.repos.Runs.GetByID(context.Background(), fixture.runID)
	if err != nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run.Status == "running" {
		t.Fatalf("run status = %q, want the run closed so activeRuns is not inflated", run.Status)
	}

	// Daemon returns to healthy: the counter feeding quarantine_orphan_debt is
	// clear, with no restart and no manual database edit.
	after := fixture.debt(t)
	if after.QuarantinedActiveExecutions != 0 || after.QuarantinedRunningRuns != 0 {
		t.Fatalf("debt after settlement = %#v, want zero", after)
	}

	types := fixture.eventTypes(t)
	if !containsString(types, recoveryExecutionQuarantinedEventType) {
		t.Fatalf("event types = %v, want the original quarantine event preserved", types)
	}
	if !containsString(types, recoveryExecutionQuarantineRetiredEventType) {
		t.Fatalf("event types = %v, want a retirement event", types)
	}
	if len(*fixture.signaled) != 0 {
		t.Fatalf("SignalProcess called for pids %v; settlement must not kill processes", *fixture.signaled)
	}
}

// The distinction #150 asks for: a quarantined execution whose process is still
// there stays debt even after the operator disposed of the loop.
func TestQuarantineSettlementRetainsLiveExecutionAsDebt(t *testing.T) {
	t.Parallel()

	fixture := newQuarantineSettlementFixture(t, "queued", true)

	summary, err := fixture.runtime.ReconcileStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
	}
	if summary.QuarantineSettlement.SettledExecutions != 0 {
		t.Fatalf("SettledExecutions = %d, want 0 while the process is observed live", summary.QuarantineSettlement.SettledExecutions)
	}
	if summary.QuarantineSettlement.LiveExecutionsRetained != 1 {
		t.Fatalf("LiveExecutionsRetained = %d, want 1", summary.QuarantineSettlement.LiveExecutionsRetained)
	}

	execution, err := fixture.repos.AgentExecutions.GetByID(context.Background(), fixture.executionID)
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if execution.Status != "running" {
		t.Fatalf("execution status = %q, want it left running while the process matches", execution.Status)
	}
	if types := fixture.eventTypes(t); containsString(types, recoveryExecutionQuarantineRetiredEventType) {
		t.Fatalf("event types = %v, want no retirement while the process is live", types)
	}
	if len(*fixture.signaled) != 0 {
		t.Fatalf("SignalProcess called for pids %v; settlement must not kill processes", *fixture.signaled)
	}
}

// A dead PID is not Authority on its own: while the loop is still parked, the
// work is genuinely outstanding and the daemon stays degraded until a human
// decides something.
func TestQuarantineSettlementKeepsParkedLoopAsDebt(t *testing.T) {
	t.Parallel()

	fixture := newQuarantineSettlementFixture(t, "paused", false)

	before := fixture.debt(t)
	if before.QuarantinedActiveExecutions != 1 || before.QuarantinedRunningRuns != 1 {
		t.Fatalf("debt before settlement = %#v, want 1 execution and 1 running run", before)
	}

	summary, err := fixture.runtime.ReconcileStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
	}
	if summary.QuarantineSettlement.SettledExecutions != 0 {
		t.Fatalf("SettledExecutions = %d, want 0 while the loop is still parked", summary.QuarantineSettlement.SettledExecutions)
	}
	if summary.QuarantineSettlement.ParkedExecutionsRetained != 1 {
		t.Fatalf("ParkedExecutionsRetained = %d, want 1", summary.QuarantineSettlement.ParkedExecutionsRetained)
	}

	after := fixture.debt(t)
	if after.QuarantinedActiveExecutions != 1 {
		t.Fatalf("debt after settlement = %#v, want the parked execution still counted", after)
	}
	if len(after.Loops) != 1 || after.Loops[0].LoopID != fixture.loopID {
		t.Fatalf("debt roster = %#v, want the parked loop listed for the operator", after.Loops)
	}
}

// human_takeover is a deliberate park, not a disposition.
func TestQuarantineSettlementKeepsHumanTakeoverAsDebt(t *testing.T) {
	t.Parallel()

	fixture := newQuarantineSettlementFixture(t, "human_takeover", false)

	summary, err := fixture.runtime.ReconcileStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
	}
	if summary.QuarantineSettlement.SettledExecutions != 0 {
		t.Fatalf("SettledExecutions = %d, want 0 for a loop parked on a human", summary.QuarantineSettlement.SettledExecutions)
	}
	if debt := fixture.debt(t); debt.QuarantinedActiveExecutions != 1 {
		t.Fatalf("debt = %#v, want the taken-over loop still counted", debt)
	}
}

// Stopping the loop is a disposition too, so close/stop clears the debt the same
// way retry does.
func TestQuarantineSettlementRetiresStoppedLoop(t *testing.T) {
	t.Parallel()

	fixture := newQuarantineSettlementFixture(t, "terminated", false)

	summary, err := fixture.runtime.ReconcileStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
	}
	if summary.QuarantineSettlement.SettledExecutions != 1 {
		t.Fatalf("SettledExecutions = %d, want 1 for a terminated loop", summary.QuarantineSettlement.SettledExecutions)
	}
	if debt := fixture.debt(t); debt.QuarantinedActiveExecutions != 0 || debt.QuarantinedRunningRuns != 0 {
		t.Fatalf("debt = %#v, want zero after the loop was stopped", debt)
	}
}
