package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	looperdruntime "github.com/nexu-io/looper/internal/runtime"
	"github.com/nexu-io/looper/internal/storage"
)

// The real operator path, end to end. Recovery parks a quarantined loop at
// `paused` and deliberately leaves its run at `running`;
// assertLoopRetryPreconditions refuses any retry whose loop has a running run.
// Before the settler ran here, `looper retry` — the remedy `looper status`
// itself prints — returned 409 forever and the daemon could never leave the
// degraded state.
func TestHandlerLoopRetrySettlesQuarantinedRunningRun(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	ctx := context.Background()
	nowISO := "2026-07-30T12:00:00.000Z"
	quarantinedISO := "2026-07-29T12:00:00.000Z"
	projectID := "project_retry_quarantine"
	loopID := "loop_retry_quarantine"
	runID := "run_retry_quarantine"
	executionID := "exec_retry_quarantine"
	targetID := projectID

	if err := services.Repositories.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(ctx, storage.LoopRecord{
		ID: loopID, Seq: 4242, ProjectID: projectID, Type: "worker", TargetType: "project",
		TargetID: &targetID, Status: "paused", CreatedAt: quarantinedISO, UpdatedAt: quarantinedISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	// The run recovery left behind: still running, nothing draining it.
	if err := services.Repositories.Runs.Upsert(ctx, storage.RunRecord{
		ID: runID, LoopID: loopID, Status: "running", StartedAt: quarantinedISO,
		CreatedAt: quarantinedISO, UpdatedAt: quarantinedISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	lastErrorKind := "manual_intervention"
	if err := services.Repositories.Queue.Upsert(ctx, storage.QueueItemRecord{
		ID: "queue_retry_quarantine", ProjectID: &projectID, LoopID: &loopID, Type: "worker",
		TargetType: "project", TargetID: targetID, DedupeKey: "worker:retry_quarantine",
		Priority: storage.QueuePriorityWorker, Status: "manual_intervention", AvailableAt: quarantinedISO,
		Attempts: 1, MaxAttempts: 3, LastErrorKind: &lastErrorKind, CreatedAt: quarantinedISO, UpdatedAt: quarantinedISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	// A PID no live process holds, with the durable birth identity recovery
	// records. The probe therefore reports uncertain, never confirmed-dead.
	deadPID := int64(999999)
	metadata := `{"processIdentity":{"startTime":424200,"bootId":"boot-test"}}`
	command := `{"command":"codex","args":["exec"]}`
	if err := services.Repositories.AgentExecutions.Upsert(ctx, storage.AgentExecutionRecord{
		ID: executionID, ProjectID: &projectID, LoopID: &loopID, RunID: &runID, Vendor: "codex",
		Status: "running", PID: &deadPID, CommandJSON: &command, MetadataJSON: &metadata,
		StartedAt: quarantinedISO, CreatedAt: quarantinedISO, UpdatedAt: quarantinedISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	entityType := "agent_execution"
	entityID := executionID
	if err := services.Repositories.Events.Append(ctx, storage.EventLogRecord{
		ID: "event_retry_quarantine", EventType: "looperd.recovery.execution_quarantined",
		ProjectID: &projectID, LoopID: &loopID, RunID: &runID,
		EntityType: &entityType, EntityID: &entityID, PayloadJSON: `{}`, CreatedAt: quarantinedISO,
	}); err != nil {
		t.Fatalf("Events.Append() error = %v", err)
	}

	debtBefore, err := looperdruntime.CountOutstandingQuarantineDebt(ctx, services.Repositories)
	if err != nil {
		t.Fatalf("CountOutstandingQuarantineDebt() error = %v", err)
	}
	if debtBefore.QuarantinedActiveExecutions != 1 || debtBefore.QuarantinedRunningRuns != 1 {
		t.Fatalf("debt before retry = %#v, want 1 execution and 1 running run", debtBefore)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/4242/retry", strings.NewReader(`{"mode":"auto","resetAttempts":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}

	loop, err := services.Repositories.Loops.GetByID(ctx, loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop.Status != "queued" {
		t.Fatalf("loop status = %q, want queued after a successful retry", loop.Status)
	}
	run, err := services.Repositories.Runs.GetByID(ctx, runID)
	if err != nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run.Status == "running" {
		t.Fatalf("run status = %q, want the quarantined run closed before replacement work is claimable", run.Status)
	}
	execution, err := services.Repositories.AgentExecutions.GetByID(ctx, executionID)
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if execution.Status == "running" {
		t.Fatalf("execution status = %q, want it settled out of the active set", execution.Status)
	}

	debtAfter, err := looperdruntime.CountOutstandingQuarantineDebt(ctx, services.Repositories)
	if err != nil {
		t.Fatalf("CountOutstandingQuarantineDebt() error = %v", err)
	}
	if debtAfter.QuarantinedActiveExecutions != 0 || debtAfter.QuarantinedRunningRuns != 0 {
		t.Fatalf("debt after retry = %#v, want zero so the daemon leaves the degraded state", debtAfter)
	}
}

// Settlement must not outlive the retry it was for. It runs inside the requeue
// transaction, so a precondition failure after it rolls the settlement back
// and leaves the quarantine evidence exactly as it was — rather than clearing
// the debt for a retry that never happened.
func TestHandlerLoopRetryRollsBackSettlementWhenRequeueFails(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	ctx := context.Background()
	nowISO := "2026-07-30T12:00:00.000Z"
	quarantinedISO := "2026-07-29T12:00:00.000Z"
	projectID := "project_retry_quarantine_rollback"
	loopID := "loop_retry_quarantine_rollback"
	otherLoopID := "loop_retry_quarantine_rollback_other"
	runID := "run_retry_quarantine_rollback"
	executionID := "exec_retry_quarantine_rollback"
	targetID := projectID
	dedupeKey := "worker:retry_quarantine_rollback"

	if err := services.Repositories.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	for id, seq := range map[string]int64{loopID: 5150, otherLoopID: 5151} {
		if err := services.Repositories.Loops.Upsert(ctx, storage.LoopRecord{
			ID: id, Seq: seq, ProjectID: projectID, Type: "worker", TargetType: "project",
			TargetID: &targetID, Status: "paused", CreatedAt: quarantinedISO, UpdatedAt: quarantinedISO,
		}); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", id, err)
		}
	}
	if err := services.Repositories.Runs.Upsert(ctx, storage.RunRecord{
		ID: runID, LoopID: loopID, Status: "running", StartedAt: quarantinedISO,
		CreatedAt: quarantinedISO, UpdatedAt: quarantinedISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	lastErrorKind := "manual_intervention"
	if err := services.Repositories.Queue.Upsert(ctx, storage.QueueItemRecord{
		ID: "queue_retry_quarantine_rollback", ProjectID: &projectID, LoopID: &loopID, Type: "worker",
		TargetType: "project", TargetID: targetID, DedupeKey: dedupeKey,
		Priority: storage.QueuePriorityWorker, Status: "failed", AvailableAt: quarantinedISO,
		Attempts: 1, MaxAttempts: 3, LastErrorKind: &lastErrorKind, CreatedAt: quarantinedISO, UpdatedAt: quarantinedISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	deadPID := int64(999998)
	metadata := `{"processIdentity":{"startTime":515000,"bootId":"boot-test"}}`
	command := `{"command":"codex","args":["exec"]}`
	if err := services.Repositories.AgentExecutions.Upsert(ctx, storage.AgentExecutionRecord{
		ID: executionID, ProjectID: &projectID, LoopID: &loopID, RunID: &runID, Vendor: "codex",
		Status: "running", PID: &deadPID, CommandJSON: &command, MetadataJSON: &metadata,
		StartedAt: quarantinedISO, CreatedAt: quarantinedISO, UpdatedAt: quarantinedISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	entityType := "agent_execution"
	entityID := executionID
	if err := services.Repositories.Events.Append(ctx, storage.EventLogRecord{
		ID: "event_retry_quarantine_rollback", EventType: "looperd.recovery.execution_quarantined",
		ProjectID: &projectID, LoopID: &loopID, RunID: &runID,
		EntityType: &entityType, EntityID: &entityID, PayloadJSON: `{}`, CreatedAt: quarantinedISO,
	}); err != nil {
		t.Fatalf("Events.Append() error = %v", err)
	}

	// Make the requeue fail after settlement has already been applied inside the
	// transaction, the way a concurrent requeue for the same target would.
	h.retryAfterClearStopGateHook = func(id string) {
		if id != loopID {
			return
		}
		if err := services.Repositories.Queue.Upsert(ctx, storage.QueueItemRecord{
			ID: "queue_retry_quarantine_rollback_active", ProjectID: &projectID, LoopID: &otherLoopID, Type: "worker",
			TargetType: "project", TargetID: targetID, DedupeKey: dedupeKey,
			Priority: storage.QueuePriorityWorker, Status: "queued", AvailableAt: nowISO,
			Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
		}); err != nil {
			t.Errorf("inject active dedupe: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/5150/retry", strings.NewReader(`{"mode":"auto","resetAttempts":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code == http.StatusOK {
		t.Fatalf("status = 200, want the retry to fail so rollback is exercised; body=%s", recorder.Body.String())
	}
	// The failure must come from the requeue, not from the running-run
	// precondition: otherwise settlement never ran and this asserts nothing.
	if body := recorder.Body.String(); strings.Contains(body, "while a run is active") {
		t.Fatalf("retry failed on the running-run precondition, so settlement never ran: %s", body)
	}

	execution, err := services.Repositories.AgentExecutions.GetByID(ctx, executionID)
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if execution.Status != "running" {
		t.Fatalf("execution status = %q, want it still running: a failed retry must not retire quarantine evidence", execution.Status)
	}
	run, err := services.Repositories.Runs.GetByID(ctx, runID)
	if err != nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run.Status != "running" {
		t.Fatalf("run status = %q, want it still running after the failed retry", run.Status)
	}
	debt, err := looperdruntime.CountOutstandingQuarantineDebt(ctx, services.Repositories)
	if err != nil {
		t.Fatalf("CountOutstandingQuarantineDebt() error = %v", err)
	}
	if debt.QuarantinedActiveExecutions != 1 || debt.QuarantinedRunningRuns != 1 {
		t.Fatalf("debt = %#v, want the debt intact after a failed retry", debt)
	}
}
