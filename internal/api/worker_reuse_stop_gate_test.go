package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/agent"
	"github.com/MumuTW/looper/internal/storage"
)

// TestHandlerWorkersCreateReuseClearsStickyStopGate ensures issue-worker reuse
// (paused → queued) reopens the sticky stop spawn gate closed by looper stop.
// Without this, recreate-same-issue claims the queue then AgentExecutor.Start
// fails with ErrSpawnLoopStopping forever.
func TestHandlerWorkersCreateReuseClearsStickyStopGate(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"

	baseBranch := "main"
	metadata := `{"repo":"acme/looper"}`
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: "project_worker_reuse_stop_gate", Name: "Looper", RepoPath: "/tmp/repos/looper",
		BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	loopID := "loop_worker_reuse_stop_gate"
	targetID := "issue:acme/looper:99"
	repo := "acme/looper"
	workerMeta := `{"worker":{"title":"Stopped issue worker","repo":"acme/looper","baseBranch":"main","issueNumber":99}}`
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 3125, ProjectID: "project_worker_reuse_stop_gate", Type: "worker",
		TargetType: "issue", TargetID: &targetID, Repo: &repo, Status: "paused",
		MetadataJSON: &workerMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	if services.ActiveExecutions == nil {
		t.Fatal("ActiveExecutions is nil")
	}
	// Simulate sticky gate left by successful looper stop (pause without release).
	if _, err := services.ActiveExecutions.BeginLoopStop(loopID, "looper stop"); err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}
	if !services.ActiveExecutions.LoopStopActive(loopID) {
		t.Fatal("LoopStopActive = false before reuse, want sticky closed")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader(
		`{"projectId":"project_worker_reuse_stop_gate","repo":"acme/looper","issueNumber":99,"baseBranch":"main"}`,
	))
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("worker reuse status = %d body=%s, want 200", recorder.Code, recorder.Body.String())
	}
	if services.ActiveExecutions.LoopStopActive(loopID) {
		t.Fatal("LoopStopActive = true after issue-worker reuse, want ClearLoopStop for queued reactivation")
	}
	if _, err := services.ActiveExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: loopID, RunID: "run_reuse", ExecutionID: "exec_reuse",
	}); err != nil {
		t.Fatalf("AdmitSpawn after worker reuse error = %v, want success", err)
	}
}

// TestHandlerWorkersCreateReuseRestoresStopGateOnTXFailure ensures a failed
// issue-worker reuse restores the sticky stop gate when it was pre-cleared
// before the reuse TX published claimable work. force+running is still found as
// a conflicting issue worker (pre-clear runs) then resume fails with conflict.

// TestHandlerWorkersCreateReuseRestoresStopGateOnTXFailure ensures a failed
// issue-worker reuse restores the sticky stop gate when it was pre-cleared
// before the reuse TX published claimable work. force+running is still found as
// a conflicting issue worker (pre-clear runs) then resume fails with conflict.

// TestHandlerWorkersCreateReuseRestoresStopGateOnTXFailure ensures a failed
// issue-worker reuse restores the sticky stop gate when it was pre-cleared
// before the reuse TX published claimable work. force+running is still found as
// a conflicting issue worker (pre-clear runs) then resume fails with conflict.
func TestHandlerWorkersCreateReuseRestoresStopGateOnTXFailure(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"

	baseBranch := "main"
	metadata := `{"repo":"acme/looper"}`
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: "project_worker_reuse_restore_gate", Name: "Looper", RepoPath: "/tmp/repos/looper",
		BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	loopID := "loop_worker_reuse_restore_gate"
	targetID := "issue:acme/looper:98"
	repo := "acme/looper"
	workerMeta := `{"worker":{"title":"Running issue worker","repo":"acme/looper","baseBranch":"main","issueNumber":98}}`
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 3126, ProjectID: "project_worker_reuse_restore_gate", Type: "worker",
		TargetType: "issue", TargetID: &targetID, Repo: &repo, Status: "running",
		MetadataJSON: &workerMeta, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if services.ActiveExecutions == nil {
		t.Fatal("ActiveExecutions is nil")
	}
	if _, err := services.ActiveExecutions.BeginLoopStop(loopID, "looper stop"); err != nil {
		t.Fatalf("BeginLoopStop: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader(
		`{"projectId":"project_worker_reuse_restore_gate","repo":"acme/looper","issueNumber":98,"baseBranch":"main","force":true}`,
	))
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code == http.StatusOK {
		t.Fatalf("worker reuse status = 200, want error; body=%s", recorder.Body.String())
	}
	if !services.ActiveExecutions.LoopStopActive(loopID) {
		t.Fatal("LoopStopActive = false after failed force reuse of running worker, want sticky gate restored")
	}
	if _, err := services.ActiveExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: loopID, RunID: "run_reuse_restore", ExecutionID: "exec_reuse_restore",
	}); !errors.Is(err, agent.ErrSpawnLoopStopping) {
		t.Fatalf("AdmitSpawn after failed reuse error = %v, want ErrSpawnLoopStopping", err)
	}
}

