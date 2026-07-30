package runtime

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/processcontainment"
	"github.com/nexu-io/looper/internal/storage"
)

func TestRuntimeReconcileStaleRunningRunsQuarantinesSameCommandPIDReuse(t *testing.T) {
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
		MetadataJSON:    stringPtr(`{"processIdentity":{"startTime":2201,"bootId":"boot-a"}}`),
		LastHeartbeatAt: &oldISO, StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	summary, err := rt.ReconcileStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
	}
	if summary.SkippedUncertainRuns != 1 || summary.QuarantinedExecutions != 1 || summary.CleanedExecutions != 0 || summary.LoopsRequeued != 0 {
		t.Fatalf("summary = %#v, want reused PID evidence quarantined without overlap", summary)
	}
}

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
	release := bindStaleExecutionTestOwner(t, rt.Services().ActiveExecutions, loopID, runID, executionID)
	defer release()

	summary, err := rt.ReconcileStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
	}
	if summary.InterruptedRuns != 0 || summary.CleanedExecutions != 0 || summary.LoopsRequeued != 0 {
		t.Fatalf("summary = %#v, want current Supervisor owner preserved", summary)
	}
}

func bindStaleExecutionTestOwner(t *testing.T, registry *ActiveExecutionRegistry, loopID, runID, executionID string) func() {
	t.Helper()
	lease, err := registry.AdmitSpawn(context.Background(), agent.SpawnMeta{LoopID: loopID, RunID: runID, ExecutionID: executionID})
	if err != nil {
		t.Fatalf("AdmitSpawn: %v", err)
	}
	cmd := exec.Command("sleep", "60")
	processcontainment.Configure(cmd)
	if err := cmd.Start(); err != nil {
		lease.Release()
		t.Fatalf("start test process: %v", err)
	}
	handle, err := processcontainment.Bind(cmd, processcontainment.Options{GracePeriod: 10 * time.Millisecond, DrainTimeout: time.Second})
	if err != nil {
		_ = cmd.Process.Kill()
		lease.Release()
		t.Fatalf("bind test process: %v", err)
	}
	if err := lease.BindHandle(handle, nil); err != nil {
		_ = handle.Kill(context.Background())
		lease.Release()
		t.Fatalf("BindHandle: %v", err)
	}
	return func() {
		_ = handle.Kill(context.Background())
		lease.Release()
	}
}
