package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestRuntimeReconcileStaleRunningRunsRequeuesExpiredMissingPIDExecution(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	rt, repos := newStaleExecutionLeaseRuntime(t, now, nil)
	loopID, runID, queueID := seedStaleExecutionLeaseRun(t, repos, now, "missing_pid")
	oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID:              "execution_missing_pid",
		ProjectID:       stringPtr("project_1"),
		LoopID:          &loopID,
		RunID:           &runID,
		Vendor:          "codex",
		Status:          "running",
		CommandJSON:     stringPtr(`{"command":"codex","args":["exec"]}`),
		CWD:             stringPtr(t.TempDir()),
		LastHeartbeatAt: &oldISO,
		StartedAt:       oldISO,
		CreatedAt:       oldISO,
		UpdatedAt:       oldISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	summary, err := rt.ReconcileStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
	}
	if summary.InterruptedRuns != 1 || summary.LoopsRequeued != 1 || summary.CleanedExecutions != 1 {
		t.Fatalf("summary = %#v, want expired execution finalized and work requeued", summary)
	}

	execution, err := repos.AgentExecutions.GetByID(context.Background(), "execution_missing_pid")
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if execution == nil || execution.Status != "failed" || execution.EndedAt == nil {
		t.Fatalf("execution = %#v, want terminal failed stale execution", execution)
	}
	run, _ := repos.Runs.GetByID(context.Background(), runID)
	loop, _ := repos.Loops.GetByID(context.Background(), loopID)
	queue, _ := repos.Queue.GetByID(context.Background(), queueID)
	if run == nil || run.Status != "interrupted" {
		t.Fatalf("run = %#v, want interrupted", run)
	}
	if loop == nil || loop.Status != "queued" {
		t.Fatalf("loop = %#v, want queued", loop)
	}
	if queue == nil || queue.Status != "queued" {
		t.Fatalf("queue = %#v, want queued", queue)
	}
	events, err := repos.Events.ListByEntity(context.Background(), "agent_execution", "execution_missing_pid")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if !containsEventType(events, "looperd.recovery.execution_stale") {
		t.Fatalf("events = %#v, want operator-visible execution_stale event", events)
	}
	if !containsEventType(events, "looperd.recovery.execution_stale_requeued") {
		t.Fatalf("events = %#v, want operator-visible stale-requeued outcome", events)
	}
}

func TestRuntimeReconcileStaleRunningRunsRequeuesExpiredMismatchedPID(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		reconcile func(*Runtime) (StaleRunReconcileSummary, error)
	}{
		{name: "live", reconcile: func(rt *Runtime) (StaleRunReconcileSummary, error) {
			return rt.reconcileLiveStaleRunningRuns(context.Background())
		}},
		{name: "manual", reconcile: func(rt *Runtime) (StaleRunReconcileSummary, error) {
			return rt.ReconcileStaleRunningRuns(context.Background())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
			rt, repos := newStaleExecutionLeaseRuntime(t, now, func(context.Context, int) (string, error) {
				return "python unrelated.py", nil
			})
			loopID, runID, _ := seedStaleExecutionLeaseRun(t, repos, now, "mismatched_"+tc.name)
			oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))
			pid := int64(2201)
			if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
				ID: "execution_mismatched_" + tc.name, ProjectID: stringPtr("project_1"),
				LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running",
				PID: &pid, CommandJSON: stringPtr(`{"command":"codex","args":["exec"]}`),
				LastHeartbeatAt: &oldISO, StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO,
			}); err != nil {
				t.Fatalf("AgentExecutions.Upsert() error = %v", err)
			}

			summary, err := tc.reconcile(rt)
			if err != nil {
				t.Fatalf("reconcile error = %v", err)
			}
			if summary.InterruptedRuns != 1 || summary.LoopsRequeued != 1 || summary.CleanedExecutions != 1 {
				t.Fatalf("summary = %#v, want expired mismatched execution requeued", summary)
			}
		})
	}
}

