package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

func TestRuntimeReconcileStaleRunningRunsDetectsSameCommandPIDReuse(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	rt, repos := newStaleExecutionLeaseRuntime(t, now, func(context.Context, int) (string, error) {
		return "codex exec", nil
	})
	loopID, runID, _ := seedStaleExecutionLeaseRun(t, repos, now, "same_command_pid_reuse")
	oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))
	pid := int64(2203)
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: "execution_same_command_pid_reuse", ProjectID: stringPtr("project_1"),
		LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running",
		PID: &pid, CommandJSON: stringPtr(`{"command":"codex","args":["exec"]}`),
		MetadataJSON:    stringPtr(`{"processIdentity":{"startTime":2201}}`),
		LastHeartbeatAt: &oldISO, StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	summary, err := rt.ReconcileStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
	}
	if summary.CleanedExecutions != 1 || summary.LoopsRequeued != 1 {
		t.Fatalf("summary = %#v, want reused PID identity finalized and requeued", summary)
	}
}

func TestRuntimeReconcileNeverOverridesCurrentSupervisorOwner(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	rt, repos := newStaleExecutionLeaseRuntime(t, now, nil)
	loopID, runID, _ := seedStaleExecutionLeaseRun(t, repos, now, "supervisor_owned")
	oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))
	executionID := "execution_supervisor_owned"
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: executionID, ProjectID: stringPtr("project_1"), LoopID: &loopID, RunID: &runID,
		Vendor: "codex", Status: "running", LastHeartbeatAt: &oldISO,
		StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	release := rt.Services().ActiveExecutions.Register(loopID, runID, executionID, stubAgentExecution{})
	defer release()

	summary, err := rt.ReconcileStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
	}
	if summary.InterruptedRuns != 0 || summary.CleanedExecutions != 0 || summary.LoopsRequeued != 0 {
		t.Fatalf("summary = %#v, want current Supervisor owner preserved", summary)
	}
}
