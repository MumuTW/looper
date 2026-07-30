package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

// The four PID shapes this file used to enumerate — absent, mismatched,
// matching, and fresh-but-invalid — all produce the same outcome now. The
// classifier does not read PIDs, so there is one behavior to assert, not a
// matrix: an execution this daemon does not own is settled under worktree
// generation containment.
func TestRuntimeReconcileStaleRunningRunsSettlesExecutionsNoDaemonOwns(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		pid       *int64
		process   ReadProcessCommandFunc
		heartbeat func(time.Time) time.Time
	}{
		{name: "no pid recorded", heartbeat: func(now time.Time) time.Time { return now.Add(-2 * time.Hour) }},
		{name: "pid recorded but process gone", pid: int64Ptr(2201), heartbeat: func(now time.Time) time.Time { return now.Add(-2 * time.Hour) }},
		{
			name:      "pid recorded and a matching process is running",
			pid:       int64Ptr(2202),
			process:   func(context.Context, int) (string, error) { return "codex exec", nil },
			heartbeat: func(now time.Time) time.Time { return now.Add(-2 * time.Hour) },
		},
		{
			name:      "pid recorded and an unrelated process is running",
			pid:       int64Ptr(2203),
			process:   func(context.Context, int) (string, error) { return "python unrelated.py", nil },
			heartbeat: func(now time.Time) time.Time { return now.Add(-2 * time.Hour) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
			rt, repos := newStaleExecutionLeaseRuntime(t, now, tc.process)
			suffix := strings.ReplaceAll(tc.name, " ", "_")
			loopID, runID, queueID := seedStaleExecutionLeaseRun(t, repos, now, suffix)
			heartbeatISO := formatJavaScriptISOString(tc.heartbeat(now))
			oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))
			executionID := "execution_" + suffix
			if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
				ID: executionID, ProjectID: stringPtr("project_1"),
				LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running",
				PID: tc.pid, CommandJSON: stringPtr(`{"command":"codex","args":["exec"]}`),
				MetadataJSON:    stringPtr(`{"processIdentity":{"startTime":2202,"bootId":"boot-a"}}`),
				CWD:             stringPtr(t.TempDir()),
				LastHeartbeatAt: &heartbeatISO, StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO,
			}); err != nil {
				t.Fatalf("AgentExecutions.Upsert() error = %v", err)
			}

			summary, err := rt.ReconcileStaleRunningRuns(context.Background())
			if err != nil {
				t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
			}
			if summary.InterruptedRuns != 1 || summary.SettledExecutions != 1 {
				t.Fatalf("summary = %#v, want the run interrupted and its execution settled", summary)
			}

			execution, err := repos.AgentExecutions.GetByID(context.Background(), executionID)
			if err != nil {
				t.Fatalf("AgentExecutions.GetByID() error = %v", err)
			}
			if execution == nil || execution.Status != "killed" || execution.EndedAt == nil {
				t.Fatalf("execution = %#v, want finalized row", execution)
			}
			run, _ := repos.Runs.GetByID(context.Background(), runID)
			if run == nil || run.Status != "interrupted" {
				t.Fatalf("run = %#v, want interrupted", run)
			}
			queue, _ := repos.Queue.GetByID(context.Background(), queueID)
			if queue == nil || queue.Status == "manual_intervention" {
				t.Fatalf("queue = %#v, want the claim released rather than parked", queue)
			}
			events, err := repos.Events.ListByEntity(context.Background(), "agent_execution", executionID)
			if err != nil {
				t.Fatalf("Events.ListByEntity() error = %v", err)
			}
			if !containsEventType(events, recoveryExecutionQuarantinedEventType) {
				t.Fatalf("events = %#v, want the settlement recorded for audit", events)
			}
			if !containsEventType(events, "looperd.recovery.execution_settleable") {
				t.Fatalf("events = %#v, want the classification recorded", events)
			}
		})
	}
}

// The counterpart, and the only thing that still blocks recovery: this daemon
// holds the supervisor handle, so the work is genuinely in flight here.
func TestRuntimeReconcileStaleRunningRunsNeverSettlesExecutionThisDaemonOwns(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	rt, repos := newStaleExecutionLeaseRuntime(t, now, nil)
	loopID, runID, _ := seedStaleExecutionLeaseRun(t, repos, now, "owned_by_this_daemon")
	oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))
	executionID := "execution_owned_by_this_daemon"
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: executionID, ProjectID: stringPtr("project_1"),
		LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running",
		LastHeartbeatAt: &oldISO, StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	release := rt.Services().ActiveExecutions.Register(loopID, runID, executionID, stubAgentExecution{})
	defer release()

	summary, err := rt.ReconcileStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
	}
	if summary.InterruptedRuns != 0 || summary.SettledExecutions != 0 || summary.LoopsRequeued != 0 {
		t.Fatalf("summary = %#v, want current supervisor owner preserved", summary)
	}
	execution, _ := repos.AgentExecutions.GetByID(context.Background(), executionID)
	if execution == nil || execution.Status != "running" {
		t.Fatalf("execution = %#v, want untouched running row", execution)
	}
	events, _ := repos.Events.ListByEntity(context.Background(), "agent_execution", executionID)
	if !containsEventType(events, "looperd.recovery.execution_active") {
		t.Fatalf("events = %#v, want operator-visible execution_active event", events)
	}
}

func newStaleExecutionLeaseRuntime(t *testing.T, now time.Time, readProcess ReadProcessCommandFunc) (*Runtime, *storage.Repositories) {
	t.Helper()
	root := t.TempDir()
	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(root, "runtime.sqlite")
	backupDir := filepath.Join(root, "backups")
	cfg.Storage.BackupDir = &backupDir
	rt := New(Options{
		Config:             cfg,
		Logger:             &testLogger{},
		Now:                func() time.Time { return now },
		RunSchedulerTick:   func(context.Context, Services) error { return nil },
		ReadProcessCommand: readProcess,
		ReadProcessStart:   func(context.Context, int) (int64, error) { return 2202, nil },
		ReadProcessBootID:  func(context.Context, int) (string, error) { return "boot-a", nil },
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })
	return rt, rt.Services().Repositories
}

func seedStaleExecutionLeaseRun(t *testing.T, repos *storage.Repositories, now time.Time, suffix string) (string, string, string) {
	t.Helper()
	nowISO := formatJavaScriptISOString(now)
	oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))
	loopID := "loop_lease_" + suffix
	runID := "run_lease_" + suffix
	queueID := "queue_lease_" + suffix
	repo := "nexu-io/looper"
	// Seeds must not collide when a test seeds several loops: loops.seq is
	// unique and the PR target is the loop's identity.
	prNumber := int64(22 + len(suffix))
	targetID := fmt.Sprintf("pr:nexu-io/looper:%d", prNumber)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: prNumber, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request",
		TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "running",
		CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID: runID, LoopID: loopID, Status: "running", CurrentStep: stringPtr("repair"),
		LastHeartbeatAt: &oldISO, StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: queueID, ProjectID: stringPtr("project_1"), LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", TargetID: targetID, Repo: &repo, PRNumber: &prNumber,
		DedupeKey: "fixer:project_1:" + loopID, Priority: storage.QueuePriorityFixer,
		Status: "running", AvailableAt: oldISO, Attempts: 1, MaxAttempts: 3,
		ClaimedBy: stringPtr("scheduler"), ClaimedAt: &oldISO, StartedAt: &oldISO,
		CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	return loopID, runID, queueID
}
