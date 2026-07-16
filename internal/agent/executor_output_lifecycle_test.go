package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestSpawnedProcessIsOwnedWhileInvokedEventSinkIsBlocked(t *testing.T) {
	workDir := t.TempDir()
	pidPath := filepath.Join(workDir, "agent.pid")
	eventStarted := make(chan struct{})
	releaseEvent := make(chan struct{})
	var eventOnce sync.Once
	executor := New(ExecutorOptions{Config: ExecutorConfig{
		Vendor: config.AgentVendor("custom"),
		Params: map[string]any{
			"command": "/bin/sh",
			"args":    []any{"-c", `trap '' TERM; echo $$ > "$PID_FILE"; while true; do sleep 1; done`},
		},
	}})
	executor.appendLifecycleRecord = func(context.Context, storage.EventLogRecord) error {
		eventOnce.Do(func() { close(eventStarted) })
		<-releaseEvent
		return nil
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	type startOutcome struct {
		execution Execution
		err       error
	}
	started := make(chan startOutcome, 1)
	go func() {
		execHandle, err := executor.Start(runCtx, RunInput{
			ExecutionID:      "agent_blocked_invoked_event",
			WorkingDirectory: workDir,
			Prompt:           "ignored",
			Timeout:          10 * time.Second,
			GracefulShutdown: 20 * time.Millisecond,
			Env:              map[string]string{"PID_FILE": pidPath},
		})
		started <- startOutcome{execution: execHandle, err: err}
	}()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseEvent) }) }
	t.Cleanup(release)

	select {
	case <-eventStarted:
	case <-time.After(time.Second):
		t.Fatal("invoked-event sink did not start")
	}
	pid := waitForPIDFile(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	cancelRun()
	waitForProcessExit(t, pid, time.Second)

	select {
	case outcome := <-started:
		if outcome.err != nil {
			t.Fatalf("Start() error = %v", outcome.err)
		}
		result, err := outcome.execution.Wait(context.Background())
		if err != nil {
			t.Fatalf("Wait() error = %v", err)
		}
		if result.Status != "killed" {
			t.Fatalf("result.Status = %q, want killed", result.Status)
		}
	case <-time.After(time.Second):
		t.Fatal("Start()/Wait() remained blocked behind invoked-event sink")
	}
}

func TestBlockedLogSideEffectCannotBackpressureOutputOrTeardown(t *testing.T) {
	workDir := t.TempDir()
	outputDrainedPath := filepath.Join(workDir, "output-drained")
	logStarted := make(chan struct{})
	releaseLog := make(chan struct{})
	var logOnce sync.Once
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{
			"command": "/bin/sh",
			"args": []any{"-c", `trap '' TERM; /bin/dd if=/dev/zero bs=65536 count=384 2>/dev/null; ` +
				`: > "$OUTPUT_DRAINED_FILE"; while true; do sleep 1; done`},
		}},
		LogDir: t.TempDir(),
	})
	executor.appendPersistedLog = func(string, []byte) bool {
		logOnce.Do(func() { close(logStarted) })
		<-releaseLog
		return false
	}

	execHandle, err := executor.Start(context.Background(), RunInput{
		ExecutionID:      "agent_blocked_log_side_effect",
		WorkingDirectory: workDir,
		Prompt:           "ignored",
		Timeout:          10 * time.Second,
		GracefulShutdown: 20 * time.Millisecond,
		Env:              map[string]string{"OUTPUT_DRAINED_FILE": outputDrainedPath},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseLog) }) }
	t.Cleanup(func() {
		release()
		_ = execHandle.Kill("test cleanup")
		_, _ = execHandle.Wait(context.Background())
	})

	select {
	case <-logStarted:
	case <-time.After(time.Second):
		t.Fatal("persisted-log side effect did not start")
	}
	waitForOutputLifecycleFile(t, outputDrainedPath, 5*time.Second)

	type outcome struct {
		result Result
		err    error
	}
	finished := make(chan outcome, 1)
	startedAt := time.Now()
	go func() {
		killErr := execHandle.Kill("blocked output side effect")
		if killErr != nil {
			finished <- outcome{err: killErr}
			return
		}
		result, waitErr := execHandle.Wait(context.Background())
		finished <- outcome{result: result, err: waitErr}
	}()
	select {
	case got := <-finished:
		if got.err != nil {
			t.Fatalf("Kill()/Wait() error = %v", got.err)
		}
		if got.result.Status != "killed" {
			t.Fatalf("result.Status = %q, want killed", got.result.Status)
		}
		if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
			t.Fatalf("teardown elapsed = %s, want bounded independently of log I/O", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Kill()/Wait() blocked behind persisted-log side effect")
	}
}

func waitForOutputLifecycleFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for output-drained marker %s", path)
}
