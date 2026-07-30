package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

// A daemon that dies leaves running rows behind. The next boot settles them in
// one pass — the case #149 reported as terminal, where a second restart changed
// nothing because the quarantine had no exit.
func TestRuntimeStartupSettlesExecutionsFromAPreviousDaemon(t *testing.T) {
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
	if execution == nil || execution.Status != "killed" || execution.EndedAt == nil {
		t.Fatalf("execution = %#v, want finalized row", execution)
	}
	if run == nil || run.Status == "running" {
		t.Fatalf("run = %#v, want the run finalized", run)
	}
	if loop == nil || loop.Status == "paused" {
		t.Fatalf("loop = %#v, want the loop released rather than parked", loop)
	}
	if queue == nil || queue.Status == "manual_intervention" {
		t.Fatalf("queue = %#v, want the claim released", queue)
	}

	// The daemon returns to healthy on this boot: no second restart, no manual
	// database edit.
	debt, err := CountOutstandingQuarantineDebt(context.Background(), repos)
	if err != nil {
		t.Fatalf("CountOutstandingQuarantineDebt() error = %v", err)
	}
	if debt.QuarantinedActiveExecutions != 0 || debt.QuarantinedRunningRuns != 0 || len(debt.Loops) != 0 {
		t.Fatalf("debt = %#v, want zero outstanding debt after settlement", debt)
	}

	// The audit trail survives the settlement.
	events, _ := repos.Events.ListByEntity(context.Background(), "agent_execution", "execution_startup_stale")
	if !containsEventType(events, recoveryExecutionQuarantinedEventType) {
		t.Fatalf("events = %#v, want the settlement recorded for audit", events)
	}
	if !containsEventType(events, "agent.killed") {
		t.Fatalf("events = %#v, want the finalization recorded", events)
	}
}

// A loop parked by a pre-#149 daemon is released once, on the first boot that
// carries the fence, and the marker keeps the second boot from redoing it.
func TestRuntimeStartupReleasesPreFencingParkExactlyOnce(t *testing.T) {
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
	oldISO := formatJavaScriptISOString(now.Add(-24 * time.Hour))

	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	loopID, runID, queueID := seedStaleExecutionLeaseRun(t, repos, now, "pre_fencing")
	seedPreFencingPark(t, repos, loopID, runID, queueID, "execution_pre_fencing", oldISO,
		"needs confirmation: startup liveness evidence is not authoritative (pid_not_running_not_confirmed_dead)")
	if err := coordinator.Close(); err != nil {
		t.Fatalf("coordinator.Close() error = %v", err)
	}

	first := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	repos = first.Services().Repositories
	loop, _ := repos.Loops.GetByID(context.Background(), loopID)
	if loop == nil || loop.Status != "queued" {
		t.Fatalf("loop after first boot = %#v, want released to queued", loop)
	}
	settlements, err := repos.Events.ListByType(context.Background(), preFencingSettlementEventType)
	if err != nil {
		t.Fatalf("Events.ListByType() error = %v", err)
	}
	if len(settlements) != 1 {
		t.Fatalf("settlement events after first boot = %d, want 1", len(settlements))
	}
	first.Stop("restart test")

	later := now.Add(time.Hour)
	second := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return later }})
	if err := second.Start(context.Background()); err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	t.Cleanup(func() { second.Stop("test cleanup") })
	settlements, err = second.Services().Repositories.Events.ListByType(context.Background(), preFencingSettlementEventType)
	if err != nil {
		t.Fatalf("Events.ListByType() error = %v", err)
	}
	if len(settlements) != 1 {
		t.Fatalf("settlement events after second boot = %d, want the one-shot marker to hold", len(settlements))
	}
}

