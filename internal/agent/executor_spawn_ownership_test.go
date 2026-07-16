package agent

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

// cancelAfterRunningOwnershipUpsert cancels Start's context immediately after the
// durable running AgentExecutions row commits, reproducing a stop/shutdown race
// before run/persistFinal can launch.
type cancelAfterRunningOwnershipUpsert struct {
	db     *sql.DB
	cancel context.CancelFunc
	once   sync.Once
}

func (q *cancelAfterRunningOwnershipUpsert) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	result, err := q.db.ExecContext(ctx, query, args...)
	if err == nil && strings.Contains(query, "INSERT INTO agent_executions") && len(args) > 5 {
		if status, ok := args[5].(string); ok && status == "running" {
			q.once.Do(func() {
				if q.cancel != nil {
					q.cancel()
				}
			})
		}
	}
	return result, err
}

func (q *cancelAfterRunningOwnershipUpsert) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.db.QueryContext(ctx, query, args...)
}

func (q *cancelAfterRunningOwnershipUpsert) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return q.db.QueryRowContext(ctx, query, args...)
}

func TestStopAfterRunningOwnershipUpsertPersistsTerminalStatus(t *testing.T) {
	coordinator := openAgentCoordinator(t)
	runCtx, cancelRun := context.WithCancel(context.Background())
	t.Cleanup(cancelRun)
	querier := &cancelAfterRunningOwnershipUpsert{db: coordinator.DB(), cancel: cancelRun}
	repos := storage.NewRepositories(querier)

	workDir := t.TempDir()
	pidPath := filepath.Join(workDir, "agent.pid")
	const executionID = "agent_stop_after_running_upsert"
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{
			"command": "/bin/sh",
			"args":    []any{"-c", `trap '' TERM; echo $$ > "$PID_FILE"; while true; do sleep 1; done`},
		}},
		Repos: repos,
	})

	execHandle, err := executor.Start(runCtx, RunInput{
		ExecutionID:      executionID,
		WorkingDirectory: workDir,
		Prompt:           "ignored",
		Timeout:          10 * time.Second,
		Env:              map[string]string{"PID_FILE": pidPath},
	})
	if execHandle != nil {
		t.Cleanup(func() {
			_ = execHandle.Kill("test cleanup")
			_, _ = execHandle.Wait(context.Background())
		})
		t.Fatalf("Start() execution = %#v, want nil after post-upsert stop", execHandle)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context.Canceled", err)
	}

	// Process may already be reaped; best-effort kill if the pid file raced ahead.
	if data, readErr := os.ReadFile(pidPath); readErr == nil {
		if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			waitForProcessExit(t, pid, time.Second)
		}
	}

	record, getErr := repos.AgentExecutions.GetByID(context.Background(), executionID)
	if getErr != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", getErr)
	}
	if record == nil || record.Status != "killed" || record.EndedAt == nil {
		t.Fatalf("execution after post-upsert stop = %#v, want durable killed status with ended_at", record)
	}
	active, listErr := repos.AgentExecutions.ListActive(context.Background())
	if listErr != nil {
		t.Fatalf("AgentExecutions.ListActive() error = %v", listErr)
	}
	for _, item := range active {
		if item.ID == executionID {
			t.Fatalf("ListActive still includes %s after terminal post-upsert stop", executionID)
		}
	}
}

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

func TestStopOutputPersistenceDrainsLiveUpsertBeforeTerminal(t *testing.T) {
	// Live output persistence must fully drain before persistFinal. If stop only
	// waited outputPersistenceTimeout, an in-flight "running" Upsert that outlives
	// cancellation could complete after the terminal row and reanimate ListActive.
	coordinator := openAgentCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	workDir := t.TempDir()
	pidPath := filepath.Join(workDir, "agent.pid")
	executor := New(ExecutorOptions{
		Config: ExecutorConfig{Vendor: config.AgentVendor("custom"), Params: map[string]any{
			"command": "/bin/sh",
			"args": []any{"-c", `trap '' TERM; echo $$ > "$PID_FILE"; ` +
				`while true; do echo live-output-chunk; sleep 0.05; done`},
		}},
		Repos: repos,
	})
	const executionID = "agent_drain_live_output_before_terminal"
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

	// Let the live worker issue at least one running Upsert so the next write is real.
	deadline := time.Now().Add(2 * time.Second)
	for {
		record, getErr := repos.AgentExecutions.GetByID(context.Background(), executionID)
		if getErr != nil {
			t.Fatalf("AgentExecutions.GetByID(pre-lock) error = %v", getErr)
		}
		if record != nil && record.Status == "running" && record.HeartbeatCount > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for live output heartbeat on %s: %#v", executionID, record)
		}
		time.Sleep(20 * time.Millisecond)
	}

	tx, err := coordinator.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(context.Background(), `UPDATE agent_executions SET status = status WHERE id = ?`, executionID); err != nil {
		t.Fatalf("hold SQLite write lock: %v", err)
	}
	// Keep producing output under the write lock so the live persistence worker is
	// blocked inside Upsert when teardown begins.
	time.Sleep(100 * time.Millisecond)

	type outcome struct {
		result Result
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		if killErr := execHandle.Kill("drain live output before terminal"); killErr != nil {
			finished <- outcome{err: killErr}
			return
		}
		result, waitErr := execHandle.Wait(context.Background())
		finished <- outcome{result: result, err: waitErr}
	}()

	// Previously stopOutputPersistence returned after 250ms even while the live
	// Upsert was still in flight. Drain must keep Wait blocked past that budget.
	select {
	case got := <-finished:
		t.Fatalf("Wait() returned after %v while live/terminal Upsert was blocked: %#v err=%v", outputPersistenceTimeout, got.result, got.err)
	case <-time.After(outputPersistenceTimeout + 200*time.Millisecond):
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
		t.Fatal("Wait() remained blocked after write lock was released")
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
			t.Fatalf("ListActive still includes %s after drained terminal persistence", executionID)
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
