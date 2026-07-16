package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestExecutionWaitCancellationKeepsOwnershipUntilProcessGroupIsReaped(t *testing.T) {
	workDir := t.TempDir()
	pidPath := filepath.Join(workDir, "agent.pid")
	executor := New(ExecutorOptions{Config: ExecutorConfig{
		Vendor: config.AgentVendor("custom"),
		Params: map[string]any{
			"command": "/bin/sh",
			"args":    []any{"-c", `trap '' TERM; echo $$ > "$PID_FILE"; while true; do sleep 1; done`},
		},
	}})
	execHandle, err := executor.Start(context.Background(), RunInput{
		ExecutionID:      "agent_wait_cancellation",
		WorkingDirectory: workDir,
		Prompt:           "ignored",
		Timeout:          5 * time.Second,
		GracefulShutdown: 30 * time.Millisecond,
		Env:              map[string]string{"PID_FILE": pidPath},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	pid := waitForPIDFile(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	waitCtx, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	result, err := execHandle.Wait(waitCtx)
	if err != nil {
		t.Fatalf("Wait() error = %v, want terminal result after reaping", err)
	}
	if result.Status != "killed" {
		t.Fatalf("result.Status = %q, want killed", result.Status)
	}
	if err := syscall.Kill(-pid, 0); err != syscall.ESRCH {
		t.Fatalf("process group %d remains after Wait: %v", pid, err)
	}
}

func TestExecutionForceKillEscalatesProcessGroupAndIsSafeAfterExit(t *testing.T) {
	workDir := t.TempDir()
	pidPath := filepath.Join(workDir, "agent.pid")
	executor := New(ExecutorOptions{Config: ExecutorConfig{
		Vendor: config.AgentVendor("custom"),
		Params: map[string]any{
			"command": "/bin/sh",
			"args":    []any{"-c", `trap '' TERM; echo $$ > "$PID_FILE"; while true; do sleep 1; done`},
		},
	}})
	execHandle, err := executor.Start(context.Background(), RunInput{
		ExecutionID:      "agent_force_kill",
		WorkingDirectory: workDir,
		Prompt:           "ignored",
		Timeout:          5 * time.Second,
		GracefulShutdown: time.Second,
		Env:              map[string]string{"PID_FILE": pidPath},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	pid := waitForPIDFile(t, pidPath)
	forceKiller, ok := execHandle.(interface{ ForceKill() error })
	if !ok {
		t.Fatal("execution does not expose optional ForceKill")
	}
	if err := execHandle.Kill("shutdown deadline"); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	if err := forceKiller.ForceKill(); err != nil {
		t.Fatalf("ForceKill() error = %v", err)
	}
	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "killed" {
		t.Fatalf("result.Status = %q, want killed", result.Status)
	}
	waitForProcessExit(t, pid, time.Second)
	concrete := execHandle.(*execution)
	sentinel := exec.Command("/bin/sh", "-c", "while true; do sleep 1; done")
	sentinel.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sentinel.Start(); err != nil {
		t.Fatalf("start reused-group sentinel: %v", err)
	}
	sentinelDone := make(chan error, 1)
	go func() { sentinelDone <- sentinel.Wait() }()
	sentinelWaitConsumed := false
	t.Cleanup(func() {
		_ = syscall.Kill(-sentinel.Process.Pid, syscall.SIGKILL)
		if !sentinelWaitConsumed {
			<-sentinelDone
		}
	})
	concrete.mu.Lock()
	if !concrete.processGroupResolved {
		concrete.mu.Unlock()
		t.Fatal("process group remains signalable after Wait")
	}
	// Model a stale pid handle resolving to a newly-created process group. The
	// resolved bit, not the old numeric pid, must remain authoritative.
	concrete.process = sentinel
	concrete.mu.Unlock()
	if err := forceKiller.ForceKill(); err != nil {
		t.Fatalf("ForceKill() after exit error = %v", err)
	}
	select {
	case err := <-sentinelDone:
		sentinelWaitConsumed = true
		t.Fatalf("ForceKill() signaled a reused process group: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCheckpointFallbackDoesNotSpawnAfterCancellation(t *testing.T) {
	markerPath := filepath.Join(t.TempDir(), "fallback-started")
	scriptPath := filepath.Join(t.TempDir(), "fallback-agent")
	script := "#!/bin/sh\n: > \"$FALLBACK_MARKER_FILE\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(scriptPath) error = %v", err)
	}
	executor := New(ExecutorOptions{Config: ExecutorConfig{
		Vendor: config.AgentVendor("custom"),
		Params: map[string]any{"command": scriptPath},
	}})
	x := &execution{
		executor:           executor,
		input:              RunInput{WorkingDirectory: t.TempDir(), Prompt: "checkpoint", Env: map[string]string{"FALLBACK_MARKER_FILE": markerPath}},
		startedAt:          time.Now(),
		startedAtISO:       time.Now().UTC().Format(time.RFC3339Nano),
		timeout:            time.Second,
		lastHeartbeatAtISO: time.Now().UTC().Format(time.RFC3339Nano),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, _, ok := x.runCheckpointFallback(ctx, "resume failed")
	if !ok || result.Status != "killed" {
		t.Fatalf("fallback result = %#v, ok=%v, want killed without spawn", result, ok)
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("fallback marker stat error = %v, want no fallback process", err)
	}
}

func TestExecutorProgressCallbackCannotBackpressureAgentOutput(t *testing.T) {
	workDir := t.TempDir()
	outputDrainedPath := filepath.Join(workDir, "output-drained")
	callbackStarted := make(chan struct{})
	callbackCanceled := make(chan struct{})
	releaseCallback := make(chan struct{})
	var callbackOnce sync.Once
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{
			"command": "/bin/sh",
			"args":    []any{"-c", `/bin/dd if=/dev/zero bs=65536 count=16 2>/dev/null; : > "$OUTPUT_DRAINED_FILE"; while true; do sleep 1; done`},
		}},
		OnProgress: func(ctx context.Context, _ ProgressUpdate) {
			callbackOnce.Do(func() { close(callbackStarted) })
			select {
			case <-ctx.Done():
				close(callbackCanceled)
			case <-releaseCallback:
			}
		},
	})
	execHandle, err := executor.Start(context.Background(), RunInput{
		ExecutionID:      "agent_progress_backpressure",
		WorkingDirectory: workDir,
		Prompt:           "ignored",
		Timeout:          5 * time.Second,
		GracefulShutdown: 20 * time.Millisecond,
		Env:              map[string]string{"OUTPUT_DRAINED_FILE": outputDrainedPath},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCallback) }) }
	t.Cleanup(func() {
		release()
		_ = execHandle.Kill("test cleanup")
		waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = execHandle.Wait(waitCtx)
		cancel()
	})

	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("progress callback was not invoked")
	}
	waitForFile(t, outputDrainedPath)
	if err := execHandle.Kill("output drained"); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "killed" {
		t.Fatalf("result.Status = %q, want killed", result.Status)
	}
	select {
	case <-callbackCanceled:
	case <-time.After(time.Second):
		t.Fatal("progress callback context was not canceled during teardown")
	}
	if _, err := os.Stat(outputDrainedPath); err != nil {
		t.Fatalf("agent did not drain output before teardown: %v", err)
	}
	// The callback exited through its execution context, so cleanup need not release it.
}