// The settlement releases quarantine parks and nothing else. A human takeover
// and a loop paused for a domain reason both look identical in `status`; only
// the failure text distinguishes them, and both must survive.
func TestRuntimeStartupPreFencingSettlementLeavesHumanAndDomainParks(t *testing.T) {
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
	oldISO := formatJavaScriptISOString(now.Add(-24 * time.Hour))

	coordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	repos := storage.NewRepositories(coordinator.DB())

	takeoverLoop, takeoverRun, takeoverQueue := seedStaleExecutionLeaseRun(t, repos, now, "human_takeover")
	seedPreFencingPark(t, repos, takeoverLoop, takeoverRun, takeoverQueue, "execution_human_takeover", oldISO,
		"needs confirmation: startup liveness evidence is not authoritative (pid_absent)")
	takeover, _ := repos.Loops.GetByID(context.Background(), takeoverLoop)
	takeover.Status = "human_takeover"
	// Establishing the hold is a lifecycle operation, so it needs the authority
	// entry point (#273). Plain Upsert is fenced — which is the second guarantee
	// under the one this test asserts: even if the settlement's predicate were
	// wrong, the repository would refuse the write.
	if err := repos.Loops.UpsertChangingHumanHold(context.Background(), *takeover); err != nil {
		t.Fatalf("Loops.UpsertChangingHumanHold(human_takeover) error = %v", err)
	}

	domainLoop, domainRun, domainQueue := seedStaleExecutionLeaseRun(t, repos, now, "domain_hold")
	seedPreFencingPark(t, repos, domainLoop, domainRun, domainQueue, "execution_domain_hold", oldISO,
		"risky conflict fixes require manual intervention")

	if err := coordinator.Close(); err != nil {
		t.Fatalf("coordinator.Close() error = %v", err)
	}

	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return now }})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })
	repos = rt.Services().Repositories

	if loop, _ := repos.Loops.GetByID(context.Background(), takeoverLoop); loop == nil || loop.Status != "human_takeover" {
		t.Fatalf("human takeover loop = %#v, want untouched", loop)
	}
	if loop, _ := repos.Loops.GetByID(context.Background(), domainLoop); loop == nil || loop.Status != "paused" {
		t.Fatalf("domain-hold loop = %#v, want to stay paused", loop)
	}
}

// seedPreFencingPark writes the exact durable shape a pre-#149 daemon left:
// a running execution with quarantine evidence, a paused loop, and a queue item
// failed with the old quarantine message.
func seedPreFencingPark(t *testing.T, repos *storage.Repositories, loopID, runID, queueID, executionID, atISO, reason string) {
	t.Helper()
	ctx := context.Background()
	if err := repos.AgentExecutions.Upsert(ctx, storage.AgentExecutionRecord{
		ID: executionID, ProjectID: stringPtr("project_1"),
		LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running",
		CommandJSON: stringPtr(`{"command":"codex","args":["exec"]}`),
		StartedAt:   atISO, CreatedAt: atISO, UpdatedAt: atISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	if err := repos.Events.Append(ctx, storage.EventLogRecord{
		ID: "event_quarantine_" + executionID, EventType: recoveryExecutionQuarantinedEventType,
		ProjectID: stringPtr("project_1"), LoopID: &loopID, RunID: &runID,
		EntityType: stringPtr("agent_execution"), EntityID: stringPtr(executionID),
		PayloadJSON: `{"reason":"` + reason + `"}`, CreatedAt: atISO,
	}); err != nil {
		t.Fatalf("Events.Append() error = %v", err)
	}
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	loop.Status = "paused"
	loop.NextRunAt = nil
	loop.UpdatedAt = atISO
	if err := repos.Loops.Upsert(ctx, *loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	message := reason
	if err := repos.Queue.Fail(ctx, storage.QueueFailInput{
		ID: queueID, FinishedAt: atISO, UpdatedAt: atISO,
		ErrorMessage: &message, ErrorKind: "manual_intervention",
	}); err != nil {
		t.Fatalf("Queue.Fail() error = %v", err)
	}
}
