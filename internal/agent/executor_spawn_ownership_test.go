package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestInitialCancellationDuringOwnershipPersistenceReapsProcess(t *testing.T) {
	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	tx, err := coordinator.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(context.Background(), `UPDATE agent_executions SET status = status`); err != nil {
		t.Fatalf("hold SQLite write lock: %v", err)
	}

	workDir := t.TempDir()
	pidPath := filepath.Join(workDir, "agent.pid")
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{
			"command": "/bin/sh",
			"args":    []any{"-c", `trap '' TERM; echo $$ > "$PID_FILE"; while true; do sleep 1; done`},
		}},
		Repos: repos,
	})
	type startOutcome struct {
		execution Execution
		err       error
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	startCh := make(chan startOutcome, 1)
	go func() {
		execHandle, startErr := executor.Start(runCtx, RunInput{
			ExecutionID:      "agent_initial_persist_cancel",
			WorkingDirectory: workDir,
			Prompt:           "ignored",
			Timeout:          10 * time.Second,
			Env:              map[string]string{"PID_FILE": pidPath},
		})
		startCh <- startOutcome{execution: execHandle, err: startErr}
	}()

	pid := waitForPIDFile(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	cancelRun()
	waitForProcessExit(t, pid, time.Second)
	select {
	case outcome := <-startCh:
		if outcome.execution != nil {
			t.Fatalf("Start() execution = %#v, want nil after cancellation", outcome.execution)
		}
		if !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("Start() error = %v, want context.Canceled", outcome.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start() remained blocked on ownership persistence after cancellation")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
}

func TestFallbackCancellationDuringOwnershipPersistenceReapsProcess(t *testing.T) {
	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	workDir := t.TempDir()
	resumeReadyPath := filepath.Join(workDir, "resume.ready")
	resumeReleasePath := filepath.Join(workDir, "resume.release")
	fallbackPIDPath := filepath.Join(workDir, "fallback.pid")
	scriptPath := filepath.Join(t.TempDir(), "mock-codex")
	script := "#!/bin/sh\ncase \"$*\" in *resume*) : > \"$RESUME_READY_FILE\"; while [ ! -f \"$RESUME_RELEASE_FILE\" ]; do :; done; printf '%s\\n' 'resume failed' >&2; exit 2;; esac\ntrap '' TERM\necho $$ > \"$FALLBACK_PID_FILE\"\nwhile true; do sleep 1; done\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(scriptPath) error = %v", err)
	}
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{
			Vendor:              config.AgentVendorCodex,
			Params:              map[string]any{"command": scriptPath},
			NativeResumeEnabled: true,
		},
		Repos: repos,
	})
	runCtx, cancelRun := context.WithCancel(context.Background())
	execHandle, err := executor.Start(runCtx, RunInput{
		ExecutionID:      "agent_fallback_persist_cancel",
		WorkingDirectory: workDir,
		Prompt:           "checkpoint prompt",
		NativeSessionID:  "session-1",
		Timeout:          10 * time.Second,
		GracefulShutdown: 20 * time.Millisecond,
		Env: map[string]string{
			"RESUME_READY_FILE":   resumeReadyPath,
			"RESUME_RELEASE_FILE": resumeReleasePath,
			"FALLBACK_PID_FILE":   fallbackPIDPath,
		},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		cancelRun()
		_ = execHandle.Kill("test cleanup")
		_, _ = execHandle.Wait(context.Background())
	})
	waitForFile(t, resumeReadyPath)

	tx, err := coordinator.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(context.Background(), `UPDATE agent_executions SET status = status WHERE id = ?`, "agent_fallback_persist_cancel"); err != nil {
		t.Fatalf("hold SQLite write lock: %v", err)
	}
	if err := os.WriteFile(resumeReleasePath, nil, 0o644); err != nil {
		t.Fatalf("WriteFile(resume release) error = %v", err)
	}
	fallbackPID := waitForPIDFile(t, fallbackPIDPath)
	t.Cleanup(func() { _ = syscall.Kill(-fallbackPID, syscall.SIGKILL) })

	cancelRun()
	waitForProcessExit(t, fallbackPID, time.Second)
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "killed" {
		t.Fatalf("result.Status = %q, want killed", result.Status)
	}
}
