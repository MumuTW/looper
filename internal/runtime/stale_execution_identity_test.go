package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

func TestExecutionMatchesProcessAcceptsExecTransitionWithSameBirth(t *testing.T) {
	t.Parallel()

	rt := &Runtime{
		readProcessCommand: func(context.Context, int) (string, error) { return "python real-agent.py", nil },
		readProcessStart:   func(context.Context, int) (int64, error) { return 2202, nil },
		readProcessBootID:  func(context.Context, int) (string, error) { return "boot-a", nil },
	}
	execution := storage.AgentExecutionRecord{
		CommandJSON:  stringPtr(`{"command":"wrapper.sh","args":["--run"]}`),
		MetadataJSON: stringPtr(`{"processIdentity":{"startTime":2202,"bootId":"boot-a"}}`),
	}
	matches, running, err := rt.executionMatchesProcess(context.Background(), execution, 2202)
	if err != nil || !running || !matches {
		t.Fatalf("executionMatchesProcess() = (%v, %v, %v), want same-birth exec transition", matches, running, err)
	}
}

func TestExecutionMatchesProcessRejectsSameStartAcrossLinuxBoots(t *testing.T) {
	t.Parallel()

	rt := &Runtime{
		readProcessCommand: func(context.Context, int) (string, error) { return "codex exec", nil },
		readProcessStart:   func(context.Context, int) (int64, error) { return 2202, nil },
		readProcessBootID:  func(context.Context, int) (string, error) { return "boot-new", nil },
	}
	execution := storage.AgentExecutionRecord{
		CommandJSON:  stringPtr(`{"command":"codex","args":["exec"]}`),
		MetadataJSON: stringPtr(`{"processIdentity":{"startTime":2202,"bootId":"boot-old"}}`),
	}
	matches, running, err := rt.executionMatchesProcess(context.Background(), execution, 2202)
	if err != nil || !running || matches {
		t.Fatalf("executionMatchesProcess() = (%v, %v, %v), want reboot mismatch", matches, running, err)
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