func TestExecutorPersistenceCannotBackpressureBufferedAgentOutput(t *testing.T) {
	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	workDir := t.TempDir()
	readyPath := filepath.Join(workDir, "ready")
	releasePath := filepath.Join(workDir, "release")
	outputDrainedPath := filepath.Join(workDir, "output-drained")
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{
			"command": "/bin/sh",
			"args": []any{"-c", `: > "$READY_FILE"; while [ ! -f "$RELEASE_FILE" ]; do :; done; ` +
				`/bin/dd if=/dev/zero bs=65536 count=16 2>/dev/null; : > "$OUTPUT_DRAINED_FILE"; while true; do sleep 1; done`},
		}},
		Repos: repos,
	})
	execHandle, err := executor.Start(context.Background(), RunInput{
		ExecutionID:      "agent_persistence_backpressure",
		WorkingDirectory: workDir,
		Prompt:           "ignored",
		Timeout:          5 * time.Second,
		GracefulShutdown: 20 * time.Millisecond,
		Env: map[string]string{
			"READY_FILE":          readyPath,
			"RELEASE_FILE":        releasePath,
			"OUTPUT_DRAINED_FILE": outputDrainedPath,
		},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		_ = execHandle.Kill("test cleanup")
		_, _ = execHandle.Wait(context.Background())
	})
	waitForFile(t, readyPath)

	tx, err := coordinator.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(context.Background(), `UPDATE agent_executions SET status = status WHERE id = ?`, "agent_persistence_backpressure"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("hold SQLite write lock: %v", err)
	}
	if err := os.WriteFile(releasePath, nil, 0o644); err != nil {
		_ = tx.Rollback()
		t.Fatalf("WriteFile(release) error = %v", err)
	}
	waitForFile(t, outputDrainedPath)
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}

	if err := execHandle.Kill("output drained while persistence was blocked"); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "killed" {
		t.Fatalf("result.Status = %q, want killed", result.Status)
	}
}