// A collision that arrives after issue-worker reuse has opened its stop gate
// must restore that gate when the transaction rejects the dispatch. This spans
// the API claim admission path and the runtime spawn-admission lifecycle.
func TestHandlerWorkersCreateReuseRestoresStopGateOnIssueClaimCollision(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_worker_reuse_claim_collision"
	loopID := "loop_worker_reuse_claim_collision"
	repo := "acme/looper"
	targetID := "issue:acme/looper:97"
	workerMeta := `{"worker":{"title":"Stopped issue worker","repo":"acme/looper","baseBranch":"main","issueNumber":97}}`
	baseBranch := "main"
	projectMeta := `{"repo":"acme/looper"}`
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", BaseBranch: &baseBranch, MetadataJSON: &projectMeta, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 3130, ProjectID: projectID, Type: "worker", TargetType: "issue", TargetID: &targetID, Repo: &repo, Status: "paused", MetadataJSON: &workerMeta, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert(worker) error = %v", err)
	}
	if _, err := services.ActiveExecutions.BeginLoopStop(loopID, "looper stop"); err != nil {
		t.Fatalf("BeginLoopStop() error = %v", err)
	}

	h.workerReuseAfterClearStopGateHook = func(id string) {
		if id != loopID {
			return
		}
		if services.ActiveExecutions.LoopStopActive(loopID) {
			t.Error("LoopStopActive = true in after-clear hook, want false")
		}
		prTarget := "pr:acme/looper:197"
		prNumber := int64(197)
		claimMeta := `{"worker":{"repo":"acme/looper","issueNumber":97}}`
		// The regular publication path now atomically rejects this competing
		// source-issue claim. Use the explicit override to model a claim that
		// was already admitted elsewhere while reuse had its stop gate open.
		if err := services.Repositories.Loops.UpsertForcingIssueClaimAdmission(context.Background(), storage.LoopRecord{ID: "loop_fixer_claim_collision", Seq: 3131, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", TargetID: &prTarget, Repo: &repo, PRNumber: &prNumber, Status: "running", MetadataJSON: &claimMeta, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
			t.Errorf("Loops.UpsertForcingIssueClaimAdmission(fixer collision) error = %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader(`{"projectId":"project_worker_reuse_claim_collision","repo":"acme/looper","issueNumber":97,"baseBranch":"main"}`))
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("worker reuse status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
	}
	if !services.ActiveExecutions.LoopStopActive(loopID) {
		t.Fatal("LoopStopActive = false after collision, want sticky gate restored")
	}
	if _, err := services.ActiveExecutions.AdmitSpawn(context.Background(), agent.SpawnMeta{LoopID: loopID, RunID: "run_reuse_collision", ExecutionID: "exec_reuse_collision"}); !errors.Is(err, agent.ErrSpawnLoopStopping) {
		t.Fatalf("AdmitSpawn after collision error = %v, want ErrSpawnLoopStopping", err)
	}
}

// TestHandlerWorkersCreateReuseSharesRetryLockWithDiscard ensures POST /workers
// issue-worker reuse takes the same per-loop mutex as discard+retry, so reuse
// cannot enqueue between discard preflight and git reset (wiping the worktree
// for the reuse-created queue item, then failing retry with active-queue conflict).
