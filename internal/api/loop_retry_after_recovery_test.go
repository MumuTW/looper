package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

// `looper retry` was already the operator command #149 assumed was missing. It
// was blocked by the quarantine's own evidence preservation: the runs row stayed
// `running`, and assertLoopRetryPreconditions returns 409 for that. Once startup
// recovery settles the run, retry is available again with no new command.
func TestRetryLoopIsNotBlockedAfterRecoverySettlesTheStaleRun(t *testing.T) {
	fixture := newTestFixture(t)
	rt, cfg := fixture.runtime, fixture.config
	repos := rt.Services().Repositories
	oldISO := fixture.now.Add(-2 * time.Hour).Format(time.RFC3339Nano)

	projectID := "project_retry_after_recovery"
	loopID := "loop_retry_after_recovery"
	runID := "run_retry_after_recovery"

	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Retry", RepoPath: fixture.rootDir, CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "MumuTW/looper"
	prNumber := int64(126)
	targetID := "pr:MumuTW/looper:126"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 35, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "paused", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	// The exact #149 shape: a run left at `running` by a daemon that died.
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", CurrentStep: stringPtr("execute"), StartedAt: oldISO, LastHeartbeatAt: &oldISO, CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	pid := int64(7777)
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: "execution_retry_after_recovery", ProjectID: &projectID, LoopID: &loopID, RunID: &runID,
		Vendor: "codex", Status: "running", PID: &pid, CommandJSON: stringPtr(`{"command":"codex"}`),
		CWD: stringPtr(fixture.rootDir), StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	// Before settlement, retry is exactly the 409 #149 reported.
	handler := NewHandler(Context{Config: cfg, Runtime: rt})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+loopID+"/retry", strings.NewReader("{}")))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("retry before settlement = %d, want 409 body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "while a run is active") {
		t.Fatalf("retry before settlement body = %s, want the active-run conflict", recorder.Body.String())
	}

	// Recovery settles the run under worktree-generation containment.
	if _, err := rt.ReconcileStaleRunningRuns(context.Background()); err != nil {
		t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
	}
	run, err := repos.Runs.GetByID(context.Background(), runID)
	if err != nil || run == nil {
		t.Fatalf("Runs.GetByID() = %#v, %v", run, err)
	}
	if run.Status == "running" {
		t.Fatalf("run = %#v, want the run finalized by recovery", run)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/loops/"+loopID+"/retry", strings.NewReader("{}")))
	if recorder.Code == http.StatusConflict && strings.Contains(recorder.Body.String(), "while a run is active") {
		t.Fatalf("retry after settlement = %d body=%s, want the active-run conflict gone", recorder.Code, recorder.Body.String())
	}
}
