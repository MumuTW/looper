package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/agent"
	"github.com/MumuTW/looper/internal/storage"
)

// Close and retry must share one per-loop critical section. Once close enters
// that section, a retry cannot clear its sticky stop gate or publish a new
// claim before terminalization; after close wins, spawn admission stays closed.
func TestHandlerLoopCloseSerializesAgainstRetry(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	services := rt.Services()
	ctx := context.Background()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_close_retry_gate"
	loopID := "loop_close_retry_gate"
	targetID := projectID

	if err := services.Repositories.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(ctx, storage.LoopRecord{
		ID: loopID, Seq: 3142, ProjectID: projectID, Type: "worker", TargetType: "project",
		TargetID: &targetID, Status: "failed", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	lastErrorKind := "non_retryable"
	if err := services.Repositories.Queue.Upsert(ctx, storage.QueueItemRecord{
		ID: "queue_close_retry_gate", ProjectID: &projectID, LoopID: &loopID, Type: "worker",
		TargetType: "project", TargetID: targetID, DedupeKey: "worker:close_retry_gate",
		Priority: storage.QueuePriorityWorker, Status: "failed", AvailableAt: nowISO,
		Attempts: 1, MaxAttempts: 3, LastErrorKind: &lastErrorKind, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	if services.ActiveExecutions == nil {
		t.Fatal("ActiveExecutions is nil")
	}
	if _, err := services.ActiveExecutions.BeginLoopStop(loopID, "failed loop"); err != nil {
		t.Fatalf("BeginLoopStop() error = %v", err)
	}

	closeEntered := make(chan struct{})
	allowClose := make(chan struct{})
	h := NewHandler(Context{
		Config:  cfg,
		Runtime: rt,
		CloseLoop: func(closeCtx context.Context, id, reason string) (any, error) {
			close(closeEntered)
			<-allowClose
			if _, err := services.ActiveExecutions.BeginLoopStop(id, reason); err != nil {
				return nil, err
			}
			if _, err := services.Loops.Terminate(closeCtx, id, &reason); err != nil {
				return nil, err
			}
			return map[string]any{"stopped": true, "loopId": id}, nil
		},
	})

	closeRecorder := httptest.NewRecorder()
	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		h.ServeHTTP(closeRecorder, httptest.NewRequest(http.MethodPost, "/api/v1/runs/active/3142/close", nil))
	}()
	select {
	case <-closeEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not enter lifecycle callback")
	}

	retryRecorder := httptest.NewRecorder()
	retryDone := make(chan struct{})
	go func() {
		defer close(retryDone)
		h.ServeHTTP(retryRecorder, httptest.NewRequest(http.MethodPost, "/api/v1/loops/3142/retry", strings.NewReader(`{"mode":"auto"}`)))
	}()
	select {
	case <-retryDone:
		close(allowClose)
		t.Fatalf("retry completed while close held the lifecycle section: status=%d body=%s", retryRecorder.Code, retryRecorder.Body.String())
	case <-time.After(150 * time.Millisecond):
	}

	close(allowClose)
	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not finish")
	}
	if closeRecorder.Code != http.StatusOK {
		t.Fatalf("close status = %d, want 200; body=%s", closeRecorder.Code, closeRecorder.Body.String())
	}
	select {
	case <-retryDone:
	case <-time.After(5 * time.Second):
		t.Fatal("retry did not finish after close released the lifecycle section")
	}
	if retryRecorder.Code == http.StatusOK {
		t.Fatalf("retry status = 200 after close won; body=%s", retryRecorder.Body.String())
	}

	loop, err := services.Repositories.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil || loop.Status != "terminated" || loop.NextRunAt != nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want terminated loop without next run", loop, err)
	}
	if active, err := services.Repositories.Queue.FindActiveByLoopID(ctx, loopID); err != nil || active != nil {
		t.Fatalf("Queue.FindActiveByLoopID() = (%#v, %v), want no claimable work", active, err)
	}
	if _, err := services.ActiveExecutions.AdmitSpawn(ctx, agent.SpawnMeta{
		LoopID: loopID, RunID: "run_after_close_retry", ExecutionID: "exec_after_close_retry",
	}); !errors.Is(err, agent.ErrSpawnLoopStopping) {
		t.Fatalf("AdmitSpawn after close won error = %v, want ErrSpawnLoopStopping", err)
	}
}

// Retry must restore the sticky stop gate when a later TX validation fails so
// a failed retry cannot reopen AdmitSpawn for stale pre-stop runners.
func TestHandlerLoopRetryRestoresStopGateOnTXConflict(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_retry_restore_stop_gate"
	loopID := "loop_retry_restore_stop_gate"
	targetID := projectID
	dedupeKey := "worker:restore_stop_gate"
	otherLoopID := "loop_retry_restore_stop_gate_other"

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 3141, ProjectID: projectID, Type: "worker", TargetType: "project",
		TargetID: &targetID, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	lastErrorKind := "manual_intervention"
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_retry_restore_stop_gate_failed", ProjectID: &projectID, LoopID: &loopID, Type: "worker",
		TargetType: "project", TargetID: targetID, DedupeKey: dedupeKey,
		Priority: storage.QueuePriorityWorker, Status: "failed", AvailableAt: nowISO,
		Attempts: 1, MaxAttempts: 3, LastErrorKind: &lastErrorKind, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() failed item error = %v", err)
	}
	// Sibling loop exists but has no active queue yet so preflight dedupe passes.
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: otherLoopID, Seq: 3142, ProjectID: projectID, Type: "worker", TargetType: "project",
		TargetID: &targetID, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() other loop error = %v", err)
	}
	if services.ActiveExecutions == nil {
		t.Fatal("ActiveExecutions is nil")
	}
	if _, err := services.ActiveExecutions.BeginLoopStop(loopID, "looper stop"); err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	if !services.ActiveExecutions.LoopStopActive(loopID) {
		t.Fatal("LoopStopActive = false before retry, want sticky closed")
	}

	// After ClearLoopStop and before the requeue TX, inject an active dedupe
	// row so the TX fails the way a concurrent requeue would.
	h.retryAfterClearStopGateHook = func(id string) {
		if id != loopID {
			return
		}
		if services.ActiveExecutions.LoopStopActive(loopID) {
			t.Error("LoopStopActive still true in after-clear hook, want gate already cleared")
		}
		if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{
			ID: "queue_retry_restore_stop_gate_active", ProjectID: &projectID, LoopID: &otherLoopID, Type: "worker",
			TargetType: "project", TargetID: targetID, DedupeKey: dedupeKey,
			Priority: storage.QueuePriorityWorker, Status: "queued", AvailableAt: nowISO,
			Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO,
		}); err != nil {
			t.Errorf("inject active dedupe: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3141/retry", strings.NewReader(`{"mode":"auto","resetAttempts":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
	}
	if !services.ActiveExecutions.LoopStopActive(loopID) {
		t.Fatal("LoopStopActive = false after failed retry, want sticky gate restored")
	}
	if _, err := services.ActiveExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: loopID, RunID: "run_retry_restore", ExecutionID: "exec_retry_restore",
	}); !errors.Is(err, agent.ErrSpawnLoopStopping) {
		t.Fatalf("AdmitSpawn after failed retry error = %v, want ErrSpawnLoopStopping", err)
	}
}

// Retry must clear the sticky stop gate before publishing claimable queue work
// so a concurrent scheduler tick cannot claim then fail AdmitSpawn.

// Retry must clear the sticky stop gate before publishing claimable queue work
// so a concurrent scheduler tick cannot claim then fail AdmitSpawn.

// Retry must clear the sticky stop gate before publishing claimable queue work
// so a concurrent scheduler tick cannot claim then fail AdmitSpawn.
func TestHandlerLoopRetryClearsStopGateBeforeClaimable(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_retry_clear_stop_gate"
	loopID := "loop_retry_clear_stop_gate"
	targetID := projectID

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 3140, ProjectID: projectID, Type: "worker", TargetType: "project",
		TargetID: &targetID, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	lastErrorKind := "manual_intervention"
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_retry_clear_stop_gate", ProjectID: &projectID, LoopID: &loopID, Type: "worker",
		TargetType: "project", TargetID: targetID, DedupeKey: "worker:clear_stop_gate",
		Priority: storage.QueuePriorityWorker, Status: "failed", AvailableAt: nowISO,
		Attempts: 1, MaxAttempts: 3, LastErrorKind: &lastErrorKind, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	if services.ActiveExecutions == nil {
		t.Fatal("ActiveExecutions is nil")
	}
	if _, err := services.ActiveExecutions.BeginLoopStop(loopID, "looper stop"); err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	if !services.ActiveExecutions.LoopStopActive(loopID) {
		t.Fatal("LoopStopActive = false before retry, want sticky closed")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/3140/retry", strings.NewReader(`{"mode":"auto","resetAttempts":true}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if services.ActiveExecutions.LoopStopActive(loopID) {
		t.Fatal("LoopStopActive = true after retry, want ClearLoopStop before claimable queue publish")
	}
	if _, err := services.ActiveExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: loopID, RunID: "run_retry_clear", ExecutionID: "exec_retry_clear",
	}); err != nil {
		t.Fatalf("AdmitSpawn after retry error = %v, want success", err)
	}
}

// TestHandlerLoopStartClearsStopGateBeforeClaimable ensures start/unpause clears
// the sticky stop gate before the requeue TX commits claimable work (same race
// as retry: concurrent tick must not see running+queued with gate still closed).