func TestRuntimeReconcileStaleRunningRunsNeverOverlapsMatchingLiveProcess(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	rt, repos := newStaleExecutionLeaseRuntime(t, now, func(context.Context, int) (string, error) {
		return "codex exec", nil
	})
	loopID, runID, _ := seedStaleExecutionLeaseRun(t, repos, now, "matching_live")
	oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))
	pid := int64(2202)
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: "execution_matching_live", ProjectID: stringPtr("project_1"),
		LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running",
		PID: &pid, CommandJSON: stringPtr(`{"command":"codex","args":["exec"]}`),
		MetadataJSON:    stringPtr(`{"processIdentity":{"startTime":2202}}`),
		LastHeartbeatAt: &oldISO, StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	summary, err := rt.ReconcileStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
	}
	if summary.InterruptedRuns != 0 || summary.LoopsRequeued != 0 || summary.CleanedExecutions != 0 {
		t.Fatalf("summary = %#v, want matching live process to block overlap", summary)
	}
	run, _ := repos.Runs.GetByID(context.Background(), runID)
	if run == nil || run.Status != "running" {
		t.Fatalf("run = %#v, want running", run)
	}
	events, err := repos.Events.ListByEntity(context.Background(), "agent_execution", "execution_matching_live")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if !containsEventType(events, "looperd.recovery.execution_active") {
		t.Fatalf("events = %#v, want operator-visible execution_active event", events)
	}
}

func TestRuntimeReconcileStaleRunningRunsParksFreshInvalidIdentityForConfirmation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	rt, repos := newStaleExecutionLeaseRuntime(t, now, nil)
	loopID, runID, queueID := seedStaleExecutionLeaseRun(t, repos, now, "fresh_missing_pid")
	nowISO := formatJavaScriptISOString(now)
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: "execution_fresh_missing_pid", ProjectID: stringPtr("project_1"),
		LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running",
		LastHeartbeatAt: &nowISO, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	summary, err := rt.ReconcileStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
	}
	if summary.SkippedUncertainRuns != 1 || summary.QuarantinedExecutions != 1 || summary.InterruptedRuns != 0 {
		t.Fatalf("summary = %#v, want explicit confirmation-needed quarantine", summary)
	}
	loop, _ := repos.Loops.GetByID(context.Background(), loopID)
	queue, _ := repos.Queue.GetByID(context.Background(), queueID)
	if loop == nil || loop.Status != "paused" || queue == nil || queue.Status != "manual_intervention" {
		t.Fatalf("loop = %#v queue = %#v, want paused/manual_intervention", loop, queue)
	}
	events, err := repos.Events.ListByEntity(context.Background(), "agent_execution", "execution_fresh_missing_pid")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if !containsEventType(events, "looperd.recovery.execution_confirmation_needed") {
		t.Fatalf("events = %#v, want operator-visible confirmation-needed event", events)
	}
}

func TestRuntimeReconcileStaleRunningRunsReleasesLivenessQuarantineAfterExpiry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	rt, repos := newStaleExecutionLeaseRuntime(t, now, nil)
	loopID, runID, _ := seedStaleExecutionLeaseRun(t, repos, now, "confirmation_then_expired")
	nowISO := formatJavaScriptISOString(now)
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: "execution_confirmation_then_expired", ProjectID: stringPtr("project_1"),
		LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running",
		LastHeartbeatAt: &nowISO, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	if _, err := rt.ReconcileStaleRunningRuns(context.Background()); err != nil {
		t.Fatalf("first reconcile error = %v", err)
	}

	later := now.Add(executionLivenessLeaseTTL + time.Minute)
	rt.mu.Lock()
	rt.now = func() time.Time { return later }
	rt.mu.Unlock()
	summary, err := rt.ReconcileStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("second reconcile error = %v", err)
	}
	if summary.CleanedExecutions != 1 || summary.LoopsRequeued != 1 || summary.QueueItemsRequeued != 1 {
		t.Fatalf("summary = %#v, want confirmation quarantine released and work requeued", summary)
	}
	loop, _ := repos.Loops.GetByID(context.Background(), loopID)
	activeQueue, _ := repos.Queue.FindActiveByLoopID(context.Background(), loopID)
	if loop == nil || loop.Status != "queued" || activeQueue == nil || activeQueue.Status != "queued" {
		t.Fatalf("loop = %#v activeQueue = %#v, want queued recovery", loop, activeQueue)
	}
	events, _ := repos.Events.ListByEntity(context.Background(), "agent_execution", "execution_confirmation_then_expired")
	if !containsEventType(events, "looperd.recovery.execution_stale_requeued") {
		t.Fatalf("events = %#v, want stale-requeued outcome", events)
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
		ReadProcessCommand: readProcess,
		ReadProcessStart:   func(context.Context, int) (int64, error) { return 2202, nil },
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
	prNumber := int64(22)
	targetID := "pr:nexu-io/looper:22"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 22, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request",
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
