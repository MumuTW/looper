package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
)

func TestRuntimeStopForceReapsTermResistantRegisteredExecution(t *testing.T) {
	workingDir := t.TempDir()
	pidPath := filepath.Join(workingDir, "agent.pid")
	executor := agent.New(agent.ExecutorOptions{Config: agent.ExecutorConfig{
		Vendor: config.AgentVendor("custom"),
		Params: map[string]any{
			"command": "/bin/sh",
			"args":    []any{"-c", `trap '' TERM; echo $$ > "$PID_FILE"; while :; do sleep 1; done`},
		},
	}})
	execution, err := executor.Start(context.Background(), agent.RunInput{
		ExecutionID:      "exec_runtime_shutdown",
		LoopID:           "loop_runtime_shutdown",
		RunID:            "run_runtime_shutdown",
		WorkingDirectory: workingDir,
		Prompt:           "ignored",
		Timeout:          10 * time.Second,
		GracefulShutdown: 5 * time.Second,
		Env:              map[string]string{"PID_FILE": pidPath},
	})
	if err != nil {
		t.Fatalf("executor.Start() error = %v", err)
	}
	waitForRecoveryContractFile(t, pidPath)
	rawPID, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil {
		t.Fatalf("parse pid file: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	rt := New(Options{ShutdownTimeout: 30 * time.Millisecond})
	rt.activeExecutions.Register("loop_runtime_shutdown", "run_runtime_shutdown", "exec_runtime_shutdown", execution)
	rt.Stop("test shutdown")

	if err := syscall.Kill(-pid, 0); err == nil || !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("Runtime.Stop returned while process group %d was still live: %v", pid, err)
	}
	result, err := execution.Wait(context.Background())
	if err != nil {
		t.Fatalf("execution.Wait() error = %v", err)
	}
	if result.Status != "killed" {
		t.Fatalf("execution status = %q, want killed", result.Status)
	}
}
