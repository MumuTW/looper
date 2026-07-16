package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestPersistFinalWaitsForTerminalStatusBeyondOutputTimeout(t *testing.T) {
	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	workDir := t.TempDir()
	pidPath := filepath.Join(workDir, "agent.pid")
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{
			"command": "/bin/sh",
			"args":    []any{"-c", `trap '' TERM; echo $$ > "$PID_FILE"; while true; do sleep 1; done`},
		}},
		Repos: repos,
	})
	const executionID = "agent_terminal_persist_authoritative"
	execHandle, err := executor.Start(context.Background(), RunInput{
		ExecutionID:      executionID,
		WorkingDirectory: workDir,
		Prompt:           "ignored",
		Timeout:          10 * time.Second,
		GracefulShutdown: 20 * time.Millisecond,
		Env:              map[string]string{"PID_FILE": pidPath},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	pid := waitForPIDFile(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	running, err := repos.AgentExecutions.GetByID(context.Background(), executionID)
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID(running) error = %v", err)
	}
	if running == nil || running.Status != "running" {
		t.Fatalf("pre-terminal execution = %#v, want status running", running)
	}

	tx, err := coordinator.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(context.Background(), `UPDATE agent_executions SET status = status WHERE id = ?`, executionID); err != nil {
		t.Fatalf("hold SQLite write lock: %v", err)
	}

	type outcome struct {
		result Result
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		if killErr := execHandle.Kill("terminal persist lock"); killErr != nil {
			finished <- outcome{err: killErr}
			return
		}
		result, waitErr := execHandle.Wait(context.Background())
		finished <- outcome{result: result, err: waitErr}
	}()

	// Best-effort outputPersistenceTimeout is 250ms. Terminal persistence must
	// still be waiting for the durable write after that budget expires.
	select {
	case got := <-finished:
		t.Fatalf("Wait() returned after %v while terminal Upsert was blocked: %#v err=%v", outputPersistenceTimeout, got.result, got.err)
	case <-time.After(outputPersistenceTimeout + 150*time.Millisecond):
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	select {
	case got := <-finished:
		if got.err != nil {
			t.Fatalf("Kill()/Wait() error = %v", got.err)
		}
		if got.result.Status != "killed" {
			t.Fatalf("result.Status = %q, want killed", got.result.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() remained blocked after terminal write lock was released")
	}

	record, err := repos.AgentExecutions.GetByID(context.Background(), executionID)
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID(terminal) error = %v", err)
	}
	if record == nil || record.Status != "killed" || record.EndedAt == nil {
		t.Fatalf("terminal execution = %#v, want durable killed status with ended_at", record)
	}
	active, err := repos.AgentExecutions.ListActive(context.Background())
	if err != nil {
		t.Fatalf("AgentExecutions.ListActive() error = %v", err)
	}
	for _, item := range active {
		if item.ID == executionID {
			t.Fatalf("ListActive still includes %s after terminal persistence", executionID)
		}
	}
}

func TestPersistFinalFailurePropagatesFromWait(t *testing.T) {
	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	workDir := t.TempDir()
	pidPath := filepath.Join(workDir, "agent.pid")
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{
			"command": "/bin/sh",
			"args":    []any{"-c", `trap '' TERM; echo $$ > "$PID_FILE"; while true; do sleep 1; done`},
		}},
		Repos: repos,
	})
	const executionID = "agent_terminal_persist_failure"
	execHandle, err := executor.Start(context.Background(), RunInput{
		ExecutionID:      executionID,
		WorkingDirectory: workDir,
		Prompt:           "ignored",
		Timeout:          10 * time.Second,
		GracefulShutdown: 20 * time.Millisecond,
		Env:              map[string]string{"PID_FILE": pidPath},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	pid := waitForPIDFile(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	tx, err := coordinator.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(context.Background(), `UPDATE agent_executions SET status = status WHERE id = ?`, executionID); err != nil {
		t.Fatalf("hold SQLite write lock: %v", err)
	}

	type outcome struct {
		result Result
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		if killErr := execHandle.Kill("terminal persist timeout"); killErr != nil {
			finished <- outcome{err: killErr}
			return
		}
		result, waitErr := execHandle.Wait(context.Background())
		finished <- outcome{result: result, err: waitErr}
	}()

	// Keep the write lock past ownershipPersistenceTimeout so terminal Upsert fails.
	// Wait must surface that failure so ActiveExecutionRegistry retains ownership.
	select {
	case got := <-finished:
		if got.err == nil {
			t.Fatalf("Wait() err = nil after terminal persist deadline; result=%#v", got.result)
		}
		if !errors.Is(got.err, context.DeadlineExceeded) && !strings.Contains(got.err.Error(), "persist terminal agent execution status") {
			t.Fatalf("Wait() error = %v, want terminal persist failure", got.err)
		}
		if got.result.Status != "killed" {
			t.Fatalf("result.Status = %q, want killed (process reaped even when durable write fails)", got.result.Status)
		}
	case <-time.After(ownershipPersistenceTimeout + 3*time.Second):
		t.Fatal("Wait() remained blocked after ownershipPersistenceTimeout for failed terminal Upsert")
	}

	record, err := repos.AgentExecutions.GetByID(context.Background(), executionID)
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if record == nil || record.Status != "running" {
		t.Fatalf("durable execution = %#v, want still running while terminal write failed", record)
	}
	active, err := repos.AgentExecutions.ListActive(context.Background())
	if err != nil {
		t.Fatalf("AgentExecutions.ListActive() error = %v", err)
	}
	found := false
	for _, item := range active {
		if item.ID == executionID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ListActive missing %s after failed terminal persistence", executionID)
	}
}
